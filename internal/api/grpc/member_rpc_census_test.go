package grpc

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// memberRPCs classifies every RPC whose request names a run, a session, a
// user, a walk or a data scope, by the gate that holds a member (isolated or
// not) to its own conversations there. The census derives the list from the
// service descriptor, so an RPC that gains such a field fails it until it is
// classified here — and confined.
var memberRPCs = map[string]string{
	"Run":                 "applyPrincipal: the run's user is the caller's subject; a session_id continuation needs sessionOwnershipOK",
	"Continue":            "sessionOwnershipOK in RunOnce",
	"CompactRun":          "connector CompactRun: runOwnershipOK",
	"DeleteRunTree":       "connector DeleteRunTree: runOwnershipOK / sessionOwnershipOK",
	"CreateConfiguredRun": "applyPrincipal; a session_id needs sessionOwnershipOK",
	"UpdateConfiguredRun": "configured-run gate: sessionOwnershipOK",
	"StartConfiguredRun":  "configured-run gate: sessionOwnershipOK",
	"DeleteConfiguredRun": "configured-run gate: sessionOwnershipOK",
	"ReplaySession":       "sessionOwnershipOK on the source session",
	"RunInput":            "runForSteer: liveRunOwnershipOK",
	"RetuneRun":           "runForSteer: liveRunOwnershipOK",
	"CancelTurn":          "liveRunOwnershipOK / runOwnershipOK",
	"ReviewRun":           "review gate: runOwnershipOK",
	"ResolveInterrupt":    "callerOwnsRun",
	"StreamRun":           "connector StreamRunEvents: runOwnershipOK",
	"GetTranscript":       "OwnedRowVisible on the session",
	"GetAgent":            "OwnedRowVisible on the run",
	"GetRun":              "OwnedRowVisible on the run",
	"CancelAgent":         "connector CancelRun: runOwnershipOK",
	"ListUserAgents":      "OwnRowsOnly: another user's id lists nothing",
	"ListWalkRuns":        "OwnedRowVisible on the walk and each run",
	"ListChildRuns":       "OwnedRowVisible on the parent and each run",
	"StreamUserRunStates": "OwnRowsOnly: another user's id is NotFound",
	"PublishChannel":      "principalMayUseChannelScope: a user scope is the caller's own subject",
	"SubscribeChannel":    "principalMayUseChannelScope",
	"PeekChannel":         "principalMayUseChannelScope",
	"AckChannel":          "principalMayUseChannelScope",
	"AwaitChannels":       "principalMayUseChannelScope",
	"BroadcastChannels":   "principalMayUseChannelScope",
	"PeekTeamChannel":     "teamChannelKeyspace: a user-scoped team channel is the caller's own subject",
	"ReleaseTeamChannel":  "teamChannelKeyspace",
	"TokenLimit":          "budgets, not conversations: limits_admin confines a scoped caller to its tenant",
	"ListReviewQueue":     "statement review: own user scope for a member (the review plane's own gate)",
	"RuleStatements":      "document statements, not conversations: a member reviews by its review grant",
	"RevertRuling":        "document statements, not conversations: a member reviews by its review grant",
	"ListRulings":         "document statements, not conversations: a member reviews by its review grant",
	"ApplyReviewPolicy":   "document statements, not conversations: a member reviews by its review grant",
}

// The request fields that name whose conversation or data a call reaches.
var memberAddressingFields = map[protoreflect.Name]bool{
	"run_id": true, "agent_id": true, "session_id": true, "user_id": true,
	"scope_id": true, "walk_id": true, "parent_run_id": true, "source_session_id": true,
}

func TestMemberRPCCensus_EveryRunAddressingRPCIsClassified(t *testing.T) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName("loomcycle.v1.Loomcycle")
	if err != nil {
		t.Fatal(err)
	}
	sd := d.(protoreflect.ServiceDescriptor)
	found := map[string]bool{}
	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		fields := m.Input().Fields()
		for j := 0; j < fields.Len(); j++ {
			if memberAddressingFields[fields.Get(j).Name()] {
				found[string(m.Name())] = true
				break
			}
		}
	}
	if len(found) < 20 {
		t.Fatalf("found only %d addressing RPCs: the descriptor this census reads has changed", len(found))
	}
	for name := range found {
		if _, ok := memberRPCs[name]; !ok {
			t.Errorf("RPC %s names a run, session, user or scope but is not in memberRPCs: classify it, and confine a member to its own there", name)
		}
	}
	for name := range memberRPCs {
		if !found[name] {
			t.Errorf("memberRPCs lists %s, which no longer names a run, session, user or scope", name)
		}
	}
}
