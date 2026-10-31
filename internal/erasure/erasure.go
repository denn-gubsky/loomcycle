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
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ErrConfirmMismatch is returned when a live erasure's confirm does not equal
// the subject. Sentinel so each transport can map it to its own status code
// (HTTP 400 / gRPC InvalidArgument) without string-matching.
var ErrConfirmMismatch = errors.New(
	"a live erasure requires confirm to equal subject — this is irreversible")

// ErrAuditUnavailable is returned when a live erasure's intent record cannot
// be written, so the erasure is refused before anything is deleted. Sentinel
// so each transport maps it to its own status (HTTP 503 / gRPC Unavailable).
var ErrAuditUnavailable = errors.New("refusing to erase without an audit record")

// ErrNoSubject is returned when subject is empty.
var ErrNoSubject = errors.New("subject is required")

// Service performs erasure reads and deletes. SqlMem may be nil — the report
// then declares that plane UNEXAMINED rather than omitting it, because with the
// subsystem absent it genuinely was not looked at.
type Service struct {
	Store  store.Store
	SqlMem *sqlmem.Manager
	// ACLStatic returns the config-declared access control (yaml `groups:` /
	// `acl_bindings:`). Those rows live in a file, not the store, so erasure
	// can only NAME them for the operator to remove. nil = none declared.
	ACLStatic func() *authz.Static
	// ACLChanged runs after a live erasure removed access-control rows. The
	// transport owns what that means — dropping the compiled sets cached on
	// every replica and recording the write in the ACL audit trail, exactly as
	// the management writes do — because the cache and the backplane are its.
	// Without it a replica keeps serving the erased subject's grants until its
	// cache TTL lapses. nil = nothing cached.
	ACLChanged func(ctx context.Context, tenant, subject string)
	// ListTokens returns the subject's bearer tokens in the tenant: the ones
	// an erasure revokes (active or already retired), and the ones it leaves.
	// RetireTokens retires the active revocable ones and stops them
	// authenticating on every replica, returning those it retired (also on
	// error). The transport owns both: which tokens are the subject's to lose
	// is the member-token rule its token surfaces share, and the auth cache
	// and backplane are its. Either nil = the token plane is not examined.
	ListTokens   func(ctx context.Context, tenant, subject string) (revocable, kept []store.OperatorTokenDefRow, err error)
	RetireTokens func(ctx context.Context, tenant, subject string) ([]store.OperatorTokenDefRow, error)
	// BoundSubjects lists the tenant's subjects (documents in the tenant scope
	// whose root carries `fields.principal`) bound to the subject.
	// ClearBoundSubjects removes those bindings and returns the subjects it
	// cleared (also on error). AnnounceErased publishes `erased` on
	// memory/subjects for them, once the erasure is done, and returns why it
	// did not ("" when it did). The transport owns all three: the document
	// store and the channel bus are its, and the caller's read-access rules
	// decide whether the event may name a subject. Any nil = the plane is not
	// examined.
	BoundSubjects      func(ctx context.Context, tenant, subject string) ([]BoundSubject, error)
	ClearBoundSubjects func(ctx context.Context, tenant, subject string) ([]BoundSubject, error)
	AnnounceErased     func(ctx context.Context, tenant string, subjects []BoundSubject) string
}

// BoundSubject is one tenant subject bound to the erased principal. An erasure
// clears the binding and keeps the subject: deleting it deletes every
// statement and fact the tenant holds about that entity, which is the
// operator's call, not a mechanical erasure.
type BoundSubject struct {
	DocumentID string `json:"document_id"`
	NaturalKey string `json:"natural_key"`
	Path       string `json:"path,omitempty"`
	// RootChunkID is what the announcement names; not part of the report.
	RootChunkID string `json:"-"`
	// Readable is false when the caller's access-control rules hide the
	// subject's document from reading: it is then counted, never named.
	Readable bool `json:"-"`
}

