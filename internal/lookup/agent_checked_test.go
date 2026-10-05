package lookup

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// tierStore answers each dynamic tier with a fixed error, or a row.
type tierStore struct {
	dynErr, defErr error
	def            store.AgentDefRow
}

func (s tierStore) DynamicAgentGet(context.Context, string, string) (store.DynamicAgent, error) {
	return store.DynamicAgent{}, s.dynErr
}

func (s tierStore) AgentDefGetActive(context.Context, string, string) (store.AgentDefRow, error) {
	return s.def, s.defErr
}

// AgentChecked resolves what Agent resolves, and differs in one thing: a store
// fault is an error, where Agent reads it as "no such agent". Agent itself
// must keep answering as it always has.
func TestAgentChecked_ReportsAStoreFaultThatAgentReadsAsNotFound(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Agents: map[string]config.AgentDef{"static": {Tier: "low"}}}
	notFound := &store.ErrNotFound{Kind: "agent", ID: "x"}
	fault := errors.New("database unavailable")
	row := store.AgentDefRow{DefID: "def_1", Definition: []byte(`{"tier":"low"}`)}

	for _, tc := range []struct {
		name      string
		st        tierStore
		tenant    string
		agent     string
		wantFound bool
		wantErr   bool
		// agentFound is what the unchanged Agent answers for the same input.
		agentFound bool
	}{
		{"absent everywhere", tierStore{dynErr: notFound, defErr: notFound}, "t", "x", false, false, false},
		{"an AgentDef version", tierStore{dynErr: notFound, def: row}, "t", "x", true, false, true},
		{"static", tierStore{dynErr: notFound, defErr: notFound}, "t", "static", true, false, true},
		{"registered-agent tier faults", tierStore{dynErr: fault, defErr: notFound}, "t", "x", false, true, false},
		{"AgentDef tier faults", tierStore{dynErr: notFound, defErr: fault}, "t", "x", false, true, false},
		// Agent falls through a faulting first tier to the second; Checked
		// cannot vouch for a name whose first tier it could not read.
		{"first tier faults, second finds", tierStore{dynErr: fault, def: row}, "t", "x", false, true, true},
		// The tenant tier faults before the static one is ever consulted.
		{"tenant tier faults for a static name", tierStore{dynErr: fault, defErr: notFound}, "t", "static", false, true, true},
		{"shared tenant, static name, faults behind it", tierStore{dynErr: fault, defErr: fault}, "", "static", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := AgentChecked(ctx, tc.st, cfg, tc.tenant, tc.agent)
			if found != tc.wantFound || (err != nil) != tc.wantErr {
				t.Errorf("AgentChecked = found %v, err %v; want found %v, error %v", found, err, tc.wantFound, tc.wantErr)
			}
			if _, got := Agent(ctx, tc.st, cfg, tc.tenant, tc.agent); got != tc.agentFound {
				t.Errorf("Agent = %v, want %v — its behaviour must not have changed", got, tc.agentFound)
			}
		})
	}
}
