package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Cross-session synthesis support (RFC DB).
//
// WHY A THIRD CORPUS. LoCoMo and LongMemEval both measure RETRIEVAL: find the
// turn that holds the answer. RFC CV's P5 claims something retrieval cannot
// show — that following typed relations beats fetching more rows — and neither
// corpus can test it, for reasons that were measured rather than argued. In
// LoCoMo 284 of 285 extracted facts carry exactly one subject, so the corpus has
// no B in the middle of an A->B->C chain; LongMemEval's questions are
// single-session by construction. So the corpus is BUILT, and built means every
// property that matters is enforced by the generator rather than hoped for:
// bench/synthesis/gen_corpus.py checks that no question's supports share a
// session and that no single fact naming the question's anchor also names its
// answer. The second is the one that matters — a question one hop can answer
// measures retrieval volume, not traversal.
//
// WHY AN ADAPTER RATHER THAN A NEW COMMAND. Same reasoning the LongMemEval
// loader records: the MCP client, `Memory op=add` ingest, the answerer and judge
// agents, per-category aggregation, the paired report and the McNemar joiner are
// all dataset-independent already. Only the loader is new, so this enters as a
// third loader producing the SAME Conversation values.
//
// The corpus IS committed, unlike the other two. It is generated rather than
// licensed, it is 200KB rather than 100MB, and RFC DB section 7 pins extractor
// sampling so that extraction is the only stochastic part of the benchmark —
// which only holds if the corpus itself is byte-stable across runs.

// Synthesis categories are numbered by HOP COUNT so a per-category report reads
// directly as a per-depth one. They start at 200 so they cannot collide with
// LoCoMo's 1-4 or LongMemEval's 101-106 in a filter or a joined result.
const SynthCatBase = 200

// SynthCategory maps a question's hop count to its report category.
func SynthCategory(hops int) int { return SynthCatBase + hops }

type synthCorpus struct {
	Seed         int             `json:"seed"`
	EntityCounts map[string]int  `json:"entity_counts"`
	Facts        []synthFact     `json:"facts"`
	Sessions     []synthSession  `json:"sessions"`
	Questions    []synthQuestion `json:"questions"`
}

type synthFact struct {
	ID        string `json:"id"`
	Session   string `json:"session"`
	TurnDiaID string `json:"turn_dia_id"`
	Text      string `json:"text"`
}

type synthSession struct {
	ID    string      `json:"id"`
	Date  string      `json:"date"`
	Turns []synthTurn `json:"turns"`
}

type synthTurn struct {
	Role  string `json:"role"`
	DiaID string `json:"dia_id"`
	Text  string `json:"text"`
}

type synthQuestion struct {
	ID         string   `json:"id"`
	Hops       int      `json:"hops"`
	Q          string   `json:"q"`
	Gold       string   `json:"gold"`
	Anchor     string   `json:"anchor"`
	Relations  []string `json:"relations"`
	Supports   []string `json:"supports"`
	GuessFloor float64  `json:"guess_floor"`
}

// SynthDefects counts everything the loader could not use. Same rule as the
// other two loaders: a benchmark that silently discards part of its answer key
// reports a number computed against a smaller key than it claims.
type SynthDefects struct {
	QuestionsWithoutSupports int `json:"questions_without_supports"`
	UnresolvableSupports     int `json:"unresolvable_supports"`
	FactsWithoutTurn         int `json:"facts_without_turn"`
	SessionsWithoutTurns     int `json:"sessions_without_turns"`
}

func (d SynthDefects) Any() bool {
	return d.QuestionsWithoutSupports > 0 || d.UnresolvableSupports > 0 ||
		d.FactsWithoutTurn > 0 || d.SessionsWithoutTurns > 0
}

func (d SynthDefects) String() string {
	return fmt.Sprintf("%d questions without usable supports, %d unresolvable support ids, "+
		"%d facts not rendered into a turn, %d sessions without turns",
		d.QuestionsWithoutSupports, d.UnresolvableSupports, d.FactsWithoutTurn,
		d.SessionsWithoutTurns)
}

// Speaker names for the rendered turns. The extractor reads the speaker label,
// so it has to be stable and has to distinguish the two sides; the corpus is
// about third parties, so neither name carries meaning beyond that.
const (
	synthUserSpeaker      = "Operator"
	synthAssistantSpeaker = "Assistant"
)

// LoadSynthesis reads a generated synthesis corpus into the one Conversation the
// rest of the harness consumes.
//
// The WHOLE corpus is a single Conversation, not one per session. A Conversation
// is the unit that gets its own memory scope, and the entire point here is that
// the supporting facts live in DIFFERENT sessions of the SAME memory — split
// across scopes, every question becomes unanswerable for a reason that has
// nothing to do with traversal.
func LoadSynthesis(path string, limit int) ([]Conversation, SynthDefects, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, SynthDefects{}, err
	}
	var raw synthCorpus
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, SynthDefects{}, fmt.Errorf(
			"synthesis: %s is not the shape gen_corpus.py emits: %w", path, err)
	}
	if len(raw.Sessions) == 0 {
		return nil, SynthDefects{}, fmt.Errorf("synthesis: %s holds no sessions", path)
	}

	var defects SynthDefects

	// dia_id of the turn each fact was rendered into — the retrieval answer key
	// names the exact turn, not the session.
	turnOf := make(map[string]string, len(raw.Facts))
	for _, f := range raw.Facts {
		if strings.TrimSpace(f.TurnDiaID) == "" {
			defects.FactsWithoutTurn++
			continue
		}
		turnOf[f.ID] = f.TurnDiaID
	}

	conv := Conversation{SampleID: fmt.Sprintf("db2-%d", raw.Seed)}
	present := make(map[string]bool)
	for si, s := range raw.Sessions {
		if len(s.Turns) == 0 {
			defects.SessionsWithoutTurns++
			continue
		}
		for _, t := range s.Turns {
			speaker := synthUserSpeaker
			if t.Role == "assistant" {
				speaker = synthAssistantSpeaker
			}
			conv.Turns = append(conv.Turns, Turn{
				DiaID:    t.DiaID,
				Session:  si + 1,
				DateTime: s.Date,
				Speaker:  speaker,
				Text:     t.Text,
			})
			present[t.DiaID] = true
		}
	}

	for _, q := range raw.Questions {
		if limit > 0 && len(conv.Queries) >= limit {
			break
		}
		var expected []string
		bad := false
		for _, sup := range q.Supports {
			dia, ok := turnOf[sup]
			if !ok || !present[dia] {
				defects.UnresolvableSupports++
				bad = true
				continue
			}
			expected = append(expected, dia)
		}
		if bad || len(expected) == 0 {
			defects.QuestionsWithoutSupports++
			continue
		}
		conv.Queries = append(conv.Queries, Query{
			Question: q.Q,
			Category: SynthCategory(q.Hops),
			Expected: expected,
			Answer:   q.Gold,
			// Every synthesis question IS answerable from the history. The
			// abstention slice is the no-memory arm's job, not a gold behaviour.
			Abstain: false,
		})
	}
	if len(conv.Queries) == 0 {
		return nil, defects, fmt.Errorf("synthesis: %s yielded no scoreable questions", path)
	}
	return []Conversation{conv}, defects, nil
}
