package config

import (
	"strings"
	"testing"
	"time"
)

func TestResolveAuth_DefaultsToTokenModeWithImplicitTokenGate(t *testing.T) {
	c := &Config{}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	p := c.ResolvedAuth
	if p.ModeName() != AuthModeToken || p.SessionIdle() != 8*time.Hour || p.SessionAbsolute() != 168*time.Hour || p.ReauthWindow != 15*time.Minute {
		t.Errorf("policy = %+v", p)
	}
	g, ok := p.TokenGate()
	if !ok || g.Name != "token" || g.Kind != AuthGateKindToken {
		t.Errorf("token mode must offer the implicit token gate; got %+v, %v", g, ok)
	}
}

func TestResolveAuth_ZeroPolicyIsTokenMode(t *testing.T) {
	var p AuthPolicy
	if p.Strict() || p.ModeName() != AuthModeToken || p.SessionIdle() != DefaultSessionIdle {
		t.Errorf("zero policy = %+v", p)
	}
	if _, ok := p.TokenGate(); !ok {
		t.Error("zero policy must offer the token gate")
	}
}

func TestResolveAuth_StrictWithoutInteractiveGateFails(t *testing.T) {
	c := &Config{Auth: AuthConfig{Mode: "strict"}}
	err := resolveAuth(c)
	if err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("err = %v, want the lock-out refusal", err)
	}
	// The same refusal holds through the env default.
	c = &Config{Env: Env{AuthMode: "strict"}}
	if err := resolveAuth(c); err == nil {
		t.Fatal("LOOMCYCLE_AUTH_MODE=strict with no gate loaded")
	}
}

func TestResolveAuth_StrictListsOnlyConfiguredGates(t *testing.T) {
	c := &Config{Auth: AuthConfig{Mode: "strict", Gates: map[string]AuthGateConfig{"paste": {Kind: "token", Label: "Paste"}}}}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	gates := c.ResolvedAuth.LoginGates()
	if len(gates) != 1 || gates[0] != (AuthGate{Name: "paste", Kind: "token", Label: "Paste"}) {
		t.Errorf("strict gates = %+v, want only the listed paste gate", gates)
	}
}

func TestResolveAuth_ConfiguringAGateNeverFlipsTheMode(t *testing.T) {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token"}}}}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if c.ResolvedAuth.Strict() {
		t.Error("listing a gate turned strict mode on (decision 1)")
	}
	if g, _ := c.ResolvedAuth.TokenGate(); g.Name != "paste" {
		t.Errorf("token gate = %+v, want the listed one to replace the implicit one", g)
	}
}

// The effective mode is the stricter of auth.mode and LOOMCYCLE_AUTH_MODE:
// neither plane lowers the other, and no pair is refused for disagreeing. A
// value that is not a mode still fails the load, whichever plane holds it.
func TestResolveAuth_ModeIsTheStricterOfYAMLAndEnv(t *testing.T) {
	gates := map[string]AuthGateConfig{"paste": {Kind: "token"}}
	values := []string{"", "token", "strict", " Strict ", "TOKEN", "lax", "open"}
	// valid is the mode a value names ("" for unset), or false when it names none.
	valid := func(v string) (string, bool) {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "":
			return "", true
		case "token":
			return AuthModeToken, true
		case "strict":
			return AuthModeStrict, true
		}
		return "", false
	}
	for _, yaml := range values {
		for _, env := range values {
			c := &Config{Env: Env{AuthMode: env}, Auth: AuthConfig{Mode: yaml, Gates: gates}}
			err := resolveAuth(c)
			yamlMode, yamlOK := valid(yaml)
			envMode, envOK := valid(env)
			if !yamlOK || !envOK {
				if err == nil {
					t.Errorf("yaml=%q env=%q: loaded with a value that is not a mode", yaml, env)
					continue
				}
				bad := "LOOMCYCLE_AUTH_MODE"
				if envOK {
					bad = "auth.mode"
				}
				if !strings.Contains(err.Error(), bad) {
					t.Errorf("yaml=%q env=%q: err = %v, want it to name %s", yaml, env, err, bad)
				}
				continue
			}
			if err != nil {
				t.Errorf("yaml=%q env=%q: refused: %v", yaml, env, err)
				continue
			}
			got := c.ResolvedAuth.ModeName()
			want := max(authModeRank(yamlMode), authModeRank(envMode))
			if authModeRank(got) != want {
				t.Errorf("yaml=%q env=%q: mode = %s, want the stricter of the two", yaml, env, got)
			}
			for plane, in := range map[string]string{"yaml": yamlMode, "env": envMode} {
				if authModeRank(got) < authModeRank(in) {
					t.Errorf("yaml=%q env=%q: mode %s is weaker than the %s asked for", yaml, env, got, plane)
				}
			}
			// The plane that decided is named only when the two disagree.
			wantBy := ""
			if yamlMode != "" && envMode != "" && yamlMode != envMode {
				wantBy = "auth.mode"
				if authModeRank(envMode) > authModeRank(yamlMode) {
					wantBy = "LOOMCYCLE_AUTH_MODE"
				}
			}
			if c.ResolvedAuth.ModeDecidedBy != wantBy {
				t.Errorf("yaml=%q env=%q: ModeDecidedBy = %q, want %q", yaml, env, c.ResolvedAuth.ModeDecidedBy, wantBy)
			}
		}
	}
}

