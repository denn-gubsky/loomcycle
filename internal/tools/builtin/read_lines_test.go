package builtin

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// readFixture writes content to a file in a fresh default volume and returns
// a function that calls r on it with the given arguments.
func readFixture(t *testing.T, r *Read, content string) func(args map[string]any) tools.Result {
	t.Helper()
	root := realDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	return func(args map[string]any) tools.Result {
		t.Helper()
		if args == nil {
			args = map[string]any{}
		}
		args["path"] = "f.txt"
		in, _ := json.Marshal(args)
		res, err := r.Execute(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "this is line %d\n", i)
	}
	return b.String()
}

func TestRead_LineRange_ReturnsExactlyTheNumberedLines(t *testing.T) {
	read := readFixture(t, &Read{}, numberedLines(3044))
	res := read(map[string]any{"offset": 812, "limit": 7, "line_numbers": true})
	var want strings.Builder
	for i := 812; i <= 818; i++ {
		fmt.Fprintf(&want, "%6d\tthis is line %d\n", i, i)
	}
	want.WriteString("[lines 812-818 of 3044]\n")
	if res.IsError || res.Text != want.String() {
		t.Errorf("got (error=%v):\n%s\nwant:\n%s", res.IsError, res.Text, want.String())
	}
}

// A call with none of the line arguments must stay what it was: the file's
// bytes and nothing else. `line_numbers: false` is not a line argument.
func TestRead_NoLineArguments_ReturnsTheBytesWithNoStatusLine(t *testing.T) {
	content := numberedLines(50)
	read := readFixture(t, &Read{}, content)
	for _, args := range []map[string]any{nil, {"line_numbers": false}} {
		if res := read(args); res.IsError || res.Text != content {
			t.Errorf("args %v: result differs from the file's bytes:\n%q", args, res.Text)
		}
	}
}

var continueRe = regexp.MustCompile(`continue with offset (\d+)\]\n$`)

// Following "continue with offset N" to the end returns every line once, in
// order, and no result ends inside a line.
func TestRead_LineRange_CutAtTheSizeLimitContinuesWithoutLossOrRepeat(t *testing.T) {
	content := numberedLines(900)
	read := readFixture(t, &Read{MaxBytes: 1024}, content)
	var got strings.Builder
	offset, calls := 1, 0
	for {
		calls++
		if calls > 200 {
			t.Fatal("did not reach the end of the file")
		}
		res := read(map[string]any{"offset": offset})
		if res.IsError {
			t.Fatalf("offset %d: %s", offset, res.Text)
		}
		i := strings.LastIndex(strings.TrimSuffix(res.Text, "\n"), "\n")
		lines, status := res.Text[:i+1], res.Text[i+1:]
		if int64(len(lines)) > 1024 {
			t.Fatalf("offset %d returned %d bytes of lines, over the limit", offset, len(lines))
		}
		got.WriteString(lines)
		m := continueRe.FindStringSubmatch(status)
		if m == nil {
			if !strings.HasSuffix(status, " of 900]\n") {
				t.Errorf("last status line = %q", status)
			}
			break
		}
		if !strings.Contains(status, "cut at 1 KiB") {
			t.Errorf("status line does not name the limit: %q", status)
		}
		offset, _ = strconv.Atoi(m[1])
	}
	if got.String() != content {
		t.Errorf("the portions do not add up to the file (%d bytes, want %d)", got.Len(), len(content))
	}
	if calls < 5 {
		t.Errorf("only %d calls: the limit did not cut anything, so the test asserts nothing", calls)
	}
}

func TestRead_LineRange_ALineOverTheLimitIsCutBetweenCharacters(t *testing.T) {
	long := strings.Repeat("жы", 600) // 2,400 bytes of two-byte characters
	read := readFixture(t, &Read{MaxBytes: 1001}, "short\n"+long+"\nafter\n")
	res := read(map[string]any{"offset": 2})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !utf8.ValidString(res.Text) {
		t.Error("the result is cut inside a character")
	}
	want := "[line 2 cut at 1000 bytes of 2401]\n[lines 2-2 of 3; cut at 1001 bytes, continue with offset 3]\n"
	if !strings.HasSuffix(res.Text, want) {
		t.Errorf("result ends %q, want it to end %q", res.Text[len(res.Text)-120:], want)
	}
	if next := read(map[string]any{"offset": 3}); next.Text != "after\n[lines 3-3 of 3]\n" {
		t.Errorf("continuing = %q", next.Text)
	}
}

