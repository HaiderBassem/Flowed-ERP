package app

import (
	"context"
	"io"
	"log/slog"

	"flowed/internal/port"
)

// Shared test doubles.
//
// These lived in cashier_service_test.go until the cash drawer was removed with
// it. They are here rather than in whichever test file happens to need them
// first, because two tests defining the same fake is how two tests end up
// asserting against different behaviour under one name.

// fakeAudit records what a command wrote to the trail.
//
// Every money command is supposed to append exactly one entry, and the usual
// failure is not a wrong entry but no entry: a command that returns nil having
// silently skipped its audit write passes every other assertion in the test.
type fakeAudit struct {
	port.AuditRepository
	entries []port.AuditEntry
}

func (f *fakeAudit) Append(_ context.Context, e port.AuditEntry) error {
	f.entries = append(f.entries, e)
	return nil
}

// actions lists the recorded audit actions, in order.
func (f *fakeAudit) actions() []string {
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.Action)
	}
	return out
}

// discardLogger is for services that take a logger and tests that do not want
// its output interleaved with the test runner's.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
