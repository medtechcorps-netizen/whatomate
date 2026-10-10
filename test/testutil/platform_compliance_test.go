package testutil

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestPlatformComplianceFixtureRLSRetryReturnsFirstSuccess(t *testing.T) {
	attempts := 0
	err := retryPlatformComplianceTestContract(func() error {
		attempts++
		return nil
	}, func(int) { t.Fatal("successful installation must not retry") })
	require.NoError(t, err)
	require.Equal(t, 1, attempts)
}

func TestPlatformComplianceFixtureRLSRetryRepeatsCompleteWrappedDeadlockAttempt(t *testing.T) {
	attempts := 0
	var logged []int
	err := retryPlatformComplianceTestContract(func() error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("apply tenant RLS: install policy: %w", &pgconn.PgError{Code: "40P01"})
		}
		return nil
	}, func(attempt int) { logged = append(logged, attempt) })
	require.NoError(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, []int{2, 3}, logged)
}

func TestPlatformComplianceFixtureRLSRetryExhaustionReturnsFinalError(t *testing.T) {
	attempts := 0
	var last error
	var logged []int
	err := retryPlatformComplianceTestContract(func() error {
		attempts++
		last = fmt.Errorf("attempt %d: %w", attempts, &pgconn.PgError{Code: "40P01"})
		return last
	}, func(attempt int) { logged = append(logged, attempt) })
	require.Same(t, last, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, []int{2, 3}, logged)
}

type fixtureSQLStateOnlyError struct{}

func (fixtureSQLStateOnlyError) Error() string    { return "40P01" }
func (fixtureSQLStateOnlyError) SQLState() string { return "40P01" }

func TestPlatformComplianceFixtureRLSRetryRefusesUnrelatedOrUntypedErrors(t *testing.T) {
	for _, original := range []error{
		&pgconn.PgError{Code: "23505"}, // Constraint failures remain visible.
		&pgconn.PgError{Code: "42501"}, // A permission/guard failure is not transient.
		&pgconn.PgError{Code: "55P03"},
		&pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "25P02"},
		errors.New("deadlock detected (SQLSTATE 40P01)"),
		fixtureSQLStateOnlyError{},
	} {
		t.Run(original.Error(), func(t *testing.T) {
			attempts := 0
			wrapped := fmt.Errorf("fixture installation: %w", original)
			err := retryPlatformComplianceTestContract(func() error {
				attempts++
				return wrapped
			}, func(int) { t.Fatal("only typed PostgreSQL deadlocks may retry") })
			require.Same(t, wrapped, err)
			require.Equal(t, 1, attempts)
		})
	}
}

func TestPlatformComplianceFixtureRLSRetryStopsOnDifferentSecondFailure(t *testing.T) {
	attempts := 0
	wanted := &pgconn.PgError{Code: "42501"}
	var logged []int
	err := retryPlatformComplianceTestContract(func() error {
		attempts++
		if attempts == 1 {
			return &pgconn.PgError{Code: "40P01"}
		}
		return wanted
	}, func(attempt int) { logged = append(logged, attempt) })
	require.Same(t, wanted, err)
	require.Equal(t, 2, attempts)
	require.Equal(t, []int{2}, logged)
}
