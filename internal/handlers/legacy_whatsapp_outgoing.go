package handlers

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	legacyReplyIdempotencyMetadataKey  = "idempotency_key"
	legacyReplyPayloadDigestKey        = "payload_digest"
	legacyReplyAccountMetadataKey      = "legacy_whatsapp_account_id"
	legacyReplyConversationMetadataKey = "inbox_conversation_id"
)

var (
	errLegacyReplyBindingUnavailable   = errors.New("legacy WhatsApp reply binding is unavailable")
	errLegacyReplyEntitlementInactive  = errors.New("omnichannel subscription is not active")
	errLegacyReplyIdempotencyCollision = errors.New("idempotency key was already used for a different reply payload")
)

// legacyWhatsAppReplyPolicy is set only by the exact-conversation endpoint.
// It is deliberately not part of the public JSON request shape used by the
// established Chat sender.
type legacyWhatsAppReplyPolicy struct {
	ConversationID    uuid.UUID
	ChannelAccountID  uuid.UUID
	WhatsAppAccountID uuid.UUID
	IdempotencyKey    string
	PayloadDigest     string
}

type outgoingIdempotentReplay struct {
	message models.Message
	contact models.Contact
}

func (e *outgoingIdempotentReplay) Error() string {
	return "outgoing message is an idempotent replay"
}

func legacyReplyAdvisoryLockID(
	organizationID, accountID, conversationID uuid.UUID,
	idempotencyKey string,
) int64 {
	scope := organizationID.String() + "\x00" +
		accountID.String() + "\x00" +
		conversationID.String() + "\x00" +
		idempotencyKey
	digest := sha256.Sum256([]byte(scope))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// claimLegacyReplyIdempotency serializes the durable pending-message claim.
// PostgreSQL advisory transaction locks keep concurrent API replicas from
// creating two pending Messages for the same logical send without requiring a
// release-only schema migration.
func claimLegacyReplyIdempotency(
	tx *gorm.DB,
	organizationID uuid.UUID,
	policy *legacyWhatsAppReplyPolicy,
) (*models.Message, error) {
	if tx == nil || policy == nil {
		return nil, fmt.Errorf("%w: policy is required", errLegacyReplyBindingUnavailable)
	}
	if tx.Name() != "postgres" {
		return nil, errors.New("durable legacy reply idempotency requires PostgreSQL")
	}
	lockID := legacyReplyAdvisoryLockID(
		organizationID,
		policy.WhatsAppAccountID,
		policy.ConversationID,
		policy.IdempotencyKey,
	)
	if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockID).Error; err != nil {
		return nil, fmt.Errorf("lock legacy reply idempotency claim: %w", err)
	}

	var existing []models.Message
	err := tx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND inbox_conversation_id = ? AND metadata ->> 'idempotency_key' = ? AND metadata ->> 'legacy_whatsapp_account_id' = ?",
			organizationID,
			policy.ConversationID,
			policy.IdempotencyKey,
			policy.WhatsAppAccountID.String(),
		).
		Limit(2).
		Find(&existing).Error
	if err != nil {
		return nil, fmt.Errorf("load legacy reply idempotency claim: %w", err)
	}
	if len(existing) == 0 {
		return nil, nil
	}
	if len(existing) != 1 || !legacyReplyMessageMatchesPolicy(&existing[0], policy) {
		return nil, errLegacyReplyIdempotencyCollision
	}
	return &existing[0], nil
}

func legacyReplyMessageMatchesPolicy(
	message *models.Message,
	policy *legacyWhatsAppReplyPolicy,
) bool {
	if message == nil || policy == nil ||
		message.OrganizationID == uuid.Nil ||
		message.InboxConversationID == nil ||
		*message.InboxConversationID != policy.ConversationID ||
		message.Direction != models.DirectionOutgoing ||
		message.MessageType != models.MessageTypeText {
		return false
	}
	return legacyReplyStringJSONValue(message.Metadata, legacyReplyIdempotencyMetadataKey) == policy.IdempotencyKey &&
		legacyReplyStringJSONValue(message.Metadata, legacyReplyPayloadDigestKey) == policy.PayloadDigest &&
		legacyReplyStringJSONValue(message.Metadata, legacyReplyAccountMetadataKey) == policy.WhatsAppAccountID.String() &&
		legacyReplyStringJSONValue(message.Metadata, legacyReplyConversationMetadataKey) == policy.ConversationID.String()
}

func legacyReplyStringJSONValue(value models.JSONB, key string) string {
	if value == nil {
		return ""
	}
	result, _ := value[key].(string)
	return strings.TrimSpace(result)
}

