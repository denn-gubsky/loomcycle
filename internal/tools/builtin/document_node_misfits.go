package builtin

// document_node_misfits.go — misfits (RFC GM §5.1, plan P5.1): Document op=node_stats
// mode misfits lists each member document nearer a sibling concept's centroid than
// its own concept's by more than nodestats.MisfitMargin, and with propose files a
// `subject` proposal from the document to that sibling, method rule:centroid.
//
// WHAT IS COMPARED. A concept's members and vectors are the node_stats plan's: only
// the documents every member of the tenant may read (RFC GM C8), each with its
// document annotation's vector of the embed model; a metadata-only member has none
// (C9). The comparison set is a concept's siblings (the concepts under the same
// broader concept, or a scheme's top level): similarities to centroids at different
// depths are not comparable (RFC GM §3.3). Document classes are not compared: a
// misfit of a class would be a retype, not a placement.
//
// REPORT BY DEFAULT. Without propose the mode writes nothing. With propose it files
// at most `limit` proposals per call (default misfitProposalCap), largest margin
// first, each through propose_statement, so the access-control question, the
// rejection tombstone and the review policy apply exactly as to any proposal. The
// method is the runtime's own (D11): no agent is granted rule:centroid, and the
// pass stamps it on the unexported edgeMethod, never from the wire.
//
// NEVER AGAIN. A placement any reviewer rejected — a person, a bulk ruling, a judge
// or a review policy — is not proposed again for that document, nor one that an
// open proposal or a live statement already makes. propose_statement refuses only a
// person's rejection; this mode is stricter, because a pass that re-files what a
// judge refused on every run would flood the queue with one decision.
//
// ADDITIONS ONLY. A document may hold several topics, so a proposal adds the nearer
// concept and leaves the existing placement as it is. The RFC's competing "move"
// form (propose_statement id: on the existing statement when the document sits
// clearly outside its node) is not filed here: it needs a threshold the P0
// measurement did not set. Nothing here proposes an ontology change.
//
// The vector stage runs only where the store keeps vectors (Postgres with
// pgvector) and an embedder names the model; elsewhere the mode says so and lists
// nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

const (
	nodeStatsModeMisfits = "misfits"

	// misfitMethod is the method of every proposal the mode files (RFC GM D11).
	misfitMethod = "rule:centroid"
	// misfitProposalCap is the proposals one call files when limit is not given;
	// misfitProposalMax bounds limit.
	misfitProposalCap = 20
	misfitProposalMax = 100
	// misfitReportCap bounds the misfits an answer lists; the count is whole.
	misfitReportCap = 200
)

// nodeMisfit is one misfit in the answer.
type nodeMisfit struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	// NodeID / Node: the concept the document is placed under (directly or through a
	// narrower concept).
	NodeID string `json:"node_id"`
	Node   string `json:"node"`
	// NearestNodeID / Nearest: the sibling concept whose centroid it is nearer.
	NearestNodeID string `json:"nearest_node_id"`
	Nearest       string `json:"nearest"`
	// OwnSimilarity is the cosine to its own concept's centroid without it;
	// NearestSimilarity to the sibling's; Margin the difference.
	OwnSimilarity     float64 `json:"own_similarity"`
	NearestSimilarity float64 `json:"nearest_similarity"`
	Margin            float64 `json:"margin"`
	// With propose: StatementID and Status of the proposal filed, or Skipped (why
	// none was filed: rejected, exists, cap, duplicate, refused:<code>) and Detail.
	StatementID string `json:"statement_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Skipped     string `json:"skipped,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// NodeMisfitsReport is node_stats mode misfits' answer.
type NodeMisfitsReport struct {
	Mode   string `json:"mode"`
	Scheme string `json:"scheme,omitempty"`
	// Vectors: the comparison ran; Note says why it did not.
	Vectors    bool    `json:"vectors"`
	Note       string  `json:"note,omitempty"`
	EmbedModel string  `json:"embed_model,omitempty"`
	Margin     float64 `json:"margin"`
	MinMembers int     `json:"min_members"`
	Propose    bool    `json:"propose"`
	// Cap is the most proposals this call files (propose only).
	Cap       int          `json:"cap,omitempty"`
	Count     int          `json:"count"`
	Misfits   []nodeMisfit `json:"misfits"`
	Truncated bool         `json:"truncated,omitempty"`
	// Proposed and Skipped count the outcomes (propose only).
	Proposed int            `json:"proposed,omitempty"`
	Skipped  map[string]int `json:"skipped,omitempty"`
}

// misfitCap is the proposal cap a call asks for: limit, bounded, else the default.
func misfitCap(limit int) int {
	switch {
	case limit <= 0:
		return misfitProposalCap
	case limit > misfitProposalMax:
		return misfitProposalMax
	}
	return limit
}

