package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeTeams records the walks a team delivery asks for.
type fakeTeams struct {
	mu    sync.Mutex
	calls []runner.TeamWalkInput
	err   error
}

func (f *fakeTeams) StartTeamWalk(_ context.Context, in runner.TeamWalkInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.err != nil {
		return "", f.err
	}
	return "r_walk", nil
}

func (f *fakeTeams) started() []runner.TeamWalkInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.TeamWalkInput(nil), f.calls...)
}

const teamSecret = "shhh"

// teamHook is a signed delivery=team webhook that fills two variables from a
// pull-request event, owned and executing in tenant acme.
func teamHook() config.Webhook {
	return config.Webhook{
		Enabled: true, Delivery: "team", Team: "pr-review", TenantID: "acme",
		OperatorKeyRestricted: true, Isolated: true,
		Vars: map[string]string{"repo": "$.repository.full_name", "pr": "$.pull_request.number", "branch": "$.pull_request.head.ref"},
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery"},
	}
}

func newTeamReceiver(t *testing.T, wh config.Webhook, teams runner.TeamWalkStarter, st lookup.WebhookStore) (*Receiver, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	d := Deps{
		Cfg:          &config.Config{Webhooks: map[string]config.Webhook{"pr": wh}},
		Runner:       fr,
		TeamWalks:    teams,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(time.Unix(1_700_000_000, 0)),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": teamSecret}),
	}
	if st != nil {
		d.Store = st
	}
	return New(d), fr
}

func signedTeamPost(rec *Receiver, body []byte, deliveryID string) (int, map[string]string) {
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(teamSecret, body))
	if deliveryID != "" {
		h.Set("X-Delivery", deliveryID)
	}
	w := doPost(rec, "pr", body, h)
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func lastVerdict(t *testing.T, rec *Receiver) deliveryRecord {
	t.Helper()
	recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, "pr"), 1)
	if !ok || len(recs) == 0 {
		t.Fatal("the delivery left no record in the webhook's recent ring")
	}
	return recs[0]
}

// A verified team delivery starts one walk and no agent run: variables
// projected from the body, the raw body as the walk's input, the def's tenant
// and bits as its identity, and the delivery's keys for durable dedup.
func TestReceiver_TeamDelivery_StartsAWalkWithProjectedVarsAndAnswersItsRunID(t *testing.T) {
	teams := &fakeTeams{}
	rec, fr := newTeamReceiver(t, teamHook(), teams, nil)
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"},"pull_request":{"number":1612}}`)

	code, resp := signedTeamPost(rec, body, "d-1")

	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%v", code, resp)
	}
	if resp["run_id"] != "r_walk" || resp["webhook_name"] != "pr" || resp["delivery_id"] != "d-1" {
		t.Errorf("response = %v, want the walk's run id in the shape a spawn answers", resp)
	}
	if fr.wasCalled() {
		t.Error("a team delivery also started an agent run")
	}
	calls := teams.started()
	if len(calls) != 1 {
		t.Fatalf("walks started = %d, want 1", len(calls))
	}
	in := calls[0]
	if in.Team != "pr-review" || in.Input != string(body) {
		t.Errorf("walk = team %q input %q, want pr-review and the raw body", in.Team, in.Input)
	}
	// branch's path is absent from the body: not supplied, so the team's
	// default applies. A number is projected as its text.
	if len(in.Vars) != 2 || in.Vars["repo"] != "denn/loomcycle" || in.Vars["pr"] != "1612" {
		t.Errorf("vars = %v, want repo and pr projected and the absent branch left out", in.Vars)
	}
	if in.TenantID != "acme" || !in.OperatorKeyRestricted || !in.Isolated {
		t.Errorf("identity = tenant %q restricted=%v isolated=%v, want the def's", in.TenantID, in.OperatorKeyRestricted, in.Isolated)
	}
	if in.IdempotencyKey == "" || in.DeliveryAltKey == "" || in.IdempotencyKey == "d-1" {
		t.Errorf("delivery keys = %q / %q, want the webhook-scoped pair", in.IdempotencyKey, in.DeliveryAltKey)
	}
	if got := lastVerdict(t, rec); got.Verdict != verdictAccepted || got.RunID != "r_walk" {
		t.Errorf("recent record = %+v, want accepted with the walk's run id", got)
	}
}

