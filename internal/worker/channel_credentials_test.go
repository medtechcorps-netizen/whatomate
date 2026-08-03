package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const threadsTestWorkerEncryptionKey = "threads-worker-test-encryption-key"

var threadsRefreshTestAppSequence atomic.Uint64

type threadsRefreshTestFixture struct {
	Organization *models.Organization
	Account      models.ChannelAccount
	Credential   models.ChannelCredential
	Integration  models.ProviderIntegration
	AppID        string
}

func TestThreadsCredentialRefreshExpiresStaleGrantFailClosed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	expiredAt := time.Now().UTC().Add(-time.Hour)
	fixture := createThreadsRefreshTestFixture(t, db, expiredAt)
	worker := threadsRefreshTestWorker(db)

	processed, err := worker.refreshThreadsCredential(
		context.Background(),
		fixture.Organization.ID,
		time.Now().UTC().Add(defaultThreadsCredentialRefreshWindow),
	)
	require.NoError(t, err)
	assert.True(t, processed)
	require.NoError(t, db.First(&fixture.Credential, "id = ?", fixture.Credential.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusExpired, fixture.Credential.Status)
	require.NoError(t, db.First(&fixture.Account, "id = ?", fixture.Account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, fixture.Account.Status)
	assert.Equal(t, false, fixture.Account.Config["outbound_enabled"])
	assert.NotEmpty(t, fixture.Account.LastError)
}

func TestThreadsCredentialRefreshRecoversStaleClaim(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()
	fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
	fixture.Credential.Metadata[threadsCredentialRefreshClaimTokenKey] = "stale-claim"
	fixture.Credential.Metadata[threadsCredentialRefreshClaimTimestampKey] = now.
		Add(-threadsCredentialRefreshClaimTTL - time.Minute).
		Format(time.RFC3339Nano)
	require.NoError(t, db.Model(&fixture.Credential).Update("metadata", fixture.Credential.Metadata).Error)

	claim, processed, err := threadsRefreshTestWorker(db).claimThreadsCredentialRefresh(
		fixture.Organization.ID,
		now.Add(defaultThreadsCredentialRefreshWindow),
		now,
	)
	require.NoError(t, err)
	require.True(t, processed)
	require.NotNil(t, claim)
	assert.NotEqual(t, "stale-claim", claim.Token)

	var stored models.ChannelCredential
	require.NoError(t, db.First(&stored, "id = ?", fixture.Credential.ID).Error)
	assert.Equal(t, claim.Token, stored.Metadata[threadsCredentialRefreshClaimTokenKey])
	assert.True(t, threadsCredentialRefreshClaimActive(stored.Metadata, now))
}

func TestThreadsCredentialRefreshRejectsCredentialFromDifferentApp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()
	fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
	fixture.Credential.Metadata["app_id"] = nextThreadsRefreshTestAppID()
	require.NoError(t, db.Model(&fixture.Credential).
		Update("metadata", fixture.Credential.Metadata).Error)
	var providerCalls int

	processed, err := threadsRefreshTestWorker(db).refreshThreadsCredentialWithProvider(
		context.Background(),
		fixture.Organization.ID,
		now.Add(defaultThreadsCredentialRefreshWindow),
		func(context.Context, *models.ChannelAccount) (channelapi.CredentialRefreshResult, error) {
			providerCalls++
			return channelapi.CredentialRefreshResult{}, nil
		},
	)
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Zero(t, providerCalls)
}

