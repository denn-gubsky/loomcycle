package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A rejected relation stays rejected: not re-filed as a revision of itself, and
// not re-filed under another valid_at.
func TestProposeStatement_CannotRefileARejectedRelation(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "refile")
		p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q,"reason":"wrong"}`, p["id"])); r.IsError {
			t.Fatal(r.Text)
		}
		for name, body := range map[string]string{
			"revision": fmt.Sprintf(`{"op":"propose_statement","scope":"user","id":%q}`, p["id"]),
			"dated":    fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","valid_at":%d}`, a, b, time.Now().UnixNano()),
		} {
			if _, r := docExec(t, d, ctx, body); !r.IsError || !strings.Contains(r.Text, "statement_rejected") {
				t.Errorf("%s: want statement_rejected, got %s", name, r.Text)
			}
		}
	})
}
