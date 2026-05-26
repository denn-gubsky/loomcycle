// circuit-stress — multi-agent load test driver.
//
// Spawns N "circuits", each a 3-agent pipeline (researcher → editor
// → evaluator) communicating via the Channel tool, persisting state
// via Memory, and emitting a verdict via Evaluation.submit. Circuits
// are grouped 10-20 per user_id so the Web UI's per-user agents tree
// is exercised at scale too.
//
// Used to characterise (a) Anthropic OAuth-dev MAX subscription
// capacity, (b) loomcycle binary bottlenecks under x100-x1000
// concurrent runs, (c) functional regressions only visible under
// contention (cursor drift, lost notifies, etc.).
//
// See test/load/circuit-stress/README.md for the full ramp protocol.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed prompts.txt
var defaultPrompts string

type flags struct {
	scale           int
	circuitsPerUser int
	baseURL         string
	token           string
	promptsPath     string
	resultsDir      string
	circuitTimeout  time.Duration
	noCleanup       bool
}

func parseFlags() flags {
	var f flags
	flag.IntVar(&f.scale, "scale", 1, "number of parallel circuits")
	flag.IntVar(&f.circuitsPerUser, "circuits-per-user", 10, "circuits grouped per user_id")
	flag.StringVar(&f.baseURL, "base-url", "http://127.0.0.1:8787", "loomcycle endpoint")
	flag.StringVar(&f.token, "token", os.Getenv("LOOMCYCLE_AUTH_TOKEN"), "bearer (defaults to $LOOMCYCLE_AUTH_TOKEN)")
	flag.StringVar(&f.promptsPath, "prompts", "", "questions file (default: bundled prompts.txt)")
	flag.StringVar(&f.resultsDir, "results-dir", "", "results dir (default: ./results/<RFC3339>)")
	flag.DurationVar(&f.circuitTimeout, "circuit-timeout", 5*time.Minute, "per-circuit deadline before marking failed")
	flag.BoolVar(&f.noCleanup, "no-cleanup", false, "skip post-test sweep of channels + memory")
	flag.Parse()
	if f.scale < 1 {
		fatal("--scale must be >= 1")
	}
	if f.circuitsPerUser < 1 {
		fatal("--circuits-per-user must be >= 1")
	}
	if f.token == "" {
		fatal("--token or $LOOMCYCLE_AUTH_TOKEN must be set")
	}
	if f.resultsDir == "" {
		f.resultsDir = filepath.Join("results", time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	}
	return f
}

// ─── HTTP client (shared, tuned for high concurrency) ───────────────

type lcClient struct {
	base   string
	token  string
	http   *http.Client
	dryRun bool
}

func newClient(base, token string) *lcClient {
	return &lcClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        500,
				MaxIdleConnsPerHost: 500,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *lcClient) do(method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, err
		}
	} else if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, string(buf))
	}
	return resp.StatusCode, nil
}

// ─── Wire shapes ────────────────────────────────────────────────────

type promptSegment struct {
	Role    string                 `json:"role"`
	Content []promptContentBlock   `json:"content"`
	_       struct{}               // sentinel
	Meta    map[string]interface{} `json:"-"`
}

type promptContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type runRequest struct {
	Agent    string          `json:"agent"`
	AgentID  string          `json:"agent_id"`
	UserID   string          `json:"user_id"`
	Segments []promptSegment `json:"segments"`
}

