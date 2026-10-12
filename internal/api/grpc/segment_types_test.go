package grpc

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// A content block whose type is not one the loop knows used to be read as
// trusted text, so a caller that misspelled "untrusted-block" sent its
// evidence as instructions and was told nothing. It is now a client error on
// every surface that takes a caller's segments — gRPC and HTTP side by side,
// against the one real server. A block with no type is still trusted text.

// typedSegment is one user segment holding one block of the given type.
func typedSegment(blockType string) []*loomcyclepb.PromptSegment {
	return []*loomcyclepb.PromptSegment{{Role: "user", Content: []*loomcyclepb.PromptContentBlock{{Type: blockType, Text: "evidence"}}}}
}

func typedSegmentJSON(blockType string) string {
	b, _ := json.Marshal(map[string]string{"type": blockType, "text": "evidence"})
	return `"segments":[{"role":"user","content":[` + string(b) + `]}]`
}

// typeOutcomeGRPC is a clean success, or InvalidArgument naming the type.
// Any other error fails the test: a refusal surfacing as Internal is the
// wrong shape even though it refuses.
func typeOutcomeGRPC(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return pcAccepted
	}
	if status.Code(err) == codes.InvalidArgument && strings.Contains(err.Error(), "unknown type") {
		return pcRefused
	}
	t.Fatalf("error = %v, want nil or InvalidArgument naming the unknown type", err)
	return ""
}

func typeOutcomeHTTP(t *testing.T, code int, body string) string {
	t.Helper()
	switch {
	case code == nethttp.StatusOK, code == nethttp.StatusCreated:
		return pcAccepted
	case code == nethttp.StatusBadRequest && strings.Contains(body, "unknown type"):
		return pcRefused
	}
	t.Fatalf("status = %d %s, want 200/201 or 400 naming the unknown type", code, body)
	return ""
}

func TestSegments_UnknownBlockTypeRefusedOnEveryTransport(t *testing.T) {
	ctx := context.Background()
	seedSession := func(t *testing.T, h *pcHarness) string {
		t.Helper()
		seed, err := h.client.Run(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: userSegment("seed")})
		if err != nil {
			t.Fatal(err)
		}
		sessionID, err := drainRunStream(seed)
		if err != nil || sessionID == "" {
			t.Fatalf("seed run: session %q, %v", sessionID, err)
		}
		return sessionID
	}
	cases := []struct {
		name string
		call func(t *testing.T, h *pcHarness, blockType string) string
	}{
		{"gRPC Run", func(t *testing.T, h *pcHarness, bt string) string {
			stream, err := h.client.Run(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: typedSegment(bt)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = drainRunStream(stream)
			return typeOutcomeGRPC(t, err)
		}},
		{"gRPC Continue", func(t *testing.T, h *pcHarness, bt string) string {
			stream, err := h.client.Continue(ctx, &loomcyclepb.ContinueRequest{SessionId: seedSession(t, h), Segments: typedSegment(bt)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = drainRunStream(stream)
			return typeOutcomeGRPC(t, err)
		}},
		{"gRPC SpawnRunBatch", func(t *testing.T, h *pcHarness, bt string) string {
			res, err := h.client.SpawnRunBatch(ctx, &loomcyclepb.BatchSpawnRequest{Spawns: []*loomcyclepb.RunRequest{
				{Agent: "agent", Segments: typedSegment(bt)},
			}})
			outcome := typeOutcomeGRPC(t, err)
			if outcome == pcAccepted {
				if len(res.GetResults()) != 1 {
					t.Fatalf("results = %+v, want one", res.GetResults())
				}
				// A refusal reported as one failed child in a green envelope
				// is neither a refusal nor an acceptance.
				batchChildCompleted(t, res.GetResults()[0].GetStatus(), res.GetResults()[0].GetError())
			}
			return outcome
		}},
		{"gRPC CreateConfiguredRun", func(t *testing.T, h *pcHarness, bt string) string {
			_, err := h.client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: typedSegment(bt)})
			return typeOutcomeGRPC(t, err)
		}},
		{"POST /v1/runs", func(t *testing.T, h *pcHarness, bt string) string {
			code, body := httpDo(t, h.url+"/v1/runs", `{"agent":"agent",`+typedSegmentJSON(bt)+`}`)
			return typeOutcomeHTTP(t, code, body)
		}},
		{"POST /v1/runs, start:false", func(t *testing.T, h *pcHarness, bt string) string {
			code, body := httpDo(t, h.url+"/v1/runs", `{"agent":"agent","user_id":"u1","start":false,`+typedSegmentJSON(bt)+`}`)
			return typeOutcomeHTTP(t, code, body)
		}},
		{"POST /v1/sessions/{id}/messages", func(t *testing.T, h *pcHarness, bt string) string {
			code, body := httpDo(t, h.url+"/v1/sessions/"+seedSession(t, h)+"/messages", `{`+typedSegmentJSON(bt)+`}`)
			return typeOutcomeHTTP(t, code, body)
		}},
		{"POST /v1/runs:batch", func(t *testing.T, h *pcHarness, bt string) string {
			code, body := httpDo(t, h.url+"/v1/runs:batch", `{"spawns":[{"agent":"agent",`+typedSegmentJSON(bt)+`}]}`)
			outcome := typeOutcomeHTTP(t, code, body)
			if outcome == pcAccepted {
				var res struct {
					Results []struct{ Status, Error string } `json:"results"`
				}
				if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Results) != 1 {
					t.Fatalf("envelope = %s (%v), want one result", body, err)
				}
				batchChildCompleted(t, res.Results[0].Status, res.Results[0].Error)
			}
			return outcome
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPCHarness(t)
			// "untrusted" is the misspelling that matters: evidence the
			// caller meant to fence, run as instructions.
			for _, bad := range []string{"untrusted", "untrusted_block", "text"} {
				if got := tc.call(t, h, bad); got != pcRefused {
					t.Errorf("a block of type %q was %s, want %s", bad, got, pcRefused)
				}
			}
			for _, good := range []string{"", "trusted-text", "untrusted-block"} {
				if got := tc.call(t, h, good); got != pcAccepted {
					t.Errorf("a block of type %q was %s, want %s", good, got, pcAccepted)
				}
			}
		})
	}
}
