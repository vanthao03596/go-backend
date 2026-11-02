-- +goose Up
-- +goose StatementBegin
-- One row per counted redirect. Written in batches by the in-process click
-- buffer; links.click_count stays the authoritative total and is bumped in
-- the same transaction. No client IP is stored.
CREATE TABLE clicks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    link_id     BIGINT      NOT NULL REFERENCES links (id),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    referrer    TEXT,
    user_agent  TEXT,
    CONSTRAINT clicks_referrer_len   CHECK (char_length(referrer) <= 2048),
    CONSTRAINT clicks_user_agent_len CHECK (char_length(user_agent) <= 512)
);

-- Per-link daily stats: range scan on (link_id, occurred_at).
CREATE INDEX clicks_link_occurred_idx ON clicks (link_id, occurred_at);

-- Retention purge: oldest rows first.
CREATE INDEX clicks_occurred_idx ON clicks (occurred_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE clicks;
-- +goose StatementEnd
