package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// panickingConnector is a connector whose SpawnRun panics.
type panickingConnector struct{ *mockConnector }

func (panickingConnector) SpawnRun(context.Context, connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	panic("connector bug")
}

// A tools/call whose handler panics is answered with a JSON-RPC error, so the
// client neither loses the server nor waits out its own timeout.
func TestServer_ToolsCallThatPanicsIsAnsweredWithAnError(t *testing.T) {
	srv := New(Config{Connector: panickingConnector{&mockConnector{}}, Logger: discardLogger()})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa-agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (init + the failed call)", len(resps))
	}
	if resps[1].ID != 2 || resps[1].Error == nil {
		t.Fatalf("call response = %+v, want a JSON-RPC error for id 2", resps[1])
	}
}
