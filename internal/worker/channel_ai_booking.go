package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/booking"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/customeractivity"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	channelAIBookingSessionKey           = "ai_booking_session_v1"
	channelAIBookingGenerationKey        = "ai_booking_generation_sha256"
	channelAIBookingKindKey              = "ai_booking_kind"
	channelAIBookingOfferDigestKey       = "ai_booking_offer_sha256"
	channelAIBookingOfferIDKey           = "ai_booking_offer_id"
	channelAIBookingIDKey                = "ai_booking_id"
	channelAIBookingHandoffResponse      = `{"action":"handoff","reply":"","service":"","date":""}`
	channelAIBookingConfirmationResponse = `{"action":"reply","reply":"Server confirmation input.","service":"","date":""}`
)

type channelAIBookingReceipt struct {
	BookingID      uuid.UUID `json:"booking_id"`
	MessageID      uuid.UUID `json:"message_id"`
	ChoiceToken    string    `json:"choice_token"`
	ReservationKey string    `json:"reservation_key"`
}

type channelAIBookingSession struct {
	SchemaVersion int                      `json:"schema_version"`
	Offer         booking.Offer            `json:"offer"`
	Fingerprint   string                   `json:"offer_sha256"`
	Receipt       *channelAIBookingReceipt `json:"receipt,omitempty"`
}

type channelAIBookingResult struct {
	Response   string
	Metadata   models.JSONB
	SettleOnly bool
	Reason     string
}

// A booking binding includes the immutable account/conversation/contact tuple
// and the last Pause/handoff boundary. Resume must not revive an older offer.
func channelAIBookingBinding(snapshot channelAIReplySnapshot) (booking.Binding, bool, error) {
	revision, enabled := models.AIBookingAuthorityForInbound(&snapshot.Account, snapshot.Inbound.EffectiveIngestedAt())
	if !enabled {
		return booking.Binding{}, false, nil
	}
	a, c := snapshot.Account, snapshot.Conversation
	if a.Provider != channelapi.RelayProvider || (a.Channel != models.ChannelInstagram && a.Channel != models.ChannelMessenger) ||
		c.ID == uuid.Nil || c.OrganizationID != a.OrganizationID || c.ChannelAccountID != a.ID || c.Channel != a.Channel ||
		snapshot.Contact.ID != c.ContactID || snapshot.Contact.OrganizationID != a.OrganizationID ||
		snapshot.Contact.MergedIntoID != nil || snapshot.Contact.DeletedAt.Valid ||
		snapshot.Inbound.OrganizationID != a.OrganizationID || snapshot.Inbound.ContactID != c.ContactID ||
		snapshot.Inbound.InboxConversationID == nil || *snapshot.Inbound.InboxConversationID != c.ID {
		return booking.Binding{}, false, errors.New("AI booking canonical binding is invalid")
	}
	encoded, err := json.Marshal([]any{"rereply:managed-ai-booking:route:v1", a.OrganizationID, a.ID, a.Channel,
		a.Provider, a.ExternalAccountID, a.Metadata["meta_platform_app_id"], c.ID, c.ExternalConversationID,
		c.ContactIdentityID, c.Config[models.ConversationConfigAIPausedAt], snapshot.Contact.Metadata[contacthandoff.CutoffKey]})
	if err != nil {
		return booking.Binding{}, false, err
	}
	digest := sha256.Sum256(encoded)
	return booking.Binding{OrganizationID: a.OrganizationID, ChannelAccountID: a.ID, ContactID: c.ContactID,
		ScopeID: c.ID, Channel: string(a.Channel), Revision: revision, RoutingDigest: hex.EncodeToString(digest[:])}, true, nil
}

