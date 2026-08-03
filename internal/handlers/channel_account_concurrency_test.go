package handlers

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type channelAccountConcurrencyFixture struct {
	App          *App
	Organization *models.Organization
	User         *models.User
	Account      *models.ChannelAccount
}

func TestChannelMessageEnqueueWaitsForDisconnectAndFailsClosed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, organization.ID)
	require.NoError(t, db.Create(&models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: conversation.ChannelAccountID,
		Kind:             models.ChannelCredentialKindPrimary,
		Version:          1,
		CredentialBlob:   models.JSONB{"test": "credential"},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test:v1",
		Metadata:         models.JSONB{},
	}).Error)

	disconnectTx := db.Begin()
	require.NoError(t, disconnectTx.Error)
	t.Cleanup(func() { _ = disconnectTx.Rollback().Error })
	var account models.ChannelAccount
	require.NoError(t, disconnectTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"id = ? AND organization_id = ?",
			conversation.ChannelAccountID,
			organization.ID,
		).
		First(&account).Error)
	account.Status = models.ChannelAccountStatusDisconnected
	account.Config["outbound_enabled"] = false
	require.NoError(t, disconnectTx.Save(&account).Error)

	enqueuePID := make(chan int, 1)
	enqueueDone := make(chan error, 1)
	go func() {
		enqueueDone <- db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				enqueuePID <- 0
				return err
			}
			enqueuePID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				return lockChannelAccountForMessageEnqueue(
					tx,
					organization.ID,
					conversation.ChannelAccountID,
					conversation.Channel,
					time.Now().UTC(),
				)
			})
		})
	}()

	backendPID := <-enqueuePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)
	require.NoError(t, disconnectTx.Commit().Error)

	select {
	case err := <-enqueueDone:
		assert.True(t, errors.Is(err, errChannelAccountUnavailableAtEnqueue), err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "message enqueue did not resume after disconnect committed")
	}
}

func TestChannelAIEnqueueSerializesBeforeAccountDisableCancellation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, organization.ID)

	enqueueTx := db.Begin()
	require.NoError(t, enqueueTx.Error)
	t.Cleanup(func() {
		_ = enqueueTx.Rollback().Error
	})
	require.NoError(
		t,
		lockChannelAIOrganizationScopeTx(enqueueTx, organization.ID),
	)
	job := createAIControlScheduledJob(
		t,
		enqueueTx,
		organization.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusPending,
	)

	disablePID := make(chan int, 1)
	disableDone := make(chan error, 1)
	go func() {
		disableDone <- db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				disablePID <- 0
				return err
			}
			disablePID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelAIOrganizationScopeTx(
					tx,
					organization.ID,
				); err != nil {
					return err
				}
				var account models.ChannelAccount
				if err := tx.Where(
					"id = ? AND organization_id = ?",
					conversation.ChannelAccountID,
					organization.ID,
				).First(&account).Error; err != nil {
					return err
				}
				disabled := false
				if err := applyChannelAIReplyOptIn(&account, &disabled); err != nil {
					return err
				}
				if err := tx.Model(&models.ChannelAccount{}).
					Where(
						"id = ? AND organization_id = ?",
						account.ID,
						organization.ID,
					).
					Update("config", account.Config).Error; err != nil {
					return err
				}
				return cancelChannelAIReplyJobsForAccountTx(
					tx,
					organization.ID,
					account.ID,
					"channel_ai_disabled",
				)
			})
		})
	}()

	backendPID := <-disablePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)
	require.NoError(t, enqueueTx.Commit().Error)

	select {
	case err := <-disableDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "account disable did not resume after enqueue committed")
	}

	assertChannelAccountConcurrencyJobCancelled(
		t,
		db,
		job.ID,
		"channel_ai_disabled",
	)
	var account models.ChannelAccount
	require.NoError(t, db.First(
		&account,
		"id = ? AND organization_id = ?",
		conversation.ChannelAccountID,
		organization.ID,
	).Error)
	assert.Equal(t, false, account.Config["ai_reply_enabled"])
}

