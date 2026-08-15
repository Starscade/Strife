package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		"details":   details,
		"level":     level,
		"timestamp": time.Now().Format(time.RFC3339),
		"topic":     topic,
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

	evalTarget, err := filepath.EvalSymlinks(cleanTarget)
	if err == nil {
		return evalTarget, nil
	}

	return cleanTarget, nil
}

func handleLuaScript(w http.ResponseWriter, r *http.Request, scriptPath string, db *sql.DB, rootDir, host string) {
	L := lua.NewState()
	defer L.Close()

	var stdout bytes.Buffer
	var statusCode = http.StatusOK
	headers := make(http.Header)

	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		top := L.GetTop()
		var args []string
		for i := 1; i <= top; i++ {
			args = append(args, L.ToString(i))
		}
		stdout.WriteString(strings.Join(args, "\t") + "\n")
		return 0
	}))

	L.SetGlobal("set_status", L.NewFunction(func(L *lua.LState) int {
		statusCode = L.CheckInt(1)
		return 0
	}))

	L.SetGlobal("set_header", L.NewFunction(func(L *lua.LState) int {
		headers.Set(L.CheckString(1), L.CheckString(2))
		return 0
	}))

	L.SetGlobal("db_query", L.NewFunction(func(L *lua.LState) int {
		if db == nil {
			return pushLuaError(L, fmt.Errorf("Database not initialized!"))
		}
		query := L.CheckString(1)
		rows, err := db.QueryContext(r.Context(), query)
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

	L.SetGlobal("read_file", L.NewFunction(func(L *lua.LState) int {
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

	L.SetGlobal("write_file", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}
		content := L.CheckString(2)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return pushLuaBoolResult(L, false, err)
		}
		err = os.WriteFile(targetPath, []byte(content), 0644)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	L.SetGlobal("remove_file", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}
		err = os.Remove(targetPath)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	L.SetGlobal("read_dir", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaError(L, err)
		}
		entries, err := os.ReadDir(targetPath)
		if err != nil {
			return pushLuaError(L, err)
		}

		resultTable := L.NewTable()
		for i, entry := range entries {
			entryTable := L.NewTable()
			entryTable.RawSetString("name", lua.LString(entry.Name()))

			isDir := entry.IsDir()
			var info os.FileInfo
			if inf, infErr := entry.Info(); infErr == nil {
				info = inf
				if info.Mode()&os.ModeSymlink != 0 {
					if evalInfo, evalErr := os.Stat(filepath.Join(targetPath, entry.Name())); evalErr == nil {
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
			resultTable.RawSetInt(i+1, entryTable)
		}

		L.Push(resultTable)
		L.Push(lua.LNil)
		return 2
	}))

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

	L.SetGlobal("REQUEST", reqTable)

	if err := L.DoFile(scriptPath); err != nil {
		writeLog("ERROR", "LUA", map[string]string{"error": err.Error(), "path": scriptPath})
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Lua error: %v", err)))
		return
	}

	for k, vals := range headers {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(statusCode)
	w.Write(stdout.Bytes())
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
	dbPath := os.Getenv("STRIFE_DB")
	if dbPath == "" {
		dbPath = ":memory:"
	} else if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			writeLog("ERROR", "DATABASE", map[string]string{"error": err.Error(), "path": dbPath})
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil || db.Ping() != nil {
		if db != nil {
			db.Close()
		}
		writeLog("ERROR", "DATABASE", map[string]string{"path": dbPath})
		os.Exit(1)
	}
	defer db.Close()

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

		targetPath := filepath.Join(dir, filepath.FromSlash(r.URL.Path))
		if resolved, err := filepath.EvalSymlinks(targetPath); err == nil {
			targetPath = resolved
		}

		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/") {
			if info, err := os.Stat(targetPath); err == nil && !info.IsDir() {
				// Fallthrough to standard file handling below
			} else {
				if tryServeIndexOrScript(w, r, targetPath, db, rootDir, host) {
					return
				}
				if infoDir, err := os.Stat(targetPath); err == nil && infoDir.IsDir() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
			}
		}

		if strings.HasSuffix(targetPath, ".lua") {
			if info, err := os.Stat(targetPath); err == nil && !info.IsDir() {
				handleLuaScript(w, r, targetPath, db, rootDir, host)
				return
			}
		}

		info, err := os.Stat(targetPath)
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if err == nil && info.IsDir() {
			if tryServeIndexOrScript(w, r, targetPath, db, rootDir, host) {
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}

		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
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
		"db":   dbPath,
		"port": port,
		"root": rootDir,
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
