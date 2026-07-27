package channel

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// LegacyMetaProvider identifies the read-only ChannelAccount shadow for an
	// account that is still delivered by the established Meta WhatsApp path.
	// It deliberately has no provider adapter or ChannelCredential.
	LegacyMetaProvider = "meta_legacy"

	legacyMetaIDPrefix = "legacy-"
)

var ErrLegacyMetaBridgeConflict = errors.New("legacy Meta WhatsApp bridge conflict")

// LegacyMetaAccountRef is the complete account input accepted by the bridge.
// Keeping this separate from models.WhatsAppAccount makes it impossible for
// callers to hand credential fields to mirroring or backfill code.
type LegacyMetaAccountRef struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	Status         string
}

type LegacyMetaMirrorResult struct {
	ChannelAccountID uuid.UUID
	ConversationID   uuid.UUID
	Linked           bool
}

type LegacyMetaBackfillStats struct {
	Accounts int
	Messages int
	Linked   int
}

// MirrorLegacyWhatsAppMessage links an existing legacy Message envelope into
// the provider-neutral inbox. It never creates an outbound job, sends to Meta,
// copies credentials, or duplicates message content.
func MirrorLegacyWhatsAppMessage(
	db *gorm.DB,
	accountRef LegacyMetaAccountRef,
	messageID uuid.UUID,
) (LegacyMetaMirrorResult, error) {
	var result LegacyMetaMirrorResult
	if db == nil {
		return result, errors.New("legacy Meta bridge database is required")
	}
	if accountRef.ID == uuid.Nil || accountRef.OrganizationID == uuid.Nil ||
		strings.TrimSpace(accountRef.Name) == "" || messageID == uuid.Nil {
		return result, errors.New("legacy Meta bridge account and message identifiers are required")
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		verifiedRef, err := verifiedLegacyMetaAccountRef(tx, accountRef)
		if err != nil {
			return err
		}
		accountRef = verifiedRef

		var message models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", messageID, accountRef.OrganizationID).
			First(&message).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp message: %w", err)
		}
		if message.WhatsAppAccount != accountRef.Name {
			return fmt.Errorf(
				"%w: message account does not match the selected legacy account",
				ErrLegacyMetaBridgeConflict,
			)
		}

		var contact models.Contact
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ? AND organization_id = ?", message.ContactID, accountRef.OrganizationID).
			First(&contact).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp contact: %w", err)
		}

		account, err := ensureLegacyMetaAccount(tx, accountRef)
		if err != nil {
			return err
		}
		result.ChannelAccountID = account.ID

		identity, err := ensureLegacyMetaIdentity(tx, account, &contact, message.CreatedAt)
		if err != nil {
			return err
		}
		conversation, err := ensureLegacyMetaConversation(
			tx,
			account,
			identity,
			&contact,
			message.CreatedAt,
		)
		if err != nil {
			return err
		}
		result.ConversationID = conversation.ID

		if err := ensureLegacyMetaParticipant(tx, conversation, identity, &contact); err != nil {
			return err
		}

		if message.InboxConversationID != nil {
			if *message.InboxConversationID != conversation.ID {
				return fmt.Errorf(
					"%w: message is already linked to another inbox conversation",
					ErrLegacyMetaBridgeConflict,
				)
			}
		} else {
			update := tx.Model(&models.Message{}).
				Where(
					"id = ? AND organization_id = ? AND inbox_conversation_id IS NULL",
					message.ID,
					accountRef.OrganizationID,
				).
				Update("inbox_conversation_id", conversation.ID)
			if update.Error != nil {
				return fmt.Errorf("link legacy WhatsApp message: %w", update.Error)
			}
			result.Linked = update.RowsAffected == 1
			if !result.Linked {
				var current models.Message
				if err := tx.Select("inbox_conversation_id").
					Where("id = ? AND organization_id = ?", message.ID, accountRef.OrganizationID).
					First(&current).Error; err != nil {
					return fmt.Errorf("verify legacy WhatsApp message link: %w", err)
				}
				if current.InboxConversationID == nil || *current.InboxConversationID != conversation.ID {
					return fmt.Errorf(
						"%w: concurrent message link targeted another conversation",
						ErrLegacyMetaBridgeConflict,
					)
				}
			}
		}

		if err := refreshLegacyMetaConversation(tx, conversation, account, &contact, &message); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

