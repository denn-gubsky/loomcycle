package http

// erasure_report.go — what this deployment holds about one subject.
//
// Read-only, deliberately, and shipped before the deletion half. Two reasons.
//
// It is useful on its own: "what do you hold about me" is a request in its own
// right, asked far more often than "delete it", and answering it should not require
// arming something destructive.
//
// And it is the honest order. An erasure op that cannot enumerate what it will miss
// reads as completeness, which is worse than an honest gap: a subject-keyed delete
// that silently leaves facts ABOUT the subject in a shared scope looks like
// compliance from the outside. Naming that residue is the more valuable half.
//
// THREE TIERS, kept apart because they carry different guarantees rather than
// because they are three lists:
//
//   1. Subject-keyed, and existing primitives already delete it.
//   2. Subject-keyed, and NOTHING deletes it today.
//   3. Not subject-keyed at all: a fact about the subject that a shared agent or the
//      tenant recorded. Reachable only through provenance, and only for rows that
//      carry it.

import (
	"context"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// erasureReport is the wire shape of GET /v1/_erasure.
type erasureReport struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`

	Tier1 erasureTier    `json:"tier1_covered"`
	Tier2 erasureTier    `json:"tier2_uncovered"`
	Tier3 erasureResidue `json:"tier3_residue"`

	// Notes carry limits a count cannot express, so the numbers are not mistaken
	// for the whole truth.
	Notes []string `json:"notes"`
	// Errors names planes that could not be read. A plane that FAILED is not a
	// plane that is empty, and a report that cannot tell them apart is unusable
	// for the question it exists to answer.
	Errors []string `json:"errors,omitempty"`
}

type erasureTier struct {
	Counts map[string]int64 `json:"counts"`
	Total  int64            `json:"total"`
}

// erasureResidue is what a subject-keyed delete cannot reach.
type erasureResidue struct {
	Rows int `json:"rows"`
	// Scopes names WHERE those rows are: "3 rows somewhere" is not actionable,
	// "3 rows in agent:curator" is.
	Scopes []string `json:"scopes"`
	// SessionsExamined is how many chats the walk traced from. A residue of 0
	// across 0 sessions means "nothing to trace from" — a different statement from
	// "nothing derives from them".
	SessionsExamined int `json:"sessions_examined"`
	// Truncated reports the provenance read hit its bound, making the count a floor.
	Truncated bool `json:"truncated"`
}

func (t *erasureTier) set(name string, n int64) {
	t.Counts[name] = n
	t.Total += n
}

// handleErasureReport serves GET /v1/_erasure?subject=…
//
// Admin-only via the /v1/_* catch-all, and correctly so: it enumerates one
// subject's footprint across every scope in the tenant, including scopes that
// subject cannot read. An operator's view of a person, not a person's view of
// themselves.
func (s *Server) handleErasureReport(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject",
			"pass ?subject=<user id> — the principal whose footprint to report")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "no store configured")
		return
	}
	ctx := r.Context()
	tenant, all := s.principalTenantScope(ctx, r.URL.Query().Get("tenant"))
	// An admin with no ?tenant= means "all tenants" everywhere else in this API,
	// and here that cannot be served: a subject id is only meaningful WITHIN a
	// tenant — the same string in two tenants is two different footprints, and
	// merging them would report one person's residue as another's.
	//
	// Refusing rather than defaulting, because the default is the trap: `all` also
	// carries tenant "", which every store read below would happily interpret as
	// the DEFAULT tenant and answer confidently about the wrong people.
	//
	// Has() rather than != "": it distinguishes an explicit `?tenant=` (the default
	// tenant, which is the whole deployment on a single-tenant install) from an
	// omitted one. Without that, a single-tenant admin could not ask at all.
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: a subject id is only unique within one. "+
				"Pass ?tenant=<id>, or ?tenant= for the default tenant.")
		return
	}

	rep := erasureReport{
		Tenant:  tenant,
		Subject: subject,
		Tier1:   erasureTier{Counts: map[string]int64{}},
		Tier2:   erasureTier{Counts: map[string]int64{}},
	}
	// fail records a plane it could not read. Every read below is best-effort so
	// one broken plane does not deny the whole report — but a silent skip would
	// UNDERCOUNT, which is the one failure mode this report must not have.
	fail := func(plane string, err error) {
		rep.Errors = append(rep.Errors, plane+": "+err.Error())
	}

	// sqlMemUnexamined records that SQL Memory is unconfigured, so the plane was
	// never looked at. Distinct from "looked and found nothing", and surfaced in
	// the notes below.
	var sqlMemUnexamined bool

	// ---- tier 1: subject-keyed, and already deletable ----
	sessions, err := s.subjectSessions(ctx, tenant, subject)
	if err != nil {
		fail("chats", err)
	}
	rep.Tier1.set("chats", int64(len(sessions)))

	if entries, _, err := s.store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0); err != nil {
		fail("memory", err)
	} else {
		rep.Tier1.set("memory_rows", int64(len(entries)))
	}
	if s.sqlMem != nil {
		// A user-scope SQL Memory database holds the entity graph and every document
		// this subject authored; DropScope removes it whole.
		if scopes, err := s.sqlMem.ListScopes(ctx); err != nil {
			fail("sql_memory", err)
		} else {
			// Counted into a local first and set UNCONDITIONALLY, including zero.
			// Setting it only on a hit would make the key's absence mean either "no
			// scope" or "never looked" — the same ambiguity between failed and empty
			// that `errors` exists to prevent, reintroduced one field lower down.
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
		// Deferred to a flag rather than appended here: rep.Notes is ASSIGNED (not
		// appended to) further down, so a note written now would be silently
		// discarded.
		sqlMemUnexamined = true
	}
	if rows, err := s.store.DirentListUnder(ctx, tenant, "user", subject, "/"); err != nil {
		fail("paths", err)
	} else {
		rep.Tier1.set("path_entries", int64(len(rows)))
	}

	// ---- tier 2: subject-keyed, and nothing deletes it ----
	//
	// Its own tier rather than folded in, because the difference is not how many
	// rows there are but whether anything can remove them. A subject's ENCRYPTED
	// CREDENTIALS surviving an "erasure" is the worst entry on this list, and it is
	// invisible unless something counts it.
	if creds, err := s.store.CredentialDefList(ctx, tenant, "user", subject); err != nil {
		fail("credentials", err)
	} else {
		rep.Tier2.set("credentials", int64(len(creds)))
	}
	if limits, err := s.store.TokenLimitsAll(ctx); err != nil {
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
	if ints, err := s.store.InterruptListByUser(ctx, subject, tenant, ""); err != nil {
		fail("interrupts", err)
	} else {
		rep.Tier2.set("interrupts", int64(len(ints)))
	}
	if agg, err := s.store.UsageReport(ctx, store.UsageQuery{
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

	// ---- tier 3: not subject-keyed at all ----
	rep.Tier3, err = s.erasureResidueFor(ctx, tenant, subject, sessions)
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
	// With no sessions there is nothing to trace from, so tier3's 0 means UNKNOWN,
	// not NONE — and the difference is the whole value of the number.
	//
	// This is the state a subject is left in AFTER an erasure, which is exactly
	// when someone is most likely to re-run the report and read 0 as confirmation
	// that nothing remains. Facts derived from those chats can still be sitting in
	// a shared agent's scope; deleting the chats destroyed the only index to them.
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
	writeJSON(w, http.StatusOK, rep)
}

// subjectSessions lists the subject's chats — the handle tier 3 is traced from.
func (s *Server) subjectSessions(ctx context.Context, tenant, subject string) ([]string, error) {
	var out []string
	const page = 500
	for offset := 0; ; offset += page {
		rows, _, err := s.store.ListSessions(ctx,
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

// erasureResidueFor walks the subject's chats to the facts derived from them —
// the only route to a fact about them living in a scope they do not own.
func (s *Server) erasureResidueFor(ctx context.Context, tenant, subject string, sessions []string) (erasureResidue, error) {
	res := erasureResidue{SessionsExamined: len(sessions), Scopes: []string{}}
	if len(sessions) == 0 {
		return res, nil
	}
	const bound = 5000
	rows, err := s.store.MemoryListBySourceSessions(ctx, tenant, sessions, bound)
	if err != nil {
		return res, err
	}
	seen := map[string]bool{}
	for _, row := range rows {
		// The subject's OWN user scope is tier 1 — already counted, already
		// deletable. Every other scope is residue, INCLUDING another user's scope:
		// a fact about this subject filed under a different person is exactly what
		// a subject-keyed delete misses, and is the case most worth surfacing.
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
	res.Truncated = len(rows) == bound
	return res, nil
}
