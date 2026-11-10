// Package pinchtabpin holds a test over the images that carry the PinchTab
// MCP client: loomcycle-browser and loomcycle-media copy the same binary out
// of the same pinned pinchtab/pinchtab image, and ship its MIT notices from
// third_party/pinchtab, whose PROVENANCE.md names the version they came from.
// Dependabot bumps a FROM line per file, so the two can drift apart, and the
// notices with them.
package pinchtabpin

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// repoRoot is read as a literal so scripts/ci/affected counts this test as a
// walker and selects it on every change.
var repoRoot = filepath.Join("..", "..", "..")

var (
	fromLine    = regexp.MustCompile(`(?m)^FROM pinchtab/pinchtab:(v[0-9][^@\s]*)@(sha256:[0-9a-f]{64}) AS pinchtab$`)
	provenance  = regexp.MustCompile("\\*\\*Version:\\*\\* `(v[0-9][^`]*)`")
	dockerfiles = []string{"Dockerfile.browser", "Dockerfile.media"}
)

// "Every" in the name puts it in scripts/ci/census-tests.sh, which runs on
// every pull request: a change to a Dockerfile alone runs no Go job.
func TestPinchtabPin_EveryImageCopiesTheSamePinnedClient(t *testing.T) {
	var tag, digest, first string
	for _, name := range dockerfiles {
		raw, err := os.ReadFile(filepath.Join(repoRoot, name)) // #nosec G304 -- a fixed repository path
		if err != nil {
			t.Fatal(err)
		}
		m := fromLine.FindAllStringSubmatch(string(raw), -1)
		if len(m) != 1 {
			t.Fatalf("%s: want one `FROM pinchtab/pinchtab:<tag>@sha256:<digest> AS pinchtab` line, found %d", name, len(m))
		}
		if first == "" {
			tag, digest, first = m[0][1], m[0][2], name
			continue
		}
		if m[0][1] != tag || m[0][2] != digest {
			t.Errorf("%s pins pinchtab %s@%s, %s pins %s@%s: move both together", name, m[0][1], m[0][2], first, tag, digest)
		}
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot, "third_party", "pinchtab", "PROVENANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := provenance.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("third_party/pinchtab/PROVENANCE.md: no **Version:** line")
	}
	if m[1] != tag {
		t.Errorf("third_party/pinchtab/PROVENANCE.md names %s, the Dockerfiles pin %s: refresh the notices from that tag", m[1], tag)
	}
}
