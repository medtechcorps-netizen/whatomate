package worker

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type managedInstagramAIGenerationFixture struct {
	db      *gorm.DB
	fixture *channelAIReplyFixture
	worker  *Worker
	oauth   models.ChannelCredential
	webhook models.ChannelCredential
}

func TestManagedInstagramChannelAIGenerationExactBindingCompletes(t *testing.T) {
	var qwenCalls atomic.Int32
	fixture := createManagedInstagramAIGenerationFixture(
		t,
		func(_ *http.Request) (*http.Response, error) {
			qwenCalls.Add(1)
			return channelAIReplyQwenResponse("Synthetic authorized reply"), nil
		},
	)
	const workerID = "managed-instagram-ai-exact"
	jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, fixture.worker.processChannelAIReplyJob(
		context.Background(),
		fixture.fixture.Organization.ID,
		jobID,
		workerID,
	))

	assert.EqualValues(t, 1, qwenCalls.Load())
	var job models.ScheduledJob
	require.NoError(t, fixture.db.First(&job, "id = ?", jobID).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	idempotencyKey := models.ChannelAIReplyIdempotencyKey(fixture.fixture.Inbound.ID)
	var outboxCount, messageCount int64
	require.NoError(t, fixture.db.Model(&models.OutboxJob{}).Where(
		"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
		fixture.fixture.Organization.ID,
		fixture.fixture.Account.ID,
		idempotencyKey,
	).Count(&outboxCount).Error)
	require.NoError(t, fixture.db.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ? AND content = ?",
		fixture.fixture.Organization.ID,
		channelAIReplyMessageID(idempotencyKey),
		"Synthetic authorized reply",
	).Count(&messageCount).Error)
	assert.EqualValues(t, 1, outboxCount)
	assert.EqualValues(t, 1, messageCount)
}

func TestManagedInstagramChannelAIGenerationRequiresExactRuntimeFence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *managedInstagramAIGenerationFixture)
	}{
		{
			name: "pilot is quarantine only",
			mutate: func(_ *testing.T, fixture *managedInstagramAIGenerationFixture) {
				fixture.worker.Config.MetaInstagram.QuarantineOnly = true
			},
		},
		{
			name: "both management markers were stripped but platform residue remains",
			mutate: func(t *testing.T, fixture *managedInstagramAIGenerationFixture) {
				accountConfig := cloneChannelOutboxTestJSONB(fixture.fixture.Account.Config)
				delete(accountConfig, "meta_registry_managed")
				delete(accountConfig, "meta_management_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?",
					fixture.fixture.Account.ID,
					fixture.fixture.Organization.ID,
				).Update("config", accountConfig).Error)
			},
		},
		{
			name: "data deletion callback is pending",
			mutate: func(t *testing.T, fixture *managedInstagramAIGenerationFixture) {
				metadata := cloneChannelOutboxTestJSONB(fixture.fixture.Account.Metadata)
				metadata["meta_data_deletion_pending_digest"] = "synthetic-pending-digest"
				metadata["meta_data_deletion_pending_issued_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?",
					fixture.fixture.Account.ID,
					fixture.fixture.Organization.ID,
				).Update("metadata", metadata).Error)
			},
		},
		{
			name: "subscription credential generation drifted",
			mutate: func(t *testing.T, fixture *managedInstagramAIGenerationFixture) {
				metadata := cloneChannelOutboxTestJSONB(fixture.fixture.Account.Metadata)
				metadata["meta_subscription_oauth_version"] = fixture.oauth.Version + 1
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?",
					fixture.fixture.Account.ID,
					fixture.fixture.Organization.ID,
				).Update("metadata", metadata).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var qwenCalls atomic.Int32
			fixture := createManagedInstagramAIGenerationFixture(
				t,
				func(_ *http.Request) (*http.Response, error) {
					qwenCalls.Add(1)
					return channelAIReplyQwenResponse("must not be generated"), nil
				},
			)
			test.mutate(t, &fixture)

			const workerID = "managed-instagram-ai-drift"
			jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
				fixture.fixture.Organization.ID,
				workerID,
			)
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, fixture.worker.processChannelAIReplyJob(
				context.Background(),
				fixture.fixture.Organization.ID,
				jobID,
				workerID,
			))

			assert.Zero(t, qwenCalls.Load())
			assertManagedInstagramAIGenerationProducedNothing(t, fixture)
			var job models.ScheduledJob
			require.NoError(t, fixture.db.First(&job, "id = ?", fixture.fixture.Job.ID).Error)
			assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
			assert.Equal(t, "managed_instagram_generation_not_authorized", job.LastError)
		})
	}
}

