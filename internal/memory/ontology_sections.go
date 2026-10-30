package memory

// ontology_sections.go — the sectioned ontology document (RFC FO P1a).
//
// The ontology was a flat list of entity types that RFC BZ turned into a class tree by
// reading the chunk tree. RFC FO makes it a document of SECTIONS: each root child may
// declare, on its first directive line, what nesting beneath it means —
//
//	@section classes     nesting is subclassing with field inheritance (today's rule)
//	@section relations   nesting is sub-kinding (amends ⊂ changes)
//	@section schemes     nesting is SKOS broader/narrower, no inheritance
//	@section instances   nesting is a declared relation between real things
//
// ONE document, one editor, one confirm gate, rather than a document per concern: a
// relation whose domain names a class renamed in another document is drift this project
// has already paid for once.
//
// A document with NO @section directive reads exactly as it always did — every root
// child is a class — and that is the compatibility guarantee every function here keeps.
// This file is pure parsing: no I/O, so the chunk-tree reader in tools/builtin and any
// future reader agree on what a directive, a field and a grammar are.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Section kinds a root child of the ontology document may declare.
const (
	SectionClasses   = "classes"
	SectionRelations = "relations"
	SectionSchemes   = "schemes"
	SectionInstances = "instances"
)

// SectionKinds lists the recognised section kinds in document order.
func SectionKinds() []string {
	return []string{SectionClasses, SectionRelations, SectionSchemes, SectionInstances}
}

func isSectionKind(s string) bool {
	for _, k := range SectionKinds() {
		if s == k {
			return true
		}
	}
	return false
}

// Directive is one `@name value` declaration in a chunk body.
//
// Name is lowercased and carries no sigil. Value keeps its case and its inner spacing,
// because a directive like @identifier_pattern carries a regular expression where both
// are significant — the older directiveValue, which lowercases and keeps one word, is
// right for an enum like @memory_scope and wrong for a pattern.
type Directive struct {
	Name  string
	Value string
}

// knownDirectives are the directive names this build recognises. A name outside the set
// is REPORTED and never acted on (fail-open): an operator who typo'd @identiy should be
// told, and should not have their class change behaviour over it.
//
// Directives of the later sections (relations, schemes, instances) are listed now so an
// operator authoring ahead of those phases is not shown a wall of "unknown" advisories
// for words the RFC already defines.
var knownDirectives = map[string]bool{
	// document and section
	"section": true, "version": true,
	// classes
	"memory_scope": true, "identity": true, "identifier_pattern": true,
	"title_pattern": true, "anchor_grammar": true, "citation_grammar": true,
	"exclude": true, "renamed_from": true, "definitions": true,
	// relations
	"inverse": true, "domain": true, "range": true, "symmetric": true,
	"asymmetric": true, "irreflexive": true, "transitive": true,
	"transitive_within": true, "functional": true, "qualifiers": true,
	"discriminator": true, "dated": true, "walk": true, "auto_assert": true,
	// schemes
	"ordered": true, "notation": true, "alt": true, "exact_match": true,
	"retention": true, "acl": true, "render": true,
	// instances
	"a": true, "nesting": true,
}

var bareDirectiveRe = regexp.MustCompile(`^@([A-Za-z_][A-Za-z0-9_]*)(?:\s+(.*))?$`)

// ParseDirectives returns every directive in a body, in order, in either spelling:
//
//	@identifier_pattern (?P<number>\d+-[IVX]+)      ← the bare line RFC FO writes
//	- `@memory_scope` tenant — everyone sees these   ← the bullet the template has always used
//
// Both are accepted for every directive, so no existing document changes meaning and no
// operator has to learn which directive takes which spelling.
//
// A bare line indented as a Markdown code block (four spaces or a tab), or inside a ```
// fence, is an EXAMPLE, not a declaration — the template shows directives in exactly
// those blocks. The bullet form is not given that exemption because it never had it:
// changing how an existing bullet reads would change existing documents.
func ParseDirectives(body string) []Directive {
	var out []Directive
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if strings.HasPrefix(t, "- ") {
			f := firstBackticked(t)
			if !strings.HasPrefix(f, "@") || len(f) < 2 {
				continue
			}
			out = append(out, Directive{Name: strings.ToLower(f[1:]), Value: bulletDirectiveValue(t)})
			continue
		}
		if inFence || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		m := bareDirectiveRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		out = append(out, Directive{Name: strings.ToLower(m[1]), Value: strings.TrimSpace(m[2])})
	}
	return out
}

