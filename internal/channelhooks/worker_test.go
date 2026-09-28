package channelhooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// fixture is a store, a writer that resolves channel definitions from defs,
// and a worker deciding against the same definitions.
type fixture struct {
	t      *testing.T
	st     store.Store
	w      *Worker
	writer *channels.StorePublisher
	mu     sync.Mutex
	defs   map[string]Def // "tenant|channel"
	hdefs  map[string]hooks.Def
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, defs: map[string]Def{}, hdefs: map[string]hooks.Def{}}
	bus := channels.NewBus()
	f.writer = &channels.StorePublisher{Store: st, Bus: bus, HooksEnabled: true,
		Defs: func(_ context.Context, tenant, ch string) (channels.WriteDef, error) {
			// The channel is operator yaml ("") unless the tenant declared it.
			for _, owner := range []string{tenant, ""} {
				if d, ok := f.def(owner, ch); ok {
					return channels.WriteDef{Hold: d.Hold, Hooked: len(d.Hooks) > 0, HookTenant: owner}, nil
				}
			}
			return channels.WriteDef{}, nil
		}}
	disp := hooks.NewDispatcher(nil, nil)
	disp.SetCodeRunner(codehook.New(nil))
	f.w = New(Config{
		Store: st, Dispatcher: disp, Writer: f.writer, Bus: bus, Owner: "w1", MaxBodyBytes: 1 << 16,
		Lookup: func(_ context.Context, tenant, name string, _ int) (hooks.Def, string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d, ok := f.hdefs[tenant+"|"+name]
			if !ok {
				return hooks.Def{}, "", errors.New("not found")
			}
			return d, "hdf_" + name, nil
		},
		Defs: func(_ context.Context, tenant, ch string) (Def, bool, error) {
			d, ok := f.def(tenant, ch)
			return d, ok, nil
		},
	})
	return f
}

func (f *fixture) def(tenant, ch string) (Def, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.defs[tenant+"|"+ch]
	return d, ok
}

func (f *fixture) setDef(tenant, ch string, d Def) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defs[tenant+"|"+ch] = d
}

func webhookEntry(name, url string) hooks.Entry {
	return hooks.Entry{Inline: &hooks.Inline{Name: name, URL: url, FailMode: hooks.FailClosed}}
}

func chain(entries ...hooks.Entry) hooks.EventHooks {
	return hooks.EventHooks{hooks.PhaseChannelPublish: entries}
}

// answer is a webhook that replies with body and counts its calls.
func answer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func (f *fixture) publish(ch, tenant string, scope store.MemoryScope, payload string, mut func(*channels.WriteRequest)) channels.WriteResult {
	f.t.Helper()
	req := channels.WriteRequest{Channel: ch, TenantID: tenant, Scope: scope, Payload: json.RawMessage(payload), PublishedBy: "u"}
	if mut != nil {
		mut(&req)
	}
	res, err := f.writer.Write(context.Background(), req)
	if err != nil {
		f.t.Fatalf("write: %v", err)
	}
	return res
}

// drain runs the worker until nothing more is due.
func (f *fixture) drain() {
	f.t.Helper()
	for i := 0; i < 20; i++ {
		claimed := f.w.claim(context.Background())
		f.w.wg.Wait()
		if !claimed && f.inFlightNothingDue() {
			return
		}
	}
}

func (f *fixture) inFlightNothingDue() bool {
	now := f.w.now()
	items, _ := f.st.ChannelHookClaim(context.Background(), "probe", now, now, 100)
	return len(items) == 0
}

func (f *fixture) peek(ch, tenant string, scope store.MemoryScope) []store.ChannelMessage {
	f.t.Helper()
	msgs, err := f.st.ChannelPeek(context.Background(), tenant, ch, scope, "", "", 50)
	if err != nil {
		f.t.Fatal(err)
	}
	return msgs
}