// subjectBindingsWired reports whether the transport wired the bound-subject plane.
func (s *Service) subjectBindingsWired() bool {
	return s.BoundSubjects != nil && s.ClearBoundSubjects != nil && s.AnnounceErased != nil
}

// nameBound splits subjects into the ones the caller may see named and a count
// of the rest. The slice is never nil, so the wire carries [] rather than null.
func nameBound(subjects []BoundSubject) ([]BoundSubject, int) {
	named := []BoundSubject{}
	var withheld int
	for _, b := range subjects {
		if b.Readable {
			named = append(named, b)
		} else {
			withheld++
		}
	}
	return named, withheld
}

// retainedBoundSubjects is the Retained entry for bound subjects. It counts and
// never names them: Retained is copied into the audit record, and the audit
// log must not carry the subjects the erased person was bound to.
func retainedBoundSubjects(n int) string {
	return fmt.Sprintf("%d subject(s) in the tenant's documents were bound to this principal. The "+
		"binding is cleared (subject_bindings) and the subjects are kept, listed in bound_subjects: an "+
		"erasure never deletes a subject, because that deletes every statement and fact the tenant holds "+
		"about it. Deleting one is the operator's call — delete_document on it, which announces the "+
		"removal on memory/subjects.", n)
}

// subjectTokens is the subject's bearer-token footprint in one tenant.
type subjectTokens struct {
	active  []store.OperatorTokenDefRow // revocable and still authenticating
	retired []string                    // def ids of revocable tokens already retired or expired
	kept    []store.OperatorTokenDefRow // not revocable and still authenticating
}

// allIDs is every revocable def id, active or not: the bindings keyed to
// them are what an erasure removes.
func (t subjectTokens) allIDs() []string {
	return append(append([]string{}, t.retired...), tokenIDs(t.active)...)
}

// keptNote names the tokens an erasure leaves; "" when there are none.
func (t subjectTokens) keptNote() string {
	if len(t.kept) == 0 {
		return ""
	}
	return fmt.Sprintf("%d token(s) bound to the subject hold an operator scope (substrate:tenant or "+
		"substrate:admin) and are not revoked by an erasure: def id(s) [%s]. A role is not the member's "+
		"credential; retire them with the operator-token surface (operatortokendef op=retire).",
		len(t.kept), strings.Join(tokenIDs(t.kept), ", "))
}

func tokenIDs(rows []store.OperatorTokenDefRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.DefID)
	}
	return out
}

func tokenRefs(rows []store.OperatorTokenDefRow) []TokenRef {
	out := make([]TokenRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, TokenRef{DefID: r.DefID, Name: r.Name})
	}
	return out
}

// tokens reads the subject's tokens; examined is false when the transport
// wired no token plane.
func (s *Service) tokens(ctx context.Context, tenant, subject string) (out subjectTokens, examined bool, err error) {
	if s.ListTokens == nil || s.RetireTokens == nil {
		return out, false, nil
	}
	revocable, kept, err := s.ListTokens(ctx, tenant, subject)
	if err != nil {
		return out, true, err
	}
	now := time.Now()
	for _, r := range revocable {
		if r.ValidAt(now) {
			out.active = append(out.active, r)
		} else {
			out.retired = append(out.retired, r.DefID)
		}
	}
	for _, r := range kept {
		if r.ValidAt(now) {
			out.kept = append(out.kept, r)
		}
	}
	return out, true, nil
}

// subjectACL is a subject's RFC EY access-control footprint in one tenant.
type subjectACL struct {
	groups   []string // stored direct memberships (member kind user)
	bindings int      // stored user-kind binding rows, retired ones included
	lists    []string // the distinct lists those bindings name
	// The token-kind bindings, retired ones included, of the subject's
	// revocable tokens, and the distinct lists they name.
	tokenBindings int
	tokenLists    []string
	// Config-declared: not erasable from the store.
	cfgGroups []string
	cfgLists  []string
}

