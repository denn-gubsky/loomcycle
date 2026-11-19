package workerlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseTargets_ReadsEveryForm(t *testing.T) {
	got, err := ParseTargets(" unix:///run/w.sock, 10.0.0.5:7788 ,,dns:///conversion.internal:7788")
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{{addr: "unix:///run/w.sock"}, {addr: "10.0.0.5:7788"}, {dnsHost: "conversion.internal", dnsPort: "7788"}}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := got[2].String(); s != "dns:///conversion.internal:7788" {
		t.Errorf("String() = %q", s)
	}
}

func TestParseTargets_RefusesAnEmptyListAndABadDNSName(t *testing.T) {
	for _, spec := range []string{"", " , ", "dns:///no-port", "dns:///:7788"} {
		if _, err := ParseTargets(spec); err == nil {
			t.Errorf("%q: parsed, want an error", spec)
		}
	}
}

func TestPlaintextAllowed_OnlyOnThisHost(t *testing.T) {
	for target, want := range map[string]bool{
		"unix:///run/w.sock": true, "127.0.0.1:7788": true, "[::1]:7788": true, "localhost:7788": true,
		"10.0.0.5:7788": false, "conversion.internal:7788": false, "no-port": false,
	} {
		if got := plaintextAllowed(target); got != want {
			t.Errorf("%s: %v, want %v", target, got, want)
		}
	}
}

// A dispatcher off this host with neither TLS nor Insecure is refused at
// once, and the session never runs: the token would cross the network in
// the clear.
func TestServe_RefusesAPlaintextDispatcherOffThisHost(t *testing.T) {
	targets, _ := ParseTargets("192.0.2.10:7788")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ran atomic.Bool
	err := Serve(ctx, targets, Options{Logger: quiet()}, func(context.Context, *grpc.ClientConn, *slog.Logger) error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if ctx.Err() != nil || ran.Load() {
		t.Fatalf("the refusal waited (%v) or ran a session (%v)", ctx.Err(), ran.Load())
	}
}

// A session that ends with an ordinary error is run again; one that wraps
// ErrRefused stops the link, and with every target refused, Serve returns it.
func TestServe_ReattachesUntilTheSessionIsRefused(t *testing.T) {
	targets, _ := ParseTargets("127.0.0.1:1")
	var runs atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := Serve(ctx, targets, Options{Logger: quiet()}, func(context.Context, *grpc.ClientConn, *slog.Logger) error {
		if runs.Add(1) < 2 {
			return errors.New("lost it")
		}
		return fmt.Errorf("%w: bad token", ErrRefused)
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("session ran %d times, want 2 (once lost, once refused)", n)
	}
}

// With one target refused and another still attached, Serve keeps serving
// the other until ctx ends, and then returns nil.
func TestServe_KeepsServingWhileOneTargetIsLeft(t *testing.T) {
	targets, _ := ParseTargets("127.0.0.1:1,127.0.0.2:1")
	ctx, cancel := context.WithCancel(context.Background())
	attached := make(chan struct{})
	var once sync.Once
	errc := make(chan error, 1)
	go func() {
		errc <- Serve(ctx, targets, Options{Logger: quiet()}, func(sctx context.Context, conn *grpc.ClientConn, _ *slog.Logger) error {
			if conn.Target() == "127.0.0.1:1" {
				return ErrRefused
			}
			once.Do(func() { close(attached) })
			<-sctx.Done()
			return sctx.Err()
		})
	}()
	<-attached
	select {
	case err := <-errc:
		t.Fatalf("Serve returned %v with a target still attached", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Serve = %v after ctx ended, want nil", err)
	}
}

// A dns:/// target is every address its name resolves to: the link attaches
// to an address that appears and leaves one that is gone, and keeps what it
// has when a lookup fails.
func TestServe_FollowsADNSName(t *testing.T) {
	var (
		mu    sync.Mutex
		addrs = []string{"127.0.0.1"}
		fail  bool
		live  = map[string]int{}
	)
	resolve := func(context.Context, string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("no DNS")
		}
		return append([]string(nil), addrs...), nil
	}
	targets, _ := ParseTargets("dns:///conversion.internal:7788")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Serve(ctx, targets, Options{Logger: quiet(), Resolve: resolve, ResolveEvery: 10 * time.Millisecond},
			func(sctx context.Context, conn *grpc.ClientConn, _ *slog.Logger) error {
				mu.Lock()
				live[conn.Target()]++
				mu.Unlock()
				<-sctx.Done()
				mu.Lock()
				live[conn.Target()]--
				mu.Unlock()
				return sctx.Err()
			})
	}()
	waitLive := func(want map[string]int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			ok := true
			for _, a := range []string{"127.0.0.1:7788", "127.0.0.2:7788"} {
				if live[a] != want[a] {
					ok = false
				}
			}
			snap := fmt.Sprint(live)
			mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("attached = %s, want %v", snap, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitLive(map[string]int{"127.0.0.1:7788": 1})
	mu.Lock()
	addrs = []string{"127.0.0.1", "127.0.0.2"}
	mu.Unlock()
	waitLive(map[string]int{"127.0.0.1:7788": 1, "127.0.0.2:7788": 1})
	mu.Lock()
	fail = true
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	waitLive(map[string]int{"127.0.0.1:7788": 1, "127.0.0.2:7788": 1})
	mu.Lock()
	fail, addrs = false, []string{"127.0.0.2"}
	mu.Unlock()
	waitLive(map[string]int{"127.0.0.2:7788": 1})
}

// A dns:/// name every address of which refuses the worker is refused: Serve
// stops and says why instead of idling.
func TestServe_ADNSNameEveryAddressOfWhichRefusesIsRefused(t *testing.T) {
	resolve := func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil }
	targets, _ := ParseTargets("dns:///conversion.internal:7788")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Serve(ctx, targets, Options{Logger: quiet(), Resolve: resolve, ResolveEvery: 20 * time.Millisecond},
		func(context.Context, *grpc.ClientConn, *slog.Logger) error { return nil })
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}
