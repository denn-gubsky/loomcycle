// Package aclres resolves a principal's RFC EY authority: its transitively
// closed groups and its compiled ACL set, read from the store and cached per
// replica for as long as the tenant's acl_version has not moved.
package aclres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Store is the slice of store.AclStore the resolver reads.
type Store interface {
	GroupsOfSubject(ctx context.Context, tenantID, subject string) ([]string, error)
	GroupGraph(ctx context.Context, tenantID string) (map[string][]string, error)
	AclBindingsFor(ctx context.Context, tenantID, subject string, groups []string, tokenDefID string) ([]store.AclBindingRow, error)
	AclDefGet(ctx context.Context, tenantID, name string) (store.AclDefRow, error)
	AclVersion(ctx context.Context, tenantID string) (int64, error)
}

// InvalidateTopic is the backplane channel a write to the ACL tables
// publishes on; the payload is the tenant id.
const InvalidateTopic = "loomcycle.acl_changed"

// maxEntries bounds the per-replica cache (one entry per live principal).
const maxEntries = 16384

type key struct{ tenant, subject, token, class string }

type entry struct {
	eff       *authz.Effective // its Subject carries the closed groups
	version   int64
	expiresAt time.Time
}

type versionEntry struct {
	version   int64
	checkedAt time.Time
}

// Resolver is safe for concurrent use.
type Resolver struct {
	st   Store
	mode authz.Mode
	ttl  time.Duration
	now  func() time.Time

	mu       sync.Mutex
	entries  map[key]entry
	versions map[string]versionEntry
	// static reads the operator's config-declared lists, groups and
	// bindings (RFC EY §9) at compile time, so a config reload is seen by
	// the next compile. nil: none declared.
	static func() *authz.Static
}

// SetStatic wires the config-declared access control. It is read on every
// compile (never cached by the resolver), so a reload needs no call here;
// the caller drops the cached sets when the static source changes.
func (r *Resolver) SetStatic(src func() *authz.Static) {
	r.mu.Lock()
	r.static = src
	r.mu.Unlock()
}

func (r *Resolver) staticNow() *authz.Static {
	r.mu.Lock()
	src := r.static
	r.mu.Unlock()
	if src == nil {
		return nil
	}
	return src()
}

// New builds a resolver. ttl bounds how long a compiled set is trusted
// before the tenant's acl_version is re-read; a bus invalidation drops it
// sooner. ttl <= 0 re-reads the version on every request.
func New(st Store, mode authz.Mode, ttl time.Duration) *Resolver {
	return &Resolver{st: st, mode: mode, ttl: ttl, now: time.Now, entries: map[key]entry{}, versions: map[string]versionEntry{}}
}

// Mode is the rollout mode the resolver was built for.
func (r *Resolver) Mode() authz.Mode { return r.mode }

// Class names the baseline list a principal's token class gives it (RFC
// EY §11): admin (incl. the legacy secret), tenant operator, isolated user,
// or member. The run-plane deny list is added on top when RFC EF confines
// the token.
func Class(p auth.Principal) string {
	switch {
	case p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin):
		return "admin"
	case auth.HasScope(p.Scopes, auth.ScopeTenant):
		return "tenant"
	case auth.IsIsolated(p, true):
		return "isolated"
	}
	return "member"
}

// runPlaneSuffix marks a class key whose token RFC EF confines to the run
// plane: the run-plane deny list is bound on top of the class baseline.
const runPlaneSuffix = "+runplane"

// ClassKey is the class a principal's set is compiled and cached for: its
// baseline class (Class), with runPlaneSuffix when the token is confined to
// the run plane. It is what authz.Subject.Class records, so a set can be
// compiled again for the same subject without its principal (ResolveSubject).
func ClassKey(p auth.Principal) string {
	class := Class(p)
	if p.Restrictions.RunPlaneOnly() {
		class += runPlaneSuffix
	}
	return class
}

// splitClassKey reads a class key back: the baseline class and whether the
// run-plane list applies. ok is false for a key ClassKey never produces.
func splitClassKey(key string) (class string, runPlane, ok bool) {
	class, runPlane = strings.CutSuffix(key, runPlaneSuffix)
	switch class {
	case "admin", "tenant", "isolated", "member":
		return class, runPlane, true
	}
	return "", false, false
}