// accessControl reads the subject's memberships and bindings. Retired
// bindings count: a retired row still names the subject, and the erasure
// deletes it. Group- and token-kind rows are not the subject's even when
// their id equals it, so the filter is on kind as well as id; the token-kind
// rows counted are those of tokenIDs, the subject's revocable tokens.
func (s *Service) accessControl(ctx context.Context, tenant, subject string, tokenIDs []string) (subjectACL, error) {
	var out subjectACL
	groups, err := s.Store.GroupsOfSubject(ctx, tenant, subject)
	if err != nil {
		return out, err
	}
	out.groups = sortedUnique(groups)
	rows, err := s.Store.AclBindingList(ctx, tenant, true)
	if err != nil {
		return out, err
	}
	tokens := map[string]bool{}
	for _, id := range tokenIDs {
		tokens[id] = true
	}
	var lists, tokenLists []string
	for _, r := range rows {
		switch {
		case r.SubjectKind == string(authz.SubjectUser) && r.SubjectID == subject:
			out.bindings++
			lists = append(lists, r.ACLName)
		case r.SubjectKind == string(authz.SubjectToken) && tokens[r.SubjectID]:
			out.tokenBindings++
			tokenLists = append(tokenLists, r.ACLName)
		}
	}
	out.lists = sortedUnique(lists)
	out.tokenLists = sortedUnique(tokenLists)
	if s.ACLStatic != nil {
		static := s.ACLStatic()
		direct, _ := static.GroupsOf(tenant, subject)
		out.cfgGroups = sortedUnique(direct)
		var cfgLists []string
		for _, b := range static.BindingsIn(tenant) {
			if b.SubjectKind == authz.SubjectUser && b.SubjectID == subject {
				cfgLists = append(cfgLists, b.ACL)
			}
		}
		out.cfgLists = sortedUnique(cfgLists)
	}
	return out, nil
}

// storedNote names the stored rows; "" when there are none.
func (a subjectACL) storedNote() string {
	if len(a.groups) == 0 && a.bindings == 0 && a.tokenBindings == 0 {
		return ""
	}
	return fmt.Sprintf("access control (stored, removed by an erasure): member of group(s) [%s]; "+
		"%d binding row(s) to the subject (retired ones included) naming list(s) [%s]; "+
		"%d binding row(s) to its tokens naming list(s) [%s]. "+
		"The groups and lists themselves are not the subject's and stay.",
		strings.Join(a.groups, ", "), a.bindings, strings.Join(a.lists, ", "),
		a.tokenBindings, strings.Join(a.tokenLists, ", "))
}

// allLists is the distinct lists named by both kinds of binding.
func (a subjectACL) allLists() []string {
	return sortedUnique(append(append([]string{}, a.lists...), a.tokenLists...))
}

// configNote names the config-declared rows; "" when there are none.
func (a subjectACL) configNote() string {
	if len(a.cfgGroups) == 0 && len(a.cfgLists) == 0 {
		return ""
	}
	return fmt.Sprintf("declared in config; remove from the config file: the subject is a member of "+
		"group(s) [%s] and bound to list(s) [%s] in the yaml `groups:` / `acl_bindings:` blocks. "+
		"An erasure cannot remove them, and they apply again to a re-created user of the same id.",
		strings.Join(a.cfgGroups, ", "), strings.Join(a.cfgLists, ", "))
}

