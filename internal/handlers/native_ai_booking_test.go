package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/booking"
	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type nativeBookingTestFixture struct {
	app     *App
	org     *models.Organization
	account *models.WhatsAppAccount
	contact *models.Contact
	session *models.ChatbotSession
	shadow  *models.ChannelAccount
	event   models.BookingEvent
}

func nativeBookingFixture(t *testing.T) *nativeBookingTestFixture {
	t.Helper()
	app, org, account, contact, session := newGraphTestFixtures(t)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "bookings.enabled")
	event := createCapacityRaceBookingEvent(t, app.DB, org.ID, user.ID, 1)
	require.NoError(t, app.DB.Preload("Service").First(&event, "id = ?", event.ID).Error)
	var shadow *models.ChannelAccount
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		shadow, err = channel.EnsureLegacyMetaWhatsAppAccount(tx, channel.LegacyMetaAccountRef{ID: account.ID, OrganizationID: org.ID, Name: account.Name, Status: account.Status})
		if err != nil {
			return err
		}
		shadow.Config = cloneInboundContinuationJSONB(shadow.Config)
		shadow.Config[models.ChannelConfigAIBookingEnabled] = true
		shadow.Config[models.ChannelConfigAIBookingRevision] = uuid.NewString()
		shadow.Config[models.ChannelConfigAIBookingEnabledAt] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		shadow.Config[models.ChannelConfigAIBookingRouteBinding] = channel.LegacyMetaAIBookingRouteBinding(account)
		return tx.Model(shadow).Update("config", shadow.Config).Error
	}))
	require.NoError(t, app.DB.First(session, "id = ?", session.ID).Error)
	return &nativeBookingTestFixture{app, org, account, contact, session, shadow, event}
}

func (f *nativeBookingTestFixture) inbound(t *testing.T, text string) {
	t.Helper()
	bindNewGraphTestInbound(t, f.app, f.org, f.account, f.contact, text)
	require.NoError(t, f.app.DB.First(f.session, "id = ?", f.session.ID).Error)
}

func (f *nativeBookingTestFixture) identity(t *testing.T) inboundContinuationActionIdentity {
	t.Helper()
	f.app.inboundContinuation.actionScope, f.app.inboundContinuation.actionIndex = "native-booking-test", 0
	id, err := f.app.nextInboundContinuationActionKey(nativeBookingEffect, nil)
	require.NoError(t, err)
	return id
}

func (f *nativeBookingTestFixture) prepare(t *testing.T, intent booking.Intent, token string, confirmation bool) (*nativeBookingPrepared, error) {
	t.Helper()
	if intent.Action == "offer" && intent.Service == "" {
		intent.Service = f.event.Service.Name
	}
	id := f.identity(t)
	var prepared *nativeBookingPrepared
	err := database.WithTenantReadCommitted(f.app.rootApp().DB, f.org.ID, func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, f.org.ID); err != nil {
			return err
		}
		binding, account, err := f.app.nativeBookingAuthorityTx(tx, f.account.ID, f.session.ID)
		if err != nil {
			return err
		}
		var stored models.ChatbotSession
		if err := tx.First(&stored, "id = ?", f.session.ID).Error; err != nil {
			return err
		}
		prepared, err = f.app.prepareNativeBookingTx(tx, id, binding, account, f.contact, &stored, f.session, intent, token, confirmation)
		return err
	})
	return prepared, err
}

func (f *nativeBookingTestFixture) offer(t *testing.T, send bool) (*nativeBookingPrepared, booking.Offer) {
	t.Helper()
	prepared, err := f.prepare(t, booking.Intent{Action: "offer"}, "", false)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.Equal(t, "offer", prepared.Proof.Kind)
	if send {
		require.NoError(t, f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact))
	}
	var session models.ChatbotSession
	require.NoError(t, f.app.DB.First(&session, "id = ?", f.session.ID).Error)
	proposal, ok := nativeBookingObject(session.SessionData[nativeBookingProposalKey])
	require.True(t, ok)
	offer, err := booking.ParseOffer(proposal["offer"])
	require.NoError(t, err)
	return prepared, offer
}

