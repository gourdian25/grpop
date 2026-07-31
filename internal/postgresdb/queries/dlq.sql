-- File: internal/postgresdb/queries/dlq.sql

-- name: UpsertDLQEvent :exec
-- A single atomic upsert handles both "new failure" (insert) and "another
-- failure for a send_id already pending/retrying" (update, appending onto
-- the existing history) in one write path. The attempt_history assignment
-- caps the concatenated array to at most $11 entries, keeping the most
-- recent ones (oldest dropped first) — see the identical cap applied in
-- FinalizeRetryPending/FinalizeRetryTerminal below; all three share this
-- exact subquery shape so a pathological repeatedly-failing send_id can't
-- grow attempt_history without bound.
INSERT INTO grpop_dlq (send_id, channel, message_data, failure_reason, retry_count, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, attempt_history, created_at, updated_at)
VALUES ($1, $2, $3, $4, 0, $5, $6, $6, $7, $8, $9, $10, $6, $6)
ON CONFLICT (send_id) DO UPDATE SET
    message_data = EXCLUDED.message_data,
    failure_reason = EXCLUDED.failure_reason,
    last_attempt_at = EXCLUDED.last_attempt_at,
    updated_at = EXCLUDED.updated_at,
    attempt_history = (
        SELECT COALESCE(jsonb_agg(elem ORDER BY ord), '[]'::jsonb)
        FROM (
            SELECT elem, ord
            FROM jsonb_array_elements(grpop_dlq.attempt_history || EXCLUDED.attempt_history) WITH ORDINALITY AS t(elem, ord)
            ORDER BY ord DESC
            LIMIT $11::int
        ) capped
    );

-- name: ExpirePastDeadlineEvents :exec
-- Step 1 of ClaimRetryableEvents: proactively transition any Pending row
-- whose deadline has already passed to 'expired', regardless of whether
-- this call's own claim step (below) would otherwise reach it. Plain
-- UPDATE, no FOR UPDATE SKIP LOCKED — multiple replicas running this
-- concurrently is redundant but harmless (each row transitions once; a
-- second attempt on an already-'expired' row matches zero rows), so no
-- cross-replica coordination is needed here the way it is for the claim
-- step below.
UPDATE grpop_dlq
SET status = 'expired', updated_at = $2
WHERE status = 'pending' AND expires_at <= $1;

-- name: ClaimRetryableEvents :many
-- Step 2 of ClaimRetryableEvents: single-statement atomic claim, extended
-- with "AND (expires_at IS NULL OR expires_at > $1)" so a row can never be
-- claimed for retry past its own deadline — this predicate is what
-- actually enforces the cutoff; step 1 above only exists so an event
-- nobody claims in time still surfaces as 'expired' instead of sitting
-- silently in 'pending' forever. A NULL expires_at ("no deadline") must be
-- explicitly OR'd in — plain SQL comparison (NULL > $1) evaluates to NULL,
-- not true, so it would otherwise silently exclude every no-deadline row
-- from ever being claimed. The inner SELECT ... FOR UPDATE SKIP LOCKED
-- lets N concurrent callers each lock a disjoint set of candidate rows
-- without blocking each other.
UPDATE grpop_dlq
SET status = 'retrying', updated_at = $2
WHERE send_id IN (
    SELECT candidate.send_id FROM grpop_dlq AS candidate
    WHERE candidate.status = 'pending' AND candidate.next_retry_at <= $1
        AND (candidate.expires_at IS NULL OR candidate.expires_at > $1)
    ORDER BY candidate.next_retry_at
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: IncrementRetryCount :one
-- Step 1 of MarkRetried: atomically increment retry_count, scoped to the
-- claimed ("retrying") state — the WHERE clause is what makes this safe.
-- Returns no row if send_id doesn't exist or isn't currently claimed; the
-- caller distinguishes those two cases via a follow-up GetDLQEventByID.
UPDATE grpop_dlq SET retry_count = retry_count + 1, last_attempt_at = $2, updated_at = $2
WHERE send_id = $1 AND status = 'retrying'
RETURNING *;

-- name: FinalizeRetryPending :exec
-- Step 2 of MarkRetried when the event goes back to pending (a retryable
-- failure, retries not yet exhausted and not yet past expires_at). See
-- UpsertDLQEvent's doc comment for the attempt_history cap subquery.
UPDATE grpop_dlq SET
    status = 'pending',
    next_retry_at = $2,
    updated_at = $3,
    attempt_history = (
        SELECT COALESCE(jsonb_agg(elem ORDER BY ord), '[]'::jsonb)
        FROM (
            SELECT elem, ord
            FROM jsonb_array_elements(attempt_history || $4) WITH ORDINALITY AS t(elem, ord)
            ORDER BY ord DESC
            LIMIT $5::int
        ) capped
    )
WHERE send_id = $1;

-- name: FinalizeRetryTerminal :exec
-- Step 2 of MarkRetried when the event reaches a terminal state: resolved
-- (success), exhausted (MaxRetries reached), or expired (recomputed
-- NextRetryAt would land at or past ExpiresAt — checked before the
-- MaxRetries case by the caller, see DLQHandler's doc comment). See
-- UpsertDLQEvent's doc comment for the attempt_history cap subquery.
UPDATE grpop_dlq SET
    status = $2,
    updated_at = $3,
    attempt_history = (
        SELECT COALESCE(jsonb_agg(elem ORDER BY ord), '[]'::jsonb)
        FROM (
            SELECT elem, ord
            FROM jsonb_array_elements(attempt_history || $4) WITH ORDINALITY AS t(elem, ord)
            ORDER BY ord DESC
            LIMIT $5::int
        ) capped
    )
WHERE send_id = $1;

-- name: GetDLQEventByID :one
SELECT * FROM grpop_dlq WHERE send_id = $1;

-- name: PurgeExpiredEvents :execrows
-- "Expired" here means grnoti/grpop's ORIGINAL sense (old enough to
-- delete), which predates and is unrelated to this table's own
-- DLQStatusExpired status — despite the shared word, they mean different
-- things (see DLQHandler.PurgeExpiredEvents' doc comment in interfaces.go).
-- This deletes every row in a terminal status (resolved/exhausted/expired)
-- OR any row older than $1, regardless of status.
DELETE FROM grpop_dlq WHERE status IN ('resolved', 'exhausted', 'expired') OR created_at < $1;