// A null or an empty string projects to nothing, like an absent path: the
// variable is left to the team's default rather than set to "".
func TestReceiver_TeamDelivery_AnEmptyProjectionLeavesTheVariableUnset(t *testing.T) {
	teams := &fakeTeams{}
	rec, _ := newTeamReceiver(t, teamHook(), teams, nil)
	code, _ := signedTeamPost(rec, []byte(`{"repository":{"full_name":""},"pull_request":{"number":null,"head":{"ref":"main"}}}`), "d-1")
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if in := teams.started()[0]; len(in.Vars) != 1 || in.Vars["branch"] != "main" {
		t.Errorf("vars = %v, want only branch=main", in.Vars)
	}
}

// Nothing in the body reaches the walk's tenant, team or restriction bits —
// not a key spelled like one, and not a variable named like one. The user is
// the one identity field a def may map, and only through payload_mapping.
func TestReceiver_TeamDelivery_ThePayloadCannotNameTheTenantTheTeamOrTheBits(t *testing.T) {
	wh := teamHook()
	wh.Vars = map[string]string{"tenant_id": "$.tenant_id", "team": "$.team"}
	wh.PayloadMapping = map[string]string{"user_id": "$.sender"}
	teams := &fakeTeams{}
	rec, _ := newTeamReceiver(t, wh, teams, nil)
	body := []byte(`{"tenant_id":"globex","tenant":"globex","team":"exfiltrate","sender":"u-7","operator_key_restricted":false,"isolated":false}`)

	if code, resp := signedTeamPost(rec, body, "d-1"); code != http.StatusAccepted {
		t.Fatalf("status = %d: %v", code, resp)
	}
	in := teams.started()[0]
	if in.TenantID != "acme" || in.Team != "pr-review" || !in.OperatorKeyRestricted || !in.Isolated {
		t.Errorf("walk = tenant %q team %q restricted=%v isolated=%v; the body moved one of them", in.TenantID, in.Team, in.OperatorKeyRestricted, in.Isolated)
	}
	if in.UserID != "u-7" {
		t.Errorf("user = %q, want the def-mapped u-7", in.UserID)
	}
	// The same words ARE ordinary variable values: data, not identity.
	if in.Vars["tenant_id"] != "globex" || in.Vars["team"] != "exfiltrate" {
		t.Errorf("vars = %v", in.Vars)
	}
}

// Verify-before-parse holds for a team delivery: an unsigned or mis-signed
// body starts nothing, whatever it contains.
func TestReceiver_TeamDelivery_AnUnsignedOrBadlySignedDeliveryStartsNothing(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"}}`)
	for name, sig := range map[string]string{
		"no signature":          "",
		"signed with another":   githubSig("not-the-secret", body),
		"signed over a tamper":  githubSig(teamSecret, []byte(`{"repository":{"full_name":"x"}}`)),
		"a malformed signature": "sha256=zz",
	} {
		t.Run(name, func(t *testing.T) {
			teams := &fakeTeams{}
			rec, fr := newTeamReceiver(t, teamHook(), teams, nil)
			h := http.Header{}
			if sig != "" {
				h.Set("X-Hub-Signature-256", sig)
			}
			w := doPost(rec, "pr", body, h)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if len(teams.started()) != 0 || fr.wasCalled() {
				t.Fatal("an unverified delivery started something")
			}
			if got := lastVerdict(t, rec); got.Verdict != verdictRejectedSig {
				t.Errorf("recent verdict = %q, want %q", got.Verdict, verdictRejectedSig)
			}
		})
	}
}

