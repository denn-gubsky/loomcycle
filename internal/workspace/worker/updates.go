package worker

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// A workspace's updates (RFC GW §5.3, P9 plan §3.4, owner ruling D-P9-66):
// its application posts what a person did in its page to an address the
// worker serves, and the agent's await takes them in order.
//
// The address is the workspace's alone. On the host engine it is a
// loopback port of the worker with a secret path of 256 random bits: every
// process of the host shares the loopback, so the path is what keeps one
// workspace from posting into another's queue. It is a check, not
// isolation (D-P9-9): a process of the worker's user that can read a
// server's environment can read the path. In a container session the
// address is a relay inside the session's own network namespace, which
// nothing outside the session reaches.
//
// The path is a credential: it is put in the servers' environment only,
// compared in constant time, and never logged, answered or recorded.

// updateSecretBytes is the secret path's length: 256 bits.
const updateSecretBytes = 32

// maxAwaitMS is workspace.MaxAwaitSeconds in milliseconds.
const maxAwaitMS = workspace.MaxAwaitSeconds * 1000

// inbox is one workspace's updates and the feed that fills them.
type inbox struct {
	mu      sync.Mutex
	q       [][]byte
	dropped int
	// ready holds a token whenever q may be non-empty: a push leaves one,
	// an await takes it.
	ready chan struct{}

	// feedMu serialises making and closing the feed.
	feedMu sync.Mutex
	url    string
	// stop ends the feed; done is closed once it has ended (a relay that
	// exited on its own included), so the next server start makes another.
	stop func()
	done <-chan struct{}
	gone bool
}

func newInbox() *inbox { return &inbox{ready: make(chan struct{}, 1)} }

// push queues an update and reports whether it fit.
func (b *inbox) push(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.q) >= workspace.MaxUpdates {
		b.dropped++
		return false
	}
	b.q = append(b.q, p)
	select {
	case b.ready <- struct{}{}:
	default:
	}
	return true
}

// pop takes the oldest update, how many still wait, and the drops since the
// last pop that reported them.
func (b *inbox) pop() ([]byte, bool, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.q) == 0 {
		return nil, false, 0, 0
	}
	p := b.q[0]
	b.q[0] = nil
	b.q = b.q[1:]
	dropped := b.dropped
	b.dropped = 0
	if len(b.q) > 0 {
		select {
		case b.ready <- struct{}{}:
		default:
		}
	}
	return p, true, len(b.q), dropped
}

// close ends the feed; what was queued goes with the inbox.
func (b *inbox) close() {
	b.feedMu.Lock()
	defer b.feedMu.Unlock()
	b.gone = true
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
}

// inboxFor is workspace id's inbox, made on first use.
func (s *servers) inboxFor(id string) *inbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	b := s.inboxes[id]
	if b == nil {
		b = newInbox()
		s.inboxes[id] = b
	}
	return b
}

// dropInbox closes workspace id's inbox and forgets it.
func (s *servers) dropInbox(id string) {
	s.mu.Lock()
	b := s.inboxes[id]
	delete(s.inboxes, id)
	s.mu.Unlock()
	if b != nil {
		b.close()
	}
}

// updateURL is the address workspace id's servers post updates to, making
// the workspace's inbox and its feed on first use, and again when the feed
// ended (a container relay that exited, a session a restore ended).
func (w *worker) updateURL(ctx context.Context, id, real string) (string, error) {
	b := w.servers.inboxFor(id)
	b.feedMu.Lock()
	defer b.feedMu.Unlock()
	if b.gone {
		return "", errors.New("the workspace is being removed")
	}
	if b.stop != nil {
		select {
		case <-b.done:
			b.stop()
			b.stop = nil
		default:
			return b.url, nil
		}
	}
	var (
		url  string
		stop func()
		done <-chan struct{}
		err  error
	)
	if w.opt.Engine == workspace.EngineContainer {
		url, stop, done, err = w.relayUpdates(ctx, id, real, b)
	} else {
		url, stop, done, err = hostUpdates(b)
	}
	if err != nil {
		return "", err
	}
	b.url, b.stop, b.done = url, stop, done
	return url, nil
}

// hostUpdates serves b on a loopback port of its own with a secret path.
func hostUpdates(b *inbox) (string, func(), <-chan struct{}, error) {
	var raw [updateSecretBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, nil, err
	}
	secret := hex.EncodeToString(raw[:])
	want := []byte("/" + secret)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			// The path first, before anything else about the request is
			// looked at, so no answer tells a wrong path from a wrong method.
			if subtle.ConstantTimeCompare([]byte(r.URL.EscapedPath()), want) != 1 || r.URL.RawQuery != "" {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method != http.MethodPost {
				rw.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			acceptUpdate(rw, http.MaxBytesReader(rw, r.Body, workspace.MaxUpdateBytes), b)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// The server's own error lines could carry a request's path.
		ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError),
	}
	done := make(chan struct{})
	safego.Go(context.Background(), "workspace.worker.updates", func() {
		defer close(done)
		_ = srv.Serve(l)
	})
	port := l.Addr().(*net.TCPAddr).Port
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/" + secret, func() { _ = srv.Close() }, done, nil
}

