package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// LongMemEval support (RFC CV / CW's second gate).
//
// WHY A SECOND HARNESS AT ALL. LoCoMo measures QA about ONE long conversation.
// loomcycle is a runtime for application agents whose memory is preferences,
// decisions and organisational knowledge across MANY sessions — and two of the
// things this design must not regress, knowledge-update and abstention, have no
// LoCoMo equivalent at all. Supersede-not-delete and the NOT_FOUND-strict
// answerer are exactly what those slices test.
//
// WHY AN ADAPTER RATHER THAN A NEW COMMAND. Everything structural is already
// here and dataset-independent: the MCP client, `Memory op=add` ingest, the
// answerer and judge agents, per-category aggregation, the paired report shape
// and the McNemar joiner. Only the loader and the task-type slicing are new, so
// LongMemEval enters as a second loader producing the SAME Conversation values
// the LoCoMo path produces. A separate command would have had to copy the parts
// that must not drift between the two harnesses, or those parts would have had
// to move to an internal package first — a refactor this does not need.
//
// The dataset is fetched at run time and never vendored, matching the LoCoMo
// rule. Note the licences differ and the difference is real: LoCoMo is
// CC BY-NC 4.0, so a derived fixture may not be committed; LongMemEval is MIT,
// so one COULD be. It still is not, so that provenance has a single source and
// the repo does not carry a 100MB+ corpus.
//
//	longmemeval_oracle.json      only the evidence sessions   (smallest; start here)
//	longmemeval_s_cleaned.json   ~40 sessions per instance    (~115k tokens of history)
//	longmemeval_m_cleaned.json   ~500 sessions per instance
//
// from https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned

// LongMemEval question types, numbered from 101 so they cannot be confused with
// LoCoMo's 1-4 in a report, a filter or a joined result. A shared numbering
// would silently merge two datasets' slices the first time someone compared
// them.
const (
	LMECatSingleSessionUser = 101 + iota
	LMECatSingleSessionAssistant
	LMECatSingleSessionPreference
	LMECatMultiSession
	LMECatTemporalReasoning
	LMECatKnowledgeUpdate
)

var lmeTypeToCategory = map[string]int{
	"single-session-user":       LMECatSingleSessionUser,
	"single-session-assistant":  LMECatSingleSessionAssistant,
	"single-session-preference": LMECatSingleSessionPreference,
	"multi-session":             LMECatMultiSession,
	"temporal-reasoning":        LMECatTemporalReasoning,
	"knowledge-update":          LMECatKnowledgeUpdate,
}

// LMECategoryName names a LongMemEval slice for a report. Returns "" for
// anything outside the LongMemEval range, so CategoryName can chain to it
// without having to know which dataset produced a result.
func LMECategoryName(c int) string {
	switch c {
	case LMECatSingleSessionUser:
		return "single-session-user"
	case LMECatSingleSessionAssistant:
		return "single-session-assistant"
	case LMECatSingleSessionPreference:
		return "single-session-preference"
	case LMECatMultiSession:
		return "multi-session"
	case LMECatTemporalReasoning:
		return "temporal-reasoning"
	case LMECatKnowledgeUpdate:
		return "knowledge-update"
	}
	return ""
}

// lmeTurn is one turn of history. has_answer marks the turns carrying the
// evidence — the dataset's own turn-level recall label, which becomes this
// harness's `Expected` answer key.
type lmeTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer"`
}

// lmeInstance is one of the 500 evaluation instances, per the dataset's
// documented format.
type lmeInstance struct {
	QuestionID   string      `json:"question_id"`
	QuestionType string      `json:"question_type"`
	Question     string      `json:"question"`
	Answer       string      `json:"answer"`
	QuestionDate string      `json:"question_date"`
	SessionIDs   []string    `json:"haystack_session_ids"`
	Dates        []string    `json:"haystack_dates"`
	Sessions     [][]lmeTurn `json:"haystack_sessions"`
}

// IsAbstention reports whether the gold behaviour is to REFUSE.
//
// The dataset encodes this in the id rather than the type: a `_abs` suffix means
// the history does not contain the answer, so abstaining is CORRECT. This
// inverts the harness's scoring, which treats NOT_FOUND as a miss because every
// LoCoMo category is answerable. Getting this wrong would score a system's
// correct refusals as failures and reward one that confabulates — the opposite
// of what the abstention slice exists to measure.
func (i lmeInstance) IsAbstention() bool { return strings.HasSuffix(i.QuestionID, "_abs") }

