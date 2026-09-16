package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func redactURLForLog(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid_url>"
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return parsed.Path
	}

	// Tenant-authored paths can themselves contain credentials. Log only the
	// origin; query, fragment, userinfo, and path are deliberately excluded.
	return parsed.Scheme + "://" + parsed.Host
}

// IncomingTextMessage represents a text, interactive, or media message from the webhook
type IncomingTextMessage struct {
	From             string `json:"from"`
	FromUserID       string `json:"from_user_id,omitempty"`        // BSUID
	FromParentUserID string `json:"from_parent_user_id,omitempty"` // parent BSUID
	To               string `json:"to,omitempty"`
	ToUserID         string `json:"to_user_id,omitempty"`        // BSUID
	ToParentUserID   string `json:"to_parent_user_id,omitempty"` // parent BSUID
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp"`
	Type             string `json:"type"`
	Text             *struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply,omitempty"`
		ListReply *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"list_reply,omitempty"`
		NFMReply *struct {
			ResponseJSON string `json:"response_json"`
			Body         string `json:"body"`
			Name         string `json:"name"`
		} `json:"nfm_reply,omitempty"`
		CallPermissionReply *struct {
			Response            string      `json:"response"`
			IsPermanent         bool        `json:"is_permanent"`
			ExpirationTimestamp json.Number `json:"expiration_timestamp,omitempty"`
			ResponseSource      string      `json:"response_source"`
		} `json:"call_permission_reply,omitempty"`
	} `json:"interactive,omitempty"`
	Image *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Caption  string `json:"caption,omitempty"`
	} `json:"image,omitempty"`
	Document *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Filename string `json:"filename,omitempty"`
		Caption  string `json:"caption,omitempty"`
	} `json:"document,omitempty"`
	Audio *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	} `json:"audio,omitempty"`
	Video *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Caption  string `json:"caption,omitempty"`
	} `json:"video,omitempty"`
	Sticker *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Animated bool   `json:"animated,omitempty"`
	} `json:"sticker,omitempty"`
	Context *struct {
		From string `json:"from"`
		ID   string `json:"id"` // WhatsApp message ID being replied to
	} `json:"context,omitempty"`
	Reaction *struct {
		MessageID string `json:"message_id"` // WhatsApp message ID being reacted to
		Emoji     string `json:"emoji"`      // The emoji reaction (empty string = remove reaction)
	} `json:"reaction,omitempty"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name,omitempty"`
		Address   string  `json:"address,omitempty"`
	} `json:"location,omitempty"`
	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button,omitempty"`
	Contacts []struct {
		Name struct {
			FormattedName string `json:"formatted_name"`
			FirstName     string `json:"first_name,omitempty"`
			LastName      string `json:"last_name,omitempty"`
		} `json:"name"`
		Phones []struct {
			Phone string `json:"phone"`
			Type  string `json:"type,omitempty"`
		} `json:"phones,omitempty"`
	} `json:"contacts,omitempty"`
}

// persistedIncomingMessage is the durable hand-off between Meta's webhook ACK
// path and the asynchronous media/chatbot continuation. Every field needed by
// the continuation is copied only after the contact, message, lifecycle event,
// and outbox row have committed.
type persistedIncomingMessage struct {
	OrganizationID uuid.UUID
	PhoneNumberID  string
	Message        IncomingTextMessage
	Account        models.WhatsAppAccount
	Contact        models.Contact
	Extracted      ExtractedMessage
	Persisted      models.Message
}

const (
	incomingAutomaticAISuppressedKey          = "automatic_ai_suppressed"
	incomingAutomaticAISuppressionReasonKey   = "automatic_ai_suppression_reason"
	incomingAutomaticAISuppressedAtKey        = "automatic_ai_suppressed_at"
	incomingIdentityReviewHoldIDKey           = "whatsapp_identity_review_hold_id"
	incomingIdentityReviewReasonKey           = "whatsapp_identity_review_reason"
	incomingIdentityReviewRouteModeKey        = "whatsapp_identity_review_route_mode"
	incomingIdentityReviewSelectorDigestKey   = "whatsapp_identity_review_selector_digest"
	incomingIdentityReviewRouteHeldDirect     = "held_direct"
	incomingIdentityReviewRouteReviewedFuture = "reviewed_future"
)

// incomingMessageAdmissionPolicy is server-derived by the authenticated
// WhatsApp identity admission wrapper. It accepts neither a tenant-authored
// selector nor a client-chosen route. A nil value preserves legacy callers.
type incomingMessageAdmissionPolicy struct {
	CanonicalContactID         uuid.UUID
	SuppressAutomaticAI        bool
	SuppressionReason          string
	IdentityReviewHoldID       uuid.UUID
	IdentityReviewReason       string
	IdentityReviewRouteMode    string
	IdentityReviewSelectorHash string
}

// Message identity never comes from the mutable WhatsAppAccount display name.
// Every durable proof must identify the same row; missing attribution is not a
// license to backfill it from a phone/contact/name match.
type whatsAppMessageLookup struct {
	WAMID            string
	MessageID        uuid.UUID
	ContactID        uuid.UUID
	Identity         coexistenceContactIdentity
	Direction        models.Direction // Empty allows either proven reply-target direction.
	Lock             bool
	RepairProjection bool
}

type whatsAppMessageResolution struct {
	Message          models.Message
	Contact          models.Contact
	IncomingActivity bool
	Continuation     *models.ScheduledJob
}

var errWhatsAppMessageOwnerDeleted = errors.New("WhatsApp message owner is soft-deleted")

type whatsAppMessageAuthorityKey struct{}
type whatsAppMessageAuthority struct {
	pool                  gorm.ConnPool
	account               models.WhatsAppAccount
	channelIDs            map[uuid.UUID]bool
	accessTokenGeneration [sha256.Size]byte
}

// prepareWhatsAppMessageAuthority must precede state/contact/message locks.
// Global order: ChannelAccount -> WhatsAppAccount -> canonical Contact path ->
// Message -> continuation job. Joined conversations are read, never locked here;
// the bridge's ChannelAccount -> Conversation -> Contact -> Message order must
// not be inverted when this resolver is reentered from an already locked caller.
// Mirror is always after commit.
// The transaction-bound token prevents a nested target operation from taking
// a NEW channel/account lock behind a contact/message already held by its caller.
func (a *App) prepareWhatsAppMessageAuthority(account *models.WhatsAppAccount) error {
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil {
		return errors.New("WhatsApp message account authority is required")
	}
	if _, ok := a.DB.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errors.New("WhatsApp message authority requires a transaction")
	}
	if authority, ok := a.DB.Statement.Context.Value(whatsAppMessageAuthorityKey{}).(*whatsAppMessageAuthority); ok && authority.pool == a.DB.Statement.ConnPool {
		if authority.account.ID != account.ID || authority.account.OrganizationID != account.OrganizationID {
			return errors.New("WhatsApp message transaction account changed")
		}
		*account = authority.account
		return nil
	}
	var shadows []models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "SHARE"}).Where(
		"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
		account.OrganizationID, models.ChannelWhatsApp, channelapi.LegacyMetaProvider, "legacy-account:"+account.ID.String(),
	).Order("id").Find(&shadows).Error; err != nil {
		return err
	}
	if len(shadows) > 1 {
		return errors.New("ambiguous WhatsApp message channel authority")
	}
	channelIDs := make(map[uuid.UUID]bool)
	for i := range shadows {
		boundID, err := channelapi.LegacyMetaWhatsAppAccountID(&shadows[i])
		if err != nil || boundID != account.ID {
			return errors.New("WhatsApp message channel authority mismatch")
		}
		channelIDs[shadows[i].ID] = true
	}
	var current models.WhatsAppAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "SHARE"}).Where(
		"id = ? AND organization_id = ?", account.ID, account.OrganizationID,
	).First(&current).Error; err != nil {
		return err
	}
	if strings.TrimSpace(account.PhoneID) != "" && strings.TrimSpace(current.PhoneID) != strings.TrimSpace(account.PhoneID) {
		return errors.New("WhatsApp message phone authority changed")
	}
	tokenGeneration := sha256.Sum256([]byte(current.AccessToken))
	a.decryptAccountSecrets(&current)
	authority := &whatsAppMessageAuthority{pool: a.DB.Statement.ConnPool, account: current, channelIDs: channelIDs, accessTokenGeneration: tokenGeneration}
	a.DB = a.DB.WithContext(context.WithValue(a.DB.Statement.Context, whatsAppMessageAuthorityKey{}, authority))
	*account = current
	return nil
}

func (a *App) resolveWhatsAppMessage(account *models.WhatsAppAccount, lookup whatsAppMessageLookup) (*whatsAppMessageResolution, error) {
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil || strings.TrimSpace(lookup.WAMID) == "" {
		return nil, errors.New("WhatsApp message ownership lookup is incomplete")
	}
	lookup.WAMID = strings.TrimSpace(lookup.WAMID)
	var authority *whatsAppMessageAuthority
	if lookup.Lock || lookup.RepairProjection {
		authority, _ = a.DB.Statement.Context.Value(whatsAppMessageAuthorityKey{}).(*whatsAppMessageAuthority)
		if authority == nil || authority.pool != a.DB.Statement.ConnPool || authority.account.ID != account.ID || authority.account.OrganizationID != account.OrganizationID {
			return nil, errors.New("WhatsApp message mutation lacks prelocked account authority")
		}
		if lookup.RepairProjection && !lookup.Lock {
			return nil, errors.New("WhatsApp message projection repair requires a locked message")
		}
	}
	var currentAccount models.WhatsAppAccount
	if err := a.DB.Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).First(&currentAccount).Error; err != nil {
		return nil, err
	}
	if strings.TrimSpace(currentAccount.PhoneID) != strings.TrimSpace(account.PhoneID) {
		return nil, errors.New("WhatsApp message account phone mismatch")
	}
	activityKey := "message-incoming:" + uuid.NewSHA1(account.ID, []byte(lookup.WAMID)).String()
	jobKey := "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(lookup.WAMID)).String()
	deterministicID := uuid.NewSHA1(account.ID, []byte("coexistence-message:"+lookup.WAMID))
	var candidates []models.Message
	if err := a.DB.Unscoped().Where(`organization_id = ? AND (
			id = ?
			OR (
				BTRIM(whats_app_message_id) = ?
				AND (
					inbox_conversation_id IS NULL
					OR COALESCE(metadata, '{}'::jsonb) @> ?::jsonb
				)
			)
		)`, account.OrganizationID, deterministicID, lookup.WAMID, database.WhatsAppWAMIDOwnerMetadataJSON).
		Order("id").Find(&candidates).Error; err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(candidates)+1)
	textIDs := make([]string, 0, len(candidates)+1)
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
		textIDs = append(textIDs, candidate.ID.String())
	}
	if lookup.MessageID != uuid.Nil {
		ids = append(ids, lookup.MessageID)
		textIDs = append(textIDs, lookup.MessageID.String())
	}
	var activities []models.CustomerActivityEvent
	if err := a.DB.Where("organization_id = ? AND (idempotency_key = ? OR (source_object_id IN ? AND (event_type = ? OR idempotency_key LIKE ?)))",
		account.OrganizationID, activityKey, ids, models.CustomerActivityMessageIncoming, "message-incoming:%").Find(&activities).Error; err != nil {
		return nil, err
	}
	var jobs []models.ScheduledJob
	if err := a.DB.Where("organization_id = ? AND (idempotency_key = ? OR ((kind = ? OR idempotency_key LIKE ?) AND (aggregate_id IN ? OR payload->>'wamid' = ? OR payload->>'message_id' IN ? OR payload->'message'->>'id' = ?)))",
		account.OrganizationID, jobKey, inboundContinuationJobKind, "inbound-message-continuation:%", ids, lookup.WAMID, textIDs, lookup.WAMID).Find(&jobs).Error; err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		if len(activities) != 0 || len(jobs) != 0 || lookup.MessageID != uuid.Nil {
			return nil, errors.New("WhatsApp message provenance references a missing winner")
		}
		return nil, gorm.ErrRecordNotFound
	}
	// More than one org/WAMID row is ambiguity, not permission to select the
	// first linked/unlinked representation or manufacture a new execution ID.
	if len(candidates) != 1 {
		return nil, errors.New("ambiguous WhatsApp message winners")
	}
	message := candidates[0]
	if message.WhatsAppMessageID != lookup.WAMID || (lookup.MessageID != uuid.Nil && lookup.MessageID != message.ID) ||
		(message.Direction != models.DirectionIncoming && message.Direction != models.DirectionOutgoing) ||
		(lookup.Direction != "" && message.Direction != lookup.Direction) {
		return nil, errors.New("WhatsApp message winner identity or direction mismatch")
	}
	if message.DeletedAt.Valid {
		// The WAMID remains durably reserved, but a replay or mutation may never
		// revive, repair, or otherwise modify its soft-deleted first owner.
		return nil, errWhatsAppMessageOwnerDeleted
	}
	reviewRouted, err := a.validatePersistedWhatsAppIdentityReviewRoute(account, &message, lookup.Identity)
	if err != nil {
		return nil, err
	}
	canonical, err := contactutil.ResolveCanonicalContact(a.DB, account.OrganizationID, message.ContactID)
	if err != nil {
		return nil, err
	}
	if lookup.ContactID != uuid.Nil {
		expected, err := contactutil.ResolveCanonicalContact(a.DB, account.OrganizationID, lookup.ContactID)
		if err != nil || expected.ID != canonical.ID {
			return nil, errors.New("WhatsApp message canonical contact mismatch")
		}
	}
	if !reviewRouted {
		if err := a.validateWhatsAppMessageContactIdentity(account.OrganizationID, canonical, lookup.Identity); err != nil {
			return nil, err
		}
	}
	proven := message.ID == deterministicID
	result := &whatsAppMessageResolution{Message: message, Contact: *canonical}
	for _, activity := range activities {
		if activity.IdempotencyKey != activityKey || activity.SourceObjectType != "message" || activity.SourceObjectID == nil || *activity.SourceObjectID != message.ID ||
			activity.EventType != models.CustomerActivityMessageIncoming || activity.Category != models.CustomerActivityCategoryMessage || message.Direction != models.DirectionIncoming {
			return nil, errors.New("conflicting WhatsApp incoming activity provenance")
		}
		activityContact, err := contactutil.ResolveCanonicalContact(a.DB, account.OrganizationID, activity.ContactID)
		if err != nil || activityContact.ID != canonical.ID {
			return nil, errors.New("WhatsApp activity canonical contact mismatch")
		}
		proven, result.IncomingActivity = true, true
	}
	for i := range jobs {
		inbound, err := validateInboundContinuationJobProof(&jobs[i], &currentAccount, message.ID, lookup.WAMID)
		if err != nil || message.Direction != models.DirectionIncoming {
			return nil, errors.New("conflicting WhatsApp continuation provenance")
		}
		if !reviewRouted {
			if err := a.validateWhatsAppMessageContactIdentity(account.OrganizationID, canonical, coexistenceContactIdentity{
				Phone: inbound.From, UserID: inbound.FromUserID, ParentUserID: inbound.FromParentUserID,
			}); err != nil {
				return nil, err
			}
		} else if normalizeIdentityReviewPhone(inbound.From) != normalizeIdentityReviewPhone(lookup.Identity.Phone) ||
			strings.TrimSpace(inbound.FromUserID) != strings.TrimSpace(lookup.Identity.primaryUserID()) ||
			strings.TrimSpace(inbound.FromParentUserID) != strings.TrimSpace(lookup.Identity.ParentUserID) {
			return nil, errors.New("WhatsApp reviewed continuation selector proof changed")
		}
		if result.Continuation != nil {
			return nil, errors.New("multiple WhatsApp continuation winners")
		}
		proven, result.Continuation = true, &jobs[i]
	}
	// Historical ownership reserves the WAMID; it does not authorize mutation
	// through a linked projection that now contradicts the established owner.
	if message.InboxConversationID != nil {
		var conversation models.InboxConversation
		conversationQuery := a.DB.Where("id = ? AND organization_id = ?", *message.InboxConversationID, account.OrganizationID)
		if err := conversationQuery.First(&conversation).Error; err != nil {
			return nil, err
		}
		if conversation.Channel != models.ChannelWhatsApp || conversation.ContactIdentityID == nil {
			return nil, errors.New("WhatsApp message conversation provider mismatch")
		}
		linkedContact, err := contactutil.ResolveCanonicalContact(a.DB, account.OrganizationID, conversation.ContactID)
		if err != nil || linkedContact.ID != canonical.ID || conversation.ExternalConversationID != "legacy-contact:"+conversation.ContactID.String() {
			return nil, errors.New("WhatsApp message conversation contact mismatch")
		}
		var shadow models.ChannelAccount
		if err := a.DB.Where("id = ? AND organization_id = ?", conversation.ChannelAccountID, account.OrganizationID).First(&shadow).Error; err != nil {
			return nil, err
		}
		boundID, err := channelapi.LegacyMetaWhatsAppAccountID(&shadow)
		if err != nil || boundID != account.ID {
			return nil, errors.New("WhatsApp message joined account provenance mismatch")
		}
		if lookup.Lock && !authority.channelIDs[shadow.ID] {
			return nil, contactutil.ErrCanonicalContactChanged
		}
		var linkedIdentity models.ContactIdentity
		if err := a.DB.Where("id = ? AND organization_id = ? AND channel_account_id = ? AND channel = ?",
			*conversation.ContactIdentityID, account.OrganizationID, shadow.ID, models.ChannelWhatsApp).First(&linkedIdentity).Error; err != nil {
			return nil, err
		}
		identityContact, err := contactutil.ResolveCanonicalContact(a.DB, account.OrganizationID, linkedIdentity.ContactID)
		if err != nil || identityContact.ID != canonical.ID || linkedIdentity.ExternalID != "legacy-contact:"+conversation.ContactID.String() {
			return nil, errors.New("WhatsApp message joined contact identity mismatch")
		}
		proven = true
	}
	if !proven {
		return nil, errors.New("WhatsApp message lacks durable account provenance")
	}
	if lookup.Lock {
		lockedContact, err := contactutil.ResolveCanonicalContactForUpdate(a.DB, account.OrganizationID, message.ContactID)
		if err != nil || lockedContact.ID != canonical.ID {
			return nil, contactutil.ErrCanonicalContactChanged
		}
		var locked models.Message
		if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", message.ID, account.OrganizationID).First(&locked).Error; err != nil {
			return nil, err
		}
		if locked.ContactID != message.ContactID || locked.WhatsAppMessageID != message.WhatsAppMessageID || locked.Direction != message.Direction ||
			!customerWorkspaceUUIDPointersEqual(locked.InboxConversationID, message.InboxConversationID) {
			return nil, contactutil.ErrCanonicalContactChanged
		}
		// Validate every proof again after the lock; no earlier authority lock
		// is acquired here, and a concurrent provenance conflict fails closed.
		recheck := lookup
		recheck.Lock, recheck.RepairProjection, recheck.MessageID = false, false, locked.ID
		verified, err := a.resolveWhatsAppMessage(account, recheck)
		if err != nil {
			return nil, err
		}
		result = verified
		if lookup.RepairProjection && result.Message.WhatsAppAccount != currentAccount.Name {
			if err := a.DB.Model(&models.Message{}).Where("id = ? AND organization_id = ?", message.ID, account.OrganizationID).
				UpdateColumn("whats_app_account", currentAccount.Name).Error; err != nil {
				return nil, err
			}
			result.Message.WhatsAppAccount = currentAccount.Name
		}
		// Optional BSUID is learned only after independent account/WAMID
		// authority and the SAME canonical contact/message have been locked and
		// fully rechecked. A failed optional write must not make the unchanged
		// enqueue/worker resolver reject an otherwise proven phone-only winner.
		if !reviewRouted {
			a.enrichProvenWhatsAppMessageBSUID(account, &result.Contact, lookup)
		}
	}
	return result, nil
}

