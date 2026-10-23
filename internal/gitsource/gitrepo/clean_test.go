package gitrepo

import (
	"fmt"
	"testing"
)

// Clean with dirs removes only untracked files: an ignored file, and the
// whole of a nested repository (its .git and its uncommitted work), stay,
// and so does a directory still holding one. A dry run lists exactly what
// the real run removes.
func TestClean_DirsKeepsIgnoredFilesAndNestedRepositories(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	write(t, fs, ".gitignore", "*.env\n")
	commitAll(t, r, c, "first")
	sub, err := fs.Chroot("vendor/lib")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(sub, ""); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "vendor/lib/work.go", "uncommitted nested work\n")
	write(t, fs, "vendor/lib/deep/more.go", "more\n")
	write(t, fs, "new/main.go", "x\n")
	write(t, fs, "new/secret.env", "KEY=1\n")
	write(t, fs, "gone/a/b/c.txt", "c\n")
	write(t, fs, "gone/d.txt", "d\n")

	want := "[gone/a/b/c.txt gone/d.txt new/main.go gone/a/b/ gone/a/ gone/]"
	dry, err := r.Clean(true, true)
	if err != nil || fmt.Sprint(dry) != want {
		t.Errorf("dry run = %v, %v; want %s", dry, err, want)
	}
	got, err := r.Clean(true, false)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(dry) {
		t.Errorf("clean = %v, %v; want what the dry run listed, %v", got, err, dry)
	}
	for _, kept := range []string{"vendor/lib/work.go", "vendor/lib/deep/more.go", "vendor/lib/.git/HEAD", "new/secret.env", "a.txt", ".gitignore"} {
		if !exists(fs, kept) {
			t.Errorf("%s was cleaned", kept)
		}
	}
	for _, gone := range []string{"new/main.go", "gone"} {
		if exists(fs, gone) {
			t.Errorf("%s survived the clean", gone)
		}
	}
}