// The stricter mode holds its own requirement whichever plane asked for it:
// strict from the environment over a yaml token still needs a gate.
func TestResolveAuth_StrictFromEitherPlaneNeedsAGate(t *testing.T) {
	c := &Config{Env: Env{AuthMode: "strict"}, Auth: AuthConfig{Mode: "token"}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("err = %v, want the lock-out refusal", err)
	}
}

func TestResolveAuth_RefusesBadValues(t *testing.T) {
	cases := map[string]AuthConfig{
		"open is not a mode":   {Mode: "open"},
		"unknown mode":         {Mode: "lax"},
		"later kind":           {Gates: map[string]AuthGateConfig{"google": {Kind: "oidc"}}},
		"unknown kind":         {Gates: map[string]AuthGateConfig{"x": {Kind: "magic"}}},
		"missing kind":         {Gates: map[string]AuthGateConfig{"x": {}}},
		"bad name":             {Gates: map[string]AuthGateConfig{"Bad Name": {Kind: "token"}}},
		"two token gates":      {Gates: map[string]AuthGateConfig{"a": {Kind: "token"}, "b": {Kind: "token"}}},
		"bad duration":         {Session: AuthSessionConfig{Idle: "soon"}},
		"negative duration":    {Session: AuthSessionConfig{Absolute: "-1h"}},
		"idle beyond absolute": {Session: AuthSessionConfig{Idle: "48h", Absolute: "24h"}},
		"zero reauth window":   {Session: AuthSessionConfig{ReauthWindow: "0s"}},
	}
	for name, ac := range cases {
		if err := resolveAuth(&Config{Auth: ac}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	// Every kind the RFC defines is built; anything else is refused as unknown.
	err := resolveAuth(&Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"magic": {Kind: "magic"}}}})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("an unknown kind must be refused as unknown; got %v", err)
	}
}

func TestLoad_StrictWithoutGateFailsTheLoad(t *testing.T) {
	_, err := LoadLayers(Layer{Name: "auth.yaml", Data: []byte("auth:\n  mode: strict\n")})
	if err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("load err = %v, want the strict lock-out refusal", err)
	}
	// Strict mode turns the signed audit stream on, which needs the key.
	t.Setenv("LOOMCYCLE_SECRET_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	cfg, err := LoadLayers(Layer{Name: "auth.yaml", Data: []byte("auth:\n  mode: strict\n  session: {idle: 1h}\n  gates:\n    paste: {kind: token}\n")})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ResolvedAuth.Strict() || cfg.ResolvedAuth.SessionIdle() != time.Hour {
		t.Errorf("resolved = %+v", cfg.ResolvedAuth)
	}
}

func TestAuthModeLowered_OnlyStrictToTokenLowers(t *testing.T) {
	strict, token := AuthPolicy{Mode: AuthModeStrict}, AuthPolicy{Mode: AuthModeToken}
	if !AuthModeLowered(strict, token) || AuthModeLowered(token, strict) || AuthModeLowered(strict, strict) || AuthModeLowered(token, AuthPolicy{}) {
		t.Error("AuthModeLowered disagrees with strict > token")
	}
}

// auth.login_origins lists the origins the login page may take a bearer from
// over postMessage. None by default; entries are trimmed and lose a trailing
// slash, so they compare equal to what a browser reports as an origin.
func TestAuthLoginOrigins_YAMLList(t *testing.T) {
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.LoginOrigins) != 0 {
		t.Errorf("no block: login origins = %q, want none", cfg.Auth.LoginOrigins)
	}
	cfg, err = loadYAMLString(t, "auth:\n  login_origins: [\" https://landing.example/ \", \"https://other.example\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auth.LoginOrigins; len(got) != 2 || got[0] != "https://landing.example" || got[1] != "https://other.example" {
		t.Errorf("login origins = %q", got)
	}
	if _, err := loadYAMLString(t, "auth:\n  login_origins: [\"https://landing.example\", \"\"]\n"); err == nil || !strings.Contains(err.Error(), "auth.login_origins") {
		t.Errorf("an empty entry: err = %v, want a refusal naming the key", err)
	}

	// The variable is retired: reported when the yaml lists its origins,
	// refused when it does not.
	t.Setenv("LOOMCYCLE_UI_LOGIN_ORIGINS", "https://landing.example/")
	cfg, err = loadYAMLString(t, "auth:\n  login_origins: [\"https://landing.example\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_UI_LOGIN_ORIGINS")
	if _, err := loadYAMLString(t, "agents: {}\n"); err == nil || !strings.Contains(err.Error(), "auth.login_origins") {
		t.Errorf("a retired origin the yaml does not list: err = %v, want a refusal naming the yaml key", err)
	}
}