func (a *App) validatePersistedWhatsAppIdentityReviewRoute(
	account *models.WhatsAppAccount,
	message *models.Message,
	identity coexistenceContactIdentity,
) (bool, error) {
	if account == nil || message == nil {
		return false, errors.New("WhatsApp identity-review route identity is incomplete")
	}
	holdValue, hasHold := message.Metadata[incomingIdentityReviewHoldIDKey]
	reasonValue, hasReason := message.Metadata[incomingIdentityReviewReasonKey]
	modeValue, hasMode := message.Metadata[incomingIdentityReviewRouteModeKey]
	digestValue, hasDigest := message.Metadata[incomingIdentityReviewSelectorDigestKey]
	if !hasHold && !hasReason && !hasMode && !hasDigest {
		return false, nil
	}
	if !hasHold || !hasReason || !hasMode || !hasDigest || message.Direction != models.DirectionIncoming ||
		message.OrganizationID != account.OrganizationID || strings.TrimSpace(message.WhatsAppMessageID) == "" {
		return false, errors.New("WhatsApp identity-review route proof is incomplete")
	}
	holdText, holdOK := holdValue.(string)
	reason, reasonOK := reasonValue.(string)
	routeMode, modeOK := modeValue.(string)
	selectorDigest, digestOK := digestValue.(string)
	if !holdOK || !reasonOK || !modeOK || !digestOK || holdText != strings.TrimSpace(holdText) ||
		reason != strings.TrimSpace(reason) || selectorDigest != strings.ToLower(strings.TrimSpace(selectorDigest)) ||
		!isSHA256Hex(selectorDigest) {
		return false, errors.New("WhatsApp identity-review route proof is malformed")
	}
	holdID, err := uuid.Parse(holdText)
	if err != nil || holdID == uuid.Nil || holdID.String() != holdText || !whatsAppIdentityReviewRouteReasonAllowed(reason) {
		return false, errors.New("WhatsApp identity-review route proof is invalid")
	}
	var hold models.WhatsAppIdentityReviewHold
	if err := a.DB.Where("id = ? AND organization_id = ?", holdID, account.OrganizationID).First(&hold).Error; err != nil {
		return false, err
	}
	if hold.WhatsAppAccountID != account.ID || hold.OnboardingCycle == 0 ||
		hold.ProtocolVersion != models.WhatsAppIdentityReviewProtocolVersion || !hold.Supported ||
		hold.VerifiedEventProvenance != WhatsAppIdentityReviewVerifiedMetaEvent ||
		!isSHA256Hex(hold.VerifiedEventDigest) || !isSHA256Hex(hold.SelectorBodyDigest) ||
		hold.DirectPrimaryBSUID != strings.TrimSpace(identity.primaryUserID()) ||
		hold.ParentBSUID != strings.TrimSpace(identity.ParentUserID) ||
		hold.Phone != normalizeIdentityReviewPhone(identity.Phone) {
		return false, errors.New("WhatsApp identity-review route authority changed")
	}
	expectedDigest, err := whatsappIdentityReviewBindingDigest(whatsAppIdentityReviewSelectorBinding{
		Protocol:           models.WhatsAppIdentityReviewInboundProtocol,
		OrganizationID:     account.OrganizationID,
		WhatsAppAccountID:  account.ID,
		OnboardingCycle:    hold.OnboardingCycle,
		WAMID:              message.WhatsAppMessageID,
		DirectPrimaryBSUID: strings.TrimSpace(identity.primaryUserID()),
		ParentBSUID:        strings.TrimSpace(identity.ParentUserID),
		Phone:              normalizeIdentityReviewPhone(identity.Phone),
	})
	if err != nil || expectedDigest != selectorDigest {
		return false, errors.New("WhatsApp identity-review selector proof changed")
	}
	snapshot, err := loadWhatsAppIdentityReviewSnapshot(a.DB, &hold)
	if err != nil {
		return false, err
	}
	if identityReviewSemanticDigest(identityReviewClaimFromHold(hold), snapshot.MemberDigest) != hold.SemanticClaimDigest {
		return false, errors.New("WhatsApp identity-review semantic authority changed")
	}
	if target, visible := whatsappIdentityReviewVisibleIntakeTarget(&message.ContactID, snapshot.Candidates); !visible || target != message.ContactID {
		return false, errors.New("WhatsApp identity-review route target changed")
	}
	switch routeMode {
	case incomingIdentityReviewRouteReviewedFuture:
		if reason != "reviewed_future_route" && reason != "another_review_open" {
			return false, errors.New("WhatsApp reviewed future route reason changed")
		}
		if hold.Disposition != models.WhatsAppIdentityReviewDispositionFutureRouting ||
			hold.DecisionTargetContactID == nil || *hold.DecisionTargetContactID != message.ContactID ||
			hold.DecisionResolvedByID == nil || hold.DecisionResolvedAt == nil ||
			hold.DecisionRequestID == nil || !isSHA256Hex(hold.DecisionRequestDigest) ||
			!isSHA256Hex(hold.DecisionChainDigest) {
			return false, errors.New("WhatsApp reviewed future route changed")
		}
	case incomingIdentityReviewRouteHeldDirect:
		if reason == "reviewed_future_route" || len(snapshot.Candidates) == 0 {
			return false, errors.New("WhatsApp held direct route reason changed")
		}
		directOwners := make([]uuid.UUID, 0, 1)
		for _, candidate := range snapshot.Candidates {
			if candidate.SelectorReasons.Has(models.WhatsAppIdentityReviewSelectorPrimaryBSUID) {
				directOwners = append(directOwners, candidate.ContactID)
			}
		}
		allowed, _ := identityReviewDirectRouteAllowed(snapshot.Candidates, message.ContactID)
		if len(directOwners) != 1 || directOwners[0] != message.ContactID || !allowed {
			return false, errors.New("WhatsApp held direct route target changed")
		}
	default:
		return false, errors.New("WhatsApp identity-review route mode is invalid")
	}
	if reason != "reviewed_future_route" {
		suppressionReason, _ := message.Metadata[incomingAutomaticAISuppressionReasonKey].(string)
		if !incomingMessageAutomaticAISuppressed(message) || suppressionReason != "whatsapp_identity_review:"+reason {
			return false, errors.New("WhatsApp held identity-review route lost suppression")
		}
	}
	return true, nil
}

func whatsAppIdentityReviewRouteReasonAllowed(reason string) bool {
	switch reason {
	case "phone_selector_conflict", "phone_selector_drift", "unique_direct_primary_drift",
		"review_open", "another_review_open", "reviewed_future_route":
		return true
	default:
		return false
	}
}

// This is only a contact corroboration/enrichment predicate, NEVER message or
// account provenance. In particular an old merged phone alias, absent phone or
// non-dialable placeholder cannot authorize learning a new BSUID.
func canEnrichProvenWhatsAppMessageBSUID(canonical *models.Contact, identity coexistenceContactIdentity) bool {
	if canonical == nil || canonical.BSUID != "" || identity.primaryUserID() == "" || isCoexistencePlaceholderPhone(canonical.PhoneNumber) {
		return false
	}
	phone := normalizeCoexistencePhone(identity.Phone)
	return phone != "" && phone == normalizeCoexistencePhone(canonical.PhoneNumber)
}

// Called only from the post-lock/post-proof resolver branch. Retain a savepoint
// around optional metadata so a trigger/constraint failure cannot poison the
// transaction containing the required message, lifecycle and continuation fact.
func (a *App) enrichProvenWhatsAppMessageBSUID(account *models.WhatsAppAccount, canonical *models.Contact, lookup whatsAppMessageLookup) {
	if !canEnrichProvenWhatsAppMessageBSUID(canonical, lookup.Identity) {
		return
	}
	userID := lookup.Identity.primaryUserID()
	var updated bool
	err := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		result := tx.Model(&models.Contact{}).Where(
			"organization_id = ? AND id = ? AND phone_number = ? AND COALESCE(bs_uid, '') = '' AND merged_into_id IS NULL",
			canonical.OrganizationID, canonical.ID, canonical.PhoneNumber,
		).Update("bs_uid", userID)
		updated = result.RowsAffected == 1
		if result.Error != nil || !updated {
			return result.Error
		}
		// Optional learning must not invalidate another durable proof (for
		// example a queued payload whose optional identifier was never stored).
		// Roll back only the enrichment if the full proof no longer agrees.
		recheck := lookup
		recheck.Lock, recheck.RepairProjection = false, false
		scoped := a.scopedApp(tx, account.OrganizationID)
		_, err := scoped.resolveWhatsAppMessage(account, recheck)
		return err
	})
	if err != nil {
		a.Log.Warn("Optional BSUID enrichment failed for proven incoming message", "error", err, "contact_id", canonical.ID)
		return
	}
	if updated {
		canonical.BSUID = userID
	}
}

