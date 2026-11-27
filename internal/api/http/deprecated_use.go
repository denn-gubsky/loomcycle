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

// deprecated_use.go — a run whose definition names what v1.172.0 removed
// (RFC GW P15): the Bash tool, Bashbox or the shell_credentials field. The
// run is not given it (it has a capability_inert notice for that); here it is
// counted (loomcycle_deprecated_use_total, its P14 name kept, P15 Q3) and the
// first run per tenant and agent in this process is logged, which is how an
// operator finds the stored definitions to fork. Only names are written:
// never a credential the definition lists.

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

// deprecatedRows are the removed items def names, each once, in table order:
// what a run of it is not given.
func deprecatedRows(def config.AgentDef) []config.Removal { return config.RemovedItemsOf(def) }

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
	d.log.WarnContext(ctx, "a run's definition names what was removed, and the run was not given it",
		"tenant", tenant, "agent", agent, "uses", strings.Join(names, ", "),
		"removed_in", config.RemovedIn, "replacement", "the Workspace tool on a workspace worker", "see", config.MigrationGuide)
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
	writeLine("# HELP loomcycle_deprecated_use_total Runs on this replica whose definition names something v1.172.0 removed (the Bash tool, Bashbox, shell_credentials) and was not given it, by kind and name.")
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
// (admin; RFC GW P14 Q5, kept by P15 Q3): config, always empty now, since a
// config naming what v1.172.0 removed does not load; runs, how many runs on
// this replica had a definition naming each removed item since it started,
// which is where a stored definition's use shows. Both lists are present.
func deprecationsReport(st map[deprecatedUseKey]int64) map[string]any {
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
	return map[string]any{"config": []string{}, "runs": runs}
}
