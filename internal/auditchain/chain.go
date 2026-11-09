// Package auditchain is the store-backed audit stream (RFC FZ Part E): the
// signed per-tenant hash chain, its verification and retention, and the
// audit.Sink that writes to it. It sits on the store; package audit (the
// event, the file sink) sits below everything.
//
// Every replica appends to one hash chain per tenant in the store:
//
//	hash = hex(SHA-256(prev_hash || canonical(row)))
//
// where canonical(row) is the length-prefixed (uvarint length, then bytes)
// concatenation of version, tenant, seq, ts_ns, actor, action, subject and
// payload, exactly as stored, and prev_hash is the previous row's hash (""
// for a chain's first row). The hash alone shows that rows were changed,
// removed or reordered — but anyone who can write the database could rewrite
// a row and recompute every hash after it. So each link is also signed:
//
//	mac = hex(HMAC-SHA256(K, lp("row", tenant, seq, hash)))
//
// with K derived from LOOMCYCLE_SECRET_KEY (credential.DeriveAuditChainKey).
// The "segment" an HMAC covers is one row; since the hash commits to every
// row before it, a valid MAC on row n vouches for rows 1..n. The tenant's head
// (lp("head", tenant, seq, hash)) and the anchor a retention prune leaves
// (lp("anchor", tenant, seq, hash, state)) are signed in their own domains,
// so neither a row's MAC nor the head's can be replayed as an anchor to pass
// off deleted rows as pruned ones.
//
// The payload (the event as JSON) is the record; actor, action and subject
// are display fields derived from it, stored escaped (SafeText) where the
// event carried a NUL or invalid UTF-8.
//
// A chain that is deleted outright (head and rows) would otherwise read as a
// tenant with no history, so a tenant chain's first append also writes a
// signed ActionChainOpened row to the operator chain "" in the same
// transaction. Verification of a tenant first verifies the operator chain —
// a registry read from an unverified chain could be forged — and then breaks
// when the operator chain records an opening but the chain has no head
// ("disappeared") or an empty one, records two (deleted and begun again), or
// records none (the operator chain itself deleted). A retention prune of the
// operator chain carries the openings it removes in its signed anchor, so the
// check outlives the rows. A head missing or reset while rows remain is a
// break too: rows past the head are reported.
//
// A tenant chain rolled back to an earlier, genuinely signed head — its
// newest rows deleted and an older head (from a backup, a replica or an
// export's trailer) restored — verifies row by row. So the store sink
// checkpoints each tenant's head into the operator chain (ActionChainCheckpoint:
// seq, hash and the head's MAC) every checkpointRows rows, every
// checkpointInterval, and at a clean shutdown; verification breaks when a
// tenant's head is behind its newest checkpoint or the row at ANY checkpoint
// still in the chain has another hash (after a rollback the deployment goes
// on appending, so a newer checkpoint can cover forged history while an
// older one is the witness). A tenant prune checks the same against the rows
// it removes and is refused while the operator chain does not verify, so a
// rollback is never folded under a signed anchor. A prune of the operator
// chain carries each tenant's newest checkpoints in its anchor, as it does
// the openings.
//
// What it cannot show without an outside witness: a database writer who
// deletes EVERY chain — the operator chain and all tenant chains — leaves an
// empty audit store that verifies as empty; one who rolls the operator chain
// back too (to before the checkpoints, with an older operator head, which
// only the operator's own exports and backups hold) is not caught; and rows
// written since a tenant's last checkpoint can still be rolled back. The
// JSONL mirror (LOOMCYCLE_AUDIT_LOG_PATH) and exports taken with GET
// /v1/_audit are that witness.
package auditchain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/audit"
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

// lp appends each field length-prefixed (uvarint length, then the bytes):
// no field can run into the next, and every byte is covered as stored —
// unlike a JSON encoding, which maps invalid UTF-8 to U+FFFD and would let
// two different stored values hash alike.
func lp(fields ...string) []byte {
	var b []byte
	for _, f := range fields {
		b = binary.AppendUvarint(b, uint64(len(f)))
		b = append(b, f...)
	}
	return b
}

