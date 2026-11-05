package audit

// The store-backed audit stream (RFC FZ Part E).
//
// Every replica appends to one hash chain per tenant in the store:
//
//	hash = hex(SHA-256(prev_hash || canonical(row)))
//
// where canonical(row) is the JSON array [version, tenant, seq, ts_ns, actor,
// action, subject, payload] and prev_hash is the previous row's hash ("" for a
// chain's first row). The hash alone shows that rows were changed, removed or
// reordered — but anyone who can write the database could rewrite a row and
// recompute every hash after it. So each link is also signed:
//
//	mac = hex(HMAC-SHA256(K, ["row", tenant, seq, hash]))
//
// with K derived from LOOMCYCLE_SECRET_KEY (credential.DeriveAuditChainKey).
// The "segment" an HMAC covers is one row; since the hash commits to every
// row before it, a valid MAC on row n vouches for rows 1..n. The tenant's head
// (["head", tenant, seq, hash]) and the anchor a retention prune leaves
// (["anchor", tenant, seq, hash]) are signed in their own domains, so neither
// a row's MAC nor the head's can be replayed as an anchor to pass off deleted
// rows as pruned ones.
//
// A chain that is deleted outright (head and rows) would otherwise read as a
// tenant with no history, so a tenant chain's first append also writes a
// signed ActionChainOpened row to the operator chain "" in the same
// transaction. Verification of a tenant first verifies the operator chain —
// a registry read from an unverified chain could be forged — and then breaks
// when the operator chain records an opening but the chain has no head
// ("disappeared") or an empty one, records two
// (deleted and begun again), or records none while the operator chain is
// unpruned (the operator chain itself deleted). A head missing or reset while
// rows remain is a break too: rows past the head are reported.
//
// What it cannot show without an outside witness: a database writer who
// deletes EVERY chain — the operator chain and all tenant chains — leaves an
// empty audit store that verifies as empty; and one who rolls the tables back
// to an earlier, genuinely signed state (dropping the newest rows AND
// restoring an older head). A tenant chain deleted after the operator chain's
// retention pruned its opening is likewise undetectable. The JSONL mirror
// (LOOMCYCLE_AUDIT_LOG_PATH) and exports taken with GET /v1/_audit are that
// witness.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// chainVersion is the first element of every canonical row. Changing it, or
// the element order, makes every stored row fail verification.
const chainVersion = "loomcycle-audit-v1"

// MAC domains.
const (
	macRow    = "row"
	macHead   = "head"
	macAnchor = "anchor"
)

// ChainKeys signs and checks the chain. Current signs; a row signed under
// Previous (LOOMCYCLE_SECRET_KEY_PREVIOUS, during a key rotation) still
// verifies.
type ChainKeys struct {
	Current  []byte
	Previous []byte
}

// canonical is the hashed encoding of a row's content. A JSON array of
// strings and integers has exactly one encoding, and no field can run into
// the next.
func canonical(r store.AuditEventRow) []byte {
	b, _ := json.Marshal([]any{chainVersion, r.TenantID, r.Seq, r.TS.UnixNano(), r.Actor, r.Action, r.Subject, r.Payload})
	return b
}

// RowHash is H(prev_hash ‖ canonical row). prevHash is fixed-length hex or
// empty, and canonical starts with '[', so the concatenation is unambiguous.
func RowHash(prevHash string, r store.AuditEventRow) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(canonical(r))
	return hex.EncodeToString(h.Sum(nil))
}

func chainMAC(key []byte, domain, tenant string, seq int64, hash string) string {
	m := hmac.New(sha256.New, key)
	b, _ := json.Marshal([]any{domain, tenant, seq, hash})
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}

func (k ChainKeys) sign(domain, tenant string, seq int64, hash string) string {
	return chainMAC(k.Current, domain, tenant, seq, hash)
}

// check reports whether mac signs (domain, tenant, seq, hash) under the
// current or the previous key, comparing in constant time.
func (k ChainKeys) check(mac, domain, tenant string, seq int64, hash string) bool {
	for _, key := range [][]byte{k.Current, k.Previous} {
		if len(key) == 0 {
			continue
		}
		if hmac.Equal([]byte(mac), []byte(chainMAC(key, domain, tenant, seq, hash))) {
			return true
		}
	}
	return false
}

