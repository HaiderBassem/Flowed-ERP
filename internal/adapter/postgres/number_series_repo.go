package postgres

import (
	"context"

	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
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
	ctx context.Context, kind payment.SeriesKind, yearID shared.ID, deskID *shared.ID,
) (string, shared.ID, error) {
	if err := r.db.RequireTx(ctx, "number_series.NextNumber"); err != nil {
		return "", shared.NilID, err
	}
	const query = `
		UPDATE number_series
		SET next_number = next_number + 1, updated_at = now()
		WHERE series_kind = $1
		  AND academic_year_id = $2
		  AND cashier_desk_id IS NOT DISTINCT FROM $3
		RETURNING id, prefix, next_number - 1, padding`

	q := r.db.Conn(ctx)
	var (
		id      shared.ID
		prefix  string
		number  int64
		padding int16
	)
	err := q.QueryRow(ctx, query, kind, yearID, deskID).Scan(&id, &prefix, &number, &padding)
	if pg.IsNotFound(err) {
		return "", shared.NilID, shared.NotFound("number_series.missing",
			"no %s number series exists for this year and desk", kind).
			WithDetail("series_kind", string(kind)).
			WithDetail("academic_year_id", yearID.String()).
			WithDetail("remedy", "call EnsureSeries when the year or the desk is set up").
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
	ctx context.Context, kind payment.SeriesKind, yearID shared.ID, deskID *shared.ID, prefix string,
) (shared.ID, error) {
	if err := r.db.RequireTx(ctx, "number_series.EnsureSeries"); err != nil {
		return shared.NilID, err
	}
	// The desk's own code goes into the prefix, resolved here rather than by
	// the caller.
	//
	// A receipt number is read by a human matching a slip against a paper
	// book, so it has to be short and meaningful: R-2025-2026-D01-000008, not
	// a line of hexadecimal. The caller holds only the desk's identifier, and
	// this is the one place that already knows which desk the series belongs
	// to — and it runs once per desk per year, so the join costs nothing.
	if deskID != nil {
		var code string
		err := r.db.Conn(ctx).QueryRow(ctx,
			`SELECT code FROM cashier_desk WHERE id = $1`, *deskID).Scan(&code)
		switch {
		case err == nil:
			prefix += code + "-"
		case pg.IsNotFound(err):
			// A series for a desk that no longer exists is still better than
			// refusing the collection in front of a waiting student.
		default:
			return shared.NilID, pg.WrapQuery("number_series.EnsureSeries", err)
		}
	}

	const query = `
		INSERT INTO number_series (id, series_kind, academic_year_id, cashier_desk_id, prefix)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (series_kind, academic_year_id, cashier_desk_id)
		DO UPDATE SET updated_at = now()
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, shared.NewID(), kind, yearID, deskID, prefix).Scan(&id)
	if err != nil {
		return shared.NilID, pg.WrapQuery("number_series.EnsureSeries", err)
	}
	return id, nil
}
