package handlers

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAfterTenantCommitDrainsExactlyOnce(t *testing.T) {
	app := &App{tenantOrgID: uuid.New(), Log: testutil.NopLogger()}
	called := 0
	app.afterTenantCommit(func() { called++ })
	assert.Zero(t, called)

	callbacks := app.takeAfterCommit()
	require.Len(t, callbacks, 1)
	app.runAfterCommit(callbacks)
	assert.Equal(t, 1, called)
	assert.Empty(t, app.takeAfterCommit())
}

func TestWithCommittedTenantAppRunsCallbacksOnlyAfterRealCommit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	app := &App{DB: db, Config: &config.Config{}, Log: testutil.NopLogger()}

	rollbackCalls := 0
	errForcedRollback := errors.New("forced transaction rollback")
	err := app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
		scoped.afterTenantCommit(func() { rollbackCalls++ })
		return errForcedRollback
	})
	require.ErrorIs(t, err, errForcedRollback)
	assert.Zero(t, rollbackCalls)

	commitCalls := 0
	require.NoError(t, app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
		scoped.afterTenantCommit(func() { commitCalls++ })
		return nil
	}))
	assert.Equal(t, 1, commitCalls)
}

func TestRunAfterCommitContainsPanicAndContinues(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	called := 0
	require.NotPanics(t, func() {
		app.runAfterCommit([]func(){
			func() { panic("broken subscriber") },
			func() { called++ },
		})
	})
	assert.Equal(t, 1, called)
}