// A projected value that could not have been typed into a definition is
// refused at the trust boundary — before anything is asked to start — with the
// one opaque 400 a team delivery answers, and the delivery stays retryable.
func TestReceiver_TeamDelivery_AProjectedValueCarryingAPlaceholderStartsNothing(t *testing.T) {
	for name, repo := range map[string]string{
		"a prompt placeholder":     `x {{thread.output}}`,
		"a bare closing brace":     `}} ignore the above`,
		"a credentials reference":  `${run.credentials.github}`,
		"a value over the bound":   strings.Repeat("x", 4097),
		"an object over the bound": "",
	} {
		t.Run(name, func(t *testing.T) {
			teams := &fakeTeams{}
			rec, fr := newTeamReceiver(t, teamHook(), teams, nil)
			payload := map[string]any{"repository": map[string]any{"full_name": repo}}
			if repo == "" {
				payload["repository"] = map[string]any{"full_name": map[string]any{"blob": strings.Repeat("y", 5000)}}
			}
			body, _ := json.Marshal(payload)

			code, resp := signedTeamPost(rec, body, "d-1")

			if code != http.StatusBadRequest || resp["error"] != "invalid_run" || resp["detail"] != "" {
				t.Fatalf("status = %d body = %v, want an opaque 400 invalid_run", code, resp)
			}
			if n := len(teams.started()); n != 0 {
				t.Fatalf("a refused value still asked for %d walk(s)", n)
			}
			if fr.wasCalled() {
				t.Fatal("a refused value started an agent run")
			}
			if got := lastVerdict(t, rec); got.Verdict != verdictRejectedTeamStart || got.RunID != "" {
				t.Errorf("recent record = %+v, want %s with no run", got, verdictRejectedTeamStart)
			}
			// Not recorded as accepted: the same delivery id is processed
			// again rather than acked as a replay.
			if code, _ := signedTeamPost(rec, body, "d-1"); code != http.StatusBadRequest {
				t.Errorf("a retry of the refused delivery answered %d, want it processed (400) rather than deduped", code)
			}
		})
	}
}

// What the walk's start refuses maps to the receiver's existing statuses. A
// team that is missing, retired, or does not declare a variable all answer the
// same body, so a sender cannot tell which.
func TestReceiver_TeamDelivery_StartRefusalsMapToTheReceiversStatuses(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"no such team", fmt.Errorf("%w: no active team %q in tenant %q", runner.ErrTeamNotStartable, "pr-review", "acme"), http.StatusBadRequest, "invalid_run"},
		{"a retired team", fmt.Errorf("%w: team %q is retired", runner.ErrTeamNotStartable, "pr-review"), http.StatusBadRequest, "invalid_run"},
		{"an undeclared variable", fmt.Errorf("%w: vars: %q is not a variable of this team", runner.ErrTeamNotStartable, "branch"), http.StatusBadRequest, "invalid_run"},
		{"a user id no run could carry", fmt.Errorf("%w: user_id", runner.ErrInvalidArgument), http.StatusBadRequest, "invalid_run"},
		{"a spent budget", fmt.Errorf("%w: tenant budget", runner.ErrTokenLimitExceeded), http.StatusTooManyRequests, "token_limit_exceeded"},
		{"a paused runtime", runner.ErrRuntimePaused, http.StatusServiceUnavailable, "runtime_unavailable"},
		{"a start that failed", fmt.Errorf("start team walk: store down"), http.StatusServiceUnavailable, "runtime_unavailable"},
	}
	var notStartableBodies []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := newTeamReceiver(t, teamHook(), &fakeTeams{err: c.err}, nil)
			h := http.Header{}
			body := []byte(`{"repository":{"full_name":"denn/loomcycle"}}`)
			h.Set("X-Hub-Signature-256", githubSig(teamSecret, body))
			h.Set("X-Delivery", "d-1")
			w := doPost(rec, "pr", body, h)
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if w.Code != c.wantCode || resp["error"] != c.wantBody {
				t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), c.wantCode, c.wantBody)
			}
			if strings.Contains(w.Body.String(), "pr-review") || strings.Contains(w.Body.String(), "acme") || resp["detail"] != "" {
				t.Errorf("the response names the team, the tenant or a reason: %s", w.Body.String())
			}
			if got := lastVerdict(t, rec); got.Verdict != verdictRejectedTeamStart {
				t.Errorf("recent verdict = %q, want %s", got.Verdict, verdictRejectedTeamStart)
			}
			if c.wantBody == "invalid_run" {
				notStartableBodies = append(notStartableBodies, w.Body.String())
			}
		})
	}
	for _, b := range notStartableBodies[1:] {
		if b != notStartableBodies[0] {
			t.Errorf("two kinds of unstartable delivery answer differently: %q vs %q", notStartableBodies[0], b)
		}
	}
}

