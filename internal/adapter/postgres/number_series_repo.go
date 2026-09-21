package postgres

import (
	"context"

	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// NumberSeriesRepository allocates receipt and refund numbers.
type NumberSeriesRepository struct{ db *pg.DB }

// NewNumberSeriesRepository builds the number series store over a connection pool.
func NewNumberSeriesRepository(db *pg.DB) *NumberSeriesRepository {
	return &NumberSeriesRepository{db: db}
}

var _ port.NumberSeriesRepository = (*NumberSeriesRepository)(nil)

// NextNumber takes the next number in a series and returns it formatted.
//
// The increment is a single UPDATE, which takes the row lock, reads the counter
// and advances it in one atomic step — no separate SELECT ... FOR UPDATE is
// needed, and there is no window in which two cashiers can read the same value.
// RETURNING sees post-update values, so next_number - 1 is the number this
// caller was handed.
//
// It runs in the caller's transaction on purpose. A payment that rolls back
// returns its number rather than burning it, which is what makes the printed
// sequence gapless. The counter is also the last lock a posting command takes,
// after the account and the year, so a hot series cannot deadlock against an
// account lock.
func (r *NumberSeriesRepository) NextNumber(
	ctx context.Context, kind payment.SeriesKind, yearID shared.ID,
) (string, shared.ID, error) {
	if err := r.db.RequireTx(ctx, "number_series.NextNumber"); err != nil {
		return "", shared.NilID, err
	}
	const query = `
		UPDATE number_series
		SET next_number = next_number + 1, updated_at = now()
		WHERE series_kind = $1
		  AND academic_year_id = $2
		RETURNING id, prefix, next_number - 1, padding`

	q := r.db.Conn(ctx)
	var (
		id      shared.ID
		prefix  string
		number  int64
		padding int16
	)
	err := q.QueryRow(ctx, query, kind, yearID).Scan(&id, &prefix, &number, &padding)
	if pg.IsNotFound(err) {
		return "", shared.NilID, shared.NotFound("number_series.missing",
			"no %s number series exists for this year", kind).
			WithDetail("series_kind", string(kind)).
			WithDetail("academic_year_id", yearID.String()).
			WithDetail("remedy", "call EnsureSeries when the year is set up").
			WithCause(err)
	}
	if err != nil {
		return "", shared.NilID, pg.WrapQuery("number_series.NextNumber", err)
	}
	return formatSeriesNumber(prefix, number, padding), id, nil
}

// EnsureSeries returns the identifier of a series, creating it if it is not
// there yet.
//
// The conflict path updates rather than doing nothing, for two reasons: DO
// NOTHING returns no row, so the existing identifier would need a second query,
// and the update makes two concurrent setup calls serialise instead of one
// finding nothing because the other has not committed.
//
// An existing series keeps its prefix. Changing it mid-year would split one
// receipt book into two, and an auditor reconciling against the paper would
// find a sequence that stops and restarts under a different name.
func (r *NumberSeriesRepository) EnsureSeries(
	ctx context.Context, kind payment.SeriesKind, yearID shared.ID, prefix string,
) (shared.ID, error) {
	if err := r.db.RequireTx(ctx, "number_series.EnsureSeries"); err != nil {
		return shared.NilID, err
	}
	const query = `
		INSERT INTO number_series (id, series_kind, academic_year_id, prefix)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (series_kind, academic_year_id)
		DO UPDATE SET updated_at = now()
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, shared.NewID(), kind, yearID, prefix).Scan(&id)
	if err != nil {
		return shared.NilID, pg.WrapQuery("number_series.EnsureSeries", err)
	}
	return id, nil
}
