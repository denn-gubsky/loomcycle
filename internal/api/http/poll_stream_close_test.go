package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// A background child that ends after its parent's response has been written
// in full still reports through the parent's emit — here its parent's
// subagent_stop hook decision. Nothing may reach the parent's ResponseWriter
// then: its handler has returned, and the write dereferenced the buffer
// net/http had already released — a nil-pointer panic the child's recover
// hid, ending the child as "internal error".
func TestPollMode_AChildsEventAfterItsParentsStreamEndedIsNotWritten(t *testing.T) {
	logOf, restore := captureLog(t)
	defer restore()
	hook := newRecordingHook(t, `{"additional_context":"(late check)"}`)
	prov := newBGFamily(
		agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"late one","mode":"poll","on_parent_end":"cancel"}`),
		answer("not waiting"),
	)
	late := make(chan struct{})
	var lateOnce sync.Once
	endLate := func() { lateOnce.Do(func() { close(late) }) }
	prov.late = late
	srv, _ := makeServer(t, prov, bgResumeConfig(hook.srv.URL))
	srv.resetTestHooks()
	settle(t, srv, prov)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	defer endLate()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"lead","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	stream := string(b)
	i := strings.Index(stream, `"run_id":"`)
	if i < 0 {
		t.Fatalf("no run_id in the stream:\n%s", stream)
	}
	leadID := stream[i+len(`"run_id":"`):]
	leadID = leadID[:strings.Index(leadID, `"`)]
	lead, err := srv.store.GetRun(context.Background(), leadID)
	if err != nil {
		t.Fatal(err)
	}

	// The response is complete, so its handler has returned. Only now does
	// the child end, and its parent's hook decide on it.
	if n := srv.liveChildren.Alive(lead.ID); n != 1 {
		t.Fatalf("%d live children once the parent's response ended, want the late one", n)
	}
	endLate()
	waitFor(t, "the late child to end", func() bool { return srv.liveChildren.Alive(lead.ID) == 0 })
	logs := logOf()
	if !strings.Contains(logs, "hooks: subagent_stop") {
		t.Fatalf("the parent's subagent_stop hook never decided on the late child:\n%s", logs)
	}
	if strings.Contains(logs, "panicked") {
		t.Fatalf("the child panicked writing to its parent's ended stream:\n%s", logs)
	}
}

// RunOnce — what the configured-run, gRPC, MCP, webhook and A2A callers drive
// — never calls its caller's OnEvent once it has returned: a background child
// winding down after its parent's end still emits through the parent's emit,
// into a callback whose transport may be gone.
func TestRunOnce_CallsNoCallbackAfterItReturns(t *testing.T) {
	logOf, restore := captureLog(t)
	defer restore()
	hook := newRecordingHook(t, `{"additional_context":"(late check)"}`)
	prov := newBGFamily(
		agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"late one","mode":"poll","on_parent_end":"cancel"}`),
		answer("not waiting"),
	)
	late := make(chan struct{})
	var lateOnce sync.Once
	endLate := func() { lateOnce.Do(func() { close(late) }) }
	prov.late = late
	srv, _ := makeServer(t, prov, bgResumeConfig(hook.srv.URL))
	srv.resetTestHooks()
	settle(t, srv, prov)
	defer endLate()

	var mu sync.Mutex
	returned := false
	var leadID string
	var after []providers.EventType
	err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "lead", UserID: "alice",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}, runner.RunCallbacks{
		OnRegistered: func(_, runID, _, _ string) { leadID = runID },
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			defer mu.Unlock()
			if returned {
				after = append(after, ev.Type)
			}
		},
	})
	mu.Lock()
	returned = true
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.liveChildren.Alive(leadID); n != 1 {
		t.Fatalf("%d live children once RunOnce returned, want the late one", n)
	}
	endLate()
	waitFor(t, "the late child to end", func() bool { return srv.liveChildren.Alive(leadID) == 0 })
	if logs := logOf(); !strings.Contains(logs, "hooks: subagent_stop") {
		t.Fatalf("the parent's subagent_stop hook never decided on the late child:\n%s", logs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(after) > 0 {
		t.Errorf("OnEvent was called after RunOnce returned, with %v", after)
	}
}
