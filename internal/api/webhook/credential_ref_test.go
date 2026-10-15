package webhook

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A webhook run's credential references were named by the definition's
// author, and its user_id may come from the delivery: the run says so, so its
// "$cred:" values resolve at tenant level only.
//
// A reference from the BODY is the sender's choice of which tenant secret to
// send, so it is never taken; a literal per-delivery token still is.
func TestBuildRunInput_CredentialReferencesAreTheAuthorsOnly(t *testing.T) {
	w := config.Webhook{
		Agent:           "worker",
		UserCredentials: map[string]string{"gh": "$cred:tok"},
	}
	proj := projectResult{Fields: map[string]string{
		"user_id":                   "mallory",
		"user_credentials.gh":       "$cred:prod-admin",
		"user_credentials.app":      "Bearer $cred:other",
		"user_credentials.ghapp":    "$ghapp:installer",
		"user_credentials.per_call": "a-literal-token",
	}}
	var logged []string
	in := buildRunInput(w, proj, map[string]bool{}, func(string) string { return "" }, func(format string, args ...any) {
		logged = append(logged, format)
	})

	if !in.CredentialRefsAtTenant {
		t.Error("a webhook run does not say its credential references are its definition's")
	}
	if got := in.UserCredentials["gh"]; got != "$cred:tok" {
		t.Errorf("gh = %q, want the definition's reference (the body's must not replace it)", got)
	}
	for _, k := range []string{"app", "ghapp"} {
		if v, ok := in.UserCredentials[k]; ok {
			t.Errorf("%s = %q came from the body, want it refused", k, v)
		}
	}
	if got := in.UserCredentials["per_call"]; got != "a-literal-token" {
		t.Errorf("per_call = %q, want the body's literal token", got)
	}
	if len(logged) == 0 {
		t.Error("a refused body reference was not logged")
	}
}