// NodeMisfits lists the misfits of the tenant scope key names (only scheme's
// concepts when scheme is set) and, with propose, files at most limit proposals.
// The caller has checked who may run it.
func (d *Document) NodeMisfits(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string, propose bool, limit int) (NodeMisfitsReport, error) {
	ans := NodeMisfitsReport{Mode: nodeStatsModeMisfits, Scheme: scheme, Margin: nodestats.MisfitMargin,
		MinMembers: nodestats.MinMembers, Propose: propose, Misfits: []nodeMisfit{}}
	if propose {
		ans.Cap = misfitCap(limit)
	}
	p, err := d.buildNodeStatsPlan(ctx, key, read, scheme)
	if err != nil {
		return ans, err
	}
	ans.Vectors, ans.Note, ans.EmbedModel = p.model != "", p.note, p.model
	if p.model == "" {
		ans.Note = "misfits need the vector stage, which does not run here: " + p.note
		return ans, nil
	}
	found, err := p.misfits()
	if err != nil {
		return ans, err
	}
	names := map[string]string{}
	for _, n := range p.nodes {
		if n.Kind == nodeKindConcept {
			names[n.ID] = n.Name
		}
	}
	ans.Count = len(found)
	for _, m := range found {
		title := ""
		if sd := p.docs[m.MemberID]; sd != nil {
			title = sd.title
		}
		ans.Misfits = append(ans.Misfits, nodeMisfit{DocumentID: m.MemberID, Title: title,
			NodeID: m.NodeID, Node: names[m.NodeID], NearestNodeID: m.NearestNodeID, Nearest: names[m.NearestNodeID],
			OwnSimilarity: m.OwnSimilarity, NearestSimilarity: m.NearestSimilarity, Margin: m.NearestSimilarity - m.OwnSimilarity})
	}
	if propose {
		ans.Skipped = map[string]int{}
		if err := d.proposeMisfits(ctx, key, p, &ans); err != nil {
			return ans, err
		}
	}
	if len(ans.Misfits) > misfitReportCap {
		ans.Misfits, ans.Truncated = ans.Misfits[:misfitReportCap], true
	}
	return ans, nil
}

// misfits compares each sibling group of the plan's concepts, largest margin first
// (then node, then document, so the order is stable).
func (p *nodeStatsPlan) misfits() ([]nodestats.Misfit, error) {
	groups := map[string][]nodestats.Node{}
	var order []string
	for _, n := range p.nodes {
		if n.Kind != nodeKindConcept {
			continue
		}
		node := nodestats.Node{ID: n.ID}
		for _, id := range n.members {
			if v, ok := p.vecs[id]; ok {
				node.Members = append(node.Members, nodestats.Member{ID: id, Vec: v})
			}
		}
		if _, seen := groups[n.group]; !seen {
			order = append(order, n.group)
		}
		groups[n.group] = append(groups[n.group], node)
	}
	var out []nodestats.Misfit
	for _, g := range order {
		if len(groups[g]) < 2 {
			continue
		}
		found, err := nodestats.Misfits(groups[g], nodestats.MisfitMargin, nodestats.MinMembers)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		mi, mj := out[i].NearestSimilarity-out[i].OwnSimilarity, out[j].NearestSimilarity-out[j].OwnSimilarity
		if mi != mj {
			return mi > mj
		}
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].MemberID < out[j].MemberID
	})
	return out, nil
}

// proposeMisfits files the proposals, in the answer's order, until the cap.
// Every read it needs is made before the first proposal; each proposal is its own
// propose_statement call, made after the reads (SQLite has one connection).
func (d *Document) proposeMisfits(ctx context.Context, key sqlmem.ScopeKey, p *nodeStatsPlan, ans *NodeMisfitsReport) error {
	var docs, targets []string
	seenDoc, seenTarget := map[string]bool{}, map[string]bool{}
	for _, m := range ans.Misfits {
		if !seenDoc[m.DocumentID] {
			seenDoc[m.DocumentID] = true
			docs = append(docs, m.DocumentID)
		}
		if !seenTarget[m.NearestNodeID] {
			seenTarget[m.NearestNodeID] = true
			targets = append(targets, m.NearestNodeID)
		}
	}
	blocked, err := d.misfitBlockedPlacements(ctx, key, docs, targets)
	if err != nil {
		return err
	}
	rootTitles, err := d.rootTitles(ctx, key, p, docs)
	if err != nil {
		return err
	}
	filed := map[string]bool{}
	for i := range ans.Misfits {
		m := &ans.Misfits[i]
		pair := m.DocumentID + "\x00" + m.NearestNodeID
		switch {
		case blocked[pair] != "":
			m.Skipped = blocked[pair]
		case filed[pair]:
			m.Skipped = "duplicate"
		case ans.Proposed >= ans.Cap:
			m.Skipped = "cap"
		}
		if m.Skipped != "" {
			ans.Skipped[m.Skipped]++
			continue
		}
		filed[pair] = true
		d.proposeMisfit(ctx, p, m, rootTitles[m.DocumentID])
		if m.Skipped != "" {
			ans.Skipped[strings.SplitN(m.Skipped, ":", 2)[0]]++
			continue
		}
		ans.Proposed++
	}
	return nil
}