// Contact identity corroborates a separately proven account/message binding; it
// is never an account proof itself. Historical sender aliases remain legitimate
// only when their existing merge chain reaches the same canonical contact.
func (a *App) validateWhatsAppMessageContactIdentity(orgID uuid.UUID, canonical *models.Contact, identity coexistenceContactIdentity) error {
	if canonical == nil || canonical.OrganizationID != orgID {
		return errors.New("WhatsApp message canonical contact is missing")
	}
	phone := normalizeCoexistencePhone(identity.Phone)
	if phone != "" && phone != normalizeCoexistencePhone(canonical.PhoneNumber) {
		var aliases []models.Contact
		if err := a.DB.Unscoped().Where("organization_id = ? AND phone_number IN ?", orgID, []string{phone, "+" + phone}).Find(&aliases).Error; err != nil {
			return err
		}
		// A BSUID placeholder may acquire its first real phone, but an existing
		// phone belonging to a different canonical contact is not that proof.
		placeholderReveal := isCoexistencePlaceholderPhone(canonical.PhoneNumber) &&
			identity.primaryUserID() != "" && canonical.BSUID == identity.primaryUserID()
		if len(aliases) == 0 && !placeholderReveal {
			return errors.New("WhatsApp message sender phone mismatch")
		}
		for _, alias := range aliases {
			resolved, err := contactutil.ResolveCanonicalContact(a.DB, orgID, alias.ID)
			if err != nil || resolved.ID != canonical.ID {
				return errors.New("WhatsApp message sender phone canonical mismatch")
			}
		}
	}
	userID, parentID := identity.primaryUserID(), strings.TrimSpace(identity.ParentUserID)
	if userID != "" {
		identifiers := []string{userID}
		if parentID != "" && parentID != userID {
			identifiers = append(identifiers, parentID)
		}
		var aliases []models.Contact
		if err := a.DB.Unscoped().Where("organization_id = ? AND bs_uid IN ?", orgID, identifiers).Find(&aliases).Error; err != nil {
			return err
		}
		matched := canonical.BSUID == userID || (parentID != "" && canonical.BSUID == parentID)
		for _, alias := range aliases {
			resolved, err := contactutil.ResolveCanonicalContact(a.DB, orgID, alias.ID)
			if err != nil || resolved.ID != canonical.ID {
				return errors.New("WhatsApp message sender user canonical mismatch")
			}
			matched = true
		}
		if !matched && !canEnrichProvenWhatsAppMessageBSUID(canonical, identity) {
			return errors.New("WhatsApp message sender user identity mismatch")
		}
	}
	return nil
}

// updateContactBSUID persists Meta's optional business-scoped user ID without
// allowing a metadata-write failure to poison the surrounding inbound-message
// transaction. GORM implements nested transactions with a PostgreSQL savepoint,
// so rolling this callback back restores the outer tenant transaction.
func (a *App) updateContactBSUID(contact *models.Contact, bsuid string) {
	if contact == nil || bsuid == "" || contact.BSUID == bsuid {
		return
	}

	err := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		return tx.Model(&models.Contact{}).
			Where("id = ?", contact.ID).
			Update("bs_uid", bsuid).Error
	})
	if err != nil {
		a.Log.Warn("Failed to update contact BSUID; continuing inbound message processing",
			"error", err,
			"contact_id", contact.ID,
		)
		return
	}
	contact.BSUID = bsuid
}

// processIncomingMessageFull processes incoming WhatsApp messages with chatbot logic
func (a *App) processIncomingMessageFull(phoneNumberID string, msg IncomingTextMessage, profileName string) {
	a.Log.Info("Processing incoming message",
		"phone_number_id", phoneNumberID,
		"from", msg.From,
		"type", msg.Type,
		"profile_name", profileName,
	)

	// Find the WhatsApp account by phone_number_id (use cache)
	account, err := a.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		a.Log.Error("WhatsApp account not found", "phone_id", phoneNumberID, "error", err)
		return
	}

	// Handle reaction messages specially - they update existing messages, not create new ones
	if msg.Type == "reaction" && msg.Reaction != nil {
		a.handleIncomingReaction(account, msg.From, msg.Reaction.MessageID, msg.Reaction.Emoji, profileName)
		return
	}

	work, duplicate, err := a.persistIncomingMessageForAccount(
		phoneNumberID,
		msg,
		profileName,
		account,
	)
	if err != nil {
		a.Log.Error("Failed to persist incoming message", "from", msg.From, "error", err)
		return
	}
	if duplicate {
		a.startPersistedIncomingMessageContinuation(work)
		return
	}
	a.startPersistedIncomingMessageContinuation(work)
}

