package store

import "context"

// RunsByUserPager is the slice of Store CollectRunsByUser reads.
type RunsByUserPager interface {
	ListRunsByUser(ctx context.Context, tenantID, userID string, statuses []RunStatus, limit int, cursor string) ([]Run, string, error)
}

// CollectRunsScanCap is CollectRunsByUser's default bound on rows read.
const CollectRunsScanCap = 1000

// collectRunsPage is how many rows one store read of the scan asks for.
const collectRunsPage = 100

// CollectRunsByUser returns up to max of userID's runs that keep accepts,
// newest first, paging ListRunsByUser past the rows keep drops — so a filter
// the query cannot express still yields a full answer rather than whatever
// survived of the first page. It reads at most scanCap rows (<= 0 →
// CollectRunsScanCap). statuses and tenantID read as in ListRunsByUser.
//
// truncated reports that the scan stopped with rows it never judged: max was
// reached with more left, or the scan cap was. A scan that judged every row
// is not truncated.
func CollectRunsByUser(ctx context.Context, st RunsByUserPager, tenantID, userID string, statuses []RunStatus, keep func(Run) bool, scanCap, max int) (runs []Run, truncated bool, err error) {
	if scanCap <= 0 {
		scanCap = CollectRunsScanCap
	}
	scanned := 0
	cursor := ""
	for {
		page, next, err := st.ListRunsByUser(ctx, tenantID, userID, statuses, min(collectRunsPage, scanCap-scanned), cursor)
		if err != nil {
			return nil, false, err
		}
		scanned += len(page)
		for i, r := range page {
			if !keep(r) {
				continue
			}
			runs = append(runs, r)
			if max > 0 && len(runs) >= max {
				return runs, i < len(page)-1 || next != "", nil
			}
		}
		switch {
		case next == "":
			return runs, false, nil
		case scanned >= scanCap:
			return runs, true, nil
		}
		cursor = next
	}
}
