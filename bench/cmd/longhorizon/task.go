package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// task.go — the synthetic horizon-scaling task for RFC CS P1.
//
// A deterministic COUNTER-TRACKING task: the agent is fed T instructions, one per
// step, each mutating a small set of named counters (SET/ADD/SUB/RESET). After the
// stream it must answer queries (GET a counter, SUM all, MAX key) from the final
// state. It is state-tracking with light arithmetic — enough that a context
// strategy which loses an early mutation gives a wrong answer, which is exactly
// what separates full-history (A0), reasoning-recap (A1), and structured-state (A2)
// at long horizons.
//
// Deterministic from a seed (reproducible), horizon-scalable (T), and gradable by
// an oracle that computes ground truth directly. Optional NOISE (distractor lines)
// and a DRIFT event (an external correction) exercise the two robustness axes RFC CS
// names. NOTE (RFC CS Q3): this task is maximally schema-able, so it likely
// OVERSTATES the A2 win — it is the clean-curve instrument; the marketing-research
// team run (P2) is the reality check.

// Op is a counter mutation.
type Op int

const (
	OpSet Op = iota
	OpAdd
	OpSub
	OpReset
	OpNote       // a distractor line (noise); does not mutate state
	OpCorrection // an external drift correction: sets a key to a value out-of-band
)

// Instruction is one step's observation.
type Instruction struct {
	Op    Op
	Key   string
	Value int
	Text  string // the rendered natural-language line the agent sees
}

// Query is a post-stream question with a known answer.
type Query struct {
	Kind   string // "get" | "sum" | "max"
	Key    string // for "get"
	Answer string // ground-truth answer, canonicalised
}

// Task is a generated instance.
type Task struct {
	Seed         int64
	Horizon      int
	Keys         []string
	Instructions []Instruction
	Queries      []Query
	FinalState   map[string]int // the oracle's ground-truth end state
}

// GenerateTask builds a deterministic instance. noiseRate in [0,1] is the fraction
// of steps that are distractor NOTE lines; drift, when true, injects one CORRECTION
// at ~the midpoint that changes a key out-of-band (the agent must honour it).
func GenerateTask(seed int64, horizon, nKeys int, noiseRate float64, drift bool) Task {
	r := rand.New(rand.NewSource(seed))
	keys := make([]string, nKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("counter_%02d", i)
	}
	state := map[string]int{}
	for _, k := range keys {
		state[k] = 0
	}

	driftAt := -1
	if drift {
		driftAt = horizon / 2
	}

	insns := make([]Instruction, 0, horizon)
	for t := 0; t < horizon; t++ {
		if t == driftAt {
			k := keys[r.Intn(nKeys)]
			v := r.Intn(100)
			state[k] = v // out-of-band: overrides whatever the stream implied
			insns = append(insns, Instruction{Op: OpCorrection, Key: k, Value: v,
				Text: fmt.Sprintf("CORRECTION: regardless of prior steps, %s is now exactly %d.", k, v)})
			continue
		}
		if noiseRate > 0 && r.Float64() < noiseRate {
			insns = append(insns, Instruction{Op: OpNote,
				Text: fmt.Sprintf("NOTE: shift %d completed a routine audit; no counter changes.", t)})
			continue
		}
		k := keys[r.Intn(nKeys)]
		switch r.Intn(4) {
		case 0:
			v := r.Intn(50)
			state[k] = v
			insns = append(insns, Instruction{Op: OpSet, Key: k, Value: v,
				Text: fmt.Sprintf("SET %s = %d.", k, v)})
		case 1:
			v := 1 + r.Intn(20)
			state[k] += v
			insns = append(insns, Instruction{Op: OpAdd, Key: k, Value: v,
				Text: fmt.Sprintf("ADD %d to %s.", v, k)})
		case 2:
			v := 1 + r.Intn(20)
			state[k] -= v
			insns = append(insns, Instruction{Op: OpSub, Key: k, Value: v,
				Text: fmt.Sprintf("SUBTRACT %d from %s.", v, k)})
		default:
			state[k] = 0
			insns = append(insns, Instruction{Op: OpReset, Key: k,
				Text: fmt.Sprintf("RESET %s to 0.", k)})
		}
	}

	// Queries: GET each key, plus SUM and MAX. Deterministic + fully covered.
	queries := make([]Query, 0, nKeys+2)
	for _, k := range keys {
		queries = append(queries, Query{Kind: "get", Key: k, Answer: fmt.Sprintf("%d", state[k])})
	}
	sum := 0
	for _, k := range keys {
		sum += state[k]
	}
	queries = append(queries, Query{Kind: "sum", Answer: fmt.Sprintf("%d", sum)})
	queries = append(queries, Query{Kind: "max", Answer: maxKey(state)})

	final := map[string]int{}
	for k, v := range state {
		final[k] = v
	}
	return Task{Seed: seed, Horizon: horizon, Keys: keys, Instructions: insns, Queries: queries, FinalState: final}
}

// maxKey returns the key with the largest value, ties broken by name (stable).
func maxKey(state map[string]int) string {
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best := keys[0]
	for _, k := range keys[1:] {
		if state[k] > state[best] {
			best = k
		}
	}
	return best
}

// QuestionText renders a query as the natural-language question posed to the model.
func (q Query) QuestionText() string {
	switch q.Kind {
	case "get":
		return fmt.Sprintf("What is the current value of %s? Answer with only the integer.", q.Key)
	case "sum":
		return "What is the sum of all counters right now? Answer with only the integer."
	case "max":
		return "Which counter currently has the highest value? Answer with only its name (e.g. counter_03)."
	}
	return ""
}

// Grade canonicalises a model answer and compares it to the ground truth. Integers
// are compared numerically (tolerating surrounding text); the max-key is compared as
// the first counter_NN token found.
func (q Query) Grade(modelAnswer string) bool {
	got := strings.TrimSpace(modelAnswer)
	switch q.Kind {
	case "get", "sum":
		return firstInt(got) == q.Answer
	case "max":
		return firstKey(got) == q.Answer
	}
	return false
}
