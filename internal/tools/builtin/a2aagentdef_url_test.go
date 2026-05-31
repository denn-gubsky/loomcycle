package builtin

import "testing"

// TestValidateA2AAgentDef_RejectsNonHTTPReachability pins the upfront
// URL-shape gate on model-authorable reachability fields. Regression-grade:
// on the unfixed validator a file:// / hostless agent_card_url passed.
func TestValidateA2AAgentDef_RejectsNonHTTPReachability(t *testing.T) {
	cardCases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"file scheme", "file:///etc/passwd", true},
		{"gopher scheme", "gopher://attacker/", true},
		{"no host", "http://", true},
		{"https ok", "https://peer.example/.well-known/agent-card.json", false},
		{"http ok", "http://peer.example", false},
	}
	for _, c := range cardCases {
		err := validateA2AAgentDef(mergedA2AAgentDef{AgentCardURL: c.url})
		if (err != nil) != c.wantErr {
			t.Errorf("agent_card_url %q: err=%v, wantErr=%v", c.url, err, c.wantErr)
		}
	}

	// REST/JSON-RPC endpoints are HTTP too and get the same gate.
	if err := validateA2AAgentDef(mergedA2AAgentDef{Endpoint: "file:///x", Binding: "rest"}); err == nil {
		t.Error("rest endpoint with file:// scheme was accepted")
	}
	if err := validateA2AAgentDef(mergedA2AAgentDef{Endpoint: "https://peer.example", Binding: "jsonrpc"}); err != nil {
		t.Errorf("valid https jsonrpc endpoint rejected: %v", err)
	}
}
