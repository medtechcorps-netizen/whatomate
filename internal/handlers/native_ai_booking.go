package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/booking"
	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/customeractivity"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	nativeBookingProposalKey    = "__ai_booking_proposal"
	nativeBookingProofKey       = "native_ai_booking_proof"
	nativeBookingFingerprintKey = "native_ai_booking_fingerprint"
	nativeBookingEffect         = "native_booking_send"
	// Unlike generic pre_attempt, this state is proof that no provider boundary
	// was entered. It changes atomically with the Message dispatch marker.
	inboundActionStateBookingPrepared = "booking_prepared_unattempted"
)

type nativeBookingProof struct {
	Version           int             `json:"version"`
	Kind              string          `json:"kind"`
	Binding           booking.Binding `json:"binding"`
	NativeAccountID   uuid.UUID       `json:"native_account_id"`
	InboundID         uuid.UUID       `json:"inbound_id"`
	MessageID         uuid.UUID       `json:"message_id"`
	OfferID           uuid.UUID       `json:"offer_id"`
	BookingID         uuid.UUID       `json:"booking_id"`
	ActionKey         string          `json:"action_key"`
	ContentDigest     string          `json:"content_digest"`
	SessionAcquiredAt time.Time       `json:"session_acquired_at"`
}

type nativeBookingPrepared struct {
	Claim   *inboundContinuationActionClaim
	Message models.Message
	Proof   nativeBookingProof
}

func isNativeBookingConfirmation(input string) bool {
	_, ok := booking.ConfirmationToken(input)
	return ok
}

func nativeBookingCheckpointData(data models.JSONB) clause.Expr {
	// Use the target row's reserved value, including its absence. Neither an
	// authored variable nor a stale in-memory proposal can create authority.
	return gorm.Expr("(COALESCE(CAST(? AS jsonb), '{}'::jsonb) - CAST(? AS text)) || CASE WHEN jsonb_exists(COALESCE(session_data, '{}'::jsonb), CAST(? AS text)) THEN jsonb_build_object(CAST(? AS text), session_data -> CAST(? AS text)) ELSE '{}'::jsonb END",
		data, nativeBookingProposalKey, nativeBookingProposalKey, nativeBookingProposalKey, nativeBookingProposalKey)
}

func nativeBookingProposalPending(data models.JSONB) bool {
	proposal, ok := nativeBookingObject(data[nativeBookingProposalKey])
	if !ok || proposal["reservation_key"] != nil {
		return false
	}
	offer, err := booking.ParseOffer(proposal["offer"])
	return err == nil && time.Now().Before(offer.ExpiresAt)
}

func nativeBookingDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func nativeBookingProofJSON(proof nativeBookingProof) (models.JSONB, string, error) {
	if proof.Version != 1 || proof.Binding.OrganizationID == uuid.Nil || proof.Binding.ChannelAccountID == uuid.Nil ||
		proof.Binding.ContactID == uuid.Nil || proof.Binding.ScopeID == uuid.Nil || proof.Binding.Channel != "whatsapp" ||
		proof.Binding.Revision == "" || len(proof.Binding.RoutingDigest) != 64 || proof.NativeAccountID == uuid.Nil ||
		proof.InboundID == uuid.Nil || proof.MessageID == uuid.Nil || proof.ActionKey == "" ||
		len(proof.ContentDigest) != 64 || proof.SessionAcquiredAt.IsZero() ||
		(proof.Kind != "offer" && proof.Kind != "confirmation" && proof.Kind != "reply") ||
		(proof.Kind == "offer" && proof.OfferID == uuid.Nil) ||
		(proof.Kind == "confirmation" && (proof.OfferID == uuid.Nil || proof.BookingID == uuid.Nil)) {
		return nil, "", errors.New("native booking proof is incomplete")
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return nil, "", err
	}
	var value models.JSONB
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, "", err
	}
	return value, nativeBookingDigest("native-booking:v1\n" + string(raw)), nil
}

func parseNativeBookingProof(value any) (nativeBookingProof, string, error) {
	var proof nativeBookingProof
	raw, err := json.Marshal(value)
	if err != nil {
		return proof, "", err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return proof, "", err
	}
	_, fingerprint, err := nativeBookingProofJSON(proof)
	return proof, fingerprint, err
}

