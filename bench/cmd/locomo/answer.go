package main

// answer.go — the ANSWER axis (phase 3).
//
// Phase 2 measured retrieval: does the supporting turn come back. This measures
// the whole memory pipeline: conversation turns go in through the memory LAYER
// (`Memory op=add`), the consolidator distils them into facts, an agent answers
// each question from recall alone, and a second model grades the answer against
// the gold one.
//
// Two things make this measure memory rather than something else:
//
//   - Ingest is DETERMINISTIC. The turns are handed to `Memory op=add` verbatim
//     over MCP. The obvious alternative — prompting an agent to store the turns —
//     would have measured that agent's transcription fidelity, and a benchmark
//     whose ingest step can lose content cannot attribute a miss to memory.
//   - The answerer has the Memory tool and NOTHING else, with a system prompt
//     that forbids general knowledge and requires NOT_FOUND when recall comes up
//     empty. A chat agent with web search would have answered some of LoCoMo's
//     open-domain questions without consulting memory at all.
//
// ONE CONVERSATION AT A TIME. `scope_id` is server-derived from the run
// identity, never caller-supplied, so an off-run `add` lands under the token's
// own subject — there is no way to give each conversation its own partition from
// outside a run. Conversations are therefore run in sequence with the memory
// layer purged between them, which is why -conversations defaults to 1 here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdict is the judge's grade.
type Verdict string

const (
	VerdictCorrect Verdict = "correct"
	VerdictPartial Verdict = "partial"
	VerdictWrong   Verdict = "wrong"
	// VerdictUnparsed means the judge did not return a readable verdict. It is
	// counted and EXCLUDED from accuracy rather than folded into "wrong":
	// scoring a judge malfunction as a memory miss would quietly understate the
	// system under test.
	VerdictUnparsed Verdict = "unparsed"
)

// Score maps a verdict to the LoCoMo convention: correct 1, partial 0.5,
// wrong 0.
func (v Verdict) Score() float64 {
	switch v {
	case VerdictCorrect:
		return 1
	case VerdictPartial:
		return 0.5
	default:
		return 0
	}
}

// notFound is the abstention token the answerer's prompt mandates.
const notFound = "NOT_FOUND"

var (
	// jsonObjectRe finds the first {...} block, so a judge that wraps its JSON
	// in prose is still readable.
	jsonObjectRe = regexp.MustCompile(`(?s)\{.*?\}`)
	// queuedRe reads the consolidator's own report of what is left to drain.
	queuedRe = regexp.MustCompile(`queued items (\d+), acked (\d+)`)
	factsRe  = regexp.MustCompile(`facts written (\d+)`)
	// busyRe matches the pass's refusal when another consolidator holds the
	// target's lease. The bundle leases for 30 minutes, so a consolidator killed
	// mid-pass blocks the target for that long — and every later pass returns
	// this instead of a queue depth.
	busyRe = regexp.MustCompile(`(?i)target busy`)
)

// ErrConsolidatorBusy reports that the target's consolidation lease is held.
// Surfaced as its own error because the alternative is grading against a store
// that was never consolidated, which scores the memory system at zero for a
// reason that has nothing to do with memory.
type ErrConsolidatorBusy struct{ Report string }

func (e *ErrConsolidatorBusy) Error() string {
	return "consolidation target is leased by another consolidator (the bundle leases for 30 minutes, " +
		"so a pass killed mid-flight blocks the target until it expires): " + truncate(e.Report, 160)
}

// ParseVerdict reads the judge's reply.
func ParseVerdict(reply string) (Verdict, string) {
	for _, candidate := range jsonObjectRe.FindAllString(reply, 4) {
		var out struct {
			Verdict string `json:"verdict"`
			Why     string `json:"why"`
		}
		if err := json.Unmarshal([]byte(candidate), &out); err != nil {
			continue
		}
		switch Verdict(strings.ToLower(strings.TrimSpace(out.Verdict))) {
		case VerdictCorrect:
			return VerdictCorrect, out.Why
		case VerdictPartial:
			return VerdictPartial, out.Why
		case VerdictWrong:
			return VerdictWrong, out.Why
		}
	}
	return VerdictUnparsed, truncate(strings.TrimSpace(reply), 160)
}

