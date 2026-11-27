package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GW P15 Q2: a stored definition that names what v1.172.0 removed keeps
// running. The run is given every other tool and not the removed one, the
// model is never offered it, and the run starts with one capability_inert
// event per removed item (gate `removed`), on the live stream and in the
// transcript. The same for an AgentDef version (snake_case JSON) and a
// registered agent (config.AgentDef's Go field names), so both readers of
// stored JSON are crossed.
func TestRunStart_AStoredDefinitionNamingBashRunsWithoutItAndSaysSo(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	_, st, ts := spawnCeilingServer(t, cfg, prov, []tools.Tool{namedStubTool{"Read"}})

	putAgentDef(t, st, "def_shell_v1", "versioned", 1,
		`{"provider":"scripted","model":"stub-model","system_prompt":"v1","tools":["Read","Bash","Bashbox"],"shell_credentials":["cred-name-77ab"]}`, true)
	if err := st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		Name: "registered", CreatedAt: time.Now(),
		Definition: []byte(`{"Provider":"scripted","Model":"stub-model","SystemPrompt":"r","Tools":["Read","Bash"],"ShellCredentials":["cred-name-77ab"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	for agent, want := range map[string][]string{
		"versioned":  {"Bash", "Bashbox", "shell_credentials"},
		"registered": {"Bash", "shell_credentials"},
	} {
		t.Run(agent, func(t *testing.T) {
			prov.mu.Lock()
			prov.reqs = nil
			prov.mu.Unlock()
			run := postAgentRun(t, st, ts, agent)
			if run.Status != store.RunCompleted {
				t.Fatalf("run status = %s, want completed: a removed item must not stop the run", run.Status)
			}
			evs, err := st.GetRunEventsSince(context.Background(), run.ID, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, ev := range evs {
				if ev.Type == "capability_inert" && strings.Contains(string(ev.Payload), `"gate":"removed"`) {
					n++
					if strings.Contains(string(ev.Payload), "cred-name-77ab") {
						t.Errorf("the notice names a credential: %s", ev.Payload)
					}
				}
			}
			if n != len(want) {
				t.Errorf("%d removed notices, want %d (%v)", n, len(want), want)
			}
			for _, item := range want {
				if !hasEvent(evs, "capability_inert", `"tool":"`+item+`"`) {
					t.Errorf("no capability_inert notice for %s", item)
				}
			}
			prov.mu.Lock()
			reqs := append([]providers.Request(nil), prov.reqs...)
			prov.mu.Unlock()
			if len(reqs) == 0 {
				t.Fatal("the model was never called")
			}
			var offered []string
			for _, ts := range reqs[0].Tools {
				offered = append(offered, ts.Name)
			}
			for _, name := range offered {
				if _, removed := config.RemovedTool(name); removed {
					t.Errorf("the model was offered %s: %v", name, offered)
				}
			}
			if !strings.Contains(strings.Join(offered, ","), "Read") {
				t.Errorf("the model was not offered Read, which the definition still grants: %v", offered)
			}
		})
	}
}
