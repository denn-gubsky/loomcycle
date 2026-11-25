package builtin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3, D24, P9 PR 6: the surface and surface_closed events.

type eventLog struct {
	mu  sync.Mutex
	evs []providers.Event
}

func (l *eventLog) add(ev providers.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

func (l *eventLog) ofType(typ providers.EventType) []providers.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []providers.Event
	for _, ev := range l.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// surfaceFixture is an exposing run whose stream is captured, with the
// runtime's notifier wired as boot wires it.
func surfaceFixture(t *testing.T) (*wsFixture, context.Context, *eventLog) {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	f.tool.Manager.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxPerWorkspace: 4, IdleTimeout: 10 * time.Minute})
	f.tool.Manager.SetSurfaceNotifier(SurfaceNotifier(f.st))
	log := &eventLog{}
	ctx := tools.WithEventEmitter(f.runCtx(t, "alice", "", wsExposer), log.add)
	return f, ctx, log
}

// The keys a surface's payload may carry: what the RFC names, nothing else.
func surfaceKeys(t *testing.T, s *providers.SurfaceInfo) []string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expose puts one surface event on the run's own stream: the page's id,
// its workspace's name, title, kind, base path and when it closes if
// unused; nothing else, and no host path or credential anywhere in it.
func TestWorkspaceSurface_ExposeTellsTheRunsStream(t *testing.T) {
	f, ctx, log := surfaceFixture(t)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	before := time.Now()
	res := f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8000,"title":"Chart","kind":"panel"}`)
	var out struct {
		ID string `json:"exposure_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	evs := log.ofType(providers.EventSurface)
	if len(evs) != 1 || evs[0].Surface == nil {
		t.Fatalf("surface events = %+v, want one", evs)
	}
	s := evs[0].Surface
	if s.ExposureID != out.ID || s.Workspace != "web" || s.Title != "Chart" || s.Kind != "panel" ||
		s.BasePath != lifecycle.ExposurePrefix(out.ID) || s.Reason != "" {
		t.Errorf("surface = %+v", s)
	}
	if s.ExpiresAt == nil || s.ExpiresAt.Before(before.Add(9*time.Minute)) || s.ExpiresAt.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("expires_at = %v, want about the idle timeout from now", s.ExpiresAt)
	}
	if got := strings.Join(surfaceKeys(t, s), ","); got != "base_path,expires_at,exposure_id,kind,title,workspace" {
		t.Errorf("surface keys = %s", got)
	}
	b, _ := json.Marshal(evs[0])
	rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(ctx))
	for _, leak := range []string{f.h.Dir(rows[0].ID), "Bearer", "ticket.", rows[0].WorkerID} {
		if leak != "" && strings.Contains(string(b), leak) {
			t.Errorf("the event carries %q: %s", leak, b)
		}
	}
}

// unexpose and the workspace's release each tell the run's stream which
// pages went, and why.
func TestWorkspaceSurface_ClosesAreToldOnTheRunsStream(t *testing.T) {
	f, ctx, log := surfaceFixture(t)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8000}`)
	f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8001}`)
	first := log.ofType(providers.EventSurface)[0].Surface.ExposureID
	f.mustOK(t, ctx, `{"op":"unexpose","name":"web","port":8000}`)
	closed := log.ofType(providers.EventSurfaceClosed)
	if len(closed) != 1 || closed[0].Surface.Reason != lifecycle.SurfaceUnexposed || closed[0].Surface.ExposureID != first {
		t.Fatalf("after unexpose: %+v", closed)
	}
	if got := strings.Join(surfaceKeys(t, closed[0].Surface), ","); got != "exposure_id,reason" {
		t.Errorf("surface_closed keys = %s", got)
	}
	f.mustOK(t, ctx, `{"op":"release","name":"web"}`)
	closed = log.ofType(providers.EventSurfaceClosed)
	if len(closed) != 2 || closed[1].Surface.Reason != lifecycle.SurfaceReleased {
		t.Errorf("after release: %+v, want the other page closed as released", closed)
	}
}

// A page that goes outside the run's own tool call — its run ended, or it
// went idle under the sweeper — is told on the run's transcript, which a
// re-attached stream reads.
func TestWorkspaceSurface_AClosingOutsideTheRunIsInItsTranscript(t *testing.T) {
	f, ctx, _ := surfaceFixture(t)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8000}`)
	runID := tools.RunID(ctx)
	if err := f.tool.Manager.ReleaseRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.st.GetRunEventsSince(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Type != string(providers.EventSurfaceClosed) {
			continue
		}
		var got providers.Event
		if json.Unmarshal(ev.Payload, &got) == nil && got.Surface != nil && got.Surface.Reason == lifecycle.SurfaceReleased {
			found = true
		}
	}
	if !found {
		t.Errorf("the run's transcript has no surface_closed (released): %d rows", len(evs))
	}
}

// A page that went idle — found by the sweeper, or by the route when it is
// next asked — is told as idle on the owning run.
func TestWorkspaceSurface_AnIdlePageIsToldAsIdle(t *testing.T) {
	for name, close := range map[string]func(m *lifecycle.Manager, id string){
		"sweep": func(m *lifecycle.Manager, _ string) { _, _ = m.SweepExposures(context.Background()) },
		"route": func(m *lifecycle.Manager, id string) {
			_, _, _ = m.OpenExposure(context.Background(), "acme", "alice", id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, ctx, _ := surfaceFixture(t)
			var mu sync.Mutex
			var got []lifecycle.SurfaceClosed
			var owners []string
			f.tool.Manager.SetSurfaceNotifier(func(_ context.Context, owner string, c lifecycle.SurfaceClosed) {
				mu.Lock()
				defer mu.Unlock()
				got, owners = append(got, c), append(owners, owner)
			})
			f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
			res := f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8000}`)
			var out struct {
				ID string `json:"exposure_id"`
			}
			_ = json.Unmarshal([]byte(res.Text), &out)
			if err := f.st.WorkspaceExposureTouch(context.Background(), "acme", out.ID, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			close(f.tool.Manager, out.ID)
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 || got[0].Reason != lifecycle.SurfaceIdle || got[0].ExposureID != out.ID || owners[0] != tools.RunID(ctx) {
				t.Errorf("told %+v for %v, want one idle close for the owning run", got, owners)
			}
		})
	}
}