func nativeBookingCount(t *testing.T, f *nativeBookingTestFixture, model any) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.app.DB.Model(model).Where("organization_id = ?", f.org.ID).Count(&count).Error)
	return count
}

func TestNativeAIBookingPreparedIntentAtomicReplayAndSystemActor(t *testing.T) {
	f := nativeBookingFixture(t)
	calls := nativePausePolicyProvider(t, f.app, nil)
	preparedOffer, offer := f.offer(t, false)
	require.Zero(t, calls.Load())
	require.Zero(t, nativeBookingCount(t, f, &models.Booking{}))
	var job models.ScheduledJob
	require.NoError(t, f.app.DB.First(&job, "id = ?", preparedOffer.Claim.ID).Error)
	require.Equal(t, inboundActionStateBookingPrepared, job.Payload["state"])
	require.Zero(t, job.Attempts)
	require.NoError(t, f.app.dispatchNativeBookingPrepared(preparedOffer, f.account, f.contact))
	require.EqualValues(t, 1, calls.Load())
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	prepared, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.Equal(t, "confirmation", prepared.Proof.Kind)
	require.EqualValues(t, 1, nativeBookingCount(t, f, &models.Booking{}))
	var reserved models.Booking
	require.NoError(t, f.app.DB.First(&reserved, "id = ?", prepared.Proof.BookingID).Error)
	require.Nil(t, reserved.BookedByID)
	require.Nil(t, reserved.UpdatedByID)
	require.Nil(t, reserved.ContactPackageID)
	require.Equal(t, models.BookingStatusReserved, reserved.Status)
	require.Equal(t, 1, reserved.Quantity)
	var activity models.CustomerActivityEvent
	require.NoError(t, f.app.DB.First(&activity, "organization_id = ? AND source_object_id = ?", f.org.ID, reserved.ID).Error)
	require.Equal(t, models.CustomerActivityActorSystem, activity.ActorType)
	require.Nil(t, activity.ActorUserID)
	require.EqualValues(t, 1, nativeBookingCount(t, f, &models.OutboxEvent{}))
	require.Zero(t, nativeBookingCount(t, f, &models.AuditLog{}))
	require.Nil(t, prepared.Message.SentByUserID)
	require.Equal(t, models.MessageStatusPending, prepared.Message.Status)
	firstMessageID := prepared.Message.ID
	replayed, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.NoError(t, err)
	require.Equal(t, firstMessageID, replayed.Message.ID)
	require.NoError(t, f.app.dispatchNativeBookingPrepared(replayed, f.account, f.contact))
	require.EqualValues(t, 2, calls.Load())
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	repeated, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.NoError(t, err)
	require.Nil(t, repeated, "a distinct confirmation inbound consumes the original receipt without a second send")
	require.EqualValues(t, 1, nativeBookingCount(t, f, &models.Booking{}))
	require.EqualValues(t, 2, calls.Load())
}

func TestNativeAIBookingPreparedRecoveryReusesOriginalMessage(t *testing.T) {
	f := nativeBookingFixture(t)
	calls := nativePausePolicyProvider(t, f.app, nil)
	prepared, _ := f.offer(t, false)
	original := prepared.Message.ID
	// A process restart reacquires the session, but is still allowed to send
	// this proven unattempted message if the current offer is unchanged.
	now := time.Now().UTC()
	require.NoError(t, f.app.DB.Model(f.session).Update("last_activity_at", now).Error)
	var loaded *nativeBookingPrepared
	id := f.identity(t)
	require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		loaded, err = f.app.loadNativeBookingPreparedTx(tx, id)
		return err
	}))
	require.Equal(t, original, loaded.Message.ID)
	require.NoError(t, f.app.dispatchNativeBookingPrepared(loaded, f.account, f.contact))
	require.EqualValues(t, 1, calls.Load())
	var again *nativeBookingPrepared
	require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		again, err = f.app.loadNativeBookingPreparedTx(tx, id)
		return err
	}))
	require.NoError(t, f.app.dispatchNativeBookingPrepared(again, f.account, f.contact))
	require.EqualValues(t, 1, calls.Load(), "resolved recovery must never resend")
}

