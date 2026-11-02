-- name: CreateAPIKey :one
INSERT INTO api_keys (key_hash, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetAPIKeyByHash :one
SELECT * FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;
