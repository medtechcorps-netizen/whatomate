package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestChannelAccountResponseRedactsCredentialsAndRestrictedConfig(t *testing.T) {
	t.Parallel()

	account := &models.ChannelAccount{
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "Instagram",
		ExternalAccountID: "page-1",
		Config: models.JSONB{
			"relay_url":           "https://relay.example.test",
			"api_token":           "must-not-leak",
			"allow_localhost_dev": true,
			"nested": map[string]any{
				"password": "nested-must-not-leak",
				"label":    "safe",
			},
		},
		Capabilities: models.JSONB{"text": true},
		Credentials: []models.ChannelCredential{{
			Status: models.ChannelCredentialStatusActive,
			CredentialBlob: models.JSONB{
				"inbound_secret": "must-not-leak",
			},
		}},
	}

	response := channelAccountToResponse(account)
	assert.True(t, response.HasCredentials)
	assert.Equal(t, "https://relay.example.test", response.Config["relay_url"])
	assert.NotContains(t, response.Config, "api_token")
	assert.NotContains(t, response.Config, "allow_localhost_dev")
	assert.Equal(t, map[string]any{"label": "safe"}, response.Config["nested"])

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "must-not-leak")
	assert.NotContains(t, string(encoded), "allow_localhost_dev")
	assert.NotContains(t, string(encoded), "nested-must-not-leak")
}

