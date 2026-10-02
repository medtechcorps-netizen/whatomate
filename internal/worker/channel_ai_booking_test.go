package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/booking"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelAIBookingPureProtocolBoundary(t *testing.T) {
	fixture := channelAIReplySnapshot{}
	_, ok := channelAIBookingLocalResponse(fixture)
	require.False(t, ok)
	fixture.BookingGeneration = "synthetic"
	fixture.UserText = "I have chest pain"
	raw, ok := channelAIBookingLocalResponse(fixture)
	require.True(t, ok)
	require.Equal(t, channelAIBookingHandoffResponse, raw)
	fixture.UserText = "yes"
	_, ok = channelAIBookingLocalResponse(fixture)
	require.False(t, ok)
	for _, raw := range []string{"```json\n{\"action\":\"handoff\"}\n```", "{\"action\":\"reply\",\"reply\":\"hi\",\"action\":\"offer\"}", "{\"action\":\"offer\",\"service\":\"test\",\"confirm\":true}", "{\"action\":\"reply\",\"reply\":\"hi\"}\x00"} {
		_, err := booking.ParseIntent(normalizeChannelAIBookingResponse(raw))
		require.Error(t, err)
	}
	assert.Equal(t, channelAIBookingHandoffResponse, normalizeChannelAIBookingResponse(strings.Repeat("x", 8193)))
	require.False(t, channelAIBookingGenerationMatches(nil, fixture))
	require.False(t, channelAIBookingGenerationMatches(&models.ScheduledJob{Payload: models.JSONB{channelAIBookingGenerationKey: true}}, fixture))
	// Provider CreatedAt cannot turn a pre-opt-in admitted message into new work.
	epoch := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	admitted := epoch.Add(-time.Second)
	org, account, conversation, contact := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	boundary := channelAIReplySnapshot{
		Account: models.ChannelAccount{BaseModel: models.BaseModel{ID: account}, OrganizationID: org, Channel: models.ChannelInstagram, Provider: "relay", Status: models.ChannelAccountStatusActive, Config: models.JSONB{
			models.ChannelConfigAIBookingEnabled: true, models.ChannelConfigAIBookingRevision: uuid.NewString(), models.ChannelConfigAIBookingEnabledAt: epoch.Format(time.RFC3339Nano)}},
		Conversation: models.InboxConversation{BaseModel: models.BaseModel{ID: conversation}, OrganizationID: org, ChannelAccountID: account, ContactID: contact, Channel: models.ChannelInstagram},
		Contact:      models.Contact{BaseModel: models.BaseModel{ID: contact}, OrganizationID: org},
		Inbound:      models.Message{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: epoch.Add(time.Minute)}, IngestedAt: &admitted, OrganizationID: org, ContactID: contact, InboxConversationID: &conversation},
	}
	_, enabled, err := channelAIBookingBinding(boundary)
	require.NoError(t, err)
	require.False(t, enabled)
	admitted = epoch.Add(time.Second)
	_, enabled, err = channelAIBookingBinding(boundary)
	require.NoError(t, err)
	require.True(t, enabled)
}

func createChannelAIBookingFixture(t *testing.T, db *gorm.DB) (*channelAIReplyFixture, *models.BookingEvent) {
	t.Helper()
	f := createChannelAIReplyWorkerFixture(t, db)
	f.Account.Config[models.ChannelConfigAIBookingEnabled] = true
	f.Account.Config[models.ChannelConfigAIBookingRevision] = uuid.NewString()
	f.Account.Config[models.ChannelConfigAIBookingEnabledAt] = f.Inbound.CreatedAt.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	require.NoError(t, db.Model(f.Account).Update("config", f.Account.Config).Error)
	require.NoError(t, db.Model(&models.Subscription{}).Where("organization_id = ?", f.Organization.ID).
		Update("entitlements_snapshot", models.JSONB{"omnichannel.enabled": true, "bookings.enabled": true}).Error)
	service := models.BookingService{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.Organization.ID, Name: "Synthetic consultation", Kind: models.BookingServiceKindAppointment,
		DurationMinutes: 30, DefaultCapacity: 1, Currency: "MYR", IsActive: true, Version: 1}
	resource := models.BookingResource{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.Organization.ID, Name: "Synthetic practitioner", Kind: models.BookingResourceKindPractitioner,
		Timezone: "Asia/Kuala_Lumpur", Location: "Synthetic room", IsActive: true, Version: 1}
	require.NoError(t, db.Create(&service).Error)
	require.NoError(t, db.Create(&resource).Error)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	event := &models.BookingEvent{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.Organization.ID, ServiceID: service.ID, ResourceID: resource.ID,
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), Capacity: 1, Status: models.BookingEventStatusScheduled, Version: 1}
	require.NoError(t, db.Create(event).Error)
	return f, event
}

