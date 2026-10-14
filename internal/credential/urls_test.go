package credential

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func urls(u ...string) *[]string { return &u }

func TestURLAllowed_MatchesByHostAndWholePathSegments(t *testing.T) {
	list := urls("https://github.com/acme/app")
	for target, want := range map[string]bool{
		"https://github.com/acme/app":              true,
		"https://github.com/acme/app.git":          true,
		"https://github.com/acme/app/":             true,
		"https://GitHub.com/acme/app":              true, // host case
		"https://github.com:443/acme/app":          true, // the default port
		"https://github.com/acme/app/info/refs":    true, // under it
		"https://github.com/acme/app-fork":         false,
		"https://github.com/acme/application":      false,
		"https://github.com/acme":                  false,
		"https://github.com/":                      false,
		"https://github.com/acme/App":              false, // paths are compared as written
		"https://github.com.evil.example/acme/app": false,
		"https://evil.example/github.com/acme/app": false,
		"https://github.com:8443/acme/app":         false,
		"http://github.com/acme/app":               false,
		"https://token@github.com/acme/app":        false,
		"https://github.com/acme/app?x=1":          false,
		"https://github.com/acme/app#frag":         false,
		"https://github.com/acme/../acme/app":      false,
		"https://github.com/acme/%61pp":            false,
		"https://github.com/acme/app\n":            false,
		"":                                         false,
		"github.com/acme/app":                      false,
	} {
		if got := URLAllowed(list, target); got != want {
			t.Errorf("URLAllowed(%q) = %v, want %v", target, got, want)
		}
	}
}

func TestURLAllowed_NoListAllowsAndAnEmptyListAllowsNothing(t *testing.T) {
	if !URLAllowed(nil, "https://anything.example/x") {
		t.Error("an unrestricted credential refused a URL")
	}
	if URLAllowed(urls(), "https://anything.example/x") {
		t.Error("an empty list allowed a URL; it means the value is sent nowhere")
	}
	// A host-wide entry covers every path of that host and no other host.
	host := urls("https://gitea.example")
	if !URLAllowed(host, "https://gitea.example/a/b.git") || URLAllowed(host, "https://gitea.example.org/a/b") {
		t.Error("a host-wide entry matched wrongly")
	}
}

