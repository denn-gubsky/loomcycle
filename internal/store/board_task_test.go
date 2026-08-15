package store

import (
	"context"
	"testing"
)

func TestBoardTaskContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := BoardTaskFromContext(ctx); ok {
		t.Fatal("empty ctx should carry no board task")
	}

	bt := BoardTask{Scope: "user", ChunkID: "c1", DocumentID: "d1"}
	ctx = WithBoardTask(ctx, bt)
	got, ok := BoardTaskFromContext(ctx)
	if !ok || got != bt {
		t.Fatalf("round-trip: got %+v ok=%v, want %+v", got, ok, bt)
	}

	// A zero task CLEARS it (so a handler's sub-agents aren't tagged).
	ctx = WithBoardTask(ctx, BoardTask{})
	if _, ok := BoardTaskFromContext(ctx); ok {
		t.Fatal("a zero board task should clear it")
	}
}

func TestParentContextBoardFields(t *testing.T) {
	var p *ParentContext
	if !p.IsZero() {
		t.Fatal("nil ParentContext is zero")
	}
	p = &ParentContext{BoardChunkID: "c1"}
	if p.IsZero() {
		t.Fatal("a board chunk id makes ParentContext non-zero")
	}
	cp := p.Clone()
	if cp == p {
		t.Fatal("clone must not alias")
	}
	if cp.BoardChunkID != "c1" {
		t.Fatalf("clone must copy board fields: %+v", cp)
	}
	// Board fields survive the JSON round-trip persisted in runs.parent_context.
	enc, ok, err := EncodeParentContext(&ParentContext{BoardScope: "user", BoardChunkID: "c1", BoardDocumentID: "d1"})
	if err != nil || !ok {
		t.Fatalf("encode: ok=%v err=%v", ok, err)
	}
	if enc == "" {
		t.Fatal("encoded board ParentContext should not be empty")
	}
}
