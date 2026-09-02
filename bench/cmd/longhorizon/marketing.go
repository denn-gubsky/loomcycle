package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// marketing.go — the RFC CS P2a "marketing-research" task: a harder, more
// confusable variant of the state-tracking benchmark. Instead of integer counters
// the model tracks a table of named COMPETITORS, each with a unique segment, a
// feature, and a monthly price that can be UPDATED mid-stream — interleaved with
// distractor market-report NOISE. Then it answers questions from the FINAL table
// (a competitor's latest price, which competitor owns a segment, the roster, the
// count). Named entities + reverse lookups + price drift are far more prone to
// long-context confusion than clean counters, so this is where the accuracy axis
// should finally separate A0 (full history) from A1/A2 (distilled context).
//
// Self-contained (its own task, strategies, runner) so the P1 counter benchmark is
// untouched; it reuses Message / ModelClient / Usage / RunResult / printSummary.
// Fixed, deterministic stream (not the full agentic SEARCH/NOTE/DRAFT loop — that
// is P2b); oracle-graded, no LLM judge needed.

// Competitor is one row of the market's ground truth.
type Competitor struct {
	Name    string
	Segment string
	Feature string
	Price   int
}

var (
	compNames = []string{"Acme", "Bolt", "Cirrus", "Delta", "Ember", "Flux", "Grove", "Helix", "Ion", "Juno", "Kite", "Lumen"}
	segments  = []string{"enterprises", "startups", "agencies", "freelancers", "nonprofits", "retailers", "clinics", "schools", "gyms", "studios", "hotels", "farms"}
	features  = []string{"SSO", "analytics", "automation", "white-label", "API-access", "24/7-support", "offline-mode", "audit-logs", "SLA", "multi-region", "SCIM", "webhooks"}
)

// MktEventKind classifies a stream line.
type MktEventKind int

const (
	MktIntro MktEventKind = iota // introduces a competitor (name/segment/feature/price)
	MktRepin                     // updates a competitor's price
	MktNoise                     // a distractor market-report line
)

// MktEvent is one observation fed to the model.
type MktEvent struct {
	Kind MktEventKind
	Text string
}

// MktQuery is a post-stream question with a known answer.
type MktQuery struct {
	Kind   string // "price" | "segment" | "who" | "count" | "list"
	Arg    string // competitor name (price/segment) or segment (who)
	Answer string
}

// MktTask is a generated instance.
type MktTask struct {
	Seed        int64
	Horizon     int
	Competitors []Competitor // introduction order
	Stream      []MktEvent
	Queries     []MktQuery
	Final       map[string]Competitor // oracle: latest state by name
}

// GenerateMktTask builds a deterministic instance: nComp competitors introduced in
// the first steps, then a mix of price REPINS and NOISE up to the horizon.
func GenerateMktTask(seed int64, horizon, nComp int, noiseRate float64) MktTask {
	r := rand.New(rand.NewSource(seed))
	if nComp > len(compNames) {
		nComp = len(compNames)
	}
	// Unique (name, segment, feature) per competitor for a clean oracle.
	names := append([]string(nil), compNames[:nComp]...)
	segs := append([]string(nil), segments...)
	feats := append([]string(nil), features...)
	r.Shuffle(len(segs), func(i, j int) { segs[i], segs[j] = segs[j], segs[i] })
	r.Shuffle(len(feats), func(i, j int) { feats[i], feats[j] = feats[j], feats[i] })

	comps := make([]Competitor, nComp)
	state := map[string]Competitor{}
	stream := make([]MktEvent, 0, horizon)
	for i := 0; i < nComp; i++ {
		c := Competitor{Name: names[i], Segment: segs[i], Feature: feats[i], Price: 20 + r.Intn(180)}
		comps[i] = c
		state[c.Name] = c
		stream = append(stream, MktEvent{Kind: MktIntro,
			Text: fmt.Sprintf("%s launched: it targets %s with %s at $%d/mo.", c.Name, c.Segment, c.Feature, c.Price)})
	}
	// Remaining steps: price repins (state drift) + noise.
	for t := len(stream); t < horizon; t++ {
		if noiseRate > 0 && r.Float64() < noiseRate {
			stream = append(stream, MktEvent{Kind: MktNoise,
				Text: fmt.Sprintf("Market report Q%d: the sector grew %d%% and analysts stayed neutral.", t, 3+r.Intn(20))})
			continue
		}
		name := names[r.Intn(nComp)]
		c := state[name]
		c.Price = 20 + r.Intn(180)
		state[name] = c
		stream = append(stream, MktEvent{Kind: MktRepin,
			Text: fmt.Sprintf("%s repriced its plan to $%d/mo.", name, c.Price)})
	}

	// Queries: price of each competitor, segment of a couple, a reverse who-owns,
	// the count, and the roster. Deterministic + fully answerable from Final.
	queries := make([]MktQuery, 0, nComp+4)
	for _, c := range comps {
		queries = append(queries, MktQuery{Kind: "price", Arg: c.Name, Answer: fmt.Sprintf("%d", state[c.Name].Price)})
	}
	queries = append(queries,
		MktQuery{Kind: "segment", Arg: comps[0].Name, Answer: comps[0].Segment},
		MktQuery{Kind: "who", Arg: comps[nComp/2].Segment, Answer: comps[nComp/2].Name},
		MktQuery{Kind: "count", Answer: fmt.Sprintf("%d", nComp)},
		MktQuery{Kind: "list", Answer: strings.Join(names, ",")},
	)

	final := map[string]Competitor{}
	for k, v := range state {
		final[k] = v
	}
	return MktTask{Seed: seed, Horizon: horizon, Competitors: comps, Stream: stream, Queries: queries, Final: final}
}

