package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Every verdict the receiver can finish a delivery with is counted under a
// stated outcome: a refusal under its reason, a re-send as a duplicate.
func TestCountOutcome_EveryVerdictHasAnOutcome(t *testing.T) {
	for verdict, want := range map[string][2]string{
		verdictAccepted:          {"accepted", ""},
		verdictAcceptedReplay:    {"duplicate", ""},
		verdictPing:              {"ping", ""},
		verdictIgnored:           {"ignored", ""},
		verdictFiltered:          {"filtered", ""},
		verdictRejectedSig:       {"refused", "signature"},
		verdictUnresolved:        {"refused", "secret"},
		verdictRejectedEvent:     {"refused", "event"},
		verdictRejectedShape:     {"refused", "shape"},
		verdictRejectedDedupKey:  {"refused", "dedup_key"},
		verdictRejectedTeamStart: {"refused", "team_start"},
		"rejected_body":          {"refused", "body"},
		"rejected_rate":          {"refused", "rate"},
		"rejected_mapping":       {"refused", "mapping"},
		"rejected_disabled":      {"refused", "disabled"},
		"rejected_spawn_setup":   {"refused", "spawn_setup"},
		"timeout":                {"other", "timeout"},
		"client_gone":            {"other", "client_gone"},
	} {
		if o, r := countOutcome(verdict); o != want[0] || r != want[1] {
			t.Errorf("%s → (%q, %q), want (%q, %q)", verdict, o, r, want[0], want[1])
		}
	}
}

// The counters name their definition by reading webhookKey's value back, so
// every key it makes must come back as the same owner and name — a tenant
// that spells the static marker included.
func TestParseWebhookKey_ReadsBackEveryKeyWebhookKeyMakes(t *testing.T) {
	for _, owner := range []lookup.WebhookOwner{
		{Static: true}, {}, {TenantID: "acme"}, {TenantID: "static:"}, {TenantID: "static"}, {TenantID: "a:b%c d/é"},
	} {
		for _, name := range []string{"gh", "a:b", "100%", "with space", "static:", "é/x?y=z"} {
			o, n, ok := parseWebhookKey(webhookKey(owner, name))
			if !ok || o != owner || n != name {
				t.Errorf("parse(webhookKey(%+v, %q)) = %+v, %q, %v", owner, name, o, n, ok)
			}
		}
	}
	for _, bad := range []string{"", "no-separator", "acme:", "static::", "%zz:name"} {
		if _, _, ok := parseWebhookKey(bad); ok {
			t.Errorf("parse(%q) read a definition from a key webhookKey never makes", bad)
		}
	}
}

// countsOf is a definition's stored counters as a summary.
func countsOf(t *testing.T, st store.Store, owner lookup.WebhookOwner, name string) store.WebhookCountSummary {
	t.Helper()
	rows, err := st.WebhookCounts(context.Background(), owner.TenantID, name, owner.Static)
	if err != nil {
		t.Fatal(err)
	}
	return store.SummarizeWebhookCounts(rows)
}

