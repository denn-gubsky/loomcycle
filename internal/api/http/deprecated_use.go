package http

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// deprecated_use.go — a run given what a future release removes (RFC GW §9,
// P14): the Bash tool, Bashbox, the builder sidecar's mcp__sandbox__ tools or
// the shell_credentials field. Boot warns about the operator's yaml; a stored
// or registered definition is only seen when a run is given it, so it is
// counted here (loomcycle_deprecated_use_total) and the first run per tenant
// and agent in this process is logged. Nothing reaches the model, the
// transcript or the wire, and only names are written: never a credential the
// definition lists.

// maxDeprecatedLogKeys bounds the (tenant, agent) pairs remembered as logged.
// Past it, uses are still counted and no longer logged, so a stream of
// distinct names cannot grow the process.
const maxDeprecatedLogKeys = 4096

// deprecatedUseKey is a series: a table row's kind and name. The label set is
// bounded by the table (config.Removals).
type deprecatedUseKey struct{ Kind, Name string }

type deprecatedUse struct {
	log    *slog.Logger
	mu     sync.Mutex
	counts map[deprecatedUseKey]int64
	logged map[string]bool
}

func newDeprecatedUse(log *slog.Logger) *deprecatedUse {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &deprecatedUse{log: log, counts: map[deprecatedUseKey]int64{}, logged: map[string]bool{}}
}

// deprecatedRows are the table rows def uses, each once, in table order.
func deprecatedRows(def config.AgentDef) []config.Removal {
	used := map[string]bool{}
	for _, tool := range def.Tools {
		if d, ok := config.RemovedTool(tool); ok {
			used[d.Kind+"\x00"+d.Name] = true
		}
	}
	if def.ShellCredentials != nil {
		used[config.RemovedKindField+"\x00shell_credentials"] = true
	}
	var out []config.Removal
	for _, d := range config.Removals() {
		if used[d.Kind+"\x00"+d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// note counts the rows def uses and, when logIt and this (tenant, agent) has
// not been logged, writes one line naming them.
func (d *deprecatedUse) note(ctx context.Context, tenant, agent string, def config.AgentDef, logIt bool) {
	if d == nil {
		return
	}
	rows := deprecatedRows(def)
	if len(rows) == 0 {
		return
	}
	key := tenant + "\x00" + agent
	d.mu.Lock()
	for _, r := range rows {
		d.counts[deprecatedUseKey{r.Kind, r.Name}]++
	}
	first := logIt && !d.logged[key] && len(d.logged) < maxDeprecatedLogKeys
	if first {
		d.logged[key] = true
	}
	d.mu.Unlock()
	if !first {
		return
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	d.log.WarnContext(ctx, "a run was given what will be removed in a future release",
		"tenant", tenant, "agent", agent, "uses", strings.Join(names, ", "),
		"replacement", "the Workspace tool on a workspace worker", "see", config.MigrationGuide)
}

func (d *deprecatedUse) snapshot() map[deprecatedUseKey]int64 {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[deprecatedUseKey]int64, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// writeDeprecatedUse writes loomcycle_deprecated_use_total, by kind and name.
func writeDeprecatedUse(w io.Writer, replicaLabels string, st map[deprecatedUseKey]int64) {
	keys := make([]deprecatedUseKey, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Name < keys[j].Name
	})
	writeLine := func(s string) { _, _ = io.WriteString(w, s+"\n") }
	writeLine("# HELP loomcycle_deprecated_use_total Runs on this replica given something a future release removes (the Bash tool, Bashbox, the builder sidecar's tools, shell_credentials), by kind and name.")
	writeLine("# TYPE loomcycle_deprecated_use_total counter")
	for _, k := range keys {
		writeLine("loomcycle_deprecated_use_total" + mergeLabels(replicaLabels, map[string]string{"kind": k.Kind, "name": k.Name}) + " " + strconv.FormatInt(st[k], 10))
	}
	writeLine("")
}

// deprecatedUseRow is one series of loomcycle_deprecated_use_total, as the
// Settings health panel reads it.
type deprecatedUseRow struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// deprecationsReport is the deprecations field of GET /v1/_config/teams
// (admin; RFC GW P14, owner ruling Q5): config, the load's deprecation lines
// (the words boot and doctor print, names only); runs, how many runs this
// replica gave each deprecated row since it started, which is the only place
// a stored definition's use shows. Both lists are present and empty when
// nothing is used.
func deprecationsReport(lines []string, st map[deprecatedUseKey]int64) map[string]any {
	runs := make([]deprecatedUseRow, 0, len(st))
	for k, n := range st {
		runs = append(runs, deprecatedUseRow{Kind: k.Kind, Name: k.Name, Count: n})
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Kind != runs[j].Kind {
			return runs[i].Kind < runs[j].Kind
		}
		return runs[i].Name < runs[j].Name
	})
	return map[string]any{"config": append([]string{}, lines...), "runs": runs}
}