// A message on a hooked channel is not delivered until its hooks decide; a
// release delivers it as written.
func TestWorker_ReleaseAsIsDelivers(t *testing.T) {
	f := newFixture(t)
	srv, n := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	res := f.publish("inbox", "acme", store.MemoryScopeUser, `{"text":"hi"}`, func(r *channels.WriteRequest) { r.ScopeID = "u1" })
	if !res.AwaitingHooks || res.Held || !store.IsChannelHookHeld(res.Message.VisibleAt) {
		t.Fatalf("write result = %+v", res)
	}
	msgs, _ := f.st.ChannelPeek(context.Background(), "acme", "inbox", store.MemoryScopeUser, "u1", "", 10)
	if len(msgs) != 0 {
		t.Fatalf("delivered before its hooks decided: %d", len(msgs))
	}
	f.drain()
	msgs, _ = f.st.ChannelPeek(context.Background(), "acme", "inbox", store.MemoryScopeUser, "u1", "", 10)
	if n.Load() != 1 || len(msgs) != 1 || string(msgs[0].Payload) != `{"text":"hi"}` {
		t.Fatalf("calls=%d delivered=%+v", n.Load(), msgs)
	}
}

// A rewrite is what readers get.
func TestWorker_UpdatedBodyIsDelivered(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, `{"updated_body":{"text":"[redacted]"}}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("redact", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"text":"secret"}`, nil)
	f.drain()
	msgs := f.peek("inbox", "", store.MemoryScopeGlobal)
	if len(msgs) != 1 || string(msgs[0].Payload) != `{"text":"[redacted]"}` {
		t.Fatalf("delivered %+v", msgs)
	}
}

