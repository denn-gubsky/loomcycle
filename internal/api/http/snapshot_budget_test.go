package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// A restore brings budgets and month-to-date usage with it, and they bind at
// once: the refresh pushes them into the tracker before the restored paused
// runs resume, on every restore call site.

// restoreOnEveryCallSite restores envelope through the named transport.
var restoreOnEveryCallSite = []struct {
	name    string
	restore func(t *testing.T, srv *Server, envelope []byte) map[string]int
}{
	{"connector", func(t *testing.T, srv *Server, envelope []byte) map[string]int {
		res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: envelope})
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		return res.Restored
	}},
	{"http", func(t *testing.T, srv *Server, envelope []byte) map[string]int {
		body, _ := json.Marshal(map[string]any{"json": json.RawMessage(envelope)})
		req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
		req.SetPathValue("id", "inline")
		rec := httptest.NewRecorder()
		srv.handleRestoreSnapshot(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp snapshotRestoreResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Restored
	}},
}

// seedBudget gives tenant acme a hard budget and user alice month-to-date
// usage on a source store.
func seedBudget(t *testing.T, st store.Store, hard int64, used int) {
	t.Helper()
	ctx := context.Background()
	if err := st.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(hard), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{
		RunID: "src_run", TenantID: "acme", UserID: "alice", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: used, TS: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A paused run resumed by the restore that brought its tenant's budget is
// bound by that budget: its first call crosses the restored hard ceiling on
// top of the restored usage, and the run records the crossing. Without the
// refresh the tracker holds neither, and the run spends past the budget
// silently.
func TestRestore_TheRunItResumesIsBoundByTheRestoredBudget(t *testing.T) {
	for _, tc := range restoreOnEveryCallSite {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
				Agents: map[string]config.AgentDef{
					"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
				},
				Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
			}
			cfg.Env.AuthToken = ""
			prov := &scriptedProvider{defaultS: []providers.Event{
				{Type: providers.EventText, Text: "resumed and done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 10}},
			}}
			srv, _ := makeServer(t, prov, cfg)
			runID, envelope := capturePausedRun(t, store.RunIdentity{AgentID: "a_budget_resume", UserID: "alice", TenantID: "acme", Model: "stub-model"}, "resumer",
				func(st store.Store, runID string) {
					seedBudget(t, st, 1000, 995)
					b, _ := json.Marshal([]loop.PromptSegment{
						{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
					})
					if err := st.AppendEvent(context.Background(), runID, "user_input", b); err != nil {
						t.Fatal(err)
					}
				})

			restored := tc.restore(t, srv, envelope)
			if restored["token_limits"] != 1 || restored["usage_carry"] != 1 || restored["paused_runs_resumed"] != 1 {
				t.Fatalf("restored map = %v, want token_limits, usage_carry and paused_runs_resumed all 1", restored)
			}
			awaitRunCompleted(t, srv.store, runID)

			events, err := srv.store.GetRunEventsSince(context.Background(), runID, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			var hard *providers.LimitInfo
			for _, e := range events {
				if e.Type != "limit" {
					continue
				}
				var ev providers.Event
				if err := json.Unmarshal(e.Payload, &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Limit != nil && ev.Limit.Severity == "hard" {
					hard = ev.Limit
				}
			}
			if hard == nil {
				t.Fatalf("the resumed run crossed 995+10 of a 1000 budget but recorded no hard limit event (events %d)", len(events))
			}
			if hard.Scope != "tenant" || hard.Used != 1005 || hard.Limit != 1000 {
				t.Errorf("limit event = %+v, want tenant 1005 of 1000", hard)
			}
			if dec := srv.limits.Check("acme", "alice"); dec.Allowed {
				t.Error("a new run for acme/alice is still admitted after the resumed run spent past the budget")
			}
		})
	}
}

// A tenant that had spent its whole budget on the source is refused a new run
// on the target straight after the restore, before it has spent anything
// there — and still after a restart, which re-seeds the tracker from the
// stored carry rather than from anything the restore left in memory.
func TestRestore_ASpentBudgetRefusesANewRunRightAwayAndAfterARestart(t *testing.T) {
	for _, tc := range restoreOnEveryCallSite {
		t.Run(tc.name, func(t *testing.T) {
			src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			seedBudget(t, src, 1000, 1000)
			_, envelope, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}

			srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
			ts := httptest.NewServer(srv.Mux())
			defer ts.Close()
			postRun := func() (int, string) {
				resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
					`{"agent":"default","tenant_id":"acme","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}

			tc.restore(t, srv, envelope)
			if code, body := postRun(); code != http.StatusTooManyRequests || !strings.Contains(body, "token_limit_exceeded") {
				t.Fatalf("a run right after the restore: %d %s; want 429 token_limit_exceeded", code, body)
			}

			// A restart: a fresh tracker seeded from the store alone.
			srv.limits = limits.New(srv.store)
			if err := srv.SeedLimits(context.Background()); err != nil {
				t.Fatal(err)
			}
			if code, body := postRun(); code != http.StatusTooManyRequests {
				t.Fatalf("a run after a restart: %d %s; want 429 (Seed must count the stored carry)", code, body)
			}
		})
	}
}

// Carried usage is budget state only: the usage report — what an operator
// bills from — reads the same before and after a restore that carried usage.
func TestRestore_CarriedUsageIsNotInTheUsageReport(t *testing.T) {
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	seedBudget(t, src, 1000, 900)
	_, envelope, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	if err := srv.store.RecordCallUsage(context.Background(), store.TokenUsageRow{
		RunID: "local_run", TenantID: "acme", UserID: "alice", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 7, TS: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	report := func() string {
		resp, err := http.Get(ts.URL + "/v1/_usage?group_by=tenant,user")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/_usage: %d %s", resp.StatusCode, b)
		}
		return string(b)
	}

	before := report()
	restored := restoreOnEveryCallSite[1].restore(t, srv, envelope)
	if restored["usage_carry"] != 1 {
		t.Fatalf("restored map = %v; nothing was carried, so the report check proves nothing", restored)
	}
	if used := srv.limits.UsedFor("user", "acme", "alice"); used != 907 {
		t.Errorf("the tracker counts %d for acme/alice, want 907 (7 local + 900 carried)", used)
	}
	if after := report(); after != before {
		t.Errorf("GET /v1/_usage changed across the restore:\nbefore %s\nafter  %s", before, after)
	}
}