func sortedUnique(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// provenanceBound caps the residue walk. A hit sets Residue.Truncated, making
// the count a floor rather than a total.
const provenanceBound = 5000

// Report is the wire shape shared by every transport.
type Report struct {
	Tenant  string  `json:"tenant"`
	Subject string  `json:"subject"`
	Tier1   Tier    `json:"tier1_covered"`
	Tier2   Tier    `json:"tier2_uncovered"`
	Tier3   Residue `json:"tier3_residue"`
	Access  Access  `json:"access"`
	// BoundSubjects are the tenant's subjects bound to this principal, which
	// an erasure unbinds and keeps; BoundSubjectsWithheld counts the ones the
	// caller's rules hide from reading, which are not named.
	BoundSubjects         []BoundSubject `json:"bound_subjects"`
	BoundSubjectsWithheld int            `json:"bound_subjects_withheld,omitempty"`
	Notes                 []string       `json:"notes"`
	// Errors names planes that could not be READ. A plane that failed is not a
	// plane that is empty, and a report unable to distinguish them cannot answer
	// the question it exists for.
	Errors []string `json:"errors,omitempty"`
}

// Access names, rather than counts, what an erasure removes from the
// subject's authority — or, in a report or a dry run, what it would: the
// groups the subject leaves and the lists its bindings named. The notes say
// the same in prose; this is the form a client can act on. Config-declared
// rows are not the store's to remove and are not listed here (see Retained).
// The slices are never nil, so the wire carries [] rather than null.
type Access struct {
	Groups []string `json:"groups"`
	Lists  []string `json:"lists"`
	// Tokens are the subject's bearer tokens revoked (or that would be).
	Tokens []TokenRef `json:"tokens"`
}

// TokenRef names one bearer token by its def id and name. Never the bearer:
// the plaintext is not stored, and its hash is not a name.
type TokenRef struct {
	DefID string `json:"def_id"`
	Name  string `json:"name"`
}

func emptyAccess() Access { return Access{Groups: []string{}, Lists: []string{}, Tokens: []TokenRef{}} }

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
	// SourceAddr is the caller's network address, for the audit record. Set
	// by a transport that knows it; never read from a request body.
	SourceAddr string
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
	Access   Access            `json:"access"`
	// BoundSubjects are the subjects unbound (or, in a dry run, that would be),
	// named as in Report.
	BoundSubjects         []BoundSubject `json:"bound_subjects"`
	BoundSubjectsWithheld int            `json:"bound_subjects_withheld,omitempty"`
	// NotAnnounced says why `erased` was not published for the unbound
	// subjects; empty when it was, or when there was nothing to announce.
	NotAnnounced string   `json:"not_announced,omitempty"`
	Errors       []string `json:"errors,omitempty"`
	Notes        []string `json:"notes"`
}

const retainedTokenRows = "a revoked token is RETIRED, not deleted: its row (def id, name, " +
	"subject, scopes — never the bearer) stays as the token registry and audit record, and stops " +
	"authenticating at once. A row with an expiry is pruned after it; one without stays."

const retainedUsageLedger = "retained by design: cost rows are accounting records an " +
	"operator may be legally required to keep. The correct treatment is to break the personal " +
	"linkage rather than destroy the totals, which is a row merge (usage_archive keys user_id " +
	"in its primary key) and is not implemented yet."

// sqlScopeTenant maps the raw principal tenant onto SQL Memory's spelling.
//
// The planes canonicalize differently and this catches out every caller that
// touches both: SQL Memory REJECTS an empty tenant and stores the default one as
// "default" (it sanitizes the value into a schema name and a LOGIN role), while
// the k/v Memory plane and the Path tree key on the RAW tenant and leave it "".
// Same rule as builtin.sqlScopeTenant, which is the source of truth.
//
// Getting this wrong is not cosmetic here: DropScope built from a raw "" tenant
// silently matches nothing, so a single-tenant deployment's erasure would leave
// the subject's ENTIRE SQL Memory database — documents, entity graph — in place
// while reporting success.
// sqlScopeTenant delegates to sqlmem.ScopeTenant, which owns this rule.
// Kept as a local shim so the many call sites below read unchanged; the
// mapping itself is no longer duplicated here.
func sqlScopeTenant(tenant string) string { return sqlmem.ScopeTenant(tenant) }

