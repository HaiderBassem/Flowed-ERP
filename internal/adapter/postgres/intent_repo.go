package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// IntentRepository stores collections begun at an external provider.
type IntentRepository struct{ db *pg.DB }

// NewIntentRepository builds the intent store over a connection pool.
func NewIntentRepository(db *pg.DB) *IntentRepository { return &IntentRepository{db: db} }

var _ port.IntentRepository = (*IntentRepository)(nil)

const intentColumns = `
	id, provider_code, account_id, student_id, academic_year_id, amount, status,
	provider_ref, client_ref, payment_idempotency_key, redirect_url, expires_at,
	failure_code, failure_message, payment_id, created_at, created_by, confirmed_at`

func scanIntent(row pgx.Row) (*payment.Intent, error) {
	var i payment.Intent
	if err := row.Scan(
		&i.ID, &i.ProviderCode, &i.AccountID, &i.StudentID, &i.AcademicYearID, &i.Amount, &i.Status,
		&i.ProviderRef, &i.ClientRef, &i.PaymentIdempotencyKey, &i.RedirectURL, &i.ExpiresAt,
		&i.FailureCode, &i.FailureMessage, &i.PaymentID, &i.CreatedAt, &i.CreatedBy, &i.ConfirmedAt,
	); err != nil {
		return nil, err
	}
	return &i, nil
}

// Create records a collection request.
func (r *IntentRepository) Create(ctx context.Context, i *payment.Intent) error {
	if err := r.db.RequireTx(ctx, "intent.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO payment_intent (
			id, provider_code, account_id, student_id, academic_year_id, amount, status,
			provider_ref, client_ref, payment_idempotency_key, redirect_url, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		i.ID, i.ProviderCode, i.AccountID, i.StudentID, i.AcademicYearID, i.Amount.Int64(),
		string(i.Status), i.ProviderRef, i.ClientRef, i.PaymentIdempotencyKey,
		i.RedirectURL, i.ExpiresAt, i.CreatedBy).Scan(&i.CreatedAt)
	return pg.WrapQuery("intent.Create", err)
}

// Update writes back an intent whose state moved.
func (r *IntentRepository) Update(ctx context.Context, i *payment.Intent) error {
	if err := r.db.RequireTx(ctx, "intent.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE payment_intent SET
			status = $2, provider_ref = $3, redirect_url = $4, expires_at = $5,
			failure_code = $6, failure_message = $7, payment_id = $8, confirmed_at = $9
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		i.ID, string(i.Status), i.ProviderRef, i.RedirectURL, i.ExpiresAt,
		i.FailureCode, i.FailureMessage, i.PaymentID, i.ConfirmedAt).Scan(&id)
	return pg.WrapQuery("intent.Update", err)
}

// GetByID returns one collection request.
func (r *IntentRepository) GetByID(ctx context.Context, id shared.ID) (*payment.Intent, error) {
	q := r.db.Conn(ctx)
	i, err := scanIntent(q.QueryRow(ctx, `SELECT`+intentColumns+` FROM payment_intent WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("intent.GetByID", err)
	}
	return i, nil
}

// GetForUpdate locks the intent row.
//
// A provider callback and a status poll can arrive at the same instant. Without
// the lock both would read an unconfirmed intent, both would post a payment,
// and the student would be charged twice for one transaction — the idempotency
// key would catch the second at the payment layer, but the intent would be left
// pointing at whichever won, which is not a state anyone can reason about.
func (r *IntentRepository) GetForUpdate(ctx context.Context, id shared.ID) (*payment.Intent, error) {
	if err := r.db.RequireTx(ctx, "intent.GetForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	i, err := scanIntent(q.QueryRow(ctx,
		`SELECT`+intentColumns+` FROM payment_intent WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("intent.GetForUpdate", err)
	}
	return i, nil
}

// FindByReference resolves whichever reference the provider echoed back.
func (r *IntentRepository) FindByReference(ctx context.Context, providerCode, clientRef, providerRef string) (*payment.Intent, error) {
	const query = `
		SELECT` + intentColumns + `
		FROM payment_intent
		WHERE provider_code = $1
		  AND (($2::text <> '' AND client_ref = $2::text)
		    OR ($3::text <> '' AND provider_ref = $3::text))
		LIMIT 1`

	q := r.db.Conn(ctx)
	i, err := scanIntent(q.QueryRow(ctx, query, providerCode, clientRef, providerRef))
	if err != nil {
		return nil, pg.WrapQuery("intent.FindByReference", err)
	}
	return i, nil
}

// ListForAccount returns an account's collection requests, newest first.
func (r *IntentRepository) ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Intent, error) {
	const query = `SELECT` + intentColumns + `
		FROM payment_intent WHERE account_id = $1 ORDER BY created_at DESC LIMIT 100`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("intent.ListForAccount", err)
	}
	intents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Intent, error) {
		return scanIntent(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("intent.ListForAccount", err)
	}
	return intents, nil
}

