package builtin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FP §13 R6 — a named lease on one document scope.

func leaseKey(t *testing.T, d *Document, ctx context.Context) context.Context {
	t.Helper()
	return tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"*"}})
}

func TestDocumentLease_SecondOwnerFindsItHeld(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		now := time.Unix(1_000, 0)
		a, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now, time.Minute)
		if err != nil || !a.Acquired {
			t.Fatalf("first acquire = %+v, %v; want acquired", a, err)
		}
		b, err := d.acquireDocumentLease(ctx, key, "linker", "run-b", now.Add(time.Second), time.Minute)
		if err != nil || b.Acquired || b.HeldBy != "run-a" || b.ExpiresAt != a.ExpiresAt {
			t.Fatalf("second owner = %+v, %v; want refused, held_by run-a until %d", b, err, a.ExpiresAt)
		}
		again, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now.Add(2*time.Second), time.Minute)
		if err != nil || !again.Acquired || again.ExpiresAt <= a.ExpiresAt {
			t.Fatalf("holder re-lease = %+v, %v; want acquired and extended past %d", again, err, a.ExpiresAt)
		}
	})
}

func TestDocumentLease_ExpiredLeaseIsTakenOver(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		now := time.Unix(1_000, 0)
		if r, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now, time.Second); err != nil || !r.Acquired {
			t.Fatalf("acquire = %+v, %v", r, err)
		}
		r, err := d.acquireDocumentLease(ctx, key, "linker", "run-b", now.Add(2*time.Second), time.Minute)
		if err != nil || !r.Acquired {
			t.Fatalf("takeover after expiry = %+v, %v; want acquired", r, err)
		}
		// The previous holder no longer holds it, so its release is a no-op.
		if released, err := d.releaseDocumentLease(ctx, key, "linker", "run-a"); err != nil || released {
			t.Fatalf("stale holder release = %v, %v; want false", released, err)
		}
	})
}

// The scope's SQL database is also reachable through Memory sql_exec, so a lease
// row can be forged with an expiry far past the cap. Such a row must not hold a
// pass off the scope: anything expiring beyond now+max counts as expired.
func TestDocumentLease_ForgedFarFutureExpiryIsTakenOver(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatal(err)
		}
		now := time.Unix(1_000, 0)
		if err := d.exec(ctx, key, `INSERT INTO document_leases (name, owner, expires_at) VALUES (?, ?, ?)`,
			"linker", "squatter", now.Add(365*24*time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
		r, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now, time.Minute)
		if err != nil || !r.Acquired {
			t.Fatalf("acquire over a forged year-long lease = %+v, %v; want acquired", r, err)
		}
		// A lease written by the op, at the cap, still holds.
		if r, _ := d.acquireDocumentLease(ctx, key, "other", "run-a", now, maxDocumentLeaseTTL); !r.Acquired {
			t.Fatal("setup: acquire other")
		}
		if r, _ := d.acquireDocumentLease(ctx, key, "other", "run-b", now, time.Minute); r.Acquired {
			t.Fatal("a lease at exactly the cap was treated as forged")
		}
	})
}

func TestDocumentLease_OnlyTheHolderReleasesIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		now := time.Unix(1_000, 0)
		if r, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now, time.Hour); err != nil || !r.Acquired {
			t.Fatalf("acquire = %+v, %v", r, err)
		}
		if released, err := d.releaseDocumentLease(ctx, key, "linker", "run-b"); err != nil || released {
			t.Fatalf("non-holder release = %v, %v; want false", released, err)
		}
		if r, _ := d.acquireDocumentLease(ctx, key, "linker", "run-b", now, time.Hour); r.Acquired {
			t.Fatal("a non-holder's release freed the lease")
		}
		if released, err := d.releaseDocumentLease(ctx, key, "linker", "run-a"); err != nil || !released {
			t.Fatalf("holder release = %v, %v; want true", released, err)
		}
		if r, err := d.acquireDocumentLease(ctx, key, "linker", "run-b", now, time.Hour); err != nil || !r.Acquired {
			t.Fatalf("acquire after release = %+v, %v; want acquired", r, err)
		}
	})
}

