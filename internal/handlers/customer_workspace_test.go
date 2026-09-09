package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

func enableCustomerWorkspaceEntitlements(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
	keys ...string,
) {
	t.Helper()
	snapshot := models.JSONB{}
	for _, key := range keys {
		snapshot[key] = true
	}
	plan := &models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "workspace-" + uuid.NewString(),
		Code:        "workspace-" + uuid.NewString(),
		Name:        "Customer workspace test plan",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(plan).Error)
	account := &models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := &models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       orgID,
		BillingAccountID:     account.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "send_invoice",
		EntitlementsSnapshot: snapshot,
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &userID,
	}
	require.NoError(t, db.Create(subscription).Error)
}

func TestDedupeCustomerTimelinePrefersStoredActivity(t *testing.T) {
	sourceID := uuid.New()
	now := time.Now().UTC()
	items := []CustomerTimelineItem{
		{
			ID:         "history:booking:" + sourceID.String(),
			Type:       string(models.CustomerActivityBookingCreated),
			Category:   string(models.CustomerActivityCategoryBooking),
			Title:      "Synthesized booking",
			OccurredAt: now,
			SourceType: "booking",
			SourceID:   &sourceID,
		},
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityBookingCreated),
			Category:   string(models.CustomerActivityCategoryBooking),
			Title:      "Durable booking activity",
			OccurredAt: now,
			SourceType: "booking",
			SourceID:   &sourceID,
			Metadata:   models.JSONB{"status": string(models.BookingStatusConfirmed)},
		},
	}

	result := dedupeCustomerTimeline(items)
	require.Len(t, result, 1)
	require.Equal(t, "Durable booking activity", result[0].Title)
	require.Contains(t, result[0].ID, "activity:")
}

func TestDedupeCustomerTimelineKeepsDistinctStageTransitions(t *testing.T) {
	leadID := uuid.New()
	firstStage := uuid.New()
	secondStage := uuid.New()
	thirdStage := uuid.New()
	items := []CustomerTimelineItem{
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityCRMStageMoved),
			SourceType: "crm_lead",
			SourceID:   &leadID,
			Metadata: models.JSONB{
				"from_stage_id": firstStage.String(),
				"to_stage_id":   secondStage.String(),
			},
		},
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityCRMStageMoved),
			SourceType: "crm_lead",
			SourceID:   &leadID,
			Metadata: models.JSONB{
				"from_stage_id": secondStage.String(),
				"to_stage_id":   thirdStage.String(),
			},
		},
	}

	require.Len(t, dedupeCustomerTimeline(items), 2)
}

func TestDedupeCustomerTimelineNormalizesBookingTransitionMetadata(t *testing.T) {
	bookingID := uuid.New()
	items := []CustomerTimelineItem{
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityBookingStatus),
			SourceType: "booking",
			SourceID:   &bookingID,
			Metadata:   models.JSONB{"from_status": "reserved", "to_status": "confirmed"},
		},
		{
			ID:         "history:booking-status:" + bookingID.String(),
			Type:       string(models.CustomerActivityBookingStatus),
			SourceType: "booking",
			SourceID:   &bookingID,
			Metadata:   models.JSONB{"status": "confirmed"},
		},
	}

	result := dedupeCustomerTimeline(items)
	require.Len(t, result, 1)
	require.Contains(t, result[0].ID, "activity:")
}

func TestDedupeCustomerTimelineKeepsRepeatedDurableFacts(t *testing.T) {
	contactID := uuid.New()
	items := []CustomerTimelineItem{
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityContactUpdated),
			SourceType: "contact",
			SourceID:   &contactID,
		},
		{
			ID:         "activity:" + uuid.NewString(),
			Type:       string(models.CustomerActivityContactUpdated),
			SourceType: "contact",
			SourceID:   &contactID,
		},
	}

	require.Len(t, dedupeCustomerTimeline(items), 2)
}

func TestCustomerMessageTimelineSynthesizesIncomingEvent(t *testing.T) {
	message := models.Message{
		BaseModel:   models.BaseModel{ID: uuid.New(), CreatedAt: time.Now().UTC()},
		Direction:   models.DirectionIncoming,
		MessageType: models.MessageTypeText,
		Content:     "Can I book tomorrow?",
		Status:      models.MessageStatusReceived,
	}
	items := customerMessageTimeline([]models.Message{message})
	require.Len(t, items, 1)
	require.Equal(t, string(models.CustomerActivityMessageIncoming), items[0].Type)
	require.Equal(t, string(models.CustomerActivityCategoryMessage), items[0].Category)
	require.Equal(t, "Can I book tomorrow?", items[0].Summary)
	require.Equal(t, message.ID, *items[0].SourceID)
}

func TestCustomerActivityOutboxPayloadPreservesLegacyFieldsAndProtectsLifecycleData(
	t *testing.T,
) {
	activityID := uuid.New()
	contactID := uuid.New()
	leadID := uuid.New()
	sourceID := uuid.New()
	actorUserID := uuid.New()
	occurredAt := time.Now().UTC()
	metadata := models.JSONB{"message_status": "received"}
	event := models.CustomerActivityEvent{
		ID:               activityID,
		OrganizationID:   uuid.New(),
		ContactID:        contactID,
		LeadID:           &leadID,
		EventType:        models.CustomerActivityMessageIncoming,
		Category:         models.CustomerActivityCategoryMessage,
		Title:            "Incoming WhatsApp message",
		Summary:          "Can I book tomorrow?",
		ActorType:        models.CustomerActivityActorContact,
		ActorUserID:      &actorUserID,
		SourceObjectType: "message",
		SourceObjectID:   &sourceID,
		OccurredAt:       occurredAt,
		Metadata:         metadata,
	}
	untrustedID := uuid.NewString()
	payload := customerActivityOutboxPayload(event, models.JSONB{
		"message_id":        sourceID.String(),
		"contact_phone":     "+60123456789",
		"contact_name":      "Customer",
		"message_type":      "text",
		"content":           "Can I book tomorrow?",
		"whatsapp_account":  "support",
		"direction":         "incoming",
		"activity_event_id": untrustedID,
		"contact_id":        untrustedID,
		"lead_id":           untrustedID,
		"event_type":        string(models.CustomerActivityContactUpdated),
		"category":          string(models.CustomerActivityCategoryContact),
		"title":             "Untrusted title",
		"summary":           "Untrusted summary",
		"occurred_at":       time.Time{}.Format(time.RFC3339Nano),
		"source_type":       "contact",
		"source_id":         untrustedID,
		"actor_type":        string(models.CustomerActivityActorSystem),
		"actor_user_id":     untrustedID,
		"metadata":          models.JSONB{"untrusted": true},
		"outbox_event_id":   untrustedID,
		"Event_Type":        "mixed-case untrusted event",
		"Outbox_Event_ID":   "mixed-case untrusted outbox",
	})

	require.Equal(t, sourceID.String(), payload["message_id"])
	require.Equal(t, "+60123456789", payload["contact_phone"])
	require.Equal(t, "Customer", payload["contact_name"])
	require.Equal(t, "text", payload["message_type"])
	require.Equal(t, "Can I book tomorrow?", payload["content"])
	require.Equal(t, "support", payload["whatsapp_account"])
	require.Equal(t, "incoming", payload["direction"])
	require.Equal(t, activityID.String(), payload["activity_event_id"])
	require.Equal(t, contactID.String(), payload["contact_id"])
	require.Equal(t, leadID.String(), payload["lead_id"])
	require.Equal(t, string(event.EventType), payload["event_type"])
	require.Equal(t, string(event.Category), payload["category"])
	require.Equal(t, event.Title, payload["title"])
	require.Equal(t, event.Summary, payload["summary"])
	require.Equal(t, occurredAt.Format(time.RFC3339Nano), payload["occurred_at"])
	require.Equal(t, event.SourceObjectType, payload["source_type"])
	require.Equal(t, sourceID.String(), payload["source_id"])
	require.Equal(t, string(event.ActorType), payload["actor_type"])
	require.Equal(t, actorUserID.String(), payload["actor_user_id"])
	require.Equal(t, metadata, payload["metadata"])
	require.NotContains(t, payload, "outbox_event_id")
	require.NotContains(t, payload, "Event_Type")
	require.NotContains(t, payload, "Outbox_Event_ID")
}