func TestChannelAIEnqueueSerializesBeforeConversationPauseCancellation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, organization.ID)

	enqueueTx := db.Begin()
	require.NoError(t, enqueueTx.Error)
	t.Cleanup(func() {
		_ = enqueueTx.Rollback().Error
	})
	require.NoError(
		t,
		lockChannelAIOrganizationScopeTx(enqueueTx, organization.ID),
	)
	job := createAIControlScheduledJob(
		t,
		enqueueTx,
		organization.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusPending,
	)

	pausePID := make(chan int, 1)
	pauseDone := make(chan error, 1)
	go func() {
		pauseDone <- db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				pausePID <- 0
				return err
			}
			pausePID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				_, err := setInboxConversationAIStateTx(
					tx,
					organization.ID,
					conversation.ID,
					true,
					nil,
					"manual_pause",
				)
				return err
			})
		})
	}()

	backendPID := <-pausePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)
	require.NoError(t, enqueueTx.Commit().Error)

	select {
	case err := <-pauseDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "conversation pause did not resume after enqueue committed")
	}

	assertChannelAccountConcurrencyJobCancelled(
		t,
		db,
		job.ID,
		"conversation_ai_paused",
	)
	var persisted models.InboxConversation
	require.NoError(t, db.First(
		&persisted,
		"id = ? AND organization_id = ?",
		conversation.ID,
		organization.ID,
	).Error)
	assert.True(t, inboxConversationAIIsPaused(persisted.Config))
}

func TestUpdateChannelAccountAIOptInProducesConfigAuditChange(t *testing.T) {
	fixture := newChannelAccountConcurrencyFixture(t, false)

	updateChannelAccountForConcurrencyTest(
		t,
		fixture,
		map[string]any{"ai_reply_enabled": true},
	)

	var account models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&account, "id = ?", fixture.Account.ID).Error)
	require.Equal(t, true, account.Config["ai_reply_enabled"])

	var entry models.AuditLog
	require.NoError(t, fixture.App.DB.
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
			fixture.Organization.ID,
			"channel_account",
			fixture.Account.ID,
			models.AuditActionUpdated,
		).
		Order("created_at DESC").
		First(&entry).Error)

	change := requireChannelAccountAuditChange(t, entry.Changes, "config")
	oldConfig := requireChannelAccountAuditObject(t, change["old_value"])
	newConfig := requireChannelAccountAuditObject(t, change["new_value"])
	assert.Equal(t, false, oldConfig["ai_reply_enabled"])
	assert.Equal(t, true, newConfig["ai_reply_enabled"])
	assert.Equal(t, oldConfig["relay_url"], newConfig["relay_url"])
}

func TestUpdateChannelAccountRenameAndRetestCancelPriorAIJobs(t *testing.T) {
	fixture := newChannelAccountConcurrencyFixture(t, true)
	renameJob := createChannelAccountConcurrencyJob(
		t,
		fixture,
		models.ScheduledJobStatusPending,
	)
	renamed := "renamed-" + uuid.NewString()

	updateChannelAccountForConcurrencyTest(
		t,
		fixture,
		map[string]any{"name": renamed},
	)

	var afterRename models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&afterRename, "id = ?", fixture.Account.ID).Error)
	assert.Equal(t, renamed, afterRename.Name)
	assert.Equal(t, models.ChannelAccountStatusActive, afterRename.Status)
	assert.Equal(t, true, afterRename.Config["outbound_enabled"])
	assert.Equal(t, true, afterRename.Config["ai_reply_enabled"])
	assertChannelAccountConcurrencyJobCancelled(
		t,
		fixture.App.DB,
		renameJob.ID,
		"channel_account_profile_changed",
	)

	retestJob := createChannelAccountConcurrencyJob(
		t,
		fixture,
		models.ScheduledJobStatusProcessing,
	)
	updateChannelAccountForConcurrencyTest(
		t,
		fixture,
		map[string]any{
			"config": map[string]any{
				"relay_url": "https://relay-retest.example.com/meta",
			},
		},
	)

	var afterRetest models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&afterRetest, "id = ?", fixture.Account.ID).Error)
	assert.Equal(t, renamed, afterRetest.Name)
	assert.Equal(t, models.ChannelAccountStatusPending, afterRetest.Status)
	assert.Equal(t, false, afterRetest.Config["outbound_enabled"])
	assert.Equal(t, false, afterRetest.Config["ai_reply_enabled"])
	assert.Equal(t, "https://relay-retest.example.com/meta", afterRetest.Config["relay_url"])
	assertChannelAccountConcurrencyJobCancelled(
		t,
		fixture.App.DB,
		retestJob.ID,
		"channel_account_retest_required",
	)
}