func baselineName(class string) string {
	switch class {
	case "admin":
		return authz.BaselineAdmin
	case "tenant":
		return authz.BaselineTenantOperator
	case "isolated":
		return authz.BaselineIsolated
	}
	return authz.BaselineMember
}

// Resolve fills p.Groups and p.Effective. A store fault returns p unchanged
// and the error; the caller decides by mode (compat and shadow continue on
// the scope answer, enforce refuses).
func (r *Resolver) Resolve(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	if r == nil || p.Subject == "" {
		return p, nil
	}
	eff, err := r.resolve(ctx, authz.Subject{TenantID: p.TenantID, ID: p.Subject, TokenDefID: p.TokenDefID, Class: ClassKey(p)})
	if err != nil {
		return p, err
	}
	out := p
	out.Groups, out.Effective = eff.Subject.Groups, eff
	return out, nil
}

// ResolveSubject compiles the CURRENT set of the subject a set was compiled
// for before — authz.Effective.Subject, its class included — with no
// principal to hand: a run resumed after a pause or a restart, whose
// caller's request is long gone, is narrowed by its caller's rules as they
// stand now (a binding revoked during the pause holds). subj.Groups is
// ignored and recomputed. A subject with no id or with a class ClassKey never
// produces is an error, never a guess at a baseline.
func (r *Resolver) ResolveSubject(ctx context.Context, subj authz.Subject) (*authz.Effective, error) {
	if r == nil {
		return nil, errors.New("no access-control resolver")
	}
	if subj.ID == "" {
		return nil, errors.New("subject has no id")
	}
	if _, _, ok := splitClassKey(subj.Class); !ok {
		return nil, fmt.Errorf("subject class %q is not one this resolver compiles", subj.Class)
	}
	return r.resolve(ctx, subj)
}