// nativeBookingAuthorityTx never creates a missing shadow. Both alternate
// immutable selectors are included so a corrupt second binding fails closed.
// Caller owns the organization fence; shadow precedes native account locks.
func (a *App) nativeBookingAuthorityTx(tx *gorm.DB, accountID, sessionID uuid.UUID) (booking.Binding, *models.WhatsAppAccount, error) {
	var binding booking.Binding
	execution := a.inboundContinuation
	if execution == nil || accountID == uuid.Nil || sessionID == uuid.Nil {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking inbound identity is missing"}
	}
	if err := a.requireInboundContinuationPolicyTx(tx); err != nil {
		return binding, nil, err
	}
	suppressed, err := contacthandoff.SuppressedTx(tx, execution.OrganizationID, execution.MessageID)
	if err != nil {
		return binding, nil, err
	}
	if suppressed {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking inbound was cut off by handoff"}
	}
	allowed, err := booking.HasEntitlementTx(tx, execution.OrganizationID, time.Now().UTC())
	if err != nil {
		return binding, nil, err
	}
	if !allowed {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking entitlement is unavailable"}
	}
	var shadows []models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where(
		"organization_id = ? AND channel = ? AND provider = ? AND (external_account_id = ? OR metadata->>'legacy_account_id' = ?)",
		execution.OrganizationID, models.ChannelWhatsApp, channel.LegacyMetaProvider,
		"legacy-account:"+accountID.String(), accountID.String()).Find(&shadows).Error; err != nil {
		return binding, nil, err
	}
	if len(shadows) != 1 {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking requires exactly one live native shadow"}
	}
	var native models.WhatsAppAccount
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND organization_id = ?", accountID, execution.OrganizationID).First(&native).Error; err != nil {
		return binding, nil, err
	}
	revision, enabled := channel.LegacyMetaAIBookingAuthority(&shadows[0], &native)
	var inbound models.Message
	if err := tx.Where("id = ? AND organization_id = ?", execution.MessageID, execution.OrganizationID).First(&inbound).Error; err != nil {
		return binding, nil, err
	}
	now := time.Now().UTC()
	if inbound.CreatedAt.IsZero() || inbound.CreatedAt.After(now) || !now.Before(inbound.CreatedAt.Add(24*time.Hour)) {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "native booking customer service window is unavailable"}
	}
	inboundRevision, admitted := models.AIBookingAuthorityForInbound(&shadows[0], inbound.EffectiveIngestedAt())
	if !enabled || !admitted || inboundRevision != revision || inbound.WhatsAppAccount != native.Name {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "native booking is off or this inbound predates its approval"}
	}
	var session models.ChatbotSession
	if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ? AND status = ? AND whats_app_account = ?",
		sessionID, execution.OrganizationID, execution.ContactID, models.SessionStatusActive, native.Name).First(&session).Error; err != nil {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking session authority is unavailable"}
	}
	if session.SessionData[contacthandoff.ProposalInvalidatedKey] != nil {
		return binding, nil, &inboundContinuationPolicyStop{Reason: "booking session was invalidated by handoff"}
	}
	binding = booking.Binding{OrganizationID: execution.OrganizationID, ChannelAccountID: shadows[0].ID, ContactID: execution.ContactID,
		ScopeID: sessionID, Channel: "whatsapp", Revision: revision, RoutingDigest: channel.LegacyMetaAIBookingRouteBinding(&native)}
	return binding, &native, nil
}

func (a *App) readNativeBookingAuthority(account *models.WhatsAppAccount, session *models.ChatbotSession) (booking.Binding, error) {
	var binding booking.Binding
	err := database.WithTenantReadCommitted(a.rootApp().DB, account.OrganizationID, func(tx *gorm.DB) error {
		if err := database.LockOrganizationAIAttemptScope(tx, account.OrganizationID); err != nil {
			return err
		}
		var err error
		binding, _, err = a.nativeBookingAuthorityTx(tx, account.ID, session.ID)
		return err
	})
	return binding, err
}

