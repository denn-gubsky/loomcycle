package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// RFC GW P9 plan §3.4, owner ruling D-P9-66: a workspace's servers post
// updates to the worker; await takes them. On the host engine the address
// is a loopback port with a secret path: the path is the only thing that
// says which workspace a post is for.

func hostUpdateURL(t *testing.T, w *worker, id string) (*url.URL, string) {
	t.Helper()
	raw, err := w.updateURL(context.Background(), id, filepath.Join(w.opt.Root, id))
	if err != nil {
		t.Fatalf("updateURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatalf("update url %q: want http://127.0.0.1:<port>/<secret>", raw)
	}
	secret := strings.TrimPrefix(u.Path, "/")
	return u, secret
}

func post(t *testing.T, u string, body []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func awaitUpdate(t *testing.T, w *worker, id string, timeout time.Duration) workspace.AwaitAnswer {
	t.Helper()
	raw, _ := json.Marshal(workspace.AwaitArgs{TimeoutMS: timeout.Milliseconds()})
	a := w.awaitOp(context.Background(), id, raw)
	if a.class != "" {
		t.Fatalf("await refused: %s: %s", a.class, a.detail)
	}
	var ans workspace.AwaitAnswer
	if err := json.Unmarshal(a.output, &ans); err != nil {
		t.Fatalf("await answer %q: %v", a.output, err)
	}
	return ans
}

func updateText(a workspace.AwaitAnswer) string {
	if a.Update == nil {
		return ""
	}
	return *a.Update
}

// The secret is at least 128 bits, and a second workspace gets its own.
func TestUpdates_TheSecretPathIsLongAndPerWorkspace(t *testing.T) {
	w, root := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	ua, a := hostUpdateURL(t, w, "ws1")
	ub, b := hostUpdateURL(t, w, "ws2")
	if len(a) < 32 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("secret %d chars, want at least 32 hex (128 bits)", len(a))
	}
	if a == b || ua.Host == ub.Host {
		t.Error("two workspaces share a secret or an address")
	}
	if again, _ := hostUpdateURL(t, w, "ws1"); again.String() != ua.String() {
		t.Error("a second call made a new address for the same workspace")
	}
}

// A post to the right path is queued; await returns each once, in order,
// and times out when none waits.
func TestUpdates_AwaitTakesEachPostOnceInOrder(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	u, _ := hostUpdateURL(t, w, "ws1")
	for i := range 3 {
		if code, _ := post(t, u.String(), []byte(fmt.Sprintf(`{"n":%d}`, i))); code != http.StatusAccepted {
			t.Fatalf("post %d = %d, want 202", i, code)
		}
	}
	for i := range 3 {
		a := awaitUpdate(t, w, "ws1", time.Second)
		if updateText(a) != fmt.Sprintf(`{"n":%d}`, i) || a.TimedOut || a.Pending != 2-i {
			t.Errorf("await %d = %+v %q", i, a, updateText(a))
		}
	}
	start := time.Now()
	if a := awaitUpdate(t, w, "ws1", 200*time.Millisecond); !a.TimedOut || a.Update != nil {
		t.Errorf("await with nothing queued = %+v, want timed_out", a)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("await returned before its timeout with nothing queued")
	}
	// A post while await waits wakes it.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = http.Post(u.String(), "text/plain", strings.NewReader("late"))
	}()
	if a := awaitUpdate(t, w, "ws1", 5*time.Second); updateText(a) != "late" {
		t.Errorf("await woken by a post = %+v", a)
	}
}

// A wrong path, another method and the bare address are refused alike with
// no detail, and queue nothing.
func TestUpdates_AWrongPathIsRefusedWithNoDetail(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	u, secret := hostUpdateURL(t, w, "ws1")
	flip := []byte(secret)
	flip[len(flip)-1] ^= 1
	if flip[len(flip)-1] == secret[len(secret)-1] {
		t.Fatal("flip did nothing")
	}
	base := "http://" + u.Host
	for _, p := range []string{"/", "/" + string(flip), "/" + secret + "x", "/" + secret[:len(secret)-1], "/" + secret + "/", "/x/" + secret, "/" + strings.ToUpper(secret)} {
		code, body := post(t, base+p, []byte("nope"))
		if code != http.StatusNotFound || body != "" {
			t.Errorf("post to a wrong path = %d %q, want 404 with no body", code, body)
		}
	}
	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || strings.Contains(string(b), secret) {
		t.Errorf("GET on the right path = %d %q, want a refusal that does not echo the path", resp.StatusCode, b)
	}
	if a := awaitUpdate(t, w, "ws1", 100*time.Millisecond); !a.TimedOut || a.Pending != 0 {
		t.Errorf("after refused posts = %+v, want nothing queued", a)
	}
}

