package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// v0.8.18 gRPC surface for Pause/Resume/Snapshot. Every handler is a
// thin translation between the proto wire shape and the Connector
// interface — same pattern as CancelAgent. Typed errors from the
// Connector (see internal/connector/errors.go) map to gRPC status
// codes via translatePauseSnapshotError.

// PauseRuntime — mirrors POST /v1/_pause.
func (s *Server) PauseRuntime(ctx context.Context, req *loomcyclepb.PauseRuntimeRequest) (*loomcyclepb.PauseRuntimeResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.PauseRuntime(ctx, int(req.GetTimeoutMs()))
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.PauseRuntimeResponse{
		Status:              res.Status,
		DurationMs:          res.DurationMS,
		ForceCancelledCount: int32(res.ForceCancelledCount),
		PausedRunsCount:     int32(res.PausedRunsCount),
		Warnings:            res.Warnings,
	}, nil
}

// ResumeRuntime — mirrors POST /v1/_resume.
func (s *Server) ResumeRuntime(ctx context.Context, _ *loomcyclepb.ResumeRuntimeRequest) (*loomcyclepb.ResumeRuntimeResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.ResumeRuntime(ctx)
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.ResumeRuntimeResponse{
		Status:          res.Status,
		ResumedRunCount: int32(res.ResumedRunCount),
		Warnings:        res.Warnings,
	}, nil
}

// GetRuntimeState — mirrors GET /v1/_state.
func (s *Server) GetRuntimeState(ctx context.Context, _ *loomcyclepb.GetRuntimeStateRequest) (*loomcyclepb.RuntimeStateResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.GetRuntimeState(ctx)
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	out := &loomcyclepb.RuntimeStateResponse{
		Status:         res.Status,
		PausedRunCount: int32(res.PausedRunCount),
		SnapshotsCount: int32(res.SnapshotsCount),
	}
	if res.PausedAt != nil {
		out.PausedAt = timestamppb.New(*res.PausedAt)
	}
	return out, nil
}

// CreateSnapshot — mirrors POST /v1/_snapshots.
func (s *Server) CreateSnapshot(ctx context.Context, req *loomcyclepb.CreateSnapshotRequest) (*loomcyclepb.SnapshotDescriptor, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	in := connector.CreateSnapshotRequest{
		IncludeHistory: req.GetIncludeHistory(),
		Description:    req.GetDescription(),
		MaxBytes:       req.GetMaxBytes(),
	}
	if req.GetSinceTs() != nil {
		t := req.GetSinceTs().AsTime()
		in.SinceTS = &t
	}
	desc, err := s.connector.CreateSnapshot(ctx, in)
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return descriptorToProto(desc), nil
}

// ListSnapshots — mirrors GET /v1/_snapshots.
func (s *Server) ListSnapshots(ctx context.Context, _ *loomcyclepb.ListSnapshotsRequest) (*loomcyclepb.ListSnapshotsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	list, err := s.connector.ListSnapshots(ctx)
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	out := &loomcyclepb.ListSnapshotsResponse{
		Snapshots: make([]*loomcyclepb.SnapshotDescriptor, 0, len(list)),
	}
	for _, d := range list {
		out.Snapshots = append(out.Snapshots, descriptorToProto(d))
	}
	return out, nil
}

// GetSnapshot — mirrors GET /v1/_snapshots/{id}. Returns the full
// envelope including JSON content (v0.8.18+).
func (s *Server) GetSnapshot(ctx context.Context, req *loomcyclepb.GetSnapshotRequest) (*loomcyclepb.SnapshotEnvelope, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	env, err := s.connector.GetSnapshot(ctx, req.GetSnapshotId())
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.SnapshotEnvelope{
		SnapshotId:    env.SnapshotID,
		CreatedAt:     timestamppb.New(env.CreatedAt),
		Description:   env.Description,
		FormatVersion: env.FormatVersion,
		SizeBytes:     env.SizeBytes,
		JsonContent:   env.JSONContent,
	}, nil
}

