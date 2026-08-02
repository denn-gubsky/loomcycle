package http

// erasure_execute.go — the deletion half of RFC BL P5 0d.
//
// POST /v1/_erasure removes what the GET report calls tier 1 and tier 2, reports
// what it cannot reach, and defaults to doing nothing.
//
// THE RESIDUE REPORT IS ONE-SHOT, and this is the sharpest thing to understand
// about the whole operation.
//
// Tier 3 is reachable ONLY by tracing provenance from the subject's sessions. This
// call deletes those sessions. So the moment it succeeds, the trace handle is gone
// and no future query can find the residue again — a report run afterwards returns
// `residue: 0` while the facts are still sitting in a shared agent's scope. Not a
// bug to be fixed here: it is what "the subject's chats are deleted" MEANS when
// chats are the only index into derived facts.
//
// Two consequences the code owes the caller:
//
//   - The residue is measured BEFORE anything is deleted, and the session ids are
//     captured up front rather than re-derived, so the measurement is unaffected by
//     the deletions that follow.
//   - THE RESPONSE IS THE ONLY DURABLE RECORD OF THE RESIDUE. Said in `notes`,
//     because an operator who discards it has destroyed the sole evidence of what
//     the erasure did not reach.
//
// The GET report closes the matching lie from its side: with zero sessions to trace
// from it reports residue-undeterminable rather than residue-zero.
//
// Chats still go LAST, but for the weaker reason only — transcripts are the
// recovery record, so a crash mid-erasure should leave them standing.
//
// WHAT IT DELIBERATELY DOES NOT DELETE is as much the point as what it does. The
// usage/cost ledger is retained, not erased — those rows are accounting records an
// operator may be under a legal obligation to keep, and the right treatment is to
// break the personal linkage rather than destroy the totals. That is a merge, not a
// delete (usage_archive keys user_id inside its PRIMARY KEY, so anonymising means
// folding rows into the '' bucket and summing), and it deserves its own change
// rather than being swept in here. Reported under `retained` with the reason, so it
// is a stated decision rather than a silent gap.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

type erasureRequest struct {
	Subject string `json:"subject"`
	// DryRun defaults TRUE via the pointer: an omitted field must mean "do
	// nothing". A plain bool would make the zero value destructive, so a caller
	// who forgot the field — or sent {} — would erase.
	DryRun *bool `json:"dry_run"`
	// Confirm must equal Subject on a live run. A typo'd subject that matches
	// nothing is harmless; one that matches the WRONG person is not, and this is
	// the cheapest guard that catches it.
	Confirm string `json:"confirm"`
}

type erasureResult struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	DryRun  bool   `json:"dry_run"`

	// Deleted is per-plane counts — what went, or what WOULD go on a dry run.
	Deleted map[string]int `json:"deleted"`
	// Retained is plane → why it was kept. Never empty: the residue is always
	// reported, because an erasure that lists only its successes reads as
	// complete.
	Retained map[string]string `json:"retained"`
	// Residue is the tier-3 report, carried through unchanged.
	Residue erasureResidue `json:"residue"`

	Errors []string `json:"errors,omitempty"`
	Notes  []string `json:"notes"`
}

