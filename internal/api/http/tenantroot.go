package http

// tenantroot.go — the deployment-context Document behind {{memory:tenant_info}}.
//
// The tenant twin of the user-root document: same lazy-provision-then-export
// shape, one scope up. What differs is who it is for. The user root is what the
// agent should know about the PERSON; this is what every agent in the tenant should
// know about the DEPLOYMENT — the service names, the vocabulary, the conventions
// that are equally true whichever user is asking.
//
// Two documents rather than two sections of one, because they have different blast
// radius: anything here is read by every user of the tenant, and an operator
// deciding what goes where should be making that call at the level of a document
// they can point at.
//
// The tenant grants are stamped SERVER-SIDE, exactly as renderOntology does. This
// is a server-side read of operator-authored config on the run's own tenant, not an
// agent reaching tenant memory, so an agent injecting the placeholder does not need
// to hold `tenant` in memory_scopes/sql_scopes — which matters, because the chat
// agents that consume it are deliberately user-scoped.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// renderTenantInfo composes the {{memory:tenant_info}} body: the tenant's
// deployment-context document as clean Markdown, provisioning it from the embedded
// template on first reference.
//
// Best-effort throughout. No store, no SQL Memory, no tenant, an unreadable
// document: the run continues with the section empty rather than failing. Context
// is help, and a run that dies for want of it is worse than one without it.
func (s *Server) renderTenantInfo(ctx context.Context, mi memInject) string {
	if s.store == nil || s.sqlMem == nil {
		return ""
	}
	s.ensureTenantRootDoc(ctx, mi)
	return s.readTenantRootMarkdown(ctx, mi)
}

// tenantRootCtx stamps the tenant identity plus the tenant-scope grants the read
// needs (see the file comment on why they are server-side).
func (s *Server) tenantRootCtx(ctx context.Context, mi memInject) context.Context {
	dctx := s.docToolCtx(ctx, mi)
	dctx = withTenantDocGrants(dctx)
	return dctx
}

// withTenantDocGrants grants the tenant-scope Memory and SQL Memory a
// server-side read or write of an operator-authored tenant Document needs (a
// Document's chunk bodies are in Memory and its structure in SQL Memory). The
// rest of ctx's policy is kept.
func withTenantDocGrants(ctx context.Context) context.Context {
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		p.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
}

// ensureTenantRootDoc provisions the document on first reference. Mirrors
// ensureUserRootDoc's race safety: an in-process singleflight collapses a
// concurrent burst, and a PG advisory lock admits one replica in a cluster.
//
// Keyed on the tenant alone — one document per tenant, unlike the per-principal
// user root.
func (s *Server) ensureTenantRootDoc(ctx context.Context, mi memInject) {
	if s.store == nil || s.sqlMem == nil {
		return
	}
	_, _, _ = s.userRootProvisionSF.Do("tenantroot\x00"+mi.Tenant, func() (any, error) {
		s.provisionTenantRootDoc(ctx, mi)
		return nil, nil
	})
}

func (s *Server) provisionTenantRootDoc(ctx context.Context, mi memInject) {
	if s.sessionLockPG != nil {
		// NUL-free lock id (pg text params reject 0x00).
		release, ok := s.sessionLockPG.TryLock(ctx, "memory:tenantroot:"+mi.Tenant)
		if !ok {
			return
		}
		defer release()
	}

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.tenantRootCtx(ctx, mi)

	// Provision ONLY on "absent". The probe asks the dirent store directly because a
	// Document get_document answers a store fault and a missing name with the same
	// error category, and a fault read as "absent" would import the template over the
	// operator's deployment context. An existing name is left alone, whatever it holds.
	cut := strings.LastIndex(meminject.TenantRootPath, "/") + 1
	_, err := s.store.DirentGet(dctx, mi.Tenant, "tenant", "", meminject.TenantRootPath[:cut], meminject.TenantRootPath[cut:])
	if err == nil {
		return // already provisioned, here or by another replica
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		slog.WarnContext(ctx, "tenant root: probe failed; not provisioning", "tenant", mi.Tenant, "err", err)
		return
	}

	// A reserved tenant name (RFC FY Part C), given through the one entry point that
	// may give it outside the operator plane: this runs inside a run's prompt render.
	_, _ = doc.ProvisionReservedDocument(dctx, meminject.TenantRootPath, meminject.TenantRootTemplate()) // best-effort; the next reference retries
}

// readTenantRootMarkdown exports the document to clean Markdown for injection.
func (s *Server) readTenantRootMarkdown(ctx context.Context, mi memInject) string {
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.tenantRootCtx(ctx, mi)
	req, _ := json.Marshal(map[string]any{
		"op": "export_md", "scope": "tenant", "path": meminject.TenantRootPath,
		"include_metadata": false,
	})
	res, _ := doc.Execute(dctx, req)
	if res.IsError {
		return ""
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Markdown)
}
