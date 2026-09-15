package http

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/errkind"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

func TestRunErrorEvent_ClassifiesTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		category  errkind.Category
		retryable bool
	}{
		{"backpressure", runner.ErrBackpressure, errkind.CategoryTransient, true},
		{"runtime paused", runner.ErrRuntimePaused, errkind.CategoryTransient, true},
		{"token budget", runner.ErrTokenLimitExceeded, errkind.CategoryBusiness, false},
		{"unknown agent", runner.ErrUnknownAgent, errkind.CategoryValidation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := runErrorEvent(fmt.Errorf("run: %w", tc.err))
			if ev.Type != providers.EventError {
				t.Fatalf("type = %v, want error", ev.Type)
			}
			if ev.ErrorInfo == nil {
				t.Fatal("terminal failure carries no classification")
			}
			if ev.ErrorInfo.Category != tc.category {
				t.Errorf("category = %q, want %q", ev.ErrorInfo.Category, tc.category)
			}
			if ev.ErrorInfo.Retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", ev.ErrorInfo.Retryable, tc.retryable)
			}
			// The human-readable half is untouched: this is added signal, not
			// a replacement for what consumers already render.
			if !strings.Contains(ev.Error, tc.err.Error()) {
				t.Errorf("error text lost: %q", ev.Error)
			}
		})
	}
}

// No fallback bucket, same rule as every other surface in this line.
func TestRunErrorEvent_UnclassifiedStaysUnclassified(t *testing.T) {
	ev := runErrorEvent(fmt.Errorf("something nobody has reasoned about"))
	if ev.ErrorInfo != nil {
		t.Errorf("invented a category %q for an unclassified failure", ev.ErrorInfo.Category)
	}
	if ev.Error == "" {
		t.Error("lost the message")
	}
}

// THE CROSSING. The struct carrying a field proves nothing about what an SSE
// consumer receives — the event is marshalled to JSON on the way out, and a
// missing or wrong tag would drop it silently while every struct-level test
// stayed green.
func TestRunErrorEvent_SurvivesSSESerialization(t *testing.T) {
	ev := runErrorEvent(fmt.Errorf("admit: %w", runner.ErrTokenLimitExceeded))

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	info, ok := wire["error_info"].(map[string]any)
	if !ok {
		t.Fatalf("error_info absent from the SSE frame — a consumer still sees only a string:\n%s", raw)
	}
	if info["Category"] != string(errkind.CategoryBusiness) && info["category"] != string(errkind.CategoryBusiness) {
		t.Errorf("category did not survive serialization: %v", info)
	}

	// And it round-trips back, which is what a transcript replay depends on.
	var back providers.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.ErrorInfo == nil || back.ErrorInfo.Category != errkind.CategoryBusiness {
		t.Errorf("classification lost on round-trip: %+v", back.ErrorInfo)
	}
}

// An unclassified event must not grow the key at all, so existing consumers
// see byte-identical frames.
func TestRunErrorEvent_UnclassifiedFrameIsUnchanged(t *testing.T) {
	raw, err := json.Marshal(runErrorEvent(fmt.Errorf("plain failure")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "error_info") {
		t.Errorf("unclassified frame grew the key: %s", raw)
	}
}

// --- census guard ---

// TestRunErrorEvent_NoWriterBypassesTheHelper keeps the five writers at five.
//
// A census found the streaming run path, the detached path, the
// continue-a-session path, the resume path and the resident sub-agent path all
// building this event themselves. Centralising only helps while nobody adds a
// sixth inline — and that failure is silent, because the event still goes out,
// just without the classification.
//
// So: no composite literal in this package may build an EventError from a run
// error directly. Build it with runErrorEvent.
func TestRunErrorEvent_NoWriterBypassesTheHelper(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	files := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			files++
			if strings.HasSuffix(name, "run_error_event.go") {
				continue // the helper itself
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isProvidersEvent(lit) {
					return true
				}
				if !litHasEventError(lit) {
					return true
				}
				// An EventError built from a run error is the case that must
				// go through the helper; one built from a literal string is a
				// different thing (a message this layer authored itself).
				if litMentionsIdent(lit, "runErr") {
					t.Errorf("%s:%d builds an EventError from runErr inline — use runErrorEvent(runErr), "+
						"or the classification is silently dropped on this path",
						name[strings.LastIndex(name, "/")+1:], fset.Position(lit.Pos()).Line)
				}
				return true
			})
		}
	}
	if files == 0 {
		t.Fatal("parsed zero files — the scan is broken, so this guard proves nothing")
	}
}

func isProvidersEvent(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "providers" && sel.Sel.Name == "Event"
}

func litHasEventError(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := kv.Key.(*ast.Ident)
		if !ok || k.Name != "Type" {
			continue
		}
		if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "EventError" {
			return true
		}
	}
	return false
}

func litMentionsIdent(lit *ast.CompositeLit, name string) bool {
	found := false
	ast.Inspect(lit, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
