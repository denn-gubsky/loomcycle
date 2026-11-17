package builtin

// document_annotation_nodes.go — an annotation on every node of every confirmed
// taxonomy (RFC GM §4, D33; plan P4.2).
//
// THREE LEVELS join branch, document and path: scheme (a concept scheme as a whole),
// concept (one concept, at any depth) and class (a document class the ontology
// declares). The record is the same record (brief, abstract, provenance, manual,
// stale), kept on the node's own chunk in the ontology document. A seed class (the
// standard Document class, unless the tenant overrides it) has no chunk and so no
// annotation: it is refused by name with standard_class, and the annotator counts it.
//
// THREE CASES (RFC GM §4.1), which annotation_inputs reports as `basis`:
//
//	written   at least nodestats.MinMembers members: the full request — definition,
//	          member count, the members' stored statuses, the class histogram, the
//	          narrower nodes' records, exemplars, nearest siblings, distinctive terms.
//	thin      one or two members: the definition and those members' own briefs.
//	declared  no member: annotation_inputs composes the text from the definition and
//	          the annotator writes it with no model call.
//
// The stored record carries its basis. A declared record covers nothing: it is shown
// to anyone who may read the ontology. A written or thin one is computed from every
// member (the count, the histogram, the terms), so it is shown only to a caller who
// may read every CURRENT member, and — to a caller with any read limit — only while
// it is fresh: a stale record may have been written from a member that has since
// left, which this caller might not be allowed to read (RFC GM D8 (i), §7). No id
// list is stored; the check runs the membership query at read time.
//
// THE HASH covers what the request carries (RFC GM §4.3, D5): the definition (and
// the scheme header), the basis, the member set, each exemplar's title and stored
// annotation text, the narrower nodes' records, the sibling names, the terms, the
// class histogram, the language and the prompt version. NOT the members' statuses
// (the member's own annotation changes when its status does) and nothing per period
// (D31): a new period marks no node stale. A non-exemplar member's text is not in it,
// so editing one marks nothing stale until the statistics pick a new exemplar.
//
// NOT INDEXED. Node annotations get no search index row in this phase, so no search
// returns them; P4.3 adds the opt-in search leg (RFC GM §6.1, §7).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	annotationLevelScheme  = "scheme"
	annotationLevelConcept = "concept"
	annotationLevelClass   = "class"

	annotationBasisWritten  = "written"
	annotationBasisThin     = "thin"
	annotationBasisDeclared = "declared"

	// nodeAnnotationCanonVersion is the version of the node hash's form; a change to
	// what it covers bumps it, which marks every node record stale on purpose.
	nodeAnnotationCanonVersion = "node_annotation/1"
	// nodeAnnotationNarrowerCap and nodeAnnotationSiblingCap bound how many narrower
	// nodes and siblings one request names.
	nodeAnnotationNarrowerCap = 30
	nodeAnnotationSiblingCap  = 10
	nodeAnnotationClassCap    = 20
	// nodeAnnotationTopicsCap bounds the topics a scheme's declared abstract names.
	nodeAnnotationTopicsCap = 12
)

// nodeAnnotationLevels are the node levels, in the order a record listing shows them.
var nodeAnnotationLevels = []string{annotationLevelScheme, annotationLevelConcept, annotationLevelClass}

func isNodeAnnotationLevel(level string) bool {
	return level == annotationLevelScheme || level == annotationLevelConcept || level == annotationLevelClass
}

// annotationNode is one node of the taxonomy an annotation is of.
type annotationNode struct {
	level, chunkID, name, meaning string
	scheme                        *memrank.OntologyScheme
	concept                       *memrank.OntologyConcept
	class                         *describedClass
	// def is the node's definition as hashed; place the names above it, top first.
	def   string
	place []string
	// statKey is the node_stats key ("" for a scheme, which has no row); ownGroup is
	// the statNode.group it carries (its siblings carry it too); narrower are the
	// node_stats keys of its narrower nodes, in document order.
	statKey, ownGroup string
	narrower          []string
	members           []string // sorted document ids
}

// nodeMemberBrief is a member document as a node request shows it.
type nodeMemberBrief struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Class      string `json:"class,omitempty"`
	Brief      string `json:"brief,omitempty"`
	Abstract   string `json:"abstract,omitempty"`
}

// nodeNarrower is a narrower node with its own record, when it has one.
type nodeNarrower struct {
	Name     string `json:"name"`
	Members  int    `json:"members"`
	Brief    string `json:"brief,omitempty"`
	Abstract string `json:"abstract,omitempty"`
}

