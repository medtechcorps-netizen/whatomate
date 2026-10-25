package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1KeywordPhaseMessage(t *testing.T, env *h1KeywordOutcomeEnv) models.Message {
	t.Helper()
	var message models.Message
	require.NoError(t, env.shared.
		Where("id = ? AND organization_id = ?", env.work.Persisted.ID, env.org.ID).
		First(&message).Error)
	return message
}

func h1KeywordPhase(t *testing.T, env *h1KeywordOutcomeEnv) *inboundKeywordProgress {
	t.Helper()
	progress, err := readInboundKeywordProgress(h1KeywordPhaseMessage(t, env).Metadata)
	require.NoError(t, err)
	return progress
}

func h1StoreKeywordPhase(t *testing.T, env *h1KeywordOutcomeEnv, progress inboundKeywordProgress) {
	t.Helper()
	progress.Version = inboundKeywordProgressVersion
	progress.MessageID = env.work.Persisted.ID
	progress.ContactID = env.work.Contact.ID
	progress.AccountID = env.account.ID
	progress.Account = env.account.Name
	progress.PhoneID = env.account.PhoneID
	message := h1KeywordPhaseMessage(t, env)
	metadata := cloneInboundContinuationJSONB(message.Metadata)
	metadata[inboundKeywordProgressKey] = inboundKeywordProgressToJSONB(&progress)
	require.NoError(t, env.shared.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
		Update("metadata", metadata).Error)
}

func h1KeywordPhaseRaw(t *testing.T, metadata models.JSONB) models.JSONB {
	t.Helper()
	encoded, err := json.Marshal(metadata[inboundKeywordProgressKey])
	require.NoError(t, err)
	var raw models.JSONB
	require.NoError(t, json.Unmarshal(encoded, &raw))
	return raw
}

func h1KeywordPhaseReopen(t *testing.T, env *h1KeywordOutcomeEnv) {
	t.Helper()
	require.NoError(t, env.shared.Model(&models.ChatbotSettings{}).
		Where("id = ? AND organization_id = ?", env.settings.ID, env.org.ID).
		Updates(map[string]any{
			"business_hours_enabled": false,
			"out_of_hours_message":   "Changed settings must not replace a selected reply",
		}).Error)
	key := settingsCachePrefix + env.org.ID.String() + ":" + env.account.Name
	require.NoError(t, env.app.Redis.Del(context.Background(), key).Err())
}