// processNativeAIBooking is shared by fallback and graph AI nodes. The model
// can select only a bounded intent; exact customer tokens bypass it entirely.
func (a *App) processNativeAIBooking(account *models.WhatsAppAccount, contact *models.Contact, session *models.ChatbotSession,
	settings *models.ChatbotSettings, input string) (handled, awaiting bool, resultErr error) {
	if a == nil || a.inboundContinuation == nil || account == nil || contact == nil || session == nil {
		return false, false, nil
	}
	restore := a.pushInboundContinuationActionScope("native-booking:" + session.ID.String() + ":" + a.inboundContinuation.actionScope)
	defer restore()
	identity, err := a.nextInboundContinuationActionKey(nativeBookingEffect, nil)
	if err != nil {
		return true, false, err
	}
	// Recovery is keyed before generation. A prepared response is immutable;
	// neither a second model answer nor a newly randomized offer replaces it.
	var existing *nativeBookingPrepared
	err = database.WithTenantReadCommitted(a.rootApp().DB, account.OrganizationID, func(tx *gorm.DB) error {
		var loadErr error
		existing, loadErr = a.loadNativeBookingPreparedTx(tx, identity)
		return loadErr
	})
	if err != nil {
		return true, false, err
	}
	if existing != nil {
		a.inboundContinuation.nativeBookingCheckpoint = true
		return true, existing.Proof.Kind == "offer" || (existing.Proof.Kind == "reply" && nativeBookingProposalPending(session.SessionData)), a.dispatchNativeBookingPrepared(existing, account, contact)
	}
	binding, err := a.readNativeBookingAuthority(account, session)
	token, confirmation := booking.ConfirmationToken(input)
	if err != nil {
		if !confirmation && inboundContinuationStoppedByPolicy(err) {
			return false, false, nil
		}
		return true, false, err
	}
	a.inboundContinuation.nativeBookingCheckpoint = true
	intent := booking.Intent{}
	if !confirmation && booking.RequiresHandoff(input) {
		intent.Action = "handoff"
	} else if !confirmation {
		if settings == nil || !settings.AI.Enabled || settings.AI.Provider == "" || settings.AI.APIKey == "" {
			return false, false, nil
		}
		result, err := a.runInboundContinuationCapturedAction("native_booking_intent", func() models.JSONB {
			bounded := *settings
			bounded.AI.SystemPrompt += "\n\n" + booking.IntentInstructions
			answer, generationErr := a.generateAIResponse(&bounded, session, input)
			if generationErr != nil {
				return models.JSONB{"outcome": "handoff", "revision": binding.Revision}
			}
			return models.JSONB{"answer": answer, "revision": binding.Revision}
		})
		if err != nil {
			return true, false, err
		}
		if result["revision"] != binding.Revision {
			return true, false, &inboundContinuationPolicyStop{Reason: "booking generation approval changed"}
		}
		answer, _ := result["answer"].(string)
		intent, err = booking.ParseIntent(answer)
		if err != nil || booking.RequiresHandoff(intent.Reply) {
			intent = booking.Intent{Action: "handoff"}
		}
	}
	var prepared *nativeBookingPrepared
	err = database.WithTenantReadCommitted(a.rootApp().DB, account.OrganizationID, func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, account.OrganizationID); err != nil {
			return err
		}
		current, native, err := a.nativeBookingAuthorityTx(tx, account.ID, session.ID)
		if err != nil {
			return err
		}
		if current != binding {
			return &inboundContinuationPolicyStop{Reason: "booking channel approval changed"}
		}
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(tx, account.OrganizationID, contact.ID)
		if err != nil {
			return err
		}
		if canonical.ID != binding.ContactID {
			return &inboundContinuationPolicyStop{Reason: "booking contact identity changed"}
		}
		var stored models.ChatbotSession
		if err := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).Where("id = ? AND organization_id = ?", session.ID, account.OrganizationID).First(&stored).Error; err != nil {
			return err
		}
		if stored.Status != models.SessionStatusActive || !stored.LastActivityAt.Equal(session.LastActivityAt) {
			return &inboundContinuationPolicyStop{Reason: "booking session was superseded"}
		}
		if intent.Action == "handoff" {
			const reason = "AI booking requires human attention"
			handoff, err := contacthandoff.HandoffTx(tx, contacthandoff.Request{OrganizationID: account.OrganizationID, ContactID: canonical.ID,
				AccountName: native.Name, Reason: reason, Source: models.TransferSourceFlow})
			if err != nil {
				return err
			}
			if handoff.Contact.ID != canonical.ID {
				return errors.New("native AI booking handoff contact changed")
			}
			// Staff attention and its system timeline/webhook fact are one intent.
			// Neither a failed activity/outbox insertion nor a replay may leave a
			// transfer without its fact or fabricate a staff actor/acknowledgment.
			_, err = customeractivity.RecordTx(tx, account.OrganizationID, customeractivity.Input{
				ContactID: canonical.ID, EventType: "booking.staff_requested", Category: models.CustomerActivityCategoryBooking,
				Title: "Booking needs staff assistance", ActorType: models.CustomerActivityActorSystem,
				SourceObjectType: "agent_transfer", SourceObjectID: &handoff.Transfer.ID,
				IdempotencyKey: "ai-booking-handoff:" + a.inboundContinuation.MessageID.String(),
				Metadata:       models.JSONB{"source": "ai_booking", "reason": reason, "inbound_message_id": a.inboundContinuation.MessageID.String()},
			})
			return err
		}
		prepared, err = a.prepareNativeBookingTx(tx, identity, binding, native, canonical, &stored, session, intent, token, confirmation)
		return err
	})
	if err != nil {
		return true, false, err
	}
	if intent.Action == "handoff" {
		return true, false, &inboundContinuationPolicyStop{Reason: "human booking handoff committed"}
	}
	if prepared == nil {
		return true, false, nil
	} // a different repeated confirmation already has its receipt
	return true, prepared.Proof.Kind == "offer" || (prepared.Proof.Kind == "reply" && nativeBookingProposalPending(session.SessionData)), a.dispatchNativeBookingPrepared(prepared, account, contact)
}

