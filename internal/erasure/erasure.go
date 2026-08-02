package erasure

// Package erasure answers "what does this deployment hold about one subject",
// and removes what it can.
//
// Extracted from the HTTP handlers so MCP, gRPC and the adapters run the SAME
// code rather than four re-implementations. That matters more here than for a
// typical lift: an erasure that deleted a different set of planes depending on
// which transport asked would make "we erased them" a claim about a client
// library rather than about the data.
//
// THE SAFETY GUARDS LIVE HERE, NOT IN THE TRANSPORTS. Requiring `confirm` to
// match the subject, and defaulting to a dry run, are properties of the
// operation itself; leaving them to each caller would mean the newest transport
// is the one missing them. The only thing a transport owns is resolving WHICH
// tenant the caller may act in — that is an auth decision and cannot be made
// here, so Tenant arrives already resolved and is never guessed.
//
// The three tiers are separated by what they GUARANTEE, not by what they hold:
//
//  1. subject-keyed, and existing primitives delete it
//  2. subject-keyed, and nothing deletes it (before this feature)
//  3. not subject-keyed at all — reachable only by tracing provenance from the
//     subject's chats, and therefore lost the moment those chats are deleted

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ErrConfirmMismatch is returned when a live erasure's confirm does not equal
// the subject. Sentinel so each transport can map it to its own status code
// (HTTP 400 / gRPC InvalidArgument) without string-matching.
var ErrConfirmMismatch = errors.New(
	"a live erasure requires confirm to equal subject — this is irreversible")

// ErrNoSubject is returned when subject is empty.
var ErrNoSubject = errors.New("subject is required")

// Service performs erasure reads and deletes. SqlMem may be nil — the report
// then declares that plane UNEXAMINED rather than omitting it, because with the
// subsystem absent it genuinely was not looked at.
type Service struct {
	Store  store.Store
	SqlMem *sqlmem.Manager
}

// provenanceBound caps the residue walk. A hit sets Residue.Truncated, making
// the count a floor rather than a total.
const provenanceBound = 5000

// Report is the wire shape shared by every transport.
type Report struct {
	Tenant  string   `json:"tenant"`
	Subject string   `json:"subject"`
	Tier1   Tier     `json:"tier1_covered"`
	Tier2   Tier     `json:"tier2_uncovered"`
	Tier3   Residue  `json:"tier3_residue"`
	Notes   []string `json:"notes"`
	// Errors names planes that could not be READ. A plane that failed is not a
	// plane that is empty, and a report unable to distinguish them cannot answer
	// the question it exists for.
	Errors []string `json:"errors,omitempty"`
}

type Tier struct {
	Counts map[string]int64 `json:"counts"`
	Total  int64            `json:"total"`
}

func (t *Tier) set(name string, n int64) {
	if t.Counts == nil {
		t.Counts = map[string]int64{}
	}
	t.Counts[name] = n
	t.Total += n
}

// Residue is what a subject-keyed delete cannot reach.
type Residue struct {
	Rows   int      `json:"rows"`
	Scopes []string `json:"scopes"`
	// SessionsExamined distinguishes "nothing derives from them" from "nothing to
	// trace from" — a zero across zero sessions means UNKNOWN, not NONE.
	SessionsExamined int  `json:"sessions_examined"`
	Truncated        bool `json:"truncated"`
}

// ExecuteRequest carries a resolved erasure. Tenant is authoritative and comes
// from the caller's principal — never from a request field.
type ExecuteRequest struct {
	Tenant  string
	Subject string
	DryRun  bool
	Confirm string
}

// Result reports what an erasure did, or would do.
type Result struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	DryRun  bool   `json:"dry_run"`
	// Deleted is per-plane. A key's PRESENCE means the plane was examined; its
	// value means how many rows went. Planes are registered at zero up front so
	// "found none" can never look like "never looked".
	Deleted map[string]int `json:"deleted"`
	// Retained is plane → why. Never empty: an erasure listing only its successes
	// reads as complete.
	Retained map[string]string `json:"retained"`
	Residue  Residue           `json:"residue"`
	Errors   []string          `json:"errors,omitempty"`
	Notes    []string          `json:"notes"`
}