func TestH1KeywordProgress_SecondDeclineSurvivesRetryAndReopenedHours(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	message := h1KeywordPhaseMessage(t, env)
	metadata := cloneInboundContinuationJSONB(message.Metadata)
	metadata["unrelated_test_fact"] = "retain"
	require.NoError(t, env.shared.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
		Update("metadata", metadata).Error)

	fired := env.beforeHelperCount(t, func(_ *gorm.DB) {
		env.closeHours(t, h1KeywordClosedBody)
	})
	claimErr := errors.New("synthetic decline claim failure")
	var failed atomic.Bool
	var ordinals []int
	var keys []string
	name := "h1:keyword_phase_decline_claim"
	require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		job, ok := tx.Statement.Dest.(*models.ScheduledJob)
		if !ok || job.Kind != inboundContinuationActionJobKind ||
			job.OrganizationID != env.org.ID || job.AggregateID == nil ||
			*job.AggregateID != env.work.Persisted.ID {
			return
		}
		ordinal, ok := inboundContinuationExactInteger(job.Payload["action_ordinal"])
		require.True(t, ok)
		ordinals = append(ordinals, ordinal)
		keys = append(keys, job.IdempotencyKey)
		if ordinal == 1 && failed.CompareAndSwap(false, true) {
			progress := h1KeywordPhase(t, env)
			require.NotNil(t, progress)
			require.Equal(t, inboundKeywordStageBodyDeclined, progress.Stage)
			require.Equal(t, h1KeywordClosedBody, progress.Decline)
			_ = tx.AddError(claimErr)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove(name) })

	require.ErrorIs(t, env.run(env.baseTime), claimErr)
	require.True(t, fired.Load())
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageBodyDeclined, progress.Stage)
	assert.Equal(t, h1KeywordTransferBody, progress.Body)
	assert.Equal(t, h1KeywordClosedBody, progress.Decline)
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.Equal(t, models.ScheduledJobStatusPending, env.job(t).Status)

	h1KeywordPhaseReopen(t, env)
	require.NoError(t, env.shared.Model(&models.KeywordRule{}).
		Where("organization_id = ?", env.org.ID).
		Update("response_content", models.JSONB{
			"body": "New rule body must not replace the selected body",
		}).Error)
	ruleKey := keywordRulesCachePrefix + env.org.ID.String() + ":" + env.account.Name
	require.NoError(t, env.app.Redis.Del(context.Background(), ruleKey).Err())

	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, []string{h1KeywordTransferBody, h1KeywordClosedBody}, env.bodies())
	assert.Equal(t, []int{0, 1, 0, 1}, ordinals)
	require.Len(t, keys, 4)
	assert.Equal(t, keys[0], keys[2])
	assert.Equal(t, keys[1], keys[3])
	assert.NotEqual(t, keys[0], keys[1])
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 2)

	message = h1KeywordPhaseMessage(t, env)
	assert.Equal(t, "retain", message.Metadata["unrelated_test_fact"])
	assert.Equal(t, true, message.Metadata["inbound_continuation_completed"])
}

func TestH1KeywordProgress_InitialDeclineKeepsOrdinalZero(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	env.closeHours(t, h1KeywordClosedBody)
	claimErr := errors.New("synthetic initial decline claim failure")
	var failed atomic.Bool
	var ordinals []int
	name := "h1:keyword_initial_decline_claim"
	require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		job, ok := tx.Statement.Dest.(*models.ScheduledJob)
		if !ok || job.Kind != inboundContinuationActionJobKind ||
			job.OrganizationID != env.org.ID || job.AggregateID == nil ||
			*job.AggregateID != env.work.Persisted.ID {
			return
		}
		ordinal, ok := inboundContinuationExactInteger(job.Payload["action_ordinal"])
		require.True(t, ok)
		ordinals = append(ordinals, ordinal)
		if failed.CompareAndSwap(false, true) {
			progress := h1KeywordPhase(t, env)
			require.NotNil(t, progress)
			require.Equal(t, inboundKeywordStageDeclined, progress.Stage)
			_ = tx.AddError(claimErr)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove(name) })

	require.ErrorIs(t, env.run(env.baseTime), claimErr)
	require.Empty(t, env.bodies())
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageDeclined, progress.Stage)
	assert.Empty(t, progress.Body)
	assert.Equal(t, h1KeywordClosedBody, progress.Decline)
	assert.Equal(t, models.ScheduledJobStatusPending, env.job(t).Status)

	h1KeywordPhaseReopen(t, env)
	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, []int{0, 0}, ordinals)
	assert.Equal(t, []string{h1KeywordClosedBody}, env.bodies())
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
}

