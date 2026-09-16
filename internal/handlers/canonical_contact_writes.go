package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/shridarpatil/whatomate/internal/contactutil"
	"gorm.io/gorm"
)

const canonicalContactWriteAttempts = 6

var canonicalContactWriteRetryBackoffs = [...]time.Duration{
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
}

var errActiveAgentTransferExists = errors.New("contact already has an active transfer")

// canonicalContactWriteTransaction retries only failures that mean the
// canonical redirect changed while locks were being acquired, or PostgreSQL
// aborted the transaction to resolve a serialization/deadlock race.
func canonicalContactWriteTransaction(
	db *gorm.DB,
	write func(tx *gorm.DB) error,
) error {
	return canonicalContactWriteTransactionWithRetry(
		db,
		write,
		isRetryableCanonicalContactWrite,
	)
}

func canonicalContactWriteTransactionWithRetry(
	db *gorm.DB,
	write func(tx *gorm.DB) error,
	retryable func(error) bool,
) error {
	ctx := context.Background()
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		ctx = db.Statement.Context
	}

	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = db.Transaction(write)
		if !retryable(err) || attempt == canonicalContactWriteAttempts-1 {
			return err
		}
		if waitErr := waitForCanonicalContactWriteRetry(ctx, attempt); waitErr != nil {
			return waitErr
		}
	}
	return err
}

func waitForCanonicalContactWriteRetry(ctx context.Context, failedAttempt int) error {
	timer := time.NewTimer(canonicalContactWriteRetryBackoffs[failedAttempt])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableCanonicalContactWrite(err error) bool {
	if errors.Is(err, contactutil.ErrCanonicalContactChanged) {
		return true
	}
	switch postgresErrorCode(err) {
	case "40001", "40P01", "55P03":
		return true
	default:
		return false
	}
}

func isUniqueViolation(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey) || postgresErrorCode(err) == "23505"
}

func postgresErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var sqlState interface {
		SQLState() string
	}
	if errors.As(err, &sqlState) {
		return sqlState.SQLState()
	}
	return ""
}
