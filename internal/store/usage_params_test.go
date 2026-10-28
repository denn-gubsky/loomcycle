package store

import (
	"strings"
	"testing"
	"time"
)

func TestParseUsageQuery_ValidatesEveryParameter(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	q, err := ParseUsageQuery(UsageParams{
		GroupBy: []string{"model", " tier "}, From: "2026-10-05T00:00:00Z", Bucket: "hour", Top: 6, TopBy: "tokens",
		Filters: map[string][]string{"class": {"background"}, "user": {" ", "u1"}},
	}, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.GroupBy) != 2 || q.GroupBy[1] != UsageByTier || q.Bucket != UsageBucketHour || q.Top != 6 || q.TopBy != "tokens" ||
		len(q.Filters[UsageByUser]) != 1 || q.Filters[UsageByUser][0] != "u1" {
		t.Errorf("parsed = %+v", q)
	}
	for name, p := range map[string]UsageParams{
		"unknown group_by":         {GroupBy: []string{"tenant_id"}},
		"bad from":                 {From: "yesterday"},
		"unknown bucket":           {Bucket: "fortnight"},
		"hour before the detail":   {Bucket: "hour", From: "2026-09-01T00:00:00Z"},
		"hour with no from":        {Bucket: "hour"},
		"top without group_by":     {Top: 3},
		"top too large":            {GroupBy: []string{"model"}, Top: UsageMaxTop + 1},
		"negative top":             {GroupBy: []string{"model"}, Top: -1},
		"unknown top_by":           {GroupBy: []string{"model"}, Top: 3, TopBy: "vibes"},
		"unknown filter dimension": {Filters: map[string][]string{"model; DROP TABLE token_usage": {"x"}}},
		"unknown class":            {Filters: map[string][]string{"class": {"sideways"}}},
	} {
		if _, err := ParseUsageQuery(p, since); err == nil {
			t.Errorf("%s: accepted, want refused", name)
		}
	}
	// With the sweeper off nothing is archived, so hours reach everywhere.
	if _, err := ParseUsageQuery(UsageParams{Bucket: "hour"}, time.Time{}); err != nil {
		t.Errorf("hour with no archive: %v", err)
	}
	if _, err := ParseUsageQuery(UsageParams{Bucket: "hour", From: "2026-09-01T00:00:00Z"}, since); err == nil || !strings.Contains(err.Error(), "2026-10-01T00:00:00Z") {
		t.Errorf("hour refusal = %v, want it to name where the detail starts", err)
	}
}

func TestUsageDetailSince(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if got := UsageDetailSince(now, 0, true); !got.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Errorf("default retention = %v", got)
	}
	if got := UsageDetailSince(now, 48*time.Hour, true); !got.Equal(now.Add(-48 * time.Hour)) {
		t.Errorf("48h retention = %v", got)
	}
	if got := UsageDetailSince(now, 48*time.Hour, false); !got.IsZero() {
		t.Errorf("sweeper off = %v, want zero", got)
	}
}

func TestFinishUsageReport_FoldsByTopByAcrossPeriods(t *testing.T) {
	d1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	d2 := d1.Add(24 * time.Hour)
	rows := []UsageAggregate{
		{Model: "a", Period: d1, CallCount: 10, Cost: 1},
		{Model: "b", Period: d1, CallCount: 1, Cost: 9},
		{Model: "c", Period: d1, CallCount: 2, Cost: 1},
		{Model: "c", Period: d2, CallCount: 3, Cost: 1},
		{Model: "a", Period: d2, CallCount: 1, Cost: 1},
	}
	got := FinishUsageReport(rows, UsageQuery{GroupBy: []UsageDimension{UsageByModel}, Top: 1, TopBy: UsageTopByCalls})
	// Calls: a=11, c=5, b=1 → keep a; b and c fold per period.
	want := []UsageAggregate{
		{Model: "a", Period: d1, CallCount: 10, Cost: 1},
		{Period: d1, CallCount: 3, Cost: 10, Other: true},
		{Model: "a", Period: d2, CallCount: 1, Cost: 1},
		{Period: d2, CallCount: 3, Cost: 1, Other: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Model != want[i].Model || !got[i].Period.Equal(want[i].Period) || got[i].CallCount != want[i].CallCount ||
			got[i].Cost != want[i].Cost || got[i].Other != want[i].Other {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// No fold when there are no more values than Top.
	if got := FinishUsageReport(rows, UsageQuery{GroupBy: []UsageDimension{UsageByModel}, Top: 3}); len(got) != len(rows) {
		t.Errorf("top 3 of 3 values changed the rows: %+v", got)
	}
}

func TestSummarizeCallTimings_PercentilesPerModel(t *testing.T) {
	var rows []TokenUsageRow
	for i := 1; i <= 20; i++ {
		rows = append(rows, TokenUsageRow{Provider: "p", Model: "a", DurationMs: int64(i * 100), TTFTMs: int64(i * 10),
			OutputTokens: 100, DecodeMs: 1000})
	}
	rows = append(rows, TokenUsageRow{Provider: "p", Model: "b", DurationMs: 50})
	got := SummarizeCallTimings(rows)
	if len(got) != 2 || got[0].Model != "a" || got[0].Calls != 20 {
		t.Fatalf("summary = %+v, want a (20 calls) first", got)
	}
	a := got[0]
	if a.DurationP50Ms != 1000 || a.DurationP95Ms != 1900 || a.TTFTP50Ms != 100 || a.OutputTokensPS != 100 {
		t.Errorf("a = %+v, want p50 1000, p95 1900, ttft 100, 100 tok/s", a)
	}
	if b := got[1]; b.DurationP50Ms != 50 || b.TTFTP50Ms != 0 || b.OutputTokensPS != 0 {
		t.Errorf("b = %+v, want only a duration", b)
	}
}
