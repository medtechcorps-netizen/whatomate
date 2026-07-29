package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
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

	replayReq := mergeRequest(true, mergeKey)
	require.NoError(t, app.MergeContact(replayReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replayReq))

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
