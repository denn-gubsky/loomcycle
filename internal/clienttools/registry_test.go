package clienttools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// echoSender returns a send closure that, on each invoke, replies ok with the
// echoed input via DeliverResult on the conn *connp points to (set after
// Register). tag, when non-empty, is returned as the output instead (to tell
// two connections apart).
func echoSender(connp **Conn, tag string) func(context.Context, any) error {
	return func(_ context.Context, v any) error {
		f, ok := v.(InvokeFrame)
		if !ok {
			return nil
		}
		out := f.Input
		if tag != "" {
			out = json.RawMessage(`"` + tag + `"`)
		}
		go (*connp).DeliverResult(ResultFrame{Type: FrameResult, CallID: f.CallID, OK: true, Output: out})
		return nil
	}
}

func TestRegistry_InvokeRoundTrip(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	var c *Conn
	conn, dereg, err := r.Register(key, []ToolSchema{{Name: "browser.read_page"}}, echoSender(&c, ""))
	if err != nil {
		t.Fatal(err)
	}
	c = conn
	defer dereg()

	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}
	res, err := r.Invoke(context.Background(), key, "browser.read_page", json.RawMessage(`{"q":1}`), InvokeMeta{RunID: "r1"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK || string(res.Output) != `{"q":1}` {
		t.Errorf("result = %+v, want ok + echoed input", res)
	}
}

func TestRegistry_NoClient(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	// A connection exists but doesn't provide the tool → still ErrNoClient.
	var c *Conn
	conn, dereg, _ := r.Register(key, []ToolSchema{{Name: "browser.click"}}, echoSender(&c, ""))
	c = conn
	defer dereg()

	if _, err := r.Invoke(context.Background(), key, "browser.read_page", nil, InvokeMeta{}); !errors.Is(err, ErrNoClient) {
		t.Errorf("want ErrNoClient for an unprovided tool, got %v", err)
	}
	// A different principal → ErrNoClient.
	if _, err := r.Invoke(context.Background(), PrincipalKey{"t1", "other"}, "browser.click", nil, InvokeMeta{}); !errors.Is(err, ErrNoClient) {
		t.Errorf("want ErrNoClient for a different principal, got %v", err)
	}
}

func TestRegistry_DisconnectFailsPending(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	// A sender that never replies — the invoke blocks until dereg.
	silent := func(context.Context, any) error { return nil }
	_, dereg, _ := r.Register(key, []ToolSchema{{Name: "fs.read"}}, silent)

	got := make(chan error, 1)
	go func() {
		_, err := r.Invoke(context.Background(), key, "fs.read", nil, InvokeMeta{})
		got <- err
	}()
	// Give the invoke time to register its pending waiter, then disconnect.
	time.Sleep(20 * time.Millisecond)
	dereg()

	select {
	case err := <-got:
		if !errors.Is(err, ErrClientDisconnected) {
			t.Errorf("want ErrClientDisconnected on mid-call disconnect, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Invoke did not unblock on disconnect (hang)")
	}
}

func TestRegistry_Timeout(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	silent := func(context.Context, any) error { return nil }
	_, dereg, _ := r.Register(key, []ToolSchema{{Name: "fs.read"}}, silent)
	defer dereg()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := r.Invoke(ctx, key, "fs.read", nil, InvokeMeta{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

func TestRegistry_MostRecentWins(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	var c1, c2 *Conn
	conn1, d1, _ := r.Register(key, []ToolSchema{{Name: "browser.read_page"}}, echoSender(&c1, "first"))
	c1 = conn1
	defer d1()
	conn2, d2, _ := r.Register(key, []ToolSchema{{Name: "browser.read_page"}}, echoSender(&c2, "second"))
	c2 = conn2
	defer d2()

	res, err := r.Invoke(context.Background(), key, "browser.read_page", nil, InvokeMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `"second"` {
		t.Errorf("most-recently-registered should win; got %s", res.Output)
	}
	if r.Count() != 2 {
		t.Errorf("Count = %d, want 2", r.Count())
	}
}

func TestRegistry_MaxPerKey(t *testing.T) {
	r := NewRegistry(2)
	key := PrincipalKey{"t1", "u1"}
	silent := func(context.Context, any) error { return nil }
	_, d1, err := r.Register(key, nil, silent)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.Register(key, nil, silent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(key, nil, silent); !errors.Is(err, ErrTooManyConnections) {
		t.Errorf("want ErrTooManyConnections at cap, got %v", err)
	}
	// Freeing one slot lets a new connection in.
	d1()
	if _, _, err := r.Register(key, nil, silent); err != nil {
		t.Errorf("register after freeing a slot should succeed, got %v", err)
	}
}

func TestRegistry_ProvidesUnion(t *testing.T) {
	r := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	silent := func(context.Context, any) error { return nil }
	_, d1, _ := r.Register(key, []ToolSchema{{Name: "browser.read_page"}, {Name: "browser.click"}}, silent)
	defer d1()
	_, d2, _ := r.Register(key, []ToolSchema{{Name: "browser.navigate"}}, silent)
	defer d2()

	names := map[string]bool{}
	for _, s := range r.Provides(key) {
		names[s.Name] = true
	}
	for _, want := range []string{"browser.read_page", "browser.click", "browser.navigate"} {
		if !names[want] {
			t.Errorf("Provides union missing %q; got %v", want, names)
		}
	}
	if len(r.Provides(PrincipalKey{"t1", "nobody"})) != 0 {
		t.Error("Provides for an unknown principal should be empty")
	}
}