func (a *App) prepareNativeBookingTx(tx *gorm.DB, identity inboundContinuationActionIdentity, binding booking.Binding,
	account *models.WhatsAppAccount, contact *models.Contact, stored, session *models.ChatbotSession,
	intent booking.Intent, token string, confirmation bool) (*nativeBookingPrepared, error) {
	if existing, err := a.loadNativeBookingPreparedTx(tx, identity); existing != nil || err != nil {
		return existing, err
	}
	now := time.Now().UTC()
	message := a.createOutgoingMessage(OutgoingMessageRequest{Account: account, Contact: contact, Type: models.MessageTypeText}, ChatbotSendOptions())
	proof := nativeBookingProof{Version: 1, Kind: "reply", Binding: binding, NativeAccountID: account.ID, InboundID: a.inboundContinuation.MessageID,
		MessageID: message.ID, ActionKey: identity.Key, SessionAcquiredAt: stored.LastActivityAt}
	data := cloneInboundContinuationJSONB(session.SessionData)
	// The reserved proposal is DB-owned, never an authored graph variable.
	delete(data, nativeBookingProposalKey)
	if value, ok := stored.SessionData[nativeBookingProposalKey]; ok {
		data[nativeBookingProposalKey] = value
	}
	if confirmation {
		proposal, ok := nativeBookingObject(data[nativeBookingProposalKey])
		if !ok {
			return nil, &inboundContinuationPolicyStop{Reason: "there is no current booking offer"}
		}
		offer, err := booking.ParseOffer(proposal["offer"])
		if err != nil {
			return nil, err
		}
		var offerMessage models.Message
		if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ?", offer.OfferMessageID, binding.OrganizationID, binding.ContactID).First(&offerMessage).Error; err != nil {
			return nil, err
		}
		confirmationAt := now
		if proposal["reservation_key"] != nil {
			// Expiry forbids a new reservation, not verification of an already
			// committed immutable receipt. This path can only consume that receipt.
			confirmationAt = offer.CreatedAt
		}
		choice, err := booking.ValidateConfirmation(offer, binding, token, nativeBookingOfferWasSent(&offerMessage, offer), confirmationAt)
		if err != nil {
			if booking.IsOfferStale(err) && nativeBookingOfferWasSent(&offerMessage, offer) {
				return a.prepareNativeBookingFreshOfferReplyTx(tx, identity, binding, account, contact, stored, session)
			}
			return nil, &inboundContinuationPolicyStop{Reason: "booking offer is unavailable; request a fresh offer"}
		}
		key := booking.ReservationKey(offer.ID, token)
		if receipt, exists := proposal["reservation_key"]; exists {
			if receipt == key {
				return nil, a.verifyNativeBookingReceiptTx(tx, proposal, offer, choice)
			}
			return nil, &inboundContinuationPolicyStop{Reason: "this offer has already been used"}
		}
		reserved, err := booking.ReserveTx(tx, binding.OrganizationID, booking.ReserveInput{EventID: choice.Slot.EventID, ContactID: binding.ContactID,
			Quantity: 1, Status: models.BookingStatusReserved, Source: models.BookingSourceWhatsApp, IdempotencyKey: key, Expected: &choice.Slot, OfferExpiresAt: offer.ExpiresAt,
			Metadata: models.JSONB{"ai_generated": true, "ai_booking_offer_id": offer.ID.String(), "ai_booking_channel_account_id": binding.ChannelAccountID.String()}}, time.Now)
		if err != nil {
			if booking.IsOfferStale(err) {
				return a.prepareNativeBookingFreshOfferReplyTx(tx, identity, binding, account, contact, stored, session)
			}
			return nil, err
		}
		if !reserved.Created {
			return nil, &inboundContinuationManualReviewError{Reason: "reservation already exists without its atomic native receipt"}
		}
		if _, err := customeractivity.RecordTx(tx, binding.OrganizationID, customeractivity.Input{ContactID: binding.ContactID,
			EventType: models.CustomerActivityBookingCreated, Category: models.CustomerActivityCategoryBooking, Title: "Booking reserved by AI",
			ActorType: models.CustomerActivityActorSystem, SourceObjectType: "booking", SourceObjectID: &reserved.Booking.ID,
			OccurredAt: reserved.Now, Metadata: models.JSONB{"ai_generated": true, "offer_id": offer.ID.String()}, IdempotencyKey: "booking.created:" + reserved.Booking.ID.String()}); err != nil {
			return nil, err
		}
		message.Content = booking.RenderConfirmation(choice)
		proof.Kind, proof.OfferID, proof.BookingID = "confirmation", offer.ID, reserved.Booking.ID
		proposal["reservation_key"], proposal["booking_id"], proposal["confirmation_message_id"] = key, reserved.Booking.ID.String(), message.ID.String()
		data[nativeBookingProposalKey] = proposal
	} else if intent.Action == "offer" {
		delete(data, nativeBookingProposalKey)
		page, err := booking.ListSlots(tx, binding.OrganizationID, booking.SlotFilter{From: now, To: now.AddDate(0, 0, 90), Service: intent.Service, Date: intent.Date, Limit: 3})
		if err != nil {
			return nil, err
		}
		if len(page.Slots) == 0 {
			message.Content = "No matching scheduled places are available. Please choose another date or ask for staff."
		} else {
			offer, err := booking.NewOffer(binding, proof.InboundID, message.ID, page.Slots, now, 15*time.Minute)
			if err != nil {
				return nil, err
			}
			value, err := booking.OfferJSON(offer)
			if err != nil {
				return nil, err
			}
			data[nativeBookingProposalKey] = models.JSONB{"offer": value}
			message.Content, proof.Kind, proof.OfferID = booking.RenderOffer(offer), "offer", offer.ID
		}
	} else if intent.Action == "reply" {
		message.Content = intent.Reply
	} else {
		return nil, errors.New("unsupported native booking intent")
	}
	if strings.TrimSpace(message.Content) == "" {
		return nil, errors.New("native booking response is empty")
	}
	proof.ContentDigest = nativeBookingDigest(message.Content)
	proofValue, fingerprint, err := nativeBookingProofJSON(proof)
	if err != nil {
		return nil, err
	}
	message.Metadata = models.JSONB{nativeBookingProofKey: proofValue, nativeBookingFingerprintKey: fingerprint,
		"ai_generated": true, "sender_role": "bot", "inbound_continuation_action_key": identity.Key,
		"inbound_continuation_message_id": proof.InboundID.String(), "inbound_continuation_wamid": a.inboundContinuation.WAMID}
	if err := tx.Create(message).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&models.Contact{}).Where("id = ? AND organization_id = ?", contact.ID, binding.OrganizationID).
		Updates(map[string]any{"last_message_at": now, "last_message_preview": a.getMessagePreview(OutgoingMessageRequest{Type: models.MessageTypeText, Content: message.Content})}).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&models.ChatbotSession{}).Where("id = ? AND organization_id = ?", stored.ID, binding.OrganizationID).
		Updates(map[string]any{"session_data": data, "current_flow_id": session.CurrentFlowID, "current_step": session.CurrentStep}).Error; err != nil {
		return nil, err
	}
	job := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: binding.OrganizationID, Kind: inboundContinuationActionJobKind,
		AggregateType: "inbound_message_action", AggregateID: &proof.InboundID, RunAt: now, Status: models.ScheduledJobStatusProcessing,
		MaxAttempts: 1, Version: 1, IdempotencyKey: identity.Key, Payload: models.JSONB{"inbound_message_id": proof.InboundID.String(),
			"effect_kind": nativeBookingEffect, "action_scope": identity.Scope, "action_ordinal": identity.Ordinal, "state": inboundActionStateBookingPrepared,
			nativeBookingProofKey: proofValue, nativeBookingFingerprintKey: fingerprint}}
	if err := tx.Create(&job).Error; err != nil {
		return nil, err
	}
	session.SessionData = data
	return &nativeBookingPrepared{Message: *message, Proof: proof, Claim: &inboundContinuationActionClaim{ID: job.ID, Key: identity.Key, Kind: nativeBookingEffect,
		Scope: identity.Scope, Ordinal: identity.Ordinal, State: inboundActionStateBookingPrepared, Execute: true}}, nil
}

