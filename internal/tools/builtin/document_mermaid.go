package builtin

// document_mermaid.go — what text to embed for a mermaid chunk (RFC BU phase 3).
//
// Embedding mermaid SOURCE is poor: `graph TD`, `-->`, `[`, `|` dominate the
// tokens and carry no meaning, so a diagram about authentication ranks against
// every other flowchart rather than against the word "authentication".
//
// Embedding a RENDERED description would work and is wrong here: it pays a vision
// call for information that is already text. That is the rule this file exists to
// apply — USE A MODEL ONLY WHEN THE CONTENT IS NOT ALREADY TEXT. Mermaid is text
// pretending to be a picture; an image is a picture. Treating them alike would cost
// a vision call per diagram for a worse result than a regex.
//
// ONE EXTRACTOR, NOT EIGHT PARSERS. Mermaid has a dozen dialects and each has its
// own grammar, but they share how they carry human language: labels sit in
// brackets, in quotes, after a colon, or between pipes. A per-dialect parser would
// buy marginal recall for a permanent maintenance burden — every new mermaid
// release could break one. So this extracts the label-bearing shapes generically
// and names the diagram kind, which is enough for retrieval and cannot silently
// stop working when mermaid adds a diagram type.

import (
	"regexp"
	"strings"
)

// mermaidKindRe reads the diagram kind from the first meaningful line.
var mermaidKindRe = regexp.MustCompile(`^\s*(graph|flowchart|sequenceDiagram|classDiagram|stateDiagram(?:-v2)?|erDiagram|gantt|pie|mindmap|journey|gitGraph|quadrantChart|timeline|C4Context)\b`)

// mermaidLabelRes are the shapes that carry human language, in the order a reader
// would encounter them. Each capture group is a candidate label.
var mermaidLabelRes = []*regexp.Regexp{
	// Bracketed node text, innermost first so [(Memory)] yields "Memory" rather
	// than "(Memory)". Also covers ((circle)), {rhombus}, >flag], [[subroutine]].
	regexp.MustCompile(`\[\(([^)\]]+)\)\]`),
	regexp.MustCompile(`\(\(([^)]+)\)\)`),
	regexp.MustCompile(`\[\[([^\]]+)\]\]`),
	regexp.MustCompile(`\[([^\]|]+)\]`),
	regexp.MustCompile(`\{\{?([^}]+)\}?\}`),
	regexp.MustCompile(`\(([^)]+)\)`),
	// Edge labels: -->|text| and -- text -->
	regexp.MustCompile(`\|([^|]+)\|`),
	regexp.MustCompile(`--\s+([^->|]+?)\s+--[->]`),
	// Quoted text (pie slices, C4, quadrant titles).
	regexp.MustCompile(`"([^"]+)"`),
	// `participant A as Alice` / `class Foo as Bar` — the alias is the human name.
	regexp.MustCompile(`\bas\s+([A-Za-z0-9_][^\n;]*)`),
	// Anything after a colon: sequence messages, state transition labels, gantt
	// task names, section titles.
	regexp.MustCompile(`:\s*([^\n:;]+)`),
}

// mermaidNoise are tokens that survive extraction but say nothing. Matched
// case-insensitively against a whole candidate, so a label CONTAINING one of these
// words is kept — only a label that is nothing but syntax is dropped.
var mermaidNoise = map[string]bool{
	"td": true, "tb": true, "lr": true, "rl": true, "bt": true,
	"as": true, "root": true, "of": true, "over": true, "right": true, "left": true,
	"participant": true, "actor": true, "class": true, "state": true,
	"section": true, "title": true, "autonumber": true, "direction": true,
	"subgraph": true, "end": true, "note": true, "loop": true, "alt": true,
	"else": true, "opt": true, "par": true, "and": true, "rect": true,
	"activate": true, "deactivate": true, "click": true, "style": true,
	"classdef": true, "linkstyle": true, "accdescr": true, "acctitle": true,
	// The diagram keywords themselves: pass 2 sweeps words, and the kind line's
	// keyword would otherwise appear in the labels (classDiagram leaked this way).
	"graph": true, "flowchart": true, "sequencediagram": true, "classdiagram": true,
	"statediagram": true, "statediagram-v2": true, "erdiagram": true, "gantt": true,
	"pie": true, "mindmap": true, "journey": true, "gitgraph": true,
	"quadrantchart": true, "timeline": true, "c4context": true,
	// Common gantt/date noise that survives the letter check.
	"dateformat": true, "axisformat": true, "todaymarker": true, "excludes": true,
}

// mermaidStripRe removes syntax so the FALLBACK (an unrecognised diagram kind with
// no extractable labels) still embeds words rather than punctuation.
var mermaidStripRe = regexp.MustCompile(`[-=.]{2,}[>ox|]?|[\[\]{}()|;>]+|--`)

