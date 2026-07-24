// metrics_prom.go — GET /metrics in Prometheus text-format. Designed
// to satisfy a stock Prometheus scrape config out of the box (15 s
// interval, bearer-auth via the same middleware as /v1/_metrics/*).
//
// Unlike /v1/_metrics/* (which reads pre-aggregated DB samples), this
// endpoint is a LIVE read against the runtime — runtime.MemStats +
// Semaphore.Stats — so each scrape returns the current process shape
// without a DB hop. Works even when the DB-backed sampler is disabled
// (LOOMCYCLE_METRICS_ENABLED=0) since the live counters are always
// trivially available.
//
// Architectural lock (RFC observability-profiles.md Decision 1): this
// endpoint exposes ONLY substrate metrics — process resources +
// concurrency state. Per-run shape (latency, token spend, error rate)
// is handled separately: spans flow through OTEL to Tempo, and the
// OTEL Collector's spanmetrics connector aggregates them into
// Prometheus histograms downstream (no in-process span processor).
package http

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
)

// handleMetricsProm serves GET /metrics. Bearer-authed (middleware
// already gates the route). Live-read; no DB query.
func (s *Server) handleMetricsProm(w http.ResponseWriter, _ *http.Request) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	replicaLabels := promReplicaLabels()
	buildLabels := promBuildLabels()

	// Process-resource gauges.
	writeGauge(w, "loomcycle_process_rss_bytes",
		"Process resident set size as reported by runtime.MemStats.Sys (bytes).",
		replicaLabels, float64(memStats.Sys))
	writeGauge(w, "loomcycle_process_heap_alloc_bytes",
		"Bytes of heap currently allocated (runtime.MemStats.HeapAlloc).",
		replicaLabels, float64(memStats.HeapAlloc))
	writeGauge(w, "loomcycle_process_goroutines",
		"Number of goroutines that currently exist (runtime.NumGoroutine).",
		replicaLabels, float64(runtime.NumGoroutine()))

	// Concurrency gauges. When the semaphore is nil (early init / unit
	// tests with a partial Server), emit zeros — keeps the metric series
	// monotonic from the operator's POV rather than appearing/disappearing.
	active, queued, perUserOpt := semaphoreSnapshot(s)
	writeGauge(w, "loomcycle_concurrency_active",
		"Runs currently holding a concurrency slot (semaphore active count).",
		replicaLabels, float64(active))
	writeGauge(w, "loomcycle_concurrency_queued",
		"Runs waiting in the queue for a slot (semaphore queued count).",
		replicaLabels, float64(queued))

	// TODO(RFC BL): per-scope memory footprint gauge (row count / bytes per
	// scope). Deferred from PR6 — there is no cheap all-scopes footprint source:
	// store.MemoryEmbedStats is per-(tenant,scope) and would require enumerating
	// every scope_id at scrape time, i.e. the hot per-scrape scan this
	// substrate-only, live-read endpoint must avoid (see the file-header lock).
	// Wire it once a cached per-scope sampler exists (RFC BL P2).

	// Per-user series — only emitted when per-user cap is engaged,
	// otherwise the cardinality of `user_id` could explode on anonymous
	// workloads. We can't directly observe the cap value from Stats(),
	// but the PerUser map is nil/empty when the cap is disabled (see
	// internal/concurrency/semaphore.go — only populated under the
	// per-user-capped Acquire path).
	if len(perUserOpt) > 0 {
		fmt.Fprintln(w, "# HELP loomcycle_concurrency_per_user Runs currently held (active+queued) per user_id; only emitted when MaxConcurrentRunsPerUser > 0.")
		fmt.Fprintln(w, "# TYPE loomcycle_concurrency_per_user gauge")
		// Sort user IDs so successive scrapes produce deterministic
		// output — easier to diff in regression tests + nicer for
		// operators eyeballing the endpoint manually.
		users := make([]string, 0, len(perUserOpt))
		for u := range perUserOpt {
			users = append(users, u)
		}
		sort.Strings(users)
		for _, u := range users {
			labels := mergeLabels(replicaLabels, map[string]string{"user_id": u})
			fmt.Fprintf(w, "loomcycle_concurrency_per_user%s %d\n", labels, perUserOpt[u])
		}
		fmt.Fprintln(w)
	}

	// RFC BF P2b per-provider concurrency gates — only emitted when at least one
	// provider sets max_concurrent (otherwise no gates exist and the series would
	// be empty). Two gauges (slots in use + queue depth), labelled by provider,
	// sorted for deterministic scrape output.
	if pg := providerGateSnapshot(s); len(pg) > 0 {
		provs := make([]string, 0, len(pg))
		for id := range pg {
			provs = append(provs, id)
		}
		sort.Strings(provs)
		fmt.Fprintln(w, "# HELP loomcycle_provider_slots_in_use Runs currently holding a per-provider concurrency slot (providers.<id>.max_concurrent).")
		fmt.Fprintln(w, "# TYPE loomcycle_provider_slots_in_use gauge")
		for _, id := range provs {
			labels := mergeLabels(replicaLabels, map[string]string{"provider": id})
			fmt.Fprintf(w, "loomcycle_provider_slots_in_use%s %d\n", labels, pg[id].Active)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "# HELP loomcycle_provider_queue_depth Runs waiting for a per-provider concurrency slot.")
		fmt.Fprintln(w, "# TYPE loomcycle_provider_queue_depth gauge")
		for _, id := range provs {
			labels := mergeLabels(replicaLabels, map[string]string{"provider": id})
			fmt.Fprintf(w, "loomcycle_provider_queue_depth%s %d\n", labels, pg[id].Queued)
		}
		fmt.Fprintln(w)
	}

	// Build info as a single-series gauge=1 with version metadata as
	// labels. Standard Prometheus convention — operators alert on
	// version churn or filter by version in a multi-replica cluster.
	fmt.Fprintln(w, "# HELP loomcycle_build_info Loomcycle build identification (version, commit, go version) as labels; value is always 1.")
	fmt.Fprintln(w, "# TYPE loomcycle_build_info gauge")
	fmt.Fprintf(w, "loomcycle_build_info%s 1\n", buildLabels)
}

