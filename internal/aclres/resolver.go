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
	principal auth.Principal // Groups + Effective filled
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
	class := Class(p)
	if p.Restrictions.RunPlaneOnly() {
		class += "+runplane"
	}
	k := key{p.TenantID, p.Subject, p.TokenDefID, class}
	version, err := r.version(ctx, p.TenantID)
	if err != nil {
		return p, err
	}
	r.mu.Lock()
	if e, ok := r.entries[k]; ok && e.version == version && r.now().Before(e.expiresAt) {
		r.mu.Unlock()
		out := p
		out.Groups, out.Effective = e.principal.Groups, e.principal.Effective
		return out, nil
	}
	r.mu.Unlock()

	resolved, err := r.compile(ctx, p, version)
	if err != nil {
		return p, err
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
	r.entries[k] = entry{principal: resolved, version: version, expiresAt: r.now().Add(ttl)}
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

func (r *Resolver) compile(ctx context.Context, p auth.Principal, version int64) (auth.Principal, error) {
	direct, err := r.st.GroupsOfSubject(ctx, p.TenantID, p.Subject)
	if err != nil {
		return p, fmt.Errorf("groups: %w", err)
	}
	var groups []string
	if len(direct) > 0 {
		graph, err := r.st.GroupGraph(ctx, p.TenantID)
		if err != nil {
			return p, fmt.Errorf("group graph: %w", err)
		}
		groups = authz.Closure(graph, direct)
	}
	rows, err := r.st.AclBindingsFor(ctx, p.TenantID, p.Subject, groups, p.TokenDefID)
	if err != nil {
		return p, fmt.Errorf("bindings: %w", err)
	}
	in := authz.Input{
		Subject: authz.Subject{TenantID: p.TenantID, ID: p.Subject, Groups: groups, TokenDefID: p.TokenDefID},
		Now:     r.now(),
		Version: version,
		ACLs:    map[string]authz.ACLVersion{},
	}
	// The class baseline, and the run-plane deny list for an RFC EF-confined
	// token, are bound tenant-wide and resolved from the built-in table.
	baselines := authz.Baselines()
	base := baselineName(Class(p))
	in.Bindings = append(in.Bindings, authz.Binding{ACL: base, SubjectKind: authz.SubjectTenant})
	in.ACLs[base] = authz.ACLVersion{ACL: baselines[base]}
	if p.Restrictions.RunPlaneOnly() {
		in.Bindings = append(in.Bindings, authz.Binding{ACL: authz.BaselineRunPlane, SubjectKind: authz.SubjectTenant})
		in.ACLs[authz.BaselineRunPlane] = authz.ACLVersion{ACL: baselines[authz.BaselineRunPlane]}
	}
	for _, row := range rows {
		in.Bindings = append(in.Bindings, authz.Binding{ID: row.BindingID, ACL: row.ACLName, SubjectKind: authz.SubjectKind(row.SubjectKind), SubjectID: row.SubjectID, ExpiresAt: row.ExpiresAt})
		if _, seen := in.ACLs[row.ACLName]; seen || authz.IsBuiltin(row.ACLName) {
			continue
		}
		def, err := r.st.AclDefGet(ctx, p.TenantID, row.ACLName)
		if err != nil {
			if isNotFound(err) {
				continue // Compile reports it as missing; it matches nothing
			}
			return p, fmt.Errorf("acl %s: %w", row.ACLName, err)
		}
		if def.Retired() {
			continue
		}
		rules, err := authz.ParseRules(def.Rules)
		if err != nil {
			// A stored list this build cannot read is refused as a whole:
			// the principal's set is unavailable (enforce refuses, compat
			// falls back), never partially applied.
			return p, fmt.Errorf("acl %s (version %d): %w", def.Name, def.Version, err)
		}
		in.ACLs[row.ACLName] = authz.ACLVersion{ACL: authz.ACL{Name: def.Name, Description: def.Description, Rules: rules}, Version: def.Version}
	}
	eff, err := authz.Compile(in)
	if err != nil {
		return p, err
	}
	out := p
	out.Groups, out.Effective = groups, eff
	return out, nil
}

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
