package providers

import (
	"context"
	"testing"
)

type visionCaps struct{ vision bool }

func (c visionCaps) ID() string                                   { return "v" }
func (c visionCaps) Probe(context.Context) error                  { return nil }
func (c visionCaps) ListModels(context.Context) ([]string, error) { return nil, nil }
func (c visionCaps) Capabilities() Capabilities                   { return Capabilities{SupportsVision: c.vision} }
func (c visionCaps) Call(context.Context, Request) (<-chan Event, error) {
	return nil, nil
}

type visionPerModel struct{ visionCaps }

func (visionPerModel) SupportsVisionFor(model string) bool { return model == "sees" }

// The Capabilities bit is the floor; a driver's per-model answer can only
// narrow it.
func TestSeesImages_CapabilitiesFloorThenPerModel(t *testing.T) {
	if SeesImages(visionCaps{false}, "m") || !SeesImages(visionCaps{true}, "m") {
		t.Error("without a per-model answer the Capabilities bit decides")
	}
	if SeesImages(visionPerModel{visionCaps{false}}, "sees") {
		t.Error("a per-model yes must not lift a provider that sees no images")
	}
	if !SeesImages(visionPerModel{visionCaps{true}}, "sees") || SeesImages(visionPerModel{visionCaps{true}}, "blind") {
		t.Error("the per-model answer must refine a vision provider")
	}
	if SeesImages(nil, "m") {
		t.Error("no provider sees nothing")
	}
}

// A picture inside a tool result counts as an image for the drivers' and the
// loop's gates, and a prompt snapshot keeps its type but not its bytes.
func TestToolImage_CountedAsImageAndDroppedFromSnapshot(t *testing.T) {
	tr := ContentBlock{Type: "tool_result", ToolUseID: "t", Text: "x",
		Images: []ImagePart{{MediaType: "image/png", Data: "QUJD"}}}
	msgs := []Message{{Role: "user", Content: []ContentBlock{tr}}}
	if !RequestHasImage(Request{Messages: msgs}) {
		t.Error("RequestHasImage must count a tool result's picture")
	}
	snap := NewPromptSnapshot(nil, msgs)
	if got := snap.Input[0].Images; len(got) != 1 || got[0].Data != "" || got[0].MediaType != "image/png" {
		t.Errorf("snapshot images = %+v, want type kept and bytes dropped", got)
	}
	if msgs[0].Content[0].Images[0].Data != "QUJD" {
		t.Error("the snapshot must not change the request's own blocks")
	}
}