func TestH1KeywordProgress_EmptyBodyDeclineStaysOrdinalZero(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	require.NoError(t, env.shared.Model(&models.KeywordRule{}).
		Where("organization_id = ?", env.org.ID).
		Update("response_content", models.JSONB{"body": ""}).Error)
	ruleKey := keywordRulesCachePrefix + env.org.ID.String() + ":" + env.account.Name
	require.NoError(t, env.app.Redis.Del(context.Background(), ruleKey).Err())
	var ordinals []int
	name := "h1:keyword_empty_body_ordinal"
	require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		job, ok := tx.Statement.Dest.(*models.ScheduledJob)
		if !ok || job.Kind != inboundContinuationActionJobKind ||
			job.OrganizationID != env.org.ID || job.AggregateID == nil ||
			*job.AggregateID != env.work.Persisted.ID {
			return
		}
		ordinal, ok := inboundContinuationExactInteger(job.Payload["action_ordinal"])
		require.True(t, ok)
		ordinals = append(ordinals, ordinal)
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove(name) })

	var agentTransferCounts atomic.Int32
	var closed atomic.Bool
	queryName := "h1:keyword_empty_body_hours"
	require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(queryName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "agent_transfers" {
			return
		}
		if _, ok := tx.Statement.Dest.(*int64); !ok {
			return
		}
		if agentTransferCounts.Add(1) != 2 {
			return
		}
		closed.Store(true)
		env.closeHours(t, h1KeywordClosedBody)
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(queryName) })

	require.NoError(t, env.run(env.baseTime))
	assert.True(t, closed.Load())
	assert.Equal(t, []int{0}, ordinals)
	assert.Equal(t, []string{h1KeywordClosedBody}, env.bodies())
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageBodyDeclined, progress.Stage)
	assert.Empty(t, progress.Body)
	assert.Equal(t, h1KeywordClosedBody, progress.Decline)
}

func TestH1KeywordProgress_HandoverRollsBackWithLaterCutoffFailure(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	cutoffErr := errors.New("synthetic cutoff failure after keyword arbitration")
	var armed, failed atomic.Bool
	var handoverWrites atomic.Int32
	name := "h1:keyword_cutoff_rollback"
	require.NoError(t, env.pool.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" {
			return
		}
		if update, ok := tx.Statement.Dest.(map[string]any); ok {
			if metadata, ok := update["metadata"].(models.JSONB); ok {
				if progress, err := readInboundKeywordProgress(metadata); err == nil &&
					progress != nil && progress.Stage == inboundKeywordStageHandover {
					handoverWrites.Add(1)
					armed.Store(true)
					return
				}
			}
		}
		if armed.Load() && failed.CompareAndSwap(false, true) {
			_ = tx.AddError(cutoffErr)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Update().Remove(name) })

	err := env.run(env.baseTime)
	require.ErrorIs(t, err, cutoffErr)
	assert.True(t, failed.Load())
	assert.Equal(t, int32(1), handoverWrites.Load())
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageBody, progress.Stage, "phase must roll back with the transfer")
	assert.Equal(t, models.ScheduledJobStatusPending, env.job(t).Status)

	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	progress = h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageHandover, progress.Stage)
}

func TestH1KeywordProgress_ActiveTransferStillRunsTerminalPhase(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	h1StoreKeywordPhase(t, env, inboundKeywordProgress{
		Stage: inboundKeywordStageBody,
		Body:  h1KeywordTransferBody,
	})
	existing := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  env.org.ID,
		ContactID:       env.work.Contact.ID,
		WhatsAppAccount: env.account.Name,
		PhoneNumber:     env.work.Contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}
	require.NoError(t, env.shared.Create(&existing).Error)
	env.processor.process = func(ctx context.Context, scoped *App, work *persistedIncomingMessage) error {
		require.True(t, inboundKeywordProgressDeclared(work.Persisted.Metadata))
		var active int64
		require.NoError(t, scoped.DB.Model(&models.AgentTransfer{}).
			Where("organization_id = ? AND contact_id = ? AND status = ?",
				env.org.ID, work.Contact.ID, models.TransferStatusActive).
			Count(&active).Error)
		require.EqualValues(t, 1, active)
		return scoped.continuePersistedIncomingMessage(ctx, work)
	}

	require.NoError(t, env.run(env.baseTime))
	var transfers []models.AgentTransfer
	require.NoError(t, env.shared.
		Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).
		Find(&transfers).Error)
	require.Len(t, transfers, 1)
	assert.Equal(t, existing.ID, transfers[0].ID)
	assert.Equal(t, models.TransferSourceManual, transfers[0].Source)
	assert.Empty(t, env.bodies(), "an existing active transfer keeps its prior no-body behavior")
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageHandover, progress.Stage)
	var contact models.Contact
	require.NoError(t, env.shared.
		Where("id = ? AND organization_id = ?", env.work.Contact.ID, env.org.ID).
		First(&contact).Error)
	_, cutoffExists := contact.Metadata[contacthandoff.CutoffKey]
	assert.False(t, cutoffExists, "an existing active transfer must not be cut off again")
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
}