// continuePersistedIncomingMessage performs work that is intentionally outside
// Meta's acknowledgement critical path. Media download and every chatbot or
// provider call happen here, after the normalized inbound fact is durable.
func (a *App) continuePersistedIncomingMessage(
	ctx context.Context,
	work *persistedIncomingMessage,
) error {
	if work == nil {
		return nil
	}

	account := &work.Account
	contact, err := contactutil.ResolveCanonicalContact(
		a.DB,
		work.OrganizationID,
		work.Contact.ID,
	)
	if err != nil {
		a.Log.Error(
			"Failed to resolve inbound contact for asynchronous continuation",
			"error", err,
			"contact_id", work.Contact.ID,
			"message_id", work.Persisted.ID,
		)
		return fmt.Errorf("resolve inbound continuation contact: %w", err)
	}

	if err := a.hydratePersistedIncomingMedia(ctx, work, account); err != nil {
		return err
	}
	if work.Persisted.Metadata[coexistenceMediaRevokedMetadataKey] == true {
		// A late live delivery cannot hydrate, rebroadcast or process the
		// pre-deletion payload of an imported tombstone.
		return nil
	}
	// Broadcast only after optional hydration so a media message reaches the UI
	// once, with its durable object-store URL when download succeeded.
	// The inbound fact and optional media hydration are already durable. Publish
	// through the root app so realtime delivery completes before the first
	// automatic-action boundary instead of waiting for the continuation's
	// bookkeeping transaction to finish after provider I/O.
	a.rootApp().broadcastNewMessage(work.OrganizationID, &work.Persisted, contact)
	if incomingMessageAutomaticAISuppressed(&work.Persisted) {
		reason, _ := work.Persisted.Metadata[incomingAutomaticAISuppressionReasonKey].(string)
		return &inboundContinuationPolicyStop{Reason: reason}
	}

	msg := work.Message
	extracted := work.Extracted
	messageText := extracted.Text
	buttonID := extracted.ButtonID
	flowResponseData := extracted.FlowResponseData

	// Clear chatbot tracking since this client message is newer than the bot
	// message it acknowledges. An older continuation replay must not erase a
	// newer bot reply from another committed continuation.
	if err := a.clearContactChatbotTrackingForInbound(contact.ID); err != nil {
		return fmt.Errorf("clear inbound chatbot tracking: %w", err)
	}

	// Check for active agent transfer - skip chatbot processing if transferred
	if a.hasActiveAgentTransfer(account.OrganizationID, contact.ID) {
		a.Log.Info("Contact has active agent transfer, skipping chatbot processing",
			"contact_id", contact.ID,
			"phone_number", contact.PhoneNumber)
		return nil
	}

	// Check if chatbot is enabled for this account (use cache)
	settings, err := a.getChatbotSettingsCached(account.OrganizationID, account.Name)
	if err != nil {
		a.Log.Error("Failed to load chatbot settings", "error", err, "account", account.Name, "org_id", account.OrganizationID)
		return fmt.Errorf("load inbound continuation chatbot settings: %w", err)
	}
	if !settings.IsEnabled {
		a.Log.Debug("Chatbot not enabled for this account, creating transfer for agent queue", "account", account.Name, "settings_id", settings.ID)
		// Create transfer to agent queue when chatbot is disabled
		a.createTransferToQueue(account, contact, models.TransferSourceChatbotDisabled)
		return nil
	}
	a.Log.Info("Chatbot settings loaded", "settings_id", settings.ID, "is_enabled", settings.IsEnabled, "ai_enabled", settings.AI.Enabled, "ai_provider", settings.AI.Provider, "default_response", settings.DefaultResponse)

	// Check business hours if enabled
	if settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
		if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
			// If automated responses are not allowed outside hours, send out-of-hours message and stop
			if !settings.BusinessHours.AllowAutomatedOutside {
				a.Log.Info("Outside business hours, sending out of hours message")
				if settings.BusinessHours.OutOfHoursMessage != "" {
					if err := a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage); err != nil {
						a.Log.Error("Failed to send out of hours message", "error", err, "contact", contact.PhoneNumber)
						return fmt.Errorf("send out-of-hours response: %w", err)
					}
				}
				return nil
			}
			// AllowAutomatedOutsideHours is true, continue processing flows/keywords/AI
			a.Log.Info("Outside business hours but automated responses allowed, continuing")
		}
	}

	// Only process text and interactive messages for chatbot
	if messageText == "" {
		a.Log.Debug("Skipping message with no text content for chatbot", "type", msg.Type)
		return nil
	}

	a.Log.Info("Processing message", "text", messageText, "buttonID", buttonID, "from", msg.From)

	// Get or create active session for this contact
	session, isNewSession, err := a.getOrCreateSession(
		account.OrganizationID,
		contact.ID,
		account.Name,
		msg.From,
		settings.SessionTimeoutMins,
	)
	if err != nil || session == nil {
		a.Log.Error(
			"Failed to get or create chatbot session",
			"error", err,
			"contact_id", contact.ID,
			"account", account.Name,
		)
		if err == nil {
			err = errors.New("chatbot session was not returned")
		}
		return fmt.Errorf("get inbound continuation chatbot session: %w", err)
	}

	// Log incoming message to session
	a.logSessionMessage(session.ID, models.DirectionIncoming, messageText, "keyword_check")

	// An exact server token is never a keyword, greeting or model instruction.
	// Consume it once against the durable offer before any ordinary response.
	if isNativeBookingConfirmation(messageText) {
		_, _, bookingErr := a.processNativeAIBooking(account, contact, session, settings, messageText)
		return bookingErr
	}

	// Check for transfer keyword BEFORE sending greeting (transfer takes priority)
	keywordResponse, keywordMatched := a.matchKeywordRules(account.OrganizationID, account.Name, messageText)
	if keywordMatched && keywordResponse.ResponseType == models.ResponseTypeTransfer {
		a.Log.Info("Transfer keyword matched", "response", keywordResponse.Body)
		// Check business hours - if outside hours, send out of hours message instead
		if settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
			if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
				a.Log.Info("Outside business hours, sending out of hours message instead of transfer")
				if settings.BusinessHours.OutOfHoursMessage != "" {
					if err := a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage); err != nil {
						a.Log.Error("Failed to send out of hours message", "error", err, "contact", contact.PhoneNumber)
						return fmt.Errorf(
							"send transfer out-of-hours response: %w",
							err,
						)
					}
				}
				return nil
			}
		}
		// Within business hours - send transfer message and create transfer
		if keywordResponse.Body != "" {
			if err := a.sendAndSaveTextMessage(account, contact, keywordResponse.Body); err != nil {
				a.Log.Error("Failed to send transfer message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send transfer response: %w", err)
			}
		}
		a.createTransferFromKeyword(account, contact)
		return nil
	}

	// Check if user is in an active flow. After Phase 4.2 every flow has
	// a v2 Graph populated; any flow without one is a misconfiguration
	// (manual DB edit or failed backfill) — log and exit cleanly.
	if session.CurrentFlowID != nil {
		flow, err := a.getChatbotFlowByIDCached(account.OrganizationID, *session.CurrentFlowID)
		if err != nil || flow == nil {
			a.Log.Error("Active chatbot flow not loadable", "error", err, "session", session.ID, "flow", session.CurrentFlowID)
			a.exitFlow(session)
			if err != nil {
				return fmt.Errorf("load active chatbot flow: %w", err)
			}
			return nil
		}
		if flow.Graph == nil {
			a.Log.Error("Active chatbot flow has no v2 graph; ignoring inbound", "session", session.ID, "flow", flow.ID)
			a.exitFlow(session)
			return nil
		}
		if err := a.runChatGraph(account, contact, session, flow, messageText, buttonID, flowResponseData); err != nil {
			a.Log.Error("Chat graph runner failed", "error", err, "session", session.ID, "flow", flow.ID)
			return fmt.Errorf("run active chatbot flow: %w", err)
		}
		return nil
	}

	// Try to match flow trigger keywords first (before greeting to avoid duplicate messages)
	if flow := a.matchFlowTrigger(account.OrganizationID, messageText); flow != nil {
		if flow.Graph == nil {
			a.Log.Error("Triggered chatbot flow has no v2 graph; ignoring", "flow", flow.ID)
			return nil
		}
		session.CurrentFlowID = &flow.ID
		session.CurrentStep = ""
		session.StepRetries = 0
		session.SessionData = models.JSONB{
			"_flow_id":   flow.ID.String(),
			"_flow_name": flow.Name,
		}
		if err := a.runChatGraph(account, contact, session, flow, messageText, buttonID, flowResponseData); err != nil {
			a.Log.Error("Chat graph runner failed at flow start", "error", err, "session", session.ID, "flow", flow.ID)
			return fmt.Errorf("start chatbot flow: %w", err)
		}
		return nil
	}

	// An opted-in native fallback uses the same constrained booking lane as a
	// graph AI node, including the first message of a newly acquired session.
	if handled, _, bookingErr := a.processNativeAIBooking(account, contact, session, settings, messageText); handled {
		return bookingErr
	} else if bookingErr != nil {
		return bookingErr
	}

	// Send greeting message for new sessions (only if no flow was triggered)
	if isNewSession && settings.DefaultResponse != "" {
		a.Log.Info("New session - sending greeting message", "contact", contact.PhoneNumber)
		if len(settings.GreetingButtons) > 0 {
			greetingButtons := make([]map[string]any, 0)
			for _, btn := range settings.GreetingButtons {
				if btnMap, ok := btn.(map[string]any); ok {
					greetingButtons = append(greetingButtons, btnMap)
				}
			}
			if len(greetingButtons) > 0 {
				if err := a.sendAndSaveInteractiveButtons(account, contact, settings.DefaultResponse, greetingButtons); err != nil {
					a.Log.Error("Failed to send greeting buttons", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send chatbot greeting buttons: %w", err)
				}
			} else {
				if err := a.sendAndSaveTextMessage(account, contact, settings.DefaultResponse); err != nil {
					a.Log.Error("Failed to send greeting message", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send chatbot greeting: %w", err)
				}
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, settings.DefaultResponse); err != nil {
				a.Log.Error("Failed to send greeting message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send chatbot greeting: %w", err)
			}
		}
		a.logSessionMessage(session.ID, models.DirectionOutgoing, settings.DefaultResponse, "greeting")
		return nil // After greeting, don't process further for new sessions
	}

	// Handle non-transfer keyword matches (transfer was already handled above)
	if keywordMatched && keywordResponse.ResponseType != models.ResponseTypeTransfer {
		a.Log.Info("Keyword rule matched", "response_type", keywordResponse.ResponseType, "response", keywordResponse.Body)

		// Handle regular text response
		if len(keywordResponse.Buttons) > 0 {
			if err := a.sendAndSaveInteractiveButtons(account, contact, keywordResponse.Body, keywordResponse.Buttons); err != nil {
				a.Log.Error("Failed to send interactive buttons", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send keyword response buttons: %w", err)
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, keywordResponse.Body); err != nil {
				a.Log.Error("Failed to send text message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send keyword response: %w", err)
			}
		}
		// Log outgoing message
		a.logSessionMessage(session.ID, models.DirectionOutgoing, keywordResponse.Body, "keyword_response")
		return nil
	}

	// If no keyword matched, try AI response if enabled
	if settings.AI.Enabled && settings.AI.Provider != "" && settings.AI.APIKey != "" {
		a.Log.Info("Attempting AI response", "provider", settings.AI.Provider, "model", settings.AI.Model)
		aiResult, actionErr := a.runInboundContinuationCapturedAction(
			"legacy_ai_generate",
			func() models.JSONB {
				aiResponse, generationErr := a.generateAIResponse(
					settings,
					session,
					messageText,
				)
				result := models.JSONB{"outcome": "empty"}
				switch {
				case generationErr != nil:
					result["outcome"] = "error"
					a.Log.Error("AI response failed", "error", generationErr, "provider", settings.AI.Provider, "model", settings.AI.Model)
				case aiResponse != "":
					result["outcome"] = "generated"
					// Store only the user-facing answer needed for recovery;
					// prompts, API keys, and request payloads are excluded.
					result["answer"] = aiResponse
				}
				return result
			},
		)
		if actionErr != nil {
			return fmt.Errorf("generate durable AI response: %w", actionErr)
		}
		aiResponse, _ := aiResult["answer"].(string)
		aiOutcome, _ := aiResult["outcome"].(string)
		if aiOutcome == "error" {
			a.Log.Error(
				"AI response failed",
				"provider", settings.AI.Provider,
				"model", settings.AI.Model,
			)
			// Fall through to default response
		} else if aiResponse != "" {
			a.Log.Info("AI response generated successfully", "response_length", len(aiResponse))
			if err := a.sendAndSaveTextMessage(account, contact, aiResponse); err != nil {
				a.Log.Error("Failed to send AI response", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send AI response: %w", err)
			}
			a.logSessionMessage(session.ID, models.DirectionOutgoing, aiResponse, "ai_response")
			return nil
		} else {
			a.Log.Warn("AI returned empty response")
		}
	} else {
		a.Log.Info("AI not configured", "ai_enabled", settings.AI.Enabled, "has_provider", settings.AI.Provider != "", "has_api_key", settings.AI.APIKey != "")
	}

	// If no AI response or AI not enabled, send fallback message (for existing sessions)
	// Greeting is already sent for new sessions above
	if settings.FallbackMessage != "" && !isNewSession {
		a.Log.Info("Sending fallback message", "response", settings.FallbackMessage)
		if len(settings.FallbackButtons) > 0 {
			fallbackButtons := make([]map[string]any, 0)
			for _, btn := range settings.FallbackButtons {
				if btnMap, ok := btn.(map[string]any); ok {
					fallbackButtons = append(fallbackButtons, btnMap)
				}
			}
			if len(fallbackButtons) > 0 {
				if err := a.sendAndSaveInteractiveButtons(account, contact, settings.FallbackMessage, fallbackButtons); err != nil {
					a.Log.Error("Failed to send fallback buttons", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send fallback buttons: %w", err)
				}
			} else {
				if err := a.sendAndSaveTextMessage(account, contact, settings.FallbackMessage); err != nil {
					a.Log.Error("Failed to send fallback message", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send fallback response: %w", err)
				}
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, settings.FallbackMessage); err != nil {
				a.Log.Error("Failed to send fallback message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send fallback response: %w", err)
			}
		}
		a.logSessionMessage(session.ID, models.DirectionOutgoing, settings.FallbackMessage, "fallback_response")
	} else if !isNewSession {
		a.Log.Info("No fallback message configured for existing session")
	}
	return nil
}

func incomingMessageAutomaticAISuppressed(message *models.Message) bool {
	if message == nil || message.Direction != models.DirectionIncoming {
		return false
	}
	suppressed, _ := message.Metadata[incomingAutomaticAISuppressedKey].(bool)
	return suppressed
}

// KeywordResponse holds the response content and optional buttons
type KeywordResponse struct {
	Body         string
	Buttons      []map[string]any
	ResponseType models.ResponseType // text, transfer
}

// matchKeywordRules checks if the message matches any keyword rules
func (a *App) matchKeywordRules(orgID uuid.UUID, accountName, messageText string) (*KeywordResponse, bool) {
	// Use cached keyword rules (includes both account-specific and global rules)
	rules, err := a.getKeywordRulesCached(orgID, accountName)
	if err != nil {
		a.Log.Error("Failed to fetch keyword rules", "error", err)
		return nil, false
	}

	messageLower := strings.ToLower(messageText)

	for _, rule := range rules {
		for _, keyword := range rule.Keywords {
			keywordLower := strings.ToLower(keyword)
			matched := false

			switch rule.MatchType {
			case models.MatchTypeExact:
				if rule.CaseSensitive {
					matched = messageText == keyword
				} else {
					matched = messageLower == keywordLower
				}
			case models.MatchTypeContains:
				if rule.CaseSensitive {
					matched = strings.Contains(messageText, keyword)
				} else {
					matched = strings.Contains(messageLower, keywordLower)
				}
			case models.MatchTypeStartsWith:
				if rule.CaseSensitive {
					matched = strings.HasPrefix(messageText, keyword)
				} else {
					matched = strings.HasPrefix(messageLower, keywordLower)
				}
			case models.MatchTypeRegex:
				re, err := regexp.Compile(keyword)
				if err == nil {
					matched = re.MatchString(messageText)
				}
			default:
				// Default to contains
				matched = strings.Contains(messageLower, keywordLower)
			}

			if matched {
				response := &KeywordResponse{
					ResponseType: rule.ResponseType,
				}

				// For transfer type, use body as the transfer message
				if rule.ResponseType == models.ResponseTypeTransfer {
					if body, ok := rule.ResponseContent["body"].(string); ok {
						response.Body = body
					}
					return response, true
				}

				// Get response body
				if body, ok := rule.ResponseContent["body"].(string); ok {
					response.Body = body
				}

				// Get buttons if present
				if buttons, ok := rule.ResponseContent["buttons"].([]any); ok && len(buttons) > 0 {
					response.Buttons = make([]map[string]any, 0, len(buttons))
					for _, btn := range buttons {
						if btnMap, ok := btn.(map[string]any); ok {
							response.Buttons = append(response.Buttons, btnMap)
						}
					}
				}

				if response.Body != "" {
					return response, true
				}
			}
		}
	}

	return nil, false
}

// sendAndSaveTextMessage sends a text message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveTextMessage(account *models.WhatsAppAccount, contact *models.Contact, message string) error {
	return a.runInboundContinuationSend(
		"text",
		map[string]any{
			"contact_id": contact.ID,
			"content":    message,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account: account,
					Contact: contact,
					Type:    models.MessageTypeText,
					Content: message,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// sendAndSaveInteractiveButtons sends an interactive button message and saves it to the database.
// Buttons with type "url" are automatically separated and sent as CTA URL messages,
// since WhatsApp doesn't allow mixing reply buttons and URL buttons in the same message.
func (a *App) sendAndSaveInteractiveButtons(account *models.WhatsAppAccount, contact *models.Contact, bodyText string, buttons []map[string]any) error {
	// Separate reply buttons from CTA buttons (url / phone)
	replyButtons := make([]map[string]any, 0, len(buttons))
	ctaButtons := make([]map[string]any, 0)
	for _, btn := range buttons {
		btnType, _ := btn["type"].(string)
		switch btnType {
		case "url":
			ctaButtons = append(ctaButtons, btn)
		case "phone":
			// Convert phone button to CTA URL with tel: scheme
			phoneNumber, _ := btn["phone_number"].(string)
			if phoneNumber != "" {
				ctaButtons = append(ctaButtons, map[string]any{
					"title": btn["title"],
					"url":   "tel:" + phoneNumber,
				})
			}
		default:
			replyButtons = append(replyButtons, btn)
		}
	}

	// WhatsApp doesn't allow mixing reply and CTA buttons.
	// If both exist (legacy configs), ignore CTA buttons.
	if len(replyButtons) > 0 && len(ctaButtons) > 0 {
		ctaButtons = nil
	}

	// Send reply buttons (with the body text)
	if len(replyButtons) > 0 {
		waButtons := make([]whatsapp.Button, 0, len(replyButtons))
		for i, btn := range replyButtons {
			if i >= 10 {
				break
			}
			buttonID, _ := btn["id"].(string)
			buttonTitle, _ := btn["title"].(string)
			if buttonID == "" {
				buttonID = fmt.Sprintf("btn_%d", i+1)
			}
			if buttonTitle == "" {
				continue
			}
			waButtons = append(waButtons, whatsapp.Button{
				ID:    buttonID,
				Title: buttonTitle,
			})
		}

		if len(waButtons) > 0 {
			interactiveType := "button"
			if len(waButtons) > 3 {
				interactiveType = "list"
			}
			if err := a.runInboundContinuationSend(
				"interactive_buttons",
				map[string]any{
					"contact_id":       contact.ID,
					"interactive_type": interactiveType,
					"body":             bodyText,
					"buttons":          waButtons,
				},
				func() (*models.Message, error) {
					return a.SendOutgoingMessage(
						context.Background(),
						OutgoingMessageRequest{
							Account:         account,
							Contact:         contact,
							Type:            models.MessageTypeInteractive,
							InteractiveType: interactiveType,
							BodyText:        bodyText,
							Buttons:         waButtons,
						},
						ChatbotSendOptions(),
					)
				},
			); err != nil {
				return err
			}
		}
	}

	// Send CTA-only buttons (no reply buttons mixed in)
	// WhatsApp allows max 2 CTA buttons, each sent as a separate cta_url message.
	if len(ctaButtons) > 2 {
		ctaButtons = ctaButtons[:2]
	}
	for i, ctaBtn := range ctaButtons {
		btnTitle, _ := ctaBtn["title"].(string)
		btnURL, _ := ctaBtn["url"].(string)
		if btnTitle != "" && btnURL != "" {
			// First CTA button carries the body text
			ctaBody := bodyText
			if i > 0 {
				ctaBody = btnTitle
			}
			if err := a.sendAndSaveCTAURLButton(account, contact, ctaBody, btnTitle, btnURL); err != nil {
				return err
			}
		}
	}

	// No buttons at all — fall back to text
	if len(replyButtons) == 0 && len(ctaButtons) == 0 {
		return a.sendAndSaveTextMessage(account, contact, bodyText)
	}

	return nil
}

// sendAndSaveCTAURLButton sends a CTA URL button message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveCTAURLButton(account *models.WhatsAppAccount, contact *models.Contact, bodyText, buttonText, url string) error {
	return a.runInboundContinuationSend(
		"cta_url",
		map[string]any{
			"contact_id": contact.ID,
			"body":       bodyText,
			"button":     buttonText,
			"url":        url,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account:         account,
					Contact:         contact,
					Type:            models.MessageTypeInteractive,
					InteractiveType: "cta_url",
					BodyText:        bodyText,
					ButtonText:      buttonText,
					URL:             url,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// sendAndSaveFlowMessage sends a WhatsApp Flow message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveFlowMessage(account *models.WhatsAppAccount, contact *models.Contact, flowID, headerText, bodyText, ctaText, flowToken, firstScreen string) error {
	return a.runInboundContinuationSend(
		"whatsapp_flow",
		map[string]any{
			"contact_id":   contact.ID,
			"flow_id":      flowID,
			"header":       headerText,
			"body":         bodyText,
			"cta":          ctaText,
			"first_screen": firstScreen,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account:         account,
					Contact:         contact,
					Type:            models.MessageTypeFlow,
					FlowID:          flowID,
					FlowHeader:      headerText,
					BodyText:        bodyText,
					FlowCTA:         ctaText,
					FlowToken:       flowToken,
					FlowFirstScreen: firstScreen,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// getOrCreateSession finds an active session or creates a new one
// Returns the session and a boolean indicating if it's a new session
func (a *App) getOrCreateSession(
	orgID, contactID uuid.UUID,
	accountName, phoneNumber string,
	timeoutMins int,
) (*models.ChatbotSession, bool, error) {
	if timeoutMins <= 0 {
		timeoutMins = 30
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	timeout := now.Add(-time.Duration(timeoutMins) * time.Minute)
	var result models.ChatbotSession
	isNew := false

	write := func(tx *gorm.DB) error {
		// Admission and every policy writer take the organization fence before
		// contact/session locks. Recheck after that fence: the earlier transfer
		// lookup is only an optimization, not permission to start another session.
		if err := database.LockOrganizationPolicyScope(tx, orgID); err != nil {
			return err
		}
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(tx, orgID, contactID)
		if err != nil {
			return err
		}
		policy, err := database.EvaluateContactAutomaticReplyPolicy(tx, orgID, canonical.ID)
		if err != nil {
			return err
		}
		if !policy.Allowed {
			return &inboundContinuationPolicyStop{Reason: policy.Reason}
		}

		var activeSessions []models.ChatbotSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
				orgID,
				canonical.ID,
				accountName,
				models.SessionStatusActive,
			).
			Order("last_activity_at DESC, created_at DESC, id DESC").
			Find(&activeSessions).Error; err != nil {
			return err
		}

		chosen := -1
		staleIDs := make([]uuid.UUID, 0, len(activeSessions))
		duplicateLiveIDs := make([]uuid.UUID, 0, len(activeSessions))
		for i := range activeSessions {
			if activeSessions[i].LastActivityAt.After(timeout) {
				if chosen == -1 {
					chosen = i
				} else {
					duplicateLiveIDs = append(duplicateLiveIDs, activeSessions[i].ID)
				}
				continue
			}
			staleIDs = append(staleIDs, activeSessions[i].ID)
		}

		if len(staleIDs) > 0 {
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id IN ? AND status = ?", staleIDs, models.SessionStatusActive).
				Updates(map[string]any{
					"status":       models.SessionStatusTimeout,
					"completed_at": now,
				}).Error; err != nil {
				return err
			}
		}
		if len(duplicateLiveIDs) > 0 {
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id IN ? AND status = ?", duplicateLiveIDs, models.SessionStatusActive).
				Updates(map[string]any{
					"status":       models.SessionStatusCancelled,
					"completed_at": now,
				}).Error; err != nil {
				return err
			}
		}

		if chosen >= 0 {
			result = activeSessions[chosen]
			// PostgreSQL stores microsecond precision. Make each acquisition a
			// distinct generation even when two messages arrive in one clock tick.
			now = now.UTC().Truncate(time.Microsecond)
			if !now.After(result.LastActivityAt) {
				now = result.LastActivityAt.Add(time.Microsecond)
			}
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id = ? AND status = ?", result.ID, models.SessionStatusActive).
				Update("last_activity_at", now).Error; err != nil {
				return err
			}
			result.LastActivityAt = now
			isNew = false
			return nil
		}

		canonicalPhone := canonical.PhoneNumber
		if canonicalPhone == "" {
			canonicalPhone = phoneNumber
		}
		result = models.ChatbotSession{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgID,
			ContactID:       canonical.ID,
			WhatsAppAccount: accountName,
			PhoneNumber:     canonicalPhone,
			Status:          models.SessionStatusActive,
			SessionData:     models.JSONB{},
			StartedAt:       now,
			LastActivityAt:  now,
		}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		isNew = true
		return nil
	}
	db := a.DB
	if a.inboundContinuation != nil {
		// Release the canonical Contact/session locks before any subsequent
		// physical AI provider attempt. The provider fence may open its own
		// independently committed dispatch transaction through the root pool.
		db = a.rootApp().DB
	}
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		// The policy query after a contended organization lock must see the
		// writer's commit even if the pool defaults to REPEATABLE READ.
		err = database.WithTenantReadCommitted(db, orgID, func(tx *gorm.DB) error {
			if err := requireOrdinaryTenantOrganization(tx, orgID); err != nil {
				return err
			}
			return write(tx)
		})
		if !isRetryableCanonicalContactWrite(err) {
			break
		}
		if attempt+1 < canonicalContactWriteAttempts {
			if waitErr := waitForCanonicalContactWriteRetry(db.Statement.Context, attempt); waitErr != nil {
				err = waitErr
				break
			}
		}
	}
	if err != nil {
		return nil, false, err
	}
	return &result, isNew, nil
}

// logSessionMessage logs a message to the chatbot session
func (a *App) logSessionMessage(sessionID uuid.UUID, direction models.Direction, message, stepName string) {
	msg := models.ChatbotSessionMessage{
		BaseModel: models.BaseModel{ID: uuid.New()},
		SessionID: sessionID,
		Direction: direction,
		Message:   message,
		StepName:  stepName,
	}
	if err := a.DB.Create(&msg).Error; err != nil {
		a.Log.Error("Failed to log session message", "error", err)
	}
}

// matchFlowTrigger checks if the message triggers any flow
func (a *App) matchFlowTrigger(orgID uuid.UUID, messageText string) *models.ChatbotFlow {
	// Use cached flows (includes steps)
	flows, err := a.getChatbotFlowsCached(orgID)
	if err != nil {
		a.Log.Error("Failed to fetch chatbot flows", "error", err)
		return nil
	}

	messageLower := strings.ToLower(messageText)

	for _, flow := range flows {
		for _, keyword := range flow.TriggerKeywords {
			if strings.Contains(messageLower, strings.ToLower(keyword)) {
				return &flow
			}
		}
	}
	return nil
}

// startFlow initiates a chatbot flow for a user
func (a *App) exitFlow(session *models.ChatbotSession) {
	query, err := a.activeChatSessionScope(session)
	if err != nil {
		return
	}
	now := time.Now()
	result := query.Updates(map[string]any{
		"current_step": "",
		"step_retries": 0,
		"status":       models.SessionStatusCompleted,
		"completed_at": now,
	})
	if result.Error != nil || result.RowsAffected != 1 {
		// A stale missing-flow fallback has no authority to overwrite a Pause
		// cancellation or clear tracking belonging to a newer session owner.
		return
	}

	// Clear chatbot tracking so SLA doesn't fire after flow exit
	a.ClearContactChatbotTracking(session.ContactID)
}

// closeSession ends the chatbot session and clears contact tracking
// It takes the full flow to find next steps when skipping
// executeConfiguredAPI builds and executes an HTTP request from a chatbot API config.
// replaceVar is called to substitute variables in the URL, body, and header values.
// Returns the response body and status code.
func (a *App) executeConfiguredAPI(apiConfig models.JSONB, replaceVar func(string) string) ([]byte, int, error) {
	requestClient, err := a.chatbotRequestClient()
	if err != nil {
		return nil, 0, err
	}
	apiURL, ok := apiConfig["url"].(string)
	if !ok || apiURL == "" {
		return nil, 0, fmt.Errorf("API URL is required")
	}
	if replaceVar == nil {
		replaceVar = func(value string) string { return value }
	}
	apiURL = replaceVar(apiURL)
	if err := validateWebhookRuntimeURL(apiURL); err != nil {
		return nil, 0, errors.New("configured API URL is not allowed")
	}
	logURL := redactURLForLog(apiURL)

	method, err := configuredChatbotMethod(map[string]any(apiConfig))
	if err != nil {
		return nil, 0, errors.New("configured API method is not allowed")
	}

	var bodyReader io.Reader
	if bodyTemplate, ok := apiConfig["body"].(string); ok && bodyTemplate != "" {
		bodyReader = strings.NewReader(replaceVar(bodyTemplate))
	}

	req, err := http.NewRequest(method, apiURL, bodyReader)
	if err != nil {
		return nil, 0, errors.New("failed to create configured API request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resolvedHeaders, err := a.resolveChatbotHeaders(apiConfig["headers"])
	if err != nil {
		return nil, 0, fmt.Errorf("configured API headers are unavailable: %w", err)
	}
	seenHeaderNames := make(map[string]struct{}, len(resolvedHeaders))
	for key, value := range resolvedHeaders {
		renderedValue := replaceVar(value)
		if !validChatbotHeaderName(key) || !validChatbotHeaderValue(renderedValue) {
			return nil, 0, errors.New("configured API headers are invalid")
		}
		normalizedName := strings.ToLower(key)
		if _, duplicate := seenHeaderNames[normalizedName]; duplicate {
			return nil, 0, errors.New("configured API headers are invalid")
		}
		seenHeaderNames[normalizedName] = struct{}{}
		req.Header.Set(key, renderedValue)
	}

	a.Log.Info("Executing configured API request", "method", method, "url", logURL)

	resp, err := requestClient.Do(req)
	if err != nil {
		// net/http errors may embed the full tenant-authored URL. Do not log or
		// return them because paths and query strings can carry credentials.
		a.Log.Error("Configured API request failed", "method", method, "url", logURL)
		return nil, 0, errors.New("configured API request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	limitReader := io.LimitReader(resp.Body, 1024*1024)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		a.Log.Error("Failed to read configured API response", "method", method, "url", logURL, "status_code", resp.StatusCode)
		return nil, 0, errors.New("failed to read configured API response")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.Log.Warn(
			"Configured API request returned non-2xx",
			"method", method,
			"url", logURL,
			"status_code", resp.StatusCode,
		)
	} else {
		a.Log.Info(
			"Configured API request completed",
			"method", method,
			"url", logURL,
			"status_code", resp.StatusCode,
			"response_bytes", len(body),
		)
	}

	return body, resp.StatusCode, nil
}

type ApiResponse struct {
	Message      string
	Buttons      []map[string]any
	MappedData   map[string]any // Data extracted via response_mapping
	ResponseData map[string]any // Full API response data
}

// fetchApiResponse fetches a response from an external API, supporting message + buttons
// and response_mapping for storing API data in session variables.
//
// Mirrors fetchAPIContext in seeding implicit variables (phone_number) so flow-step
// API templates can interpolate {{phone_number}} just like AI-context API templates.
func (a *App) generateAIResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string) (string, error) {
	// Build context from AIContext entries
	contextData := a.buildAIContext(settings.OrganizationID, session, userMessage)

	switch settings.AI.Provider {
	case models.AIProviderOpenAI:
		return a.generateOpenAIResponse(settings, session, userMessage, contextData)
	case models.AIProviderAnthropic:
		return a.generateAnthropicResponse(settings, session, userMessage, contextData)
	case models.AIProviderGoogle:
		return a.generateGoogleResponse(settings, session, userMessage, contextData)
	case models.AIProviderQwen:
		return a.generateQwenResponse(settings, session, userMessage, contextData)
	default:
		return "", fmt.Errorf("unsupported AI provider: %s", settings.AI.Provider)
	}
}

// buildAIContext fetches and combines all AI context data
func (a *App) buildAIContext(orgID uuid.UUID, session *models.ChatbotSession, userMessage string) string {
	// Get WhatsApp account for cache key
	whatsAppAccount := ""
	if session != nil {
		whatsAppAccount = session.WhatsAppAccount
	}

	// Use cached AI contexts
	contexts, err := a.getAIContextsCached(orgID, whatsAppAccount)
	if err != nil || len(contexts) == 0 {
		return ""
	}

	var contextParts []string

	for _, ctx := range contexts {
		var content string

		switch ctx.ContextType {
		case models.ContextTypeStatic:
			content = ctx.StaticContent

		case models.ContextTypeAPI:
			// Start with static content/prompt if provided
			content = ctx.StaticContent

			// Fetch data from external API and append
			apiContent, err := a.fetchAPIContext(ctx.ApiConfig, session, userMessage)
			if err != nil {
				a.Log.Error("Failed to fetch API context", "context_name", ctx.Name, "error", err)
				// Still use static content if API fails
			} else if apiContent != "" {
				if content != "" {
					content = content + "\n\nData:\n" + apiContent
				} else {
					content = apiContent
				}
			}
		}

		if content != "" {
			contextParts = append(contextParts, fmt.Sprintf("### %s\n%s", ctx.Name, content))
		}
	}

	if len(contextParts) == 0 {
		return ""
	}

	return "## Context Information\n\n" + strings.Join(contextParts, "\n\n")
}

// fetchAPIContext fetches context data from an external API
func (a *App) fetchAPIContext(apiConfig models.JSONB, session *models.ChatbotSession, userMessage string) (string, error) {
	if apiConfig == nil {
		return "", fmt.Errorf("API config is empty")
	}

	// Build session data for variable replacement
	sessionData := models.JSONB{}
	if session != nil {
		sessionData = session.SessionData
		if sessionData == nil {
			sessionData = models.JSONB{}
		}
		sessionData["phone_number"] = session.PhoneNumber
		sessionData["user_message"] = userMessage
	}

	replaceVar := func(s string) string { return processTemplate(s, sessionData) }
	respBody, statusCode, err := a.executeConfiguredAPI(apiConfig, replaceVar)
	if err != nil {
		return "", err
	}

	if statusCode < 200 || statusCode >= 300 {
		return "", fmt.Errorf("API returned status %d", statusCode)
	}

	// Check for response_path to extract specific field
	if responsePath, ok := apiConfig["response_path"].(string); ok && responsePath != "" {
		var jsonResp map[string]any
		if err := json.Unmarshal(respBody, &jsonResp); err == nil {
			if value := getNestedValue(jsonResp, responsePath); value != nil {
				return formatValue(value), nil
			}
		}
	}

	return string(respBody), nil
}

// generateOpenAICompatibleResponse calls an OpenAI-compatible chat completions
// endpoint. Qwen uses this protocol through Alibaba Cloud Model Studio.
func (a *App) generateOpenAICompatibleResponse(providerName, url string, settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string, extraPayload map[string]any) (string, error) {
	// Build messages array
	messages := []map[string]string{}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system prompt if configured
	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, map[string]string{
				"role":    role,
				"content": msg.Message,
			})
		}
	}

	// Add current user message
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userMessage,
	})

	payload := map[string]any{
		"model":      settings.AI.Model,
		"messages":   messages,
		"max_tokens": settings.AI.MaxTokens,
	}
	for key, value := range extraPayload {
		payload[key] = value
	}

	if settings.AI.Temperature > 0 {
		payload["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+settings.AI.APIKey)

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	const maxAIResponseBytes = 4 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAIResponseBytes+1))
	if readErr != nil {
		return "", fmt.Errorf("failed to read %s response: %w", providerName, readErr)
	}
	if len(body) > maxAIResponseBytes {
		return "", fmt.Errorf("%s response exceeded the size limit", providerName)
	}

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		if errResp.Error.Message == "" {
			errResp.Error.Message = strings.TrimSpace(string(body))
		}
		return "", fmt.Errorf("%s API error: %s", providerName, errResp.Error.Message)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Choices) > 0 {
		return strings.TrimSpace(result.Choices[0].Message.Content), nil
	}

	return "", fmt.Errorf("no response from %s", providerName)
}

// generateOpenAIResponse generates a response using OpenAI API.
func (a *App) generateOpenAIResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	return a.generateOpenAICompatibleResponse(
		"OpenAI",
		"https://api.openai.com/v1/chat/completions",
		settings,
		session,
		userMessage,
		contextData,
		nil,
	)
}

// generateQwenResponse generates a low-latency customer-service response using
// Alibaba Cloud Model Studio's OpenAI-compatible Qwen API. Thinking is disabled
// for routine CRM replies to keep latency and token usage predictable.
func (a *App) generateQwenResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	messages := make([]qwenapi.Message, 0, 8)
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}
	if systemPrompt != "" {
		messages = append(messages, qwenapi.Message{Role: "system", Content: systemPrompt})
	}
	if settings.AI.IncludeHistory && session != nil {
		for _, message := range a.getSessionHistory(session.ID, settings.AI.HistoryLimit) {
			role := "user"
			if message.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, qwenapi.Message{Role: role, Content: message.Message})
		}
	}
	messages = append(messages, qwenapi.Message{Role: "user", Content: userMessage})

	baseURL := qwenapi.DefaultBaseURL
	if a.Config != nil && a.Config.AI.QwenBaseURL != "" {
		baseURL = a.Config.AI.QwenBaseURL
	}
	if strings.TrimSpace(settings.AI.BaseURL) != "" {
		baseURL = settings.AI.BaseURL
	}
	return qwenapi.Generate(context.Background(), a.HTTPClient, qwenapi.Options{
		APIKey:      settings.AI.APIKey,
		BaseURL:     baseURL,
		Model:       settings.AI.Model,
		MaxTokens:   settings.AI.MaxTokens,
		Temperature: settings.AI.Temperature,
		Messages:    messages,
	})
}

// generateAnthropicResponse generates a response using Anthropic API
func (a *App) generateAnthropicResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	url := "https://api.anthropic.com/v1/messages"

	// Build messages array
	messages := []map[string]string{}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, map[string]string{
				"role":    role,
				"content": msg.Message,
			})
		}
	}

	// Add current user message
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userMessage,
	})

	payload := map[string]any{
		"model":      settings.AI.Model,
		"messages":   messages,
		"max_tokens": settings.AI.MaxTokens,
	}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system prompt if configured
	if systemPrompt != "" {
		payload["system"] = systemPrompt
	}

	if settings.AI.Temperature > 0 {
		payload["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", settings.AI.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("anthropic API error: %s", errResp.Error.Message)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	for _, content := range result.Content {
		if content.Type == "text" {
			return strings.TrimSpace(content.Text), nil
		}
	}

	return "", fmt.Errorf("no text response from Anthropic")
}

// generateGoogleResponse generates a response using Google Gemini API
func (a *App) generateGoogleResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		settings.AI.Model, settings.AI.APIKey)

	// Build contents array
	contents := []map[string]any{}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "model"
			}
			contents = append(contents, map[string]any{
				"role": role,
				"parts": []map[string]string{
					{"text": msg.Message},
				},
			})
		}
	}

	// Add current user message
	contents = append(contents, map[string]any{
		"role": "user",
		"parts": []map[string]string{
			{"text": userMessage},
		},
	})

	payload := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": settings.AI.MaxTokens,
		},
	}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system instruction if configured
	if systemPrompt != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]string{
				{"text": systemPrompt},
			},
		}
	}

	if settings.AI.Temperature > 0 {
		payload["generationConfig"].(map[string]any)["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("google AI API error: %s", errResp.Error.Message)
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
		return strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text), nil
	}

	return "", fmt.Errorf("no response from Google AI")
}