// canonical is the hashed encoding of a row's content.
func canonical(r store.AuditEventRow) []byte {
	return lp(chainVersion, r.TenantID, strconv.FormatInt(r.Seq, 10), strconv.FormatInt(r.TS.UnixNano(), 10),
		r.Actor, r.Action, r.Subject, r.Payload)
}

// RowHash is H(prev_hash ‖ canonical row). prevHash is fixed-length hex or
// empty, and canonical starts with a length byte that is not a hex digit, so
// the concatenation is unambiguous.
func RowHash(prevHash string, r store.AuditEventRow) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(canonical(r))
	return hex.EncodeToString(h.Sum(nil))
}

// chainMAC signs (domain, tenant, seq, hash, extra...). An anchor's state is
// its extra field; rows and heads have none.
func chainMAC(key []byte, domain, tenant string, seq int64, hash string, extra ...string) string {
	m := hmac.New(sha256.New, key)
	m.Write(lp(append([]string{domain, tenant, strconv.FormatInt(seq, 10), hash}, extra...)...))
	return hex.EncodeToString(m.Sum(nil))
}

// SafeText makes a recorded field storable as text on every backend and
// hashed exactly as stored. A string that is valid UTF-8 with no NUL is
// returned unchanged. Otherwise the whole field is escaped, reversibly: a
// backslash becomes `\\`, a NUL `\x00`, and each byte of an invalid UTF-8
// sequence `\xHH` — so the original bytes can be recovered from the stored
// text, where replacing them (with U+FFFD, say) would lose which bytes a
// caller sent. Postgres refuses NUL and invalid UTF-8 in text outright; a
// row it refuses is an event nobody can see, which is what someone sending
// a %00 in a denied path would want.
//
// An escaped field is valid UTF-8 with no NUL, so SafeText is idempotent.
// The escape is not marked: a clean field holding the literal text `\x00`
// reads like an escaped NUL. The row's payload — the event as JSON, which
// writes a NUL as \u0000 — tells them apart.
func SafeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == 0:
			b.WriteString(`\x00`)
		case r == '\\':
			b.WriteString(`\\`)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func (k ChainKeys) sign(domain, tenant string, seq int64, hash string, extra ...string) string {
	return chainMAC(k.Current, domain, tenant, seq, hash, extra...)
}

// signedBy says which key a MAC verifies under.
type signedBy int

const (
	signedByNone signedBy = iota
	signedByCurrent
	signedByPrevious
)

// which reports which key mac signs (domain, tenant, seq, hash) under,
// comparing in constant time.
func (k ChainKeys) which(mac, domain, tenant string, seq int64, hash string, extra ...string) signedBy {
	if len(k.Current) > 0 && hmac.Equal([]byte(mac), []byte(chainMAC(k.Current, domain, tenant, seq, hash, extra...))) {
		return signedByCurrent
	}
	if len(k.Previous) > 0 && hmac.Equal([]byte(mac), []byte(chainMAC(k.Previous, domain, tenant, seq, hash, extra...))) {
		return signedByPrevious
	}
	return signedByNone
}