func TestManagedInstagramChannelAIGenerationDowngradeWinsOrganizationMutex(t *testing.T) {
	var qwenCalls atomic.Int32
	fixture := createManagedInstagramAIGenerationFixture(
		t,
		func(_ *http.Request) (*http.Response, error) {
			qwenCalls.Add(1)
			return channelAIReplyQwenResponse("must not be generated"), nil
		},
	)
	const workerID = "managed-instagram-ai-downgrade"
	jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)

	downgradeTx := fixture.db.Begin()
	require.NoError(t, downgradeTx.Error)
	t.Cleanup(func() { _ = downgradeTx.Rollback().Error })
	require.NoError(t, lockChannelOutboxOrganizationScopeTx(
		downgradeTx,
		fixture.fixture.Organization.ID,
	))
	var account models.ChannelAccount
	require.NoError(t, downgradeTx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ?",
		fixture.fixture.Account.ID,
		fixture.fixture.Organization.ID,
	).First(&account).Error)
	metadata := cloneChannelOutboxTestJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_reason"] = "synthetic_downgrade_won"
	metadata["meta_ownership_checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	accountConfig := cloneChannelOutboxTestJSONB(account.Config)
	accountConfig["outbound_enabled"] = false
	accountConfig["ai_reply_enabled"] = false
	require.NoError(t, downgradeTx.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		fixture.fixture.Organization.ID,
	).Updates(map[string]any{
		"status":   models.ChannelAccountStatusDegraded,
		"config":   accountConfig,
		"metadata": metadata,
	}).Error)
	result := downgradeTx.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
		jobID,
		fixture.fixture.Organization.ID,
		models.ScheduledJobStatusProcessing,
		workerID,
	).Updates(map[string]any{
		"status":       models.ScheduledJobStatusCancelled,
		"completed_at": time.Now().UTC(),
		"last_error":   "synthetic lifecycle downgrade",
		"locked_at":    nil,
		"locked_by":    "",
	})
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)

	workerPID := make(chan int, 1)
	workerDone := make(chan error, 1)
	go func() {
		_ = fixture.db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				workerPID <- 0
				workerDone <- err
				return nil
			}
			workerPID <- backendPID
			threadWorker := *fixture.worker
			threadWorker.DB = connection
			workerDone <- threadWorker.processChannelAIReplyJob(
				context.Background(),
				fixture.fixture.Organization.ID,
				jobID,
				workerID,
			)
			return nil
		})
	}()
	backendPID := <-workerPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, downgradeTx.Commit().Error)
	select {
	case err := <-workerDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed Instagram AI generation did not resume after downgrade committed")
	}

	assert.Zero(t, qwenCalls.Load())
	assertManagedInstagramAIGenerationProducedNothing(t, fixture)
	var job models.ScheduledJob
	require.NoError(t, fixture.db.First(&job, "id = ?", jobID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assert.Equal(t, "synthetic lifecycle downgrade", job.LastError)
}

func TestManagedInstagramChannelAIFinalizationRejectsPostQwenDrift(t *testing.T) {
	var qwenCalls atomic.Int32
	var statusAtQwen models.ScheduledJobStatus
	var fixture managedInstagramAIGenerationFixture
	fixture = createManagedInstagramAIGenerationFixture(
		t,
		func(_ *http.Request) (*http.Response, error) {
			qwenCalls.Add(1)
			var observedJob models.ScheduledJob
			require.NoError(t, fixture.db.Select("status").Where(
				"id = ? AND organization_id = ?",
				fixture.fixture.Job.ID,
				fixture.fixture.Organization.ID,
			).First(&observedJob).Error)
			statusAtQwen = observedJob.Status
			require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelOutboxOrganizationScopeTx(
					tx,
					fixture.fixture.Organization.ID,
				); err != nil {
					return err
				}
				var account models.ChannelAccount
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
					"id = ? AND organization_id = ?",
					fixture.fixture.Account.ID,
					fixture.fixture.Organization.ID,
				).First(&account).Error; err != nil {
					return err
				}
				metadata := cloneChannelOutboxTestJSONB(account.Metadata)
				metadata["meta_ownership_state"] = metaregistry.OwnershipStale
				metadata["meta_ownership_reason"] = "synthetic_post_qwen_downgrade"
				accountConfig := cloneChannelOutboxTestJSONB(account.Config)
				accountConfig["outbound_enabled"] = false
				accountConfig["ai_reply_enabled"] = false
				if err := tx.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?",
					account.ID,
					fixture.fixture.Organization.ID,
				).Updates(map[string]any{
					"status":   models.ChannelAccountStatusDegraded,
					"config":   accountConfig,
					"metadata": metadata,
				}).Error; err != nil {
					return err
				}
				cancel := tx.Model(&models.ScheduledJob{}).Where(
					"id = ? AND organization_id = ? AND status = ?",
					fixture.fixture.Job.ID,
					fixture.fixture.Organization.ID,
					models.ScheduledJobStatusProcessing,
				).Update("status", models.ScheduledJobStatusCancelled)
				if cancel.Error != nil {
					return cancel.Error
				}
				require.Zero(t, cancel.RowsAffected, "generating is the Qwen point of no return")
				return nil
			}))
			return channelAIReplyQwenResponse("must not be persisted"), nil
		},
	)
	const workerID = "managed-instagram-ai-final-drift"
	jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, fixture.worker.processChannelAIReplyJob(
		context.Background(),
		fixture.fixture.Organization.ID,
		jobID,
		workerID,
	))

	assert.EqualValues(t, 1, qwenCalls.Load())
	assert.Equal(t, models.ScheduledJobStatusGenerating, statusAtQwen)
	assertManagedInstagramAIGenerationProducedNothing(t, fixture)
	var job models.ScheduledJob
	require.NoError(t, fixture.db.First(&job, "id = ?", jobID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assert.Equal(t, "channel_account_outbound_disabled", job.LastError)
}

func TestManagedInstagramStaleGeneratingAfterRotationIsTerminal(t *testing.T) {
	var qwenCalls atomic.Int32
	fixture := createManagedInstagramAIGenerationFixture(
		t,
		func(_ *http.Request) (*http.Response, error) {
			qwenCalls.Add(1)
			return channelAIReplyQwenResponse("must not be generated twice"), nil
		},
	)
	staleAt := time.Now().UTC().Add(-defaultChannelAIReplyLease - time.Minute)
	require.NoError(t, fixture.db.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ?",
		fixture.fixture.Job.ID,
		fixture.fixture.Organization.ID,
	).Updates(map[string]any{
		"status":    models.ScheduledJobStatusGenerating,
		"locked_at": staleAt,
		"locked_by": "dead-worker",
	}).Error)
	rotateManagedInstagramAIGeneration(t, &fixture)

	jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		"replacement-worker",
	)
	require.NoError(t, err)
	require.False(t, claimed)
	assert.Equal(t, uuid.Nil, jobID)
	var job models.ScheduledJob
	require.NoError(t, fixture.db.First(&job, "id = ?", fixture.fixture.Job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assert.Equal(t, "managed_instagram_generation_ambiguous_after_lease_loss", job.LastError)
	assert.Empty(t, job.LockedBy)
	assert.Zero(t, qwenCalls.Load(), "an ambiguous provider attempt must never be replayed under N+1")
	assertManagedInstagramAIGenerationProducedNothing(t, fixture)
}

func TestManagedInstagramQwenErrorAfterRotationIsTerminal(t *testing.T) {
	var qwenCalls atomic.Int32
	var fixture managedInstagramAIGenerationFixture
	fixture = createManagedInstagramAIGenerationFixture(
		t,
		func(_ *http.Request) (*http.Response, error) {
			qwenCalls.Add(1)
			rotateManagedInstagramAIGeneration(t, &fixture)
			return nil, errors.New("synthetic Qwen transport ambiguity")
		},
	)
	const workerID = "managed-instagram-ai-error-rotation"
	jobID, claimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, fixture.worker.processChannelAIReplyJob(
		context.Background(),
		fixture.fixture.Organization.ID,
		jobID,
		workerID,
	))

	assert.EqualValues(t, 1, qwenCalls.Load())
	var job models.ScheduledJob
	require.NoError(t, fixture.db.First(&job, "id = ?", jobID).Error)
	assert.Equal(t, models.ScheduledJobStatusFailed, job.Status)
	assert.Contains(t, job.LastError, "managed_instagram_generation_ambiguous_after_qwen_error")
	assert.NotNil(t, job.CompletedAt)
	assertManagedInstagramAIGenerationProducedNothing(t, fixture)
	retryID, retryClaimed, err := fixture.worker.claimChannelAIReplyJob(
		fixture.fixture.Organization.ID,
		"must-not-retry-under-n-plus-one",
	)
	require.NoError(t, err)
	assert.False(t, retryClaimed)
	assert.Equal(t, uuid.Nil, retryID)
}