// getSessionHistory retrieves recent messages from the session
func (a *App) getSessionHistory(sessionID uuid.UUID, limit int) []models.ChatbotSessionMessage {
	var messages []models.ChatbotSessionMessage
	a.DB.Where("session_id = ?", sessionID).
		Order("created_at DESC").
		Limit(limit).
		Find(&messages)

	// Reverse to get chronological order
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages
}

// Reaction represents a reaction on a message
type Reaction struct {
	Emoji     string `json:"emoji"`
	FromPhone string `json:"from_phone,omitempty"` // Phone number if from contact
	FromUser  string `json:"from_user,omitempty"`  // User ID if from agent
}

// handleIncomingReaction handles incoming reaction messages from WhatsApp
func (a *App) handleIncomingReaction(account *models.WhatsAppAccount, fromPhone, messageWAMID, emoji, profileName string) {
	if a == nil || account == nil || account.OrganizationID == uuid.Nil || account.ID == uuid.Nil {
		return
	}
	a.Log.Info("Handling incoming reaction",
		"from", fromPhone,
		"message_wamid", messageWAMID,
		"emoji", emoji,
	)

	_ = profileName // A reaction may corroborate an existing contact; it never creates one.
	messageWAMID = strings.TrimSpace(messageWAMID)
	fromPhone = normalizeCoexistencePhone(fromPhone)
	if messageWAMID == "" || fromPhone == "" {
		a.Log.Warn("Reaction identity is incomplete")
		return
	}

	root := a.rootApp()
	accountCopy := *account
	var updatedMessageID, contactID uuid.UUID
	var newReactions []Reaction
	err := root.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		// Find the exact WhatsApp-owner candidate without trusting the mutable
		// account display name. The resolver below proves the stable account,
		// canonical contact, direction, and durable message provenance.
		loadOwnedTarget := func(wamid string, suffixOnly bool) ([]models.Message, error) {
			var candidates []models.Message
			query := scoped.DB.Unscoped().Where(`organization_id = ? AND (
					inbox_conversation_id IS NULL
					OR COALESCE(metadata, '{}'::jsonb) @> ?::jsonb
				)`, account.OrganizationID, database.WhatsAppWAMIDOwnerMetadataJSON)
			if suffixOnly {
				query = query.Where("RIGHT(whats_app_message_id, LENGTH(?)) = ?", wamid, wamid)
			} else {
				query = query.Where("whats_app_message_id = ?", wamid)
			}
			err := query.Order("id").Limit(2).Find(&candidates).Error
			return candidates, err
		}

		candidates, err := loadOwnedTarget(messageWAMID, false)
		if err != nil {
			return err
		}
		if len(candidates) > 1 {
			return errors.New("ambiguous exact WhatsApp reaction target")
		}
		if len(candidates) == 0 {
			idx := strings.Index(messageWAMID, "FQIA")
			suffixStart := idx + 8
			if idx < 0 || suffixStart >= len(messageWAMID) {
				return gorm.ErrRecordNotFound
			}
			suffix := messageWAMID[suffixStart:]
			candidates, err = loadOwnedTarget(suffix, true)
			if err != nil {
				return err
			}
			if len(candidates) != 1 {
				if len(candidates) > 1 {
					return errors.New("ambiguous WhatsApp reaction suffix")
				}
				return gorm.ErrRecordNotFound
			}
		}

		candidate := candidates[0]
		if err := database.LockWhatsAppWAMIDScopes(scoped.DB, account.OrganizationID, candidate.WhatsAppMessageID); err != nil {
			return err
		}
		if err := scoped.prepareWhatsAppMessageAuthority(&accountCopy); err != nil {
			return err
		}
		resolved, err := scoped.resolveWhatsAppMessage(&accountCopy, whatsAppMessageLookup{
			WAMID: candidate.WhatsAppMessageID, MessageID: candidate.ID,
			Identity: coexistenceContactIdentity{Phone: fromPhone},
			Lock:     true, RepairProjection: true,
		})
		if errors.Is(err, errWhatsAppMessageOwnerDeleted) {
			return nil
		}
		if err != nil {
			return err
		}

		metadata := cloneMessageMetadata(resolved.Message.Metadata)
		var reactions []Reaction
		if reactionsArray, ok := metadata["reactions"].([]any); ok {
			for _, raw := range reactionsArray {
				if reactionMap, ok := raw.(map[string]any); ok {
					reactionEmoji, _ := reactionMap["emoji"].(string)
					reactions = append(reactions, Reaction{
						Emoji: reactionEmoji, FromPhone: getStringFromMap(reactionMap, "from_phone"),
						FromUser: getStringFromMap(reactionMap, "from_user"),
					})
				}
			}
		}
		newReactions = make([]Reaction, 0, len(reactions)+1)
		for _, reaction := range reactions {
			if normalizeCoexistencePhone(reaction.FromPhone) != fromPhone {
				newReactions = append(newReactions, reaction)
			}
		}
		if emoji != "" {
			newReactions = append(newReactions, Reaction{Emoji: emoji, FromPhone: fromPhone})
		}
		metadata["reactions"] = newReactions
		if err := scoped.DB.Model(&models.Message{}).
			Where("organization_id = ? AND id = ?", account.OrganizationID, resolved.Message.ID).
			Update("metadata", metadata).Error; err != nil {
			return err
		}
		updatedMessageID, contactID = resolved.Message.ID, resolved.Contact.ID
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		a.Log.Warn("Message not found for reaction", "wamid", messageWAMID)
		return
	}
	if err != nil {
		a.Log.Error("Failed to update message reaction", "error", err, "wamid", messageWAMID)
		return
	}
	if updatedMessageID == uuid.Nil {
		return
	}
	a.Log.Info("Updated message reaction", "message_id", updatedMessageID, "reactions_count", len(newReactions))
	a.broadcastReactionUpdate(account.OrganizationID, updatedMessageID, contactID, newReactions)
}

