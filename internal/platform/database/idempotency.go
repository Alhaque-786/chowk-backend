package database

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// IdempotencyStore is the Postgres-backed implementation of
// httpx.IdempotencyStore.
//
// It lives in platform/database rather than in a module because idempotency is
// infrastructure: every module's write endpoints share one table and one policy.
type IdempotencyStore struct {
	db *DB
}

func NewIdempotencyStore(db *DB) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

// Claim atomically reserves a key, using the primary key as the mutex.
//
// The whole correctness of this hinges on one INSERT ... ON CONFLICT statement.
// A read-then-write ("does this key exist? no? insert it") has a window between
// the two where a concurrent retry also reads "no" — and you get exactly the
// double order the mechanism exists to prevent. The database must arbitrate.
func (s *IdempotencyStore) Claim(
	ctx context.Context,
	key, fingerprint, endpoint, userID string,
	now time.Time,
) ([]byte, bool, error) {
	// DO NOTHING then inspect: if the insert took effect we own the key; if it
	// conflicted, the existing row tells us whether to replay or reject.
	const claim = `
		INSERT INTO idempotency_keys (key, user_id, endpoint, fingerprint, status, created_at)
		VALUES ($1, NULLIF($2,'')::uuid, $3, $4, 'in_progress', $5)
		ON CONFLICT (key) DO NOTHING
		RETURNING key`

	var claimedKey string
	err := s.db.Q(ctx).QueryRow(ctx, claim, key, userID, endpoint, fingerprint, now).Scan(&claimedKey)
	if err == nil {
		return nil, true, nil
	}
	if !IsNotFound(err) {
		return nil, false, apperr.Internal(fmt.Errorf("idempotency: claim %q: %w", key, err))
	}

	// Conflict: a row already exists. Decide what to do with it.
	const load = `
		SELECT status, fingerprint, response, created_at
		FROM idempotency_keys
		WHERE key = $1`

	var (
		status      string
		storedPrint string
		response    []byte
		createdAt   time.Time
	)
	if err := s.db.Q(ctx).QueryRow(ctx, load, key).Scan(&status, &storedPrint, &response, &createdAt); err != nil {
		if IsNotFound(err) {
			// The row vanished between our INSERT and this SELECT — a cleanup job
			// pruned it. Treat as claimed; the worst case is redoing the work.
			return nil, true, nil
		}
		return nil, false, apperr.Internal(fmt.Errorf("idempotency: load %q: %w", key, err))
	}

	// Key reused for a DIFFERENT payload. Almost always a client bug (a key
	// generated once and reused across distinct requests). Serving the old
	// response would hide the bug and produce a nonsensical result, so reject.
	if storedPrint != fingerprint {
		return nil, false, apperr.New(apperr.KindConflict, "IDEMPOTENCY_KEY_REUSED",
			"This Idempotency-Key was already used for a different request. Generate a new key per request.")
	}

	if status == "completed" {
		return response, false, nil
	}

	// Still in flight. A stale claim would otherwise wedge the key forever if the
	// process that owned it crashed before completing, so treat an old claim as
	// abandoned and take it over.
	const staleAfter = 60 * time.Second
	if now.Sub(createdAt) > staleAfter {
		const takeover = `
			UPDATE idempotency_keys
			SET created_at = $2, fingerprint = $3
			WHERE key = $1 AND status = 'in_progress'`
		if _, err := s.db.Q(ctx).Exec(ctx, takeover, key, now, fingerprint); err != nil {
			return nil, false, apperr.Internal(fmt.Errorf("idempotency: takeover %q: %w", key, err))
		}
		return nil, true, nil
	}

	return nil, false, apperr.New(apperr.KindConflict, "REQUEST_IN_PROGRESS",
		"An identical request is already being processed. Please wait a moment and try again.")
}

// Complete stores the response for replay.
func (s *IdempotencyStore) Complete(ctx context.Context, key string, status int, body []byte) error {
	record, err := httpx.MarshalStoredResponse(status, body)
	if err != nil {
		return apperr.Internal(fmt.Errorf("idempotency: marshal response: %w", err))
	}

	const q = `
		UPDATE idempotency_keys
		SET status = 'completed', response = $2, completed_at = now()
		WHERE key = $1`
	if _, err := s.db.Q(ctx).Exec(ctx, q, key, record); err != nil {
		return apperr.Internal(fmt.Errorf("idempotency: complete %q: %w", key, err))
	}
	return nil
}

// Release deletes a claim whose work failed, so the client can retry properly.
//
// Delete rather than mark-failed: the client is entitled to a genuine retry, and
// keeping a failed marker would make a transient error permanent.
func (s *IdempotencyStore) Release(ctx context.Context, key string) error {
	const q = `DELETE FROM idempotency_keys WHERE key = $1 AND status = 'in_progress'`
	if _, err := s.db.Q(ctx).Exec(ctx, q, key); err != nil {
		return apperr.Internal(fmt.Errorf("idempotency: release %q: %w", key, err))
	}
	return nil
}

// PruneOlderThan deletes expired keys. Called by the worker.
//
// Retention only needs to outlive a client's retry window; keeping keys forever
// grows a table that is on the write path of every checkout.
func (s *IdempotencyStore) PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM idempotency_keys WHERE created_at < $1`
	tag, err := s.db.Q(ctx).Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("idempotency: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

var _ httpx.IdempotencyStore = (*IdempotencyStore)(nil)