// handleErasureExecute serves POST /v1/_erasure.
func (s *Server) handleErasureExecute(w http.ResponseWriter, r *http.Request) {
	var req erasureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "decode body: "+err.Error())
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "no store configured")
		return
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	if !dryRun && req.Confirm != subject {
		writeJSONError(w, http.StatusBadRequest, "confirm_mismatch",
			"a live erasure requires confirm to equal subject — this is irreversible")
		return
	}

	ctx := r.Context()
	tenant, all := s.principalTenantScope(ctx, r.URL.Query().Get("tenant"))
	// Same refusal as the GET, and it matters more here: tenant "" would not merely
	// report the wrong people, it would DELETE them.
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: a subject id is only unique within one. "+
				"Pass ?tenant=<id>, or ?tenant= for the default tenant.")
		return
	}

	res := erasureResult{
		Tenant: tenant, Subject: subject, DryRun: dryRun,
		Deleted:  map[string]int{},
		Retained: map[string]string{},
	}
	// Every plane this operation CONSIDERS is registered at zero up front, so a
	// key's presence means "examined" and its value means "this many went".
	//
	// Without it the accumulating planes below (`++` from an absent key) simply
	// vanish when they delete nothing, and a caller cannot distinguish "no
	// credentials existed" from "credentials were never looked at" — which for the
	// tier-2 entry that matters most is the difference between a clean erasure and
	// one that left a subject's keys behind. sql_memory is registered only when the
	// subsystem exists, because there it really is unexamined.
	for _, plane := range []string{
		"chats", "memory_rows", "path_entries",
		"credentials", "token_limits", "interrupts",
	} {
		res.Deleted[plane] = 0
	}
	if s.sqlMem != nil {
		res.Deleted["sql_memory_scopes"] = 0
	}
	fail := func(plane string, err error) {
		res.Errors = append(res.Errors, plane+": "+err.Error())
	}

	// The session list is read FIRST and used LAST: it is both the tier-3 trace
	// handle and the final thing deleted.
	sessions, err := s.subjectSessions(ctx, tenant, subject)
	if err != nil {
		fail("chats", err)
	}

	// Measured BEFORE anything is deleted, from session ids already captured — see
	// the file header on why this number can never be recovered afterwards.
	res.Residue, err = s.erasureResidueFor(ctx, tenant, subject, sessions)
	if err != nil {
		fail("provenance", err)
	}

	// ---- tier 2 first: no other plane depends on it ----
	if creds, err := s.store.CredentialDefList(ctx, tenant, "user", subject); err != nil {
		fail("credentials", err)
	} else {
		for _, c := range creds {
			if dryRun {
				res.Deleted["credentials"]++
				continue
			}
			if ok, err := s.store.CredentialDefDelete(ctx, tenant, "user", subject, c.Name); err != nil {
				fail("credentials", err)
			} else if ok {
				res.Deleted["credentials"]++
			}
		}
	}
	if limits, err := s.store.TokenLimitsAll(ctx); err != nil {
		fail("token_limits", err)
	} else {
		for _, l := range limits {
			if l.TenantID != tenant || l.Scope != "user" || l.ScopeID != subject {
				continue
			}
			if dryRun {
				res.Deleted["token_limits"]++
				continue
			}
			if err := s.store.TokenLimitDelete(ctx, tenant, "user", subject); err != nil {
				fail("token_limits", err)
			} else {
				res.Deleted["token_limits"]++
			}
		}
	}
	if dryRun {
		if ints, err := s.store.InterruptListByUser(ctx, subject, tenant, ""); err != nil {
			fail("interrupts", err)
		} else {
			res.Deleted["interrupts"] = len(ints)
		}
	} else if n, err := s.store.InterruptDeleteAllByUser(ctx, subject, tenant); err != nil {
		fail("interrupts", err)
	} else {
		res.Deleted["interrupts"] = n
	}

	// ---- tier 1, chats excepted ----
	if dryRun {
		if entries, _, err := s.store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0); err != nil {
			fail("memory", err)
		} else {
			res.Deleted["memory_rows"] = len(entries)
		}
	} else if n, err := s.store.MemoryDeleteScope(ctx, tenant, store.MemoryScopeUser, subject); err != nil {
		fail("memory", err)
	} else {
		res.Deleted["memory_rows"] = n
	}

	if s.sqlMem != nil {
		key := sqlmem.ScopeKey{Tenant: tenant, Scope: "user", ScopeID: subject}
		if dryRun {
			if scopes, err := s.sqlMem.ListScopes(ctx); err != nil {
				fail("sql_memory", err)
			} else {
				for _, sk := range scopes {
					if sk.Scope == "user" && sk.ScopeID == subject {
						res.Deleted["sql_memory_scopes"] = 1
						break
					}
				}
			}
		} else if removed, err := s.sqlMem.DropScope(ctx, key); err != nil {
			fail("sql_memory", err)
		} else if removed {
			res.Deleted["sql_memory_scopes"] = 1
		}
	}

	if rows, err := s.store.DirentListUnder(ctx, tenant, "user", subject, "/"); err != nil {
		fail("paths", err)
	} else {
		for _, d := range rows {
			if dryRun {
				res.Deleted["path_entries"]++
				continue
			}
			if ok, err := s.store.DirentDelete(ctx, tenant, "user", subject, d.ParentPath, d.Name); err != nil {
				fail("paths", err)
			} else if ok {
				res.Deleted["path_entries"]++
			}
		}
	}

	// ---- chats LAST — see the file header ----
	if dryRun {
		res.Deleted["chats"] = len(sessions)
	} else {
		for _, id := range sessions {
			if err := s.store.DeleteSessionCascade(ctx, id); err != nil {
				fail("chats", err)
				continue
			}
			res.Deleted["chats"]++
		}
	}

	// ---- what was deliberately kept ----
	res.Retained["usage_ledger"] = "retained by design: cost rows are accounting records an " +
		"operator may be legally required to keep. The correct treatment is to break the personal " +
		"linkage rather than destroy the totals, which is a row merge (usage_archive keys user_id " +
		"in its primary key) and is not implemented yet."
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
	if dryRun {
		res.Notes = append(res.Notes,
			"DRY RUN — nothing was deleted. Re-send with dry_run:false and confirm:<subject> to execute.")
	}
	if len(res.Errors) > 0 {
		res.Notes = append(res.Notes,
			"one or more planes faulted (see errors). This erasure is INCOMPLETE — re-run it; "+
				"the operation is idempotent.")
	}
	writeJSON(w, http.StatusOK, res)
}