const retainedUsageLedger = "retained by design: cost rows are accounting records an " +
	"operator may be legally required to keep. The correct treatment is to break the personal " +
	"linkage rather than destroy the totals, which is a row merge (usage_archive keys user_id " +
	"in its primary key) and is not implemented yet."

// Report enumerates one subject's footprint. Read-only.
func (s *Service) Report(ctx context.Context, tenant, subject string) (Report, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Report{}, ErrNoSubject
	}
	rep := Report{
		Tenant:  tenant,
		Subject: subject,
		Tier1:   Tier{Counts: map[string]int64{}},
		Tier2:   Tier{Counts: map[string]int64{}},
	}
	fail := func(plane string, err error) { rep.Errors = append(rep.Errors, plane+": "+err.Error()) }

	sessions, err := s.subjectSessions(ctx, tenant, subject)
	if err != nil {
		fail("chats", err)
	}
	rep.Tier1.set("chats", int64(len(sessions)))

	if entries, _, err := s.Store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0); err != nil {
		fail("memory", err)
	} else {
		rep.Tier1.set("memory_rows", int64(len(entries)))
	}

	var sqlMemUnexamined bool
	if s.SqlMem != nil {
		if scopes, err := s.SqlMem.ListScopes(ctx); err != nil {
			fail("sql_memory", err)
		} else {
			// Counted into a local and set UNCONDITIONALLY, including zero: setting
			// only on a hit would make absence mean either "no scope" or "never
			// looked" — the failed-vs-empty ambiguity `Errors` exists to prevent,
			// reintroduced one field lower down.
			var n int64
			for _, sk := range scopes {
				if sk.Scope == "user" && sk.ScopeID == subject {
					n = 1
					break
				}
			}
			rep.Tier1.set("sql_memory_scopes", n)
		}
	} else {
		sqlMemUnexamined = true
	}

	if rows, err := s.Store.DirentListUnder(ctx, tenant, "user", subject, "/"); err != nil {
		fail("paths", err)
	} else {
		rep.Tier1.set("path_entries", int64(len(rows)))
	}

	// ---- tier 2 ----
	if creds, err := s.Store.CredentialDefList(ctx, tenant, "user", subject); err != nil {
		fail("credentials", err)
	} else {
		rep.Tier2.set("credentials", int64(len(creds)))
	}
	if limits, err := s.Store.TokenLimitsAll(ctx); err != nil {
		fail("token_limits", err)
	} else {
		var n int64
		for _, l := range limits {
			if l.TenantID == tenant && l.Scope == "user" && l.ScopeID == subject {
				n++
			}
		}
		rep.Tier2.set("token_limits", n)
	}
	if ints, err := s.Store.InterruptListByUser(ctx, subject, tenant, ""); err != nil {
		fail("interrupts", err)
	} else {
		rep.Tier2.set("interrupts", int64(len(ints)))
	}
	if agg, err := s.Store.UsageReport(ctx, store.UsageQuery{
		TenantID: tenant, GroupBy: []store.UsageDimension{store.UsageByUser},
	}); err != nil {
		fail("usage", err)
	} else {
		var calls int64
		for _, a := range agg {
			if a.UserID == subject {
				calls += a.CallCount
			}
		}
		rep.Tier2.set("usage_ledger_calls", calls)
	}

	rep.Tier3, err = s.residueFor(ctx, tenant, subject, sessions)
	if err != nil {
		fail("provenance", err)
	}

	rep.Notes = []string{
		"tier1 is deletable with existing primitives. tier2 is subject-keyed but nothing deletes it yet — the credential rows are the ones that matter.",
		"tier3 is found through provenance, so it covers only facts that RECORDED the chat they came from. A fact written about this subject WITHOUT provenance is not reachable by any mechanism, and is not counted here.",
		"counts are live rows: expired and superseded rows are excluded, because they are invisible to every read and counting them would overstate what is held.",
	}
	if sqlMemUnexamined {
		rep.Notes = append(rep.Notes,
			"SQL Memory is not configured on this deployment, so the sql_memory plane was NOT "+
				"examined — its absence from the counts is not evidence that it is empty.")
	}
	if rep.Tier3.Truncated {
		rep.Notes = append(rep.Notes,
			"the provenance walk hit its row bound, so tier3 is a FLOOR rather than a total.")
	}
	if rep.Tier3.SessionsExamined == 0 {
		rep.Notes = append(rep.Notes,
			"this subject has NO chats, so tier3 is UNDETERMINABLE rather than zero: derived "+
				"facts are found only by tracing provenance from a chat. If the subject was "+
				"previously erased, residue may exist that nothing can now locate — the erasure "+
				"response is the only record of it.")
	}
	if len(rep.Errors) > 0 {
		rep.Notes = append(rep.Notes,
			"one or more planes could not be read (see errors) — every count here is a LOWER BOUND.")
	}
	return rep, nil
}