// LayerMessages renders one session's turns as memory-layer messages.
//
// LoCoMo has exactly two speakers, so the first one seen becomes "user" and the
// other "assistant". The speaker NAME stays in the content regardless: role
// alone loses identity, and several questions turn on who said a thing. The
// session timestamp is prefixed for the same reason it is on the retrieval
// rows — LoCoMo dates live on the session, not the turn.
func (c Conversation) LayerMessages() [][]LayerMessage {
	roles := map[string]string{}
	bySession := map[int][]LayerMessage{}
	var order []int
	for _, t := range c.Turns {
		role, ok := roles[t.Speaker]
		if !ok {
			role = "user"
			if len(roles) > 0 {
				role = "assistant"
			}
			roles[t.Speaker] = role
		}
		if _, seen := bySession[t.Session]; !seen {
			order = append(order, t.Session)
		}
		bySession[t.Session] = append(bySession[t.Session], LayerMessage{Role: role, Content: t.Body()})
	}
	sort.Ints(order)
	out := make([][]LayerMessage, 0, len(order))
	for _, s := range order {
		out = append(out, bySession[s])
	}
	return out
}

// SampleQueries picks n queries, stratified across categories and taken at an
// even stride within each so the sample is deterministic — a benchmark whose
// sample moves between runs cannot show a regression.
func SampleQueries(qs []Query, n int) []Query {
	if n <= 0 || n >= len(qs) {
		return qs
	}
	byCat := map[int][]Query{}
	for _, q := range qs {
		byCat[q.Category] = append(byCat[q.Category], q)
	}
	cats := make([]int, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Ints(cats)

	var out []Query
	for _, c := range cats {
		bucket := byCat[c]
		// Proportional share, at least one per represented category.
		want := int(float64(n) * float64(len(bucket)) / float64(len(qs)))
		if want < 1 {
			want = 1
		}
		if want > len(bucket) {
			want = len(bucket)
		}
		stride := len(bucket) / want
		if stride < 1 {
			stride = 1
		}
		for i := 0; i < len(bucket) && len(out) < n+len(cats); i += stride {
			out = append(out, bucket[i])
			if countCat(out, c) >= want {
				break
			}
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func countCat(qs []Query, c int) int {
	n := 0
	for _, q := range qs {
		if q.Category == c {
			n++
		}
	}
	return n
}

// AnswerResult is one graded question.
type AnswerResult struct {
	Question  string  `json:"question"`
	Category  int     `json:"category"`
	Gold      string  `json:"gold"`
	Answer    string  `json:"answer"`
	Verdict   Verdict `json:"verdict"`
	Why       string  `json:"why,omitempty"`
	NotFound  bool    `json:"not_found"`
	Model     string  `json:"model,omitempty"`
	LatencyMs float64 `json:"latency_ms"`
	// WindowUsed records that a resolved `when` window was supplied with this
	// question, so a run splits into the questions the predicate could act on and
	// the ones it could not.
	WindowUsed bool `json:"window_used,omitempty"`
}

// AnswerStats aggregates graded answers.
type AnswerStats struct {
	Label string `json:"label"`
	// Graded excludes unparsed verdicts; Queries counts everything attempted.
	Queries      int     `json:"queries"`
	Graded       int     `json:"graded"`
	Accuracy     float64 `json:"accuracy"`
	Correct      int     `json:"correct"`
	Partial      int     `json:"partial"`
	Wrong        int     `json:"wrong"`
	Unparsed     int     `json:"unparsed"`
	NotFoundRate float64 `json:"not_found_rate"`
	LatencyP50Ms float64 `json:"latency_p50_ms"`
}

// AggregateAnswers computes the accuracy metrics.
func AggregateAnswers(label string, rs []AnswerResult) AnswerStats {
	st := AnswerStats{Label: label, Queries: len(rs)}
	if len(rs) == 0 {
		return st
	}
	var sum float64
	var nf int
	lat := make([]time.Duration, 0, len(rs))
	for _, r := range rs {
		switch r.Verdict {
		case VerdictCorrect:
			st.Correct++
		case VerdictPartial:
			st.Partial++
		case VerdictWrong:
			st.Wrong++
		default:
			st.Unparsed++
		}
		if r.Verdict != VerdictUnparsed {
			st.Graded++
			sum += r.Verdict.Score()
		}
		if r.NotFound {
			nf++
		}
		lat = append(lat, time.Duration(r.LatencyMs*float64(time.Millisecond)))
	}
	if st.Graded > 0 {
		st.Accuracy = sum / float64(st.Graded)
	}
	st.NotFoundRate = float64(nf) / float64(len(rs))
	st.LatencyP50Ms = percentileMs(lat, 0.50)
	return st
}

// AnswersByCategory buckets per LoCoMo category, category id ascending.
func AnswersByCategory(rs []AnswerResult) []AnswerStats {
	buckets := map[int][]AnswerResult{}
	for _, r := range rs {
		buckets[r.Category] = append(buckets[r.Category], r)
	}
	cats := make([]int, 0, len(buckets))
	for c := range buckets {
		cats = append(cats, c)
	}
	sort.Ints(cats)
	out := make([]AnswerStats, 0, len(cats))
	for _, c := range cats {
		out = append(out, AggregateAnswers(CategoryName(c), buckets[c]))
	}
	return out
}

// factsDiverted reports how many written facts are reachable in the partition the
// answerer recalls from, and whether the shortfall is large enough to invalidate a
// grading run.
//
// Exported from the guard rather than inlined so the THRESHOLD is pinned by a test
// against the real numbers from both a contaminated run and a healthy one — a
// guard whose arithmetic only exists inside the function it protects is a guard
// nobody can check.
//
// `doc.chunk:` rows are chunk BODIES, not recallable facts. Counting them is
// precisely what made a diverted partition look populated: 16 rows present, ~7 of
// them actual facts, and the empty-store check waved it through.
func factsDiverted(factsWritten int, keys []string) (reachable int, diverted bool) {
	for _, k := range keys {
		if strings.HasPrefix(k, "memory/") {
			reachable++
		}
	}
	// Half is the tolerance: supersede/retire legitimately removes a few, but a
	// diversion loses most or all of them (76 written, 7 reachable on the run this
	// exists for).
	return reachable, factsWritten > 0 && reachable*2 < factsWritten
}

// purgeLayer empties the memory-layer partition so the next conversation starts
// from nothing. Keys are read back from the store rather than derived, because
// the consolidator names the rows it writes (memory/fact/...), not this harness.
func purgeLayer(ctx context.Context, rest *Client, scope, scopeID string, stdout io.Writer) (int, error) {
	deleted := 0
	for pass := 0; pass < 50; pass++ {
		keys, err := rest.ListKeys(ctx, scope, scopeID, 500)
		if err != nil {
			return deleted, err
		}
		if len(keys) == 0 {
			break
		}
		for _, k := range keys {
			if err := rest.DeleteEntry(ctx, scope, scopeID, k); err != nil {
				return deleted, fmt.Errorf("purge %s: %w", k, err)
			}
			deleted++
		}
	}
	fmt.Fprintf(stdout, "  purged %d rows from %s/%s\n", deleted, scope, scopeID)
	return deleted, nil
}

// seedTurnRows writes one embedded row per turn into the partition the ANSWERER
// reads, keyed by dia_id. It is the answer-axis mirror of what -mode=ingest does
// for the retrieval axis, and it uses the same Turn.Body() — which prefixes the
// SESSION timestamp, without which the temporal category is unretrievable no
// matter how good the embedder is (LoCoMo dates live on the session, not the turn).
//
// Off-run through the REST client on purpose: only an off-run caller may name an
// explicit scope_id, which is exactly the asymmetry that makes this necessary.
func seedTurnRows(ctx context.Context, rest *Client, userID string, conv Conversation, opts options, stdout io.Writer) (int, error) {
	jobs := make(chan Turn)
	var (
		mu     sync.Mutex
		wrote  int
		firstE error
	)
	workers := opts.concurrency
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				// Dated when asked for, so the RFC CL `when` predicate has something to
				// filter on. Undated otherwise, which is what every prior run measured.
				obs := ""
				if opts.dated {
					obs = t.ObservedAt()
				}
				if _, err := rest.PutEntryAt(ctx, "user", userID, t.DiaID, t.Body(), !opts.noEmbed, obs); err != nil {
					mu.Lock()
					if firstE == nil {
						firstE = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				wrote++
				mu.Unlock()
			}
		}()
	}
	for _, t := range conv.Turns {
		if err := ctx.Err(); err != nil {
			break
		}
		mu.Lock()
		stop := firstE != nil
		mu.Unlock()
		if stop {
			break
		}
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	if firstE != nil {
		return wrote, firstE
	}
	fmt.Fprintf(stdout, "  seeded %d turn row(s) into user/%s (embedded, keyed by dia_id)\n", wrote, userID)
	return wrote, nil
}

// ingestLayer adds every session of one conversation to the consolidation queue.
func ingestLayer(ctx context.Context, mc *MCPClient, conv Conversation, stdout io.Writer) (int, error) {
	sessions := conv.LayerMessages()
	for i, msgs := range sessions {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if _, err := mc.MemoryAdd(ctx, "user", msgs); err != nil {
			return i, fmt.Errorf("memory add (session %d/%d): %w", i+1, len(sessions), err)
		}
	}
	fmt.Fprintf(stdout, "  queued %d sessions (%d turns) for consolidation\n", len(sessions), len(conv.Turns))
	return len(sessions), nil
}

// consolidateDrain runs consolidation passes until the queue is empty.
//
// The pass reports what it did in prose, including "queued items N, acked M",
// and that report is the only signal for when to stop — so it is parsed. A pass
// whose report cannot be read stops the loop rather than spinning: an unbounded
// loop against a changed message would burn extractor spend indefinitely.
// flushPendingQueue discards whatever is left in the consolidation queue and
// returns how many items it dropped. No extractor calls, no facts written.
//
// WHY DISCARD RATHER THAN CONSOLIDATE. These items belong to a previous
// conversation or an interrupted run. This harness purges the partition and
// re-ingests from the dataset file, so the same turns arrive again — extracting
// from the leftovers produces facts that are either duplicates or, worse, another
// conversation's, in the partition about to be measured.
//
// WHY IT HOLDS THE LEASE. pending_ack is lease-gated on the server
// (requireConsolidationLease), so a flush has to hold the target exactly as a
// pass would or every ack is refused as a non-owner. The TTL is deliberately
// SHORT: this is a handful of round trips, and if the process dies mid-flush a
// short lease means the target frees itself in a minute rather than blocking
// every pass for the bundle's half-hour default.
func flushPendingQueue(ctx context.Context, mc *MCPClient, stdout io.Writer) (int, error) {
	const leaseTTLMs = 60_000
	lease, err := memoryOp(ctx, mc, map[string]any{
		"op": "cursor_lease", "scope": "user", "lease_ttl_ms": leaseTTLMs,
	})
	if err != nil {
		return 0, fmt.Errorf("lease the target: %w", err)
	}
	if acquired, _ := lease["acquired"].(bool); !acquired {
		// Someone else owns the target. Refuse rather than flush around them:
		// their pass is reading the very queue this would empty.
		return 0, &ErrConsolidatorBusy{Report: fmt.Sprintf(
			"cannot flush: the target is leased by %v until %v",
			lease["leased_by"], lease["lease_expires_at"])}
	}
	// Release on every path, including the error paths below — a flush that
	// leaves the lease held is worse than one that does nothing.
	defer func() {
		if _, rerr := memoryOp(ctx, mc, map[string]any{"op": "cursor_release", "scope": "user"}); rerr != nil {
			fmt.Fprintf(stdout, "  warning: could not release the flush lease: %v\n", rerr)
		}
	}()

	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		drained, err := memoryOp(ctx, mc, map[string]any{
			"op": "pending_drain", "scope": "user", "limit": 50,
		})
		if err != nil {
			return total, fmt.Errorf("drain: %w", err)
		}
		rows, _ := drained["pending"].([]any)
		ids := make([]any, 0, len(rows))
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				if id, ok := m["id"].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			return total, nil
		}
		if _, err := memoryOp(ctx, mc, map[string]any{
			"op": "pending_ack", "scope": "user", "ids": ids,
		}); err != nil {
			return total, fmt.Errorf("ack %d item(s): %w", len(ids), err)
		}
		total += len(ids)
	}
}

// memoryOp calls one op on the Memory tool and decodes its JSON result.
func memoryOp(ctx context.Context, mc *MCPClient, args map[string]any) (map[string]any, error) {
	txt, err := mc.CallTool(ctx, "memory", args)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		return nil, fmt.Errorf("memory %v: unreadable result %q: %w", args["op"], txt, err)
	}
	return out, nil
}

func consolidateDrain(ctx context.Context, mc *MCPClient, userID string, maxPasses int, stdout io.Writer) (facts int, passes int, err error) {
	const prompt = "Run one consolidation pass for your assigned memory target."
	for passes = 1; passes <= maxPasses; passes++ {
		rr, err := mc.SpawnRun(ctx, "memory/consolidator", userID, prompt)
		if err != nil {
			return facts, passes, fmt.Errorf("consolidation pass %d: %w", passes, err)
		}
		if m := factsRe.FindStringSubmatch(rr.FinalText); m != nil {
			var n int
			_, _ = fmt.Sscanf(m[1], "%d", &n)
			facts += n
		}
		if busyRe.MatchString(rr.FinalText) {
			return facts, passes, &ErrConsolidatorBusy{Report: rr.FinalText}
		}
		m := queuedRe.FindStringSubmatch(rr.FinalText)
		if m == nil {
			fmt.Fprintf(stdout, "  pass %d: no queue depth in the pass report, treating as drained — %s\n",
				passes, truncate(rr.FinalText, 120))
			return facts, passes, nil
		}
		var queued int
		_, _ = fmt.Sscanf(m[1], "%d", &queued)
		fmt.Fprintf(stdout, "  pass %d: %s\n", passes, truncate(rr.FinalText, 160))
		if queued == 0 {
			return facts, passes, nil
		}
	}
	fmt.Fprintf(stdout, "  stopped after %d passes with work still queued — raise -consolidate-passes\n", maxPasses)
	return facts, passes - 1, nil
}

// answerOne asks the answerer, then has the judge grade it against gold.
func answerOne(ctx context.Context, mc *MCPClient, userID string, q Query, answerer, judge string, injectWhen bool) AnswerResult {
	res := AnswerResult{Question: q.Question, Category: q.Category, Gold: q.Answer}
	// The window is resolved HERE and handed to the answerer, rather than left for
	// it to derive. Asked to build one from a system-prompt rule, the answerer
	// ignored the instruction on every question tried; handed one, it passes it
	// through faithfully. Supplying it measures the PREDICATE rather than whether a
	// particular model remembers to construct a window.
	askPrompt := q.Question
	if injectWhen {
		if from, to, ok := ResolveWhen(q.Question, 2023, 3); ok {
			askPrompt += WhenInstruction(from, to)
			res.WindowUsed = true
		}
	}
	start := time.Now()
	rr, err := mc.SpawnRun(ctx, answerer, userID, askPrompt)
	res.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		res.Verdict = VerdictUnparsed
		res.Why = "answer run failed: " + truncate(err.Error(), 140)
		return res
	}
	res.Answer = strings.TrimSpace(rr.FinalText)
	res.Model = rr.Usage.Provider + "/" + rr.Usage.Model
	res.NotFound = strings.Contains(strings.ToUpper(res.Answer), notFound)

	// An abstention needs no judge call — it cannot match a gold answer, and for
	// categories 1-4 (all answerable) it is a miss. Skipping the call saves a
	// model round trip per abstention and removes a chance for the judge to
	// mis-grade one.
	if res.NotFound {
		res.Verdict = VerdictWrong
		res.Why = "answerer abstained (NOT_FOUND)"
		return res
	}
	prompt := fmt.Sprintf("Question: %s\nGold answer: %s\nModel answer: %s", q.Question, q.Answer, res.Answer)
	jr, err := mc.SpawnRun(ctx, judge, "", prompt)
	if err != nil {
		res.Verdict = VerdictUnparsed
		res.Why = "judge run failed: " + truncate(err.Error(), 140)
		return res
	}
	res.Verdict, res.Why = ParseVerdict(jr.FinalText)
	return res
}

