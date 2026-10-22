package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The canary suite. Each scenario runs a no-trace tree whose every part
// carries one unique string, on each store backend, and then looks for that
// string everywhere the run could have left it: every table of the database,
// the database's own files, and the process log. It must be in none.
//
// Each scenario is also run in the default mode, where the string MUST be
// found: a search that cannot find a stored string proves nothing.

// canaryBackend is one store the suite runs on.
type canaryBackend struct {
	name string
	// open returns a fresh store and a search of everything it holds: the
	// places the canary was found, empty when it is nowhere.
	open func(t *testing.T) (store.Store, func(t *testing.T) []string)
}

func canaryBackends() []canaryBackend {
	return []canaryBackend{
		{"sqlite", func(t *testing.T) (store.Store, func(t *testing.T) []string) {
			path := filepath.Join(t.TempDir(), "canary.db")
			st, err := storesqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st, func(t *testing.T) []string {
				// As bytes, so a table nobody thought of counts, and so does
				// a row that was written and deleted but is still in the log.
				var found []string
				for _, p := range []string{path, path + "-wal", path + "-journal"} {
					if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(canary)) {
						found = append(found, filepath.Base(p))
					}
				}
				// By table too, so the control below can say WHERE an
				// ordinary run's content is.
				events, _, err := st.ListEvents(context.Background(), store.EventFilter{}, 1000, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range events {
					if bytes.Contains(ev.Payload, []byte(canary)) {
						found = append(found, "events")
						break
					}
				}
				return found
			}
		}},
		{"postgres", func(t *testing.T) (store.Store, func(t *testing.T) []string) {
			dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
			if dsn == "" {
				t.Skip("LOOMCYCLE_TEST_PG_DSN not set; the postgres half of the canary runs in the CI job \"Go (Postgres tiers)\"")
			}
			ctx := context.Background()
			schema := fmt.Sprintf("canary_%d_%d", time.Now().UnixNano()%1_000_000, canarySchemaCounter.Add(1))
			admin, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatalf("dial postgres: %v", err)
			}
			t.Cleanup(func() {
				_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
				admin.Close()
			})
			if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
				t.Fatalf("create schema: %v", err)
			}
			sep := "?"
			if strings.Contains(dsn, "?") {
				sep = "&"
			}
			st, err := postgres.Open(ctx, postgres.Config{DSN: dsn + sep + "search_path=" + schema + ",public", MaxOpenConns: 8, AutoMigrate: true})
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st, func(t *testing.T) []string {
				tables, err := st.SchemaTables(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var found []string
				for _, table := range tables {
					var n int
					// The whole row as text: every column, of every type. A
					// bytea column prints as hex, so the canary is looked
					// for in that form as well — event payloads are bytea,
					// and a search for the plain string walks straight past
					// them.
					q := fmt.Sprintf(`SELECT count(*) FROM %s.%q x
					                  WHERE x::text LIKE '%%' || $1 || '%%'
					                     OR x::text LIKE '%%' || encode(convert_to($1, 'UTF8'), 'hex') || '%%'`, schema, table)
					if err := admin.QueryRow(ctx, q, canary).Scan(&n); err != nil {
						t.Fatalf("search %s: %v", table, err)
					}
					if n > 0 {
						found = append(found, table)
					}
				}
				return found
			}
		}},
	}
}

var canarySchemaCounter atomic.Int64

// logCapture collects the process log while a scenario runs.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Contains(l.buf.Bytes(), []byte(s))
}

func captureCanaryLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(prev) })
	return c
}

// canaryScenario is one way a run can end. run drives it on srv; it must put
// the canary through the run however it ends.
type canaryScenario struct {
	name     string
	provider func() providers.Provider
	run      func(t *testing.T, srv *Server)
}

func canarySegments() []loop.PromptSegment {
	return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "review " + canary}}}}
}

