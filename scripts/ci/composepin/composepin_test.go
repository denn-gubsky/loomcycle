// Package composepin holds a test over the repository's compose files: every
// third-party image is pinned by digest, so a republished tag cannot change
// what an operator who copies the file runs.
package composepin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is read as a literal so scripts/ci/affected counts this test as a
// walker: every change selects it, and a new compose file is checked on the
// pull request that adds it.
var repoRoot = filepath.Join("..", "..", "..")

// ownImagePrefixes are this repository's own images. Each compose file moves
// them to a new release by hand, in step with the release (dependabot ignores
// them), so they carry the release tag, not a digest: a digest would have to be
// rewritten after every release publish, and the tag names the release a
// reader is meant to match.
var ownImagePrefixes = []string{"denngubsky/", "ghcr.io/denn-gubsky/"}

// skipDirs are not walked. cloud-deployment holds the hosted deployment's
// compose file, whose images move with that deployment's own change (it is
// pinned there, not here); node_modules and .git are not the repository's
// files.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "cloud-deployment": true}

var (
	composeName = regexp.MustCompile(`^(docker-)?compose([.-][\w.-]+)?\.ya?ml$`)
	imageLine   = regexp.MustCompile(`^\s*#?\s*image:\s*(\S+)`)
	// ${VAR:-default} or ${VAR-default}: compose uses the default when VAR is
	// unset, so the default is what a copied file runs.
	envDefault = regexp.MustCompile(`^\$\{\w+:?-(.+)\}$`)
	digest     = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// TestComposeImages_EveryThirdPartyPinnedByDigest also reads commented-out image
// lines: they are instructions to uncomment, and an operator who does runs
// whatever the tag points at that day.
//
// "Every" in the name puts it in scripts/ci/census-tests.sh, which runs on
// every pull request: a change to compose files alone runs no Go job.
func TestComposeImages_EveryThirdPartyPinnedByDigest(t *testing.T) {
	var files []string
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != repoRoot && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if composeName.MatchString(d.Name()) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that found nothing (a moved root) would pass every check below.
	for _, want := range []string{"docker-compose.cluster.yaml", filepath.Join("deploy", "truenas", "docker-compose.yaml")} {
		found := false
		for _, f := range files {
			if f == filepath.Join(repoRoot, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("walk did not find %s; found %v", want, files)
		}
	}

	images := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) // #nosec G304 -- a path from walking the repository
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(repoRoot, f)
		for i, line := range strings.Split(string(raw), "\n") {
			m := imageLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := strings.Trim(m[1], `"'`)
			if d := envDefault.FindStringSubmatch(ref); d != nil {
				ref = d[1]
			}
			if strings.Contains(ref, "${") {
				continue // no default, or a variable inside the reference
			}
			images++
			if own(ref) {
				if strings.HasSuffix(ref, ":latest") || !strings.Contains(ref[strings.LastIndex(ref, "/"):], ":") {
					t.Errorf("%s:%d: %s: name a release tag, not latest", rel, i+1, ref)
				}
				continue
			}
			if !digest.MatchString(ref) {
				t.Errorf("%s:%d: %s: pin as name:tag@sha256:<index digest>", rel, i+1, ref)
			}
		}
	}
	if images == 0 {
		t.Fatal("no image lines found")
	}
}

func own(ref string) bool {
	for _, p := range ownImagePrefixes {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}