func (a *App) prepareNativeBookingFreshOfferReplyTx(tx *gorm.DB, identity inboundContinuationActionIdentity, binding booking.Binding,
	account *models.WhatsAppAccount, contact *models.Contact, stored, session *models.ChatbotSession) (*nativeBookingPrepared, error) {
	proposal, ok := nativeBookingObject(stored.SessionData[nativeBookingProposalKey])
	if !ok {
		return nil, errors.New("stale booking offer is unavailable")
	}
	offer, err := booking.ParseOffer(proposal["offer"])
	if err != nil {
		return nil, err
	}
	copy := *stored
	copy.SessionData = cloneInboundContinuationJSONB(stored.SessionData)
	delete(copy.SessionData, nativeBookingProposalKey)
	return a.prepareNativeBookingTx(tx, identity, binding, account, contact, &copy, session,
		booking.Intent{Action: "offer", Service: offer.Choices[0].Slot.ServiceName}, "", false)
}

func (a *App) verifyNativeBookingReceiptTx(tx *gorm.DB, proposal models.JSONB, offer booking.Offer, choice booking.Choice) error {
	fail := func() error {
		return &inboundContinuationManualReviewError{Reason: "consumed booking receipt is inconsistent"}
	}
	bookingText, _ := proposal["booking_id"].(string)
	messageText, _ := proposal["confirmation_message_id"].(string)
	bookingID, err := uuid.Parse(bookingText)
	if err != nil {
		return fail()
	}
	messageID, err := uuid.Parse(messageText)
	if err != nil {
		return fail()
	}
	var reserved models.Booking
	if err := tx.Where("id = ? AND organization_id = ?", bookingID, offer.Binding.OrganizationID).First(&reserved).Error; err != nil {
		return err
	}
	if reserved.ContactID != offer.Binding.ContactID || reserved.EventID != choice.Slot.EventID || reserved.Quantity != 1 ||
		reserved.IdempotencyKey != booking.ReservationKey(offer.ID, choice.Token) || reserved.BookedByID != nil || reserved.ContactPackageID != nil || reserved.Metadata["ai_generated"] != true {
		return fail()
	}
	var message models.Message
	if err := tx.Where("id = ? AND organization_id = ?", messageID, offer.Binding.OrganizationID).First(&message).Error; err != nil {
		return err
	}
	if err := validateNativeBookingMessageIdentity(&message, &message); err != nil {
		return err
	}
	proof, fingerprint, err := parseNativeBookingProof(message.Metadata[nativeBookingProofKey])
	if err != nil || proof.Kind != "confirmation" || proof.BookingID != bookingID || proof.OfferID != offer.ID || proof.Binding != offer.Binding || message.Content != booking.RenderConfirmation(choice) {
		return fail()
	}
	var job models.ScheduledJob
	if err := tx.Where("organization_id = ? AND idempotency_key = ?", offer.Binding.OrganizationID, proof.ActionKey).First(&job).Error; err != nil {
		return err
	}
	scope, _ := job.Payload["action_scope"].(string)
	ordinal, ok := inboundContinuationExactInteger(job.Payload["action_ordinal"])
	if !ok || validateInboundContinuationActionProof(&job, offer.Binding.OrganizationID, proof.InboundID, proof.ActionKey, nativeBookingEffect, scope, ordinal) != nil || job.Payload[nativeBookingFingerprintKey] != fingerprint {
		return fail()
	}
	jobProof, jobFingerprint, err := parseNativeBookingProof(job.Payload[nativeBookingProofKey])
	if err != nil || jobProof != proof || jobFingerprint != fingerprint {
		return fail()
	}
	return nil
}

