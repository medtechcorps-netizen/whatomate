package handlers

// Source-only additional coverage for H1 953a2654 + keyword outcome patch b921cef.
// Requires the unchanged positive fixture/helper file pinned at 3b46ffb1.
// The known hours-transition ordinal repair is NOT_READY and excluded here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1P2RecordWriteCallbacks(t *testing.T, db *gorm.DB) *atomic.Int32 {
	t.Helper()
	var writes atomic.Int32
	name := "h1:p2_write_observer:" + uuid.NewString()
	observe := func(_ *gorm.DB) { writes.Add(1) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(name, observe))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, observe))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(name, observe))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(name)
		_ = db.Callback().Update().Remove(name)
		_ = db.Callback().Delete().Remove(name)
	})
	return &writes
}

func h1P2ClearSettings(t *testing.T, env *h1KeywordOutcomeEnv) {
	t.Helper()
	key := settingsCachePrefix + env.org.ID.String() + ":" + env.account.Name
	require.NoError(t, env.app.Redis.Del(context.Background(), key).Err())
}

func h1P2RequireOrdinaryFailure(t *testing.T, env *h1KeywordOutcomeEnv, err, original error) {
	t.Helper()
	require.ErrorIs(t, err, original)
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "wedge guard")
	assert.False(t, inboundContinuationRequiresManualReview(err))
	job := env.job(t)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 1, job.Attempts)
	_, refunded := job.Payload["wedge_guard_deferrals"]
	assert.False(t, refunded)
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
}

func TestH1P2Keyword_HelperReadErrorsReturnFalseBeforeWrites(t *testing.T) {
	for _, failing := range []string{"checked_count", "settings_read"} {
		t.Run(failing, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			h1P2ClearSettings(t, env)
			original := errors.New("synthetic keyword " + failing + " failure")
			var countReads, settingsReads atomic.Int32
			name := "h1:p2_read_error:" + uuid.NewString()
			require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil {
					return
				}
				if tx.Statement.Schema.Table == "agent_transfers" {
					if _, ok := tx.Statement.Dest.(*int64); ok {
						countReads.Add(1)
						if failing == "checked_count" {
							_ = tx.AddError(original)
						}
					}
				}
				if tx.Statement.Schema.Table == "chatbot_settings" {
					settingsReads.Add(1)
					if failing == "settings_read" {
						_ = tx.AddError(original)
					}
				}
			}))
			t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
			writes := h1P2RecordWriteCallbacks(t, env.pool)
			created, err := env.app.createTransferFromKeyword(env.account, &env.work.Contact)
			assert.False(t, created)
			require.ErrorIs(t, err, original)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.NotContains(t, err.Error(), "wedge guard")
			assert.EqualValues(t, 1, countReads.Load())
			if failing == "checked_count" {
				assert.Zero(t, settingsReads.Load(), "a failed COUNT must stop before settings")
			} else {
				assert.EqualValues(t, 1, settingsReads.Load())
			}
			assert.Zero(t, writes.Load(), "no create/update/delete callback after a read failure")
			assert.Empty(t, env.bodies())
			assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
		})
	}
}

func TestH1P2Keyword_HelperDeclineReturnsFalseWithoutError(t *testing.T) {
	for _, reply := range []string{"", h1KeywordClosedBody} {
		name := "without_reply"
		if reply != "" {
			name = "with_reply"
		}
		t.Run(name, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			env.closeHours(t, reply)
			// Enter through the real durable claim and tenant execution scope. Capture
			// the helper error separately: ProcessMessage consumes policy stops.
			var called, created bool
			var helperErr error
			env.processor.process = func(_ context.Context, scoped *App, work *persistedIncomingMessage) error {
				called = true
				require.NotNil(t, scoped.inboundContinuation)
				require.Equal(t, env.work.Persisted.ID, scoped.inboundContinuation.MessageID)
				created, helperErr = scoped.createTransferFromKeyword(env.account, &work.Contact)
				return helperErr
			}
			err := env.run(env.baseTime)
			require.True(t, called, "the durable processor must execute the helper")
			assert.False(t, created, "the caller consumes an intentional decline, not a handover")
			require.NoError(t, helperErr)
			require.NoError(t, err)
			assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
			assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
			if reply == "" {
				assert.Empty(t, env.bodies())
			} else {
				assert.Equal(t, []string{reply}, env.bodies())
			}
		})
	}
}

