package gitrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitAs stages everything and commits it as who.
func commitAs(t *testing.T, r *Repo, c *clock, who, msg string) string {
	t.Helper()
	if err := r.Add([]string{"."}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := c.sig()
	s.Name, s.Email = who, strings.ToLower(who)+"@example.com"
	h, err := r.Commit(CommitOptions{Message: msg, Author: s, Committer: s})
	if err != nil {
		t.Fatalf("Commit %q: %v", msg, err)
	}
	return h
}

func blameOwners(t *testing.T, res BlameResult) []string {
	t.Helper()
	var out []string
	for i, l := range res.Lines {
		if l.Line != i+1 {
			t.Fatalf("line %d is numbered %d", i+1, l.Line)
		}
		out = append(out, l.Author+":"+l.Text)
	}
	return out
}

func TestBlame_AttributesEachLineToTheCommitThatLastChangedIt(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "f.txt", "one\ntwo\nthree\n")
	first := commitAs(t, r, c, "Ann", "first")
	write(t, fs, "other.txt", "x\n") // a commit that leaves f.txt alone
	commitAs(t, r, c, "Zed", "other")
	write(t, fs, "f.txt", "one\nTWO\nthree\nfour\n")
	second := commitAs(t, r, c, "Bob", "second")

	res, err := r.Blame("f.txt", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Ann:one", "Bob:TWO", "Ann:three", "Bob:four"}
	if got := blameOwners(t, res); fmt.Sprint(got) != fmt.Sprint(want) || res.Truncated {
		t.Fatalf("Blame = %v (truncated %v), want %v", got, res.Truncated, want)
	}
	if res.Lines[0].Hash != first || res.Lines[1].Hash != second || res.Lines[0].AuthorEmail != "ann@example.com" || res.Lines[0].Date.IsZero() {
		t.Errorf("line details = %+v", res.Lines[:2])
	}

	// maxLines cuts; an older revision blames its own version.
	res, err = r.Blame("f.txt", "", 2)
	if err != nil || len(res.Lines) != 2 || !res.Truncated {
		t.Fatalf("Blame with max 2 = %+v, %v", res, err)
	}
	res, err = r.Blame("f.txt", first, 10)
	if err != nil || fmt.Sprint(blameOwners(t, res)) != fmt.Sprint([]string{"Ann:one", "Ann:two", "Ann:three"}) {
		t.Errorf("Blame at the first commit = %v, %v", blameOwners(t, res), err)
	}
	if res, err := r.Blame("other.txt", "", 1); err != nil || res.Truncated || len(res.Lines) != 1 {
		t.Errorf("Blame of a one-line file with max 1 = %+v, %v", res, err)
	}
}

// A merge passes each line to the parent that has it; lines no parent has
// are the merge's own.
func TestBlame_FollowsEachParentOfAMerge(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "f.txt", "1\n2\n3\n")
	commitAs(t, r, c, "Ann", "base")
	if err := r.BranchCreate("side", ""); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "f.txt", "1\n2\n3m\n")
	main := commitAs(t, r, c, "Cid", "main")
	if err := r.Checkout(CheckoutOptions{Branch: "side"}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "f.txt", "1s\n2\n3\n")
	side := commitAs(t, r, c, "Bob", "side")
	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "f.txt", "1s\n2\n3m\nmerged\n")
	if err := r.Add([]string{"."}); err != nil {
		t.Fatal(err)
	}
	w, err := r.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	s := c.sig()
	sig := &object.Signature{Name: "Dan", Email: "dan@example.com", When: s.When}
	if _, err := w.Commit("merge", &git.CommitOptions{Author: sig, Committer: sig, Parents: []plumbing.Hash{mustHash(main), mustHash(side)}}); err != nil {
		t.Fatal(err)
	}
	res, err := r.Blame("f.txt", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Bob:1s", "Ann:2", "Cid:3m", "Dan:merged"}
	if got := blameOwners(t, res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Blame of a merge = %v, want %v", got, want)
	}
}

