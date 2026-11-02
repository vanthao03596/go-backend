-- name: InsertClicks :copyfrom
INSERT INTO clicks (link_id, occurred_at, referrer, user_agent)
VALUES ($1, $2, $3, $4);

-- name: AddClickCounts :exec
-- Applies one batch of per-link deltas. link_ids and deltas are parallel
-- arrays; a link id must appear at most once per call.
UPDATE links AS l
SET click_count = l.click_count + c.n
FROM (
  SELECT unnest(sqlc.arg(link_ids)::bigint[]) AS link_id,
         unnest(sqlc.arg(deltas)::bigint[])   AS n
) AS c
WHERE l.id = c.link_id;

-- name: CountClicksByDay :many
-- Clicks per UTC calendar day in [since, until). Days without clicks are
-- absent; the caller zero-fills.
SELECT (occurred_at AT TIME ZONE 'UTC')::date AS day, count(*)::bigint AS clicks
FROM clicks
WHERE link_id = sqlc.arg(link_id)::bigint
  AND occurred_at >= sqlc.arg(since)::timestamptz
  AND occurred_at <  sqlc.arg(until)::timestamptz
GROUP BY 1
ORDER BY 1;

-- name: PurgeClicksBefore :execrows
-- Deletes at most batch_size rows older than before, oldest first, so a
-- large backlog is removed in short transactions rather than one long lock.
DELETE FROM clicks
WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM clicks
  WHERE occurred_at < sqlc.arg(before)::timestamptz
  ORDER BY occurred_at
  LIMIT sqlc.arg(batch_size)::int
));
