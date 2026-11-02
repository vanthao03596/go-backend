-- +goose Up
-- +goose StatementBegin
CREATE TABLE api_keys (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_hash    BYTEA       NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE links (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        TEXT        NOT NULL,
    target_url  TEXT        NOT NULL,
    api_key_id  BIGINT      NOT NULL REFERENCES api_keys (id),
    click_count BIGINT      NOT NULL DEFAULT 0,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    CONSTRAINT links_code_unique UNIQUE (code),
    CONSTRAINT links_code_format CHECK (code ~ '^[A-Za-z0-9_-]{3,32}$'),
    CONSTRAINT links_target_url_len CHECK (char_length(target_url) <= 2048)
);

-- Redirect hot path: lookup by code for live links only.
CREATE INDEX links_code_live_idx ON links (code) WHERE deleted_at IS NULL;

-- Listing a caller's links, newest first, with (created_at, id) as the cursor.
CREATE INDEX links_api_key_created_idx ON links (api_key_id, created_at DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE links;
DROP TABLE api_keys;
-- +goose StatementEnd