func TestCustomerWorkspaceCurrentSummaryEligibility(t *testing.T) {
	now := time.Now().UTC()
	active := &models.ContactPackage{Status: models.ContactPackageStatusActive}
	require.True(t, customerWorkspacePackageIsCurrent(active, now))
	expired := now.Add(-time.Minute)
	active.ExpiresAt = &expired
	require.False(t, customerWorkspacePackageIsCurrent(active, now))
	active.ExpiresAt = nil
	active.Status = models.ContactPackageStatusExpired
	require.False(t, customerWorkspacePackageIsCurrent(active, now))

	require.True(t, customerWorkspaceInvoiceIsOutstanding(&models.CommerceInvoice{
		Status:   models.CommerceInvoiceStatusOpen,
		DueMinor: 100,
	}))
	for _, status := range []models.CommerceInvoiceStatus{
		models.CommerceInvoiceStatusDraft,
		models.CommerceInvoiceStatusPaid,
		models.CommerceInvoiceStatusVoid,
		models.CommerceInvoiceStatusRefunded,
	} {
		require.False(t, customerWorkspaceInvoiceIsOutstanding(&models.CommerceInvoice{
			Status:   status,
			DueMinor: 100,
		}))
	}
}

func TestCustomerWorkspaceAllowedActivityCategoriesFollowCapabilities(t *testing.T) {
	categories := customerWorkspaceAllowedActivityCategories(CustomerWorkspaceCapabilities{
		CRM:      true,
		Payments: true,
		Messages: true,
	})
	require.ElementsMatch(t, []models.CustomerActivityCategory{
		models.CustomerActivityCategoryContact,
		models.CustomerActivityCategoryMessage,
		models.CustomerActivityCategoryConsent,
		models.CustomerActivityCategoryCRM,
		models.CustomerActivityCategoryInvoice,
		models.CustomerActivityCategoryPayment,
	}, categories)
	require.NotContains(t, categories, models.CustomerActivityCategoryTask)
	require.NotContains(t, categories, models.CustomerActivityCategoryBooking)
	require.NotContains(t, categories, models.CustomerActivityCategoryPackage)
}

func TestCustomerWorkspaceEnforcesTenantAndAssignmentVisibility(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	otherOrg := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-restricted", nil)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))

	assigned := testutil.CreateTestContact(t, db, org.ID)
	require.NoError(t, db.Model(assigned).Update("assigned_user_id", user.ID).Error)
	unassigned := testutil.CreateTestContact(t, db, org.ID)
	foreign := testutil.CreateTestContact(t, db, otherOrg.ID)

	request := func(contactID uuid.UUID) *fastglue.Request {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", contactID.String())
		return req
	}

	assignedReq := request(assigned.ID)
	require.NoError(t, app.GetCustomerWorkspace(assignedReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(assignedReq))
	var response CustomerWorkspaceResponse
	testutil.ParseEnvelopeResponse(t, assignedReq, &response)
	require.Equal(t, assigned.ID, response.Contact.ID)
	require.NotNil(t, response.Identities)
	require.NotNil(t, response.Journeys)
	require.NotNil(t, response.Tasks)
	require.NotNil(t, response.Bookings)
	require.NotNil(t, response.Packages)
	require.NotNil(t, response.Invoices)
	require.NotNil(t, response.Payments)
	require.NotNil(t, response.Timeline)
	require.NotNil(t, response.Summary.PipelineValue)
	require.NotNil(t, response.Summary.Outstanding)
	require.NotNil(t, response.Summary.Collected)

	unassignedReq := request(unassigned.ID)
	require.NoError(t, app.GetCustomerWorkspace(unassignedReq))
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(unassignedReq))

	foreignReq := request(foreign.ID)
	require.NoError(t, app.GetCustomerWorkspace(foreignReq))
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(foreignReq))
}

func TestCustomerWorkspaceGatesMessageHistoryByPermission(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)
	message := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		WhatsAppAccount:   "workspace-message-test",
		ContactID:         contact.ID,
		WhatsAppMessageID: "workspace-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "private conversation history",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	durable := &models.CustomerActivityEvent{
		ID:               uuid.New(),
		OrganizationID:   org.ID,
		ContactID:        contact.ID,
		EventType:        models.CustomerActivityMessageIncoming,
		Category:         models.CustomerActivityCategoryMessage,
		Title:            "Durable incoming message",
		ActorType:        models.CustomerActivityActorContact,
		SourceObjectType: "message",
		SourceObjectID:   &message.ID,
		OccurredAt:       message.CreatedAt,
		Metadata:         models.JSONB{},
		IdempotencyKey:   "workspace-message:" + message.ID.String(),
	}
	require.NoError(t, db.Create(durable).Error)

	load := func(user *models.User) CustomerWorkspaceResponse {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", contact.ID.String())
		require.NoError(t, app.GetCustomerWorkspace(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var response CustomerWorkspaceResponse
		testutil.ParseEnvelopeResponse(t, req, &response)
		return response
	}

	contactOnlyRole := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-contact-only", []string{
		models.ResourceContacts + ":" + models.ActionRead,
	})
	contactOnlyUser := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&contactOnlyRole.ID))
	denied := load(contactOnlyUser)
	require.False(t, denied.Capabilities.Messages)
	for _, item := range denied.Timeline {
		require.NotEqual(t, string(models.CustomerActivityMessageIncoming), item.Type)
	}

	chatRole := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-chat-reader", []string{
		models.ResourceContacts + ":" + models.ActionRead,
		models.ResourceChat + ":" + models.ActionRead,
	})
	chatUser := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&chatRole.ID))
	allowed := load(chatUser)
	require.True(t, allowed.Capabilities.Messages)
	var messageItems []CustomerTimelineItem
	for _, item := range allowed.Timeline {
		if item.Type == string(models.CustomerActivityMessageIncoming) {
			messageItems = append(messageItems, item)
		}
	}
	require.Len(t, messageItems, 1)
	require.Equal(t, "Durable incoming message", messageItems[0].Title)
}

