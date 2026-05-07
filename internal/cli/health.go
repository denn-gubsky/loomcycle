package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RunHealth GETs /healthz against a running loomcycle instance and
// reports the result. Exits 0 if the response was 200 OK with a
// recognisable {"ok":true} body; non-zero otherwise.
//
// Used as a smoke test in deployment pipelines and as the operator's
// "is the sidecar up?" sanity check.
func RunHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultHealthTarget(), "loomcycle base URL (default: $LOOMCYCLE_BASE_URL or http://127.0.0.1:8787)")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	url := strings.TrimRight(*target, "/") + "/healthz"
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// Malformed --target — user error.
		return fail(stderr, "build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Network failure — operational.
		return failOp(stderr, "GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyStr := strings.TrimSpace(string(body))

	fmt.Fprintf(stdout, "GET %s -> %d\n", url, resp.StatusCode)
	if bodyStr != "" {
		fmt.Fprintf(stdout, "%s\n", bodyStr)
	}

	if resp.StatusCode != http.StatusOK {
		// Server reachable but unhealthy — operational.
		return failOp(stderr, "non-OK status: %d", resp.StatusCode)
	}
	return 0
}

func defaultHealthTarget() string {
	return getenvDefault("LOOMCYCLE_BASE_URL", "http://127.0.0.1:8787")
}
