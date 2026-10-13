// Command schedprobe measures what a model host does with several agents sharing
// it: how much of each prompt its KV cache saves, how that changes with the number
// of agents and with how calls are scheduled, and when it reloads a model. It is
// RFC EI phase P0 — the lane defaults (affinity hold, quantum, caps, loaded-model
// budget) are chosen from these numbers, under the rules in
// bench/docs/schedprobe/PREREG.md.
//
// It drives the SHIPPED Ollama chat driver, embedder and decision driver, so the
// requests are the ones loomcycle sends, and schedules them through a lane of its
// own (lane.go) that implements each policy being compared.
//
//	schedprobe -gen -agents 8 -turns 10 -model qwen3.6:latest -num-ctx 65536 > w.jsonl
//	schedprobe -workload w.jsonl -policy lease -quantum 4 -hold-ms 5000 \
//	    -slots chat=4,embed=2,decision=1 -label E3-q4-h5000 -out runs/E3-q4-h5000.jsonl
//
// Output: one {"type":"run"} header, then one {"type":"call"} record per call.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// agentSpec is one line of a workload file.
type agentSpec struct {
	Agent string `json:"agent"`
	// Kind is the lane the agent's calls take: chat, embed or decision.
	Kind      string `json:"kind"`
	Model     string `json:"model"`
	NumCtx    int    `json:"num_ctx,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Effort    string `json:"effort,omitempty"`
	// System is a chat agent's system prompt; Turns are the texts each call adds
	// (a chat turn, an embed batch's text, a decision's state).
	System string   `json:"system,omitempty"`
	Turns  []string `json:"turns"`
}

// callRecord is one call's measurements. Server-side fields are 0 when the server
// did not report them.
type callRecord struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Model  string `json:"model"`
	NumCtx int    `json:"num_ctx,omitempty"`
	Turn   int    `json:"turn"`
	Slot   int    `json:"slot"`
	Cold   bool   `json:"cold,omitempty"`
	// StartUnixMs is when the call was sent, after its wait for a slot.
	StartUnixMs int64 `json:"start_unix_ms"`
	WaitMs      int64 `json:"wait_ms"`
	WallMs      int64 `json:"wall_ms"`
	TTFTMs      int64 `json:"ttft_ms,omitempty"`
	PromptChars int   `json:"prompt_chars"`

	PromptEvalCount int   `json:"prompt_eval_count,omitempty"`
	EvalCount       int   `json:"eval_count,omitempty"`
	LoadMs          int64 `json:"load_ms,omitempty"`
	PrefillMs       int64 `json:"prefill_ms,omitempty"`
	DecodeMs        int64 `json:"decode_ms,omitempty"`
	ServerTotalMs   int64 `json:"server_total_ms,omitempty"`

	Error string `json:"error,omitempty"`
}

type runHeader struct {
	Type     string         `json:"type"`
	Label    string         `json:"label"`
	Policy   string         `json:"policy"`
	Quantum  int            `json:"quantum"`
	HoldMs   int            `json:"hold_ms"`
	Slots    map[string]int `json:"slots"`
	Cold     bool           `json:"cold"`
	ToolMs   int            `json:"tool_ms"`
	BaseURL  string         `json:"base_url"`
	Agents   int            `json:"agents"`
	Started  time.Time      `json:"started"`
	Finished *time.Time     `json:"finished,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "schedprobe:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("schedprobe", flag.ContinueOnError)
	gen := fs.Bool("gen", false, "write a generated workload to stdout instead of running one")
	var g genOptions
	fs.IntVar(&g.agents, "agents", 4, "gen: chat agents")
	fs.IntVar(&g.turns, "turns", 10, "gen: calls per agent")
	fs.IntVar(&g.systemChars, "system-chars", 16000, "gen: system prompt size per chat agent")
	fs.IntVar(&g.turnChars, "turn-chars", 1500, "gen: text each call adds")
	fs.StringVar(&g.model, "model", "", "gen: chat model")
	fs.StringVar(&g.numCtx, "num-ctx", "0", "gen: chat num_ctx; a comma list is dealt to agents in turn")
	fs.IntVar(&g.maxTokens, "max-tokens", 256, "gen: chat reply cap")
	fs.StringVar(&g.effort, "effort", "low", "gen: chat effort (low = thinking off)")
	fs.IntVar(&g.embedAgents, "embed-agents", 0, "gen: embed agents")
	fs.StringVar(&g.embedModel, "embed-model", "", "gen: embed model")
	fs.IntVar(&g.decisionAgents, "decision-agents", 0, "gen: decision agents")
	fs.StringVar(&g.decisionModel, "decision-model", "", "gen: decision model")
	fs.Int64Var(&g.seed, "seed", 1, "gen: workload seed")
	fs.StringVar(&g.prefix, "agent-prefix", "a", "gen: agent name prefix")

	baseURL := fs.String("base-url", envOr("OLLAMA_BASE_URL", "http://localhost:11434"), "Ollama base URL")
	workload := fs.String("workload", "", "workload jsonl")
	policy := fs.String("policy", "lease", "none | run | call | lease")
	quantum := fs.Int("quantum", 4, "lease: consecutive calls while others wait (0 = unlimited)")
	holdMs := fs.Int("hold-ms", 5000, "lease: slot reserved for its owner after a call (-1 = until done)")
	slotSpec := fs.String("slots", "chat=4,embed=2,decision=1", "slots per lane")
	toolMs := fs.Int("tool-ms", 500, "sleep between an agent's calls, standing in for tool time")
	cold := fs.Bool("cold", false, "prefix every chat call's system prompt with a fresh nonce: no cache can match")
	label := fs.String("label", "", "run label, recorded on every record")
	out := fs.String("out", "", "records jsonl (appended)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *gen {
		return writeWorkload(stdout, g)
	}
	if *workload == "" || *out == "" || *label == "" {
		return errors.New("-workload, -out and -label are required (or -gen)")
	}
	switch *policy {
	case "none", "run", "call", "lease":
	default:
		return fmt.Errorf("unknown -policy %q", *policy)
	}
	specs, err := readWorkload(*workload)
	if err != nil {
		return err
	}
	slots, err := parseSlots(*slotSpec)
	if err != nil {
		return err
	}
	hold := time.Duration(*holdMs) * time.Millisecond
	if *holdMs < 0 {
		hold = holdForever
	}
	lanes := map[string]*lane{}
	for kind, n := range slots {
		lanes[kind] = policyLane(*policy, n, *quantum, hold)
	}
	// Checked before any agent starts: a refusal once some are running would
	// return with their calls still writing to the output.
	for _, s := range specs {
		if _, ok := lanes[s.Kind]; !ok && *policy != "none" {
			return fmt.Errorf("agent %s: no slots for kind %q in -slots", s.Agent, s.Kind)
		}
	}

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	rec := &recorder{enc: json.NewEncoder(f)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	hdr := runHeader{Type: "run", Label: *label, Policy: *policy, Quantum: *quantum, HoldMs: *holdMs,
		Slots: slots, Cold: *cold, ToolMs: *toolMs, BaseURL: *baseURL, Agents: len(specs), Started: time.Now().UTC()}
	if err := rec.write(hdr); err != nil {
		return err
	}
	p := &prober{baseURL: *baseURL, label: *label, cold: *cold, tool: time.Duration(*toolMs) * time.Millisecond, rec: rec,
		chat: ollama.New("ollama-local", "", *baseURL, streamhttp.Options{HeaderTimeout: time.Hour, IdleTimeout: 10 * time.Minute}, nil)}
	var wg sync.WaitGroup
	errs := make(chan error, len(specs))
	for _, s := range specs {
		l := lanes[s.Kind]
		wg.Add(1)
		go func(s agentSpec, l *lane) {
			defer wg.Done()
			if err := p.runAgent(ctx, s, l); err != nil {
				errs <- fmt.Errorf("agent %s: %w", s.Agent, err)
			}
		}(s, l)
	}
	wg.Wait()
	close(errs)
	fin := time.Now().UTC()
	hdr.Finished = &fin
	if err := rec.write(hdr); err != nil {
		return err
	}
	var all []error
	for e := range errs {
		all = append(all, e)
	}
	return errors.Join(all...)
}

type recorder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (r *recorder) write(v any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enc.Encode(v)
}

type prober struct {
	baseURL string
	label   string
	cold    bool
	tool    time.Duration
	rec     *recorder
	chat    *ollama.Driver
}

// runAgent makes the agent's calls in order, each through its lane (nil = no gate).
// A failed call is recorded and the agent goes on: one fault must not end the run.
func (p *prober) runAgent(ctx context.Context, s agentSpec, l *lane) error {
	if l != nil {
		defer l.done(s.Agent)
	}
	var call func(ctx context.Context, turn int, r *callRecord) error
	switch s.Kind {
	case "chat":
		call = p.chatCall(s)
	case "embed":
		emb, err := ollama.NewEmbedder(providers.EmbedderOptions{BaseURL: p.baseURL, Model: s.Model, Timeout: time.Hour})
		if err != nil {
			return err
		}
		call = func(ctx context.Context, turn int, r *callRecord) error {
			r.PromptChars = len(s.Turns[turn])
			_, err := emb.Embed(ctx, []string{s.Turns[turn]})
			return err
		}
	case "decision":
		d, err := decision.New("ollama", decision.Options{ProviderID: "ollama-local", BaseURL: p.baseURL, Timeout: time.Hour, MaxConcurrent: 1 << 10})
		if err != nil {
			return err
		}
		call = func(ctx context.Context, turn int, r *callRecord) error {
			r.PromptChars = len(s.Turns[turn])
			resp, err := d.Decide(ctx, decisionRequest(s.Model, s.Turns[turn]))
			if err == nil {
				r.PromptEvalCount, r.EvalCount = resp.Usage.InputTokens, resp.Usage.OutputTokens
			}
			return err
		}
	default:
		return fmt.Errorf("unknown kind %q", s.Kind)
	}

	for turn := range s.Turns {
		if turn > 0 && p.tool > 0 {
			select {
			case <-time.After(p.tool):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r := callRecord{Type: "call", Label: p.label, Agent: s.Agent, Kind: s.Kind, Model: s.Model, NumCtx: s.NumCtx, Turn: turn, Slot: -1, Cold: p.cold && s.Kind == "chat"}
		waitStart := time.Now()
		if l != nil {
			i, err := l.acquire(ctx, s.Agent)
			if err != nil {
				return err
			}
			r.Slot = i
		}
		start := time.Now()
		r.WaitMs = start.Sub(waitStart).Milliseconds()
		r.StartUnixMs = start.UnixMilli()
		err := call(ctx, turn, &r)
		r.WallMs = time.Since(start).Milliseconds()
		if l != nil {
			l.release(s.Agent, r.Slot)
		}
		if err != nil {
			r.Error = err.Error()
		}
		if werr := p.rec.write(r); werr != nil {
			return werr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// chatCall returns the call for a chat agent. The conversation grows by each turn
// and the model's own reply, as in the agent loop, so a warm cache covers both.
func (p *prober) chatCall(s agentSpec) func(context.Context, int, *callRecord) error {
	var msgs []providers.Message
	temp, seed := 0.0, 1
	return func(ctx context.Context, turn int, r *callRecord) error {
		system := s.System
		if p.cold {
			system = fmt.Sprintf("[%d] %s", rand.Int63(), system)
		}
		msgs = append(msgs, textMessage("user", s.Turns[turn]+"\n\nSummarize the key facts above in three short bullet points."))
		r.PromptChars = len(system)
		for _, m := range msgs {
			for _, c := range m.Content {
				r.PromptChars += len(c.Text)
			}
		}
		req := providers.Request{
			Model: s.Model, System: []providers.ContentBlock{{Type: "text", Text: system}},
			Messages: msgs, MaxTokens: s.MaxTokens, Temperature: &temp, Seed: &seed,
			Effort: s.Effort, Stream: true, MaxContextTokens: s.NumCtx,
		}
		start := time.Now()
		ch, err := p.chat.Call(ctx, req)
		if err != nil {
			return err
		}
		var reply strings.Builder
		var callErr error
		for ev := range ch {
			switch ev.Type {
			case providers.EventText, providers.EventThinking, providers.EventToolCall:
				if r.TTFTMs == 0 {
					r.TTFTMs = time.Since(start).Milliseconds()
				}
				if ev.Type == providers.EventText {
					reply.WriteString(ev.Text)
				}
			case providers.EventDone:
				if u := ev.Usage; u != nil {
					r.PromptEvalCount, r.EvalCount = u.InputTokens, u.OutputTokens
					if t := u.Timing; t != nil {
						r.LoadMs, r.PrefillMs, r.DecodeMs, r.ServerTotalMs = t.LoadMs, t.PrefillMs, t.DecodeMs, t.ServerTotalMs
					}
				}
			case providers.EventError:
				callErr = errors.New(ev.Error)
			}
		}
		text := reply.String()
		if text == "" {
			text = "(no reply)"
		}
		msgs = append(msgs, textMessage("assistant", text))
		return callErr
	}
}

func textMessage(role, text string) providers.Message {
	return providers.Message{Role: role, Content: []providers.ContentBlock{{Type: "text", Text: text}}}
}

func decisionRequest(model, text string) decision.Request {
	return decision.Request{
		Model: model,
		State: map[string]any{"text": text},
		Questions: map[string]decision.Question{
			"topic": {
				Type:         decision.TypeChoice,
				Instructions: "Which topic does the text mostly discuss?",
				Criteria:     json.RawMessage(`{"infrastructure":null,"finance":null,"science":null,"other":null}`),
			},
		},
	}
}

func readWorkload(path string) ([]agentSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var specs []agentSpec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s agentSpec
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if s.Agent == "" || s.Model == "" || len(s.Turns) == 0 {
			return nil, fmt.Errorf("%s:%d: agent, model and turns are required", path, n)
		}
		specs = append(specs, s)
	}
	return specs, sc.Err()
}

// parseSlots reads "chat=4,embed=2".
func parseSlots(spec string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil || n < 1 {
			return nil, fmt.Errorf("-slots: %q is not kind=N with N >= 1", part)
		}
		out[k] = n
	}
	return out, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