// The point of a NAMED lease: a links pass and another kind of pass on the same
// scope do not exclude each other.
func TestDocumentLease_NamesAreIndependent(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		now := time.Unix(1_000, 0)
		if r, err := d.acquireDocumentLease(ctx, key, "linker", "run-a", now, time.Hour); err != nil || !r.Acquired {
			t.Fatalf("acquire linker = %+v, %v", r, err)
		}
		if r, err := d.acquireDocumentLease(ctx, key, "classifier", "run-b", now, time.Hour); err != nil || !r.Acquired {
			t.Fatalf("acquire classifier while linker is held = %+v, %v; want acquired", r, err)
		}
	})
}

// Racing owners for one free lease: the conditional UPDATE lets exactly one win.
func TestDocumentLease_ConcurrentAcquireHasOneWinner(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		// Execute provisions the scope before any op; these direct calls must too.
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatal(err)
		}
		now := time.Unix(1_000, 0)
		const n = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := d.acquireDocumentLease(ctx, key, "linker", fmt.Sprintf("run-%d", i), now, time.Hour)
				if err != nil {
					t.Errorf("acquire run-%d: %v", i, err)
					return
				}
				if r.Acquired {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Fatalf("%d of %d racing owners acquired the lease; want exactly 1", won, n)
		}
	})
}

// Until the statement_methods grant exists, the lease is the operator plane's
// alone: an agent inside a run is refused, before it can hold a pass off a scope.
func TestDocumentLease_RunWithoutTheGrantIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	for name, c := range map[string]context.Context{
		"no plane":   ctx,
		"inside run": tools.WithRunID(leaseKey(t, d, ctx), "r_agent"),
	} {
		for _, op := range []string{"lease", "release"} {
			_, r := docExec(t, d, c, fmt.Sprintf(`{"op":%q,"scope":"user","name":"linker"}`, op))
			if !r.IsError || !strings.Contains(r.Text, "lease is held only by") {
				t.Errorf("%s %s = %q; want a permission refusal", name, op, r.Text)
			}
		}
	}
}

func TestDocumentLease_OperatorTakesAndReleasesThroughTheTool(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	op := leaseKey(t, d, ctx)
	before := time.Now()
	out, r := docExec(t, d, op, `{"op":"lease","scope":"user","name":"linker","ttl_ms":86400000}`)
	if r.IsError || out["acquired"] != true {
		t.Fatalf("lease = %s; want acquired", r.Text)
	}
	// ttl_ms of a day is capped at an hour.
	exp := int64(out["expires_at"].(float64))
	if limit := before.Add(maxDocumentLeaseTTL + time.Minute).UnixNano(); exp > limit {
		t.Fatalf("expires_at %d is past the one-hour cap %d", exp, limit)
	}
	key, _, _ := d.resolveScope(op, "user")
	if other, _ := d.acquireDocumentLease(op, key, "linker", "run-x", time.Now(), time.Minute); other.Acquired || other.HeldBy != "agent:doc-agent" {
		t.Fatalf("another owner = %+v; want refused, held by agent:doc-agent", other)
	}
	out, r = docExec(t, d, op, `{"op":"release","scope":"user","name":"linker"}`)
	if r.IsError || out["released"] != true {
		t.Fatalf("release = %s; want released", r.Text)
	}
}

func TestDocumentLease_BadInputIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	op := leaseKey(t, d, ctx)
	for _, c := range []struct{ body, want string }{
		{`{"op":"lease","scope":"user"}`, "missing required field: name"},
		{`{"op":"lease","scope":"user","name":"Linker!"}`, "name must be"},
		{`{"op":"lease","scope":"user","name":"` + strings.Repeat("a", 65) + `"}`, "name must be"},
		{`{"op":"lease","scope":"user","name":"linker","ttl_ms":-1}`, "ttl_ms must not be negative"},
		{`{"op":"release","scope":"user"}`, "missing required field: name"},
	} {
		if _, r := docExec(t, d, op, c.body); !r.IsError || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s = %q; want an error containing %q", c.body, r.Text, c.want)
		}
	}
}
