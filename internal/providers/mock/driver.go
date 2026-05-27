// Package mock implements a synthetic LLM provider for cost-free
// stress testing of the agent runtime. It emits scripted tool_use
// sequences shaped to drive the canonical circuit-stress 3-agent
// pipeline (researcher → editor → evaluator) without burning real
// provider quota.
//
// Operators opt in via LOOMCYCLE_MOCK_ENABLED=1; the driver registers
// as provider id "mock" alongside the real providers. Per-agent yaml
// pins which model variant to use:
//
//	mock-researcher  — 2-step FSM (Memory.set → Channel.publish)
//	mock-editor      — 5-step FSM (Channel.subscribe → Memory.get →
//	                    Memory.set → Context.self → Channel.publish)
//	mock-evaluator   — 5-step FSM (Channel.subscribe → Memory.get(×2)
//	                    → Memory.set → Evaluation.submit)
//	mock-generic     — no-op text response (defensive default for
//	                    mis-pinned configs)
//
// Failure injection knobs (env vars, read once at New() time):
//
//	LOOMCYCLE_MOCK_429_RATE         — fraction [0.0, 1.0] of calls to
//	                                  reject with a 429 (drives the
//	                                  MarkRateLimited matrix cooldown
//	                                  PR #241 path).
//	LOOMCYCLE_MOCK_500_RATE         — same shape, 5xx-error path.
//	LOOMCYCLE_MOCK_LATENCY_MS       — base sleep per Call() (default 50).
//	LOOMCYCLE_MOCK_LATENCY_JITTER_MS — uniform [0, jitter] random add
//	                                   to the base (default 25).
//
// The driver intentionally does NOT wrap in ratelimit.Do — we want
// the loop to see the injected 429s + 500s so the resolver matrix
// and runtime-fallback paths get exercised by the same load test.
//
// No real HTTP, no real tokens. Usage fields are derived from
// request character count so the metrics endpoints have something
// non-zero to plot under load.
package mock

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Driver is the synthetic provider. Constructed once via New(), then
// shared across all runs that resolve to the "mock" provider. The
// rng is mu-protected because Call() is invoked concurrently from
// many goroutines under load.
type Driver struct {
	latencyBase   time.Duration
	latencyJitter time.Duration
	rate429       float64
	rate500       float64

	mu  sync.Mutex
	rng *rand.Rand
}

