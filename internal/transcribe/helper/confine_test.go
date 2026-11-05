//go:build unix

package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argvEcho installs, in a fresh directory put first on PATH, a program named
// name that prints its own path and then each argument on a line, and returns
// the directory.
func argvEcho(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func lines(b []byte) []string { return strings.Split(strings.TrimRight(string(b), "\n"), "\n") }

// TestTranscribeHelper_FFmpegProtocolWhitelistIsPassed: an ffmpeg template
// runs with -nostdin and, before each input, -protocol_whitelist file,pipe,
// so a nested protocol in the input (concat:, hls segments over http, a
// subfile: or data: URL) cannot reach the network or the protocols that
// re-read local files. An operator who sets either option keeps theirs, and
// any other program is left as written.
func TestTranscribeHelper_FFmpegProtocolWhitelistIsPassed(t *testing.T) {
	dir := argvEcho(t, "ffmpeg")
	vars := map[string]string{"in": "/scratch/input.mp3", "out": "/scratch/audio.wav"}
	res := mustRun(t, spec(t, `ffmpeg -loglevel error -i {in} -ac 1 -ar 16000 -f wav {out}`, vars))
	want := []string{filepath.Join(dir, "ffmpeg"), "-nostdin", "-loglevel", "error",
		"-protocol_whitelist", "file,pipe", "-i", "/scratch/input.mp3", "-ac", "1", "-ar", "16000", "-f", "wav", "/scratch/audio.wav"}
	if got := lines(res.Stdout); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}

	// Two inputs: each is pinned.
	res = mustRun(t, spec(t, `ffmpeg -i {in} -i {in} {out}`, vars))
	if got := strings.Count(string(res.Stdout), "-protocol_whitelist\nfile,pipe\n-i\n"); got != 2 {
		t.Errorf("two inputs carried the whitelist %d times, want 2:\n%s", got, res.Stdout)
	}

	// The operator's own choice stands, once.
	res = mustRun(t, spec(t, `ffmpeg -nostdin -protocol_whitelist file -i {in} {out}`, vars))
	got := string(res.Stdout)
	if strings.Count(got, "-nostdin\n") != 1 || strings.Count(got, "-protocol_whitelist\n") != 1 || !strings.Contains(got, "-protocol_whitelist\nfile\n-i\n") {
		t.Errorf("the operator's options were changed:\n%s", got)
	}

	// Not ffmpeg: untouched.
	argvEcho(t, "pdftoppm")
	res = mustRun(t, spec(t, `pdftoppm -r 150 -png {in} {out}`, vars))
	if strings.Contains(string(res.Stdout), "-protocol_whitelist") || strings.Contains(string(res.Stdout), "-nostdin") {
		t.Errorf("ffmpeg options were added to another program:\n%s", res.Stdout)
	}
}

// TestTranscribeHelper_WrapperRunsTheHelperAsArgv: an operator wrapper is
// prepended as its own words, the helper's resolved program and arguments
// following as separate argv elements — no shell, so a value that looks like
// shell syntax stays one literal argument. A wrapper with a placeholder, a
// shell character or no program is refused, as is one not installed.
func TestTranscribeHelper_WrapperRunsTheHelperAsArgv(t *testing.T) {
	wdir := argvEcho(t, "confine")
	hdir := argvEcho(t, "ffmpeg")
	s := spec(t, `ffmpeg -i {in} {out}`, map[string]string{"in": "a b; $(id)", "out": "o.wav"})
	s.Wrapper = `confine --ro-bind / / --unshare-net "--label=two words" --`
	res := mustRun(t, s)
	want := []string{filepath.Join(wdir, "confine"), "--ro-bind", "/", "/", "--unshare-net", "--label=two words", "--",
		filepath.Join(hdir, "ffmpeg"), "-nostdin", "-protocol_whitelist", "file,pipe", "-i", "a b; $(id)", "o.wav"}
	if got := lines(res.Stdout); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}

	for _, bad := range []string{`confine {in}`, `confine | tee`, `   `, `confine $HOME`} {
		if _, err := ParseWrapper(bad); err == nil {
			t.Errorf("wrapper %q accepted", bad)
		}
	}
	s.Wrapper = "no-such-confinement-program --"
	if _, err := Run(t.Context(), s); err == nil || !strings.Contains(err.Error(), "no-such-confinement-program") {
		t.Errorf("a missing wrapper program: err = %v, want it named", err)
	}
}
