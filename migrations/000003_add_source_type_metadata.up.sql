ALTER TABLE conversations ADD COLUMN IF NOT EXISTS source_type VARCHAR(50) NOT NULL DEFAULT 'proxy';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS metadata JSONB;
CREATE INDEX IF NOT EXISTS idx_conversations_source_type ON conversations(source_type);
