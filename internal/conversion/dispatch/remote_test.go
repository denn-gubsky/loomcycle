package dispatch_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
)

// One worker given several dispatchers attaches to each of them.
func TestWorker_AttachesToEveryDispatcher(t *testing.T) {
	d1, a1 := pool(t, dispatch.Options{})
	d2, a2 := pool(t, dispatch.Options{})
	helperWorker(t, a1+","+a2, "w1", worker.Options{})
	waitIdle(t, d1, 1)
	waitIdle(t, d2, 1)
}

// slowCopy is a helper that copies its input after a pause, and fails when it
// finds another copy of itself running: a worker with one slot attached to
// two dispatchers must never run two jobs at once.
func slowCopy(t *testing.T) *convert.Helper {
	t.Helper()
	dir := t.TempDir()
	lock := filepath.Join(dir, "running")
	script := filepath.Join(dir, "slow-copy")
	body := fmt.Sprintf("#!/bin/sh\nmkdir %q 2>/dev/null || exit 7\nsleep 0.4\nrmdir %q\ncp \"$1\" \"$2\"\n", lock, lock)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return &convert.Helper{Key: "speech.encode", Command: script + " {in} {out}"}
}

// A one-slot worker attached to two dispatchers runs one job at a time: the
// second dispatcher's job is answered Busy while the first runs, waits in its
// queue, and runs once the slot frees. Both succeed.
func TestWorker_RunsOnlyItsSlotsAcrossDispatchers(t *testing.T) {
	d1, a1 := pool(t, dispatch.Options{})
	d2, a2 := pool(t, dispatch.Options{})
	helperWorker(t, a1+","+a2, "w1", worker.Options{Encode: slowCopy(t)})
	waitIdle(t, d1, 1)
	waitIdle(t, d2, 1)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		d := d1
		if i%2 == 1 {
			d = d2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := []byte(fmt.Sprintf("RIFF-%d", i))
			out, err := d.Client().EncodeAudio(context.Background(), in, "mp3")
			if err == nil && !bytes.Equal(out, in) {
				err = fmt.Errorf("got %q back", out)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("job %d: %v", i, err)
		}
	}
	busy := d1.Stats().Jobs["audio_encode/busy"] + d2.Stats().Jobs["audio_encode/busy"]
	if busy != 0 {
		t.Errorf("%d jobs were answered busy to their caller; a full worker must make them wait", busy)
	}
}

// A dns:/// dispatcher is every address its name resolves to: the worker
// attaches to an address that appears and leaves one that is gone. The two
// dispatchers share a port on two loopback addresses, as cores behind one
// headless service do.
func TestWorker_FollowsADNSName(t *testing.T) {
	d1, a1 := pool(t, dispatch.Options{})
	_, port, _ := net.SplitHostPort(a1)
	l2, err := net.Listen("tcp", "127.0.0.2:"+port)
	if err != nil {
		t.Skipf("no second loopback address on this host: %v", err)
	}
	d2 := dispatch.New(dispatch.Options{Token: token})
	go func() { _ = d2.Serve(l2) }()
	t.Cleanup(d2.Stop)

	var moved atomic.Bool
	resolve := func(context.Context, string) ([]string, error) {
		if moved.Load() {
			return []string{"127.0.0.2"}, nil
		}
		return []string{"127.0.0.1"}, nil
	}
	helperWorker(t, "dns:///conversion.internal:"+port, "w1", worker.Options{Resolve: resolve, ResolveEvery: 50 * time.Millisecond})
	waitIdle(t, d1, 1)
	if n := d2.Stats().Workers; n != 0 {
		t.Fatalf("d2 has %d workers before the name lists it", n)
	}
	moved.Store(true)
	waitIdle(t, d2, 1)
	deadline := time.Now().Add(10 * time.Second)
	for d1.Stats().Workers != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker stayed attached to an address the name no longer lists")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
