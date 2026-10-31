package http

// erasure_subjects.go — the tenant subjects an erasure finds bound to the erased
// principal (a subject root's `fields.principal`). The erasure service owns what is
// done (report, unbind, announce, never delete); this file only reaches the document
// store and the channel bus, which are the server's.

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// wireErasureSubjects gives svc the bound-subject plane. Without SQL Memory there are
// no documents, so the plane is left unwired and reported as not examined.
//
// Every call acts through the operator-plane Document (ontologyDocument: it carries the
// bus, so the announcement is published) in the erasure's tenant. The caller's
// principal stays on the context, so its access-control rules decide both which
// subjects are named and whether `erased` may be published. The run identity carries
// no user: the event's publisher must never be the erased principal.
func (s *Server) wireErasureSubjects(svc *erasure.Service) {
	if s.sqlMem == nil {
		return
	}
	dctx := func(ctx context.Context, tenant string) context.Context {
		return s.ontologyDocCtx(ctx, memInject{Tenant: tenant})
	}
	svc.BoundSubjects = func(ctx context.Context, tenant, subject string) ([]erasure.BoundSubject, error) {
		bound, err := s.ontologyDocument().TenantSubjectsBoundTo(dctx(ctx, tenant), subject)
		return toErasureSubjects(bound), err
	}
	svc.ClearBoundSubjects = func(ctx context.Context, tenant, subject string) ([]erasure.BoundSubject, error) {
		cleared, err := s.ontologyDocument().ClearTenantSubjectBindings(dctx(ctx, tenant), subject)
		return toErasureSubjects(cleared), err
	}
	svc.AnnounceErased = func(ctx context.Context, tenant string, subjects []erasure.BoundSubject) string {
		out := make([]builtin.SubjectBinding, 0, len(subjects))
		for _, b := range subjects {
			out = append(out, builtin.SubjectBinding{DocumentID: b.DocumentID, RootChunkID: b.RootChunkID, NaturalKey: b.NaturalKey})
		}
		return s.ontologyDocument().AnnounceTenantSubjectsErased(dctx(ctx, tenant), out)
	}
}

func toErasureSubjects(in []builtin.SubjectBinding) []erasure.BoundSubject {
	out := make([]erasure.BoundSubject, 0, len(in))
	for _, b := range in {
		out = append(out, erasure.BoundSubject{
			DocumentID: b.DocumentID, NaturalKey: b.NaturalKey, Path: b.Path,
			RootChunkID: b.RootChunkID, Readable: b.Readable,
		})
	}
	return out
}
