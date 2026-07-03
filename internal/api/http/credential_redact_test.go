package http

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMaskCredentialCreateValue pins the RFC AR no-leak invariant: a
// CredentialDef create tool-call's plaintext `value` is masked out of the
// persisted transcript, deterministically and independent of the general
// redactor toggle.
func TestMaskCredentialCreateValue(t *testing.T) {
	create := json.RawMessage(`{"op":"create","name":"serper","value":"sk-super-secret"}`)

	for _, name := range []string{"CredentialDef", "credentialdef"} {
		out := maskCredentialCreateValue(name, create)
		if strings.Contains(string(out), "sk-super-secret") {
			t.Errorf("%s: plaintext value survived masking: %s", name, out)
		}
		if !strings.Contains(string(out), "redacted") {
			t.Errorf("%s: expected a redaction marker: %s", name, out)
		}
	}

	// A non-credential tool is never touched, even if it has a value field.
	other := json.RawMessage(`{"value":"keep-me"}`)
	if got := maskCredentialCreateValue("Bash", other); string(got) != string(other) {
		t.Errorf("non-credential tool input mangled: %s", got)
	}

	// get/list/delete carry no secret — left unchanged.
	get := json.RawMessage(`{"op":"get","name":"serper"}`)
	if got := maskCredentialCreateValue("CredentialDef", get); string(got) != string(get) {
		t.Errorf("credential get input mangled: %s", got)
	}

	// Malformed credential input is blanked defensively (never persisted raw).
	bad := json.RawMessage(`{"op":"create","value":"leak`)
	if got := maskCredentialCreateValue("CredentialDef", bad); strings.Contains(string(got), "leak") {
		t.Errorf("malformed credential input not defensively blanked: %s", got)
	}
}
