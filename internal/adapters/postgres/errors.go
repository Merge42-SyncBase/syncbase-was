package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/jackc/pgx/v5/pgconn"
)

func databaseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if isTemporaryDatabaseError(err) {
		return fmt.Errorf("%s: %w: %w", operation, knowledge.ErrTemporarilyUnavailable, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func isTemporaryDatabaseError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return false
	}
	if strings.HasPrefix(postgresError.Code, "08") || strings.HasPrefix(postgresError.Code, "53") {
		return true
	}
	switch postgresError.Code {
	case "40001", "40P01", "57P01", "57P02", "57P03":
		return true
	default:
		return false
	}
}
