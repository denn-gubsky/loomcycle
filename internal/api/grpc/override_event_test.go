package grpc

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RFC DC V9. The override event must reach a TYPED gRPC client, not just the Go
// struct.
//
// This is the failure RFC DA actually shipped: a field added to providers.Event
// that eventToProto never mapped, invisible to every typed consumer while every
// Go-side test passed. The struct carrying a field proves nothing about what a
// client receives.
func TestEventToProto_CarriesTheOverridePayload(t *testing.T) {
	ev := providers.Event{
		Type: providers.EventOverride,
		Text: "routing changed by operator: primary/model-a → secondary/model-b",
		Override: &providers.OverrideInfo{
			Source:    "operator",
			FromModel: "primary/model-a",
			ToModel:   "secondary/model-b",
			Fields:    []string{"model"},
		},
	}

	got := eventToProto(ev)
	if got == nil {
		t.Fatal("eventToProto returned nil")
	}
	if got.Override == nil {
		t.Fatal("the override payload stopped at the Go struct — a typed gRPC client " +
			"sees an override frame with nothing in it")
	}
	if got.Override.Source != "operator" {
		t.Errorf("source = %q, want operator", got.Override.Source)
	}
	if got.Override.FromModel != "primary/model-a" || got.Override.ToModel != "secondary/model-b" {
		t.Errorf("routing pair = %q → %q, want primary/model-a → secondary/model-b",
			got.Override.FromModel, got.Override.ToModel)
	}
	if len(got.Override.Fields) != 1 || got.Override.Fields[0] != "model" {
		t.Errorf("fields = %v, want [model]", got.Override.Fields)
	}
	// The type string a client switches on.
	if !strings.EqualFold(got.Type, string(providers.EventOverride)) {
		t.Errorf("type = %q, want %q", got.Type, providers.EventOverride)
	}
}

// An event with no override must not grow an empty payload — a client that
// checks presence would see every frame as an override.
func TestEventToProto_LeavesOtherEventsWithoutAnOverride(t *testing.T) {
	got := eventToProto(providers.Event{Type: providers.EventText, Text: "hello"})
	if got.Override != nil {
		t.Error("a plain text frame carries an override payload")
	}
}
