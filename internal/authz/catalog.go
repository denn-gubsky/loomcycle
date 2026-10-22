package authz

import (
	"fmt"
	"sort"
	"strings"
)

// CatalogVersion is stamped on every stored ACL. A rule whose type or verb a
// later catalog dropped is refused at read, never read as "no rule", and the
// doctor names it (RFC EY §4).
const CatalogVersion = 1

// TypeSpec describes one resource type of the closed catalog: the verbs a
// rule may name on it and the shape of a selector.
//
// A selector is `<type>:<name>`. The name's shape is one of:
//   - a plain `/`-segmented glob (Scopes empty): `agent:chat/*`;
//   - a scoped name (Scopes set, Sep set): `<scope><Sep><glob>`, where scope
//     is one of Scopes or `*`, and the glob may be omitted to mean every
//     instance of that scope: `memory:tenant`, `document:tenant:/kb/**`,
//     `def:agent/chat/*`;
//   - an enum (Scopes set, Sep zero): exactly one of Scopes or `*`:
//     `run:own`, `gateway:chat`.
type TypeSpec struct {
	Name   string
	Verbs  []string
	Scopes []string
	Sep    byte
}

// enumType is a type whose selector is one of a fixed set of values.
func enumType(name string, values []string, verbs ...string) TypeSpec {
	return TypeSpec{Name: name, Verbs: verbs, Scopes: values}
}

// scopedType is a type whose selector is `<scope><sep><glob>`.
func scopedType(name string, sep byte, scopes []string, verbs ...string) TypeSpec {
	return TypeSpec{Name: name, Verbs: verbs, Scopes: scopes, Sep: sep}
}

// globType is a type whose selector is a plain `/`-segmented glob.
func globType(name string, verbs ...string) TypeSpec {
	return TypeSpec{Name: name, Verbs: verbs}
}

var (
	memoryScopes     = []string{"user", "agent", "tenant", "global"}
	sqlScopes        = []string{"user", "agent", "tenant"}
	documentScopes   = []string{"agent", "user", "tenant"}
	historyScopes    = []string{"self", "user", "tenant", "global"}
	credentialScopes = []string{"tenant", "user", "agent"}
	cryptoScopes     = []string{"tenant", "user", "agent"}
	evaluationScopes = []string{"self", "siblings", "descendants", "any"}
	runScopes        = []string{"own", "tenant"}
	gatewayKinds     = []string{"chat", "embeddings", "decide"}
	principalKinds   = []string{"user", "group", "token"}
	runtimeAreas     = []string{"pause", "snapshot", "limits", "retention", "erasure", "usage", "routing", "events", "config", "tenants", "metrics"}
	// defFamilies are the definition families a rule may name under `def:`.
	// `acl` and `group` are the RFC EY families themselves, so authoring an
	// ACL is an ACL decision like any other.
	defFamilies = []string{"agent", "skill", "team", "mcpserver", "schedule", "webhook", "volume", "a2a", "a2aserver", "hook", "credential", "memorybackend", "documentsource", "acl", "group"}
)

// catalog is the closed set (RFC EY §3.3, owner decision D8: every primitive
// a principal can reach has a type here). Adding a type or a verb is a code
// change; TestCatalog_* pins the shape, and the surfaces' census tests
// (RFC EZ §2) hold that every pair is enforced somewhere.
var catalog = []TypeSpec{
	globType("agent", "run", "read", "spawn", "steer", "cancel", "review"),
	globType("volume", "read", "write", "provision", "mount"),
	globType("tool", "invoke"),
	globType("skill", "invoke", "read", "author"),
	globType("mcp", "invoke", "read"),
	globType("mcp-tool", "invoke"),
	globType("a2a", "invoke", "expose", "author"),
	globType("client", "invoke"),
	globType("localapi", "invoke"),
	globType("search", "use"),
	scopedType("memory", ':', memoryScopes, "read", "write", "recall", "browse"),
	scopedType("sqlmem", ':', sqlScopes, "read", "write"),
	scopedType("document", ':', documentScopes, "read", "write", "delete", "link", "browse"),
	enumType("history", historyScopes, "read"),
	globType("channel", "publish", "subscribe", "ack", "await", "broadcast", "manage"),
	scopedType("def", '/', defFamilies, "author", "retire", "read", "manage"),
	globType("team", "run", "read", "cancel", "author"),
	scopedType("credential", ':', credentialScopes, "use", "manage"),
	enumType("crypto", cryptoScopes, "use", "manage"),
	globType("model", "use"),
	globType("tier", "use"),
	enumType("gateway", gatewayKinds, "use"),
	enumType("evaluation", evaluationScopes, "submit", "read"),
	globType("interruption", "raise", "resolve"),
	enumType("run", runScopes, "read", "delete", "steer", "cancel", "compact", "retune", "review"),
	scopedType("principal", ':', principalKinds, "read", "manage", "mint"),
	enumType("runtime", runtimeAreas, "read", "manage"),
	globType("transport", "use"),
	// git is RFC FB's network authority (fetch / push per remote host and
	// path); the Git tool's local ops are `tool:Git`.
	globType("git", "fetch", "push"),
}

var catalogByName = func() map[string]TypeSpec {
	m := make(map[string]TypeSpec, len(catalog))
	for _, t := range catalog {
		if _, dup := m[t.Name]; dup {
			panic("authz: duplicate catalog type " + t.Name)
		}
		m[t.Name] = t
	}
	return m
}()

// Types returns every catalog type name, sorted.
func Types() []string {
	out := make([]string, 0, len(catalogByName))
	for name := range catalogByName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TypeSpecOf returns the spec of a catalog type.
func TypeSpecOf(name string) (TypeSpec, bool) {
	t, ok := catalogByName[name]
	return t, ok
}

// ValidVerb reports whether verb is one of typ's verbs.
func ValidVerb(typ, verb string) bool {
	t, ok := catalogByName[typ]
	if !ok {
		return false
	}
	for _, v := range t.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// validSelectorScope reports whether scope is one of t's scopes (or `*`).
func (t TypeSpec) validSelectorScope(scope string) bool {
	if scope == "*" {
		return true
	}
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// isEnum reports whether t's selector is an enum (scopes, no glob part).
func (t TypeSpec) isEnum() bool { return len(t.Scopes) > 0 && t.Sep == 0 }

// isScoped reports whether t's selector is `<scope><sep><glob>`.
func (t TypeSpec) isScoped() bool { return len(t.Scopes) > 0 && t.Sep != 0 }

// SplitSelector splits `type:name` into its two halves. The name may itself
// contain `:` (a scoped selector), so only the first `:` splits.
func SplitSelector(sel string) (typ, name string, err error) {
	i := strings.IndexByte(sel, ':')
	if i <= 0 {
		return "", "", fmt.Errorf("selector %q: want <type>:<name>", sel)
	}
	typ, name = sel[:i], sel[i+1:]
	if _, ok := catalogByName[typ]; !ok {
		return "", "", fmt.Errorf("selector %q: %q is not a catalog type", sel, typ)
	}
	return typ, name, nil
}
