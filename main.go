package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	lua "github.com/yuin/gopher-lua"
	_ "modernc.org/sqlite"
)

type contextKey string

const cancelKey contextKey = "cancel"

type responseRecorder struct {
	http.ResponseWriter
	status int
	size   int64
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(b)
	rec.size += int64(n)
	return n, err
}

func writeLog(level, topic string, details interface{}) {
	logData := map[string]interface{}{
		"details": details,
		"level":   level,
		"time":    time.Now().Format(time.RFC3339),
		"topic":   topic,
	}
	if logJSON, err := json.Marshal(logData); err == nil {
		fmt.Println(string(logJSON))
	}
}

func getCleanHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func hasHiddenComponent(hostRootDir, targetPath string) bool {
	rel, err := filepath.Rel(hostRootDir, targetPath)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." && part != "_" {
			return true
		}
	}
	return false
}

func pushLuaError(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	return 2
}

func pushLuaBoolResult(L *lua.LState, success bool, err error) int {
	if err != nil {
		L.Push(lua.LBool(false))
		L.Push(lua.LString(err.Error()))
	} else {
		L.Push(lua.LBool(true))
		L.Push(lua.LNil)
	}
	return 2
}

func resolveHostPath(rootDir, host, scriptPath, relPath string) (string, error) {
	hostRootDir, err := filepath.Abs(filepath.Join(rootDir, host))
	if err != nil {
		return "", err
	}

	var targetPath string
	if strings.HasPrefix(relPath, "/") {
		targetPath = filepath.Join(hostRootDir, filepath.FromSlash(relPath))
	} else {
		scriptDir := filepath.Dir(scriptPath)
		targetPath = filepath.Join(scriptDir, filepath.FromSlash(relPath))
	}

	cleanTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return "", err
	}

	if hasHiddenComponent(hostRootDir, cleanTarget) {
		return "", fmt.Errorf("access denied: path contains hidden components")
	}

	evalTarget, err := filepath.EvalSymlinks(cleanTarget)
	if err == nil {
		if hasHiddenComponent(hostRootDir, evalTarget) {
			return "", fmt.Errorf("access denied: path contains hidden components")
		}
		return evalTarget, nil
	}

	return cleanTarget, nil
}

func toHostRelativePath(hostRootDir, fullPath string) (string, error) {
	rel, err := filepath.Rel(hostRootDir, fullPath)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "/", nil
	}
	return "/" + filepath.ToSlash(rel), nil
}

func goValueToLua(L *lua.LState, val interface{}) lua.LValue {
	if val == nil {
		return lua.LNil
	}
	switch v := val.(type) {
	case bool:
		return lua.LBool(v)
	case float64:
		return lua.LNumber(v)
	case string:
		return lua.LString(v)
	case map[string]interface{}:
		tbl := L.NewTable()
		for mk, mv := range v {
			tbl.RawSetString(mk, goValueToLua(L, mv))
		}
		return tbl
	case []interface{}:
		tbl := L.NewTable()
		for i, iv := range v {
			tbl.RawSetInt(i+1, goValueToLua(L, iv))
		}
		return tbl
	default:
		return lua.LString(string([]byte{}))
	}
}

func luaValueToGo(val lua.LValue) interface{} {
	switch v := val.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		isMap := false
		maxInt := 0
		count := 0

		v.ForEach(func(k, _ lua.LValue) {
			count++
			if ki, ok := k.(lua.LNumber); ok {
				i := int(ki)
				if i > maxInt {
					maxInt = i
				}
			} else {
				isMap = true
			}
		})

		if isMap || maxInt != count {
			m := make(map[string]interface{})
			v.ForEach(func(k, val lua.LValue) {
				m[k.String()] = luaValueToGo(val)
			})
			return m
		} else {
			slice := make([]interface{}, maxInt)
			for i := 1; i <= maxInt; i++ {
				slice[i-1] = luaValueToGo(v.RawGetInt(i))
			}
			return slice
		}
	default:
		return v.String()
	}
}

