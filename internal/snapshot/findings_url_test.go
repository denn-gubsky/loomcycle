package snapshot

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A literal credential can reach an envelope in more places than a header or
// env map: in a URL a definition is dialed at (userinfo, a credential-shaped
// query parameter), and in a stdio server's command line. Each is reported
// by location, never by value, and still travels as authored.

// urlMarks are the literal credentials the fixture plants, built at runtime
// so the source holds no token-shaped literal. Each is distinct, so a leak
// names which one.
var urlMarks = map[string]string{
	"userinfo-pw": "dpUrlMarkUserinfoPw" + strings.Repeat("u", 8),
	"query":       "dpUrlMarkQueryTok" + strings.Repeat("q", 8),
	"command":     "dpUrlMarkCommandKey" + strings.Repeat("c", 8),
	"args":        "dpUrlMarkArgsTok" + strings.Repeat("a", 8),
	"mem-query":   "dpUrlMarkMemTok" + strings.Repeat("m", 8),
	"doc-user":    "ghp_" + "dpUrlMarkDocUser" + strings.Repeat("D", 24),
	"card-sig":    "dpUrlMarkCardSig" + strings.Repeat("s", 8),
	"endpoint-pw": "dpUrlMarkEndpointPw" + strings.Repeat("e", 8),
}

func assertNoURLMark(t *testing.T, where, text string) {
	t.Helper()
	for name, m := range urlMarks {
		// "dpUrlMark" and "ghp_" are fixture scaffolding; what identifies a
		// value starts after them.
		stem := len("dpUrlMark")
		if strings.HasPrefix(m, "ghp_") {
			stem = len("ghp_dpUrlMark")
		}
		for n := stem + 4; n <= len(m); n++ {
			if strings.Contains(text, m[:n]) {
				t.Errorf("%s carries %q, a prefix of the %s literal", where, m[:n], name)
				break
			}
		}
	}
}