func TestStaticInstagramAndMessengerKeepProcessingRetrySemantics(t *testing.T) {
	for _, channel := range []models.Channel{models.ChannelInstagram, models.ChannelMessenger} {
		t.Run(string(channel), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			fixture := createChannelAIReplyWorkerFixture(t, db)
			if channel == models.ChannelMessenger {
				require.NoError(t, db.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?",
					fixture.Account.ID,
					fixture.Organization.ID,
				).Update("channel", channel).Error)
				require.NoError(t, db.Model(&models.InboxConversation{}).Where(
					"id = ? AND organization_id = ?",
					fixture.Conversation.ID,
					fixture.Organization.ID,
				).Update("channel", channel).Error)
				require.NoError(t, db.Model(&models.ContactIdentity{}).Where(
					"id = ? AND organization_id = ?",
					fixture.Identity.ID,
					fixture.Organization.ID,
				).Update("channel", channel).Error)
			}
			var statusAtQwen models.ScheduledJobStatus
			worker := channelAIReplyTestWorker(db, func(_ *http.Request) (*http.Response, error) {
				var observedJob models.ScheduledJob
				require.NoError(t, db.Select("status").Where(
					"id = ? AND organization_id = ?",
					fixture.Job.ID,
					fixture.Organization.ID,
				).First(&observedJob).Error)
				statusAtQwen = observedJob.Status
				return nil, errors.New("synthetic retryable static Qwen error")
			})
			workerID := "static-ai-" + string(channel)
			jobID, claimed, err := worker.claimChannelAIReplyJob(
				fixture.Organization.ID,
				workerID,
			)
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, worker.processChannelAIReplyJob(
				context.Background(),
				fixture.Organization.ID,
				jobID,
				workerID,
			))
			assert.Equal(t, models.ScheduledJobStatusProcessing, statusAtQwen)
			var job models.ScheduledJob
			require.NoError(t, db.First(&job, "id = ?", jobID).Error)
			assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
			assert.Nil(t, job.CompletedAt)
		})
	}
}