func TestH1KeywordProgress_RetryableCanonicalWriteRebuildsArbitration(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	var creates, handoverWrites atomic.Int32
	var injected atomic.Bool
	createName := "h1:keyword_retryable_create"
	require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register(createName, func(tx *gorm.DB) {
		transfer, ok := tx.Statement.Dest.(*models.AgentTransfer)
		if !ok || transfer.OrganizationID != env.org.ID {
			return
		}
		creates.Add(1)
		if injected.CompareAndSwap(false, true) {
			_ = tx.AddError(contactutil.ErrCanonicalContactChanged)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove(createName) })

	updateName := "h1:keyword_retryable_phase"
	require.NoError(t, env.pool.Callback().Update().Before("gorm:update").Register(updateName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" {
			return
		}
		update, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		metadata, ok := update["metadata"].(models.JSONB)
		if !ok {
			return
		}
		progress, err := readInboundKeywordProgress(metadata)
		if err == nil && progress != nil && progress.Stage == inboundKeywordStageHandover {
			handoverWrites.Add(1)
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Update().Remove(updateName) })

	require.NoError(t, env.run(env.baseTime))
	assert.True(t, injected.Load())
	assert.GreaterOrEqual(t, creates.Load(), int32(2))
	assert.GreaterOrEqual(t, handoverWrites.Load(), int32(2))
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageHandover, progress.Stage)
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
}

func TestH1KeywordProgress_DecisionWinnerOrders(t *testing.T) {
	for _, winner := range []string{"decline", "handover"} {
		t.Run(winner, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			h1StoreKeywordPhase(t, env, inboundKeywordProgress{
				Stage: inboundKeywordStageBody,
				Body:  h1KeywordTransferBody,
			})
			job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
			require.NotNil(t, job)
			now := env.baseTime.UTC()
			require.NoError(t, env.shared.Model(&models.ScheduledJob{}).
				Where("id = ? AND organization_id = ?", job.ID, env.org.ID).
				Updates(map[string]any{
					"status":    models.ScheduledJobStatusProcessing,
					"locked_at": now,
					"locked_by": "keyword-winner-test",
				}).Error)
			execution := env.app.scopedApp(env.pool, env.org.ID)
			execution.inboundContinuation = &inboundContinuationExecution{
				OrganizationID: env.org.ID,
				ContactID:      env.work.Contact.ID,
				MessageID:      env.work.Persisted.ID,
				WAMID:          env.work.Message.ID,
				jobID:          job.ID,
				workerID:       "keyword-winner-test",
				lease:          5 * time.Minute,
				leaseNow:       func() time.Time { return now },
				ownershipCtx:   context.Background(),
			}
			decide := func(decline *string) (inboundKeywordTransferDecision, error) {
				var decision inboundKeywordTransferDecision
				err := env.pool.Transaction(func(tx *gorm.DB) error {
					if err := database.LockOrganizationPolicyScope(tx, env.org.ID); err != nil {
						return err
					}
					var canonical models.Contact
					if err := tx.Where("id = ? AND organization_id = ?", env.work.Contact.ID, env.org.ID).
						First(&canonical).Error; err != nil {
						return err
					}
					var err error
					decision, err = execution.decideInboundKeywordTransferTx(
						tx,
						env.account,
						canonical.ID,
						decline,
					)
					return err
				})
				return decision, err
			}
			reply := h1KeywordClosedBody
			if winner == "decline" {
				first, err := decide(&reply)
				require.NoError(t, err)
				assert.Equal(t, inboundKeywordTransferDecline, first.Kind)
				second, err := decide(nil)
				require.NoError(t, err)
				assert.Equal(t, inboundKeywordTransferDecline, second.Kind)
				progress := h1KeywordPhase(t, env)
				require.NotNil(t, progress)
				assert.Equal(t, inboundKeywordStageBodyDeclined, progress.Stage)
				assert.Equal(t, reply, progress.Decline)
				created, err := execution.createTransferFromKeywordWithPhase(
					env.account,
					&env.work.Contact,
				)
				require.NoError(t, err)
				assert.False(t, created)
				assert.Equal(t, []string{reply}, env.bodies())
				assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
			} else {
				first, err := decide(nil)
				require.NoError(t, err)
				assert.Equal(t, inboundKeywordTransferCreate, first.Kind)
				second, err := decide(&reply)
				require.NoError(t, err)
				assert.Equal(t, inboundKeywordTransferExisting, second.Kind)
				progress := h1KeywordPhase(t, env)
				require.NotNil(t, progress)
				assert.Equal(t, inboundKeywordStageHandover, progress.Stage)
				settings := env.settings
				settings.BusinessHours.OutOfHoursMessage = h1KeywordClosedBody
				require.NoError(t, execution.runInboundKeywordProgress(
					env.account,
					&env.work.Contact,
					&settings,
					nil,
				))
				assert.Empty(t, env.bodies(), "a committed handover must suppress a later OOH reply")
				assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
			}
		})
	}
}

