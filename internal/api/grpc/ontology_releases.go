// ontology_releases.go — the gRPC twins of GET /v1/_ontology/releases and
// /v1/_ontology/releases/diff (RFC GQ P3.5). The connector holds who may read and
// in which tenant, and the reads themselves, so this transport only maps the
// shapes and the refusals.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// ListOntologyReleases lists the tenant ontology's releases, newest first.
func (s *Server) ListOntologyReleases(ctx context.Context, req *loomcyclepb.ListOntologyReleasesRequest) (*loomcyclepb.ListOntologyReleasesResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ListOntologyReleases requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.ListOntologyReleases(ctx, connector.ListOntologyReleasesRequest{Tenant: req.GetTenant()})
	if err != nil {
		return nil, ontologyReleaseErrToStatus(err)
	}
	out := &loomcyclepb.ListOntologyReleasesResponse{Tenant: res.Tenant}
	for _, r := range res.Releases {
		out.Releases = append(out.Releases, &loomcyclepb.OntologyRelease{
			Seq: r.Seq, ContentVersion: r.ContentVersion, Fingerprint: r.Fingerprint, Reason: r.Reason,
			Actor: r.Actor, CreatedAt: r.CreatedAt, Entries: int32(r.Entries),
		})
	}
	return out, nil
}

// DiffOntologyReleases compares two releases, or a release with the ontology in
// force (to 0).
func (s *Server) DiffOntologyReleases(ctx context.Context, req *loomcyclepb.DiffOntologyReleasesRequest) (*loomcyclepb.DiffOntologyReleasesResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DiffOntologyReleases requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.DiffOntologyReleases(ctx, connector.DiffOntologyReleasesRequest{
		Tenant: req.GetTenant(), From: req.GetFrom(), To: req.GetTo(),
	})
	if err != nil {
		return nil, ontologyReleaseErrToStatus(err)
	}
	side := func(d connector.OntologyDiffSide) *loomcyclepb.OntologyDiffSide {
		return &loomcyclepb.OntologyDiffSide{Seq: d.Seq, Current: d.Current, ContentVersion: d.ContentVersion}
	}
	out := &loomcyclepb.DiffOntologyReleasesResponse{
		Tenant: res.Tenant, From: side(res.From), To: side(res.To), Issues: res.Issues,
	}
	for _, c := range res.Changes {
		out.Changes = append(out.Changes, &loomcyclepb.OntologyEntryDiff{
			ChunkId: c.ChunkID, Kind: c.Kind, Change: c.Change, Changed: c.Changed,
			Before: entryStateToProto(c.Before), After: entryStateToProto(c.After),
		})
	}
	return out, nil
}

// entryStateToProto keeps an absent side absent: an added entry has no before.
func entryStateToProto(e *connector.OntologyEntryState) *loomcyclepb.OntologyEntryState {
	if e == nil {
		return nil
	}
	return &loomcyclepb.OntologyEntryState{
		Name: e.Name, Parent: e.Parent, ParentName: e.ParentName, Status: e.Status, Meaning: e.Meaning,
		DirectivesJson: e.Directives,
	}
}

// ontologyReleaseErrToStatus maps a release-read refusal onto the gRPC code
// matching HTTP's status (400/403/404/503), with the shared code as the ErrorInfo
// reason, as planOntologyErrToStatus does for the plan. An internal failure's text
// is not returned, as on HTTP: a store fault's text can carry a path or a DSN, and
// the connector has logged it.
func ontologyReleaseErrToStatus(err error) error {
	reason := connector.OntologyReleaseCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyReleaseInvalid):
		return statusWithReason(codes.InvalidArgument, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseForbidden):
		return statusWithReason(codes.PermissionDenied, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseNotFound):
		return statusWithReason(codes.NotFound, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseUnavailable):
		return statusWithReason(codes.Unavailable, reason, err.Error())
	}
	return statusWithReason(codes.Internal, "release_read_failed", "the ontology releases could not be read; see the server log")
}