func createManagedInstagramAIGenerationFixture(
	t *testing.T,
	roundTrip channelAIReplyRoundTripper,
) managedInstagramAIGenerationFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
	).Updates(map[string]any{
		"status":     models.ChannelCredentialStatusRevoked,
		"revoked_at": now,
	}).Error)

	profileID := nextManagedMetaOutboxProfileID()
	expiresAt := now.Add(2 * time.Hour)
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{"access_token": "synthetic-managed-generation-token"},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "test:v1",
		ExpiresAt: &expiresAt, Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  "synthetic-managed-generation-inbound",
			"outbound_secret": "synthetic-managed-generation-outbound",
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "test:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	metadata := models.JSONB{
		"meta_ownership_state":                    metaregistry.OwnershipVerified,
		"meta_ownership_checked_at":               now.Add(-time.Minute).Format(time.RFC3339Nano),
		"meta_platform_app_id":                    managedMetaOutboxTestAppID,
		"meta_webhook_app":                        "instagram_login",
		"meta_authorizing_user_id":                profileID,
		"meta_oauth_subject_id":                   profileID,
		"meta_authority_asset_id":                 profileID,
		"meta_authorization_token_kind":           "USER",
		"meta_authorized_at":                      now.Add(-time.Hour).Format(time.RFC3339Nano),
		"meta_granted_scopes":                     []string{"instagram_business_basic", "instagram_business_manage_messages"},
		"meta_release_evidence_mode":              "app_review_approved",
		"meta_release_review_status":              "approved",
		"meta_subscription_desired_state":         "subscribed",
		"meta_subscription_operation_id":          uuid.NewString(),
		"meta_subscription_operation_state":       "subscribe_complete",
		"meta_subscription_operation_expires_at":  now.Add(time.Hour).Format(time.RFC3339Nano),
		"meta_subscription_oauth_credential_id":   oauth.ID.String(),
		"meta_subscription_oauth_version":         oauth.Version,
		"meta_subscription_webhook_credential_id": webhook.ID.String(),
		"meta_subscription_webhook_version":       webhook.Version,
		"meta_subscription_remote_state":          "subscribed",
	}
	accountConfig := models.JSONB{
		"meta_registry_managed": true,
		"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
		"instagram_api_mode":    "instagram_login",
		"rereply_webhook_url": "https://app.example.test/api/webhooks/channels/" +
			fixture.Account.ID.String(),
		"relay_url":        "https://relay.example.test/v1/accounts/instagram/" + profileID,
		"outbound_enabled": true,
		"ai_reply_enabled": true,
	}
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?",
		fixture.Account.ID,
		fixture.Organization.ID,
	).Updates(map[string]any{
		"external_account_id": profileID,
		"config":              accountConfig,
		"metadata":            metadata,
	}).Error)
	fixture.Account.ExternalAccountID = profileID
	fixture.Account.Config = accountConfig
	fixture.Account.Metadata = metadata
	fixture.Account.Credentials = []models.ChannelCredential{oauth, webhook}

	worker := channelAIReplyTestWorker(db, roundTrip)
	worker.Config.App.Environment = "production"
	worker.Config.MetaRegistry.Enabled = true
	worker.Config.MetaRegistry.OwnershipMaxAgeMins = 24 * 60
	worker.Config.MetaInstagram.Enabled = true
	worker.Config.MetaInstagram.AppID = managedMetaOutboxTestAppID
	worker.Config.MetaInstagram.AppReviewStatus = "approved"
	worker.Config.MetaInstagram.AllowedOrganizationID = fixture.Organization.ID.String()
	worker.Config.MetaInstagram.ReReplyBaseURL = "https://app.example.test"
	worker.Config.MetaInstagram.RelayBaseURL = "https://relay.example.test"
	return managedInstagramAIGenerationFixture{
		db: db, fixture: fixture, worker: worker, oauth: oauth, webhook: webhook,
	}
}