// Helper function to safely get string from map
func getStringFromMap(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ExtractedMessage holds the derived content fields of a message.
type ExtractedMessage struct {
	Text             string
	Type             string // may differ from msg.Type, e.g. "button_reply"
	Media            *MediaInfo
	ButtonID         string         // used by chatbot routing only
	FlowResponseData map[string]any // used by chatbot routing only
}

// extractMessageContentForPersistence normalizes only data already present in
// Meta's payload. It must remain free of provider and object-store calls.
func (a *App) extractMessageContentForPersistence(msg IncomingTextMessage) ExtractedMessage {
	extracted := ExtractedMessage{
		Type: msg.Type,
	}

	if msg.Type == "text" && msg.Text != nil {
		extracted.Text = msg.Text.Body
	} else if msg.Type == "button" && msg.Button != nil {
		// Template quick_reply button click — WhatsApp sends type "button"
		extracted.Text = msg.Button.Text
		extracted.ButtonID = msg.Button.Payload
		extracted.Type = "button_reply"
	} else if msg.Type == "interactive" && msg.Interactive != nil {
		// Handle button reply
		if msg.Interactive.ButtonReply != nil {
			extracted.Text = msg.Interactive.ButtonReply.Title
			extracted.ButtonID = msg.Interactive.ButtonReply.ID
			extracted.Type = "button_reply"
		}
		// Handle list reply
		if msg.Interactive.ListReply != nil {
			extracted.Text = msg.Interactive.ListReply.Title
			extracted.ButtonID = msg.Interactive.ListReply.ID
			extracted.Type = "button_reply"
		}
		// Handle WhatsApp Flow reply (nfm_reply)
		if msg.Interactive.NFMReply != nil {
			extracted.Text = msg.Interactive.NFMReply.Body
			extracted.Type = "nfm_reply"
			// Parse the response JSON to extract form data
			if msg.Interactive.NFMReply.ResponseJSON != "" {
				var responseData map[string]any
				if err := json.Unmarshal([]byte(msg.Interactive.NFMReply.ResponseJSON), &responseData); err != nil {
					a.Log.Error("Failed to parse flow response JSON", "error", err, "response_json", msg.Interactive.NFMReply.ResponseJSON)
				} else {
					extracted.FlowResponseData = responseData
					a.Log.Info("Parsed WhatsApp Flow response", "data", extracted.FlowResponseData)
				}
			}
		}
	} else if msg.Type == "image" && msg.Image != nil {
		// Handle image message
		extracted.Text = msg.Image.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Image.MimeType,
		}
	} else if msg.Type == "document" && msg.Document != nil {
		// Handle document message
		extracted.Text = msg.Document.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Document.MimeType,
			MediaFilename: msg.Document.Filename,
		}
	} else if msg.Type == "video" && msg.Video != nil {
		// Handle video message
		extracted.Text = msg.Video.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Video.MimeType,
		}
	} else if msg.Type == "audio" && msg.Audio != nil {
		// Handle audio message
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Audio.MimeType,
		}
	} else if msg.Type == "sticker" && msg.Sticker != nil {
		// Handle sticker message (treat like image)
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Sticker.MimeType,
		}
	} else if msg.Type == "location" && msg.Location != nil {
		// Handle location message - store as JSON in content
		locationData := map[string]any{
			"latitude":  msg.Location.Latitude,
			"longitude": msg.Location.Longitude,
		}
		if msg.Location.Name != "" {
			locationData["name"] = msg.Location.Name
		}
		if msg.Location.Address != "" {
			locationData["address"] = msg.Location.Address
		}
		if jsonBytes, err := json.Marshal(locationData); err == nil {
			extracted.Text = string(jsonBytes)
		}
	} else if msg.Type == "contacts" && len(msg.Contacts) > 0 {
		// Handle contacts message - store as JSON in content
		contactsData := make([]map[string]any, 0, len(msg.Contacts))
		for _, c := range msg.Contacts {
			contactVal := map[string]any{
				"name": c.Name.FormattedName,
			}
			if len(c.Phones) > 0 {
				phones := make([]string, 0, len(c.Phones))
				for _, p := range c.Phones {
					phones = append(phones, p.Phone)
				}
				contactVal["phones"] = phones
			}
			contactsData = append(contactsData, contactVal)
		}
		if jsonBytes, err := json.Marshal(contactsData); err == nil {
			extracted.Text = string(jsonBytes)
		}
	}

	return extracted
}

// hydrateExtractedIncomingMedia performs the optional provider download after
// the payload-only representation has been normalized.
func (a *App) hydrateExtractedIncomingMedia(
	ctx context.Context,
	msg IncomingTextMessage,
	account *models.WhatsAppAccount,
	extracted *ExtractedMessage,
) {
	if account == nil || extracted == nil || extracted.Media == nil {
		return
	}

	var mediaID, mimeType, label string
	switch {
	case msg.Type == "image" && msg.Image != nil:
		mediaID, mimeType, label = msg.Image.ID, msg.Image.MimeType, "image"
	case msg.Type == "document" && msg.Document != nil:
		mediaID, mimeType, label = msg.Document.ID, msg.Document.MimeType, "document"
	case msg.Type == "video" && msg.Video != nil:
		mediaID, mimeType, label = msg.Video.ID, msg.Video.MimeType, "video"
	case msg.Type == "audio" && msg.Audio != nil:
		mediaID, mimeType, label = msg.Audio.ID, msg.Audio.MimeType, "audio"
	case msg.Type == "sticker" && msg.Sticker != nil:
		mediaID, mimeType, label = msg.Sticker.ID, msg.Sticker.MimeType, "sticker"
	default:
		return
	}
	if mediaID == "" {
		return
	}

	waAccount := a.toWhatsAppAccount(account)
	localPath, err := a.DownloadAndSaveMedia(
		ctx,
		account.OrganizationID,
		mediaID,
		mimeType,
		waAccount,
	)
	if err != nil {
		a.Log.Error(
			"Failed to hydrate incoming media",
			"error", err,
			"media_id", mediaID,
			"media_type", label,
		)
		return
	}
	extracted.Media.MediaURL = localPath
}

// MediaInfo holds media-related information for an incoming message
type MediaInfo struct {
	MediaURL      string
	MediaMimeType string
	MediaFilename string
}

var errIncomingMessageAlreadyProcessed = errors.New("incoming message already processed")

// persistIncomingMessageBeforeAck is the synchronous Meta webhook boundary for
// regular inbound messages. It returns only after the contact, normalized
// Message, customer activity, and durable outbox rows have committed. A replay
// is a successful no-op so Meta can safely stop retrying it.
func (a *App) persistIncomingMessageBeforeAck(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
) (work *persistedIncomingMessage, duplicate bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			work = nil
			duplicate = false
			err = fmt.Errorf("panic while persisting incoming message: %v", recovered)
		}
	}()

	if strings.TrimSpace(phoneNumberID) == "" {
		return nil, false, errors.New("incoming message phone number ID is required")
	}
	if strings.TrimSpace(msg.ID) == "" {
		return nil, false, errors.New("incoming WhatsApp message ID is required")
	}
	if msg.Type == "reaction" {
		return nil, false, errors.New("reaction messages use the specialized inbound path")
	}

	if a.rlsEnabled() && !a.hasTenantScope() {
		err = a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			var scopedErr error
			work, duplicate, scopedErr = scoped.persistIncomingMessageForAccount(
				phoneNumberID,
				msg,
				profileName,
				nil,
			)
			return scopedErr
		})
		return work, duplicate, err
	}

	if a.rlsEnabled() {
		return a.persistIncomingMessageForAccount(
			phoneNumberID,
			msg,
			profileName,
			nil,
		)
	}

	// RLS supplies an outer tenant transaction in production. Keep the same
	// all-or-nothing boundary when RLS is disabled by binding a scoped clone to
	// one explicit transaction.
	var afterCommit []func()
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		txApp := a.scopedApp(tx, uuid.Nil)
		var transactionErr error
		work, duplicate, transactionErr = txApp.persistIncomingMessageForAccount(
			phoneNumberID,
			msg,
			profileName,
			nil,
		)
		if transactionErr == nil {
			afterCommit = txApp.takeAfterCommit()
		}
		return transactionErr
	})
	if err == nil {
		a.runAfterCommit(afterCommit)
	}
	return work, duplicate, err
}

func (a *App) persistIncomingMessageForAccount(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
	account *models.WhatsAppAccount,
) (*persistedIncomingMessage, bool, error) {
	return a.persistIncomingMessageForAccountWithAdmission(
		phoneNumberID,
		msg,
		profileName,
		account,
		nil,
	)
}

