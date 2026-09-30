package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// V2c end to end: a webhook whose literal credentials a snapshot stripped is
// restored disabled and answers a correctly signed delivery with 404; a fork
// that re-supplies only some of the keys leaves it 404; a fork whose own
// overlay re-supplies every key and sets enabled:true brings it back, and the
// same signed delivery is accepted.
func TestReceiver_RestoredCredentialStrippedWebhook404sUntilAForkReSuppliesEveryKey(t *testing.T) {
	ctx := context.Background()
	open := func(name string) store.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	src, dst := open("src.db"), open("dst.db")

	body := json.RawMessage(`{"delivery":"spawn","agent":"researcher","enabled":true,
		"auth":{"kind":"hmac","header":"X-Hub-Signature-256","signing_secret_env":"WH_SECRET"},
		"user_credentials":{"jobs":"literal-jobs","slack":"literal-slack"}}`)
	if _, err := src.SnapshotRestoreWebhookDef(ctx, store.WebhookDefRow{DefID: "wh_src", Name: "gh", Version: 1, CreatedAt: time.Now(), Definition: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{Name: "gh", DefID: "wh_src", PromotedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, envelope, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := snapshot.Restore(ctx, dst, envelope, snapshot.RestoreOptions{
		Validators: map[string]func(json.RawMessage) error{migrations.SectionWebhookDefs: builtin.ValidateWebhookDefBody},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.WebhookDefsRestored != 1 || res.DefsDisabledForCredentials != 1 {
		t.Fatalf("restore = %v, warnings %v; want the webhook restored, disabled", res.Counts(), res.Warnings)
	}

	secret := "shhh"
	payload := []byte(`{"goal":"go"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, payload))
	deliver := func() int {
		fr := &fakeRunner{runID: "r", agentID: "a"}
		rec := New(Deps{
			Cfg: &config.Config{}, Store: dst, Runner: fr,
			EnvAllowlist: map[string]bool{"WH_SECRET": true},
			Now:          fixedClock(time.Unix(1_700_000_000, 0)),
			Getenv:       mapGetenv(map[string]string{"WH_SECRET": secret}),
		})
		return doPost(rec, "gh", payload, h).Code
	}
	if code := deliver(); code != http.StatusNotFound {
		t.Fatalf("restored credential-stripped webhook answered %d, want 404", code)
	}

	tool := &builtin.WebhookDef{Store: dst, Cfg: &config.Config{}}
	tctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "operator"})
	tctx = tools.WithWebhookDefPolicy(tctx, tools.WebhookDefPolicyValue{Scopes: []string{"any"}})
	fork := func(overlay string) {
		t.Helper()
		r, _ := tool.Execute(tctx, json.RawMessage(`{"op":"fork","name":"gh","overlay":`+overlay+`}`))
		if r.IsError {
			t.Fatalf("fork: %s", r.Text)
		}
	}
	fork(`{"enabled":true,"user_credentials":{"jobs":"new-jobs"}}`)
	if code := deliver(); code != http.StatusNotFound {
		t.Fatalf("after a partial re-supply the webhook answered %d, want 404", code)
	}
	fork(`{"enabled":true,"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}`)
	if code := deliver(); code != http.StatusAccepted {
		t.Fatalf("after every key was re-supplied the webhook answered %d, want 202", code)
	}
}