// lockStrictLegacyReplyOrder establishes the same global row-lock prefix as
// the idempotent legacy bridge before the send path resolves (and therefore
// locks) its canonical Contact. Ordinary mirrors lock ChannelAccount ->
// ContactIdentity -> InboxConversation -> Contact -> Message, then write the
// ConversationParticipant. Strict replies must never take any part of that
// prefix in the opposite order.
func lockStrictLegacyReplyOrder(
	tx *gorm.DB,
	organizationID uuid.UUID,
	policy *legacyWhatsAppReplyPolicy,
) error {
	if tx == nil || organizationID == uuid.Nil || policy == nil ||
		policy.ChannelAccountID == uuid.Nil || policy.ConversationID == uuid.Nil ||
		policy.WhatsAppAccountID == uuid.Nil {
		return fmt.Errorf("%w: strict lock scope is required", errLegacyReplyBindingUnavailable)
	}

	var shadow models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"id = ? AND organization_id = ?",
			policy.ChannelAccountID,
			organizationID,
		).
		First(&shadow).Error; err != nil {
		return fmt.Errorf("%w: lock channel account: %v", errLegacyReplyBindingUnavailable, err)
	}
	boundAccountID, err := channelapi.LegacyMetaWhatsAppAccountID(&shadow)
	if err != nil || shadow.Provider != channelapi.LegacyMetaProvider ||
		boundAccountID != policy.WhatsAppAccountID {
		return fmt.Errorf("%w: immutable account mapping", errLegacyReplyBindingUnavailable)
	}

	// Resolve the immutable identity key without taking the conversation lock;
	// the final locked query below revalidates every projected field. Ordinary
	// legacy mirrors lock this identity before they lock the conversation.
	var conversationProjection models.InboxConversation
	if err := tx.Select("id, organization_id, channel_account_id, contact_id, contact_identity_id").
		Where(
			"id = ? AND organization_id = ? AND channel_account_id = ?",
			policy.ConversationID,
			organizationID,
			policy.ChannelAccountID,
		).
		First(&conversationProjection).Error; err != nil ||
		conversationProjection.ContactIdentityID == nil ||
		*conversationProjection.ContactIdentityID == uuid.Nil {
		return fmt.Errorf("%w: conversation identity binding", errLegacyReplyBindingUnavailable)
	}
	var identity models.ContactIdentity
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND contact_id = ?",
			*conversationProjection.ContactIdentityID,
			organizationID,
			policy.ChannelAccountID,
			conversationProjection.ContactID,
		).
		First(&identity).Error; err != nil {
		return fmt.Errorf("%w: lock contact identity: %v", errLegacyReplyBindingUnavailable, err)
	}

	var conversation models.InboxConversation
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where(
			`id = ? AND organization_id = ? AND channel_account_id = ?
			 AND contact_id = ? AND contact_identity_id = ?`,
			policy.ConversationID,
			organizationID,
			policy.ChannelAccountID,
			conversationProjection.ContactID,
			*conversationProjection.ContactIdentityID,
		).
		First(&conversation).Error; err != nil {
		return fmt.Errorf("%w: lock conversation: %v", errLegacyReplyBindingUnavailable, err)
	}
	return nil
}