func TestNativeAIBookingNeverReclaimsGenericOrUncertainAttempt(t *testing.T) {
	for _, state := range []string{inboundActionStatePreAttempt, "dispatching", "uncertain", inboundActionStateManualReview, "job_review_flag", "message_review_flag", "malformed_review_flag"} {
		t.Run(state, func(t *testing.T) {
			f := nativeBookingFixture(t)
			calls := nativePausePolicyProvider(t, f.app, nil)
			prepared, _ := f.offer(t, false)
			switch state {
			case "job_review_flag", "malformed_review_flag":
				value := any(true)
				if state == "malformed_review_flag" {
					value = "false"
				}
				require.NoError(t, f.app.DB.Model(&models.ScheduledJob{}).Where("id = ?", prepared.Claim.ID).
					Update("payload", gorm.Expr("payload || ?::jsonb", models.JSONB{"manual_review_required": value})).Error)
			case "message_review_flag":
				require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", prepared.Message.ID).
					Update("metadata", gorm.Expr("metadata || ?::jsonb", models.JSONB{"ai_booking_manual_confirmation_required": true})).Error)
			default:
				require.NoError(t, f.app.DB.Model(&models.ScheduledJob{}).Where("id = ?", prepared.Claim.ID).
					Update("payload", gorm.Expr("payload || ?::jsonb", models.JSONB{"state": state})).Error)
			}
			id := f.identity(t)
			_, err := f.app.loadNativeBookingPreparedTx(f.app.DB, id)
			var review *inboundContinuationManualReviewError
			require.ErrorAs(t, err, &review)
			// A previously loaded in-memory prepared intent cannot bypass the
			// current flags at the atomic dispatch claim either.
			require.Error(t, f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact))
			require.Zero(t, calls.Load())
		})
	}
}

func TestNativeAIBookingOfferMustHaveConfirmedUnchangedDelivery(t *testing.T) {
	f := nativeBookingFixture(t)
	_, offer := f.offer(t, false)
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	_, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.Error(t, err)
	require.Zero(t, nativeBookingCount(t, f, &models.Booking{}))
}

func TestNativeAIBookingAtomicConfirmationInsertionFailures(t *testing.T) {
	for _, table := range []string{"customer_activity_events", "outbox_events", "messages", "scheduled_jobs"} {
		t.Run(table, func(t *testing.T) {
			f := nativeBookingFixture(t)
			_, offer := f.offer(t, true)
			f.inbound(t, "BOOK "+offer.Choices[0].Token)
			before := bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "bookings", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts")
			callback := "fail_native_booking_" + uuid.NewString()
			require.NoError(t, f.app.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("synthetic atomic insertion failure"))
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
			_, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
			require.NoError(t, f.app.DB.Callback().Create().Remove(callback))
			require.Error(t, err)
			require.Equal(t, before, bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "bookings", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts"))
		})
	}
}

func TestNativeAIBookingCurrentAuthorityBlocksOffersAndDispatch(t *testing.T) {
	for _, change := range []string{"off", "epoch", "route", "shadow_binding", "deleted_shadow", "transfer", "inbound_suppression", "window", "license"} {
		t.Run(change, func(t *testing.T) {
			f := nativeBookingFixture(t)
			calls := nativePausePolicyProvider(t, f.app, nil)
			prepared, _ := f.offer(t, false)
			switch change {
			case "off":
				f.shadow.Config[models.ChannelConfigAIBookingEnabled] = false
			case "epoch":
				f.shadow.Config[models.ChannelConfigAIBookingEnabledAt] = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
			case "route":
				require.NoError(t, f.app.DB.Model(f.account).Update("phone_id", "different-route").Error)
			case "shadow_binding":
				require.NoError(t, f.app.DB.Model(f.shadow).Update("metadata", models.JSONB{"legacy_account_id": uuid.NewString()}).Error)
			case "deleted_shadow":
				require.NoError(t, f.app.DB.Delete(f.shadow).Error)
			case "transfer":
				nativePausePolicyTransfer(t, f.app, f.account, f.contact)
			case "inbound_suppression":
				require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.app.inboundContinuation.MessageID).Update("metadata", models.JSONB{incomingAutomaticAISuppressedKey: true}).Error)
			case "window":
				require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.app.inboundContinuation.MessageID).UpdateColumn("created_at", time.Now().Add(-25*time.Hour)).Error)
			case "license":
				require.NoError(t, f.app.DB.Model(&models.Subscription{}).Where("organization_id = ?", f.org.ID).Update("status", models.SubscriptionStatusCanceled).Error)
			}
			if change == "off" || change == "epoch" {
				require.NoError(t, f.app.DB.Model(f.shadow).Update("config", f.shadow.Config).Error)
			}
			err := f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact)
			require.Error(t, err)
			require.Zero(t, calls.Load())
			require.Zero(t, nativeBookingCount(t, f, &models.Booking{}))
			var pending models.Message
			require.NoError(t, f.app.DB.First(&pending, "id = ?", prepared.Message.ID).Error)
			require.Equal(t, true, pending.Metadata["ai_booking_manual_confirmation_required"])
		})
	}
}