// mermaidEmbedText renders a mermaid source into the text worth embedding.
//
// TWO PASSES, and the second is what makes this work across dialects.
//
// Pass 1 takes bracketed / quoted / piped text as PHRASES, so "Backfill plan"
// survives as a phrase rather than two tokens where mermaid marks it as one label.
//
// Pass 2 sweeps every remaining WORD after syntax removal. That pass is not a
// fallback — it is load-bearing. A first attempt at this used shape regexes alone
// and lost the content of half the dialects: erDiagram entity names (CHUNK,
// EMBEDDING) are bare identifiers, gantt task names sit BEFORE the colon while the
// metadata sits after it, and mindmap nodes are bare indented words. Anything that
// is a word survives; anything that is syntax does not.
//
// Returns "" when there is nothing to index — an empty diagram, or one that is
// purely structural. The caller then skips embedding rather than storing a vector
// for punctuation, which is a row that can only produce false matches.
func mermaidEmbedText(src string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	kind := ""
	var labels []string
	seen := map[string]bool{}
	var meaningful []string

	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		// %% is a mermaid comment. Dropped HERE so it cannot reach either pass —
		// the first version leaked comment text into the word sweep.
		if line == "" || strings.HasPrefix(line, "%%") {
			continue
		}
		if kind == "" {
			if m := mermaidKindRe.FindStringSubmatch(line); m != nil {
				kind = normalizeMermaidKind(m[1])
				// Drop only the keyword; the rest of the line may carry a title.
				line = strings.TrimSpace(strings.Replace(line, m[1], "", 1))
			}
		}
		meaningful = append(meaningful, line)
		for _, re := range mermaidLabelRes {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				addMermaidLabel(&labels, seen, m[1])
			}
		}
	}

	// Pass 2: every word that is not syntax, not a keyword, and not a bare node id.
	joined := strings.Join(meaningful, " ")
	for _, w := range strings.Fields(mermaidStripRe.ReplaceAllString(joined, " ")) {
		addMermaidLabel(&labels, seen, w)
	}

	if len(labels) == 0 {
		return ""
	}
	body := strings.Join(labels, " ")
	if kind == "" {
		return body
	}
	// The kind is prefixed rather than dropped: "flowchart" / "sequence diagram" is
	// itself a useful query term when someone asks for a diagram of something.
	return kind + ": " + body
}

// addMermaidLabel appends a cleaned candidate, deduped, preserving reading order.
func addMermaidLabel(labels *[]string, seen map[string]bool, candidate string) {
	s := strings.TrimSpace(candidate)
	s = strings.Trim(s, `"'`)
	// Mermaid's shape and edge punctuation, plus the member-visibility sigils
	// classDiagram uses (+writeBody → writeBody).
	s = strings.Trim(s, "()[]{}<>-|:,+#~*")
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	// An opening bracket whose partner was trimmed off the end: `MemorySet(body)`
	// reaches here as `MemorySet(body`. Cut at the bracket rather than keep a
	// half-token — the inner text was already captured by the bracket regex.
	if i := strings.IndexAny(s, "([{"); i > 0 && !strings.ContainsAny(s, ")]}") {
		s = strings.TrimSpace(s[:i])
		if s == "" {
			return
		}
	}
	low := strings.ToLower(s)
	if mermaidNoise[low] {
		return
	}
	// A bare node id says nothing: a single character, or a short all-caps/alnum
	// token with no vowel-bearing word in it (A1, B, CHUNK is kept because it is
	// long enough to be a name). The threshold is deliberately generous — a false
	// keep costs one token of noise, a false drop loses an entity name.
	if len(s) < 2 {
		return
	}
	if !strings.ContainsFunc(s, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	}) {
		// Pure numbers / dates / durations: gantt metadata, pie values.
		return
	}
	if seen[low] {
		return
	}
	seen[low] = true
	// A multi-word PHRASE claims its constituent words too, so the word sweep does
	// not re-add them individually. Without this, "embedder succeeded" is followed
	// by "embedder" and "succeeded" — the same content three times, which skews the
	// embedding toward whatever happens to be bracketed.
	if fields := strings.Fields(low); len(fields) > 1 {
		for _, w := range fields {
			seen[strings.Trim(w, "()[]{}<>-|:,+#~*")] = true
		}
	}
	*labels = append(*labels, s)
}

// normalizeMermaidKind turns a mermaid keyword into a phrase a human would search
// for. `graph` becomes "flowchart" because that is what mermaid's own docs call it
// and what someone looking for one would type.
func normalizeMermaidKind(kw string) string {
	switch kw {
	case "graph", "flowchart":
		return "flowchart"
	case "sequenceDiagram":
		return "sequence diagram"
	case "classDiagram":
		return "class diagram"
	case "stateDiagram", "stateDiagram-v2":
		return "state diagram"
	case "erDiagram":
		return "entity relationship diagram"
	case "gantt":
		return "gantt chart"
	case "pie":
		return "pie chart"
	case "mindmap":
		return "mindmap"
	case "journey":
		return "user journey"
	case "gitGraph":
		return "git graph"
	case "quadrantChart":
		return "quadrant chart"
	case "timeline":
		return "timeline"
	case "C4Context":
		return "C4 context diagram"
	}
	return kw
}
