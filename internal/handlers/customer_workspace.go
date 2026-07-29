package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type customerActivityInput struct {
	ContactID        uuid.UUID
	LeadID           *uuid.UUID
	EventType        models.CustomerActivityEventType
	Category         models.CustomerActivityCategory
	Title            string
	Summary          string
	ActorType        models.CustomerActivityActorType
	ActorUserID      *uuid.UUID
	SourceObjectType string
	SourceObjectID   *uuid.UUID
	OccurredAt       time.Time
	Metadata         models.JSONB
	WebhookData      models.JSONB
	IdempotencyKey   string
}

// recordCustomerActivity writes the customer timeline event and webhook outbox
// row on the caller's transaction. Replays return the original event; an
// idempotency key reused for a different fact fails closed.
func recordCustomerActivity(
	tx *gorm.DB,
	orgID uuid.UUID,
	input customerActivityInput,
) (*models.CustomerActivityEvent, error) {
	if tx == nil || orgID == uuid.Nil || input.ContactID == uuid.Nil {
		return nil, errors.New("customer activity requires transaction, organization, and contact")
	}
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 255 {
		return nil, errors.New("customer activity idempotency key is required and must not exceed 255 characters")
	}
	if input.EventType == "" || input.Category == "" || strings.TrimSpace(input.Title) == "" {
		return nil, errors.New("customer activity type, category, and title are required")
	}
	if input.ActorType == "" {
		input.ActorType = models.CustomerActivityActorSystem
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now().UTC()
	} else {
		input.OccurredAt = input.OccurredAt.UTC()
	}
	if input.Metadata == nil {
		input.Metadata = models.JSONB{}
	}

	event := models.CustomerActivityEvent{
		ID:               uuid.New(),
		OrganizationID:   orgID,
		ContactID:        input.ContactID,
		LeadID:           input.LeadID,
		EventType:        input.EventType,
		Category:         input.Category,
		Title:            strings.TrimSpace(input.Title),
		Summary:          strings.TrimSpace(input.Summary),
		ActorType:        input.ActorType,
		ActorUserID:      input.ActorUserID,
		SourceObjectType: strings.TrimSpace(input.SourceObjectType),
		SourceObjectID:   input.SourceObjectID,
		OccurredAt:       input.OccurredAt,
		Metadata:         input.Metadata,
		IdempotencyKey:   input.IdempotencyKey,
	}
	if event.SourceObjectType == "" {
		event.SourceObjectType = string(input.Category)
	}

	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "idempotency_key"},
		},
		DoNothing: true,
	}).Create(&event)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		var existing models.CustomerActivityEvent
		if err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID,
			input.IdempotencyKey,
		).First(&existing).Error; err != nil {
			return nil, err
		}
		if existing.ContactID != input.ContactID ||
			existing.EventType != input.EventType ||
			existing.SourceObjectType != event.SourceObjectType ||
			!customerWorkspaceUUIDPointersEqual(existing.SourceObjectID, input.SourceObjectID) {
			return nil, errors.New("customer activity idempotency key was reused for a different event")
		}
		return &existing, nil
	}

	aggregateID := event.SourceObjectID
	if aggregateID == nil {
		aggregateID = &event.ID
	}
	outbox := models.OutboxEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		EventType:      string(event.EventType),
		AggregateType:  event.SourceObjectType,
		AggregateID:    aggregateID,
		Payload:        customerActivityOutboxPayload(event, input.WebhookData),
		AvailableAt:    time.Now().UTC(),
		Status:         models.OutboxEventStatusPending,
		MaxAttempts:    10,
		IdempotencyKey: "customer-activity-webhook:" + event.ID.String(),
		Version:        1,
	}
	if err := tx.Create(&outbox).Error; err != nil {
		return nil, err
	}
	return &event, nil
}

func customerActivityOutboxPayload(
	event models.CustomerActivityEvent,
	webhookData models.JSONB,
) models.JSONB {
	payload := models.JSONB{
		"activity_event_id": event.ID.String(),
		"contact_id":        event.ContactID.String(),
		"lead_id":           customerWorkspaceUUIDString(event.LeadID),
		"event_type":        string(event.EventType),
		"category":          string(event.Category),
		"title":             event.Title,
		"summary":           event.Summary,
		"occurred_at":       event.OccurredAt.Format(time.RFC3339Nano),
		"source_type":       event.SourceObjectType,
		"source_id":         customerWorkspaceUUIDString(event.SourceObjectID),
		"actor_type":        string(event.ActorType),
		"actor_user_id":     customerWorkspaceUUIDString(event.ActorUserID),
		"metadata":          event.Metadata,
	}
	for key, value := range webhookData {
		key = strings.TrimSpace(key)
		if key == "" || strings.EqualFold(key, "outbox_event_id") {
			continue
		}
		if _, reserved := payload[strings.ToLower(key)]; reserved {
			continue
		}
		payload[key] = value
	}
	return payload
}

type CustomerWorkspaceCapabilities struct {
	CRM         bool `json:"crm"`
	Tasks       bool `json:"tasks"`
	Bookings    bool `json:"bookings"`
	Packages    bool `json:"packages"`
	Payments    bool `json:"payments"`
	Copilot     bool `json:"copilot"`
	Omnichannel bool `json:"omnichannel"`
	Messages    bool `json:"messages"`
	Merge       bool `json:"merge"`
}

type CustomerWorkspaceAmount struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

type CustomerTimelineActor struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type CustomerTimelineItem struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Category   string                 `json:"category"`
	Title      string                 `json:"title"`
	Summary    string                 `json:"summary,omitempty"`
	OccurredAt time.Time              `json:"occurred_at"`
	SourceType string                 `json:"source_type,omitempty"`
	SourceID   *uuid.UUID             `json:"source_id,omitempty"`
	LeadID     *uuid.UUID             `json:"lead_id,omitempty"`
	Actor      *CustomerTimelineActor `json:"actor,omitempty"`
	Metadata   models.JSONB           `json:"metadata,omitempty"`
}

// CustomerWorkspaceIdentity intentionally omits provider external IDs,
// normalized lookup keys, and metadata. Those fields are operational
// identifiers and can contain provider-specific or personal data that the
// workspace does not need to expose.
type CustomerWorkspaceIdentity struct {
	ID               uuid.UUID      `json:"id"`
	ChannelAccountID uuid.UUID      `json:"channel_account_id"`
	Channel          models.Channel `json:"channel"`
	Address          string         `json:"address,omitempty"`
	DisplayName      string         `json:"display_name,omitempty"`
	IsPrimary        bool           `json:"is_primary"`
	IsVerified       bool           `json:"is_verified"`
	FirstSeenAt      *time.Time     `json:"first_seen_at,omitempty"`
	LastSeenAt       *time.Time     `json:"last_seen_at,omitempty"`
}

type CustomerWorkspaceSummary struct {
	OpenJourneys        int64                     `json:"open_journeys"`
	PipelineValue       []CustomerWorkspaceAmount `json:"pipeline_value"`
	OverdueTasks        int64                     `json:"overdue_tasks"`
	NextBooking         *models.Booking           `json:"next_booking,omitempty"`
	ActivePackages      int64                     `json:"active_packages"`
	AvailableCredits    int64                     `json:"available_credits"`
	PackageExpiringSoon int64                     `json:"package_expiring_soon"`
	Outstanding         []CustomerWorkspaceAmount `json:"outstanding"`
	Collected           []CustomerWorkspaceAmount `json:"collected"`
	LastActivityAt      *time.Time                `json:"last_activity_at,omitempty"`
}