func TestNativeAIBookingReservationSurvivesPauseWithoutConfirmationSend(t *testing.T) {
	f := nativeBookingFixture(t)
	calls := nativePausePolicyProvider(t, f.app, nil)
	_, offer := f.offer(t, true)
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	prepared, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.NoError(t, err)
	nativePausePolicyTransfer(t, f.app, f.account, f.contact)
	require.Error(t, f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact))
	require.EqualValues(t, 1, calls.Load(), "only the offer was sent")
	require.EqualValues(t, 1, nativeBookingCount(t, f, &models.Booking{}))
	var reserved models.Booking
	require.NoError(t, f.app.DB.First(&reserved, "id = ?", prepared.Proof.BookingID).Error)
	require.Equal(t, models.BookingStatusReserved, reserved.Status)
}

func TestNativeAIBookingHandoffIsDurableWithoutAcknowledgment(t *testing.T) {
	t.Run("committed_system_activity", func(t *testing.T) {
		f := nativeBookingFixture(t)
		calls := nativePausePolicyProvider(t, f.app, nil)
		f.app.HTTPClient = graphExternalAPIClient(func(*http.Request) (int, string, error) {
			return http.StatusOK, `{"choices":[{"message":{"content":"{\"action\":\"handoff\",\"reply\":\"\",\"service\":\"\",\"date\":\"\"}"}}]}`, nil
		})
		settings := &models.ChatbotSettings{OrganizationID: f.org.ID, AI: models.AIConfig{Enabled: true, Provider: models.AIProviderOpenAI, APIKey: "synthetic", Model: "test"}}
		handled, _, err := f.app.processNativeAIBooking(f.account, f.contact, f.session, settings, "Please get staff")
		require.True(t, handled)
		require.True(t, inboundContinuationStoppedByPolicy(err))
		require.EqualValues(t, 1, nativeBookingCount(t, f, &models.AgentTransfer{}))
		require.Zero(t, calls.Load())
		var contact models.Contact
		require.NoError(t, f.app.DB.First(&contact, "id = ?", f.contact.ID).Error)
		require.NotEmpty(t, contact.Metadata[contacthandoff.CutoffKey])
		var session models.ChatbotSession
		require.NoError(t, f.app.DB.First(&session, "id = ?", f.session.ID).Error)
		require.Equal(t, models.SessionStatusCancelled, session.Status)
		require.Equal(t, true, session.SessionData[contacthandoff.ProposalInvalidatedKey])
		var transfer models.AgentTransfer
		require.NoError(t, f.app.DB.Where("organization_id = ?", f.org.ID).First(&transfer).Error)
		require.Equal(t, f.contact.ID, transfer.ContactID)
		require.Equal(t, models.TransferSourceFlow, transfer.Source)
		require.Nil(t, transfer.AgentID)
		var event models.CustomerActivityEvent
		require.NoError(t, f.app.DB.Where("organization_id = ?", f.org.ID).First(&event).Error)
		require.EqualValues(t, 1, nativeBookingCount(t, f, &models.CustomerActivityEvent{}))
		require.Equal(t, models.CustomerActivityEventType("booking.staff_requested"), event.EventType)
		require.Equal(t, models.CustomerActivityCategoryBooking, event.Category)
		require.Equal(t, models.CustomerActivityActorSystem, event.ActorType)
		require.Nil(t, event.ActorUserID)
		require.Equal(t, f.contact.ID, event.ContactID)
		require.Equal(t, "agent_transfer", event.SourceObjectType)
		require.Equal(t, &transfer.ID, event.SourceObjectID)
		require.Equal(t, "ai-booking-handoff:"+f.app.inboundContinuation.MessageID.String(), event.IdempotencyKey)
		require.Equal(t, "ai_booking", event.Metadata["source"])
		require.Equal(t, transfer.Notes, event.Metadata["reason"])
		require.Equal(t, f.app.inboundContinuation.MessageID.String(), event.Metadata["inbound_message_id"])
		var outbox models.OutboxEvent
		require.NoError(t, f.app.DB.Where("organization_id = ?", f.org.ID).First(&outbox).Error)
		require.EqualValues(t, 1, nativeBookingCount(t, f, &models.OutboxEvent{}))
		require.Equal(t, "booking.staff_requested", outbox.EventType)
		require.Equal(t, "agent_transfer", outbox.AggregateType)
		require.Equal(t, &transfer.ID, outbox.AggregateID)
		require.Equal(t, "customer-activity-webhook:"+event.ID.String(), outbox.IdempotencyKey)
		require.Equal(t, event.ID.String(), outbox.Payload["activity_event_id"])
		require.Equal(t, string(models.CustomerActivityActorSystem), outbox.Payload["actor_type"])
		require.Equal(t, "", outbox.Payload["actor_user_id"])
		require.Equal(t, event.Metadata, models.JSONB(outbox.Payload["metadata"].(map[string]any)))
		beforeReplay := bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "agent_transfers", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts")
		_, _, _ = f.app.processNativeAIBooking(f.account, f.contact, f.session, settings, "Please get staff")
		require.Equal(t, beforeReplay, bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "agent_transfers", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts"))
		require.Zero(t, calls.Load(), "neither the handoff nor its replay acknowledges through the provider")
	})
	for _, table := range []string{"customer_activity_events", "outbox_events"} {
		t.Run("rollback_"+table, func(t *testing.T) {
			f := nativeBookingFixture(t)
			calls := nativePausePolicyProvider(t, f.app, nil)
			const request = "Please speak to staff"
			require.True(t, booking.RequiresHandoff(request))
			f.inbound(t, request)
			before := bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "agent_transfers", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts")
			injected := errors.New("synthetic native handoff fact insertion failure")
			callback := "fail_native_booking_handoff_" + uuid.NewString()
			injections := 0
			require.NoError(t, f.app.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					injections++
					tx.AddError(injected)
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
			handled, _, err := f.app.processNativeAIBooking(f.account, f.contact, f.session, nil, request)
			require.NoError(t, f.app.DB.Callback().Create().Remove(callback))
			require.True(t, handled)
			require.ErrorIs(t, err, injected)
			require.Equal(t, 1, injections, "the rollback must reach the exact intended insertion")
			require.Equal(t, before, bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "agent_transfers", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts"))
			require.Zero(t, calls.Load())
		})
	}
}

func TestNativeAIBookingGraphYieldPreservesOfferAndRejectsModelConfirmation(t *testing.T) {
	f := nativeBookingFixture(t)
	calls := nativePausePolicyProvider(t, f.app, nil)
	modelCalls := 0
	f.app.HTTPClient = graphExternalAPIClient(func(*http.Request) (int, string, error) {
		modelCalls++
		intentRaw, _ := json.Marshal(booking.Intent{Action: "offer", Service: f.event.Service.Name})
		content := string(intentRaw)
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
		return http.StatusOK, string(raw), nil
	})
	createChatbotSettings(t, f.app, f.org.ID, f.account.Name, models.AIConfig{Enabled: true, Provider: models.AIProviderOpenAI, APIKey: "synthetic", Model: "test"})
	flow := newAIResponseFlow(t, f.app, f.org, f.account, "")
	f.session.CurrentFlowID = &flow.ID
	require.NoError(t, f.app.runChatGraph(f.account, f.contact, f.session, flow, "I'd like a booking", "", nil))
	require.Equal(t, models.SessionStatusActive, f.session.Status)
	require.Equal(t, "ai", f.session.CurrentStep)
	require.True(t, nativeBookingProposalPending(f.session.SessionData))
	require.EqualValues(t, 1, calls.Load())
	var fresh models.ChatbotSession
	require.NoError(t, f.app.DB.First(&fresh, "id = ?", f.session.ID).Error)
	proposal, ok := nativeBookingObject(fresh.SessionData[nativeBookingProposalKey])
	require.True(t, ok)
	offer, err := booking.ParseOffer(proposal["offer"])
	require.NoError(t, err)
	// Public stale checkpoint writes cannot erase, fabricate or replace the
	// separately committed proposal, even across graph yield/recovery.
	fresh.SessionData[nativeBookingProposalKey] = map[string]any{"forged": true}
	require.NoError(t, f.app.persistChatSession(&fresh))
	require.NoError(t, f.app.DB.First(f.session, "id = ?", f.session.ID).Error)
	proposal, ok = nativeBookingObject(f.session.SessionData[nativeBookingProposalKey])
	require.True(t, ok)
	restored, err := booking.ParseOffer(proposal["offer"])
	require.NoError(t, err)
	require.Equal(t, offer.ID, restored.ID)
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	require.NoError(t, f.app.runChatGraph(f.account, f.contact, f.session, flow, "BOOK "+offer.Choices[0].Token, "", nil))
	require.Equal(t, 1, modelCalls, "confirmation must not call the model")
	require.EqualValues(t, 1, nativeBookingCount(t, f, &models.Booking{}))
	require.EqualValues(t, 2, calls.Load(), "no second ordinary graph response")
}

func TestNativeAIBookingProofRejectsTampering(t *testing.T) {
	proof := nativeBookingProof{Version: 1, Kind: "confirmation", Binding: booking.Binding{OrganizationID: uuid.New(), ChannelAccountID: uuid.New(), ContactID: uuid.New(), ScopeID: uuid.New(), Channel: "whatsapp", Revision: uuid.NewString(), RoutingDigest: strings.Repeat("a", 64)},
		NativeAccountID: uuid.New(), InboundID: uuid.New(), MessageID: uuid.New(), OfferID: uuid.New(), BookingID: uuid.New(), ActionKey: "inbound-action:test", ContentDigest: nativeBookingDigest("Reserved"), SessionAcquiredAt: time.Now().UTC()}
	value, fingerprint, err := nativeBookingProofJSON(proof)
	require.NoError(t, err)
	message := &models.Message{BaseModel: models.BaseModel{ID: proof.MessageID}, OrganizationID: proof.Binding.OrganizationID, ContactID: proof.Binding.ContactID,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText, Content: "Reserved", Metadata: models.JSONB{nativeBookingProofKey: value, nativeBookingFingerprintKey: fingerprint, "ai_generated": true, "sender_role": "bot"}}
	require.NoError(t, validateNativeBookingMessageIdentity(message, message))
	changed := *message
	changed.Content = "Forged confirmation"
	require.Error(t, validateNativeBookingMessageIdentity(&changed, message))
	changed = *message
	staff := uuid.New()
	changed.SentByUserID = &staff
	require.Error(t, validateNativeBookingMessageIdentity(&changed, message))
	changed = *message
	changed.ContactID = uuid.New()
	require.Error(t, validateNativeBookingMessageIdentity(&changed, message))
	value["confirm"] = true
	_, _, err = parseNativeBookingProof(value)
	require.Error(t, err)
	_, ok := booking.ConfirmationToken("yes")
	require.False(t, ok)
	_, ok = booking.ConfirmationToken(`{"confirm":true}`)
	require.False(t, ok)
}

func TestNativeAIBookingShadowHarnessDoesNotWriteOrSend(t *testing.T) {
	// The approved shadow stage is a pure local harness, not live traffic or
	// a hidden runtime mode. It has no App/DB/provider handle to mutate.
	intent, err := booking.ParseIntent(`{"action":"offer","reply":"","service":"Consultation","date":"2026-10-01"}`)
	require.NoError(t, err)
	require.Equal(t, "offer", intent.Action)
	_, err = booking.ParseIntent(`{"action":"offer","reply":"","service":"","date":"","event_id":"invented"}`)
	require.Error(t, err)
	_, err = booking.ParseIntent(`{"action":"confirm","reply":"","service":"","date":""}`)
	require.Error(t, err)
}

func TestNativeAIBookingRejectsOrphanReservationAndCorruptReceipt(t *testing.T) {
	for _, scenario := range []string{"orphan", "message", "action"} {
		t.Run(scenario, func(t *testing.T) {
			f := nativeBookingFixture(t)
			_, offer := f.offer(t, true)
			choice := offer.Choices[0]
			f.inbound(t, "BOOK "+choice.Token)
			if scenario == "orphan" {
				require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
					_, err := booking.ReserveTx(tx, f.org.ID, booking.ReserveInput{EventID: choice.Slot.EventID, ContactID: f.contact.ID, Quantity: 1, Status: models.BookingStatusReserved,
						Source: models.BookingSourceWhatsApp, IdempotencyKey: booking.ReservationKey(offer.ID, choice.Token), Expected: &choice.Slot, OfferExpiresAt: offer.ExpiresAt, Metadata: models.JSONB{"ai_generated": true}}, time.Now)
					return err
				}))
			} else {
				prepared, err := f.prepare(t, booking.Intent{}, choice.Token, true)
				require.NoError(t, err)
				if scenario == "message" {
					require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", prepared.Message.ID).Update("content", "corrupt confirmation").Error)
				} else {
					require.NoError(t, f.app.DB.Model(&models.ScheduledJob{}).Where("id = ?", prepared.Claim.ID).Update("payload", gorm.Expr("payload - ?", nativeBookingFingerprintKey)).Error)
				}
				f.inbound(t, "BOOK "+choice.Token)
			}
			before := bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "bookings", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts")
			_, err := f.prepare(t, booking.Intent{}, choice.Token, true)
			require.Error(t, err)
			require.Equal(t, before, bookingCommerceRowsSnapshot(t, f.app.DB, f.org.ID, "bookings", "customer_activity_events", "outbox_events", "messages", "scheduled_jobs", "chatbot_sessions", "contacts"))
			require.EqualValues(t, 1, nativeBookingCount(t, f, &models.Booking{}))
		})
	}
}