func TestBlame_RefusesBadArgumentsAndAnUnboundedChange(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "f.txt", "a\n")
	write(t, fs, "bin.dat", "a\x00b")
	commitAll(t, r, c, "first")
	for _, tc := range []struct {
		path string
		max  int
	}{{"f.txt", 0}, {".git/config", 5}, {"../x", 5}, {"missing.txt", 5}, {"bin.dat", 5}} {
		if _, err := r.Blame(tc.path, "", tc.max); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Blame(%q, max %d): %v, want invalid_argument", tc.path, tc.max, err)
		}
	}
	if _, err := r.Blame("f.txt", "nope", 5); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("Blame at a missing revision: %v", err)
	}

	// A change of more lines than a diff is allowed is refused, not
	// guessed at.
	var a, b strings.Builder
	for i := 0; i <= maxDiffLines; i++ {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	write(t, fs, "big.txt", a.String())
	commitAll(t, r, c, "big")
	write(t, fs, "big.txt", b.String())
	commitAll(t, r, c, "rewrite")
	if _, err := r.Blame("big.txt", "", 5); !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "too large to blame") {
		t.Errorf("Blame across a %d-line change: %v", maxDiffLines+1, err)
	}
	// The first version alone has no change to diff.
	if res, err := r.Blame("big.txt", "HEAD~1", 3); err != nil || len(res.Lines) != 3 {
		t.Errorf("Blame of the added file = %+v, %v", res, err)
	}
}

func grepKeys(ms []GrepMatch) []string {
	var out []string
	for _, m := range ms {
		out = append(out, fmt.Sprintf("%s:%d:%s", m.Path, m.Line, m.Text))
	}
	return out
}

func TestGrep_PatternCaseInvertPathsAndLimit(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "Hello\nworld\nhello again\n")
	write(t, fs, "bin.dat", "hello\x00")
	write(t, fs, "src/b.go", "package b\n// hello\n")
	write(t, fs, "src2/c.txt", "hello\n")
	first := commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "hello in the worktree only\n")

	check := func(o GrepOptions, want []string, wantTrunc bool) {
		t.Helper()
		got, trunc, err := r.Grep(o)
		if err != nil {
			t.Fatalf("Grep(%+v): %v", o, err)
		}
		if fmt.Sprint(grepKeys(got)) != fmt.Sprint(want) || trunc != wantTrunc {
			t.Errorf("Grep(%+v) = %v (truncated %v), want %v (truncated %v)", o, grepKeys(got), trunc, want, wantTrunc)
		}
	}
	// HEAD's tree, not the worktree; binary files skipped.
	check(GrepOptions{Pattern: "hello", MaxMatches: 10},
		[]string{"a.txt:3:hello again", "src/b.go:2:// hello", "src2/c.txt:1:hello"}, false)
	check(GrepOptions{Pattern: "hello", IgnoreCase: true, MaxMatches: 10},
		[]string{"a.txt:1:Hello", "a.txt:3:hello again", "src/b.go:2:// hello", "src2/c.txt:1:hello"}, false)
	// A path is a pathspec: "src" is not "src2".
	check(GrepOptions{Pattern: "hello", Paths: []string{"src"}, Invert: true, MaxMatches: 10},
		[]string{"src/b.go:1:package b"}, false)
	check(GrepOptions{Pattern: "hello", MaxMatches: 2},
		[]string{"a.txt:3:hello again", "src/b.go:2:// hello"}, true)
	check(GrepOptions{Pattern: "hello", MaxMatches: 3},
		[]string{"a.txt:3:hello again", "src/b.go:2:// hello", "src2/c.txt:1:hello"}, false)
	check(GrepOptions{Pattern: "^world$", Rev: first, MaxMatches: 10}, []string{"a.txt:2:world"}, false)

	// A long line comes back cut.
	write(t, fs, "long.txt", "x"+strings.Repeat("y", 2*maxLineText)+"\n")
	commitAll(t, r, c, "long")
	got, _, err := r.Grep(GrepOptions{Pattern: "^xy", MaxMatches: 1})
	if err != nil || len(got) != 1 || len(got[0].Text) != maxLineText {
		t.Errorf("Grep of a long line = %d bytes, %v", len(got[0].Text), err)
	}

	for name, o := range map[string]GrepOptions{
		"no max":         {Pattern: "x"},
		"no pattern":     {MaxMatches: 1},
		"bad regexp":     {Pattern: "(", MaxMatches: 1},
		"too long":       {Pattern: strings.Repeat("a", maxGrepPattern+1), MaxMatches: 1},
		"path into .git": {Pattern: "x", Paths: []string{".git"}, MaxMatches: 1},
	} {
		if _, _, err := r.Grep(o); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Grep %s: %v, want invalid_argument", name, err)
		}
	}
	if _, _, err := r.Grep(GrepOptions{Pattern: strings.Repeat("a", maxGrepPattern), MaxMatches: 1}); err != nil {
		t.Errorf("a pattern at the limit: %v", err)
	}
	empty, _, _ := memRepo(t)
	if got, trunc, err := empty.Grep(GrepOptions{Pattern: "x", MaxMatches: 1}); err != nil || len(got) != 0 || trunc {
		t.Errorf("Grep of an unborn branch = %v, %v, %v", got, trunc, err)
	}
}

