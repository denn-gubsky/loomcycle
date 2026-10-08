package tools

import (
	"context"
	"testing"
)

func TestMeteredOffRunCall_IsAbsentUnlessStamped(t *testing.T) {
	if _, ok := MeteredOffRunCall(context.Background()); ok {
		t.Error("a bare context reads as a metered off-run call")
	}
	// A run identity is what every admin and MCP dispatch stamps; it must not
	// read as the marker.
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{TenantID: "acme", UserID: "alice"})
	if _, ok := MeteredOffRunCall(ctx); ok {
		t.Error("a context carrying only a run identity reads as a metered off-run call")
	}
	v, ok := MeteredOffRunCall(WithMeteredOffRunCall(ctx, MeteredOffRunCallValue{TenantID: "acme", UserID: "alice"}))
	if !ok || v.TenantID != "acme" || v.UserID != "alice" {
		t.Errorf("the stamped value read back as %+v, %v", v, ok)
	}
}