// nodeSchemeHeader is a scheme's header (RFC GM D14), shown with every node of it.
type nodeSchemeHeader struct {
	Name     string   `json:"name"`
	Meaning  string   `json:"meaning,omitempty"`
	Domain   string   `json:"domain,omitempty"`
	Projects []string `json:"projects,omitempty"`
	Origin   string   `json:"origin,omitempty"`
	Source   string   `json:"source,omitempty"`
	Version  string   `json:"version,omitempty"`
}

// nodeComposed is the text a declared node's annotation is: written by the runtime.
type nodeComposed struct {
	Brief    string `json:"brief"`
	Abstract string `json:"abstract"`
}

// nodeAnnotationInputs is what a node's annotation is written from, and its hash.
type nodeAnnotationInputs struct {
	ChunkID    string            `json:"chunk_id"`
	DocumentID string            `json:"document_id"`
	Level      string            `json:"level"`
	Title      string            `json:"title"`
	Meaning    string            `json:"meaning,omitempty"`
	Place      []string          `json:"place"`
	Scheme     *nodeSchemeHeader `json:"scheme,omitempty"`
	Basis      string            `json:"basis"`
	Members    int               `json:"members"`
	MinMembers int               `json:"min_members"`
	// MemberStatuses counts the members by their stored status ("" as "none"), most
	// first: the request's status line, so a brief does not call shipped work a
	// proposal. Not hashed.
	MemberStatuses []folderCount     `json:"member_statuses"`
	MemberClasses  []folderCount     `json:"member_classes,omitempty"`
	Narrower       []nodeNarrower    `json:"narrower,omitempty"`
	NarrowerMore   int               `json:"narrower_more,omitempty"`
	Exemplars      []nodeMemberBrief `json:"exemplars,omitempty"`
	// ExemplarsFrom: statistics (the members nearest the centroid), sample (the first
	// members by id, where no statistics are stored) or members (a thin node's every
	// member).
	ExemplarsFrom string   `json:"exemplars_from,omitempty"`
	Siblings      []string `json:"siblings,omitempty"`
	Terms         []string `json:"terms,omitempty"`
	// Statistics is the state of the node's node_stats row: fresh, stale, missing,
	// counts_only (no vectors on this store) or none (a scheme has no row).
	Statistics    string            `json:"statistics"`
	Composed      *nodeComposed     `json:"composed,omitempty"`
	Language      string            `json:"language"`
	PromptVersion int               `json:"prompt_version"`
	InputsSHA256  string            `json:"inputs_sha256"`
	Record        *annotationRecord `json:"record,omitempty"`
	Stale         bool              `json:"stale"`
}

// nodeAnnotationCanon is the hashed form; every list in it is in a fixed order.
type nodeAnnotationCanon struct {
	V             string   `json:"v"`
	Level         string   `json:"level"`
	Def           string   `json:"def"`
	Basis         string   `json:"basis"`
	Language      string   `json:"language"`
	PromptVersion int      `json:"prompt_version"`
	Members       string   `json:"members"`
	Exemplars     []string `json:"exemplars"`
	Narrower      []string `json:"narrower"`
	Siblings      []string `json:"siblings"`
	Terms         []string `json:"terms"`
	Classes       []string `json:"classes"`
}

// nodeRefusal is a refused node target.
type nodeRefusal struct{ r tools.Result }

func (e *nodeRefusal) Error() string { return e.r.Text }

// nodePlan is the node statistics plan of the ontology in force, read once per scope.
func (s *annotationScope) nodePlan(ctx context.Context) (*nodeStatsPlan, error) {
	if s.plan != nil {
		return s.plan, nil
	}
	onto := s.ontology(ctx)
	if onto == nil {
		return nil, fmt.Errorf("no tenant ontology")
	}
	p, err := s.d.buildNodeStatsPlan(ctx, s.key, *onto, "")
	if err != nil {
		return nil, err
	}
	s.plan = p
	return p, nil
}