// writeGauge emits one HELP + TYPE + sample triple. Trailing blank
// line separates metric blocks per Prometheus convention.
func writeGauge(w io.Writer, name, help, labels string, value float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	// %g produces a compact float repr (no trailing zeros). For integer
	// values from runtime + sem.Stats this is identical to strconv.Itoa.
	fmt.Fprintf(w, "%s%s %s\n\n", name, labels, strconv.FormatFloat(value, 'g', -1, 64))
}

// semaphoreSnapshot is a nil-safe view of Server.sem state. Returns
// zero values + nil PerUser when the semaphore is unwired.
func semaphoreSnapshot(s *Server) (active, queued int, perUser map[string]int) {
	if s == nil || s.sem == nil {
		return 0, 0, nil
	}
	stats := s.sem.Stats()
	return stats.Active, stats.Queued, stats.PerUser
}

// providerGateSnapshot is a nil-safe view of the RFC BF P2b per-provider gates.
// Returns nil when none are wired (the common case), so the gauges are omitted.
func providerGateSnapshot(s *Server) map[string]concurrency.Stats {
	if s == nil {
		return nil
	}
	return s.providerGates.Stats()
}

// promReplicaLabels returns `{replica_id="..."}` when running in
// cluster mode (LOOMCYCLE_REPLICA_ID set), otherwise empty string.
// Empty-string omission keeps single-replica deployments clean —
// operators never see a label they don't care about.
func promReplicaLabels() string {
	id := strings.TrimSpace(os.Getenv("LOOMCYCLE_REPLICA_ID"))
	if id == "" {
		return ""
	}
	return labelsToString(map[string]string{"replica_id": id})
}

// promBuildLabels reads version metadata via runtime/debug.ReadBuildInfo.
// Same source main.go consults for --version output. Stable across
// release styles (ldflags-injected version OR pure VCS-stamped build).
func promBuildLabels() string {
	info, ok := debug.ReadBuildInfo()
	labels := map[string]string{
		"version":    "unknown",
		"commit":     "unknown",
		"built":      "unknown",
		"go_version": runtime.Version(),
	}
	if ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			labels["version"] = info.Main.Version
		}
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				if kv.Value != "" {
					labels["commit"] = kv.Value
				}
			case "vcs.time":
				if kv.Value != "" {
					labels["built"] = kv.Value
				}
			}
		}
	}
	if r := strings.TrimSpace(os.Getenv("LOOMCYCLE_REPLICA_ID")); r != "" {
		labels["replica_id"] = r
	}
	return labelsToString(labels)
}

// mergeLabels merges a base label set (already pre-formatted as the
// `{...}` literal) with an additional map. Used to add `user_id` to
// the existing replica-labels base for per-user series. Returns the
// formatted `{...}` literal.
func mergeLabels(base string, extra map[string]string) string {
	// Decompose base into a map, merge, reformat. Cheap given the
	// label set is tiny.
	merged := decomposeLabels(base)
	for k, v := range extra {
		merged[k] = v
	}
	return labelsToString(merged)
}

// labelsToString formats a label map as the Prometheus `{k="v",...}`
// literal. Empty map returns empty string (no braces). Keys sorted
// for deterministic output.
func labelsToString(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(m[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// decomposeLabels parses the inverse of labelsToString. Used by
// mergeLabels. Empty input returns an empty map.
func decomposeLabels(s string) map[string]string {
	out := map[string]string{}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return out
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return out
	}
	// Split on `,` but careful: values can't contain `,` because
	// escapeLabelValue escapes it. So a simple split is safe.
	for _, kv := range strings.Split(inner, ",") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		val := kv[eq+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		out[key] = unescapeLabelValue(val)
	}
	return out
}

// escapeLabelValue per the Prometheus exposition format: backslash,
// double-quote, and newline must be escaped. Operators almost never
// hit these in practice but the contract is the contract.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unescapeLabelValue inverts escapeLabelValue.
func unescapeLabelValue(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
