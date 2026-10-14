package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// userAgentsList is GET /v1/users/{user_id}/agents's answer, decoded.
type userAgentsList struct {
	Agents []struct {
		AgentID      string `json:"agent_id"`
		Status       string `json:"status"`
		AwaitedState string `json:"awaited_state"`
	} `json:"agents"`
	Truncated *bool `json:"truncated"`
}

func (l userAgentsList) agentIDs() []string {
	out := make([]string, len(l.Agents))
	for i, a := range l.Agents {
		out[i] = a.AgentID
	}
	slices.Sort(out)
	return out
}

// listUserAgents calls the handler as an admin and returns the status code,
// the decoded body and the raw body.
func listUserAgents(t *testing.T, s *Server, query string) (int, userAgentsList, string) {
	t.Helper()
	req := principalReq("GET", "/v1/users/alice/agents"+query, auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}})
	req.SetPathValue("user_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListUserAgents(rec, req)
	var out userAgentsList
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out, rec.Body.String()
}

// seedAliceRun creates one run for alice and leaves it in status st, with the
// given events appended (type, payload pairs).
func seedAliceRun(t *testing.T, st store.Store, agentID string, status store.RunStatus, events ...[2]string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "echo", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := st.AppendEvent(ctx, run.ID, e[0], []byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if status != store.RunRunning {
		if err := st.FinishRun(ctx, run.ID, status, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	heldForReviewEvent = [2]string{"awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1}}`}
	parkedOnInputEvent = [2]string{"awaiting_input", `{"type":"awaiting_input","awaiting_input":{"since_turn":1}}`}
	textEvent          = [2]string{"text", `{"type":"text"}`}
)

// ?status= takes several statuses — comma-separated or repeated — and lists a
// run in any of them; absent or empty still means running and "all" still
// means every status.
func TestHandleListUserAgents_StatusListMatchesAnyListedStatus(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedAliceRun(t, st, "a_running", store.RunRunning)
	seedAliceRun(t, st, "a_completed", store.RunCompleted)
	seedAliceRun(t, st, "a_failed", store.RunFailed)
	seedAliceRun(t, st, "a_cancelled", store.RunCancelled)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"?status=completed,failed", []string{"a_completed", "a_failed"}},
		{"?status=completed&status=failed", []string{"a_completed", "a_failed"}},
		{"?status=cancelled,%20running", []string{"a_cancelled", "a_running"}},
		{"?status=failed", []string{"a_failed"}},
		{"", []string{"a_running"}},
		{"?status=", []string{"a_running"}},
		{"?status=all", []string{"a_cancelled", "a_completed", "a_failed", "a_running"}},
	} {
		code, got, body := listUserAgents(t, s, tc.query)
		if code != http.StatusOK {
			t.Errorf("%q: status %d: %s", tc.query, code, body)
			continue
		}
		if !slices.Equal(got.agentIDs(), tc.want) {
			t.Errorf("%q listed %v, want %v", tc.query, got.agentIDs(), tc.want)
		}
		if got.Truncated != nil {
			t.Errorf("%q carried truncated=%v; a plain listing never sets it", tc.query, *got.Truncated)
		}
	}
}

// A filter value the route does not know is a 400 naming it, not a listing
// that matches nothing — a typo used to read exactly like "no runs".
func TestHandleListUserAgents_RefusesUnknownOrConflictingFilters(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedAliceRun(t, st, "a_running", store.RunRunning)

	for _, tc := range []struct{ query, code string }{
		{"?status=runing", "invalid_status"},
		{"?status=running,done", "invalid_status"},
		{"?status=all,running", "invalid_status"},
		{"?awaited_state=reviw", "invalid_awaited_state"},
		{"?awaited_state=review,nope", "invalid_awaited_state"},
		{"?awaited_state=review&status=completed", "invalid_status"},
		{"?awaited_state=review&status=running,failed", "invalid_status"},
		{"?awaited_state=review&status=all", "invalid_status"},
	} {
		code, _, body := listUserAgents(t, s, tc.query)
		if code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400: %s", tc.query, code, body)
			continue
		}
		var e struct{ Code string }
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Code != tc.code {
			t.Errorf("%q: body %s, want code %q", tc.query, body, tc.code)
		}
	}
	for _, ok := range []string{"?awaited_state=review", "?awaited_state=review&status=running", "?awaited_state=children,channel,interrupted,input"} {
		if code, _, body := listUserAgents(t, s, ok); code != http.StatusOK {
			t.Errorf("%q: status %d, want 200: %s", ok, code, body)
		}
	}
}

// ?awaited_state= lists only the running runs blocked on one of the named
// waits, each carrying the state it matched; a finished run is never listed,
// whatever its last events say.
func TestHandleListUserAgents_AwaitedStateListsOnlyRunsInThatState(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedAliceRun(t, st, "a_review", store.RunRunning, textEvent, heldForReviewEvent)
	seedAliceRun(t, st, "a_input", store.RunRunning, parkedOnInputEvent)
	seedAliceRun(t, st, "a_plain", store.RunRunning, textEvent)
	seedAliceRun(t, st, "a_done_while_held", store.RunCompleted, heldForReviewEvent)

	code, got, body := listUserAgents(t, s, "?awaited_state=review")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if !slices.Equal(got.agentIDs(), []string{"a_review"}) {
		t.Fatalf("awaited_state=review listed %v, want only a_review", got.agentIDs())
	}
	if a := got.Agents[0]; a.AwaitedState != "review" || a.Status != "running" {
		t.Errorf("a_review listed as status=%q awaited_state=%q, want running/review", a.Status, a.AwaitedState)
	}
	if got.Truncated != nil {
		t.Errorf("a scan that read every running run carried truncated=%v", *got.Truncated)
	}

	_, both, _ := listUserAgents(t, s, "?awaited_state=review,input&status=running")
	if !slices.Equal(both.agentIDs(), []string{"a_input", "a_review"}) {
		t.Errorf("awaited_state=review,input listed %v, want a_input + a_review", both.agentIDs())
	}
}

// The awaited_state scan reads at most its cap of running runs, newest first,
// and says truncated when runs were left unread. 1,000 in production; the test
// lowers it rather than writing a thousand runs.
func TestHandleListUserAgents_AwaitedStateScanReportsTruncatedAtTheCap(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	for _, id := range []string{"a_1", "a_2", "a_3", "a_4", "a_5"} {
		seedAliceRun(t, st, id, store.RunRunning, heldForReviewEvent)
	}

	s.awaitedScanCap = 3
	code, got, body := listUserAgents(t, s, "?awaited_state=review")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if !slices.Equal(got.agentIDs(), []string{"a_3", "a_4", "a_5"}) {
		t.Errorf("capped scan listed %v, want the 3 newest", got.agentIDs())
	}
	if got.Truncated == nil || !*got.Truncated {
		t.Errorf("capped scan with runs left unread did not report truncated: %s", body)
	}

	s.awaitedScanCap = 5
	_, all, body := listUserAgents(t, s, "?awaited_state=review")
	if len(all.Agents) != 5 || all.Truncated != nil {
		t.Errorf("a scan that read every run listed %d, body %s; want 5 and no truncated", len(all.Agents), body)
	}
}