func nativeBookingObject(value any) (models.JSONB, bool) {
	raw, err := json.Marshal(value)
	if err != nil || string(raw) == "null" {
		return nil, false
	}
	var object models.JSONB
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func nativeBookingManualReviewRequired(metadata models.JSONB) bool {
	for _, key := range []string{"manual_review_required", "ai_booking_manual_confirmation_required"} {
		if raw, exists := metadata[key]; exists {
			required, valid := raw.(bool)
			if !valid || required {
				return true
			}
		}
	}
	return false
}

func nativeBookingOfferWasSent(message *models.Message, offer booking.Offer) bool {
	if message == nil || message.ID != offer.OfferMessageID || message.OrganizationID != offer.Binding.OrganizationID ||
		message.ContactID != offer.Binding.ContactID || message.Direction != models.DirectionOutgoing || message.MessageType != models.MessageTypeText ||
		strings.TrimSpace(message.WhatsAppMessageID) == "" || (message.Status != models.MessageStatusSent && message.Status != models.MessageStatusDelivered && message.Status != models.MessageStatusRead) {
		return false
	}
	proof, fingerprint, err := parseNativeBookingProof(message.Metadata[nativeBookingProofKey])
	settledText, _ := message.Metadata[automaticAIDispatchSettledAtKey].(string)
	settled, settledErr := time.Parse(time.RFC3339Nano, settledText)
	return err == nil && proof.Kind == "offer" && proof.OfferID == offer.ID && proof.Binding == offer.Binding &&
		proof.MessageID == message.ID && proof.ContentDigest == nativeBookingDigest(message.Content) &&
		message.Content == booking.RenderOffer(offer) && message.Metadata[nativeBookingFingerprintKey] == fingerprint &&
		message.Metadata[automaticAIDispatchStateMetadataKey] == automaticAIDispatchStateResolved && settledErr == nil && !settled.Before(message.CreatedAt)
}

func (a *App) loadNativeBookingPreparedTx(tx *gorm.DB, identity inboundContinuationActionIdentity) (*nativeBookingPrepared, error) {
	var job models.ScheduledJob
	err := tx.Where("organization_id = ? AND idempotency_key = ?", a.inboundContinuation.OrganizationID, identity.Key).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validateInboundContinuationActionProof(&job, a.inboundContinuation.OrganizationID, a.inboundContinuation.MessageID,
		identity.Key, nativeBookingEffect, identity.Scope, identity.Ordinal); err != nil {
		return nil, err
	}
	if nativeBookingManualReviewRequired(job.Payload) {
		return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "booking action was marked for manual review"}
	}
	proof, fingerprint, err := parseNativeBookingProof(job.Payload[nativeBookingProofKey])
	if err != nil || job.Payload[nativeBookingFingerprintKey] != fingerprint || proof.ActionKey != identity.Key ||
		proof.InboundID != a.inboundContinuation.MessageID || proof.Binding.ContactID != a.inboundContinuation.ContactID ||
		proof.Binding.OrganizationID != a.inboundContinuation.OrganizationID {
		return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "booking action proof changed"}
	}
	var message models.Message
	if err := tx.Where("id = ? AND organization_id = ?", proof.MessageID, proof.Binding.OrganizationID).First(&message).Error; err != nil {
		return nil, err
	}
	if nativeBookingManualReviewRequired(message.Metadata) {
		return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "booking message was marked for manual review"}
	}
	messageProof, messageFingerprint, proofErr := parseNativeBookingProof(message.Metadata[nativeBookingProofKey])
	if proofErr != nil || messageProof != proof || messageFingerprint != fingerprint || message.Metadata[nativeBookingFingerprintKey] != fingerprint ||
		message.ContactID != proof.Binding.ContactID || message.Direction != models.DirectionOutgoing || message.MessageType != models.MessageTypeText ||
		message.SentByUserID != nil || nativeBookingDigest(message.Content) != proof.ContentDigest {
		return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "booking message fingerprint changed"}
	}
	state, _ := job.Payload["state"].(string)
	if state == inboundActionStateBookingPrepared {
		if job.Status != models.ScheduledJobStatusProcessing || message.Status != models.MessageStatusPending ||
			message.Metadata[automaticAIDispatchStateMetadataKey] != nil || message.WhatsAppMessageID != "" || job.Attempts != 0 {
			return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "prepared booking has attempt evidence"}
		}
	} else if state != inboundActionStateResolved || job.Status != models.ScheduledJobStatusCompleted {
		return nil, &inboundContinuationManualReviewError{ActionKey: identity.Key, Reason: "booking provider attempt is not retryable"}
	}
	return &nativeBookingPrepared{Message: message, Proof: proof, Claim: &inboundContinuationActionClaim{ID: job.ID, Key: identity.Key, Kind: nativeBookingEffect,
		Scope: identity.Scope, Ordinal: identity.Ordinal, State: state, Execute: state == inboundActionStateBookingPrepared, Result: inboundContinuationActionResult(job.Payload)}}, nil
}

