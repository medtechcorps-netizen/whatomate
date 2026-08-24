package database_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCreateIndexesRejectsConcurrentMigratorForSameDatabase(t *testing.T) {
	db := testutil.SetupTestDB(t)

	const migrationLockNamespace int32 = 1380270905
	ready := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Connection(func(session *gorm.DB) error {
			var acquired bool
			if err := session.Raw(
				"SELECT pg_try_advisory_lock(hashtext(current_database()), ?)",
				migrationLockNamespace,
			).Scan(&acquired).Error; err != nil {
				ready <- err
				return err
			}
			if !acquired {
				err := errors.New("test could not acquire database migration advisory lock")
				ready <- err
				return err
			}
			ready <- nil
			<-release

			var unlocked bool
			if err := session.Raw(
				"SELECT pg_advisory_unlock(hashtext(current_database()), ?)",
				migrationLockNamespace,
			).Scan(&unlocked).Error; err != nil {
				return err
			}
			if !unlocked {
				return errors.New("test database migration advisory lock was not released")
			}
			return nil
		})
	}()
	require.NoError(t, <-ready)
	released := false
	finished := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		if !finished {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})

	startedAt := time.Now()
	err := database.CreateIndexes(db)
	assert.ErrorContains(t, err, "another database migrator already owns")
	assert.Less(t, time.Since(startedAt), 2*time.Second, "the second migrator must fail fast")

	close(release)
	released = true
	require.NoError(t, <-done)
	finished = true
}
