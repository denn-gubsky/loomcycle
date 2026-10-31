package main

import (
	"fmt"
	"strings"
	"testing"
)

// The lease and the ingest queue of kb/linker, run through the real loop against
// the fixture store and queue. These are the pass's exit paths: what is released,
// what is acked, and what a busy or lost scope stops.

// callTrace renders the calls as "Tool.op" (plus an ack's cursor), for asserting
// order.
func callTrace(f *kbFixture) []string {
	var out []string
	for _, c := range f.calls {
		s := c.Tool + "." + c.Op
		if c.Tool == "Agent" {
			s = "Agent.spawn"
		}
		if c.Op == "ack" {
			s += "(" + fmt.Sprint(c.Input["cursor"]) + ")"
		}
		out = append(out, s)
	}
	return out
}

func indexOf(trace []string, s string) int {
	for i, v := range trace {
		if v == s {
			return i
		}
	}
	return -1
}

// TestKBLinker_LeaseIsReleasedOnTheFailurePath: the lease is the first call, and
// a pass that fails after it has written still releases it as its last.
func TestKBLinker_LeaseIsReleasedOnTheFailurePath(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
	f.failOps["list_statements"] = "list_statements: the database is locked"

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2","L6"]}`)

	if rep["status"] != "failed" || !strings.Contains(rep["failure"].(string), "database is locked") {
		t.Fatalf("status = %v failure = %v, want the pass failed on list_statements", rep["status"], rep["failure"])
	}
	trace := callTrace(f)
	if trace[0] != "Document.lease" {
		t.Errorf("first call = %s, want the lease", trace[0])
	}
	if indexOf(trace, "Document.propose_statement") < 0 {
		t.Fatalf("no write before the failure — the test needs a pass that fails mid-way: %v", trace)
	}
	if trace[len(trace)-1] != "Document.release" {
		t.Errorf("last call = %s, want the release; trace %v", trace[len(trace)-1], trace)
	}
	if n := len(f.callsOf("Document", "release")); n != 1 {
		t.Errorf("release called %d times, want once", n)
	}
	if lease := rep["lease"].(map[string]any); lease["released"] != true {
		t.Errorf("lease = %v, want released", lease)
	}
	if f.leaseHeld {
		t.Error("the fixture still holds the lease after the pass")
	}
}

// TestKBLinker_BusyLeaseWritesNothing: another pass holds the scope. This one
// costs one call, writes nothing, reads nothing, acks nothing, and does not
// release a lease that is not its own.
func TestKBLinker_BusyLeaseWritesNothing(t *testing.T) {
	for name, input := range map[string]string{
		"document": `{"document_id":"doc-target","write":"propose"}`,
		"drain":    `{"mode":"drain","write":"propose"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := kbBaseFixture()
			f.ontology = kbOntology()
			f.leaseBusy = true
			f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
			f.statements = []*kbStatement{citeStatement("st-gone", "c1", "x-deleted")}
			f.queue = []kbMessage{{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "created"}}}

			rep, _ := runLinker(t, f, input)

			if rep["status"] != "busy" {
				t.Errorf("status = %v, want busy", rep["status"])
			}
			if trace := callTrace(f); len(trace) != 1 || trace[0] != "Document.lease" {
				t.Errorf("calls = %v, want the lease alone", trace)
			}
			lease := rep["lease"].(map[string]any)
			if lease["acquired"] != false || lease["held_by"] != "run-other" {
				t.Errorf("lease = %v, want not acquired, held by run-other", lease)
			}
		})
	}
}

// TestKBLinker_RefusedLeaseFailsWithoutReading: until a code agent may hold the
// lease, the runtime refuses it, and a writing pass fails before any read.
func TestKBLinker_RefusedLeaseFailsWithoutReading(t *testing.T) {
	f := kbBaseFixture()
	f.leaseRefused = true

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose"}`)

	if rep["status"] != "failed" || !strings.HasPrefix(rep["failure"].(string), "lease: ") {
		t.Errorf("status = %v failure = %v, want failed on the lease", rep["status"], rep["failure"])
	}
	if trace := callTrace(f); len(trace) != 1 {
		t.Errorf("calls = %v, want the lease alone", trace)
	}
}

