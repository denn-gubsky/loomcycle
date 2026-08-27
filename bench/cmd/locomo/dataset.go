package main

// dataset.go — LoCoMo (locomo10.json) parsing, and its conversion into
// loomcycle memory rows plus retrieval queries.
//
// THE DATASET IS NOT VENDORED. LoCoMo ships under CC BY-NC 4.0, so this
// harness reads a copy the operator downloaded themselves (--data) and writes
// nothing derived from it into the repo. See README.md.
//
// The load-bearing property is qa[].evidence: the dia_ids of the turns that
// support each answer. Keying every ingested turn by its dia_id turns each
// annotation into retrieval ground truth, which is why the retrieval axis
// needs no LLM judge and no answer-string matching.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Categories as released.
//
// The id -> label mapping is DERIVED from the released category counts, not
// copied from documentation the file carries (it carries none): the file holds
// 282/321/96/841 questions for categories 1/2/3/4, matching the published
// 282 multi-hop, 321 temporal, 96 open-domain and 841 single-hop exactly. If a
// future release renumbers them, every report is mislabelled — re-check the
// counts before trusting the labels.
//
// 5 is adversarial and 444 of its 446 questions have a null answer, which is
// why both Mem0 and Zep excluded it; 1-4 are the 1,540 questions everyone
// else reports on.
const (
	CategoryMultiHop    = 1
	CategoryTemporal    = 2
	CategoryOpenDomain  = 3
	CategorySingleHop   = 4
	CategoryAdversarial = 5
)

// CategoryName labels a category in reports. Unknown ids render numerically so
// a future category does not silently print as one of these.
func CategoryName(c int) string {
	switch c {
	case CategoryMultiHop:
		return "multi-hop"
	case CategoryTemporal:
		return "temporal"
	case CategoryOpenDomain:
		return "open-domain"
	case CategorySingleHop:
		return "single-hop"
	case CategoryAdversarial:
		return "adversarial"
	default:
		return "category-" + strconv.Itoa(c)
	}
}

// DefaultCategories excludes 5 (adversarial): its ground-truth answers are
// missing, so it can be neither answered nor scored. Every report states the
// filter it ran under, because differing filters are exactly why published
// LoCoMo numbers are not comparable to one another.
var DefaultCategories = []int{CategoryMultiHop, CategoryTemporal, CategoryOpenDomain, CategorySingleHop}

// Turn is one dialogue turn of one session.
type Turn struct {
	DiaID    string
	Session  int
	DateTime string
	Speaker  string
	Text     string
	Caption  string
}

// Text renders the turn as the string that is both stored and embedded.
//
// The session timestamp is prefixed deliberately: the temporal category asks
// "when did X happen", and the date lives on the SESSION in LoCoMo, not in the
// turn text. A row without it is unretrievable for those questions no matter
// how good the embedder is, which would measure the dataset's shape rather
// than the memory stack.
func (t Turn) Body() string {
	var b strings.Builder
	if t.DateTime != "" {
		b.WriteString("[" + t.DateTime + "] ")
	}
	if t.Speaker != "" {
		b.WriteString(t.Speaker + ": ")
	}
	b.WriteString(t.Text)
	// Image turns carry a BLIP caption instead of (or alongside) prose. Keep it
	// — several questions are answerable only from the caption. That the caption
	// is sometimes inadequate is a documented dataset defect, not ours to fix.
	if c := strings.TrimSpace(t.Caption); c != "" && !strings.Contains(t.Text, c) {
		b.WriteString(" (shared an image: " + c + ")")
	}
	return b.String()
}

// Query is one QA annotation reduced to a retrieval probe.
type Query struct {
	Question string
	Category int
	// Expected holds the dia_ids of the supporting turns — the answer key for
	// recall/precision. Always non-empty (queries without resolvable evidence
	// are dropped and counted as a defect).
	Expected []string
	// Answer is the gold answer, kept for the later answer-accuracy phase. Empty
	// for the adversarial category.
	Answer string
}

// Conversation is one LoCoMo sample: its turns and its queries.
type Conversation struct {
	SampleID string
	Turns    []Turn
	Queries  []Query
}

