package builtin

// document_classify.go — Document op=classify and op=accept_classification (RFC FO P5a).
//
// classify says what a document is, from the confirmed ontology, without a model:
//
//  1. CLASS RULES. Each document class's @title_pattern against the document's title,
//     its import source title and its first section titles; a class with only an
//     @identifier_pattern, against the title. The pattern's named groups fill the
//     class's fields, so the class's @identity makes the work key. An issuer is an
//     Instances entry whose name or @alt label occurs in the title or first sections.
//  2. NEIGHBOURHOOD VOTE. `related` over the first section, inside the document's own
//     Path folder, among documents the caller may read that already carry a declared
//     class: their classes and their concepts, with the share that agrees.
//
// The model step (RFC §5 step 3) is P5b's; without it a document neither step settles
// is `unclassified`.
//
// WHAT IS WRITTEN (owner decisions, RFC FO §10j). One class matching by rule — a class
// and its own ancestor matching count as the more specific — is written LIVE: the root's
// type, the identity fields its pattern filled, and issued_by when exactly one instance
// is named. Everything else is a PROPOSAL: kept in the root's `classification` field and,
// in the tenant scope, filed as proposed statements; accept_classification makes it live.
// A class set by hand is never replaced.
//
// WHY STATEMENTS ONLY IN THE TENANT SCOPE. Concepts and the subjects instances are
// adopted as live in the tenant scope, and a statement never crosses scopes. Elsewhere a
// concept is carried by its mirror tag — the same rule the concept search filter reads
// (#2255) — so a proposed concept waits in the record and becomes the tag on accept.

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// edgeMethodClassify is the method of a statement classify writes, so the review
	// queue can tell it from a person's and an operator can find every one it made.
	edgeMethodClassify = "classify"
	// classifyField is the root field that holds a document's classification.
	classifyField = "classification"

	// How classify found a statement, recorded on it as attributes.classified_by:
	// its class rules or an instance pattern (rules), the neighbours' own
	// asserted statements (vote), or the model (model). A review policy's
	// rule:matched asks it — the first two are rules, the last is not.
	classifiedByRules = "rules"
	classifiedByVote  = "vote"
	classifiedByModel = "model"

	classifySections     = 5    // first sections read for rules and as the vote's seed
	classifyBodyChars    = 2000 // of the first sections' text searched for an issuer
	classifyNeighbours   = 20   // related hits the vote looks at
	classifyMinVotes     = 2    // a vote needs at least this many classified neighbours
	classifyMinShare     = 0.5  // and this share of them agreeing
	classifyWriteDefault = "propose"
	classifyWriteReport  = "report"
)

// classification is what classify keeps on the root: what it applied, and what waits
// for an operator.
type classification struct {
	By       string `json:"by"`
	At       int64  `json:"at"`
	Applied  string `json:"applied_type,omitempty"`
	Proposed string `json:"proposed_type,omitempty"`
	// ProposedFields are the identity fields the proposed class's pattern filled.
	ProposedFields map[string]any `json:"proposed_fields,omitempty"`
	// ProposedTags are concepts proposed outside the tenant scope, as their mirror tags.
	ProposedTags []string `json:"proposed_tags,omitempty"`
	// Statements are the proposed statements filed with this classification.
	Statements []string `json:"statements,omitempty"`
}

type classifyCandidate struct {
	Class  string         `json:"class"`
	Quote  string         `json:"quote,omitempty"`
	From   string         `json:"from,omitempty"` // title | source_title | section
	Groups map[string]any `json:"groups,omitempty"`
	Share  float64        `json:"share,omitempty"`
	Votes  int            `json:"votes,omitempty"`
}

type classifyStep struct {
	Step string `json:"step"`
	// Method is which model answered the model step: decision or chat.
	Method     string              `json:"method,omitempty"`
	Candidates []classifyCandidate `json:"candidates"`
	Chosen     string              `json:"chosen,omitempty"`
	Neighbours int                 `json:"neighbours,omitempty"`
	Reason     string              `json:"reason,omitempty"`
}

type classifyConcept struct {
	Concept string  `json:"concept"`
	Tag     string  `json:"tag"`
	Kind    string  `json:"kind,omitempty"`
	ChunkID string  `json:"chunk_id,omitempty"`
	Share   float64 `json:"share"`
}

