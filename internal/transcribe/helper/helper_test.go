//go:build unix

package helper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spec is a Spec rooted at a fresh directory, running there.
func spec(t *testing.T, template string, vars map[string]string) Spec {
	t.Helper()
	root := t.TempDir()
	return Spec{Key: "transcription.test.helper", Template: template, Vars: vars, Root: root, Dir: root, Timeout: 10 * time.Second}
}

func mustRun(t *testing.T, s Spec) Result {
	t.Helper()
	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run(%q): %v (stderr %q)", s.Template, err, res.Stderr)
	}
	return res
}

// TestRun_ShellMetacharactersInAValueStayOneLiteralArgument — no shell ever
// sees a value: each one arrives as exactly one argv element, byte for byte.
func TestRun_ShellMetacharactersInAValueStayOneLiteralArgument(t *testing.T) {
	vals := map[string]string{
		"a": "; rm -rf /",
		"b": "$(id)",
		"c": "`id`",
		"d": "x | cat /etc/passwd > out",
		"e": "two  words\tand a tab",
		"f": `it's "quoted" \ and {braced}`,
		"g": "",
	}
	res := mustRun(t, spec(t, `printf "[%s]\n" {a} {b} {c} {d} {e} {f} {g}`, vals))
	want := "[; rm -rf /]\n[$(id)]\n[`id`]\n[x | cat /etc/passwd > out]\n[two  words\tand a tab]\n" +
		`[it's "quoted" \ and {braced}]` + "\n[]\n"
	if string(res.Stdout) != want {
		t.Errorf("stdout =\n%q\nwant\n%q", res.Stdout, want)
	}
}

// TestRun_PlaceholderInsideAnArgumentFillsOnlyThatArgument — a placeholder
// that is part of a word fills that word and never splits it.
func TestRun_PlaceholderInsideAnArgumentFillsOnlyThatArgument(t *testing.T) {
	res := mustRun(t, spec(t, `printf "[%s]\n" --lang={lang} "{in}.wav" pre'{in}'post {out_prefix}-%d.png`,
		map[string]string{"lang": "uk", "in": "my file", "out_prefix": "/tmp/a b/page"}))
	want := "[--lang=uk]\n[my file.wav]\n[pre{in}post]\n[/tmp/a b/page-%d.png]\n"
	if string(res.Stdout) != want {
		t.Errorf("stdout =\n%q\nwant\n%q", res.Stdout, want)
	}
}