// ScopeID is the memory scope_id this conversation is ingested under.
//
// One scope_id PER CONVERSATION is mandatory, not tidiness: dia_ids are
// numbered per conversation and collide heavily across them (the released file
// has 5,882 turns but only 1,033 distinct dia_ids). A single shared keyspace
// would both overwrite rows and let a question about one conversation retrieve
// another's turns.
func (c Conversation) ScopeID() string { return "locomo-" + c.SampleID }

// Defects counts everything the loader could not use. A benchmark that
// silently discards part of its answer key reports a number computed against a
// smaller key than it claims, so these are counted, printed, and carried into
// the report.
type Defects struct {
	MalformedEvidenceFragments   int      `json:"malformed_evidence_fragments"`
	UnresolvedEvidenceIDs        int      `json:"unresolved_evidence_ids"`
	QueriesWithoutEvidence       int      `json:"queries_without_evidence"`
	QueriesFilteredByCategory    int      `json:"queries_filtered_by_category"`
	SessionsDeclaredWithoutTurns int      `json:"sessions_declared_without_turns"`
	Examples                     []string `json:"examples,omitempty"`
}

// maxDefectExamples bounds the examples list so a systematically broken file
// cannot produce a report larger than the dataset.
const maxDefectExamples = 12

func (d *Defects) note(format string, args ...any) {
	if len(d.Examples) < maxDefectExamples {
		d.Examples = append(d.Examples, fmt.Sprintf(format, args...))
	}
}

// Any reports whether anything was discarded.
func (d Defects) Any() bool {
	return d.MalformedEvidenceFragments > 0 || d.UnresolvedEvidenceIDs > 0 ||
		d.QueriesWithoutEvidence > 0 || d.SessionsDeclaredWithoutTurns > 0
}

var (
	// diaIDRe is the only well-formed evidence shape.
	diaIDRe = regexp.MustCompile(`^D\d+:\d+$`)
	// evidenceSplitRe splits a raw entry that packed several ids into one string.
	evidenceSplitRe = regexp.MustCompile(`[;,\s]+`)
	sessionKeyRe    = regexp.MustCompile(`^session_(\d+)$`)
)

// parseEvidence splits one raw evidence entry into dia_ids, returning the
// fragments it could not read.
//
// LoCoMo's evidence field is dirty. Alongside clean "D23:1" strings the
// released file carries "D8:6; D9:17" (two ids in one entry), "D9:1 D4:4 D4:6"
// (space-separated), "D:11:26" (extra colon) and a bare "D". A parser that
// accepted only the clean form would drop those without a word and shrink the
// answer key underneath the metric, so this splits permissively and hands back
// what it rejected for the caller to count.
func parseEvidence(raw string) (ids []string, malformed []string) {
	for _, frag := range evidenceSplitRe.Split(strings.TrimSpace(raw), -1) {
		if frag == "" {
			continue
		}
		if diaIDRe.MatchString(frag) {
			ids = append(ids, frag)
			continue
		}
		// "D:11:26" is a transposed "D11:26" — the only malformation frequent
		// enough to be worth repairing, and unambiguous when it repairs to a
		// well-formed id. Everything else is reported, never guessed at.
		if repaired := strings.Replace(frag, "D:", "D", 1); repaired != frag && diaIDRe.MatchString(repaired) {
			ids = append(ids, repaired)
			continue
		}
		malformed = append(malformed, frag)
	}
	return ids, malformed
}

// rawSample mirrors the released schema. answer is json.RawMessage because the
// field is a string for 1,536 questions, a bare number for 6, and null for the
// 444 adversarial ones.
type rawSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []rawQA                    `json:"qa"`
}

type rawQA struct {
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"`
	Category int             `json:"category"`
	Evidence []string        `json:"evidence"`
}

type rawTurn struct {
	Speaker     string `json:"speaker"`
	DiaID       string `json:"dia_id"`
	Text        string `json:"text"`
	BlipCaption string `json:"blip_caption"`
}

// goldAnswer renders the answer field's several JSON types as one string.
func goldAnswer(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.TrimSpace(string(raw))
}