// resolveNode finds the node (chunkID, level) names in the confirmed ontology: a
// class may be named by name instead of its chunk. A refusal is a *nodeRefusal.
func (s *annotationScope) resolveNode(ctx context.Context, op, chunkID, level, name string) (*annotationNode, error) {
	refuse := func(r tools.Result) (*annotationNode, error) { return nil, &nodeRefusal{r: r} }
	onto := s.ontology(ctx)
	if onto == nil {
		return refuse(errBusiness(op+": the tenant has no ontology, so there is no taxonomy to annotate", ""))
	}
	if !onto.Confirmed {
		return refuse(errBusiness(op+": the tenant ontology is a draft: its schemes and classes are not in force",
			"Annotate its nodes once an operator confirms it."))
	}
	if chunkID == "" && (level != annotationLevelClass || strings.TrimSpace(name) == "") {
		return refuse(errValidation(op+": missing required field: id (the node's chunk in the ontology document)",
			"Pass the chunk_id describe_ontology gives the scheme or concept; a class may be named by name."))
	}
	n := &annotationNode{level: level}
	switch level {
	case annotationLevelScheme, annotationLevelConcept:
		for i := range onto.Schemes {
			sc := &onto.Schemes[i]
			if level == annotationLevelScheme && sc.ChunkID == chunkID {
				n.scheme, n.chunkID, n.name, n.meaning = sc, sc.ChunkID, sc.Name, sc.Meaning
			}
			for j := range sc.Concepts {
				if c := &sc.Concepts[j]; level == annotationLevelConcept && c.ChunkID == chunkID && c.Tag != "" {
					n.scheme, n.concept, n.chunkID, n.name, n.meaning = sc, c, c.ChunkID, c.Name, c.Meaning
				}
			}
		}
		if n.chunkID == "" {
			return refuse(errNotFound(fmt.Sprintf("%s: chunk %s is not a %s of the ontology in force", op, chunkID, level),
				"Take the chunk_id from describe_ontology's schemes."))
		}
	case annotationLevelClass:
		if refused, err := s.resolveClassNode(ctx, op, n, chunkID, name); refused != nil || err != nil {
			if refused != nil {
				return refuse(*refused)
			}
			return nil, err
		}
	}
	p, err := s.nodePlan(ctx)
	if err != nil {
		return nil, err
	}
	s.fillNode(ctx, n, p)
	return n, nil
}

// resolveClassNode fills n with the document class chunkID or name names.
func (s *annotationScope) resolveClassNode(ctx context.Context, op string, n *annotationNode, chunkID, name string) (*tools.Result, error) {
	fail := func(r tools.Result) (*tools.Result, error) { return &r, nil }
	onto := s.ontology(ctx)
	if chunkID != "" {
		row, ok, err := s.d.getChunkRow(ctx, s.key, chunkID)
		if err != nil {
			return nil, err
		}
		if !ok || row.DocumentID != onto.DocumentID {
			return fail(errNotFound(op+": chunk "+chunkID+" is not a class of the ontology in force", ""))
		}
		name = memrank.TrimOntologyName(row.Title)
	}
	var cls *describedClass
	described := describeOntologyRead(*onto)
	for i := range described.Classes {
		if c := &described.Classes[i]; strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			cls = c
			break
		}
	}
	if cls == nil || cls.Kind != "document" {
		return fail(errNotFound(fmt.Sprintf("%s: %q is not a document class of the ontology in force", op, name),
			"Name a class describe_ontology lists with kind document; entity classes get no annotation (RFC GM D6)."))
	}
	id, err := s.d.classChunkID(ctx, s.key, *onto, cls.Name)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return fail(errBusiness(op+": standard_class: "+cls.Name+" is a standard class with no entry in the ontology document, so it has no chunk to annotate",
			"Declare the class in the ontology to annotate it."))
	}
	if chunkID != "" && id != chunkID {
		return fail(errNotFound(op+": chunk "+chunkID+" is not the entry of class "+cls.Name, ""))
	}
	n.class, n.chunkID, n.name, n.meaning = cls, id, cls.Name, cls.Meaning
	return nil, nil
}

