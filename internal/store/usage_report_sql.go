package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The usage report's SQL, shared by both backends. Only what genuinely differs
// between them — placeholders, how a time is bound, and the bucket arithmetic
// (Postgres has date_trunc over timestamptz; sqlite stores unix nanoseconds) —
// comes from the dialect, so the two cannot drift on what a report means.

// UsageBucket is the period a usage report splits its window into.
type UsageBucket string

const (
	UsageBucketNone  UsageBucket = ""
	UsageBucketHour  UsageBucket = "hour"
	UsageBucketDay   UsageBucket = "day"
	UsageBucketWeek  UsageBucket = "week" // ISO weeks: Monday 00:00 UTC
	UsageBucketMonth UsageBucket = "month"
)

// ParseUsageBucket validates a caller-supplied bucket.
func ParseUsageBucket(s string) (UsageBucket, error) {
	switch b := UsageBucket(s); b {
	case UsageBucketNone, UsageBucketHour, UsageBucketDay, UsageBucketWeek, UsageBucketMonth:
		return b, nil
	}
	return "", fmt.Errorf("unknown bucket %q (allowed: hour, day, week, month)", s)
}

// The values UsageQuery.TopBy takes.
const (
	UsageTopByCost   = "cost"
	UsageTopByTokens = "tokens"
	UsageTopByCalls  = "calls"
)

// UsageCanonicalDims is the fixed order the report's dimension columns are
// SELECTed and scanned in. A grouped dimension SELECTs its expression; an
// ungrouped one SELECTs an empty string — a fixed shape whatever the group_by.
var UsageCanonicalDims = []UsageDimension{
	UsageByTenant, UsageByUser, UsageByProvider, UsageByModel, UsageBySource, UsageByPurpose,
	UsageByTier, UsageByUserTier, UsageByAgent, UsageByClass,
}

// UsageDimNames is the comma-joined list of the whitelisted dimensions, in
// canonical order — what a refusal of an unknown group_by tells the caller is
// allowed, derived here so the transports cannot list a stale set.
func UsageDimNames() string {
	names := make([]string, len(UsageCanonicalDims))
	for i, d := range UsageCanonicalDims {
		names[i] = string(d)
	}
	return strings.Join(names, ",")
}

// UsageDimColumn maps a whitelisted dimension to its SQL expression over the
// report's union of token_usage and usage_archive. The whitelist IS the
// SQL-injection guard: only these map to an expression, so a caller-supplied
// group_by or filter key can never inject SQL. ok=false rejects the rest.
func UsageDimColumn(d UsageDimension) (expr string, ok bool) {
	switch d {
	case UsageByTenant:
		return "tenant_id", true
	case UsageByUser:
		return "user_id", true
	case UsageByProvider:
		return "provider", true
	case UsageByModel:
		return "model", true
	case UsageBySource:
		return "credential_source", true
	case UsageByPurpose:
		return "purpose", true
	case UsageByTier:
		return "tier", true
	case UsageByUserTier:
		return "user_tier", true
	case UsageByAgent:
		return "agent_id", true
	case UsageByClass:
		return usageClassExpr(), true
	}
	return "", false
}

// usageClassExpr derives the class from the purpose, from the same table
// NormalizeUsageRow checks a purpose against. Purpose labels are fixed
// identifiers, never caller input, so they are inlined.
func usageClassExpr() string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, p := range sortedPurposes() {
		fmt.Fprintf(&b, " WHEN purpose = '%s' THEN '%s'", p, usagePurposeClass[p])
	}
	b.WriteString(" ELSE '' END")
	return b.String()
}

