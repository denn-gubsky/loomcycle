package builtin

// document_annotation_repair.go — what set_annotation corrects in a model's
// annotation before it stores it, and the document identity both the inputs
// and the correction read.
//
// Two defects are repaired, and only these two: model text is otherwise
// stored as it came.
//
//   - A brief or abstract that names its document by another document's code
//     (a section of one RFC summarised as "RFC FT …" when the document is not
//     FT). The model copies a code it saw somewhere other than the part it
//     summarised; the class description's examples are the usual source. A
//     token with the shape of the class's identifier pattern whose value is
//     not the document's own is rewritten to the document's own value, unless
//     the token is in the text the annotation was written from: that is a
//     reference to another document, and rewriting it would be the defect.
//   - A folder brief that only repeats the folder's name, or is too short to
//     choose by. It is replaced by the first sentence of the abstract written
//     with it.
//
// Every repair is returned to the writer and logged at Info, without the text.

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

const (
	// annotationRepairIdentity: a token of the class's identifier shape named
	// another document.
	annotationRepairIdentity = "identity_mismatch"
	// annotationRepairFolderName: the folder brief only repeated the folder name.
	annotationRepairFolderName = "brief_repeats_folder_name"
	// annotationRepairFolderShort: the folder brief was under the floor.
	annotationRepairFolderShort = "brief_too_short"

	// minFolderBriefChars is the floor under which a folder brief gives a
	// listing nothing to choose by.
	minFolderBriefChars = 20
)

