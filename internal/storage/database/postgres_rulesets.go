package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"pad-core/logger"
	"pad-core/models"
)

// Rule sets recorded on baselines and health snapshots (migration 0037, ADR 0016). Each row carries the whole set as
// JSONB next to its digest; a set counts only while rule_set_at equals the row's created_at (analyzed_at).

// encodeRuleSet returns the rule_set and rule_set_digest arguments of rs: NULL, NULL for none.
func encodeRuleSet(rs *models.RuleSet) (setJSON, digest any, err error) {
	if rs == nil {
		return nil, nil, nil
	}
	data, err := json.Marshal(rs)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal rule set: %w", err)
	}
	return string(data), rs.Digest, nil
}

// decodeRuleSet reads a row's rule set and digest. An undecodable set reads as unknown, both empty: the record is
// legacy rather than unreadable, which keeps its findings drift.
func decodeRuleSet(setJSON []byte, digest sql.NullString) (*models.RuleSet, string) {
	if len(setJSON) == 0 || !digest.Valid {
		return nil, ""
	}
	var rs models.RuleSet
	if err := json.Unmarshal(setJSON, &rs); err != nil {
		logger.Warn("rule set: undecodable record read as unknown", "digest", digest.String, "err", err)
		return nil, ""
	}
	return &rs, digest.String
}

// ruleSetsByDigestSQL takes, per digest, one baseline that still carries it, else one health snapshot: an index probe
// each (flow_*_rule_set_digest_idx), however many flows share the set.
const ruleSetsByDigestSQL = `
	SELECT d.digest, COALESCE(bl.rule_set, fa.rule_set)
	  FROM unnest($1::text[]) AS d(digest)
	  LEFT JOIN LATERAL (SELECT rule_set FROM flow_baselines
	                      WHERE rule_set_digest = d.digest AND rule_set_at = created_at LIMIT 1) bl ON true
	  LEFT JOIN LATERAL (SELECT rule_set FROM flow_analysis
	                      WHERE rule_set_digest = d.digest AND rule_set_at = analyzed_at LIMIT 1) fa ON bl.rule_set IS NULL
	 WHERE COALESCE(bl.rule_set, fa.rule_set) IS NOT NULL`

// RuleSetsByDigest resolves recorded rule sets by digest (a system read: the scanner runs on the pool).
func (b *PostgresStorageBackend) RuleSetsByDigest(ctx context.Context, digests []string) (map[string]*models.RuleSet, error) {
	out := make(map[string]*models.RuleSet, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, ruleSetsByDigestSQL, textArray(digests))
	if err != nil {
		return nil, fmt.Errorf("load rule sets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var digest string
		var setJSON []byte
		if err := rows.Scan(&digest, &setJSON); err != nil {
			return nil, fmt.Errorf("scan rule sets: %w", err)
		}
		if rs, _ := decodeRuleSet(setJSON, sql.NullString{String: digest, Valid: true}); rs != nil {
			out[digest] = rs
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load rule sets: %w", err)
	}
	return out, nil
}
