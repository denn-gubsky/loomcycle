package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
)

type genOptions struct {
	agents, turns, systemChars, turnChars, maxTokens int
	model, numCtx, effort, prefix                    string
	embedAgents, decisionAgents                      int
	embedModel, decisionModel                        string
	seed                                             int64
}

// writeWorkload writes one agentSpec per line. Every agent draws from its own
// seeded source, so two agents share no prompt prefix and the same flags always
// produce the same workload.
func writeWorkload(w io.Writer, g genOptions) error {
	if g.agents > 0 && g.model == "" {
		return fmt.Errorf("-gen: -model is required for chat agents")
	}
	if g.embedAgents > 0 && g.embedModel == "" {
		return fmt.Errorf("-gen: -embed-model is required for embed agents")
	}
	if g.decisionAgents > 0 && g.decisionModel == "" {
		return fmt.Errorf("-gen: -decision-model is required for decision agents")
	}
	var ctxs []int
	for _, s := range strings.Split(g.numCtx, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 {
			return fmt.Errorf("-num-ctx: %q is not a number", s)
		}
		ctxs = append(ctxs, n)
	}
	enc := json.NewEncoder(w)
	n := 0
	add := func(kind, model string, numCtx int, system bool) error {
		rng := rand.New(rand.NewSource(g.seed*1000 + int64(n)))
		name := fmt.Sprintf("%s%d", g.prefix, n)
		s := agentSpec{Agent: name, Kind: kind, Model: model, NumCtx: numCtx}
		if system {
			s.System = systemPrompt(rng, name, g.systemChars)
			s.MaxTokens, s.Effort = g.maxTokens, g.effort
		}
		for t := 0; t < g.turns; t++ {
			s.Turns = append(s.Turns, prose(rng, g.turnChars))
		}
		n++
		return enc.Encode(s)
	}
	for i := 0; i < g.agents; i++ {
		if err := add("chat", g.model, ctxs[i%len(ctxs)], true); err != nil {
			return err
		}
	}
	for i := 0; i < g.embedAgents; i++ {
		if err := add("embed", g.embedModel, 0, false); err != nil {
			return err
		}
	}
	for i := 0; i < g.decisionAgents; i++ {
		if err := add("decision", g.decisionModel, 0, false); err != nil {
			return err
		}
	}
	return nil
}

// systemPrompt is shaped like an agent's: instructions, then tool schemas, which
// is what makes a real system prompt long. The agent's name comes first, so two
// agents differ from the first token.
func systemPrompt(rng *rand.Rand, agent string, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an assistant that reads tool output and reports the facts in it.\n\nInstructions:\n", agent)
	b.WriteString(prose(rng, size/3))
	b.WriteString("\n\nTools:\n")
	for t := 0; b.Len() < size; t++ {
		fmt.Fprintf(&b, `{"name":"tool_%d_%s","description":%q,"input_schema":{"type":"object","properties":{"%s":{"type":"string"},"%s":{"type":"integer"}}}}`+"\n",
			t, word(rng), prose(rng, 200), word(rng), word(rng))
	}
	return b.String()
}

// prose is n characters of sentences over a fixed vocabulary.
func prose(rng *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		words := 6 + rng.Intn(10)
		for i := 0; i < words; i++ {
			w := word(rng)
			if i == 0 {
				w = strings.ToUpper(w[:1]) + w[1:]
			}
			b.WriteString(w)
			if i < words-1 {
				b.WriteByte(' ')
			}
		}
		fmt.Fprintf(&b, " (%d). ", rng.Intn(10000))
	}
	return b.String()[:n]
}

func word(rng *rand.Rand) string { return vocabulary[rng.Intn(len(vocabulary))] }

var vocabulary = strings.Fields(`server cluster replica queue request latency budget
invoice ledger balance payment account transfer report quarter revenue margin cost
sample model weight token cache memory window layer prefix suffix batch stream slot
deploy release version branch commit review test failure error retry timeout signal
disk volume backup snapshot restore index table column schema record field value
network packet route gateway proxy domain certificate secret policy scope tenant user
energy particle measure experiment result method variance error estimate interval
river mountain forest weather season harvest market price supply demand order
team project meeting decision owner task deadline status priority risk plan goal`)