// annotationRepair is one correction set_annotation made, as its result
// reports it.
type annotationRepair struct {
	Field  string `json:"field"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// classTerm is a document class of the tenant ontology, by name (case ignored).
func (s *annotationScope) classTerm(ctx context.Context, name string) (memrank.OntologyTerm, bool) {
	onto := s.ontology(ctx)
	if onto == nil || name == "" {
		return memrank.OntologyTerm{}, false
	}
	for _, t := range memrank.EffectiveClasses(onto.Terms, onto.Confirmed) {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return memrank.OntologyTerm{}, false
}

// identity is what says WHICH work the target's document is: the root's
// natural key and identity-like fields, plus the values of the fields its
// class declares as identity, read from the root's fields or else from the
// class's identifier pattern over the document title. A section gets its
// document's identity: without it a model summarising a section that carries
// no code of its own has nothing to name the document by but what it guesses.
func (s *annotationScope) identity(ctx context.Context, t annotationTarget, own chunkBody) (map[string]string, error) {
	root := own
	if t.chunk.ID != t.rootID {
		b, err := s.body(ctx, t.rootID)
		if err != nil {
			return nil, err
		}
		root = b
	}
	out := s.d.annotationIdentity(ctx, s.key, t.rootID, root)
	term, ok := s.classTerm(ctx, t.docType)
	if !ok {
		return out, nil
	}
	var fields map[string]any
	if len(root.Fields) > 0 {
		_ = json.Unmarshal(root.Fields, &fields)
	}
	groups := identifierGroups(term, t.docTitle)
	for _, f := range memrank.IdentityFields(term) {
		if f == "title" {
			continue
		}
		v, _ := fields[f].(string)
		if strings.TrimSpace(v) == "" {
			v = groups[f]
		}
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		if _, has := out[f]; !has {
			out[f] = truncateRunes(v, 120)
		}
	}
	return out, nil
}

// identifierGroups is the named groups of the class's identifier pattern
// matched against title; nil when it has none or does not match.
func identifierGroups(term memrank.OntologyTerm, title string) map[string]string {
	ip := memrank.EffectiveDirective(term, "identifier_pattern")
	if ip == "" {
		return nil
	}
	re, err := regexp.Compile(ip)
	if err != nil {
		return nil
	}
	m := re.FindStringSubmatch(title)
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for i, n := range re.SubexpNames() {
		if n != "" && m[i] != "" {
			out[n] = m[i]
		}
	}
	return out
}

// identifierShape is the class's identifier pattern with its start and end
// anchors removed, so it finds the same kind of token anywhere in a text.
func identifierShape(term memrank.OntologyTerm) *regexp.Regexp {
	ip := strings.TrimSpace(memrank.EffectiveDirective(term, "identifier_pattern"))
	ip = strings.TrimPrefix(strings.TrimPrefix(ip, `\A`), "^")
	if strings.HasSuffix(ip, "$") && !strings.HasSuffix(ip, `\$`) {
		ip = strings.TrimSuffix(ip, "$")
	}
	ip = strings.TrimSuffix(ip, `\z`)
	if ip == "" {
		return nil
	}
	re, err := regexp.Compile(ip)
	if err != nil || len(re.SubexpNames()) < 2 {
		return nil
	}
	return re
}

// repairIdentity rewrites, in brief and abstract, each token of the class's
// identifier shape whose value differs from the document's own and that the
// inputs do not contain. cur is the target's inputs as set_annotation read
// them.
func (s *annotationScope) repairIdentity(ctx context.Context, t annotationTarget, cur annotationInputs, brief, abstract string) (string, string, []annotationRepair, error) {
	term, ok := s.classTerm(ctx, t.docType)
	if !ok {
		return brief, abstract, nil, nil
	}
	re := identifierShape(term)
	if re == nil {
		return brief, abstract, nil, nil
	}
	own := identifierGroups(term, t.docTitle)
	for k, v := range cur.Identity {
		if own == nil {
			own = map[string]string{}
		}
		if _, has := own[k]; !has {
			own[k] = v
		}
	}
	if len(own) == 0 {
		return brief, abstract, nil, nil
	}
	material, err := s.inputsMaterial(ctx, t, cur)
	if err != nil {
		return brief, abstract, nil, err
	}
	var repairs []annotationRepair
	brief = rewriteIdentityTokens(re, own, material, "brief", brief, &repairs)
	abstract = rewriteIdentityTokens(re, own, material, "abstract", abstract, &repairs)
	brief = cutOnWord(brief, maxAnnotationBriefChars)
	abstract = cutOnWord(abstract, maxAnnotationAbstractChars)
	return brief, abstract, repairs, nil
}

// inputsMaterial is the text an annotation was written from, in full: the
// target's titles and body, and each part's title, record and text. The class
// description is not in it: its examples are where a wrong code comes from.
func (s *annotationScope) inputsMaterial(ctx context.Context, t annotationTarget, cur annotationInputs) (string, error) {
	var b strings.Builder
	add := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	own, err := s.body(ctx, t.chunk.ID)
	if err != nil {
		return "", err
	}
	add(t.chunk.Title, t.docTitle, own.Body)
	for _, ch := range cur.Children {
		cb, err := s.body(ctx, ch.ChunkID)
		if err != nil {
			return "", err
		}
		add(ch.Title, ch.Brief, ch.Abstract, cb.Body)
	}
	for _, r := range cur.Relations {
		add(r.Titles...)
	}
	return b.String(), nil
}

// rewriteIdentityTokens rewrites the tokens of one field, appending a repair
// per token changed.
func rewriteIdentityTokens(re *regexp.Regexp, own map[string]string, material, field, text string, repairs *[]annotationRepair) string {
	names := re.SubexpNames()
	var out strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		token := text[m[0]:m[1]]
		if strings.Contains(material, token) {
			continue
		}
		// Rebuild the token with each differing group set to the document's own.
		var nt strings.Builder
		at := m[0]
		for i := 1; i < len(names); i++ {
			want, known := own[names[i]]
			if names[i] == "" || !known || m[2*i] < 0 || text[m[2*i]:m[2*i+1]] == want {
				continue
			}
			nt.WriteString(text[at:m[2*i]])
			nt.WriteString(want)
			at = m[2*i+1]
		}
		if at == m[0] {
			continue
		}
		nt.WriteString(text[at:m[1]])
		out.WriteString(text[last:m[0]])
		out.WriteString(nt.String())
		last = m[1]
		*repairs = append(*repairs, annotationRepair{Field: field, From: token, To: nt.String(), Reason: annotationRepairIdentity})
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

// repairFolderBrief replaces a folder brief that only repeats the folder's
// name, or is shorter than the floor, with the abstract's first sentence cut
// on a word to the brief's limit. With no sentence to take, the brief stays.
func repairFolderBrief(folder, brief, abstract string) (string, *annotationRepair) {
	name := folder
	if i := strings.LastIndex(strings.TrimSuffix(folder, "/"), "/"); i >= 0 && folder != "/" {
		name = strings.TrimSuffix(folder, "/")[i+1:]
	}
	reason := ""
	switch {
	case normaliseName(brief) == normaliseName(name):
		reason = annotationRepairFolderName
	case utf8.RuneCountInString(brief) < minFolderBriefChars:
		reason = annotationRepairFolderShort
	default:
		return brief, nil
	}
	to := cutOnWord(firstSentence(abstract), maxAnnotationBriefChars)
	if to == "" || normaliseName(to) == normaliseName(name) {
		return brief, nil
	}
	return to, &annotationRepair{Field: "brief", From: brief, To: to, Reason: reason}
}

// normaliseName folds case and turns every run of non-letters and non-digits
// into one space, so "Docs.", "docs" and "DOCS" read the same.
func normaliseName(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// firstSentence is text up to and including its first sentence end (. ! or ?
// followed by a space or the end), trimmed.
func firstSentence(text string) string {
	text = strings.TrimSpace(text)
	for i, r := range text {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		next := i + utf8.RuneLen(r)
		if next == len(text) || unicode.IsSpace(rune(text[next])) {
			return text[:next]
		}
	}
	return text
}

// cutOnWord shortens text to at most max runes, ending on a whole word.
func cutOnWord(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	cut := truncateRunes(text, max)
	if i := strings.LastIndexFunc(cut, unicode.IsSpace); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-—")
}

// logAnnotationRepairs records each repair of a stored record at Info: the
// folder or the chunk and document, and why; never the text.
func logAnnotationRepairs(ctx context.Context, rec annotationRecord, repairs []annotationRepair) {
	for _, r := range repairs {
		if rec.Level == annotationLevelPath {
			slog.InfoContext(ctx, "document: annotation repaired", "path", rec.Folder, "level", rec.Level, "field", r.Field, "reason", r.Reason)
			continue
		}
		slog.InfoContext(ctx, "document: annotation repaired", "document_id", rec.DocumentID, "chunk_id", rec.ChunkID,
			"level", rec.Level, "field", r.Field, "reason", r.Reason)
	}
}