func TestThreadsCredentialRefreshReleasesClaimAfterProviderFailure(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()
	fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
	worker := threadsRefreshTestWorker(db)

	processed, err := worker.refreshThreadsCredentialWithProvider(
		context.Background(),
		fixture.Organization.ID,
		now.Add(defaultThreadsCredentialRefreshWindow),
		func(context.Context, *models.ChannelAccount) (channelapi.CredentialRefreshResult, error) {
			return channelapi.CredentialRefreshResult{}, &channelapi.ProviderError{
				Provider:  channelapi.ThreadsProvider,
				Operation: "refresh_token",
				Code:      "temporarily_unavailable",
				Message:   "Threads refresh is temporarily unavailable",
				Retryable: true,
			}
		},
	)
	require.NoError(t, err)
	assert.True(t, processed)

	require.NoError(t, db.First(&fixture.Credential, "id = ?", fixture.Credential.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, fixture.Credential.Status)
	assert.Empty(t, fixture.Credential.Metadata[threadsCredentialRefreshClaimTokenKey])
	assert.Empty(t, fixture.Credential.Metadata[threadsCredentialRefreshClaimTimestampKey])
	assert.Contains(t, fixture.Credential.ValidationError, "temporarily unavailable")
	require.NoError(t, db.First(&fixture.Account, "id = ?", fixture.Account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, fixture.Account.Status)
	assert.Equal(t, true, fixture.Account.Config["outbound_enabled"])
	assert.Contains(t, fixture.Account.LastError, "temporarily unavailable")
}

func TestThreadsCredentialRefreshRotatesAfterClaimAndPreservesAppID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()
	fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
	worker := threadsRefreshTestWorker(db)
	refreshedExpiry := now.Add(30 * 24 * time.Hour)

	processed, err := worker.refreshThreadsCredentialWithProvider(
		context.Background(),
		fixture.Organization.ID,
		now.Add(defaultThreadsCredentialRefreshWindow),
		func(_ context.Context, account *models.ChannelAccount) (channelapi.CredentialRefreshResult, error) {
			require.Len(t, account.Credentials, 1)
			assert.Equal(t, models.ChannelCredentialStatusActive, account.Credentials[0].Status)
			assert.NotEmpty(t, account.Credentials[0].Metadata[threadsCredentialRefreshClaimTokenKey])
			return channelapi.CredentialRefreshResult{
				CredentialBlob: models.JSONB{"access_token": "encrypted-refreshed-token"},
				KeyVersion:     "app:v1",
				ExpiresAt:      &refreshedExpiry,
				Metadata: models.JSONB{
					"granted_scopes":         []string{"threads_basic", "threads_content_publish"},
					"permissions_checked_at": now.Format(time.RFC3339Nano),
				},
			}, nil
		},
	)
	require.NoError(t, err)
	assert.True(t, processed)

	var credentials []models.ChannelCredential
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
	).Order("version ASC").Find(&credentials).Error)
	require.Len(t, credentials, 2)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, credentials[0].Status)
	assert.Empty(t, credentials[0].Metadata[threadsCredentialRefreshClaimTokenKey])
	assert.Equal(t, models.ChannelCredentialStatusActive, credentials[1].Status)
	assert.Equal(t, fixture.AppID, credentials[1].Metadata["app_id"])
	assert.Empty(t, credentials[1].Metadata[threadsCredentialRefreshClaimTokenKey])
}

func TestThreadsCredentialRefreshAppChangeWinsDuringProviderIO(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()
	fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
	worker := threadsRefreshTestWorker(db)
	refreshedExpiry := now.Add(30 * 24 * time.Hour)
	newAppID := nextThreadsRefreshTestAppID()
	var mutationErr error

	processed, err := worker.refreshThreadsCredentialWithProvider(
		context.Background(),
		fixture.Organization.ID,
		now.Add(defaultThreadsCredentialRefreshWindow),
		func(context.Context, *models.ChannelAccount) (channelapi.CredentialRefreshResult, error) {
			mutationContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			mutationErr = db.WithContext(mutationContext).Transaction(func(tx *gorm.DB) error {
				// Acquiring this account lock proves the provider callback is not
				// running inside the refresh claim transaction.
				var account models.ChannelAccount
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					First(&account, "id = ?", fixture.Account.ID).Error; err != nil {
					return err
				}
				account.Metadata = cloneThreadsRefreshJSONB(account.Metadata)
				account.Metadata["app_id"] = newAppID
				if err := tx.Save(&account).Error; err != nil {
					return err
				}
				var integration models.ProviderIntegration
				if err := tx.First(&integration, "id = ?", fixture.Integration.ID).Error; err != nil {
					return err
				}
				integration.Config = cloneThreadsRefreshJSONB(integration.Config)
				integration.Config["app_id"] = newAppID
				integration.ThreadsAppID = &newAppID
				return tx.Save(&integration).Error
			})
			return channelapi.CredentialRefreshResult{
				CredentialBlob: models.JSONB{"access_token": "must-not-be-stored"},
				KeyVersion:     "app:v1",
				ExpiresAt:      &refreshedExpiry,
				Metadata:       models.JSONB{},
			}, nil
		},
	)
	require.NoError(t, mutationErr)
	require.NoError(t, err)
	assert.True(t, processed)

	var credentials []models.ChannelCredential
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
	).Find(&credentials).Error)
	require.Len(t, credentials, 1)
	assert.Equal(t, models.ChannelCredentialStatusActive, credentials[0].Status)
	assert.Empty(t, credentials[0].Metadata[threadsCredentialRefreshClaimTokenKey])
	require.NoError(t, db.First(&fixture.Account, "id = ?", fixture.Account.ID).Error)
	assert.Equal(t, newAppID, fixture.Account.Metadata["app_id"])
}