// check reports whether mac verifies under the current or the previous key.
func (k ChainKeys) check(mac, domain, tenant string, seq int64, hash string, extra ...string) bool {
	return k.which(mac, domain, tenant, seq, hash, extra...) != signedByNone
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

// anchorState is what an anchor carries forward from the rows a prune
// removed. Opened counts, per tenant, the ActionChainOpened rows the operator
// chain had before its anchor; it is empty for a tenant chain.
type anchorState struct {
	Opened map[string]int `json:"opened,omitempty"`
	// Rotated records that a row up to the anchor verified under the current
	// key, so the previous key no longer signs a row after it: the rotation
	// point survives the rows that established it being pruned.
	Rotated bool `json:"rotated,omitempty"`
	// Checkpoints is, per tenant, the checkpoints among the operator chain's
	// rows before its anchor that the tenant chain can still be compared
	// with; empty for a tenant chain.
	//
	// Every checkpoint counts, not just the newest: after a rollback the
	// deployment goes on appending to the rolled-back head and in time
	// checkpoints that history too, so a newer checkpoint can be the forged
	// one and an older one the witness. So none is dropped while it can
	// still be compared: only those below the tenant chain's own signed
	// retention anchor go (the tenant prune that moved the anchor checked
	// them). The set is bounded by the tenant chain's retention.
	Checkpoints map[string][]Checkpoint `json:"checkpoints,omitempty"`
}

// Checkpoint is a tenant chain's head as the operator chain recorded it.
type Checkpoint struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// ActionChainCheckpoint is the operator-chain record of a tenant chain's
// head (its Subject is the tenant; the payload carries seq, hash and the
// head's MAC).
const ActionChainCheckpoint = "audit_chain_checkpoint"

// checkpointOf reads a checkpoint row's payload. A row that verified was
// written by the runtime, so one that does not decode is reported as an
// error, not skipped.
func checkpointOf(row store.AuditEventRow) (Checkpoint, error) {
	var p struct {
		Seq  int64  `json:"checkpoint_seq"`
		Hash string `json:"checkpoint_hash"`
	}
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil || p.Seq <= 0 || p.Hash == "" {
		return Checkpoint{}, fmt.Errorf("audit: operator row %d is a checkpoint that does not decode", row.Seq)
	}
	return Checkpoint{Seq: p.Seq, Hash: p.Hash}, nil
}

// addCheckpoint records cp for tenant, keeping each tenant's checkpoints in
// seq order. Two checkpoints at one position with different hashes are both
// kept: they cannot both match the chain, and verification says so.
func addCheckpoint(m map[string][]Checkpoint, tenant string, cp Checkpoint) map[string][]Checkpoint {
	if m == nil {
		m = map[string][]Checkpoint{}
	}
	l := m[tenant]
	i := sort.Search(len(l), func(i int) bool {
		return l[i].Seq > cp.Seq || (l[i].Seq == cp.Seq && l[i].Hash >= cp.Hash)
	})
	if i < len(l) && l[i] == cp {
		return m
	}
	l = append(l, Checkpoint{})
	copy(l[i+1:], l[i:])
	l[i] = cp
	m[tenant] = l
	return m
}

// checkCheckpoints checks a chain's head and the rows (or anchor) at its
// checkpoints' positions against them: a head behind the newest is rows
// removed, a row with another hash is history rewritten. rowHash looks up
// the hash at a position still in the chain. A checkpoint below the anchor
// was checked by the prune that removed its row. It returns the break, or
// seq 0.
func checkCheckpoints(cps []Checkpoint, head store.AuditHead, rowHash func(int64) (string, bool)) (int64, string) {
	if len(cps) == 0 {
		return 0, ""
	}
	if newest := cps[len(cps)-1]; newest.Seq > head.Seq {
		return head.Seq + 1, fmt.Sprintf("the chain's head (at %d) is behind its checkpoint at %d in the operator chain: rows were removed",
			head.Seq, newest.Seq)
	}
	for _, cp := range cps {
		switch {
		case cp.Seq < head.AnchorSeq:
		case cp.Seq == head.AnchorSeq:
			if cp.Hash != head.AnchorHash {
				return cp.Seq, fmt.Sprintf("the retention anchor does not match the operator chain's checkpoint at %d", cp.Seq)
			}
		default:
			if h, ok := rowHash(cp.Seq); ok && h != cp.Hash {
				return cp.Seq, fmt.Sprintf("row %d does not match the operator chain's checkpoint of it", cp.Seq)
			}
		}
	}
	return 0, ""
}

// checkAnchor checks a chain's retention anchor and returns the state it
// carries, or why it does not hold. With no anchor (seq 0) every anchor
// field must be empty: a state planted there is never carried or trusted.
// Verification and the prune's seal share it.
func (k ChainKeys) checkAnchor(head store.AuditHead) (anchorState, string) {
	switch {
	case head.AnchorSeq < 0:
		return anchorState{}, "the retention anchor's position is negative"
	case head.AnchorSeq == 0:
		if head.AnchorHash != "" || head.AnchorMAC != "" || head.AnchorState != "" {
			return anchorState{}, "the chain has no retention anchor but its anchor fields are set"
		}
		return anchorState{}, ""
	}
	by := k.which(head.AnchorMAC, macAnchor, head.TenantID, head.AnchorSeq, head.AnchorHash, head.AnchorState)
	if by == signedByNone {
		return anchorState{}, "the retention anchor's signature does not verify"
	}
	st, err := decodeAnchorState(head.AnchorState)
	if err != nil {
		return anchorState{}, "the retention anchor's state does not decode"
	}
	// "rotated" speaks of the key that signed the anchor. An anchor that
	// verifies only under the previous key was signed before a rotation that
	// has happened since, so the rows after it may be signed with that
	// (now previous) key: the rotation point starts over.
	if by == signedByPrevious {
		st.Rotated = false
	}
	return st, ""
}

func decodeAnchorState(s string) (anchorState, error) {
	var st anchorState
	if s == "" {
		return st, nil
	}
	err := json.Unmarshal([]byte(s), &st)
	return st, err
}

// ErrChainBroken is a prune's refusal: the chain, or the range it would
// remove, does not verify. Re-signing an anchor over it would launder the
// tampering under the key, so the rows stay and the break stays reported.
var ErrChainBroken = errors.New("audit: the chain does not verify")

// SealAnchor returns the store.AuditAnchorSealFunc a prune signs its anchor
// with, inside the prune's transaction. Before it signs anything it checks
// what it is handed: the previous anchor's signature over its seq, hash and
// state, and every pruned row's link from that anchor (in place, unaltered,
// signed). Any failure is ErrChainBroken and aborts the prune — nothing
// unverified is folded into a new signature. The new state is the previous
// anchor's openings plus the pruned rows' (operator chain only).
func (k ChainKeys) SealAnchor() store.AuditAnchorSealFunc {
	return k.sealAnchor(nil, nil)
}

// sealAnchor is SealAnchor with what the chain's prune knows besides its own
// rows. For a tenant chain, cps are its checkpoints from the operator chain:
// the prune is also refused when the locked head is behind the newest one or
// a pruned row contradicts one, so a rollback is never folded under a signed
// anchor. For the operator chain, tenantAnchors are the tenant chains'
// verified retention anchors: a carried checkpoint is dropped only below its
// tenant's anchor, never because there are many.
func (k ChainKeys) sealAnchor(cps []Checkpoint, tenantAnchors map[string]int64) store.AuditAnchorSealFunc {
	return func(prev store.AuditHead, pruned []store.AuditEventRow) (string, string, string, error) {
		tenant := prev.TenantID
		if len(pruned) == 0 {
			return "", "", "", fmt.Errorf("%w: nothing to prune", ErrChainBroken)
		}
		st, why := k.checkAnchor(prev)
		if why != "" {
			return "", "", "", fmt.Errorf("%w: %s", ErrChainBroken, why)
		}
		// The head, as locked: signed, and not behind the rows removed. Without
		// this, valid rows past a head moved back (an old head replayed) would
		// be folded under the new anchor, and a chain pruned to its head is
		// then vouched for by that anchor alone.
		if !k.check(prev.MAC, macHead, tenant, prev.Seq, prev.Hash) {
			return "", "", "", fmt.Errorf("%w: the head's signature does not verify", ErrChainBroken)
		}
		if last := pruned[len(pruned)-1]; last.Seq > prev.Seq {
			return "", "", "", fmt.Errorf("%w: row %d lies past the chain's head (at %d)", ErrChainBroken, last.Seq, prev.Seq)
		} else if last.Seq == prev.Seq && last.Hash != prev.Hash {
			return "", "", "", fmt.Errorf("%w: the head does not match the last row", ErrChainBroken)
		}
		byPos := make(map[int64]string, len(pruned))
		for _, r := range pruned {
			byPos[r.Seq] = r.Hash
		}
		if at, why := checkCheckpoints(cps, prev, func(seq int64) (string, bool) {
			h, ok := byPos[seq]
			return h, ok
		}); at != 0 {
			return "", "", "", fmt.Errorf("%w: %s", ErrChainBroken, why)
		}
		link, want, rotated := prev.AnchorHash, prev.AnchorSeq+1, st.Rotated
		for _, r := range pruned {
			if why := k.checkLink(tenant, link, want, r, &rotated); why != "" {
				return "", "", "", fmt.Errorf("%w: %s", ErrChainBroken, why)
			}
			if tenant == "" && r.Action == ActionChainOpened {
				if st.Opened == nil {
					st.Opened = map[string]int{}
				}
				st.Opened[r.Subject]++
			}
			if tenant == "" && r.Action == ActionChainCheckpoint {
				cp, err := checkpointOf(r)
				if err != nil {
					return "", "", "", fmt.Errorf("%w: %v", ErrChainBroken, err)
				}
				st.Checkpoints = addCheckpoint(st.Checkpoints, r.Subject, cp)
			}
			link = r.Hash
			want++
		}
		st.Rotated = rotated
		for t, l := range st.Checkpoints {
			below := tenantAnchors[t]
			kept := l[:0:0]
			for _, cp := range l {
				if cp.Seq >= below {
					kept = append(kept, cp)
				}
			}
			if len(kept) == 0 {
				delete(st.Checkpoints, t)
			} else {
				st.Checkpoints[t] = kept
			}
		}
		state := ""
		if len(st.Opened) > 0 || st.Rotated || len(st.Checkpoints) > 0 {
			b, err := json.Marshal(st) // map keys marshal sorted: one encoding
			if err != nil {
				return "", "", "", err
			}
			state = string(b)
		}
		last := pruned[len(pruned)-1]
		// Pruned up to the head: re-sign the head (verified above) with the
		// current key, as the anchor is, so a later key rotation finds both
		// under the same key.
		headMAC := ""
		if last.Seq == prev.Seq {
			headMAC = k.sign(macHead, tenant, prev.Seq, prev.Hash)
		}
		return state, k.sign(macAnchor, tenant, last.Seq, last.Hash, state), headMAC, nil
	}
}

// PruneStore is what Prune reads and prunes through.
type PruneStore interface {
	ChainReader
	AuditTenants(ctx context.Context) ([]string, error)
	AuditPrune(ctx context.Context, tenantID string, before time.Time, limit int, seal store.AuditAnchorSealFunc) (int, error)
}

// tenantAnchors reads each tenant chain's retention anchor position, for the
// operator chain's prune to drop only checkpoints below it. An anchor whose
// signature does not verify counts as none (0): a planted anchor must not
// make the prune drop a witness. An older genuine anchor replayed is lower,
// so it only keeps more. Anchors move only up, so one read before the prune
// stays a floor while it runs.
func tenantAnchors(ctx context.Context, st PruneStore, keys ChainKeys) (map[string]int64, error) {
	tenants, err := st.AuditTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, t := range tenants {
		if t == "" {
			continue
		}
		h, err := st.AuditHeadGet(ctx, t)
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			continue
		}
		if err != nil {
			return nil, err
		}
		h.TenantID = t
		if _, why := keys.checkAnchor(h); why == "" {
			out[t] = h.AnchorSeq
		}
	}
	return out, nil
}

