package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// parityMock embeds the zero-returning mockConnector and overrides the two
// run-lifecycle methods under test so a test can record the inbound request +
// inject a result. The RPC round-trip is synchronous, so the recorded fields
// are written (server goroutine) before the client call returns (test reads).
type parityMock struct {
	mockConnector
	lastBatch     connector.BatchSpawnRequest
	batchResult   connector.BatchSpawnResult
	batchErr      error
	lastCompactID string
	compactResult connector.CompactResult
	compactErr    error
}

func (m *parityMock) SpawnRunBatch(_ context.Context, req connector.BatchSpawnRequest) (connector.BatchSpawnResult, error) {
	m.lastBatch = req
	return m.batchResult, m.batchErr
}

func (m *parityMock) CompactRun(_ context.Context, runID string) (connector.CompactResult, error) {
	m.lastCompactID = runID
	return m.compactResult, m.compactErr
}

// statusErr is a connector error that carries an HTTP status, like the http
// package's compactErr — so compactErrToStatus maps it to the matching code.
type statusErr struct {
	code int
	msg  string
}

func (e statusErr) Error() string   { return e.msg }
func (e statusErr) HTTPStatus() int { return e.code }

func TestSamplingFromProto_PreservesPresence(t *testing.T) {
	if samplingFromProto(nil) != nil {
		t.Fatal("nil proto must map to nil config")
	}
	// temperature explicitly 0.0 (deterministic, NOT unset); top_k set; others unset.
	got := samplingFromProto(&loomcyclepb.Sampling{
		Temperature: proto.Float64(0),
		TopK:        proto.Int32(40),
		Stop:        []string{"END"},
	})
	if got == nil || got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("temperature 0.0 lost presence: %+v", got)
	}
	if got.TopK == nil || *got.TopK != 40 {
		t.Errorf("top_k = %v, want 40", got.TopK)
	}
	if got.TopP != nil {
		t.Errorf("top_p should stay unset (nil), got %v", *got.TopP)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "END" {
		t.Errorf("stop = %v, want [END]", got.Stop)
	}
}

func TestCompactionFromProto_PreservesPresence(t *testing.T) {
	if compactionFromProto(nil) != nil {
		t.Fatal("nil proto must map to nil config")
	}
	got := compactionFromProto(&loomcyclepb.Compaction{
		Enabled:          proto.Bool(false), // explicit off, not unset
		KeepLastN:        proto.Int32(6),
		AutocompactAtPct: proto.Int32(75),
	})
	if got == nil || got.Enabled == nil || *got.Enabled {
		t.Errorf("enabled=false lost presence: %+v", got)
	}
	if got.KeepLastN == nil || *got.KeepLastN != 6 {
		t.Errorf("keep_last_n = %v, want 6", got.KeepLastN)
	}
	if got.AutoCompactAtPct == nil || *got.AutoCompactAtPct != 75 {
		t.Errorf("autocompact_at_pct = %v, want 75", got.AutoCompactAtPct)
	}
	if got.TargetPercentage != nil {
		t.Errorf("target_percentage should stay unset, got %v", *got.TargetPercentage)
	}
}

func TestSpawnRunBatch_DispatchesAndMaps(t *testing.T) {
	mc := &parityMock{batchResult: connector.BatchSpawnResult{
		Spawned: 2,
		Results: []connector.SpawnRunResult{
			{AgentID: "a0", RunID: "r0", Status: "completed", FinalText: "zero"},
			{AgentID: "a1", RunID: "r1", Status: "failed", Error: "boom"},
		},
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.SpawnRunBatch(context.Background(), &loomcyclepb.BatchSpawnRequest{
		Mode: "join",
		Spawns: []*loomcyclepb.RunRequest{
			{Agent: "rev", Compaction: &loomcyclepb.Compaction{Enabled: proto.Bool(true), KeepLastN: proto.Int32(8)}},
			{Agent: "rev", Sampling: &loomcyclepb.Sampling{Temperature: proto.Float64(0.2)}},
		},
	})
	if err != nil {
		t.Fatalf("SpawnRunBatch: %v", err)
	}
	// Request mapped to the connector.
	if len(mc.lastBatch.Spawns) != 2 || mc.lastBatch.Mode != "join" {
		t.Fatalf("connector saw %+v", mc.lastBatch)
	}
	if c := mc.lastBatch.Spawns[0].Compaction; c == nil || c.Enabled == nil || !*c.Enabled || c.KeepLastN == nil || *c.KeepLastN != 8 {
		t.Errorf("spawn[0] compaction not mapped: %+v", mc.lastBatch.Spawns[0].Compaction)
	}
	if s := mc.lastBatch.Spawns[1].Sampling; s == nil || s.Temperature == nil || *s.Temperature != 0.2 {
		t.Errorf("spawn[1] sampling not mapped: %+v", mc.lastBatch.Spawns[1].Sampling)
	}
	// Result mapped back to proto (index-aligned; per-child error in-envelope).
	if resp.GetSpawned() != 2 || len(resp.GetResults()) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.GetResults()[0].GetRunId() != "r0" || resp.GetResults()[1].GetError() != "boom" {
		t.Errorf("results not mapped: %+v", resp.GetResults())
	}
}

func TestSpawnRunBatch_MalformedIsInvalidArgument(t *testing.T) {
	// The connector returns a plain error for a malformed batch (over-cap /
	// unsupported mode); the handler maps any such error to InvalidArgument.
	mc := &parityMock{batchErr: errOverCap{}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.SpawnRunBatch(context.Background(), &loomcyclepb.BatchSpawnRequest{
		Spawns: []*loomcyclepb.RunRequest{{Agent: "x"}},
		Mode:   "detach",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", status.Code(err))
	}
}

type errOverCap struct{}

func (errOverCap) Error() string { return "spawn_runs: mode \"detach\" not supported" }

func TestCompactRun_DispatchesAndMaps(t *testing.T) {
	mc := &parityMock{compactResult: connector.CompactResult{
		RunID: "r_x", Compacted: true, BeforeTokens: 900, AfterTokens: 120, Applied: "live",
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.CompactRun(context.Background(), &loomcyclepb.CompactRunRequest{RunId: "r_x"})
	if err != nil {
		t.Fatalf("CompactRun: %v", err)
	}
	if mc.lastCompactID != "r_x" {
		t.Errorf("connector saw run_id=%q, want r_x", mc.lastCompactID)
	}
	if !resp.GetCompacted() || resp.GetApplied() != "live" || resp.GetAfterTokens() != 120 {
		t.Errorf("resp = %+v, want compacted live after=120", resp)
	}
}

func TestCompactRun_MidTurnIsFailedPrecondition(t *testing.T) {
	// A 409-status connector error (mid-turn run_busy) maps to FailedPrecondition.
	mc := &parityMock{compactErr: statusErr{code: 409, msg: "the agent is mid-turn"}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.CompactRun(context.Background(), &loomcyclepb.CompactRunRequest{RunId: "r_x"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", status.Code(err))
	}
}
