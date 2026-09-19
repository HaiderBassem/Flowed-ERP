package postgres

import (
	"context"
	"time"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// IdempotencyRepository remembers commands already executed, so a retry
// returns the original outcome instead of doing the work twice.
type IdempotencyRepository struct{ db *pg.DB }

// NewIdempotencyRepository builds the idempotency store over a connection pool.
func NewIdempotencyRepository(db *pg.DB) *IdempotencyRepository {
	return &IdempotencyRepository{db: db}
}

var _ port.IdempotencyRepository = (*IdempotencyRepository)(nil)

// This repository is the one deliberate exception to the rule that every
// mutating method runs inside the caller's transaction.
//
// The claim must be durable and visible the instant it is made, before the
// command it guards begins. Enrolled in the command's transaction it would be
// invisible to a concurrent duplicate until commit — under read committed the
// second request would see no claim, proceed, and collect the money twice,
// which is the exact failure the key exists to prevent. Worse, a command that
// rolled back would take its own claim with it, so a client retrying after a
// genuine failure would find no record of the first attempt.
//
// So Begin, Complete and Fail each run on the pool, in their own implicit
// transaction, outside whatever the command is doing. Settling the record and
// committing the command are therefore not atomic: a process killed between
// the two leaves an in_progress record, which is why the middleware releases
// the key on panic and why records carry an expiry.

// Begin claims a key for a command.
//
// It returns a nil record when the claim succeeded and the caller should do the
// work. When the key was already used it returns the stored record, and the
// caller replays that outcome rather than acting again.
//
// A key replayed with a different payload is refused. A client that reuses a
// key for a different amount or a different student has a bug, and replaying
// the first outcome would hand back a receipt for a collection that never
// happened.
func (r *IdempotencyRepository) Begin(
	ctx context.Context, key, commandName, payloadHash string, actor *shared.ID,
) (*port.IdempotencyRecord, error) {
	const claim = `
		INSERT INTO idempotency_record (id, idempotency_key, command_name, payload_hash, actor_user_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key, command_name) DO NOTHING
		RETURNING id`

	q := r.db.Conn(ctx)
	var claimed shared.ID
	err := q.QueryRow(ctx, claim, shared.NewID(), key, commandName, payloadHash, actor).Scan(&claimed)
	if err == nil {
		return nil, nil
	}
	if !pg.IsNotFound(err) {
		return nil, pg.WrapQuery("idempotency.Begin", err)
	}

	const load = `
		SELECT idempotency_key, command_name, payload_hash, status,
		       response_status, response_body, error_code, created_at, completed_at
		FROM idempotency_record
		WHERE idempotency_key = $1 AND command_name = $2`

	var (
		record       port.IdempotencyRecord
		responseCode *int32
		errorCode    *string
	)
	err = q.QueryRow(ctx, load, key, commandName).Scan(
		&record.Key, &record.CommandName, &record.PayloadHash, &record.Status,
		&responseCode, &record.ResponseBody, &errorCode, &record.CreatedAt, &record.CompletedAt,
	)
	if pg.IsNotFound(err) {
		// The conflicting row belongs to a transaction that has not committed:
		// the same command is in flight elsewhere right now.
		return nil, shared.Conflict("idempotency.in_flight",
			"command %q with this idempotency key is already being processed", commandName).
			WithDetail("command_name", commandName)
	}
	if err != nil {
		return nil, pg.WrapQuery("idempotency.Begin.load", err)
	}
	if responseCode != nil {
		record.ResponseCode = int(*responseCode)
	}
	if errorCode != nil {
		record.ErrorCode = *errorCode
	}

	if record.PayloadHash != payloadHash {
		return nil, shared.Conflict("idempotency.payload_mismatch",
			"idempotency key for command %q was already used with a different payload", commandName).
			WithDetail("command_name", commandName).
			WithDetail("stored_payload_hash", record.PayloadHash).
			WithDetail("supplied_payload_hash", payloadHash).
			WithDetail("remedy", "use a fresh idempotency key for a different request")
	}

	return &record, nil
}

// Complete stores the outcome a replay will return.
func (r *IdempotencyRepository) Complete(ctx context.Context, key, commandName string, status int, body []byte) error {
	const query = `
		UPDATE idempotency_record SET
			status          = 'succeeded',
			response_status = $3,
			response_body   = $4,
			error_code      = NULL,
			completed_at    = now()
		WHERE idempotency_key = $1 AND command_name = $2
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, key, commandName, status, body).Scan(&id)
	return pg.WrapQuery("idempotency.Complete", err)
}

// Fail records that the command did not succeed, so a retry is not told it
// already worked.
func (r *IdempotencyRepository) Fail(ctx context.Context, key, commandName, errorCode string) error {
	const query = `
		UPDATE idempotency_record SET
			status       = 'failed',
			error_code   = $3,
			completed_at = now()
		WHERE idempotency_key = $1 AND command_name = $2
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, key, commandName, errorCode).Scan(&id)
	return pg.WrapQuery("idempotency.Fail", err)
}

// PurgeExpired removes records past their retention window. They are not an
// audit trail — the audit log is — so deleting them loses nothing.
func (r *IdempotencyRepository) PurgeExpired(ctx context.Context, before time.Time) (int64, error) {
	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, `DELETE FROM idempotency_record WHERE expires_at < $1`, before)
	if err != nil {
		return 0, pg.WrapQuery("idempotency.PurgeExpired", err)
	}
	return tag.RowsAffected(), nil
}