// One workspace's path never posts into another's queue: neither at the
// other's address nor at its own.
func TestUpdates_OneWorkspacesPathNeverPostsIntoAnothers(t *testing.T) {
	w, root := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	ua, a := hostUpdateURL(t, w, "ws1")
	ub, b := hostUpdateURL(t, w, "ws2")
	if code, _ := post(t, "http://"+ub.Host+"/"+a, []byte("from a")); code != http.StatusNotFound {
		t.Errorf("ws1's path at ws2's address = %d, want 404", code)
	}
	if code, _ := post(t, "http://"+ua.Host+"/"+b, []byte("from b")); code != http.StatusNotFound {
		t.Errorf("ws2's path at ws1's address = %d, want 404", code)
	}
	for _, id := range []string{"ws1", "ws2"} {
		if got := awaitUpdate(t, w, id, 100*time.Millisecond); !got.TimedOut {
			t.Errorf("%s queue = %+v, want empty", id, got)
		}
	}
	if code, _ := post(t, ub.String(), []byte("to b")); code != http.StatusAccepted {
		t.Fatalf("ws2's own post = %d", code)
	}
	if got := awaitUpdate(t, w, "ws1", 100*time.Millisecond); !got.TimedOut {
		t.Errorf("ws1 got ws2's update: %+v", got)
	}
	if got := awaitUpdate(t, w, "ws2", time.Second); updateText(got) != "to b" {
		t.Errorf("ws2 = %+v", got)
	}
}

// The queue holds MaxUpdates; a post past it is refused 429 and counted as
// dropped, and one over MaxUpdateBytes is refused 413.
func TestUpdates_TheQueueIsBoundedInCountAndSize(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	u, _ := hostUpdateURL(t, w, "ws1")
	if code, _ := post(t, u.String(), bytes.Repeat([]byte("x"), workspace.MaxUpdateBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an update over the size bound = %d, want 413", code)
	}
	if code, _ := post(t, u.String(), bytes.Repeat([]byte("y"), workspace.MaxUpdateBytes)); code != http.StatusAccepted {
		t.Fatalf("an update at the size bound = %d, want 202", code)
	}
	for i := 1; i < workspace.MaxUpdates; i++ {
		if code, _ := post(t, u.String(), []byte("z")); code != http.StatusAccepted {
			t.Fatalf("post %d = %d", i, code)
		}
	}
	for i := range 3 {
		if code, _ := post(t, u.String(), []byte("over")); code != http.StatusTooManyRequests {
			t.Errorf("post past the bound (%d) = %d, want 429", i, code)
		}
	}
	a := awaitUpdate(t, w, "ws1", time.Second)
	if len(updateText(a)) != workspace.MaxUpdateBytes || a.Pending != workspace.MaxUpdates-1 || a.Dropped != 3 {
		t.Errorf("first await = len %d pending %d dropped %d, want %d, %d, 3", len(updateText(a)), a.Pending, a.Dropped, workspace.MaxUpdateBytes, workspace.MaxUpdates-1)
	}
	if a := awaitUpdate(t, w, "ws1", time.Second); a.Dropped != 0 {
		t.Errorf("dropped is reported once, then %d", a.Dropped)
	}
}

// A body that is not UTF-8 comes back in base64.
func TestUpdates_ABinaryUpdateComesInBase64(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	u, _ := hostUpdateURL(t, w, "ws1")
	post(t, u.String(), []byte{0xff, 0x00, 0xfe})
	a := awaitUpdate(t, w, "ws1", time.Second)
	if a.Update != nil || a.UpdateBase64 == nil || *a.UpdateBase64 != "/wD+" {
		t.Errorf("binary update = %+v", a)
	}
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A server finds the address in LOOMCYCLE_UPDATE_URL and posts to it; the
// secret is in no answer the agent reads (start, logs, await) and in no log
// line of the worker, refusals included.
func TestUpdates_AServerPostsAndTheSecretIsNeverAnsweredOrLogged(t *testing.T) {
	requirePython(t)
	w, _ := newTestWorker(t)
	logs := &syncBuf{}
	w.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(w.servers.Close)
	src := `import os, urllib.request, time
u = os.environ["` + workspace.UpdateURLEnv + `"]
urllib.request.urlopen(urllib.request.Request(u, data=b'{"clicked":"ok"}', method="POST"))
print("posted", flush=True)
time.sleep(600)`
	if err := os.WriteFile(filepath.Join(w.opt.Root, "ws1", "poster.py"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	a := serverOpRun(t, w, workspace.OpServerStart, workspace.ServerStartArgs{Command: "python3 poster.py"})
	if a.class != "" {
		t.Fatalf("start: %s %s", a.class, a.detail)
	}
	got := awaitUpdate(t, w, "ws1", 15*time.Second)
	if updateText(got) != `{"clicked":"ok"}` {
		t.Fatalf("await = %+v", got)
	}
	u, secret := hostUpdateURL(t, w, "ws1")
	post(t, "http://"+u.Host+"/wrong", []byte("x"))
	logsAns := serverOpRun(t, w, workspace.OpServerLogs, workspace.ServerLogsArgs{Server: "srv-1"})
	for what, text := range map[string]string{"start": string(a.output), "logs": string(logsAns.output), "worker log": logs.String()} {
		if strings.Contains(text, secret) {
			t.Errorf("the secret is in the %s: %s", what, text)
		}
	}
}

// Removing the workspace closes its address and drops what waited.
func TestUpdates_RemovalClosesTheAddress(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(w.servers.Close)
	u, _ := hostUpdateURL(t, w, "ws1")
	post(t, u.String(), []byte("before"))
	w.servers.Drop("ws1")
	if resp, err := http.Post(u.String(), "text/plain", strings.NewReader("after")); err == nil {
		resp.Body.Close()
		t.Errorf("a post after removal = %d, want the address closed", resp.StatusCode)
	}
	if a := awaitUpdate(t, w, "ws1", 100*time.Millisecond); !a.TimedOut {
		t.Errorf("after removal = %+v, want nothing queued", a)
	}
}
