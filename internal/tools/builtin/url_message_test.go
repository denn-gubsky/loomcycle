package builtin

import (
	"strings"
	"testing"
)

// The secret parts of the fixture URLs below. A message must hold none of
// them, whole or quoted.
var urlMessageSecrets = []string{"svc", "it's-secret", "hunter 2secret", `pa"ss`, "qtok", "ftok", "gpw"}

func assertNoURLSecret(t *testing.T, where, msg string) {
	t.Helper()
	for _, s := range urlMessageSecrets {
		if strings.Contains(msg, s) || strings.Contains(msg, strings.ReplaceAll(s, `"`, `\"`)) {
			t.Errorf("%s carries %q: %s", where, s, msg)
		}
	}
}

// A runtime-authored base_url at a private IP literal is refused, and the
// refusal is echoed into tool results and snapshot restore warnings. It
// names the endpoint without the userinfo, the query or the fragment — a ' in
// a password is valid userinfo, so %q-quoting the raw URL leaked it past the
// restore warning redactor.
func TestRequirePublicIPLiteral_MessageOmitsUserinfo(t *testing.T) {
	err := requirePublicIPLiteral("config.base_url", "https://svc:it's-secret@10.0.0.1/x?token=qtok#ftok")
	if err == nil {
		t.Fatal("a private IP literal was accepted")
	}
	msg := err.Error()
	assertNoURLSecret(t, "the refusal", msg)
	if !strings.Contains(msg, "10.0.0.1/x") {
		t.Errorf("the refusal no longer names the endpoint: %s", msg)
	}
}

func TestURLValidators_MessagesOmitCredentials(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string // what the message must still say
	}{
		{"unparseable userinfo (space)", requireHTTPURL("endpoint", "https://svc:hunter 2secret@peer.example/x"), "invalid userinfo"},
		{"unparseable userinfo (quote)", requireHTTPURL("endpoint", `https://svc:pa"ss@peer.example/x`), "invalid userinfo"},
		{"no host, credential in path", requireHTTPURL("agent_card_url", "https:///svc:it's-secret@peer.example/x?token=qtok"), "has no host"},
		{"no host, credential opaque", requireHTTPURL("agent_card_url", "https:svc:it's-secret@peer.example"), "has no host"},
		{"grpc private literal", requireSafeGRPCEndpoint("endpoint", "grpc://10.0.0.1:443?token=gpw"), "10.0.0.1"},
	} {
		if c.err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		assertNoURLSecret(t, c.name, c.err.Error())
		if !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s: message lost %q: %s", c.name, c.want, c.err)
		}
	}
}

func TestURLForMessage_KeepsSchemeHostAndPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://peer.example/a2a":                                     "https://peer.example/a2a",
		"https://svc:it's-secret@peer.example:8443/v1?token=qtok#ftok": "https://REDACTED@peer.example:8443/v1",
		"https://ghp_tokenAsUser@github.example/x":                     "https://REDACTED@github.example/x",
	} {
		if got := urlForMessage(in); got != want {
			t.Errorf("urlForMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
