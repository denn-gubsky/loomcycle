package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A literal credential in an MCP server def's stdio env map is reported like
// a literal header value: by location, never by value, and it still travels.

// envMark is a GitHub-token-shaped literal no other fixture spells. Built at
// runtime so the source holds no token-shaped literal.
var envMark = "ghp_" + "Dp1aEnvMark" + strings.Repeat("Z", 30)

// assertNoEnvMark fails when text holds the env literal or any prefix of it
// long enough to identify it (past the shared "ghp_" scheme).
func assertNoEnvMark(t *testing.T, where, text string) {
	t.Helper()
	for n := len("ghp_") + 4; n <= len(envMark); n++ {
		if strings.Contains(text, envMark[:n]) {
			t.Errorf("%s carries %q, a prefix of a literal env value", where, envMark[:n])
			return
		}
	}
}

func TestCapture_LiteralStdioEnvIsReportedByLocationNotValue(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	src, srcClose := newTestStore(t)
	defer srcClose()
	ctx := context.Background()
	_, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID: "mcpdef_env", TenantID: "acme", Name: "gh-stdio", Version: 1, CreatedAt: time.Now().UTC(),
		Definition: json.RawMessage(`{"transport":"stdio","command":"github-mcp","args":["stdio"],"env":{` +
			`"GITHUB_TOKEN":"` + envMark + `",` +
			`"LOG_LEVEL":"debug",` +
			`"API_KEY":"${API_KEY}",` +
			`"SESSION_TOKEN":"$cred:gh-session",` +
			`"APP_TOKEN":"$ghapp:gh-app"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := CaptureReport(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("CaptureReport: %v", err)
	}
	if !strings.Contains(string(c.JSON), envMark) {
		t.Error("the envelope lacks the env literal; it must travel as authored")
	}

	var doc struct {
		Sections Sections `json:"sections"`
	}
	if err := json.Unmarshal(c.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	sec := doc.Sections.CaptureFindings
	if sec == nil {
		t.Fatal("no capture_findings section in the envelope")
	}
	want := "mcp_server_defs|gh-stdio|mcpdef_env|env.GITHUB_TOKEN"
	if got := strings.Join(findingKeys(sec.Entries), "\n"); got != want {
		t.Errorf("findings:\n%s\nwant exactly:\n%s", got, want)
	}
	secBytes, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	assertNoEnvMark(t, "the capture_findings section", string(secBytes))

	if len(c.Warnings) != 1 {
		t.Fatalf("capture warnings = %q, want one", c.Warnings)
	}
	w := c.Warnings[0]
	assertNoEnvMark(t, "the capture warning", w)
	if !strings.Contains(w, "mcp_server_defs gh-stdio (tenant acme) def mcpdef_env: env.GITHUB_TOKEN") {
		t.Errorf("the warning does not name the env entry: %q", w)
	}
	// Nothing resolves a $cred: in a stdio env, so the advice must not offer one.
	if strings.Contains(w, "$cred:") || !strings.Contains(w, "${LOOMCYCLE_*}") {
		t.Errorf("the warning's advice does not fit a stdio env: %q", w)
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(ctx, dst, c.JSON, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	var reEmitted []string
	for _, rw := range res.Warnings {
		if strings.HasPrefix(rw, "capture finding: ") {
			reEmitted = append(reEmitted, strings.TrimPrefix(rw, "capture finding: "))
		}
	}
	if strings.Join(reEmitted, "\n") != w {
		t.Errorf("restore re-emitted %q, want the capture's warning %q", reEmitted, w)
	}
	assertNoEnvMark(t, "the restore warnings", strings.Join(res.Warnings, "\n"))

	if !strings.Contains(logs.String(), "env.GITHUB_TOKEN") {
		t.Errorf("no log line names the env entry: %q", logs.String())
	}
	assertNoEnvMark(t, "the log", logs.String())
}

// A secret-shaped value is reported whatever the env var is called, and the
// walk reaches an env map nested anywhere in a body.
func TestScanLiterals_SecretShapedEnvUnderInnocuousNameIsReported(t *testing.T) {
	body := json.RawMessage(`{"transport":"stdio","command":"x","env":{"UPSTREAM":"sk-proj-` +
		strings.Repeat("Q", 24) + `","MODE":"fast"}}`)
	got := scanLiterals(findingSubject{"mcp_server_defs", "acme", "n", "d"}, "", body)
	if len(got) != 1 || got[0].Field != "env.UPSTREAM" || got[0].Detector != detectorSecretPattern {
		t.Errorf("findings = %+v, want one secret-pattern finding at env.UPSTREAM", got)
	}
}

func TestEnvDetector_FlagsLiteralsNotReferences(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"GITHUB_TOKEN", "opaque", detectorCredentialEnv},
		{"DB_PASSWORD", "hunter2", detectorCredentialEnv},
		{"service_auth", "opaque", detectorCredentialEnv},
		{"Signing_Secret", "opaque", detectorCredentialEnv},
		{"GCP_CREDENTIAL", "opaque", detectorCredentialEnv},
		{"STRIPE_KEY", "opaque", detectorCredentialEnv},
		{"API_KEY", "opaque", detectorSecretPattern},
		{"UPSTREAM", "sk-proj-ABCDEFGHIJKLMNOPQRST", detectorSecretPattern},
		{"MODE", "ghp_" + strings.Repeat("a", 36), detectorSecretPattern},

		{"GITHUB_TOKEN", "${LOOMCYCLE_GITHUB_TOKEN}", ""},
		{"GITHUB_TOKEN", "$cred:gh", ""},
		{"GITHUB_TOKEN", "$ghapp:app", ""},
		{"GITHUB_TOKEN", "", ""},
		{"LOG_LEVEL", "debug", ""},
		{"MONKEY", "banana", ""},
		{"TOKENIZER", "bpe", ""},
		{"KEYBOARD", "us", ""},
	} {
		if got := envDetector(tc.name, tc.value); got != tc.want {
			t.Errorf("envDetector(%q, %q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}
