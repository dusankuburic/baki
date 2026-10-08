-- baki:compat expand
-- flow_analysis and share_tokens get the policy of the other flow-child tables (v27): a row is reachable inside a
-- user's RLS scope only when its flow is, so an id-only read or write in a request can no longer reach another
-- tenant's analysis summary or share links (ADR 0013). Without a user context the policies allow every row, as all
-- of them do: the public /api/shared token lookup, the governance scanner, the dashboard record (written outside
-- the request transaction) and the retention sweep keep working. share_tokens shipped without row-level security on
-- the belief that the anonymous lookup needs it off; it does not.
ALTER TABLE flow_analysis ENABLE ROW LEVEL SECURITY;
ALTER TABLE flow_analysis FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_flow_analysis_visible ON flow_analysis;
CREATE POLICY rls_flow_analysis_visible ON flow_analysis FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_analysis.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_analysis.flow_id));

ALTER TABLE share_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE share_tokens FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_share_tokens_visible ON share_tokens;
CREATE POLICY rls_share_tokens_visible ON share_tokens FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = share_tokens.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = share_tokens.flow_id));
