package skillmatch

import "testing"

func TestValidateName(t *testing.T) {
	valid := []string{"seo", "doc/redactor", "marketing/social/x", "a-b_c/d1"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{
		"",              // empty
		"/seo",          // leading slash
		"seo/",          // trailing slash
		"doc//redactor", // double slash
		"..",            // parent
		"doc/..",        // parent segment
		"doc/../etc",    // traversal
		".",             // dot
		"doc/*",         // glob not allowed in a concrete name
		"doc/red actor", // space
		"doc/red.actor", // dot char
	}
	for _, n := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestValidatePattern(t *testing.T) {
	valid := []string{
		"seo", "+seo", "-seo",
		"doc/*", "+doc/*", "-doc/*",
		"marketing/**", "*", "**", "-*",
		"doc/red?ctor",
	}
	for _, p := range valid {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("ValidatePattern(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{
		"",        // empty
		"+",       // sign only
		"-",       // sign only
		"doc//x",  // double slash
		"/doc",    // leading slash
		"doc/",    // trailing slash
		"doc/..",  // parent segment
		"..",      // parent
		"doc/x y", // space
		"doc/x.y", // dot char
	}
	for _, p := range invalid {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("ValidatePattern(%q) = nil, want error", p)
		}
	}
}