// pruneBatch is how many rows one prune transaction removes at most, so a
// first prune of a long chain does not read it all into memory at once.
const pruneBatch = 5000

// Prune removes a chain's rows recorded before `before`, in batches of at
// most pruneBatch, behind a signed anchor. It does not walk the whole chain:
// each batch's transaction hands the seal (SealAnchor) the current anchor and
// every row it would remove, and the seal verifies exactly those — the
// anchor's signature and state, each row's link, hash and signature — before
// it signs anything, refusing with ErrChainBroken otherwise. That is what
// keeps a prune from re-signing tampering; the rest of the chain is left as
// it is and stays as verifiable as it was. Full verification (the head, rows
// past the prune point, the operator chain's record of each chain) is the
// separate, on-demand Verify / `loomcycle audit verify`, so a prune's cost
// is bounded by what it removes, however long the chain or the operator
// chain has grown.
func Prune(ctx context.Context, st PruneStore, keys ChainKeys, tenant string, before time.Time) (int, error) {
	seal := keys.SealAnchor()
	if tenant == "" {
		anchors, err := tenantAnchors(ctx, st, keys)
		if err != nil {
			return 0, err
		}
		seal = keys.sealAnchor(nil, anchors)
	} else {
		// A tenant prune is checked against the operator chain's checkpoints
		// of it, read from a verified operator chain: a broken one refuses
		// the prune rather than fold rows nothing can vouch for. This reads
		// the operator chain once per tenant prune.
		reg, op, err := ReadRegistry(ctx, st, keys)
		if err != nil {
			return 0, err
		}
		if !op.OK {
			return 0, fmt.Errorf("%w: the operator chain does not verify (row %d: %s)", ErrChainBroken, op.BreakSeq, op.Reason)
		}
		seal = keys.sealAnchor(reg.Checkpoints[tenant], nil)
	}
	total := 0
	for {
		n, err := st.AuditPrune(ctx, tenant, before, pruneBatch, seal)
		total += n
		if err != nil || n < pruneBatch {
			return total, err
		}
	}
}