// validateNativeBookingDispatchTx runs in BOTH physical dispatch phases.
// Non-booking native messages keep their existing semantics unchanged.
func (a *App) validateNativeBookingDispatchTx(tx *gorm.DB, message *models.Message) error {
	if _, exists := message.Metadata[nativeBookingProofKey]; !exists {
		return nil
	}
	proof, fingerprint, err := parseNativeBookingProof(message.Metadata[nativeBookingProofKey])
	if err != nil || message.Metadata[nativeBookingFingerprintKey] != fingerprint || proof.MessageID != message.ID || proof.ContentDigest != nativeBookingDigest(message.Content) {
		return &inboundContinuationManualReviewError{Reason: "native booking dispatch proof is invalid"}
	}
	current, _, err := a.nativeBookingAuthorityTx(tx, proof.NativeAccountID, proof.Binding.ScopeID)
	if err != nil {
		return err
	}
	if current != proof.Binding || a.inboundContinuation.MessageID != proof.InboundID {
		return &inboundContinuationPolicyStop{Reason: "native booking dispatch approval changed"}
	}
	var session models.ChatbotSession
	if err := tx.Select("session_data").Where("id = ? AND organization_id = ?", proof.Binding.ScopeID, proof.Binding.OrganizationID).First(&session).Error; err != nil {
		return err
	}
	if proof.Kind == "offer" || proof.Kind == "confirmation" {
		proposal, ok := nativeBookingObject(session.SessionData[nativeBookingProposalKey])
		if !ok {
			return &inboundContinuationPolicyStop{Reason: "native booking proposal was superseded"}
		}
		offer, err := booking.ParseOffer(proposal["offer"])
		if err != nil || offer.ID != proof.OfferID || offer.Binding != proof.Binding {
			return &inboundContinuationPolicyStop{Reason: "native booking proposal changed"}
		}
		if proof.Kind == "confirmation" && (proposal["booking_id"] != proof.BookingID.String() || proposal["confirmation_message_id"] != proof.MessageID.String()) {
			return &inboundContinuationManualReviewError{Reason: "booking confirmation receipt changed"}
		}
		if proof.Kind == "confirmation" {
			var reserved models.Booking
			if err := tx.Where("id = ? AND organization_id = ? AND contact_id = ?", proof.BookingID, proof.Binding.OrganizationID, proof.Binding.ContactID).First(&reserved).Error; err != nil {
				return err
			}
			if reserved.IdempotencyKey != proposal["reservation_key"] || reserved.Quantity != 1 || reserved.BookedByID != nil || reserved.ContactPackageID != nil ||
				(reserved.Status != models.BookingStatusReserved && reserved.Status != models.BookingStatusConfirmed) || reserved.Metadata["ai_generated"] != true {
				return &inboundContinuationManualReviewError{Reason: "booking changed before confirmation dispatch"}
			}
		}
		if proof.Kind == "offer" && (!time.Now().Before(offer.ExpiresAt) || proposal["reservation_key"] != nil) {
			return &inboundContinuationPolicyStop{Reason: "native booking offer expired or was consumed"}
		}
	}
	return nil
}

func validateNativeBookingMessageIdentity(stored, original *models.Message) error {
	_, originalBooking := original.Metadata[nativeBookingProofKey]
	_, storedBooking := stored.Metadata[nativeBookingProofKey]
	if !originalBooking && !storedBooking {
		return nil
	}
	proof, fingerprint, err := parseNativeBookingProof(stored.Metadata[nativeBookingProofKey])
	if err != nil || !originalBooking || !storedBooking || stored.Metadata[nativeBookingFingerprintKey] != fingerprint ||
		original.Metadata[nativeBookingFingerprintKey] != fingerprint || proof.MessageID != stored.ID || stored.ID != original.ID ||
		proof.Binding.OrganizationID != stored.OrganizationID || proof.Binding.ContactID != stored.ContactID ||
		stored.Direction != models.DirectionOutgoing || stored.MessageType != models.MessageTypeText || stored.SentByUserID != nil ||
		stored.Content != original.Content || proof.ContentDigest != nativeBookingDigest(stored.Content) ||
		stored.Metadata["ai_generated"] != true || stored.Metadata["sender_role"] != "bot" {
		return &inboundContinuationManualReviewError{Reason: "native booking pending message identity changed"}
	}
	return nil
}

