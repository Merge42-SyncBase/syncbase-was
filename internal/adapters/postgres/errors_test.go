package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseErrorClassifiesDependencyOutages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "connection exception", err: &pgconn.PgError{Code: "08006"}},
		{name: "administrator shutdown", err: &pgconn.PgError{Code: "57P01"}},
		{name: "serialization failure", err: &pgconn.PgError{Code: "40001"}},
		{name: "lock timeout", err: &pgconn.PgError{Code: "55P03"}},
		{name: "statement timeout", err: &pgconn.PgError{Code: "57014"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := databaseError("query", test.err)
			if !errors.Is(got, knowledge.ErrTemporarilyUnavailable) || !errors.Is(got, test.err) {
				t.Fatalf("databaseError() = %v, want temporary error preserving cause", got)
			}
		})
	}
}

func TestDatabaseErrorDoesNotRetryInvalidSQL(t *testing.T) {
	t.Parallel()

	err := databaseError("query", &pgconn.PgError{Code: "42601"})
	if errors.Is(err, knowledge.ErrTemporarilyUnavailable) {
		t.Fatalf("databaseError() = %v, syntax error must not be retryable", err)
	}
}

func TestDatabaseErrorPreservesCancellation(t *testing.T) {
	t.Parallel()

	err := databaseError("query", context.Canceled)
	if !errors.Is(err, context.Canceled) || errors.Is(err, knowledge.ErrTemporarilyUnavailable) {
		t.Fatalf("databaseError() = %v, want cancellation only", err)
	}
}