// bulletDirectiveValue is the text after a bullet directive's closing backtick, with the
// separator an operator naturally types (":", "=") dropped and a trailing " — note"
// removed, matching how a field bullet's prose is ignored.
func bulletDirectiveValue(line string) string {
	i := strings.Index(line, "`")
	rest := line[i+1:]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	tail := strings.TrimSpace(rest[j+1:])
	tail = strings.TrimSpace(strings.TrimLeft(tail, ":="))
	if k := strings.Index(tail, " — "); k >= 0 {
		tail = tail[:k]
	}
	return strings.TrimSpace(tail)
}

// SectionKind returns the section kind a root child declares, or "" when it declares
// none (it is a class, as before RFC FO).
//
// Only the FIRST directive counts, so a class whose body happens to mention @section
// further down is not silently turned into a section. A @section naming an unknown kind
// also yields "" — the RFC's fail-open reading is that the chunk stays a class — and
// the caller reports it via SectionIssue.
func SectionKind(body string) string {
	ds := ParseDirectives(body)
	if len(ds) == 0 || ds[0].Name != "section" {
		return ""
	}
	k := strings.ToLower(strings.TrimSpace(ds[0].Value))
	if isSectionKind(k) {
		return k
	}
	return ""
}

// SectionIssue explains a first-line @section the build does not recognise, or "".
func SectionIssue(body string) string {
	ds := ParseDirectives(body)
	if len(ds) == 0 || ds[0].Name != "section" {
		return ""
	}
	k := strings.ToLower(strings.TrimSpace(ds[0].Value))
	if isSectionKind(k) {
		return ""
	}
	return "@section " + strconv.Quote(ds[0].Value) + " is not a section kind (want " +
		joinComma(SectionKinds()) + "); this chunk is read as a class"
}

// Field types a typed field may declare.
const (
	FieldText    = "text"
	FieldNumber  = "number"
	FieldDate    = "date"
	FieldEnum    = "enum"
	FieldConcept = "concept"
	FieldRef     = "ref"
	FieldAnchor  = "anchor"
)

// FieldSpec is one typed field of a class:
//
//   - `rank` concept(normative-rank) @required — where it sits in the hierarchy
//
// A bullet that declares no type is `text`, so every field written before typed fields
// existed is a text field and nothing about how it is read changes.
type FieldSpec struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Enum is the allowed values of an enum field.
	Enum []string `json:"enum,omitempty"`
	// Range is the scheme a concept field draws from, or the class a ref field points at.
	Range    string `json:"range,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Unique is parsed and shown but not yet enforced: checking it means reading every
	// chunk of the class, and the RFC's warn-first posture refuses only identity
	// collisions, which have their own indexed column.
	Unique   bool `json:"unique,omitempty"`
	Identity bool `json:"identity,omitempty"`
	// Issue is an operator-facing advisory about the declaration itself (an unclosed
	// enum, say). The field stays in force as text.
	Issue string `json:"issue,omitempty"`
}

// ParseOntologyFieldSpecs reads the typed fields of ONE class body.
//
// It returns the same names in the same order as ParseOntologyFields — that reader is
// unchanged and remains what the prompt renders from — plus each field's type and flags.
func ParseOntologyFieldSpecs(body string) []FieldSpec {
	var out []FieldSpec
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		name := firstBackticked(t)
		if name == "" || strings.HasPrefix(name, "@") {
			continue
		}
		out = append(out, parseFieldTail(name, afterFirstBacktickPair(t)))
	}
	return out
}

func afterFirstBacktickPair(s string) string {
	i := strings.Index(s, "`")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[j+1:])
}