func TestCustomerWorkspaceCurrentConversationOnlyScopesDurableAndLegacyMessages(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-current-conversation", []string{
		models.ResourceChat + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, db, org.ID)
	require.NoError(t, db.Create(&models.ChatbotSettings{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: "",
		AgentAssignment: models.AgentAssignmentConfig{
			CurrentConversationOnly: true,
		},
	}).Error)

	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: "workspace-current-conversation",
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		AgentID:         &user.ID,
		TransferredAt:   cutoff,
	}).Error)

	createMessageAndActivity := func(label string, occurredAt time.Time) *models.Message {
		message := &models.Message{
			BaseModel: models.BaseModel{
				ID:        uuid.New(),
				CreatedAt: occurredAt,
				UpdatedAt: occurredAt,
			},
			OrganizationID:    org.ID,
			WhatsAppAccount:   "workspace-current-conversation",
			ContactID:         contact.ID,
			WhatsAppMessageID: "workspace-current-" + uuid.NewString(),
			Direction:         models.DirectionIncoming,
			MessageType:       models.MessageTypeText,
			Content:           label,
			Status:            models.MessageStatusReceived,
			Metadata:          models.JSONB{},
		}
		require.NoError(t, db.Create(message).Error)
		require.NoError(t, db.Create(&models.CustomerActivityEvent{
			ID:               uuid.New(),
			OrganizationID:   org.ID,
			ContactID:        contact.ID,
			EventType:        models.CustomerActivityMessageIncoming,
			Category:         models.CustomerActivityCategoryMessage,
			Title:            label,
			Summary:          label,
			ActorType:        models.CustomerActivityActorContact,
			SourceObjectType: "message",
			SourceObjectID:   &message.ID,
			OccurredAt:       occurredAt,
			Metadata:         models.JSONB{},
			IdempotencyKey:   "workspace-current:" + message.ID.String(),
		}).Error)
		return message
	}
	createMessageAndActivity("must remain hidden", cutoff.Add(-time.Minute))
	createMessageAndActivity("current conversation", cutoff.Add(time.Minute))

	workspaceReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(workspaceReq, org.ID, user.ID)
	testutil.SetPathParam(workspaceReq, "id", contact.ID.String())
	require.NoError(t, app.GetCustomerWorkspace(workspaceReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(workspaceReq))
	var workspace CustomerWorkspaceResponse
	testutil.ParseEnvelopeResponse(t, workspaceReq, &workspace)
	require.True(t, workspace.Capabilities.Messages)
	require.Len(t, workspace.Timeline, 1)
	require.Equal(t, "current conversation", workspace.Timeline[0].Title)
	require.NotContains(t, string(testutil.GetResponseBody(workspaceReq)), "must remain hidden")

	activitiesReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(activitiesReq, org.ID, user.ID)
	testutil.SetPathParam(activitiesReq, "id", contact.ID.String())
	require.NoError(t, app.ListCustomerActivities(activitiesReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(activitiesReq))
	var activities struct {
		Activities []CustomerTimelineItem `json:"activities"`
		Total      int64                  `json:"total"`
	}
	testutil.ParseEnvelopeResponse(t, activitiesReq, &activities)
	require.EqualValues(t, 1, activities.Total)
	require.Len(t, activities.Activities, 1)
	require.Equal(t, "current conversation", activities.Activities[0].Title)
}

func TestCustomerWorkspaceCurrentConversationOnlyFailsClosedWithoutBoundary(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-no-conversation", []string{
		models.ResourceChat + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, db, org.ID)
	require.NoError(t, db.Model(contact).Update("assigned_user_id", user.ID).Error)
	require.NoError(t, db.Create(&models.ChatbotSettings{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: "",
		AgentAssignment: models.AgentAssignmentConfig{
			CurrentConversationOnly: true,
		},
	}).Error)
	message := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		WhatsAppAccount:   "workspace-no-conversation",
		ContactID:         contact.ID,
		WhatsAppMessageID: "workspace-no-conversation-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "history without a safe boundary",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	require.NoError(t, db.Create(&models.CustomerActivityEvent{
		ID:               uuid.New(),
		OrganizationID:   org.ID,
		ContactID:        contact.ID,
		EventType:        models.CustomerActivityMessageIncoming,
		Category:         models.CustomerActivityCategoryMessage,
		Title:            "history without a safe boundary",
		ActorType:        models.CustomerActivityActorContact,
		SourceObjectType: "message",
		SourceObjectID:   &message.ID,
		OccurredAt:       message.CreatedAt,
		Metadata:         models.JSONB{},
		IdempotencyKey:   "workspace-no-conversation:" + message.ID.String(),
	}).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", contact.ID.String())
	require.NoError(t, app.GetCustomerWorkspace(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var workspace CustomerWorkspaceResponse
	testutil.ParseEnvelopeResponse(t, req, &workspace)
	require.True(t, workspace.Capabilities.Messages)
	require.Empty(t, workspace.Timeline)
	require.NotContains(t, string(testutil.GetResponseBody(req)), "history without a safe boundary")
}

func TestCustomerWorkspaceIdentityIsEntitledRedactedAndMasked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	require.NoError(t, db.Model(org).Update(
		"settings",
		models.JSONB{"mask_phone_numbers": true},
	).Error)
	adminRole := testutil.CreateAdminRole(t, db, org.ID)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, db, org.ID)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "workspace-test-provider",
		Name:              "Workspace identity account",
		ExternalAccountID: "account-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	const rawPhone = "+60123456789"
	const rawExternalID = "provider-secret-external-id"
	identity := &models.ContactIdentity{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		ContactID:         contact.ID,
		ChannelAccountID:  account.ID,
		Channel:           models.ChannelWhatsApp,
		ExternalID:        rawExternalID,
		Address:           rawPhone,
		NormalizedAddress: rawPhone,
		DisplayName:       rawPhone,
		IsPrimary:         true,
		IsVerified:        true,
		Metadata:          models.JSONB{"private_provider_key": "must-not-leak"},
	}
	require.NoError(t, db.Create(identity).Error)

	load := func() (*fastglue.Request, CustomerWorkspaceResponse) {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", contact.ID.String())
		require.NoError(t, app.GetCustomerWorkspace(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var response CustomerWorkspaceResponse
		testutil.ParseEnvelopeResponse(t, req, &response)
		return req, response
	}

	_, unlicensed := load()
	require.False(t, unlicensed.Capabilities.Omnichannel)
	require.Empty(t, unlicensed.Identities)

	enableCustomerWorkspaceEntitlements(t, db, org.ID, user.ID, "omnichannel.enabled")
	req, licensed := load()
	require.True(t, licensed.Capabilities.Omnichannel)
	require.Len(t, licensed.Identities, 1)
	require.NotEqual(t, rawPhone, licensed.Identities[0].Address)
	require.Contains(t, licensed.Identities[0].Address, "6789")
	require.NotEqual(t, rawPhone, licensed.Identities[0].DisplayName)
	body := string(testutil.GetResponseBody(req))
	require.NotContains(t, body, rawExternalID)
	require.NotContains(t, body, "private_provider_key")
	require.NotContains(t, body, rawPhone)
}

func TestMergeContactRequiresVisibilityToTargetAndSource(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(t, db, org.ID, "workspace-merge-assigned", []string{
		models.ResourceContacts + ":" + models.ActionWrite,
	})
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	target := testutil.CreateTestContact(t, db, org.ID)
	source := testutil.CreateTestContact(t, db, org.ID)
	require.NoError(t, db.Model(target).Update("assigned_user_id", user.ID).Error)

	preview := func() *fastglue.Request {
		req := testutil.NewJSONRequest(t, MergeContactRequest{SourceContactID: source.ID})
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", target.ID.String())
		require.NoError(t, app.MergeContact(req))
		return req
	}
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(preview()))

	require.NoError(t, db.Model(source).Update("assigned_user_id", user.ID).Error)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(preview()))
}