// QuestionText renders a query.
func (q MktQuery) QuestionText() string {
	switch q.Kind {
	case "price":
		return fmt.Sprintf("What is %s's current monthly price in dollars? Answer with only the integer.", q.Arg)
	case "segment":
		return fmt.Sprintf("Which market segment does %s target? Answer with only the one-word segment.", q.Arg)
	case "who":
		return fmt.Sprintf("Which competitor targets the %s segment? Answer with only the company name.", q.Arg)
	case "count":
		return "How many distinct competitors have been introduced? Answer with only the integer."
	case "list":
		return "List the names of all competitors introduced, comma-separated. Names only."
	}
	return ""
}

// Grade scores a model answer against the oracle.
func (q MktQuery) Grade(ans string) bool {
	got := strings.TrimSpace(ans)
	switch q.Kind {
	case "price", "count":
		return firstInt(got) == q.Answer
	case "segment":
		return strings.Contains(strings.ToLower(got), strings.ToLower(q.Answer))
	case "who":
		return strings.Contains(strings.ToLower(got), strings.ToLower(q.Answer))
	case "list":
		lo := strings.ToLower(got)
		for _, n := range strings.Split(q.Answer, ",") {
			if !strings.Contains(lo, strings.ToLower(n)) {
				return false
			}
		}
		return true
	}
	return false
}

// ── strategies (marketing domain) ────────────────────────────────────────────

func mktRules() string {
	return "You are tracking a competitive market. Facts arrive one at a time: a competitor 'launched' " +
		"(with its segment, feature, and monthly price), 'repriced' (its price changed — keep the LATEST), " +
		"or a 'Market report' line (ignore it — no competitor data). Maintain each competitor's segment, " +
		"feature, and current price. Later, answer questions from the current state only."
}

func mktStepUser(e MktEvent) string {
	return "Update: " + e.Text + "\nAcknowledge in one short line."
}

type MktStrategy interface {
	Name() string
	StepMessages(e MktEvent) []Message
	Observe(e MktEvent, response string)
	PendingRecap() []Message
	SetRecap(string)
	QueryMessages(q MktQuery) []Message
}

// MktA0 — full history.
type MktA0 struct{ history []Message }

func (s *MktA0) Name() string { return "A0-append" }
func (s *MktA0) StepMessages(e MktEvent) []Message {
	m := []Message{{Role: "system", Content: mktRules()}}
	m = append(m, s.history...)
	return append(m, Message{Role: "user", Content: mktStepUser(e)})
}
func (s *MktA0) Observe(e MktEvent, r string) {
	s.history = append(s.history, Message{Role: "user", Content: mktStepUser(e)}, Message{Role: "assistant", Content: r})
}
func (s *MktA0) PendingRecap() []Message { return nil }
func (s *MktA0) SetRecap(string)         {}
func (s *MktA0) QueryMessages(q MktQuery) []Message {
	m := []Message{{Role: "system", Content: mktRules()}}
	m = append(m, s.history...)
	return append(m, Message{Role: "user", Content: q.QuestionText()})
}

// MktA1 — last-N window + model-maintained recap.
type MktA1 struct {
	keepLastN int
	window    []Message
	recap     string
	pending   []Message
}

func (s *MktA1) Name() string { return "A1-recap" }
func (s *MktA1) preamble() []Message {
	m := []Message{{Role: "system", Content: mktRules()}}
	if s.recap != "" {
		m = append(m, Message{Role: "system", Content: "Competitor table so far (name → segment/feature/latest price):\n" + s.recap})
	}
	return m
}
func (s *MktA1) StepMessages(e MktEvent) []Message {
	m := s.preamble()
	m = append(m, s.window...)
	return append(m, Message{Role: "user", Content: mktStepUser(e)})
}
func (s *MktA1) Observe(e MktEvent, r string) {
	s.window = append(s.window, Message{Role: "user", Content: mktStepUser(e)}, Message{Role: "assistant", Content: r})
	if len(s.window) > 2*s.keepLastN {
		ev := s.window[:2]
		s.window = s.window[2:]
		prior := s.recap
		if prior == "" {
			prior = "(empty)"
		}
		s.pending = []Message{
			{Role: "system", Content: "Maintain a compact competitor table (name → segment/feature/latest price). " +
				"Given the prior table and the updates leaving the window, output the updated table only."},
			{Role: "user", Content: fmt.Sprintf("Prior table:\n%s\n\nUpdates leaving the window:\n%s\n%s\n\nUpdated table:", prior, ev[0].Content, ev[1].Content)},
		}
	}
}
func (s *MktA1) PendingRecap() []Message { p := s.pending; s.pending = nil; return p }
func (s *MktA1) SetRecap(r string)       { s.recap = strings.TrimSpace(r) }
func (s *MktA1) QueryMessages(q MktQuery) []Message {
	m := s.preamble()
	m = append(m, s.window...)
	return append(m, Message{Role: "user", Content: q.QuestionText()})
}