// resolve returns subj's compiled set from the cache while the tenant's
// acl_version has not moved, else compiles it.
func (r *Resolver) resolve(ctx context.Context, subj authz.Subject) (*authz.Effective, error) {
	k := key{subj.TenantID, subj.ID, subj.TokenDefID, subj.Class}
	version, err := r.version(ctx, subj.TenantID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if e, ok := r.entries[k]; ok && e.version == version && r.now().Before(e.expiresAt) {
		r.mu.Unlock()
		return e.eff, nil
	}
	r.mu.Unlock()

	resolved, err := r.compile(ctx, subj, version)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if len(r.entries) >= maxEntries {
		r.entries = map[key]entry{} // a bounded cache that resets is simpler than an LRU here
	}
	// Entries live for the version check interval or an hour, whichever is
	// longer: the version is what invalidates, the expiry only bounds memory.
	ttl := r.ttl
	if ttl < time.Hour {
		ttl = time.Hour
	}
	r.entries[k] = entry{eff: resolved, version: version, expiresAt: r.now().Add(ttl)}
	r.mu.Unlock()
	return resolved, nil
}

// version returns the tenant's acl_version, re-read from the store at most
// once per ttl.
func (r *Resolver) version(ctx context.Context, tenant string) (int64, error) {
	r.mu.Lock()
	v, ok := r.versions[tenant]
	r.mu.Unlock()
	if ok && r.ttl > 0 && r.now().Sub(v.checkedAt) < r.ttl {
		return v.version, nil
	}
	cur, err := r.st.AclVersion(ctx, tenant)
	if err != nil {
		return 0, fmt.Errorf("acl version: %w", err)
	}
	r.mu.Lock()
	r.versions[tenant] = versionEntry{version: cur, checkedAt: r.now()}
	r.mu.Unlock()
	return cur, nil
}

// Input returns what Compile would be given for a subject right now — its
// closed groups, applicable bindings, the lists they name and the class
// baseline — so a simulation can add a draft to it. The class is read as
// the subject's token class would be; subj.Groups is recomputed.
func (r *Resolver) Input(ctx context.Context, subj authz.Subject) (authz.Input, error) {
	subj = authz.Subject{TenantID: subj.TenantID, ID: subj.ID, TokenDefID: subj.TokenDefID, Class: "member"}
	version, err := r.version(ctx, subj.TenantID)
	if err != nil {
		return authz.Input{}, err
	}
	return r.input(ctx, subj, version)
}

func (r *Resolver) compile(ctx context.Context, subj authz.Subject, version int64) (*authz.Effective, error) {
	in, err := r.input(ctx, subj, version)
	if err != nil {
		return nil, err
	}
	return authz.Compile(in)
}

// input assembles what Compile is given for subj; subj.Class (a ClassKey)
// picks the baseline, and subj.Groups is recomputed.
func (r *Resolver) input(ctx context.Context, subj authz.Subject, version int64) (authz.Input, error) {
	class, runPlane, ok := splitClassKey(subj.Class)
	if !ok {
		return authz.Input{}, fmt.Errorf("subject class %q is not one this resolver compiles", subj.Class)
	}
	static := r.staticNow()
	direct, err := r.st.GroupsOfSubject(ctx, subj.TenantID, subj.ID)
	if err != nil {
		return authz.Input{}, fmt.Errorf("groups: %w", err)
	}
	staticDirect, staticGraph := static.GroupsOf(subj.TenantID, subj.ID)
	direct = append(direct, staticDirect...)
	var groups []string
	if len(direct) > 0 {
		graph, err := r.st.GroupGraph(ctx, subj.TenantID)
		if err != nil {
			return authz.Input{}, fmt.Errorf("group graph: %w", err)
		}
		groups = authz.Closure(authz.MergeGraph(graph, staticGraph), direct)
	}
	rows, err := r.st.AclBindingsFor(ctx, subj.TenantID, subj.ID, groups, subj.TokenDefID)
	if err != nil {
		return authz.Input{}, fmt.Errorf("bindings: %w", err)
	}
	in := authz.Input{
		Subject: authz.Subject{TenantID: subj.TenantID, ID: subj.ID, Groups: groups, TokenDefID: subj.TokenDefID, Class: subj.Class},
		Now:     r.now(),
		Version: version,
		ACLs:    map[string]authz.ACLVersion{},
	}
	// The class baseline, and the run-plane deny list for an RFC EF-confined
	// token, are bound tenant-wide and resolved from the built-in table.
	baselines := authz.Baselines()
	base := baselineName(class)
	in.Bindings = append(in.Bindings, authz.Binding{ACL: base, SubjectKind: authz.SubjectTenant})
	in.ACLs[base] = authz.ACLVersion{ACL: baselines[base]}
	if runPlane {
		in.Bindings = append(in.Bindings, authz.Binding{ACL: authz.BaselineRunPlane, SubjectKind: authz.SubjectTenant})
		in.ACLs[authz.BaselineRunPlane] = authz.ACLVersion{ACL: baselines[authz.BaselineRunPlane]}
	}
	for _, row := range rows {
		in.Bindings = append(in.Bindings, authz.Binding{ID: row.BindingID, ACL: row.ACLName, SubjectKind: authz.SubjectKind(row.SubjectKind), SubjectID: row.SubjectID, ExpiresAt: row.ExpiresAt})
	}
	// Config-declared bindings of the tenant apply beside the stored ones;
	// AppliesTo filters them to this principal at compile.
	in.Bindings = append(in.Bindings, static.BindingsIn(subj.TenantID)...)
	for _, b := range in.Bindings {
		if _, seen := in.ACLs[b.ACL]; seen || authz.IsBuiltin(b.ACL) || !b.AppliesTo(in.Subject) {
			continue
		}
		av, found, err := r.lookupACL(ctx, subj.TenantID, b.ACL, static)
		if err != nil {
			return authz.Input{}, err
		}
		if found {
			in.ACLs[b.ACL] = av
		}
	}
	return in, nil
}

// lookupACL resolves a list by name: the tenant's stored row, then the
// shared tenant's, then the config-declared one — the dynamic-over-static
// order of internal/lookup. A retired row is absent (the next layer may
// still answer); a missing name is reported by Compile and matches nothing.
func (r *Resolver) lookupACL(ctx context.Context, tenant, name string, static *authz.Static) (authz.ACLVersion, bool, error) {
	def, found, err := r.storedACL(ctx, tenant, name)
	if err != nil {
		return authz.ACLVersion{}, false, err
	}
	if found {
		rules, err := authz.ParseRules(def.Rules)
		if err != nil {
			// A stored list this build cannot read is refused as a whole:
			// the principal's set is unavailable (enforce refuses, compat
			// falls back), never partially applied.
			return authz.ACLVersion{}, false, fmt.Errorf("acl %s (version %d): %w", def.Name, def.Version, err)
		}
		return authz.ACLVersion{ACL: authz.ACL{Name: def.Name, Description: def.Description, Rules: rules}, Version: def.Version}, true, nil
	}
	if static != nil {
		if a, ok := static.ACLs[name]; ok {
			return authz.ACLVersion{ACL: a}, true, nil
		}
	}
	return authz.ACLVersion{}, false, nil
}

// storedACL is the live stored list a binding in tenant resolves to: the
// tenant's own row, else the shared tenant's. A retired row is absent.
func (r *Resolver) storedACL(ctx context.Context, tenant, name string) (store.AclDefRow, bool, error) {
	tenants := []string{tenant}
	if tenant != "" {
		tenants = append(tenants, "")
	}
	for _, t := range tenants {
		def, err := r.st.AclDefGet(ctx, t, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return store.AclDefRow{}, false, fmt.Errorf("acl %s: %w", name, err)
		}
		if def.Retired() {
			continue
		}
		return def, true, nil
	}
	return store.AclDefRow{}, false, nil
}

// ACL sources, as ACLSource reports them.
const (
	SourceStore  = "store"
	SourceConfig = "config"
)

// ACLSource reports where a binding to name in tenant finds its list, in
// the order the resolver looks: SourceStore for a live stored row (the
// tenant's, then the shared tenant's), SourceConfig for a config-declared
// list no stored row shadows, "" when no layer has it — a binding to such a
// name matches nothing. A built-in name is "": the resolver never applies a
// built-in through a binding.
func (r *Resolver) ACLSource(ctx context.Context, tenant, name string) (string, error) {
	if authz.IsBuiltin(name) {
		return "", nil
	}
	_, found, err := r.storedACL(ctx, tenant, name)
	if err != nil {
		return "", err
	}
	if found {
		return SourceStore, nil
	}
	if static := r.staticNow(); static != nil {
		if _, ok := static.ACLs[name]; ok {
			return SourceConfig, nil
		}
	}
	return "", nil
}

// Static returns the config-declared lists, groups and bindings the
// resolver merges under the store's rows (nil when none are declared).
func (r *Resolver) Static() *authz.Static { return r.staticNow() }

func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

// Invalidate drops every cached set and version of one tenant ("" drops
// all), so the next request re-reads the store.
func (r *Resolver) Invalidate(tenant string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenant == "" {
		r.entries = map[key]entry{}
		r.versions = map[string]versionEntry{}
		return
	}
	delete(r.versions, tenant)
	for k := range r.entries {
		if k.tenant == tenant {
			delete(r.entries, k)
		}
	}
}

// Publish tells every replica (this one included, which filters its own
// message, so Invalidate is called locally too) that tenant's ACL rows
// changed. Best-effort: a missed message is bounded by the TTL.
func Publish(ctx context.Context, bp coord.Backplane, r *Resolver, tenant string) {
	r.Invalidate(tenant)
	if bp == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"tenant": tenant})
	if err := bp.Publish(ctx, InvalidateTopic, payload); err != nil {
		slog.Warn("authz: invalidation publish failed", "err", err)
	}
}

// Subscribe drops the local cache for a tenant when a peer replica reports
// a change. Returns once subscribed; the goroutine exits on ctx.Done.
func (r *Resolver) Subscribe(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, InvalidateTopic)
	if err != nil {
		return err
	}
	go func() {
		for ev := range ch {
			var msg struct {
				Tenant string `json:"tenant"`
			}
			_ = json.Unmarshal(ev.Payload, &msg)
			r.Invalidate(msg.Tenant)
		}
	}()
	return nil
}
