package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/fence"
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
		{"github-app-installation", `x ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij y`, "ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "y"},
		{"github-oauth", `x gho_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij y`, "gho_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "y"},
		{"github-user-to-server", `x ghu_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij y`, "ghu_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "y"},
		{"github-refresh", `x ghr_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij y`, "ghr_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "y"},
		{"github-fine-grained", `x github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV y`, "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV", "y"},
		{"gitlab", `x glpat-abcdEFGH1234ijklMNOP5678 y`, "glpat-abcdEFGH1234ijklMNOP5678", "y"},
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

// TestRedactor_RegisterMasksDynamicValue is the fix-5 regression: a
// runtime-resolved credential value (RFC AV/AR $cred: / provider-key override)
// registered via Register is masked in a later transcript string — AND is left
// intact BEFORE registration (a value the redactor doesn't hold isn't masked).
// The pre-fix wiring passed register=nil, so a downstream echo of a resolved
// credential persisted in cleartext.
func TestRedactor_RegisterMasksDynamicValue(t *testing.T) {
	// Patterns-only redactor: the raw credential doesn't match any Tier-B shape,
	// and the surrounding text names no secret-shaped key, so it's masked ONLY
	// because Register added it.
	r := New(nil, true)
	const secret = "aa11-bb22-cc33-dd44" // not a recognised secret shape

	before := r.String("the value is " + secret + " today")
	if !strings.Contains(before, secret) {
		t.Fatalf("value should NOT be masked before Register; got %q", before)
	}

	r.Register(secret)
	after := r.String("the value is " + secret + " today")
	if strings.Contains(after, secret) {
		t.Errorf("registered value survived redaction: %q", after)
	}
	if !strings.Contains(after, "[redacted:credential]") {
		t.Errorf("expected the credential marker; got %q", after)
	}
	// Also masked inside a JSON leaf via Bytes.
	b := r.Bytes(json.RawMessage(`{"note":"the value is ` + secret + ` here"}`))
	if strings.Contains(string(b), secret) {
		t.Errorf("registered value survived Bytes redaction: %s", b)
	}
}

// TestRedactor_RegisterEnablesOtherwiseEmpty — an otherwise no-op redactor (no
// env values, patterns off) becomes Enabled once a value is registered, so the
// emit path (which gates on Enabled) actually applies it.
func TestRedactor_RegisterEnablesOtherwiseEmpty(t *testing.T) {
	r := New(nil, false)
	if r.Enabled() {
		t.Fatal("empty redactor should start disabled")
	}
	r.Register("super-secret-value")
	if !r.Enabled() {
		t.Error("redactor should be Enabled after Register")
	}
	if got := r.String("x super-secret-value y"); strings.Contains(got, "super-secret-value") {
		t.Errorf("registered value not masked: %q", got)
	}
}

// TestRedactor_RegisterDedupAndShortIgnored — re-registering the same value is a
// no-op (the set stays bounded), and a too-short value is ignored (won't mask
// noise).
func TestRedactor_RegisterDedupAndShortIgnored(t *testing.T) {
	r := New(nil, false)
	r.Register("abcd-token-value")
	r.Register("abcd-token-value") // dedup — no-op
	if got := len(r.dynamic); got != 1 {
		t.Errorf("dynamic set size = %d, want 1 after duplicate Register", got)
	}
	r.Register("xy") // below minDynamicSecretLen — ignored
	if got := len(r.dynamic); got != 1 {
		t.Errorf("dynamic set size = %d, want 1 (short value ignored)", got)
	}
}

// TestRedactor_RegisterNilNoOp — Register on a nil redactor is a no-op (callers
// hold a nil redactor when redaction is disabled).
func TestRedactor_RegisterNilNoOp(t *testing.T) {
	var nilR *Redactor
	nilR.Register("whatever-value") // must not panic
}

// TestRedactor_RegisterConcurrent asserts concurrent Register + String don't
// race (run under -race). Guards the RWMutex around the dynamic set.
func TestRedactor_RegisterConcurrent(t *testing.T) {
	r := New(nil, true)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			r.Register("secret-value-" + string(rune('A'+i%26)))
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		_ = r.String("some text with secret-value-A embedded")
	}
	<-done
}

// MatchesPattern answers exactly what a patterns-only Redactor would change:
// a value it would mask matches, a value it would leave alone does not.
func TestMatchesPattern_AgreesWithPatternsOnlyRedactor(t *testing.T) {
	r := New(nil, true)
	for _, s := range []string{
		`Authorization: Bearer sk-abc123def456ghi789jkl`,
		`--api-key=sk-proj-abcdefghij1234567890`,
		`token=abcdef`,
		`pat ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok`,
		`AKIAABCDEFGHIJKLMNOP`,
		`stdio`,
		`--log-level=debug`,
		`commit 0123456789abcdef0123456789abcdef01234567`,
		``,
	} {
		if got, want := MatchesPattern(s), r.String(s) != s; got != want {
			t.Errorf("MatchesPattern(%q) = %v, but the redactor changes it: %v", s, got, want)
		}
	}
	if !MatchesPattern(`token=abcdef`) || MatchesPattern(`stdio`) {
		t.Error("MatchesPattern lost its positive or negative case")
	}
}

// The new token prefixes are matched as prefixes of a token, not inside
// ordinary words, and a short tail is left alone.
func TestRedactor_TokenPrefixesDoNotMaskOrdinaryText(t *testing.T) {
	r := New(nil, true)
	for _, in := range []string{
		"the ghost_story file",
		"ghs_tooShort1234",
		"glpat-short",
		"github_pat_tooShort",
		"set ghostwriter_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij0123 aside", // a word that only starts like a prefix
	} {
		if got := r.String(in); got != in {
			t.Errorf("masked ordinary text: %q -> %q", in, got)
		}
	}
}

// TestRedactor_FenceTagsSurviveEveryPattern pins that redaction never deletes
// the tags around an untrusted block. The loop sends untrusted text as
// "<kind>\n<escaped body>\n</kind>" and the redact context plugin runs over
// that text afterwards. A rule whose separator crossed the line break took the
// closing tag as the "value" of a body ending "password=", leaving the block
// open, so the trusted text after it read as part of the block — and likewise
// took an opening tag after trusted text ending "token:".
//
// One case per pattern rule, in rule order, so a rule added later has to say
// what it does at a line end. Each case's line is a true positive the rule
// must still mask inside the fence; dangling is the rule's prefix left at the
// end of the body.
func TestRedactor_FenceTagsSurviveEveryPattern(t *testing.T) {
	cases := []struct{ secret, line, dangling string }{
		{"ghp_realtokenvalue1234567890", "Authorization: token ghp_realtokenvalue1234567890", "Authorization: Bearer"},
		{"sk-proj-abcdefghij1234567890", "key sk-proj-abcdefghij1234567890", "sk-"},
		{"AKIAIOSFODNN7EXAMPLE", "id AKIAIOSFODNN7EXAMPLE", "AKIA"},
		{"xoxb-123456789012-abcdefABCDEF", "slack xoxb-123456789012-abcdefABCDEF", "xoxb-"},
		{"ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "app ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "ghp_"},
		{"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV", "pat github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV", "github_pat_"},
		{"glpat-abcdEFGH1234ijklMNOP5678", "lab glpat-abcdEFGH1234ijklMNOP5678", "glpat-"},
		{"supersecretvalue123", "password=supersecretvalue123", "password="},
	}
	if len(cases) != len(patternRules) {
		t.Fatalf("%d cases for %d pattern rules: add a case for the new rule", len(cases), len(patternRules))
	}
	r := New(nil, true)
	for i, tc := range cases {
		t.Run(tc.secret, func(t *testing.T) {
			if !patternRules[i].re.MatchString(tc.line) {
				t.Fatalf("case %d does not exercise rule %d (%s)", i, i, patternRules[i].re)
			}
			body := fence.EscapeUntrusted(tc.line + "\n" + tc.dangling)
			prompt := "Summarise this token:\n<web_content>\n" + body + "\n</web_content>\nReply to the user only."
			got := r.String(prompt)
			if strings.Contains(got, tc.secret) {
				t.Errorf("secret not masked inside the fence: %q", got)
			}
			if !strings.Contains(got, "token:\n<web_content>\n") {
				t.Errorf("opening tag lost to redaction: %q", got)
			}
			if !strings.HasSuffix(got, "\n</web_content>\nReply to the user only.") {
				t.Errorf("closing tag lost to redaction: %q", got)
			}
		})
	}
}

// A YAML value on the next, indented line is still a secret's value. Allowing
// the match onto an INDENTED next line keeps masking it without letting a rule
// reach a fence tag, which always starts its line.
func TestRedactor_MasksAValueOnTheNextIndentedLine(t *testing.T) {
	r := New(nil, true)
	got := r.String("db:\n  password:\n    hunter2-secret-value\n")
	if strings.Contains(got, "hunter2-secret-value") {
		t.Errorf("an indented next-line value was not masked: %q", got)
	}
	fenced := r.String("<web_content>\npassword:\n</web_content>\nnext")
	if fenced != "<web_content>\npassword:\n</web_content>\nnext" {
		t.Errorf("a column-0 tag after a key was touched: %q", fenced)
	}
}