type CustomerWorkspaceResponse struct {
	Contact      ContactResponse               `json:"contact"`
	Capabilities CustomerWorkspaceCapabilities `json:"capabilities"`
	Identities   []CustomerWorkspaceIdentity   `json:"identities"`
	Journeys     []CRMLeadResponse             `json:"journeys"`
	Tasks        []FollowUpTaskResponse        `json:"tasks"`
	Bookings     []models.Booking              `json:"bookings"`
	Packages     []models.ContactPackage       `json:"packages"`
	Invoices     []models.CommerceInvoice      `json:"invoices"`
	Payments     []models.PaymentTransaction   `json:"payments"`
	Summary      CustomerWorkspaceSummary      `json:"summary"`
	Timeline     []CustomerTimelineItem        `json:"timeline"`
}

// GetCustomerWorkspace returns the permission-aware Customer 360 aggregate.
func (a *App) GetCustomerWorkspace(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	contactID, err := parsePathUUID(r, "id", "contact")
	if err != nil {
		return nil
	}
	contact, contactIDs, err := a.loadCanonicalWorkspaceContact(orgID, userID, contactID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}

	response := CustomerWorkspaceResponse{
		Contact:    a.buildContactResponse(contact, orgID),
		Identities: []CustomerWorkspaceIdentity{},
		Journeys:   []CRMLeadResponse{},
		Tasks:      []FollowUpTaskResponse{},
		Bookings:   []models.Booking{},
		Packages:   []models.ContactPackage{},
		Invoices:   []models.CommerceInvoice{},
		Payments:   []models.PaymentTransaction{},
		Timeline:   []CustomerTimelineItem{},
		Summary: CustomerWorkspaceSummary{
			PipelineValue: []CustomerWorkspaceAmount{},
			Outstanding:   []CustomerWorkspaceAmount{},
			Collected:     []CustomerWorkspaceAmount{},
		},
	}
	response.Capabilities, err = a.customerWorkspaceCapabilities(orgID, userID)
	if err != nil {
		a.Log.Error("Failed to evaluate customer workspace capabilities", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load customer workspace", nil, "")
	}
	conversationRestricted := false
	var conversationCutoff *time.Time
	if response.Capabilities.Messages {
		conversationRestricted, conversationCutoff, err = a.customerConversationScope(
			orgID,
			userID,
			contactIDs,
		)
		if err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
	}

	if response.Capabilities.Omnichannel {
		var identities []models.ContactIdentity
		if err := a.DB.Where(
			"organization_id = ? AND contact_id IN ?",
			orgID,
			contactIDs,
		).Order("is_primary DESC, last_seen_at DESC NULLS LAST").
			Find(&identities).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		response.Identities = a.customerWorkspaceIdentities(orgID, identities)
	}

	activityCategories := customerWorkspaceAllowedActivityCategories(response.Capabilities)
	if conversationRestricted && conversationCutoff == nil {
		activityCategories = customerWorkspaceCategoriesWithoutMessages(activityCategories)
	}
	activityQuery := a.DB.Where(
		"organization_id = ? AND contact_id IN ? AND category IN ?",
		orgID,
		contactIDs,
		activityCategories,
	)
	if conversationCutoff != nil {
		activityQuery = activityQuery.Where(
			"(category <> ? OR occurred_at >= ?)",
			models.CustomerActivityCategoryMessage,
			*conversationCutoff,
		)
	}
	var activityEvents []models.CustomerActivityEvent
	if err := activityQuery.Preload("Actor").
		Order("occurred_at DESC, id DESC").
		Limit(250).
		Find(&activityEvents).Error; err != nil {
		return a.customerWorkspaceLoadError(r, err)
	}
	response.Timeline = customerActivityTimeline(activityEvents)

	if response.Capabilities.Messages &&
		(!conversationRestricted || conversationCutoff != nil) {
		var incomingMessages []models.Message
		messageQuery := a.DB.Where(
			"organization_id = ? AND contact_id IN ? AND direction = ?",
			orgID,
			contactIDs,
			models.DirectionIncoming,
		)
		if conversationCutoff != nil {
			messageQuery = messageQuery.Where("created_at >= ?", *conversationCutoff)
		}
		if err := messageQuery.Order("created_at DESC, id DESC").
			Limit(250).
			Find(&incomingMessages).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		response.Timeline = append(response.Timeline, customerMessageTimeline(incomingMessages)...)
	}

	if response.Capabilities.CRM {
		var journeys []models.CRMLead
		pipelineValue := map[string]int64{}
		query := a.DB.Where("crm_leads.organization_id = ? AND crm_leads.contact_id IN ?", orgID, contactIDs)
		if err := productCRMLeadPreloads(query, orgID, true).
			Order("crm_leads.updated_at DESC").
			Find(&journeys).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		for i := range journeys {
			response.Journeys = append(response.Journeys, crmLeadToResponse(&journeys[i]))
			response.Timeline = append(response.Timeline, customerJourneyTimeline(&journeys[i])...)
			if journeys[i].Status == models.CRMLeadStatusOpen {
				response.Summary.OpenJourneys++
				pipelineValue[journeys[i].Currency] += journeys[i].ValueMinor
			}
		}
		response.Summary.PipelineValue = customerWorkspaceAmountsFromMap(pipelineValue)
	}

	if response.Capabilities.Tasks {
		var tasks []models.FollowUpTask
		if err := productCRMTaskPreloads(
			a.DB.Where("follow_up_tasks.organization_id = ? AND follow_up_tasks.contact_id IN ?", orgID, contactIDs),
			orgID,
		).Order("follow_up_tasks.due_at ASC NULLS LAST, follow_up_tasks.created_at DESC").
			Find(&tasks).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		now := time.Now().UTC()
		for i := range tasks {
			response.Tasks = append(response.Tasks, followUpTaskToResponse(&tasks[i]))
			response.Timeline = append(response.Timeline, customerTaskTimeline(&tasks[i])...)
			if tasks[i].DueAt != nil && tasks[i].DueAt.Before(now) &&
				tasks[i].Status != models.FollowUpTaskStatusCompleted &&
				tasks[i].Status != models.FollowUpTaskStatusCancelled {
				response.Summary.OverdueTasks++
			}
		}
	}

	if response.Capabilities.Bookings {
		if err := a.DB.Where("bookings.organization_id = ? AND bookings.contact_id IN ?", orgID, contactIDs).
			Preload("Event", "organization_id = ?", orgID).
			Preload("Event.Service", "organization_id = ?", orgID).
			Preload("Event.Resource", "organization_id = ?", orgID).
			Preload("ContactPackage", "organization_id = ?", orgID).
			Order("bookings.created_at DESC").
			Find(&response.Bookings).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		now := time.Now().UTC()
		for i := range response.Bookings {
			booking := &response.Bookings[i]
			response.Timeline = append(response.Timeline, customerBookingTimeline(booking)...)
			if booking.Event != nil && booking.Event.StartsAt.After(now) &&
				booking.Status != models.BookingStatusCancelled &&
				booking.Status != models.BookingStatusNoShow &&
				booking.Status != models.BookingStatusCompleted &&
				(response.Summary.NextBooking == nil ||
					response.Summary.NextBooking.Event == nil ||
					booking.Event.StartsAt.Before(response.Summary.NextBooking.Event.StartsAt)) {
				copyBooking := *booking
				response.Summary.NextBooking = &copyBooking
			}
		}
	}

	if response.Capabilities.Packages {
		if err := a.DB.Where("contact_packages.organization_id = ? AND contact_packages.contact_id IN ?", orgID, contactIDs).
			Preload("PackageDefinition", "organization_id = ?", orgID).
			Preload("Balances", "organization_id = ?", orgID).
			Preload("Balances.PackageEntitlement", "organization_id = ?", orgID).
			Order("contact_packages.created_at DESC").
			Find(&response.Packages).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		now := time.Now().UTC()
		expiryCutoff := now.Add(30 * 24 * time.Hour)
		for i := range response.Packages {
			pkg := &response.Packages[i]
			response.Timeline = append(response.Timeline, customerPackageTimeline(pkg)...)
			if !customerWorkspacePackageIsCurrent(pkg, now) {
				continue
			}
			response.Summary.ActivePackages++
			if pkg.ExpiresAt != nil && pkg.ExpiresAt.Before(expiryCutoff) {
				response.Summary.PackageExpiringSoon++
			}
			for _, balance := range pkg.Balances {
				if balance.PackageEntitlement != nil && balance.PackageEntitlement.IsUnlimited {
					continue
				}
				response.Summary.AvailableCredits += int64(balance.Available)
			}
		}
	}

	if response.Capabilities.Payments {
		if err := a.DB.Where("commerce_invoices.organization_id = ? AND commerce_invoices.contact_id IN ?", orgID, contactIDs).
			Preload("Lines", "organization_id = ?", orgID).
			Order("commerce_invoices.created_at DESC").
			Find(&response.Invoices).Error; err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
		invoiceIDs := make([]uuid.UUID, 0, len(response.Invoices))
		outstanding := map[string]int64{}
		for i := range response.Invoices {
			invoice := &response.Invoices[i]
			invoiceIDs = append(invoiceIDs, invoice.ID)
			response.Timeline = append(response.Timeline, customerInvoiceTimeline(invoice)...)
			if customerWorkspaceInvoiceIsOutstanding(invoice) {
				outstanding[invoice.Currency] += invoice.DueMinor
			}
		}
		response.Summary.Outstanding = customerWorkspaceAmountsFromMap(outstanding)
		if len(invoiceIDs) > 0 {
			if err := a.DB.Where("payment_transactions.organization_id = ? AND payment_transactions.invoice_id IN ?", orgID, invoiceIDs).
				Preload("Invoice", "organization_id = ?", orgID).
				Order("payment_transactions.occurred_at DESC").
				Find(&response.Payments).Error; err != nil {
				return a.customerWorkspaceLoadError(r, err)
			}
		}
		collected := map[string]int64{}
		for i := range response.Payments {
			payment := &response.Payments[i]
			response.Timeline = append(response.Timeline, customerPaymentTimeline(payment)...)
			if payment.Status != models.PaymentTransactionStatusSucceeded {
				continue
			}
			switch payment.Type {
			case models.PaymentTransactionTypeRefund:
				collected[payment.Currency] -= payment.AmountMinor
			case models.PaymentTransactionTypeCharge:
				collected[payment.Currency] += payment.AmountMinor
			}
		}
		response.Summary.Collected = customerWorkspaceAmountsFromMap(collected)
	}

	response.Timeline = dedupeCustomerTimeline(response.Timeline)
	sort.SliceStable(response.Timeline, func(i, j int) bool {
		if response.Timeline[i].OccurredAt.Equal(response.Timeline[j].OccurredAt) {
			return response.Timeline[i].ID > response.Timeline[j].ID
		}
		return response.Timeline[i].OccurredAt.After(response.Timeline[j].OccurredAt)
	})
	if len(response.Timeline) > 250 {
		response.Timeline = response.Timeline[:250]
	}
	if len(response.Timeline) > 0 {
		last := response.Timeline[0].OccurredAt
		response.Summary.LastActivityAt = &last
	}
	return r.SendEnvelope(response)
}

func (a *App) customerWorkspaceLoadError(r *fastglue.Request, err error) error {
	a.Log.Error("Failed to load customer workspace", "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load customer workspace", nil, "")
}

func (a *App) loadCanonicalWorkspaceContact(
	orgID, userID, requestedID uuid.UUID,
) (*models.Contact, []uuid.UUID, error) {
	canonicalID := requestedID
	seen := map[uuid.UUID]struct{}{}
	for {
		if _, exists := seen[canonicalID]; exists {
			return nil, nil, errors.New("contact merge redirect contains a cycle")
		}
		seen[canonicalID] = struct{}{}
		var candidate models.Contact
		if err := a.DB.Unscoped().
			Where("id = ? AND organization_id = ?", canonicalID, orgID).
			First(&candidate).Error; err != nil {
			return nil, nil, err
		}
		if candidate.MergedIntoID == nil {
			break
		}
		canonicalID = *candidate.MergedIntoID
	}

	query := a.scopeAssignedContact(
		a.DB.Where("id = ? AND organization_id = ?", canonicalID, orgID),
		userID,
		orgID,
	)
	var contact models.Contact
	if err := query.First(&contact).Error; err != nil {
		return nil, nil, err
	}
	contactIDs, err := customerWorkspaceContactFamilyIDs(a.DB, orgID, contact.ID)
	if err != nil {
		return nil, nil, err
	}
	return &contact, contactIDs, nil
}

// customerWorkspaceContactFamilyIDs returns the canonical contact and every
// merged descendant, including legacy multi-hop chains created before merges
// were flattened.
func customerWorkspaceContactFamilyIDs(
	db *gorm.DB,
	orgID, canonicalID uuid.UUID,
) ([]uuid.UUID, error) {
	result := []uuid.UUID{canonicalID}
	seen := map[uuid.UUID]struct{}{canonicalID: {}}
	frontier := []uuid.UUID{canonicalID}
	for len(frontier) > 0 {
		var children []uuid.UUID
		if err := db.Unscoped().Model(&models.Contact{}).
			Where("organization_id = ? AND merged_into_id IN ?", orgID, frontier).
			Pluck("id", &children).Error; err != nil {
			return nil, err
		}
		next := make([]uuid.UUID, 0, len(children))
		for _, childID := range children {
			if _, exists := seen[childID]; exists {
				continue
			}
			seen[childID] = struct{}{}
			result = append(result, childID)
			next = append(next, childID)
			if len(result) > 10000 {
				return nil, errors.New("contact merge family exceeds safety limit")
			}
		}
		frontier = next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result, nil
}

func (a *App) customerWorkspaceCapabilities(
	orgID, userID uuid.UUID,
) (CustomerWorkspaceCapabilities, error) {
	capabilities := CustomerWorkspaceCapabilities{
		Merge:    a.HasPermission(userID, models.ResourceContacts, models.ActionWrite, orgID),
		Messages: a.HasPermission(userID, models.ResourceChat, models.ActionRead, orgID),
	}
	check := func(resource, entitlement string) (bool, error) {
		if !a.HasPermission(userID, resource, models.ActionRead, orgID) {
			return false, nil
		}
		return a.HasProductEntitlement(userID, orgID, entitlement)
	}
	var err error
	if capabilities.CRM, err = check(models.ResourceCRMLeads, "crm.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Tasks, err = check(models.ResourceTasks, "crm.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Bookings, err = check(models.ResourceBookings, "bookings.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Packages, err = check(models.ResourcePackages, "commerce.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Payments, err = check(models.ResourcePayments, "commerce.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Copilot, err = check(models.ResourceCopilot, "copilot.enabled"); err != nil {
		return capabilities, err
	}
	if capabilities.Omnichannel, err = check(models.ResourceConversations, "omnichannel.enabled"); err != nil {
		return capabilities, err
	}
	capabilities.Messages = capabilities.Messages || capabilities.Omnichannel
	return capabilities, nil
}

func (a *App) customerWorkspaceIdentities(
	orgID uuid.UUID,
	identities []models.ContactIdentity,
) []CustomerWorkspaceIdentity {
	result := make([]CustomerWorkspaceIdentity, 0, len(identities))
	mask := a.ShouldMaskPhoneNumbers(orgID)
	for i := range identities {
		identity := &identities[i]
		address := identity.Address
		displayName := identity.DisplayName
		if mask {
			address = utils.MaskIfPhoneNumber(address)
			displayName = utils.MaskIfPhoneNumber(displayName)
		}
		result = append(result, CustomerWorkspaceIdentity{
			ID:               identity.ID,
			ChannelAccountID: identity.ChannelAccountID,
			Channel:          identity.Channel,
			Address:          address,
			DisplayName:      displayName,
			IsPrimary:        identity.IsPrimary,
			IsVerified:       identity.IsVerified,
			FirstSeenAt:      identity.FirstSeenAt,
			LastSeenAt:       identity.LastSeenAt,
		})
	}
	return result
}

func customerWorkspaceAllowedActivityCategories(
	capabilities CustomerWorkspaceCapabilities,
) []models.CustomerActivityCategory {
	categories := []models.CustomerActivityCategory{
		models.CustomerActivityCategoryContact,
		models.CustomerActivityCategoryConsent,
	}
	if capabilities.Messages {
		categories = append(categories, models.CustomerActivityCategoryMessage)
	}
	if capabilities.CRM {
		categories = append(categories, models.CustomerActivityCategoryCRM)
	}
	if capabilities.Tasks {
		categories = append(categories, models.CustomerActivityCategoryTask)
	}
	if capabilities.Bookings {
		categories = append(categories, models.CustomerActivityCategoryBooking)
	}
	if capabilities.Packages {
		categories = append(categories, models.CustomerActivityCategoryPackage)
	}
	if capabilities.Payments {
		categories = append(
			categories,
			models.CustomerActivityCategoryInvoice,
			models.CustomerActivityCategoryPayment,
		)
	}
	return categories
}

func customerWorkspaceCategoriesWithoutMessages(
	categories []models.CustomerActivityCategory,
) []models.CustomerActivityCategory {
	filtered := make([]models.CustomerActivityCategory, 0, len(categories))
	for _, category := range categories {
		if category != models.CustomerActivityCategoryMessage {
			filtered = append(filtered, category)
		}
	}
	return filtered
}

// customerConversationScope returns the earliest message timestamp that a
// restricted assignee may see. Contacts readers are unrestricted. When the
// organization enables CurrentConversationOnly, the newest session or active
// transfer assignment is the boundary; if neither exists, message history is
// denied rather than exposed.
func (a *App) customerConversationScope(
	orgID, userID uuid.UUID,
	contactIDs []uuid.UUID,
) (bool, *time.Time, error) {
	if a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) {
		return false, nil, nil
	}

	settings, err := a.getChatbotSettingsCached(orgID, "")
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil, nil
		}
		a.Log.Warn(
			"Failed to load current-conversation policy; hiding message history",
			"organization_id", orgID,
			"user_id", userID,
			"error", err,
		)
		return true, nil, nil
	}
	if !settings.AgentAssignment.CurrentConversationOnly {
		return false, nil, nil
	}

	var cutoff *time.Time
	var session models.ChatbotSession
	sessionErr := a.DB.Where(
		"organization_id = ? AND contact_id IN ?",
		orgID,
		contactIDs,
	).Order("started_at DESC, id DESC").First(&session).Error
	if sessionErr == nil {
		startedAt := session.StartedAt.UTC()
		cutoff = &startedAt
	} else if !errors.Is(sessionErr, gorm.ErrRecordNotFound) {
		return true, nil, sessionErr
	}

	// Chatbot-disabled and manual transfers may have no session. The transfer
	// assignment time is a safe boundary that exposes new work without leaking
	// the earlier conversation.
	var transfer models.AgentTransfer
	transferErr := a.DB.Where(
		"organization_id = ? AND contact_id IN ? AND agent_id = ? AND status = ?",
		orgID,
		contactIDs,
		userID,
		models.TransferStatusActive,
	).Order("transferred_at DESC, id DESC").First(&transfer).Error
	if transferErr == nil {
		transferredAt := transfer.TransferredAt.UTC()
		if cutoff == nil || transferredAt.After(*cutoff) {
			cutoff = &transferredAt
		}
	} else if !errors.Is(transferErr, gorm.ErrRecordNotFound) {
		return true, nil, transferErr
	}

	return true, cutoff, nil
}

// ListCustomerActivities returns the append-only activity stream only. The
// workspace endpoint additionally synthesizes older domain history.
func (a *App) ListCustomerActivities(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	contactID, err := parsePathUUID(r, "id", "contact")
	if err != nil {
		return nil
	}
	_, contactIDs, err := a.loadCanonicalWorkspaceContact(orgID, userID, contactID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}
	capabilities, err := a.customerWorkspaceCapabilities(orgID, userID)
	if err != nil {
		return a.customerWorkspaceLoadError(r, err)
	}
	conversationRestricted := false
	var conversationCutoff *time.Time
	if capabilities.Messages {
		conversationRestricted, conversationCutoff, err = a.customerConversationScope(
			orgID,
			userID,
			contactIDs,
		)
		if err != nil {
			return a.customerWorkspaceLoadError(r, err)
		}
	}
	activityCategories := customerWorkspaceAllowedActivityCategories(capabilities)
	if conversationRestricted && conversationCutoff == nil {
		activityCategories = customerWorkspaceCategoriesWithoutMessages(activityCategories)
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND contact_id IN ? AND category IN ?",
			orgID,
			contactIDs,
			activityCategories,
		)
	if conversationCutoff != nil {
		query = query.Where(
			"(category <> ? OR occurred_at >= ?)",
			models.CustomerActivityCategoryMessage,
			*conversationCutoff,
		)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.customerWorkspaceLoadError(r, err)
	}
	var events []models.CustomerActivityEvent
	if err := pg.Apply(query.Preload("Actor").Order("occurred_at DESC, id DESC")).
		Find(&events).Error; err != nil {
		return a.customerWorkspaceLoadError(r, err)
	}
	return r.SendEnvelope(listEnvelope("activities", customerActivityTimeline(events), total, pg))
}

type MergeContactRequest struct {
	SourceContactID uuid.UUID `json:"source_contact_id"`
	Confirm         bool      `json:"confirm"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

type ContactMergePreview struct {
	TargetContactID uuid.UUID        `json:"target_contact_id"`
	SourceContactID uuid.UUID        `json:"source_contact_id"`
	ConfirmRequired bool             `json:"confirm_required"`
	Counts          map[string]int64 `json:"counts"`
	Preserved       map[string]int64 `json:"preserved"`
	Collisions      []string         `json:"collisions"`
}

func (a *App) authorizeContactMergePair(
	db *gorm.DB,
	orgID, userID, targetID, sourceID uuid.UUID,
) error {
	var target models.Contact
	targetQuery := db.Where("id = ? AND organization_id = ?", targetID, orgID)
	targetQuery = a.scopeAssignedContact(targetQuery, userID, orgID)
	if err := targetQuery.First(&target).Error; err != nil {
		return err
	}

	var source models.Contact
	if err := db.Unscoped().
		Where("id = ? AND organization_id = ?", sourceID, orgID).
		First(&source).Error; err != nil {
		return err
	}
	if source.MergedIntoID != nil {
		if *source.MergedIntoID == targetID {
			return nil
		}
		return gorm.ErrRecordNotFound
	}
	sourceQuery := db.Where("id = ? AND organization_id = ?", sourceID, orgID)
	sourceQuery = a.scopeAssignedContact(sourceQuery, userID, orgID)
	return sourceQuery.First(&source).Error
}

func (a *App) customerWorkspaceCanAccessContact(
	db *gorm.DB,
	orgID, userID uuid.UUID,
	contact *models.Contact,
) (bool, error) {
	if contact == nil {
		return false, nil
	}
	if a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) {
		return true, nil
	}
	if contact.AssignedUserID != nil && *contact.AssignedUserID == userID {
		return true, nil
	}
	var count int64
	if err := db.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND agent_id = ? AND status = ?",
			orgID,
			contact.ID,
			userID,
			models.TransferStatusActive,
		).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// MergeContact previews by default. A confirmed merge moves only associations
// whose constraints and semantics are unambiguous. Commerce history and linked
// package bookings stay on the soft-deleted alias and remain visible through
// the canonical workspace, preserving immutable financial evidence.
func (a *App) MergeContact(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceContacts, models.ActionWrite)
	if err != nil {
		return nil
	}
	targetID, err := parsePathUUID(r, "id", "contact")
	if err != nil {
		return nil
	}
	var req MergeContactRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.SourceContactID == uuid.Nil || req.SourceContactID == targetID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "source_contact_id must identify a different contact", nil, "")
	}

	if err := a.authorizeContactMergePair(a.DB, orgID, userID, targetID, req.SourceContactID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Source or target contact not found", nil, "")
		}
		return a.customerWorkspaceLoadError(r, err)
	}
	preview, err := buildContactMergePreview(a.DB, orgID, targetID, req.SourceContactID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Source or target contact not found", nil, "")
		}
		return a.customerWorkspaceLoadError(r, err)
	}
	if !req.Confirm {
		return r.SendEnvelope(preview)
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 180 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "idempotency_key is required for a confirmed merge", nil, "")
	}
	if len(preview.Collisions) > 0 {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Merge has unresolved collisions; review the preview before retrying",
			map[string]any{"collisions": preview.Collisions},
			"",
		)
	}

	var target models.Contact
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		targetFamilyIDs, err := customerWorkspaceContactFamilyIDs(tx, orgID, targetID)
		if err != nil {
			return err
		}
		sourceFamilyIDs, err := customerWorkspaceContactFamilyIDs(tx, orgID, req.SourceContactID)
		if err != nil {
			return err
		}
		idSet := map[uuid.UUID]struct{}{}
		for _, id := range append(targetFamilyIDs, sourceFamilyIDs...) {
			idSet[id] = struct{}{}
		}
		idSet[targetID] = struct{}{}
		idSet[req.SourceContactID] = struct{}{}
		ids := make([]uuid.UUID, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		var locked []models.Contact
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND id IN ?", orgID, ids).
			Order("id").
			Find(&locked).Error; err != nil {
			return err
		}
		var source models.Contact
		foundTarget := false
		foundSource := false
		for i := range locked {
			switch locked[i].ID {
			case targetID:
				target = locked[i]
				foundTarget = true
			case req.SourceContactID:
				source = locked[i]
				foundSource = true
			}
		}
		if !foundTarget || !foundSource {
			return gorm.ErrRecordNotFound
		}
		if target.DeletedAt.Valid || target.MergedIntoID != nil {
			return newProductCRMClientError(fasthttp.StatusConflict, "Target contact is not canonical")
		}
		targetVisible, err := a.customerWorkspaceCanAccessContact(tx, orgID, userID, &target)
		if err != nil {
			return err
		}
		if !targetVisible {
			return gorm.ErrRecordNotFound
		}
		if source.MergedIntoID != nil {
			if *source.MergedIntoID == target.ID {
				var replay models.CustomerActivityEvent
				if err := tx.Where(
					`organization_id = ?
					 AND idempotency_key = ?
					 AND contact_id = ?
					 AND event_type = ?
					 AND source_object_type = ?
					 AND source_object_id = ?`,
					orgID,
					"contact-merge:"+req.IdempotencyKey,
					target.ID,
					models.CustomerActivityContactMerged,
					"contact",
					source.ID,
				).First(&replay).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return newProductCRMClientError(
							fasthttp.StatusConflict,
							"Source contact is already merged; the idempotency key does not match the original merge",
						)
					}
					return err
				}
				return nil
			}
			return newProductCRMClientError(fasthttp.StatusConflict, "Source contact is already merged elsewhere")
		}
		if source.DeletedAt.Valid {
			return gorm.ErrRecordNotFound
		}
		sourceVisible, err := a.customerWorkspaceCanAccessContact(tx, orgID, userID, &source)
		if err != nil {
			return err
		}
		if !sourceVisible {
			return gorm.ErrRecordNotFound
		}

		contactFamilyIDs := append(append([]uuid.UUID{}, targetFamilyIDs...), sourceFamilyIDs...)
		var lockedTransfers []models.AgentTransfer
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND contact_id IN ? AND status = ?",
				orgID,
				contactFamilyIDs,
				models.TransferStatusActive,
			).
			Find(&lockedTransfers).Error; err != nil {
			return err
		}
		var lockedSessions []models.ChatbotSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND contact_id IN ? AND status = ?",
				orgID,
				contactFamilyIDs,
				models.SessionStatusActive,
			).
			Find(&lockedSessions).Error; err != nil {
			return err
		}
		var lockedConsentStates []models.ConsentState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND contact_id IN ?", orgID, contactFamilyIDs).
			Find(&lockedConsentStates).Error; err != nil {
			return err
		}

		freshPreview, err := buildContactMergePreview(tx, orgID, target.ID, source.ID)
		if err != nil {
			return err
		}
		if len(freshPreview.Collisions) > 0 {
			return newProductCRMClientError(fasthttp.StatusConflict, "Merge acquired new collisions; preview again")
		}

		for _, table := range []string{
			"messages",
			"conversation_notes",
			"chatbot_sessions",
			"agent_transfers",
			"contact_identities",
			"inbox_conversations",
			"contact_channel_preferences",
			"copilot_runs",
			"consent_states",
			"privacy_requests",
			"call_logs",
			"call_transfers",
			"call_permissions",
		} {
			if err := tx.Table(table).
				Where("organization_id = ? AND contact_id = ?", orgID, source.ID).
				Update("contact_id", target.ID).Error; err != nil {
				return fmt.Errorf("move %s: %w", table, err)
			}
		}
		// Journey-linked tasks remain with their preserved journey alias.
		if err := tx.Model(&models.FollowUpTask{}).
			Where("organization_id = ? AND contact_id = ? AND lead_id IS NULL", orgID, source.ID).
			Update("contact_id", target.ID).Error; err != nil {
			return err
		}
		// A booking tied to a package carries a same-contact composite FK. Keep
		// it with the preserved commerce alias; standalone bookings are safe.
		if err := tx.Model(&models.Booking{}).
			Where("organization_id = ? AND contact_id = ? AND contact_package_id IS NULL", orgID, source.ID).
			Update("contact_id", target.ID).Error; err != nil {
			return err
		}

		oldTarget := target
		target.ProfileName = customerWorkspaceFirstNonEmpty(target.ProfileName, source.ProfileName)
		target.AssignedUserID = customerWorkspaceFirstUUID(target.AssignedUserID, source.AssignedUserID)
		target.MarketingOptOut = target.MarketingOptOut || source.MarketingOptOut
		target.Tags = customerWorkspaceMergeTags(target.Tags, source.Tags)
		target.Metadata = customerWorkspaceMergeMetadata(target.Metadata, source.Metadata)
		target.LastMessageAt = customerWorkspaceLatestTime(target.LastMessageAt, source.LastMessageAt)
		target.LastInboundAt = customerWorkspaceLatestTime(target.LastInboundAt, source.LastInboundAt)
		if source.LastMessageAt != nil &&
			(oldTarget.LastMessageAt == nil || source.LastMessageAt.After(*oldTarget.LastMessageAt)) {
			target.LastMessagePreview = source.LastMessagePreview
		}
		target.IsRead = target.IsRead && source.IsRead
		if err := tx.Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", target.ID, orgID).
			Updates(map[string]any{
				"profile_name":         target.ProfileName,
				"assigned_user_id":     target.AssignedUserID,
				"marketing_opt_out":    target.MarketingOptOut,
				"tags":                 target.Tags,
				"metadata":             target.Metadata,
				"last_message_at":      target.LastMessageAt,
				"last_inbound_at":      target.LastInboundAt,
				"last_message_preview": target.LastMessagePreview,
				"is_read":              target.IsRead,
			}).Error; err != nil {
			return err
		}

		now := time.Now().UTC()
		oldSource := source
		aliasIDs := make([]uuid.UUID, 0, len(sourceFamilyIDs))
		for _, aliasID := range sourceFamilyIDs {
			if aliasID != source.ID && aliasID != target.ID {
				aliasIDs = append(aliasIDs, aliasID)
			}
		}
		if len(aliasIDs) > 0 {
			if err := tx.Unscoped().Model(&models.Contact{}).
				Where("organization_id = ? AND id IN ?", orgID, aliasIDs).
				Update("merged_into_id", target.ID).Error; err != nil {
				return err
			}
		}
		if err := tx.Unscoped().Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", source.ID, orgID).
			Updates(map[string]any{
				"merged_into_id": target.ID,
				"merged_at":      now,
				"merged_by_id":   userID,
				"deleted_at":     now,
			}).Error; err != nil {
			return err
		}
		source.MergedIntoID = &target.ID
		source.MergedAt = &now
		source.MergedByID = &userID
		source.DeletedAt = gorm.DeletedAt{Time: now, Valid: true}

		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        target.ID,
			EventType:        models.CustomerActivityContactMerged,
			Category:         models.CustomerActivityCategoryContact,
			Title:            "Duplicate contact merged",
			Summary:          strings.TrimSpace(req.Reason),
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: "contact",
			SourceObjectID:   &source.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"source_contact_id": source.ID.String(),
				"target_contact_id": target.ID.String(),
				"preserved":         freshPreview.Preserved,
			},
			IdempotencyKey: "contact-merge:" + req.IdempotencyKey,
		}); err != nil {
			return err
		}
		userName := audit.GetUserName(tx, userID)
		if err := audit.LogAudit(
			tx, orgID, userID, userName, "contact", target.ID,
			models.AuditActionUpdated, &oldTarget, &target,
		); err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, userName, "contact", source.ID,
			models.AuditActionDeleted, &oldSource, &source,
		)
	})
	if err != nil {
		var clientErr *productCRMClientError
		if errors.As(err, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Source or target contact not found", nil, "")
		}
		return a.customerWorkspaceLoadError(r, err)
	}
	return r.SendEnvelope(map[string]any{
		"merged":            true,
		"target_contact_id": target.ID,
		"source_contact_id": req.SourceContactID,
		"preserved":         preview.Preserved,
	})
}

func buildContactMergePreview(
	db *gorm.DB,
	orgID, targetID, sourceID uuid.UUID,
) (ContactMergePreview, error) {
	preview := ContactMergePreview{
		TargetContactID: targetID,
		SourceContactID: sourceID,
		ConfirmRequired: true,
		Counts:          map[string]int64{},
		Preserved:       map[string]int64{},
		Collisions:      []string{},
	}
	var contacts []models.Contact
	if err := db.Unscoped().Where("organization_id = ? AND id IN ?", orgID, []uuid.UUID{targetID, sourceID}).
		Find(&contacts).Error; err != nil {
		return preview, err
	}
	if len(contacts) != 2 {
		return preview, gorm.ErrRecordNotFound
	}
	sourceContactIDs, err := customerWorkspaceContactFamilyIDs(db, orgID, sourceID)
	if err != nil {
		return preview, err
	}
	targetContactIDs, err := customerWorkspaceContactFamilyIDs(db, orgID, targetID)
	if err != nil {
		return preview, err
	}
	sourceIDSet := make(map[uuid.UUID]struct{}, len(sourceContactIDs))
	for _, id := range sourceContactIDs {
		sourceIDSet[id] = struct{}{}
	}
	distinctTargetIDs := make([]uuid.UUID, 0, len(targetContactIDs))
	for _, id := range targetContactIDs {
		if _, overlapsSource := sourceIDSet[id]; !overlapsSource {
			distinctTargetIDs = append(distinctTargetIDs, id)
		}
	}
	if len(distinctTargetIDs) == 0 {
		distinctTargetIDs = []uuid.UUID{targetID}
	}
	for _, table := range []string{
		"messages", "conversation_notes", "chatbot_sessions", "agent_transfers",
		"contact_identities", "inbox_conversations", "contact_channel_preferences",
		"crm_leads", "follow_up_tasks", "bookings", "contact_packages",
		"commerce_invoices", "payment_intents", "copilot_runs", "consent_events",
		"consent_states", "privacy_requests", "call_logs", "call_transfers",
		"call_permissions", "customer_activity_events",
	} {
		var count int64
		if err := db.Table(table).
			Where("organization_id = ? AND contact_id IN ?", orgID, sourceContactIDs).
			Count(&count).Error; err != nil {
			return preview, err
		}
		preview.Counts[table] = count
	}
	preview.Preserved["contact_packages"] = preview.Counts["contact_packages"]
	preview.Preserved["commerce_invoices"] = preview.Counts["commerce_invoices"]
	preview.Preserved["payment_intents"] = preview.Counts["payment_intents"]
	preview.Preserved["crm_leads"] = preview.Counts["crm_leads"]
	preview.Preserved["customer_activity_events"] = preview.Counts["customer_activity_events"]
	preview.Preserved["consent_events"] = preview.Counts["consent_events"]
	preview.Preserved["contact_aliases"] = int64(len(sourceContactIDs) - 1)
	var journeyTasks int64
	if err := db.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND contact_id IN ? AND lead_id IS NOT NULL", orgID, sourceContactIDs).
		Count(&journeyTasks).Error; err != nil {
		return preview, err
	}
	preview.Preserved["journey_linked_tasks"] = journeyTasks
	var linkedBookings int64
	if err := db.Model(&models.Booking{}).
		Where("organization_id = ? AND contact_id IN ? AND contact_package_id IS NOT NULL", orgID, sourceContactIDs).
		Count(&linkedBookings).Error; err != nil {
		return preview, err
	}
	preview.Preserved["package_linked_bookings"] = linkedBookings

	var preferenceCollisions int64
	if err := db.Raw(`
		SELECT COUNT(*)
		 FROM contact_channel_preferences source
		 JOIN contact_channel_preferences target
		  ON target.organization_id = source.organization_id
		 AND target.contact_id IN ?
		 AND target.channel_account_id = source.channel_account_id
		 AND target.purpose = source.purpose
		 AND target.deleted_at IS NULL
		WHERE source.organization_id = ?
		  AND source.contact_id IN ?
		  AND source.deleted_at IS NULL
	`, distinctTargetIDs, orgID, sourceContactIDs).Scan(&preferenceCollisions).Error; err != nil {
		return preview, err
	}
	if preferenceCollisions > 0 {
		preview.Collisions = append(preview.Collisions, "contact channel preferences overlap")
	}

	combinedContactIDs := append(append([]uuid.UUID{}, distinctTargetIDs...), sourceContactIDs...)
	var activeTransfers int64
	if err := db.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id IN ? AND status = ?",
			orgID,
			combinedContactIDs,
			models.TransferStatusActive,
		).
		Count(&activeTransfers).Error; err != nil {
		return preview, err
	}
	if activeTransfers > 1 {
		preview.Collisions = append(preview.Collisions, "multiple active agent transfers would overlap")
	}

	var sessionCollisions int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM chatbot_sessions source
		JOIN chatbot_sessions target
		  ON target.organization_id = source.organization_id
		 AND target.contact_id IN ?
		 AND target.whats_app_account = source.whats_app_account
		 AND target.status = ?
		 AND target.deleted_at IS NULL
		 AND target.last_activity_at > NOW() - make_interval(mins => CAST(COALESCE(NULLIF((
			 SELECT settings.session_timeout_mins
			 FROM chatbot_settings settings
			 WHERE settings.organization_id = target.organization_id
			   AND settings.deleted_at IS NULL
			   AND (settings.whats_app_account = target.whats_app_account OR settings.whats_app_account = '')
			 ORDER BY CASE WHEN settings.whats_app_account = target.whats_app_account THEN 0 ELSE 1 END
			 LIMIT 1
		 ), 0), 30) AS integer))
		WHERE source.organization_id = ?
		  AND source.contact_id IN ?
		  AND source.status = ?
		  AND source.deleted_at IS NULL
		  AND source.last_activity_at > NOW() - make_interval(mins => CAST(COALESCE(NULLIF((
			 SELECT settings.session_timeout_mins
			 FROM chatbot_settings settings
			 WHERE settings.organization_id = source.organization_id
			   AND settings.deleted_at IS NULL
			   AND (settings.whats_app_account = source.whats_app_account OR settings.whats_app_account = '')
			 ORDER BY CASE WHEN settings.whats_app_account = source.whats_app_account THEN 0 ELSE 1 END
			 LIMIT 1
		  ), 0), 30) AS integer))
	`, distinctTargetIDs, models.SessionStatusActive, orgID, sourceContactIDs, models.SessionStatusActive).
		Scan(&sessionCollisions).Error; err != nil {
		return preview, err
	}
	if sessionCollisions > 0 {
		preview.Collisions = append(preview.Collisions, "active chatbot sessions overlap")
	}

	var consentCollisions int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM consent_states source
		JOIN consent_states target
		  ON target.organization_id = source.organization_id
		 AND target.contact_id IN ?
		 AND target.subject_type = source.subject_type
		 AND target.purpose = source.purpose
		 AND target.channel = source.channel
		 AND target.deleted_at IS NULL
		WHERE source.organization_id = ?
		  AND source.contact_id IN ?
		  AND source.deleted_at IS NULL
	`, distinctTargetIDs, orgID, sourceContactIDs).Scan(&consentCollisions).Error; err != nil {
		return preview, err
	}
	if consentCollisions > 0 {
		preview.Collisions = append(preview.Collisions, "consent states overlap")
	}
	return preview, nil
}

func customerActivityTimeline(events []models.CustomerActivityEvent) []CustomerTimelineItem {
	items := make([]CustomerTimelineItem, 0, len(events))
	for i := range events {
		event := &events[i]
		var actor *CustomerTimelineActor
		if event.Actor != nil {
			actor = &CustomerTimelineActor{ID: event.Actor.ID, Name: event.Actor.FullName}
		}
		items = append(items, CustomerTimelineItem{
			ID:         "activity:" + event.ID.String(),
			Type:       string(event.EventType),
			Category:   string(event.Category),
			Title:      event.Title,
			Summary:    event.Summary,
			OccurredAt: event.OccurredAt,
			SourceType: event.SourceObjectType,
			SourceID:   event.SourceObjectID,
			LeadID:     event.LeadID,
			Actor:      actor,
			Metadata:   event.Metadata,
		})
	}
	return items
}

func customerMessageTimeline(messages []models.Message) []CustomerTimelineItem {
	items := make([]CustomerTimelineItem, 0, len(messages))
	for i := range messages {
		message := &messages[i]
		summary := strings.TrimSpace(message.Content)
		if summary == "" {
			summary = "[" + string(message.MessageType) + "]"
		}
		runes := []rune(summary)
		if len(runes) > 240 {
			summary = string(runes[:240]) + "…"
		}
		items = append(items, CustomerTimelineItem{
			ID:         "history:message:" + message.ID.String(),
			Type:       string(models.CustomerActivityMessageIncoming),
			Category:   string(models.CustomerActivityCategoryMessage),
			Title:      "Incoming message",
			Summary:    summary,
			OccurredAt: message.CreatedAt,
			SourceType: "message",
			SourceID:   &message.ID,
			Metadata: models.JSONB{
				"message_type": string(message.MessageType),
				"status":       string(message.Status),
			},
		})
	}
	return items
}

func customerJourneyTimeline(lead *models.CRMLead) []CustomerTimelineItem {
	items := []CustomerTimelineItem{{
		ID:         "history:crm-lead:" + lead.ID.String(),
		Type:       string(models.CustomerActivityCRMLeadCreated),
		Category:   string(models.CustomerActivityCategoryCRM),
		Title:      "Journey created",
		Summary:    lead.Title,
		OccurredAt: lead.CreatedAt,
		SourceType: "crm_lead",
		SourceID:   &lead.ID,
		LeadID:     &lead.ID,
	}}
	for i := range lead.History {
		history := &lead.History[i]
		title := "Journey stage changed"
		if history.FromStageID == nil {
			title = "Journey entered pipeline"
		}
		items = append(items, CustomerTimelineItem{
			ID:         "history:crm-stage:" + history.ID.String(),
			Type:       string(models.CustomerActivityCRMStageMoved),
			Category:   string(models.CustomerActivityCategoryCRM),
			Title:      title,
			Summary:    history.Reason,
			OccurredAt: history.ChangedAt,
			SourceType: "crm_lead",
			SourceID:   &lead.ID,
			LeadID:     &lead.ID,
			Metadata: models.JSONB{
				"from_stage_id": customerWorkspaceUUIDString(history.FromStageID),
				"to_stage_id":   history.ToStageID.String(),
			},
		})
	}
	return items
}

func customerTaskTimeline(task *models.FollowUpTask) []CustomerTimelineItem {
	items := []CustomerTimelineItem{{
		ID:         "history:task:" + task.ID.String(),
		Type:       string(models.CustomerActivityTaskCreated),
		Category:   string(models.CustomerActivityCategoryTask),
		Title:      "Follow-up task created",
		Summary:    task.Title,
		OccurredAt: task.CreatedAt,
		SourceType: "follow_up_task",
		SourceID:   &task.ID,
		LeadID:     task.LeadID,
	}}
	if task.CompletedAt != nil {
		items = append(items, CustomerTimelineItem{
			ID:         "history:task-completed:" + task.ID.String(),
			Type:       string(models.CustomerActivityTaskCompleted),
			Category:   string(models.CustomerActivityCategoryTask),
			Title:      "Follow-up task completed",
			Summary:    task.Title,
			OccurredAt: *task.CompletedAt,
			SourceType: "follow_up_task",
			SourceID:   &task.ID,
			LeadID:     task.LeadID,
		})
	}
	return items
}

func customerBookingTimeline(booking *models.Booking) []CustomerTimelineItem {
	items := []CustomerTimelineItem{{
		ID:         "history:booking:" + booking.ID.String(),
		Type:       string(models.CustomerActivityBookingCreated),
		Category:   string(models.CustomerActivityCategoryBooking),
		Title:      "Booking created",
		OccurredAt: booking.CreatedAt,
		SourceType: "booking",
		SourceID:   &booking.ID,
		Metadata:   models.JSONB{"status": string(booking.Status)},
	}}
	var at *time.Time
	switch booking.Status {
	case models.BookingStatusConfirmed:
		at = booking.ConfirmedAt
	case models.BookingStatusCheckedIn:
		at = booking.CheckedInAt
	case models.BookingStatusCompleted:
		at = booking.CompletedAt
	case models.BookingStatusNoShow:
		at = booking.NoShowAt
	case models.BookingStatusCancelled:
		at = booking.CancelledAt
	}
	if at != nil {
		items = append(items, CustomerTimelineItem{
			ID:         "history:booking-status:" + booking.ID.String(),
			Type:       string(models.CustomerActivityBookingStatus),
			Category:   string(models.CustomerActivityCategoryBooking),
			Title:      "Booking " + strings.ReplaceAll(string(booking.Status), "_", " "),
			OccurredAt: *at,
			SourceType: "booking",
			SourceID:   &booking.ID,
			Metadata:   models.JSONB{"status": string(booking.Status)},
		})
	}
	return items
}

func customerPackageTimeline(pkg *models.ContactPackage) []CustomerTimelineItem {
	title := "Package assigned"
	if pkg.Source == "sale" {
		title = "Package sold"
	}
	return []CustomerTimelineItem{{
		ID:         "history:package:" + pkg.ID.String(),
		Type:       string(models.CustomerActivityPackageSold),
		Category:   string(models.CustomerActivityCategoryPackage),
		Title:      title,
		OccurredAt: pkg.CreatedAt,
		SourceType: "contact_package",
		SourceID:   &pkg.ID,
		Metadata:   models.JSONB{"status": string(pkg.Status)},
	}}
}

func customerInvoiceTimeline(invoice *models.CommerceInvoice) []CustomerTimelineItem {
	at := invoice.CreatedAt
	if invoice.IssuedAt != nil {
		at = *invoice.IssuedAt
	}
	items := []CustomerTimelineItem{{
		ID:         "history:invoice:" + invoice.ID.String(),
		Type:       string(models.CustomerActivityInvoiceCreated),
		Category:   string(models.CustomerActivityCategoryInvoice),
		Title:      "Invoice created",
		Summary:    invoice.InvoiceNumber,
		OccurredAt: at,
		SourceType: "commerce_invoice",
		SourceID:   &invoice.ID,
		Metadata: models.JSONB{
			"currency":    invoice.Currency,
			"total_minor": invoice.TotalMinor,
			"due_minor":   invoice.DueMinor,
		},
	}}
	if invoice.PaidAt != nil {
		items = append(items, CustomerTimelineItem{
			ID:         "history:invoice-paid:" + invoice.ID.String(),
			Type:       string(models.CustomerActivityInvoicePaid),
			Category:   string(models.CustomerActivityCategoryInvoice),
			Title:      "Invoice paid",
			Summary:    invoice.InvoiceNumber,
			OccurredAt: *invoice.PaidAt,
			SourceType: "commerce_invoice",
			SourceID:   &invoice.ID,
			Metadata:   models.JSONB{"currency": invoice.Currency, "paid_minor": invoice.PaidMinor},
		})
	}
	return items
}

func customerPaymentTimeline(payment *models.PaymentTransaction) []CustomerTimelineItem {
	return []CustomerTimelineItem{{
		ID:         "history:payment:" + payment.ID.String(),
		Type:       string(models.CustomerActivityPaymentRecorded),
		Category:   string(models.CustomerActivityCategoryPayment),
		Title:      "Payment recorded",
		OccurredAt: payment.OccurredAt,
		SourceType: "payment_transaction",
		SourceID:   &payment.ID,
		Metadata: models.JSONB{
			"currency":     payment.Currency,
			"amount_minor": payment.AmountMinor,
			"status":       string(payment.Status),
			"type":         string(payment.Type),
		},
	}}
}

// dedupeCustomerTimeline keeps every durable activity event, even when the
// same transition happened repeatedly, while suppressing a matching
// synthesized legacy row. Durable event IDs are append-only facts and must
// never be collapsed by a semantic key.
func dedupeCustomerTimeline(items []CustomerTimelineItem) []CustomerTimelineItem {
	result := make([]CustomerTimelineItem, 0, len(items))
	durableKeys := map[string]struct{}{}
	seenIDs := map[string]struct{}{}
	for _, item := range items {
		if strings.HasPrefix(item.ID, "activity:") {
			if _, exists := seenIDs[item.ID]; exists {
				continue
			}
			seenIDs[item.ID] = struct{}{}
			durableKeys[customerTimelineDedupeKey(item)] = struct{}{}
			result = append(result, item)
		}
	}
	for _, item := range items {
		if strings.HasPrefix(item.ID, "activity:") {
			continue
		}
		if _, exists := durableKeys[customerTimelineDedupeKey(item)]; exists {
			continue
		}
		if _, exists := seenIDs[item.ID]; exists {
			continue
		}
		seenIDs[item.ID] = struct{}{}
		result = append(result, item)
	}
	return result
}

func customerTimelineDedupeKey(item CustomerTimelineItem) string {
	sourceID := ""
	if item.SourceID != nil {
		sourceID = item.SourceID.String()
	}
	qualifiers := []string{}
	switch models.CustomerActivityEventType(item.Type) {
	case models.CustomerActivityCRMStageMoved:
		for _, key := range []string{"from_stage_id", "to_stage_id"} {
			if value, exists := item.Metadata[key]; exists {
				qualifiers = append(qualifiers, fmt.Sprint(value))
			}
		}
	case models.CustomerActivityBookingStatus:
		if value, exists := item.Metadata["to_status"]; exists {
			qualifiers = append(qualifiers, fmt.Sprint(value))
		} else if value, exists := item.Metadata["status"]; exists {
			qualifiers = append(qualifiers, fmt.Sprint(value))
		}
	}
	return strings.Join(
		[]string{item.Type, item.SourceType, sourceID, strings.Join(qualifiers, ":")},
		"|",
	)
}

func customerWorkspacePackageIsCurrent(pkg *models.ContactPackage, now time.Time) bool {
	if pkg == nil || pkg.Status != models.ContactPackageStatusActive {
		return false
	}
	if pkg.StartsAt != nil && pkg.StartsAt.After(now) {
		return false
	}
	return pkg.ExpiresAt == nil || pkg.ExpiresAt.After(now)
}

func customerWorkspaceInvoiceIsOutstanding(invoice *models.CommerceInvoice) bool {
	return invoice != nil &&
		invoice.Status == models.CommerceInvoiceStatusOpen &&
		invoice.DueMinor > 0
}

func customerWorkspaceAmountsFromMap(values map[string]int64) []CustomerWorkspaceAmount {
	currencies := make([]string, 0, len(values))
	for currency := range values {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	result := make([]CustomerWorkspaceAmount, 0, len(currencies))
	for _, currency := range currencies {
		result = append(result, CustomerWorkspaceAmount{
			Currency:    currency,
			AmountMinor: values[currency],
		})
	}
	return result
}

func customerWorkspaceUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func customerWorkspaceUUIDPointersEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func customerWorkspaceFirstNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

func customerWorkspaceFirstUUID(primary, fallback *uuid.UUID) *uuid.UUID {
	if primary != nil {
		return primary
	}
	return fallback
}

func customerWorkspaceLatestTime(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right == nil || left.After(*right) {
		return left
	}
	return right
}

func customerWorkspaceMergeTags(left, right models.JSONBArray) models.JSONBArray {
	seen := map[string]struct{}{}
	result := models.JSONBArray{}
	for _, values := range []models.JSONBArray{left, right} {
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				continue
			}
			if _, exists := seen[text]; exists {
				continue
			}
			seen[text] = struct{}{}
			result = append(result, text)
		}
	}
	return result
}

func customerWorkspaceMergeMetadata(primary, fallback models.JSONB) models.JSONB {
	result := models.JSONB{}
	for key, value := range fallback {
		result[key] = value
	}
	for key, value := range primary {
		result[key] = value
	}
	return result
}