func (a *App) persistIncomingMessageForAccountWithAdmission(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
	account *models.WhatsAppAccount,
	admission *incomingMessageAdmissionPolicy,
) (*persistedIncomingMessage, bool, error) {
	if a == nil || a.DB == nil || strings.TrimSpace(phoneNumberID) == "" || strings.TrimSpace(msg.ID) == "" {
		return nil, false, errors.New("incoming message account and WAMID are required")
	}
	if _, transactional := a.DB.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		var work *persistedIncomingMessage
		var duplicate bool
		var nested *App
		err := a.DB.Transaction(func(tx *gorm.DB) error {
			nested = a.scopedApp(tx, a.tenantOrgID)
			var err error
			work, duplicate, err = nested.persistIncomingMessageForAccountWithAdmission(
				phoneNumberID,
				msg,
				profileName,
				account,
				admission,
			)
			return err
		})
		if err == nil {
			a.adoptAfterCommit(nested)
		}
		return work, duplicate, err
	}
	// Never use the mutable/global phone cache to establish tenant authority.
	var accounts []models.WhatsAppAccount
	query := a.DB.Where("BTRIM(phone_id) = ?", strings.TrimSpace(phoneNumberID))
	if account != nil {
		query = query.Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID)
	} else if a.tenantOrgID != uuid.Nil {
		query = query.Where("organization_id = ?", a.tenantOrgID)
	}
	if err := query.Limit(2).Find(&accounts).Error; err != nil {
		return nil, false, fmt.Errorf("load incoming account: %w", err)
	}
	if len(accounts) != 1 {
		return nil, false, errors.New("incoming account is missing or ambiguous")
	}
	current := accounts[0]
	account = &current
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, msg.ID); err != nil {
		return nil, false, fmt.Errorf("lock incoming WhatsApp message admission: %w", err)
	}
	if err := database.LockOrganizationPolicyScope(
		a.DB,
		account.OrganizationID,
	); err != nil {
		return nil, false, fmt.Errorf(
			"lock incoming organization admission: %w",
			err,
		)
	}
	if account.IsSMB {
		if _, err := channelapi.EnsureLegacyMetaWhatsAppAccount(
			a.DB,
			channelapi.LegacyMetaAccountRef{
				ID:             account.ID,
				OrganizationID: account.OrganizationID,
				Name:           account.Name,
				Status:         account.Status,
			},
		); err != nil {
			return nil, false, fmt.Errorf(
				"ensure incoming legacy channel account: %w",
				err,
			)
		}
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return nil, false, err
	}
	winner, err := a.lookupWhatsAppAdmissionWinner(
		account.OrganizationID,
		msg.ID,
	)
	if err != nil {
		return nil, false, err
	}
	if winner.Kind == whatsAppAdmissionWinnerReview {
		// The reserved, contact-free review receipt is the immutable WAMID
		// winner. It is a successful provider replay with no continuation.
		return nil, true, nil
	}
	if winner.Kind == whatsAppAdmissionWinnerMessage && winner.Message != nil && winner.Message.DeletedAt.Valid {
		// A soft-deleted Message remains the immutable first WAMID owner. A
		// provider replay is successful but cannot revive the row, its contact,
		// or its continuation side effects.
		return nil, true, nil
	}

	// Resolve existing provenance before changing contact metadata or taking
	// contact locks. A reused display name cannot adopt an unproven old row.
	lookup := whatsAppMessageLookup{
		WAMID: msg.ID, Direction: models.DirectionIncoming,
		Identity: coexistenceContactIdentity{Phone: msg.From, UserID: msg.FromUserID, ParentUserID: msg.FromParentUserID},
		Lock:     true, RepairProjection: true,
	}
	if admission != nil && admission.CanonicalContactID != uuid.Nil {
		lookup.ContactID = admission.CanonicalContactID
	}
	resolved, err := a.resolveWhatsAppMessage(account, lookup)
	var contact *models.Contact
	switch {
	case err == nil:
		contact = &resolved.Contact
	case errors.Is(err, gorm.ErrRecordNotFound):
		if admission != nil && admission.CanonicalContactID != uuid.Nil {
			contact, err = contactutil.ResolveCanonicalContactForUpdate(
				a.DB,
				account.OrganizationID,
				admission.CanonicalContactID,
			)
			if err != nil {
				return nil, false, fmt.Errorf(
					"resolve admitted inbound contact: %w",
					err,
				)
			}
		} else {
			contact, _, err = a.getOrCreateInboundContact(
				account,
				msg.From,
				profileName,
				msg.FromUserID,
			)
			if err != nil {
				return nil, false, fmt.Errorf(
					"get or create inbound contact: %w",
					err,
				)
			}
		}
	default:
		return nil, false, fmt.Errorf("resolve incoming message: %w", err)
	}

	suppressAutomaticAI := admission != nil && admission.SuppressAutomaticAI
	suppressionReason := ""
	if admission != nil {
		suppressionReason = strings.TrimSpace(admission.SuppressionReason)
	}
	policy, policyErr := database.EvaluateContactAutomaticReplyPolicy(
		a.DB,
		account.OrganizationID,
		contact.ID,
	)
	switch {
	case policyErr != nil:
		suppressAutomaticAI = true
		if suppressionReason == "" {
			suppressionReason = "automatic_reply_policy_unavailable"
		}
	case !policy.Allowed:
		suppressAutomaticAI = true
		if suppressionReason == "" {
			suppressionReason = policy.Reason
		}
	}
	paused, pauseErr := a.incomingContactConversationAIIsPaused(
		account.OrganizationID,
		contact.ID,
	)
	switch {
	case pauseErr != nil:
		suppressAutomaticAI = true
		if suppressionReason == "" {
			suppressionReason = "conversation_ai_state_unavailable"
		}
	case paused:
		suppressAutomaticAI = true
		if suppressionReason == "" {
			suppressionReason = "conversation_ai_paused"
		}
	}

	extracted := a.extractMessageContentForPersistence(msg)
	replyToWAMID := ""
	if msg.Context != nil {
		replyToWAMID = msg.Context.ID
	}
	message, err := a.persistIncomingMessageWithFlow(account, contact, msg.ID, extracted.Type,
		extracted.Text, extracted.Media, replyToWAMID, extracted.FlowResponseData)
	duplicate := errors.Is(err, errIncomingMessageAlreadyProcessed)
	if err != nil && !duplicate {
		return nil, false, err
	}
	if message == nil {
		return nil, false, errors.New("incoming message did not resolve a durable winner")
	}
	if !duplicate && (suppressAutomaticAI || (admission != nil && admission.IdentityReviewHoldID != uuid.Nil)) {
		if err := applyIncomingMessageAdmissionPolicy(
			a.DB,
			account.OrganizationID,
			message,
			admission,
			suppressAutomaticAI,
			suppressionReason,
		); err != nil {
			return nil, false, err
		}
	}
	if !duplicate || admission == nil {
		if err := a.ensureInboundContinuationJob(account, message, msg, profileName); err != nil {
			return nil, false, err
		}
	}
	return &persistedIncomingMessage{
		OrganizationID: account.OrganizationID, PhoneNumberID: account.PhoneID,
		Message: msg, Account: *account, Contact: *contact, Extracted: extracted, Persisted: *message,
	}, duplicate, nil
}

// incomingContactConversationAIIsPaused projects the established legacy
// inbox pause onto native WhatsApp admission. The account-authority step has
// already locked and validated every eligible shadow account; an unexpected
// or unreadable conversation projection therefore fails closed at admission.
func (a *App) incomingContactConversationAIIsPaused(
	organizationID, contactID uuid.UUID,
) (bool, error) {
	if a == nil || a.DB == nil || organizationID == uuid.Nil || contactID == uuid.Nil {
		return false, errors.New("incoming conversation AI policy identity is incomplete")
	}
	authority, _ := a.DB.Statement.Context.Value(whatsAppMessageAuthorityKey{}).(*whatsAppMessageAuthority)
	if authority == nil || authority.pool != a.DB.Statement.ConnPool ||
		authority.account.OrganizationID != organizationID {
		return false, errors.New("incoming conversation AI policy lacks account authority")
	}
	if len(authority.channelIDs) == 0 {
		return false, nil
	}
	channelIDs := make([]uuid.UUID, 0, len(authority.channelIDs))
	for channelID := range authority.channelIDs {
		channelIDs = append(channelIDs, channelID)
	}
	var conversations []models.InboxConversation
	if err := a.DB.Select("id", "config").Where(
		"organization_id = ? AND contact_id = ? AND channel = ? AND channel_account_id IN ?",
		organizationID,
		contactID,
		models.ChannelWhatsApp,
		channelIDs,
	).Find(&conversations).Error; err != nil {
		return false, err
	}
	for i := range conversations {
		if inboxConversationAIIsPaused(conversations[i].Config) {
			return true, nil
		}
	}
	return false, nil
}

func applyIncomingMessageAdmissionPolicy(
	tx *gorm.DB,
	organizationID uuid.UUID,
	message *models.Message,
	admission *incomingMessageAdmissionPolicy,
	suppressAutomaticAI bool,
	reason string,
) error {
	if tx == nil || organizationID == uuid.Nil || message == nil ||
		message.ID == uuid.Nil || message.OrganizationID != organizationID {
		return errors.New("incoming AI suppression identity is incomplete")
	}
	metadata := cloneMessageMetadata(message.Metadata)
	if suppressAutomaticAI {
		// Suppression is monotonic per WAMID. A later Resume or identity decision
		// affects only future provider attempts and never clears this fact.
		metadata[incomingAutomaticAISuppressedKey] = true
		if strings.TrimSpace(reason) == "" {
			reason = "policy_blocked_at_admission"
		}
		metadata[incomingAutomaticAISuppressionReasonKey] = strings.TrimSpace(reason)
		if _, exists := metadata[incomingAutomaticAISuppressedAtKey]; !exists {
			metadata[incomingAutomaticAISuppressedAtKey] = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	if admission != nil && admission.IdentityReviewHoldID != uuid.Nil {
		if admission.IdentityReviewHoldID.String() == "" ||
			!whatsAppIdentityReviewRouteReasonAllowed(admission.IdentityReviewReason) ||
			(admission.IdentityReviewRouteMode != incomingIdentityReviewRouteHeldDirect &&
				admission.IdentityReviewRouteMode != incomingIdentityReviewRouteReviewedFuture) ||
			admission.IdentityReviewSelectorHash != strings.ToLower(strings.TrimSpace(admission.IdentityReviewSelectorHash)) ||
			!isSHA256Hex(admission.IdentityReviewSelectorHash) {
			return errors.New("incoming identity-review admission proof is invalid")
		}
		metadata[incomingIdentityReviewHoldIDKey] = admission.IdentityReviewHoldID.String()
		metadata[incomingIdentityReviewReasonKey] = admission.IdentityReviewReason
		metadata[incomingIdentityReviewRouteModeKey] = admission.IdentityReviewRouteMode
		metadata[incomingIdentityReviewSelectorDigestKey] = admission.IdentityReviewSelectorHash
	}
	update := tx.Model(&models.Message{}).
		Where(
			"id = ? AND organization_id = ? AND direction = ?",
			message.ID,
			organizationID,
			models.DirectionIncoming,
		).
		Update("metadata", metadata)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return errors.New("incoming AI suppression lost its WAMID winner")
	}
	message.Metadata = metadata
	return nil
}
func (a *App) hydratePersistedIncomingMedia(
	ctx context.Context,
	work *persistedIncomingMessage,
	account *models.WhatsAppAccount,
) error {
	if work == nil || work.Extracted.Media == nil {
		return nil
	}
	if account == nil || account.ID != work.Account.ID || account.OrganizationID != work.OrganizationID {
		return errors.New("incoming media account authority changed")
	}
	lookup := whatsAppMessageLookup{
		WAMID: work.Message.ID, MessageID: work.Persisted.ID, ContactID: work.Contact.ID,
		Direction: models.DirectionIncoming,
		Identity:  coexistenceContactIdentity{Phone: work.Message.From, UserID: work.Message.FromUserID, ParentUserID: work.Message.FromParentUserID},
		Lock:      true, RepairProjection: true,
	}
	var before models.Message
	var tokenGeneration [sha256.Size]byte
	currentAccount := *account
	err := a.WithCommittedTenantApp(work.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(&currentAccount); err != nil {
			return err
		}
		resolved, err := scoped.resolveWhatsAppMessage(&currentAccount, lookup)
		if err != nil {
			return err
		}
		before, work.Persisted = resolved.Message, resolved.Message
		if err := scoped.prepareWhatsAppAccountForOutbound(&currentAccount); err != nil {
			return err
		}
		authority := scoped.DB.Statement.Context.Value(whatsAppMessageAuthorityKey{}).(*whatsAppMessageAuthority)
		tokenGeneration = authority.accessTokenGeneration
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate incoming media before download: %w", err)
	}
	if before.Metadata[coexistenceMediaRevokedMetadataKey] == true {
		return nil
	}
	mediaID, _ := coexistenceMediaIdentity(work.Message)
	if mediaID == "" || before.MessageType != models.MessageType(work.Message.Type) {
		return errors.New("incoming media type was replaced before hydration")
	}
	if expected := coexistenceMediaMetadataString(before.Metadata, coexistenceMediaProviderIDMetadataKey); expected != "" && expected != mediaID {
		return errors.New("incoming media was replaced before hydration")
	}
	if before.MediaURL != "" {
		work.Extracted.Media = &MediaInfo{MediaURL: before.MediaURL, MediaMimeType: before.MediaMimeType, MediaFilename: before.MediaFilename}
		return nil
	}

	// No database authority locks span provider/object-store I/O.
	hydrated := work.Extracted
	hydratedMedia := *work.Extracted.Media
	hydrated.Media = &hydratedMedia
	a.hydrateExtractedIncomingMedia(ctx, work.Message, &currentAccount, &hydrated)
	if hydrated.Media.MediaURL == "" {
		return nil
	}
	var discarded bool
	var discardReason error
	err = a.WithCommittedTenantApp(work.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(&currentAccount); err != nil {
			return err
		}
		resolved, err := scoped.resolveWhatsAppMessage(&currentAccount, lookup)
		if err != nil {
			return err
		}
		message := resolved.Message
		work.Persisted = message
		// A discard decision commits before exact-key cleanup. No observer has
		// been given this newly generated random key; still prove that it has
		// no durable tenant reference before allowing deletion.
		discard := func(reason error) error {
			var references int64
			if err := scoped.DB.Unscoped().Model(&models.Message{}).Where(
				"organization_id = ? AND media_url = ?", work.OrganizationID, hydrated.Media.MediaURL,
			).Count(&references).Error; err != nil {
				return err
			}
			discarded, discardReason = references == 0, reason
			return nil
		}
		authority := scoped.DB.Statement.Context.Value(whatsAppMessageAuthorityKey{}).(*whatsAppMessageAuthority)
		if authority.accessTokenGeneration != tokenGeneration || currentAccount.IsSMB != account.IsSMB {
			return discard(errors.New("incoming media credentials changed during hydration"))
		}
		if err := scoped.prepareWhatsAppAccountForOutbound(&currentAccount); err != nil {
			return discard(errors.New("incoming media account became unavailable during hydration"))
		}
		if message.Metadata[coexistenceMediaRevokedMetadataKey] == true {
			return discard(nil)
		}
		if message.MediaURL != "" {
			work.Extracted.Media = &MediaInfo{MediaURL: message.MediaURL, MediaMimeType: message.MediaMimeType, MediaFilename: message.MediaFilename}
			return discard(nil)
		}
		if message.MessageType != before.MessageType || !message.UpdatedAt.Equal(before.UpdatedAt) ||
			coexistenceMediaMetadataString(message.Metadata, coexistenceMediaProviderIDMetadataKey) != coexistenceMediaMetadataString(before.Metadata, coexistenceMediaProviderIDMetadataKey) {
			return discard(errors.New("incoming media changed during hydration"))
		}
		updates := map[string]any{
			"media_url": hydrated.Media.MediaURL, "media_mime_type": hydrated.Media.MediaMimeType,
			"media_filename": hydrated.Media.MediaFilename,
		}
		if err := scoped.DB.Model(&models.Message{}).Where("organization_id = ? AND id = ?", work.OrganizationID, message.ID).Updates(updates).Error; err != nil {
			return err
		}
		work.Extracted = hydrated
		work.Persisted.MediaURL, work.Persisted.MediaMimeType, work.Persisted.MediaFilename =
			hydrated.Media.MediaURL, hydrated.Media.MediaMimeType, hydrated.Media.MediaFilename
		return nil
	})
	if err != nil {
		// A failed/uncertain commit is not proof that the uploaded key is
		// unreferenced. Never delete media on that path.
		return fmt.Errorf("validate and persist incoming media: %w", err)
	}
	if discarded {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		if err := a.rootApp().deleteTenantMedia(cleanupCtx, work.OrganizationID, hydrated.Media.MediaURL); err != nil {
			return &inboundContinuationManualReviewError{Reason: "discarded incoming media cleanup requires review"}
		}
	}
	if discardReason != nil {
		return discardReason
	}
	*account = currentAccount
	return nil
}