func TestMergeContactPreviewBlocksActiveTransferAndSessionCollisions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, org.ID)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&adminRole.ID))
	target := testutil.CreateTestContact(t, db, org.ID)
	source := testutil.CreateTestContact(t, db, org.ID)
	now := time.Now().UTC()

	for _, contact := range []*models.Contact{target, source} {
		transfer := &models.AgentTransfer{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			ContactID:      contact.ID,
			PhoneNumber:    contact.PhoneNumber,
			Status:         models.TransferStatusActive,
			Source:         models.TransferSourceManual,
			TransferredAt:  now,
		}
		require.NoError(t, db.Create(transfer).Error)
		session := &models.ChatbotSession{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  org.ID,
			ContactID:       contact.ID,
			WhatsAppAccount: "workspace-session-account",
			PhoneNumber:     contact.PhoneNumber,
			Status:          models.SessionStatusActive,
			SessionData:     models.JSONB{},
			StartedAt:       now,
			LastActivityAt:  now,
		}
		require.NoError(t, db.Create(session).Error)
	}

	req := testutil.NewJSONRequest(t, MergeContactRequest{SourceContactID: source.ID})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", target.ID.String())
	require.NoError(t, app.MergeContact(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var preview ContactMergePreview
	testutil.ParseEnvelopeResponse(t, req, &preview)
	require.Contains(t, preview.Collisions, "multiple active agent transfers would overlap")
	require.Contains(t, preview.Collisions, "active chatbot sessions overlap")

	confirmReq := testutil.NewJSONRequest(t, MergeContactRequest{
		SourceContactID: source.ID,
		Confirm:         true,
		IdempotencyKey:  "collision-must-fail",
	})
	testutil.SetAuthContext(confirmReq, org.ID, user.ID)
	testutil.SetPathParam(confirmReq, "id", target.ID.String())
	require.NoError(t, app.MergeContact(confirmReq))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(confirmReq))
	var collisionDetails struct {
		Collisions []string `json:"collisions"`
	}
	testutil.ParseEnvelopeResponse(t, confirmReq, &collisionDetails)
	require.ElementsMatch(t, []string{
		"multiple active agent transfers would overlap",
		"active chatbot sessions overlap",
	}, collisionDetails.Collisions)
}