// storeObject encodes o into r's store.
func storeObject(t *testing.T, r *Repo, o interface {
	Encode(plumbing.EncodedObject) error
}) plumbing.Hash {
	t.Helper()
	obj := r.repo.Storer.NewEncodedObject()
	if err := o.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func storeBlob(t *testing.T, r *Repo, data string) plumbing.Hash {
	t.Helper()
	obj := r.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNotes_ListsNotesNewestFirstFlatAndFannedOut(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	c1 := commitAll(t, r, c, "one")
	write(t, fs, "a.txt", "2\n")
	c2 := commitAll(t, r, c, "two")
	write(t, fs, "a.txt", "3\n")
	c3 := commitAll(t, r, c, "three")

	if got, err := r.Notes("", 10); err != nil || len(got) != 0 {
		t.Fatalf("Notes with no notes ref = %v, %v", got, err)
	}

	// c1's note sits flat, c3's in a fan-out directory, as git lays them
	// out once the tree grows; a stray entry is not a note.
	long := strings.Repeat("n", maxNoteText+100)
	fan := &object.Tree{Entries: []object.TreeEntry{{Name: c3[2:], Mode: filemode.Regular, Hash: storeBlob(t, r, "note three\n")}}}
	entries := []object.TreeEntry{
		{Name: c1, Mode: filemode.Regular, Hash: storeBlob(t, r, long)},
		{Name: c3[:2], Mode: filemode.Dir, Hash: storeObject(t, r, fan)},
		{Name: "README", Mode: filemode.Regular, Hash: storeBlob(t, r, "not a note")},
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	s := c.sig()
	sig := object.Signature{Name: s.Name, Email: s.Email, When: s.When}
	notes := storeObject(t, r, &object.Commit{Author: sig, Committer: sig, Message: "notes\n", TreeHash: storeObject(t, r, &object.Tree{Entries: entries})})
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(notesRef, notes)); err != nil {
		t.Fatal(err)
	}

	got, err := r.Notes("", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("Notes = %+v, %v", got, err)
	}
	if got[0] != (NoteInfo{Commit: c3, Note: "note three\n"}) || got[1].Commit != c1 || got[1].Note != long[:maxNoteText] {
		t.Errorf("Notes = %q / %s (%d bytes)", got[0], got[1].Commit, len(got[1].Note))
	}
	if got, err := r.Notes("", 1); err != nil || len(got) != 1 || got[0].Commit != c3 {
		t.Errorf("Notes max 1 = %+v, %v", got, err)
	}
	if got, err := r.Notes(c2, 10); err != nil || len(got) != 1 || got[0].Commit != c1 {
		t.Errorf("Notes from c2 = %+v, %v", got, err)
	}
	if _, err := r.Notes("", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Notes max 0: %v", err)
	}
}

// Notes written by git itself read back.
func TestNotes_ReadsNotesGitWrote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("SKIPPED, NOT PASSED: git is not installed: %v", err)
	}
	wt, dir := osWorktree(t)
	r, err := Init(wt, "")
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	write(t, wt, "a.txt", "1\n")
	h := commitAll(t, r, c, "one")
	run(t, dir, "notes", "add", "-m", "reviewed", h)
	got, err := r.Notes("", 5)
	if err != nil || len(got) != 1 || got[0] != (NoteInfo{Commit: h, Note: "reviewed\n"}) {
		t.Errorf("Notes = %+v, %v", got, err)
	}
}

