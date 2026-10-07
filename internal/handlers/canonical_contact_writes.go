package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"

	"github.com/shridarpatil/whatomate/internal/contactutil"
	"gorm.io/gorm"
)

const canonicalContactWriteAttempts = 6

const canonicalContactWriteInitialRetryDelay = 25 * time.Millisecond

var errActiveAgentTransferExists = errors.New("contact already has an active transfer")

// canonicalContactWriteTransaction retries only failures that mean the
// canonical redirect changed while locks were being acquired, or PostgreSQL
// aborted the transaction to resolve a serialization/deadlock race.
func canonicalContactWriteTransaction(
	db *gorm.DB,
	write func(tx *gorm.DB) error,
) error {
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = db.Transaction(write)
		if !isRetryableCanonicalContactWrite(err) {
			return err
		}
		if attempt+1 < canonicalContactWriteAttempts {
			if waitErr := waitForCanonicalContactWriteRetry(db.Statement.Context, attempt); waitErr != nil {
				return waitErr
			}
		}
	}
	return err
}

func waitForCanonicalContactWriteRetry(ctx context.Context, attempt int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	delay := canonicalContactWriteInitialRetryDelay << attempt
	timer := time.NewTimer(delay)
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

// contactSelectorWriteTransaction queues identity/lifecycle changes before
// taking contact rows. Call it at the outermost write boundary, before any
// account/contact locks; never acquire this fence inside an already locked
// canonical-contact callback. See internal/channel/legacy_meta_fence.go.
func contactSelectorWriteTransaction(db *gorm.DB, organizationID uuid.UUID, write func(*gorm.DB) error) error {
	return canonicalContactWriteTransaction(db, func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, organizationID); err != nil {
			return err
		}
		return write(tx)
	})
}
