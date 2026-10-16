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
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
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
	semStats := semaphoreSnapshot(s)
	perUserOpt := semStats.PerUser
	writeGauge(w, "loomcycle_concurrency_active",
		"Runs currently holding a concurrency slot (semaphore active count).",
		replicaLabels, float64(semStats.Active))
	writeGauge(w, "loomcycle_concurrency_queued",
		"Runs waiting in the queue for a slot (semaphore queued count).",
		replicaLabels, float64(semStats.Queued))
	// A lent run holds no slot and is not queued, yet counts against the queue
	// bound with the queued ones: without this series the queue looks to have
	// room that admission refuses.
	writeGauge(w, "loomcycle_concurrency_lent",
		"Admitted runs that lent their slot while a model call waits for a provider lane; they count against the queue bound with the queued runs.",
		replicaLabels, float64(semStats.Lent))

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

	// Per-provider call caps (each capped provider's chat lane) — only emitted
	// when at least one provider sets max_concurrent (otherwise no lane exists and the series would
	// be empty). Two gauges (slots in use + queue depth), labelled by provider,
	// sorted for deterministic scrape output.
	if pg := providerLaneSnapshot(s); len(pg) > 0 {
		provs := make([]string, 0, len(pg))
		for id := range pg {
			provs = append(provs, id)
		}
		sort.Strings(provs)
		fmt.Fprintln(w, "# HELP loomcycle_provider_slots_in_use Model calls in flight on a capped provider (providers.<id>.max_concurrent).")
		fmt.Fprintln(w, "# TYPE loomcycle_provider_slots_in_use gauge")
		for _, id := range provs {
			labels := mergeLabels(replicaLabels, map[string]string{"provider": id})
			fmt.Fprintf(w, "loomcycle_provider_slots_in_use%s %d\n", labels, pg[id].Active)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "# HELP loomcycle_provider_queue_depth Model calls waiting for a slot on a capped provider.")
		fmt.Fprintln(w, "# TYPE loomcycle_provider_queue_depth gauge")
		for _, id := range provs {
			labels := mergeLabels(replicaLabels, map[string]string{"provider": id})
			fmt.Fprintf(w, "loomcycle_provider_queue_depth%s %d\n", labels, pg[id].Queued)
		}
		fmt.Fprintln(w)
	}
	writeLaneGauges(w, replicaLabels, s.callSched.Stats())

	// RFC DT per-(provider, model) throughput — only models this replica has
	// measured (the estimate is per replica, like every gauge here). The
	// multiplier is what a scaled timeout WOULD use; in measure mode none does.
	if stats := s.throughput.All(); len(stats) > 0 {
		writeModelGauges(w, replicaLabels, stats)
	}

	// Channel hooks — only when the worker runs.
	if s.channelHookStats != nil {
		st := s.channelHookStats()
		writeGauge(w, "loomcycle_channel_hooks_in_flight",
			"Channel messages this replica's hook worker is deciding now.",
			replicaLabels, float64(st.InFlight))
		kinds := make([]string, 0, len(st.Decisions))
		for k := range st.Decisions {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		fmt.Fprintln(w, "# HELP loomcycle_channel_hooks_decisions_total Channel hook decisions made by this replica, by decision (release, rewrite_body, drop, hold, unavailable).")
		fmt.Fprintln(w, "# TYPE loomcycle_channel_hooks_decisions_total counter")
		for _, k := range kinds {
			labels := mergeLabels(replicaLabels, map[string]string{"decision": k})
			fmt.Fprintf(w, "loomcycle_channel_hooks_decisions_total%s %d\n", labels, st.Decisions[k])
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

// writeModelGauges emits the throughput estimate as one series per measured
// (provider, model). decode_tps is skipped for a model whose decode speed was
// never measurable (no first-event time and no driver phase).
func writeModelGauges(w io.Writer, replicaLabels string, stats []throughput.Stat) {
	type gauge struct {
		name, help string
		value      func(throughput.Stat) (float64, bool)
	}
	for _, g := range []gauge{
		{"loomcycle_model_slowdown", "Measured slowdown of a model against the reference machine (timeout_scaling.reference); 1 = reference speed.",
			func(st throughput.Stat) (float64, bool) { return st.Slowdown, true }},
		{"loomcycle_model_timeout_multiplier", "Timeout multiplier the throughput estimate implies for a model (reported; applied only when timeout scaling is on).",
			func(st throughput.Stat) (float64, bool) { return st.Multiplier, true }},
		{"loomcycle_model_decode_tps", "Smoothed output tokens per second a model generates.",
			func(st throughput.Stat) (float64, bool) { return st.DecodeTPS, st.DecodeTPS > 0 }},
		{"loomcycle_model_throughput_samples", "Calls the model's throughput estimate is built from on this replica.",
			func(st throughput.Stat) (float64, bool) { return float64(st.Samples), true }},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n", g.name, g.help)
		fmt.Fprintf(w, "# TYPE %s gauge\n", g.name)
		for _, st := range stats {
			v, ok := g.value(st)
			if !ok {
				continue
			}
			labels := mergeLabels(replicaLabels, map[string]string{"provider": st.Provider, "model": st.Model})
			fmt.Fprintf(w, "%s%s %s\n", g.name, labels, strconv.FormatFloat(v, 'g', -1, 64))
		}
		fmt.Fprintln(w)
	}
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
func semaphoreSnapshot(s *Server) concurrency.Stats {
	if s == nil || s.sem == nil {
		return concurrency.Stats{}
	}
	return s.sem.Stats()
}

// laneSeries is one per-lane series: its name, type, help and the value it
// reads off a lane.
type laneSeries struct {
	name, typ, help string
	value           func(concurrency.LaneStats) uint64
}

// laneSeriesList is the per-lane series, in scrape order. The two
// loomcycle_provider_* gauges above are the chat lane alone and predate lanes;
// these carry every lane, labelled by kind.
var laneSeriesList = []laneSeries{
	{"loomcycle_provider_lane_max_concurrent", "gauge", "Configured cap on calls in flight in a provider's lane (providers.<id>.lanes.<kind>.max_concurrent).",
		func(l concurrency.LaneStats) uint64 { return uint64(l.MaxConcurrent) }},
	{"loomcycle_provider_lane_slots_in_use", "gauge", "Calls in flight in a provider's lane.",
		func(l concurrency.LaneStats) uint64 { return uint64(l.Active) }},
	{"loomcycle_provider_lane_slots_reserved", "gauge", "Slots idle but held for the run that last used them (affinity_hold_ms).",
		func(l concurrency.LaneStats) uint64 { return uint64(l.Reserved) }},
	{"loomcycle_provider_lane_queue_depth", "gauge", "Calls waiting for a slot in a provider's lane.",
		func(l concurrency.LaneStats) uint64 { return uint64(l.Queued) }},
	{"loomcycle_provider_lane_queue_depth_background", "gauge", "Of the calls waiting in a provider's lane, those that are maintenance work.",
		func(l concurrency.LaneStats) uint64 { return uint64(l.QueuedBackground) }},
	{"loomcycle_provider_lane_queue_depth_blocked_on_model", "gauge", "Of the calls waiting in a provider's lane, those held back by their model's own cap (model_caps).",
		func(l concurrency.LaneStats) uint64 { return uint64(l.BlockedOnModel) }},
	{"loomcycle_provider_lane_grants_total", "counter", "Calls admitted to a provider's lane since it was built.",
		func(l concurrency.LaneStats) uint64 { return l.Grants }},
	{"loomcycle_provider_lane_preemptions_total", "counter", "Times a run gave its held slot up to a waiter (quantum_calls).",
		func(l concurrency.LaneStats) uint64 { return l.Preemptions }},
}

// writeLaneGauges emits one series per (provider, kind) lane plus, per provider,
// how many distinct models are computing and how many calls wait for their
// model (max_models_in_flight). Nothing is written when no provider caps
// anything; output is sorted so a scrape is deterministic.
func writeLaneGauges(w io.Writer, replicaLabels string, stats map[string]concurrency.ProviderLaneStats) {
	if len(stats) == 0 {
		return
	}
	provs := make([]string, 0, len(stats))
	for id := range stats {
		provs = append(provs, id)
	}
	sort.Strings(provs)
	type lane struct {
		labels string
		stats  concurrency.LaneStats
	}
	var lanes []lane
	for _, id := range provs {
		kinds := make([]string, 0, len(stats[id].Lanes))
		for kind := range stats[id].Lanes {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			lanes = append(lanes, lane{
				labels: mergeLabels(replicaLabels, map[string]string{"provider": id, "kind": kind}),
				stats:  stats[id].Lanes[kind],
			})
		}
	}
	if len(lanes) > 0 {
		for _, s := range laneSeriesList {
			fmt.Fprintf(w, "# HELP %s %s\n", s.name, s.help)
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, s.typ)
			for _, l := range lanes {
				fmt.Fprintf(w, "%s%s %d\n", s.name, l.labels, s.value(l.stats))
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w, "# HELP loomcycle_provider_models_in_flight Distinct models computing on a provider, as its max_models_in_flight counts them.")
	fmt.Fprintln(w, "# TYPE loomcycle_provider_models_in_flight gauge")
	for _, id := range provs {
		fmt.Fprintf(w, "loomcycle_provider_models_in_flight%s %d\n",
			mergeLabels(replicaLabels, map[string]string{"provider": id}), len(stats[id].Models.InFlight))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# HELP loomcycle_provider_model_waiters Calls waiting for their model to be admitted on a provider.")
	fmt.Fprintln(w, "# TYPE loomcycle_provider_model_waiters gauge")
	for _, id := range provs {
		fmt.Fprintf(w, "loomcycle_provider_model_waiters%s %d\n",
			mergeLabels(replicaLabels, map[string]string{"provider": id}), stats[id].Models.Waiting)
	}
	fmt.Fprintln(w)
}

// providerLaneSnapshot is a nil-safe view of each capped provider's CHAT lane
// (providers.<id>.max_concurrent), keyed by provider id: its calls in flight
// and its calls waiting. A provider that caps only other kinds of call, or only
// models in flight, is left out. Returns nil when no provider caps chat calls
// (the common case), so the gauges and the stats block are omitted.
func providerLaneSnapshot(s *Server) map[string]concurrency.LaneStats {
	if s == nil {
		return nil
	}
	var out map[string]concurrency.LaneStats
	for id, ps := range s.callSched.Stats() {
		ls, ok := ps.Lanes[config.ModelKindChat]
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]concurrency.LaneStats{}
		}
		out[id] = ls
	}
	return out
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