// A drop removes the message.
func TestWorker_DropDeletes(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, `{"decision":"drop","reason":"spam"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("a dropped message was delivered: %+v", msgs)
	}
	if items, _ := f.st.ChannelHookClaim(context.Background(), "x", time.Now(), time.Now(), 10); len(items) != 0 {
		t.Fatalf("a dropped message still awaits hooks: %+v", items)
	}
	if f.w.Stats().Decisions["drop"] != 1 {
		t.Fatalf("stats = %+v", f.w.Stats())
	}
}

// Hooks run in the listed order, each seeing what the one before left, and
// the first drop stops the chain.
func TestWorker_ChainOrderAndFirstDropStops(t *testing.T) {
	f := newFixture(t)
	var seen []string
	var mu sync.Mutex
	hook := func(reply string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call hooks.ChannelHookCall
			_ = json.NewDecoder(r.Body).Decode(&call)
			mu.Lock()
			seen = append(seen, call.HookName+":"+string(call.Body))
			mu.Unlock()
			_, _ = w.Write([]byte(reply))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	a, b, c := hook(`{"updated_body":{"n":2}}`), hook(`{"decision":"drop"}`), hook(``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("a", a.URL), webhookEntry("b", b.URL), webhookEntry("c", c.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	f.drain()
	if strings.Join(seen, " ") != `a:{"n":1} b:{"n":2}` {
		t.Fatalf("calls = %v", seen)
	}
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered past the drop: %+v", msgs)
	}
}

// A channel with hooks and a hold: the hooks decide first, and what they
// release lands in the hold, for the operator to release.
func TestWorker_HoldPlusHooksLandsInHold(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "gate", Def{Hold: true, Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("gate", "", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	if msgs := f.peek("gate", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered past the hold: %+v", msgs)
	}
	released, _, err := f.st.ChannelRelease(context.Background(), "", "gate", store.MemoryScopeGlobal, "", 10)
	if err != nil || len(released) != 1 {
		t.Fatalf("the hold has %d (err %v), want the message the hooks released", len(released), err)
	}
}

// A deliver_at survives the wait for hooks: a release lands no earlier.
func TestWorker_DeferredPublishKeepsItsDeliverAt(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	at := time.Now().Add(time.Hour)
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, func(r *channels.WriteRequest) { r.DeliverAt = at })
	f.drain()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered before its deliver_at: %+v", msgs)
	}
	snap, _ := f.st.SnapshotReadChannelMessages(context.Background())
	var got []store.ChannelMessage
	for _, m := range snap {
		if m.Channel == "inbox" {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].VisibleAt.Before(at.Add(-time.Second)) || store.IsChannelReservedVisibleAt(got[0].VisibleAt) {
		t.Fatalf("released %+v, want it at %v", got, at)
	}
}

// An operator's channel is decided by the operator's hooks, whoever
// publishes: a tenant's HookDef of the same name never decides in its place.
func TestWorker_AnOperatorChannelResolvesInTheOperatorsTenant(t *testing.T) {
	f := newFixture(t)
	f.hdefs["|screen"] = hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ return {updated_body: {by: "operator"}}; }`}}
	f.hdefs["acme|screen"] = hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ return {updated_body: {by: "acme"}}; }`}}
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "screen"})})
	f.publish("inbox", "acme", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	msgs := f.peek("inbox", "", store.MemoryScopeGlobal)
	if len(msgs) != 1 || string(msgs[0].Payload) != `{"by":"operator"}` {
		t.Fatalf("delivered %+v", msgs)
	}
}

// A hook that fails open lets the message through; one that fails closed
// holds it and tries again, and drops it once its deadline passes.
func TestWorker_FailModes(t *testing.T) {
	down := "http://127.0.0.1:1/"
	t.Run("open", func(t *testing.T) {
		f := newFixture(t)
		e := hooks.Entry{Inline: &hooks.Inline{Name: "down", URL: down, FailMode: hooks.FailOpen}}
		f.setDef("", "inbox", Def{Hooks: chain(e)})
		f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
		f.drain()
		if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 {
			t.Fatalf("fail-open delivered %d", len(msgs))
		}
	})
	t.Run("closed", func(t *testing.T) {
		f := newFixture(t)
		f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("down", down))})
		f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
		f.drain()
		if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
			t.Fatalf("fail-closed delivered %d", len(msgs))
		}
		items, _ := f.st.ChannelHookClaim(context.Background(), "x", time.Now().Add(time.Hour), time.Now().Add(time.Hour), 10)
		if len(items) != 1 || items[0].Progress.Attempts != 1 || items[0].Progress.LastError == "" {
			t.Fatalf("progress = %+v, want one failed attempt kept for a retry", items)
		}
		// Past its deadline, the next failure drops it.
		_, _ = f.st.ChannelHookSaveProgress(context.Background(), keyOf(items[0].Message), "x", items[0].Progress, time.Now())
		f.w.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
		f.drain()
		if items, _ := f.st.ChannelHookClaim(context.Background(), "x", time.Now().Add(time.Hour), time.Now().Add(time.Hour), 10); len(items) != 0 {
			t.Fatalf("still awaiting after its deadline: %+v", items)
		}
		if f.w.Stats().Decisions["drop"] != 1 {
			t.Fatalf("stats = %+v", f.w.Stats())
		}
	})
}

func keyOf(m store.ChannelMessage) store.ChannelMessageKey {
	return store.ChannelMessageKey{TenantID: m.TenantID, Channel: m.Channel, Scope: m.Scope, ScopeID: m.ScopeID, ID: m.ID}
}

// A reference that no longer resolves fails closed, whatever the fail mode
// says: a gate the channel names must not vanish.
func TestWorker_AVanishedReferenceFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "gone"})})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 {
		t.Fatalf("delivered past a missing hook: %d", len(msgs))
	}
}

// Hooks taken off the channel after a message was written: nothing is left
// to decide, and it is delivered as written.
func TestWorker_HooksRemovedReleasesAsWritten(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, `{"decision":"drop"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"a":1}`, nil)
	f.setDef("", "inbox", Def{})
	f.drain()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 1 {
		t.Fatalf("delivered %d", len(msgs))
	}
}

func sinkPayload() string {
	return `{"wave":"w1","wave_size":2,"index":0,"agent":"a","run_id":"r1","status":"ok","output":"the answer"}`
}

// A drop on a Starter's result is delivered as an error result, never
// removed: the downstream fan-in counts one per run.
func TestWorker_SinkDropDeliversStatusError(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, `{"decision":"drop","reason":"leaks a secret"}`)
	f.setDef("", "sink", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("sink", "", store.MemoryScopeGlobal, sinkPayload(), func(r *channels.WriteRequest) { r.Origin = channels.OriginStarterSink })
	f.drain()
	msgs := f.peek("sink", "", store.MemoryScopeGlobal)
	if len(msgs) != 1 {
		t.Fatalf("delivered %d, want the error result", len(msgs))
	}
	var got map[string]any
	_ = json.Unmarshal(msgs[0].Payload, &got)
	if got["status"] != "error" || got["run_id"] != "r1" || got["index"] != float64(0) || got["output"] != nil ||
		!strings.Contains(got["error"].(string), "leaks a secret") {
		t.Fatalf("delivered %s", msgs[0].Payload)
	}
}

// A rewrite of a Starter's result keeps its envelope: a hook may change what
// a run produced, never which run it was.
func TestWorker_SinkRewriteKeepsTheEnvelope(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, `{"updated_body":{"status":"ok","output":"clean","index":9,"run_id":"forged"}}`)
	f.setDef("", "sink", Def{Hooks: chain(webhookEntry("redact", srv.URL))})
	f.publish("sink", "", store.MemoryScopeGlobal, sinkPayload(), func(r *channels.WriteRequest) { r.Origin = channels.OriginStarterSink })
	f.drain()
	msgs := f.peek("sink", "", store.MemoryScopeGlobal)
	var got map[string]any
	_ = json.Unmarshal(msgs[0].Payload, &got)
	if got["output"] != "clean" || got["run_id"] != "r1" || got["index"] != float64(0) || got["wave"] != "w1" {
		t.Fatalf("delivered %s", msgs[0].Payload)
	}
}

// A Starter's result is due before its TTL: a hook that keeps failing past
// that point delivers it as an error, rather than letting it expire unseen.
func TestWorker_SinkDecidedBeforeItsTTL(t *testing.T) {
	f := newFixture(t)
	f.setDef("", "sink", Def{Hooks: chain(webhookEntry("down", "http://127.0.0.1:1/"))})
	res := f.publish("sink", "", store.MemoryScopeGlobal, sinkPayload(), func(r *channels.WriteRequest) {
		r.Origin, r.ExpiresAt = channels.OriginStarterSink, time.Now().Add(20*time.Second)
	})
	if d := f.w.deadlineFor(res.Message); !d.Before(res.Message.ExpiresAt) {
		t.Fatalf("deadline %v is not before the TTL %v", d, res.Message.ExpiresAt)
	}
	f.w.now = func() time.Time { return time.Now().Add(19 * time.Second) }
	f.drain()
	msgs := f.peek("sink", "", store.MemoryScopeGlobal)
	if len(msgs) != 1 || !strings.Contains(string(msgs[0].Payload), `"status":"error"`) {
		t.Fatalf("delivered %+v", msgs)
	}
}

// A hold keeps the message from every reader; nobody can answer it yet, so
// it is dropped at its deadline.
func TestWorker_AHoldWaitsThenDrops(t *testing.T) {
	f := newFixture(t)
	srv, n := answer(t, `{"decision":"hold","reason":"needs a look"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", srv.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 0 || n.Load() != 1 {
		t.Fatalf("delivered %d, calls %d", len(msgs), n.Load())
	}
	f.w.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	f.drain()
	if n.Load() != 1 {
		t.Fatalf("the hook was asked again: %d calls", n.Load())
	}
	if items, _ := f.st.ChannelHookClaim(context.Background(), "x", time.Now().Add(time.Hour), time.Now().Add(time.Hour), 10); len(items) != 0 {
		t.Fatalf("still held after its deadline: %+v", items)
	}
}

