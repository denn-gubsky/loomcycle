package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// setSection puts value into the envelope as section name, adding it when the
// capture did not emit it, and drops the checksum that would refuse the edit.
func setSection(t *testing.T, raw []byte, name string, value any) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env["sections"].(map[string]any)[name] = value
	delete(env, "checksum")
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// seedUserAndBudget gives src one user and one tenant budget, both of which a
// restore writes in its first stage.
func seedUserAndBudget(t *testing.T, src store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := src.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := src.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(10)}); err != nil {
		t.Fatal(err)
	}
}

// A section later in the restore order at a version this reader cannot read
// refuses the restore before anything is written: before the fix users and
// token_limits landed first, and a retry of the fixed envelope then found the
// budget present and handed the caller nothing to refresh, so the budget went
// unenforced until a restart.
func TestRestore_TooNewLaterSectionWritesNothing(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	seedUserAndBudget(t, src)
	good := mustCapture(t, src)
	tooNew := editSection(t, good, migrations.SectionAgentDefs, func(sec map[string]any) {
		sec["version"] = "99.0"
	})

	_, err := Restore(ctx, dst, tooNew, RestoreOptions{})
	var tn *migrations.ErrSnapshotVersionTooNew
	if !errors.As(err, &tn) || tn.Section != migrations.SectionAgentDefs {
		t.Fatalf("Restore err = %v, want ErrSnapshotVersionTooNew naming %s", err, migrations.SectionAgentDefs)
	}
	if got, _ := dst.TokenLimitsAll(ctx); len(got) != 0 {
		t.Errorf("budgets after a refused restore = %+v, want none", got)
	}
	if got, _ := dst.UserList(ctx, ""); len(got) != 0 {
		t.Errorf("users after a refused restore = %+v, want none", got)
	}

	res := mustRestore(t, dst, good, RestoreOptions{})
	if len(res.Refresh.TokenLimits) != 1 {
		t.Errorf("retry with the good envelope: refresh rows = %+v, want the 1 budget it wrote", res.Refresh.TokenLimits)
	}
}

// A section Restore skips is not version-checked either, so it cannot refuse
// a restore it never refused: history not asked for, SQL Memory not wired.
func TestRestore_TooNewSkippedSectionStillRestores(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	seedUserAndBudget(t, src)
	raw := mustCapture(t, src)
	raw = setSection(t, raw, migrations.SectionInteractionHistory, map[string]any{"version": "99.0", "events": []any{}})
	raw = setSection(t, raw, migrations.SectionSqlMem, map[string]any{"version": "99.0"})

	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.TokenLimitsRestored != 1 {
		t.Errorf("token limits restored = %d, want 1", res.TokenLimitsRestored)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"interaction_history section present", "sqlmem section present"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %v, want one containing %q", res.Warnings, want)
		}
	}
}
