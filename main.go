package main

import (
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
	"strings"
	"syscall"
	"time"

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

func main() {
	port := os.Getenv("STRIFE_PORT")
	if port == "" {
		port = "8080"
	}

	rootDir := os.Getenv("STRIFE_ROOT")
	if rootDir == "" {
		rootDir = "."
	}

	dbPath := os.Getenv("STRIFE_DB")
	if dbPath == "" {
		dbPath = ":memory:"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err == nil {
		defer db.Close()
	}

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		dir := filepath.Join(rootDir, host)

		if r.Method == http.MethodPut {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			cleanPath := filepath.FromSlash(r.URL.Path)
			targetPath := filepath.Join(dir, cleanPath)

			parentDir := filepath.Dir(targetPath)
			if parentInfo, err := os.Stat(parentDir); err != nil || !parentInfo.IsDir() {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			outFile, err := os.Create(targetPath)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer outFile.Close()

			_, err = io.Copy(outFile, r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == http.MethodPost && db != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err == nil {
				query := strings.TrimSpace(string(bodyBytes))
				if query != "" {
					rows, err := db.QueryContext(r.Context(), query)
					if err != nil {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
						return
					}
					defer rows.Close()

					cols, err := rows.Columns()
					if err != nil {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
						return
					}

					results := []map[string]interface{}{}
					for rows.Next() {
						values := make([]interface{}, len(cols))
						valuePtrs := make([]interface{}, len(cols))
						for i := range cols {
							valuePtrs[i] = &values[i]
						}

						if err := rows.Scan(valuePtrs...); err != nil {
							continue
						}

						rowMap := make(map[string]interface{})
						for i, col := range cols {
							val := values[i]
							if b, ok := val.([]byte); ok {
								rowMap[col] = string(b)
							} else {
								rowMap[col] = val
							}
						}
						results = append(results, rowMap)
					}

					if len(results) == 0 {
						w.WriteHeader(http.StatusNoContent)
						return
					}

					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(results)
					return
				}
			}
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, cancelKey, cancel))

		if _, err := os.Stat(dir); os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		cleanPath := filepath.FromSlash(r.URL.Path)
		targetPath := filepath.Join(dir, cleanPath)

		info, err := os.Stat(targetPath)
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if err == nil {
			if info.IsDir() {
				indexPath := filepath.Join(targetPath, "index.html")
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
				fmt.Fprintf(w, "<ul>")
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
				fmt.Fprintf(w, "</ul>")
				return
			}
		} else if strings.HasSuffix(r.URL.Path, "/") {
			indexPath := filepath.Join(targetPath, "index.html")
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
					fmt.Fprintf(w, "<ul>")
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
					fmt.Fprintf(w, "</ul>")
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
		duration := time.Since(start)

		logData := map[string]interface{}{
			"timestamp": time.Now().Format(time.RFC3339),
			"level":     "INFO",
			"topic":     "REQUEST",
			"details": map[string]string{
				"method":   r.Method,
				"path":     r.URL.Path,
				"remote":   r.RemoteAddr,
				"status":   fmt.Sprintf("%d", rec.status),
				"duration": duration.String(),
			},
		}
		logJSON, _ := json.Marshal(logData)
		fmt.Println(string(logJSON))
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: handler,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	logData := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"level":     "INFO",
		"topic":     "SERVER_START",
		"details": map[string]string{
			"port": port,
			"db":   dbPath,
			"root": rootDir,
		},
	}
	logJSON, _ := json.Marshal(logData)
	fmt.Println(string(logJSON))

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errLogData := map[string]interface{}{
				"timestamp": time.Now().Format(time.RFC3339),
				"level":     "ERROR",
				"topic":     "SERVER_STOP",
				"details": map[string]string{
					"error": err.Error(),
				},
			}
			errLogJSON, _ := json.Marshal(errLogData)
			fmt.Println(string(errLogJSON))
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

	stopLogData := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"level":     "INFO",
		"topic":     "SERVER_STOP",
		"details": map[string]string{
			"signal": sig.String(),
		},
	}
	stopLogJSON, _ := json.Marshal(stopLogData)
	fmt.Println(string(stopLogJSON))
	os.Exit(0)
}