// Seal returns the store.AuditSealFunc that hashes and signs a row whose Seq
// and PrevHash the store set from the locked head.
func (k ChainKeys) Seal() store.AuditSealFunc {
	return func(r *store.AuditEventRow) (string, error) {
		if len(k.Current) == 0 {
			return "", errors.New("no audit chain key")
		}
		r.Hash = RowHash(r.PrevHash, *r)
		r.MAC = k.sign(macRow, r.TenantID, r.Seq, r.Hash)
		return k.sign(macHead, r.TenantID, r.Seq, r.Hash), nil
	}
}

// SealAnchor returns the store.AuditAnchorSealFunc a prune signs its anchor with.
func (k ChainKeys) SealAnchor() store.AuditAnchorSealFunc {
	return func(tenant string, seq int64, hash string) string {
		return k.sign(macAnchor, tenant, seq, hash)
	}
}

// ChainReader is what verification reads.
type ChainReader interface {
	AuditHeadGet(ctx context.Context, tenantID string) (store.AuditHead, error)
	AuditEventsList(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]store.AuditEventRow, error)
}

// VerifyReport is the outcome of walking one tenant's chain.
type VerifyReport struct {
	Tenant    string `json:"tenant"`
	OK        bool   `json:"ok"`
	Rows      int64  `json:"rows"`
	AnchorSeq int64  `json:"anchor_seq"`
	HeadSeq   int64  `json:"head_seq"`
	// BreakSeq is the first position that does not verify (0 when OK), and
	// Reason says why.
	BreakSeq int64  `json:"break_seq,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (r VerifyReport) broken(seq int64, format string, args ...any) VerifyReport {
	r.OK, r.BreakSeq, r.Reason = false, seq, fmt.Sprintf(format, args...)
	return r
}

// verifyPage is how many rows verification reads at a time.
const verifyPage = 500

// ActionChainOpened is the operator-chain record of a tenant chain's first
// append, written in the same transaction (store.AuditStore.AuditAppend).
// Its Subject is the tenant.
const ActionChainOpened = "audit_chain_opened"

// Registry is what the operator chain says about the tenant chains: how
// many times each was opened, and whether the operator chain has been
// pruned (an opening older than the prune is no longer there to count). It
// is read only from an operator chain that verified, up to its verified head,
// so a forged or deleted opening, or a forged anchor, breaks the operator
// chain instead of steering a tenant's verification.
type Registry struct {
	Openings map[string]int
	Pruned   bool
}

// ReadRegistry verifies the operator chain and, when it holds, reads the
// registry from the rows it verified.
func ReadRegistry(ctx context.Context, r ChainReader, keys ChainKeys) (Registry, VerifyReport, error) {
	reg := Registry{Openings: map[string]int{}}
	op, err := verifyChain(ctx, r, keys, "", Registry{})
	if err != nil || !op.OK {
		return reg, op, err
	}
	reg.Pruned = op.AnchorSeq > 0
	after := op.AnchorSeq
	for after < op.HeadSeq {
		rows, err := r.AuditEventsList(ctx, "", after, verifyPage)
		if err != nil {
			return reg, op, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if row.Seq > op.HeadSeq {
				break
			}
			if row.Action == ActionChainOpened {
				reg.Openings[row.Subject]++
			}
			after = row.Seq
		}
	}
	return reg, op, nil
}

// operatorChainBroken is a tenant's report when the operator chain, which
// vouches for the tenant chain's existence, does not verify.
func operatorChainBroken(tenant string, op VerifyReport) VerifyReport {
	rep := VerifyReport{Tenant: tenant}
	return rep.broken(1, "the operator chain does not verify (row %d: %s), so this chain's registration cannot be checked",
		op.BreakSeq, op.Reason)
}

// Verify walks a tenant's chain from its anchor to its head and reports the
// first link that does not hold. An error is a failure to read, never a
// break. The head is read first and rows past it are not walked, so appends
// made during the walk do not read as a break; a prune made during it can,
// and a re-run then passes.
//
// For a tenant chain it first verifies the operator chain and asks it
// whether the tenant's chain was opened, so a chain deleted outright — head
// and rows — is a break, not a tenant with no history.
func Verify(ctx context.Context, r ChainReader, keys ChainKeys, tenant string) (VerifyReport, error) {
	if tenant == "" {
		return verifyChain(ctx, r, keys, "", Registry{})
	}
	reg, op, err := ReadRegistry(ctx, r, keys)
	if err != nil {
		return VerifyReport{Tenant: tenant}, err
	}
	if !op.OK {
		return operatorChainBroken(tenant, op), nil
	}
	return verifyChain(ctx, r, keys, tenant, reg)
}

// VerifyAll verifies the operator chain and every tenant chain the store or
// the operator chain knows of, in tenant order (the operator chain first).
func VerifyAll(ctx context.Context, r ChainReader, keys ChainKeys, tenants []string) ([]VerifyReport, error) {
	reg, op, err := ReadRegistry(ctx, r, keys)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{"": true}
	var all []string
	for _, t := range tenants {
		if !seen[t] {
			seen[t] = true
			all = append(all, t)
		}
	}
	for t := range reg.Openings {
		if !seen[t] {
			seen[t] = true
			all = append(all, t)
		}
	}
	sort.Strings(all)
	out := []VerifyReport{op}
	for _, t := range all {
		if !op.OK {
			out = append(out, operatorChainBroken(t, op))
			continue
		}
		rep, err := verifyChain(ctx, r, keys, t, reg)
		if err != nil {
			return out, err
		}
		out = append(out, rep)
	}
	return out, nil
}

func verifyChain(ctx context.Context, r ChainReader, keys ChainKeys, tenant string, reg Registry) (VerifyReport, error) {
	rep := VerifyReport{Tenant: tenant, OK: true}
	if len(keys.Current) == 0 {
		return rep, errors.New("no audit chain key: set LOOMCYCLE_SECRET_KEY")
	}
	head, err := r.AuditHeadGet(ctx, tenant)
	var nf *store.ErrNotFound
	found := true
	if errors.As(err, &nf) {
		head, found = store.AuditHead{TenantID: tenant}, false
	} else if err != nil {
		return rep, err
	}
	rep.AnchorSeq, rep.HeadSeq = head.AnchorSeq, head.Seq
	if !found {
		// No head is an empty chain only if there are no rows either.
		rows, err := r.AuditEventsList(ctx, tenant, 0, 1)
		if err != nil {
			return rep, err
		}
		if len(rows) > 0 {
			return rep.broken(rows[0].Seq, "the chain has rows but no head"), nil
		}
	}
	if tenant != "" {
		switch n := reg.Openings[tenant]; {
		case n > 1:
			return rep.broken(1, "the chain was opened again %d times: its earlier history was removed", n-1), nil
		case n == 1 && !found:
			return rep.broken(1, "the chain disappeared: the operator chain records its opening, but it has no head"), nil
		case n == 1 && head.Seq == 0:
			// The opening and the first row are written together, and a prune
			// never moves the head back, so an opened chain's head is past 0.
			return rep.broken(1, "the chain was opened but its head is empty"), nil
		case n == 0 && found && !reg.Pruned:
			return rep.broken(1, "the operator chain has no record of this chain's opening"), nil
		}
	}
	if head.AnchorSeq > 0 && !keys.check(head.AnchorMAC, macAnchor, tenant, head.AnchorSeq, head.AnchorHash) {
		return rep.broken(head.AnchorSeq, "the retention anchor's signature does not verify"), nil
	}
	prev, want := head.AnchorHash, head.AnchorSeq+1
	for want <= head.Seq {
		rows, err := r.AuditEventsList(ctx, tenant, want-1, verifyPage)
		if err != nil {
			return rep, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if want > head.Seq {
				// Appended after the head was read.
				break
			}
			switch {
			case row.Seq != want:
				return rep.broken(want, "row %d is missing (the next row is %d)", want, row.Seq), nil
			case row.PrevHash != prev:
				return rep.broken(want, "row %d does not follow the row before it", want), nil
			case RowHash(prev, row) != row.Hash:
				return rep.broken(want, "row %d's content does not match its hash", want), nil
			case !keys.check(row.MAC, macRow, tenant, row.Seq, row.Hash):
				return rep.broken(want, "row %d's signature does not verify", want), nil
			}
			prev = row.Hash
			want++
			rep.Rows++
		}
	}
	if want <= head.Seq {
		return rep.broken(want, "the chain ends at row %d but its head is at %d", want-1, head.Seq), nil
	}
	if prev != head.Hash {
		return rep.broken(head.Seq, "the head does not match the last row"), nil
	}
	if head.Seq > 0 && !keys.check(head.MAC, macHead, tenant, head.Seq, head.Hash) {
		return rep.broken(head.Seq, "the head's signature does not verify"), nil
	}
	// Rows past the head: an append since the head was read moved the head
	// too; a head moved back over rows that remain did not.
	past, err := r.AuditEventsList(ctx, tenant, head.Seq, 1)
	if err != nil {
		return rep, err
	}
	if len(past) > 0 {
		now, err := r.AuditHeadGet(ctx, tenant)
		if err != nil && !errors.As(err, &nf) {
			return rep, err
		}
		if err != nil || now.Seq < past[0].Seq {
			return rep.broken(past[0].Seq, "row %d lies past the chain's head (at %d)", past[0].Seq, head.Seq), nil
		}
	}
	return rep, nil
}

// Appender is the store surface a StoreSink writes through.
type Appender interface {
	AuditAppend(ctx context.Context, row store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) (store.AuditEventRow, error)
}

// storeAppendTimeout bounds one append, since Sink.Record carries no context:
// a stalled database must not hold the operation being recorded forever.
const storeAppendTimeout = 10 * time.Second

// StoreSink records events into the store-backed chain.
type StoreSink struct {
	store Appender
	keys  ChainKeys
	now   func() time.Time
}

// NewStoreSink returns a sink appending to s, signing with keys. It refuses
// without a current key: an unsigned chain would promise an integrity it
// cannot keep against a database writer.
func NewStoreSink(s Appender, keys ChainKeys) (*StoreSink, error) {
	if s == nil {
		return nil, errors.New("audit: no store")
	}
	if len(keys.Current) == 0 {
		return nil, errors.New("audit: the store-backed audit stream signs its chain with a key derived from LOOMCYCLE_SECRET_KEY, which is not set")
	}
	return &StoreSink{store: s, keys: keys, now: time.Now}, nil
}

// Record appends ev to its tenant's chain.
func (s *StoreSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeAppendTimeout)
	defer cancel()
	row := RowFor(ev, payload)
	var opened *store.AuditEventRow
	if row.TenantID != "" {
		o, err := openedRow(row.TenantID, ev.TS)
		if err != nil {
			return err
		}
		opened = &o
	}
	if _, err := s.store.AuditAppend(ctx, row, s.keys.Seal(), opened); err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	return nil
}

// openedRow is the operator-chain record that tenant's chain exists.
func openedRow(tenant string, ts time.Time) (store.AuditEventRow, error) {
	ev := Event{TS: ts, Action: ActionChainOpened, TargetTenant: tenant}
	payload, err := json.Marshal(ev)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit: marshal: %w", err)
	}
	row := RowFor(ev, payload)
	row.TenantID, row.Subject = "", tenant
	return row, nil
}

// ChainTenant is the chain an event belongs to: the tenant it acted on, else
// the actor's, else the operator chain "". An operator acting on a tenant's
// users lands in that tenant's chain, where its operator can export it.
func ChainTenant(ev Event) string {
	if ev.TargetTenant != "" {
		return ev.TargetTenant
	}
	return ev.ActorTenant
}

// RowFor is the chain row for ev, before the store numbers and seals it.
func RowFor(ev Event, payload []byte) store.AuditEventRow {
	actor := ev.ActorSubject
	if actor == "" && ev.ActorTokenSuffix != "" {
		actor = "token:…" + ev.ActorTokenSuffix
	}
	subject := ev.TargetSubject
	for _, s := range []string{ev.TargetName, ev.TargetDefID} {
		if subject == "" {
			subject = s
		}
	}
	return store.AuditEventRow{
		TenantID: ChainTenant(ev),
		TS:       ev.TS,
		Actor:    actor,
		Action:   ev.Action,
		Subject:  subject,
		Payload:  string(payload),
	}
}

// multiSink records to every sink, and fails if any of them failed.
type multiSink []Sink

// Combine returns one Sink recording to each non-nil sink given, or nil when
// there is none. A caller that treats a nil sink as "nothing durable is
// written" keeps that meaning.
func Combine(sinks ...Sink) Sink {
	var out multiSink
	for _, s := range sinks {
		if s != nil {
			out = append(out, s)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

func (m multiSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		// One time for every copy, so the mirror and the chain agree.
		ev.TS = time.Now().UTC()
	}
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Record(ev))
	}
	return errors.Join(errs...)
}
