package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aclRecorder is a stand-in server that records the one request a verb sends.
type aclRecorder struct {
	method, path, query, auth string
	body                      map[string]any
}

func aclStub(t *testing.T, status int, reply string) (*httptest.Server, *aclRecorder) {
	t.Helper()
	rec := &aclRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.query, rec.auth = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("request body is not a JSON object: %s", raw)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each verb calls the management route its op lives on, with the body the
// route takes.
func TestRunACL_EachVerbCallsItsRoute(t *testing.T) {
	rules := writeTemp(t, "rules.yaml", "rules:\n  - effect: deny\n    actions: [invoke]\n    resources: [\"tool:Bash\"]\n")
	draft := writeTemp(t, "draft.json", `{"acls":[{"name":"d","rules":[{"effect":"allow","actions":["invoke"],"resources":["tool:Bash"]}]}],"requests":[]}`)
	for _, c := range []struct {
		args          []string
		method, path  string
		query         string
		bodyHas       map[string]any
		bodyHasRules  bool
		requestsAdded int
	}{
		{args: []string{"groups", "list"}, method: "GET", path: "/v1/_groups"},
		{args: []string{"groups", "get", "--name", "support"}, method: "GET", path: "/v1/_groups/support"},
		{args: []string{"groups", "create", "--name", "support", "--description", "tier 1"}, method: "POST", path: "/v1/_groups", bodyHas: map[string]any{"name": "support", "description": "tier 1"}},
		{args: []string{"groups", "delete", "--name", "a b"}, method: "DELETE", path: "/v1/_groups/a%20b"},
		{args: []string{"groups", "add-member", "--name", "support", "--kind", "user", "--id", "alice"}, method: "POST", path: "/v1/_groups/support/members", bodyHas: map[string]any{"kind": "user", "id": "alice"}},
		{args: []string{"groups", "remove-member", "--name", "support", "--kind", "user", "--id", "alice"}, method: "DELETE", path: "/v1/_groups/support/members/user/alice"},
		{args: []string{"policies", "list", "--include-retired"}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "list", "include_retired": true}},
		{args: []string{"policies", "names"}, method: "GET", path: "/v1/_acldef/names"},
		{args: []string{"policies", "catalog"}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "catalog"}},
		{args: []string{"policies", "get", "--name", "no-shell"}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "get", "name": "no-shell"}},
		{args: []string{"policies", "retire", "--name", "no-shell"}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "retire", "name": "no-shell"}},
		{args: []string{"policies", "create", "--name", "no-shell", "-f", rules}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "create", "name": "no-shell"}, bodyHasRules: true},
		{args: []string{"policies", "validate", "-f", rules}, method: "POST", path: "/v1/_acldef", bodyHas: map[string]any{"op": "validate"}, bodyHasRules: true},
		{args: []string{"bindings", "list", "--include-retired", "--tenant", "acme"}, method: "GET", path: "/v1/_aclbinding", query: "include_retired=1&tenant=acme"},
		{args: []string{"bindings", "create", "--acl", "no-shell", "--kind", "group", "--id", "support", "--expires-at", "2026-12-31T00:00:00Z"}, method: "POST", path: "/v1/_aclbinding",
			bodyHas: map[string]any{"acl": "no-shell", "subject_kind": "group", "subject_id": "support", "expires_at": "2026-12-31T00:00:00Z"}},
		{args: []string{"bindings", "delete", "--id", "b_1"}, method: "DELETE", path: "/v1/_aclbinding/b_1"},
		{args: []string{"check", "--action", "invoke", "--resource", "tool:Bash", "--subject", "alice"}, method: "POST", path: "/v1/_authz/evaluation",
			bodyHas: map[string]any{"subject": map[string]any{"id": "alice"}, "action": map[string]any{"name": "invoke"}, "resource": map[string]any{"type": "tool", "id": "Bash"}}},
		{args: []string{"explain", "--action", "run", "--resource", "agent"}, method: "POST", path: "/v1/_authz/explain",
			bodyHas: map[string]any{"action": map[string]any{"name": "run"}, "resource": map[string]any{"type": "agent", "id": ""}}},
		{args: []string{"simulate", "-f", draft, "--action", "invoke", "--resource", "tool:Bash"}, method: "POST", path: "/v1/_authz/simulate", requestsAdded: 1},
	} {
		srv, rec := aclStub(t, 200, `{"ok":true}`)
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, c.args...), "--target", srv.URL, "--token", "tok-for-the-test")
		if rc := RunACL(args, &stdout, &stderr); rc != 0 {
			t.Errorf("%v: rc %d, stderr %s", c.args, rc, stderr.String())
			continue
		}
		if rec.method != c.method || rec.path != c.path || rec.query != c.query {
			t.Errorf("%v: %s %s?%s, want %s %s?%s", c.args, rec.method, rec.path, rec.query, c.method, c.path, c.query)
		}
		if rec.auth != "Bearer tok-for-the-test" {
			t.Errorf("%v: Authorization %q", c.args, rec.auth)
		}
		for k, want := range c.bodyHas {
			got, _ := json.Marshal(rec.body[k])
			exp, _ := json.Marshal(want)
			if string(got) != string(exp) {
				t.Errorf("%v: body[%s] = %s, want %s", c.args, k, got, exp)
			}
		}
		if c.bodyHasRules {
			if rs, ok := rec.body["rules"].([]any); !ok || len(rs) != 1 {
				t.Errorf("%v: rules = %v, want the file's one rule", c.args, rec.body["rules"])
			}
		}
		if c.requestsAdded > 0 {
			if rs, _ := rec.body["requests"].([]any); len(rs) != c.requestsAdded {
				t.Errorf("%v: requests = %v", c.args, rec.body["requests"])
			}
			if _, ok := rec.body["acls"]; !ok {
				t.Errorf("%v: the draft's acls were not sent: %v", c.args, rec.body)
			}
		}
		if strings.TrimSpace(stdout.String()) != `{"ok":true}` {
			t.Errorf("%v: stdout %q, want the server's answer", c.args, stdout.String())
		}
	}
}

// A refusal prints the server's error and exits 2 for a 4xx; usage errors
// exit 2 without a call; and the bearer never appears in what is printed.
func TestRunACL_RefusalsAndUsageNeverPrintTheBearer(t *testing.T) {
	const bearer = "lct_secret_bearer_for_the_test"
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", bearer)
	srv, _ := aclStub(t, 403, `{"code":"insufficient_scope","error":"groups, access-control lists and bindings are the tenant operator's"}`)
	var stdout, stderr bytes.Buffer
	if rc := RunACL([]string{"groups", "list", "--target", srv.URL}, &stdout, &stderr); rc != 2 {
		t.Errorf("a 403: rc %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "tenant operator") {
		t.Errorf("the refusal was not printed: %q", stderr.String())
	}
	for _, args := range [][]string{
		{"groups", "get"},                     // missing --name
		{"policies", "create", "--name", "x"}, // missing -f
		{"bindings", "create", "--acl", "x", "--kind", "user", "--expires-at", "tomorrow"},
		{"check", "--action", "run"},
		{"groups", "frobnicate"},
		{"nouns"},
		{"groups", "list", "-h"},
	} {
		stdout.Reset()
		stderr.Reset()
		if rc := RunACL(args, &stdout, &stderr); rc != 2 {
			t.Errorf("%v: rc %d, want 2", args, rc)
		}
		if strings.Contains(stdout.String()+stderr.String(), bearer) {
			t.Errorf("%v printed the bearer", args)
		}
	}
}
