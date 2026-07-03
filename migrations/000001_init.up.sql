CREATE TABLE conversations (
    id            UUID PRIMARY KEY,
    model_name    VARCHAR(255) NOT NULL,
    endpoint      VARCHAR(255) NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ,
    user_prompt   TEXT,
    total_tokens  INTEGER,
    duration_ms   INTEGER,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE messages (
    id               UUID PRIMARY KEY,
    conversation_id  UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role             VARCHAR(20) NOT NULL,
    content          TEXT NOT NULL,
    thinking         TEXT,
    timestamp        TIMESTAMPTZ NOT NULL,
    token_count      INTEGER,
    response_time_ms INTEGER,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_conversations_started_at ON conversations(started_at);
CREATE INDEX idx_conversations_model      ON conversations(model_name);
CREATE INDEX idx_messages_conversation_id ON messages(conversation_id);
CREATE INDEX idx_messages_timestamp       ON messages(timestamp);