func channelAIBookingGeneration(snapshot channelAIReplySnapshot) (string, error) {
	binding, enabled, err := channelAIBookingBinding(snapshot)
	if err != nil || !enabled {
		return "", err
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("rereply:managed-ai-booking:generation:v1\n"), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func channelAIBookingGenerationMatches(job *models.ScheduledJob, snapshot channelAIReplySnapshot) bool {
	if job == nil {
		return false
	}
	value, present := job.Payload[channelAIBookingGenerationKey]
	if !present {
		return snapshot.BookingGeneration == ""
	}
	expected, ok := value.(string)
	return ok && expected == snapshot.BookingGeneration
}

func channelAIBookingLocalResponse(snapshot channelAIReplySnapshot) (string, bool) {
	if snapshot.BookingGeneration == "" {
		return "", false
	}
	if booking.RequiresHandoff(snapshot.UserText) {
		return channelAIBookingHandoffResponse, true
	}
	if _, ok := booking.ConfirmationToken(snapshot.UserText); ok {
		return channelAIBookingConfirmationResponse, true
	}
	return "", false
}

func normalizeChannelAIBookingResponse(raw string) string {
	// Do not strip code fences, NULs, unknown fields or duplicate keys into an
	// apparently valid command. The strict parser routes malformed data to staff.
	if raw == "" || len(raw) > 8192 || !utf8.ValidString(raw) {
		return channelAIBookingHandoffResponse
	}
	return raw
}

func channelAIBookingInboundSuppressed(tx *gorm.DB, contact *models.Contact, conversation *models.InboxConversation, inbound *models.Message) (bool, error) {
	if contact == nil || conversation == nil || inbound == nil {
		return true, errors.New("AI inbound policy identity missing")
	}
	if contacthandoff.MessageSuppressed(inbound) {
		return true, nil
	}
	if raw, exists := conversation.Config[models.ConversationConfigAIPausedAt]; exists {
		text, ok := raw.(string)
		cutoff, err := time.Parse(time.RFC3339Nano, text)
		if !ok || err != nil || cutoff.IsZero() {
			return true, nil
		}
		if !inbound.EffectiveIngestedAt().After(cutoff) {
			return true, nil
		}
	}
	return contacthandoff.SuppressedTx(tx, contact.OrganizationID, inbound.ID)
}

func parseChannelAIBookingSession(metadata models.JSONB) (*channelAIBookingSession, error) {
	value, exists := metadata[channelAIBookingSessionKey]
	if !exists {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 32768 {
		return nil, errors.New("invalid AI booking state")
	}
	var state channelAIBookingSession
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil || state.SchemaVersion != 1 {
		return nil, errors.New("invalid AI booking state")
	}
	if _, err := booking.ParseOffer(state.Offer); err != nil {
		return nil, err
	}
	digest, err := booking.OfferFingerprint(state.Offer)
	if err != nil || digest != state.Fingerprint {
		return nil, errors.New("AI booking offer fingerprint mismatch")
	}
	if r := state.Receipt; r != nil {
		if r.BookingID == uuid.Nil || r.MessageID == uuid.Nil || r.ReservationKey != booking.ReservationKey(state.Offer.ID, r.ChoiceToken) {
			return nil, errors.New("invalid AI booking receipt")
		}
		found := false
		for _, choice := range state.Offer.Choices {
			if choice.Token == r.ChoiceToken {
				found = true
			}
		}
		if !found {
			return nil, errors.New("AI booking receipt choice mismatch")
		}
	}
	return &state, nil
}

func saveChannelAIBookingSessionTx(tx *gorm.DB, snapshot *channelAIReplySnapshot, state *channelAIBookingSession) error {
	metadata := cloneChannelAIReplyPayload(snapshot.Conversation.Metadata)
	if state == nil {
		delete(metadata, channelAIBookingSessionKey)
	} else {
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		var value models.JSONB
		if err := json.Unmarshal(encoded, &value); err != nil {
			return err
		}
		metadata[channelAIBookingSessionKey] = value
	}
	result := tx.Model(&models.InboxConversation{}).Where("id = ? AND organization_id = ? AND channel_account_id = ?",
		snapshot.Conversation.ID, snapshot.Account.OrganizationID, snapshot.Account.ID).Update("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	snapshot.Conversation.Metadata = metadata
	return nil
}

// prepareChannelAIBookingTx runs inside finalizeChannelAIReply's organization
// fence and transaction. It never commits, calls an AI/provider, or sends.
func (w *Worker) prepareChannelAIBookingTx(tx *gorm.DB, snapshot *channelAIReplySnapshot, messageID uuid.UUID, response string) (channelAIBookingResult, error) {
	out := channelAIBookingResult{Response: response, Metadata: models.JSONB{}}
	if snapshot.BookingGeneration == "" {
		return out, nil
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ? AND channel_account_id = ?",
		snapshot.Conversation.ID, snapshot.Account.OrganizationID, snapshot.Account.ID).First(&snapshot.Conversation).Error; err != nil {
		return out, err
	}
	if err := database.LockContactPolicyScope(tx, snapshot.Account.OrganizationID, snapshot.Contact.ID); err != nil {
		return out, err
	}
	if err := tx.Where("id = ? AND organization_id = ?", snapshot.Contact.ID, snapshot.Account.OrganizationID).First(&snapshot.Contact).Error; err != nil {
		return out, err
	}
	binding, enabled, err := channelAIBookingBinding(*snapshot)
	if err != nil {
		return out, err
	}
	if !enabled {
		return out, errors.New("AI booking authority unavailable")
	}
	state, err := parseChannelAIBookingSession(snapshot.Conversation.Metadata)
	if err != nil {
		return channelAIBookingHandoffTx(tx, snapshot, "invalid_booking_state")
	}
	if booking.RequiresHandoff(snapshot.UserText) {
		return channelAIBookingHandoffTx(tx, snapshot, "customer_requested_staff_or_clinical_help")
	}
	if token, confirm := booking.ConfirmationToken(snapshot.UserText); confirm {
		return confirmChannelAIBookingTx(tx, snapshot, binding, state, token, messageID)
	}
	if strings.HasPrefix(snapshot.UserText, "BOOK ") {
		out.Response = "That confirmation was not valid. Please use the exact BOOK code from a current booking offer, or ask staff for help."
		return out, nil
	}
	intent, err := booking.ParseIntent(response)
	if err != nil || intent.Action == "handoff" || booking.RequiresHandoff(intent.Reply) {
		return channelAIBookingHandoffTx(tx, snapshot, "booking_intent_requires_staff")
	}
	switch intent.Action {
	case "reply":
		out.Response = intent.Reply
		return out, nil
	case "offer":
		return offerChannelAIBookingTx(tx, snapshot, binding, intent.Service, intent.Date, messageID)
	default:
		return channelAIBookingHandoffTx(tx, snapshot, "unsupported_booking_intent")
	}
}

func channelAIBookingMetadata(snapshot *channelAIReplySnapshot, state *channelAIBookingSession, kind string) models.JSONB {
	metadata := models.JSONB{channelAIBookingKindKey: kind, channelAIBookingGenerationKey: snapshot.BookingGeneration,
		channelAIBookingOfferIDKey: state.Offer.ID.String(), channelAIBookingOfferDigestKey: state.Fingerprint}
	if state.Receipt != nil {
		metadata[channelAIBookingIDKey] = state.Receipt.BookingID.String()
	}
	return metadata
}

func offerChannelAIBookingTx(tx *gorm.DB, snapshot *channelAIReplySnapshot, binding booking.Binding, service, date string, messageID uuid.UUID) (channelAIBookingResult, error) {
	out := channelAIBookingResult{Metadata: models.JSONB{}}
	now := time.Now().UTC()
	entitled, err := booking.HasEntitlementTx(tx, binding.OrganizationID, now)
	if err != nil {
		return out, err
	}
	if !entitled {
		return channelAIBookingHandoffTx(tx, snapshot, "booking_entitlement_unavailable")
	}
	page, err := booking.ListSlots(tx, binding.OrganizationID, booking.SlotFilter{From: now, To: now.AddDate(0, 0, 30), Service: service, Date: date, Limit: 3})
	if err != nil {
		return out, err
	}
	if len(page.Slots) == 0 {
		out.Response = "I cannot find an available scheduled place for that service and date. Please choose another date or ask staff for help."
		return out, saveChannelAIBookingSessionTx(tx, snapshot, nil)
	}
	offer, err := booking.NewOffer(binding, snapshot.Inbound.ID, messageID, page.Slots, now, 10*time.Minute)
	if err != nil {
		return out, err
	}
	fingerprint, err := booking.OfferFingerprint(offer)
	if err != nil {
		return out, err
	}
	state := &channelAIBookingSession{SchemaVersion: 1, Offer: offer, Fingerprint: fingerprint}
	delete(snapshot.Conversation.Metadata, contacthandoff.ProposalInvalidatedKey)
	if err := saveChannelAIBookingSessionTx(tx, snapshot, state); err != nil {
		return out, err
	}
	out.Response = booking.RenderOffer(offer)
	out.Metadata = channelAIBookingMetadata(snapshot, state, "offer")
	return out, nil
}

func channelAIBookingOfferSentTx(tx *gorm.DB, state *channelAIBookingSession) (bool, error) {
	if state == nil {
		return false, nil
	}
	b := state.Offer.Binding
	var message models.Message
	if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ? AND inbox_conversation_id = ? AND direction = ?",
		state.Offer.OfferMessageID, b.OrganizationID, b.ContactID, b.ScopeID, models.DirectionOutgoing).First(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if message.Content != booking.RenderOffer(state.Offer) || message.SentByUserID != nil ||
		message.Metadata[channelAIBookingOfferDigestKey] != state.Fingerprint || message.Metadata[channelAIBookingKindKey] != "offer" ||
		(message.Status != models.MessageStatusSent && message.Status != models.MessageStatusDelivered && message.Status != models.MessageStatusRead) {
		return false, nil
	}
	var jobs []models.OutboxJob
	if err := tx.Where("organization_id = ? AND channel_account_id = ? AND conversation_id = ? AND message_id = ?",
		b.OrganizationID, b.ChannelAccountID, b.ScopeID, message.ID).Limit(2).Find(&jobs).Error; err != nil {
		return false, err
	}
	if len(jobs) != 1 || jobs[0].Status != models.OutboxJobStatusSent {
		return false, nil
	}
	outbound, err := channelOutboundMessageForJob(&jobs[0])
	if err != nil {
		return false, err
	}
	return isChannelAIReplyOutbox(&jobs[0], outbound) && outbound.MessageID == message.ID &&
		outbound.Metadata[channelAIBookingOfferDigestKey] == state.Fingerprint && outbound.Metadata[channelAIBookingKindKey] == "offer", nil
}

func confirmChannelAIBookingTx(tx *gorm.DB, snapshot *channelAIReplySnapshot, binding booking.Binding, state *channelAIBookingSession, token string, messageID uuid.UUID) (channelAIBookingResult, error) {
	out := channelAIBookingResult{Response: "Please request a current booking offer before confirming.", Metadata: models.JSONB{}}
	if state == nil || state.Offer.Binding != binding {
		return out, nil
	}
	if invalidated, exists := snapshot.Conversation.Metadata[contacthandoff.ProposalInvalidatedKey]; exists && invalidated != false {
		out.Response = "That offer was passed to staff. Please request a new offer or continue with staff."
		return out, nil
	}
	if state.Receipt != nil {
		if state.Receipt.ChoiceToken != token {
			out.Response = "This offer already has a reserved choice. Please ask staff for help with changes."
			return out, nil
		}
		if err := verifyChannelAIBookingReceiptTx(tx, state); err != nil {
			return out, err
		}
		out.SettleOnly = true
		out.Reason = "ai_booking_already_reserved"
		return out, nil
	}
	sent, err := channelAIBookingOfferSentTx(tx, state)
	if err != nil {
		return out, err
	}
	// Pending or ambiguous delivery is not proof the customer received this
	// offer. Never replace it with a freshly generated, guessed authorization.
	if !sent {
		out.Response = "That offer has not been confirmed as delivered. Please ask staff for help; no booking has been made."
		return out, nil
	}
	choice, err := booking.ValidateConfirmation(state.Offer, binding, token, true, time.Now().UTC())
	if err != nil {
		if time.Now().UTC().Before(state.Offer.ExpiresAt) {
			out.Response = "That BOOK code does not match the current offer. Please use its exact code."
			return out, nil
		}
		return offerChannelAIBookingTx(tx, snapshot, binding, state.Offer.Choices[0].Slot.ServiceName, "", messageID)
	}
	key := booking.ReservationKey(state.Offer.ID, choice.Token)
	metadata := channelAIBookingMetadata(snapshot, state, "confirmation")
	metadata["ai_booking_choice_token"] = choice.Token
	metadata["ai_booking_confirmation_message_id"] = messageID.String()
	reserved, err := booking.ReserveTx(tx, binding.OrganizationID, booking.ReserveInput{EventID: choice.Slot.EventID, ContactID: binding.ContactID,
		Quantity: 1, Status: models.BookingStatusReserved, Source: models.BookingSourceAPI, IdempotencyKey: key, Metadata: metadata,
		Expected: &choice.Slot, OfferExpiresAt: state.Offer.ExpiresAt}, time.Now)
	if err != nil {
		if booking.IsOfferStale(err) {
			return offerChannelAIBookingTx(tx, snapshot, binding, choice.Slot.ServiceName, "", messageID)
		}
		return out, err
	}
	if !reserved.Created {
		return out, errors.New("booking exists without its atomic confirmation receipt")
	}
	if _, err := customeractivity.RecordTx(tx, binding.OrganizationID, customeractivity.Input{ContactID: binding.ContactID,
		EventType: models.CustomerActivityBookingCreated, Category: models.CustomerActivityCategoryBooking, Title: "Booking reserved by AI",
		ActorType: models.CustomerActivityActorSystem, SourceObjectType: "booking", SourceObjectID: &reserved.Booking.ID,
		IdempotencyKey: key, Metadata: models.JSONB{
			"booking_id": reserved.Booking.ID.String(), "event_id": choice.Slot.EventID.String(), "source": "ai_booking",
			"channel_account_id": binding.ChannelAccountID.String(), "offer_sha256": state.Fingerprint,
		}}); err != nil {
		return out, err
	}
	state.Receipt = &channelAIBookingReceipt{BookingID: reserved.Booking.ID, MessageID: messageID, ChoiceToken: choice.Token, ReservationKey: key}
	if err := saveChannelAIBookingSessionTx(tx, snapshot, state); err != nil {
		return out, err
	}
	out.Response = booking.RenderConfirmation(choice)
	out.Metadata = channelAIBookingMetadata(snapshot, state, "confirmation")
	return out, nil
}

func verifyChannelAIBookingReceiptTx(tx *gorm.DB, state *channelAIBookingSession) error {
	if state == nil || state.Receipt == nil {
		return errors.New("AI booking receipt missing")
	}
	r, b := state.Receipt, state.Offer.Binding
	var existing models.Booking
	if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ? AND idempotency_key = ?",
		r.BookingID, b.OrganizationID, b.ContactID, r.ReservationKey).First(&existing).Error; err != nil {
		return err
	}
	if existing.Quantity != 1 || existing.ContactPackageID != nil || existing.BookedByID != nil || existing.UpdatedByID != nil ||
		existing.Source != models.BookingSourceAPI || existing.Metadata[channelAIBookingOfferDigestKey] != state.Fingerprint ||
		existing.Metadata["ai_booking_choice_token"] != r.ChoiceToken || existing.Metadata["ai_booking_confirmation_message_id"] != r.MessageID.String() {
		return errors.New("AI booking receipt provenance mismatch")
	}
	var message models.Message
	if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ? AND inbox_conversation_id = ?",
		r.MessageID, b.OrganizationID, b.ContactID, b.ScopeID).First(&message).Error; err != nil {
		return err
	}
	for _, choice := range state.Offer.Choices {
		if choice.Token == r.ChoiceToken && choice.Slot.EventID == existing.EventID && message.Content == booking.RenderConfirmation(choice) &&
			message.SentByUserID == nil && message.Metadata[channelAIBookingIDKey] == existing.ID.String() &&
			message.Metadata[channelAIBookingOfferDigestKey] == state.Fingerprint {
			return nil
		}
	}
	return errors.New("AI booking confirmation content mismatch")
}

func channelAIBookingHandoffTx(tx *gorm.DB, snapshot *channelAIReplySnapshot, reason string) (channelAIBookingResult, error) {
	out := channelAIBookingResult{SettleOnly: true, Reason: "ai_booking_staff_attention"}
	// No acknowledgment is queued: an active transfer correctly blocks bot
	// dispatch. The unassigned staff item, cutoff and activity are the outcome.
	result, err := contacthandoff.HandoffTx(tx, contacthandoff.Request{
		OrganizationID: snapshot.Account.OrganizationID, ContactID: snapshot.Contact.ID, AccountName: snapshot.Account.Name,
		Reason: reason, Source: models.TransferSourceFlow,
	})
	if err != nil {
		return out, err
	}
	if result.Contact.ID != snapshot.Contact.ID {
		return out, errors.New("AI handoff canonical contact changed")
	}
	if _, err := customeractivity.RecordTx(tx, snapshot.Account.OrganizationID, customeractivity.Input{
		ContactID: snapshot.Contact.ID, EventType: "booking.staff_requested", Category: models.CustomerActivityCategoryBooking,
		Title: "Booking needs staff assistance", ActorType: models.CustomerActivityActorSystem, SourceObjectType: "agent_transfer",
		SourceObjectID: &result.Transfer.ID, IdempotencyKey: "ai-booking-handoff:" + snapshot.Inbound.ID.String(),
		Metadata: models.JSONB{"source": "ai_booking", "reason": reason},
	}); err != nil {
		return out, err
	}
	// HandoffTx has written the invalidation marker; do not save an older
	// Metadata snapshot over it or erase a consumed booking receipt.
	return out, nil
}

// verifyChannelAIBookingDispatchTx runs under the existing tenant physical
// attempt fence, never a separate organization-lock upgrade or provider call.
func verifyChannelAIBookingDispatchTx(tx *gorm.DB, account *models.ChannelAccount, conversation *models.InboxConversation, inbound *models.Message, job *models.OutboxJob, outbound channelapi.OutboundMessage) error {
	kind, present := outbound.Metadata[channelAIBookingKindKey]
	if !present {
		return nil
	}
	if kind != "offer" && kind != "confirmation" {
		return errors.New("invalid AI booking delivery kind")
	}
	if !isChannelAIReplyOutbox(job, outbound) || job.MessageID == nil || *job.MessageID != outbound.MessageID {
		return errors.New("AI booking bot delivery binding mismatch")
	}
	entitled, err := booking.HasEntitlementTx(tx, account.OrganizationID, time.Now().UTC())
	if err != nil {
		return err
	}
	if !entitled {
		return errors.New("booking entitlement unavailable at dispatch")
	}
	var contact models.Contact
	if err := tx.Where("id = ? AND organization_id = ?", conversation.ContactID, account.OrganizationID).First(&contact).Error; err != nil {
		return err
	}
	snapshot := channelAIReplySnapshot{Account: *account, Conversation: *conversation, Contact: contact, Inbound: *inbound}
	generation, err := channelAIBookingGeneration(snapshot)
	if err != nil {
		return err
	}
	if generation == "" || outbound.Metadata[channelAIBookingGenerationKey] != generation {
		return errors.New("AI booking delivery authority changed")
	}
	state, err := parseChannelAIBookingSession(conversation.Metadata)
	if err != nil {
		return err
	}
	if state == nil || outbound.Metadata[channelAIBookingOfferIDKey] != state.Offer.ID.String() || outbound.Metadata[channelAIBookingOfferDigestKey] != state.Fingerprint {
		return errors.New("AI booking delivery offer changed")
	}
	if invalidated, exists := conversation.Metadata[contacthandoff.ProposalInvalidatedKey]; exists && invalidated != false {
		return errors.New("AI booking offer invalidated by staff handoff")
	}
	if kind == "offer" {
		if state.Receipt != nil || state.Offer.OfferMessageID != outbound.MessageID || !time.Now().UTC().Before(state.Offer.ExpiresAt) ||
			len(outbound.Parts) != 1 || outbound.Parts[0].Text != booking.RenderOffer(state.Offer) {
			return errors.New("AI booking offer delivery is stale")
		}
		return nil
	}
	if state.Receipt == nil || state.Receipt.MessageID != outbound.MessageID || outbound.Metadata[channelAIBookingIDKey] != state.Receipt.BookingID.String() {
		return errors.New("AI booking confirmation receipt changed")
	}
	expectedText := ""
	for _, choice := range state.Offer.Choices {
		if choice.Token == state.Receipt.ChoiceToken {
			expectedText = booking.RenderConfirmation(choice)
		}
	}
	if expectedText == "" || len(outbound.Parts) != 1 || outbound.Parts[0].Text != expectedText {
		return errors.New("AI booking confirmation wire content changed")
	}
	if err := verifyChannelAIBookingReceiptTx(tx, state); err != nil {
		return err
	}
	var status models.Booking
	if err := tx.Select("status").Where("id = ? AND organization_id = ?", state.Receipt.BookingID, account.OrganizationID).First(&status).Error; err != nil {
		return err
	}
	if status.Status != models.BookingStatusReserved {
		return fmt.Errorf("AI booking confirmation is no longer reserved")
	}
	return nil
}