// validateLegacyReplyPolicyTx re-derives every sensitive binding from locked,
// tenant-scoped rows. It is called both while creating the pending Message and
// immediately before the provider request.
func (a *App) validateLegacyReplyPolicyTx(
	tx *gorm.DB,
	req *OutgoingMessageRequest,
	contactID uuid.UUID,
	prepareStrictMirror bool,
) (*models.InboxConversation, error) {
	if tx == nil || req == nil || req.legacyWhatsAppReply == nil || req.Account == nil {
		return nil, fmt.Errorf("%w: request policy is required", errLegacyReplyBindingUnavailable)
	}
	policy := req.legacyWhatsAppReply
	organizationID := req.Account.OrganizationID
	if !a.legacyWhatsAppReplyEnabled(organizationID) {
		return nil, fmt.Errorf("%w: rollout gate is disabled", errLegacyReplyBindingUnavailable)
	}

	var conversation models.InboxConversation
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND contact_id = ?",
			policy.ConversationID,
			organizationID,
			policy.ChannelAccountID,
			contactID,
		).
		First(&conversation).Error; err != nil {
		return nil, fmt.Errorf("%w: conversation: %v", errLegacyReplyBindingUnavailable, err)
	}
	if conversation.Channel != models.ChannelWhatsApp {
		return nil, fmt.Errorf("%w: conversation channel", errLegacyReplyBindingUnavailable)
	}

	var shadow models.ChannelAccount
	shadowLock := "SHARE"
	if prepareStrictMirror {
		// The strict mirror refreshes this row later in the same transaction.
		// Take UPDATE up front so concurrent sends do not deadlock while both
		// attempt to upgrade a shared lock.
		shadowLock = "UPDATE"
	}
	if err := tx.Clauses(clause.Locking{Strength: shadowLock}).
		Where(
			"id = ? AND organization_id = ?",
			conversation.ChannelAccountID,
			organizationID,
		).
		First(&shadow).Error; err != nil {
		return nil, fmt.Errorf("%w: channel account: %v", errLegacyReplyBindingUnavailable, err)
	}
	if shadow.Channel != models.ChannelWhatsApp ||
		shadow.Provider != channelapi.LegacyMetaProvider ||
		shadow.Status != models.ChannelAccountStatusActive ||
		stringConfigValue(shadow.Config, "reply_route") != "chat" ||
		!boolConfigValue(shadow.Config, "legacy_read_only") ||
		boolConfigValue(shadow.Config, "outbound_enabled") ||
		!boolConfigValue(shadow.Capabilities, "text") ||
		!boolConfigValue(shadow.Capabilities, "replies") ||
		!boolConfigValue(shadow.Capabilities, "service_window") {
		return nil, fmt.Errorf("%w: channel capability", errLegacyReplyBindingUnavailable)
	}
	boundAccountID, err := channelapi.LegacyMetaWhatsAppAccountID(&shadow)
	if err != nil || boundAccountID != policy.WhatsAppAccountID || boundAccountID != req.Account.ID {
		return nil, fmt.Errorf("%w: immutable account mapping", errLegacyReplyBindingUnavailable)
	}

	var account models.WhatsAppAccount
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where(
			"id = ? AND organization_id = ?",
			policy.WhatsAppAccountID,
			organizationID,
		).
		First(&account).Error; err != nil {
		return nil, fmt.Errorf("%w: established account: %v", errLegacyReplyBindingUnavailable, err)
	}
	legacyAccountName, _ := shadow.Metadata["legacy_account_name"].(string)
	if strings.TrimSpace(account.Status) != "active" ||
		strings.TrimSpace(account.Name) == "" ||
		strings.TrimSpace(account.Name) != strings.TrimSpace(legacyAccountName) ||
		strings.TrimSpace(account.PhoneID) == "" {
		return nil, fmt.Errorf("%w: established account is inactive", errLegacyReplyBindingUnavailable)
	}
	if err := a.prepareWhatsAppAccountForRuntime(&account); err != nil {
		return nil, fmt.Errorf("%w: runtime credentials: %v", errLegacyReplyBindingUnavailable, err)
	}

	parts := []channelapi.MessagePart{{
		Type: models.MessagePartTypeText,
		Text: req.Content,
	}}
	if err := channelapi.ValidateServiceWindow(
		channelapi.Capabilities{Text: true, Replies: true, ServiceWindow: true},
		conversation.ServiceWindowEndsAt,
		parts,
		time.Now().UTC(),
	); err != nil {
		return nil, fmt.Errorf("%w: %v", errLegacyReplyBindingUnavailable, err)
	}

	// Keep the commercial check last: on the delivery pass this is the final
	// database decision immediately before control returns to the provider call.
	entitled, err := channelapi.HasDurableOmnichannelEntitlement(
		tx,
		organizationID,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("evaluate durable omnichannel entitlement: %w", err)
	}
	if !entitled {
		return nil, errLegacyReplyEntitlementInactive
	}

	// Use the just-locked database record and freshly prepared credentials for
	// the eventual provider call, never the earlier HTTP projection.
	*req.Account = account
	return &conversation, nil
}

func persistStrictLegacyReplyMirror(
	tx *gorm.DB,
	req *OutgoingMessageRequest,
	message *models.Message,
) error {
	if tx == nil || req == nil || req.legacyWhatsAppReply == nil ||
		req.Account == nil || message == nil {
		return fmt.Errorf("%w: strict mirror input", errLegacyReplyBindingUnavailable)
	}
	result, err := channelapi.MirrorLegacyWhatsAppMessage(
		tx,
		channelapi.LegacyMetaAccountRef{
			ID:             req.Account.ID,
			OrganizationID: req.Account.OrganizationID,
			Name:           req.Account.Name,
			Status:         req.Account.Status,
		},
		message.ID,
	)
	if err != nil {
		return fmt.Errorf("persist strict legacy WhatsApp mirror: %w", err)
	}
	if result.ChannelAccountID != req.legacyWhatsAppReply.ChannelAccountID ||
		result.ConversationID != req.legacyWhatsAppReply.ConversationID {
		return fmt.Errorf("%w: mirror targeted another conversation", errLegacyReplyBindingUnavailable)
	}
	return nil
}