func processChannelAIBookingFixture(t *testing.T, worker *Worker, f *channelAIReplyFixture) error {
	t.Helper()
	owner := "ai-booking-test-" + uuid.NewString()
	id, claimed, err := worker.claimChannelAIReplyJob(f.Organization.ID, owner)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, f.Job.ID, id)
	return worker.processChannelAIReplyJob(context.Background(), f.Organization.ID, id, owner)
}

func reloadChannelAIBookingState(t *testing.T, db *gorm.DB, f *channelAIReplyFixture) *channelAIBookingSession {
	t.Helper()
	require.NoError(t, db.First(f.Conversation, "id = ?", f.Conversation.ID).Error)
	state, err := parseChannelAIBookingSession(f.Conversation.Metadata)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

func markChannelAIBookingOfferSent(t *testing.T, db *gorm.DB, state *channelAIBookingSession) {
	t.Helper()
	require.NoError(t, db.Model(&models.Message{}).Where("id = ?", state.Offer.OfferMessageID).Update("status", models.MessageStatusSent).Error)
	require.NoError(t, db.Model(&models.OutboxJob{}).Where("message_id = ?", state.Offer.OfferMessageID).Update("status", models.OutboxJobStatusSent).Error)
}

func nextChannelAIBookingInbound(t *testing.T, db *gorm.DB, f *channelAIReplyFixture, text string) {
	t.Helper()
	now := time.Now().UTC()
	in := &models.Message{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now}, OrganizationID: f.Organization.ID,
		WhatsAppAccount: f.Account.Name, ContactID: f.Contact.ID, WhatsAppMessageID: "synthetic-" + uuid.NewString(), ConversationID: f.Conversation.ExternalConversationID,
		InboxConversationID: &f.Conversation.ID, Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: text, Status: models.MessageStatusReceived, Metadata: models.JSONB{}}
	require.NoError(t, db.Create(in).Error)
	job := &models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.Organization.ID, Kind: models.ScheduledJobKindChannelAIReply,
		AggregateType: models.ChannelAIReplyAggregateType, AggregateID: &in.ID, RunAt: now.Add(-time.Second), Status: models.ScheduledJobStatusPending,
		MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(in.ID), Payload: models.JSONB{
			"organization_id": f.Organization.ID.String(), "channel_account_id": f.Account.ID.String(), "conversation_id": f.Conversation.ID.String(),
			"inbound_message_id": in.ID.String(), "service_window_at": now}, Version: 1}
	require.NoError(t, db.Create(job).Error)
	f.Inbound = in
	f.Job = job
}

