package directory

// Package directory answers "who is in this deployment, and what do we hold for
// them" — without inventing a users table.
//
// A "user" in loomcycle is DERIVED, not stored: ListUsers is a GROUP BY over
// runs.user_id. That is deliberate and this package keeps it that way. There is no
// row to create and none to delete, so there is no create/update here; the delete
// half of "user CRUD" already exists as the subject-erasure surface, which is the
// only thing that can remove a person's footprint coherently across four planes.
//
// What was missing is the READ: an operator could see that `alice` has 12 runs, but
// answering "what does alice actually have here" meant five separate calls against
// five surfaces, each with its own tenant-scoping rule. Inspect is that one call.
//
// Extracted as a service rather than written into a handler for the same reason
// erasure was: MCP and HTTP must not drift on a tenant-scoping decision. Tenant
// arrives ALREADY RESOLVED from the caller's principal — this package never guesses
// it, because a subject id is only unique within one tenant.

import (
	"context"
	"errors"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ErrNoSubject is returned when no subject was named. A sentinel so each transport
// maps it to its own status code without string-matching.
var ErrNoSubject = errors.New("subject is required")

// Service reads the derived directory. SqlMem may be nil; the document counts are
// then reported as unexamined rather than zero.
type Service struct {
	Store  store.Store
	SqlMem *sqlmem.Manager
}

// UserRow is one derived user: activity only, which is all runs.user_id supports.
type UserRow struct {
	Subject       string `json:"subject"`
	RunningCount  int    `json:"running_count"`
	TotalCount    int    `json:"total_count"`
	LastStartedAt string `json:"last_started_at,omitempty"`
}

// Inspection is the aggregate per-subject view — the five surfaces an operator
// previously had to visit separately.
type Inspection struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`

	Activity  UserRow          `json:"activity"`
	Chats     int              `json:"chats"`
	Memory    map[string]int64 `json:"memory"`
	Documents *int             `json:"documents,omitempty"`
	Budget    *BudgetView      `json:"budget,omitempty"`
	Usage     UsageView        `json:"usage"`

	// Errors names planes that could not be read. A plane that FAILED is not a
	// plane that is empty, and an operational view that cannot tell them apart
	// invites the wrong conclusion — the same reason the erasure report carries
	// this field.
	Errors []string `json:"errors,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// BudgetView carries POINTERS because nil and zero mean different things here:
// nil is "no ceiling at this tier", zero would be a ceiling that refuses everything.
type BudgetView struct {
	SoftLimit *int64 `json:"soft_limit,omitempty"`
	HardLimit *int64 `json:"hard_limit,omitempty"`
}

type UsageView struct {
	Calls int64   `json:"calls"`
	Cost  float64 `json:"cost"`
}

// TenantRow is one tenant with derived counts.
type TenantRow struct {
	Tenant string `json:"tenant"`
	Users  int    `json:"users"`
	Runs   int    `json:"runs"`
}

// Users lists the subjects with activity in one tenant.
func (s *Service) Users(ctx context.Context, tenant string) ([]UserRow, error) {
	rows, err := s.Store.ListUsers(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]UserRow, 0, len(rows))
	for _, u := range rows {
		r := UserRow{Subject: u.UserID, RunningCount: u.RunningCount, TotalCount: u.TotalCount}
		if !u.LastStartedAt.IsZero() {
			r.LastStartedAt = u.LastStartedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, r)
	}
	return out, nil
}

// Tenants enumerates tenants with derived per-tenant counts.
//
// Admin-only at every transport, and not because the counts are secret: the LIST
// ITSELF is the disclosure. Knowing which tenants exist is exactly what a
// tenant-confined principal must not learn, and it is the reason every other
// cross-tenant read in this codebase is opaque-404 rather than filtered.
//
// Derived from runs, so a tenant that has never run anything does not appear. That
// is stated in the notes rather than papered over — an empty result means "no
// activity", not "no tenants".
func (s *Service) Tenants(ctx context.Context) ([]TenantRow, error) {
	rows, err := s.Store.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TenantRow, 0, len(rows))
	for _, t := range rows {
		out = append(out, TenantRow{Tenant: t.TenantID, Users: t.UserCount, Runs: t.TotalCount})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out, nil
}