func TestH1KeywordProgress_CanonicalContactMismatchRequiresReview(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	h1StoreKeywordPhase(t, env, inboundKeywordProgress{
		Stage: inboundKeywordStageBody,
		Body:  h1KeywordTransferBody,
	})
	job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	now := env.baseTime.UTC()
	require.NoError(t, env.shared.Model(&models.ScheduledJob{}).
		Where("id = ? AND organization_id = ?", job.ID, env.org.ID).
		Updates(map[string]any{
			"status":    models.ScheduledJobStatusProcessing,
			"locked_at": now,
			"locked_by": "keyword-canonical-test",
		}).Error)
	execution := env.app.scopedApp(env.pool, env.org.ID)
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: env.org.ID,
		ContactID:      env.work.Contact.ID,
		MessageID:      env.work.Persisted.ID,
		WAMID:          env.work.Message.ID,
		jobID:          job.ID,
		workerID:       "keyword-canonical-test",
		lease:          5 * time.Minute,
		leaseNow:       func() time.Time { return now },
	}
	err := env.pool.Transaction(func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, env.org.ID); err != nil {
			return err
		}
		_, err := execution.decideInboundKeywordTransferTx(
			tx,
			env.account,
			uuid.New(),
			nil,
		)
		return err
	})
	require.True(t, inboundContinuationRequiresManualReview(err))
	progress := h1KeywordPhase(t, env)
	require.NotNil(t, progress)
	assert.Equal(t, inboundKeywordStageBody, progress.Stage)
}