// userSQLScopeExists reports whether the subject owns a user-scope SQL Memory
// database IN THIS TENANT.
//
// ListScopes spans every tenant, so the tenant comparison is load-bearing rather
// than defensive: without it, tenant A asking about "alice" gets a hit when
// tenant B has a user named "alice" — a cross-tenant existence oracle, and a dry
// run that promises to delete something the live path then cannot find.
func (s *Service) userSQLScopeExists(ctx context.Context, tenant, subject string) (bool, error) {
	scopes, err := s.SqlMem.ListScopes(ctx)
	if err != nil {
		return false, err
	}
	want := sqlScopeTenant(tenant)
	for _, sk := range scopes {
		if sk.Tenant == want && sk.Scope == "user" && sk.ScopeID == subject {
			return true, nil
		}
	}
	return false, nil
}

// Report enumerates one subject's footprint. Read-only.
func (s *Service) Report(ctx context.Context, tenant, subject string) (Report, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Report{}, ErrNoSubject
	}
	rep := Report{
		Tenant:        tenant,
		Subject:       subject,
		Tier1:         Tier{Counts: map[string]int64{}},
		Tier2:         Tier{Counts: map[string]int64{}},
		Access:        emptyAccess(),
		BoundSubjects: []BoundSubject{},
	}
	fail := func(plane string, err error) { rep.Errors = append(rep.Errors, plane+": "+err.Error()) }

	sessions, err := s.subjectSessions(ctx, tenant, subject)
	if err != nil {
		fail("chats", err)
	}
	rep.Tier1.set("chats", int64(len(sessions)))

	// COUNTED, not listed: a listing is capped (limit 0 reads as 100) and skips the
	// expired and superseded rows the erasure still deletes, so it under-reported
	// every subject with document bodies or indexed turns in their scope.
	if n, err := s.Store.MemoryCountScope(ctx, tenant, store.MemoryScopeUser, subject); err != nil {
		fail("memory", err)
	} else {
		rep.Tier1.set("memory_rows", int64(n))
	}

	var sqlMemUnexamined bool
	if s.SqlMem != nil {
		// Set UNCONDITIONALLY, including zero: setting only on a hit would make
		// absence mean either "no scope" or "never looked" — the failed-vs-empty
		// ambiguity `Errors` exists to prevent, reintroduced one field lower down.
		if found, err := s.userSQLScopeExists(ctx, tenant, subject); err != nil {
			fail("sql_memory", err)
		} else if found {
			rep.Tier1.set("sql_memory_scopes", 1)
		} else {
			rep.Tier1.set("sql_memory_scopes", 0)
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
	var aclNotes []string
	tk, tokensExamined, err := s.tokens(ctx, tenant, subject)
	if err != nil {
		fail("operator_tokens", err)
	} else if tokensExamined {
		rep.Tier2.set("operator_tokens", int64(len(tk.active)))
		rep.Access.Tokens = tokenRefs(tk.active)
		if n := tk.keptNote(); n != "" {
			aclNotes = append(aclNotes, n)
		}
	}
	if acl, err := s.accessControl(ctx, tenant, subject, tk.allIDs()); err != nil {
		fail("access_control", err)
	} else {
		rep.Tier2.set("acl_memberships", int64(len(acl.groups)))
		rep.Tier2.set("acl_bindings", int64(acl.bindings))
		rep.Tier2.set("acl_token_bindings", int64(acl.tokenBindings))
		rep.Access.Groups, rep.Access.Lists = acl.groups, acl.allLists()
		for _, n := range []string{acl.storedNote(), acl.configNote()} {
			if n != "" {
				aclNotes = append(aclNotes, n)
			}
		}
	}
	if s.subjectBindingsWired() {
		if bound, err := s.BoundSubjects(ctx, tenant, subject); err != nil {
			fail("subject_bindings", err)
		} else {
			rep.Tier2.set("subject_bindings", int64(len(bound)))
			rep.BoundSubjects, rep.BoundSubjectsWithheld = nameBound(bound)
			if len(bound) > 0 {
				aclNotes = append(aclNotes, retainedBoundSubjects(len(bound)))
			}
		}
	}
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
	// Month-to-date usage a snapshot restore carried in for this subject.
	if n, err := s.Store.UsageCarryCountSubject(ctx, tenant, subject); err != nil {
		fail("usage_carry", err)
	} else {
		rep.Tier2.set("usage_carry", int64(n))
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
	rep.Notes = append(rep.Notes, aclNotes...)
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
// Validate applies the guards that decide whether an erasure may proceed, WITHOUT
// touching anything.
//
// Extracted from Execute so a caller can ask "would this be accepted?" before doing
// something irreversible on the strength of it — the HTTP layer writes an audit record
// between the two, and a record written for a request that was about to be rejected
// would be noise in the one log an auditor needs to trust. Execute still calls this, so
// the rule has one home and every transport inherits it.
func (s *Service) Validate(req ExecuteRequest) error {
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return ErrNoSubject
	}
	// A subject id matching nothing is harmless; one matching the wrong person is
	// irreversible.
	if !req.DryRun && req.Confirm != subject {
		return ErrConfirmMismatch
	}
	return nil
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (Result, error) {
	if err := s.Validate(req); err != nil {
		return Result{}, err
	}
	subject := strings.TrimSpace(req.Subject)
	tenant := req.Tenant
	res := Result{
		Tenant: tenant, Subject: subject, DryRun: req.DryRun,
		Deleted: map[string]int{}, Retained: map[string]string{},
		Access: emptyAccess(), BoundSubjects: []BoundSubject{},
	}
	for _, plane := range []string{
		"chats", "memory_rows", "path_entries",
		"acl_memberships", "acl_bindings", "acl_token_bindings",
		"credentials", "token_limits", "usage_carry", "interrupts",
	} {
		res.Deleted[plane] = 0
	}
	if s.SqlMem != nil {
		res.Deleted["sql_memory_scopes"] = 0
	}
	if s.ListTokens != nil && s.RetireTokens != nil {
		res.Deleted["operator_tokens"] = 0
	}
	if s.subjectBindingsWired() {
		res.Deleted["subject_bindings"] = 0
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
	// Authority first, tokens before access control: it is the subject's
	// authority rather than its data, and an erasure that faults part-way
	// should not leave a working bearer or grants standing. A revoked token's
	// bindings go in the access-control step below.
	var bindingTokens []string // def ids whose token-kind bindings go
	tk, tokensExamined, err := s.tokens(ctx, tenant, subject)
	if err != nil {
		fail("operator_tokens", err)
	} else if tokensExamined {
		if n := tk.keptNote(); n != "" {
			res.Retained["operator_tokens_privileged"] = n
		}
		if len(tk.active)+len(tk.retired) > 0 {
			res.Retained["operator_token_rows"] = retainedTokenRows
		}
		if req.DryRun {
			res.Deleted["operator_tokens"] = len(tk.active)
			res.Access.Tokens = tokenRefs(tk.active)
			bindingTokens = tk.allIDs()
		} else {
			retired, err := s.RetireTokens(ctx, tenant, subject)
			if err != nil {
				fail("operator_tokens", err)
			}
			res.Deleted["operator_tokens"] = len(retired)
			res.Access.Tokens = tokenRefs(retired)
			// Only tokens that no longer authenticate lose their bindings: one a
			// fault left working keeps the authority it has until a re-run.
			bindingTokens = append(append([]string{}, tk.retired...), tokenIDs(retired)...)
		}
	}
	if acl, err := s.accessControl(ctx, tenant, subject, bindingTokens); err != nil {
		fail("access_control", err)
	} else {
		if n := acl.configNote(); n != "" {
			res.Retained["access_control_config"] = n
		}
		if req.DryRun {
			res.Deleted["acl_memberships"] = len(acl.groups)
			res.Deleted["acl_bindings"] = acl.bindings
			res.Deleted["acl_token_bindings"] = acl.tokenBindings
			res.Access.Groups, res.Access.Lists = acl.groups, acl.allLists()
		} else {
			var changed int
			var lists []string
			if m, b, err := s.Store.AclEraseSubject(ctx, tenant, subject); err != nil {
				fail("access_control", err)
			} else {
				res.Deleted["acl_memberships"], res.Deleted["acl_bindings"] = m, b
				// Named from the read just before the delete, and only once it
				// succeeded: a failed delete removed nothing and must not claim to.
				res.Access.Groups = acl.groups
				lists = append(lists, acl.lists...)
				changed += m + b
			}
			if n, err := s.Store.AclEraseTokenBindings(ctx, tenant, bindingTokens); err != nil {
				fail("access_control", err)
			} else {
				res.Deleted["acl_token_bindings"] = n
				lists = append(lists, acl.tokenLists...)
				changed += n
			}
			res.Access.Lists = sortedUnique(lists)
			if changed > 0 && s.ACLChanged != nil {
				s.ACLChanged(ctx, tenant, subject)
			}
		}
	}
	// The subjects bound to the principal: unbound, so no row keeps the erased
	// id, and never deleted. Announced after everything else, below.
	var unbound []BoundSubject
	if s.subjectBindingsWired() {
		var bound []BoundSubject
		var err error
		if req.DryRun {
			bound, err = s.BoundSubjects(ctx, tenant, subject)
		} else {
			bound, err = s.ClearBoundSubjects(ctx, tenant, subject)
			unbound = bound
		}
		if err != nil {
			fail("subject_bindings", err)
		}
		res.Deleted["subject_bindings"] = len(bound)
		res.BoundSubjects, res.BoundSubjectsWithheld = nameBound(bound)
		if len(bound) > 0 {
			res.Retained["bound_subjects"] = retainedBoundSubjects(len(bound))
		}
	}
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
	// The subject's carried month-to-date usage: keyed by (tenant, user) like a
	// user budget, and it must go with the subject rather than keep counting
	// against a re-created user of the same name.
	if req.DryRun {
		if n, err := s.Store.UsageCarryCountSubject(ctx, tenant, subject); err != nil {
			fail("usage_carry", err)
		} else {
			res.Deleted["usage_carry"] = n
		}
	} else if n, err := s.Store.UsageCarryDeleteSubject(ctx, tenant, subject); err != nil {
		fail("usage_carry", err)
	} else {
		res.Deleted["usage_carry"] = n
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
		// The same predicate MemoryDeleteScope uses below, so the preview is exactly
		// what the erasure removes.
		if n, err := s.Store.MemoryCountScope(ctx, tenant, store.MemoryScopeUser, subject); err != nil {
			fail("memory", err)
		} else {
			res.Deleted["memory_rows"] = n
		}
	} else if n, err := s.Store.MemoryDeleteScope(ctx, tenant, store.MemoryScopeUser, subject); err != nil {
		fail("memory", err)
	} else {
		res.Deleted["memory_rows"] = n
	}

	if s.SqlMem != nil {
		// sqlScopeTenant, not the raw tenant: see the helper. A raw "" here would
		// build a key that matches nothing and silently spare the whole database.
		key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(tenant), Scope: "user", ScopeID: subject}
		if req.DryRun {
			if found, err := s.userSQLScopeExists(ctx, tenant, subject); err != nil {
				fail("sql_memory", err)
			} else if found {
				res.Deleted["sql_memory_scopes"] = 1
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

	// After every plane, so the event follows the erasure rather than racing it:
	// a consumer re-checks the subject against what is left.
	if len(unbound) > 0 {
		res.NotAnnounced = s.AnnounceErased(ctx, tenant, unbound)
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
		// A draft's session is not listed as a chat, and an archived chat is
		// hidden from the default listing, but both hold the subject's
		// conversation: erasure must find them.
		rows, _, err := s.Store.ListSessions(ctx,
			store.SessionFilter{TenantID: tenant, UserID: subject, IncludeConfigured: true, IncludeArchived: true}, page, offset)
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