// TestRun_RefusesATemplateOrValueItCannotRunSafely — nothing is started for a
// template that names an unknown placeholder or a value that is unset, holds a
// NUL, or could be read as an option; nor for a template whose program is a
// placeholder, whose quote is open, or that leans on shell syntax.
func TestRun_RefusesATemplateOrValueItCannotRunSafely(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	for name, c := range map[string]struct {
		template string
		vars     map[string]string
	}{
		"unknown placeholder":         {`touch ` + marker + ` {nope}`, map[string]string{"in": "x"}},
		"no vars at all":              {`touch ` + marker + ` {in}`, nil},
		"NUL in a value":              {`touch ` + marker + ` {in}`, map[string]string{"in": "a\x00b"}},
		"value read as an option":     {`touch ` + marker + ` {in}`, map[string]string{"in": "-rf"}},
		"program from a value":        {`{bin} ` + marker, map[string]string{"bin": "touch"}},
		"program partly from a value": {`tou{x} ` + marker, map[string]string{"x": "ch"}},
		"open quote":                  {`touch "` + marker, nil},
		"pipe":                        {`touch ` + marker + ` | cat`, nil},
		"sequence":                    {`touch ` + marker + `; true`, nil},
		"redirect":                    {`touch ` + marker + ` > x`, nil},
		"substitution":                {`touch ` + marker + ` $(id)`, nil},
		"backtick":                    {"touch " + marker + " `id`", nil},
		"background":                  {`touch ` + marker + ` &`, nil},
		"bad placeholder name":        {`touch ` + marker + ` {in put}`, map[string]string{"in put": "x"}},
		"empty template":              {`   `, nil},
	} {
		if _, err := Run(context.Background(), spec(t, c.template, c.vars)); err == nil {
			t.Errorf("%s: Run(%q) succeeded, want refused", name, c.template)
		}
		if strings.HasPrefix(name, "unknown") || name == "pipe" || name == "open quote" || name == "program from a value" || name == "empty template" {
			var names []string
			for k := range c.vars {
				names = append(names, k)
			}
			if Validate(c.template, names) == nil {
				t.Errorf("%s: Validate(%q) = nil, want an error", name, c.template)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the command ran", name)
		}
	}
	if err := Validate(`pdftoppm -r 150 -png {in} {out_prefix}`, []string{"in", "out_prefix"}); err != nil {
		t.Errorf("Validate(rasterize default) = %v", err)
	}
	if err := Validate(`ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out}`, []string{"in", "out"}); err != nil {
		t.Errorf("Validate(media default) = %v", err)
	}
}

// TestRun_ChildSeesOnlyPathAndTheAllowedNames — loomcycle's own variables and
// secrets set in the parent never reach the child; an allowed name refused for
// looking like a secret or a loomcycle setting is an error, not a leak.
func TestRun_ChildSeesOnlyPathAndTheAllowedNames(t *testing.T) {
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "fake-bearer-for-test")
	t.Setenv("ANTHROPIC_API_KEY", "fake-key-for-test")
	t.Setenv("PG_DSN", "postgres://fake")
	t.Setenv("HELPER_TEST_ALLOWED", "ok")
	s := spec(t, `env`, nil)
	s.ExtraEnv = []string{"HELPER_TEST_ALLOWED"}
	res := mustRun(t, s)
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	if len(got) != 2 || got["HELPER_TEST_ALLOWED"] != "ok" || got["PATH"] != os.Getenv("PATH") {
		t.Errorf("child env = %v, want exactly PATH and HELPER_TEST_ALLOWED", got)
	}
	for _, bad := range []string{"LOOMCYCLE_AUTH_TOKEN", "LOOMCYCLE_ANYTHING", "ANTHROPIC_API_KEY", "GITHUB_TOKEN",
		"DB_PASSWORD", "X_SECRET", "BASIC_AUTH", "AWS_CREDENTIAL", "PG_DSN", "PATH", "1BAD", "A=B", ""} {
		s := spec(t, `env`, nil)
		s.ExtraEnv = []string{bad}
		if _, err := Run(context.Background(), s); err == nil {
			t.Errorf("ExtraEnv %q accepted, want refused", bad)
		}
	}
}