func TestH1KeywordProgress_CanonicalCallbackUsesOnlyItsConnection(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	env.sqlPool.SetMaxOpenConns(1)
	env.sqlPool.SetMaxIdleConns(1)
	h1StoreKeywordPhase(t, env, inboundKeywordProgress{
		Stage: inboundKeywordStageBody,
		Body:  h1KeywordTransferBody,
	})
	job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	now := env.baseTime.UTC()
	require.NoError(t, env.shared.Model(&models.ScheduledJob{}).
		Where("id = ? AND organization_id = ?", job.ID, env.org.ID).
		Updates(map[string]any{
			"status":    models.ScheduledJobStatusProcessing,
			"locked_at": now,
			"locked_by": "keyword-single-connection",
		}).Error)
	execution := env.app.scopedApp(env.pool, env.org.ID)
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: env.org.ID,
		ContactID:      env.work.Contact.ID,
		MessageID:      env.work.Persisted.ID,
		WAMID:          env.work.Message.ID,
		jobID:          job.ID,
		workerID:       "keyword-single-connection",
		lease:          5 * time.Minute,
		leaseNow:       func() time.Time { return now },
	}
	settings := env.settings
	transfer := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  env.org.ID,
		ContactID:       env.work.Contact.ID,
		WhatsAppAccount: env.account.Name,
		PhoneNumber:     env.work.Contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceKeyword,
		TransferredAt:   now,
	}
	var decision inboundKeywordTransferDecision
	err := execution.saveAndFinalizeTransferWithArbiter(
		&transfer,
		env.account,
		&env.work.Contact,
		&settings,
		false,
		nil,
		func(tx *gorm.DB, canonicalContactID uuid.UUID) (inboundKeywordTransferDecision, error) {
			var arbiterErr error
			decision, arbiterErr = execution.decideInboundKeywordTransferTx(
				tx,
				env.account,
				canonicalContactID,
				nil,
			)
			return decision, arbiterErr
		},
	)
	require.NoError(t, err)
	assert.Equal(t, inboundKeywordTransferCreate, decision.Kind)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	assert.Empty(t, env.bodies(), "the canonical callback must not invoke a provider")
}

func TestH1KeywordProgress_StaleLeaseAndCompletedMessageFailClosed(t *testing.T) {
	for _, scenario := range []string{"stale_lease", "completed"} {
		t.Run(scenario, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			h1StoreKeywordPhase(t, env, inboundKeywordProgress{
				Stage: inboundKeywordStageBody,
				Body:  h1KeywordTransferBody,
			})
			job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
			now := env.baseTime.UTC()
			lockedAt := now
			if scenario == "stale_lease" {
				lockedAt = now.Add(-10 * time.Minute)
			}
			require.NoError(t, env.shared.Model(&models.ScheduledJob{}).
				Where("id = ? AND organization_id = ?", job.ID, env.org.ID).
				Updates(map[string]any{
					"status":    models.ScheduledJobStatusProcessing,
					"locked_at": lockedAt,
					"locked_by": "keyword-lease-test",
				}).Error)
			if scenario == "completed" {
				message := h1KeywordPhaseMessage(t, env)
				metadata := cloneInboundContinuationJSONB(message.Metadata)
				metadata["inbound_continuation_completed"] = true
				require.NoError(t, env.shared.Model(&models.Message{}).
					Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
					Update("metadata", metadata).Error)
			}
			execution := env.app.scopedApp(env.pool, env.org.ID)
			execution.inboundContinuation = &inboundContinuationExecution{
				OrganizationID: env.org.ID,
				ContactID:      env.work.Contact.ID,
				MessageID:      env.work.Persisted.ID,
				WAMID:          env.work.Message.ID,
				jobID:          job.ID,
				workerID:       "keyword-lease-test",
				lease:          5 * time.Minute,
				leaseNow:       func() time.Time { return now },
			}
			_, err := execution.keywordProgress(env.account, &env.work.Contact, nil)
			if scenario == "completed" {
				require.True(t, inboundContinuationStoppedByPolicy(err))
			} else {
				require.Error(t, err)
				assert.False(t, inboundContinuationRequiresManualReview(err))
				assert.Contains(t, err.Error(), "lease is stale")
			}
			assert.Empty(t, env.bodies())
			assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
		})
	}
}