func TestChannelAccountValidationFingerprintDetectsStaleInputs(t *testing.T) {
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    uuid.New(),
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "validation-profile",
		ExternalAccountID: "external-validation",
		Status:            models.ChannelAccountStatusPending,
		Config: models.JSONB{
			"relay_url":         "https://relay-one.example.com/meta",
			"outbound_enabled":  false,
			"ai_reply_enabled":  false,
			"validation_option": "one",
		},
		Capabilities: models.JSONB{"text": true},
		Metadata:     models.JSONB{"display_only": "ignored"},
		Credentials: []models.ChannelCredential{
			{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				Kind:           models.ChannelCredentialKindPrimary,
				Version:        1,
				CredentialBlob: models.JSONB{"signed_material": "ciphertext-one"},
				Status:         models.ChannelCredentialStatusActive,
				KeyVersion:     "key-version-one",
			},
			{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				Kind:           models.ChannelCredentialKindSigning,
				Version:        1,
				CredentialBlob: models.JSONB{"signed_material": "ciphertext-two"},
				Status:         models.ChannelCredentialStatusExpiring,
				KeyVersion:     "key-version-two",
			},
		},
	}

	validated, err := channelAccountValidationFingerprint(account)
	require.NoError(t, err)

	t.Run("display rename is stable", func(t *testing.T) {
		current := cloneChannelAccountForFingerprintTest(account)
		current.Name = "renamed-display-profile"
		fingerprint, err := channelAccountValidationFingerprint(&current)
		require.NoError(t, err)
		assert.Equal(t, validated, fingerprint)
	})

	t.Run("credential order is stable", func(t *testing.T) {
		current := cloneChannelAccountForFingerprintTest(account)
		current.Credentials[0], current.Credentials[1] =
			current.Credentials[1], current.Credentials[0]
		fingerprint, err := channelAccountValidationFingerprint(&current)
		require.NoError(t, err)
		assert.Equal(t, validated, fingerprint)
	})

	for _, testCase := range []struct {
		name   string
		mutate func(*models.ChannelAccount)
	}{
		{
			name: "relay config changed",
			mutate: func(current *models.ChannelAccount) {
				current.Config["relay_url"] = "https://relay-two.example.com/meta"
			},
		},
		{
			name: "external account changed",
			mutate: func(current *models.ChannelAccount) {
				current.ExternalAccountID = "external-validation-two"
			},
		},
		{
			name: "capability changed",
			mutate: func(current *models.ChannelAccount) {
				current.Capabilities["text"] = false
			},
		},
		{
			name: "credential changed",
			mutate: func(current *models.ChannelAccount) {
				current.Credentials[0].Version++
				current.Credentials[0].CredentialBlob["signed_material"] =
					"rotated-ciphertext"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := cloneChannelAccountForFingerprintTest(account)
			testCase.mutate(&current)
			fingerprint, err := channelAccountValidationFingerprint(&current)
			require.NoError(t, err)
			assert.NotEqual(
				t,
				validated,
				fingerprint,
				"stale provider validation must not apply after validation inputs change",
			)
		})
	}
}

func TestChannelAccountValidationGenerationSupersedesOlderResult(t *testing.T) {
	account := &models.ChannelAccount{Metadata: models.JSONB{}}
	olderToken := uuid.NewString()
	newerToken := uuid.NewString()

	account.Metadata[channelAccountHealthValidationTokenKey] = olderToken
	assert.True(t, channelAccountValidationTokenMatches(account, olderToken))

	account.Metadata[channelAccountHealthValidationTokenKey] = newerToken
	assert.False(t, channelAccountValidationTokenMatches(account, olderToken))
	assert.True(t, channelAccountValidationTokenMatches(account, newerToken))
}

func newChannelAccountConcurrencyFixture(
	t *testing.T,
	aiReplyEnabled bool,
) channelAccountConcurrencyFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithSuperAdmin(),
		testutil.WithFullName("Channel Concurrency Tester"),
	)
	enableChannelAccountConcurrencyEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
	)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "concurrency-" + uuid.NewString(),
		ExternalAccountID: "external-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{"text": true},
		Config: models.JSONB{
			"relay_url":        "https://relay-original.example.com/meta",
			"outbound_enabled": true,
			"ai_reply_enabled": aiReplyEnabled,
			"display_option":   "preserved",
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	return channelAccountConcurrencyFixture{
		App:          app,
		Organization: organization,
		User:         user,
		Account:      account,
	}
}

