package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestRedactURLSecrets_RemovesUserinfoQueryAndFragment(t *testing.T) {
	cases := []struct{ in, want string }{
		{`config.base_url "https://user:s3cret@peer.example/v1?token=abc&x=1#frag" is bad`,
			`config.base_url "https://REDACTED@peer.example/v1?REDACTED" is bad`},
		{`parse "https://u:p@h/%zz": invalid URL escape "%zz"`, `parse "https://REDACTED@h/%zz": invalid URL escape "%zz"`},
		{`endpoint "dns:///admin:pw@10.0.0.1:443" is private`, `endpoint "dns://REDACTED@10.0.0.1:443" is private`},
		{`a http://host/path#access_token=t and https://other/?sig=s`, `a http://host/path#REDACTED and https://other/?REDACTED`},
		// Nothing to redact: the scheme, host and path stay as written.
		{`endpoint "https://peer.example/a2a" is fine`, `endpoint "https://peer.example/a2a" is fine`},
		{`no url here: user:pass@host`, `no url here: user:pass@host`},
	}
	for _, c := range cases {
		if got := redactURLSecrets(c.in); got != c.want {
			t.Errorf("redactURLSecrets(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// A URL a validator refuses is often refused BECAUSE of a space or a quote in
// it, and a %q-quoted URL (a *url.Error, a validator's own message) ends at
// the closing quote, not at that character. The whole quoted URL is
// redacted, so no tail of a password survives.
func TestRedactURLSecrets_QuotedURLWithQuoteOrSpaceInUserinfo(t *testing.T) {
	for _, c := range []struct{ raw, want, secret string }{
		{"https://svc:hunter 2secret@evil.example/x", "https://REDACTED@evil.example/x", "2secret"},
		{`https://svc:pa"ss@evil.example/x`, "https://REDACTED@evil.example/x", "ss@"},
		{"https://svc:it's-secret@10.0.0.1/x?token=t0k", "https://REDACTED@10.0.0.1/x?REDACTED", "s-secret"},
	} {
		for _, msg := range []string{
			fmt.Sprintf("config.base_url %q is a private address", c.raw),
			(&url.Error{Op: "parse", URL: c.raw, Err: errors.New("net/url: invalid userinfo")}).Error(),
		} {
			got := redactURLSecrets(msg)
			if strings.Contains(got, c.secret) || strings.Contains(got, "svc") || strings.Contains(got, "t0k") {
				t.Errorf("redactURLSecrets(%s)\n left a credential: %s", msg, got)
			}
			if !strings.Contains(got, strconv.Quote(c.want)) {
				t.Errorf("redactURLSecrets(%s)\n = %s\n want it to quote %q", msg, got, c.want)
			}
		}
	}
}

// A validator's refusal quotes the refused value. The restore warning that
// carries it must not carry a URL's credential — for the A2A sections that
// shipped before the redaction as much as for the new ones.
func TestRestore_WarningsNeverCarryURLCredentials(t *testing.T) {
	const pass, tok = "dp4-userinfo-password", "dp4-query-token"
	bad := "https://dp4user:" + pass + "@peer.example/%zz?token=" + tok
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_leak", Name: "peer", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"agent_card_url": bad}, true)

	v := passValidators()
	// What requireHTTPURL does on a parse error: quote the raw value.
	v["a2a_agent_defs"] = func(body json.RawMessage) error { return &echoErr{string(body)} }
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: v})

	refused := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, pass) || strings.Contains(w, tok) {
			t.Errorf("a warning carries a URL credential: %s", w)
		}
		if strings.Contains(w, "not restored") && strings.Contains(w, "peer.example") {
			refused++
		}
	}
	// Non-vacuity: the refusal quoted the URL (host kept), so the absence
	// above is the redaction's doing, not a warning that never named it.
	if refused != 1 {
		t.Errorf("want 1 refusal quoting the redacted URL, got %d: %v", refused, res.Warnings)
	}
}

type echoErr struct{ body string }

func (e *echoErr) Error() string { return "refused " + e.body }
