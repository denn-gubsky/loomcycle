package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC HI §4: a page's state on request, behind the real auth middleware,
// beside the page itself.

// stateServer is the ticket fixture's server with the state routes and the
// page route on one mux, as exposureRoutes registers them.
func stateServer(t *testing.T) (*ticketFixture, *httptest.Server) {
	t.Helper()
	tf := newTicketFixture(t, false, time.Hour)
	mux := http.NewServeMux()
	tf.s.exposureRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return tf, ts
}

func stateGet(t *testing.T, ts *httptest.Server, bearer, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func decodeState(t *testing.T, body string) exposureState {
	t.Helper()
	var s exposureState
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("state %q: %v", body, err)
	}
	return s
}

// The address with no slash after the id is the state; with it, the page
// itself, as before. The state names the page, its workspace and run, and
// when it closes idle, and carries no credential or host path.
func TestExposureState_TheIDIsTheStateAndTheSlashIsThePage(t *testing.T) {
	tf, ts := stateServer(t)
	code, body := stateGet(t, ts, "alice-token", exposurePrefix+testExposureID)
	if code != http.StatusOK {
		t.Fatalf("GET the id = %d %s", code, body)
	}
	s := decodeState(t, body)
	e, _ := tf.f.st.WorkspaceExposureGet(context.Background(), "acme", testExposureID)
	if s.ExposureID != testExposureID || s.State != exposureStateLive || s.Kind != store.WorkspaceExposurePage ||
		s.Workspace != "web" || s.RunID != "run1" || s.ExpiresAt == nil || !s.ExpiresAt.Equal(e.LastUsedAt.Add(15*time.Minute)) ||
		s.ClosedAt != nil || s.ClosedReason != "" || s.LeaseExpiresAt != nil {
		t.Errorf("live state = %+v; want web/run1, expiring 15m after its last use (%v)", s, e.LastUsedAt)
	}
	for _, leak := range []string{"token", "ticket", "Bearer", "/tmp", "base_path", "port"} {
		if strings.Contains(body, leak) {
			t.Errorf("the state carries %q: %s", leak, body)
		}
	}
	if code, body := stateGet(t, ts, "alice-token", exposurePrefix+testExposureID+"/"); code != http.StatusOK || body != "hello from the workspace" {
		t.Errorf("GET the page's root = %d %q, want the application's page", code, body)
	}
}

// Reading the state is not a use: it moves no idle deadline.
func TestExposureState_ReadingItIsNotAUse(t *testing.T) {
	tf, ts := stateServer(t)
	old := time.Now().Add(-10 * time.Minute)
	if err := tf.f.st.WorkspaceExposureTouch(context.Background(), "acme", testExposureID, old); err != nil {
		t.Fatal(err)
	}
	stateGet(t, ts, "alice-token", exposurePrefix+testExposureID)
	if e, _ := tf.f.st.WorkspaceExposureGet(context.Background(), "acme", testExposureID); !e.LastUsedAt.Equal(old.UTC()) {
		t.Errorf("the state read moved last use to %v", e.LastUsedAt)
	}
}

// Another user's page, an unknown id and a caller naming no user all get
// the unknown id's answer.
func TestExposureState_AnotherUsersPageIsUnknown(t *testing.T) {
	_, ts := stateServer(t)
	_, unknownBody := stateGet(t, ts, "alice-token", exposurePrefix+"ffffffffffffffffffffffff")
	for name, c := range map[string]struct{ bearer, id string }{
		"bob on alice's page": {"bob-token", testExposureID},
		"alice on bob's page": {"alice-token", exposureC},
		"unknown":             {"alice-token", "ffffffffffffffffffffffff"},
		"the legacy token":    {"legacy-secret", testExposureID},
		"a malformed page id": {"alice-token", "nothex"},
	} {
		code, body := stateGet(t, ts, c.bearer, exposurePrefix+c.id)
		if code != http.StatusNotFound || body != unknownBody {
			t.Errorf("%s = %d %q, want the unknown id's 404 %q", name, code, body, unknownBody)
		}
	}
}

// After the idle sweep the state reads closed, idle, with no stream open,
// and the page, its ticket and the list all treat it as gone.
func TestExposureState_ReportsClosedIdleAfterTheSweep(t *testing.T) {
	tf, ts := stateServer(t)
	ctx := context.Background()
	if err := tf.f.st.WorkspaceExposureTouch(ctx, "acme", testExposureID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := tf.f.m.SweepExposures(ctx); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	code, body := stateGet(t, ts, "alice-token", exposurePrefix+testExposureID)
	s := decodeState(t, body)
	if code != http.StatusOK || s.State != exposureStateClosed || s.ClosedReason != "idle" || s.ClosedAt == nil || s.ExpiresAt != nil {
		t.Errorf("after the sweep = %d %+v, want closed idle with closed_at and no expires_at", code, s)
	}
	if code, _ := stateGet(t, ts, "alice-token", pagePath); code != http.StatusNotFound {
		t.Errorf("the closed page = %d, want 404", code)
	}
	if code, _ := tf.mint(t, "alice-token", testExposureID); code != http.StatusNotFound {
		t.Errorf("a ticket for the closed page = %d, want 404", code)
	}
	_, list := stateGet(t, ts, "alice-token", "/v1/workspaces/exposures")
	if strings.Contains(list, testExposureID) || !strings.Contains(list, exposureB) {
		t.Errorf("alice's live pages = %s, want B and not the closed one", list)
	}
}

// The list holds the caller's own live pages only; a caller naming no user
// has none.
func TestExposureState_ListsTheCallersLivePages(t *testing.T) {
	_, ts := stateServer(t)
	code, body := stateGet(t, ts, "alice-token", "/v1/workspaces/exposures")
	var out struct {
		Exposures []exposureState `json:"exposures"`
	}
	if err := json.Unmarshal([]byte(body), &out); code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s (%v)", code, body, err)
	}
	got := map[string]string{}
	for _, s := range out.Exposures {
		got[s.ExposureID] = s.State
	}
	if len(got) != 2 || got[testExposureID] != exposureStateLive || got[exposureB] != exposureStateLive {
		t.Errorf("alice's pages = %v, want her two live pages", got)
	}
	if _, body := stateGet(t, ts, "legacy-secret", "/v1/workspaces/exposures"); body != `{"exposures":[]}`+"\n" && body != `{"exposures":[]}` {
		t.Errorf("the legacy token's pages = %s, want none", body)
	}
}
