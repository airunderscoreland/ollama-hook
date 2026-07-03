DROP INDEX IF EXISTS idx_conversations_source_type;
ALTER TABLE conversations DROP COLUMN IF EXISTS metadata;
ALTER TABLE conversations DROP COLUMN IF EXISTS source_type;