// fillNode sets n's definition, place, groups and members from the plan.
func (s *annotationScope) fillNode(ctx context.Context, n *annotationNode, p *nodeStatsPlan) {
	byKey := map[string]statNode{}
	for _, sn := range p.nodes {
		byKey[nodeStatsKey(sn.Kind, sn.ID)] = sn
	}
	narrowerByGroup := func(group string) {
		for _, sn := range p.nodes {
			if sn.group == group {
				n.narrower = append(n.narrower, nodeStatsKey(sn.Kind, sn.ID))
			}
		}
	}
	header := ""
	if n.scheme != nil {
		sc := n.scheme
		header = fmt.Sprintf("scheme %s\nmeaning %s\ndomain %s\nprojects %s\norigin %s\nsource %s\nversion %s",
			sc.Name, sc.Meaning, sc.Domain, strings.Join(sc.Projects, ","), sc.Origin, sc.Source, sc.Version)
	}
	switch n.level {
	case annotationLevelScheme:
		// The top-level concepts by their place, not by a tag prefix: a tag's first
		// segment is the scheme's name made a tag, which need not be the name.
		n.def, n.place = header, []string{}
		members := map[string]bool{}
		for _, c := range n.scheme.Concepts {
			if c.Parent != "" || c.ChunkID == "" || c.Tag == "" {
				continue
			}
			k := nodeStatsKey(nodeKindConcept, c.ChunkID)
			n.narrower = append(n.narrower, k)
			for _, id := range byKey[k].members {
				members[id] = true
			}
		}
		n.members = sortedSet(members)
	case annotationLevelConcept:
		n.statKey = nodeStatsKey(nodeKindConcept, n.chunkID)
		sn := byKey[n.statKey]
		n.def, n.members, n.ownGroup = header+"\n"+sn.def, sn.members, sn.group
		narrowerByGroup("concept\x00" + n.concept.Tag)
		n.place = append([]string{n.scheme.Name}, n.concept.Path[:max(len(n.concept.Path)-1, 0)]...)
	case annotationLevelClass:
		n.statKey = nodeStatsKey(nodeKindClass, n.class.Name)
		sn := byKey[n.statKey]
		n.def, n.members, n.ownGroup = sn.def, sn.members, sn.group
		narrowerByGroup("class\x00" + n.class.Name)
		parentOf := map[string]string{}
		for _, c := range describeOntologyRead(*s.ontology(ctx)).Classes {
			parentOf[c.Name] = c.Parent
		}
		n.place = []string{}
		for q, depth := n.class.Parent, 0; q != "" && depth <= memrank.OntologyMaxDepth; q, depth = parentOf[q], depth+1 {
			n.place = append([]string{q}, n.place...)
		}
	}
	if n.members == nil {
		n.members = []string{}
	}
}