// Execute removes tiers 1 and 2, and reports what it could not reach.
//
// Chats go LAST: transcripts are the recovery record, so a crash mid-erasure
// should leave them standing. The residue is measured FIRST, from session ids
// captured up front — see Result.Notes for why that number can never be
// recovered afterwards.
func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (Result, error) {
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return Result{}, ErrNoSubject
	}
	// Enforced HERE so every transport inherits it. A subject id matching nothing
	// is harmless; one matching the wrong person is irreversible.
	if !req.DryRun && req.Confirm != subject {
		return Result{}, ErrConfirmMismatch
	}
	tenant := req.Tenant
	res := Result{
		Tenant: tenant, Subject: subject, DryRun: req.DryRun,
		Deleted: map[string]int{}, Retained: map[string]string{},
	}
	for _, plane := range []string{
		"chats", "memory_rows", "path_entries",
		"credentials", "token_limits", "interrupts",
	} {
		res.Deleted[plane] = 0
	}
	if s.SqlMem != nil {
		res.Deleted["sql_memory_scopes"] = 0
	}
	fail := func(plane string, err error) { res.Errors = append(res.Errors, plane+": "+err.Error()) }

	sessions, err := s.subjectSessions(ctx, tenant, subject)
	if err != nil {
		fail("chats", err)
	}
	res.Residue, err = s.residueFor(ctx, tenant, subject, sessions)
	if err != nil {
		fail("provenance", err)
	}

	// ---- tier 2 ----
	if creds, err := s.Store.CredentialDefList(ctx, tenant, "user", subject); err != nil {
		fail("credentials", err)
	} else {
		for _, c := range creds {
			if req.DryRun {
				res.Deleted["credentials"]++
				continue
			}
			if ok, err := s.Store.CredentialDefDelete(ctx, tenant, "user", subject, c.Name); err != nil {
				fail("credentials", err)
			} else if ok {
				res.Deleted["credentials"]++
			}
		}
	}
	if limits, err := s.Store.TokenLimitsAll(ctx); err != nil {
		fail("token_limits", err)
	} else {
		for _, l := range limits {
			if l.TenantID != tenant || l.Scope != "user" || l.ScopeID != subject {
				continue
			}
			if req.DryRun {
				res.Deleted["token_limits"]++
				continue
			}
			if err := s.Store.TokenLimitDelete(ctx, tenant, "user", subject); err != nil {
				fail("token_limits", err)
			} else {
				res.Deleted["token_limits"]++
			}
		}
	}
	if req.DryRun {
		if ints, err := s.Store.InterruptListByUser(ctx, subject, tenant, ""); err != nil {
			fail("interrupts", err)
		} else {
			res.Deleted["interrupts"] = len(ints)
		}
	} else if n, err := s.Store.InterruptDeleteAllByUser(ctx, subject, tenant); err != nil {
		fail("interrupts", err)
	} else {
		res.Deleted["interrupts"] = n
	}

	// ---- tier 1, chats excepted ----
	if req.DryRun {
		if entries, _, err := s.Store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0); err != nil {
			fail("memory", err)
		} else {
			res.Deleted["memory_rows"] = len(entries)
		}
	} else if n, err := s.Store.MemoryDeleteScope(ctx, tenant, store.MemoryScopeUser, subject); err != nil {
		fail("memory", err)
	} else {
		res.Deleted["memory_rows"] = n
	}

	if s.SqlMem != nil {
		key := sqlmem.ScopeKey{Tenant: tenant, Scope: "user", ScopeID: subject}
		if req.DryRun {
			if scopes, err := s.SqlMem.ListScopes(ctx); err != nil {
				fail("sql_memory", err)
			} else {
				for _, sk := range scopes {
					if sk.Scope == "user" && sk.ScopeID == subject {
						res.Deleted["sql_memory_scopes"] = 1
						break
					}
				}
			}
		} else if removed, err := s.SqlMem.DropScope(ctx, key); err != nil {
			fail("sql_memory", err)
		} else if removed {
			res.Deleted["sql_memory_scopes"] = 1
		}
	}

	if rows, err := s.Store.DirentListUnder(ctx, tenant, "user", subject, "/"); err != nil {
		fail("paths", err)
	} else {
		for _, d := range rows {
			if req.DryRun {
				res.Deleted["path_entries"]++
				continue
			}
			if ok, err := s.Store.DirentDelete(ctx, tenant, "user", subject, d.ParentPath, d.Name); err != nil {
				fail("paths", err)
			} else if ok {
				res.Deleted["path_entries"]++
			}
		}
	}

	// ---- chats LAST ----
	if req.DryRun {
		res.Deleted["chats"] = len(sessions)
	} else {
		for _, id := range sessions {
			if err := s.Store.DeleteSessionCascade(ctx, id); err != nil {
				fail("chats", err)
				continue
			}
			res.Deleted["chats"]++
		}
	}

	res.Retained["usage_ledger"] = retainedUsageLedger
	if res.Residue.Rows > 0 {
		res.Retained["tier3_residue"] = fmt.Sprintf(
			"%d fact(s) about this subject live in scope(s) they do not own (%s). They are not "+
				"keyed to the subject, so no subject-keyed delete reaches them; removing them is a "+
				"judgement call about someone else's scope, not a mechanical erasure.",
			res.Residue.Rows, strings.Join(res.Residue.Scopes, ", "))
	}
	res.Notes = []string{
		"tier-3 residue is traceable ONLY through the subject's chats, which this operation " +
			"deletes. It is measured before anything is removed — but once the chats are gone " +
			"no later query can find it again, and a report run afterwards will show residue 0 " +
			"while the facts remain. THIS RESPONSE IS THE ONLY DURABLE RECORD OF WHAT WAS NOT " +
			"REACHED — retain it.",
	}
	if req.DryRun {
		res.Notes = append(res.Notes,
			"DRY RUN — nothing was deleted. Re-send with dry_run:false and confirm:<subject> to execute.")
	}
	if len(res.Errors) > 0 {
		res.Notes = append(res.Notes,
			"one or more planes faulted (see errors). This erasure is INCOMPLETE — re-run it; "+
				"the operation is idempotent.")
	}
	return res, nil
}

