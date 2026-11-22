package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetOrCreateCallLogLookupFailureDoesNotCreate(t *testing.T) {
	lookupFailure := errors.New("synthetic call log lookup failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "lookup_error", err: lookupFailure},
		{name: "wrapped_lookup_error", err: fmt.Errorf("query failed: %w", lookupFailure)},
		{name: "canceled", err: context.Canceled},
		{name: "deadline_exceeded", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep even an unexpected Create on this dry-run handle independent
			// of a database connection or transaction.
			db := dryRunPostgres(t).Session(&gorm.Session{SkipDefaultTransaction: true})
			pool, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, pool.Close()) })

			lookupCalls, createCalls := 0, 0
			const lookupCallback = "test:call_log_lookup_failure"
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(lookupCallback, func(tx *gorm.DB) {
				if tx.Statement.Table == "call_logs" {
					lookupCalls++
					tx.AddError(tc.err)
				}
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(lookupCallback)) })
			const createCallback = "test:call_log_unexpected_create"
			require.NoError(t, db.Callback().Create().Before("*").Register(createCallback, func(tx *gorm.DB) {
				createCalls++
				tx.AddError(errors.New("unexpected Create after call log lookup failure"))
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(createCallback)) })

			organizationID := uuid.New()
			account := &models.WhatsAppAccount{OrganizationID: organizationID, Name: "lookup-guard"}
			contact := &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID}
			app := &App{DB: db, Log: testutil.NopLogger()}

			var row *models.CallLog
			var created bool
			// A nil manager is intentional: a failed SELECT must return before
			// IVR lookup as well as Create. This does not test terminal retries.
			require.NotPanics(t, func() {
				row, created = app.getOrCreateCallLog(account, contact, "lookup-guard-call", "15550000000", time.Now())
			})
			require.Equal(t, 1, lookupCalls, "the CallLog lookup fault must actually be reached")
			require.Zero(t, createCalls, "a lookup error must not be treated as a missing CallLog")
			require.Nil(t, row)
			require.False(t, created)
		})
	}
}