// migrateAnnotationBasis adds the basis a node record was written on (written,
// thin or declared) to a scope whose chunk_annotations predates it. NULL on every
// other level's record, which has no basis.
func (d *Document) migrateAnnotationBasis(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.tableHasColumn(ctx, key, "chunk_annotations", "basis") {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_annotations ADD COLUMN basis TEXT`)
}

// basisOf is the case of RFC GM §4.1 a node with this many members is in.
func basisOf(members int) string {
	switch {
	case members == 0:
		return annotationBasisDeclared
	case members < nodestats.MinMembers:
		return annotationBasisThin
	}
	return annotationBasisWritten
}

// nodeInputs computes what n's annotation is written from, and its hash.
func (s *annotationScope) nodeInputs(ctx context.Context, n *annotationNode, promptVersion int) (nodeAnnotationInputs, error) {
	onto := s.ontology(ctx)
	p, err := s.nodePlan(ctx)
	if err != nil {
		return nodeAnnotationInputs{}, err
	}
	in := nodeAnnotationInputs{ChunkID: n.chunkID, DocumentID: onto.DocumentID, Level: n.level, Title: n.name,
		Meaning: n.meaning, Place: n.place, Basis: basisOf(len(n.members)), Members: len(n.members),
		MinMembers: nodestats.MinMembers, MemberStatuses: []folderCount{}, Statistics: "none", PromptVersion: promptVersion}
	if n.scheme != nil {
		sc := n.scheme
		in.Scheme = &nodeSchemeHeader{Name: sc.Name, Meaning: sc.Meaning, Domain: sc.Domain, Projects: sc.Projects,
			Origin: sc.Origin, Source: sc.Source, Version: sc.Version}
	}
	if in.Language, err = s.language(ctx, onto.DocumentID, onto.RootChunkID); err != nil {
		return in, err
	}
	canon := nodeAnnotationCanon{V: nodeAnnotationCanonVersion, Level: n.level, Def: n.def, Basis: in.Basis,
		Language: in.Language, PromptVersion: promptVersion, Members: membersHash(n.members),
		Exemplars: []string{}, Narrower: []string{}, Siblings: []string{}, Terms: []string{}, Classes: []string{}}

	if in.Narrower, in.NarrowerMore, canon.Narrower, err = s.nodeNarrower(ctx, n, p); err != nil {
		return in, err
	}
	if n.statKey != "" {
		in.Statistics = "missing"
	}
	row, hasRow := p.stored[n.statKey]
	usable := hasRow && row.memberSHA == membersHash(n.members)
	if hasRow {
		switch {
		case !usable:
			in.Statistics = "stale"
		case row.inputs != p.nodeHash(statNodeOf(p, n.statKey)):
			in.Statistics = "stale"
		case len(row.exemplars) == 0:
			in.Statistics = "counts_only"
		default:
			in.Statistics = "fresh"
		}
	}

	if in.Basis != annotationBasisDeclared {
		if err := s.nodeMemberFacts(ctx, n, p, &in, &canon); err != nil {
			return in, err
		}
		var exIDs []string
		switch {
		case in.Basis == annotationBasisThin:
			exIDs, in.ExemplarsFrom = n.members, "members"
		case usable && len(row.exemplars) > 0:
			for _, e := range row.exemplars {
				exIDs = append(exIDs, e.ID)
			}
			in.ExemplarsFrom = "statistics"
		default:
			exIDs, in.ExemplarsFrom = sampleMembers(n.members, p.docs), "sample"
		}
		missing := []string{}
		for _, id := range exIDs {
			if _, ok := p.docs[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			if err := s.d.loadStatDocs(ctx, s.key, missing, p.docs); err != nil {
				return in, err
			}
		}
		for _, id := range exIDs {
			sd := p.docs[id]
			if sd == nil {
				continue
			}
			m := nodeMemberBrief{DocumentID: id, Title: sd.title, Class: s.docTypes[id], Brief: sd.brief, Abstract: sd.abstract}
			in.Exemplars = append(in.Exemplars, m)
			canon.Exemplars = append(canon.Exemplars, id+":"+sha256Hex(m.Title+"\x00"+m.Class+"\x00"+m.Brief+"\x00"+m.Abstract))
		}
		if in.Basis == annotationBasisWritten && usable {
			in.Terms = row.terms
			for _, sib := range row.siblings {
				if name := statNodeOf(p, nodeStatsKey(statKind(n.statKey), sib.ID)).Name; name != "" {
					in.Siblings = append(in.Siblings, name)
				}
			}
		}
	}
	if in.Basis == annotationBasisDeclared {
		// The composed text names the narrower nodes and nothing else of them, so
		// that is all its hash covers: a narrower node's rewrite, or a sibling's
		// rename, does not make an empty node stale.
		in.Composed = composeDeclaredNode(n, in)
		// Nothing computed from members either: a declared record is shown to a
		// reader with read limits, so its hash must say nothing about who is under
		// the node (RFC GM D8 (i)). A first member still marks it stale: the
		// node's current hash is then a thin one.
		canon.Basis, canon.Members = "", ""
		canon.Narrower = canon.Narrower[:0]
		for _, nw := range in.Narrower {
			canon.Narrower = append(canon.Narrower, nw.Name)
		}
		raw, _ := json.Marshal(canon)
		in.InputsSHA256 = sha256Hex(string(raw))
		return in, nil
	}
	if len(in.Siblings) == 0 && n.ownGroup != "" {
		for _, sn := range p.nodes {
			if sn.group == n.ownGroup && nodeStatsKey(sn.Kind, sn.ID) != n.statKey && len(in.Siblings) < nodeAnnotationSiblingCap {
				in.Siblings = append(in.Siblings, sn.Name)
			}
		}
	}
	canon.Siblings = append(canon.Siblings, in.Siblings...)
	canon.Terms = append(canon.Terms, in.Terms...)
	raw, _ := json.Marshal(canon)
	in.InputsSHA256 = sha256Hex(string(raw))
	return in, nil
}

// statKind is the kind half of a node_stats key.
func statKind(statKey string) string {
	kind, _, _ := strings.Cut(statKey, "\x00")
	return kind
}

// statNodeOf is the plan's node under key, zero when there is none.
func statNodeOf(p *nodeStatsPlan, key string) statNode {
	for _, sn := range p.nodes {
		if nodeStatsKey(sn.Kind, sn.ID) == key {
			return sn
		}
	}
	return statNode{}
}

// sampleMembers is the exemplars of a node with no stored statistics: its first
// members by id that carry a document annotation, else its first members.
func sampleMembers(members []string, docs map[string]*statDoc) []string {
	var out []string
	for _, id := range members {
		if sd := docs[id]; sd != nil && sd.annotated && len(out) < nodestats.KExemplars {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		out = members[:min(len(members), nodestats.KExemplars)]
	}
	return out
}

// nodeMemberFacts fills the members' status counts (not hashed) and class histogram.
func (s *annotationScope) nodeMemberFacts(ctx context.Context, n *annotationNode, p *nodeStatsPlan, in *nodeAnnotationInputs, canon *nodeAnnotationCanon) error {
	statuses := map[string]int{}
	classes := map[string]int{}
	for start := 0; start < len(n.members); start += nodeStatsDocBatch {
		ph, args := inPlaceholders(n.members[start:min(start+nodeStatsDocBatch, len(n.members))])
		res, err := s.d.query(ctx, s.key, `SELECT id, coalesce(status, ''), coalesce(type, '') FROM documents WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("node annotation: a member batch was cut short")
		}
		for _, r := range res.Rows {
			st := strings.TrimSpace(asStr(r[1]))
			if st == "" {
				st = "none"
			}
			statuses[st]++
			if typ := asStr(r[2]); typ != "" {
				classes[typ]++
			}
			s.docTypes[asStr(r[0])] = asStr(r[2])
		}
	}
	in.MemberStatuses = sortedCounts(statuses, 0)
	in.MemberClasses = sortedCounts(classes, nodeAnnotationClassCap)
	for _, c := range in.MemberClasses {
		canon.Classes = append(canon.Classes, fmt.Sprintf("%s:%d", c.Name, c.Count))
	}
	return nil
}

