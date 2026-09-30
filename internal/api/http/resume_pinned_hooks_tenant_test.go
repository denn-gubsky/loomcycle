package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A resumed run fires the HookDef versions it pinned at start, read by def_id.
// The pins travel in the run's record, which a snapshot carries between
// deployments, and a def_id is a handle global across tenants: a pin must name
// a HookDef the lookup could have found — one in the tenant it is made in —
// and that tenant must be the run's own or the shared one, or the run stops as
// if the version were gone.

// pinTenantHooks is a resumable run of a hookless static agent in tenant acme,
// whose hooks come from its record: a HookDef "gate" on run_end, added by the
// caller (resolved in the run's tenant) or by a definition in tenant source.
// acme, globex and the shared tenant each have a "gate", reporting to their
// own endpoint; their def_ids name no tenant.
type pinTenantHooks struct {
	srv                   *Server
	acme, globex, shared  *recordingHook
	acmeID, globexID, sID string
}

func newPinTenantHooks(t *testing.T) *pinTenantHooks {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"worker": {Provider: "scripted", Model: "stub-model", SystemPrompt: "work"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	// A tenant's hook dials through the private-address guard; vouch for the
	// test endpoints, or a globex hook that fired would be blocked unseen.
	cfg.Hooks.PrivateHostAllowlist = []string{"127.0.0.1"}
	srv, _ := makeServer(t, answeringProvider(), cfg)
	f := &pinTenantHooks{srv: srv, acme: newRecordingHook(t, `{}`), globex: newRecordingHook(t, `{}`), shared: newRecordingHook(t, `{}`),
		acmeID: "hdf_7a01", globexID: "hdf_7b02", sID: "hdf_7c03"}
	for _, h := range []struct {
		id, tenant string
		rec        *recordingHook
	}{{f.acmeID, "acme", f.acme}, {f.globexID, "globex", f.globex}, {f.sID, "", f.shared}} {
		ctx := context.Background()
		row, err := srv.store.HookDefCreate(ctx, store.HookDefRow{DefID: h.id, Name: "gate", TenantID: h.tenant,
			Definition: mustJSON(t, webhookDef(hooks.PhaseRunEnd, h.rec.srv.URL))})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.HookDefSetActive(ctx, h.tenant, "gate", row.DefID, ""); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

var gateOnRunEnd = hooks.EventHooks{hooks.PhaseRunEnd: {{Ref: "gate"}}}

// callerPins is a record whose caller added gate, pinned as defs.
func callerPins(defs map[string]string) runConfigRecord {
	return runConfigRecord{Hooks: &hooks.Additions{Hooks: gateOnRunEnd}, PinnedHooks: workerPins(defs)}
}

// sourcedPins is a record whose gate a definition in tenant added, pinned as
// defs.
func sourcedPins(tenant string, defs map[string]string) runConfigRecord {
	return runConfigRecord{
		SourcedHooks: []hooks.SourcedAdditions{{Source: hooks.Source{Owner: "team:t", Tenant: tenant}, Hooks: gateOnRunEnd}},
		PinnedHooks:  workerPins(defs),
	}
}

func workerPins(defs map[string]string) *pinnedHooks {
	// The static agent as a resume resolves it: the operator's own yaml.
	return &pinnedHooks{Agent: agentHooksFingerprint(config.AgentDef{OperatorAuthored: true}), Defs: defs}
}

// resume files a paused acme run with rec, resumes it and waits for want.
func (f *pinTenantHooks) resume(t *testing.T, rec runConfigRecord, want store.RunStatus) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := f.srv.store.CreateSession(ctx, "acme", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_w", UserID: "alice", TenantID: "acme", Model: "stub-model", RunConfig: rec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	pauseMidTurn(t, f.srv, run)
	resumeOne(t, f.srv)
	return waitRunStatus(t, f.srv.store, run.ID, want)
}

func (h *recordingHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func TestResumePausedRuns_APinToAnotherTenantsHookDefStopsTheRunAsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  func(id string) runConfigRecord
		// deleted is the same record shape pinning a version that no longer
		// exists, where rec alone cannot give one; nil means rec.
		deleted func(id string) runConfigRecord
	}{
		// The run's own lookup, answered with globex's version.
		{"own tenant's lookup", func(id string) runConfigRecord {
			return callerPins(map[string]string{"acme/gate@0": id})
		}, nil},
		// The shared tenant's lookup, answered with globex's version.
		{"shared lookup", func(id string) runConfigRecord {
			return callerPins(map[string]string{"acme/gate@0": "", "/gate@0": id})
		}, nil},
		// A record naming globex as the source of a hook, and pinning globex's
		// version for the lookup made there. The row is in the tenant its
		// lookup names; the lookup is in a tenant this run cannot resolve in.
		{"globex source", func(id string) runConfigRecord {
			return sourcedPins("globex", map[string]string{"globex/gate@0": id})
		}, func(id string) runConfigRecord {
			return sourcedPins("", map[string]string{"/gate@0": id})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinTenantHooks(t)
			// The refusal for a pinned version that no longer exists.
			deleted := tc.deleted
			if deleted == nil {
				deleted = tc.rec
			}
			missing := f.resume(t, deleted("hdf_7d04"), store.RunFailed)
			gone := strings.ReplaceAll(missing.ErrorMsg, "hdf_7d04", "<id>")

			run := f.resume(t, tc.rec(f.globexID), store.RunFailed)
			if got := strings.ReplaceAll(run.ErrorMsg, f.globexID, "<id>"); got != gone {
				t.Errorf("refusal = %q, want the deleted-version refusal %q", got, gone)
			}
			if strings.Contains(run.ErrorMsg, "globex") {
				t.Errorf("refusal %q names another tenant", run.ErrorMsg)
			}
			// A hook the set resolved would have fired by the time the run
			// ended; give a stray delivery a moment to arrive before counting.
			time.Sleep(200 * time.Millisecond)
			if n := f.globex.count(); n != 0 {
				t.Errorf("globex's HookDef was called %d time(s) by an acme run", n)
			}
		})
	}
}

func TestResumePausedRuns_APinToItsOwnOrTheSharedHookDefFires(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rec   func(f *pinTenantHooks) runConfigRecord
		fired func(f *pinTenantHooks) *recordingHook
	}{
		{"own tenant's", func(f *pinTenantHooks) runConfigRecord {
			return callerPins(map[string]string{"acme/gate@0": f.acmeID})
		}, func(f *pinTenantHooks) *recordingHook { return f.acme }},
		// acme had no gate when the run started; it fell back to the shared one.
		{"shared", func(f *pinTenantHooks) runConfigRecord {
			return callerPins(map[string]string{"acme/gate@0": "", "/gate@0": f.sID})
		}, func(f *pinTenantHooks) *recordingHook { return f.shared }},
		// A definition of the shared tenant added it.
		{"shared definition's", func(f *pinTenantHooks) runConfigRecord {
			return sourcedPins("", map[string]string{"/gate@0": f.sID})
		}, func(f *pinTenantHooks) *recordingHook { return f.shared }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinTenantHooks(t)
			run := f.resume(t, tc.rec(f), store.RunCompleted)
			hookBody(t, tc.fired(f), hooks.PhaseRunEnd, run.ID)
			if n := f.globex.count(); n != 0 {
				t.Errorf("globex's HookDef was called %d time(s) by an acme run", n)
			}
		})
	}
}
