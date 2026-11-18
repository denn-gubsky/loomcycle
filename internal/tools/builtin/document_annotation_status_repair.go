package builtin

// document_annotation_status_repair.go — the save-time repair of a done
// document's annotation that still calls the document a proposal.
//
// A model summarising an RFC reads its text, which was written while the RFC
// was a proposal ("This RFC proposes …", "Status: awaiting approval"), and
// repeats that framing even when the request says the document is done. The
// stored status is the root chunk's (documents.status is its mirror, written
// on every root write), so for a done document the framing is false. This
// repair removes it, and nothing else:
//
//   - at the start of a field, a leading "Design proposal", "A design
//     proposal", "<x>-side design proposal", "Proposal for/to …" or "Design
//     RFC" naming the document (a bare "Proposal" needs the connective: a
//     done document may well be about a proposal workflow);
//   - at the start of a sentence whose subject is the document itself ("This
//     RFC", "The document", the document's own code): "proposes", "would
//     add", "is a proposal for", "is awaiting approval";
//   - a sentence that states a status other than done ("Status is Design
//     LOCKED …").
//
// It never adds "shipped" or any other claim: a proposal noun becomes
// "design", a proposing verb becomes "specifies", and a false status sentence
// is dropped. A sentence whose subject is another document is not touched.
// Only document-level records are repaired: the stored status is the
// document's, and a section may well be its proposal.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// annotationRepairStatusWording: a done document's annotation called it a
// proposal; the framing was removed.
const annotationRepairStatusWording = "status_wording"

// annotationStatusDone is the stored status the repair applies to.
const annotationStatusDone = "done"

var (
	// leadingProposalRE is a field that opens by calling its document a
	// proposal: an optional article, an optional "<x>-side" qualifier, and the
	// noun, followed by a space, punctuation or the end.
	leadingProposalRE = regexp.MustCompile(`(?is)^(an?\s+|the\s+)?([\p{L}\p{N}]+-side\s+)?(design\s+proposal|proposal|design\s+rfc)(\s+|[:,—–]\s*)(.*)$`)
	// connectiveRE is a remainder that goes on with what the proposal is for.
	connectiveRE = regexp.MustCompile(`(?i)^(for|of|to|on|about)\s`)
	// leadingPunctRE is the punctuation a stripped noun may leave behind.
	leadingPunctRE = regexp.MustCompile(`^[:,—–-]\s*`)
	// genericSelfRef is how a text names its own document without its code.
	genericSelfRef = `(?:this|the)\s+(?:rfc|document|design\s+proposal|design|proposal)`
	// sentenceBody is the rest of a sentence: a full stop inside a version or a
	// date ("v1.2.3") does not end it.
	sentenceBody = `(?:[^.!?]|[.!?]\S)*`
)

// statusWording holds the sentence rules for one document: its own code is
// one of the subjects that name it.
type statusWording struct {
	proposes, wouldVerb, isProposal, awaiting, bareAwaiting, statusSentence *regexp.Regexp
}

func newStatusWording(ownCode string) statusWording {
	subj := genericSelfRef
	if ownCode = strings.TrimSpace(ownCode); ownCode != "" {
		subj = `(?:` + genericSelfRef + `|` + regexp.QuoteMeta(ownCode) + `)`
	}
	start := `(^|[.!?]\s+)`
	return statusWording{
		proposes:   regexp.MustCompile(`(?i)` + start + `(` + subj + `)(\s+)proposes\b`),
		wouldVerb:  regexp.MustCompile(`(?i)` + start + `(` + subj + `)(\s+)would\s+(add|introduce|extend|replace|remove|change|make|let|give|move)\b`),
		isProposal: regexp.MustCompile(`(?i)` + start + `(` + subj + `)(\s+is\s+an?\s+)(?:design\s+)?proposal\b`),
		awaiting: regexp.MustCompile(`(?i)` + start + `(` + subj + `)\s+is\s+(?:still\s+)?awaiting\s+(?:[\p{L}-]+\s+)?approval` +
			sentenceBody + `(?:[.!?]|$)`),
		bareAwaiting:   regexp.MustCompile(`(?i)` + start + `awaiting\s+(?:[\p{L}-]+\s+)?approval` + sentenceBody + `(?:[.!?]|$)`),
		statusSentence: regexp.MustCompile(`(?i)` + start + `((?:its\s+|the\s+)?status\s*(?:is\b|:)` + sentenceBody + `)(?:[.!?]|$)`),
	}
}

// repairStatusWording removes, from a done document's document-level brief and
// abstract, the wording that calls the document a proposal. ownCode is the
// document's own code as its title carries it ("RFC GL"), or "".
func repairStatusWording(level, status, ownCode, brief, abstract string) (string, string, []annotationRepair) {
	if level != annotationLevelDocument || !strings.EqualFold(strings.TrimSpace(status), annotationStatusDone) {
		return brief, abstract, nil
	}
	w := newStatusWording(ownCode)
	var repairs []annotationRepair
	brief = w.repair("brief", brief, &repairs)
	abstract = w.repair("abstract", abstract, &repairs)
	return cutOnWord(brief, maxAnnotationBriefChars), cutOnWord(abstract, maxAnnotationAbstractChars), repairs
}

