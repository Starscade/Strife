package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
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

	responseTable := L.NewTable()
	responseTable.RawSetString("status", L.NewFunction(func(L *lua.LState) int {
		statusCode = L.CheckInt(1)
		return 0
	}))
	responseTable.RawSetString("header", L.NewFunction(func(L *lua.LState) int {
		headers.Set(L.CheckString(1), L.CheckString(2))
		return 0
	}))
	strifeTable.RawSetString("response", responseTable)

	dbTable := L.NewTable()
	dbTable.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
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
		content := L.CheckString(2)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return pushLuaBoolResult(L, false, err)
		}
		err = os.WriteFile(targetPath, []byte(content), 0644)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	fileTable.RawSetString("remove", L.NewFunction(func(L *lua.LState) int {
		targetPath, err := resolveHostPath(rootDir, host, scriptPath, L.CheckString(1))
		if err != nil {
			return pushLuaBoolResult(L, false, err)
		}
		err = os.Remove(targetPath)
		return pushLuaBoolResult(L, err == nil, err)
	}))

	fileTable.RawSetString("read_dir", L.NewFunction(func(L *lua.LState) int {
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
			resultTable.RawSetInt(i+1, entryTable)
		}

		L.Push(resultTable)
		L.Push(lua.LNil)
		return 2
	}))

	strifeTable.RawSetString("file", fileTable)

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

func loadOrGenerateCA(rootDir string) (*x509.Certificate, *rsa.PrivateKey, error) {
	keyPath := os.Getenv("STRIFE_TLS")
	certPath := filepath.Join(rootDir, "Strife.crt")
	privKeyPath := filepath.Join(rootDir, "Strife.key")

	var priv *rsa.PrivateKey
	var err error

	// 1. If explicit STRIFE_TLS key path is provided, use it
	if keyPath != "" {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to read STRIFE_TLS key path"})
			return nil, nil, fmt.Errorf("Failed to read STRIFE_TLS key path: %w", err)
		}
		blockKey, _ := pem.Decode(keyPEM)
		if blockKey == nil {
			writeLog("ERROR", "TLS", map[string]string{"msg": "Failed to decode PEM block from STRIFE_TLS key!"})
			return nil, nil, fmt.Errorf("Failed to decode PEM block from STRIFE_TLS key!")
		}
		priv, err = x509.ParsePKCS1PrivateKey(blockKey.Bytes)
		if err != nil {
			parsedKey, parseErr := x509.ParsePKCS8PrivateKey(blockKey.Bytes)
			if parseErr != nil {
				writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to parse private key from STRIFE_TLS"})
				return nil, nil, fmt.Errorf("Failed to parse private key from STRIFE_TLS: %w", err)
			}
			rsaKey, ok := parsedKey.(*rsa.PrivateKey)
			if !ok {
				writeLog("ERROR", "TLS", map[string]string{"msg": "STRIFE_TLS key is not an RSA private key!"})
				return nil, nil, fmt.Errorf("STRIFE_TLS key is not an RSA private key!")
			}
			priv = rsaKey
		}
	} else {
		// Check if we already persisted a CA private key locally
		if _, err := os.Stat(privKeyPath); err == nil {
			if keyPEM, err := os.ReadFile(privKeyPath); err == nil {
				if blockKey, _ := pem.Decode(keyPEM); blockKey != nil {
					if p, err := x509.ParsePKCS1PrivateKey(blockKey.Bytes); err == nil {
						priv = p
					} else if parsedKey, parseErr := x509.ParsePKCS8PrivateKey(blockKey.Bytes); parseErr == nil {
						if rsaKey, ok := parsedKey.(*rsa.PrivateKey); ok {
							priv = rsaKey
						}
					}
				}
			}
		}

		// If no private key found, generate a new one
		if priv == nil {
			priv, err = rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to generate RSA key"})
				return nil, nil, err
			}
			// Save it locally so it remains consistent across restarts
			privFile, err := os.Create(privKeyPath)
			if err == nil {
				pem.Encode(privFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
				privFile.Close()
			}
		}
	}

	// 2. Try loading existing certificate if present
	if _, err := os.Stat(certPath); err == nil {
		if certPEM, err := os.ReadFile(certPath); err == nil {
			if blockCert, _ := pem.Decode(certPEM); blockCert != nil {
				if cert, err := x509.ParseCertificate(blockCert.Bytes); err == nil {
					return cert, priv, nil
				}
			}
		}
	}

	// 3. Otherwise, create a new CA certificate signed with `priv`
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to generate serial number"})
		return nil, nil, err
	}

	tmpl := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "Shinra Inc.",
			Organization: []string{"Shinra Electric Power Company"},
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to create CA certificate"})
		return nil, nil, err
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to parse CA certificate"})
		return nil, nil, err
	}

	certFile, err := os.Create(certPath)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "msg": "Failed to create certificate file"})
		return nil, nil, err
	}
	defer certFile.Close()
	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	return cert, priv, nil
}

func generateHostCert(rootDir, host string, caCert *x509.Certificate, caKey *rsa.PrivateKey) (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "host": host, "msg": "Failed to generate host RSA key"})
		return tls.Certificate{}, err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "host": host, "msg": "Failed to generate host serial number"})
		return tls.Certificate{}, err
	}

	tmpl := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host, "*." + host}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, &priv.PublicKey, caKey)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "host": host, "msg": "Failed to create host certificate"})
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		writeLog("ERROR", "TLS", map[string]string{"error": err.Error(), "host": host, "msg": "Failed to load X509 key pair"})
	}
	return cert, err
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

	caCert, caKey, err := loadOrGenerateCA(rootDir)
	if err != nil {
		writeLog("ERROR", "SERVER", map[string]string{"error": err.Error(), "msg": "Failed to initialize root CA!"})
		os.Exit(1)
	}

	certsMap := make(map[string]*tls.Certificate)
	entries, err := os.ReadDir(rootDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				hostName := entry.Name()
				hostCert, err := generateHostCert(rootDir, hostName, caCert, caKey)
				if err == nil {
					certsMap[hostName] = &hostCert
				}
			}
		}
	}

	server.TLSConfig = &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			serverName := hello.ServerName
			if serverName == "" {
				if hello.Conn != nil && hello.Conn.LocalAddr() != nil {
					if host, _, err := net.SplitHostPort(hello.Conn.LocalAddr().String()); err == nil {
						serverName = host
					}
				}
			}
			if cert, ok := certsMap[serverName]; ok {
				return cert, nil
			}
			if h, _, err := net.SplitHostPort(serverName); err == nil {
				if cert, ok := certsMap[h]; ok {
					return cert, nil
				}
			}
			for _, cert := range certsMap {
				return cert, nil
			}
			certErr := fmt.Errorf("no certificate available for host: %s", serverName)
			writeLog("ERROR", "TLS", map[string]string{"error": certErr.Error(), "server_name": serverName})
			return nil, certErr
		},
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	writeLog("INFO", "SERVER", map[string]interface{}{
		"port": port,
		"root": rootDir,
		"sql":  initSQLPath,
		"tls":  os.Getenv("STRIFE_TLS"),
	})

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
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
