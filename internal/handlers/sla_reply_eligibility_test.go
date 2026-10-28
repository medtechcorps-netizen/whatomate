package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
)

func slaEligibilityMessage(t *testing.T, app *App, transfer *models.AgentTransfer, at time.Time, status models.MessageStatus, kind models.MessageType) *models.Message {
	t.Helper()
	// GORM updates pointer-valued model fields in place. Keep the message
	// sender independent from the transfer whose deadline we are checking.
	var senderID *uuid.UUID
	if transfer.AgentID != nil {
		id := *transfer.AgentID
		senderID = &id
	}
	message := &models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: at},
		OrganizationID: transfer.OrganizationID, ContactID: transfer.ContactID,
		WhatsAppAccount: transfer.WhatsAppAccount, Direction: models.DirectionOutgoing,
		MessageType: kind, Status: status, SentByUserID: senderID,
		Content: "Synthetic SLA eligibility fixture",
	}
	require.NoError(t, app.DB.Create(message).Error)
	return message
}

func slaEligibilityAssign(t *testing.T, app *App, transfer *models.AgentTransfer, userID uuid.UUID) {
	t.Helper()
	require.NoError(t, app.DB.Model(transfer).Update("agent_id", userID).Error)
	transfer.AgentID = &userID
}

func TestSLAConfirmedReplyEligibilityAndScope(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	agent := testutil.CreateTestUser(t, app.DB, org.ID)
	other := testutil.CreateTestUser(t, app.DB, org.ID)
	foreign := testutil.CreateTestOrganization(t, app.DB)
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	processor := NewSLAProcessor(app, time.Minute)
	for _, tc := range []struct {
		name     string
		status   models.MessageStatus
		kind     models.MessageType
		eligible bool
	}{
		{"sent", models.MessageStatusSent, models.MessageTypeText, true},
		{"delivered", models.MessageStatusDelivered, models.MessageTypeText, true},
		{"read", models.MessageStatusRead, models.MessageTypeText, true},
		{"pending", models.MessageStatusPending, models.MessageTypeText, false},
		{"failed", models.MessageStatusFailed, models.MessageTypeText, false},
		{"received", models.MessageStatusReceived, models.MessageTypeText, false},
		{"template", models.MessageStatusSent, models.MessageTypeTemplate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transfer := createTransfer(models.SLATracking{})
			slaEligibilityAssign(t, app, transfer, agent.ID)
			slaEligibilityMessage(t, app, transfer, since.Add(time.Minute), tc.status, tc.kind)
			got, err := processor.agentRespondedSince(*transfer, since)
			require.NoError(t, err)
			assert.Equal(t, tc.eligible, got)
		})
	}

	transfer := createTransfer(models.SLATracking{})
	slaEligibilityAssign(t, app, transfer, agent.ID)
	message := slaEligibilityMessage(t, app, transfer, since, models.MessageStatusSent, models.MessageTypeText)
	got, err := processor.agentRespondedSince(*transfer, since)
	require.NoError(t, err)
	assert.False(t, got, "created_at must be strictly after the anchor")
	require.NoError(t, app.DB.Model(message).Update("created_at", since.Add(-time.Minute)).Error)
	got, err = processor.agentRespondedSince(*transfer, since)
	require.NoError(t, err)
	assert.False(t, got)
	require.NoError(t, app.DB.Model(message).Update("created_at", since.Add(time.Minute)).Error)
	for _, change := range []func(*models.AgentTransfer){
		func(row *models.AgentTransfer) { row.OrganizationID = foreign.ID },
		func(row *models.AgentTransfer) { row.ContactID = createTransfer(models.SLATracking{}).ContactID },
		func(row *models.AgentTransfer) { row.AgentID = &other.ID },
		func(row *models.AgentTransfer) { row.AgentID = nil },
	} {
		selected := *transfer
		change(&selected)
		got, err = processor.agentRespondedSince(selected, since)
		require.NoError(t, err)
		assert.False(t, got)
	}
	selected := *transfer
	selected.WhatsAppAccount = "different-account-selection"
	got, err = processor.agentRespondedSince(selected, since)
	require.NoError(t, err)
	assert.True(t, got, "preserve existing contact-wide account scope")
	require.NoError(t, app.DB.Model(message).Update("direction", models.DirectionIncoming).Error)
	got, err = processor.agentRespondedSince(*transfer, since)
	require.NoError(t, err)
	assert.False(t, got, "incoming messages never prove an agent reply")
}

