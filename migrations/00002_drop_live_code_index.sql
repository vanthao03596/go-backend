-- +goose Up
-- The redirect lookup reads deleted rows too (to answer 410), so the partial
-- index on live codes no longer matches its predicate. The unique constraint
-- index on code serves every lookup.
DROP INDEX IF EXISTS links_code_live_idx;

-- +goose Down
CREATE INDEX links_code_live_idx ON links (code) WHERE deleted_at IS NULL;