// proposeMisfit files one misfit's placement through propose_statement and records
// the outcome on it. Confidence is the margin (capped at 1): the measured signal of
// how clearly the document sits nearer the sibling (P0 margin check, 2026-11-17).
func (d *Document) proposeMisfit(ctx context.Context, p *nodeStatsPlan, m *nodeMisfit, rootTitle string) {
	root := ""
	if sd := p.docs[m.DocumentID]; sd != nil {
		root = sd.root
	}
	conf := math.Min(1, math.Max(0, m.Margin))
	in := docInput{
		Op: "propose_statement", Scope: "tenant", FromID: root, ToID: m.NearestNodeID, Kind: "subject",
		Confidence: &conf, edgeMethod: misfitMethod,
		Attributes: map[string]any{"misfit_of": m.NodeID, "margin": m.Margin,
			"own_similarity": m.OwnSimilarity, "nearest_similarity": m.NearestSimilarity},
		Evidence: misfitEvidence(root, rootTitle),
	}
	res, err := d.executeInput(ctx, in, nil)
	if err != nil || res.IsError {
		code := "error"
		if err == nil && res.Error != nil {
			code = string(res.Error.Category)
		}
		m.Skipped = "refused:" + code
		if err == nil {
			m.Detail = res.Text
		}
		return
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	m.StatementID, m.Status = out.ID, out.Status
}

// misfitEvidence quotes the document's title from its root chunk: a document's
// name lives there and in no body, and an annotation brief is no evidence source
// (RFC GM §5.1). A root with no title files no evidence.
func misfitEvidence(root, title string) []edgeEvidenceIn {
	t := strings.TrimSpace(title)
	if root == "" || t == "" {
		return nil
	}
	if utf8.RuneCountInString(t) > edgeEvidenceQuoteMax {
		t, _ = cutRunes(t, edgeEvidenceQuoteMax)
	}
	return []edgeEvidenceIn{{ChunkID: root, Quote: t, Part: edgeEvidencePartTitle}}
}

// rootTitles is each document's root chunk title.
func (d *Document) rootTitles(ctx context.Context, key sqlmem.ScopeKey, p *nodeStatsPlan, docs []string) (map[string]string, error) {
	docOfRoot := map[string]string{}
	var roots []string
	for _, id := range docs {
		if sd := p.docs[id]; sd != nil && sd.root != "" {
			docOfRoot[sd.root] = id
			roots = append(roots, sd.root)
		}
	}
	out := map[string]string{}
	for start := 0; start < len(roots); start += nodeStatsDocBatch {
		ph, args := inPlaceholders(roots[start:min(start+nodeStatsDocBatch, len(roots))])
		res, err := d.query(ctx, key, `SELECT id, title FROM chunks WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		if res.Truncated {
			return nil, fmt.Errorf("node_stats misfits: a title batch was cut short")
		}
		for _, r := range res.Rows {
			out[docOfRoot[asStr(r[0])]] = asStr(r[1])
		}
	}
	return out, nil
}

// misfitBlockedPlacements is, per document and target concept ("doc\x00concept"),
// why no proposal may be filed: "rejected" when a placement of any chunk of the
// document under that concept (a subject statement, or one of a declared sub-kind)
// was ever rejected, by anyone; "exists" when one is proposed or live. Read for
// every pair the answer may file, before any proposal is written.
func (d *Document) misfitBlockedPlacements(ctx context.Context, key sqlmem.ScopeKey, docs, targets []string) (map[string]string, error) {
	out := map[string]string{}
	if len(docs) == 0 || len(targets) == 0 {
		return out, nil
	}
	kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
	kph, kargs := inPlaceholders(kinds)
	tph, targs := inPlaceholders(targets)
	for start := 0; start < len(docs); start += nodeStatsDocBatch {
		dph, dargs := inPlaceholders(docs[start:min(start+nodeStatsDocBatch, len(docs))])
		args := append(append(append([]any{}, kargs...), targs...), dargs...)
		res, err := d.query(ctx, key, `SELECT c.document_id, e.to_id, e.status, CASE WHEN e.expired_at IS NULL THEN 0 ELSE 1 END
			FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
			WHERE e.kind IN (`+kph+`) AND e.to_id IN (`+tph+`) AND c.document_id IN (`+dph+`)`, args...)
		if err != nil {
			return nil, err
		}
		if res.Truncated {
			return nil, fmt.Errorf("node_stats misfits: the existing placements were cut short")
		}
		for _, r := range res.Rows {
			pair := asStr(r[0]) + "\x00" + asStr(r[1])
			switch status, expired := asStr(r[2]), asInt(r[3]) == 1; {
			case status == edgeStatusRejected:
				out[pair] = "rejected"
			case !expired && (status == edgeStatusProposed || status == edgeStatusAsserted) && out[pair] == "":
				out[pair] = "exists"
			}
		}
	}
	return out, nil
}