// testSigner records what it was asked to sign and returns an armored
// block made from it.
type testSigner struct{ payload []byte }

func (s *testSigner) Sign(message io.Reader) ([]byte, error) {
	b, err := io.ReadAll(message)
	if err != nil {
		return nil, err
	}
	s.payload = b
	return []byte(fmt.Sprintf("-----BEGIN PGP SIGNATURE-----\n\nc2lnbmVk%d\n-----END PGP SIGNATURE-----\n", len(b))), nil
}

func (s *testSigner) sig() string {
	return fmt.Sprintf("-----BEGIN PGP SIGNATURE-----\n\nc2lnbmVk%d\n-----END PGP SIGNATURE-----\n", len(s.payload))
}

func encodedWithoutSignature(t *testing.T, o interface {
	EncodeWithoutSignature(plumbing.EncodedObject) error
}) []byte {
	t.Helper()
	m := &plumbing.MemoryObject{}
	if err := o.EncodeWithoutSignature(m); err != nil {
		t.Fatal(err)
	}
	rd, err := m.Reader()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTagCreateDelete_LightweightAnnotatedForceAndSigned(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	first := commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "b\n")
	second := commitAll(t, r, c, "second")
	tagger := c.sig()

	light, err := r.TagCreate(TagOptions{Name: "light", Rev: first})
	if err != nil || light != (TagInfo{Name: "light", Hash: first}) {
		t.Fatalf("lightweight TagCreate = %+v, %v", light, err)
	}
	ann, err := r.TagCreate(TagOptions{Name: "v1", Message: "  release one\n\n", Tagger: tagger})
	if err != nil || ann != (TagInfo{Name: "v1", Hash: second, Annotated: true, Message: "release one\n"}) {
		t.Fatalf("annotated TagCreate = %+v, %v", ann, err)
	}
	tags, err := r.Tags()
	if err != nil || len(tags) != 2 || tags[0] != light || tags[1] != ann {
		t.Errorf("Tags = %+v, %v", tags, err)
	}
	ref, _ := r.repo.Reference(plumbing.NewTagReferenceName("v1"), false)
	tag, err := r.repo.TagObject(ref.Hash())
	if err != nil || tag.Tagger.Name != tagger.Name || !tag.Tagger.When.Equal(tagger.When) || tag.PGPSignature != "" {
		t.Errorf("tag object = %+v, %v", tag, err)
	}

	if _, err := r.TagCreate(TagOptions{Name: "light"}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("an existing tag: %v", err)
	}
	if moved, err := r.TagCreate(TagOptions{Name: "light", Force: true}); err != nil || moved.Hash != second {
		t.Errorf("a forced tag = %+v, %v", moved, err)
	}
	for name, o := range map[string]TagOptions{
		"bad name":            {Name: "-x"},
		"name with a space":   {Name: "a b"},
		"ref-like name":       {Name: "refs/tags/x"},
		"annotated no tagger": {Name: "t1", Message: "m"},
		"blank message":       {Name: "t2", Message: "  ", Tagger: tagger},
		"signed lightweight":  {Name: "t3", Signer: &testSigner{}},
	} {
		if _, err := r.TagCreate(o); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("TagCreate %s: %v, want invalid_argument", name, err)
		}
	}
	if _, err := r.TagCreate(TagOptions{Name: "t4", Rev: "nope"}); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("TagCreate at a missing revision: %v", err)
	}

	// A signed tag stores what the signer returned, over the tag's
	// encoding without a signature.
	signer := &testSigner{}
	if _, err := r.TagCreate(TagOptions{Name: "v2", Message: "signed", Tagger: tagger, Signer: signer}); err != nil {
		t.Fatal(err)
	}
	ref, _ = r.repo.Reference(plumbing.NewTagReferenceName("v2"), false)
	stag, err := r.repo.TagObject(ref.Hash())
	if err != nil || stag.PGPSignature != signer.sig() || strings.TrimSpace(stag.Message) != "signed" {
		t.Fatalf("signed tag = %+v, %v", stag, err)
	}
	if !bytes.Equal(encodedWithoutSignature(t, stag), signer.payload) {
		t.Errorf("the signed payload is not the stored tag without its signature:\n%s\n---\n%s", signer.payload, encodedWithoutSignature(t, stag))
	}

	if err := r.TagDelete("v1"); err != nil {
		t.Fatal(err)
	}
	if err := r.TagDelete("v1"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("deleting a missing tag: %v", err)
	}
	tags, _ = r.Tags()
	if len(tags) != 2 || tags[0].Name != "light" || tags[1].Name != "v2" {
		t.Errorf("Tags after delete = %+v", tags)
	}
}