// LoadLongMemEval reads the dataset and converts each instance into one
// Conversation, so the rest of the harness is unchanged.
//
// ONE INSTANCE = ONE CONVERSATION = ONE SCOPE_ID. Mandatory, not tidiness, and
// for a sharper reason than LoCoMo's: instances SHARE haystack sessions, so a
// single keyspace would let a question about one instance retrieve another's
// evidence and score a hit the system never earned.
//
// limit>0 keeps the first N instances in file order. Deterministic on purpose —
// every arm of a paired comparison must see the same instances, and a random
// sample would make two arms incomparable while looking fine.
func LoadLongMemEval(path string, limit int) ([]Conversation, LMEDefects, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, LMEDefects{}, err
	}
	var raw []lmeInstance
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, LMEDefects{}, fmt.Errorf("longmemeval: %s is not the documented array-of-instances shape: %w", path, err)
	}
	var (
		out      []Conversation
		defects  LMEDefects
		abstains int
	)
	for _, inst := range raw {
		if limit > 0 && len(out) >= limit {
			break
		}
		cat, ok := lmeTypeToCategory[inst.QuestionType]
		if !ok {
			defects.UnknownType++
			continue
		}
		if strings.TrimSpace(inst.Question) == "" {
			defects.NoQuestion++
			continue
		}
		abstention := inst.IsAbstention()

		conv := Conversation{SampleID: "lme-" + inst.QuestionID}
		var expected []string
		for si, sess := range inst.Sessions {
			// The session DATE is prefixed into the turn text by Turn.Body(), the
			// same treatment LoCoMo needs: the timestamp lives on the session, and
			// a row without it is unretrievable for temporal questions no matter how
			// good the embedder is. It is also what the extractor now reads to stamp
			// observed_at, so the date has to reach the turn rather than stay in a
			// sidecar field.
			date := ""
			if si < len(inst.Dates) {
				date = inst.Dates[si]
			}
			for ti, t := range sess {
				id := fmt.Sprintf("%s:s%d:t%d", inst.QuestionID, si, ti)
				conv.Turns = append(conv.Turns, Turn{
					DiaID:    id,
					Session:  si,
					DateTime: date,
					Speaker:  lmeSpeaker(t.Role),
					Text:     t.Content,
				})
				if t.HasAnswer {
					expected = append(expected, id)
				}
			}
		}
		if len(conv.Turns) == 0 {
			defects.NoHistory++
			continue
		}
		// An abstention instance has no evidence turns BY CONSTRUCTION — that is
		// the whole point of it — so an empty Expected is correct there and a
		// defect anywhere else. Dropping them would remove exactly the slice this
		// harness was adopted for.
		if len(expected) == 0 && !abstention {
			defects.NoEvidence++
			continue
		}
		if abstention {
			abstains++
		}
		conv.Queries = []Query{{
			Question: inst.Question,
			Category: cat,
			Expected: expected,
			Answer:   inst.Answer,
			Abstain:  abstention,
		}}
		out = append(out, conv)
	}
	defects.Instances = len(out)
	defects.Abstention = abstains
	sort.SliceStable(out, func(i, j int) bool { return out[i].SampleID < out[j].SampleID })
	return out, defects, nil
}

// lmeSpeaker maps a role to a speaker name. LayerMessages derives roles back
// from speaker identity by first-seen order, so the names only need to be
// stable and distinct — but "user" must be seen first or the two roles invert.
func lmeSpeaker(role string) string {
	if strings.EqualFold(role, "assistant") {
		return "assistant"
	}
	return "user"
}

// LMEDefects is the load report. Every dropped instance is COUNTED rather than
// logged and forgotten: a silent drop changes the denominator, and a benchmark
// whose denominator moved without saying so is how two arms stop being
// comparable.
type LMEDefects struct {
	Instances   int
	Abstention  int
	UnknownType int
	NoQuestion  int
	NoHistory   int
	NoEvidence  int
}

func (d LMEDefects) String() string {
	return fmt.Sprintf("%d instances (%d abstention); dropped: %d unknown type, %d no question, %d no history, %d no evidence",
		d.Instances, d.Abstention, d.UnknownType, d.NoQuestion, d.NoHistory, d.NoEvidence)
}