// MarkLegacyWhatsAppConversationRead mirrors the established contact-level read
// state without touching conversations belonging to another tenant or provider.
func MarkLegacyWhatsAppConversationRead(
	db *gorm.DB,
	organizationID uuid.UUID,
	contactID uuid.UUID,
) error {
	if db == nil || organizationID == uuid.Nil || contactID == uuid.Nil {
		return errors.New("legacy Meta read mirror identifiers are required")
	}
	accountIDs := db.Model(&models.ChannelAccount{}).
		Select("id").
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			organizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
		)
	return db.Model(&models.InboxConversation{}).
		Where(
			"organization_id = ? AND contact_id = ? AND channel_account_id IN (?)",
			organizationID,
			contactID,
			accountIDs,
		).
		Update("unread_count", 0).Error
}

// BackfillLegacyWhatsAppInbox safely links existing WhatsApp messages in
// bounded batches. The account query explicitly selects non-secret columns and
// rerunning the function is idempotent.
func BackfillLegacyWhatsAppInbox(
	db *gorm.DB,
	batchSize int,
) (LegacyMetaBackfillStats, error) {
	var stats LegacyMetaBackfillStats
	if db == nil {
		return stats, errors.New("legacy Meta backfill database is required")
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	if batchSize > 1000 {
		batchSize = 1000
	}

	var accounts []LegacyMetaAccountRef
	if err := db.Table("whatsapp_accounts").
		Select("id, organization_id, name, status").
		Where("deleted_at IS NULL").
		Order("organization_id, id").
		Scan(&accounts).Error; err != nil {
		return stats, fmt.Errorf("list legacy WhatsApp accounts: %w", err)
	}

	for _, accountRef := range accounts {
		if _, err := ensureLegacyMetaAccount(db, accountRef); err != nil {
			return stats, err
		}
		stats.Accounts++

		for {
			var messageIDs []uuid.UUID
			if err := db.Model(&models.Message{}).
				Select("id").
				Where(
					"organization_id = ? AND whats_app_account = ? AND inbox_conversation_id IS NULL",
					accountRef.OrganizationID,
					accountRef.Name,
				).
				Order("created_at, id").
				Limit(batchSize).
				Find(&messageIDs).Error; err != nil {
				return stats, fmt.Errorf("list legacy WhatsApp messages: %w", err)
			}
			if len(messageIDs) == 0 {
				break
			}
			for _, messageID := range messageIDs {
				result, err := MirrorLegacyWhatsAppMessage(db, accountRef, messageID)
				if err != nil {
					return stats, err
				}
				stats.Messages++
				if result.Linked {
					stats.Linked++
				}
			}
			if len(messageIDs) < batchSize {
				break
			}
		}
	}
	return stats, nil
}

func verifiedLegacyMetaAccountRef(
	db *gorm.DB,
	supplied LegacyMetaAccountRef,
) (LegacyMetaAccountRef, error) {
	var persisted LegacyMetaAccountRef
	result := db.Table("whatsapp_accounts").
		Select("id, organization_id, name, status").
		Where(
			"id = ? AND organization_id = ? AND deleted_at IS NULL",
			supplied.ID,
			supplied.OrganizationID,
		).
		Limit(1).
		Scan(&persisted)
	if result.Error != nil {
		return persisted, fmt.Errorf("verify legacy Meta account: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return persisted, fmt.Errorf("verify legacy Meta account: %w", gorm.ErrRecordNotFound)
	}
	if persisted.Name != supplied.Name {
		return persisted, fmt.Errorf(
			"%w: supplied legacy account name does not match its database record",
			ErrLegacyMetaBridgeConflict,
		)
	}
	return persisted, nil
}

func ensureLegacyMetaAccount(
	db *gorm.DB,
	ref LegacyMetaAccountRef,
) (*models.ChannelAccount, error) {
	externalID := legacyMetaIDPrefix + "account:" + ref.ID.String()
	now := time.Now().UTC()
	status := models.ChannelAccountStatusSuspended
	if strings.EqualFold(strings.TrimSpace(ref.Status), "active") {
		status = models.ChannelAccountStatusActive
	}
	account := models.ChannelAccount{
		OrganizationID:    ref.OrganizationID,
		Channel:           models.ChannelWhatsApp,
		Provider:          LegacyMetaProvider,
		Name:              legacyMetaAccountName(ref.Name, ref.ID),
		ExternalAccountID: externalID,
		Status:            status,
		Capabilities: models.JSONB{
			"text":        true,
			"media":       true,
			"template":    true,
			"mark_read":   true,
			"attachments": true,
		},
		Config: models.JSONB{
			"legacy_read_only": true,
			"outbound_enabled": false,
			"reply_route":      "chat",
		},
		Metadata: models.JSONB{
			"legacy_account_id":   ref.ID.String(),
			"legacy_account_name": ref.Name,
		},
		ConnectedAt: &now,
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&account).Error; err != nil {
		return nil, fmt.Errorf("create legacy Meta channel account: %w", err)
	}

	var persisted models.ChannelAccount
	if err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			ref.OrganizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
			externalID,
		).
		First(&persisted).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf(
				"%w: shadow account name is already in use",
				ErrLegacyMetaBridgeConflict,
			)
		}
		return nil, fmt.Errorf("load legacy Meta channel account: %w", err)
	}
	if err := db.Unscoped().Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", persisted.ID, ref.OrganizationID).
		Updates(map[string]any{
			"deleted_at":   nil,
			"status":       status,
			"capabilities": account.Capabilities,
			"config":       account.Config,
			"metadata":     account.Metadata,
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta channel account: %w", err)
	}
	persisted.Status = status
	persisted.Capabilities = account.Capabilities
	persisted.Config = account.Config
	persisted.Metadata = account.Metadata
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaIdentity(
	db *gorm.DB,
	account *models.ChannelAccount,
	contact *models.Contact,
	seenAt time.Time,
) (*models.ContactIdentity, error) {
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	externalID := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	identity := models.ContactIdentity{
		OrganizationID:    account.OrganizationID,
		ContactID:         contact.ID,
		ChannelAccountID:  account.ID,
		Channel:           models.ChannelWhatsApp,
		ExternalID:        externalID,
		Address:           contact.PhoneNumber,
		NormalizedAddress: normalizeLegacyPhone(contact.PhoneNumber),
		DisplayName:       contact.ProfileName,
		IsPrimary:         true,
		IsVerified:        true,
		FirstSeenAt:       &seenAt,
		LastSeenAt:        &seenAt,
		Metadata:          models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&identity).Error; err != nil {
		return nil, fmt.Errorf("create legacy Meta contact identity: %w", err)
	}

	var persisted models.ContactIdentity
	if err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_id = ?",
			account.OrganizationID,
			account.ID,
			externalID,
		).
		First(&persisted).Error; err != nil {
		return nil, fmt.Errorf("load legacy Meta contact identity: %w", err)
	}
	if persisted.ContactID != contact.ID {
		return nil, fmt.Errorf(
			"%w: shadow identity belongs to another contact",
			ErrLegacyMetaBridgeConflict,
		)
	}
	lastSeen := seenAt
	if persisted.LastSeenAt != nil && persisted.LastSeenAt.After(lastSeen) {
		lastSeen = *persisted.LastSeenAt
	}
	if err := db.Unscoped().Model(&models.ContactIdentity{}).
		Where("id = ? AND organization_id = ?", persisted.ID, account.OrganizationID).
		Updates(map[string]any{
			"deleted_at":         nil,
			"address":            contact.PhoneNumber,
			"normalized_address": normalizeLegacyPhone(contact.PhoneNumber),
			"display_name":       contact.ProfileName,
			"is_primary":         true,
			"is_verified":        true,
			"last_seen_at":       lastSeen,
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta contact identity: %w", err)
	}
	persisted.Address = contact.PhoneNumber
	persisted.NormalizedAddress = normalizeLegacyPhone(contact.PhoneNumber)
	persisted.DisplayName = contact.ProfileName
	persisted.LastSeenAt = &lastSeen
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaConversation(
	db *gorm.DB,
	account *models.ChannelAccount,
	identity *models.ContactIdentity,
	contact *models.Contact,
	openedAt time.Time,
) (*models.InboxConversation, error) {
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	externalID := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	conversation := models.InboxConversation{
		OrganizationID:         account.OrganizationID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		ContactIdentityID:      &identity.ID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: externalID,
		Status:                 models.InboxConversationStatusOpen,
		AssignedUserID:         contact.AssignedUserID,
		OpenedAt:               openedAt,
		Config: models.JSONB{
			"legacy_read_only": true,
			"reply_route":      "chat",
		},
		Metadata: models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&conversation).Error; err != nil {
		return nil, fmt.Errorf("create legacy Meta inbox conversation: %w", err)
	}

	var persisted models.InboxConversation
	if err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			account.OrganizationID,
			account.ID,
			externalID,
		).
		First(&persisted).Error; err != nil {
		return nil, fmt.Errorf("load legacy Meta inbox conversation: %w", err)
	}
	if persisted.ContactID != contact.ID {
		return nil, fmt.Errorf(
			"%w: shadow conversation belongs to another contact",
			ErrLegacyMetaBridgeConflict,
		)
	}
	if err := db.Unscoped().Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", persisted.ID, account.OrganizationID).
		Updates(map[string]any{
			"deleted_at":          nil,
			"contact_identity_id": identity.ID,
			"assigned_user_id":    contact.AssignedUserID,
			"config": models.JSONB{
				"legacy_read_only": true,
				"reply_route":      "chat",
			},
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta inbox conversation: %w", err)
	}
	persisted.ContactIdentityID = &identity.ID
	persisted.AssignedUserID = contact.AssignedUserID
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaParticipant(
	db *gorm.DB,
	conversation *models.InboxConversation,
	identity *models.ContactIdentity,
	contact *models.Contact,
) error {
	participantKey := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	participant := models.ConversationParticipant{
		OrganizationID:    conversation.OrganizationID,
		ConversationID:    conversation.ID,
		ParticipantKey:    participantKey,
		Role:              models.ConversationParticipantRoleCustomer,
		ContactIdentityID: &identity.ID,
		ExternalID:        identity.ExternalID,
		DisplayName:       contact.ProfileName,
		Address:           contact.PhoneNumber,
		JoinedAt:          conversation.OpenedAt,
		Metadata:          models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&participant).Error; err != nil {
		return fmt.Errorf("create legacy Meta conversation participant: %w", err)
	}
	return db.Unscoped().Model(&models.ConversationParticipant{}).
		Where(
			"organization_id = ? AND conversation_id = ? AND participant_key = ?",
			conversation.OrganizationID,
			conversation.ID,
			participantKey,
		).
		Updates(map[string]any{
			"deleted_at":          nil,
			"contact_identity_id": identity.ID,
			"external_id":         identity.ExternalID,
			"display_name":        contact.ProfileName,
			"address":             contact.PhoneNumber,
			"left_at":             nil,
		}).Error
}

func refreshLegacyMetaConversation(
	db *gorm.DB,
	conversation *models.InboxConversation,
	account *models.ChannelAccount,
	contact *models.Contact,
	message *models.Message,
) error {
	messageAt := message.CreatedAt
	if messageAt.IsZero() {
		messageAt = time.Now().UTC()
	}
	updates := map[string]any{
		"assigned_user_id": contact.AssignedUserID,
		"unread_count":     legacyUnreadCount(contact),
	}
	if conversation.LastMessageAt == nil || !conversation.LastMessageAt.After(messageAt) {
		updates["last_message_at"] = messageAt
		updates["last_message_preview"] = legacyMessagePreview(message)
	}
	if message.Direction == models.DirectionIncoming &&
		(conversation.LastInboundAt == nil || !conversation.LastInboundAt.After(messageAt)) {
		updates["last_inbound_at"] = messageAt
		serviceWindowEnd := messageAt.Add(24 * time.Hour)
		updates["service_window_ends_at"] = serviceWindowEnd
	}
	if message.Direction == models.DirectionOutgoing &&
		(conversation.LastOutboundAt == nil || !conversation.LastOutboundAt.After(messageAt)) {
		updates["last_outbound_at"] = messageAt
	}
	if err := db.Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", conversation.ID, conversation.OrganizationID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("refresh legacy Meta inbox conversation activity: %w", err)
	}

	accountUpdates := map[string]any{}
	if message.Direction == models.DirectionIncoming &&
		(account.LastInboundAt == nil || !account.LastInboundAt.After(messageAt)) {
		accountUpdates["last_inbound_at"] = messageAt
	}
	if message.Direction == models.DirectionOutgoing &&
		(account.LastOutboundAt == nil || !account.LastOutboundAt.After(messageAt)) {
		accountUpdates["last_outbound_at"] = messageAt
	}
	if len(accountUpdates) == 0 {
		return nil
	}
	if err := db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Updates(accountUpdates).Error; err != nil {
		return fmt.Errorf("refresh legacy Meta channel account activity: %w", err)
	}
	return nil
}

func legacyMetaAccountName(name string, accountID uuid.UUID) string {
	const maxRunes = 100
	prefix := "WhatsApp "
	suffix := " [" + accountID.String() + "]"
	name = strings.TrimSpace(name)
	available := maxRunes - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	if available < 0 {
		available = 0
	}
	runes := []rune(name)
	if len(runes) > available {
		runes = runes[:available]
	}
	return prefix + string(runes) + suffix
}

func normalizeLegacyPhone(value string) string {
	value = strings.TrimSpace(value)
	var normalized strings.Builder
	normalized.Grow(len(value))
	for _, char := range value {
		if char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func legacyUnreadCount(contact *models.Contact) int {
	if contact.IsRead {
		return 0
	}
	return 1
}

func legacyMessagePreview(message *models.Message) string {
	if content := strings.TrimSpace(message.Content); content != "" {
		runes := []rune(content)
		if len(runes) > 100 {
			return string(runes[:97]) + "..."
		}
		return content
	}
	if message.MessageType != "" {
		return "[" + string(message.MessageType) + "]"
	}
	return "[Message]"
}