func TestH1KeywordProgress_ConservativeReplayBoundaries(t *testing.T) {
	for _, scenario := range []string{"legacy_unfinished", "unknown_version", "foreign_binding", "foreign_account", "disabled", "uncertain_action"} {
		t.Run(scenario, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			switch scenario {
			case "legacy_unfinished":
				legacy := models.ScheduledJob{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: env.org.ID,
					Kind:           inboundContinuationActionJobKind,
					AggregateType:  "inbound_message_action",
					AggregateID:    &env.work.Persisted.ID,
					Status:         models.ScheduledJobStatusCompleted,
					MaxAttempts:    1,
					IdempotencyKey: "legacy-keyword-action",
					Payload: models.JSONB{
						"inbound_message_id": env.work.Persisted.ID.String(),
						"effect_kind":        "text",
						"action_scope":       "continuation",
						"action_ordinal":     0,
						"state":              inboundActionStateResolved,
					},
				}
				require.NoError(t, env.shared.Create(&legacy).Error)
			default:
				h1StoreKeywordPhase(t, env, inboundKeywordProgress{
					Stage: inboundKeywordStageBody,
					Body:  h1KeywordTransferBody,
				})
				switch scenario {
				case "unknown_version":
					message := h1KeywordPhaseMessage(t, env)
					metadata := cloneInboundContinuationJSONB(message.Metadata)
					raw := h1KeywordPhaseRaw(t, metadata)
					raw["version"] = 2
					metadata[inboundKeywordProgressKey] = raw
					require.NoError(t, env.shared.Model(&models.Message{}).
						Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
						Update("metadata", metadata).Error)
				case "foreign_binding", "foreign_account":
					message := h1KeywordPhaseMessage(t, env)
					metadata := cloneInboundContinuationJSONB(message.Metadata)
					raw := h1KeywordPhaseRaw(t, metadata)
					if scenario == "foreign_binding" {
						raw["message_id"] = uuid.NewString()
					} else {
						raw["account_id"] = uuid.NewString()
					}
					metadata[inboundKeywordProgressKey] = raw
					require.NoError(t, env.shared.Model(&models.Message{}).
						Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
						Update("metadata", metadata).Error)
				case "disabled":
					require.NoError(t, env.shared.Model(&models.ChatbotSettings{}).
						Where("id = ? AND organization_id = ?", env.settings.ID, env.org.ID).
						Update("is_enabled", false).Error)
					key := settingsCachePrefix + env.org.ID.String() + ":" + env.account.Name
					require.NoError(t, env.app.Redis.Del(context.Background(), key).Err())
				case "uncertain_action":
					actionKey := "inbound-action:" + uuid.NewSHA1(
						env.work.Persisted.ID,
						[]byte("continuation:0:text"),
					).String()
					action := models.ScheduledJob{
						BaseModel:      models.BaseModel{ID: uuid.New()},
						OrganizationID: env.org.ID,
						Kind:           inboundContinuationActionJobKind,
						AggregateType:  "inbound_message_action",
						AggregateID:    &env.work.Persisted.ID,
						Status:         models.ScheduledJobStatusProcessing,
						MaxAttempts:    1,
						IdempotencyKey: actionKey,
						Payload: models.JSONB{
							"inbound_message_id": env.work.Persisted.ID.String(),
							"effect_kind":        "text",
							"action_scope":       "continuation",
							"action_ordinal":     0,
							"state":              inboundActionStatePreAttempt,
						},
					}
					require.NoError(t, env.shared.Create(&action).Error)
				}
			}
			err := env.run(env.baseTime)
			require.True(t, inboundContinuationRequiresManualReview(err), "actual error: %v", err)
			assert.Empty(t, env.bodies())
			assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
			job := env.job(t)
			assert.Equal(t, models.ScheduledJobStatusFailed, job.Status)
			assert.Equal(t, true, job.Payload["manual_review_required"])
		})
	}
}

func TestH1KeywordProgress_CompletedLegacyIsSkipped(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	message := h1KeywordPhaseMessage(t, env)
	metadata := cloneInboundContinuationJSONB(message.Metadata)
	metadata["inbound_continuation_completed"] = true
	require.NoError(t, env.shared.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, env.org.ID).
		Update("metadata", metadata).Error)
	require.NoError(t, env.run(env.baseTime))
	assert.Empty(t, env.bodies())
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
}