func canaryScenarios() []canaryScenario {
	post := func(t *testing.T, srv *Server) {
		if code, body := postStrictRun(t, srv, canaryRunBody); code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
	}
	return []canaryScenario{
		{"a parent and its child finish", func() providers.Provider { return canaryTree() }, post},
		{"the provider fails and quotes the prompt", func() providers.Provider {
			return &failingProvider{err: fmt.Errorf("%w: invalid request, prompt was: %s", runner.ErrTokenLimitExceeded, canary)}
		}, post},
		{"a tool call fails", func() providers.Provider {
			return &scriptedProvider{scripts: [][]providers.Event{{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "NoSuchTool", Input: json.RawMessage(`{"q":"` + canary + `"}`)}},
				spend("tool_use", 3),
			}}, defaultS: []providers.Event{{Type: providers.EventText, Text: "after the failure " + canary}, spend("end_turn", 1)}}
		}, post},
		{"the child fails under its parent", func() providers.Provider {
			return &scriptedProvider{scripts: [][]providers.Event{
				{{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"judge ` + canary + `"}`)}}, spend("tool_use", 3)},
				{{Type: providers.EventError, Error: "child blew up on " + canary}},
			}, defaultS: []providers.Event{{Type: providers.EventText, Text: "the child failed: " + canary}, spend("end_turn", 1)}}
		}, post},
		{"the run is cancelled in the middle of a model call", func() providers.Provider { return &canaryHangProvider{started: make(chan struct{})} },
			func(t *testing.T, srv *Server) {
				ctx, cancel := context.WithCancel(context.Background())
				prov := srv.providers.(*stubResolver).p.(*canaryHangProvider)
				done := make(chan error, 1)
				go func() {
					done <- srv.RunOnce(ctx, runner.RunInput{Agent: "parent", AgentID: "a_parent", UserID: "alice", Segments: canarySegments()}, runner.RunCallbacks{})
				}()
				select {
				case <-prov.started:
				case <-time.After(5 * time.Second):
					t.Fatal("the model call never started")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("the run did not end after its cancel")
				}
			}},
	}
}

// canaryHangProvider says the canary and then waits for its ctx to end.
type canaryHangProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *canaryHangProvider) ID() string                                   { return "scripted" }
func (p *canaryHangProvider) Probe(context.Context) error                  { return nil }
func (p *canaryHangProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *canaryHangProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *canaryHangProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventText, Text: "partial answer " + canary}
	go func() {
		p.once.Do(func() { close(p.started) })
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func canaryConfig() *config.Config {
	cfg := noTraceConfig()
	// The trace index copies each turn's text into Memory, from memory and
	// not from a stored event. On, so the suite covers it.
	cfg.Env.MemoryTraceIndexEnabled = true
	return cfg
}

func TestNoTraceCanary(t *testing.T) {
	for _, backend := range canaryBackends() {
		for _, sc := range canaryScenarios() {
			for _, noTrace := range []bool{true, false} {
				mode := "default"
				if noTrace {
					mode = "no-trace"
				}
				t.Run(backend.name+"/"+sc.name+"/"+mode, func(t *testing.T) {
					st, stored := backend.open(t)
					logged := captureCanaryLog(t)
					srv := New(canaryConfig(), &stubResolver{p: sc.provider()}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
					if noTrace {
						markNoTrace(srv, "parent")
					}
					sc.run(t, srv)
					// A run's last writes happen as it ends; a detached one's
					// after its caller has returned.
					waitRunsTerminal(t, srv)

					found := stored(t)
					if !noTrace {
						// The control. An ordinary run's content is in its
						// events; a search that does not see it there would
						// not see a no-trace run's either.
						if !containsString(found, "events") {
							t.Fatalf("an ordinary run's canary was found in %v, not in events: this search cannot detect a leak", found)
						}
						return
					}
					if len(found) > 0 {
						t.Errorf("the canary is in the store after a no-trace run: %v", found)
					}
					if logged.has(canary) {
						t.Errorf("the canary is in the process log after a no-trace run:\n%s", logged.buf.String())
					}
					run := runOfAgent(t, srv, "a_parent")
					if run.Retention != retentionmode.None || !store.IsTerminalRunStatus(run.Status) {
						t.Errorf("run row: retention=%q status=%q", run.Retention, run.Status)
					}
				})
			}
		}
	}
}

// waitRunsTerminal waits until no run is still running.
func waitRunsTerminal(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sessions, _, err := srv.store.ListSessions(context.Background(), store.SessionFilter{}, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, sess := range sessions {
			runs, err := srv.store.RunsForSession(context.Background(), sess.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range runs {
				if !store.IsTerminalRunStatus(r.Status) {
					running++
				}
			}
		}
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d run(s) never ended", running)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