// claimNativeBookingDispatchTx is part of the SAME transaction as the
// Message's dispatching marker. No intermediate generic pre_attempt can ever
// be mistaken for an unattempted prepared booking after a crash.
func (a *App) claimNativeBookingDispatchTx(tx *gorm.DB, message *models.Message) error {
	if _, exists := message.Metadata[nativeBookingProofKey]; !exists {
		return nil
	}
	proof, fingerprint, err := parseNativeBookingProof(message.Metadata[nativeBookingProofKey])
	if err != nil {
		return err
	}
	var job models.ScheduledJob
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND idempotency_key = ?", proof.Binding.OrganizationID, proof.ActionKey).First(&job).Error; err != nil {
		return err
	}
	if nativeBookingManualReviewRequired(job.Payload) || nativeBookingManualReviewRequired(message.Metadata) ||
		job.Kind != inboundContinuationActionJobKind || job.Status != models.ScheduledJobStatusProcessing || job.Attempts != 0 ||
		job.Payload["state"] != inboundActionStateBookingPrepared || job.Payload[nativeBookingFingerprintKey] != fingerprint {
		return &inboundContinuationManualReviewError{ActionKey: proof.ActionKey, Reason: "booking send is not demonstrably unattempted"}
	}
	payload := cloneInboundContinuationJSONB(job.Payload)
	payload["state"], payload["claimed_at"] = inboundActionStatePreAttempt, time.Now().UTC().Format(time.RFC3339Nano)
	return tx.Model(&models.ScheduledJob{}).Where("id = ? AND organization_id = ?", job.ID, job.OrganizationID).
		Updates(map[string]any{"payload": payload, "attempts": 1, "version": gorm.Expr("version + 1")}).Error
}

func (a *App) dispatchNativeBookingPrepared(prepared *nativeBookingPrepared, account *models.WhatsAppAccount, contact *models.Contact) error {
	if prepared == nil || prepared.Claim == nil {
		return errors.New("prepared native booking is required")
	}
	if !prepared.Claim.Execute {
		return a.recoverNativeBookingSend(prepared.Claim)
	}
	err := a.withInboundContinuationPhysicalAIAttempt(context.Background(), func() error {
		request := OutgoingMessageRequest{Account: account, Contact: contact, Type: models.MessageTypeText, Content: prepared.Message.Content}
		result, err := a.deliverAutomaticAIOutgoingMessage(context.Background(), &prepared.Message, request,
			func(ctx context.Context, current *models.Contact, native *models.WhatsAppAccount) (string, error) {
				return a.WhatsApp.SendTextMessage(ctx, a.toWhatsAppAccount(native), whatsapp.Recipient{Phone: current.PhoneNumber, BSUID: current.BSUID}, prepared.Message.Content)
			})
		if err != nil {
			return err
		}
		prepared.Claim.State = inboundActionStatePreAttempt
		state := inboundActionStateResolved
		if result.sendErr != nil || result.policyErr != nil || result.whatsAppMessageID == "" {
			state = inboundActionStateManualReview
		}
		if err := a.resolveInboundContinuationAction(context.Background(), prepared.Claim, state, models.JSONB{"outgoing_message_id": prepared.Message.ID.String()}); err != nil {
			return err
		}
		if state != inboundActionStateResolved {
			return &inboundContinuationManualReviewError{ActionKey: prepared.Claim.Key, Reason: "booking is retained; confirmation delivery requires staff review"}
		}
		prepared.Message.Status, prepared.Message.WhatsAppMessageID = models.MessageStatusSent, result.whatsAppMessageID
		a.finalizeMessageSend(&prepared.Message, request, ChatbotSendOptions(), result.whatsAppMessageID, nil, false)
		a.broadcastNewMessage(account.OrganizationID, &prepared.Message, contact)
		return nil
	})
	if err != nil {
		// Even a policy stop keeps the committed reservation and pending UI
		// message. Mark visible manual work; never unsend or roll back a booking.
		_ = a.markNativeBookingBlocked(prepared, err)
		return err
	}
	return a.recoverNativeBookingSend(prepared.Claim)
}

func (a *App) recoverNativeBookingSend(claim *inboundContinuationActionClaim) error {
	return database.WithTenantReadCommitted(a.rootApp().DB, a.inboundContinuation.OrganizationID, func(tx *gorm.DB) error {
		return a.scopedApp(tx, a.inboundContinuation.OrganizationID).recoverResolvedInboundContinuationSend(claim)
	})
}

func (a *App) markNativeBookingBlocked(prepared *nativeBookingPrepared, cause error) error {
	return database.WithTenantReadCommitted(a.rootApp().DB, prepared.Message.OrganizationID, func(tx *gorm.DB) error {
		return tx.Model(&models.Message{}).Where("id = ? AND organization_id = ?", prepared.Message.ID, prepared.Message.OrganizationID).
			Updates(map[string]any{"error_message": "AI booking message requires staff review", "metadata": gorm.Expr("COALESCE(metadata, '{}'::jsonb) || ?::jsonb", models.JSONB{"ai_booking_manual_confirmation_required": true})}).Error
	})
}