func TestNormalizeAllowedURLs_CanonicalSortedAndRefusesWhatCouldMislead(t *testing.T) {
	got, err := NormalizeAllowedURLs(urls("https://GitHub.com/acme/app.git/", "https://gitea.example:443", "https://github.com/acme/app"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://gitea.example", "https://github.com/acme/app"}; !reflect.DeepEqual(*got, want) {
		t.Errorf("normalised = %v, want %v", *got, want)
	}
	if n, err := NormalizeAllowedURLs(nil); n != nil || err != nil {
		t.Errorf("nil must stay nil (no restriction): %v, %v", n, err)
	}
	if n, err := NormalizeAllowedURLs(urls()); err != nil || n == nil || len(*n) != 0 {
		t.Errorf("an empty list must stay an empty list (sent nowhere): %v, %v", n, err)
	}
	for _, bad := range []string{
		"http://github.com/a", "ssh://git@github.com/a", "https://user:pw@github.com/a", "https://github.com/a?b=c",
		"https://github.com/a#b", "https://github.com/a/../b", "https://github.com/%2e%2e/b", "https:///nohost", "not a url", "",
	} {
		if _, err := NormalizeAllowedURLs(urls(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	many := make([]string, maxAllowedURLs+1)
	for i := range many {
		many[i] = "https://h.example/" + strings.Repeat("a", i+1)
	}
	if _, err := NormalizeAllowedURLs(&many); err == nil {
		t.Error("a list over the limit was accepted")
	}
}

// A restricted credential is handed out only to a caller that names a
// destination under its list. A caller that names none is refused: that is
// every consumer that existed before the restriction did.
func TestEngine_ARestrictedCredentialResolvesOnlyForAnAllowedURL(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := netguard.WithPinnable(context.Background())
	id := Identity{TenantID: "t", Scope: "tenant", Name: "repo"}
	meta, err := e.PutInline(ctx, id, "ghs_secret", nil, urls("https://github.com/acme/app"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.AllowedURLs == nil || len(*meta.AllowedURLs) != 1 {
		t.Fatalf("the stored metadata lost the list: %v", meta.AllowedURLs)
	}

	res, found, err := e.ResolveFor(ctx, "t", "", "", "repo", "https://github.com/acme/app.git")
	if err != nil || !found || res.Value != "ghs_secret" || res.AllowedURLs == nil {
		t.Fatalf("an allowed destination: %+v found=%v err=%v", res, found, err)
	}
	if res, _, err := e.ResolveFor(ctx, "t", "", "", "repo", "https://github.com/acme/other"); !errors.Is(err, ErrURLNotAllowed) || res.Value != "" {
		t.Errorf("another destination: value=%q err=%v, want ErrURLNotAllowed and no value", res.Value, err)
	}
	if res, _, err := e.Resolve(ctx, "t", "", "", "repo"); !errors.Is(err, ErrRestricted) || res.Value != "" {
		t.Errorf("a caller that names no destination: value=%q err=%v, want ErrRestricted and no value", res.Value, err)
	}
	if out, _, err := e.Substitute(ctx, "t", "", "", "Bearer $cred:repo", "https://github.com/acme/other", nil); err == nil || strings.Contains(out, "ghs_secret") {
		t.Errorf("a header for another URL got the restricted value: %q err=%v", out, err)
	}
	if out, _, err := e.Substitute(ctx, "t", "", "", "Bearer $cred:repo", "https://github.com/acme/app/mcp", nil); err != nil || out != "Bearer ghs_secret" {
		t.Errorf("a header for a covered URL: %q err=%v", out, err)
	}
	if has, _ := e.HasKey(ctx, "t", "", "", "repo"); has {
		t.Error("HasKey reported a restricted credential as a usable key")
	}

	// "Sent nowhere" resolves for no destination at all.
	nowhere := Identity{TenantID: "t", Scope: "tenant", Name: "hook-secret"}
	if _, err := e.PutInline(ctx, nowhere, "whsec", nil, urls(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ResolveFor(ctx, "t", "", "", "hook-secret", "https://github.com/acme/app"); !errors.Is(err, ErrURLNotAllowed) {
		t.Errorf("an empty list resolved for a URL: %v", err)
	}

	// An unrestricted credential is unchanged, with or without a destination.
	plain := Identity{TenantID: "t", Scope: "tenant", Name: "plain"}
	if _, err := e.PutInline(ctx, plain, "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() (Resolved, bool, error){
		func() (Resolved, bool, error) { return e.Resolve(ctx, "t", "", "", "plain") },
		func() (Resolved, bool, error) { return e.ResolveFor(ctx, "t", "", "", "plain", "https://x.example") },
	} {
		if res, found, err := f(); err != nil || !found || res.Value != "v" || res.AllowedURLs != nil {
			t.Errorf("an unrestricted credential: %+v found=%v err=%v", res, found, err)
		}
	}
}

// A restricted credential of a more specific scope is not bypassed by an
// unrestricted one of the same name further down.
func TestEngine_ARestrictedCredentialIsNotShadowedAwayByALessSpecificOne(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "agent", ScopeID: "reviewer", Name: "repo"}, "agent-secret", nil, urls("https://github.com/acme/app"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "tenant", Name: "repo"}, "tenant-secret", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if res, _, err := e.Resolve(ctx, "t", "reviewer", "", "repo"); !errors.Is(err, ErrRestricted) || res.Value != "" {
		t.Errorf("fell through to the tenant's unrestricted credential: value=%q err=%v", res.Value, err)
	}
}

// The list is stored in the clear and sealed together with the value. Editing
// it, removing it, or adding one to a credential that had none must leave a
// value that no longer opens, not a wider one.
func TestEngine_AnEditedRestrictionNoLongerOpens(t *testing.T) {
	ctx := netguard.WithPinnable(context.Background())
	rewrite := func(t *testing.T, e *Engine, id Identity, f func(def map[string]json.RawMessage)) {
		t.Helper()
		row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
		if err != nil {
			t.Fatal(err)
		}
		var def map[string]json.RawMessage
		if err := json.Unmarshal(row.Definition, &def); err != nil {
			t.Fatal(err)
		}
		f(def)
		row.Definition, _ = json.Marshal(def)
		if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	id := Identity{TenantID: "t", Scope: "tenant", Name: "repo"}
	target := "https://evil.example/x"

	t.Run("the list is widened", func(t *testing.T) {
		e := newEngine(t, testKey(1))
		if _, err := e.PutInline(ctx, id, "s", nil, urls("https://github.com/acme/app"), nil); err != nil {
			t.Fatal(err)
		}
		rewrite(t, e, id, func(def map[string]json.RawMessage) {
			def["allowed_urls"] = json.RawMessage(`["https://evil.example"]`)
		})
		if res, _, err := e.ResolveFor(ctx, "t", "", "", "repo", target); !errors.Is(err, ErrDecrypt) || res.Value != "" {
			t.Errorf("value=%q err=%v, want ErrDecrypt and no value", res.Value, err)
		}
	})
	t.Run("the list is removed", func(t *testing.T) {
		e := newEngine(t, testKey(1))
		if _, err := e.PutInline(ctx, id, "s", nil, urls("https://github.com/acme/app"), nil); err != nil {
			t.Fatal(err)
		}
		rewrite(t, e, id, func(def map[string]json.RawMessage) { delete(def, "allowed_urls") })
		if res, _, err := e.Resolve(ctx, "t", "", "", "repo"); !errors.Is(err, ErrDecrypt) || res.Value != "" {
			t.Errorf("value=%q err=%v, want ErrDecrypt and no value", res.Value, err)
		}
	})
	t.Run("an empty list is removed", func(t *testing.T) {
		e := newEngine(t, testKey(1))
		if _, err := e.PutInline(ctx, id, "s", nil, urls(), nil); err != nil {
			t.Fatal(err)
		}
		rewrite(t, e, id, func(def map[string]json.RawMessage) { delete(def, "allowed_urls") })
		if res, _, err := e.Resolve(ctx, "t", "", "", "repo"); !errors.Is(err, ErrDecrypt) || res.Value != "" {
			t.Errorf("value=%q err=%v, want ErrDecrypt and no value", res.Value, err)
		}
	})
	t.Run("a list is added to an unrestricted credential", func(t *testing.T) {
		e := newEngine(t, testKey(1))
		if _, err := e.PutInline(ctx, id, "s", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		rewrite(t, e, id, func(def map[string]json.RawMessage) {
			def["allowed_urls"] = json.RawMessage(`["https://evil.example"]`)
		})
		if res, _, err := e.ResolveFor(ctx, "t", "", "", "repo", target); !errors.Is(err, ErrDecrypt) || res.Value != "" {
			t.Errorf("value=%q err=%v, want ErrDecrypt and no value", res.Value, err)
		}
	})
}

// A value stored before the field existed has no list in its definition and
// must open exactly as it did.
func TestEngine_ACredentialStoredBeforeTheFieldStillOpens(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := Identity{TenantID: "t", Scope: "tenant", Name: "old"}
	sealed, err := e.sealer.Seal([]byte("old-secret"), id) // the pre-field path: nothing bound
	if err != nil {
		t.Fatal(err)
	}
	def, _ := json.Marshal(map[string]any{"value": sealed}) // the pre-field stored shape
	if _, err := e.store.CredentialDefPut(ctx, store.CredentialDefRow{TenantID: "t", Scope: "tenant", Name: "old", Backend: BackendInline, Definition: def}); err != nil {
		t.Fatal(err)
	}
	if res, found, err := e.Resolve(ctx, "t", "", "", "old"); err != nil || !found || res.Value != "old-secret" {
		t.Errorf("an old credential: %+v found=%v err=%v", res, found, err)
	}
}

// Rotating a value without restating the list keeps the list. Sending a list
// replaces it. A stored list that was edited is not carried over.
func TestEngine_RotationKeepsTheRestrictionUnlessOneIsSent(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := netguard.WithPinnable(context.Background())
	id := Identity{TenantID: "t", Scope: "tenant", Name: "repo"}
	if _, err := e.PutInline(ctx, id, "v1", nil, urls("https://github.com/acme/app"), nil); err != nil {
		t.Fatal(err)
	}
	meta, err := e.PutInline(ctx, id, "v2", nil, nil, nil) // a rotation that says nothing about the list
	if err != nil {
		t.Fatal(err)
	}
	if meta.AllowedURLs == nil || !reflect.DeepEqual(*meta.AllowedURLs, []string{"https://github.com/acme/app"}) {
		t.Fatalf("the rotation dropped the restriction: %v", meta.AllowedURLs)
	}
	if _, _, err := e.Resolve(ctx, "t", "", "", "repo"); !errors.Is(err, ErrRestricted) {
		t.Errorf("after the rotation the credential resolves with no destination: %v", err)
	}
	if res, _, err := e.ResolveFor(ctx, "t", "", "", "repo", "https://github.com/acme/app"); err != nil || res.Value != "v2" {
		t.Errorf("the rotated value: %q err=%v", res.Value, err)
	}
	// A list that is sent replaces the old one.
	if meta, err := e.PutInline(ctx, id, "v3", nil, urls("https://gitea.example/a/b"), nil); err != nil || !reflect.DeepEqual(*meta.AllowedURLs, []string{"https://gitea.example/a/b"}) {
		t.Errorf("a new list: %v err=%v", meta.AllowedURLs, err)
	}
	// An edited stored list is refused, not sealed into the new value.
	row, _ := e.store.CredentialDefGet(ctx, "t", "tenant", "", "repo")
	var def map[string]json.RawMessage
	_ = json.Unmarshal(row.Definition, &def)
	def["allowed_urls"] = json.RawMessage(`["https://evil.example"]`)
	row.Definition, _ = json.Marshal(def)
	if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, id, "v4", nil, nil, nil); err == nil {
		t.Error("a rotation carried an edited list into the new value")
	}
}

// A user name is stored with the value, shown with the metadata and handed
// out with a resolution. A rotation that says nothing about it keeps it; an
// empty one removes it. It is not part of what the value is sealed with, so
// a row whose user name was changed behind the engine still opens.
func TestEngine_AUserNameIsStoredKeptAndCleared(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := Identity{TenantID: "t", Scope: "tenant", Name: "repo"}
	bob, empty := "bob", ""

	meta, err := e.PutInline(ctx, id, "v1", nil, nil, &bob)
	if err != nil || meta.Username != "bob" {
		t.Fatalf("create: meta.Username=%q err=%v", meta.Username, err)
	}
	if res, _, err := e.Resolve(ctx, "t", "", "", "repo"); err != nil || res.Username != "bob" || res.Value != "v1" {
		t.Fatalf("resolve: %+v err=%v", res, err)
	}
	if meta, err = e.PutInline(ctx, id, "v2", nil, nil, nil); err != nil || meta.Username != "bob" {
		t.Errorf("a rotation dropped the user name: %q err=%v", meta.Username, err)
	}
	if meta, err = e.PutInline(ctx, id, "v3", nil, nil, &empty); err != nil || meta.Username != "" {
		t.Errorf("an empty user name did not remove it: %q err=%v", meta.Username, err)
	}

	// Not sealed with the value: an edited user name still opens.
	if _, err := e.PutInline(ctx, id, "v4", nil, nil, &bob); err != nil {
		t.Fatal(err)
	}
	row, err := e.store.CredentialDefGet(ctx, "t", "tenant", "", "repo")
	if err != nil {
		t.Fatal(err)
	}
	var def map[string]any
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	def["username"] = "mallory"
	row.Definition, _ = json.Marshal(def)
	if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if res, _, err := e.Resolve(ctx, "t", "", "", "repo"); err != nil || res.Value != "v4" || res.Username != "mallory" {
		t.Errorf("an edited user name: %+v err=%v", res, err)
	}
}

func TestValidUsername(t *testing.T) {
	for _, ok := range []string{"", "bob", "oauth2", "x-access-token", "a.b@example.com"} {
		if err := ValidUsername(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a:b", "a\nb", "a\rb", "a\x00b", "a\x7fb", strings.Repeat("a", 257)} {
		if err := ValidUsername(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A restricted credential handed out for a URL pins the request it goes into,
// so the request's client follows no redirect away from that URL. A context
// that cannot be pinned gets nothing: a consumer that forgot to make its
// request pinnable would otherwise let a redirect carry the value elsewhere.
// An unrestricted credential pins nothing.
func TestEngine_ARestrictedCredentialPinsItsRequest(t *testing.T) {
	e := newEngine(t, testKey(1))
	setup := context.Background()
	if _, err := e.PutInline(setup, Identity{TenantID: "t", Scope: "tenant", Name: "repo"}, "ghs_secret", nil, urls("https://github.com/acme/app"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(setup, Identity{TenantID: "t", Scope: "tenant", Name: "plain"}, "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	if res, _, err := e.ResolveFor(context.Background(), "t", "", "", "repo", "https://github.com/acme/app"); !errors.Is(err, ErrNotPinnable) || res.Value != "" {
		t.Errorf("a context that cannot be pinned: value=%q err=%v, want ErrNotPinnable", res.Value, err)
	}

	pinnable := netguard.WithPinnable(context.Background())
	if res, _, err := e.ResolveFor(pinnable, "t", "", "", "plain", "https://x.example"); err != nil || res.Value != "v" {
		t.Fatalf("an unrestricted credential: %q %v", res.Value, err)
	}
	if netguard.Pinned(pinnable) {
		t.Error("an unrestricted credential pinned the request")
	}
	if res, _, err := e.ResolveFor(pinnable, "t", "", "", "repo", "https://github.com/acme/app"); err != nil || res.Value != "ghs_secret" {
		t.Fatalf("a restricted credential for a covered URL: %q %v", res.Value, err)
	}
	if !netguard.Pinned(pinnable) {
		t.Error("a restricted credential did not pin the request")
	}
}