type classifyIssuer struct {
	Instance       string `json:"instance"`
	Quote          string `json:"quote"`
	SubjectChunkID string `json:"subject_chunk_id,omitempty"`
}

type classifyAnswer struct {
	DocumentID string            `json:"document_id"`
	Write      string            `json:"write"`
	Result     string            `json:"result"` // classified | proposed | kept | unclassified
	Class      string            `json:"class,omitempty"`
	Live       bool              `json:"live"`
	WorkKey    string            `json:"work_key,omitempty"`
	Fields     map[string]any    `json:"fields,omitempty"`
	IssuedBy   []classifyIssuer  `json:"issued_by"`
	Concepts   []classifyConcept `json:"concepts"`
	Steps      []classifyStep    `json:"steps"`
	Statements []edgeWriteResult `json:"statements,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// classifyDoc is everything the two steps read about the document.
type classifyDoc struct {
	id, rootID, title, sourceTitle, rootType string
	rootRevision                             int
	fields                                   map[string]any
	record                                   *classification
	sectionIDs, sectionTitles                []string
	text                                     string // title + section titles + first bodies
}

func (d *Document) classify(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	write := in.Write
	if write == "" {
		write = classifyWriteDefault
	}
	if write != classifyWriteDefault && write != classifyWriteReport {
		return errValidation("classify: write must be propose or report", "Omit write to file what classify finds, or pass report to write nothing."), nil
	}
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail("classify: ", err), nil
	}
	cd, found, err := d.classifyRead(ctx, key, mscope, docID)
	if err != nil {
		return errFrom("classify: "+err.Error(), err), nil
	}
	if !found {
		return errNotFound("classify: no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	out := classifyAnswer{DocumentID: docID, Write: write, Result: "unclassified", IssuedBy: []classifyIssuer{}, Concepts: []classifyConcept{}, Steps: []classifyStep{}}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		out.Steps = append(out.Steps, classifyStep{Step: "rules", Candidates: []classifyCandidate{}, Reason: "the tenant ontology is not confirmed; a draft classifies nothing"})
		return jsonResult(out)
	}
	acl := d.newDocACL(ctx, key)
	okey, _, kerr := d.ontologyTenantKey(ctx)
	tenantScope := kerr == nil && okey == key
	if tenantScope && docID == read.DocumentID {
		return errValidation("classify: the ontology document is not classified", "Classify a document the ontology describes."), nil
	}
	classes := map[string]memrank.OntologyTerm{}
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if c.IsDocumentClass {
			classes[strings.ToLower(c.Name)] = c
		}
	}

	// Step 1 — rules.
	rules := classifyRules(classes, cd)
	ruleStep := classifyStep{Step: "rules", Candidates: rules}
	switch len(rules) {
	case 0:
		ruleStep.Reason = "no document class's pattern matched"
	case 1:
		ruleStep.Chosen = rules[0].Class
	default:
		ruleStep.Reason = "more than one unrelated class matched"
	}
	issuers := classifyIssuers(read.Instances, cd.text)

	// Step 2 — the neighbourhood vote.
	vote, concepts, verr := d.classifyVote(ctx, key, mscope, acl, read, classes, cd, tenantScope)
	if verr != nil {
		return errFrom("classify: vote: "+verr.Error(), verr), nil
	}
	conceptsBy := classifiedByVote
	out.Steps = append(out.Steps, ruleStep, vote)
	out.Concepts = concepts

	// Combine.
	var chosen *classifyCandidate
	live := false
	switch {
	case ruleStep.Chosen != "":
		chosen, live = &rules[0], true
		if vote.Chosen != "" && !strings.EqualFold(vote.Chosen, ruleStep.Chosen) {
			out.Warnings = append(out.Warnings, "the neighbourhood votes "+vote.Chosen+", the rules "+ruleStep.Chosen+"; the rules win")
		}
	case vote.Chosen != "" && (len(rules) == 0 || hasCandidate(rules, vote.Chosen)):
		for i := range rules {
			if strings.EqualFold(rules[i].Class, vote.Chosen) {
				chosen = &rules[i]
			}
		}
		if chosen == nil {
			chosen = &classifyCandidate{Class: vote.Chosen}
		}
	}
	if chosen != nil {
		out.Class, out.Fields = chosen.Class, identityGroups(classes[strings.ToLower(chosen.Class)], chosen.Groups)
		if k, ok := memrank.IdentityKey(classes[strings.ToLower(chosen.Class)], out.Fields, cd.title); ok {
			out.WorkKey = k
		}
		out.Result = "proposed"
		if live {
			out.Result = "classified"
		}
	}
	handSet := cd.rootType != "" && (cd.record == nil || !strings.EqualFold(cd.record.Applied, cd.rootType))

	// Step 3 — the model, only where the rules and the vote left the class open (and
	// never over a type set by hand, which nothing here would change). Its answer is
	// always a proposal.
	if chosen == nil && !handSet {
		model := d.classifyModel(ctx, read, classes, rules, cd, concepts)
		out.Steps = append(out.Steps, model.step)
		if model.class != "" {
			chosen = model.candidate
			out.Class, out.Fields = chosen.Class, identityGroups(classes[strings.ToLower(chosen.Class)], chosen.Groups)
			if k, ok := memrank.IdentityKey(classes[strings.ToLower(chosen.Class)], out.Fields, cd.title); ok {
				out.WorkKey = k
			}
			out.Result = "proposed"
		}
		if len(concepts) == 0 && len(model.concepts) > 0 {
			concepts, conceptsBy = model.concepts, classifiedByModel
			out.Concepts = concepts
		}
	} else if len(concepts) == 0 && d.Classifier != nil {
		// The class is decided (by rule, or by hand), but what the document is ABOUT is
		// not: the vote needs classified neighbours, and the model's class question
		// above never ran. Ask the model for the concepts alone; they are proposals.
		cs, step := d.classifyConcepts(ctx, read, cd)
		out.Steps = append(out.Steps, step)
		if len(cs) > 0 {
			concepts, conceptsBy = cs, classifiedByModel
			out.Concepts = concepts
		}
	}
	if handSet {
		out.Warnings = append(out.Warnings, "the document's type "+cd.rootType+" was set by hand; classify does not replace it")
		out.Result, live = "kept", false
	}
	out.Live = live
	if tenantScope && len(issuers) == 1 {
		issuers[0].SubjectChunkID = d.classifyIssuerSubject(ctx, key, mscope, read, issuers[0].Instance)
	}
	out.IssuedBy = issuers

	if write == classifyWriteReport {
		return jsonResult(out)
	}
	if acl != nil {
		if r, err := acl.document(ctx, "classify", "write", docID); err != nil {
			return errFrom("classify: "+err.Error(), err), nil
		} else if r != nil {
			return *r, nil
		}
	}
	rec := classification{By: edgeMethodClassify, At: time.Now().UnixNano()}
	if cd.record != nil {
		rec.Applied = cd.record.Applied
	}
	setType := ""
	switch {
	case live:
		setType, rec.Applied = out.Class, out.Class
	case chosen != nil && !handSet:
		rec.Proposed, rec.ProposedFields = out.Class, out.Fields
	}

	// Statements: issued_by and concepts, in the tenant scope only.
	if tenantScope {
		rootDoc := docID
		if len(issuers) == 1 && issuers[0].SubjectChunkID != "" {
			status := edgeStatusProposed
			if live {
				status = edgeStatusAsserted
			}
			res, warn := d.classifyStatement(ctx, key, mscope, acl, rootDoc, cd.rootID, issuers[0].SubjectChunkID, "issued_by", status, issuers[0].Quote, classifiedByRules)
			out.addStatement(res, warn, &rec)
		}
		for _, c := range concepts {
			res, warn := d.classifyStatement(ctx, key, mscope, acl, rootDoc, cd.rootID, c.ChunkID, c.Kind, edgeStatusProposed, "", conceptsBy)
			out.addStatement(res, warn, &rec)
		}
	} else {
		for _, c := range concepts {
			rec.ProposedTags = append(rec.ProposedTags, c.Tag)
		}
		if len(issuers) > 0 {
			out.Warnings = append(out.Warnings, "issued_by is recorded only in the tenant scope, where the instances' subjects are; reported, not written")
		}
	}

	if r := d.classifyWriteRoot(ctx, key, mscope, acl, cd, setType, out.Fields, rec, &out); r != nil {
		return *r, nil
	}
	return jsonResult(out)
}

func (a *classifyAnswer) addStatement(res *edgeWriteResult, warn string, rec *classification) {
	if warn != "" {
		a.Warnings = append(a.Warnings, warn)
	}
	if res == nil {
		return
	}
	a.Statements = append(a.Statements, *res)
	if res.Status == edgeStatusProposed {
		rec.Statements = append(rec.Statements, res.ID)
	}
}

// classifyRead loads the root, its fields and record, and the first sections.
func (d *Document) classifyRead(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string) (classifyDoc, bool, error) {
	cd := classifyDoc{id: docID}
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil || len(res.Rows) == 0 {
		return cd, false, err
	}
	cd.rootID = asStr(res.Rows[0][0])
	row, ok, err := d.getChunkRow(ctx, key, cd.rootID)
	if err != nil || !ok {
		return cd, false, err
	}
	cd.title, cd.rootType, cd.rootRevision = row.Title, strings.TrimSpace(row.Type), row.Revision
	cb, err := d.readBody(ctx, mscope, key, cd.rootID)
	if err != nil {
		return cd, false, err
	}
	cd.fields = decodeFields(cb.Fields)
	if cd.fields == nil {
		cd.fields = map[string]any{}
	}
	cd.sourceTitle, _ = cd.fields[importFieldSourceTitle].(string)
	if raw, ok := cd.fields[classifyField]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var rec classification
			if json.Unmarshal(b, &rec) == nil && rec.By == edgeMethodClassify {
				cd.record = &rec
			}
		}
	}
	res, err = d.query(ctx, key, `SELECT id, title FROM chunks WHERE document_id = ? AND parent_id = ?
		ORDER BY position LIMIT `+strconv.Itoa(classifySections), docID, cd.rootID)
	if err != nil {
		return cd, false, err
	}
	var text strings.Builder
	text.WriteString(cd.title)
	for _, r := range res.Rows {
		cd.sectionIDs = append(cd.sectionIDs, asStr(r[0]))
		cd.sectionTitles = append(cd.sectionTitles, asStr(r[1]))
		text.WriteString("\n" + asStr(r[1]))
	}
	for _, id := range cd.sectionIDs {
		if text.Len() >= classifyBodyChars {
			break
		}
		if b, err := d.readBody(ctx, mscope, key, id); err == nil {
			text.WriteString("\n" + b.Body)
		}
	}
	cd.text, _ = cutRunes(text.String(), classifyBodyChars)
	return cd, true, nil
}

// classifyRules runs step 1 over every document class with a pattern of its own. A
// class and its own ancestor both matching count as the descendant: the more specific
// claim.
//
// A class is matched by the patterns it DECLARES, not those it inherits: a subclass
// that only inherits its parent's title pattern adds no evidence for itself, and if
// inherited patterns counted, two sibling subclasses of one pattern would always match
// together and nothing could ever be applied. The identity fields are filled by the
// EFFECTIVE identifier pattern, which may well be inherited.
func classifyRules(classes map[string]memrank.OntologyTerm, cd classifyDoc) []classifyCandidate {
	var out []classifyCandidate
	names := make([]string, 0, len(classes))
	for n := range classes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := classes[n]
		tp, ownIP := c.TitlePattern, c.IdentifierPattern
		ip := memrank.EffectiveDirective(c, "identifier_pattern")
		var cand *classifyCandidate
		if tp != "" {
			re, err := regexp.Compile(tp)
			if err != nil {
				continue
			}
			texts := [][2]string{{"title", cd.title}, {"source_title", cd.sourceTitle}}
			for _, s := range cd.sectionTitles {
				texts = append(texts, [2]string{"section", s})
			}
			for _, t := range texts {
				if q, g := matchGroups(re, t[1]); q != "" {
					cand = &classifyCandidate{Class: c.Name, Quote: q, From: t[0], Groups: g}
					break
				}
			}
		} else if ownIP != "" {
			if re, err := regexp.Compile(ownIP); err == nil {
				if q, g := matchGroups(re, cd.title); q != "" {
					cand = &classifyCandidate{Class: c.Name, Quote: q, From: "title", Groups: g}
				}
			}
		}
		if cand == nil {
			continue
		}
		// The identifier pattern fills the identity fields even when the title pattern
		// is what matched: the title says what kind of act, the identifier which one.
		if tp != "" && ip != "" {
			if re, err := regexp.Compile(ip); err == nil {
				for _, t := range append([]string{cd.title, cd.sourceTitle}, cd.sectionTitles...) {
					if q, g := matchGroups(re, t); q != "" {
						for k, v := range g {
							if _, has := cand.Groups[k]; !has {
								if cand.Groups == nil {
									cand.Groups = map[string]any{}
								}
								cand.Groups[k] = v
							}
						}
						break
					}
				}
			}
		}
		out = append(out, *cand)
	}
	// A match on the title (or the import's source title) outranks one on a section
	// title: a document's sections are often headed by another kind's name
	// (a Regulation's chapter "Порядок …", a Policy's "Порядок …"), and counting them
	// beside the title made a tie of nearly every internal document. Sections speak
	// only when the title says nothing.
	titled := false
	for _, c := range out {
		titled = titled || c.From != "section"
	}
	if titled {
		filtered := out[:0]
		for _, c := range out {
			if c.From != "section" {
				filtered = append(filtered, c)
			}
		}
		out = filtered
	}
	// Drop an ancestor of another match.
	var kept []classifyCandidate
	for i, a := range out {
		ancestor := false
		for j, b := range out {
			if i != j && isAncestorClass(classes, a.Class, b.Class) {
				ancestor = true
				break
			}
		}
		if !ancestor {
			kept = append(kept, a)
		}
	}
	if kept == nil {
		kept = []classifyCandidate{}
	}
	return kept
}

// isAncestorClass reports whether anc is a proper ancestor of cls.
func isAncestorClass(classes map[string]memrank.OntologyTerm, anc, cls string) bool {
	seen := map[string]bool{}
	for cur := classes[strings.ToLower(cls)].Parent; cur != "" && !seen[strings.ToLower(cur)]; cur = classes[strings.ToLower(cur)].Parent {
		if strings.EqualFold(cur, anc) {
			return true
		}
		seen[strings.ToLower(cur)] = true
	}
	return false
}

func matchGroups(re *regexp.Regexp, s string) (string, map[string]any) {
	if s == "" {
		return "", nil
	}
	m := re.FindStringSubmatchIndex(s)
	if m == nil || m[1] == m[0] {
		return "", nil
	}
	var g map[string]any
	for i, name := range re.SubexpNames() {
		if name == "" || m[2*i] < 0 {
			continue
		}
		if g == nil {
			g = map[string]any{}
		}
		g[name] = s[m[2*i]:m[2*i+1]]
	}
	return s[m[0]:m[1]], g
}

// identityGroups keeps the groups that name a field of the class.
func identityGroups(c memrank.OntologyTerm, groups map[string]any) map[string]any {
	var out map[string]any
	for _, fs := range memrank.AllFieldSpecs(c) {
		if v, ok := groups[fs.Name]; ok {
			if out == nil {
				out = map[string]any{}
			}
			out[fs.Name] = v
		}
	}
	return out
}

func hasCandidate(cs []classifyCandidate, class string) bool {
	for _, c := range cs {
		if strings.EqualFold(c.Class, class) {
			return true
		}
	}
	return false
}

// classifyIssuers names the instances whose name or @alt label occurs, as a whole
// phrase, in the text. A substring inside a longer word ("Acme" in "Acmetown") is not
// a mention.
func classifyIssuers(instances []memrank.OntologyInstance, text string) []classifyIssuer {
	out := []classifyIssuer{}
	low := strings.ToLower(text)
	for _, inst := range instances {
		for _, label := range append([]string{inst.Name}, inst.Alt...) {
			if q := phraseIn(low, text, strings.ToLower(strings.TrimSpace(label))); q != "" {
				out = append(out, classifyIssuer{Instance: inst.Name, Quote: q})
				break
			}
		}
	}
	return out
}

// phraseIn finds phrase in low (the lowered text) bounded by non-letters and returns
// the original-case quote.
func phraseIn(low, text, phrase string) string {
	if len([]rune(phrase)) < 2 {
		return ""
	}
	for from := 0; from < len(low); {
		i := strings.Index(low[from:], phrase)
		if i < 0 {
			return ""
		}
		start, end := from+i, from+i+len(phrase)
		before, after := ' ', ' '
		if start > 0 {
			before = lastRune(low[:start])
		}
		if end < len(low) {
			after = []rune(low[end:])[0]
		}
		if !unicode.IsLetter(before) && !unicode.IsDigit(before) && !unicode.IsLetter(after) && !unicode.IsDigit(after) {
			if len(low) == len(text) {
				return text[start:end]
			}
			return phrase
		}
		from = end
	}
	return ""
}

func lastRune(s string) rune {
	r := []rune(s)
	return r[len(r)-1]
}

type classifyModelResult struct {
	step      classifyStep
	class     string
	candidate *classifyCandidate
	concepts  []classifyConcept
}

// classifyModel is step 3: memory.classifier, asked about the classes the rules left
// open (or every document class when none matched) and, when the vote found none, the
// concepts in force.
func (d *Document) classifyModel(ctx context.Context, read OntologyRead, classes map[string]memrank.OntologyTerm,
	rules []classifyCandidate, cd classifyDoc, voted []classifyConcept) classifyModelResult {
	out := classifyModelResult{step: classifyStep{Step: "model", Candidates: []classifyCandidate{}}}
	if d.Classifier == nil {
		out.step.Reason = "no classifier model is configured"
		return out
	}
	req := memrank.ClassifyRequest{Title: cd.title, Text: cd.text}
	if len(rules) > 1 {
		for _, r := range rules {
			req.Classes = append(req.Classes, memrank.ClassifyOption{Name: r.Class})
		}
	} else {
		for _, c := range classes {
			if !strings.EqualFold(c.Name, memrank.DocumentClassRoot) {
				req.Classes = append(req.Classes, memrank.ClassifyOption{Name: c.Name})
			}
		}
		sort.Slice(req.Classes, func(i, j int) bool { return req.Classes[i].Name < req.Classes[j].Name })
	}
	byName := map[string]memrank.OntologyConcept{}
	if len(voted) == 0 {
		for _, s := range read.Schemes {
			for _, c := range s.Concepts {
				if _, dup := byName[strings.ToLower(c.Name)]; dup {
					continue // an ambiguous name cannot be told apart in an answer
				}
				byName[strings.ToLower(c.Name)] = c
				req.Concepts = append(req.Concepts, memrank.ClassifyOption{Name: c.Name, Meaning: c.Meaning})
			}
		}
	}
	v := d.Classifier.Classify(ctx, req)
	out.step.Method, out.step.Reason = v.Method, v.Reason
	if v.Class != "" {
		cand := classifyCandidate{Class: v.Class, Quote: v.Quote, Share: v.Probability}
		for _, r := range rules {
			if strings.EqualFold(r.Class, v.Class) {
				cand.Groups, cand.From = r.Groups, r.From
			}
		}
		out.step.Candidates = append(out.step.Candidates, cand)
		out.step.Chosen, out.class, out.candidate = v.Class, v.Class, &cand
	}
	for _, name := range v.Concepts {
		if c, ok := byName[strings.ToLower(name)]; ok {
			out.concepts = append(out.concepts, classifyConcept{Concept: c.Name, Tag: c.Tag, Kind: "subject", ChunkID: c.ChunkID})
		}
	}
	return out
}

// classifyConcepts asks memory.classifier which in-force concepts a document is
// about, when its class is already decided. A "model" step with no chosen class.
func (d *Document) classifyConcepts(ctx context.Context, read OntologyRead, cd classifyDoc) ([]classifyConcept, classifyStep) {
	step := classifyStep{Step: "model", Candidates: []classifyCandidate{}}
	byName := map[string]memrank.OntologyConcept{}
	req := memrank.ClassifyRequest{Title: cd.title, Text: cd.text, ConceptsOnly: true}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if _, dup := byName[strings.ToLower(c.Name)]; dup {
				continue
			}
			byName[strings.ToLower(c.Name)] = c
			req.Concepts = append(req.Concepts, memrank.ClassifyOption{Name: c.Name, Meaning: c.Meaning})
		}
	}
	if len(req.Concepts) == 0 {
		step.Reason = "no concept scheme is in force"
		return nil, step
	}
	v := d.Classifier.Classify(ctx, req)
	step.Method, step.Reason = v.Method, v.Reason
	var out []classifyConcept
	for _, name := range v.Concepts {
		if c, ok := byName[strings.ToLower(name)]; ok {
			out = append(out, classifyConcept{Concept: c.Name, Tag: c.Tag, Kind: "subject", ChunkID: c.ChunkID})
		}
	}
	return out, step
}