// Load reads locomo10.json and converts it, keeping only the named categories.
func Load(path string, categories []int) ([]Conversation, *Defects, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read dataset: %w", err)
	}
	return Parse(b, categories)
}

// Parse converts an in-memory locomo10.json.
func Parse(b []byte, categories []int) ([]Conversation, *Defects, error) {
	var samples []rawSample
	if err := json.Unmarshal(b, &samples); err != nil {
		return nil, nil, fmt.Errorf("parse dataset: %w", err)
	}
	if len(samples) == 0 {
		return nil, nil, fmt.Errorf("parse dataset: no samples")
	}
	keep := map[int]bool{}
	for _, c := range categories {
		keep[c] = true
	}

	defects := &Defects{}
	out := make([]Conversation, 0, len(samples))
	for _, s := range samples {
		conv := Conversation{SampleID: s.SampleID}
		byID := map[string]bool{}

		// Sessions in chronological order. 288 session_N_date_time keys exist
		// against 272 session_N turn lists in the released file, so a session can
		// be declared with no content — iterate the turn lists, not the timestamps.
		nums := make([]int, 0, len(s.Conversation))
		for k := range s.Conversation {
			if m := sessionKeyRe.FindStringSubmatch(k); m != nil {
				n, _ := strconv.Atoi(m[1])
				nums = append(nums, n)
			}
		}
		sort.Ints(nums)
		for k := range s.Conversation {
			if !strings.HasSuffix(k, "_date_time") {
				continue
			}
			base := strings.TrimSuffix(k, "_date_time")
			if !sessionKeyRe.MatchString(base) {
				continue
			}
			if _, ok := s.Conversation[base]; !ok {
				defects.SessionsDeclaredWithoutTurns++
				defects.note("%s: %s has a timestamp but no turns", s.SampleID, base)
			}
		}

		for _, n := range nums {
			key := "session_" + strconv.Itoa(n)
			var turns []rawTurn
			if err := json.Unmarshal(s.Conversation[key], &turns); err != nil {
				return nil, nil, fmt.Errorf("parse %s %s: %w", s.SampleID, key, err)
			}
			var when string
			if raw, ok := s.Conversation[key+"_date_time"]; ok {
				_ = json.Unmarshal(raw, &when)
			}
			for _, t := range turns {
				if strings.TrimSpace(t.DiaID) == "" {
					continue
				}
				conv.Turns = append(conv.Turns, Turn{
					DiaID: t.DiaID, Session: n, DateTime: when,
					Speaker: t.Speaker, Text: t.Text, Caption: t.BlipCaption,
				})
				byID[t.DiaID] = true
			}
		}

		for _, q := range s.QA {
			if !keep[q.Category] {
				defects.QueriesFilteredByCategory++
				continue
			}
			var expected []string
			seen := map[string]bool{}
			for _, raw := range q.Evidence {
				ids, bad := parseEvidence(raw)
				for _, frag := range bad {
					defects.MalformedEvidenceFragments++
					defects.note("%s: unreadable evidence %q on %q", s.SampleID, frag, truncate(q.Question, 60))
				}
				for _, id := range ids {
					// An id naming a turn this conversation does not contain cannot be
					// retrieved by anyone. Counting it as expected would charge the
					// memory stack for the dataset's error.
					if !byID[id] {
						defects.UnresolvedEvidenceIDs++
						defects.note("%s: evidence %s names no turn in this conversation", s.SampleID, id)
						continue
					}
					if !seen[id] {
						seen[id] = true
						expected = append(expected, id)
					}
				}
			}
			if len(expected) == 0 {
				// Open-domain questions draw on world knowledge and legitimately have
				// no supporting turn; they are unscoreable on a retrieval metric.
				defects.QueriesWithoutEvidence++
				defects.note("%s: no usable evidence for %q (category %d)", s.SampleID, truncate(q.Question, 60), q.Category)
				continue
			}
			conv.Queries = append(conv.Queries, Query{
				Question: q.Question, Category: q.Category,
				Expected: expected, Answer: goldAnswer(q.Answer),
			})
		}
		out = append(out, conv)
	}
	return out, defects, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