func TestChannelAIBookingOfferConfirmAndDistinctReplayAreAtomic(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, event := createChannelAIBookingFixture(t, db)
	// A delayed provider display timestamp does not change the server's newer
	// admission epoch. Both generation and prompt must use that same authority.
	require.NoError(t, db.Model(f.Inbound).Update("created_at", f.Inbound.CreatedAt.Add(-2*time.Minute)).Error)
	var calls atomic.Int32
	w := channelAIReplyTestWorker(db, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "keys action, reply, service, date")
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state := reloadChannelAIBookingState(t, db, f)
	require.Len(t, state.Offer.Choices, 1)
	require.Nil(t, state.Receipt)
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	require.Zero(t, count)
	markChannelAIBookingOfferSent(t, db, state)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state = reloadChannelAIBookingState(t, db, f)
	require.NotNil(t, state.Receipt)
	var reserved models.Booking
	require.NoError(t, db.First(&reserved, "id = ?", state.Receipt.BookingID).Error)
	assert.Equal(t, event.ID, reserved.EventID)
	assert.Equal(t, models.BookingStatusReserved, reserved.Status)
	assert.Equal(t, 1, reserved.Quantity)
	assert.Nil(t, reserved.BookedByID)
	assert.Nil(t, reserved.UpdatedByID)
	assert.Nil(t, reserved.ContactPackageID)
	var activity models.CustomerActivityEvent
	require.NoError(t, db.Where("organization_id = ? AND source_object_id = ?", f.Organization.ID, reserved.ID).First(&activity).Error)
	assert.Equal(t, models.CustomerActivityActorSystem, activity.ActorType)
	assert.Nil(t, activity.ActorUserID)
	for _, id := range []uuid.UUID{state.Offer.OfferMessageID, state.Receipt.MessageID} {
		var message models.Message
		var job models.OutboxJob
		require.NoError(t, db.First(&message, "id = ?", id).Error)
		require.NoError(t, db.Where("message_id = ?", id).First(&job).Error)
		require.True(t, strings.HasPrefix(job.IdempotencyKey, models.ChannelAIReplyKeyPrefix))
		require.Equal(t, true, message.Metadata["ai_generated"])
		require.Equal(t, "bot", message.Metadata["sender_role"])
		outbound, err := channelOutboundMessageForJob(&job)
		require.NoError(t, err)
		require.Equal(t, true, outbound.Metadata["ai_generated"])
		require.Equal(t, "bot", outbound.Metadata["sender_role"])
		require.True(t, isChannelAIReplyOutbox(&job, outbound))
		require.Nil(t, message.SentByUserID)
	}
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Receipt.ChoiceToken)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	require.NoError(t, db.Model(&models.OutboxJob{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.EqualValues(t, 2, count)
	assert.EqualValues(t, 1, calls.Load(), "server confirmation and replay must not call Qwen")
}

func TestChannelAIBookingUnsentAmbiguousAndInventedConfirmationDoNotReserve(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state := reloadChannelAIBookingState(t, db, f)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	markChannelAIBookingOfferSent(t, db, state)
	require.NoError(t, db.Model(&models.OutboxJob{}).Where("message_id = ?", state.Offer.OfferMessageID).Update("status", models.OutboxJobStatusFailed).Error)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count)
	assert.Nil(t, reloadChannelAIBookingState(t, db, f).Receipt)
	for _, mode := range []string{"invented_token", "ambiguous_yes", "model_confirm"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := createChannelAIBookingFixture(t, db)
			var calls atomic.Int32
			w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
				}
				if mode == "model_confirm" {
					return channelAIReplyQwenResponse(`{"action":"offer","service":"Synthetic consultation","confirm":true}`), nil
				}
				return channelAIReplyQwenResponse(`{"action":"reply","reply":"Please choose a displayed option.","service":"","date":""}`), nil
			})
			require.NoError(t, processChannelAIBookingFixture(t, w, f))
			state := reloadChannelAIBookingState(t, db, f)
			markChannelAIBookingOfferSent(t, db, state)
			text := "yes"
			if mode == "invented_token" {
				text = "BOOK " + strings.Repeat("A", 43)
				require.NotEqual(t, text, "BOOK "+state.Offer.Choices[0].Token)
			}
			nextChannelAIBookingInbound(t, db, f, text)
			require.NoError(t, processChannelAIBookingFixture(t, w, f))
			var count int64
			require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
			require.Zero(t, count)
			require.Nil(t, reloadChannelAIBookingState(t, db, f).Receipt)
		})
	}
}

func TestChannelAIBookingChangedMaterialFactsProduceFreshOffer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, event := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	old := reloadChannelAIBookingState(t, db, f)
	markChannelAIBookingOfferSent(t, db, old)
	require.NoError(t, db.Model(event).Updates(map[string]any{"location": "Changed synthetic room", "version": gorm.Expr("version + 1")}).Error)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+old.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	fresh := reloadChannelAIBookingState(t, db, f)
	assert.NotEqual(t, old.Offer.ID, fresh.Offer.ID)
	assert.Nil(t, fresh.Receipt)
	assert.Equal(t, "Changed synthetic room", fresh.Offer.Choices[0].Slot.Location)
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count)
}