// parseFieldTail reads `<type> [@flag…] [— comment]` after a field name.
//
// The type is recognised only when the first word IS a type keyword. Anything else is
// prose — "- `cause` what turned out to be responsible" — and the field is text with no
// flags, which is exactly how that bullet read before. Recognising a type out of the
// middle of a sentence would retype fields nobody meant to type.
func parseFieldTail(name, tail string) FieldSpec {
	fs := FieldSpec{Name: name, Type: FieldText}
	rest := tail
	lower := strings.ToLower(rest)
	switch {
	case strings.HasPrefix(lower, "enum("), strings.HasPrefix(lower, "concept("), strings.HasPrefix(lower, "ref("):
		open := strings.Index(rest, "(")
		closeAt := strings.Index(rest, ")")
		if closeAt < open {
			fs.Issue = "type " + strconv.Quote(rest) + " has no closing parenthesis; read as text"
			return fs
		}
		kind := strings.ToLower(rest[:open])
		arg := strings.TrimSpace(rest[open+1 : closeAt])
		rest = strings.TrimSpace(rest[closeAt+1:])
		fs.Type = kind
		if kind == FieldEnum {
			for _, v := range strings.Split(arg, ",") {
				if v = strings.TrimSpace(v); v != "" {
					fs.Enum = append(fs.Enum, v)
				}
			}
			if len(fs.Enum) == 0 {
				fs.Issue = "enum() lists no values; any value is accepted"
			}
		} else {
			fs.Range = arg
			if arg == "" {
				fs.Issue = kind + "() names no " + map[string]string{FieldConcept: "scheme", FieldRef: "class"}[kind]
			}
		}
	default:
		word := rest
		if k := strings.IndexAny(rest, " \t"); k >= 0 {
			word = rest[:k]
		}
		switch strings.ToLower(word) {
		case FieldText, FieldNumber, FieldDate, FieldAnchor:
			fs.Type = strings.ToLower(word)
			rest = strings.TrimSpace(rest[len(word):])
		default:
			if !strings.HasPrefix(word, "@") {
				return fs // prose, or nothing: an untyped text field
			}
		}
	}
	for _, tok := range strings.Fields(rest) {
		if !strings.HasPrefix(tok, "@") {
			break // the comment starts here
		}
		switch strings.ToLower(tok) {
		case "@required":
			fs.Required = true
		case "@unique":
			fs.Unique = true
		case "@identity":
			fs.Identity = true
		default:
			fs.Issue = "unknown field flag " + tok + " (want @required, @unique, @identity)"
		}
	}
	return fs
}

// MaxOntologyPatternLen bounds a declared pattern. RE2 cannot backtrack, so length is
// what bounds compile cost and memory; 512 bytes fits any real identifier or citation.
const MaxOntologyPatternLen = 512

// compileOntologyPattern compiles a declared pattern or explains why it cannot be used.
// Go's regexp is RE2 — linear-time, no backtracking — so a hostile pattern costs bounded
// work against stored text, never a hung request.
func compileOntologyPattern(p string) (*regexp.Regexp, string) {
	if p == "" {
		return nil, "names no pattern"
	}
	if len(p) > MaxOntologyPatternLen {
		return nil, "pattern is longer than " + strconv.Itoa(MaxOntologyPatternLen) + " characters"
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, "pattern does not compile: " + err.Error()
	}
	return re, ""
}

// ClassDecl is everything a class chunk's body declares.
type ClassDecl struct {
	Fields            []string
	FieldSpecs        []FieldSpec
	MemoryScope       string
	MemoryScopeIssue  string
	TitleIdentity     bool
	IdentifierPattern string
	TitlePattern      string
	AnchorGrammar     string
	CitationGrammar   string
	Definitions       string
	Exclude           []string
	RenamedFrom       string
	Issues            []string
}