func TestSLACustomerAnchorDistinguishesEmptyFromFailure(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	transfer := createTransfer(models.SLATracking{})
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	transfer.TransferredAt = start
	processor := NewSLAProcessor(app, time.Minute)
	got, err := processor.customerLastSpokeAt(*transfer)
	require.NoError(t, err, "SQL NULL MAX is a valid empty history")
	assert.True(t, got.Equal(start))
	message := slaEligibilityMessage(t, app, transfer, start.Add(-time.Minute), models.MessageStatusReceived, models.MessageTypeText)
	require.NoError(t, app.DB.Model(message).Update("direction", models.DirectionIncoming).Error)
	got, err = processor.customerLastSpokeAt(*transfer)
	require.NoError(t, err)
	assert.True(t, got.Equal(start))
	later := start.Add(time.Minute)
	require.NoError(t, app.DB.Model(message).Update("created_at", later).Error)
	got, err = processor.customerLastSpokeAt(*transfer)
	require.NoError(t, err)
	assert.True(t, got.Equal(later))
	// A later outgoing message must not move the customer's anchor.
	slaEligibilityMessage(t, app, transfer, later.Add(time.Minute), models.MessageStatusSent, models.MessageTypeText)
	got, err = processor.customerLastSpokeAt(*transfer)
	require.NoError(t, err)
	assert.True(t, got.Equal(later))
	foreign := testutil.CreateTestOrganization(t, app.DB)
	selected := *transfer
	selected.OrganizationID = foreign.ID
	got, err = processor.customerLastSpokeAt(selected)
	require.NoError(t, err)
	assert.True(t, got.Equal(start), "foreign-org history must not become this transfer's anchor")
	assert.NotEqual(t, org.ID, foreign.ID)
}

func TestSLAWarningAnyUserRemainsDistinctFromDeadlineAssignedAgent(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	agent := testutil.CreateTestUser(t, app.DB, org.ID)
	other := testutil.CreateTestUser(t, app.DB, org.ID)
	require.NotEqual(t, agent.ID, other.ID)
	escalated := time.Now().UTC().Add(-time.Hour)
	transfer := createTransfer(models.SLATracking{EscalationLevel: 1, EscalatedAt: &escalated})
	slaEligibilityAssign(t, app, transfer, agent.ID)
	message := slaEligibilityMessage(t, app, transfer, escalated.Add(time.Minute), models.MessageStatusPending, models.MessageTypeText)
	require.NotNil(t, transfer.AgentID)
	require.NotNil(t, message.SentByUserID)
	require.True(t, transfer.AgentID != message.SentByUserID, "fixture identities must not share a mutable UUID pointer")
	require.Equal(t, agent.ID, *transfer.AgentID)
	require.Equal(t, agent.ID, *message.SentByUserID)
	require.NoError(t, app.DB.Model(message).Update("sent_by_user_id", other.ID).Error)
	require.Equal(t, agent.ID, *transfer.AgentID, "changing the message sender must preserve the selected transfer agent")
	require.Equal(t, other.ID, *message.SentByUserID)
	for _, tc := range []struct {
		status  models.MessageStatus
		kind    models.MessageType
		warning bool
	}{
		{models.MessageStatusPending, models.MessageTypeText, true},
		{models.MessageStatusFailed, models.MessageTypeText, true},
		{models.MessageStatusReceived, models.MessageTypeText, true},
		{models.MessageStatusSent, models.MessageTypeTemplate, true},
		{models.MessageStatusSent, models.MessageTypeText, false},
		{models.MessageStatusDelivered, models.MessageTypeText, false},
		{models.MessageStatusRead, models.MessageTypeText, false},
	} {
		require.NoError(t, app.DB.Model(message).Updates(map[string]any{"status": tc.status, "message_type": tc.kind}).Error)
		warning, err := slaWarningNoticeStillCurrent(app.DB, *transfer)
		require.NoError(t, err)
		assert.Equal(t, tc.warning, warning)
		responded, err := NewSLAProcessor(app, time.Minute).agentRespondedSince(*transfer, escalated)
		require.NoError(t, err)
		assert.False(t, responded, "the other user's reply may suppress a stale warning but cannot extend the assigned agent's deadline")
	}
}

