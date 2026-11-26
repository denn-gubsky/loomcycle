package http

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot"
)

// A restored workspace row is refused when an open here could not have
// written it (RFC GW P12).
func TestValidRestoredWorkspace_RefusesWhatAnOpenCouldNotWrite(t *testing.T) {
	good := snapshot.WorkspaceEntry{ID: "ws_0123456789abcdef01234567", Name: "build", Kind: "general", Engine: "host"}
	b, _ := json.Marshal(good)
	if err := validRestoredWorkspace(b); err != nil {
		t.Fatalf("a good row: %v", err)
	}
	for name, mut := range map[string]func(*snapshot.WorkspaceEntry){
		"name with a slash": func(e *snapshot.WorkspaceEntry) { e.Name = "../x" },
		"name with an at":   func(e *snapshot.WorkspaceEntry) { e.Name = "w@a" },
		"id not an id":      func(e *snapshot.WorkspaceEntry) { e.ID = "../../etc" },
		"unknown kind":      func(e *snapshot.WorkspaceEntry) { e.Kind = "vm" },
		"unknown engine":    func(e *snapshot.WorkspaceEntry) { e.Engine = "kvm" },
	} {
		e := good
		mut(&e)
		b, _ := json.Marshal(e)
		if err := validRestoredWorkspace(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
