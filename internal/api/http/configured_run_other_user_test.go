package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// A draft runs as the user who created it, whoever starts it — and the tenant
// lets any member edit and start one. Started by someone else, it runs
// without its creator's own credentials: that person may have written the
// prompt.
func TestConfiguredRun_StartedByAnotherUserWithholdsTheCreatorsScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		starter  auth.Principal
		withhold bool
	}{
		{"alice herself", ouAlice, false},
		{"bob", ouBob, true},
	} {
		srv, prov := otherUserServer(t, true)
		draft, err := srv.CreateConfiguredRun(auth.WithPrincipal(context.Background(), ouAlice), connector.ConfiguredRunRequest{
			SpawnRunRequest: connector.SpawnRunRequest{Agent: "resumer", Segments: []loop.PromptSegment{
				{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do it"}}},
			}},
		})
		if err != nil {
			t.Fatalf("%s: create the draft: %v", tc.name, err)
		}
		if _, err := srv.StartConfiguredRun(auth.WithPrincipal(context.Background(), tc.starter), draft.RunID, connector.RunSecrets{}); err != nil {
			t.Fatalf("%s: start: %v", tc.name, err)
		}
		calls := prov.calls()
		if len(calls) != 1 || calls[0].user != "alice" || calls[0].noUserScope != tc.withhold {
			t.Errorf("%s: the draft ran with %+v, want one call as alice with no-user-scope=%t", tc.name, calls, tc.withhold)
		}
		if row, _ := srv.store.GetRun(context.Background(), draft.RunID); row.CredentialsNoUserScope != tc.withhold {
			t.Errorf("%s: the run's row credentials_no_user_scope = %t, want %t", tc.name, row.CredentialsNoUserScope, tc.withhold)
		}
	}
}

// An edit is authoring too: a draft someone else changed runs without its
// creator's credentials even when the creator starts it — the prompt may be
// the editor's.
func TestConfiguredRun_EditedByAnotherUserWithholdsTheCreatorsScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		editor   auth.Principal
		withhold bool
	}{
		{"alice herself", ouAlice, false},
		{"bob", ouBob, true},
	} {
		srv, prov := otherUserServer(t, true)
		alice := auth.WithPrincipal(context.Background(), ouAlice)
		draft, err := srv.CreateConfiguredRun(alice, connector.ConfiguredRunRequest{
			SpawnRunRequest: connector.SpawnRunRequest{Agent: "resumer", Segments: []loop.PromptSegment{
				{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do it"}}},
			}},
		})
		if err != nil {
			t.Fatalf("%s: create the draft: %v", tc.name, err)
		}
		if _, err := srv.UpdateConfiguredRun(auth.WithPrincipal(context.Background(), tc.editor), draft.RunID, json.RawMessage(`{"prompt":"do something else"}`)); err != nil {
			t.Fatalf("%s: edit: %v", tc.name, err)
		}
		if _, err := srv.StartConfiguredRun(alice, draft.RunID, connector.RunSecrets{}); err != nil {
			t.Fatalf("%s: start: %v", tc.name, err)
		}
		calls := prov.calls()
		if len(calls) != 1 || calls[0].noUserScope != tc.withhold {
			t.Errorf("%s: the draft alice started ran with %+v, want no-user-scope=%t", tc.name, calls, tc.withhold)
		}
	}
}