// A worker that stops mid-chain leaves the message to the next one once its
// lease runs out, which resumes at the hook after the last one decided.
func TestWorker_RestartReclaimsAfterLeaseExpiry(t *testing.T) {
	f := newFixture(t)
	a, na := answer(t, `{"updated_body":{"n":2}}`)
	var nb atomic.Int32
	block := make(chan struct{})
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if nb.Add(1) == 1 {
			<-block // the first worker dies while b decides
			return
		}
		var call hooks.ChannelHookCall
		_ = json.NewDecoder(r.Body).Decode(&call)
		_, _ = w.Write([]byte(`{"updated_body":` + string(call.Body) + `}`))
	}))
	t.Cleanup(func() { close(block); b.Close() })
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("a", a.URL), webhookEntry("b", b.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)

	ctx, cancel := context.WithCancel(context.Background())
	f.w.claim(ctx)
	for nb.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // the replica goes away mid-call
	f.w.wg.Wait()

	w2 := New(f.w.cfg)
	w2.cfg.Owner = "w2"
	w2.now = func() time.Time { return time.Now().Add(2 * time.Minute) } // past the lease
	w2.claim(context.Background())
	w2.wg.Wait()
	msgs := f.peek("inbox", "", store.MemoryScopeGlobal)
	if na.Load() != 1 || len(msgs) != 1 || string(msgs[0].Payload) != `{"n":2}` {
		t.Fatalf("a called %d times; delivered %+v", na.Load(), msgs)
	}
}