func updateChannelAccountForConcurrencyTest(
	t *testing.T,
	fixture channelAccountConcurrencyFixture,
	body map[string]any,
) {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(
		request,
		fixture.Organization.ID,
		fixture.User.ID,
		fixture.User.RoleID,
		true,
	)
	testutil.SetPathParam(request, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(request))
	require.Equal(
		t,
		fasthttp.StatusOK,
		testutil.GetResponseStatusCode(request),
		string(testutil.GetResponseBody(request)),
	)
}

func createChannelAccountConcurrencyJob(
	t *testing.T,
	fixture channelAccountConcurrencyFixture,
	status models.ScheduledJobStatus,
) *models.ScheduledJob {
	t.Helper()
	now := time.Now().UTC()
	messageID := uuid.New()
	job := &models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: fixture.Organization.ID,
		Kind:           models.ScheduledJobKindChannelAIReply,
		AggregateType:  models.ChannelAIReplyAggregateType,
		AggregateID:    &messageID,
		RunAt:          now,
		Status:         status,
		MaxAttempts:    5,
		IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID),
		Payload: models.JSONB{
			"organization_id":    fixture.Organization.ID.String(),
			"channel_account_id": fixture.Account.ID.String(),
			"inbound_message_id": messageID.String(),
		},
		Version: 1,
	}
	if status == models.ScheduledJobStatusProcessing {
		job.LockedAt = &now
		job.LockedBy = "validation-worker"
	}
	require.NoError(t, fixture.App.DB.Create(job).Error)
	return job
}

func assertChannelAccountConcurrencyJobCancelled(
	t *testing.T,
	db *gorm.DB,
	jobID uuid.UUID,
	reason string,
) {
	t.Helper()
	var job models.ScheduledJob
	require.NoError(t, db.First(&job, "id = ?", jobID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assert.Equal(t, reason, job.LastError)
	assert.Empty(t, job.LockedBy)
	assert.Nil(t, job.LockedAt)
	require.NotNil(t, job.CompletedAt)
}

func requireChannelAccountAuditChange(
	t *testing.T,
	changes models.JSONBArray,
	field string,
) map[string]any {
	t.Helper()
	for _, raw := range changes {
		change, ok := raw.(map[string]any)
		if ok && change["field"] == field {
			return change
		}
	}
	require.FailNow(t, "audit change not found", "field=%s changes=%v", field, changes)
	return nil
}

func requireChannelAccountAuditObject(t *testing.T, value any) map[string]any {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case models.JSONB:
		return map[string]any(typed)
	default:
		require.FailNow(t, "audit value is not an object", "type=%T", value)
		return nil
	}
}

func cloneChannelAccountForFingerprintTest(
	account *models.ChannelAccount,
) models.ChannelAccount {
	cloned := *account
	cloned.Config = cloneJSONB(account.Config)
	cloned.Capabilities = cloneJSONB(account.Capabilities)
	cloned.Metadata = cloneJSONB(account.Metadata)
	cloned.Credentials = make([]models.ChannelCredential, len(account.Credentials))
	copy(cloned.Credentials, account.Credentials)
	for index := range cloned.Credentials {
		cloned.Credentials[index].CredentialBlob = cloneJSONB(
			account.Credentials[index].CredentialBlob,
		)
		cloned.Credentials[index].Metadata = cloneJSONB(
			account.Credentials[index].Metadata,
		)
	}
	return cloned
}

func enableChannelAccountConcurrencyEntitlement(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID uuid.UUID,
) {
	t.Helper()
	plan := &models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "channel-concurrency-" + uuid.NewString(),
		Code:        "channel-concurrency-" + uuid.NewString(),
		Name:        "Channel concurrency test plan",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(plan).Error)
	billingAccount := &models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(billingAccount).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := &models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organizationID,
		BillingAccountID:     billingAccount.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "send_invoice",
		EntitlementsSnapshot: models.JSONB{channelapi.OmnichannelEntitlementKey: true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &userID,
	}
	require.NoError(t, db.Create(subscription).Error)
}