func TestH1P2Keyword_HelperActiveOutcomesReturnTrueWithoutDuplicateNotification(t *testing.T) {
	for _, mode := range []string{"already_active", "appears_after_count", "newly_created"} {
		t.Run(mode, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			envelopes := h1CountTransferEnvelopes(t, env.app, env.org.ID)
			existing := models.AgentTransfer{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
				ContactID: env.work.Contact.ID, WhatsAppAccount: env.account.Name,
				PhoneNumber: env.work.Contact.PhoneNumber, Status: models.TransferStatusActive,
				Source: models.TransferSourceManual, TransferredAt: time.Now().UTC(),
			}
			if mode == "already_active" {
				require.NoError(t, env.shared.Create(&existing).Error)
				// The early active outcome must not consult even invalid settings.
				h1P2ClearSettings(t, env)
			}
			var inserted atomic.Bool
			name := "h1:p2_active_outcome:" + uuid.NewString()
			require.NoError(t, env.pool.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil {
					return
				}
				if mode == "already_active" && tx.Statement.Schema.Table == "chatbot_settings" {
					_ = tx.AddError(errors.New("existing transfer must not read settings"))
				}
				if mode == "appears_after_count" && tx.Error == nil && tx.Statement.Schema.Table == "agent_transfers" {
					if count, ok := tx.Statement.Dest.(*int64); ok && *count == 0 && inserted.CompareAndSwap(false, true) {
						// Deterministic committed interleaving, as in the existing H1 graph test.
						_ = tx.AddError(env.shared.Create(&existing).Error)
					}
				}
			}))
			t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
			writes := h1P2RecordWriteCallbacks(t, env.pool)
			created, err := env.app.createTransferFromKeyword(env.account, &env.work.Contact)
			require.NoError(t, err)
			assert.True(t, created)
			var transfers []models.AgentTransfer
			require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).Find(&transfers).Error)
			require.Len(t, transfers, 1)
			assert.Equal(t, models.TransferStatusActive, transfers[0].Status)
			assert.Empty(t, env.bodies(), "the helper itself sends no transfer body")
			if mode == "newly_created" {
				assert.Equal(t, models.TransferSourceKeyword, transfers[0].Source)
				h1RequireEnvelopes(t, envelopes, 1)
			} else {
				assert.Equal(t, existing.ID, transfers[0].ID)
				assert.Equal(t, models.TransferSourceManual, transfers[0].Source)
				assert.Zero(t, writes.Load(), "existing handover must not be rewritten or cut off again")
				assert.Equal(t, mode == "appears_after_count", inserted.Load())
				h1RequireEnvelopes(t, envelopes, 0)
			}
		})
	}
}

func TestH1P2Keyword_SettingsReadFailureCountsWithoutResendingBody(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	original := errors.New("synthetic helper settings read failure")
	var armed, failed atomic.Bool
	fired := env.beforeHelperCount(t, func(_ *gorm.DB) {
		h1P2ClearSettings(t, env)
		armed.Store(true)
	})
	name := "h1:p2_settings_after_body:" + uuid.NewString()
	require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		execution := env.execution.Load()
		if armed.Load() && execution != nil && tx.Statement.Schema != nil &&
			tx.Statement.Schema.Table == "chatbot_settings" &&
			tx.Statement.ConnPool == execution.DB.Statement.ConnPool && failed.CompareAndSwap(false, true) {
			_ = tx.AddError(original)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
	err := env.run(env.baseTime)
	require.True(t, fired.Load())
	require.True(t, failed.Load())
	h1P2RequireOrdinaryFailure(t, env, err, original)
	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
}

func TestH1P2Keyword_ConfiguredReplyPreDispatchFailureConsumesAttempt(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	fired := env.beforeHelperCount(t, func(_ *gorm.DB) { env.closeHours(t, h1KeywordClosedBody) })
	original := errors.New("synthetic second reply claim failure before dispatch")
	var failed atomic.Bool
	name := "h1:p2_second_reply_claim:" + uuid.NewString()
	require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		job, ok := tx.Statement.Dest.(*models.ScheduledJob)
		if !ok || job.Kind != inboundContinuationActionJobKind || job.OrganizationID != env.org.ID ||
			job.AggregateID == nil || *job.AggregateID != env.work.Persisted.ID {
			return
		}
		ordinal, valid := inboundContinuationExactInteger(job.Payload["action_ordinal"])
		if valid && ordinal == 1 && failed.CompareAndSwap(false, true) {
			_ = tx.AddError(original)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove(name) })
	err := env.run(env.baseTime)
	require.True(t, fired.Load())
	require.True(t, failed.Load())
	h1P2RequireOrdinaryFailure(t, env, err, original)
	assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
	// Intentionally no replay: the separate 0,1,0 hours-transition defect remains NOT_READY.
}

func TestH1P2Keyword_ConfiguredReplyProviderFailureRemainsTerminal(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid synthetic body", http.StatusBadRequest)
			return
		}
		env.mu.Lock()
		env.sent = append(env.sent, request.Text.Body)
		env.mu.Unlock()
		if request.Text.Body == h1KeywordTransferBody {
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "wamid.h1-p2.body." + env.org.ID.String()}}})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "synthetic second reply provider failure"}})
	}))
	t.Cleanup(provider.Close)
	env.app.WhatsApp = whatsapp.NewWithBaseURL(env.app.Log, provider.URL)
	fired := env.beforeHelperCount(t, func(_ *gorm.DB) { env.closeHours(t, h1KeywordClosedBody) })
	err := env.run(env.baseTime)
	require.True(t, fired.Load())
	var review *inboundContinuationManualReviewError
	require.ErrorAs(t, err, &review)
	assert.True(t, inboundContinuationRequiresManualReview(err))
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "wedge guard")
	job := env.job(t)
	assert.Equal(t, models.ScheduledJobStatusFailed, job.Status)
	assert.Equal(t, true, job.Payload["manual_review_required"])
	assert.Equal(t, 1, job.Attempts)
	_, refunded := job.Payload["wedge_guard_deferrals"]
	assert.False(t, refunded)
	want := []string{h1KeywordTransferBody, h1KeywordClosedBody}
	assert.Equal(t, want, env.bodies(), "only the second configured reply fails after dispatch")
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	var actions []models.ScheduledJob
	require.NoError(t, env.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", env.org.ID, inboundContinuationActionJobKind, env.work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2)
	states := make(map[int]any)
	for _, action := range actions {
		ordinal, ok := inboundContinuationExactInteger(action.Payload["action_ordinal"])
		require.True(t, ok)
		states[ordinal] = action.Payload["state"]
	}
	assert.Equal(t, inboundActionStateResolved, states[0])
	assert.Equal(t, inboundActionStateManualReview, states[1])
	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, models.ScheduledJobStatusFailed, env.job(t).Status)
	assert.Equal(t, want, env.bodies(), "terminal manual review must not dispatch either reply again")
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
}