func TestSLADeadlineProcessorsUseOnlyConfirmedNonTemplateReplies(t *testing.T) {
	for _, kind := range []string{"auto_close", "escalation"} {
		for _, tc := range []struct {
			name        string
			status      models.MessageStatus
			messageType models.MessageType
			extend      bool
		}{
			{"sent", models.MessageStatusSent, models.MessageTypeText, true},
			{"delivered", models.MessageStatusDelivered, models.MessageTypeText, true},
			{"read", models.MessageStatusRead, models.MessageTypeText, true},
			{"pending", models.MessageStatusPending, models.MessageTypeText, false},
			{"failed", models.MessageStatusFailed, models.MessageTypeText, false},
			{"template", models.MessageStatusSent, models.MessageTypeTemplate, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
				agent := testutil.CreateTestUser(t, app.DB, org.ID)
				past := time.Now().UTC().Add(-time.Hour)
				sla := models.SLATracking{ExpiresAt: &past}
				if kind == "escalation" {
					sla = models.SLATracking{EscalationAt: &past}
				}
				transfer := createTransfer(sla)
				slaEligibilityAssign(t, app, transfer, agent.ID)
				slaEligibilityMessage(t, app, transfer, time.Now().Add(-30*time.Minute), tc.status, tc.messageType)
				settings := slaNoticeStateSettings(org.ID)
				require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
					processor := NewSLAProcessor(scoped, time.Minute)
					if kind == "auto_close" {
						processor.autoCloseExpiredTransfers(org.ID, settings, time.Now())
					} else {
						processor.escalateTransfers(org.ID, settings, time.Now())
					}
					return nil
				}))
				var stored models.AgentTransfer
				require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
				if tc.extend {
					assert.Equal(t, models.TransferStatusActive, stored.Status)
					assert.Zero(t, stored.SLA.EscalationLevel)
					require.NotNil(t, stored.SLA.FirstResponseAt)
					deadline := stored.SLA.ExpiresAt
					if kind == "escalation" {
						deadline = stored.SLA.EscalationAt
					}
					require.NotNil(t, deadline)
					assert.True(t, deadline.After(time.Now()))
					assert.Empty(t, deliveries())
				} else {
					assert.Nil(t, stored.SLA.FirstResponseAt)
					if kind == "auto_close" {
						assert.Equal(t, models.TransferStatusExpired, stored.Status)
					} else {
						assert.Equal(t, 1, stored.SLA.EscalationLevel)
					}
					require.Len(t, deliveries(), 1, "actual processor's committed transition must reach the synthetic provider")
					assert.Equal(t, transfer.PhoneNumber, deliveries()[0].To)
				}
			})
		}
	}
}

// Substitute a real PostgreSQL statement error only for this contact's COUNT
// or MAX query. A fake callback Error would not test transaction-abort recovery.
func slaSelectiveReadFailure(t *testing.T, db *gorm.DB, contactID uuid.UUID, aggregate string) *atomic.Int32 {
	t.Helper()
	hits := &atomic.Int32{}
	name := "test:sla-selective-read:" + uuid.NewString()
	fault := func(tx *gorm.DB) {
		if tx.Statement.Table != "messages" {
			return
		}
		callbacks.BuildQuerySQL(tx)
		sql := strings.ToLower(tx.Statement.SQL.String())
		if !strings.Contains(sql, aggregate+"(") {
			return
		}
		selected := false
		for _, value := range tx.Statement.Vars {
			if fmt.Sprint(value) == contactID.String() {
				selected = true
			}
		}
		if !selected {
			return
		}
		hits.Add(1)
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT 1 / 0")
		tx.Statement.Vars = nil
	}
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, fault))
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register(name, fault))
	t.Cleanup(func() {
		assert.NoError(t, db.Callback().Query().Remove(name))
		assert.NoError(t, db.Callback().Row().Remove(name))
	})
	return hits
}