func TestMergeContactPreviewConfirmReplayAndAliasWorkspace(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, org.ID)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&adminRole.ID))
	target := testutil.CreateTestContact(t, db, org.ID)
	source := testutil.CreateTestContact(t, db, org.ID)
	priorAlias := testutil.CreateTestContact(t, db, org.ID)
	priorMergeAt := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, db.Unscoped().Model(priorAlias).Updates(map[string]any{
		"merged_into_id": source.ID,
		"merged_at":      priorMergeAt,
		"merged_by_id":   user.ID,
		"deleted_at":     priorMergeAt,
	}).Error)
	consent := &models.ConsentEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      &source.ID,
		SubjectType:    "contact",
		SubjectKey:     source.PhoneNumber,
		Purpose:        "marketing",
		Channel:        string(models.ChannelWhatsApp),
		Action:         models.ConsentActionWithdrawn,
		Source:         "workspace-test",
		Evidence:       models.JSONB{},
		CapturedAt:     priorMergeAt,
	}
	require.NoError(t, db.Create(consent).Error)
	priorMessage := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New(), CreatedAt: priorMergeAt},
		OrganizationID:    org.ID,
		WhatsAppAccount:   "workspace-alias-account",
		ContactID:         priorAlias.ID,
		WhatsAppMessageID: "workspace-alias-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "Message retained through an older alias",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(priorMessage).Error)

	mergeRequest := func(confirm bool, idempotencyKey string) *fastglue.Request {
		req := testutil.NewJSONRequest(t, MergeContactRequest{
			SourceContactID: source.ID,
			Confirm:         confirm,
			IdempotencyKey:  idempotencyKey,
			Reason:          "duplicate profile",
		})
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", target.ID.String())
		return req
	}

	previewReq := mergeRequest(false, "")
	require.NoError(t, app.MergeContact(previewReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(previewReq))
	var preview ContactMergePreview
	testutil.ParseEnvelopeResponse(t, previewReq, &preview)
	require.True(t, preview.ConfirmRequired)
	require.Equal(t, target.ID, preview.TargetContactID)
	require.Equal(t, source.ID, preview.SourceContactID)
	require.Empty(t, preview.Collisions)
	require.EqualValues(t, 1, preview.Preserved["consent_events"])
	require.EqualValues(t, 1, preview.Preserved["contact_aliases"])

	const mergeKey = "merge-replay-contract"
	confirmReq := mergeRequest(true, mergeKey)
	require.NoError(t, app.MergeContact(confirmReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(confirmReq))
	type mergeResponse struct {
		Merged          bool             `json:"merged"`
		TargetContactID uuid.UUID        `json:"target_contact_id"`
		SourceContactID uuid.UUID        `json:"source_contact_id"`
		Preserved       map[string]int64 `json:"preserved"`
	}
	var firstMerge mergeResponse
	testutil.ParseEnvelopeResponse(t, confirmReq, &firstMerge)
	require.True(t, firstMerge.Merged)
	require.Equal(t, target.ID, firstMerge.TargetContactID)
	require.Equal(t, source.ID, firstMerge.SourceContactID)

	var mergedSource models.Contact
	require.NoError(t, db.Unscoped().
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		First(&mergedSource).Error)
	require.True(t, mergedSource.DeletedAt.Valid)
	require.NotNil(t, mergedSource.MergedIntoID)
	require.Equal(t, target.ID, *mergedSource.MergedIntoID)
	var flattenedAlias models.Contact
	require.NoError(t, db.Unscoped().
		Where("id = ? AND organization_id = ?", priorAlias.ID, org.ID).
		First(&flattenedAlias).Error)
	require.NotNil(t, flattenedAlias.MergedIntoID)
	require.Equal(t, target.ID, *flattenedAlias.MergedIntoID)
	var preservedConsent models.ConsentEvent
	require.NoError(t, db.Where("id = ? AND organization_id = ?", consent.ID, org.ID).
		First(&preservedConsent).Error)
	require.NotNil(t, preservedConsent.ContactID)
	require.Equal(t, source.ID, *preservedConsent.ContactID)

	// A replay represents the original successful operation. Facts written
	// later must remain visible to previews without retroactively turning the
	// exact same request into a collision failure or changing its response.
	laterAt := time.Now().UTC()
	for _, contact := range []*models.Contact{target, source} {
		require.NoError(t, db.Create(&models.AgentTransfer{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			ContactID:      contact.ID,
			PhoneNumber:    contact.PhoneNumber,
			Status:         models.TransferStatusActive,
			Source:         models.TransferSourceManual,
			TransferredAt:  laterAt,
		}).Error)
		require.NoError(t, db.Create(&models.ChatbotSession{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  org.ID,
			ContactID:       contact.ID,
			WhatsAppAccount: "workspace-later-collision",
			PhoneNumber:     contact.PhoneNumber,
			Status:          models.SessionStatusActive,
			SessionData:     models.JSONB{},
			StartedAt:       laterAt,
			LastActivityAt:  laterAt,
		}).Error)
	}
	channelAccount := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelWebChat,
		Provider:          "workspace-replay-test",
		Name:              "Workspace replay collision",
		ExternalAccountID: "workspace-replay-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(channelAccount).Error)
	for _, contact := range []*models.Contact{target, source} {
		require.NoError(t, db.Create(&models.ContactChannelPreference{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   org.ID,
			ContactID:        contact.ID,
			ChannelAccountID: channelAccount.ID,
			Channel:          channelAccount.Channel,
			Purpose:          models.ChannelPreferencePurposeService,
			Status:           models.ChannelPreferenceStatusOptedIn,
			Source:           "workspace-replay-test",
			QuietHours:       models.JSONB{},
			Config:           models.JSONB{},
			Metadata:         models.JSONB{},
		}).Error)
		consentEvent := &models.ConsentEvent{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			ContactID:      &contact.ID,
			SubjectType:    "contact",
			SubjectKey:     "workspace-replay-" + contact.ID.String(),
			Purpose:        string(models.ChannelPreferencePurposeService),
			Channel:        string(models.ChannelWebChat),
			Action:         models.ConsentActionGranted,
			Source:         "workspace-replay-test",
			Evidence:       models.JSONB{},
			CapturedAt:     laterAt,
		}
		require.NoError(t, db.Create(consentEvent).Error)
		require.NoError(t, db.Create(&models.ConsentState{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			ContactID:      &contact.ID,
			SubjectType:    consentEvent.SubjectType,
			SubjectKey:     consentEvent.SubjectKey,
			Purpose:        consentEvent.Purpose,
			Channel:        consentEvent.Channel,
			Status:         models.ConsentStatusGranted,
			LatestEventID:  consentEvent.ID,
			EffectiveAt:    laterAt,
			Metadata:       models.JSONB{},
		}).Error)
	}

	laterPreviewReq := mergeRequest(false, "")
	require.NoError(t, app.MergeContact(laterPreviewReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(laterPreviewReq))
	var laterPreview ContactMergePreview
	testutil.ParseEnvelopeResponse(t, laterPreviewReq, &laterPreview)
	require.ElementsMatch(t, []string{
		"contact channel preferences overlap",
		"multiple active agent transfers would overlap",
		"active chatbot sessions overlap",
		"consent states overlap",
	}, laterPreview.Collisions)

	replayReq := mergeRequest(true, mergeKey)
	require.NoError(t, app.MergeContact(replayReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replayReq))
	var replayMerge mergeResponse
	testutil.ParseEnvelopeResponse(t, replayReq, &replayMerge)
	require.Equal(t, firstMerge, replayMerge)

	var activityCount, outboxCount int64
	require.NoError(t, db.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND idempotency_key = ?", org.ID, "contact-merge:"+mergeKey).
		Count(&activityCount).Error)
	require.EqualValues(t, 1, activityCount)
	require.NoError(t, db.Model(&models.OutboxEvent{}).
		Where("organization_id = ? AND event_type = ?", org.ID, models.CustomerActivityContactMerged).
		Count(&outboxCount).Error)
	require.EqualValues(t, 1, outboxCount)

	wrongReplayReq := mergeRequest(true, "different-merge-key")
	require.NoError(t, app.MergeContact(wrongReplayReq))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(wrongReplayReq))

	differentSource := testutil.CreateTestContact(t, db, org.ID)
	differentMergeAt := time.Now().UTC()
	require.NoError(t, db.Unscoped().Model(differentSource).Updates(map[string]any{
		"merged_into_id": target.ID,
		"merged_at":      differentMergeAt,
		"merged_by_id":   user.ID,
		"deleted_at":     differentMergeAt,
	}).Error)
	falseReplayReq := testutil.NewJSONRequest(t, MergeContactRequest{
		SourceContactID: differentSource.ID,
		Confirm:         true,
		IdempotencyKey:  mergeKey,
	})
	testutil.SetAuthContext(falseReplayReq, org.ID, user.ID)
	testutil.SetPathParam(falseReplayReq, "id", target.ID.String())
	require.NoError(t, app.MergeContact(falseReplayReq))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(falseReplayReq))

	createFromAliasReq := testutil.NewJSONRequest(t, CreateContactRequest{
		PhoneNumber: source.PhoneNumber,
		ProfileName: "Canonical update through alias",
	})
	testutil.SetAuthContext(createFromAliasReq, org.ID, user.ID)
	require.NoError(t, app.CreateContact(createFromAliasReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createFromAliasReq))
	var createFromAlias ContactResponse
	testutil.ParseEnvelopeResponse(t, createFromAliasReq, &createFromAlias)
	require.Equal(t, target.ID, createFromAlias.ID)
	require.NoError(t, db.Unscoped().
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		First(&mergedSource).Error)
	require.True(t, mergedSource.DeletedAt.Valid)

	aliasReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(aliasReq, org.ID, user.ID)
	testutil.SetPathParam(aliasReq, "id", source.ID.String())
	require.NoError(t, app.GetCustomerWorkspace(aliasReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(aliasReq))
	var workspace CustomerWorkspaceResponse
	testutil.ParseEnvelopeResponse(t, aliasReq, &workspace)
	require.Equal(t, target.ID, workspace.Contact.ID)
	require.NotEmpty(t, workspace.Timeline)
	require.Equal(t, string(models.CustomerActivityContactMerged), workspace.Timeline[0].Type)

	priorAliasReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(priorAliasReq, org.ID, user.ID)
	testutil.SetPathParam(priorAliasReq, "id", priorAlias.ID.String())
	require.NoError(t, app.GetCustomerWorkspace(priorAliasReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(priorAliasReq))
	var priorAliasWorkspace CustomerWorkspaceResponse
	testutil.ParseEnvelopeResponse(t, priorAliasReq, &priorAliasWorkspace)
	require.Equal(t, target.ID, priorAliasWorkspace.Contact.ID)
	foundPriorMessage := false
	for _, item := range priorAliasWorkspace.Timeline {
		if item.SourceID != nil && *item.SourceID == priorMessage.ID {
			foundPriorMessage = true
			break
		}
	}
	require.True(t, foundPriorMessage)
}

type customerWorkspaceMergeIdentityFixture struct {
	app              *App
	db               *gorm.DB
	organization     *models.Organization
	account          *models.WhatsAppAccount
	user             *models.User
	target           *models.Contact
	targetAlias      *models.Contact
	targetGrandchild *models.Contact
	source           *models.Contact
	sourceAlias      *models.Contact
	sourceGrandchild *models.Contact
}

func newCustomerWorkspaceMergeIdentityFixture(t *testing.T) customerWorkspaceMergeIdentityFixture {
	t.Helper()
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	return customerWorkspaceMergeIdentityFixture{
		app:              app,
		db:               db,
		organization:     organization,
		account:          account,
		user:             user,
		target:           testutil.CreateTestContact(t, db, organization.ID),
		targetAlias:      testutil.CreateTestContact(t, db, organization.ID),
		targetGrandchild: testutil.CreateTestContact(t, db, organization.ID),
		source:           testutil.CreateTestContact(t, db, organization.ID),
		sourceAlias:      testutil.CreateTestContact(t, db, organization.ID),
		sourceGrandchild: testutil.CreateTestContact(t, db, organization.ID),
	}
}

func (fixture customerWorkspaceMergeIdentityFixture) attachAliases(t *testing.T) {
	t.Helper()
	mergedAt := time.Now().UTC().Add(-time.Minute)
	for _, pair := range []struct {
		alias     *models.Contact
		canonical *models.Contact
	}{
		{alias: fixture.targetAlias, canonical: fixture.target},
		{alias: fixture.targetGrandchild, canonical: fixture.targetAlias},
		{alias: fixture.sourceAlias, canonical: fixture.source},
		{alias: fixture.sourceGrandchild, canonical: fixture.sourceAlias},
	} {
		require.NoError(t, fixture.db.Unscoped().Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", pair.alias.ID, fixture.organization.ID).
			Updates(map[string]any{
				"merged_into_id": pair.canonical.ID,
				"merged_at":      mergedAt,
				"merged_by_id":   fixture.user.ID,
				"deleted_at":     mergedAt,
			}).Error)
	}
}

func (fixture customerWorkspaceMergeIdentityFixture) createOpenIdentityReviewHold(
	t *testing.T,
	contact *models.Contact,
) *WhatsAppIdentityReviewSnapshot {
	t.Helper()
	principal := "contact-merge-review-" + uuid.NewString()
	require.NoError(t, fixture.db.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", contact.ID, fixture.organization.ID).
		Update("bs_uid", principal).Error)
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID:          fixture.organization.ID,
		WhatsAppAccountID:       fixture.account.ID,
		OnboardingCycle:         1,
		DirectPrimaryBSUID:      principal,
		VerifiedEventDigest:     strings.Repeat("a", 64),
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		SelectorBodyDigest:      strings.Repeat("b", 64),
	}
	var snapshot *WhatsAppIdentityReviewSnapshot
	var created bool
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		snapshot, created, err = fixture.app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))
	require.True(t, created)
	require.NotNil(t, snapshot)
	return snapshot
}