// ListStale returns open intents past their window.
func (r *IntentRepository) ListStale(ctx context.Context, before time.Time, limit int) ([]*payment.Intent, error) {
	const query = `
		SELECT` + intentColumns + `
		FROM payment_intent
		WHERE status IN ('created', 'pending')
		  AND (expires_at IS NOT NULL AND expires_at < $1)
		ORDER BY expires_at
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, before, boundedLimit(limit, 100))
	if err != nil {
		return nil, pg.WrapQuery("intent.ListStale", err)
	}
	intents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Intent, error) {
		return scanIntent(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("intent.ListStale", err)
	}
	return intents, nil
}

// RecordEvent stores a callback delivery and reports whether it was already
// seen.
//
// ON CONFLICT DO NOTHING against the unique (provider, external event id) is
// the duplicate guard itself, and it is one statement so two concurrent
// deliveries of the same event cannot both conclude they are the first.
func (r *IntentRepository) RecordEvent(ctx context.Context, e *payment.ProviderEvent) (bool, error) {
	if err := r.db.RequireTx(ctx, "intent.RecordEvent"); err != nil {
		return false, err
	}
	const query = `
		INSERT INTO payment_provider_event (
			id, provider_code, intent_id, external_event_id, event_type, signature_ok, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (provider_code, external_event_id) DO NOTHING
		RETURNING id, received_at`

	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return false, shared.Internal("intent.payload_encode", err,
			"the provider callback could not be stored")
	}
	if e.ID == shared.NilID {
		e.ID = shared.NewID()
	}

	q := r.db.Conn(ctx)
	err = q.QueryRow(ctx, query,
		e.ID, e.ProviderCode, e.IntentID, e.ExternalEventID, e.EventType, e.SignatureOK, payload,
	).Scan(&e.ID, &e.ReceivedAt)
	if err != nil {
		if pg.IsNotFound(err) {
			// The insert did nothing: this delivery has been seen.
			return true, nil
		}
		return false, pg.WrapQuery("intent.RecordEvent", err)
	}
	return false, nil
}

// MarkEventProcessed records what acting on a callback did.
func (r *IntentRepository) MarkEventProcessed(ctx context.Context, id shared.ID, outcome string, at time.Time) error {
	const query = `
		UPDATE payment_provider_event
		SET processed_at = COALESCE($3, now()), outcome = $2
		WHERE id = $1`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, outcome, instant(at))
	return pg.WrapQuery("intent.MarkEventProcessed", err)
}

// ListEvents returns every delivery for one intent, newest first.
func (r *IntentRepository) ListEvents(ctx context.Context, intentID shared.ID) ([]*payment.ProviderEvent, error) {
	const query = `
		SELECT id, provider_code, intent_id, external_event_id, event_type,
		       signature_ok, payload, received_at, processed_at, outcome
		FROM payment_provider_event
		WHERE intent_id = $1
		ORDER BY received_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, intentID)
	if err != nil {
		return nil, pg.WrapQuery("intent.ListEvents", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.ProviderEvent, error) {
		var (
			e   payment.ProviderEvent
			raw []byte
		)
		err := row.Scan(&e.ID, &e.ProviderCode, &e.IntentID, &e.ExternalEventID, &e.EventType,
			&e.SignatureOK, &raw, &e.ReceivedAt, &e.ProcessedAt, &e.Outcome)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &e.Payload)
		}
		return &e, err
	})
	if err != nil {
		return nil, pg.WrapQuery("intent.ListEvents", err)
	}
	return events, nil
}