// MktA2 — explicit structured table, patched each step.
type MktA2 struct{ state map[string]map[string]any }

func (s *MktA2) Name() string { return "A2-stateful" }
func (s *MktA2) stateJSON() string {
	names := make([]string, 0, len(s.state))
	for n := range s.state {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(orderedMap(names, s.state))
	return string(b)
}
func (s *MktA2) preamble() []Message {
	return []Message{{Role: "system", Content: mktRules() +
		"\n\nYou are given the current TABLE as JSON (name → {segment, feature, price}). After applying the " +
		"update, reply with ONLY a JSON patch of the competitor(s) that changed, e.g. " +
		`{"Acme":{"price":99}} or {"Bolt":{"segment":"gyms","feature":"SLA","price":40}}. Reply {} if nothing changed. No prose.`}}
}
func (s *MktA2) StepMessages(e MktEvent) []Message {
	return append(s.preamble(), Message{Role: "user", Content: "TABLE: " + s.stateJSON() + "\nUpdate: " + e.Text + "\nPatch:"})
}
func (s *MktA2) Observe(e MktEvent, response string) {
	patch, ok := parsePatch(response)
	if !ok {
		return
	}
	for name, v := range patch {
		row, isObj := v.(map[string]any)
		if !isObj {
			continue
		}
		if s.state[name] == nil {
			s.state[name] = map[string]any{}
		}
		for k, val := range row {
			s.state[name][k] = val
		}
	}
}
func (s *MktA2) PendingRecap() []Message { return nil }
func (s *MktA2) SetRecap(string)         {}
func (s *MktA2) QueryMessages(q MktQuery) []Message {
	return append(
		[]Message{{Role: "system", Content: mktRules() + "\n\nThe current TABLE is given as JSON. Answer from it."}},
		Message{Role: "user", Content: "TABLE: " + s.stateJSON() + "\n" + q.QuestionText()})
}

// orderedMap returns a stable-key view for deterministic JSON.
func orderedMap(names []string, m map[string]map[string]any) *orderedJSON {
	return &orderedJSON{names: names, m: m}
}

type orderedJSON struct {
	names []string
	m     map[string]map[string]any
}

func (o *orderedJSON) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range o.names {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(n)
		vb, _ := json.Marshal(o.m[n])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func newMktStrategy(arm string, task MktTask, keepLastN int) MktStrategy {
	switch arm {
	case "A0":
		return &MktA0{}
	case "A1":
		return &MktA1{keepLastN: keepLastN}
	case "A2":
		st := map[string]map[string]any{}
		return &MktA2{state: st}
	}
	return nil
}

// MktRunOnce drives one marketing task through strat (mirror of RunOnce): apply the
// fact stream step by step, then answer the queries, accumulating tokens + accuracy.
func MktRunOnce(ctx context.Context, strat MktStrategy, task MktTask, client *ModelClient) RunResult {
	res := RunResult{Arm: strat.Name(), Model: client.model, Horizon: task.Horizon, Seed: task.Seed, Queries: len(task.Queries)}
	start := time.Now()
	acc := func(u Usage) {
		res.PromptTokens += u.Prompt
		res.CompletionTokens += u.Completion
		res.TotalTokens += u.Prompt + u.Completion
		if u.Prompt > res.PeakPromptTokens {
			res.PeakPromptTokens = u.Prompt
		}
	}
	for _, e := range task.Stream {
		resp, u, err := client.Call(ctx, strat.StepMessages(e))
		if err != nil {
			res.Err = err.Error()
			res.ElapsedMs = time.Since(start).Milliseconds()
			return res
		}
		acc(u)
		res.StepCalls++
		strat.Observe(e, resp)
		if rm := strat.PendingRecap(); rm != nil {
			recap, u2, err := client.Call(ctx, rm)
			if err != nil {
				res.Err = err.Error()
				res.ElapsedMs = time.Since(start).Milliseconds()
				return res
			}
			acc(u2)
			res.RecapCalls++
			strat.SetRecap(recap)
		}
	}
	for _, q := range task.Queries {
		ans, u, err := client.Call(ctx, strat.QueryMessages(q))
		if err != nil {
			res.Err = err.Error()
			res.ElapsedMs = time.Since(start).Milliseconds()
			return res
		}
		acc(u)
		res.QueryCalls++
		if q.Grade(ans) {
			res.Correct++
		}
	}
	if res.Queries > 0 {
		res.Accuracy = float64(res.Correct) / float64(res.Queries)
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res
}