// ParseClassBody reads one class body: its fields and the class directives.
//
// Unknown directives and unusable values become Issues; the class itself always stays
// in force — an ontology that dropped a class over a typo'd directive is worse than one
// that ignores the directive and says so.
func ParseClassBody(body string) ClassDecl {
	d := ClassDecl{
		Fields:     ParseOntologyFields(body),
		FieldSpecs: ParseOntologyFieldSpecs(body),
	}
	d.MemoryScope, d.MemoryScopeIssue = ParseOntologyScope(body)
	for _, dir := range ParseDirectives(body) {
		switch dir.Name {
		case "memory_scope", "section", "version":
			// memory_scope is read by ParseOntologyScope (one reader for both spellings);
			// section is the section reader's; version is informational on the root.
		case "identity":
			d.TitleIdentity = true
		case "identifier_pattern", "title_pattern", "definitions":
			if _, issue := compileOntologyPattern(dir.Value); issue != "" {
				d.Issues = append(d.Issues, "@"+dir.Name+" "+issue+"; ignored")
				continue
			}
			switch dir.Name {
			case "identifier_pattern":
				d.IdentifierPattern = dir.Value
			case "title_pattern":
				d.TitlePattern = dir.Value
			default:
				d.Definitions = dir.Value
			}
		case "anchor_grammar":
			if names := GrammarNames(dir.Value); len(names) > 0 {
				d.AnchorGrammar = names[0]
			}
		case "citation_grammar":
			// A class may cite several kinds of work (laws and regulator acts), and a
			// grammar has one resolve line, so the directive takes a list.
			d.CitationGrammar = strings.Join(GrammarNames(dir.Value), ", ")
		case "exclude":
			for _, v := range strings.Split(dir.Value, ",") {
				if v = strings.TrimSpace(v); v != "" {
					d.Exclude = append(d.Exclude, v)
				}
			}
		case "renamed_from":
			d.RenamedFrom = firstWord(dir.Value)
		default:
			if !knownDirectives[dir.Name] {
				d.Issues = append(d.Issues, "unknown directive @"+dir.Name+"; ignored")
			}
		}
	}
	for _, fs := range d.FieldSpecs {
		if fs.Issue != "" {
			d.Issues = append(d.Issues, "field "+fs.Name+": "+fs.Issue)
		}
	}
	return d
}

// GrammarNames splits a grammar directive's value into names: separated by commas or
// spaces, empty and repeated names dropped, order kept. An @anchor_grammar takes the
// first; an @citation_grammar may name several.
func GrammarNames(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// GrammarTitlePrefix marks a chunk inside the classes section as a grammar, not a
// subclass. A grammar is DATA — patterns that the anchor resolver and the linking agent
// read — and is never executed.
const GrammarTitlePrefix = "grammar:"

// GrammarName returns the grammar name a chunk title declares, or "" when the title is
// not a grammar's.
func GrammarName(title string) string {
	t := strings.TrimSpace(title)
	if len(t) <= len(GrammarTitlePrefix) || !strings.EqualFold(t[:len(GrammarTitlePrefix)], GrammarTitlePrefix) {
		return ""
	}
	return strings.TrimSpace(t[len(GrammarTitlePrefix):])
}

// OntologyGrammar is a named anchor or citation grammar:
//
//	#### grammar:law-ua
//	pattern: Закон(?:у)? України № ?(?P<number>\d+-[IVX]+)
//	resolve: work_key=law:{number}
type OntologyGrammar struct {
	Name string `json:"name"`
	// Class is the class the grammar chunk sits under — informational; a class uses a
	// grammar by naming it, wherever in the classes section it is declared.
	Class    string   `json:"class,omitempty"`
	Patterns []string `json:"patterns,omitempty"`
	Resolve  string   `json:"resolve,omitempty"`
	// Issue is set when the grammar cannot be used. A broken grammar is DISABLED and
	// reported; the class naming it stays in force.
	Issue string `json:"issue,omitempty"`
}

var resolveGroupRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ParseGrammarBody reads a grammar chunk's `pattern:` and `resolve:` lines (a leading
// "- " is tolerated, as an operator writing a list naturally adds one).
func ParseGrammarBody(name, class, body string) OntologyGrammar {
	g := OntologyGrammar{Name: name, Class: class}
	groups := map[string]bool{}
	var issues []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(lower, "pattern:"):
			p := strings.TrimSpace(t[len("pattern:"):])
			re, issue := compileOntologyPattern(p)
			if issue != "" {
				issues = append(issues, "pattern "+issue)
				continue
			}
			g.Patterns = append(g.Patterns, p)
			for _, n := range re.SubexpNames() {
				if n != "" {
					groups[n] = true
				}
			}
		case strings.HasPrefix(lower, "resolve:"):
			g.Resolve = strings.TrimSpace(t[len("resolve:"):])
		}
	}
	switch {
	case len(issues) > 0:
		g.Issue = strings.Join(issues, "; ")
	case len(g.Patterns) == 0:
		g.Issue = "declares no pattern: line"
	case g.Resolve == "":
		g.Issue = "declares no resolve: line"
	default:
		for _, m := range resolveGroupRe.FindAllStringSubmatch(g.Resolve, -1) {
			if !groups[m[1]] {
				g.Issue = "resolve: names {" + m[1] + "}, which no pattern captures"
				break
			}
		}
	}
	if g.Issue != "" {
		// Disabled: a caller that ignores Issue must still not run a half-valid grammar.
		g.Patterns = nil
	}
	return g
}

