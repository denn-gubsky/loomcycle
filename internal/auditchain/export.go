package auditchain

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ExportReport is the outcome of checking an export of one chain (GET
// /v1/_audit) offline.
type ExportReport struct {
	Tenant string `json:"tenant"`
	OK     bool   `json:"ok"`
	Rows   int64  `json:"rows"`
	// FirstSeq / HeadSeq are the first row in the file and the head its
	// trailer names.
	FirstSeq int64 `json:"first_seq"`
	HeadSeq  int64 `json:"head_seq"`
	// Signed says the signatures were checked (a key was given); without
	// one only the hash links are.
	Signed bool `json:"signed"`
	// Partial says the file starts after the chain's start and its anchor:
	// the export was asked from a time or a row, so the link of its first row
	// to the rows before it cannot be checked.
	Partial  bool   `json:"partial,omitempty"`
	BreakSeq int64  `json:"break_seq,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (r ExportReport) broken(seq int64, format string, args ...any) ExportReport {
	r.OK, r.BreakSeq, r.Reason = false, seq, fmt.Sprintf(format, args...)
	return r
}

// exportLine is one line of an export: a row, or the trailer.
type exportLine struct {
	Trailer bool   `json:"trailer"`
	Tenant  string `json:"tenant"`
	// A row.
	Seq      int64  `json:"seq"`
	TSNanos  int64  `json:"ts_ns"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Subject  string `json:"subject"`
	Payload  string `json:"payload"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
	MAC      string `json:"mac"`
	// The trailer.
	HeadSeq     int64  `json:"head_seq"`
	HeadHash    string `json:"head_hash"`
	HeadMAC     string `json:"head_mac"`
	AnchorSeq   int64  `json:"anchor_seq"`
	AnchorHash  string `json:"anchor_hash"`
	AnchorMAC   string `json:"anchor_mac"`
	AnchorState string `json:"anchor_state"`
}

// maxExportLine bounds one line of an export file (a row's payload is the
// event as JSON; events are small).
const maxExportLine = 16 << 20

// VerifyExport checks an export of one chain offline: every row is the next
// row of the one before (seq, prev_hash, and its hash recomputed from its
// content), the first row follows the retention anchor (or starts the
// chain), and the rows reach the head the trailer names. With keys it also
// checks every row's signature, the head's and the anchor's, so the file is
// shown to be the deployment's own chain, unaltered and not cut short, as of
// the export.
//
// Without the key it shows only that the file is internally consistent:
// anyone can rebuild a consistent file. Even with the key it cannot show
// what the store held before the export or since: a chain rolled back before
// the export (rows removed from the store) yields a consistent, signed file;
// that is checked against the operator chain's checkpoints with `loomcycle
// audit verify --tenant`. A file is read as given, so a later export is
// needed to show nothing was removed after it.
//
// An error is a file that cannot be read or parsed, never a break.
func VerifyExport(r io.Reader, keys *ChainKeys) (ExportReport, error) {
	rep := ExportReport{OK: true, Signed: keys != nil && len(keys.Current) > 0}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxExportLine)
	var rows []store.AuditEventRow
	var trailer *exportLine
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if trailer != nil {
			return rep, fmt.Errorf("line %d: a line follows the trailer", n)
		}
		var l exportLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return rep, fmt.Errorf("line %d: %w", n, err)
		}
		if l.Trailer {
			trailer = &l
			continue
		}
		rows = append(rows, store.AuditEventRow{TenantID: l.Tenant, Seq: l.Seq, TS: time.Unix(0, l.TSNanos).UTC(),
			Actor: l.Actor, Action: l.Action, Subject: l.Subject, Payload: l.Payload,
			PrevHash: l.PrevHash, Hash: l.Hash, MAC: l.MAC})
	}
	if err := sc.Err(); err != nil {
		return rep, err
	}
	if trailer == nil {
		return rep.broken(0, "the file has no trailer: the export was cut short"), nil
	}
	tenant := trailer.Tenant
	rep.Tenant, rep.HeadSeq = tenant, trailer.HeadSeq
	head := store.AuditHead{TenantID: tenant, Seq: trailer.HeadSeq, Hash: trailer.HeadHash, MAC: trailer.HeadMAC,
		AnchorSeq: trailer.AnchorSeq, AnchorHash: trailer.AnchorHash, AnchorMAC: trailer.AnchorMAC, AnchorState: trailer.AnchorState}
	rotated := false
	if rep.Signed {
		st, why := keys.checkAnchor(head)
		if why != "" {
			return rep.broken(max(head.AnchorSeq, 1), "%s", why), nil
		}
		rotated = st.Rotated
	}
	headSigned := func() bool {
		if !rep.Signed || head.Seq == 0 {
			return true
		}
		w := keys.which(head.MAC, macHead, tenant, head.Seq, head.Hash)
		return w != signedByNone && !(rotated && w == signedByPrevious)
	}
	if len(rows) == 0 {
		if !headSigned() {
			return rep.broken(head.Seq, "the head's signature does not verify"), nil
		}
		if head.Seq > head.AnchorSeq {
			return rep.broken(head.AnchorSeq+1, "the file holds no rows but the head is at %d", head.Seq), nil
		}
		return rep, nil
	}
	first := rows[0]
	rep.FirstSeq = first.Seq
	var prev string
	switch {
	case first.Seq == head.AnchorSeq+1:
		prev = head.AnchorHash
	case first.Seq > head.AnchorSeq+1:
		// Asked from a time or a row: the link to the rows before is not in
		// the file. Each row's own signature still holds it to the chain.
		rep.Partial, prev = true, first.PrevHash
	default:
		return rep.broken(first.Seq, "row %d lies at or below the retention anchor (at %d)", first.Seq, head.AnchorSeq), nil
	}
	want := first.Seq
	for _, row := range rows {
		if row.TenantID != tenant {
			return rep.broken(row.Seq, "row %d belongs to another chain", row.Seq), nil
		}
		if rep.Signed {
			if why := keys.checkLink(tenant, prev, want, row, &rotated); why != "" {
				return rep.broken(want, "%s", why), nil
			}
		} else {
			switch {
			case row.Seq != want:
				return rep.broken(want, "row %d is missing (the next row is %d)", want, row.Seq), nil
			case row.PrevHash != prev:
				return rep.broken(want, "row %d does not follow the row before it", want), nil
			case RowHash(prev, row) != row.Hash:
				return rep.broken(want, "row %d's content does not match its hash", want), nil
			}
		}
		prev = row.Hash
		want++
		rep.Rows++
	}
	last := rows[len(rows)-1]
	switch {
	case last.Seq < head.Seq:
		return rep.broken(last.Seq+1, "the file ends at row %d but its head is at %d: rows were cut off", last.Seq, head.Seq), nil
	case last.Seq > head.Seq:
		return rep.broken(head.Seq+1, "row %d lies past the head (at %d)", last.Seq, head.Seq), nil
	case last.Hash != head.Hash:
		return rep.broken(head.Seq, "the head does not match the last row"), nil
	case !headSigned():
		return rep.broken(head.Seq, "the head's signature does not verify"), nil
	}
	return rep, nil
}