// doAnswerAxis runs phase 3: for each conversation, purge the memory layer,
// ingest its sessions, drain consolidation, then answer and grade its questions.
//
// Sequential across conversations by necessity — see the file header on why a
// single partition is all an off-run caller can address.
func doAnswerAxis(ctx context.Context, convs []Conversation, defects *Defects, opts options, stdout io.Writer) error {
	rest, id, err := connect(ctx, opts, stdout)
	if err != nil {
		return err
	}
	// The memory-layer partition an off-run `add` lands in is the principal's
	// own subject, and the answering run must recall from that same partition.
	userID := id.Subject
	if userID == "" {
		return fmt.Errorf("the bearer has no subject, so the memory-layer partition cannot be addressed")
	}
	mc := NewMCPClient(opts.instance, bearer(), opts.runTimeout)

	rep := AnswerReport{
		Tool: "locomo", Axis: "answer", StartedAt: time.Now().UTC().Format(time.RFC3339),
		Instance: opts.instance, Tenant: id.TenantID, Subject: userID,
		Answerer: opts.answerer, Judge: opts.judge, Categories: opts.categories,
		Conversations: len(convs), Defects: defects,
		Notes: []string{
			"Ingest is deterministic: turns are handed to `Memory op=add` verbatim over MCP, so a miss " +
				"is attributable to consolidation or recall rather than to an ingesting agent's transcription.",
			"The answerer holds the Memory tool and nothing else, and is instructed to answer NOT_FOUND " +
				"rather than fall back on general knowledge.",
			"Conversations run in sequence with the memory layer purged between them: scope_id is " +
				"server-derived from the run identity, so an off-run caller cannot give each conversation " +
				"its own partition.",
		},
	}

	var all []AnswerResult
	for _, conv := range convs {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s:\n", conv.ScopeID())
		// FLUSH before purging, not after, and DISCARD rather than consolidate.
		// purgeLayer removes k/v rows; it cannot reach the pending consolidation
		// queue, so anything left queued from an earlier conversation (or an
		// interrupted run) would otherwise be consolidated into THIS
		// conversation's facts after the purge had already run, silently mixing
		// two conversations in one partition.
		//
		// This used to call consolidateDrain — the same function that does the
		// real work — for up to consolidatePasses passes. Leftovers only need to
		// be GONE, and extracting from them cost the full extractor spend on data
		// about to be thrown away: measured at ~70 minutes on one run before it
		// reached its own conversation. A flush is drain + ack, no model calls.
		if n, err := flushPendingQueue(ctx, mc, stdout); err != nil {
			return fmt.Errorf("pre-ingest flush: %w", err)
		} else if n > 0 {
			fmt.Fprintf(stdout, "  flushed %d leftover queued item(s) without extracting\n", n)
		}
		if _, err := purgeLayer(ctx, rest, "user", userID, stdout); err != nil {
			return err
		}
		// OPTIONALLY put the raw turns where the answerer can actually read them.
		//
		// WHY THIS EXISTS. The answerer is an in-band agent, so its scope_id is
		// server-derived from the run identity: `memory_scopes: [user]` resolves to
		// user/<subject> and `[agent]` would resolve to agent/<the agent's own
		// name>. The retrieval corpus lives at agent/<conversation-id>, a scope_id
		// no in-band agent can address — so no agent config can point the answerer
		// at it. The only way to let it answer from conversation content is to write
		// that content into the partition it already reads.
		//
		// This is also what makes the number comparable: the published systems
		// answer from retrieved conversation turns, not from a distilled fact store.
		// Measured without it, the answerer had 29 facts distilled from 419 turns,
		// abstained on 72% of questions and scored 0.1389; with it, 0.7353.
		if opts.seedTurns {
			n, err := seedTurnRows(ctx, rest, userID, conv, opts, stdout)
			if err != nil {
				return fmt.Errorf("seed turns: %w", err)
			}
			rep.SeededTurns += n
		}
		sessions, err := ingestLayer(ctx, mc, conv, stdout)
		if err != nil {
			return err
		}
		rep.Sessions += sessions
		rep.Turns += len(conv.Turns)

		facts, passes, err := consolidateDrain(ctx, mc, userID, opts.consolidatePasses, stdout)
		if err != nil {
			return err
		}
		rep.FactsWritten += facts
		fmt.Fprintf(stdout, "  consolidated in %d pass(es), %d facts written\n", passes, facts)

		// REFUSE to grade an empty store. With no rows, every recall comes back
		// empty, the answerer abstains on everything, and the report reads
		// "accuracy 0.0000" — a number about the plumbing wearing the costume of a
		// result about memory. Failing here is the difference between a broken run
		// and a false negative.
		rows, err := rest.ListKeys(ctx, "user", userID, 2*facts+500)
		if err != nil {
			return fmt.Errorf("post-consolidation check: %w", err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("consolidation left %s/%s empty after %d pass(es) over %d sessions: every "+
				"recall would return nothing and the run would score 0 for a reason unrelated to memory. "+
				"Check that the consolidator can reach its extractor model and that the target is not leased",
				"user", userID, passes, sessions)
		}

		// AND refuse when the facts went somewhere the answerer cannot read.
		//
		// The empty check above is not enough, and a live run proved it: with an
		// ontology declaring `@memory_scope: tenant` for the corpus's own entity
		// types, the consolidator PLACED most facts into the tenant scope while the
		// answerer — memory_scopes:[user] — recalls from user/<subject> only. The
		// partition held a handful of rows, so the guard passed, and the run scored
		// 0.0216 with 95% abstention: a number about the wrong partition wearing the
		// costume of a result about consolidation. Three plausible mechanisms were
		// then reasoned on top of it before anyone compared the counter to the store.
		//
		// So compare them. The pass report says how many facts it wrote; this counts
		// how many are reachable where the answerer looks. A large shortfall means
		// diversion, whatever the cause — placement, a mis-scoped consolidator, a
		// fan-out target that is not this subject.
		reachable, diverted := factsDiverted(facts, rows)
		if diverted {
			return fmt.Errorf("consolidation reported %d fact(s) written but only %d are reachable in %s/%s "+
				"(the partition the answerer recalls from): the facts were written to a scope this run cannot read, "+
				"so grading would measure the wrong partition. The usual cause is ontology-declared placement — "+
				"check for a live `@memory_scope` declaration in the tenant's /memory/ontology document and remove it "+
				"for benchmarking, then purge every scope and verify each is empty BY LISTING it",
				facts, reachable, "user", userID)
		}

		qs := SampleQueries(conv.Queries, opts.sampleQuestions)
		if opts.onlyDated {
			// The `when` predicate can only help a question that NAMES a window, so
			// grading the rest to measure it spends the judge on questions whose score
			// cannot move. The filter runs after sampling so the two compose predictably.
			kept := qs[:0:0]
			for _, q := range qs {
				if DateConstrained(q.Question) {
					kept = append(kept, q)
				}
			}
			qs = kept
		}
		if len(qs) == 0 {
			fmt.Fprintf(stdout, "  no questions selected for this conversation\n")
			continue
		}
		fmt.Fprintf(stdout, "  grading %d of %d questions\n", len(qs), len(conv.Queries))
		results, err := gradeQueries(ctx, mc, userID, qs, opts, stdout)
		if err != nil {
			return err
		}
		all = append(all, results...)

		// CHECKPOINT after every conversation, not only at the end.
		//
		// Two full runs were lost to this: one when the judge's provider ran out of
		// balance 47% in, one to a SIGTERM at 95%. Both times every graded verdict
		// lived only in memory, so ~70 minutes of judge spend produced nothing —
		// the log kept the per-conversation COUNTS and none of the verdicts.
		//
		// Writing the partial report each time makes the cost of an interruption
		// proportional to what is left rather than total. It rewrites the same two
		// files, so the last write wins and a completed run is byte-identical to
		// before; a partial one is clearly labelled by its own conversation count.
		if !opts.dryRun {
			partial := rep
			partial.Sampled = len(all)
			partial.Overall = AggregateAnswers("overall", all)
			partial.PerCategory = AnswersByCategory(all)
			partial.Results = all
			if err := partial.Write(opts.out); err != nil {
				// Never fail the run for a checkpoint: the run is the product and the
				// checkpoint is insurance. Say so and carry on.
				fmt.Fprintf(stdout, "  warning: checkpoint write failed: %v\n", err)
			}
		}
	}

	rep.Sampled = len(all)
	rep.Overall = AggregateAnswers("overall", all)
	rep.PerCategory = AnswersByCategory(all)
	rep.Results = all
	rep.Matrix(stdout)
	if opts.dryRun {
		fmt.Fprintln(stdout, "\ndry-run: report not written")
		return nil
	}
	if err := rep.Write(opts.out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nwrote %s/answer-matrix.md and answer-report.json\n", opts.out)
	return nil
}

// gradeQueries answers and grades a question set concurrently.
func gradeQueries(ctx context.Context, mc *MCPClient, userID string, qs []Query, opts options, stdout io.Writer) ([]AnswerResult, error) {
	out := make([]AnswerResult, len(qs))
	type job struct{ i int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < opts.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				out[j.i] = answerOne(ctx, mc, userID, qs[j.i], opts.answerer, opts.judge, opts.injectWhen)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range qs {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{i}:
			}
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Report progress after the fact rather than per answer: the pool finishes
	// out of order and interleaved per-answer lines are unreadable.
	graded := 0
	for _, r := range out {
		if r.Verdict != VerdictUnparsed {
			graded++
		}
	}
	fmt.Fprintf(stdout, "  graded %d/%d (%d unparsed)\n", graded, len(out), len(out)-graded)
	return out, nil
}