// Each delivery to a definition is counted once, under how it ended — before
// and after the signature, answered or refused — and written only when the
// counts are flushed: nothing reaches the store per delivery. A request to a
// webhook that does not exist is counted nowhere.
func TestReceiver_CountsEachDeliveryUnderItsOutcomeAndWritesOnlyOnFlush(t *testing.T) {
	st := openScopeStore(t)
	fr := &fakeRunner{runID: "r"}
	gh := githubPushHook("push", "pull_request")
	gh.Filters = []config.WebhookFilter{{Path: "$.ref", Op: "glob", Value: "refs/heads/*"}}
	rec := newScopeReceiver(st, fr, map[string]config.Webhook{"gh": gh})
	rec.counts = st
	static := lookup.WebhookOwner{Static: true}

	ping := `{"zen":"z","hook_id":1}`
	answer(t, "ping", rec, "/v1/_webhooks/gh", ping, githubDelivery(ping, "d1", "ping"), http.StatusOK)
	issue := `{"issue":{"number":1}}`
	answer(t, "issues", rec, "/v1/_webhooks/gh", issue, githubDelivery(issue, "d2", "issues"), http.StatusAccepted)
	answer(t, "unsigned", rec, "/v1/_webhooks/gh", pushBody, http.Header{"X-Github-Event": {"push"}}, http.StatusUnauthorized)
	answer(t, "shape", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "d3", "pull_request"), http.StatusBadRequest)
	tag := strings.Replace(pushBody, "refs/heads/main", "refs/tags/v1", 1)
	answer(t, "a tag push", rec, "/v1/_webhooks/gh", tag, githubDelivery(tag, "d4", "push"), http.StatusAccepted)
	answer(t, "push", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "d5", "push"), http.StatusAccepted)
	answer(t, "the same push again", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "d5", "push"), http.StatusOK)
	answer(t, "unknown", rec, "/v1/_webhooks/nope", pushBody, githubDelivery(pushBody, "d6", "push"), http.StatusNotFound)

	if got := countsOf(t, st, static, "gh"); got.Accepted+got.Ping+len64(got.Refused) != 0 {
		t.Fatalf("counts reached the store before a flush: %+v", got)
	}
	// Nothing is held for the unknown name: an unauthenticated sender must
	// not be able to add entries for names that do not exist.
	rec.countMu.Lock()
	for k := range rec.pending {
		if k.webhook != webhookKey(static, "gh") {
			t.Errorf("a counter is held for %q, which is no definition", k.webhook)
		}
	}
	rec.countMu.Unlock()
	if err := rec.FlushCounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := countsOf(t, st, static, "gh")
	want := store.WebhookCountSummary{
		Accepted: 1, Duplicate: 1, Ping: 1, Ignored: 1, Filtered: 1,
		Refused: map[string]int64{"signature": 1, "shape": 1},
	}
	got.LastAt = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("counts = %+v\nwant     %+v", got, want)
	}
	// Flushed once: a second flush adds nothing.
	if err := rec.FlushCounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := countsOf(t, st, static, "gh"); again.Accepted != 1 || again.Ping != 1 {
		t.Errorf("a second flush added again: %+v", again)
	}
	if rows, _ := st.WebhookCounts(context.Background(), "", "nope", true); len(rows) != 0 {
		t.Errorf("a webhook that does not exist was counted: %v", rows)
	}
}

// failingCounts refuses its first add, then stores what it is given.
type failingCounts struct {
	mu    sync.Mutex
	fails int
	added []store.WebhookCount
}

func (f *failingCounts) WebhookCountsAdd(_ context.Context, rows []store.WebhookCount) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("store down")
	}
	f.added = append(f.added, rows...)
	return nil
}

func (f *failingCounts) WebhookCounts(context.Context, string, string, bool) ([]store.WebhookCount, error) {
	return nil, nil
}

// A flush the store refuses keeps the counts for the next one, which adds
// them once — with whatever was counted in between.
func TestFlushCounts_KeepsTheCountsWhenTheStoreRefuses(t *testing.T) {
	fc := &failingCounts{fails: 1}
	rec := newScopeReceiver(openScopeStore(t), &fakeRunner{runID: "r"}, map[string]config.Webhook{"gh": githubPushHook()})
	rec.counts = fc
	key := webhookKey(lookup.WebhookOwner{TenantID: "acme"}, "hook")
	rec.countDelivery(key, verdictAccepted)
	rec.countDelivery(key, verdictAccepted)
	if err := rec.FlushCounts(context.Background()); err == nil {
		t.Fatal("a refused flush reported no error")
	}
	rec.countDelivery(key, verdictAccepted)
	if err := rec.FlushCounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.added) != 1 || fc.added[0].Count != 3 || fc.added[0].TenantID != "acme" || fc.added[0].Name != "hook" || fc.added[0].Static {
		t.Errorf("added = %+v, want one row of 3 for acme/hook", fc.added)
	}
}