// getOrCreateInboundContact keeps the contact row, immutable CRM activity, and
// durable webhook outbox in one transaction. A unique-contact race aborts the
// losing PostgreSQL transaction, so retry it from a fresh transaction.
func (a *App) getOrCreateInboundContact(
	account *models.WhatsAppAccount,
	phoneNumber, profileName, bsuid string,
) (*models.Contact, bool, error) {
	if account == nil {
		return nil, false, errors.New("WhatsApp account is required")
	}

	var result models.Contact
	var isNew bool
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = a.DB.Transaction(func(tx *gorm.DB) error {
			contact, created, createErr := contactutil.GetOrCreateContact(
				tx,
				account.OrganizationID,
				phoneNumber,
				profileName,
			)
			if createErr != nil {
				return createErr
			}

			canonical, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				account.OrganizationID,
				contact.ID,
			)
			if resolveErr != nil {
				return resolveErr
			}
			if profileName != "" && canonical.ProfileName != profileName {
				if updateErr := tx.Model(canonical).
					Update("profile_name", profileName).Error; updateErr != nil {
					return updateErr
				}
				canonical.ProfileName = profileName
			}
			if created {
				if _, activityErr := recordCustomerActivity(
					tx,
					account.OrganizationID,
					customerActivityInput{
						ContactID:        canonical.ID,
						EventType:        models.CustomerActivityContactCreated,
						Category:         models.CustomerActivityCategoryContact,
						Title:            "Contact created",
						Summary:          canonical.ProfileName,
						ActorType:        models.CustomerActivityActorContact,
						SourceObjectType: "contact",
						SourceObjectID:   &canonical.ID,
						OccurredAt:       time.Now().UTC(),
						Metadata: models.JSONB{
							"whatsapp_account": account.Name,
						},
						WebhookData: models.JSONB{
							"contact_phone":    canonical.PhoneNumber,
							"contact_name":     canonical.ProfileName,
							"whatsapp_account": account.Name,
						},
						IdempotencyKey: "contact-created:" + canonical.ID.String(),
					},
				); activityErr != nil {
					return activityErr
				}
			}

			result = *canonical
			isNew = created
			return nil
		})
		if err == nil {
			// BSUID is optional metadata. Isolate its update behind a
			// savepoint so a failure cannot roll back the durable contact,
			// CRM activity, or the inbound message written by the caller.
			a.updateContactBSUID(&result, bsuid)
			return &result, isNew, nil
		}
		if !isUniqueViolation(err) && !isRetryableCanonicalContactWrite(err) {
			return nil, false, err
		}
		if attempt+1 < canonicalContactWriteAttempts {
			if waitErr := waitForCanonicalContactWriteRetry(a.DB.Statement.Context, attempt); waitErr != nil {
				return nil, false, waitErr
			}
		}
	}
	return nil, false, err
}

// saveIncomingMessage saves an incoming message to the messages table
func (a *App) saveIncomingMessage(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string) bool {
	return a.saveIncomingMessageWithFlow(account, contact, whatsappMsgID, msgType, content, mediaInfo, replyToWAMID, nil)
}

// saveIncomingMessageWithFlow persists the normalized WhatsApp Flow response
// together with the message. Keeping the submitted fields on the immutable
// inbound record is required for idempotent booking and CRM actions; session
// data alone can be overwritten by later chatbot steps.
func (a *App) saveIncomingMessageWithFlow(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string, flowResponseData map[string]any) bool {
	message, err := a.persistIncomingMessageWithFlow(
		account,
		contact,
		whatsappMsgID,
		msgType,
		content,
		mediaInfo,
		replyToWAMID,
		flowResponseData,
	)
	if err != nil {
		if errors.Is(err, errIncomingMessageAlreadyProcessed) {
			a.Log.Info("Ignored duplicate incoming message", "whatsapp_message_id", whatsappMsgID)
		} else {
			a.Log.Error("Failed to save incoming message", "error", err)
		}
		return false
	}

	a.Log.Info("Saved incoming message", "message_id", message.ID, "contact_id", contact.ID, "media_url", message.MediaURL)
	a.broadcastNewMessage(account.OrganizationID, message, contact)
	return true
}

// persistIncomingMessageWithFlow commits the normalized message and its
// lifecycle/outbox fact but performs no provider or WebSocket work. Callers can
// therefore place an acknowledgement boundary immediately after this returns.
func (a *App) persistIncomingMessageWithFlow(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string, flowResponseData map[string]any) (*models.Message, error) {
	if account == nil || contact == nil || account.OrganizationID != contact.OrganizationID || strings.TrimSpace(whatsappMsgID) == "" {
		return nil, errors.New("cannot save incoming message without account, contact and WAMID")
	}
	whatsappMsgID = strings.TrimSpace(whatsappMsgID)
	now := time.Now().UTC()
	idempotencyKey := "message-incoming:" + uuid.NewSHA1(account.ID, []byte(whatsappMsgID)).String()
	var message models.Message
	var canonicalContact models.Contact
	var duplicate bool
	var transactionErr error

	// Every retry is a fresh transaction/savepoint. In particular PostgreSQL
	// 23505 is rolled back BEFORE any winner lookup, never swallowed in an
	// aborted transaction. This loop contains no provider work.
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		transactionErr = a.DB.Transaction(func(tx *gorm.DB) error {
			scoped := a.scopedApp(tx, account.OrganizationID)
			if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
				return err
			}
			tx = scoped.DB
			lookup := whatsAppMessageLookup{
				WAMID: whatsappMsgID, ContactID: contact.ID, Direction: models.DirectionIncoming,
				Lock: true, RepairProjection: true,
			}
			resolved, err := scoped.resolveWhatsAppMessage(account, lookup)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				canonical, lockErr := contactutil.ResolveCanonicalContactForUpdate(tx, account.OrganizationID, contact.ID)
				if lockErr != nil {
					return lockErr
				}
				canonicalContact = *canonical
				// A delivery waiting for this same contact may now have a
				// committed winner. Resolve all proofs again after the wait.
				resolved, err = scoped.resolveWhatsAppMessage(account, lookup)
			}
			newMessage := errors.Is(err, gorm.ErrRecordNotFound)
			if err != nil && !newMessage {
				return err
			}
			duplicate = false
			if !newMessage {
				message, canonicalContact = resolved.Message, resolved.Contact
				duplicate = resolved.IncomingActivity || resolved.Continuation != nil
				if duplicate {
					return nil
				}
				// Imported history is not a live-processing completion fact.
				// Keep its exact winning ID, payload and tombstone intact while
				// recording the first live lifecycle fact and continuation.
			} else {
				message = models.Message{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: account.OrganizationID, ContactID: canonicalContact.ID,
					WhatsAppAccount: account.Name, WhatsAppMessageID: whatsappMsgID,
					Direction: models.DirectionIncoming, MessageType: models.MessageType(msgType),
					Content: content, Status: models.MessageStatusReceived,
				}
				if len(flowResponseData) > 0 {
					message.FlowResponse = models.JSONB(flowResponseData)
				}
				if mediaInfo != nil {
					message.MediaURL, message.MediaMimeType, message.MediaFilename =
						mediaInfo.MediaURL, mediaInfo.MediaMimeType, mediaInfo.MediaFilename
				}
			}

			// Referenced targets can be incoming OR outgoing, but must belong
			// to this exact proven account and canonical contact. This is a
			// read-only target lookup: never acquire Conversation after Contact.
			if strings.TrimSpace(replyToWAMID) != "" {
				reply, replyErr := scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
					WAMID: replyToWAMID, ContactID: canonicalContact.ID,
				})
				if replyErr == nil && newMessage {
					message.IsReply, message.ReplyToMessageID = true, &reply.Message.ID
				} else if replyErr != nil && !errors.Is(replyErr, gorm.ErrRecordNotFound) {
					return fmt.Errorf("resolve incoming reply target: %w", replyErr)
				}
			}
			if newMessage {
				settings, settingsErr := scoped.getChatbotSettingsCached(account.OrganizationID, account.Name)
				if settingsErr == nil && settings.IsEnabled {
					var activeTransfers int64
					if err := tx.Model(&models.AgentTransfer{}).Where(
						"organization_id = ? AND contact_id = ? AND status = ?",
						account.OrganizationID, canonicalContact.ID, models.TransferStatusActive,
					).Count(&activeTransfers).Error; err != nil {
						return err
					}
					if activeTransfers == 0 {
						message.Status = models.MessageStatusRead
					}
				}
				if err := tx.Create(&message).Error; err != nil {
					return err
				}
			}
			preview := content
			eventContent := content
			if message.Metadata[coexistenceMediaRevokedMetadataKey] == true {
				preview, eventContent = "[deleted]", ""
			} else if msgType != "text" && msgType != "button_reply" && msgType != "nfm_reply" {
				preview = "[" + msgType + "]"
			} else if len(preview) > 100 {
				preview = preview[:97] + "..."
			}
			if err := tx.Model(&canonicalContact).Updates(map[string]any{
				"last_message_at": now, "last_message_preview": preview, "is_read": false,
				"whats_app_account": account.Name, "last_inbound_at": now,
			}).Error; err != nil {
				return err
			}
			_, err = recordCustomerActivity(tx, account.OrganizationID, customerActivityInput{
				ContactID: canonicalContact.ID, EventType: models.CustomerActivityMessageIncoming,
				Category: models.CustomerActivityCategoryMessage, Title: "Message received", Summary: preview,
				ActorType: models.CustomerActivityActorContact, SourceObjectType: "message", SourceObjectID: &message.ID,
				OccurredAt: now, IdempotencyKey: idempotencyKey,
				Metadata: models.JSONB{"message_type": msgType, "message_status": string(message.Status), "whatsapp_account": account.Name},
				WebhookData: models.JSONB{
					"message_id": message.ID.String(), "contact_phone": canonicalContact.PhoneNumber,
					"contact_name": canonicalContact.ProfileName, "message_type": models.MessageType(msgType),
					"content": eventContent, "whatsapp_account": account.Name, "direction": models.DirectionIncoming,
				},
			})
			if err != nil {
				return err
			}
			canonicalContact.LastMessageAt, canonicalContact.LastInboundAt = &now, &now
			canonicalContact.LastMessagePreview, canonicalContact.IsRead, canonicalContact.WhatsAppAccount = preview, false, account.Name
			return nil
		})
		if transactionErr == nil || (!isUniqueViolation(transactionErr) && !isRetryableCanonicalContactWrite(transactionErr)) {
			break
		}
		if attempt+1 < canonicalContactWriteAttempts {
			if waitErr := waitForCanonicalContactWriteRetry(a.DB.Statement.Context, attempt); waitErr != nil {
				transactionErr = waitErr
				break
			}
		}
	}
	if transactionErr != nil {
		return nil, transactionErr
	}
	*contact = canonicalContact
	a.mirrorLegacyWhatsAppMessageAfterCommit(account, message.ID)
	if duplicate {
		// Return the proven row; the caller must never rediscover it by name.
		// The sentinel is OUTSIDE the successful transaction so projection
		// repair isn't lost to a rollback of an otherwise accepted replay.
		return &message, errIncomingMessageAlreadyProcessed
	}
	return &message, nil
}

// isWithinBusinessHours checks if current time is within configured business hours
func (a *App) isWithinBusinessHours(businessHours models.JSONBArray) bool {
	now := time.Now()
	currentDay := int(now.Weekday()) // 0 = Sunday, 1 = Monday, etc.
	currentTime := now.Format("15:04")

	for _, bh := range businessHours {
		bhMap, ok := bh.(map[string]any)
		if !ok {
			continue
		}

		// Get day (0-6, Sunday-Saturday)
		day, ok := bhMap["day"].(float64)
		if !ok {
			continue
		}

		if int(day) != currentDay {
			continue
		}

		// Check if enabled for this day
		enabled, ok := bhMap["enabled"].(bool)
		if !ok || !enabled {
			return false // Day exists but is disabled
		}

		// Get start and end times
		startTime, ok := bhMap["start_time"].(string)
		if !ok {
			continue
		}
		endTime, ok := bhMap["end_time"].(string)
		if !ok {
			continue
		}

		// Compare times (simple string comparison works for HH:MM format)
		if currentTime >= startTime && currentTime <= endTime {
			return true
		}
		return false // Found the day but outside hours
	}

	// If no matching day found, assume outside business hours
	return false
}
