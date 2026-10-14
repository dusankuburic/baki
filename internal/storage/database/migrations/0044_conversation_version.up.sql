-- baki:compat expand
-- Optimistic version on chat conversations (ADR 0031). `version` names the stored message list: a compare-and-swap
-- save (SaveConversationIf) writes only while the row still has the version its writer read, so two replicas that
-- append to one conversation cannot overwrite each other's turn.
--
-- DEFAULT 0 is a constant, so adding the column is metadata only: no table rewrite, however large `conversations`
-- is. Rows that exist now keep version 0 ("legacy") until their messages change.
--
-- The database assigns every other version, from one sequence shared by all rows, in a trigger on INSERT and on an
-- UPDATE that changes `messages`:
--   * The release before this one never sets the column, yet its blind full-replace saves still bump it, so a
--     compare-and-swap holder of the new release sees them during a rolling deploy.
--   * A value is never reused. A row deleted and created again gets a fresh number from the sequence, so a holder of
--     the old row's version can never match the new row (no ABA after DeleteConversation). Only legacy rows share
--     0, and a created row is never 0.
--   * An UPDATE that leaves `messages` alone (the same list saved again) keeps the version.
-- Gaps are fine (a rolled-back write, or an insert that loses ON CONFLICT, still consumes a number).
CREATE SEQUENCE IF NOT EXISTS conversation_version_seq;

ALTER TABLE conversations ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 0;

CREATE OR REPLACE FUNCTION conversations_bump_version() RETURNS trigger
    LANGUAGE plpgsql AS $fn$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.messages IS DISTINCT FROM OLD.messages THEN
        NEW.version := nextval('conversation_version_seq');
    END IF;
    RETURN NEW;
END $fn$;

DROP TRIGGER IF EXISTS conversations_version ON conversations;
CREATE TRIGGER conversations_version BEFORE INSERT OR UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION conversations_bump_version();
