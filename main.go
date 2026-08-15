package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
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

const dirListingTemplateStart = "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>&zwj;</title></head><body><ul>"
const dirListingTemplateEnd = "</ul></body></html>"

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
		key := L.CheckString(1)
		val := L.CheckString(2)
		headers.Set(key, val)
		return 0
	}))

	L.SetGlobal("db_query", L.NewFunction(func(L *lua.LState) int {
		if db == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("Database not initialized!"))
			return 2
		}
		query := L.CheckString(1)
		rows, err := db.QueryContext(r.Context(), query)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
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
				} else if b, ok := val.([]byte); ok {
					rowTable.RawSetString(col, lua.LString(string(b)))
				} else {
					switch v := val.(type) {
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
			}
			resultTable.RawSetInt(rowIndex, rowTable)
			rowIndex++
		}

		L.Push(resultTable)
		L.Push(lua.LNil)
		return 2
	}))

	hostDir := filepath.Join(rootDir, host)

	L.SetGlobal("read_file", L.NewFunction(func(L *lua.LState) int {
		relPath := L.CheckString(1)
		targetPath := filepath.Join(hostDir, filepath.FromSlash(relPath))
		data, err := os.ReadFile(targetPath)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(string(data)))
		L.Push(lua.LNil)
		return 2
	}))

	L.SetGlobal("write_file", L.NewFunction(func(L *lua.LState) int {
		relPath := L.CheckString(1)
		content := L.CheckString(2)
		targetPath := filepath.Join(hostDir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			L.Push(lua.LBool(false))
			L.Push(lua.LString(err.Error()))
			return 2
		}
		if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
			L.Push(lua.LBool(false))
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(true))
		L.Push(lua.LNil)
		return 2
	}))

	L.SetGlobal("remove_file", L.NewFunction(func(L *lua.LState) int {
		relPath := L.CheckString(1)
		targetPath := filepath.Join(hostDir, filepath.FromSlash(relPath))
		if err := os.Remove(targetPath); err != nil {
			L.Push(lua.LBool(false))
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(true))
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

	bodyBytes, err := io.ReadAll(r.Body)
	if err == nil {
		reqTable.RawSetString("body", lua.LString(string(bodyBytes)))
	} else {
		reqTable.RawSetString("body", lua.LString(""))
	}

	L.SetGlobal("REQUEST", reqTable)

	err = L.DoFile(scriptPath)
	if err != nil {
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
	if err != nil {
		writeLog("ERROR", "DATABASE", map[string]string{"error": err.Error(), "path": dbPath})
		os.Exit(1)
	}

	if err := db.Ping(); err != nil {
		writeLog("ERROR", "DATABASE", map[string]string{"error": err.Error(), "path": dbPath})
		db.Close()
		os.Exit(1)
	}

	defer db.Close()

	htmlIndex := os.Getenv("STRIFE_INDEX")
	if htmlIndex == "" {
		htmlIndex = "index.html"
	}

	portEnv := os.Getenv("STRIFE_PORT")
	var port int
	if portEnv == "" {
		port = 8080
	} else {
		port, err = strconv.Atoi(portEnv)
		if err != nil {
			port = 8080
		}
	}

	rootDir := os.Getenv("STRIFE_ROOT")
	if rootDir == "" {
		rootDir = "."
	}

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		dir := filepath.Join(rootDir, host)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, cancelKey, cancel))

		if _, err := os.Stat(dir); os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		cleanPath := filepath.FromSlash(r.URL.Path)
		targetPath := filepath.Join(dir, cleanPath)

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

		if err == nil {
			if info.IsDir() {
				indexPath := filepath.Join(targetPath, htmlIndex)
				if indexInfo, err := os.Stat(indexPath); err == nil && !indexInfo.IsDir() {
					http.ServeFile(w, r, indexPath)
					return
				}

				f, err := os.Open(targetPath)
				if err != nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				defer f.Close()

				names, err := f.Readdirnames(-1)
				if err != nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}

				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, "%s", dirListingTemplateStart)
				if r.URL.Path != "/" && r.URL.Path != "" {
					fmt.Fprintf(w, "<li><a href=\"..\">../</a></li>")
				}
				for _, name := range names {
					urlPath := filepath.ToSlash(filepath.Join(r.URL.Path, name))
					if fi, statErr := os.Stat(filepath.Join(targetPath, name)); statErr == nil && fi.IsDir() {
						urlPath += "/"
					}
					safeName := html.EscapeString(urlPath)
					fmt.Fprintf(w, "<li><a href=\"%s\">%s</a></li>", safeName, safeName)
				}
				fmt.Fprintf(w, "%s", dirListingTemplateEnd)
				return
			}
		} else if strings.HasSuffix(r.URL.Path, "/") {
			indexPath := filepath.Join(targetPath, htmlIndex)
			if indexInfo, err := os.Stat(indexPath); err == nil && !indexInfo.IsDir() {
				http.ServeFile(w, r, indexPath)
				return
			}
		}

		if info, err := os.Stat(targetPath); err == nil && info.IsDir() {
			f, err := os.Open(targetPath)
			if err == nil {
				names, readdirErr := f.Readdirnames(-1)
				f.Close()
				if readdirErr == nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					fmt.Fprintf(w, "%s", dirListingTemplateStart)
					if r.URL.Path != "/" && r.URL.Path != "" {
						fmt.Fprintf(w, "<li><a href=\"..\">../</a></li>")
					}
					for _, name := range names {
						urlPath := filepath.ToSlash(filepath.Join(r.URL.Path, name))
						if fi, statErr := os.Stat(filepath.Join(targetPath, name)); statErr == nil && fi.IsDir() {
							urlPath += "/"
						}
						safeName := html.EscapeString(urlPath)
						fmt.Fprintf(w, "<li><a href=\"%s\">%s</a></li>", safeName, safeName)
					}
					fmt.Fprintf(w, "%s", dirListingTemplateEnd)
					return
				}
			}
		}

		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		msElapsed := float64(time.Since(start).Microseconds()) / 1000.0

		requestHost := r.Host
		if h, _, err := net.SplitHostPort(requestHost); err == nil {
			requestHost = h
		}

		writeLog("INFO", "REQUEST", map[string]interface{}{
			"host":       requestHost,
			"ip_address": r.RemoteAddr,
			"method":     r.Method,
			"ms_elapsed": msElapsed,
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

	writeLog("INFO", "SERVER_START", map[string]interface{}{
		"db":    dbPath,
		"index": htmlIndex,
		"port":  port,
		"root":  rootDir,
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			writeLog("ERROR", "SERVER_STOP", map[string]string{
				"error": err.Error(),
			})
			os.Exit(1)
		}
	}()

	sig := <-sigChan

	if db != nil {
		db.Close()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)

	writeLog("INFO", "SERVER_STOP", map[string]string{
		"signal": sig.String(),
	})
	os.Exit(0)
}
