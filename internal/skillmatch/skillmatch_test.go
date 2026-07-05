package skillmatch

import "testing"

func TestAllowed_EmptyListAllowsEverything(t *testing.T) {
	for _, name := range []string{"seo", "doc/redactor", "marketing/x/y"} {
		if !Allowed(nil, name) {
			t.Errorf("Allowed(nil, %q) = false, want true (empty = allow all)", name)
		}
		if !Allowed([]string{}, name) {
			t.Errorf("Allowed([], %q) = false, want true (empty = allow all)", name)
		}
	}
}

func TestAllowed_DenyAllStarDeniesEverything(t *testing.T) {
	for _, deny := range [][]string{{"-*"}, {"-**"}} {
		for _, name := range []string{"seo", "doc/redactor", "marketing/x/y"} {
			if Allowed(deny, name) {
				t.Errorf("Allowed(%v, %q) = true, want false (deny all)", deny, name)
			}
		}
	}
}

func TestAllowed_WhitelistMode(t *testing.T) {
	patterns := []string{"doc/*"}
	tests := map[string]bool{
		"doc/redactor":  true, // matches doc/* (one level)
		"doc/summary":   true,
		"doc/a/b":       false, // doc/* is one level only
		"marketing/seo": false, // not under doc
		"seo":           false, // not under doc
	}
	for name, want := range tests {
		if got := Allowed(patterns, name); got != want {
			t.Errorf("Allowed(%v, %q) = %v, want %v", patterns, name, got, want)
		}
	}
}

func TestAllowed_DoubleStarRecurses(t *testing.T) {
	patterns := []string{"marketing/**"}
	tests := map[string]bool{
		"marketing/seo": true,
		"marketing/x/y": true,
		// `**` matches zero+ segments, so `marketing/**` also matches the bare
		// group name — consistent with the existing glob.go doublestar matcher.
		"marketing":    true,
		"doc/redactor": false,
	}
	for name, want := range tests {
		if got := Allowed(patterns, name); got != want {
			t.Errorf("Allowed(%v, %q) = %v, want %v", patterns, name, got, want)
		}
	}
}

func TestAllowed_ExactNameWhitelist(t *testing.T) {
	patterns := []string{"seo"}
	if !Allowed(patterns, "seo") {
		t.Errorf("Allowed(%v, seo) = false, want true", patterns)
	}
	if Allowed(patterns, "seo/x") {
		t.Errorf("Allowed(%v, seo/x) = true, want false", patterns)
	}
}

func TestAllowed_PlusPrefixIsPositive(t *testing.T) {
	patterns := []string{"+doc/*"}
	if !Allowed(patterns, "doc/redactor") {
		t.Errorf("Allowed(%v, doc/redactor) = false, want true (+ is positive)", patterns)
	}
	if Allowed(patterns, "marketing/seo") {
		t.Errorf("Allowed(%v, marketing/seo) = true, want false", patterns)
	}
}

func TestAllowed_BlacklistMode(t *testing.T) {
	// Only-negative list = blacklist: allow everything except matches.
	patterns := []string{"-marketing/*"}
	tests := map[string]bool{
		"doc/redactor":  true,  // not denied
		"seo":           true,  // not denied
		"marketing/seo": false, // denied
	}
	for name, want := range tests {
		if got := Allowed(patterns, name); got != want {
			t.Errorf("Allowed(%v, %q) = %v, want %v", patterns, name, got, want)
		}
	}
}

func TestAllowed_NegativeOverridesPositive(t *testing.T) {
	// Negatives always win regardless of order.
	for _, patterns := range [][]string{
		{"doc/*", "-doc/secret"},
		{"-doc/secret", "doc/*"},
	} {
		if Allowed(patterns, "doc/secret") {
			t.Errorf("Allowed(%v, doc/secret) = true, want false (negative wins)", patterns)
		}
		if !Allowed(patterns, "doc/redactor") {
			t.Errorf("Allowed(%v, doc/redactor) = false, want true", patterns)
		}
	}
}

func TestAllowed_PositiveWithDenyAll(t *testing.T) {
	// A deny-all negative shuts down even an explicit positive.
	patterns := []string{"doc/*", "-*"}
	if Allowed(patterns, "doc/redactor") {
		t.Errorf("Allowed(%v, doc/redactor) = true, want false (-* denies all)", patterns)
	}
}

func TestHasPositive(t *testing.T) {
	tests := map[string]struct {
		patterns []string
		want     bool
	}{
		"empty":         {nil, false},
		"only negative": {[]string{"-marketing/*"}, false},
		"bare positive": {[]string{"doc/*"}, true},
		"plus positive": {[]string{"+seo"}, true},
		"mixed":         {[]string{"-x", "doc/*"}, true},
		"deny all only": {[]string{"-*"}, false},
	}
	for name, tc := range tests {
		if got := HasPositive(tc.patterns); got != tc.want {
			t.Errorf("%s: HasPositive(%v) = %v, want %v", name, tc.patterns, got, tc.want)
		}
	}
}

func TestDeniesAll(t *testing.T) {
	tests := map[string]struct {
		patterns []string
		want     bool
	}{
		"empty":              {nil, false},
		"deny star":          {[]string{"-*"}, true},
		"deny double star":   {[]string{"-**"}, true},
		"deny star with pos": {[]string{"doc/*", "-*"}, true},
		"blacklist group":    {[]string{"-marketing/*"}, false},
		"whitelist":          {[]string{"doc/*"}, false},
		"nonexistent pos":    {[]string{"+doc/none"}, false}, // policy permits it; not a deny-all
	}
	for name, tc := range tests {
		if got := DeniesAll(tc.patterns); got != tc.want {
			t.Errorf("%s: DeniesAll(%v) = %v, want %v", name, tc.patterns, got, tc.want)
		}
	}
}
