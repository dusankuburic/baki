-- Reverses v44. The trigger, its function, the column and the sequence go; the release before never reads the
-- column, and its blind saves go on working as they did.
DROP TRIGGER IF EXISTS conversations_version ON conversations;
DROP FUNCTION IF EXISTS conversations_bump_version();
ALTER TABLE conversations DROP COLUMN IF EXISTS version;
DROP SEQUENCE IF EXISTS conversation_version_seq;
