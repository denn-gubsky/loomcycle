package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// errDiskFull stands in for whatever stops a sink writing — a full disk, a read-only
// mount, a path the process cannot open.
var errDiskFull = errors.New("no space left on device")

func memStoreForTest(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// failingSink refuses every record, standing in for a full disk, a read-only mount, or a
// path the process cannot write.
type failingSink struct{ err error }

func (f failingSink) Record(audit.Event) error { return f.err }

// recordingSink keeps what was written, in order.
type recordingSink struct{ events []audit.Event }

func (r *recordingSink) Record(ev audit.Event) error {
	r.events = append(r.events, ev)
	return nil
}

// TestErasure_RefusesWithoutAnAuditSink.
//
// Every other consumer of the audit sink treats recording as best-effort — audit is
// observability, not a transaction participant. Erasure is the deliberate exception: it
// is the one operation nothing can undo, so a deployment that cannot record WHO erased
// WHICH subject does not get to perform the deletion.
//
// Refusing is recoverable (configure the sink, retry). An unrecorded erasure is not.
func TestErasure_RefusesWithoutAnAuditSink(t *testing.T) {
	s := &Server{store: memStoreForTest(t)}
	// No SetEraseAudit call at all — the shape a deployment with no audit path has.

	rec := httptest.NewRecorder()
	body := `{"subject":"alice","dry_run":false,"confirm":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant=t1", strings.NewReader(body))
	s.handleErasureExecute(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an erasure with nowhere to record it must refuse", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "audit") {
		t.Errorf("the refusal does not say why: %s", rec.Body.String())
	}
}

// TestErasure_RefusesWhenTheRecordCannotBeWritten. A configured sink that fails is the
// same situation as none: the deletion must not happen unrecorded.
func TestErasure_RefusesWhenTheRecordCannotBeWritten(t *testing.T) {
	s := &Server{store: memStoreForTest(t)}
	s.SetEraseAudit(failingSink{err: errDiskFull})

	rec := httptest.NewRecorder()
	body := `{"subject":"alice","dry_run":false,"confirm":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant=t1", strings.NewReader(body))
	s.handleErasureExecute(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestErasure_RecordsIntentBeforeDeletingAndOutcomeAfter.
//
// The ORDER is the point. An intent record written first means a crash mid-erasure still
// leaves evidence that one was attempted, by whom, against which subject — which a single
// after-the-fact record cannot promise for an operation nothing can undo.
func TestErasure_RecordsIntentBeforeDeletingAndOutcomeAfter(t *testing.T) {
	sink := &recordingSink{}
	s := &Server{store: memStoreForTest(t)}
	s.SetEraseAudit(sink)

	rec := httptest.NewRecorder()
	body := `{"subject":"alice","dry_run":false,"confirm":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant=t1", strings.NewReader(body))
	s.handleErasureExecute(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 2 {
		t.Fatalf("wrote %d records, want intent + result: %+v", len(sink.events), sink.events)
	}
	if sink.events[0].Action != "erase_intent" {
		t.Errorf("first record is %q — the intent must be written BEFORE the deletion",
			sink.events[0].Action)
	}
	if sink.events[1].Action != "erase_result" {
		t.Errorf("second record is %q, want erase_result", sink.events[1].Action)
	}
	for i, ev := range sink.events {
		if ev.TargetSubject != "alice" || ev.TargetTenant != "t1" {
			t.Errorf("record %d does not identify the subject/tenant: %+v", i, ev)
		}
	}
	// The outcome carries what went, including what was deliberately kept — a record
	// listing only deletions would read as complete when it is not.
	if sink.events[1].ErasePlanes == nil && sink.events[1].EraseRetained == nil {
		t.Errorf("the result record carries neither planes nor retentions: %+v", sink.events[1])
	}
}

// TestErasure_DryRunWritesNoRecord. A preview removes nothing, and an audit log full of
// previews is one an auditor stops reading.
func TestErasure_DryRunWritesNoRecord(t *testing.T) {
	sink := &recordingSink{}
	s := &Server{store: memStoreForTest(t)}
	s.SetEraseAudit(sink)

	rec := httptest.NewRecorder()
	body := `{"subject":"alice","dry_run":true,"confirm":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant=t1", strings.NewReader(body))
	s.handleErasureExecute(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 0 {
		t.Errorf("a dry run wrote %d audit record(s): %+v", len(sink.events), sink.events)
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if dry, _ := res["dry_run"].(bool); !dry {
		t.Errorf("the response does not report itself as a dry run: %v", res)
	}
}
