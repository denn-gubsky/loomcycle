package http

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// waitForKeepalive blocks until w holds a keepalive frame, so the goroutine
// is known to be ticking. Bounded: a keepalive that never starts fails here.
func waitForKeepalive(t *testing.T, w *captureWriter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.Snapshot(), ": keepalive") {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive frame within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// The request ctx is cancelled only after the handler has returned, so a
// stream that relied on it would keep ticking past the return. Here the ctx
// is never cancelled: only end() can stop the writes.
func TestSSE_EndStopsKeepaliveWritesWhileTheRequestCtxIsLive(t *testing.T) {
	w := newCaptureWriter()
	s, _ := newSSE(w)
	s.start()
	s.startKeepalive(context.Background(), time.Millisecond)
	waitForKeepalive(t, w)

	s.end()
	atEnd := w.Snapshot()
	time.Sleep(30 * time.Millisecond) // thirty intervals
	if got := w.Snapshot(); got != atEnd {
		t.Errorf("the stream was written after end() returned: %q", strings.TrimPrefix(got, atEnd))
	}
}

// end() returns only once the keepalive goroutine has exited, so a handler
// that defers it leaves nothing behind that could reach the writer.
func TestSSE_EndReturnsAfterTheKeepaliveGoroutineExits(t *testing.T) {
	for i := 0; i < 50; i++ {
		w := newCaptureWriter()
		s, _ := newSSE(w)
		s.start()
		s.startKeepalive(context.Background(), time.Millisecond)
		waitForKeepalive(t, w)
		done := s.keepaliveDone
		s.end()
		select {
		case <-done:
		default:
			t.Fatalf("stream %d: end() returned with the keepalive goroutine still running", i)
		}
	}
}

// Once ended, every writer is a no-op: a late send from a goroutine the
// handler did not wait for must not reach a finished response.
func TestSSE_WritesAfterEndAreDropped(t *testing.T) {
	w := newCaptureWriter()
	s, _ := newSSE(w)
	s.start()
	s.end()
	s.end() // idempotent
	w.mu.Lock()
	wantBody, wantFlushes := w.buf.String(), w.flushes
	w.mu.Unlock()

	s.send(providers.Event{Type: providers.EventText, Text: "late"})
	s.send(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{Input: json.RawMessage("{")}}) // the marshal-error frame
	s.sendRaw("agent", map[string]any{"run_id": "r"})
	s.sendOpenAIData(map[string]any{"id": "x"})
	s.sendOpenAIDone()
	s.writeKeepalive()
	s.startKeepalive(context.Background(), time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.String() != wantBody || w.flushes != wantFlushes {
		t.Errorf("an ended stream was written: body %q (flushes %d), want %q (flushes %d)",
			w.buf.String(), w.flushes, wantBody, wantFlushes)
	}
}

// Through the real net/http server: the keepalive must not write into the
// response's buffered writer while net/http finishes the request and pools
// it. Under -race a write that outlives the handler is reported against
// finishRequest's Flush/Reset.
func TestSSE_KeepaliveNeverWritesAfterTheHandlerReturns(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream, ok := newSSE(w)
		if !ok {
			http.Error(w, "no streaming", http.StatusInternalServerError)
			return
		}
		stream.start()
		stream.startKeepalive(r.Context(), 50*time.Microsecond)
		defer stream.end()
		for i := 0; i < 3; i++ {
			stream.sendRaw("tick", map[string]int{"i": i})
		}
	}))
	defer ts.Close()

	var wg sync.WaitGroup
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				resp, err := ts.Client().Get(ts.URL)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// Every handler that starts a keepalive must defer the stream's end() in
// the same function: the keepalive is otherwise stopped only by the request
// ctx, which net/http cancels after the handler has already returned.
func TestSSE_EveryKeepaliveStartSiteDefersEnd(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := func(n ast.Node, method string) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
					found = true
				}
			}
			return !found
		})
		return found
	}
	sites := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil || fn.Recv != nil && fn.Name.Name == "startKeepalive" {
					continue
				}
				if !calls(fn.Body, "startKeepalive") {
					continue
				}
				sites++
				deferred := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if ds, ok := n.(*ast.DeferStmt); ok && calls(ds, "end") {
						deferred = true
					}
					return !deferred
				})
				if !deferred {
					t.Errorf("%s: %s starts an SSE keepalive without deferring the stream's end()",
						fset.Position(fn.Pos()), fn.Name.Name)
				}
			}
		}
	}
	if sites == 0 {
		t.Fatal("found no startKeepalive call sites; the census is looking in the wrong place")
	}
}
