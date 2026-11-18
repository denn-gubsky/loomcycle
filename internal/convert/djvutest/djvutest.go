// Package djvutest installs stand-ins for DjVuLibre's djvutxt and djvused,
// so the DjVu helper wiring (RFC GV P3) is tested on a host without
// DjVuLibre. The stand-ins answer in the programs' own shapes: djvutxt
// --detail=page one `(page …)` per page of its --page list, djvused one fixed
// outline or metadata answer per script. Each logs its arguments, one call
// per line, so a test sees exactly what the runtime asked.
package djvutest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake is the installed stand-ins.
type Fake struct {
	Dir string
	log string
}

// Install writes the stand-ins into a fresh directory, puts it first on
// PATH for the test, and returns it. Unix only: they are sh scripts.
func Install(t testing.TB) *Fake {
	t.Helper()
	dir := t.TempDir()
	f := &Fake{Dir: dir, log: filepath.Join(dir, "calls.log")}
	// The helper runner passes the child only PATH, so the log's path is
	// written into each script.
	txt := `#!/bin/sh
printf 'djvutxt %s\n' "$*" >> '` + f.log + `'
spec=
for a; do case $a in --page=*) spec=${a#--page=};; esac; done
IFS=,
for part in $spec; do
	lo=${part%-*}; hi=${part#*-}; n=$lo
	while [ "$n" -le "$hi" ]; do
		printf '(page 0 0 100 100 "Text of page %s, read by the stand-in for djvutxt, long enough to count as a text layer.\\n")\n' "$n"
		n=$((n+1))
	done
done
`
	sed := `#!/bin/sh
printf 'djvused %s\n' "$*" >> '` + f.log + `'
case $2 in
print-meta) printf 'title\t"A Stand-in Title"\nauthor\t"Ada Example"\n' ;;
print-outline) printf '(bookmarks ("Chapter From The Stand-in" "#1"))\n' ;;
*) exit 10 ;;
esac
`
	for name, body := range map[string]string{"djvutxt": txt, "djvused": sed} {
		// #nosec G306 -- a test helper's stand-in program under the test's temp
		// dir: it must be executable, and only its owner may run it.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// Calls are the stand-ins' invocations so far, one line each: the program's
// name and its arguments.
func (f *Fake) Calls(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}
