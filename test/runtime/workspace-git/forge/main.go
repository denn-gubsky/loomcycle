// Command forge is the git forge of test/runtime/workspace-git: git's own
// http-backend behind TLS and Basic auth, recording every request.
//
//	forge -addr 127.0.0.1:18943 -root DIR -cert F -key F -token T [-push-token T] -log F
//
// It serves the bare repositories under -root. A request needs Basic auth
// with -token, or -push-token; a receive-pack (a push) needs -push-token when
// one is set. Each request is logged as one line, "METHOD PATH STATUS"; the
// Authorization header never is. It prints "ready" once it listens.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi" // #nosec G504 -- httpoxy is fixed since Go 1.6.3; this is a test forge on loopback
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18943", "the address to listen on")
	root := flag.String("root", "", "the directory of the bare repositories")
	cert := flag.String("cert", "", "the TLS certificate")
	key := flag.String("key", "", "the TLS key")
	token := flag.String("token", "", "the token a request needs")
	pushToken := flag.String("push-token", "", "the token a push needs; empty: -token")
	logPath := flag.String("log", "", "the request log")
	flag.Parse()
	if err := run(*addr, *root, *cert, *key, *token, *pushToken, *logPath); err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}

func run(addr, root, certFile, keyFile, token, pushToken, logPath string) error {
	if root == "" || token == "" || logPath == "" {
		return fmt.Errorf("-root, -token and -log are required")
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return fmt.Errorf("git --exec-path: %w", err)
	}
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	// #nosec G304 -- the request log is the test script's own file
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	var mu sync.Mutex
	record := func(r *http.Request, status int) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(logf, "%s %s %d\n", r.Method, r.URL.Path, status)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		push := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		allowed := ok && (pass == token || pass == pushToken && pushToken != "")
		if push && pushToken != "" {
			allowed = ok && pass == pushToken
		}
		if !allowed {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			record(r, http.StatusUnauthorized)
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		backend.ServeHTTP(sw, r)
		record(r, sw.status)
	})
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	fmt.Println("ready")
	return srv.ServeTLS(l, "", "")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