func TestCaptureFindings_ReportsURLUserinfoQueryTokenAndStdioArgs(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	now := time.Now().UTC()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	body := func(v map[string]any) json.RawMessage { return mustJSON(t, v) }

	_, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_url", TenantID: "acme", Name: "leaky-http", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"transport": "http",
			"url": "https://svc:" + urlMarks["userinfo-pw"] + "@peer.example/mcp?page=2&api_key=" + urlMarks["query"]})})
	must(err)
	_, err = src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_stdio", TenantID: "acme", Name: "leaky-stdio", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"transport": "stdio",
			"command": "/opt/peer --api-key=" + urlMarks["command"],
			"args":    []any{"stdio", "--token=" + urlMarks["args"], "${LOOMCYCLE_PEER_ARG}", "--log-level=debug"}})})
	must(err)
	_, err = src.MemoryBackendDefCreate(ctx, store.MemoryBackendDefRow{DefID: "mbd_url", TenantID: "acme", Name: "leaky-mem", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"kind": "remote", "config": map[string]any{"base_url": "https://mem.example/v1?token=" + urlMarks["mem-query"]}})})
	must(err)
	_, err = src.DocumentSourceDefCreate(ctx, store.DocumentSourceDefRow{DefID: "dsd_url", TenantID: "acme", Name: "leaky-docs", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"config": map[string]any{"base_url": "https://" + urlMarks["doc-user"] + "@docs.example/"}})})
	must(err)
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_url", TenantID: "acme", Name: "leaky-peer", Version: 1, CreatedAt: now},
		map[string]any{"agent_card_url": "https://peer.example/.well-known/agent.json?sig=" + urlMarks["card-sig"],
			"endpoint": "https://a2a:" + urlMarks["endpoint-pw"] + "@peer.example/a2a"}, true)

	// Negatives: references and innocent query parameters are not literals.
	// The first URL parses as written, so only stripping its references
	// keeps it quiet; the second is refused by the parser before stripping.
	_, err = src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_clean", TenantID: "acme", Name: "clean-http", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"transport": "http",
			"url": "https://peer.example/mcp?api_key=${LOOMCYCLE_X}&page=2&token=$cred:peer"})})
	must(err)
	_, err = src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_clean_ui", TenantID: "acme", Name: "clean-userinfo", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"transport": "http", "url": "https://u:${LOOMCYCLE_PEER_PW}@peer.example/mcp"})})
	must(err)
	_, err = src.MemoryBackendDefCreate(ctx, store.MemoryBackendDefRow{DefID: "mbd_clean", TenantID: "acme", Name: "clean-mem", Version: 1, CreatedAt: now,
		Definition: body(map[string]any{"kind": "remote", "config": map[string]any{"base_url": "https://user@mem.example/v1?page=2&token="}})})
	must(err)

	c, err := CaptureReport(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("CaptureReport: %v", err)
	}
	for name, m := range urlMarks {
		if !strings.Contains(string(c.JSON), m) {
			t.Errorf("the envelope lacks the %s literal; it must travel as authored", name)
		}
	}

	var doc struct {
		Sections Sections `json:"sections"`
	}
	must(json.Unmarshal(c.JSON, &doc))
	sec := doc.Sections.CaptureFindings
	if sec == nil {
		t.Fatal("no capture_findings section in the envelope")
	}
	var got []string
	for _, f := range sec.Entries {
		got = append(got, f.Section+"|"+f.Name+"|"+f.DefID+"|"+f.Field+"|"+f.Detector)
	}
	sort.Strings(got)
	want := []string{
		"a2a_agent_defs|leaky-peer|aad_url|agent_card_url|url-credential-query",
		"a2a_agent_defs|leaky-peer|aad_url|endpoint|url-userinfo",
		"document_source_defs|leaky-docs|dsd_url|config.base_url|url-userinfo",
		"mcp_server_defs|leaky-http|md_url|url|url-credential-query",
		"mcp_server_defs|leaky-http|md_url|url|url-userinfo",
		"mcp_server_defs|leaky-stdio|md_stdio|args[1]|secret-pattern",
		"mcp_server_defs|leaky-stdio|md_stdio|command|secret-pattern",
		"memory_backend_defs|leaky-mem|mbd_url|config.base_url|url-credential-query",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant exactly:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	secBytes, err := json.Marshal(sec)
	must(err)
	assertNoURLMark(t, "the capture_findings section", string(secBytes))
	warnings := strings.Join(c.Warnings, "\n")
	assertNoURLMark(t, "the capture warnings", warnings)

	// The advice fits the field: a URL credential moves out of the URL; a
	// stdio command line is expanded from ${LOOMCYCLE_*} only.
	urlAdvice, stdioAdvice := 0, 0
	for _, w := range c.Warnings {
		switch {
		case strings.Contains(w, ": url ") || strings.Contains(w, "base_url") || strings.Contains(w, "agent_card_url") || strings.Contains(w, ": endpoint "):
			urlAdvice++
			if !strings.Contains(w, "move it out of the URL") {
				t.Errorf("a URL finding's advice does not say to move it out of the URL: %q", w)
			}
		case strings.Contains(w, "args[1]") || strings.Contains(w, ": command "):
			stdioAdvice++
			if strings.Contains(w, "$cred:") || !strings.Contains(w, "${LOOMCYCLE_*}") {
				t.Errorf("a stdio command-line finding's advice does not fit a stdio server: %q", w)
			}
		}
	}
	if urlAdvice != 6 || stdioAdvice != 2 {
		t.Errorf("advice checked on %d URL and %d stdio warnings, want 6 and 2: %q", urlAdvice, stdioAdvice, c.Warnings)
	}
}

func TestURLDetectors_ReferencesAndInnocentQueriesAreNotFindings(t *testing.T) {
	for _, v := range []string{
		"https://peer.example/mcp?api_key=${LOOMCYCLE_X}",
		"https://peer.example/mcp?page=2",
		"https://peer.example/mcp?token=$cred:peer-token",
		"https://u:${LOOMCYCLE_PW}@peer.example/mcp",
		"https://${LOOMCYCLE_HOST}/mcp",
		"https://user@peer.example/mcp",
		"not a url",
	} {
		if d := urlDetectors(v); len(d) != 0 {
			t.Errorf("urlDetectors(%q) = %v, want none", v, d)
		}
	}
	for v, want := range map[string]string{
		"https://peer.example/mcp?Api_Key=lit": detectorURLCredentialQuery,
		"https://peer.example/mcp?signature=x": detectorURLCredentialQuery,
		"https://user:lit@peer.example/mcp":    detectorURLUserinfo,
		"grpc://user:lit@peer.example:443/a2a": detectorURLUserinfo,
	} {
		if d := urlDetectors(v); len(d) != 1 || d[0] != want {
			t.Errorf("urlDetectors(%q) = %v, want [%s]", v, d, want)
		}
	}
}