// acceptUpdate reads one update's body (bounded by the caller) and queues
// it: 202, or 413 past the size bound, or 429 when the queue is full.
func acceptUpdate(rw http.ResponseWriter, body io.Reader, b *inbox) {
	p, err := io.ReadAll(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			rw.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	if !b.push(p) {
		rw.WriteHeader(http.StatusTooManyRequests)
		return
	}
	rw.WriteHeader(http.StatusAccepted)
}

// awaitOp waits for workspace id's next update, up to the op's timeout.
func (w *worker) awaitOp(ctx context.Context, id string, raw []byte) answer {
	var in workspace.AwaitArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if in.TimeoutMS < 1 || in.TimeoutMS > maxAwaitMS {
		return refuse(workspace.ClassInvalid, "timeout_ms must be from 1 to %d", maxAwaitMS)
	}
	b := w.servers.inboxFor(id)
	timer := time.NewTimer(time.Duration(in.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	timedOut := func() answer {
		out, _ := json.Marshal(workspace.AwaitAnswer{TimedOut: true})
		return answer{output: out}
	}
	for {
		if p, ok, pending, dropped := b.pop(); ok {
			ans := workspace.AwaitAnswer{Pending: pending, Dropped: dropped}
			if utf8.Valid(p) {
				s := string(p)
				ans.Update = &s
			} else {
				s := base64.StdEncoding.EncodeToString(p)
				ans.UpdateBase64 = &s
			}
			out, _ := json.Marshal(ans)
			return answer{output: out}
		}
		select {
		case <-b.ready:
		case <-timer.C:
			return timedOut()
		case <-ctx.Done():
			return timedOut()
		}
	}
}

// updateRelaySource is the relay relayUpdates starts in a session, passed
// to python3 -I -c as the tunnel's relay is: nothing of it lives in the
// workspace.
//
//go:embed update_relay.py
var updateRelaySource string

// relayReady bounds how long a relay has to say where it listens.
const relayReady = 30 * time.Second

// relayUpdates starts the update relay in workspace id's container session
// and feeds b from it. The address is the session's own loopback: nothing
// outside the session reaches it, so it carries no secret. What the relay
// writes is read as untrusted: a frame over the size bound, or anything
// else out of shape, ends it.
func (w *worker) relayUpdates(ctx context.Context, id, real string, b *inbox) (string, func(), <-chan struct{}, error) {
	p, err := w.engine.Start(ctx, engine.StartRequest{Workspace: id, Root: real, Dir: real,
		Argv: []string{"python3", "-I", "-c", updateRelaySource, strconv.Itoa(workspace.MaxUpdateBytes)}})
	if err != nil {
		return "", nil, nil, fmt.Errorf("starting the update relay: %w", err)
	}
	stop := func() { _ = p.Kill() }
	r := bufio.NewReaderSize(p.Stdout(), 64)
	type ready struct {
		port int
		err  error
	}
	got := make(chan ready, 1)
	safego.Go(context.WithoutCancel(ctx), "workspace.worker.update-relay-ready", func() {
		line, err := r.ReadString('\n')
		if err != nil {
			got <- ready{err: err}
			return
		}
		port, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "P"), "\n"))
		if err != nil || !strings.HasPrefix(line, "P") || port < 1 || port > 65535 {
			got <- ready{err: errors.New("the relay did not say where it listens")}
			return
		}
		got <- ready{port: port}
	})
	var rd ready
	select {
	case rd = <-got:
	case <-time.After(relayReady):
		rd.err = errors.New("the relay did not start in time")
	}
	if rd.err != nil {
		stop()
		return "", nil, nil, fmt.Errorf("the update relay: %w", rd.err)
	}
	safego.Go(context.Background(), "workspace.worker.update-relay", func() {
		defer stop()
		var head [5]byte
		for {
			if _, err := io.ReadFull(r, head[:]); err != nil || head[0] != 'U' {
				return
			}
			n := binary.BigEndian.Uint32(head[1:])
			if n > workspace.MaxUpdateBytes {
				return
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			ack := []byte("A")
			if !b.push(body) {
				ack = []byte("F")
			}
			if _, err := p.Stdin().Write(ack); err != nil {
				return
			}
		}
	})
	return "http://127.0.0.1:" + strconv.Itoa(rd.port) + "/", stop, p.Done(), nil
}