func parseTemplate(tmplStr string, data *lua.LTable) (string, error) {
	t, err := template.New("template").Parse(tmplStr)
	if err != nil {
		return "", err
	}

	var goData interface{}
	if data != nil {
		goData = luaValueToGo(data)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, goData); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func handleLuaScript(w http.ResponseWriter, r *http.Request, scriptPath string, db *sql.DB, rootDir, host string) {
	L := lua.NewState()
	defer L.Close()

	hostRootDir, err := filepath.Abs(filepath.Join(rootDir, host))
	if err != nil {
		writeLog("ERROR", "LUA", map[string]string{"error": err.Error(), "host": host})
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal server error"))
		return
	}

	var stdout bytes.Buffer
	var statusCode = http.StatusOK
	headers := make(http.Header)
	var binaryWritten bool

	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		top := L.GetTop()
		var args []string
		for i := 1; i <= top; i++ {
			args = append(args, L.ToString(i))
		}
		stdout.WriteString(strings.Join(args, "\t") + "\n")
		return 0
	}))

	strifeTable := L.NewTable()

	strifeTable.RawSetString("log", L.NewFunction(func(L *lua.LState) int {
		level := "INFO"
		var details interface{}

		top := L.GetTop()
		if top >= 2 {
			level = strings.ToUpper(L.CheckString(1))
			details = luaValueToGo(L.Get(2))
		} else if top == 1 {
			details = luaValueToGo(L.Get(1))
		} else {
			details = ""
		}

		writeLog(level, "LUA", details)
		return 0
	}))

	// --- response.headers Proxy ---
	hTable := L.NewTable()
	hMeta := L.NewTable()
	hMeta.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		if v, ok := headers[key]; ok && len(v) > 0 {
			L.Push(lua.LString(v[0]))
			return 1
		}
		L.Push(lua.LNil)
		return 1
	}))
	hMeta.RawSetString("__newindex", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		val := L.CheckString(3)
		headers.Set(key, val)
		return 0
	}))
	L.SetMetatable(hTable, hMeta)

	// --- response Proxy ---
	rTable := L.NewTable()
	rMeta := L.NewTable()
	rMeta.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		if key == "status" {
			L.Push(lua.LNumber(statusCode))
			return 1
		}
		if key == "headers" {
			L.Push(hTable)
			return 1
		}
		L.Push(lua.LNil)
		return 1
	}))
	rMeta.RawSetString("__newindex", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		if key == "status" {
			statusCode = L.CheckInt(3)
			return 0
		}
		if key == "headers" {
			val := L.Get(3)
			if tbl, ok := val.(*lua.LTable); ok {
				tbl.ForEach(func(k, v lua.LValue) {
					headers.Set(k.String(), v.String())
				})
				return 0
			}
		}
		return 0
	}))

	rTable.RawSetString("writeBlob", L.NewFunction(func(L *lua.LState) int {
		blob := L.CheckString(1)
		// To write directly to the response, we must ensure headers are sent
		for k, vals := range headers {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(statusCode)
		w.Write([]byte(blob))
		binaryWritten = true
		return 0
	}))
	L.SetMetatable(rTable, rMeta)

	strifeTable.RawSetString("response", rTable)

	templateTable := L.NewTable()
	templateTable.RawSetString("parse", L.NewFunction(func(L *lua.LState) int {
		tmplStr := L.CheckString(1)
		var dataTbl *lua.LTable
		if L.GetTop() >= 2 {
			if tbl, ok := L.Get(2).(*lua.LTable); ok {
				dataTbl = tbl
			}
		}
		result, err := parseTemplate(tmplStr, dataTbl)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(result))
		L.Push(lua.LNil)
		return 2
	}))
	strifeTable.RawSetString("template", templateTable)

	httpModuleTable := L.NewTable()
	httpModuleTable.RawSetString("request", L.NewFunction(func(L *lua.LState) int {
		var urlStr, method string
		var headersTbl *lua.LTable
		var bodyReader io.Reader

		if L.GetTop() == 1 {
			if tbl, ok := L.Get(1).(*lua.LTable); ok {
				if urlVal := tbl.RawGetString("url"); urlVal != lua.LNil {
					urlStr = urlVal.String()
				}
				if methodVal := tbl.RawGetString("method"); methodVal != lua.LNil {
					method = strings.ToUpper(methodVal.String())
				}
				if headersVal := tbl.RawGetString("headers"); headersVal != lua.LNil {
					if ht, ok := headersVal.(*lua.LTable); ok {
						headersTbl = ht
					}
				}
				if bodyVal := tbl.RawGetString("body"); bodyVal != lua.LNil {
					bodyReader = strings.NewReader(bodyVal.String())
				}
			}
		}

		// Fallback to positional arguments if options table wasn't used
		if urlStr == "" {
			urlStr = L.CheckString(1)
			method = "GET"
			if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
				method = strings.ToUpper(L.CheckString(2))
			}
			if L.GetTop() >= 3 && L.Get(3) != lua.LNil {
				if ht, ok := L.Get(3).(*lua.LTable); ok {
					headersTbl = ht
				}
			}
			if L.GetTop() >= 4 && L.Get(4) != lua.LNil {
				bodyReader = strings.NewReader(L.CheckString(4))
			}
		}

		if method == "" {
			method = "GET"
		}

		req, err := http.NewRequestWithContext(r.Context(), method, urlStr, bodyReader)
		if err != nil {
			return pushLuaError(L, err)
		}

		if headersTbl != nil {
			headersTbl.ForEach(func(k, v lua.LValue) {
				req.Header.Set(k.String(), v.String())
			})
		}

		client := &http.Client{
			Timeout: 30 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return pushLuaError(L, err)
		}
		defer resp.Body.Close()

		respBodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return pushLuaError(L, err)
		}

		resTable := L.NewTable()
		resTable.RawSetString("status", lua.LNumber(resp.StatusCode))

		respHeadersTable := L.NewTable()
		for hk, hvals := range resp.Header {
			hValsTbl := L.NewTable()
			for hi, hv := range hvals {
				hValsTbl.RawSetInt(hi+1, lua.LString(hv))
			}
			respHeadersTable.RawSetString(hk, hValsTbl)
		}
		resTable.RawSetString("headers", respHeadersTable)
		resTable.RawSetString("body", lua.LString(string(respBodyBytes)))

		L.Push(resTable)
		L.Push(lua.LNil)
		return 2
	}))

	httpModuleTable.RawSetString("reverse_proxy", L.NewFunction(func(L *lua.LState) int {
		targetStr := L.CheckString(1)
		targetURL, err := url.Parse(targetStr)
		if err != nil {
			return pushLuaError(L, err)
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.ServeHTTP(w, r)

		L.Push(lua.LBool(true))
		L.Push(lua.LNil)
		return 2
	}))

	strifeTable.RawSetString("http", httpModuleTable)

	pathTable := L.NewTable()
	pathTable.RawSetString("segments", L.NewFunction(func(L *lua.LState) int {
		p := L.CheckString(1)
		clean := filepath.Clean(filepath.FromSlash(p))
		if clean == "." || clean == "/" {
			tbl := L.NewTable()
			if clean == "/" {
				tbl.RawSetInt(1, lua.LString("/"))
			}
			L.Push(tbl)
			return 1
		}
		var validParts []string
		for _, part := range strings.Split(clean, "/") {
			if part != "" {
				validParts = append(validParts, part)
			}
		}
		tbl := L.NewTable()
		for i, part := range validParts {
			if i == 0 {
				tbl.RawSetInt(i+1, lua.LString("/"+part))
			} else {
				tbl.RawSetInt(i+1, lua.LString(part))
			}
		}
		L.Push(tbl)
		return 1
	}))
	strifeTable.RawSetString("path", pathTable)

	dbTable := L.NewTable()
	dbTable.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
		if db == nil {
			return pushLuaError(L, fmt.Errorf("Database not initialized!"))
		}
		query := L.CheckString(1)
		var args []interface{}
		top := L.GetTop()
		if top > 1 {
			for i := 2; i <= top; i++ {
				args = append(args, luaValueToGo(L.Get(i)))
			}
		}
		rows, err := db.QueryContext(r.Context(), query, args...)
		if err != nil {
			return pushLuaError(L, err)
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return pushLuaError(L, err)
		}

		resultTable := L.NewTable()
		rowIndex := 1
		for rows.Next() {
			values := make([]interface{}, len(cols))
			valuePtrs := make([]interface{}, len(cols))
			for i := range cols {
				valuePtrs[i] = &values[i]
			}

			if err := rows.Scan(valuePtrs...); err != nil {
				continue
			}

			rowTable := L.NewTable()
			for i, col := range cols {
				val := values[i]
				if val == nil {
					rowTable.RawSetString(col, lua.LNil)
					continue
				}
				switch v := val.(type) {
				case []byte:
					rowTable.RawSetString(col, lua.LString(string(v)))
				case int64:
					rowTable.RawSetString(col, lua.LNumber(v))
				case float64:
					rowTable.RawSetString(col, lua.LNumber(v))
				case string:
					rowTable.RawSetString(col, lua.LString(v))
				case bool:
					rowTable.RawSetString(col, lua.LBool(v))
				default:
					rowTable.RawSetString(col, lua.LString(fmt.Sprintf("%v", v)))
				}
			}
			resultTable.RawSetInt(rowIndex, rowTable)
			rowIndex++
		}

		L.Push(resultTable)
		L.Push(lua.LNil)
		return 2
	}))
	strifeTable.RawSetString("db", dbTable)

	fileTable := L.NewTable()

	fileTable.RawSetString("read", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaError(L, err)
		}
		data, err := os.ReadFile(targetPath)
		if err != nil {
			return pushLuaError(L, err)
		}
		L.Push(lua.LString(string(data)))
		L.Push(lua.LNil)
		return 2
	}))

	fileTable.RawSetString("write", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}

		// Ensure parent directory exists and is a directory; do not create if missing
		parentDir := filepath.Dir(targetPath)
		parentInfo, err := os.Stat(parentDir)
		if err != nil {
			if os.IsNotExist(err) {
				return pushLuaBoolResult(L, false, fmt.Errorf("parent directory does not exist: %s", parentDir))
			}
			return pushLuaBoolResult(L, false, err)
		}
		if !parentInfo.IsDir() {
			return pushLuaBoolResult(L, false, fmt.Errorf("parent path is not a directory: %s", parentDir))
		}

		// Prevent overwriting an existing directory
		if info, err := os.Stat(targetPath); err == nil && info.IsDir() {
			return pushLuaBoolResult(L, false, fmt.Errorf("cannot overwrite directory with file: %s", targetPath))
		}

		content := L.CheckString(2)
		err = os.WriteFile(targetPath, []byte(content), 0644)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	fileTable.RawSetString("move", L.NewFunction(func(L *lua.LState) int {
		sourcePath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}

		destinationPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(2))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}

		// Ensure destination parent directory exists and is a directory; do not create if missing
		destParentDir := filepath.Dir(destinationPath)
		parentInfo, err := os.Stat(destParentDir)
		if err != nil {
			if os.IsNotExist(err) {
				return pushLuaBoolResult(L, false, fmt.Errorf("destination parent directory does not exist: %s", destParentDir))
			}
			return pushLuaBoolResult(L, false, err)
		}
		if !parentInfo.IsDir() {
			return pushLuaBoolResult(L, false, fmt.Errorf("destination parent path is not a directory: %s", destParentDir))
		}

		// Prevent overwriting an existing directory at destination
		if destInfo, err := os.Stat(destinationPath); err == nil && destInfo.IsDir() {
			return pushLuaBoolResult(L, false, fmt.Errorf("cannot overwrite existing directory at destination: %s", destinationPath))
		}

		err = os.Rename(sourcePath, destinationPath)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	fileTable.RawSetString("list", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaError(L, err)
		}
		entries, err := os.ReadDir(targetPath)
		if err != nil {
			return pushLuaError(L, err)
		}

		resultTable := L.NewTable()
		idx := 1
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") && entry.Name() != "_" {
				continue
			}
			entryTable := L.NewTable()
			entryTable.RawSetString("name", lua.LString(entry.Name()))

			childFullPath := filepath.Join(targetPath, entry.Name())
			hostRel, relErr := toHostRelativePath(hostRootDir, childFullPath)
			if relErr == nil {
				entryTable.RawSetString("path", lua.LString(hostRel))
			} else {
				entryTable.RawSetString("path", lua.LString(""))
			}

			isDir := entry.IsDir()
			var info os.FileInfo
			if inf, infErr := entry.Info(); infErr == nil {
				info = inf
				if info.Mode()&os.ModeSymlink != 0 {
					if evalInfo, evalErr := os.Stat(childFullPath); evalErr == nil {
						isDir = evalInfo.IsDir()
						info = evalInfo
					}
				}
			}

			entryTable.RawSetString("is_dir", lua.LBool(isDir))
			if info != nil {
				entryTable.RawSetString("size", lua.LNumber(info.Size()))
				entryTable.RawSetString("mod_time", lua.LNumber(info.ModTime().Unix()))
			}
			resultTable.RawSetInt(idx, entryTable)
			idx++
		}

		L.Push(resultTable)
		L.Push(lua.LNil)
		return 2
	}))

	strifeTable.RawSetString("file", fileTable)

	jsonTable := L.NewTable()
	jsonTable.RawSetString("encode", L.NewFunction(func(L *lua.LState) int {
		val := L.Get(1)
		goVal := luaValueToGo(val)
		bytes, err := json.Marshal(goVal)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(string(bytes)))
		L.Push(lua.LNil)
		return 2
	}))
	jsonTable.RawSetString("decode", L.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		var goVal interface{}
		if err := json.Unmarshal([]byte(str), &goVal); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(goValueToLua(L, goVal))
		L.Push(lua.LNil)
		return 2
	}))
	strifeTable.RawSetString("json", jsonTable)

	osTable := L.NewTable()
	osTable.RawSetString("exec", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		var args []string

		if L.GetTop() >= 2 {
			if tbl, ok := L.Get(2).(*lua.LTable); ok {
				tbl.ForEach(func(k, v lua.LValue) {
					args = append(args, v.String())
				})
			}
		}

		cmd := exec.CommandContext(r.Context(), name, args...)
		var outbuf, errbuf bytes.Buffer
		cmd.Stdout = &outbuf
		cmd.Stderr = &errbuf

		err := cmd.Run()

		resTable := L.NewTable()
		resTable.RawSetString("stdout", lua.LString(outbuf.String()))
		resTable.RawSetString("stderr", lua.LString(errbuf.String()))

		exitCode := 0
		if err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
					exitCode = status.ExitStatus()
				} else {
					exitCode = 1
				}
			} else {
				exitCode = 1
			}
			resTable.RawSetString("code", lua.LNumber(exitCode))
			L.Push(resTable)
			L.Push(lua.LString(err.Error()))
			return 2
		}

		resTable.RawSetString("code", lua.LNumber(0))
		L.Push(resTable)
		L.Push(lua.LNil)
		return 2
	}))
	strifeTable.RawSetString("os", osTable)

	cryptoTable := L.NewTable()
	cryptoTable.RawSetString("genUUID", L.NewFunction(func(L *lua.LState) int {
		uuid := make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, uuid); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		uuid[6] = (uuid[6] & 0x0f) | 0x40 // Version 4
		uuid[8] = (uuid[8] & 0x3f) | 0x80 // Variant 10xx
		res := fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
		L.Push(lua.LString(res))
		L.Push(lua.LNil)
		return 2
	}))
	strifeTable.RawSetString("crypto", cryptoTable)

	reqTable := L.NewTable()
	reqTable.RawSetString("method", lua.LString(r.Method))
	reqTable.RawSetString("uri", lua.LString(r.RequestURI))
	reqTable.RawSetString("path", lua.LString(r.URL.Path))
	reqTable.RawSetString("host", lua.LString(r.Host))
	reqTable.RawSetString("remote_addr", lua.LString(r.RemoteAddr))

	headersTable := L.NewTable()
	for k, vals := range r.Header {
		luaVals := L.NewTable()
		for i, v := range vals {
			luaVals.RawSetInt(i+1, lua.LString(v))
		}
		headersTable.RawSetString(k, luaVals)
	}
	reqTable.RawSetString("headers", headersTable)

	queryTable := L.NewTable()
	for k, vals := range r.URL.Query() {
		luaVals := L.NewTable()
		for i, v := range vals {
			luaVals.RawSetInt(i+1, lua.LString(v))
		}
		queryTable.RawSetString(k, luaVals)
	}
	reqTable.RawSetString("query", queryTable)

	bodyBytes, _ := io.ReadAll(r.Body)
	reqTable.RawSetString("body", lua.LString(string(bodyBytes)))

	strifeTable.RawSetString("request", reqTable)
	L.SetGlobal("strife", strifeTable)

	if err := L.DoFile(scriptPath); err != nil {
		writeLog("ERROR", "LUA", map[string]string{"error": err.Error(), "path": scriptPath})
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !binaryWritten {
		for k, vals := range headers {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(statusCode)
		w.Write(stdout.Bytes())
	}
}

func findMatchingPath(hostRootDir, requestPath string) (string, bool) {
	cleanPath := filepath.Clean(filepath.FromSlash(requestPath))
	parts := []string{}
	if cleanPath != "." && cleanPath != "/" {
		parts = strings.Split(strings.TrimPrefix(cleanPath, string(filepath.Separator)), string(filepath.Separator))
	}

	var search func(currDir string, idx int) (string, bool)
	search = func(currDir string, idx int) (string, bool) {
		if idx == len(parts) {
			return currDir, true
		}

		part := parts[idx]

		// 1. Try exact match component
		exactPath := filepath.Join(currDir, part)
		if info, err := os.Stat(exactPath); err == nil {
			if resolved, err := filepath.EvalSymlinks(exactPath); err == nil {
				if inf2, err2 := os.Stat(resolved); err2 == nil {
					exactPath = resolved
					info = inf2
				}
			}
			if info.IsDir() {
				if res, ok := search(exactPath, idx+1); ok {
					return res, true
				}
			} else if idx == len(parts)-1 {
				return exactPath, true
			}
		}

		// 2. Try wildcard match component '_'
		wildcardPath := filepath.Join(currDir, "_")
		if info, err := os.Stat(wildcardPath); err == nil {
			if resolved, err := filepath.EvalSymlinks(wildcardPath); err == nil {
				if inf2, err2 := os.Stat(resolved); err2 == nil {
					wildcardPath = resolved
					info = inf2
				}
			}
			if info.IsDir() {
				if res, ok := search(wildcardPath, idx+1); ok {
					return res, true
				}
			}
		}

		return "", false
	}

	return search(hostRootDir, 0)
}

func tryServeIndexOrScript(w http.ResponseWriter, r *http.Request, targetPath string, db *sql.DB, rootDir, host string) bool {
	luaIndex := filepath.Join(targetPath, "index.lua")
	if info, err := os.Stat(luaIndex); err == nil && !info.IsDir() {
		handleLuaScript(w, r, luaIndex, db, rootDir, host)
		return true
	}

	htmlIndex := filepath.Join(targetPath, "index.html")
	if info, err := os.Stat(htmlIndex); err == nil && !info.IsDir() {
		http.ServeFile(w, r, htmlIndex)
		return true
	}
	return false
}

func main() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil || db.Ping() != nil {
		if db != nil {
			db.Close()
		}
		writeLog("ERROR", "DATABASE", map[string]string{"path": ":memory:"})
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	initSQLPath := os.Getenv("STRIFE_SQL")
	if initSQLPath != "" {
		sqlBytes, err := os.ReadFile(initSQLPath)
		if err != nil {
			writeLog("ERROR", "DATABASE", map[string]string{"error": err.Error(), "path": initSQLPath})
		} else {
			if _, err := db.Exec(string(sqlBytes)); err != nil {
				writeLog("ERROR", "DATABASE", map[string]string{"error": err.Error(), "path": initSQLPath})
			}
		}
	}

	port := 8080
	if portEnv := os.Getenv("STRIFE_PORT"); portEnv != "" {
		if p, err := strconv.Atoi(portEnv); err == nil {
			port = p
		}
	}

	rootDir := os.Getenv("STRIFE_ROOT")
	if rootDir == "" {
		rootDir = "."
	}

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := getCleanHost(r)
		dir := filepath.Join(rootDir, host)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, cancelKey, cancel))

		if _, err := os.Stat(dir); os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		hostRootDir, err := filepath.Abs(dir)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		cleanTarget, found := findMatchingPath(hostRootDir, r.URL.Path)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if hasHiddenComponent(hostRootDir, cleanTarget) {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/") {
			if info, err := os.Stat(cleanTarget); err == nil && !info.IsDir() {
				// Fallthrough
			} else {
				if tryServeIndexOrScript(w, r, cleanTarget, db, rootDir, host) {
					return
				}
				if infoDir, err := os.Stat(cleanTarget); err == nil && infoDir.IsDir() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
			}
		}

		if strings.HasSuffix(cleanTarget, ".lua") {
			if info, err := os.Stat(cleanTarget); err == nil && !info.IsDir() {
				handleLuaScript(w, r, cleanTarget, db, rootDir, host)
				return
			}
		}

		info, err := os.Stat(cleanTarget)
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if err == nil && info.IsDir() {
			if tryServeIndexOrScript(w, r, cleanTarget, db, rootDir, host) {
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}

		http.ServeFile(w, r, cleanTarget)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, r)

		writeLog("INFO", "REQUEST", map[string]interface{}{
			"host":       getCleanHost(r),
			"ip_address": r.RemoteAddr,
			"method":     r.Method,
			"ms_elapsed": float64(time.Since(start).Microseconds()) / 1000.0,
			"path":       r.URL.Path,
			"status":     rec.status,
			"user_agent": r.UserAgent(),
		})
	})

	server := &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: handler,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	writeLog("INFO", "SERVER", map[string]interface{}{
		"port": port,
		"root": rootDir,
		"sql":  initSQLPath,
	})

	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			writeLog("ERROR", "SERVER", map[string]string{"error": err.Error()})
			os.Exit(1)
		}
	}()

	sig := <-sigChan

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)

	db.Close()

	writeLog("INFO", "SERVER", map[string]string{"signal": sig.String()})
	os.Exit(0)
}
