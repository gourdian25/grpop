-- File: internal/postgresdb/schema.sql

CREATE TABLE IF NOT EXISTS grpop_dlq (
    send_id          VARCHAR(255) PRIMARY KEY,
    channel          VARCHAR(16)  NOT NULL,
    message_data     JSONB        NOT NULL,
    failure_reason   TEXT         NOT NULL DEFAULT '',
    retry_count      INT          NOT NULL DEFAULT 0,
    max_retries      INT          NOT NULL,
    first_failure_at TIMESTAMPTZ  NOT NULL,
    last_attempt_at  TIMESTAMPTZ  NOT NULL,
    next_retry_at    TIMESTAMPTZ  NOT NULL,
    expires_at       TIMESTAMPTZ  NOT NULL,
    status           VARCHAR(32)  NOT NULL,
    attempt_history  JSONB        NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ  NOT NULL,
    updated_at       TIMESTAMPTZ  NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_next_retry ON grpop_dlq (status, next_retry_at);
CREATE INDEX IF NOT EXISTS idx_grpop_dlq_channel_status ON grpop_dlq (channel, status);
CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_expires_at ON grpop_dlq (status, expires_at);
