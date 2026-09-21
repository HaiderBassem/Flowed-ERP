package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// SettingsRepository stores the institution's details.
type SettingsRepository struct{ db *pg.DB }

// NewSettingsRepository builds the settings store over a connection pool.
func NewSettingsRepository(db *pg.DB) *SettingsRepository {
	return &SettingsRepository{db: db}
}

var _ port.SettingsRepository = (*SettingsRepository)(nil)

// All reads every setting.
//
// The whole table in one query, always. It holds a handful of short rows that
// every receipt render needs together, and fetching them one key at a time
// would be a round trip per line of the letterhead.
func (r *SettingsRepository) All(ctx context.Context) (map[string]string, error) {
	const query = `SELECT key, value FROM app_setting`

	rows, err := r.db.Conn(ctx).Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("settings.All", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, pg.WrapQuery("settings.All", err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("settings.All", err)
	}
	return out, nil
}

// Set writes several settings as one act.
//
// Upserted rather than updated, so a key introduced by a later version of the
// renderer can be written before any migration has seeded it — the alternative
// is a settings screen that silently drops the one field somebody just added.
func (r *SettingsRepository) Set(
	ctx context.Context, values map[string]string, actor shared.ID,
) error {
	if err := r.db.RequireTx(ctx, "settings.Set"); err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}

	const query = `
		INSERT INTO app_setting (key, value, updated_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by`

	var by any
	if !shared.IsNil(actor) {
		by = actor
	}

	batch := &pgx.Batch{}
	for key, value := range values {
		batch.Queue(query, key, value, by)
	}

	results := r.db.Conn(ctx).SendBatch(ctx, batch)
	defer results.Close()

	for range values {
		if _, err := results.Exec(); err != nil {
			return pg.WrapQuery("settings.Set", err)
		}
	}
	return nil
}
