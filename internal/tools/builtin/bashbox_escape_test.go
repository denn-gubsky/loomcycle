package builtin

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Bashbox escape suite. Bashbox's isolation is delegated to gbash, which
// is alpha, so every property below is re-checked on every gbash bump:
// `make bashbox-escape` runs every TestBashbox* test in this package, and CI
// runs it as its own step. These cases extend the escape tests in
// bashbox_test.go (absolute path, `..`, symlink-out, network, fallback
// smuggling, hostile scripts) with the ones RFC ES #10 listed as missing.

// bothModes runs fn against a fresh read-write and a fresh read-only volume.
func bothModes(t *testing.T, fn func(t *testing.T, root string, ctx context.Context)) {
	for _, ro := range []bool{false, true} {
		name := "rw"
		if ro {
			name = "ro"
		}
		t.Run(name, func(t *testing.T) {
			root := realDir(t)
			ctx := bashCtx(root)
			if ro {
				ctx = roCtx(root)
			}
			fn(t, root, ctx)
		})
	}
}

// /proc does not exist in the sandbox: the host process's environment,
// command line and memory map are out of reach.
func TestBashboxEscape_NoProcFilesystem(t *testing.T) {
	const canary = "proc-canary-7f3a"
	t.Setenv("LOOMCYCLE_ESCAPE_CANARY", canary)
	bothModes(t, func(t *testing.T, root string, ctx context.Context) {
		b := &Bashbox{Enabled: true}
		for _, cmd := range []string{
			"ls /proc",
			"cat /proc/self/environ",
			"cat /proc/self/cmdline",
			"cat /proc/self/maps",
			"cat /proc/1/environ",
			"cd /proc/self && cat environ",
		} {
			res := runBashbox(t, b, ctx, cmd)
			if strings.Contains(res.Text, canary) || strings.Contains(res.Text, filepath.Base(os.Args[0])) {
				t.Fatalf("%q read the host process: %q", cmd, res.Text)
			}
			if !res.IsError {
				t.Errorf("%q should fail, /proc does not exist; got %q", cmd, res.Text)
			}
		}
	})
}

// /dev holds only gbash's virtual devices. A host device (a disk, a tty,
// /dev/mem) appearing after a gbash bump fails this test, so the bump gets
// a review before it ships.
func TestBashboxEscape_DevHoldsOnlyVirtualDevices(t *testing.T) {
	virtual := []string{"full", "null", "random", "urandom", "zero", "stdin", "stdout", "stderr", "fd"}
	bothModes(t, func(t *testing.T, root string, ctx context.Context) {
		b := &Bashbox{Enabled: true}
		res := runBashbox(t, b, ctx, "ls /dev")
		if res.IsError {
			t.Fatalf("ls /dev: %q", res.Text)
		}
		for _, entry := range strings.Fields(res.Text) {
			if !slices.Contains(virtual, entry) {
				t.Errorf("/dev lists %q, which is not one of gbash's virtual devices %v", entry, virtual)
			}
		}
		// Positive control: the virtual devices work, so the listing above is
		// gbash's /dev and not an empty or missing one.
		res = runBashbox(t, b, ctx, "head -c 5 /dev/zero | wc -c")
		if res.IsError || strings.TrimSpace(res.Text) != "5" {
			t.Errorf("/dev/zero does not work in the sandbox: %q", res.Text)
		}
	})
}

// bash's /dev/tcp redirection opens no socket: a listener on loopback sees
// no connection.
func TestBashboxEscape_DevTcpOpensNoConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer ln.Close()
	var accepted atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Store(true)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	bothModes(t, func(t *testing.T, root string, ctx context.Context) {
		b := &Bashbox{Enabled: true}
		for _, cmd := range []string{
			"echo hi > /dev/tcp/127.0.0.1/" + strconv.Itoa(port),
			"cat < /dev/tcp/127.0.0.1/" + strconv.Itoa(port),
			"exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(port),
		} {
			res := runBashbox(t, b, ctx, cmd)
			if !res.IsError {
				t.Errorf("%q should fail; got %q", cmd, res.Text)
			}
		}
	})
	// A connection the sandbox opened would be accepted almost at once; give
	// the accept loop a moment so a late one is still seen.
	time.Sleep(100 * time.Millisecond)
	if accepted.Load() {
		t.Fatal("a /dev/tcp redirection reached a host socket")
	}
}

