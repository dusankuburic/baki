package database

import "pad-analyzer/internal/storage/interfaces"

// The notices domain: the scanner's "new rules" notices (migration 0037), keyed by an organisation, a user or the
// deployment rather than by a flow.
func init() {
	registerLifecycle(lifecycleDomain{
		name: "notices",
		tables: []tableClaim{
			// A user's and an organisation's notices go with them through the foreign keys on the generated user and
			// org columns, also under a release that does not know the table.
			{table: "rule_notices", noExport: "notifications derived from analysis",
				erase: erasedByCascade, org: orgByCascade},
		},
		purge: []purgeStep{
			// The retention window, as for gov_alerts, counted from the last sweep that touched the notice: one the
			// scanner still raises stays, since a purged notice would be inserted and announced again. Batched: a row per
			// scope and rule set, and the first sweep after a window is set meets the whole backlog.
			{table: "rule_notices", cond: `updated_at < NOW() - ($1 * INTERVAL '1 day')`, batched: true, windowed: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.RuleNotices }},
		},
	})
}