// TestKBLinker_LostLeaseStopsWriting: a renewal that finds another holder means
// the scope is no longer this pass's. It writes nothing more and does not
// release what it no longer holds.
func TestKBLinker_LostLeaseStopsWriting(t *testing.T) {
	f := maintainFixture()
	for i := 0; i < 40; i++ {
		f.statements = append(f.statements, citeStatement(fmt.Sprintf("st-%02d", i), "c1", "x1", "report a material incident"))
		f.statements[i].Discriminator = fmt.Sprintf("d%d", i)
		f.statements[i].Kind = "amends"
	}
	for _, r := range f.ontology["relations"].([]any) {
		if r.(map[string]any)["name"] == "amends" {
			r.(map[string]any)["discriminator"] = "anchor"
		}
	}
	f.leaseLostAt = 2 // the first renewal

	rep, _ := runLinker(t, f, maintainInput)

	trace := callTrace(f)
	renewal := -1
	for i, s := range trace {
		if s == "Document.lease" && i > 0 {
			renewal = i
			break
		}
	}
	if renewal < 0 {
		t.Fatalf("the pass never renewed its lease in %d calls: %v", len(trace), trace)
	}
	for _, s := range trace[renewal+1:] {
		t.Errorf("the pass called %s after losing its lease", s)
	}
	if rep["status"] != "failed" || !strings.Contains(rep["failure"].(string), "lease was lost") {
		t.Errorf("status = %v failure = %v, want failed: lease lost", rep["status"], rep["failure"])
	}
	if lease := rep["lease"].(map[string]any); lease["lost"] != true || lease["released"] != nil {
		t.Errorf("lease = %v, want lost and never released", lease)
	}
}

// TestKBLinker_DrainAcksEachMessageOnlyAfterItsPass: each message is read, its
// pass runs and is recorded, and only then is it acked. A malformed message and
// one naming a document the scope no longer holds are acked as such; a pass that
// fails leaves its message unacked and stops the drain, since acking any later
// message would consume it.
func TestKBLinker_DrainAcksEachMessageOnlyAfterItsPass(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
	f.docFails["doc-two"] = "get_document: the store is unavailable"
	f.queue = []kbMessage{
		{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "created", "timestamp": 1}},
		{ID: "msg-2", Value: map[string]any{"nope": true}},
		{ID: "msg-3", Value: map[string]any{"document_id": "doc-gone", "reason": "imported"}},
		{ID: "msg-4", Value: map[string]any{"document_id": "doc-two", "reason": "created"}},
		{ID: "msg-5", Value: map[string]any{"document_id": "doc-target", "reason": "created"}},
	}

	rep, _ := runLinker(t, f, `{"mode":"drain","write":"propose"}`)

	trace := callTrace(f)
	// Between an ack and the next await there is no store work: each pass is done
	// before its message is acked.
	for i, s := range trace {
		if !strings.HasPrefix(s, "Channel.ack") {
			continue
		}
		if i+1 < len(trace) && trace[i+1] != "Channel.await" && trace[i+1] != "Document.release" {
			t.Errorf("after %s came %s — the pass ran on after its message was acked; trace %v", s, trace[i+1], trace)
		}
	}
	if p, a := indexOf(trace, "Document.propose_statement"), indexOf(trace, "Channel.ack(q-1)"); p < 0 || a < p {
		t.Errorf("msg-1's write (at %d) must come before its ack (at %d); trace %v", p, a, trace)
	}
	for _, want := range []string{"Channel.ack(q-1)", "Channel.ack(q-2)", "Channel.ack(q-3)"} {
		if indexOf(trace, want) < 0 {
			t.Errorf("missing %s; trace %v", want, trace)
		}
	}
	if indexOf(trace, "Channel.ack(q-4)") >= 0 || indexOf(trace, "Channel.ack(q-5)") >= 0 {
		t.Errorf("the failed message or a later one was acked; trace %v", trace)
	}
	if n := len(f.callsOf("Channel", "await")); n != 4 {
		t.Errorf("await called %d times, want 4 — the drain stops at the failed pass", n)
	}
	if f.committed != 3 {
		t.Errorf("committed cursor = %d, want 3 (msg-4 stays unconsumed)", f.committed)
	}
	msgs := rep["messages"].([]any)
	want := []struct {
		outcome string
		acked   bool
	}{
		{"complete", true}, {"malformed", true}, {"document_missing", true}, {"failed", false},
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %v, want %d", msgs, len(want))
	}
	for i, w := range want {
		m := msgs[i].(map[string]any)
		if m["outcome"] != w.outcome || m["acked"] != w.acked {
			t.Errorf("messages[%d] = %v, want outcome %s acked %v", i, m, w.outcome, w.acked)
		}
	}
	if rep["status"] != "failed" || trace[len(trace)-1] != "Document.release" {
		t.Errorf("status = %v, last call %s — want failed, with the lease released", rep["status"], trace[len(trace)-1])
	}
}