// subjectSessions lists the subject's chats — the handle tier 3 traces from.
func (s *Service) subjectSessions(ctx context.Context, tenant, subject string) ([]string, error) {
	var out []string
	const page = 500
	for offset := 0; ; offset += page {
		rows, _, err := s.Store.ListSessions(ctx,
			store.SessionFilter{TenantID: tenant, UserID: subject}, page, offset)
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			out = append(out, r.SessionID)
		}
		if len(rows) < page {
			return out, nil
		}
	}
}

// residueFor walks the subject's chats to facts derived from them.
func (s *Service) residueFor(ctx context.Context, tenant, subject string, sessions []string) (Residue, error) {
	res := Residue{SessionsExamined: len(sessions), Scopes: []string{}}
	if len(sessions) == 0 {
		return res, nil
	}
	rows, err := s.Store.MemoryListBySourceSessions(ctx, tenant, sessions, provenanceBound)
	if err != nil {
		return res, err
	}
	seen := map[string]bool{}
	for _, row := range rows {
		// The subject's OWN user scope is tier 1. Every other scope is residue,
		// INCLUDING another user's: a fact about this subject filed under a
		// different person is exactly what a subject-keyed delete misses.
		if row.Scope == store.MemoryScopeUser && row.ScopeID == subject {
			continue
		}
		res.Rows++
		label := string(row.Scope)
		if row.ScopeID != "" {
			label += ":" + row.ScopeID
		}
		if !seen[label] {
			seen[label] = true
			res.Scopes = append(res.Scopes, label)
		}
	}
	res.Truncated = len(rows) == provenanceBound
	return res, nil
}
