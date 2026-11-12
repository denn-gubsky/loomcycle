package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// settlingConnector settles each child of a batch in turn, a gap apart, the
// way a real join reports them, and records what the batch was asked.
type settlingConnector struct {
	*mockConnector
	gap time.Duration
}

func (c *settlingConnector) SpawnRunBatch(_ context.Context, r connector.BatchSpawnRequest) (connector.BatchSpawnResult, error) {
	res := connector.BatchSpawnResult{Spawned: len(r.Spawns)}
	for i := range r.Spawns {
		time.Sleep(c.gap)
		child := connector.SpawnRunResult{RunID: fmt.Sprintf("r_%d", i), Status: "completed"}
		res.Results = append(res.Results, child)
		if r.OnChildSettled != nil {
			r.OnChildSettled(i, child)
		}
	}
	return res, nil
}

// notes records notifications, and any sent after stopped is set.
type notes struct {
	mu      sync.Mutex
	got     []progressParams
	methods []string
	stopped bool
	late    int
}

func (n *notes) notify(method string, params any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		n.late++
	}
	n.methods = append(n.methods, method)
	if p, ok := params.(progressParams); ok {
		n.got = append(n.got, p)
	}
}

func spawnRunsArgs(n int, mode string) json.RawMessage {
	spawns := make([]string, n)
	for i := range spawns {
		spawns[i] = `{"agent":"a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`
	}
	m := ""
	if mode != "" {
		m = fmt.Sprintf(`,"mode":%q`, mode)
	}
	return json.RawMessage(`{"spawns":[` + strings.Join(spawns, ",") + `]` + m + `}`)
}

// TestSpawnRuns_JoinSendsProgressWhileOpen pins RFC GK D7: with a progress
// token, an open join sends notifications/progress at once, each time a child
// settles and on a heartbeat while nothing settles, with the client's token
// and a progress that increases with every notification; none after the
// result, and the result is unchanged.
func TestSpawnRuns_JoinSendsProgressWhileOpen(t *testing.T) {
	n := &notes{}
	env := &handlerEnv{
		connector:     &settlingConnector{mockConnector: &mockConnector{}, gap: 120 * time.Millisecond},
		notify:        n.notify,
		progressToken: json.RawMessage(`"tok-1"`),
		progressEvery: 25 * time.Millisecond,
	}
	res, err := handleSpawnRuns(context.Background(), env, spawnRunsArgs(3, ""))
	if err != nil || res.IsError {
		t.Fatalf("spawn_runs: err %v result %+v", err, res)
	}
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	time.Sleep(80 * time.Millisecond) // a heartbeat left running would land here

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.late != 0 {
		t.Errorf("%d notifications after the call returned", n.late)
	}
	for _, m := range n.methods {
		if m != "notifications/progress" {
			t.Errorf("unexpected notification %q", m)
		}
	}
	if len(n.got) < 5 { // the opening one, 3 settlements, at least one heartbeat
		t.Fatalf("got %d progress notifications, want at least 5: %+v", len(n.got), n.got)
	}
	for i, p := range n.got {
		if string(p.ProgressToken) != `"tok-1"` {
			t.Errorf("notification %d token %s, want the client's", i, p.ProgressToken)
		}
		if i > 0 && p.Progress <= n.got[i-1].Progress {
			t.Errorf("progress did not increase: %d after %d", p.Progress, n.got[i-1].Progress)
		}
	}
	if first, last := n.got[0].Message, n.got[len(n.got)-1].Message; first != "0 of 3 runs settled" || last != "3 of 3 runs settled" {
		t.Errorf("messages run %q .. %q, want 0 of 3 .. 3 of 3", first, last)
	}
	var body connector.BatchSpawnResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &body); err != nil || len(body.Results) != 3 || body.Results[2].RunID != "r_2" {
		t.Errorf("result changed: %+v (%v)", body, err)
	}
}

// TestSpawnRuns_NoProgressWithoutATokenOrWhenDetached: a client that asked for
// no progress gets none, and a detached batch, which returns at once, neither.
func TestSpawnRuns_NoProgressWithoutATokenOrWhenDetached(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token json.RawMessage
		mode  string
	}{
		{"no token", nil, ""},
		{"detach", json.RawMessage(`7`), "detach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &notes{}
			env := &handlerEnv{
				connector:     &settlingConnector{mockConnector: &mockConnector{}, gap: 30 * time.Millisecond},
				notify:        n.notify,
				progressToken: tc.token,
				progressEvery: 5 * time.Millisecond,
			}
			if _, err := handleSpawnRuns(context.Background(), env, spawnRunsArgs(2, tc.mode)); err != nil {
				t.Fatal(err)
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if len(n.methods) != 0 {
				t.Errorf("got notifications %v, want none", n.methods)
			}
		})
	}
}

func TestProgressTokenOf_AcceptsOnlyAStringOrANumber(t *testing.T) {
	tooLong := `{"name":"spawn_runs","_meta":{"progressToken":"` + strings.Repeat("x", 300) + `"}}`
	for params, want := range map[string]string{
		`{"name":"spawn_runs","_meta":{"progressToken":"abc"}}`: `"abc"`,
		`{"name":"spawn_runs","_meta":{"progressToken":42}}`:    `42`,
		`{"name":"spawn_runs","_meta":{"progressToken":{}}}`:    ``,
		`{"name":"spawn_runs","_meta":{"progressToken":null}}`:  ``,
		`{"name":"spawn_runs"}`:                                 ``,
		tooLong:                                                 ``,
	} {
		if got := string(progressTokenOf(json.RawMessage(params))); got != want {
			t.Errorf("progressTokenOf(%.60s) = %q, want %q", params, got, want)
		}
	}
}