func TestChannelAIBookingConfirmationRollsBackEveryWriteBoundary(t *testing.T) {
	for _, table := range []string{"bookings", "booking_events", "customer_activity_events", "outbox_events", "messages", "message_parts", "outbox_jobs", "inbox_conversations", "scheduled_jobs"} {
		t.Run(table, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			f, _ := createChannelAIBookingFixture(t, db)
			w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
				return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
			})
			require.NoError(t, processChannelAIBookingFixture(t, w, f))
			state := reloadChannelAIBookingState(t, db, f)
			markChannelAIBookingOfferSent(t, db, state)
			nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
			var hit atomic.Bool
			name := "ai-booking-inject-" + uuid.NewString()
			fail := func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != table {
					return
				}
				if table == "scheduled_jobs" {
					fields, ok := tx.Statement.Dest.(map[string]any)
					if !ok || fields["status"] != models.ScheduledJobStatusCompleted {
						return
					}
				}
				hit.Store(true)
				tx.AddError(errors.New("synthetic booking transaction fault"))
			}
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(name, fail))
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, fail))
			err := processChannelAIBookingFixture(t, w, f)
			require.NoError(t, db.Callback().Create().Remove(name))
			require.NoError(t, db.Callback().Update().Remove(name))
			require.Error(t, err)
			require.True(t, hit.Load(), "fault selector was actually executed")
			var count int64
			for _, model := range []any{&models.Booking{}, &models.CustomerActivityEvent{}, &models.OutboxEvent{}} {
				require.NoError(t, db.Model(model).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
				assert.Zero(t, count)
			}
			require.NoError(t, db.Model(&models.OutboxJob{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			assert.Nil(t, reloadChannelAIBookingState(t, db, f).Receipt)
		})
	}
}