func rotateManagedInstagramAIGeneration(
	t *testing.T,
	fixture *managedInstagramAIGenerationFixture,
) {
	t.Helper()
	require.NotNil(t, fixture)
	now := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelOutboxOrganizationScopeTx(
			tx,
			fixture.fixture.Organization.ID,
		); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			fixture.fixture.Account.ID,
			fixture.fixture.Organization.ID,
		).First(&account).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ?",
			fixture.fixture.Organization.ID,
			fixture.fixture.Account.ID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		).Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
		}).Error; err != nil {
			return err
		}
		expiresAt := now.Add(2 * time.Hour)
		oauth := models.ChannelCredential{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.fixture.Organization.ID,
			ChannelAccountID: fixture.fixture.Account.ID,
			Kind:             models.ChannelCredentialKindOAuth, Version: fixture.oauth.Version + 1,
			CredentialBlob: models.JSONB{"access_token": "synthetic-rotated-generation-token"},
			Status:         models.ChannelCredentialStatusActive, KeyVersion: "test:v2",
			ExpiresAt: &expiresAt, Metadata: models.JSONB{},
		}
		webhook := models.ChannelCredential{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.fixture.Organization.ID,
			ChannelAccountID: fixture.fixture.Account.ID,
			Kind:             models.ChannelCredentialKindWebhook, Version: fixture.webhook.Version + 1,
			CredentialBlob: models.JSONB{
				"inbound_secret":  "synthetic-rotated-generation-inbound",
				"outbound_secret": "synthetic-rotated-generation-outbound",
			},
			Status: models.ChannelCredentialStatusActive, KeyVersion: "test:v2", Metadata: models.JSONB{},
		}
		if err := tx.Create(&oauth).Error; err != nil {
			return err
		}
		if err := tx.Create(&webhook).Error; err != nil {
			return err
		}
		metadata := cloneChannelOutboxTestJSONB(account.Metadata)
		metadata["meta_subscription_operation_id"] = uuid.NewString()
		metadata["meta_subscription_operation_expires_at"] = now.Add(time.Hour).Format(time.RFC3339Nano)
		metadata["meta_subscription_oauth_credential_id"] = oauth.ID.String()
		metadata["meta_subscription_oauth_version"] = oauth.Version
		metadata["meta_subscription_webhook_credential_id"] = webhook.ID.String()
		metadata["meta_subscription_webhook_version"] = webhook.Version
		if err := tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?",
			account.ID,
			fixture.fixture.Organization.ID,
		).Update("metadata", metadata).Error; err != nil {
			return err
		}
		// Reconnect/lifecycle cancellation targets pending and processing. The
		// original provider attempt already crossed generating, so rotation must
		// not convert it into a retry under this new pair.
		cancelled := tx.Model(&models.ScheduledJob{}).Where(
			"id = ? AND organization_id = ? AND status IN ?",
			fixture.fixture.Job.ID,
			fixture.fixture.Organization.ID,
			[]models.ScheduledJobStatus{
				models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing,
			},
		).Update("status", models.ScheduledJobStatusCancelled)
		if cancelled.Error != nil {
			return cancelled.Error
		}
		require.Zero(t, cancelled.RowsAffected)
		return nil
	}))
}

func assertManagedInstagramAIGenerationProducedNothing(
	t *testing.T,
	fixture managedInstagramAIGenerationFixture,
) {
	t.Helper()
	idempotencyKey := models.ChannelAIReplyIdempotencyKey(fixture.fixture.Inbound.ID)
	var outboxCount, messageCount int64
	require.NoError(t, fixture.db.Model(&models.OutboxJob{}).Where(
		"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
		fixture.fixture.Organization.ID,
		fixture.fixture.Account.ID,
		idempotencyKey,
	).Count(&outboxCount).Error)
	require.NoError(t, fixture.db.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ?",
		fixture.fixture.Organization.ID,
		channelAIReplyMessageID(idempotencyKey),
	).Count(&messageCount).Error)
	assert.Zero(t, outboxCount)
	assert.Zero(t, messageCount)
}