// ExportSnapshot — mirrors GET /v1/_snapshots/{id}/export. Returns
// canonical envelope bytes in raw_json.
func (s *Server) ExportSnapshot(ctx context.Context, req *loomcyclepb.ExportSnapshotRequest) (*loomcyclepb.ExportSnapshotResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	out, err := s.connector.ExportSnapshot(ctx, req.GetSnapshotId())
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.ExportSnapshotResponse{
		SnapshotId: out.SnapshotID,
		FilePath:   out.FilePath,
		Checksum:   out.Checksum,
		SizeBytes:  out.SizeBytes,
		RawJson:    out.RawJSON,
	}, nil
}

// RestoreSnapshot — mirrors POST /v1/_snapshots/{id}/restore.
func (s *Server) RestoreSnapshot(ctx context.Context, req *loomcyclepb.RestoreSnapshotRequest) (*loomcyclepb.RestoreSnapshotResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	in := connector.RestoreSnapshotRequest{
		SnapshotID:     req.GetSnapshotId(),
		RawJSON:        req.GetRawJson(),
		IncludeHistory: req.GetIncludeHistory(),
	}
	res, err := s.connector.RestoreSnapshot(ctx, in)
	if err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.RestoreSnapshotResponse{
		AgentDefsRestored:          int32(res.AgentDefsRestored),
		AgentDefActiveRestored:     int32(res.AgentDefActiveRestored),
		MemoryRestored:             int32(res.MemoryRestored),
		ChannelMessagesRestored:    int32(res.ChannelMessagesRestored),
		ChannelCursorsRestored:     int32(res.ChannelCursorsRestored),
		EvaluationsRestored:        int32(res.EvaluationsRestored),
		PausedRunsRestored:         int32(res.PausedRunsRestored),
		SynthesizedSessions:        int32(res.SynthesizedSessions),
		TranscriptEventsRestored:   int32(res.TranscriptEventsRestored),
		InteractionHistoryRestored: int32(res.InteractionHistoryRestored),
		Warnings:                   res.Warnings,
		FormatMigrations:           res.FormatMigrations,
	}, nil
}

// DeleteSnapshot — mirrors DELETE /v1/_snapshots/{id}. Idempotent.
func (s *Server) DeleteSnapshot(ctx context.Context, req *loomcyclepb.DeleteSnapshotRequest) (*loomcyclepb.DeleteSnapshotResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	if err := s.connector.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil {
		return nil, translatePauseSnapshotError(err)
	}
	return &loomcyclepb.DeleteSnapshotResponse{
		Deleted:    true,
		SnapshotId: req.GetSnapshotId(),
	}, nil
}

// descriptorToProto converts a connector.SnapshotDescriptor into the
// proto wire shape. Reused by CreateSnapshot + ListSnapshots.
func descriptorToProto(d connector.SnapshotDescriptor) *loomcyclepb.SnapshotDescriptor {
	out := &loomcyclepb.SnapshotDescriptor{
		SnapshotId:      d.SnapshotID,
		CreatedAt:       timestamppb.New(d.CreatedAt),
		SizeBytes:       d.SizeBytes,
		IncludesHistory: d.IncludesHistory,
		Description:     d.Description,
		FormatVersion:   d.FormatVersion,
	}
	if d.SinceTS != nil {
		out.SinceTs = timestamppb.New(*d.SinceTS)
	}
	return out
}

// translatePauseSnapshotError maps Connector typed errors to gRPC
// status codes. See connector/errors.go for the canonical taxonomy.
// Unknown errors map to Internal with the underlying message.
func translatePauseSnapshotError(err error) error {
	switch {
	case errors.Is(err, connector.ErrPauseNotConfigured):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, connector.ErrAlreadyPausing):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrNotPaused):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrSnapshotNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSnapshotTooLarge):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, connector.ErrSnapshotVersionTooNew):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrSnapshotVersionUnknown):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}
