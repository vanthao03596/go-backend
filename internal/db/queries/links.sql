-- name: CreateLink :one
INSERT INTO links (code, target_url, api_key_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetLinkByCode :one
SELECT * FROM links
WHERE code = $1 AND deleted_at IS NULL;

-- name: GetLinkByCodeForAPIKey :one
SELECT * FROM links
WHERE code = $1 AND api_key_id = $2 AND deleted_at IS NULL;

-- name: ListLinksByAPIKey :many
-- Cursor pagination: pass the (created_at, id) of the last row seen, or
-- a far-future timestamp and max bigint for the first page.
SELECT * FROM links
WHERE api_key_id = sqlc.arg(api_key_id)::bigint
  AND deleted_at IS NULL
  AND (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::bigint)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size)::int;

-- name: SoftDeleteLink :execrows
UPDATE links SET deleted_at = now()
WHERE code = $1 AND api_key_id = $2 AND deleted_at IS NULL;

-- name: IncrementClick :exec
UPDATE links SET click_count = click_count + sqlc.arg(delta)::bigint
WHERE id = sqlc.arg(link_id)::bigint;