func sortedPurposes() []string {
	out := make([]string, 0, len(usagePurposeClass))
	for p := range usagePurposeClass {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// purposesOfClass lists the purposes of one class, for a class filter.
func purposesOfClass(class string) []string {
	var out []string
	for _, p := range sortedPurposes() {
		if usagePurposeClass[p] == class {
			out = append(out, p)
		}
	}
	return out
}

// UsageGroupColumns builds a report's per-dimension SELECT expressions and its
// GROUP BY list, in UsageCanonicalDims order: a grouped dimension emits its
// expression into BOTH lists; an ungrouped one emits an empty-string literal into
// dimExprs only.
func UsageGroupColumns(groupBy []UsageDimension) (dimExprs, groupCols []string) {
	grouped := map[UsageDimension]bool{}
	for _, d := range groupBy {
		if _, ok := UsageDimColumn(d); ok {
			grouped[d] = true
		}
	}
	for _, d := range UsageCanonicalDims {
		col, _ := UsageDimColumn(d)
		if grouped[d] {
			dimExprs = append(dimExprs, col)
			groupCols = append(groupCols, col)
		} else {
			dimExprs = append(dimExprs, "''")
		}
	}
	return dimExprs, groupCols
}

// UsageSQLDialect is what a backend supplies to BuildUsageReportQuery.
type UsageSQLDialect struct {
	// Placeholder renders the n-th (1-based) bound parameter: "$n" or "?".
	Placeholder func(n int) string
	// TimeArg is how a time is bound: a time.Time for Postgres, unix
	// nanoseconds for sqlite.
	TimeArg func(t time.Time) any
	// Bucket is the SQL for the start of the bucket holding the time column
	// col, for a bucket other than none.
	Bucket func(col string, b UsageBucket) string
	// NoPeriod is the period column of an unbucketed report, so the scan
	// target count never changes: a typed NULL or 0.
	NoPeriod string
}

// BuildUsageReportQuery returns the report's SQL and its arguments. The
// result's columns, in order: the UsageCanonicalDims expressions, then
// input, output, cache-creation and cache-read tokens, cost, calls, unpriced
// calls, audio seconds, input characters, the currency, and the period.
//
// Tenant and window are applied inside each leg of the union (so the tables'
// indexes serve them); the dimension filters outside it, on the union.
func BuildUsageReportQuery(q UsageQuery, d UsageSQLDialect) (string, []any, error) {
	var args []any
	ph := func(v any) string { args = append(args, v); return d.Placeholder(len(args)) }

	where := func(tsCol string, from time.Time) string {
		var conds []string
		if q.TenantID != "" {
			conds = append(conds, "tenant_id = "+ph(q.TenantID))
		}
		if !from.IsZero() {
			conds = append(conds, tsCol+" >= "+ph(d.TimeArg(from)))
		}
		if !q.To.IsZero() {
			conds = append(conds, tsCol+" <= "+ph(d.TimeArg(q.To)))
		}
		if len(conds) == 0 {
			return ""
		}
		return " WHERE " + strings.Join(conds, " AND ")
	}

	// The archive keeps no agent: a report grouped or filtered by agent reads
	// the detail window alone rather than show archived days as agent "".
	agentOnly := len(q.Filters[UsageByAgent]) > 0
	for _, g := range q.GroupBy {
		if g == UsageByAgent {
			agentOnly = true
		}
	}

	inner := `SELECT tenant_id, COALESCE(user_id,'') AS user_id, provider, model, credential_source, purpose,
			tier, user_tier, COALESCE(agent_id,'') AS agent_id,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			COALESCE(cost,0) AS cost, COALESCE(cost_currency,'') AS cost_currency,
			1 AS call_count, CASE WHEN cost IS NULL THEN 1 ELSE 0 END AS unpriced_calls,
			audio_seconds, input_chars, ts AS t
		FROM token_usage` + where("ts", q.From)
	if !agentOnly {
		// The archive is day-bucketed (period_start = UTC midnight), so an
		// intra-day `from` compared exactly would drop the whole from-day bucket.
		// Floor it to its UTC day: over-inclusive on the boundary day, never
		// under-inclusive.
		from := q.From
		if !from.IsZero() {
			from = from.UTC().Truncate(24 * time.Hour)
		}
		inner += `
		UNION ALL
		SELECT tenant_id, user_id, provider, model, credential_source, purpose,
			tier, user_tier, '' AS agent_id,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost, cost_currency, call_count, unpriced_calls, audio_seconds, input_chars, period_start AS t
		FROM usage_archive` + where("period_start", from)
	}

	for dim := range q.Filters {
		if _, ok := UsageDimColumn(dim); !ok {
			return "", nil, fmt.Errorf("unknown filter dimension %q (allowed: %s)", dim, UsageDimNames())
		}
	}

	var filters []string
	for _, dim := range UsageCanonicalDims {
		vals := q.Filters[dim]
		if len(vals) == 0 {
			continue
		}
		col, _ := UsageDimColumn(dim)
		if dim == UsageByClass {
			// Filter on the stored purpose: the class is derived from it.
			col = "purpose"
			var ps []string
			for _, c := range vals {
				if c != UsageClassForeground && c != UsageClassBackground {
					return "", nil, fmt.Errorf("unknown class %q (allowed: %s, %s)", c, UsageClassForeground, UsageClassBackground)
				}
				ps = append(ps, purposesOfClass(c)...)
			}
			vals = ps
		}
		phs := make([]string, len(vals))
		for i, v := range vals {
			phs[i] = ph(v)
		}
		filters = append(filters, col+" IN ("+strings.Join(phs, ", ")+")")
	}

	dimExprs, groupCols := UsageGroupColumns(q.GroupBy)
	period := d.NoPeriod
	if q.Bucket != UsageBucketNone {
		period = d.Bucket("t", q.Bucket)
	}
	query := `SELECT ` + strings.Join(dimExprs, ", ") + `,
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(cost),0), COALESCE(SUM(call_count),0), COALESCE(SUM(unpriced_calls),0),
		COALESCE(SUM(audio_seconds),0), COALESCE(SUM(input_chars),0), cost_currency, ` + period + `
		FROM (` + inner + `) u`
	if len(filters) > 0 {
		query += " WHERE " + strings.Join(filters, " AND ")
	}
	// cost_currency is ALWAYS grouped, so a row never sums across currencies;
	// unpriced rows (currency '') group together.
	groupCols = append(groupCols, "cost_currency")
	order := "COALESCE(SUM(cost),0) DESC"
	if q.Bucket != UsageBucketNone {
		groupCols = append(groupCols, period)
		order = period + " ASC, " + order
	}
	query += " GROUP BY " + strings.Join(groupCols, ", ") + " ORDER BY " + order
	return query, args, nil
}

// usageDimField returns the field of a holding dimension d.
func usageDimField(a *UsageAggregate, d UsageDimension) *string {
	switch d {
	case UsageByTenant:
		return &a.TenantID
	case UsageByUser:
		return &a.UserID
	case UsageByProvider:
		return &a.Provider
	case UsageByModel:
		return &a.Model
	case UsageBySource:
		return &a.CredentialSource
	case UsageByPurpose:
		return &a.Purpose
	case UsageByTier:
		return &a.Tier
	case UsageByUserTier:
		return &a.UserTier
	case UsageByAgent:
		return &a.AgentID
	case UsageByClass:
		return &a.Class
	}
	return nil
}

// FinishUsageReport applies the query's Top to the scanned rows: the Top
// largest values of GroupBy[0] over the whole window (by TopBy) are kept, and
// every other value's rows are summed into rows marked Other, one per
// remaining key (the other dimensions, the period and the currency). Both
// backends call it, so the fold is the same everywhere.
func FinishUsageReport(rows []UsageAggregate, q UsageQuery) []UsageAggregate {
	if q.Top <= 0 || len(q.GroupBy) == 0 {
		return rows
	}
	dim := q.GroupBy[0]
	weight := func(a UsageAggregate) float64 {
		switch q.TopBy {
		case UsageTopByTokens:
			return float64(a.InputTokens + a.OutputTokens + a.CacheCreationTokens + a.CacheReadTokens)
		case UsageTopByCalls:
			return float64(a.CallCount)
		}
		return a.Cost
	}
	totals := map[string]float64{}
	for i := range rows {
		totals[*usageDimField(&rows[i], dim)] += weight(rows[i])
	}
	if len(totals) <= q.Top {
		return rows
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	kept := map[string]bool{}
	for _, k := range keys[:q.Top] {
		kept[k] = true
	}

	var out []UsageAggregate
	others := map[string]int{} // fold key → index in out
	for _, a := range rows {
		if kept[*usageDimField(&a, dim)] {
			out = append(out, a)
			continue
		}
		*usageDimField(&a, dim) = ""
		a.Other = true
		k := usageFoldKey(a)
		if i, ok := others[k]; ok {
			o := &out[i]
			o.InputTokens += a.InputTokens
			o.OutputTokens += a.OutputTokens
			o.CacheCreationTokens += a.CacheCreationTokens
			o.CacheReadTokens += a.CacheReadTokens
			o.Cost += a.Cost
			o.CallCount += a.CallCount
			o.UnpricedCalls += a.UnpricedCalls
			o.AudioSeconds += a.AudioSeconds
			o.InputChars += a.InputChars
			continue
		}
		others[k] = len(out)
		out = append(out, a)
	}
	// Each period's rows by cost, the folded row last: the same order the query
	// returns, with "other" never ranked above a value it outweighs by sum.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Period.Equal(out[j].Period) {
			return out[i].Period.Before(out[j].Period)
		}
		if out[i].Other != out[j].Other {
			return !out[i].Other
		}
		return out[i].Cost > out[j].Cost
	})
	return out
}

