package handlers

import (
	"errors"

	"github.com/shridarpatil/whatomate/internal/contactutil"
	"gorm.io/gorm"
)

const canonicalContactWriteAttempts = 3

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
	}
	return err
}

func isRetryableCanonicalContactWrite(err error) bool {
	if errors.Is(err, contactutil.ErrCanonicalContactChanged) {
		return true
	}
	switch postgresErrorCode(err) {
	case "40001", "40P01":
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