func TestNativeAIBookingStaleSlotProducesFreshAuthoritativeOffer(t *testing.T) {
	f := nativeBookingFixture(t)
	_, offer := f.offer(t, true)
	require.NoError(t, f.app.DB.Model(&models.BookingEvent{}).Where("id = ?", f.event.ID).Update("status", models.BookingEventStatusCancelled).Error)
	next := models.BookingEvent{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.org.ID, ServiceID: f.event.ServiceID, ResourceID: f.event.ResourceID,
		StartsAt: f.event.StartsAt.Add(time.Hour), EndsAt: f.event.EndsAt.Add(time.Hour), Capacity: 1, Status: models.BookingEventStatusScheduled, Version: 1, Metadata: models.JSONB{}}
	require.NoError(t, f.app.DB.Create(&next).Error)
	f.inbound(t, "BOOK "+offer.Choices[0].Token)
	prepared, err := f.prepare(t, booking.Intent{}, offer.Choices[0].Token, true)
	require.NoError(t, err)
	require.Equal(t, "offer", prepared.Proof.Kind)
	require.NotEqual(t, offer.ID, prepared.Proof.OfferID)
	require.Zero(t, nativeBookingCount(t, f, &models.Booking{}))
	var session models.ChatbotSession
	require.NoError(t, f.app.DB.First(&session, "id = ?", f.session.ID).Error)
	proposal, ok := nativeBookingObject(session.SessionData[nativeBookingProposalKey])
	require.True(t, ok)
	fresh, err := booking.ParseOffer(proposal["offer"])
	require.NoError(t, err)
	require.Len(t, fresh.Choices, 1)
	require.Equal(t, next.ID, fresh.Choices[0].Slot.EventID)
	require.NotEqual(t, offer.Choices[0].Token, fresh.Choices[0].Token)
}