// The retired variable was one comma-separated value; the yaml key is a list.
// An entry that still carries several origins (or a space) matches no origin
// a browser reports, so the receiver would be on and never accept: refused,
// with the key and what to write.
func TestAuthLoginOrigins_RefusesSeveralOriginsInOneEntry(t *testing.T) {
	for _, entry := range []string{
		"https://a.example,https://b.example",
		"https://a.example, https://b.example",
		"https://a.example https://b.example",
		"https://a.example\thttps://b.example",
	} {
		_, err := loadYAMLString(t, "auth:\n  login_origins: [\"https://landing.example\", \""+entry+"\"]\n")
		if err == nil {
			t.Errorf("%q: loaded, want an error", entry)
			continue
		}
		for _, want := range []string{"auth.login_origins", "entry 2", "one origin per list item"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error does not say %q: %s", entry, want, err)
			}
		}
		if strings.Contains(err.Error(), "b.example") {
			t.Errorf("%q: error carries the entry: %s", entry, err)
		}
	}
}

// An error raised because the mode is strict names the plane that made it
// strict. Blaming `auth.mode: strict` in front of a yaml that says token
// sends the operator to the wrong file.
func TestResolveAuth_StrictErrorNamesThePlaneThatAskedForIt(t *testing.T) {
	for _, tc := range []struct {
		yaml, env string
		want, not string
	}{
		{"strict", "", "auth.mode: strict needs", "LOOMCYCLE_AUTH_MODE"},
		{"strict", "token", "auth.mode: strict needs", "LOOMCYCLE_AUTH_MODE"},
		{"strict", "strict", "auth.mode: strict needs", "LOOMCYCLE_AUTH_MODE"},
		{"token", "strict", "strict mode (from LOOMCYCLE_AUTH_MODE) needs", "auth.mode: strict"},
		{"", "strict", "strict mode (from LOOMCYCLE_AUTH_MODE) needs", "auth.mode: strict"},
	} {
		c := &Config{Env: Env{AuthMode: tc.env}, Auth: AuthConfig{Mode: tc.yaml}}
		err := resolveAuth(c)
		if err == nil || !strings.Contains(err.Error(), "interactive gate") {
			t.Fatalf("yaml=%q env=%q: err = %v, want the lock-out refusal", tc.yaml, tc.env, err)
		}
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.not) {
			t.Errorf("yaml=%q env=%q: err = %v, want %q and not %q", tc.yaml, tc.env, err, tc.want, tc.not)
		}
		// The same plane is named where strict mode is the reason the audit
		// stream is required.
		wantReason, source := "auth.mode is strict", "auth.mode"
		if tc.not == "auth.mode: strict" {
			wantReason, source = "LOOMCYCLE_AUTH_MODE is strict", "LOOMCYCLE_AUTH_MODE"
		}
		c.ResolvedAuth = AuthPolicy{Mode: AuthModeStrict, ModeSource: source}
		if got := c.AuditRequiredReason(0); got != wantReason {
			t.Errorf("yaml=%q env=%q: audit reason = %q, want %q", tc.yaml, tc.env, got, wantReason)
		}
	}
}

// Two planes that disagree about the mode are reported where every other
// advisory is, so `validate`, `doctor` and a reload print it too, and once.
func TestResolveAuth_AdvisesWhenThePlanesDisagree(t *testing.T) {
	paste := map[string]AuthGateConfig{"paste": {Kind: "token"}}
	count := func(c *Config) (n int) {
		for _, w := range c.Warnings {
			if strings.Contains(w, "auth.mode") && strings.Contains(w, "LOOMCYCLE_AUTH_MODE") {
				n++
			}
		}
		return n
	}
	c := &Config{Env: Env{AuthMode: "strict"}, Auth: AuthConfig{Mode: "token", Gates: paste}}
	for i := 0; i < 2; i++ { // resolved again: still said once
		if err := resolveAuth(c); err != nil {
			t.Fatal(err)
		}
	}
	if count(c) != 1 {
		t.Fatalf("warnings = %q, want one naming both planes", c.Warnings)
	}
	for _, want := range []string{"strict", "token", "decided by LOOMCYCLE_AUTH_MODE"} {
		if !strings.Contains(strings.Join(c.Warnings, "\n"), want) {
			t.Errorf("warning does not say %q: %q", want, c.Warnings)
		}
	}
	c = &Config{Env: Env{AuthMode: "token"}, Auth: AuthConfig{Mode: "strict", Gates: paste}}
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if count(c) != 1 || !strings.Contains(strings.Join(c.Warnings, "\n"), "decided by auth.mode") {
		t.Errorf("warnings = %q, want one saying auth.mode decided", c.Warnings)
	}
	// Agreement, or one plane silent, is nothing to report.
	for _, pair := range [][2]string{{"strict", "strict"}, {"", "strict"}, {"strict", ""}, {"", ""}} {
		c := &Config{Env: Env{AuthMode: pair[1]}, Auth: AuthConfig{Mode: pair[0], Gates: paste}}
		if err := resolveAuth(c); err != nil {
			t.Fatal(err)
		}
		if count(c) != 0 {
			t.Errorf("yaml=%q env=%q: warnings = %q, want none about the mode", pair[0], pair[1], c.Warnings)
		}
	}
}