// repair applies the leading rule, then each sentence rule, to one field.
func (w statusWording) repair(field, text string, repairs *[]annotationRepair) string {
	add := func(from, to string) {
		*repairs = append(*repairs, annotationRepair{Field: field, From: strings.TrimSpace(from), To: strings.TrimSpace(to), Reason: annotationRepairStatusWording})
	}
	if out, from, to, ok := rewordLeadingProposal(text); ok {
		add(from, to)
		text = out
	}
	// Each sentence rule keeps the boundary it matched (the previous sentence's
	// end and the space after it).
	text = replaceReporting(w.proposes, text, func(m []string) string { return m[1] + m[2] + m[3] + "specifies" }, add)
	text = replaceReporting(w.wouldVerb, text, func(m []string) string { return m[1] + m[2] + m[3] + strings.ToLower(m[4]) + "s" }, add)
	text = replaceReporting(w.isProposal, text, func(m []string) string { return m[1] + m[2] + m[3] + "design" }, add)
	text = dropSentences(w.awaiting, text, func([]string) bool { return true }, add)
	text = dropSentences(w.bareAwaiting, text, func([]string) bool { return true }, add)
	text = dropSentences(w.statusSentence, text, func(m []string) bool {
		return !strings.Contains(strings.ToLower(m[2]), annotationStatusDone)
	}, add)
	return text
}

// rewordLeadingProposal rewrites a field that opens by calling its document a
// proposal, returning the new text and the span changed. "Design proposal
// (RFC AM) for a Web UI" becomes "RFC AM: a Web UI"; "Design proposal RFC GL,
// done" becomes "RFC GL, done"; "A design proposal for X" becomes "A design
// for X"; "Design RFC GL" becomes "RFC GL".
func rewordLeadingProposal(text string) (out, from, to string, ok bool) {
	m := leadingProposalRE.FindStringSubmatch(text)
	if m == nil {
		return text, "", "", false
	}
	article, qualifier, noun, sep, rest := strings.ToLower(strings.TrimSpace(m[1])), strings.TrimSpace(m[2]), strings.ToLower(m[3]), m[4], m[5]
	if strings.TrimSpace(rest) == "" {
		return text, "", "", false
	}
	consumed := len(text) - len(rest)
	if strings.HasPrefix(noun, "design") && strings.HasSuffix(noun, "rfc") {
		head := "RFC"
		switch article {
		case "a", "an":
			head = "An RFC"
		case "the":
			head = "The RFC"
		}
		return head + sep + rest, text[:consumed], head + sep, true
	}
	if noun == "proposal" && !connectiveRE.MatchString(rest) {
		return text, "", "", false
	}
	if strings.HasPrefix(rest, "(") {
		// Only a parenthesised name ("(RFC AM)") becomes the head; "(draft)"
		// is left as written.
		if end := strings.Index(rest, ")"); end > 1 && startsUpperOrDigit(rest[1:end]) {
			inner, after := rest[1:end], strings.TrimLeftFunc(rest[end+1:], unicode.IsSpace)
			if c := connectiveRE.FindString(after); strings.EqualFold(strings.TrimSpace(c), "for") {
				head := inner + ": "
				return head + after[len(c):], text[:len(text)-len(after)+len(c)], head, true
			}
			head := inner
			if after != "" && !strings.ContainsAny(after[:1], ",;:.") {
				head += " "
			}
			return head + after, text[:len(text)-len(after)], head, true
		}
		return text, "", "", false
	}
	if connectiveRE.MatchString(rest) {
		// The article and an "<x>-side" qualifier stay: only the noun changes.
		head := upperFirst(strings.TrimSpace(strings.Join([]string{article, qualifier, "design"}, " ")))
		head = strings.Join(strings.Fields(head), " ") + " "
		return head + rest, text[:consumed], head, true
	}
	rest = leadingPunctRE.ReplaceAllString(rest, "")
	if rest == "" {
		return text, "", "", false
	}
	return upperFirst(rest), text[:len(text)-len(rest)], "", true
}

// startsUpperOrDigit reports whether s opens with an upper-case letter or a
// digit, as a name or a code does.
func startsUpperOrDigit(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r) || unicode.IsDigit(r)
}

// replaceReporting replaces each match of re with repl(submatches), reporting
// the matched span and its replacement.
func replaceReporting(re *regexp.Regexp, text string, repl func([]string) string, add func(from, to string)) string {
	return re.ReplaceAllStringFunc(text, func(s string) string {
		m := re.FindStringSubmatch(s)
		to := repl(m)
		add(s[len(m[1]):], to[len(m[1]):])
		return to
	})
}

// dropSentences removes each matched sentence drop accepts, leaving the
// previous sentence's end in place.
func dropSentences(re *regexp.Regexp, text string, drop func([]string) bool, add func(from, to string)) string {
	dropped := false
	out := re.ReplaceAllStringFunc(text, func(s string) string {
		m := re.FindStringSubmatch(s)
		if !drop(m) {
			return s
		}
		dropped = true
		add(s[len(m[1]):], "")
		return strings.TrimRightFunc(m[1], unicode.IsSpace)
	})
	if !dropped {
		return text
	}
	return strings.TrimSpace(out)
}

// upperFirst upper-cases the first rune of s.
func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
