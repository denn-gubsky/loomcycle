// Command locomo runs the LoCoMo long-term-conversational-memory benchmark
// against an externally-running loomcycle instance.
//
// The dataset (CC BY-NC 4.0) is never vendored here — pass a copy you
// downloaded with -data. See bench/cmd/locomo/README.md.
//
// Modes:
//
//	convert  locomo10.json  ->  one memory-eval JSONL per conversation (no instance needed)
//	ingest   write every turn as a memory row keyed by its dia_id, embedded
//	search   run every QA probe and score retrieval against the evidence key
//	all      ingest then search
//	purge    delete the rows ingest wrote (reclaim the scope)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "locomo:", err)
		os.Exit(1)
	}
}

type options struct {
	mode          string
	data          string
	instance      string
	scope         string
	topK          int
	categories    []int
	conversations int
	concurrency   int
	out           string
	dryRun        bool
	noEmbed       bool
	// unit is the row GRANULARITY (RFC CM-1): "turn" reproduces every run before
	// this one, "session" collapses each session into one row. The comparison is the
	// whole point of CM-1 — an episode tier that does not beat verbatim turns is not
	// worth building.
	unit string
	// dated stamps each row's observed time from its session date, so the RFC CL
	// `when` predicate has something to filter on. Off reproduces the undated corpus
	// every prior number was measured against.
	dated bool
	// onlyDated restricts grading to the date-constrained slice — 31 of 1,535
	// questions. It is the only slice the `when` predicate can help, and grading the
	// other 1,504 to measure it costs 75 minutes of judge to move a headline number
	// by half a point.
	onlyDated         bool
	injectWhen        bool
	allowSharedTenant bool
	timeout           time.Duration
	answerer          string
	judge             string
	sampleQuestions   int
	consolidatePasses int
	seedTurns         bool
	runTimeout        time.Duration
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("locomo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		mode        = fs.String("mode", "convert", "convert | ingest | search | all | purge | answer")
		data        = fs.String("data", "", "path to locomo10.json (required; not vendored — see README)")
		instance    = fs.String("loomcycle", "http://127.0.0.1:8787", "base URL of the running loomcycle")
		scope       = fs.String("scope", "agent", "memory scope to write/read (agent|user|tenant)")
		topK        = fs.Int("top-k", 10, "retrieval depth metrics are computed at")
		categories  = fs.String("categories", "1,2,3,4", "LoCoMo categories to score (5 is adversarial and has no ground truth)")
		convLimit   = fs.Int("conversations", 0, "only the first N conversations (0 = all)")
		concurrency = fs.Int("concurrency", 4, "parallel requests during ingest")
		out         = fs.String("out", "", "output directory (default: bench/results/locomo-<timestamp>)")
		dryRun      = fs.Bool("dry-run", false, "parse and report, write nothing")
		noEmbed     = fs.Bool("no-embed", false, "skip embedding on ingest (use /v1/_memory/backfill_embeddings after)")
		unit        = fs.String("unit", "turn", "row granularity: turn|session (RFC CM-1)")
		dated       = fs.Bool("dated", false, "stamp observed_at from each row's session date (RFC CL)")
		onlyDated   = fs.Bool("only-date-questions", false, "grade ONLY questions naming an absolute date/window (RFC CL slice)")
		injectWhen  = fs.Bool("inject-when", false, "resolve the question date phrase and hand the answerer a when window")
		allowShared = fs.Bool("allow-shared-tenant", false, "permit writing into the default/legacy tenant (NOT isolated)")
		timeout     = fs.Duration("timeout", 60*time.Second, "per-request timeout")
		answerer    = fs.String("answerer", "locomo/answerer", "agent that answers from memory (answer axis)")
		judge       = fs.String("judge", "locomo/judge", "agent that grades an answer against gold (answer axis)")
		sampleQ     = fs.Int("sample-questions", 0, "answer axis: grade only N questions, stratified by category (0 = all)")
		consPasses  = fs.Int("consolidate-passes", 12, "answer axis: max consolidation passes per conversation (0 = skip consolidation entirely)")
		seedTurns   = fs.Bool("seed-turns", false, "answer axis: also write one embedded row per TURN into the partition the answerer reads, so it answers from conversation content rather than only from distilled facts (this is what the published systems do)")
		runTimeout  = fs.Duration("run-timeout", 10*time.Minute, "answer axis: per-run timeout (agent runs are slower than REST calls)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*data) == "" {
		return fmt.Errorf("-data is required (path to locomo10.json)")
	}
	cats, err := parseCategories(*categories)
	if err != nil {
		return err
	}
	opts := options{
		mode: *mode, data: *data, instance: *instance, scope: *scope,
		topK: *topK, categories: cats, conversations: *convLimit,
		concurrency: *concurrency, out: *out, dryRun: *dryRun, noEmbed: *noEmbed,
		unit: *unit, dated: *dated, onlyDated: *onlyDated, injectWhen: *injectWhen,
		allowSharedTenant: *allowShared, timeout: *timeout,
		answerer: *answerer, judge: *judge, sampleQuestions: *sampleQ,
		consolidatePasses: *consPasses, runTimeout: *runTimeout, seedTurns: *seedTurns,
	}
	if opts.concurrency < 1 {
		opts.concurrency = 1
	}
	if opts.out == "" {
		opts.out = DefaultOutDir(time.Now())
	}

	convs, defects, inFile, err := loadConversations(opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "loaded %d conversations, %d turns, %d scoreable queries from %s\n",
		len(convs), countTurns(convs), countQueries(convs), opts.data)
	if defects.Any() {
		fmt.Fprintf(stdout, "dataset defects: %d unreadable evidence fragments, %d unresolvable ids, "+
			"%d questions without usable evidence, %d sessions without turns\n",
			defects.MalformedEvidenceFragments, defects.UnresolvedEvidenceIDs,
			defects.QueriesWithoutEvidence, defects.SessionsDeclaredWithoutTurns)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch opts.mode {
	case "convert":
		return doConvert(convs, opts, stdout)
	case "ingest":
		_, err := doIngest(ctx, convs, opts, stdout)
		return err
	case "search":
		return doSearch(ctx, convs, defects, inFile, opts, stdout)
	case "all":
		if _, err := doIngest(ctx, convs, opts, stdout); err != nil {
			return err
		}
		return doSearch(ctx, convs, defects, inFile, opts, stdout)
	case "purge":
		return doPurge(ctx, convs, opts, stdout)
	case "answer", "answer-all":
		return doAnswerAxis(ctx, convs, defects, opts, stdout)
	default:
		return fmt.Errorf("unknown -mode %q (convert|ingest|search|all|purge|answer)", opts.mode)
	}
}

// loadConversations loads and applies the -conversations limit, also returning
// how many conversations the FILE held.
//
// The two differ under -conversations, and the defect counts are computed over
// the whole file — so the report has to say which population they describe or a
// smoke run looks like it discarded far more than it did.
func loadConversations(opts options) (convs []Conversation, defects *Defects, inFile int, err error) {
	convs, defects, err = Load(opts.data, opts.categories)
	if err != nil {
		return nil, nil, 0, err
	}
	inFile = len(convs)
	if opts.conversations > 0 && opts.conversations < len(convs) {
		convs = convs[:opts.conversations]
	}
	return convs, defects, inFile, nil
}

func parseCategories(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("-categories: %q is not a number", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-categories: at least one category is required")
	}
	return out, nil
}

func countTurns(cs []Conversation) int {
	n := 0
	for _, c := range cs {
		n += len(c.Turns)
	}
	return n
}

func countQueries(cs []Conversation) int {
	n := 0
	for _, c := range cs {
		n += len(c.Queries)
	}
	return n
}

// bearer reads the token. A benchmark-specific variable is preferred so an
// operator can point this at an isolated tenant without touching the operator
// bearer their tools already use.
func bearer() string {
	// LOCOMO_BENCH_TENANT_TOKEN is accepted because it is what operators
	// actually name the dedicated bench bearer when they mint one.
	for _, env := range []string{"LOOMCYCLE_LOCOMO_TOKEN", "LOCOMO_BENCH_TENANT_TOKEN", "LOOMCYCLE_AUTH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return ""
}

// connect builds the client and refuses to proceed if the bearer resolves to a
// tenant that is not isolated.
//
// This check is the whole reason the harness asks who it is before it writes:
// ~6,000 synthetic conversational rows landing in the tenant an operator
// actually uses is not something you notice until recall gets worse.
func connect(ctx context.Context, opts options, stdout io.Writer) (*Client, Identity, error) {
	tok := bearer()
	if tok == "" {
		return nil, Identity{}, fmt.Errorf("no bearer: set LOOMCYCLE_LOCOMO_TOKEN (preferred) or LOOMCYCLE_AUTH_TOKEN")
	}
	c := NewClient(opts.instance, tok, opts.timeout)
	id, err := c.Whoami(ctx)
	if err != nil {
		return nil, Identity{}, fmt.Errorf("preflight GET /v1/_me: %w", err)
	}
	fmt.Fprintf(stdout, "principal: tenant=%q subject=%q admin=%v scopes=%v\n",
		id.TenantID, id.Subject, id.IsAdmin, id.Scopes)
	if strings.TrimSpace(id.TenantID) == "" && !opts.allowSharedTenant {
		return nil, Identity{}, fmt.Errorf("this bearer resolves to the DEFAULT/legacy tenant, so the corpus " +
			"would mix with real memory. Mint an OperatorTokenDef for a dedicated tenant (e.g. locomo-bench) " +
			"with the substrate:tenant scope and set LOOMCYCLE_LOCOMO_TOKEN to it, or pass -allow-shared-tenant " +
			"to override deliberately")
	}
	return c, id, nil
}

// doConvert writes one memory-eval JSONL per conversation.
//
// One file per conversation, not one combined file, because dia_ids collide
// across conversations — a combined corpus would overwrite rows and let
// queries retrieve another conversation's turns.
func doConvert(convs []Conversation, opts options, stdout io.Writer) error {
	if opts.dryRun {
		fmt.Fprintf(stdout, "dry-run: would write %d datasets to %s\n", len(convs), opts.out)
		return nil
	}
	if err := os.MkdirAll(opts.out, 0o755); err != nil {
		return err
	}
	for _, conv := range convs {
		ds := eval.Dataset{
			Name:   conv.ScopeID(),
			TopK:   opts.topK,
			Corpus: make([]eval.CorpusItem, 0, len(conv.Turns)),
		}
		for _, t := range conv.Turns {
			body := t.Body()
			raw, err := json.Marshal(body)
			if err != nil {
				return err
			}
			ds.Corpus = append(ds.Corpus, eval.CorpusItem{Key: t.DiaID, Value: raw, EmbedText: body})
		}
		var b strings.Builder
		// The header line carries the corpus; LoadJSONL ignores any queries on it
		// and reads them from the following lines.
		hdr := ds
		hdr.Queries = nil
		raw, err := json.Marshal(hdr)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteString("\n")
		for _, q := range conv.Queries {
			raw, err := json.Marshal(eval.Query{Query: q.Question, Expected: q.Expected})
			if err != nil {
				return err
			}
			b.Write(raw)
			b.WriteString("\n")
		}
		path := filepath.Join(opts.out, conv.ScopeID()+".jsonl")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s (%d rows, %d queries)\n", path, len(ds.Corpus), len(conv.Queries))
	}
	fmt.Fprintf(stdout, "\nrun one with: loomcycle memory-eval --dataset %s\n",
		filepath.Join(opts.out, convs[0].ScopeID()+".jsonl"))
	fmt.Fprintln(stdout, "NOTE: memory-eval embeds with a deterministic bag-of-tokens stub, so those "+
		"numbers validate the plumbing, not retrieval quality. Use -mode=all against a real "+
		"instance for a semantic number.")
	return nil
}

type ingestStats struct {
	rows     int
	embedded int
	warnings int
}

func doIngest(ctx context.Context, convs []Conversation, opts options, stdout io.Writer) (ingestStats, error) {
	var st ingestStats
	if opts.dryRun {
		fmt.Fprintf(stdout, "dry-run: would write %d rows across %d scope_ids\n", countTurns(convs), len(convs))
		return st, nil
	}
	c, _, err := connect(ctx, opts, stdout)
	if err != nil {
		return st, err
	}

	// A failure that is not per-row — an expired bearer, a store outage — would
	// otherwise be re-hit once per remaining turn, so the first error cancels the
	// pool rather than driving thousands of doomed requests.
	ctx, cancelPool := context.WithCancel(ctx)
	defer cancelPool()

	// One job shape for both granularities (RFC CM-1). Collapsing them here rather
	// than forking the pool keeps the concurrency, the error handling and the
	// embed-warning abort identical between the two arms — a comparison whose arms
	// differ in their plumbing measures the plumbing.
	type job struct {
		scopeID    string
		key        string
		body       string
		observedAt string
	}
	jobs := make(chan job)
	var (
		mu       sync.Mutex
		firstErr error
		// firstEmbedWarning aborts the run rather than writing thousands of
		// un-embedded rows: an unconfigured embedder or a vector-less store makes
		// every later search return nothing, and finding that out after 6,000
		// writes is a wasted run.
		embedWarn string
	)
	var wg sync.WaitGroup
	for i := 0; i < opts.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				resp, err := c.PutEntryAt(ctx, opts.scope, j.scopeID, j.key, j.body, !opts.noEmbed, j.observedAt)
				mu.Lock()
				switch {
				case err != nil:
					if firstErr == nil {
						firstErr = fmt.Errorf("put %s/%s: %w", j.scopeID, j.key, err)
						cancelPool()
					}
				default:
					st.rows++
					if resp.Embedded {
						st.embedded++
					}
					if resp.EmbedWarning != "" {
						st.warnings++
						if embedWarn == "" {
							embedWarn = resp.EmbedWarning
						}
					}
				}
				mu.Unlock()
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, conv := range convs {
			if opts.unit == "session" {
				for _, r := range SessionRows(conv) {
					obs := ""
					if opts.dated {
						obs = r.ObservedAt
					}
					select {
					case <-ctx.Done():
						return
					case jobs <- job{scopeID: conv.ScopeID(), key: r.Key, body: r.Body, observedAt: obs}:
					}
				}
				continue
			}
			for _, t := range conv.Turns {
				obs := ""
				if opts.dated {
					obs = t.ObservedAt()
				}
				select {
				case <-ctx.Done():
					return
				case jobs <- job{scopeID: conv.ScopeID(), key: t.DiaID, body: t.Body(), observedAt: obs}:
				}
			}
		}
	}()
	wg.Wait()

	if firstErr != nil {
		return st, firstErr
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	fmt.Fprintf(stdout, "ingested %d rows (%d embedded, %d embed warnings)\n", st.rows, st.embedded, st.warnings)
	if !opts.noEmbed && embedWarn != "" {
		return st, fmt.Errorf("the instance could not embed %d row(s); first warning: %s "+
			"(a search over un-embedded rows returns nothing, so this run would score zero for the "+
			"wrong reason)", st.warnings, embedWarn)
	}
	return st, nil
}

func doSearch(ctx context.Context, convs []Conversation, defects *Defects, inFile int, opts options, stdout io.Writer) error {
	c, id, err := connect(ctx, opts, stdout)
	if err != nil {
		return err
	}
	rep := Report{
		Tool: "locomo", StartedAt: time.Now().UTC().Format(time.RFC3339),
		Instance: opts.instance, Tenant: id.TenantID, Subject: id.Subject,
		Scope: opts.scope, TopK: opts.topK, Categories: opts.categories,
		Conversations: len(convs), ConversationsInFile: inFile,
		CorpusRows: countTurns(convs), Defects: defects,
		Notes: []string{
			"Rows are embedded from the JSON-encoded value (the PUT endpoint has no embed_text " +
				"field), so a stored row's embedded text carries surrounding quotes the query text " +
				"does not.",
			"Each conversation is a separate memory scope_id: dia_ids are per-conversation and " +
				"collide across them.",
			"Session timestamps are prefixed onto each row because LoCoMo dates live on the " +
				"session, not the turn — without them the temporal category is unretrievable.",
		},
	}

	var all []QueryResult
	for _, conv := range convs {
		var perConv []QueryResult
		for _, q := range conv.Queries {
			if err := ctx.Err(); err != nil {
				return err
			}
			start := time.Now()
			keys, dim, err := c.Search(ctx, opts.scope, conv.ScopeID(), q.Question, opts.topK)
			if err != nil {
				return fmt.Errorf("search %s: %w", conv.ScopeID(), err)
			}
			if dim > 0 {
				rep.EmbeddingDim = dim
			}
			res := Score(q, keys, opts.topK, time.Since(start))
			perConv = append(perConv, res)
			all = append(all, res)
		}
		rep.PerConversation = append(rep.PerConversation, Aggregate(conv.ScopeID(), perConv, opts.topK))
		fmt.Fprintf(stdout, "  %s: %d queries scored\n", conv.ScopeID(), len(perConv))
	}

	rep.Queries = len(all)
	rep.Overall = Aggregate("overall", all, opts.topK)
	rep.PerCategory = ByCategory(all, opts.topK)
	rep.Results = all

	rep.Matrix(stdout)
	if opts.dryRun {
		fmt.Fprintln(stdout, "\ndry-run: report not written")
		return nil
	}
	if err := rep.Write(opts.out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nwrote %s/matrix.md and report.json\n", opts.out)
	return nil
}

func doPurge(ctx context.Context, convs []Conversation, opts options, stdout io.Writer) error {
	if opts.dryRun {
		fmt.Fprintf(stdout, "dry-run: would delete %d rows across %d scope_ids\n", countTurns(convs), len(convs))
		return nil
	}
	c, _, err := connect(ctx, opts, stdout)
	if err != nil {
		return err
	}
	// EVERY KEY SHAPE THE HARNESS CAN WRITE, or a purge silently leaves rows behind.
	//
	// Keys are derived from the dataset, never from a listing, so purge can only ever
	// remove rows this harness itself would have written — a deliberate safety
	// property, and the reason a NEW key shape has to be added here explicitly.
	// Adding the CM-1 session unit without this left every `D<n>:s` row in place
	// across a re-ingest, and 15.6% of the next run's retrieved keys were rows from
	// the previous arm — a contaminated comparison that looked like a spectacular
	// result. The count was the tell: it read 5,882 both times, because it counts
	// dataset turns rather than rows actually removed.
	deleted := 0
	for _, conv := range convs {
		keys := make([]string, 0, len(conv.Turns))
		for _, t := range conv.Turns {
			keys = append(keys, t.DiaID)
		}
		for _, r := range SessionRows(conv) {
			keys = append(keys, r.Key)
		}
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := c.DeleteEntry(ctx, opts.scope, conv.ScopeID(), k); err != nil {
				return fmt.Errorf("delete %s/%s: %w", conv.ScopeID(), k, err)
			}
			deleted++
		}
	}
	fmt.Fprintf(stdout, "deleted %d rows\n", deleted)
	return nil
}
