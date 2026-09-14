package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

func decodeStructured(t *testing.T, res *loommcp.CallToolResult) map[string]any {
	t.Helper()
	if len(res.StructuredContent) == 0 {
		t.Fatal("no structuredContent on result")
	}
	var m map[string]any
	if err := json.Unmarshal(res.StructuredContent, &m); err != nil {
		t.Fatalf("structuredContent is not valid JSON: %v", err)
	}
	return m
}

// V5 — the additive claim, tested rather than asserted. A failure the runtime
// cannot categorise must produce EXACTLY the bytes it produced before this
// change, so a client that never heard of the key sees nothing new.
func TestStructured_UnclassifiedErrorIsByteIdenticalToBefore(t *testing.T) {
	plain := toolErr("spawn_run: " + errors.New("something nobody classified").Error())
	viaFrom := toolErrFrom("spawn_run", errors.New("something nobody classified"))

	if len(viaFrom.StructuredContent) != 0 {
		t.Fatalf("unclassified error emitted structuredContent: %s", viaFrom.StructuredContent)
	}

	a, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(viaFrom)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("wire bytes differ for an unclassified error:\n old: %s\n new: %s", a, b)
	}
	if strings.Contains(string(b), "structuredContent") {
		t.Errorf("key present on an unclassified failure: %s", b)
	}
}

// A classified error carries the three convention fields.
func TestStructured_ClassifiedErrorCarriesCategory(t *testing.T) {
	res := toolErrFrom("spawn_run", fmt.Errorf("admit: %w", runner.ErrRuntimePaused))
	if !res.IsError {
		t.Error("IsError should stay true")
	}
	m := decodeStructured(t, res)

	if m["errorCategory"] != "transient" {
		t.Errorf("errorCategory = %v, want transient", m["errorCategory"])
	}
	if m["isRetryable"] != true {
		t.Errorf("isRetryable = %v, want true", m["isRetryable"])
	}
	if d, _ := m["description"].(string); strings.TrimSpace(d) == "" {
		t.Error("description empty — a category with no next step is not actionable")
	}
	// The text block is unchanged: structuredContent is a sibling, not a
	// replacement, and a client reading only text must lose nothing.
	if len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, "runtime paused") {
		t.Errorf("human-readable text was altered: %+v", res.Content)
	}
}

// V7 — retryAfterSeconds rides only on retryable failures, and is ABSENT
// rather than 0 when there is no hint. A zero would read as "retry now",
// which is the opposite of "wait as you judge best".
func TestStructured_RetryAfterOnlyWhenRetryable(t *testing.T) {
	t.Run("transient with hint carries seconds", func(t *testing.T) {
		m := decodeStructured(t, toolErrFrom("spawn_run", runner.ErrBackpressure))
		secs, ok := m["retryAfterSeconds"].(float64)
		if !ok {
			t.Fatalf("retryAfterSeconds missing on a hinted transient: %v", m)
		}
		if secs != 5 {
			t.Errorf("retryAfterSeconds = %v, want 5 (must agree with the HTTP Retry-After for the same condition)", secs)
		}
	})

	t.Run("business never carries a backoff", func(t *testing.T) {
		m := decodeStructured(t, toolErrFrom("spawn_run", runner.ErrTokenLimitExceeded))
		if m["errorCategory"] != "business" {
			t.Fatalf("errorCategory = %v, want business", m["errorCategory"])
		}
		if _, present := m["retryAfterSeconds"]; present {
			t.Error("a non-retryable failure carries a backoff hint — it would tell an agent to wait and then retry something that can never succeed")
		}
		if m["isRetryable"] != false {
			t.Errorf("isRetryable = %v, want false", m["isRetryable"])
		}
	})

	t.Run("transient without a hint omits the key", func(t *testing.T) {
		m := decodeStructured(t, toolErrFrom("memory", store.ErrDimensionMismatch))
		if _, present := m["retryAfterSeconds"]; present {
			t.Error("key present with no hint to give")
		}
	})
}

// A backoff attached to a non-retryable ErrorInfo must be dropped at render
// time rather than trusted. Guards the renderer itself, not just the
// classifier that feeds it — a future construction site could get the pairing
// wrong, and this is the last place to catch it.
func TestStructured_RendererDropsBackoffOnNonRetryable(t *testing.T) {
	d := 30 * time.Second
	raw := loommcp.StructuredErrorJSON(tools.ErrorInfo{
		Category:    tools.CategoryBusiness,
		Retryable:   false,
		Description: "refused by a rule",
		RetryAfter:  &d, // deliberately inconsistent
	})
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["retryAfterSeconds"]; present {
		t.Error("renderer forwarded a backoff on a non-retryable failure instead of dropping it")
	}
}

// An empty category renders nothing at all, so "not classified" can never
// become a half-populated payload.
func TestStructured_EmptyCategoryRendersNothing(t *testing.T) {
	if raw := loommcp.StructuredErrorJSON(tools.ErrorInfo{}); raw != nil {
		t.Errorf("unclassified info rendered %s, want nil", raw)
	}
}

// V3 — the security invariant. A cross-tenant miss folds to an opaque
// not-found; if it ever carried errorCategory "permission" the category would
// confirm the row exists, turning the error into an existence oracle.
//
// This asserts the property at the seam that decides it: the classifier is
// never taught a "belongs to another tenant" condition, so no not-found path
// can acquire the permission label by accident.
func TestStructured_CrossTenantMissIsNeverPermission(t *testing.T) {
	// The errors a tenant-folded read actually produces.
	notFounds := []error{
		&store.ErrNotFound{},
		fmt.Errorf("get: def_id %q not found", "other-tenants-def"),
		errors.New("session not found"),
	}
	for _, err := range notFounds {
		res := toolErrFrom("agentdef", err)
		if len(res.StructuredContent) == 0 {
			continue // unclassified is fine — it discloses nothing
		}
		m := decodeStructured(t, res)
		if m["errorCategory"] == "permission" {
			t.Errorf("%v classified as permission — a cross-tenant miss must stay an opaque not-found, "+
				"or the category confirms the row exists", err)
		}
	}
}

// toolErrValidation labels a refusal the handler decides itself, and must hand
// the agent a next step rather than restate the failure.
func TestStructured_ValidationCarriesAFix(t *testing.T) {
	res := toolErrValidation(
		"compact_run: agent_id is required",
		"Pass agent_id, the handle returned by spawn_run. Call list_runs if you do not have it.",
	)
	m := decodeStructured(t, res)
	if m["errorCategory"] != "validation" {
		t.Errorf("errorCategory = %v, want validation", m["errorCategory"])
	}
	if m["isRetryable"] != false {
		t.Error("a malformed request is not fixed by resending it unchanged")
	}
	fix, _ := m["description"].(string)
	if strings.TrimSpace(fix) == "" {
		t.Fatal("no next step given")
	}
	if fix == res.Content[0].Text {
		t.Error("description merely restates the message — costs tokens, carries no decision")
	}
}