// alive reports whether pid names a live process. kill(pid, 0) succeeds on a
// zombie too, so a process the kernel lists as a zombie counts as dead.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps lists nothing: gone
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// TestRun_TimeoutKillsTheWholeProcessGroup — a helper past its timeout is
// killed together with the processes it started, and Run returns promptly
// with ErrTimeout instead of waiting for the grandchild.
func TestRun_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	s := spec(t, `sh -c 'sleep 30 & echo $! > "$1"; wait' sh {pidfile}`, map[string]string{"pidfile": pidfile})
	s.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), s)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run past its timeout = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("Run took %v after a 500ms timeout", took)
	}
	b, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the helper never started its grandchild: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || pid <= 1 {
		t.Fatalf("grandchild pid %q", b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the helper's timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRun_CapsCapturedOutput — each stream keeps its first MaxOutputBytes and
// says it was cut; the helper is not killed for writing more.
func TestRun_CapsCapturedOutput(t *testing.T) {
	s := spec(t, `sh -c 'head -c 100000 /dev/zero | tr "\0" a; head -c 50000 /dev/zero | tr "\0" b >&2'`, nil)
	s.MaxOutputBytes = 1000
	res := mustRun(t, s)
	if len(res.Stdout) != 1000 || strings.Trim(string(res.Stdout), "a") != "" || !res.StdoutTruncated {
		t.Errorf("stdout = %d bytes, truncated %v; want the first 1000 a's, truncated", len(res.Stdout), res.StdoutTruncated)
	}
	if len(res.Stderr) != 1000 || strings.Trim(string(res.Stderr), "b") != "" || !res.StderrTruncated {
		t.Errorf("stderr = %d bytes, truncated %v; want the first 1000 b's, truncated", len(res.Stderr), res.StderrTruncated)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

// TestRun_NonZeroExitReportsTheCode — a helper that runs and fails returns an
// ExitError with its code, and its output is still there to read.
func TestRun_NonZeroExitReportsTheCode(t *testing.T) {
	res, err := Run(context.Background(), spec(t, `sh -c 'echo oops >&2; exit 3'`, nil))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || res.ExitCode != 3 || string(res.Stderr) != "oops\n" {
		t.Errorf("Run = (%+v, %v), want ExitError code 3 with stderr oops", res, err)
	}
}

// TestRun_MissingBinaryNamesTheHelperKey — an operator whose host lacks the
// helper's program is told which program and which setting.
func TestRun_MissingBinaryNamesTheHelperKey(t *testing.T) {
	s := spec(t, `loomcycle-no-such-helper-binary -i {in}`, map[string]string{"in": "/x"})
	s.Key = "transcription.stt.media"
	_, err := Run(context.Background(), s)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run = %v, want ErrNotFound", err)
	}
	for _, want := range []string{"loomcycle-no-such-helper-binary", "transcription.stt.media", "loomcycle-media"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestRun_WorkingDirectoryStaysInsideTheRoot — the helper runs only in an
// existing directory under the operator's root, reached without following a
// link below the root; the root itself may be a link.
func TestRun_WorkingDirectoryStaysInsideTheRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(realRoot, "inbox", "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rootLink := filepath.Join(base, "root-link")
	for link, target := range map[string]string{
		rootLink:                          realRoot,
		filepath.Join(realRoot, "escape"): outside,
		filepath.Join(realRoot, "inbox", "alias"): filepath.Join(realRoot, "inbox", "sub"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realRoot, "inbox", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The operator's root may be a link; the directory under it is reached by
	// name, and the helper runs in its real path.
	s := Spec{Key: "k", Template: `pwd -P`, Root: rootLink, Dir: filepath.Join(rootLink, "inbox", "sub"), Timeout: 5 * time.Second}
	res := mustRun(t, s)
	wantDir, _ := filepath.EvalSymlinks(filepath.Join(realRoot, "inbox", "sub"))
	if strings.TrimSpace(string(res.Stdout)) != wantDir {
		t.Errorf("helper ran in %q, want %q", strings.TrimSpace(string(res.Stdout)), wantDir)
	}

	for name, dir := range map[string]string{
		"link out of the root":        filepath.Join(rootLink, "escape"),
		"link inside the root":        filepath.Join(rootLink, "inbox", "alias"),
		"dot-dot out of the root":     filepath.Join(rootLink, "..", "outside"),
		"absolute path outside":       outside,
		"missing directory":           filepath.Join(rootLink, "inbox", "nope"),
		"a file, not a directory":     filepath.Join(rootLink, "inbox", "file"),
		"relative directory":          "inbox",
		"root's real path, not named": filepath.Join(realRoot+"-sibling", "inbox"),
	} {
		bad := s
		bad.Dir = dir
		if _, err := Run(context.Background(), bad); err == nil {
			t.Errorf("%s: Run in %q succeeded, want refused", name, dir)
		}
	}
	noRoot := s
	noRoot.Root = ""
	if _, err := Run(context.Background(), noRoot); err == nil {
		t.Errorf("Run with no root succeeded, want refused")
	}
}
