-- Reverses v48: the constraints and indexes go, the v47 token index comes back. The rows the step deleted stay
-- deleted, and the policies it gave an id keep it; the release before reads both as it always did. The five indexes
-- SchemaUpkeep builds after boot on a v48 schema go too, so v47 is v47 again (a v48 boot builds them again).
ALTER TABLE share_tokens ALTER COLUMN created_by DROP NOT NULL, ALTER COLUMN created_by DROP DEFAULT;
ALTER TABLE policies ALTER COLUMN description DROP NOT NULL;
CREATE INDEX IF NOT EXISTS org_invites_token_hash_idx ON org_invites (token_hash);
DROP INDEX IF EXISTS org_invites_token_hash_key;
DROP INDEX IF EXISTS finding_status_assignee_idx;
DROP INDEX IF EXISTS finding_status_updated_by_idx;
DROP INDEX IF EXISTS finding_comments_author_idx;
DROP INDEX IF EXISTS flow_versions_created_by_idx;
DROP INDEX IF EXISTS gov_alerts_dismissed_flow_idx;
ALTER TABLE refresh_tokens      DROP CONSTRAINT IF EXISTS refresh_tokens_user_id_fkey;
ALTER TABLE org_custom_rules    DROP CONSTRAINT IF EXISTS org_custom_rules_org_id_fkey;
ALTER TABLE org_channels        DROP CONSTRAINT IF EXISTS org_channels_org_id_fkey;
ALTER TABLE policies            DROP CONSTRAINT IF EXISTS policies_org_id_fkey;
ALTER TABLE knowledge_chunks    DROP CONSTRAINT IF EXISTS knowledge_chunks_doc_id_org_id_fkey;
ALTER TABLE knowledge_documents DROP CONSTRAINT IF EXISTS knowledge_documents_org_id_fkey;
DROP INDEX IF EXISTS knowledge_documents_id_org_id_key;
DROP INDEX IF EXISTS users_lower_email_key;