// nodeNarrower is the narrower nodes of n in document order, each with its own
// record at its level, and their hashed form.
func (s *annotationScope) nodeNarrower(ctx context.Context, n *annotationNode, p *nodeStatsPlan) ([]nodeNarrower, int, []string, error) {
	type kid struct {
		sn      statNode
		chunkID string
	}
	var kids []kid
	level := annotationLevelConcept
	if n.level == annotationLevelClass {
		level = annotationLevelClass
	}
	for _, k := range n.narrower {
		sn := statNodeOf(p, k)
		if sn.ID == "" {
			continue
		}
		id := sn.ID
		if sn.Kind == nodeKindClass {
			var err error
			if id, err = s.d.classChunkID(ctx, s.key, *s.ontology(ctx), sn.Name); err != nil {
				return nil, 0, nil, err
			}
		}
		kids = append(kids, kid{sn: sn, chunkID: id})
	}
	more := 0
	if len(kids) > nodeAnnotationNarrowerCap {
		more, kids = len(kids)-nodeAnnotationNarrowerCap, kids[:nodeAnnotationNarrowerCap]
	}
	var ids []string
	for _, k := range kids {
		if k.chunkID != "" {
			ids = append(ids, k.chunkID)
		}
	}
	recs, err := s.d.annotationRecordsFor(ctx, s.key, ids, level)
	if err != nil {
		return nil, 0, nil, err
	}
	out := make([]nodeNarrower, 0, len(kids))
	canon := []string{}
	for _, k := range kids {
		nw := nodeNarrower{Name: k.sn.Name, Members: len(k.sn.members)}
		h := "none"
		if rec, ok := recs[k.chunkID][level]; ok && k.chunkID != "" {
			nw.Brief, nw.Abstract = rec.Brief, rec.Abstract
			h = sha256Hex(rec.Brief + "\x00" + rec.Abstract)
		}
		out = append(out, nw)
		canon = append(canon, fmt.Sprintf("%s:%d:%s", nw.Name, nw.Members, h))
	}
	if len(out) == 0 {
		out = nil
	}
	return out, more, canon, nil
}

// composeDeclaredNode is a declared node's annotation, written by the runtime from
// the definition alone (RFC GM §4.1, third row): what belongs here, and that nothing
// is held under it yet. In English around the declared meaning, which is kept as
// written.
func composeDeclaredNode(n *annotationNode, in nodeAnnotationInputs) *nodeComposed {
	what := "a topic"
	switch n.level {
	case annotationLevelScheme:
		what = "a classification scheme"
	case annotationLevelClass:
		what = "a class of documents"
	}
	meaning := strings.TrimSpace(n.meaning)
	brief := n.name + " — " + what
	if n.level == annotationLevelConcept {
		brief += " of the " + n.scheme.Name + " scheme"
	}
	if meaning != "" {
		brief = n.name + ": " + meaning
	}
	var b strings.Builder
	b.WriteString(n.name + " is " + what)
	switch {
	case n.level == annotationLevelConcept && len(in.Place) > 1:
		b.WriteString(" in the " + n.scheme.Name + " scheme, under " + strings.Join(in.Place[1:], " › "))
	case n.level == annotationLevelConcept:
		b.WriteString(" in the " + n.scheme.Name + " scheme")
	case n.level == annotationLevelClass && len(in.Place) > 0:
		b.WriteString(", a kind of " + in.Place[len(in.Place)-1])
	}
	if n.level == annotationLevelScheme && in.Scheme != nil {
		if in.Scheme.Domain != "" {
			b.WriteString(" for " + in.Scheme.Domain)
		}
		if len(in.Scheme.Projects) > 0 {
			b.WriteString(", arranging " + strings.Join(in.Scheme.Projects, ", "))
		}
	}
	b.WriteString(".")
	if meaning != "" {
		b.WriteString(" " + strings.TrimSuffix(meaning, ".") + ".")
	}
	if len(in.Narrower) > 0 {
		names := make([]string, 0, nodeAnnotationTopicsCap)
		for i, nw := range in.Narrower {
			if i == nodeAnnotationTopicsCap {
				break
			}
			names = append(names, nw.Name)
		}
		label := " Its narrower topics: "
		if n.level == annotationLevelScheme {
			label = " Its top-level topics: "
		} else if n.level == annotationLevelClass {
			label = " Its narrower classes: "
		}
		b.WriteString(label + strings.Join(names, ", ") + ".")
	}
	if n.level == annotationLevelScheme {
		b.WriteString(" Nothing is placed under it yet.")
	} else {
		b.WriteString(" Nothing is held under it yet.")
	}
	return &nodeComposed{Brief: truncateRunes(brief, maxAnnotationBriefChars), Abstract: truncateRunes(b.String(), maxAnnotationAbstractChars)}
}