// Inspect aggregates what one subject has in one tenant.
//
// Every read is best-effort so a single broken plane does not deny the whole view,
// but each fault is NAMED — an all-zero inspection assembled from failed queries
// would read as "this user has nothing", which is the one answer this must never
// give silently.
func (s *Service) Inspect(ctx context.Context, tenant, subject string) (Inspection, error) {
	if subject == "" {
		return Inspection{}, ErrNoSubject
	}
	ins := Inspection{Tenant: tenant, Subject: subject, Memory: map[string]int64{}}
	fail := func(plane string, err error) { ins.Errors = append(ins.Errors, plane+": "+err.Error()) }

	if rows, err := s.Users(ctx, tenant); err != nil {
		fail("activity", err)
	} else {
		for _, r := range rows {
			if r.Subject == subject {
				ins.Activity = r
				break
			}
		}
	}

	// ListSessions returns the total as its second value, so one page of size 1 is
	// enough for a count — no need to walk pages.
	if _, total, err := s.Store.ListSessions(ctx,
		store.SessionFilter{TenantID: tenant, UserID: subject}, 1, 0); err != nil {
		fail("chats", err)
	} else {
		ins.Chats = int(total)
	}

	// Memory is reported PER SCOPE rather than as one number: a subject's own
	// user-scope rows and the facts a shared agent holds about them are different
	// things, and collapsing them hides exactly the distinction the erasure
	// surface exists to make.
	if entries, _, err := s.Store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0); err != nil {
		fail("memory", err)
	} else {
		ins.Memory["user_scope_rows"] = int64(len(entries))
	}

	if s.SqlMem != nil {
		if scopes, err := s.SqlMem.ListScopes(ctx); err != nil {
			fail("documents", err)
		} else {
			n := 0
			want := sqlScopeTenant(tenant)
			for _, sk := range scopes {
				// Tenant-compared, because ListScopes spans every tenant — the same
				// trap that produced a cross-tenant oracle in the erasure report.
				if sk.Tenant == want && sk.Scope == "user" && sk.ScopeID == subject {
					n++
				}
			}
			ins.Documents = &n
		}
	} else {
		ins.Notes = append(ins.Notes,
			"SQL Memory is not configured, so the document plane was NOT examined — its "+
				"absence here is not evidence that it is empty.")
	}

	if limits, err := s.Store.TokenLimitsAll(ctx); err != nil {
		fail("budget", err)
	} else {
		for _, l := range limits {
			if l.TenantID == tenant && l.Scope == "user" && l.ScopeID == subject {
				// Both are *int64: nil means "no ceiling at this tier", which is
				// different from zero (a zero hard limit would refuse every run).
				b := BudgetView{}
				if l.SoftLimit != nil {
					b.SoftLimit = l.SoftLimit
				}
				if l.HardLimit != nil {
					b.HardLimit = l.HardLimit
				}
				ins.Budget = &b
				break
			}
		}
	}

	if agg, err := s.Store.UsageReport(ctx, store.UsageQuery{
		TenantID: tenant, GroupBy: []store.UsageDimension{store.UsageByUser},
	}); err != nil {
		fail("usage", err)
	} else {
		for _, a := range agg {
			if a.UserID == subject {
				ins.Usage.Calls += a.CallCount
				ins.Usage.Cost += a.Cost
			}
		}
	}

	if len(ins.Errors) > 0 {
		ins.Notes = append(ins.Notes,
			"one or more planes could not be read (see errors) — every count here is a LOWER BOUND.")
	}
	return ins, nil
}

// sqlScopeTenant maps the raw principal tenant onto SQL Memory's spelling: it
// refuses an empty tenant and stores the default one as "default", while the k/v
// plane keeps the raw "". Same rule as builtin.sqlScopeTenant and the erasure
// service — getting it wrong here would count another tenant's documents.
// sqlScopeTenant delegates to sqlmem.ScopeTenant, which owns this rule.
func sqlScopeTenant(tenant string) string { return sqlmem.ScopeTenant(tenant) }