// A run of a webhook that lists events gets the delivery as the receiver saw
// it — provider, event, delivery id — in its trusted metadata, beside the
// definition's own keys and without writing to the definition's map. A
// webhook without events keeps its metadata as it was, and a delivery id
// that is not a short token is left out.
func TestReceiver_ARunGetsTheDeliveryInItsMetadataWhenTheWebhookListsEvents(t *testing.T) {
	gh := githubPushHook()
	gh.Metadata = map[string]any{"repo": "acme/app"}
	plain := scopeWebhook("X-Delivery")
	plain.Metadata = map[string]any{"repo": "acme/app"}
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": gh, "plain": plain})

	answer(t, "push", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "72d3162e-cc78-11e3", "push"), http.StatusAccepted)
	meta := fr.input().Metadata
	if meta["repo"] != "acme/app" {
		t.Errorf("the definition's own key was lost: %v", meta)
	}
	wantWH := map[string]any{"provider": "github", "event": "push", "delivery_id": "72d3162e-cc78-11e3"}
	if !reflect.DeepEqual(meta[config.WebhookMetadataKey], wantWH) {
		t.Errorf("metadata.webhook = %v, want %v", meta[config.WebhookMetadataKey], wantWH)
	}
	if _, wrote := gh.Metadata[config.WebhookMetadataKey]; wrote {
		t.Error("the definition's own metadata map was written")
	}

	long := strings.Repeat("x", 200)
	other := strings.Replace(pushBody, "bbb", "ccc", 1)
	answer(t, "push with a long id", rec, "/v1/_webhooks/gh", other, githubDelivery(other, long, "push"), http.StatusAccepted)
	if wh, _ := fr.input().Metadata[config.WebhookMetadataKey].(map[string]any); wh == nil || wh["delivery_id"] != nil {
		t.Errorf("a delivery id that is not a short token was passed: %v", wh)
	}

	body := `{"x":1}`
	h := signed([]byte(body), "d-plain")
	h.Set("X-Delivery", "d-plain")
	answer(t, "no events", rec, "/v1/_webhooks/plain", body, h, http.StatusAccepted)
	if _, set := fr.input().Metadata[config.WebhookMetadataKey]; set {
		t.Errorf("a webhook without events got metadata.webhook: %v", fr.input().Metadata)
	}
}

// The admin route answers a definition's counts as they were last flushed,
// and 404 for a webhook that does not resolve.
func TestDeliveryCounts_RouteAnswersTheFlushedCounts(t *testing.T) {
	st := openScopeStore(t)
	rec := newScopeReceiver(st, &fakeRunner{runID: "r"}, map[string]config.Webhook{"gh": githubPushHook()})
	rec.counts = st
	answer(t, "ping", rec, "/v1/_webhooks/gh", `{"zen":"z"}`, githubDelivery(`{"zen":"z"}`, "d1", "ping"), http.StatusOK)
	if err := rec.FlushCounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := get("/v1/_webhooks/gh/delivery-counts")
	var resp struct {
		Deliveries store.WebhookCountSummary `json:"deliveries"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Deliveries.Ping != 1 {
		t.Fatalf("delivery-counts = %d %s", w.Code, w.Body.String())
	}
	if w := get("/v1/_webhooks/nope/delivery-counts"); w.Code != http.StatusNotFound {
		t.Errorf("an unknown webhook answered %d", w.Code)
	}
}

func len64(m map[string]int64) int64 { return int64(len(m)) }

// newestVerdict is the verdict the newest delivery to the yaml webhook name
// was recorded with — what it is counted under.
func newestVerdict(t *testing.T, rec *Receiver, name string) string {
	t.Helper()
	recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, name), 1)
	if !ok || len(recs) != 1 {
		t.Fatalf("no delivery to %q was recorded", name)
	}
	return recs[0].Verdict
}

// A yaml webhook that lists events and sets metadata.webhook still loads —
// the receiver's value replaces it — but the boot warning names it.
func TestUnresolvableStaticSecrets_NamesAYamlWebhooksOwnMetadataKey(t *testing.T) {
	gh := githubPushHook()
	gh.Metadata = map[string]any{config.WebhookMetadataKey: "mine"}
	warns := UnresolvableStaticSecrets(&config.Config{Webhooks: map[string]config.Webhook{"gh": gh}},
		map[string]bool{"WH_SECRET": true}, mapGetenv(map[string]string{"WH_SECRET": scopeSecret}))
	for _, w := range warns {
		if strings.Contains(w, `metadata key "webhook"`) {
			return
		}
	}
	t.Errorf("warnings %q do not name the metadata key", warns)
}