// TestKBLinker_DrainStopsUnackedWhenTheBudgetRunsOut: a pass cut by the write
// budget did not finish, so its message is left for the next drain. The budget
// is two: L0's classify, then one of the two citations.
func TestKBLinker_DrainStopsUnackedWhenTheBudgetRunsOut(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citations = []map[string]any{
		kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
		kbCiteHit("report a material incident", map[string]any{"resolved": resolvedTo("x1")}),
	}
	f.queue = []kbMessage{{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "created"}}}

	rep, _ := runLinker(t, f, `{"mode":"drain","write":"propose","max_writes":2}`)

	if n := len(f.callsOf("Document", "propose_statement")); n != 1 {
		t.Errorf("proposals = %d, want 1 — the budget caps writes", n)
	}
	if n := len(f.callsOf("Channel", "ack")); n != 0 {
		t.Errorf("acked %d messages — a pass the budget cut is not done", n)
	}
	if rep["status"] != "budget_stopped" {
		t.Errorf("status = %v, want budget_stopped", rep["status"])
	}
	if m := rep["messages"].([]any)[0].(map[string]any); m["outcome"] != "budget_stopped" || m["acked"] != false {
		t.Errorf("message = %v, want budget_stopped and unacked", m)
	}
}

// TestKBLinker_ReimportReadsOnlyTheChangedParts: a reimport names the parts that
// changed. Citations and terms are read for those parts only — and only for parts
// of that document — while maintenance still covers the whole document.
func TestKBLinker_ReimportReadsOnlyTheChangedParts(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.chunks = append(f.chunks, kbChunk{ID: "c2", DocumentID: "doc-target", Type: "section", Body: kbBodySelf})
	f.queue = []kbMessage{{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "reimported",
		"changed_chunk_ids": []any{"c2", "x1"}}}}

	rep, _ := runLinker(t, f, `{"mode":"drain","write":"propose"}`)

	var read []string
	for _, c := range f.callsOf("Document", "extract_citations") {
		if c.Input["document_id"] != nil {
			t.Errorf("a reimport read the whole document's citations: %v", c.Input)
		}
		read = append(read, fmt.Sprint(c.Input["id"]))
	}
	if strings.Join(read, ",") != "c2" {
		t.Errorf("parts read = %v, want [c2] — x1 is another document's chunk", read)
	}
	if n := len(f.callsOf("Document", "list_statements")); n == 0 {
		t.Error("maintenance did not run on the reimported document")
	}
	if rep["status"] != "complete" || f.committed != 1 {
		t.Errorf("status = %v committed = %d, want complete and the message acked", rep["status"], f.committed)
	}
}

// TestKBLinker_ReportModeDrainWalksTheQueueWithoutAcking: report mode reads the
// queue from the committed cursor with its own cursor, runs every pass in report
// mode, and consumes nothing.
func TestKBLinker_ReportModeDrainWalksTheQueueWithoutAcking(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
	f.queue = []kbMessage{
		{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "created"}},
		{ID: "msg-2", Value: map[string]any{"document_id": "doc-target", "reason": "imported"}},
	}

	rep, _ := runLinker(t, f, `{"mode":"drain"}`)

	assertNoWrites(t, f, rep)
	awaits := f.callsOf("Channel", "await")
	if len(awaits) != 3 {
		t.Fatalf("await calls = %d, want 3 (two messages, then an empty queue)", len(awaits))
	}
	if awaits[0].Input["from_cursor"] != nil || awaits[1].Input["from_cursor"] != "q-1" || awaits[2].Input["from_cursor"] != "q-2" {
		t.Errorf("await cursors = %v %v %v, want committed, q-1, q-2", awaits[0].Input["from_cursor"],
			awaits[1].Input["from_cursor"], awaits[2].Input["from_cursor"])
	}
	if f.committed != 0 {
		t.Errorf("committed = %d, want 0 — report mode consumes nothing", f.committed)
	}
	q := rep["queue"].(map[string]any)
	if q["read"] != float64(2) || q["acked"] != float64(0) || q["empty"] != true {
		t.Errorf("queue = %v, want 2 read, 0 acked, empty", q)
	}
	if ww := rep["would_write"].(map[string]any); ww["propose_statement"] != float64(2) {
		t.Errorf("would_write = %v, want two citation proposals counted", ww)
	}
}

