//go:build linux || darwin

package rootbilly

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// A copy of the swap-race fixture of internal/tools/builtin
// (volume_swap_race_test.go), which the file tools' tests keep using: a test
// file cannot be shared across packages. A goroutine flips a directory
// component of the target path between a real directory inside the volume and
// a symlink to a directory outside it while the filesystem works on it.
const swapRaceIterations = 5000

const swapSentinel = "SENTINEL-outside-the-volume"

// realDir EvalSymlinks's a fresh temp dir so a /var→/private/var (macOS) root
// matches the resolved paths.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// swapRace is the fixture: root (the volume) holds d/secret.txt, outside
// holds secret.txt with content that carries the sentinel, and a goroutine
// swaps d for a symlink to outside and back until stop() is called.
type swapRace struct {
	root, outside, outsideContent string
	stop                          func()
}

func newSwapRace(t *testing.T, inside, outsideContent string) *swapRace {
	t.Helper()
	outside := realDir(t)
	return newSwapRaceAt(t, realDir(t), outside, outside, inside, outsideContent)
}

// newSwapRaceAt is newSwapRace with the outside directory and the symlink's
// target chosen by the caller: the fence test puts outside inside the volume
// and links to it relatively, so the link stays inside the root.
func newSwapRaceAt(t *testing.T, root, outside, linkTarget, inside, outsideContent string) *swapRace {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "secret.txt"), []byte(inside), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// d and d.swap trade places in one syscall each turn, so d is always
	// either the real directory or a symlink to outside, never missing.
	d, swap := filepath.Join(root, "d"), filepath.Join(root, "d.swap")
	if err := os.Symlink(linkTarget, swap); err != nil {
		t.Fatal(err)
	}
	if err := exchangePaths(d, swap); err != nil {
		skipWithoutExchange(t, err)
	}
	if err := exchangePaths(d, swap); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !done.Load() {
			_ = exchangePaths(d, swap)
		}
	}()
	s := &swapRace{root: root, outside: outside, outsideContent: outsideContent}
	s.stop = sync.OnceFunc(func() {
		done.Store(true)
		wg.Wait()
		// Leave d the real directory, whichever way the last swap went.
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := exchangePaths(d, swap); err != nil {
				t.Errorf("restoring d: %v", err)
			}
		}
	})
	t.Cleanup(s.stop)
	return s
}

// skipWithoutExchange skips a swap-race test on a filesystem with no atomic
// rename exchange. CI sets LOOMCYCLE_TEST_REQUIRE_EXCHANGE so a race proof can
// never pass by being skipped.
func skipWithoutExchange(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("LOOMCYCLE_TEST_REQUIRE_EXCHANGE") != "" {
		t.Fatalf("atomic rename exchange unsupported here and LOOMCYCLE_TEST_REQUIRE_EXCHANGE is set: %v", err)
	}
	t.Skipf("atomic rename exchange unsupported here: %v", err)
}

// outsideUntouched fails the test if anything outside the volume changed.
func (s *swapRace) outsideUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(s.outside, "secret.txt"))
	if err != nil || string(got) != s.outsideContent {
		t.Errorf("the file outside the volume was rewritten: %q (err %v)", got, err)
	}
	ents, _ := os.ReadDir(s.outside)
	for _, e := range ents {
		if e.Name() != "secret.txt" {
			t.Errorf("a file was created outside the volume: %s", e.Name())
		}
	}
}