func TestNativeAIBookingFallbackOrdinaryReplyAndDefaultOff(t *testing.T) {
	f := nativeBookingFixture(t)
	calls := nativePausePolicyProvider(t, f.app, nil)
	modelCalls := 0
	f.app.HTTPClient = graphExternalAPIClient(func(*http.Request) (int, string, error) {
		modelCalls++
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"action":"reply","reply":"How can I help?","service":"","date":""}`}}}})
		return http.StatusOK, string(raw), nil
	})
	settings := &models.ChatbotSettings{OrganizationID: f.org.ID, AI: models.AIConfig{Enabled: true, Provider: models.AIProviderOpenAI, APIKey: "synthetic", Model: "test"}}
	handled, awaiting, err := f.app.processNativeAIBooking(f.account, f.contact, f.session, settings, "Hello")
	require.NoError(t, err)
	require.True(t, handled)
	require.False(t, awaiting)
	require.Equal(t, 1, modelCalls)
	require.EqualValues(t, 1, calls.Load())
	require.Zero(t, nativeBookingCount(t, f, &models.Booking{}))
	f.inbound(t, "Another ordinary hello")
	delete(f.shadow.Config, models.ChannelConfigAIBookingEnabled)
	require.NoError(t, f.app.DB.Model(f.shadow).Update("config", f.shadow.Config).Error)
	handled, _, err = f.app.processNativeAIBooking(f.account, f.contact, f.session, settings, "Another ordinary hello")
	require.NoError(t, err)
	require.False(t, handled)
	require.Equal(t, 1, modelCalls)
	require.EqualValues(t, 1, calls.Load())
}

func TestNativeAIBookingCheckpointSurvivesOlderOuterSnapshot(t *testing.T) {
	f := nativeBookingFixture(t)
	require.NoError(t, f.app.DB.Transaction(func(outer *gorm.DB) error {
		require.NoError(t, outer.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ").Error)
		var old models.ChatbotSession
		require.NoError(t, outer.First(&old, "id = ?", f.session.ID).Error)
		// A normal session history FK may hold KEY SHARE in the outer transaction.
		require.NoError(t, outer.Create(&models.ChatbotSessionMessage{BaseModel: models.BaseModel{ID: uuid.New()}, SessionID: f.session.ID, Direction: models.DirectionIncoming, Message: "Synthetic older snapshot", StepName: "test"}).Error)
		_, offer := f.offer(t, false)
		scoped := f.app.scopedApp(outer, f.org.ID)
		scoped.inboundContinuation.nativeBookingCheckpoint = true
		old.SessionData = models.JSONB{"ordinary_variable": "preserved", nativeBookingProposalKey: models.JSONB{"forged": true}}
		require.NoError(t, scoped.persistChatSession(&old))
		var current models.ChatbotSession
		require.NoError(t, f.app.DB.First(&current, "id = ?", f.session.ID).Error)
		proposal, ok := nativeBookingObject(current.SessionData[nativeBookingProposalKey])
		require.True(t, ok)
		preserved, err := booking.ParseOffer(proposal["offer"])
		require.NoError(t, err)
		require.Equal(t, offer.ID, preserved.ID)
		require.Equal(t, "preserved", current.SessionData["ordinary_variable"])
		return nil
	}))
	var stale models.ChatbotSession
	require.NoError(t, f.app.DB.First(&stale, "id = ?", f.session.ID).Error)
	require.Contains(t, stale.SessionData, nativeBookingProposalKey)
	require.NoError(t, f.app.DB.Model(&models.ChatbotSession{}).Where("id = ? AND organization_id = ?", stale.ID, f.org.ID).
		UpdateColumn("session_data", models.JSONB{"ordinary_variable": "before_checkpoint"}).Error)
	require.Contains(t, stale.SessionData, nativeBookingProposalKey)
	stale.SessionData["ordinary_variable"] = "after_checkpoint"
	require.NoError(t, f.app.persistChatSession(&stale))
	var withoutProposal models.ChatbotSession
	require.NoError(t, f.app.DB.First(&withoutProposal, "id = ?", stale.ID).Error)
	require.NotContains(t, withoutProposal.SessionData, nativeBookingProposalKey, "a stale checkpoint cannot resurrect removed booking authority")
	require.Equal(t, "after_checkpoint", withoutProposal.SessionData["ordinary_variable"])
}
