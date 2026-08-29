package main

// episodes.go — RFC CM-1: measure, build nothing.
//
// CM-1 asks one question before any episode tier is designed: does a coarser,
// curated unit beat dumping the raw turns in? Verbatim turns are the strongest
// configuration this harness has ever measured (answer 0.6915, retrieval recall@10
// 0.6675), so an episode tier that merely ties with them is machinery bought for
// nothing. This file provides the alternative units to compare against, and the
// observed-time stamping that RFC CL's `when` predicate needs.
//
// Nothing here writes an episode TIER. It builds the rows a tier would produce and
// measures them through the existing search and answer axes, which is the whole
// point of a phase that builds nothing.

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// sessionDateRe pulls the day out of LoCoMo's session stamp, e.g.
// "1:56 pm on 8 May, 2023". Only the DATE is taken: the clock time is present but
// every turn in a session shares it, so it carries no ordering information and
// pretending otherwise would stamp forty turns at the same instant with false
// precision.
var sessionDateRe = regexp.MustCompile(`on\s+(\d{1,2})\s+([A-Za-z]+),\s*(\d{4})`)

// ObservedAt renders the turn's session date as an RFC3339 instant, or "" when the
// stamp cannot be read.
//
// EMPTY ON FAILURE, never a guess. A wrong observed time is worse than none: it files
// the row under a day it does not belong to, where a window query finds it and a
// query for the right day does not.
func (t Turn) ObservedAt() string {
	m := sessionDateRe.FindStringSubmatch(t.DateTime)
	if m == nil {
		return ""
	}
	d, err := time.Parse("2 January 2006", fmt.Sprintf("%s %s %s", m[1], m[2], m[3]))
	if err != nil {
		return ""
	}
	return d.UTC().Format(time.RFC3339)
}

// SessionRow is one session's turns collapsed into a single stored row — the
// coarser unit CM-1 measures against per-turn.
type SessionRow struct {
	Key        string
	Body       string
	ObservedAt string
}

// SessionRows groups a conversation's turns by session, in session order.
//
// The KEY keeps a dia_id shape (`D<session>:s`) so the retrieval axis can still map
// a hit back to ground truth: LoCoMo's `qa.evidence` names turn ids, so a
// session-level row is scored as containing any of its turns' ids. That mapping is
// the reason this comparison is possible at all without re-annotating the corpus.
func SessionRows(conv Conversation) []SessionRow {
	order := []int{}
	byTurn := map[int][]Turn{}
	for _, t := range conv.Turns {
		if _, seen := byTurn[t.Session]; !seen {
			order = append(order, t.Session)
		}
		byTurn[t.Session] = append(byTurn[t.Session], t)
	}
	out := make([]SessionRow, 0, len(order))
	for _, sess := range order {
		turns := byTurn[sess]
		if len(turns) == 0 {
			continue
		}
		var b strings.Builder
		// The session stamp goes in ONCE at the top rather than on every turn: it is
		// the same date for all of them, and repeating it forty times would let the
		// date dominate the embedding of a row whose value is its content.
		b.WriteString("[" + turns[0].DateTime + "]\n")
		for _, t := range turns {
			b.WriteString(t.Speaker)
			b.WriteString(": ")
			b.WriteString(t.Text)
			if t.Caption != "" {
				b.WriteString(" (" + t.Caption + ")")
			}
			b.WriteString("\n")
		}
		out = append(out, SessionRow{
			Key:        fmt.Sprintf("D%d:s", sess),
			Body:       b.String(),
			ObservedAt: turns[0].ObservedAt(),
		})
	}
	return out
}

// SessionOfDiaID maps a turn id onto the session row key that would contain it, so
// a recall@k computed over session rows can be scored against turn-level ground
// truth. Returns "" for an unparseable id rather than inventing a session.
func SessionOfDiaID(diaID string) string {
	i := strings.Index(diaID, ":")
	if i <= 0 {
		return ""
	}
	return diaID[:i] + ":s"
}

// DateConstrained reports whether a question names an absolute date or window —
// "on October 3, 2023", "in the first weekend of August", "during July 2023".
//
// This is the slice RFC CL's `when` predicate exists for, and the only slice where
// it can help: a topic-shaped question has no window to pass. It is deliberately the
// SAME pattern the CL analysis used, so the measured 0.5484 baseline and any rerun
// are computed over the same set rather than over two similar-looking ones.
var dateConstrainedRe = regexp.MustCompile(
	`(?i)\b(on|in|during|first|second|third|last)\s+(week|weekend|month)?\s*(of\s+)?` +
		`(January|February|March|April|May|June|July|August|September|October|November|December|` +
		`\d{1,2}\s+\w+|\d{4})`)

func DateConstrained(question string) bool {
	return dateConstrainedRe.MatchString(question)
}
