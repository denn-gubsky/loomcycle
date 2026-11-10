package otel

import (
	"context"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// PromOptions is what one /metrics reader may see of the instruments, and the
// labels every series carries.
type PromOptions struct {
	// Confined renders only the series whose tenant label is Tenant — the
	// shared "" tenant included, which is a tenant like any other. A series
	// with no tenant label — a database pool, a provider attempt, the
	// overflow series that folds every tenant together — is operator-wide
	// and is left out. A non-empty Tenant confines too, so a view can never
	// name a tenant and still see every one.
	Confined bool
	Tenant   string
	// OmitUserSeries leaves out every series that names a user (a label in
	// userLabels). No instrument carries one today; the switch is here so
	// one that ever does is left out of a view that must not show users.
	OmitUserSeries bool
	// Labels are added to every series: the replica id, so these series
	// carry the same instance label as the hand-rendered ones.
	Labels map[string]string
}

// userLabels are the label names that identify a user.
var userLabels = []string{"user", "user_id", "user_hash", "subject"}

// WritePrometheus renders the current value of every loomcycle_* instrument
// in the Prometheus text exposition format (version 0.0.4): HELP and TYPE
// lines, counters suffixed _total, histograms as cumulative _bucket{le}
// series plus _sum and _count, families sorted by name and series by labels,
// so two scrapes of the same state are byte-identical. It reads memory only.
// Nothing is written before InitMetrics.
func WritePrometheus(ctx context.Context, w io.Writer, opts PromOptions) error {
	rm, err := collect(ctx)
	if err != nil || rm == nil {
		return err
	}
	_, err = io.WriteString(w, renderPrometheus(rm, opts))
	return err
}

type promFamily struct {
	name, help, typ string
	series          []promSeries
}

type promSeries struct {
	labels string   // the rendered label set, the sort key
	lines  []string // one sample line, or a histogram's bucket/sum/count lines
}

// renderPrometheus is WritePrometheus on a collected snapshot.
func renderPrometheus(rm *metricdata.ResourceMetrics, opts PromOptions) string {
	var fams []promFamily
	for _, sm := range rm.ScopeMetrics {
		// The GenAI convention histograms (their own scope) go out over OTLP
		// only; they repeat loomcycle_* series under other names.
		if sm.Scope.Name != MeterName {
			continue
		}
		for _, m := range sm.Metrics {
			if f, ok := promFamilyOf(m, opts); ok && len(f.series) > 0 {
				fams = append(fams, f)
			}
		}
	}
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	var b strings.Builder
	for _, f := range fams {
		sort.Slice(f.series, func(i, j int) bool { return f.series[i].labels < f.series[j].labels })
		b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n")
		b.WriteString("# TYPE " + f.name + " " + f.typ + "\n")
		for _, s := range f.series {
			for _, l := range s.lines {
				b.WriteString(l)
				b.WriteByte('\n')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func promFamilyOf(m metricdata.Metrics, opts PromOptions) (promFamily, bool) {
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		return sumFamily(m, d.DataPoints, d.IsMonotonic, opts), true
	case metricdata.Sum[float64]:
		return sumFamily(m, d.DataPoints, d.IsMonotonic, opts), true
	case metricdata.Gauge[int64]:
		return sumFamily(m, d.DataPoints, false, opts), true
	case metricdata.Gauge[float64]:
		return sumFamily(m, d.DataPoints, false, opts), true
	case metricdata.Histogram[int64]:
		return histogramFamily(m, d.DataPoints, opts), true
	case metricdata.Histogram[float64]:
		return histogramFamily(m, d.DataPoints, opts), true
	}
	return promFamily{}, false
}

// sumFamily renders a sum (a counter when monotonic, else a gauge, as an
// up-down counter is) or a gauge.
func sumFamily[N int64 | float64](m metricdata.Metrics, dps []metricdata.DataPoint[N], monotonic bool, opts PromOptions) promFamily {
	typ := "gauge"
	if monotonic {
		typ = "counter"
	}
	f := promFamily{name: PromName(m.Name, m.Unit, typ), help: m.Description, typ: typ}
	for _, dp := range dps {
		if !opts.shows(dp.Attributes) {
			continue
		}
		labels := promLabels(dp.Attributes, opts.Labels, "", "")
		f.series = append(f.series, promSeries{labels: labels, lines: []string{f.name + labels + " " + formatNumber(dp.Value)}})
	}
	return f
}

func histogramFamily[N int64 | float64](m metricdata.Metrics, dps []metricdata.HistogramDataPoint[N], opts PromOptions) promFamily {
	f := promFamily{name: PromName(m.Name, m.Unit, "histogram"), help: m.Description, typ: "histogram"}
	for _, dp := range dps {
		if !opts.shows(dp.Attributes) {
			continue
		}
		labels := promLabels(dp.Attributes, opts.Labels, "", "")
		lines := make([]string, 0, len(dp.Bounds)+3)
		var cum uint64
		for i, bound := range dp.Bounds {
			if i < len(dp.BucketCounts) {
				cum += dp.BucketCounts[i]
			}
			lines = append(lines, f.name+"_bucket"+promLabels(dp.Attributes, opts.Labels, "le", formatNumber(bound))+" "+strconv.FormatUint(cum, 10))
		}
		lines = append(lines,
			f.name+"_bucket"+promLabels(dp.Attributes, opts.Labels, "le", "+Inf")+" "+strconv.FormatUint(dp.Count, 10),
			f.name+"_sum"+labels+" "+formatNumber(dp.Sum),
			f.name+"_count"+labels+" "+strconv.FormatUint(dp.Count, 10))
		f.series = append(f.series, promSeries{labels: labels, lines: lines})
	}
	return f
}

// shows reports whether a series with attrs is in the view.
func (o PromOptions) shows(attrs attribute.Set) bool {
	if o.Confined || o.Tenant != "" {
		v, ok := attrs.Value(LabelTenant)
		if !ok || v.Emit() != o.Tenant {
			return false
		}
	}
	if o.OmitUserSeries {
		for _, k := range userLabels {
			if attrs.HasValue(attribute.Key(k)) {
				return false
			}
		}
	}
	return true
}

// PromName is the Prometheus name of an instrument: its name with every
// character Prometheus does not allow made "_", the unit's suffix (s →
// seconds, By → bytes, 1 → ratio on a gauge; an annotation such as {run}
// adds nothing), and _total on a counter.
func PromName(name, unit, typ string) string {
	n := sanitizePromName(name)
	if s := unitSuffix(unit, typ); s != "" && !strings.HasSuffix(n, "_"+s) {
		n += "_" + s
	}
	if typ == "counter" && !strings.HasSuffix(n, "_total") {
		n += "_total"
	}
	return n
}

func unitSuffix(unit, typ string) string {
	switch unit {
	case "s":
		return "seconds"
	case "ms":
		return "milliseconds"
	case "By":
		return "bytes"
	case "1":
		if typ == "gauge" {
			return "ratio"
		}
	}
	return ""
}

func sanitizePromName(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := c == '_' || c == ':' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (i > 0 && '0' <= c && c <= '9')
		if !ok {
			b[i] = '_'
		}
	}
	return string(b)
}

// sanitizePromLabel is a label name: no ':' either.
func sanitizePromLabel(s string) string {
	return strings.ReplaceAll(sanitizePromName(s), ":", "_")
}

// promLabels renders attrs plus extra (and an le label when leKey is set) as
// `{k="v",...}`, keys sorted; "" for no labels. An attribute wins over an
// extra label of the same name.
func promLabels(attrs attribute.Set, extra map[string]string, leKey, leValue string) string {
	m := make(map[string]string, attrs.Len()+len(extra)+1)
	for k, v := range extra {
		m[k] = v
	}
	it := attrs.Iter()
	for it.Next() {
		kv := it.Attribute()
		m[sanitizePromLabel(string(kv.Key))] = kv.Value.Emit()
	}
	if leKey != "" {
		m[leKey] = leValue
	}
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
		b.WriteString(escapeLabel(m[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes a label value as the exposition format requires:
// backslash, double quote and newline.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// escapeHelp escapes HELP text: backslash and newline.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func formatNumber[N int64 | float64](v N) string {
	switch x := any(v).(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		switch {
		case math.IsInf(x, 1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		case math.IsNaN(x):
			return "NaN"
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}
