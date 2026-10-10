package database

import "pad-analyzer/internal/storage/interfaces"

// ciDeliveryRetention is how long the CI webhook's replay record keeps a delivery id, an SQL interval. A signed call is
// fresh for hmacsig.MaxSkew either side of the server's clock, so a replay can come up to ten minutes after the first
// call; the rest is margin for skew between replicas.
const ciDeliveryRetention = `INTERVAL '1 hour'`

// The CI webhook domain: the replay record of POST /api/integrations/ci (migration 0039). The organisations' CI
// credentials themselves are the organisations domain's.
func init() {
	registerLifecycle(lifecycleDomain{
		name: "ci webhook",
		tables: []tableClaim{
			// Rows name a key (an org credential's or an API token's id, or the deployment secret) and a random id the
			// runner chose, and live about an hour: those of a revoked key, a deleted organisation or an erased
			// account can no longer authenticate anything and go with the next sweep.
			{table: "ci_webhook_deliveries", noExport: "the CI webhook's replay record: key ids and random delivery ids",
				erase: notPersonal, eraseWhy: "a credential's or API token's id and a random delivery id, kept about an hour",
				org: orgNotKeyed},
		},
		purge: []purgeStep{
			// Each insert also removes its own caller's old records (RecordCIDelivery); this catches the keys that
			// stopped calling. Batched: one row per CI call, and the sweep runs daily by default.
			{table: "ci_webhook_deliveries", cond: `received_at < NOW() - ` + ciDeliveryRetention, batched: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.CIDeliveries }},
		},
	})
}