func (fixture customerWorkspaceMergeIdentityFixture) mergeRequest(
	t *testing.T,
	idempotencyKey string,
) *fastglue.Request {
	t.Helper()
	return fixture.mergePairRequest(t, fixture.target.ID, fixture.source.ID, idempotencyKey)
}

func (fixture customerWorkspaceMergeIdentityFixture) mergePairRequest(
	t *testing.T,
	targetID, sourceID uuid.UUID,
	idempotencyKey string,
) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, MergeContactRequest{
		SourceContactID: sourceID,
		Confirm:         true,
		IdempotencyKey:  idempotencyKey,
		Reason:          "duplicate identity",
	})
	testutil.SetAuthContext(req, fixture.organization.ID, fixture.user.ID)
	testutil.SetPathParam(req, "id", targetID.String())
	return req
}

func TestMergeContactExactReplaySurvivesLaterCanonicalMerge(t *testing.T) {
	fixture := newCustomerWorkspaceMergeIdentityFixture(t)
	finalTarget := testutil.CreateTestContact(t, fixture.db, fixture.organization.ID)
	loadFirstMergeEvidence := func() (models.CustomerActivityEvent, models.OutboxEvent) {
		t.Helper()
		var activities []models.CustomerActivityEvent
		require.NoError(t, fixture.db.Where(
			"organization_id = ? AND idempotency_key = ?",
			fixture.organization.ID,
			"contact-merge:merge-chain-first",
		).Find(&activities).Error)
		require.Len(t, activities, 1)

		var outboxes []models.OutboxEvent
		require.NoError(t, fixture.db.Where(
			"organization_id = ? AND idempotency_key = ?",
			fixture.organization.ID,
			"customer-activity-webhook:"+activities[0].ID.String(),
		).Find(&outboxes).Error)
		require.Len(t, outboxes, 1)
		return activities[0], outboxes[0]
	}

	type mergeResponse struct {
		Merged          bool             `json:"merged"`
		TargetContactID uuid.UUID        `json:"target_contact_id"`
		SourceContactID uuid.UUID        `json:"source_contact_id"`
		Preserved       map[string]int64 `json:"preserved"`
	}

	firstRequest := fixture.mergePairRequest(
		t,
		fixture.target.ID,
		fixture.source.ID,
		"merge-chain-first",
	)
	require.NoError(t, fixture.app.MergeContact(firstRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(firstRequest))
	var firstResponse mergeResponse
	testutil.ParseEnvelopeResponse(t, firstRequest, &firstResponse)
	firstActivity, firstOutbox := loadFirstMergeEvidence()
	require.Equal(t, fixture.target.ID, firstActivity.ContactID)
	require.Equal(t, &fixture.source.ID, firstActivity.SourceObjectID)
	require.Equal(t, string(models.CustomerActivityContactMerged), firstOutbox.EventType)
	require.Equal(t, "contact", firstOutbox.AggregateType)
	require.Equal(t, &fixture.source.ID, firstOutbox.AggregateID)
	require.Equal(t, firstActivity.ID.String(), firstOutbox.Payload["activity_event_id"])
	require.Equal(t, fixture.target.ID.String(), firstOutbox.Payload["contact_id"])
	require.Equal(t, fixture.source.ID.String(), firstOutbox.Payload["source_id"])

	secondRequest := fixture.mergePairRequest(
		t,
		finalTarget.ID,
		fixture.target.ID,
		"merge-chain-second",
	)
	require.NoError(t, fixture.app.MergeContact(secondRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondRequest))

	replayRequest := fixture.mergePairRequest(
		t,
		fixture.target.ID,
		fixture.source.ID,
		"merge-chain-first",
	)
	require.NoError(t, fixture.app.MergeContact(replayRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replayRequest))
	var replayResponse mergeResponse
	testutil.ParseEnvelopeResponse(t, replayRequest, &replayResponse)
	require.Equal(t, firstResponse, replayResponse)

	replayActivity, replayOutbox := loadFirstMergeEvidence()
	require.Equal(t, firstActivity, replayActivity)
	require.Equal(t, firstOutbox.ID, replayOutbox.ID)
	require.Equal(t, firstOutbox.EventType, replayOutbox.EventType)
	require.Equal(t, firstOutbox.AggregateType, replayOutbox.AggregateType)
	require.Equal(t, firstOutbox.AggregateID, replayOutbox.AggregateID)
	require.Equal(t, firstOutbox.IdempotencyKey, replayOutbox.IdempotencyKey)
	require.Equal(t, firstOutbox.Payload, replayOutbox.Payload)
}

func TestMergeContactRejectsOpenIdentityReviewAcrossCompleteFamilies(t *testing.T) {
	cases := []struct {
		name   string
		member func(customerWorkspaceMergeIdentityFixture) *models.Contact
	}{
		{name: "target root", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.target }},
		{name: "target alias", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.targetAlias }},
		{name: "target multi-hop descendant", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.targetGrandchild }},
		{name: "source root", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.source }},
		{name: "source alias", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.sourceAlias }},
		{name: "source multi-hop descendant", member: func(fixture customerWorkspaceMergeIdentityFixture) *models.Contact { return fixture.sourceGrandchild }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCustomerWorkspaceMergeIdentityFixture(t)
			memberContact := testCase.member(fixture)
			hold := fixture.createOpenIdentityReviewHold(t, memberContact)
			fixture.attachAliases(t)

			var memberBefore models.WhatsAppIdentityReviewMember
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND hold_id = ? AND contact_id = ?",
				fixture.organization.ID,
				hold.HoldID,
				memberContact.ID,
			).First(&memberBefore).Error)

			req := fixture.mergeRequest(t, "open-review-"+strings.ReplaceAll(testCase.name, " ", "-"))
			require.NoError(t, fixture.app.MergeContact(req))
			require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
			require.Contains(t, string(testutil.GetResponseBody(req)), "WhatsApp identity review is open")

			var persistedSource models.Contact
			require.NoError(t, fixture.db.Unscoped().Where(
				"id = ? AND organization_id = ?",
				fixture.source.ID,
				fixture.organization.ID,
			).First(&persistedSource).Error)
			require.False(t, persistedSource.DeletedAt.Valid)
			require.Nil(t, persistedSource.MergedIntoID)

			var persistedTargetAlias, persistedTargetChild models.Contact
			var persistedSourceAlias, persistedSourceChild models.Contact
			require.NoError(t, fixture.db.Unscoped().First(
				&persistedTargetAlias,
				"id = ? AND organization_id = ?",
				fixture.targetAlias.ID,
				fixture.organization.ID,
			).Error)
			require.NoError(t, fixture.db.Unscoped().First(
				&persistedTargetChild,
				"id = ? AND organization_id = ?",
				fixture.targetGrandchild.ID,
				fixture.organization.ID,
			).Error)
			require.NoError(t, fixture.db.Unscoped().First(
				&persistedSourceAlias,
				"id = ? AND organization_id = ?",
				fixture.sourceAlias.ID,
				fixture.organization.ID,
			).Error)
			require.NoError(t, fixture.db.Unscoped().First(
				&persistedSourceChild,
				"id = ? AND organization_id = ?",
				fixture.sourceGrandchild.ID,
				fixture.organization.ID,
			).Error)
			require.Equal(t, fixture.target.ID, *persistedTargetAlias.MergedIntoID)
			require.Equal(t, fixture.targetAlias.ID, *persistedTargetChild.MergedIntoID)
			require.Equal(t, fixture.source.ID, *persistedSourceAlias.MergedIntoID)
			require.Equal(t, fixture.sourceAlias.ID, *persistedSourceChild.MergedIntoID)

			var memberAfter models.WhatsAppIdentityReviewMember
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND hold_id = ? AND contact_id = ?",
				fixture.organization.ID,
				hold.HoldID,
				memberContact.ID,
			).First(&memberAfter).Error)
			require.Equal(t, memberBefore, memberAfter, "immutable review membership must not be rewritten")
			var persistedHold models.WhatsAppIdentityReviewHold
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND id = ?",
				fixture.organization.ID,
				hold.HoldID,
			).First(&persistedHold).Error)
			require.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, persistedHold.Disposition)
			require.Equal(t, uint64(1), persistedHold.Version)
		})
	}
}

