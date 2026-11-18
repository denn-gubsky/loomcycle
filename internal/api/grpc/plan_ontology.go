// plan_ontology.go — the gRPC twin of POST /v1/_ontology/plan (RFC GQ P1.2).
// The connector holds the rules — who may plan, in which tenant, and the plan
// itself — so this transport only bounds the request, maps the shapes and the
// refusals.
package grpc

import (
	"context"
	"errors"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// PlanOntology reports what a set of ontology changes would do, writing nothing.
func (s *Server) PlanOntology(ctx context.Context, req *loomcyclepb.PlanOntologyRequest) (*loomcyclepb.PlanOntologyResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "PlanOntology requires a connector; this Server was constructed without one")
	}
	// HTTP's body bound, so this transport accepts no larger plan than HTTP does.
	if n := proto.Size(req); n > connector.OntologyPlanMaxRequestBytes {
		return nil, statusWithReason(codes.InvalidArgument, "invalid_body",
			"the plan request is "+strconv.Itoa(n)+" bytes; at most "+strconv.Itoa(connector.OntologyPlanMaxRequestBytes))
	}
	changes := make([]connector.OntologyChange, 0, len(req.GetChanges()))
	for _, c := range req.GetChanges() {
		changes = append(changes, connector.OntologyChange{
			Op: c.GetOp(), Entry: c.GetEntry(), Name: c.GetName(), Parent: c.GetParent(), Body: c.GetBody(),
		})
	}
	res, err := s.connector.PlanOntology(ctx, connector.PlanOntologyRequest{Tenant: req.GetTenant(), Changes: changes})
	if err != nil {
		return nil, planOntologyErrToStatus(err)
	}
	return planOntologyToProto(res), nil
}

func planOntologyToProto(res connector.PlanOntologyResult) *loomcyclepb.PlanOntologyResponse {
	out := &loomcyclepb.PlanOntologyResponse{
		Tenant:        res.Tenant,
		Fingerprint:   res.Fingerprint,
		Issues:        res.Issues,
		NewIssues:     res.NewIssues,
		ScopesCounted: int32(res.ScopesCounted),
		ScopesSkipped: int32(res.ScopesSkipped),
		Complete:      res.Complete,
	}
	for _, c := range res.Changes {
		pc := &loomcyclepb.OntologyPlannedChange{
			Op: c.Op, Entry: planEntryToProto(c.Entry), Risk: c.Risk, Notes: c.Notes,
		}
		if len(c.Counts) > 0 {
			pc.Counts = make(map[string]*loomcyclepb.OntologyPlanCount, len(c.Counts))
			for k, n := range c.Counts {
				pc.Counts[k] = planCountToProto(n)
			}
		}
		for _, e := range c.Subtree {
			pc.Subtree = append(pc.Subtree, planEntryToProto(e))
		}
		out.Changes = append(out.Changes, pc)
	}
	return out
}

func planEntryToProto(e connector.OntologyPlanEntry) *loomcyclepb.OntologyPlanEntry {
	return &loomcyclepb.OntologyPlanEntry{ChunkId: e.ChunkID, Name: e.Name, Section: e.Section, Kind: e.Kind}
}

func planCountToProto(n connector.OntologyPlanCount) *loomcyclepb.OntologyPlanCount {
	out := &loomcyclepb.OntologyPlanCount{
		N: int32(n.N), Effect: n.Effect, TenantScope: int32(n.TenantScope), OtherScopes: int32(n.OtherScopes), Sample: n.Sample,
	}
	if len(n.ByStatus) > 0 {
		out.ByStatus = make(map[string]int32, len(n.ByStatus))
		for k, v := range n.ByStatus {
			out.ByStatus[k] = int32(v)
		}
	}
	return out
}

// planOntologyErrToStatus maps a plan refusal onto the gRPC code matching HTTP's
// status (400/403/404/503), with the shared code as the ErrorInfo reason. An
// internal failure's text is not returned, as on HTTP: a store fault's text can
// carry a path or a DSN, and the connector has logged it.
func planOntologyErrToStatus(err error) error {
	reason := connector.OntologyPlanCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyPlanInvalid):
		return statusWithReason(codes.InvalidArgument, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanForbidden):
		return statusWithReason(codes.PermissionDenied, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanNotFound):
		return statusWithReason(codes.NotFound, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanUnavailable):
		return statusWithReason(codes.Unavailable, reason, err.Error())
	}
	return statusWithReason(codes.Internal, "plan_failed", "the plan could not be computed; see the server log")
}

// statusWithReason is a status carrying reason as its ErrorInfo, so a client
// branches on the reason rather than on the sentence.
func statusWithReason(code codes.Code, reason, msg string) error {
	st := status.New(code, msg)
	if withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}); derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}
