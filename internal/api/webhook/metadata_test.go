package webhook

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestBuildRunInput_MetadataTrustedAndPayloadUntrusted pins the non-secret
// metadata sourcing: the static def `metadata` becomes RunInput.Metadata
// (trusted), and payload_mapping `run_metadata.*` targets become
// RunInput.PayloadMetadata (untrusted). Fails on the pre-feature code, which
// had no metadata fields and discarded run_metadata.* projection targets.
func TestBuildRunInput_MetadataTrustedAndPayloadUntrusted(t *testing.T) {
	w := config.Webhook{
		Agent:    "reviewer",
		Metadata: map[string]any{"policy": "strict", "skills": []any{"go"}},
	}
	proj := projectResult{Fields: map[string]string{
		"goal":              "review the PR",
		"run_metadata.repo": "acme/app",
		"run_metadata.pr":   "42",
	}}
	in := buildRunInput(w, proj, map[string]bool{}, func(string) string { return "" }, nil)

	if in.Metadata["policy"] != "strict" {
		t.Errorf("static def metadata not carried as trusted Metadata: %v", in.Metadata)
	}
	if in.PayloadMetadata["repo"] != "acme/app" || in.PayloadMetadata["pr"] != "42" {
		t.Errorf("run_metadata.* not routed to PayloadMetadata: %v", in.PayloadMetadata)
	}
}

// TestBuildRunInput_EmptyProjectedValuesSkipped pins that an absent optional
// body path (projected to "") does NOT surface as a phantom {name:""} entry —
// neither a blanked credential nor an empty run_metadata key. Fails on the
// pre-fix run_metadata loop, which lacked the val=="" skip the credentials
// loop had, so it injected {repo:""} into PayloadMetadata.
func TestBuildRunInput_EmptyProjectedValuesSkipped(t *testing.T) {
	w := config.Webhook{
		Agent:                  "x",
		UserCredentialsFromEnv: map[string]string{"slack": "SLACK_ENV"},
	}
	proj := projectResult{Fields: map[string]string{
		"run_metadata.repo":      "acme/app",
		"run_metadata.pr":        "", // absent optional path
		"user_credentials.slack": "", // absent — must not clobber the env value
	}}
	env := map[string]bool{"SLACK_ENV": true}
	getenv := func(k string) string { return map[string]string{"SLACK_ENV": "env-slack"}[k] }
	in := buildRunInput(w, proj, env, getenv, nil)

	if _, ok := in.PayloadMetadata["pr"]; ok {
		t.Errorf("empty run_metadata.pr must be skipped, not injected as \"\": %v", in.PayloadMetadata)
	}
	if in.PayloadMetadata["repo"] != "acme/app" {
		t.Errorf("non-empty run_metadata.repo should still be present: %v", in.PayloadMetadata)
	}
	if in.UserCredentials["slack"] != "env-slack" {
		t.Errorf("empty payload credential must not clobber the env value; got %q", in.UserCredentials["slack"])
	}
}

// TestBuildRunInput_UserCredentialsPrecedence pins env < fork-time < payload.
func TestBuildRunInput_UserCredentialsPrecedence(t *testing.T) {
	w := config.Webhook{
		Agent:                  "x",
		UserCredentialsFromEnv: map[string]string{"slack": "SLACK_ENV", "jobs": "JOBS_ENV"},
		UserCredentials:        map[string]string{"slack": "fork-slack", "tg": "fork-tg"},
	}
	proj := projectResult{Fields: map[string]string{
		"user_credentials.slack": "payload-slack",
	}}
	env := map[string]bool{"SLACK_ENV": true, "JOBS_ENV": true}
	getenv := func(k string) string {
		return map[string]string{"SLACK_ENV": "env-slack", "JOBS_ENV": "env-jobs"}[k]
	}
	in := buildRunInput(w, proj, env, getenv, nil)

	if in.UserCredentials["slack"] != "payload-slack" {
		t.Errorf("slack: payload must win over fork-time + env; got %q", in.UserCredentials["slack"])
	}
	if in.UserCredentials["jobs"] != "env-jobs" {
		t.Errorf("jobs: env-only should resolve to env value; got %q", in.UserCredentials["jobs"])
	}
	if in.UserCredentials["tg"] != "fork-tg" {
		t.Errorf("tg: fork-time-only should resolve to fork value; got %q", in.UserCredentials["tg"])
	}
}