// usageFoldKey is a row's identity apart from its sums.
func usageFoldKey(a UsageAggregate) string {
	var b strings.Builder
	for _, d := range UsageCanonicalDims {
		b.WriteString(*usageDimField(&a, d))
		b.WriteByte(0)
	}
	b.WriteString(a.Currency)
	b.WriteByte(0)
	b.WriteString(a.Period.UTC().Format(time.RFC3339Nano))
	return b.String()
}

// UsageMaxTop bounds UsageQuery.Top: a chart shows a handful of series, and a
// larger fold is a table the caller can get by leaving Top unset.
const UsageMaxTop = 50

// UsageParams is a usage report request as a transport receives it, before
// validation: the HTTP query string and the gRPC request both map onto it, so
// the two refuse the same inputs with the same words.
type UsageParams struct {
	GroupBy  []string
	From, To string // RFC 3339; "" = unbounded
	Bucket   string
	Top      int
	TopBy    string
	Filters  map[string][]string
}

// ParseUsageQuery validates p into a UsageQuery. detailSince is the oldest
// instant the per-call ledger still holds (zero when nothing has been folded
// into the archive): hour buckets are refused for a window reaching before
// it, since the archive holds whole days.
func ParseUsageQuery(p UsageParams, detailSince time.Time) (UsageQuery, error) {
	var q UsageQuery
	for _, g := range p.GroupBy {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if _, ok := UsageDimColumn(UsageDimension(g)); !ok {
			return q, fmt.Errorf("unknown group_by dimension: %s (allowed: %s)", g, UsageDimNames())
		}
		q.GroupBy = append(q.GroupBy, UsageDimension(g))
	}
	for name, v := range map[string]string{"from": p.From, "to": p.To} {
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, fmt.Errorf("%s must be RFC3339: %v", name, err)
		}
		if name == "from" {
			q.From = t
		} else {
			q.To = t
		}
	}
	b, err := ParseUsageBucket(p.Bucket)
	if err != nil {
		return q, err
	}
	q.Bucket = b
	if b == UsageBucketHour && !detailSince.IsZero() && (q.From.IsZero() || q.From.Before(detailSince)) {
		return q, fmt.Errorf("hour buckets reach only the per-call detail, which starts at %s; set from on or after it, or use day buckets",
			detailSince.UTC().Format(time.RFC3339))
	}
	if p.Top < 0 || p.Top > UsageMaxTop {
		return q, fmt.Errorf("top must be between 0 and %d", UsageMaxTop)
	}
	if p.Top > 0 && len(q.GroupBy) == 0 {
		return q, fmt.Errorf("top needs a group_by: it keeps the largest values of the first dimension")
	}
	q.Top = p.Top
	switch p.TopBy {
	case "", UsageTopByCost, UsageTopByTokens, UsageTopByCalls:
		q.TopBy = p.TopBy
	default:
		return q, fmt.Errorf("unknown top_by %q (allowed: %s, %s, %s)", p.TopBy, UsageTopByCost, UsageTopByTokens, UsageTopByCalls)
	}
	for k, vals := range p.Filters {
		d := UsageDimension(k)
		if _, ok := UsageDimColumn(d); !ok {
			return q, fmt.Errorf("unknown filter dimension: %s (allowed: %s)", k, UsageDimNames())
		}
		var clean []string
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" {
				clean = append(clean, v)
			}
		}
		if len(clean) == 0 {
			continue
		}
		if q.Filters == nil {
			q.Filters = map[UsageDimension][]string{}
		}
		q.Filters[d] = append(q.Filters[d], clean...)
	}
	if cs := q.Filters[UsageByClass]; len(cs) > 0 {
		for _, c := range cs {
			if c != UsageClassForeground && c != UsageClassBackground {
				return q, fmt.Errorf("unknown class %q (allowed: %s, %s)", c, UsageClassForeground, UsageClassBackground)
			}
		}
	}
	return q, nil
}

// UsageDetailSince is the oldest instant the per-call ledger still holds when
// the sweeper folds rows older than retention into the archive (0 = its 30-day
// default); zero when the sweeper is off, since nothing is then folded.
func UsageDetailSince(now time.Time, retention time.Duration, sweeperEnabled bool) time.Time {
	if !sweeperEnabled {
		return time.Time{}
	}
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	return now.Add(-retention).UTC()
}
