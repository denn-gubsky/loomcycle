package builtin

import (
	"strings"
	"testing"
)

func TestNormalizePath_Canonical(t *testing.T) {
	cases := map[string]string{
		"":              "/",
		"/":             "/",
		"/docs":         "/docs",
		"/docs/x":       "/docs/x",
		"//docs//x/":    "/docs/x", // collapse empty segments + trailing slash
		"/a.b/c-d/e_f":  "/a.b/c-d/e_f",
		"/docs/launch1": "/docs/launch1",
	}
	for in, want := range cases {
		got, err := normalizePath(in)
		if err != nil {
			t.Errorf("normalize(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePath_Rejects(t *testing.T) {
	bad := []string{
		"docs/x",                      // not absolute
		"/docs/../etc",                // ".." escape
		"/docs/./x",                   // "." segment
		"/docs/a b",                   // space
		"/docs/a\tb",                  // control char
		"/docs/a/b\\c",                // backslash
		"/" + strings.Repeat("a", 65), // segment too long
		"/" + strings.Repeat("a/", maxPathSegments+1), // too many segments
	}
	for _, in := range bad {
		if got, err := normalizePath(in); err == nil {
			t.Errorf("normalize(%q) = %q, want error", in, got)
		}
	}
}

func TestSplitPath(t *testing.T) {
	cases := []struct {
		in, parent, name string
		root             bool
	}{
		{"/", "", "", true},
		{"/x", "/", "x", false},
		{"/docs/x", "/docs/", "x", false},
		{"/a/b/c", "/a/b/", "c", false},
	}
	for _, c := range cases {
		p, n, r := splitPath(c.in)
		if p != c.parent || n != c.name || r != c.root {
			t.Errorf("splitPath(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, n, r, c.parent, c.name, c.root)
		}
	}
}

func TestDirPrefix(t *testing.T) {
	if got := dirPrefix("/"); got != "/" {
		t.Errorf("dirPrefix(/) = %q, want /", got)
	}
	if got := dirPrefix("/docs"); got != "/docs/" {
		t.Errorf("dirPrefix(/docs) = %q, want /docs/", got)
	}
}