func TestThreadsCredentialRefreshConnectionChangesPreventStaleRotation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, threadsRefreshTestFixture, time.Time)
		want   []models.ChannelCredentialStatus
	}{
		{
			name: "disconnect",
			mutate: func(t *testing.T, tx *gorm.DB, fixture threadsRefreshTestFixture, now time.Time) {
				t.Helper()
				var account models.ChannelAccount
				require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					First(&account, "id = ?", fixture.Account.ID).Error)
				account.Status = models.ChannelAccountStatusDisconnected
				account.Config = cloneThreadsRefreshJSONB(account.Config)
				account.Config["outbound_enabled"] = false
				require.NoError(t, tx.Save(&account).Error)
				var credential models.ChannelCredential
				require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					First(&credential, "id = ?", fixture.Credential.ID).Error)
				credential.Status = models.ChannelCredentialStatusRevoked
				credential.RevokedAt = &now
				require.NoError(t, tx.Save(&credential).Error)
			},
			want: []models.ChannelCredentialStatus{models.ChannelCredentialStatusRevoked},
		},
		{
			name: "reauthorization",
			mutate: func(t *testing.T, tx *gorm.DB, fixture threadsRefreshTestFixture, now time.Time) {
				t.Helper()
				var account models.ChannelAccount
				require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					First(&account, "id = ?", fixture.Account.ID).Error)
				var credential models.ChannelCredential
				require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					First(&credential, "id = ?", fixture.Credential.ID).Error)
				credential.Status = models.ChannelCredentialStatusRevoked
				credential.RevokedAt = &now
				require.NoError(t, tx.Save(&credential).Error)
				reauthorizedExpiry := now.Add(45 * 24 * time.Hour)
				require.NoError(t, tx.Create(&models.ChannelCredential{
					BaseModel:        models.BaseModel{ID: uuid.New()},
					OrganizationID:   fixture.Organization.ID,
					ChannelAccountID: fixture.Account.ID,
					Kind:             models.ChannelCredentialKindOAuth,
					Version:          2,
					CredentialBlob:   models.JSONB{"access_token": "reauthorized-token"},
					Status:           models.ChannelCredentialStatusActive,
					KeyVersion:       "app:v1",
					ExpiresAt:        &reauthorizedExpiry,
					Metadata:         models.JSONB{"app_id": fixture.AppID},
				}).Error)
			},
			want: []models.ChannelCredentialStatus{
				models.ChannelCredentialStatusRevoked,
				models.ChannelCredentialStatusActive,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			now := time.Now().UTC()
			fixture := createThreadsRefreshTestFixture(t, db, now.Add(time.Hour))
			refreshedExpiry := now.Add(30 * 24 * time.Hour)
			var mutationErr error

			processed, err := threadsRefreshTestWorker(db).refreshThreadsCredentialWithProvider(
				context.Background(),
				fixture.Organization.ID,
				now.Add(defaultThreadsCredentialRefreshWindow),
				func(context.Context, *models.ChannelAccount) (channelapi.CredentialRefreshResult, error) {
					mutationContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					mutationErr = db.WithContext(mutationContext).Transaction(func(tx *gorm.DB) error {
						test.mutate(t, tx, fixture, now)
						return nil
					})
					return channelapi.CredentialRefreshResult{
						CredentialBlob: models.JSONB{"access_token": "must-not-be-stored"},
						KeyVersion:     "app:v1",
						ExpiresAt:      &refreshedExpiry,
						Metadata:       models.JSONB{},
					}, nil
				},
			)
			require.NoError(t, mutationErr)
			require.NoError(t, err)
			assert.True(t, processed)

			var credentials []models.ChannelCredential
			require.NoError(t, db.Where(
				"organization_id = ? AND channel_account_id = ?",
				fixture.Organization.ID,
				fixture.Account.ID,
			).Order("version ASC").Find(&credentials).Error)
			require.Len(t, credentials, len(test.want))
			for index, expected := range test.want {
				assert.Equal(t, expected, credentials[index].Status)
				assert.Empty(t, credentials[index].Metadata[threadsCredentialRefreshClaimTokenKey])
			}
		})
	}
}

