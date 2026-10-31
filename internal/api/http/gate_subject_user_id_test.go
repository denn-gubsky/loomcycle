package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A user a sign-in gate created is "<gate>:<provider id>". These were refused
// as malformed user ids (the old [A-Za-z0-9_-] rule), so such a user's runs
// list, inbox and run start all answered 400.
const gateSubject = "pocketid:2f7d3d17-82dd-43c7-bb3c-678a8aff8860"

func TestGateSubject_ListsOwnRunsAndInterrupts(t *testing.T) {
	srv, mux := runReadServer(t)
	run := addRun(t, srv.store, "loomcycle-dev", gateSubject, "a_gate", "")
	ctx := principalCtx("loomcycle-dev", gateSubject, auth.ScopeUser, auth.ScopeRunsRead)

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return rec
	}
	base := "/v1/users/" + url.PathEscape(gateSubject)
	rec := get(base + "/agents?status=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("runs list = %d %s, want 200", rec.Code, rec.Body)
	}
	var list struct {
		Agents []agentResponse `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 1 || list.Agents[0].RunID != run.ID {
		t.Errorf("runs list = %+v, want the gate user's one run %s", list.Agents, run.ID)
	}
	if rec := get(base + "/interrupts"); rec.Code != http.StatusOK {
		t.Errorf("interrupts = %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestGateSubject_PathUserIDWithSlashStillRefused(t *testing.T) {
	_, mux := runReadServer(t)
	ctx := principalCtx("loomcycle-dev", gateSubject, auth.ScopeUser, auth.ScopeRunsRead)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/users/"+url.PathEscape("a/b")+"/agents", nil).WithContext(ctx))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a '/' user id in the path = %d, want 400", rec.Code)
	}
}

func TestGateSubject_StartsARunNamingItself(t *testing.T) {
	for _, subject := range []string{
		gateSubject,
		// An AWS IAM gate's subject is a role ARN, with '/': not a valid user
		// id elsewhere, but a caller naming itself is never malformed.
		"awsiam:arn:aws:sts::123456789012:assumed-role/ci/session",
	} {
		_, mux := runReadServer(t)
		ctx := principalCtx("loomcycle-dev", subject, auth.ScopeUser, auth.ScopeRunsCreate, auth.ScopeRunsRead)
		body, _ := json.Marshal(map[string]any{
			"agent":    "writer",
			"user_id":  subject,
			"segments": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "trusted-text", "text": "hi"}}}},
		})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(string(body))).WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: POST /v1/runs = %d %s, want 200", subject, rec.Code, rec.Body)
		}
	}
}

func TestGateSubject_AnotherUsersMalformedIDStillRefused(t *testing.T) {
	_, mux := runReadServer(t)
	ctx := principalCtx("loomcycle-dev", gateSubject, auth.ScopeUser, auth.ScopeRunsCreate)
	body := `{"agent":"writer","user_id":"someone/else","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body)).WithContext(ctx))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /v1/runs naming another, malformed user = %d %s, want 400", rec.Code, rec.Body)
	}
}