// DocumentClassRoot is the class every document class descends from. Seed-provided and
// overridable like every seed type.
const DocumentClassRoot = "Document"

// BaseSeedDocumentClasses is the seed's document-class half: `Document` alone.
//
// SEPARATE from BaseSeedOntology on purpose. Every entity seed term is rendered into
// the extraction prompt, so adding `Document` there would change the prompt of every
// deployment — a cache miss and an extraction shift paid by people who never declared a
// section. Document classes govern documents.type, not what memory records, and never
// reach the prompt.
func BaseSeedDocumentClasses() []OntologyTerm {
	return []OntologyTerm{{
		Name:            DocumentClassRoot,
		Fields:          []string{"title"},
		FieldSpecs:      []FieldSpec{{Name: "title", Type: FieldText, Required: true}},
		Source:          "base",
		IsDocumentClass: true,
	}}
}

// EffectiveClasses is every class in force: the entity ontology plus the document
// classes. A tenant class named like the seed `Document` (case-insensitively) replaces
// it, by the same wholesale-override rule as the entity seed.
func EffectiveClasses(tenantTerms []OntologyTerm, tenantConfirmed bool) []OntologyTerm {
	out := EffectiveOntology(tenantTerms, tenantConfirmed)
	for _, seed := range BaseSeedDocumentClasses() {
		overridden := false
		for _, t := range out {
			if strings.EqualFold(t.Name, seed.Name) {
				overridden = true
				break
			}
		}
		if !overridden {
			out = append(out, seed)
		}
	}
	return out
}

// ReservedStructuralTypes are the values of chunks.type the document primitive itself
// writes. They are not classes, cannot be declared as one, and list_types reports them
// separately so an operator does not mistake a section chunk for an untyped class.
func ReservedStructuralTypes() []string {
	return []string{"root", "section", "image", "mermaid", "canvas"}
}

// IsReservedStructuralType reports whether a type is one the primitive owns.
func IsReservedStructuralType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, r := range ReservedStructuralTypes() {
		if t == r {
			return true
		}
	}
	return false
}

// AllFieldSpecs is a class's complete typed field set: inherited first, then declared.
func AllFieldSpecs(t OntologyTerm) []FieldSpec {
	if len(t.InheritedSpecs) == 0 {
		return t.FieldSpecs
	}
	return append(append([]FieldSpec{}, t.InheritedSpecs...), t.FieldSpecs...)
}

// IdentityFields is the ordered list of fields whose values make a class's natural key:
// every @identity field it has (inherited first), or `title` when the class carries the
// bare @identity directive and no field is marked — the sugar for `title text @identity`.
func IdentityFields(t OntologyTerm) []string {
	var out []string
	for _, fs := range AllFieldSpecs(t) {
		if fs.Identity {
			out = append(out, fs.Name)
		}
	}
	if len(out) == 0 && (t.TitleIdentity || t.InheritedTitleIdentity) {
		out = []string{"title"}
	}
	return out
}

