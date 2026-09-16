// Package customeractivity writes lifecycle facts and webhook intents on a caller-owned transaction.
package customeractivity

import (
	"errors"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type Input struct {
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

// RecordTx writes the customer timeline event and webhook outbox
// row on the caller's transaction. Replays return the original event; an
// idempotency key reused for a different fact fails closed.
func RecordTx(
	tx *gorm.DB,
	orgID uuid.UUID,
	input Input,
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
			!uuidPointersEqual(existing.SourceObjectID, input.SourceObjectID) {
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
		Payload:        OutboxPayload(event, input.WebhookData),
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

func OutboxPayload(
	event models.CustomerActivityEvent,
	webhookData models.JSONB,
) models.JSONB {
	payload := models.JSONB{
		"activity_event_id": event.ID.String(),
		"contact_id":        event.ContactID.String(),
		"lead_id":           uuidString(event.LeadID),
		"event_type":        string(event.EventType),
		"category":          string(event.Category),
		"title":             event.Title,
		"summary":           event.Summary,
		"occurred_at":       event.OccurredAt.Format(time.RFC3339Nano),
		"source_type":       event.SourceObjectType,
		"source_id":         uuidString(event.SourceObjectID),
		"actor_type":        string(event.ActorType),
		"actor_user_id":     uuidString(event.ActorUserID),
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

func uuidString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}
func uuidPointersEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