// TestKBLinker_SubjectDrainRunsOneSubjectPassPerMessage: at scope tenant, drain reads
// the memory/subjects queue and answers each message with an L6 subject pass:
//   - merged re-points what remains on the duplicate to the survivor;
//   - removed has nothing left in the scope to check;
//   - an unknown op is malformed;
//   - a later message's pass does not inherit an earlier merge's survivor.
//
// Each is acked only after its pass, on the subject queue.
func TestKBLinker_SubjectDrainRunsOneSubjectPassPerMessage(t *testing.T) {
	f := subjectFixture()
	markMerged(f, "c-root", "doc-surv")
	f.queueChannel = "memory/subjects"
	f.queue = []kbMessage{
		{ID: "msg-1", Value: map[string]any{"op": "merged", "subject_natural_key": "person:ada",
			"document_id": "doc-target", "surviving_document_id": "doc-surv"}},
		{ID: "msg-2", Value: map[string]any{"op": "removed", "document_id": "doc-gone"}},
		{ID: "msg-3", Value: map[string]any{"op": "renamed", "document_id": "doc-target"}},
		{ID: "msg-4", Value: map[string]any{"op": "merged", "document_id": "doc-target"}},
		{ID: "msg-5", Value: map[string]any{"op": "retyped", "document_id": "doc-target", "old_type": "person", "new_type": "author"}},
	}

	rep, _ := runLinker(t, f, `{"mode":"drain","scope":"tenant","write":"propose"}`)

	got := proposals(f)
	if len(got) != 1 || got[0]["to_id"] != "v-root" {
		t.Fatalf("proposals = %v, want st-mach moved to the survivor once", got)
	}
	if a := actionsOf(f, "st-mach"); !strings.HasPrefix(a, "expire") {
		t.Errorf("st-mach actions = %q, want it expired by the merge", a)
	}
	for _, op := range []string{"extract_citations", "classify", "resolve_anchor"} {
		if n := len(f.callsOf("Document", op)); n != 0 {
			t.Errorf("a subject pass called %s %d times; it runs L6 only", op, n)
		}
	}
	if f.committed != 5 {
		t.Errorf("committed = %d, want all five acked", f.committed)
	}
	if q := rep["queue"].(map[string]any); q["channel"] != "memory/subjects" || q["acked"] != float64(5) {
		t.Errorf("queue = %v, want memory/subjects with five acked", q)
	}
	want := []struct{ op, outcome string }{
		{"merged", "complete"}, {"removed", "nothing_to_check"}, {"", "malformed"}, {"", "malformed"}, {"retyped", "complete"},
	}
	msgs := rep["messages"].([]any)
	if len(msgs) != len(want) {
		t.Fatalf("messages = %v", msgs)
	}
	for i, w := range want {
		m := msgs[i].(map[string]any)
		if m["op"] != w.op || m["outcome"] != w.outcome || m["acked"] != true {
			t.Errorf("messages[%d] = %v, want op %q outcome %s acked", i, m, w.op, w.outcome)
		}
	}
	noViolations(t, f)
}

// TestKBLinker_SubjectDrainReportModeWritesNothing: report mode walks the subject
// queue with its own cursor and consumes nothing, as it does the ingest queue.
func TestKBLinker_SubjectDrainReportModeWritesNothing(t *testing.T) {
	f := subjectFixture()
	markMerged(f, "c-root", "doc-surv")
	f.queueChannel = "memory/subjects"
	f.queue = []kbMessage{{ID: "msg-1", Value: map[string]any{"op": "merged",
		"document_id": "doc-target", "surviving_document_id": "doc-surv"}}}

	rep, _ := runLinker(t, f, `{"mode":"drain","scope":"tenant"}`)

	assertNoWrites(t, f, rep)
	if f.committed != 0 || len(f.callsOf("Channel", "ack")) != 0 {
		t.Errorf("report mode acked: committed %d", f.committed)
	}
	if ww := rep["would_write"].(map[string]any); ww["propose_statement"] != float64(1) {
		t.Errorf("would_write = %v, want the re-point counted", ww)
	}
}