// Two workers racing for the same messages decide each one once.
func TestWorker_TwoWorkersDecideEachMessageOnce(t *testing.T) {
	f := newFixture(t)
	var calls sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call hooks.ChannelHookCall
		_ = json.NewDecoder(r.Body).Decode(&call)
		v, _ := calls.LoadOrStore(call.MessageID, new(atomic.Int32))
		v.(*atomic.Int32).Add(1)
	}))
	t.Cleanup(srv.Close)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	for i := 0; i < 30; i++ {
		f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	}
	w2 := New(f.w.cfg)
	w2.cfg.Owner = "w2"
	var wg sync.WaitGroup
	for _, w := range []*Worker{f.w, w2} {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				w.claim(context.Background())
				w.wg.Wait()
			}
		}(w)
	}
	wg.Wait()
	if msgs := f.peek("inbox", "", store.MemoryScopeGlobal); len(msgs) != 30 {
		t.Fatalf("delivered %d of 30", len(msgs))
	}
	calls.Range(func(k, v any) bool {
		if n := v.(*atomic.Int32).Load(); n != 1 {
			t.Errorf("message %s was decided %d times", k, n)
		}
		return true
	})
}

// Decisions are recorded in the tenant whose definition carries the hook, and
// only there.
func TestChannelHookDecisions_AreTenantScoped(t *testing.T) {
	f := newFixture(t)
	f.hdefs["acme|screen"] = hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ return {decision: "drop", reason: "no"}; }`}}
	f.setDef("acme", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "screen"})})
	f.publish("inbox", "acme", store.MemoryScopeTenant, `{}`, nil)
	f.drain()
	mine := f.peek(DecisionsChannel, "acme", store.MemoryScopeTenant)
	if len(mine) != 1 || !strings.Contains(string(mine[0].Payload), `"decision":"drop"`) || !strings.Contains(string(mine[0].Payload), `"channel":"inbox"`) {
		t.Fatalf("acme's decisions = %+v", mine)
	}
	for _, other := range []string{"", "other"} {
		if got := f.peek(DecisionsChannel, other, store.MemoryScopeTenant); len(got) != 0 {
			t.Errorf("tenant %q sees %d of acme's decisions", other, len(got))
		}
	}
	if got := f.peek(DecisionsChannel, "", store.MemoryScopeGlobal); len(got) != 0 {
		t.Errorf("the global keyspace holds %d decisions", len(got))
	}
}

// Run wakes on a write, without waiting out its poll.
func TestWorker_RunWakesOnAWrite(t *testing.T) {
	f := newFixture(t)
	srv, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.w.cfg.Poll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond) // Run is waiting
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.peek("inbox", "", store.MemoryScopeGlobal)) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the worker did not wake on the write")
}

// A tenant's channel hook dials through the private-address guard, as a
// tenant's run hooks do: its webhook never reaches an internal address, and a
// hook that fails closed keeps the message from everyone.
func TestWorker_ATenantsWebhookIsGuarded(t *testing.T) {
	f := newFixture(t)
	srv, n := answer(t, ``)
	f.setDef("acme", "inbox", Def{Hooks: chain(webhookEntry("screen", srv.URL))})
	f.publish("inbox", "acme", store.MemoryScopeTenant, `{}`, nil)
	f.drain()
	if n.Load() != 0 {
		t.Fatalf("a tenant's webhook reached %s (%d calls)", srv.URL, n.Load())
	}
	if msgs := f.peek("inbox", "acme", store.MemoryScopeTenant); len(msgs) != 0 {
		t.Fatalf("delivered %d past a hook that could not run", len(msgs))
	}
}