type agentResp struct {
	AgentID    string    `json:"agent_id"`
	RunID      string    `json:"run_id"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	Usage      struct {
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Model        string `json:"model"`
	} `json:"usage"`
}

type memoryEntry struct {
	Value json.RawMessage `json:"value"`
}

// ─── Driver state ───────────────────────────────────────────────────

type circuitResult struct {
	CircuitID       int               `json:"circuit_id"`
	UserID          string            `json:"user_id"`
	Question        string            `json:"question"`
	StartedAt       time.Time         `json:"started_at"`
	EndedAt         time.Time         `json:"ended_at"`
	DurationMS      int64             `json:"duration_ms"`
	Status          string            `json:"status"` // "completed" | "failed" | "timeout"
	AgentStatus     map[string]string `json:"agent_status"`
	AgentDurationMS map[string]int64  `json:"agent_duration_ms"`
	InputTokens     int               `json:"input_tokens"`
	OutputTokens    int               `json:"output_tokens"`
	Score           *float64          `json:"score,omitempty"`
	Rationale       string            `json:"rationale,omitempty"`
	Error           string            `json:"error,omitempty"`
}

func main() {
	f := parseFlags()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := os.MkdirAll(f.resultsDir, 0755); err != nil {
		fatal("mkdir results: %v", err)
	}

	prompts := loadPrompts(f.promptsPath)
	if len(prompts) == 0 {
		fatal("no prompts available")
	}

	c := newClient(f.baseURL, f.token)

	log.Printf("preflight: GET %s/healthz", f.baseURL)
	var health map[string]any
	if status, err := c.do("GET", "/healthz", nil, &health); err != nil {
		fatal("healthz failed: %v (status=%d)", err, status)
	}
	log.Printf("preflight OK: %v", health)

	// Channels are yaml-generated by run.sh (the runtime CRUD
	// validator rejects names with `/` even though the yaml validator
	// accepts them, and we need `/` for the `prefix/*` ACL wildcard).
	// If a custom driver invocation runs against a server whose yaml
	// doesn't include the channels, agent spawns will fail loudly on
	// Channel.publish — that's the right signal.

	log.Printf("spawning %d circuits (%d per user, ~%d users)…",
		f.scale, f.circuitsPerUser, (f.scale+f.circuitsPerUser-1)/f.circuitsPerUser)

	results := runCircuits(c, f, prompts)

	log.Printf("test complete; writing results to %s/", f.resultsDir)
	writeResults(f.resultsDir, results)

	if !f.noCleanup {
		log.Printf("sanity sweep: deleting %d channel pairs + memory entries…", f.scale)
		sweep(c, f.scale, usersFrom(results))
	} else {
		log.Printf("--no-cleanup set; skipping sanity sweep")
	}

	printSummary(results, f.resultsDir)
}

// ─── Prompts ────────────────────────────────────────────────────────

func loadPrompts(path string) []string {
	var raw string
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			fatal("read prompts: %v", err)
		}
		raw = string(b)
	} else {
		raw = defaultPrompts
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// ─── Circuit execution ──────────────────────────────────────────────

func userIDFor(circuitID, perUser int) string {
	group := (circuitID-1)/perUser + 1
	return fmt.Sprintf("user-%03d", group)
}

func runCircuits(c *lcClient, f flags, prompts []string) []circuitResult {
	var (
		wg              sync.WaitGroup
		results         = make([]circuitResult, f.scale)
		quotaExhausted  atomic.Bool
		startedCount    atomic.Int32
		completedCount  atomic.Int32
		// Throttle initial spawn so we don't slam the runs-admit
		// semaphore with all N at once — 50 concurrent launches is
		// plenty for x1000.
		launchSem = make(chan struct{}, 50)
	)

	startWall := time.Now()
	go progressTicker(&startedCount, &completedCount, int32(f.scale), startWall)

	for i := 1; i <= f.scale; i++ {
		if quotaExhausted.Load() {
			results[i-1] = circuitResult{
				CircuitID: i,
				Status:    "skipped",
				Error:     "quota exhausted before launch",
			}
			continue
		}
		wg.Add(1)
		launchSem <- struct{}{}
		go func(circuitID int) {
			defer wg.Done()
			defer func() { <-launchSem }()

			question := prompts[(circuitID-1)%len(prompts)]
			userID := userIDFor(circuitID, f.circuitsPerUser)

			startedCount.Add(1)
			res := runOneCircuit(c, circuitID, userID, question, f.circuitTimeout)
			results[circuitID-1] = res
			completedCount.Add(1)

			if isQuotaError(res.Error) {
				if quotaExhausted.CompareAndSwap(false, true) {
					log.Printf("⚠ QUOTA EXHAUSTED at circuit %d (T+%.0fs) — halting new launches",
						circuitID, time.Since(startWall).Seconds())
				}
			}
		}(i)
	}
	wg.Wait()
	return results
}

func runOneCircuit(c *lcClient, circuitID int, userID, question string, timeout time.Duration) circuitResult {
	res := circuitResult{
		CircuitID:       circuitID,
		UserID:          userID,
		Question:        question,
		StartedAt:       time.Now(),
		AgentStatus:     make(map[string]string),
		AgentDurationMS: make(map[string]int64),
	}
	cid := fmt.Sprintf("c%d", circuitID)
	prompt := fmt.Sprintf("Your circuit_id is %s. Question: %s", cid, question)

	roles := []string{"researcher", "editor", "evaluator"}
	agentIDs := make(map[string]string)
	for _, role := range roles {
		agentIDs[role] = fmt.Sprintf("%s-c%d", role, circuitID)
	}

	for _, role := range roles {
		req := runRequest{
			Agent:   role,
			AgentID: agentIDs[role],
			UserID:  userID,
			Segments: []promptSegment{{
				Role:    "user",
				Content: []promptContentBlock{{Type: "trusted-text", Text: prompt}},
			}},
		}
		if status, err := c.do("POST", "/v1/runs", req, nil); err != nil {
			res.Status = "failed"
			res.Error = fmt.Sprintf("POST /v1/runs (%s): %v (status=%d)", role, err, status)
			res.EndedAt = time.Now()
			res.DurationMS = res.EndedAt.Sub(res.StartedAt).Milliseconds()
			return res
		}
	}

	deadline := time.Now().Add(timeout)
	pending := map[string]bool{}
	for _, r := range roles {
		pending[r] = true
	}
	for len(pending) > 0 {
		if time.Now().After(deadline) {
			res.Status = "timeout"
			res.Error = fmt.Sprintf("circuit timed out after %s; pending=%v", timeout, mapKeys(pending))
			break
		}
		for _, role := range roles {
			if !pending[role] {
				continue
			}
			var ar agentResp
			if status, err := c.do("GET", "/v1/agents/"+agentIDs[role], nil, &ar); err != nil {
				if status == http.StatusNotFound {
					continue // run not yet recorded
				}
				continue
			}
			if isTerminal(ar.Status) {
				res.AgentStatus[role] = ar.Status
				if ar.CompletedAt != nil {
					res.AgentDurationMS[role] = ar.CompletedAt.Sub(ar.StartedAt).Milliseconds()
				}
				res.InputTokens += ar.Usage.InputTokens
				res.OutputTokens += ar.Usage.OutputTokens
				if ar.Status != "completed" && res.Error == "" {
					res.Error = fmt.Sprintf("%s: %s (%s)", role, ar.Status, ar.Error)
				}
				delete(pending, role)
			}
		}
		if len(pending) > 0 {
			time.Sleep(750 * time.Millisecond)
		}
	}

	res.EndedAt = time.Now()
	res.DurationMS = res.EndedAt.Sub(res.StartedAt).Milliseconds()

	allOK := true
	for _, role := range roles {
		if res.AgentStatus[role] != "completed" {
			allOK = false
			break
		}
	}
	if allOK {
		res.Status = "completed"
		fetchScore(c, &res, cid, userID)
	} else if res.Status == "" {
		res.Status = "failed"
	}
	return res
}

func fetchScore(c *lcClient, res *circuitResult, cid, userID string) {
	path := fmt.Sprintf("/v1/_memory/scopes/user/%s/keys/%s-research-scored", userID, cid)
	var entry memoryEntry
	if status, err := c.do("GET", path, nil, &entry); err != nil || status != 200 {
		return
	}
	// value may be a JSON object {score, rationale} OR a JSON string
	// (some models emit it as one). Try both.
	var obj struct {
		Score     float64 `json:"score"`
		Rationale string  `json:"rationale"`
	}
	if err := json.Unmarshal(entry.Value, &obj); err == nil && obj.Score > 0 {
		res.Score = &obj.Score
		res.Rationale = obj.Rationale
		return
	}
	var s string
	if err := json.Unmarshal(entry.Value, &s); err == nil {
		// fall through — rationale captured as the raw string
		res.Rationale = s
	}
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

func isQuotaError(s string) bool {
	if s == "" {
		return false
	}
	l := strings.ToLower(s)
	return strings.Contains(l, "subscription") && strings.Contains(l, "quota")
}

// ─── Sanity sweep ───────────────────────────────────────────────────

func sweep(c *lcClient, scale int, users []string) {
	// Channels are yaml-declared; runtime CRUD DELETE refuses with
	// 409 channel_yaml_immutable. The next test run regenerates the
	// yaml from scratch so any leftover channel state is naturally
	// wiped on loomcycle restart. No-op here.
	_ = scale

	// Memory entries: DELETE each c{N}-* key under each user_id we
	// actually used. Idempotent — 404s on already-missing rows are
	// expected (failed circuits never wrote anything).
	for _, uid := range users {
		// List existing keys so we only DELETE what's there. Skip the
		// list if it errors — just attempt the known patterns.
		path := fmt.Sprintf("/v1/_memory/scopes/user/%s/keys", uid)
		var ldoc struct {
			Entries []struct {
				Key string `json:"key"`
			} `json:"entries"`
		}
		if status, err := c.do("GET", path, nil, &ldoc); err == nil && status == 200 {
			for _, e := range ldoc.Entries {
				if !strings.HasPrefix(e.Key, "c") {
					continue
				}
				delPath := fmt.Sprintf("/v1/_memory/scopes/user/%s/keys/%s", uid, e.Key)
				_, _ = c.do("DELETE", delPath, nil, nil)
			}
		}
	}
}

func usersFrom(results []circuitResult) []string {
	seen := map[string]bool{}
	for _, r := range results {
		if r.UserID != "" {
			seen[r.UserID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// ─── Output ─────────────────────────────────────────────────────────

func writeResults(dir string, results []circuitResult) {
	jsonl, err := os.Create(filepath.Join(dir, "circuits.jsonl"))
	if err != nil {
		fatal("create circuits.jsonl: %v", err)
	}
	defer jsonl.Close()
	enc := json.NewEncoder(jsonl)
	for _, r := range results {
		_ = enc.Encode(r)
	}
}

func printSummary(results []circuitResult, dir string) {
	var (
		completed, failed, timedOut, skipped int
		durations                            []int64
		totalIn, totalOut                    int
		quotaSeen                            bool
		quotaCircuit                         int
		scoreSum                             float64
		scoreN                               int
	)
	for _, r := range results {
		switch r.Status {
		case "completed":
			completed++
			durations = append(durations, r.DurationMS)
			totalIn += r.InputTokens
			totalOut += r.OutputTokens
			if r.Score != nil {
				scoreSum += *r.Score
				scoreN++
			}
		case "failed":
			failed++
		case "timeout":
			timedOut++
		case "skipped":
			skipped++
		}
		if isQuotaError(r.Error) && !quotaSeen {
			quotaSeen = true
			quotaCircuit = r.CircuitID
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p := func(pct float64) int64 {
		if len(durations) == 0 {
			return 0
		}
		idx := int(float64(len(durations)-1) * pct)
		return durations[idx]
	}

	fmt.Println()
	fmt.Println("─── Summary ────────────────────────────────────────────────")
	fmt.Printf("  Circuits: %d total / %d completed / %d failed / %d timeout / %d skipped\n",
		len(results), completed, failed, timedOut, skipped)
	if len(durations) > 0 {
		fmt.Printf("  Duration: p50=%dms  p95=%dms  p99=%dms  max=%dms\n",
			p(0.50), p(0.95), p(0.99), durations[len(durations)-1])
	}
	if completed > 0 {
		fmt.Printf("  Tokens:   total_in=%d  total_out=%d  avg_per_circuit=%d in / %d out\n",
			totalIn, totalOut, totalIn/completed, totalOut/completed)
	}
	if scoreN > 0 {
		fmt.Printf("  Quality:  mean score=%.2f over %d evaluations\n", scoreSum/float64(scoreN), scoreN)
	}
	if quotaSeen {
		fmt.Printf("  ⚠ Anthropic OAuth-dev quota exhausted at circuit %d\n", quotaCircuit)
	}
	fmt.Printf("  Results:  %s/circuits.jsonl\n", dir)
	fmt.Println()
	fmt.Println("Post-test resource snapshot:")
	fmt.Println("  GET /v1/_metrics/summary  (RSS / CPU / goroutines)")
	fmt.Println("  psql -c 'SELECT count(*) FROM runs WHERE agent_id LIKE \\'researcher-c%\\''")
}

// ─── Helpers ────────────────────────────────────────────────────────

func progressTicker(started, completed *atomic.Int32, total int32, start time.Time) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for range tick.C {
		s, c := started.Load(), completed.Load()
		if c >= total {
			return
		}
		log.Printf("progress: %d started, %d completed / %d (T+%.0fs)",
			s, c, total, time.Since(start).Seconds())
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "circuit-stress: error: "+format+"\n", args...)
	os.Exit(2)
}