// Every kind of write a script can make to a read-only volume — overwrite,
// append, delete, rename, chmod, mkdir, a new file, a symlink, a copy —
// happens in the sandbox's in-memory overlay and leaves the host tree
// exactly as it was.
func TestBashboxEscape_RoOverlayMutationsNeverReachHost(t *testing.T) {
	root := realDir(t)
	keep := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(keep, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(root, "sub", "inner.txt")
	if err := os.WriteFile(inner, []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := hostTree(t, root)

	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, roCtx(root), strings.Join([]string{
		"echo changed > keep.txt",
		"echo more >> keep.txt",
		"echo also > sub/inner.txt",
		"cp -r sub sub2",
		"rm sub/inner.txt",
		"mv keep.txt moved.txt",
		"chmod 777 moved.txt",
		"mkdir -p newdir/deeper",
		"touch newfile",
		"ln -s /etc/passwd lnk",
		"rm -rf sub",
		"cat moved.txt",
	}, "; "))
	// Positive control: the writes happened, in the overlay.
	if !strings.Contains(res.Text, "changed") || !strings.Contains(res.Text, "more") {
		t.Fatalf("the writes did not take effect inside the sandbox, so nothing is tested: %q", res.Text)
	}

	if after := hostTree(t, root); !slices.Equal(before, after) {
		t.Errorf("the host tree changed under a read-only volume:\nbefore %v\nafter  %v", before, after)
	}
	if got, _ := os.ReadFile(keep); string(got) != "original" {
		t.Errorf("keep.txt = %q on the host, want %q", got, "original")
	}
	if got, _ := os.ReadFile(inner); string(got) != "inner" {
		t.Errorf("sub/inner.txt = %q on the host, want %q", got, "inner")
	}
}

// hostTree lists every path under root with its type, mode and size, sorted.
func hostTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel+" "+info.Mode().String()+" "+strconv.Itoa(int(info.Size())))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// A symlink the model creates inside the sandbox, pointing out of the
// volume, is not followed out by the host-side file tools: Read, Grep and
// Edit refuse it and Write does not reach the target. gbash writes a
// relative target verbatim, so the host tree really holds the escaping link.
func TestBashboxEscape_SandboxSymlinkNotFollowedByHostFileTools(t *testing.T) {
	root := realDir(t)
	secretDir := realDir(t) // a sibling of root, outside the volume
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := bashCtx(root)
	sib := filepath.Join("..", filepath.Base(secretDir))
	res := runBashbox(t, &Bashbox{Enabled: true}, ctx,
		"ln -s "+filepath.Join(sib, "secret.txt")+" rel && ln -s "+sib+" dirlink && ln -s "+secret+" abs")
	if res.IsError {
		t.Fatalf("creating the links failed: %q", res.Text)
	}
	// Positive control: the escaping links exist on the host.
	for _, l := range []string{"rel", "dirlink"} {
		if target, err := os.Readlink(filepath.Join(root, l)); err != nil || !strings.HasPrefix(target, "..") {
			t.Fatalf("%s is not an escaping symlink on the host (target %q, %v); nothing is tested", l, target, err)
		}
	}

	call := func(tool tools.Tool, args map[string]any) tools.Result {
		t.Helper()
		body, _ := json.Marshal(args)
		r, err := tool.Execute(ctx, body)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		return r
	}
	for _, p := range []string{"rel", "dirlink/secret.txt", "abs"} {
		if r := call(&Read{}, map[string]any{"path": p}); strings.Contains(r.Text, "TOPSECRET") {
			t.Errorf("Read %s followed the link out of the volume: %q", p, r.Text)
		}
		call(&Edit{}, map[string]any{"path": p, "old_string": "TOPSECRET", "new_string": "EDITED"})
		call(&Write{}, map[string]any{"path": p, "content": "WRITTEN"})
	}
	if r := call(&Grep{}, map[string]any{"pattern": "TOPSECRET"}); strings.Contains(r.Text, "TOPSECRET") {
		t.Errorf("Grep followed a link out of the volume: %q", r.Text)
	}
	if got, _ := os.ReadFile(secret); string(got) != "TOPSECRET" {
		t.Errorf("a host file tool wrote through a sandbox-made symlink: the target now holds %q", got)
	}
}

// gbash shares loomcycle's heap and CPU, so a runaway script must be stopped
// by gbash's own limits — without growing the heap without bound — rather
// than by the call's timeout. A case only the timeout stops ends in exit 124.
//
// Not covered, because it is not bounded: a write that never stops into a
// read-only volume's in-memory overlay (`yes > f`) grows the heap until the
// timeout. See the Bashbox notes in docs/TOOLS.md.
func TestBashboxEscape_RunawayScriptsHitLimits(t *testing.T) {
	const heapCeiling = 1 << 30
	root := realDir(t)
	b := &Bashbox{Enabled: true}
	for _, script := range []string{
		"while true; do :; done",           // CPU: command count
		"for i in {1..50000}; do :; done",  // CPU: loop iterations
		"f(){ f; }; f",                     // stack: function depth
		"x=a; while true; do x=$x$x; done", // memory: variable size
		"yes | sort > /dev/null",           // memory: buffered input
		"echo {1..100000000} > /dev/null",  // memory: brace expansion
		"x=$(yes)",                         // memory: command substitution
	} {
		t.Run(script, func(t *testing.T) {
			var peak atomic.Uint64
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				var m runtime.MemStats
				for {
					runtime.ReadMemStats(&m)
					if m.HeapInuse > peak.Load() {
						peak.Store(m.HeapInuse)
					}
					select {
					case <-stop:
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}()
			body, _ := json.Marshal(map[string]any{"command": script, "timeout_seconds": 60})
			start := time.Now()
			res, err := b.Execute(bashCtx(root), body)
			elapsed := time.Since(start)
			close(stop)
			<-done
			runtime.GC()
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Errorf("a runaway script should fail; got %.200q", res.Text)
			}
			// Exit 124 is gbash's timeout: no limit stopped the script.
			if strings.Contains(res.Text, "[exit: 124]") {
				t.Errorf("only the timeout stopped it (after %v), no limit did: %.200q", elapsed, res.Text)
			}
			if p := peak.Load(); p > heapCeiling {
				t.Errorf("heap reached %d MiB (ceiling %d MiB)", p>>20, heapCeiling>>20)
			}
		})
	}
}