func TestManagedMessengerResponseExposesOnlyDerivedReconciliationState(t *testing.T) {
	t.Parallel()

	account := &models.ChannelAccount{
		Channel:  models.ChannelMessenger,
		Provider: channelapi.RelayProvider,
		Config: models.JSONB{
			"meta_registry_managed": true,
		},
		Metadata: models.JSONB{
			metaMessengerSubscriptionOperationStateKey:    metaMessengerSubscriptionSubscribeFailed,
			metaMessengerSubscriptionDesiredStateKey:      metaMessengerSubscriptionDesiredSubscribed,
			metaMessengerSubscriptionFencedOperationIDKey: "operation-id-must-not-leak",
			"meta_authorizing_user_id":                    "authorizer-must-not-leak",
			"meta_deauthorization_event_digest":           "digest-must-not-leak",
		},
	}

	response := channelAccountToResponse(account)
	assert.True(t, response.MetaSubscriptionReconciliationRequired)
	assert.Equal(t, metaMessengerSubscriptionDesiredSubscribed, response.MetaSubscriptionReconciliationDesired)

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"meta_subscription_reconciliation_required":true`)
	assert.NotContains(t, string(encoded), "operation-id-must-not-leak")
	assert.NotContains(t, string(encoded), "authorizer-must-not-leak")
	assert.NotContains(t, string(encoded), "digest-must-not-leak")
	assert.NotContains(t, string(encoded), `"metadata"`)

	account.Channel = models.ChannelInstagram
	assert.False(t, channelAccountToResponse(account).MetaSubscriptionReconciliationRequired)
	account.Config["instagram_api_mode"] = "instagram_login"
	instagramResponse := channelAccountToResponse(account)
	assert.True(t, instagramResponse.MetaSubscriptionReconciliationRequired)
	assert.Equal(t, metaMessengerSubscriptionDesiredSubscribed, instagramResponse.MetaSubscriptionReconciliationDesired)
}

func TestChannelIdentifiersAreAllowlisted(t *testing.T) {
	t.Parallel()

	for _, channelName := range []models.Channel{
		models.ChannelWhatsApp,
		models.ChannelInstagram,
		models.ChannelMessenger,
		models.ChannelWebChat,
		models.ChannelEmail,
		models.ChannelSMS,
		models.ChannelTelegram,
		models.ChannelTikTok,
	} {
		assert.True(t, validChannelIdentifier(channelName), channelName)
	}
	assert.False(t, validChannelIdentifier(models.Channel("arbitrary-provider-channel")))
	assert.True(t, channelProviderIdentifier.MatchString("microsoft_graph"))
	assert.False(t, channelProviderIdentifier.MatchString("../provider"))
	assert.False(t, channelProviderIdentifier.MatchString("Provider With Spaces"))
}

func TestListInboxConversationsFiltersByExactConversationID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"conversation-filter-"+uuid.NewString(),
		[]string{models.ResourceConversations + ":" + models.ActionRead},
	)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		reader.ID,
		"omnichannel.enabled",
	)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "Exact conversation filter",
		ExternalAccountID: "filter-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	selected := createAttentionConversation(
		t,
		db,
		organization.ID,
		account.ID,
		contact.ID,
		"selected",
	)
	createAttentionConversation(t, db, organization.ID, account.ID, contact.ID, "other")
	otherOrganization := testutil.CreateTestOrganization(t, db)
	otherContact := testutil.CreateTestContact(t, db, otherOrganization.ID)
	otherAccount := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    otherOrganization.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "Other tenant conversation filter",
		ExternalAccountID: "other-filter-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&otherAccount).Error)
	otherConversation := createAttentionConversation(
		t,
		db,
		otherOrganization.ID,
		otherAccount.ID,
		otherContact.ID,
		"other-tenant",
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, reader.ID)
	testutil.SetQueryParam(request, "conversation_id", selected.ID.String())
	require.NoError(t, app.ListInboxConversations(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data struct {
			Conversations []InboxConversationResponse `json:"conversations"`
			Total         int                         `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	require.Len(t, response.Data.Conversations, 1)
	assert.Equal(t, 1, response.Data.Total)
	assert.Equal(t, selected.ID, response.Data.Conversations[0].ID)
	assert.Equal(t, WhatsAppIdentityReviewEffectiveState{
		Known:     true,
		AIAllowed: true,
		Blocked:   false,
		Reason:    "no_open_identity_review",
	}, response.Data.Conversations[0].IdentityReviewAIState)

	whatsAppAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	coexistenceState := models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    organization.ID,
		WhatsAppAccountID: whatsAppAccount.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusConnected,
		OnboardingCycle:   1,
		SyncStatus:        models.CoexistenceSyncStatusNotRequested,
		ContactSyncStatus: models.CoexistenceSyncStatusNotRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusNotRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}
	require.NoError(t, db.Create(&coexistenceState).Error)
	require.NoError(t, db.Model(contact).Update("bs_uid", "selected-contact-principal").Error)
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID:          organization.ID,
		WhatsAppAccountID:       whatsAppAccount.ID,
		OnboardingCycle:         1,
		DirectPrimaryBSUID:      "selected-contact-principal",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("a", 64),
		SelectorBodyDigest:      strings.Repeat("b", 64),
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))

	openHold := testutil.NewGETRequest(t)
	testutil.SetAuthContext(openHold, organization.ID, reader.ID)
	testutil.SetQueryParam(openHold, "conversation_id", selected.ID.String())
	require.NoError(t, app.ListInboxConversations(openHold))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(openHold))
	var openHoldResponse struct {
		Data struct {
			Conversations []InboxConversationResponse `json:"conversations"`
			Total         int                         `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(openHold), &openHoldResponse))
	require.Len(t, openHoldResponse.Data.Conversations, 1)
	assert.Equal(t, 1, openHoldResponse.Data.Total)
	assert.Equal(t, selected.ID, openHoldResponse.Data.Conversations[0].ID)
	openState := openHoldResponse.Data.Conversations[0].IdentityReviewAIState
	assert.True(t, openState.Known)
	assert.False(t, openState.AIAllowed)
	assert.True(t, openState.Blocked)
	assert.Positive(t, openState.OpenHoldCount)
	assert.Equal(t, "identity_review_open", openState.Reason)

	crossTenant := testutil.NewGETRequest(t)
	testutil.SetAuthContext(crossTenant, organization.ID, reader.ID)
	testutil.SetQueryParam(crossTenant, "conversation_id", otherConversation.ID.String())
	require.NoError(t, app.ListInboxConversations(crossTenant))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(crossTenant))
	var crossTenantResponse struct {
		Data struct {
			Conversations []InboxConversationResponse `json:"conversations"`
			Total         int                         `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(crossTenant), &crossTenantResponse))
	assert.Zero(t, crossTenantResponse.Data.Total)
	assert.Empty(t, crossTenantResponse.Data.Conversations)

	invalid := testutil.NewGETRequest(t)
	testutil.SetAuthContext(invalid, organization.ID, reader.ID)
	testutil.SetQueryParam(invalid, "conversation_id", "not-a-uuid")
	require.NoError(t, app.ListInboxConversations(invalid))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(invalid))

	callbackName := "test:inbox_identity_review_read_failure_" + uuid.NewString()
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "member" || strings.Contains(tx.Statement.Table, "whatsapp_identity_review_members") {
			_ = tx.AddError(errors.New("forced identity-review read failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Row().Remove(callbackName) })

	fallback := testutil.NewGETRequest(t)
	testutil.SetAuthContext(fallback, organization.ID, reader.ID)
	testutil.SetQueryParam(fallback, "conversation_id", selected.ID.String())
	require.NoError(t, app.ListInboxConversations(fallback))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(fallback))
	var fallbackResponse struct {
		Data struct {
			Conversations []InboxConversationResponse `json:"conversations"`
			Total         int                         `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(fallback), &fallbackResponse))
	require.Len(t, fallbackResponse.Data.Conversations, 1)
	assert.Equal(t, 1, fallbackResponse.Data.Total)
	fallbackState := fallbackResponse.Data.Conversations[0].IdentityReviewAIState
	assert.False(t, fallbackState.Known)
	assert.False(t, fallbackState.AIAllowed)
	assert.True(t, fallbackState.Blocked)
	assert.Equal(t, "identity_review_read_failed", fallbackState.Reason)
}

func TestTenantConfigCannotEnableLocalhostRelay(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "allow_localhost_dev", unsafeConfigKey(models.JSONB{
		"allow_localhost_dev": true,
	}))
}

func TestValidateOutboundPartsHonorsCapabilities(t *testing.T) {
	t.Parallel()

	request := SendInboxConversationMessageRequest{
		Parts: []channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Hello",
		}},
	}
	require.NoError(t, validateOutboundParts(channelapi.Capabilities{Text: true}, request))

	err := validateOutboundParts(channelapi.Capabilities{}, request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support text")

	request.Parts = append(request.Parts, channelapi.MessagePart{
		Type:     models.MessagePartTypeImage,
		MediaURL: "https://cdn.example.test/image.jpg",
	})
	err = validateOutboundParts(channelapi.Capabilities{
		Text:                true,
		Media:               true,
		MultipleAttachments: false,
	}, request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple message parts")

	request.Parts = []channelapi.MessagePart{{
		Type: models.MessagePartType("provider_private"),
	}}
	err = validateOutboundParts(channelapi.Capabilities{Text: true}, request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not supported")
}

func TestFailedRawWebhookCanBeReclaimedOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "reclaim-" + uuid.NewString()[:8],
		ExternalAccountID: "page-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	failed := &models.InboundEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		DedupeKey:        "raw:" + uuid.NewString(),
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusFailed,
		SignatureValid:   true,
		ReceivedAt:       time.Now().UTC(),
		ProcessedAt:      timePointer(time.Now().UTC()),
		ErrorCode:        "processing_failed",
		ErrorMessage:     "temporary failure",
		Headers:          models.JSONB{},
		Payload:          models.JSONB{},
	}
	require.NoError(t, db.Create(failed).Error)

	retry := &models.InboundEvent{
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		DedupeKey:        failed.DedupeKey,
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusPending,
		ReceivedAt:       time.Now().UTC(),
		Headers:          models.JSONB{},
		Payload:          models.JSONB{},
	}
	var shouldProcess, reclaimed bool
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		shouldProcess, reclaimed, err = persistOrClaimRawInboundEvent(tx, retry)
		return err
	}))
	assert.True(t, shouldProcess)
	assert.True(t, reclaimed)
	assert.Equal(t, failed.ID, retry.ID)

	duplicate := &models.InboundEvent{
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		DedupeKey:        failed.DedupeKey,
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusPending,
		ReceivedAt:       time.Now().UTC(),
		Headers:          models.JSONB{},
		Payload:          models.JSONB{},
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		shouldProcess, reclaimed, err = persistOrClaimRawInboundEvent(tx, duplicate)
		return err
	}))
	assert.False(t, shouldProcess)
	assert.False(t, reclaimed)
}

func TestRawWebhookCrashGapLeaseRecovery(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelWebChat,
		Provider:          channelapi.RelayProvider,
		Name:              "lease-" + uuid.NewString()[:8],
		ExternalAccountID: "site-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)

	staleAt := time.Now().UTC().Add(-rawInboundProcessingLease - time.Second)
	for _, fixture := range []struct {
		name              string
		status            models.InboundEventStatus
		processingStarted *time.Time
		wantClaim         bool
	}{
		{
			name:              "stale processing",
			status:            models.InboundEventStatusProcessing,
			processingStarted: &staleAt,
			wantClaim:         true,
		},
		{
			name:              "legacy pending without timestamp",
			status:            models.InboundEventStatusPending,
			processingStarted: nil,
			wantClaim:         true,
		},
		{
			name:              "live processing lease",
			status:            models.InboundEventStatusProcessing,
			processingStarted: timePointer(time.Now().UTC()),
			wantClaim:         false,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			persisted := &models.InboundEvent{
				BaseModel:           models.BaseModel{ID: uuid.New()},
				OrganizationID:      org.ID,
				ChannelAccountID:    account.ID,
				DedupeKey:           "raw:" + uuid.NewString(),
				EventType:           "raw_webhook",
				Status:              fixture.status,
				SignatureValid:      true,
				ReceivedAt:          time.Now().UTC(),
				ProcessingStartedAt: fixture.processingStarted,
				AttemptCount:        1,
				Headers:             models.JSONB{},
				Payload:             models.JSONB{},
			}
			require.NoError(t, db.Create(persisted).Error)

			retry := &models.InboundEvent{
				OrganizationID:   org.ID,
				ChannelAccountID: account.ID,
				DedupeKey:        persisted.DedupeKey,
				EventType:        "raw_webhook",
				ReceivedAt:       time.Now().UTC(),
				Headers:          models.JSONB{},
				Payload:          models.JSONB{},
			}
			var shouldProcess, reclaimed bool
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				var err error
				shouldProcess, reclaimed, err = persistOrClaimRawInboundEvent(tx, retry)
				return err
			}))
			assert.Equal(t, fixture.wantClaim, shouldProcess)
			assert.Equal(t, fixture.wantClaim, reclaimed)
			assert.Equal(t, models.InboundEventStatusProcessing, retry.Status)
			assert.Equal(t, !fixture.wantClaim, rawInboundEventLeaseActive(retry.Status) && !shouldProcess)
		})
	}
}

func TestChannelWebhookResolverUsesChannelAccountIDAcrossOrganizations(t *testing.T) {
	db := testutil.SetupTestDB(t)
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	externalID := "shared-provider-id-" + uuid.NewString()
	accountA := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgA.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "shared",
		ExternalAccountID: externalID,
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	accountB := *accountA
	accountB.BaseModel = models.BaseModel{ID: uuid.New()}
	accountB.OrganizationID = orgB.ID
	accountB.ExternalAccountID = "provider-id-" + uuid.NewString()
	require.NoError(t, db.Create(accountA).Error)
	require.NoError(t, db.Create(&accountB).Error)

	resolvedA, err := resolveChannelWebhookOrganization(db, accountA.ID, false)
	require.NoError(t, err)
	resolvedB, err := resolveChannelWebhookOrganization(db, accountB.ID, false)
	require.NoError(t, err)
	assert.Equal(t, orgA.ID, resolvedA)
	assert.Equal(t, orgB.ID, resolvedB)
}

func TestIgnoredUnlicensedWebhookRedactsRawPayload(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelEmail,
		Provider:          channelapi.RelayProvider,
		Name:              "discard-" + uuid.NewString()[:8],
		ExternalAccountID: "mailbox-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	event := &models.InboundEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		DedupeKey:        "raw:" + uuid.NewString(),
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusProcessing,
		SignatureValid:   true,
		ReceivedAt:       time.Now().UTC(),
		Headers:          models.JSONB{"authorization": "secret"},
		Payload:          models.JSONB{"patient": "private"},
	}
	require.NoError(t, db.Create(event).Error)

	require.NoError(t, markInboundEventIgnored(
		db,
		org.ID,
		event.ID,
		"omnichannel_not_entitled",
		"discarded",
	))
	require.NoError(t, db.Where("id = ? AND organization_id = ?", event.ID, org.ID).First(event).Error)
	assert.Equal(t, models.InboundEventStatusIgnored, event.Status)
	assert.Equal(t, models.JSONB{"redacted": true}, event.Payload)
	assert.Empty(t, event.Headers)
}

func TestExpiredChannelCredentialIsNotCurrent(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	account := &models.ChannelAccount{Credentials: []models.ChannelCredential{{
		Status:    models.ChannelCredentialStatusActive,
		ExpiresAt: &expired,
	}}}
	assert.Nil(t, currentChannelCredential(account))
}

func TestInboxConversationResponseIncludesContactIdentity(t *testing.T) {
	t.Parallel()

	identity := &models.ContactIdentity{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ExternalID:  "external-contact",
		DisplayName: "Amira",
	}
	conversation := &models.InboxConversation{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		ContactIdentity: identity,
	}
	response := inboxConversationToResponse(conversation)
	require.NotNil(t, response.ContactIdentity)
	assert.Equal(t, identity.ID, response.ContactIdentity.ID)
	assert.Equal(t, "Amira", response.ContactIdentity.DisplayName)
}

func TestLegacyWhatsAppReplyCapabilityMatchesRolloutGate(t *testing.T) {
	organizationID := uuid.New()
	account := &models.ChannelAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Channel:        models.ChannelWhatsApp,
		Provider:       channelapi.LegacyMetaProvider,
		Capabilities:   models.JSONB{"text": true, "template": true},
		Config:         models.JSONB{"reply_route": "chat"},
	}

	disabled := (&App{Config: &config.Config{}}).channelAccountToResponse(account)
	assert.NotContains(t, disabled.Capabilities, "legacy_text_reply_endpoint")
	assert.True(t, boolConfigValue(disabled.Capabilities, "template"))

	allowed := (&App{Config: &config.Config{LegacyWhatsAppReply: config.LegacyWhatsAppReplyConfig{
		Enabled:                true,
		AllowedOrganizationIDs: organizationID.String(),
	}}}).channelAccountToResponse(account)
	assert.True(t, boolConfigValue(allowed.Capabilities, "legacy_text_reply_endpoint"))

	notAllowed := (&App{Config: &config.Config{LegacyWhatsAppReply: config.LegacyWhatsAppReplyConfig{
		Enabled:                true,
		AllowedOrganizationIDs: uuid.NewString(),
	}}}).channelAccountToResponse(account)
	assert.NotContains(t, notAllowed.Capabilities, "legacy_text_reply_endpoint")

	account.Capabilities = nil
	nilCapabilities := (&App{Config: &config.Config{LegacyWhatsAppReply: config.LegacyWhatsAppReplyConfig{
		Enabled:                true,
		AllowedOrganizationIDs: organizationID.String(),
	}}}).channelAccountToResponse(account)
	assert.True(t, boolConfigValue(nilCapabilities.Capabilities, "legacy_text_reply_endpoint"))
}

func TestChannelIdempotencyReplayBindsConversationAndPayload(t *testing.T) {
	t.Parallel()

	conversationID := uuid.New()
	request := SendInboxConversationMessageRequest{
		Purpose: models.ChannelPreferencePurposeService,
		Parts: []channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Hello",
		}},
	}
	digest, err := channelSendRequestDigest(conversationID, request)
	require.NoError(t, err)
	job := &models.OutboxJob{
		ConversationID: conversationID,
		PayloadDigest:  digest,
	}
	require.NoError(t, validateChannelOutboxReplay(job, conversationID, digest))
	assert.ErrorIs(
		t,
		validateChannelOutboxReplay(job, uuid.New(), digest),
		errChannelIdempotencyCollision,
	)

	changed := request
	changed.Parts = []channelapi.MessagePart{{
		Type: models.MessagePartTypeText,
		Text: "Different body",
	}}
	changedDigest, err := channelSendRequestDigest(conversationID, changed)
	require.NoError(t, err)
	assert.NotEqual(t, digest, changedDigest)
	assert.ErrorIs(
		t,
		validateChannelOutboxReplay(job, conversationID, changedDigest),
		errChannelIdempotencyCollision,
	)
}