func TestThreadsCredentialRefreshClaimLiveness(t *testing.T) {
	now := time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC)
	recent := models.JSONB{
		threadsCredentialRefreshClaimTokenKey:     "claim-token",
		threadsCredentialRefreshClaimTimestampKey: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	assert.True(t, threadsCredentialRefreshClaimActive(recent, now))

	stale := cloneThreadsRefreshJSONB(recent)
	stale[threadsCredentialRefreshClaimTimestampKey] = now.
		Add(-threadsCredentialRefreshClaimTTL - time.Nanosecond).
		Format(time.RFC3339Nano)
	assert.False(t, threadsCredentialRefreshClaimActive(stale, now))
	assert.False(t, threadsCredentialRefreshClaimActive(models.JSONB{
		threadsCredentialRefreshClaimTokenKey:     "claim-token",
		threadsCredentialRefreshClaimTimestampKey: "not-a-timestamp",
	}, now))
}

func createThreadsRefreshTestFixture(
	t *testing.T,
	db *gorm.DB,
	expiresAt time.Time,
) threadsRefreshTestFixture {
	t.Helper()
	organization := testutil.CreateTestOrganization(t, db)
	appID := nextThreadsRefreshTestAppID()
	integration := models.ProviderIntegration{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		Provider:        channelapi.ThreadsProvider,
		ThreadsAppID:    &appID,
		Enabled:         true,
		Config:          models.JSONB{"app_id": appID},
		CredentialData:  models.JSONB{},
		ValidationToken: "",
	}
	require.NoError(t, db.Create(&integration).Error)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              "Threads credential refresh test " + appID,
		ExternalAccountID: appID,
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{"text": true, "replies": true},
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{"app_id": appID},
	}
	require.NoError(t, db.Create(&account).Error)
	encrypted, err := appcrypto.Encrypt("threads-token-"+appID, threadsTestWorkerEncryptionKey)
	require.NoError(t, err)
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": encrypted},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		ExpiresAt:        &expiresAt,
		Metadata:         models.JSONB{"app_id": appID},
	}
	require.NoError(t, db.Create(&credential).Error)
	return threadsRefreshTestFixture{
		Organization: organization,
		Account:      account,
		Credential:   credential,
		Integration:  integration,
		AppID:        appID,
	}
}

func threadsRefreshTestWorker(db *gorm.DB) *Worker {
	return &Worker{
		DB:     db,
		Config: &config.Config{App: config.AppConfig{EncryptionKey: threadsTestWorkerEncryptionKey}},
		Log:    testutil.NopLogger(),
	}
}

func nextThreadsRefreshTestAppID() string {
	return fmt.Sprintf("1331429%09d", threadsRefreshTestAppSequence.Add(1))
}