func TestRead_LineRange_EdgesOfTheFile(t *testing.T) {
	for _, c := range []struct {
		name, content string
		args          map[string]any
		want          string
	}{
		{"offset past the end", numberedLines(120), map[string]any{"offset": 500}, "[no lines: the file has 120 lines]\n"},
		{"empty file", "", map[string]any{"offset": 1}, "[no lines: the file has 0 lines]\n"},
		{"limit past the end", "a\nb\nc\n", map[string]any{"offset": 2, "limit": 50}, "b\nc\n[lines 2-3 of 3]\n"},
		{"last line has no newline", "a\nb", map[string]any{"limit": 9}, "a\nb\n[lines 1-2 of 2]\n"},
		{"line endings are kept", "a\r\nb\r\n", map[string]any{"line_numbers": true}, "     1\ta\r\n     2\tb\r\n[lines 1-2 of 2]\n"},
		{"blank lines count", "a\n\n\nb\n", map[string]any{"offset": 3, "limit": 2}, "\nb\n[lines 3-4 of 4]\n"},
	} {
		if res := readFixture(t, &Read{}, c.content)(c.args); res.IsError || res.Text != c.want {
			t.Errorf("%s: got %q (error=%v), want %q", c.name, res.Text, res.IsError, c.want)
		}
	}
}

func TestRead_LineRange_RefusesABinaryFileAndBadNumbers(t *testing.T) {
	bin := readFixture(t, &Read{}, "abc\x00def\n")
	if res := bin(map[string]any{"offset": 1}); !res.IsError || !strings.Contains(res.Text, "binary file") {
		t.Errorf("binary file in line mode: %q (error=%v)", res.Text, res.IsError)
	}
	if res := bin(nil); res.IsError || res.Text != "abc\x00def\n" {
		t.Errorf("binary file without line arguments must read as before: %q", res.Text)
	}
	text := readFixture(t, &Read{}, "a\nb\n")
	for _, args := range []map[string]any{{"offset": 0}, {"offset": -3}, {"limit": 0}} {
		if res := text(args); !res.IsError {
			t.Errorf("args %v were accepted: %q", args, res.Text)
		}
	}
}

// The scan limit stops the count of lines, never the read: a range that
// starts past it is still returned.
func TestRead_LineRange_ScanLimitBoundsTheCountNotTheRead(t *testing.T) {
	content := numberedLines(1000) // about 17 KB
	r := readFixture(t, &Read{MaxScanBytes: 2000}, content)
	res := r(map[string]any{"offset": 1, "limit": 2})
	if !strings.HasPrefix(res.Text, "this is line 1\nthis is line 2\n[lines 1-2 of more than ") {
		t.Errorf("counting past the scan limit: %q", res.Text)
	}
	res = r(map[string]any{"offset": 900, "limit": 1})
	if !strings.HasPrefix(res.Text, "this is line 900\n[lines 900-900 of ") {
		t.Errorf("a range past the scan limit was not returned: %q", res.Text)
	}
}

// An argument Read does not have is refused by name. Before, a misspelt
// `offset` was dropped and the whole file came back with no hint.
func TestRead_UnknownArgumentIsRefused(t *testing.T) {
	read := readFixture(t, &Read{}, numberedLines(20))
	res := read(map[string]any{"start_line": 10})
	if !res.IsError || !strings.Contains(res.Text, `unknown argument "start_line"`) {
		t.Errorf("unknown argument: error=%v text=%q", res.IsError, res.Text)
	}
	if strings.Contains(res.Text, "this is line") {
		t.Errorf("the refusal carries file content: %q", res.Text)
	}
	if res := read(map[string]any{"volume": "default", "offset": 10, "limit": 1, "line_numbers": true}); res.IsError {
		t.Errorf("every declared argument together was refused: %s", res.Text)
	}
}

// A limit near the largest integer means "to the end". offset+limit used to
// wrap negative, so such a call answered "no lines" on a file that had them.
func TestRead_LineRange_HugeLimitReadsToTheEnd(t *testing.T) {
	read := readFixture(t, &Read{}, "a\nb\nc\nd\n")
	for _, c := range []struct {
		offset int
		want   string
	}{
		{1, "a\nb\nc\nd\n[lines 1-4 of 4]\n"},
		{2, "b\nc\nd\n[lines 2-4 of 4]\n"},
	} {
		res := read(map[string]any{"offset": c.offset, "limit": math.MaxInt})
		if res.IsError || res.Text != c.want {
			t.Errorf("offset %d, limit MaxInt: got %q (error=%v), want %q", c.offset, res.Text, res.IsError, c.want)
		}
	}
}