func TestMergeContactAllowsResolvedAndSupersededIdentityReviewHolds(t *testing.T) {
	cases := []struct {
		name        string
		disposition models.WhatsAppIdentityReviewDisposition
		resolve     func(*testing.T, customerWorkspaceMergeIdentityFixture, *WhatsAppIdentityReviewSnapshot)
	}{
		{
			name:        "future routing decision",
			disposition: models.WhatsAppIdentityReviewDispositionFutureRouting,
			resolve: func(t *testing.T, fixture customerWorkspaceMergeIdentityFixture, hold *WhatsAppIdentityReviewSnapshot) {
				t.Helper()
				resolvedAt := time.Now().UTC()
				require.NoError(t, fixture.db.Model(&models.WhatsAppIdentityReviewHold{}).
					Where("organization_id = ? AND id = ?", fixture.organization.ID, hold.HoldID).
					Updates(map[string]any{
						"version":                    2,
						"disposition":                models.WhatsAppIdentityReviewDispositionFutureRouting,
						"decision_target_contact_id": fixture.source.ID,
						"decision_resolved_by_id":    fixture.user.ID,
						"decision_resolved_at":       resolvedAt,
						"decision_request_id":        uuid.New(),
						"decision_request_digest":    strings.Repeat("c", 64),
						"decision_chain_digest":      strings.Repeat("d", 64),
					}).Error)
			},
		},
		{
			name:        "later onboarding cycle",
			disposition: models.WhatsAppIdentityReviewDispositionSupersededByCycle,
			resolve: func(t *testing.T, fixture customerWorkspaceMergeIdentityFixture, _ *WhatsAppIdentityReviewSnapshot) {
				t.Helper()
				require.NoError(t, fixture.db.Model(&models.WhatsAppCoexistenceState{}).
					Where(
						"organization_id = ? AND whats_app_account_id = ?",
						fixture.organization.ID,
						fixture.account.ID,
					).
					Update("onboarding_cycle", 2).Error)
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCustomerWorkspaceMergeIdentityFixture(t)
			hold := fixture.createOpenIdentityReviewHold(t, fixture.source)
			testCase.resolve(t, fixture, hold)
			fixture.attachAliases(t)

			var memberBefore models.WhatsAppIdentityReviewMember
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND hold_id = ? AND contact_id = ?",
				fixture.organization.ID,
				hold.HoldID,
				fixture.source.ID,
			).First(&memberBefore).Error)
			var resolved models.WhatsAppIdentityReviewHold
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND id = ?",
				fixture.organization.ID,
				hold.HoldID,
			).First(&resolved).Error)
			require.Equal(t, testCase.disposition, resolved.Disposition)

			req := fixture.mergeRequest(t, "closed-review-"+strings.ReplaceAll(testCase.name, " ", "-"))
			require.NoError(t, fixture.app.MergeContact(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

			var persistedSource models.Contact
			require.NoError(t, fixture.db.Unscoped().Where(
				"id = ? AND organization_id = ?",
				fixture.source.ID,
				fixture.organization.ID,
			).First(&persistedSource).Error)
			require.True(t, persistedSource.DeletedAt.Valid)
			require.NotNil(t, persistedSource.MergedIntoID)
			require.Equal(t, fixture.target.ID, *persistedSource.MergedIntoID)

			var memberAfter models.WhatsAppIdentityReviewMember
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND hold_id = ? AND contact_id = ?",
				fixture.organization.ID,
				hold.HoldID,
				fixture.source.ID,
			).First(&memberAfter).Error)
			require.Equal(t, memberBefore, memberAfter, "resolved review membership must remain immutable")
		})
	}
}

