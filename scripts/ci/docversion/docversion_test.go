// Package docversion checks that the documents naming loomcycle's current
// version name the newest release tag.
package docversion

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// claims are the hand-written "current version" strings. Each must match
// exactly once, so rewording one fails here instead of passing vacuously.
var claims = []struct {
	file string
	re   *regexp.Regexp
}{
	{"CLAUDE.md", regexp.MustCompile(`latest tag v(\d+)\.(\d+)\.\d+`)},
	{"README.md", regexp.MustCompile(`\(currently v(\d+)\.(\d+)\)`)},
	{"SECURITY.md", regexp.MustCompile(`\| (\d+)\.(\d+)\.x \(latest minor\) \|`)},
}

// TestDocVersionDrift_EachCurrentVersionIsTheNewestTag: a release's notes pull
// request moves these strings to the version it is about to tag, so a string
// may be the newest tag's minor or the one after it (the window between the
// notes merge and the tag push), never older. CLAUDE.md said v1.142.0 two
// releases after v1.144.0 shipped, and README.md said v1.123.
//
// The newest tag comes from LOOMCYCLE_LATEST_TAG (CI reads it from the remote,
// since a shallow checkout has no tags), else from the local tags; with
// neither the test is skipped.
func TestDocVersionDrift_EachCurrentVersionIsTheNewestTag(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	latest := os.Getenv("LOOMCYCLE_LATEST_TAG")
	if latest == "" {
		latest = newestLocalTag(root)
	}
	if latest == "" {
		t.Skip("no release tag: set LOOMCYCLE_LATEST_TAG or fetch tags")
	}
	tagMajor, tagMinor, ok := majorMinor(strings.TrimPrefix(latest, "v"))
	if !ok {
		t.Fatalf("newest tag %q is not vMAJOR.MINOR.PATCH", latest)
	}
	for _, c := range claims {
		raw, err := os.ReadFile(filepath.Join(root, c.file))
		if err != nil {
			t.Fatal(err)
		}
		m := c.re.FindAllStringSubmatch(string(raw), -1)
		if len(m) != 1 {
			t.Errorf("%s: %d matches of %q, want exactly 1", c.file, len(m), c.re)
			continue
		}
		major, _ := strconv.Atoi(m[0][1])
		minor, _ := strconv.Atoi(m[0][2])
		current := major == tagMajor && minor == tagMinor
		next := (major == tagMajor && minor == tagMinor+1) || (major == tagMajor+1 && minor == 0)
		if !current && !next {
			t.Errorf("%s says %d.%d (%q); the newest tag is %s — update it in the release notes pull request",
				c.file, major, minor, m[0][0], latest)
		}
	}
}

// newestLocalTag is the highest vMAJOR.MINOR.PATCH tag in the checkout, or ""
// outside a git checkout or with no such tag.
func newestLocalTag(root string) string {
	out, err := exec.Command("git", "-C", root, "tag", "-l", "v[0-9]*", "--sort=-v:refname").Output()
	if err != nil {
		return ""
	}
	for _, tag := range strings.Fields(string(out)) {
		if _, _, ok := majorMinor(strings.TrimPrefix(tag, "v")); ok {
			return tag
		}
	}
	return ""
}

var semver = regexp.MustCompile(`^(\d+)\.(\d+)\.\d+$`)

func majorMinor(v string) (major, minor int, ok bool) {
	m := semver.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}