// New constructs the driver from LOOMCYCLE_MOCK_* env vars. Invalid
// values fall back to sensible defaults rather than refusing to
// start — this is a test tool, not a production hot path; the
// operator can always check the boot log to see what was applied.
func New() *Driver {
	d := &Driver{
		latencyBase:   parseDurationMS(os.Getenv("LOOMCYCLE_MOCK_LATENCY_MS"), 50*time.Millisecond),
		latencyJitter: parseDurationMS(os.Getenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS"), 25*time.Millisecond),
		rate429:       parseRate(os.Getenv("LOOMCYCLE_MOCK_429_RATE"), 0),
		rate500:       parseRate(os.Getenv("LOOMCYCLE_MOCK_500_RATE"), 0),
		// Seed off wall time so re-runs against the same harness
		// produce different failure-injection patterns; tests that
		// need determinism use the WithRNG variant below.
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	return d
}

// NewWithRNG is the test seam — lets unit tests inject a deterministic
// rand source so the 429/500/score paths are reproducible. The
// runtime constructor (New) seeds from wall time.
func NewWithRNG(rng *rand.Rand) *Driver {
	d := New()
	d.rng = rng
	return d
}

func (d *Driver) ID() string { return "mock" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		Streaming:         false,
		ParallelToolCalls: false,
		// MaxContextTokens left at 0 — the loop treats 0 as "no cap"
		// for capability-driven decisions; the mock has no real
		// context window so this is honest.
		MaxContextTokens:  0,
		NativePromptCache: false,
		SupportsThinking:  false,
		SupportsEffort:    false,
	}
}

func (d *Driver) Probe(_ context.Context) error { return nil }

func (d *Driver) ListModels(_ context.Context) ([]string, error) {
	return []string{
		"mock-researcher",
		"mock-editor",
		"mock-evaluator",
		"mock-generic",
	}, nil
}

// Call dispatches on req.Model. Each model has a deterministic
// finite-state machine driven by the number of tool_result blocks
// already in req.Messages: zero results = emit the first tool,
// one result = emit the second, and so on, until the agent has run
// through its scripted sequence and stop_reason flips to "end_turn".
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	// 1. Failure injection — checked BEFORE the latency sleep so
	// rate-limit cascades surface promptly under load. Random draw
	// is mu-protected (rng is not safe for concurrent use).
	if err := d.rollFailure(); err != nil {
		return nil, err
	}

	// 2. Realistic latency. ctx-aware so a cancelled run doesn't
	// keep the goroutine parked the full window.
	if err := d.sleepWithLatency(ctx, req); err != nil {
		return nil, err
	}

	// 3. Dispatch on model.
	step := countToolResults(req.Messages)
	circuitID := extractCircuitID(req)

	var events []providers.Event
	switch req.Model {
	case "mock-researcher":
		events = scriptResearcher(step, circuitID)
	case "mock-editor":
		events = scriptEditor(step, circuitID, req.Messages)
	case "mock-evaluator":
		events = scriptEvaluator(step, circuitID, req.Messages)
	default:
		// mock-generic + anything unpinned. Single text turn, done.
		// Usage is attached to EventDone below by finalizeUsage —
		// the loop reads it from there.
		events = []providers.Event{
			{Type: providers.EventText, Text: "ok"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}

	// Attach Usage to the terminal EventDone frame so the loop's
	// iterUsage = ev.Usage merge captures non-zero tokens + Model.
	// See internal/loop/loop.go case providers.EventDone — Usage on
	// a separate EventUsage frame would be ignored by the switch.
	events = finalizeUsage(events, req)

	ch := make(chan providers.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// ---- Failure injection ----

func (d *Driver) rollFailure() error {
	d.mu.Lock()
	roll429 := d.rng.Float64()
	roll500 := d.rng.Float64()
	d.mu.Unlock()
	if roll429 < d.rate429 {
		// Format must match errclass.statusRe ("^[a-z][a-z0-9_-]* (\d{3}):")
		// so providers.IsRateLimit returns true for this error.
		return fmt.Errorf("mock 429: rate_limited (injected at %s)", time.Now().Format(time.RFC3339Nano))
	}
	if roll500 < d.rate500 {
		return fmt.Errorf("mock 500: server_error (injected at %s)", time.Now().Format(time.RFC3339Nano))
	}
	return nil
}

func (d *Driver) sleepWithLatency(ctx context.Context, req providers.Request) error {
	if d.latencyBase <= 0 && d.latencyJitter <= 0 {
		return nil
	}
	// Latency proportional to request size so longer conversations
	// model a real provider's "more tokens to process" delay. ~1ms
	// per 100 chars; matches a real provider's order-of-magnitude
	// behaviour without burning real per-token time.
	d.mu.Lock()
	jitter := time.Duration(0)
	if d.latencyJitter > 0 {
		jitter = time.Duration(d.rng.Int63n(int64(d.latencyJitter)))
	}
	d.mu.Unlock()
	chars := requestChars(req)
	body := time.Duration(chars/100) * time.Microsecond
	total := d.latencyBase + body + jitter
	if total <= 0 {
		return nil
	}
	t := time.NewTimer(total)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- FSM scripts ----

// scriptResearcher emits the researcher's 2-tool sequence. The
// canned answer is intentionally short (~12 chars) so Usage fields
// stay bounded under x10K and so the editor's downstream
// "tightening" has something to compare against.
func scriptResearcher(step int, circuitID string) []providers.Event {
	switch step {
	case 0:
		key := fmt.Sprintf("%s-research", circuitID)
		return []providers.Event{
			textEvent("storing research"),
			toolEvent("Memory", map[string]any{
				"op":    "set",
				"scope": "user",
				"key":   key,
				"value": map[string]any{"text": cannedResearch(circuitID)},
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 1:
		channel := fmt.Sprintf("research-done/%s", circuitID)
		return []providers.Event{
			textEvent("publishing done signal"),
			toolEvent("Channel", map[string]any{
				"op":      "publish",
				"channel": channel,
				"value":   map[string]any{"circuit_id": circuitID},
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	default:
		return []providers.Event{
			textEvent("done"),
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}
}

// scriptEditor emits the editor's 5-tool sequence (cases 0–4,
// indices matching the count of prior tool_result blocks). Case 3
// (Context.self) is fired BEFORE case 4 (Channel.publish) so the
// publish payload can carry editor_run_id extracted from the
// Context.self tool_result.
func scriptEditor(step int, circuitID string, messages []providers.Message) []providers.Event {
	switch step {
	case 0:
		return []providers.Event{
			textEvent("subscribing to research-done"),
			toolEvent("Channel", map[string]any{
				"op":           "subscribe",
				"channel":      fmt.Sprintf("research-done/%s", circuitID),
				"wait_ms":      120000,
				"max_messages": 1,
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 1:
		return []providers.Event{
			textEvent("reading research"),
			toolEvent("Memory", map[string]any{
				"op":    "get",
				"scope": "user",
				"key":   fmt.Sprintf("%s-research", circuitID),
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 2:
		return []providers.Event{
			textEvent("storing edited version"),
			toolEvent("Memory", map[string]any{
				"op":    "set",
				"scope": "user",
				"key":   fmt.Sprintf("%s-research-edited", circuitID),
				"value": map[string]any{"text": cannedEdit(circuitID)},
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 3:
		return []providers.Event{
			textEvent("fetching self identity"),
			toolEvent("Context", map[string]any{"op": "self"}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 4:
		// Extract the editor's run_id from the prior Context.self
		// tool_result. Fall back to a deterministic synthetic id
		// derived from circuit_id when the parse fails (defensive —
		// the next tool_result in the harness still carries enough
		// for the evaluator to retry on its own).
		runID := extractRunID(messages)
		if runID == "" {
			runID = "mock-editor-" + circuitID
		}
		return []providers.Event{
			textEvent("publishing editing-done"),
			toolEvent("Channel", map[string]any{
				"op":      "publish",
				"channel": fmt.Sprintf("editing-done/%s", circuitID),
				"value": map[string]any{
					"circuit_id":    circuitID,
					"editor_run_id": runID,
				},
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	default:
		return []providers.Event{
			textEvent("done"),
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}
}

// scriptEvaluator emits the evaluator's 5-tool sequence (cases 0–4,
// indices matching the count of prior tool_result blocks). Case 4
// (Evaluation.submit) requires the editor_run_id extracted from the
// channel message body delivered in case 0's Channel.subscribe
// tool_result.
func scriptEvaluator(step int, circuitID string, messages []providers.Message) []providers.Event {
	switch step {
	case 0:
		return []providers.Event{
			textEvent("subscribing to editing-done"),
			toolEvent("Channel", map[string]any{
				"op":           "subscribe",
				"channel":      fmt.Sprintf("editing-done/%s", circuitID),
				"wait_ms":      120000,
				"max_messages": 1,
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 1:
		return []providers.Event{
			textEvent("reading research"),
			toolEvent("Memory", map[string]any{
				"op":    "get",
				"scope": "user",
				"key":   fmt.Sprintf("%s-research", circuitID),
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 2:
		return []providers.Event{
			textEvent("reading edited version"),
			toolEvent("Memory", map[string]any{
				"op":    "get",
				"scope": "user",
				"key":   fmt.Sprintf("%s-research-edited", circuitID),
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 3:
		editorRunID := extractEditorRunID(messages)
		score := deriveScore(editorRunID + circuitID)
		return []providers.Event{
			textEvent("storing score"),
			toolEvent("Memory", map[string]any{
				"op":    "set",
				"scope": "user",
				"key":   fmt.Sprintf("%s-research-scored", circuitID),
				"value": map[string]any{
					"score":     score,
					"rationale": "ok",
				},
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 4:
		editorRunID := extractEditorRunID(messages)
		score := deriveScore(editorRunID + circuitID)
		return []providers.Event{
			textEvent("submitting evaluation"),
			toolEvent("Evaluation", map[string]any{
				"op":        "submit",
				"run_id":    editorRunID,
				"score":     score,
				"rationale": "ok",
			}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	default:
		return []providers.Event{
			textEvent("done"),
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}
}

// ---- Helpers ----

// countToolResults walks req.Messages and counts the tool_result
// content blocks across all user-role messages. Each successful
// tool execution by the loop appends ONE user message containing
// the tool_result(s) for the prior iteration's tool_use(s), so
// this count is the FSM's step index (0 on the first call).
func countToolResults(messages []providers.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

// circuitRe matches the c{N} circuit id the driver embeds in the
// initial user message ("Your circuit_id is c5. Question: ...").
var circuitRe = regexp.MustCompile(`\bc\d+\b`)

// extractCircuitID scans req.Messages for the c{N} pattern from the
// initial user prompt. Returns "c0" if none found — the mock's
// memory keys + channel names will be "c0-…" in that case, which
// is still a valid string but doesn't match any harness-allocated
// circuit. Operators see this in the resulting Memory rows.
func extractCircuitID(req providers.Request) string {
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && b.Text != "" {
				if hit := circuitRe.FindString(b.Text); hit != "" {
					return hit
				}
			}
		}
	}
	return "c0"
}

// extractRunID parses the most recent Context.self tool_result for
// the run_id field. Returns empty string when no Context.self
// tool_result has been seen yet.
func extractRunID(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			if id := scanJSONForKey(b.Text, "run_id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// extractEditorRunID scans tool_results for `editor_run_id` (the
// channel message body field). Looks at the Channel.subscribe
// tool_result delivered in step 1 of the evaluator's FSM.
func extractEditorRunID(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			if id := scanJSONForKey(b.Text, "editor_run_id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// scanJSONForKey extracts a top-level (or nested-once) string-valued
// field from a JSON blob. Used to pull run_id / editor_run_id out of
// tool_result text without strictly typing every possible shape.
//
// Two strategies: (1) full JSON decode into map[string]any and look
// for the key at the top level and in any nested {messages: [...]}
// array (matches the Channel.subscribe response shape), (2) regex
// fallback for cases where the JSON is wrapped or partial.
func scanJSONForKey(blob, key string) string {
	if blob == "" {
		return ""
	}
	// Strategy 1: structured decode.
	var top map[string]any
	if err := json.Unmarshal([]byte(blob), &top); err == nil {
		if v, ok := top[key].(string); ok && v != "" {
			return v
		}
		// Channel.subscribe nests payloads under messages[].value.
		if msgs, ok := top["messages"].([]any); ok {
			for _, raw := range msgs {
				msg, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if v, ok := msg[key].(string); ok && v != "" {
					return v
				}
				if val, ok := msg["value"]; ok {
					switch vv := val.(type) {
					case map[string]any:
						if s, ok := vv[key].(string); ok && s != "" {
							return s
						}
					case string:
						// value may itself be a JSON-encoded string.
						var inner map[string]any
						if err := json.Unmarshal([]byte(vv), &inner); err == nil {
							if s, ok := inner[key].(string); ok && s != "" {
								return s
							}
						}
					}
				}
			}
		}
	}
	// Strategy 2: regex fallback. `"key":"value"` shape, allowing
	// whitespace, handles cases where the JSON is wrapped or the
	// value lives inside a stringified inner object.
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"([^"]+)"`)
	if m := re.FindStringSubmatch(blob); len(m) == 2 {
		return m[1]
	}
	return ""
}

// deriveScore hashes the input to a [0.50, 0.99] float so the same
// run_id always produces the same score (deterministic for tests)
// but the score distribution across many run_ids spans the band
// fairly uniformly (50 buckets, hash spreads them out).
func deriveScore(seed string) float64 {
	if seed == "" {
		return 0.85 // defensive fixed score when run_id missing
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	bucket := int(h.Sum32() % 50)
	return 0.50 + float64(bucket)/100.0
}

// cannedResearch + cannedEdit produce short payloads so token
// counts stay bounded under x10K. Content includes the circuit_id
// so visual inspection of Memory rows after a run is sensible.
func cannedResearch(circuitID string) string {
	return fmt.Sprintf("Mock research for %s: a brief factual answer.", circuitID)
}

func cannedEdit(circuitID string) string {
	return fmt.Sprintf("Mock edit %s: tightened.", circuitID)
}

// textEvent constructs an EventText frame. Used to emit a short
// "narration" line before each tool_use so the SSE stream isn't
// just tool calls — matches real-provider shape where models emit
// a sentence of reasoning before invoking a tool.
func textEvent(s string) providers.Event {
	return providers.Event{Type: providers.EventText, Text: s}
}

// toolEvent constructs an EventToolCall frame with a generated
// tool_use id. The id format mirrors the anthropic shape
// ("toolu_<hex>") so SSE consumers that parse ids don't choke; it
// has no semantic meaning to the mock.
func toolEvent(name string, input map[string]any) providers.Event {
	raw, _ := json.Marshal(input)
	id := mintToolUseID()
	return providers.Event{
		Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{
			ID:    id,
			Name:  name,
			Input: raw,
		},
	}
}

// finalizeUsage attaches a Usage struct to the terminal EventDone
// frame. The loop reads iterUsage from EventDone.Usage (see
// internal/loop/loop.go case providers.EventDone), so a separate
// EventUsage frame in the stream would be ignored. Model is set to
// req.Model unconditionally so the loop's totalUsage merge gets a
// non-empty Model field regardless of which mock variant ran.
//
// Idempotent — if the script already populated EventDone.Usage,
// only the Model field is filled in when missing; token counts
// stay as the script set them.
func finalizeUsage(events []providers.Event, req providers.Request) []providers.Event {
	inputTokens := estimateTokens(req)
	outputTokens := 0
	for _, e := range events {
		if e.Type == providers.EventText {
			outputTokens += len(e.Text) / 4
		}
		if e.Type == providers.EventToolCall && e.ToolUse != nil {
			outputTokens += len(e.ToolUse.Input) / 4
		}
	}
	for i := range events {
		if events[i].Type != providers.EventDone {
			continue
		}
		if events[i].Usage == nil {
			events[i].Usage = &providers.Usage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
				Model:        req.Model,
			}
		} else if events[i].Usage.Model == "" {
			events[i].Usage.Model = req.Model
		}
		return events
	}
	return events
}

// requestChars sums the bytes of every text + tool_result block
// across messages. Drives both the latency body and the input-
// token estimate.
func requestChars(req providers.Request) int {
	n := 0
	for _, sys := range req.System {
		n += len(sys.Text)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			n += len(b.Text)
			n += len(b.ToolInput)
		}
		n += len(m.Reasoning)
	}
	return n
}

// estimateTokens approximates input tokens as chars/4 — the
// industry rule-of-thumb that matches GPT-style BPE. Floor at 1
// so empty conversations don't report zero (which would skew
// downstream throughput-per-token metrics).
func estimateTokens(req providers.Request) int {
	n := requestChars(req) / 4
	if n < 1 {
		return 1
	}
	return n
}

// parseRate parses a [0.0, 1.0] env-var value. Out-of-range and
// malformed values fall back to the default — the mock is a test
// tool, not a production hot path; a typo shouldn't refuse start.
func parseRate(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 || v > 1 {
		return def
	}
	return v
}

// parseDurationMS parses a millisecond-valued env-var. Negative
// values reset to zero (no sleep), malformed values fall back to
// the default.
func parseDurationMS(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if n < 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// mintToolUseID returns a stable-shape tool_use id ("mock_tu_<16hex>").
// Mirrors the anthropic shape's distinctive prefix so SSE consumers
// that filter on id prefix can identify mock-origin tool calls.
//
// Uses crypto/rand rather than the math/rand source: the loop
// correlates tool_use_id → tool_result by the exact string it
// received, so collision resistance matters far more than
// reproducibility. Going through d.rng would also bypass the
// driver's NewWithRNG test seam since this function has no Driver
// receiver. crypto/rand removes both concerns at once.
func mintToolUseID() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// Test tool only — operator sees the panic in the boot log
		// and re-runs. Production drivers would degrade gracefully.
		panic("mock: mintToolUseID: " + err.Error())
	}
	return fmt.Sprintf("mock_tu_%016x", b)
}

// ErrUnsupportedModel is the typed error returned (today: never;
// the default case in Call() handles unpinned models via the
// mock-generic branch). Reserved for future tightening when we
// want resolver-side rejection of unknown mock-* model names.
var ErrUnsupportedModel = errors.New("mock: unsupported model")