func TestCommit_SignerSignsTheCommit(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	if err := r.Add([]string{"."}); err != nil {
		t.Fatal(err)
	}
	signer := &testSigner{}
	s := c.sig()
	h, err := r.Commit(CommitOptions{Message: "signed", Author: s, Committer: s, Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.repo.CommitObject(mustHash(h))
	if err != nil || commit.PGPSignature != signer.sig() {
		t.Fatalf("PGPSignature = %q, %v; want %q", commit.PGPSignature, err, signer.sig())
	}
	if !bytes.Equal(encodedWithoutSignature(t, commit), signer.payload) {
		t.Errorf("the signed payload is not the stored commit without its signature")
	}
	// No signer, no signature.
	write(t, fs, "a.txt", "b\n")
	h = commitAll(t, r, c, "plain")
	if commit, _ := r.repo.CommitObject(mustHash(h)); commit.PGPSignature != "" {
		t.Errorf("an unsigned commit has a signature")
	}
}

func TestRemoteAddRemoveSetURL(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	h := commitAll(t, r, c, "first")
	if err := r.RemoteAdd("up", "https://example.com/acme/app.git"); err != nil {
		t.Fatal(err)
	}
	if u, err := r.RemoteURL("up"); err != nil || u != "https://example.com/acme/app.git" {
		t.Errorf("RemoteURL = %q, %v", u, err)
	}
	if err := r.RemoteAdd("up", "https://example.com/other.git"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("adding an existing remote: %v", err)
	}
	for _, bad := range [][2]string{{"a/b", "https://x/y"}, {"-x", "https://x/y"}, {"", "https://x/y"}, {"ok", ""}, {"ok", "https://x/\ny"}} {
		if err := r.RemoteAdd(bad[0], bad[1]); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("RemoteAdd(%q, %q): %v, want invalid_argument", bad[0], bad[1], err)
		}
	}

	// set-url replaces the URL, also when an insteadOf rule rewrote the one
	// read (the library would write the old one back).
	cfg := read(t, fs, ".git/config") + "[url \"https://mirror.example/\"]\n\tinsteadOf = https://example.com/\n"
	if err := util.WriteFile(fs, ".git/config", []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoteSetURL("up", "https://new.example/acme/app.git"); err != nil {
		t.Fatal(err)
	}
	if u, err := r.RemoteURL("up"); err != nil || u != "https://new.example/acme/app.git" {
		t.Errorf("RemoteURL after set-url = %q, %v", u, err)
	}
	if strings.Contains(read(t, fs, ".git/config"), "url = https://example.com/acme/app.git") {
		t.Errorf("the old URL is still in the config:\n%s", read(t, fs, ".git/config"))
	}
	if err := r.RemoteSetURL("nope", "https://x/y"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("RemoteSetURL of a missing remote: %v", err)
	}

	// remove drops the section, the tracking refs and the upstream.
	tracking := plumbing.NewRemoteReferenceName("up", "main")
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(tracking, mustHash(h))); err != nil {
		t.Fatal(err)
	}
	bcfg, _ := r.repo.Config()
	bcfg.Branches["main"] = &config.Branch{Name: "main", Remote: "up", Merge: "refs/heads/main"}
	if err := r.repo.Storer.SetConfig(bcfg); err != nil {
		t.Fatal(err)
	}
	if bs, _ := r.Branches(); len(bs) != 1 || bs[0].Upstream != "up/main" {
		t.Fatalf("Branches before remove = %+v", bs)
	}
	if err := r.RemoteRemove("up"); err != nil {
		t.Fatal(err)
	}
	if rs, err := r.Remotes(); err != nil || len(rs) != 0 {
		t.Errorf("Remotes after remove = %+v, %v", rs, err)
	}
	if _, err := r.repo.Reference(tracking, false); err == nil {
		t.Error("the tracking ref survived the remove")
	}
	if bs, _ := r.Branches(); len(bs) != 1 || bs[0].Upstream != "" {
		t.Errorf("Branches after remove = %+v", bs)
	}
	if err := r.RemoteRemove("up"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("removing a missing remote: %v", err)
	}
}

// sparseRepo is a repository of four files over two commits.
func sparseRepo(t *testing.T) (*Repo, billy.Filesystem, string, string) {
	t.Helper()
	r, fs, c := memRepo(t)
	write(t, fs, "a/1.txt", "one\n")
	write(t, fs, "b/2.txt", "two\n")
	write(t, fs, "a2/x.txt", "x\n")
	write(t, fs, "top.txt", "top\n")
	first := commitAll(t, r, c, "first")
	write(t, fs, "b/2.txt", "two, changed\n")
	second := commitAll(t, r, c, "second")
	return r, fs, first, second
}

func present(fs billy.Filesystem, paths ...string) string {
	var out []string
	for _, p := range paths {
		if exists(fs, p) {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func TestCheckout_SparseMaterialisesOnlyTheListedDirectories(t *testing.T) {
	all := []string{"a/1.txt", "a2/x.txt", "b/2.txt", "top.txt"}
	r, fs, first, _ := sparseRepo(t)

	// On the branch already checked out: the others leave the worktree.
	if err := r.Checkout(CheckoutOptions{Branch: "main", SparseDirs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if got := present(fs, all...); got != "a/1.txt" {
		t.Errorf("after a sparse checkout of a: %s", got)
	}
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("status after a sparse checkout = %v", st)
	}
	if d, _, err := r.Diff(DiffOptions{MaxBytes: 1 << 20}); err != nil || d != "" {
		t.Errorf("diff after a sparse checkout = %q, %v", d, err)
	}
	idx, _ := r.repo.Storer.Index()
	if len(idx.Entries) != 4 {
		t.Errorf("the index has %d entries, want all 4", len(idx.Entries))
	}

	// To another commit, with a wider set: b comes back, at that commit.
	if err := r.Checkout(CheckoutOptions{Rev: first, SparseDirs: []string{"a", "b/"}}); err != nil {
		t.Fatal(err)
	}
	if got := present(fs, all...); got != "a/1.txt b/2.txt" || read(t, fs, "b/2.txt") != "two\n" {
		t.Errorf("after a sparse checkout of a and b at the first commit: %s", got)
	}
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("status = %v", st)
	}

	// Forced, to the branch, narrowing to b.
	if err := r.Checkout(CheckoutOptions{Branch: "main", Force: true, SparseDirs: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if got := present(fs, all...); got != "b/2.txt" || read(t, fs, "b/2.txt") != "two, changed\n" {
		t.Errorf("after a forced sparse checkout of b: %s", got)
	}
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("status = %v", st)
	}
}

func TestCheckout_SparseRefusesDirtyWorkAndBadDirectories(t *testing.T) {
	r, fs, first, _ := sparseRepo(t)
	write(t, fs, "top.txt", "my change\n")
	if err := r.Checkout(CheckoutOptions{Branch: "main", SparseDirs: []string{"a"}}); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("a sparse checkout over a change: %v, want worktree_dirty", err)
	}
	if read(t, fs, "top.txt") != "my change\n" {
		t.Error("the refused checkout touched the change")
	}
	for name, o := range map[string]CheckoutOptions{
		".git":       {Rev: first, SparseDirs: []string{".git"}},
		"escape":     {Rev: first, SparseDirs: []string{"../x"}},
		"absolute":   {Rev: first, SparseDirs: []string{"/a"}},
		"with keep":  {Branch: "main", Keep: true, SparseDirs: []string{"a"}},
		"empty name": {Rev: first, SparseDirs: []string{""}},
	} {
		if err := r.Checkout(o); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Checkout %s: %v, want invalid_argument", name, err)
		}
	}
}

// gitlinkRepo is a repository whose HEAD records a submodule at "lib" and
// at "vendor/lib", and whose .gitmodules is modules.
func gitlinkRepo(t *testing.T, modules string) (*Repo, billy.Filesystem, string) {
	t.Helper()
	r, fs, c := memRepo(t)
	write(t, fs, ".gitmodules", modules)
	if err := r.Add([]string{".gitmodules"}); err != nil {
		t.Fatal(err)
	}
	const sub = "1111111111111111111111111111111111111111"
	idx, err := r.repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	idx.Entries = append(idx.Entries,
		&index.Entry{Name: "lib", Mode: filemode.Submodule, Hash: mustHash(sub)},
		&index.Entry{Name: "vendor/lib", Mode: filemode.Submodule, Hash: mustHash(sub)})
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Name < idx.Entries[j].Name })
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	s := c.sig()
	if _, err := r.Commit(CommitOptions{Message: "add lib", Author: s, Committer: s}); err != nil {
		t.Fatal(err)
	}
	return r, fs, sub
}

func TestSubmodules_ListsResolvedURLsAndGitlinks(t *testing.T) {
	r, _, sub := gitlinkRepo(t, "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib.git\n"+
		"[submodule \"abs\"]\n\tpath = vendor/abs\n\turl = https://other.example/abs.git\n")
	got, err := r.Submodules()
	if err != nil {
		t.Fatal(err)
	}
	// No origin: the relative URL cannot be resolved.
	want := []SubmoduleInfo{
		{Name: "abs", Path: "vendor/abs", URL: "https://other.example/abs.git"},
		{Name: "lib", Path: "lib", Commit: sub},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Submodules = %+v, want %+v", got, want)
	}
	if err := r.RemoteAdd("origin", "https://forge.example/acme/app.git"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Submodules()
	if len(got) != 2 || got[1].URL != "https://forge.example/acme/lib.git" {
		t.Errorf("Submodules with an origin = %+v", got)
	}
}

func TestResolveSubmoduleURL_FollowsGitsRelativeRules(t *testing.T) {
	base := "https://forge.example/acme/app.git"
	for raw, want := range map[string]string{
		"https://x.example/y.git": "https://x.example/y.git",
		"../lib.git":              "https://forge.example/acme/lib.git",
		"./lib.git":               "https://forge.example/acme/app.git/lib.git",
		"../../other/lib.git":     "https://forge.example/other/lib.git",
		"../../../lib.git":        "",
		"../a/../b.git":           "",
		"../":                     "",
	} {
		if got := resolveSubmoduleURL(raw, base); got != want {
			t.Errorf("resolveSubmoduleURL(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := resolveSubmoduleURL("../lib.git", ""); got != "" {
		t.Errorf("no base: %q", got)
	}
	if got := resolveSubmoduleURL("../lib.git", "https://forge.example/acme/app.git/"); got != "https://forge.example/acme/lib.git" {
		t.Errorf("a base with a trailing slash: %q", got)
	}
}

// What .gitmodules says is checked before anything is dialled: a path into
// .git, a path HEAD records no gitlink at, an unknown name, and a link where
// the submodule's directory should be.
func TestSubmoduleUpdate_RefusesPathsItMustNotUse(t *testing.T) {
	n := Network{URL: "https://forge.example/acme/lib.git", MaxObjectBytes: 1, SpoolDir: t.TempDir()}
	r, fs, _ := gitlinkRepo(t, "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib.git\n"+
		"[submodule \"evil\"]\n\tpath = .git/x\n\turl = ../lib.git\n"+
		"[submodule \"evil2\"]\n\tpath = sub/.GIT\n\turl = ../lib.git\n"+
		"[submodule \"nolink\"]\n\tpath = nolink\n\turl = ../lib.git\n"+
		"[submodule \"nested\"]\n\tpath = vendor/lib\n\turl = ../lib.git\n")
	for _, name := range []string{"evil", "evil2", "nolink", "unknown"} {
		if _, err := r.SubmoduleUpdate(t.Context(), name, n); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("SubmoduleUpdate(%s): %v, want invalid_argument", name, err)
		}
	}
	if err := fs.Symlink(".git", "lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SubmoduleUpdate(t.Context(), "lib", n); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("SubmoduleUpdate through a link to .git: %v, want invalid_argument", err)
	}
	// A link above the submodule's directory is refused too.
	if err := fs.Symlink(".git", "vendor"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SubmoduleUpdate(t.Context(), "nested", n); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("SubmoduleUpdate below a link to .git: %v, want invalid_argument", err)
	}
	if exists(fs, ".git/.git") || exists(fs, ".git/lib") {
		t.Error("something was written into .git")
	}
	if err := fs.Remove("lib"); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "lib/file.txt", "mine\n")
	if _, err := r.SubmoduleUpdate(t.Context(), "lib", n); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("SubmoduleUpdate into a directory of files: %v, want already_exists", err)
	}
}

// After a sparse checkout, the operations that reset the index to a commit
// keep the left-out entries true to that commit, and left out.
func TestSparse_LaterResetsKeepLeftOutEntriesTrue(t *testing.T) {
	r, fs, first, second := sparseRepo(t)
	if err := r.Checkout(CheckoutOptions{Branch: "main", SparseDirs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	entry := func(p string) *index.Entry {
		t.Helper()
		idx, err := r.repo.Storer.Index()
		if err != nil {
			t.Fatal(err)
		}
		e, err := idx.Entry(p)
		if err != nil {
			t.Fatalf("no index entry %s: %v", p, err)
		}
		return e
	}
	blobAt := func(rev, p string) plumbing.Hash {
		t.Helper()
		c, err := r.resolveCommit(rev)
		if err != nil {
			t.Fatal(err)
		}
		h, ok, err := blameEntry(c, p)
		if err != nil || !ok {
			t.Fatalf("%s at %s: %v", p, rev, err)
		}
		return h
	}
	for _, step := range []struct {
		name string
		do   func() error
		rev  string
	}{
		{"hard reset", func() error { return r.Reset("hard", first, nil) }, first},
		{"mixed reset", func() error { return r.Reset("mixed", second, nil) }, second},
		{"checkout", func() error { return r.Checkout(CheckoutOptions{Rev: first}) }, first},
		{"merge", func() error { _, err := r.MergeFF(second); return err }, second},
		{"soft reset, then restore staged", func() error {
			if err := r.Reset("soft", first, nil); err != nil {
				return err
			}
			return r.Restore([]string{"b"}, true, false)
		}, first},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		e := entry("b/2.txt")
		if !e.SkipWorktree || e.Hash != blobAt(step.rev, "b/2.txt") {
			t.Errorf("after %s: b/2.txt skip %v, hash %s; want skipped at %s's", step.name, e.SkipWorktree, e.Hash, step.rev)
		}
		if st := statusMap(t, r); len(st) != 0 {
			t.Errorf("status after %s = %v", step.name, st)
		}
		if exists(fs, "b/2.txt") || exists(fs, "top.txt") {
			t.Errorf("after %s a left-out file is back", step.name)
		}
	}
}