// --- ops ---

// resolveNodeAnnotationTarget is resolveAnnotationTarget for a node level: the node's
// chunk in the ontology document, which must be the scope's own (annotations are
// tenant-only, and the ontology is the tenant's).
func (d *Document) resolveNodeAnnotationTarget(ctx context.Context, s *annotationScope, op string, in docInput) (annotationTarget, *tools.Result) {
	fail := func(r tools.Result) (annotationTarget, *tools.Result) { return annotationTarget{}, &r }
	if s.key.Scope != "tenant" {
		return fail(errValidation(op+": a taxonomy node's annotation is kept in the tenant scope", `Pass scope "tenant".`))
	}
	n, err := s.resolveNode(ctx, op, in.ID, in.Level, in.Name)
	if err != nil {
		var nr *nodeRefusal
		if errors.As(err, &nr) {
			return fail(nr.r)
		}
		return fail(errFrom(op+": "+err.Error(), err))
	}
	t, ok, err := s.targetOf(ctx, n.chunkID, in.Level)
	if err != nil {
		return fail(errFrom(op+": "+err.Error(), err))
	}
	if !ok {
		return fail(errNotFound(op+": no such chunk: "+n.chunkID, ""))
	}
	t.node = n
	return t, nil
}

// nodeAnnotationsReadEverything refuses a node annotation's inputs or write to a
// caller whose access-control rules hide a document of the scope: the request is
// computed from every member, as node_stats is.
func (d *Document) nodeAnnotationsReadEverything(ctx context.Context, op string, key sqlmem.ScopeKey) *tools.Result {
	list, err := d.readableDocList(ctx, d.newDocACL(ctx, key))
	if err != nil {
		r := errFrom(op+": "+err.Error(), err)
		return &r
	}
	if !readsEverything(list) {
		r := errPermission(op+": permission: a node's annotation is written from every member, and this caller's access-control rules hide some documents",
			"Run it as an operator, or from an agent whose rules let it read the whole tenant scope.")
		return &r
	}
	return nil
}

