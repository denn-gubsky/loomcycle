package store

import (
	"reflect"
	"strings"
	"testing"
)

// TestUsageCanonicalDims_EveryDimensionMapsToAColumnAndIsListed holds the
// report's dimension list together: every canonical dimension passes the
// whitelist, the refusal text names exactly them, and the SELECT keeps one
// expression per dimension in canonical order whatever is grouped — the scan
// targets in both backends depend on that order.
func TestUsageCanonicalDims_EveryDimensionMapsToAColumnAndIsListed(t *testing.T) {
	var cols []string
	for _, d := range UsageCanonicalDims {
		col, ok := UsageDimColumn(d)
		if !ok {
			t.Fatalf("canonical dimension %q is not whitelisted", d)
		}
		cols = append(cols, col)
	}
	if got := strings.Split(UsageDimNames(), ","); len(got) != len(UsageCanonicalDims) {
		t.Errorf("UsageDimNames() = %q, want one name per canonical dimension", UsageDimNames())
	}
	if !strings.Contains(UsageDimNames(), "purpose") {
		t.Errorf("UsageDimNames() = %q, want purpose listed", UsageDimNames())
	}

	exprs, group := UsageGroupColumns(nil)
	if len(exprs) != len(UsageCanonicalDims) || len(group) != 0 {
		t.Errorf("ungrouped = (%v, %v), want %d literal expressions and no GROUP BY", exprs, group, len(UsageCanonicalDims))
	}
	exprs, group = UsageGroupColumns([]UsageDimension{UsageByPurpose, UsageByTenant})
	wantExprs := []string{"tenant_id", "''", "''", "''", "''", "purpose"}
	if !reflect.DeepEqual(exprs, wantExprs) || !reflect.DeepEqual(group, []string{"tenant_id", "purpose"}) {
		t.Errorf("grouped by purpose,tenant = (%v, %v), want (%v, [tenant_id purpose]) in canonical order", exprs, group, wantExprs)
	}
	exprs, _ = UsageGroupColumns(UsageCanonicalDims)
	if !reflect.DeepEqual(exprs, cols) {
		t.Errorf("all grouped = %v, want %v", exprs, cols)
	}
	if _, ok := UsageDimColumn("purpose; DROP TABLE token_usage"); ok {
		t.Error("an arbitrary dimension passed the whitelist")
	}
}