func TestMergeContactExactReplayPrecedesOpenIdentityReviewCheck(t *testing.T) {
	fixture := newCustomerWorkspaceMergeIdentityFixture(t)
	const idempotencyKey = "merge-before-later-review"

	first := fixture.mergeRequest(t, idempotencyKey)
	require.NoError(t, fixture.app.MergeContact(first))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))

	hold := fixture.createOpenIdentityReviewHold(t, fixture.target)
	replay := fixture.mergeRequest(t, idempotencyKey)
	require.NoError(t, fixture.app.MergeContact(replay))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replay))

	var activityCount int64
	require.NoError(t, fixture.db.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND idempotency_key = ?",
			fixture.organization.ID,
			"contact-merge:"+idempotencyKey,
		).
		Count(&activityCount).Error)
	require.EqualValues(t, 1, activityCount)
	var persistedHold models.WhatsAppIdentityReviewHold
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND id = ?",
		fixture.organization.ID,
		hold.HoldID,
	).First(&persistedHold).Error)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, persistedHold.Disposition)
}

func TestMergeContactWaitsForPhysicalAIAttemptFence(t *testing.T) {
	fixture := newCustomerWorkspaceMergeIdentityFixture(t)
	request := fixture.mergeRequest(t, "merge-policy-fence")

	attemptTx := fixture.db.Begin()
	require.NoError(t, attemptTx.Error)
	t.Cleanup(func() { _ = attemptTx.Rollback().Error })
	require.NoError(t, database.LockOrganizationAIAttemptScope(attemptTx, fixture.organization.ID))
	var blockerPID int
	require.NoError(t, attemptTx.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error)
	require.Positive(t, blockerPID)

	type mergeResult struct {
		err    error
		status int
	}
	writerPID := make(chan int, 1)
	writerDone := make(chan mergeResult, 1)
	go func() {
		result := mergeResult{}
		result.err = fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				writerPID <- 0
				return err
			}
			writerPID <- backendPID
			scoped := &App{
				DB:     session,
				Log:    fixture.app.Log,
				Config: fixture.app.Config,
			}
			err := scoped.MergeContact(request)
			result.status = testutil.GetResponseStatusCode(request)
			return err
		})
		writerDone <- result
	}()

	backendPID := <-writerPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	var blockingMatches int64
	require.NoError(t, fixture.db.Raw(`
		SELECT COUNT(*)
		  FROM pg_catalog.unnest(pg_catalog.pg_blocking_pids(?)) AS blocker(pid)
		 WHERE blocker.pid = ?
	`, backendPID, blockerPID).Scan(&blockingMatches).Error)
	require.EqualValues(t, 1, blockingMatches)
	var contactLocks []uuid.UUID
	require.NoError(t, attemptTx.Raw(`
		SELECT id
		  FROM contacts
		 WHERE organization_id = ?
		   AND id IN ?
		 ORDER BY id
		 FOR UPDATE NOWAIT
	`, fixture.organization.ID, []uuid.UUID{fixture.target.ID, fixture.source.ID}).Scan(&contactLocks).Error)
	require.ElementsMatch(t, []uuid.UUID{fixture.target.ID, fixture.source.ID}, contactLocks,
		"the waiting merge must not lock contacts before the organization fence")

	var beforeRelease models.Contact
	require.NoError(t, fixture.db.Unscoped().Where(
		"id = ? AND organization_id = ?",
		fixture.source.ID,
		fixture.organization.ID,
	).First(&beforeRelease).Error)
	require.False(t, beforeRelease.DeletedAt.Valid)
	require.Nil(t, beforeRelease.MergedIntoID)
	select {
	case result := <-writerDone:
		require.Failf(t, "contact merge bypassed the policy fence", "result: %+v", result)
	default:
	}

	require.NoError(t, attemptTx.Commit().Error)
	select {
	case result := <-writerDone:
		require.NoError(t, result.err)
		require.Equal(t, fasthttp.StatusOK, result.status)
	case <-time.After(5 * time.Second):
		require.Fail(t, "contact merge did not resume after the attempt fence")
	}

	var persistedSource models.Contact
	require.NoError(t, fixture.db.Unscoped().Where(
		"id = ? AND organization_id = ?",
		fixture.source.ID,
		fixture.organization.ID,
	).First(&persistedSource).Error)
	require.True(t, persistedSource.DeletedAt.Valid)
	require.NotNil(t, persistedSource.MergedIntoID)
	require.Equal(t, fixture.target.ID, *persistedSource.MergedIntoID)
}