// IdentityKey derives a class's natural key, `<class>:<v1>|<v2>`, from the values of
// its identity fields. ok is false when the class has no identity fields or any of them
// is empty — a partial key would make two different things collide.
//
// `title` is read from fields first and falls back to the chunk's own title, because a
// document class whose key is its title should not need the title written twice.
//
// Values are trimmed, lowercased and whitespace-collapsed, so "Закон  України" and
// "закон україни" are one thing; the class name is lowercased because type names are
// matched case-insensitively everywhere else.
func IdentityKey(t OntologyTerm, fields map[string]any, title string) (string, bool) {
	ids := IdentityFields(t)
	if len(ids) == 0 {
		return "", false
	}
	vals := make([]string, 0, len(ids))
	for _, name := range ids {
		v := scalarString(fields[name])
		if v == "" && name == "title" {
			v = title
		}
		v = strings.Join(strings.Fields(strings.ToLower(v)), " ")
		if v == "" {
			return "", false
		}
		vals = append(vals, v)
	}
	return strings.ToLower(t.Name) + ":" + strings.Join(vals, "|"), true
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

// ReservedRootFields are the fields the runtime itself keeps on a document's root chunk.
// They are never a class's to declare, so they are never reported as undeclared: an
// operator told "classification is not declared by Law" on every write of a classified
// law would learn to ignore the warning that matters.
var ReservedRootFields = map[string]bool{"classification": true}

// ValidateFields checks a chunk's fields against its class's typed fields and returns
// one warning per problem: an unknown field, a value of the wrong type, a missing
// @required field.
//
// WARN, NEVER REJECT (RFC BZ §10.2, kept by RFC FO). Free-text fields are valid today;
// refusing a write because a field is undeclared would break every caller that wrote
// one before the class was typed. Identity collisions are the one refusal, and they are
// enforced by an index elsewhere, not here.
//
// title satisfies a `title` field, for the same reason IdentityKey reads it.
func ValidateFields(t OntologyTerm, fields map[string]any, title string) []string {
	specs := AllFieldSpecs(t)
	if len(specs) == 0 {
		return nil
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, fs := range specs {
		byName[fs.Name] = fs
	}
	var warns []string
	for _, fs := range specs {
		v, present := fields[fs.Name]
		if (!present || v == nil) && fs.Name == "title" && strings.TrimSpace(title) != "" {
			continue
		}
		if !present || v == nil {
			if fs.Required {
				warns = append(warns, "field "+fs.Name+" is required by "+t.Name+" and missing")
			}
			continue
		}
		if msg := checkFieldValue(fs, v); msg != "" {
			warns = append(warns, "field "+fs.Name+": "+msg)
		}
	}
	var unknown []string
	for name := range fields {
		if ReservedRootFields[name] {
			continue
		}
		if _, ok := byName[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		warns = append(warns, "field "+name+" is not declared by "+t.Name)
	}
	return warns
}

func checkFieldValue(fs FieldSpec, v any) string {
	switch fs.Type {
	case FieldNumber:
		switch x := v.(type) {
		case float64, int, int64:
			return ""
		case string:
			if _, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return ""
			}
		}
		return "expected a number"
	case FieldDate:
		switch x := v.(type) {
		case float64, int, int64:
			return "" // unix time
		case string:
			s := strings.TrimSpace(x)
			for _, layout := range []string{"2006-01-02", time.RFC3339, time.RFC3339Nano, "2006-01"} {
				if _, err := time.Parse(layout, s); err == nil {
					return ""
				}
			}
		}
		return "expected a date (YYYY-MM-DD or RFC 3339)"
	case FieldEnum:
		s := scalarString(v)
		if len(fs.Enum) == 0 {
			return ""
		}
		for _, e := range fs.Enum {
			if strings.EqualFold(e, s) {
				return ""
			}
		}
		return strconv.Quote(s) + " is not one of " + joinComma(fs.Enum)
	default: // text, concept, ref, anchor: a scalar string-ish value
		switch v.(type) {
		case map[string]any, []any:
			return "expected a single value, got a structure"
		}
		return ""
	}
}