func (d *Document) nodeAnnotationInputsOp(ctx context.Context, s *annotationScope, t annotationTarget, promptVersion int) (tools.Result, error) {
	const op = "annotation_inputs"
	if r := d.nodeAnnotationsReadEverything(ctx, op, s.key); r != nil {
		return *r, nil
	}
	got, err := s.nodeInputs(ctx, t.node, promptVersion)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	recs, err := d.annotationRecordsFor(ctx, s.key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	got.Stale = true
	if rec, ok := recs[t.chunk.ID][t.level]; ok {
		stale := rec.InputsSHA256 != got.InputsSHA256
		rec.Stale = &stale
		got.Record, got.Stale = &rec, stale
	}
	return jsonResult(got)
}

// setNodeAnnotation writes a node's record. Its basis is the case the node is in
// now, whoever wrote the text. It gets no search index row (P4.3).
func (d *Document) setNodeAnnotation(ctx context.Context, s *annotationScope, t annotationTarget, in docInput, brief, abstract string) (tools.Result, error) {
	const op = "set_annotation"
	if r := d.nodeAnnotationsReadEverything(ctx, op, s.key); r != nil {
		return *r, nil
	}
	cur, err := s.nodeInputs(ctx, t.node, in.PromptVersion)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if in.InputsSHA256 != "" && in.InputsSHA256 != cur.InputsSHA256 {
		return errBusiness(op+": annotation_inputs_changed: the node's inputs changed since they were read",
			"Read them again with annotation_inputs and write an annotation of the current node."), nil
	}
	existing, err := d.annotationRecordsFor(ctx, s.key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if prev, ok := existing[t.chunk.ID][t.level]; ok && prev.Manual && !in.Manual {
		return errBusiness(op+": manual_record: an operator wrote this annotation; it is not overwritten",
			"Leave it, or ask an operator to delete it first."), nil
	}
	language := in.Language
	if language == "" {
		language = cur.Language
	}
	manual := 0
	if in.Manual {
		manual = 1
	}
	var model any
	if in.Model != "" {
		model = in.Model
	}
	if err := d.exec(ctx, s.key, `INSERT INTO chunk_annotations (chunk_id, level, brief, abstract, language, class,
		ontology_version, inputs_sha256, prompt_version, model, manual, written_at, written_by, basis)
		VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chunk_id, level) DO UPDATE SET brief = excluded.brief, abstract = excluded.abstract,
		language = excluded.language, class = excluded.class, ontology_version = excluded.ontology_version,
		inputs_sha256 = excluded.inputs_sha256, prompt_version = excluded.prompt_version, model = excluded.model,
		manual = excluded.manual, written_at = excluded.written_at, written_by = excluded.written_by, basis = excluded.basis`,
		t.chunk.ID, t.level, brief, abstract, language, cur.InputsSHA256, in.PromptVersion, model, manual,
		time.Now().UnixNano(), annotationWriter(ctx), cur.Basis); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	recs, err := d.annotationRecordsFor(ctx, s.key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rec := recs[t.chunk.ID][t.level]
	notStale := false
	rec.Stale = &notStale
	return annotationWriteResult(ctx, rec, nil)
}

// readsLimited reports whether the caller acl stands for has any read limit,
// deriving its read list once per scope.
func (s *annotationScope) readsLimited(ctx context.Context, acl *docACL) (bool, error) {
	if !s.listRead {
		list, err := s.d.readableDocList(ctx, acl)
		if err != nil {
			return true, err
		}
		s.listRead = true
		if !readsEverything(list) {
			s.readList = list
		}
	}
	return s.readList != nil, nil
}

// nodeRecordShown applies RFC GM D8 (i) to rec, a node record whose staleness is
// stale: whether the caller acl stands for sees it, and the staleness it is told.
// A declared record is shown to anyone; any other only to a caller who reads every
// current member and, with any read limit, only while it is fresh. A caller with a
// read limit is never told a node record's staleness (reported is nil): a declared
// record goes stale when a first member is filed under it, which may be a document
// this caller cannot read.
func (s *annotationScope) nodeRecordShown(ctx context.Context, acl *docACL, rec annotationRecord, stale bool) (shown bool, reported *bool, err error) {
	limited, err := s.readsLimited(ctx, acl)
	if err != nil {
		return false, nil, err
	}
	if !limited {
		return true, &stale, nil
	}
	if rec.Basis == annotationBasisDeclared {
		return true, nil, nil
	}
	if withheld, err := s.nodeRecordWithheld(ctx, rec, stale); err != nil || withheld {
		return false, nil, err
	}
	return true, nil, nil
}

// nodeRecordWithheld reports whether rec, a written or thin node record, is
// withheld from a caller with a read limit (s.readList): it is unless the record
// is fresh and the caller reads every current member.
func (s *annotationScope) nodeRecordWithheld(ctx context.Context, rec annotationRecord, stale bool) (bool, error) {
	if stale {
		return true, nil
	}
	n, err := s.resolveNode(ctx, "get_annotation", rec.ChunkID, rec.Level, "")
	if err != nil {
		var nr *nodeRefusal
		if errors.As(err, &nr) {
			return true, nil
		}
		return true, err
	}
	for _, id := range n.members {
		if !s.readList.readable(id) {
			return true, nil
		}
	}
	return false, nil
}

// nodeStale is a node record's staleness: its node's current inputs under the
// prompt version it was written with. A record whose node is no longer in force is
// stale.
func (s *annotationScope) nodeStale(ctx context.Context, rec annotationRecord) (bool, error) {
	n, err := s.resolveNode(ctx, "get_annotation", rec.ChunkID, rec.Level, "")
	if err != nil {
		var nr *nodeRefusal
		if errors.As(err, &nr) {
			return true, nil
		}
		return false, err
	}
	in, err := s.nodeInputs(ctx, n, rec.PromptVersion)
	if err != nil {
		return false, err
	}
	return in.InputsSHA256 != rec.InputsSHA256, nil
}
