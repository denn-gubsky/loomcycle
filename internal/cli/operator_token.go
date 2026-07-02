package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RFC L OSS multi-tenant authorization — the `loomcycle operator-token`
// CLI. A thin HTTP client to POST /v1/_operatortokendef (and GET
// /v1/_operatortokendef/names), mirroring the pause/snapshot CLIs.
//
//	loomcycle operator-token create --tenant T [--subject S] [--scopes a,b] --name N
//	loomcycle operator-token rotate (--name N | --def-id D) [--grace-seconds N]
//	loomcycle operator-token retire (--name N | --def-id D)
//	loomcycle operator-token show   --def-id D
//	loomcycle operator-token list   [--name N]
//
// --target / --token default to $LOOMCYCLE_BASE_URL / $LOOMCYCLE_AUTH_TOKEN.
// Exit codes match the other admin CLIs (0 ok / 1 operational / 2 usage).

// RunOperatorToken dispatches the operator-token verbs. args[0] is the
// verb; the rest are flags.
func RunOperatorToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "loomcycle: error: usage: loomcycle operator-token <create|rotate|retire|show|list> ...")
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "create":
		return runOperatorTokenCreate(rest, stdout, stderr)
	case "rotate":
		return runOperatorTokenMutate(rest, stdout, stderr, "rotate")
	case "retire":
		return runOperatorTokenMutate(rest, stdout, stderr, "retire")
	case "show":
		return runOperatorTokenShow(rest, stdout, stderr)
	case "list":
		return runOperatorTokenList(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "loomcycle: error: unknown operator-token verb %q (want: create / rotate / retire / show / list)\n", verb)
		return 2
	}
}

func runOperatorTokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("operator-token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := registerTokenFlag(fs)
	name := fs.String("name", "", "token name (required)")
	tenant := fs.String("tenant", "", "authoritative tenant_id (required)")
	subject := fs.String("subject", "", "authoritative subject (default tok-<name>)")
	scopes := fs.String("scopes", "", "comma-separated scopes (default substrate:admin)")
	copyFromEnv := fs.Bool("copy-from-env", false, "migration: bind the existing $LOOMCYCLE_AUTH_TOKEN instead of minting (zero-disruption upgrade)")
	httpTimeout := fs.Duration("http-timeout", 15*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *tenant == "" {
		fmt.Fprintln(stderr, "loomcycle: error: --name and --tenant are required")
		return 2
	}
	payload := map[string]any{"op": "create", "name": *name, "tenant_id": *tenant}
	if *subject != "" {
		payload["subject"] = *subject
	}
	if *scopes != "" {
		payload["scopes"] = splitCSV(*scopes)
	}
	if *copyFromEnv {
		env := getenvDefault("LOOMCYCLE_AUTH_TOKEN", "")
		if env == "" {
			fmt.Fprintln(stderr, "loomcycle: error: --copy-from-env set but $LOOMCYCLE_AUTH_TOKEN is empty")
			return 2
		}
		payload["import_token"] = env
	}
	return postOperatorToken(*target, *token, payload, *httpTimeout, stdout, stderr)
}

func runOperatorTokenMutate(args []string, stdout, stderr io.Writer, op string) int {
	fs := flag.NewFlagSet("operator-token "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := registerTokenFlag(fs)
	name := fs.String("name", "", "token name (the name's current token)")
	defID := fs.String("def-id", "", "specific def_id")
	graceSeconds := fs.Int("grace-seconds", -1, "rotate: grace window in seconds (default server 24h)")
	httpTimeout := fs.Duration("http-timeout", 15*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" && *defID == "" {
		fmt.Fprintf(stderr, "loomcycle: error: %s needs --name or --def-id\n", op)
		return 2
	}
	payload := map[string]any{"op": op}
	if *name != "" {
		payload["name"] = *name
	}
	if *defID != "" {
		payload["def_id"] = *defID
	}
	if op == "rotate" && *graceSeconds >= 0 {
		payload["grace_seconds"] = *graceSeconds
	}
	return postOperatorToken(*target, *token, payload, *httpTimeout, stdout, stderr)
}

func runOperatorTokenShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("operator-token show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := registerTokenFlag(fs)
	defID := fs.String("def-id", "", "def_id (required)")
	httpTimeout := fs.Duration("http-timeout", 15*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *defID == "" {
		fmt.Fprintln(stderr, "loomcycle: error: --def-id is required")
		return 2
	}
	return postOperatorToken(*target, *token, map[string]any{"op": "get", "def_id": *defID}, *httpTimeout, stdout, stderr)
}

func runOperatorTokenList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("operator-token list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := registerTokenFlag(fs)
	name := fs.String("name", "", "list one name's token history (omit to list all names)")
	httpTimeout := fs.Duration("http-timeout", 15*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		// List all names via the GET endpoint (no secret material).
		url := strings.TrimRight(*target, "/") + "/v1/_operatortokendef/names"
		rc, resp, err := doAdminRequest(http.MethodGet, url, *token, nil, *httpTimeout)
		if err != nil {
			return failOp(stderr, "GET %s: %v", url, err)
		}
		if rc != 0 {
			return failPrintingBody(stderr, url, resp, rc)
		}
		stdout.Write(append(resp, '\n'))
		return 0
	}
	return postOperatorToken(*target, *token, map[string]any{"op": "list", "name": *name}, *httpTimeout, stdout, stderr)
}

// postOperatorToken POSTs the tool input to /v1/_operatortokendef and
// prints the response body verbatim (it is already JSON). On create/
// rotate the body carries the show-once token plaintext.
func postOperatorToken(target, token string, payload map[string]any, timeout time.Duration, stdout, stderr io.Writer) int {
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(target, "/") + "/v1/_operatortokendef"
	rc, resp, err := doAdminRequest(http.MethodPost, url, token, body, timeout)
	if err != nil {
		return failOp(stderr, "POST %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	stdout.Write(append(resp, '\n'))
	return 0
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
