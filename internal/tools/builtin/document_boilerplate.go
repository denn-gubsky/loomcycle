package builtin

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// document_boilerplate.go — which paragraphs of a chunk are boilerplate (RFC FP
// Phase C).
//
// A document class may declare @boilerplate <pattern>, one or more RE2 patterns over
// paragraphs: an amendment instruction, an effective-date clause, a review or
// ownership line — text a linker should draw no relation from. get_chunk reports the
// matching paragraphs as rune spans, for the reason definitional is reported rather
// than evaluated by the caller: the linker runs in JavaScript, whose regex reads
// (?i), (?P<name>…) and \b over Cyrillic differently from RE2.

// boilerplateSpans returns the rune spans of the paragraphs of body that its
// document's class marks as boilerplate; nil when there are none.
//
// A paragraph is a run of non-blank lines; its span runs from its first character to
// its last, without the line breaks around it. The class is the document root chunk's
// type, resolved case-insensitively against the confirmed classes, as for
// definitionalChunks; only a confirmed ontology governs. A store fault reading the
// class is returned, not read as "no boilerplate": a linker told a boilerplate
// paragraph is ordinary text draws relations from it.
func (d *Document) boilerplateSpans(ctx context.Context, key sqlmem.ScopeKey, row chunkRow, body string) ([][2]int, error) {
	if d.Store == nil || d.SqlMem == nil || row.DocumentID == "" || strings.TrimSpace(body) == "" {
		return nil, nil
	}
	patterns := d.classPatterns(ctx, "boilerplate")
	if len(patterns) == 0 {
		return nil, nil
	}
	res, err := d.query(ctx, key, `SELECT c.type FROM documents doc JOIN chunks c ON c.id = doc.root_chunk_id
		WHERE doc.id = ?`, row.DocumentID)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	p, ok := patterns[strings.ToLower(strings.TrimSpace(asStr(res.Rows[0][0])))]
	if !ok {
		return nil, nil
	}
	// The parser has validated and length-capped every line of the pattern; a compile
	// error here means "no pattern".
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, nil
	}
	return paragraphSpans(body, re), nil
}

// paragraphSpans returns the rune spans of the paragraphs of body that re matches. One
// pass: rune offsets are counted as the lines are walked, so a long chunk with many
// paragraphs costs its length, not its length times its paragraph count.
func paragraphSpans(body string, re *regexp.Regexp) [][2]int {
	var out [][2]int
	start, end := -1, -1         // byte offsets of the paragraph being read
	runeStart, runeEnd := -1, -1 // the same, in runes
	runes := 0                   // runes before the line being read
	flush := func() {
		if start >= 0 && re.MatchString(body[start:end]) {
			out = append(out, [2]int{runeStart, runeEnd})
		}
		start, end = -1, -1
	}
	for at := 0; at <= len(body); {
		nl := strings.IndexByte(body[at:], '\n')
		lineEnd := len(body)
		if nl >= 0 {
			lineEnd = at + nl
		}
		line := body[at:lineEnd]
		if strings.TrimSpace(line) == "" {
			flush()
		} else {
			lead := len(line) - len(strings.TrimLeft(line, " \t\r"))
			trimmed := strings.TrimRight(line, " \t\r")
			if start < 0 {
				start, runeStart = at+lead, runes+utf8.RuneCountInString(line[:lead])
			}
			end, runeEnd = at+len(trimmed), runes+utf8.RuneCountInString(trimmed)
		}
		runes += utf8.RuneCountInString(line)
		if nl < 0 {
			break
		}
		runes++ // the line break
		at = lineEnd + 1
	}
	flush()
	return out
}