func TestSLASelectiveReadFailureSkipsRowAndRecoversTransaction(t *testing.T) {
	for _, rls := range []bool{false, true} {
		for _, tc := range []struct{ kind, aggregate string }{{"auto_close", "count"}, {"escalation", "count"}, {"escalation", "max"}} {
			t.Run(fmt.Sprintf("rls_%t/%s/%s", rls, tc.kind, tc.aggregate), func(t *testing.T) {
				app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
				app.Config.Database.RLSEnabled = rls
				agent := testutil.CreateTestUser(t, app.DB, org.ID)
				past := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
				sla := models.SLATracking{ExpiresAt: &past}
				if tc.kind == "escalation" {
					sla = models.SLATracking{EscalationAt: &past}
				}
				broken := createTransfer(sla)
				slaEligibilityAssign(t, app, broken, agent.ID)
				var before models.AgentTransfer
				require.NoError(t, app.DB.First(&before, "id = ?", broken.ID).Error)
				// The healthy candidate is strictly later, so it proves the pass
				// continued after the real error rather than succeeding before it.
				later := past.Add(time.Minute)
				healthySLA := models.SLATracking{ExpiresAt: &later}
				if tc.kind == "escalation" {
					healthySLA = models.SLATracking{EscalationAt: &later}
				}
				healthy := createTransfer(healthySLA)
				slaEligibilityAssign(t, app, healthy, agent.ID)
				hits := slaSelectiveReadFailure(t, app.DB, broken.ContactID, tc.aggregate)
				settings := slaNoticeStateSettings(org.ID)
				require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
					var probe int
					if err := scoped.DB.Raw("SELECT 1").Scan(&probe).Error; err != nil {
						return err
					}
					processor := NewSLAProcessor(scoped, time.Minute)
					if tc.aggregate == "max" {
						anchor, err := processor.customerLastSpokeAt(*broken)
						require.ErrorContains(t, err, "division by zero")
						assert.True(t, anchor.IsZero())
					} else {
						responded, err := processor.agentRespondedSince(*broken, past)
						require.ErrorContains(t, err, "division by zero")
						assert.False(t, responded)
					}
					if err := scoped.DB.Raw("SELECT 1").Scan(&probe).Error; err != nil {
						return err
					}
					beforePass := hits.Load()
					if tc.kind == "auto_close" {
						processor.autoCloseExpiredTransfers(org.ID, settings, time.Now())
					} else {
						processor.escalateTransfers(org.ID, settings, time.Now())
					}
					assert.Greater(t, hits.Load(), beforePass, "the actual processor must encounter the failing aggregate after the direct probe")
					return scoped.DB.Raw("SELECT 1").Scan(&probe).Error
				}))
				var stored, good models.AgentTransfer
				require.NoError(t, app.DB.First(&stored, "id = ?", broken.ID).Error)
				require.NoError(t, app.DB.First(&good, "id = ?", healthy.ID).Error)
				assert.Equal(t, models.TransferStatusActive, stored.Status)
				assert.Equal(t, before.SLA, stored.SLA, "unknown history must not change any deadline, breach, or first-response field")
				if tc.kind == "auto_close" {
					assert.Equal(t, models.TransferStatusExpired, good.Status)
				} else {
					assert.Equal(t, 1, good.SLA.EscalationLevel)
				}
				require.Len(t, deliveries(), 1)
				assert.Equal(t, healthy.PhoneNumber, deliveries()[0].To, "failed-query row must not queue a notice")
			})
		}
	}
}

func TestSLAActivityReadsPreserveCancellationAndDeadline(t *testing.T) {
	app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
	agent := testutil.CreateTestUser(t, app.DB, org.ID)
	transfer := createTransfer(models.SLATracking{})
	slaEligibilityAssign(t, app, transfer, agent.ID)
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline_%t", deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			scoped := &App{DB: app.DB.WithContext(ctx), Config: app.Config, Log: app.Log, Redis: app.Redis}
			processor := NewSLAProcessor(scoped, time.Minute)
			responded, err := processor.agentRespondedSince(*transfer, time.Now().Add(-time.Hour))
			require.ErrorIs(t, err, want)
			assert.False(t, responded)
			anchor, err := processor.customerLastSpokeAt(*transfer)
			require.ErrorIs(t, err, want)
			assert.True(t, anchor.IsZero(), "failed query cannot masquerade as fallback history")
			assert.Empty(t, scoped.afterCommit)
		})
	}
	assert.Empty(t, deliveries())
}
