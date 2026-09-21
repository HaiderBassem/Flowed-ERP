package postgres

import (
	"context"

	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// TxManager adapts the platform connection pool to the transaction boundary
// the application layer depends on.
//
// It holds no state of its own: the open transaction travels in the context,
// which is what lets a command handler wrap its whole body once and have every
// repository call inside join the same transaction without a handle being
// threaded through the domain.
type TxManager struct {
	db *pg.DB
}

// NewTxManager builds the transaction boundary over a connection pool.
func NewTxManager(db *pg.DB) *TxManager { return &TxManager{db: db} }

var _ port.TxManager = (*TxManager)(nil)

// Write runs fn in a read-write transaction, retrying on serialization failure
// and deadlock. A nested call runs in a savepoint, so one row of a bulk command
// can fail without discarding the batch.
func (m *TxManager) Write(ctx context.Context, fn func(ctx context.Context) error) error {
	return m.db.WithTx(ctx, pg.DefaultTxOptions(), fn)
}

// Read runs fn in a read-only transaction, for a report that must see one
// consistent snapshot across several queries.
func (m *TxManager) Read(ctx context.Context, fn func(ctx context.Context) error) error {
	return m.db.ReadOnly(ctx, fn)
}

// RequireTx returns an invariant violation unless a transaction is already open.
func (m *TxManager) RequireTx(ctx context.Context, operation string) error {
	return m.db.RequireTx(ctx, operation)
}