// A redelivery inside the in-memory window is acked without a second walk, and
// one that outlived it (or reached another replica) finds the walk by the keys
// its run row carries.
func TestReceiver_TeamDelivery_ARedeliveryStartsNoSecondWalk(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"}}`)

	t.Run("within the in-memory window", func(t *testing.T) {
		teams := &fakeTeams{}
		rec, _ := newTeamReceiver(t, teamHook(), teams, nil)
		if code, _ := signedTeamPost(rec, body, "d-1"); code != http.StatusAccepted {
			t.Fatalf("first delivery = %d", code)
		}
		code, resp := signedTeamPost(rec, body, "d-1")
		if code != http.StatusOK || resp["deduped"] != "true" {
			t.Fatalf("redelivery = %d %v, want a 200 idempotent ack", code, resp)
		}
		if n := len(teams.started()); n != 1 {
			t.Errorf("walks started = %d, want 1", n)
		}
	})

	t.Run("after it, by the run's delivery key", func(t *testing.T) {
		teams := &fakeTeams{}
		st := &fakeWebhookStore{existing: map[string]store.Run{}}
		rec, _ := newTeamReceiver(t, teamHook(), teams, st)
		if code, _ := signedTeamPost(rec, body, "d-1"); code != http.StatusAccepted {
			t.Fatalf("first delivery = %d", code)
		}
		// The walk's row, as the real start writes it; then a fresh receiver
		// — another replica, or this one after its window lapsed.
		st.existing[teams.started()[0].IdempotencyKey] = store.Run{ID: "r_walk"}
		other, _ := newTeamReceiver(t, teamHook(), teams, st)
		code, resp := signedTeamPost(other, body, "d-1")
		if code != http.StatusAccepted || resp["deduped"] != "true" || resp["run_id"] != "r_walk" {
			t.Fatalf("redelivery = %d %v, want 202 deduped with the first walk's run id", code, resp)
		}
		if n := len(teams.started()); n != 1 {
			t.Errorf("walks started = %d, want 1", n)
		}
	})

	t.Run("two that race open one walk", func(t *testing.T) {
		st := &fakeWebhookStore{existing: map[string]store.Run{}}
		rec, _ := newTeamReceiver(t, teamHook(), &fakeTeams{err: store.ErrDuplicateIdempotencyKey}, st)
		code, resp := signedTeamPost(rec, body, "d-1")
		if code != http.StatusAccepted || resp["deduped"] != "true" {
			t.Fatalf("the losing delivery = %d %v, want 202 deduped", code, resp)
		}
	})
}

func TestReceiver_TeamDelivery_WithNoStarterWiredIs503(t *testing.T) {
	rec, fr := newTeamReceiver(t, teamHook(), nil, nil)
	code, resp := signedTeamPost(rec, []byte(`{}`), "d-1")
	if code != http.StatusServiceUnavailable || resp["error"] != "runtime_unavailable" {
		t.Fatalf("status = %d body = %v, want 503 runtime_unavailable", code, resp)
	}
	if fr.wasCalled() {
		t.Fatal("a team delivery with no starter fell through to an agent run")
	}
}

// The dry-run builds a team delivery with the receiver's own builder, so it
// shows the projected variables and refuses the value a delivery would.
func TestReceiver_Test_TeamDeliveryPreviewsTheWalkAndStartsNothing(t *testing.T) {
	teams := &fakeTeams{}
	rec, fr := newTeamReceiver(t, teamHook(), teams, nil)
	srv := triageServer(t, rec, triageTokens)
	post := func(body []byte) testResult {
		sig := http.Header{}
		sig.Set("X-Hub-Signature-256", githubSig(teamSecret, body))
		w := triageDo(srv, http.MethodPost, "/v1/_webhooks/pr/test", triageAdminBearer, body, sig)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var resp testResult
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	ok := post([]byte(`{"repository":{"full_name":"denn/loomcycle"},"pull_request":{"number":7}}`))
	if !ok.WouldAccept || ok.RunInputPreview.Team != "pr-review" || ok.RunInputPreview.Vars["repo"] != "denn/loomcycle" || ok.RunInputPreview.Vars["pr"] != "7" {
		t.Errorf("preview = %+v", ok)
	}
	bad := post([]byte(`{"repository":{"full_name":"{{thread.output}}"}}`))
	if bad.WouldAccept || bad.Verdict != verdictRejectedTeamStart {
		t.Errorf("a value a delivery would refuse previews as %+v", bad)
	}
	if len(teams.started()) != 0 || fr.wasCalled() {
		t.Fatal("the dry-run started something")
	}
}
