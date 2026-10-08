-- Reverses v34: both tables are back to no row-level security. Nothing is lost; id-only reads and writes inside a
-- request reach every tenant's rows again, as before v34.
DROP POLICY IF EXISTS rls_share_tokens_visible ON share_tokens;
ALTER TABLE share_tokens NO FORCE ROW LEVEL SECURITY;
ALTER TABLE share_tokens DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_flow_analysis_visible ON flow_analysis;
ALTER TABLE flow_analysis NO FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_analysis DISABLE ROW LEVEL SECURITY;
