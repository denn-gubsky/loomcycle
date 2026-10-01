package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A registered agent (register_agent) has no versions: registering its name
// again rewrites the row in place. A run records a digest of the row it
// started on, and a resume refuses a row that has changed since.

func registerWorker(t *testing.T, f *versionFixture, prompt string, toolNames ...string) {
	t.Helper()
	if _, err := f.srv.RegisterAgent(context.Background(), connector.RegisterAgentRequest{
		Name: "worker", SystemPrompt: prompt, Tools: toolNames, Provider: "scripted", Model: "stub-model",
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
}

func TestResumedRun_WhoseRegisteredAgentWasChangedFailsWithoutRunning(t *testing.T) {
	// Live: one turn that ends. A resume that ran would call Wider.
	f := newVersionFixture(t, versionConfig(), endTurn(), toolCallTurn("tu_w", "Wider", `{}`), endTurn())
	registerWorker(t, f, "prompt v1", "Narrow")
	run := postAgentRun(t, f.st, f.ts, "worker")
	if live := offered(f.prov.waitForRequests(t, 1)[0]); hasTool(live, "Wider") || !hasTool(live, "Narrow") {
		t.Fatalf("fixture drifted: the live run was offered %v", live)
	}
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.AgentVersion == nil || rec.AgentVersion.RegisteredSHA256 == "" {
		t.Fatalf("the run recorded no digest of the registered agent it started on: %s", run.RunConfig)
	}

	registerWorker(t, f, "prompt v2", "Narrow", "Wider")
	parkForResume(t, f.srv, run.ID)
	ctx := context.Background()
	// The row finished once already (live), so a failure cannot be read off
	// its status; the resume pass says why it refused.
	n, warnings := f.srv.ResumePausedRuns(ctx)
	if n != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "changed or removed since it started") {
		t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning that the registered agent changed", n, warnings)
	}
	if n := len(f.prov.requests()); n != 1 {
		t.Errorf("the provider saw %d calls, want only the live one — the run resumed on the changed registration", n)
	}
	if n := f.wider.calls.Load(); n != 0 {
		t.Errorf("Wider ran %d time(s)", n)
	}
}

// The digest must not fail a run whose registration is as it was.
func TestResumedRun_OfAnUnchangedRegisteredAgentResumes(t *testing.T) {
	f := newVersionFixture(t, versionConfig(), endTurn(), toolCallTurn("tu_n", "Narrow", `{}`), endTurn())
	registerWorker(t, f, "prompt v1", "Narrow")
	run := postAgentRun(t, f.st, f.ts, "worker")
	f.prov.waitForRequests(t, 1)

	resumeAndFinish(t, f.srv, run)
	assertResumedOnV1(t, f, 1)
	if got, _ := f.st.GetRun(context.Background(), run.ID); got.Status != store.RunCompleted {
		t.Errorf("the resumed run ended %q (%s), want completed", got.Status, got.ErrorMsg)
	}
}

// A run recorded before the digest existed carries none, and resumes by name
// as it always did — even on a registration changed since.
func TestResumedRun_OfARegisteredAgentWithNoRecordedDigestResumesByName(t *testing.T) {
	f := newVersionFixture(t, versionConfig(), toolCallTurn("tu_n", "Narrow", `{}`), endTurn())
	registerWorker(t, f, "prompt v1", "Narrow")
	ctx := context.Background()
	sess, err := f.st.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{AgentVersion: &agentVersionRecord{}} // a pre-digest record
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_predigest", UserID: "alice", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registerWorker(t, f, "prompt v1 edited", "Narrow")

	resumeAndFinish(t, f.srv, run)
	if got, _ := f.st.GetRun(ctx, run.ID); got.Status != store.RunCompleted {
		t.Errorf("a pre-digest run ended %q (%s), want completed — resumed by name", got.Status, got.ErrorMsg)
	}
}