func TestChannelAIBookingGenerationChangeCancelsBeforeReservation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, nil)
	const owner = "booking-generation-change"
	id, claimed, err := w.claimChannelAIReplyJob(f.Organization.ID, owner)
	require.NoError(t, err)
	require.True(t, claimed)
	check, err := w.authorizeChannelAIReplyGeneration(f.Organization.ID, id, owner)
	require.NoError(t, err)
	require.NotEmpty(t, check.Snapshot.BookingGeneration)
	f.Account.Config[models.ChannelConfigAIBookingRevision] = uuid.NewString()
	require.NoError(t, db.Model(f.Account).Update("config", f.Account.Config).Error)
	require.NoError(t, w.finalizeChannelAIReply(f.Organization.ID, id, owner, `{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`))
	var job models.ScheduledJob
	require.NoError(t, db.First(&job, "id = ?", id).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assert.Equal(t, "ai_booking_authority_changed", job.LastError)
	var count int64
	require.NoError(t, db.Model(&models.OutboxJob{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count)
}

func TestChannelAIBookingHandoffIsDurableAfterResume(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		t.Fatal("explicit staff request must not leave the process")
		return nil, errors.New("forbidden synthetic call")
	})
	f.Inbound.Content = "I need to speak to staff about symptoms"
	require.NoError(t, db.Model(f.Inbound).Update("content", f.Inbound.Content).Error)
	originalInbound := f.Inbound.ID
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	var transfer models.AgentTransfer
	require.NoError(t, db.Where("organization_id = ? AND contact_id = ?", f.Organization.ID, f.Contact.ID).First(&transfer).Error)
	assert.Equal(t, models.TransferStatusActive, transfer.Status)
	assert.Nil(t, transfer.AgentID)
	require.NoError(t, db.Model(&transfer).Update("status", models.TransferStatusResumed).Error)
	suppressed, err := contacthandoff.SuppressedTx(db, f.Organization.ID, originalInbound)
	require.NoError(t, err)
	assert.True(t, suppressed)
	var count int64
	require.NoError(t, db.Model(&models.OutboxJob{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count, "no fake staff acknowledgment")
	var activity models.CustomerActivityEvent
	require.NoError(t, db.Where("organization_id = ?", f.Organization.ID).First(&activity).Error)
	assert.Equal(t, models.CustomerActivityActorSystem, activity.ActorType)
	assert.Nil(t, activity.ActorUserID)
}

func TestChannelAIBookingBookingEntitlementCannotBeReplacedByOmnichannel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	require.NoError(t, db.Model(&models.Subscription{}).Where("organization_id = ?", f.Organization.ID).Update("entitlements_snapshot", models.JSONB{"omnichannel.enabled": true, "bookings.enabled": false}).Error)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&models.AgentTransfer{}).Where("organization_id = ? AND status = ?", f.Organization.ID, models.TransferStatusActive).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestChannelAIBookingDispatchRechecksAuthorityWithoutUndoingReservation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state := reloadChannelAIBookingState(t, db, f)
	var offerJob models.OutboxJob
	require.NoError(t, db.Where("message_id = ?", state.Offer.OfferMessageID).First(&offerJob).Error)
	require.NoError(t, db.Model(&offerJob).Updates(map[string]any{"status": models.OutboxJobStatusProcessing, "locked_by": "booking-offer-dispatch"}).Error)
	// A fresh valid offer is admitted by the normal AI classifier and fence;
	// this test never invokes a provider adapter.
	require.NoError(t, w.recheckChannelAIOutboxDispatch(f.Organization.ID, offerJob.ID, "booking-offer-dispatch"))
	markChannelAIBookingOfferSent(t, db, state)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state = reloadChannelAIBookingState(t, db, f)
	require.NotNil(t, state.Receipt)
	var confirmJob models.OutboxJob
	require.NoError(t, db.Where("message_id = ?", state.Receipt.MessageID).First(&confirmJob).Error)
	require.NoError(t, db.Model(&confirmJob).Updates(map[string]any{"status": models.OutboxJobStatusProcessing, "locked_by": "booking-confirm-dispatch"}).Error)
	f.Account.Config[models.ChannelConfigAIBookingEnabled] = false
	require.NoError(t, db.Model(f.Account).Update("config", f.Account.Config).Error)
	require.ErrorIs(t, w.recheckChannelAIOutboxDispatch(f.Organization.ID, confirmJob.ID, "booking-confirm-dispatch"), errChannelOutboxAIPolicy)
	var reserved models.Booking
	require.NoError(t, db.First(&reserved, "id = ?", state.Receipt.BookingID).Error)
	assert.Equal(t, models.BookingStatusReserved, reserved.Status)
	require.NoError(t, db.First(&confirmJob, "id = ?", confirmJob.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, confirmJob.Status, "rejected recheck cannot cross the wire fence")
	var sends atomic.Int32
	adapter := &channelOutboxAttemptAdapter{send: func(context.Context, *models.ChannelAccount, channelapi.OutboundMessage) (channelapi.SendResult, error) {
		sends.Add(1)
		return channelapi.SendResult{}, errors.New("disabled authority reached mock adapter")
	}}
	outbound, err := channelOutboundMessageForJob(&confirmJob)
	require.NoError(t, err)
	require.NoError(t, w.deliverChannelAIOutboxPhysicalAttempt(context.Background(), f.Organization.ID, &confirmJob, f.Account, "booking-confirm-dispatch", outbound, adapter))
	require.Zero(t, sends.Load())
	require.NoError(t, db.First(&confirmJob, "id = ?", confirmJob.ID).Error)
	require.Equal(t, models.OutboxJobStatusCancelled, confirmJob.Status)
	require.NoError(t, db.First(&reserved, "id = ?", reserved.ID).Error)
	require.Equal(t, models.BookingStatusReserved, reserved.Status)
}

func TestChannelAIBookingPauseResumeCannotConsumePriorOffer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f, _ := createChannelAIBookingFixture(t, db)
	w := channelAIReplyTestWorker(db, func(*http.Request) (*http.Response, error) {
		return channelAIReplyQwenResponse(`{"action":"offer","reply":"","service":"Synthetic consultation","date":""}`), nil
	})
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	state := reloadChannelAIBookingState(t, db, f)
	markChannelAIBookingOfferSent(t, db, state)
	require.NoError(t, db.Model(f.Conversation).Update("config", models.JSONB{
		models.ConversationConfigAIPaused: false, models.ConversationConfigAIPausedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}).Error)
	nextChannelAIBookingInbound(t, db, f, "BOOK "+state.Offer.Choices[0].Token)
	require.NoError(t, processChannelAIBookingFixture(t, w, f))
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", f.Organization.ID).Count(&count).Error)
	assert.Zero(t, count)
}