// checkLink checks that row is the chain's row want following a row hashed
// prev: in place, linked, unaltered, and signed. It returns why not, or "".
// rotated tracks the key rotation: once a row verifies under the current
// key, the previous key no longer signs a later one (it would be someone
// still holding the rotated-out key). Verification and the prune's
// re-check share it, so the two cannot disagree on what a valid link is.
func (k ChainKeys) checkLink(tenant, prev string, want int64, row store.AuditEventRow, rotated *bool) string {
	switch {
	case row.Seq != want:
		return fmt.Sprintf("row %d is missing (the next row is %d)", want, row.Seq)
	case row.PrevHash != prev:
		return fmt.Sprintf("row %d does not follow the row before it", want)
	case RowHash(prev, row) != row.Hash:
		return fmt.Sprintf("row %d's content does not match its hash", want)
	}
	switch k.which(row.MAC, macRow, tenant, row.Seq, row.Hash) {
	case signedByCurrent:
		*rotated = true
	case signedByPrevious:
		if *rotated {
			return fmt.Sprintf("row %d is signed with the previous key after rows signed with the current one", want)
		}
	default:
		return fmt.Sprintf("row %d's signature does not verify", want)
	}
	return ""
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

	// anchorState is the verified head's anchor state, for ReadRegistry.
	anchorState string
	// noOpening marks a break for a missing opening record, which a chain
	// opened after the operator chain was read also shows: the caller
	// re-reads the operator chain once before reporting it.
	noOpening bool
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
// many times each was opened — the openings its signed anchor carries from
// pruned rows, plus those in its remaining rows. It is read only from an
// operator chain that verified, up to its verified head, so a forged or
// deleted opening, or a forged anchor, breaks the operator chain instead of
// steering a tenant's verification.
type Registry struct {
	Openings map[string]int
	// Checkpoints is each tenant chain's checkpoints, in seq order.
	Checkpoints map[string][]Checkpoint
}

// ReadRegistry verifies the operator chain and, when it holds, reads the
// registry from the rows it verified.
func ReadRegistry(ctx context.Context, r ChainReader, keys ChainKeys) (Registry, VerifyReport, error) {
	reg := Registry{Openings: map[string]int{}, Checkpoints: map[string][]Checkpoint{}}
	op, err := verifyChain(ctx, r, keys, "", Registry{})
	if err != nil || !op.OK {
		return reg, op, err
	}
	st, err := decodeAnchorState(op.anchorState)
	if err != nil {
		// The anchor's MAC verified over this state, so only the runtime
		// itself could have written it; treat it as a read failure.
		return reg, op, fmt.Errorf("audit: the operator chain's anchor state does not decode: %w", err)
	}
	for t, n := range st.Opened {
		reg.Openings[t] += n
	}
	for t, l := range st.Checkpoints {
		for _, cp := range l {
			reg.Checkpoints = addCheckpoint(reg.Checkpoints, t, cp)
		}
	}
	after := op.AnchorSeq
	for after < op.HeadSeq {
		rows, err := r.AuditEventsList(ctx, "", after, verifyPage)
		if err != nil {
			return reg, op, err
		}
		for _, row := range rows {
			if row.Seq > op.HeadSeq {
				// Appended since the walk (or the rows below were pruned
				// meanwhile): the verified range is read.
				return reg, op, nil
			}
			switch row.Action {
			case ActionChainOpened:
				reg.Openings[row.Subject]++
			case ActionChainCheckpoint:
				cp, err := checkpointOf(row)
				if err != nil {
					return reg, op, err
				}
				reg.Checkpoints = addCheckpoint(reg.Checkpoints, row.Subject, cp)
			}
			after = row.Seq
		}
		if len(rows) < verifyPage {
			break
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
	return verifyWithRetry(ctx, r, keys, tenant, reg)
}

// verifyWithRetry verifies a tenant chain and, when its opening is missing
// from the operator chain as read, reads the operator chain again once: a
// chain whose first append committed after that read shows the same way.
func verifyWithRetry(ctx context.Context, r ChainReader, keys ChainKeys, tenant string, reg Registry) (VerifyReport, error) {
	rep, err := verifyChain(ctx, r, keys, tenant, reg)
	if err != nil || rep.OK || !rep.noOpening {
		return rep, err
	}
	reg, op, err := ReadRegistry(ctx, r, keys)
	if err != nil {
		return rep, err
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
		rep, err := verifyWithRetry(ctx, r, keys, t, reg)
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
		case n == 0 && found:
			rep = rep.broken(1, "the operator chain has no record of this chain's opening")
			rep.noOpening = true
			return rep, nil
		}
	}
	head.TenantID = tenant
	st, why := keys.checkAnchor(head)
	if why != "" {
		return rep.broken(max(head.AnchorSeq, 1), "%s", why), nil
	}
	if head.AnchorSeq > 0 {
		// Rows at or below the anchor were pruned; one there now was put
		// there by something other than the chain.
		low, err := r.AuditEventsList(ctx, tenant, 0, 1)
		if err != nil {
			return rep, err
		}
		if len(low) > 0 && low[0].Seq <= head.AnchorSeq {
			return rep.broken(low[0].Seq, "row %d lies below the retention anchor (at %d)", low[0].Seq, head.AnchorSeq), nil
		}
	}
	// The operator chain's checkpoints of this chain: a head behind the
	// newest is a rollback; the anchor, and each row walked below, must hold
	// the hash checkpointed at its position.
	var cps []Checkpoint
	if tenant != "" {
		cps = reg.Checkpoints[tenant]
		if at, why := checkCheckpoints(cps, head, func(int64) (string, bool) { return "", false }); at != 0 {
			return rep.broken(at, "%s", why), nil
		}
	}
	cpAt := make(map[int64][]string, len(cps))
	for _, cp := range cps {
		cpAt[cp.Seq] = append(cpAt[cp.Seq], cp.Hash)
	}
	rep.anchorState = head.AnchorState
	prev, want := head.AnchorHash, head.AnchorSeq+1
	rotated := st.Rotated
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
			if why := keys.checkLink(tenant, prev, want, row, &rotated); why != "" {
				return rep.broken(want, "%s", why), nil
			}
			for _, h := range cpAt[row.Seq] {
				if h != row.Hash {
					return rep.broken(want, "row %d does not match the operator chain's checkpoint of it", want), nil
				}
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
	if head.Seq > 0 {
		if w := keys.which(head.MAC, macHead, tenant, head.Seq, head.Hash); w == signedByNone || (rotated && w == signedByPrevious) {
			return rep.broken(head.Seq, "the head's signature does not verify"), nil
		}
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

// ChainTenant is the chain an event belongs to: the tenant it acted on, else
// the actor's, else the operator chain "". An operator acting on a tenant's
// users lands in that tenant's chain, where its operator can export it.
func ChainTenant(ev audit.Event) string {
	if ev.TargetTenant != "" {
		return ev.TargetTenant
	}
	return ev.ActorTenant
}

// RowFor is the chain row for ev, before the store numbers and seals it.
func RowFor(ev audit.Event, payload []byte) store.AuditEventRow {
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
	// Every text field through SafeText: an event may carry bytes a caller
	// chose (a path, a definition name), and the row must still be written.
	return store.AuditEventRow{
		TenantID: SafeText(ChainTenant(ev)),
		TS:       ev.TS,
		Actor:    SafeText(actor),
		Action:   SafeText(ev.Action),
		Subject:  SafeText(subject),
		Payload:  SafeText(string(payload)),
	}
}
