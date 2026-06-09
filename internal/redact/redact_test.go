package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactor_ExactEnvValue is the F32 core: the resolved value of a secret
// env var, inlined anywhere in tool I/O, is masked as [redacted:NAME].
func TestRedactor_ExactEnvValue(t *testing.T) {
	r := New(map[string]string{"LOOMCYCLE_GITEA_TOKEN": "abc123def456ghi789"}, false)

	in := `curl -H "Authorization: token abc123def456ghi789" https://gitea/api`
	got := r.String(in)
	if strings.Contains(got, "abc123def456ghi789") {
		t.Fatalf("secret value not masked: %q", got)
	}
	if !strings.Contains(got, "[redacted:LOOMCYCLE_GITEA_TOKEN]") {
		t.Errorf("expected named redaction marker; got %q", got)
	}
}

// TestRedactor_ShortEnvValueSkipped — a short value (< minSecretLen) is NOT
// masked: too generic, and masking it would be a false positive.
func TestRedactor_ShortEnvValueSkipped(t *testing.T) {
	r := New(map[string]string{"LOOMCYCLE_FLAG": "true"}, false)
	if got := r.String("the flag is true today"); got != "the flag is true today" {
		t.Errorf("short value should not be masked; got %q", got)
	}
}

func TestRedactor_Patterns(t *testing.T) {
	r := New(nil, true) // patterns only, no exact values
	cases := []struct {
		name     string
		in       string
		mustGo   string // substring that must be gone
		mustStay string // substring that must remain (structure preserved)
	}{
		{"auth-token", `Authorization: token ghp_realtokenvalue1234567890`, "ghp_realtokenvalue1234567890", "Authorization: token"},
		{"auth-bearer", `Authorization: Bearer sk-abc123def456ghi789jkl`, "sk-abc123def456ghi789jkl", "Authorization: Bearer"},
		{"sk-key", `using key sk-proj-abcdefghij1234567890 here`, "sk-proj-abcdefghij1234567890", "using key"},
		{"aws-key", `id=AKIAIOSFODNN7EXAMPLE done`, "AKIAIOSFODNN7EXAMPLE", "done"},
		{"slack", `token xoxb-123456789012-abcdefABCDEF here`, "xoxb-123456789012-abcdefABCDEF", "token"},
		{"github-pat", `pat ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok`, "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ok"},
		{"api_key-assign", `API_KEY=supersecretvalue123`, "supersecretvalue123", "API_KEY"},
		{"password-assign", `password: "hunter2hunter2"`, "hunter2hunter2", "password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.String(tc.in)
			if strings.Contains(got, tc.mustGo) {
				t.Errorf("secret not masked: %q (still contains %q)", got, tc.mustGo)
			}
			if !strings.Contains(got, tc.mustStay) {
				t.Errorf("structure not preserved: %q (lost %q)", got, tc.mustStay)
			}
		})
	}
}

// TestRedactor_GitSHANotMasked guards the deliberate FP exclusion: a bare
// 40-hex git commit SHA must survive (we do not match bare 40-hex).
func TestRedactor_GitSHANotMasked(t *testing.T) {
	r := New(nil, true)
	sha := "6c6b0871f0a3e2d4c5b6a7980123456789abcdef"
	in := "commit " + sha + " merged"
	if got := r.String(in); got != in {
		t.Errorf("git SHA must not be masked; got %q", got)
	}
}

// TestRedactor_PatternsDisabled — withPatterns=false applies only Tier A.
func TestRedactor_PatternsDisabled(t *testing.T) {
	r := New(map[string]string{"X_TOKEN": "knownvalue1234"}, false)
	in := `Authorization: Bearer sk-someothertoken12345 and knownvalue1234`
	got := r.String(in)
	if strings.Contains(got, "knownvalue1234") {
		t.Errorf("Tier A value should still be masked: %q", got)
	}
	// The pattern-only secret survives because patterns are off.
	if !strings.Contains(got, "sk-someothertoken12345") {
		t.Errorf("with patterns disabled, sk- key should NOT be masked: %q", got)
	}
}

// TestRedactor_BytesRedactsJSONStringLeaf — a token inlined in a Bash command
// string inside a tool_call input is masked, and the result stays valid JSON.
func TestRedactor_BytesRedactsJSONStringLeaf(t *testing.T) {
	r := New(map[string]string{"LOOMCYCLE_GITEA_TOKEN": "abc123def456ghi789"}, true)
	in := json.RawMessage(`{"command":"curl -H \"Authorization: token abc123def456ghi789\" https://gitea","timeout":30}`)

	out := r.Bytes(in)
	if !json.Valid(out) {
		t.Fatalf("redacted Bytes output is not valid JSON: %s", out)
	}
	if strings.Contains(string(out), "abc123def456ghi789") {
		t.Errorf("secret survived in JSON input: %s", out)
	}
	// Non-secret fields are preserved.
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["timeout"] != float64(30) {
		t.Errorf("non-secret field mangled: %v", decoded["timeout"])
	}
}

// TestRedactor_BytesNonJSONUnchanged — a non-JSON input is returned verbatim
// rather than risk corrupting the persisted event.
func TestRedactor_BytesNonJSONUnchanged(t *testing.T) {
	r := New(map[string]string{"X_TOKEN": "knownvalue1234"}, true)
	in := []byte("not json knownvalue1234")
	if got := r.Bytes(in); string(got) != string(in) {
		t.Errorf("non-JSON Bytes should pass through unchanged; got %q", got)
	}
}

// TestRedactor_NoOp — a nil / no-op redactor leaves input untouched and reports
// Enabled()==false so callers can skip the copy.
func TestRedactor_NoOp(t *testing.T) {
	var nilR *Redactor
	if nilR.Enabled() {
		t.Error("nil redactor should not be Enabled")
	}
	if got := nilR.String("Authorization: token secret"); got != "Authorization: token secret" {
		t.Errorf("nil redactor mutated input: %q", got)
	}

	empty := New(nil, false)
	if empty.Enabled() {
		t.Error("empty redactor (no values, no patterns) should not be Enabled")
	}
	if got := empty.Bytes(json.RawMessage(`{"a":"b"}`)); string(got) != `{"a":"b"}` {
		t.Errorf("empty redactor mutated input: %q", got)
	}
}