// markMerged records on a subject's root that it was merged into survivor, as the
// merge op writes it before announcing.
func markMerged(f *kbFixture, root, survivor string) {
	for i := range f.chunks {
		if f.chunks[i].ID == root {
			f.chunks[i].Fields = map[string]any{"merged_into": survivor}
			return
		}
	}
	c := f.others[root]
	c.Fields = map[string]any{"merged_into": survivor}
	f.others[root] = c
}

// TestKBLinker_SubjectDrainActsOnlyOnAConfirmedMerge: a merge message moves
// statements only when the duplicate's own record names that survivor. One that
// does not — a forged message, or a merge since undone — is reported, acked and
// moves nothing.
func TestKBLinker_SubjectDrainActsOnlyOnAConfirmedMerge(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"no record":        nil,
		"another survivor": {"merged_into": "doc-other"},
	} {
		t.Run(name, func(t *testing.T) {
			f := subjectFixture()
			if fields != nil {
				markMerged(f, "c-root", fmt.Sprint(fields["merged_into"]))
			}
			f.queueChannel = "memory/subjects"
			f.queue = []kbMessage{{ID: "msg-1", Value: map[string]any{"op": "merged",
				"document_id": "doc-target", "surviving_document_id": "doc-surv"}}}

			rep, _ := runLinker(t, f, `{"mode":"drain","scope":"tenant","write":"propose"}`)

			if got := proposals(f); len(got) != 0 {
				t.Errorf("proposals = %v, want none for an unconfirmed merge", got)
			}
			if a := actionsOf(f, "st-mach"); a != "" {
				t.Errorf("st-mach actions = %q, want untouched", a)
			}
			m := rep["messages"].([]any)[0].(map[string]any)
			if m["outcome"] != "merge_unconfirmed" || m["acked"] != true {
				t.Errorf("message = %v, want merge_unconfirmed and acked", m)
			}
		})
	}
}

// TestKBLinker_DrainStopsAtAMessageOfAnotherScope: a message that names its own
// scope is passed; one that names another scope means the channel is declared at a
// scope this drain does not run at, so the drain stops there with it unacked
// rather than consume it as a missing document. The guard is symmetric: a user
// drain on a deployment that declared the queue tenant-scoped stops the same way.
func TestKBLinker_DrainStopsAtAMessageOfAnotherScope(t *testing.T) {
	f := kbBaseFixture()
	f.queue = []kbMessage{
		{ID: "msg-1", Value: map[string]any{"document_id": "doc-target", "reason": "created", "scope": "user"}},
		{ID: "msg-2", Value: map[string]any{"document_id": "doc-target", "reason": "created", "scope": "tenant"}},
		{ID: "msg-3", Value: map[string]any{"document_id": "doc-target", "reason": "created"}},
	}

	rep, _ := runLinker(t, f, `{"mode":"drain","write":"propose"}`)

	if rep["status"] != "failed" {
		t.Fatalf("status = %v, want failed", rep["status"])
	}
	if fail, _ := rep["failure"].(string); fail != "documents/ingested here holds tenant-scope messages; declare the channel at scope user to drain it there" {
		t.Errorf("failure = %q", fail)
	}
	if f.committed != 1 {
		t.Errorf("committed cursor = %d, want 1 (msg-2 stays unconsumed)", f.committed)
	}
	msgs := rep["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want the drain stopped at msg-2", msgs)
	}
	if m := msgs[0].(map[string]any); m["outcome"] != "complete" || m["acked"] != true {
		t.Errorf("messages[0] = %v, want its own scope's message passed and acked", m)
	}
	if m := msgs[1].(map[string]any); m["outcome"] != "scope_mismatch" || m["acked"] != false || m["document_id"] != "doc-target" {
		t.Errorf("messages[1] = %v, want scope_mismatch, unacked", m)
	}
	if n := len(f.callsOf("Document", "get_document")); n != 1 {
		t.Errorf("get_document called %d times, want 1 — no pass over another scope's message", n)
	}
}
