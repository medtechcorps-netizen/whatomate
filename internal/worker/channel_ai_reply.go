package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultChannelAIReplyBatchSize    = 20
	defaultChannelAIReplyPollInterval = time.Second
	defaultChannelAIReplyLease        = 2 * time.Minute
	maxChannelAIReplyTokens           = 160
	maxChannelAIContextRunes          = 16000
	maxChannelAIReplyRunes            = 4000

	channelAISocialSafetySuffix = "Non-overridable social-channel safety rules: sound warm and natural, but keep routine replies to 1-3 short sentences and ask at most one useful follow-up question. Avoid long lists, repetition, and excessive emoji. Provide customer-service information only. Never diagnose, prescribe, recommend a dosage, or claim clinical certainty. For urgent or emergency symptoms, tell the customer to contact local emergency services or seek urgent in-person care. Collect only the minimum information needed and offer human handover or booking. Use English or Bahasa Melayu Malaysia according to the configured prompt and customer; never use Bahasa Indonesia."
)

type channelAIReplySnapshot struct {
	Job                 models.ScheduledJob
	Account             models.ChannelAccount
	Conversation        models.InboxConversation
	Inbound             models.Message
	Contact             models.Contact
	Identity            *models.ContactIdentity
	Settings            models.ChatbotSettings
	ServiceWindowEndsAt time.Time
	UserText            string
	Messages            []qwenapi.Message
	APIKey              string
	MaxTokens           int
}

type channelAIReplyCheck struct {
	Snapshot      channelAIReplySnapshot
	CancelReason  string
	AlreadyQueued bool
}

// RunChannelAIReplies runs the durable Qwen reply loop until cancellation.
func (w *Worker) RunChannelAIReplies(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return errors.New("channel AI reply worker requires a database")
	}
	workerID := "channel-ai-reply:" + uuid.NewString()
	ticker := time.NewTicker(defaultChannelAIReplyPollInterval)
	defer ticker.Stop()

	for {
		if _, err := w.ProcessChannelAIReplyBatch(
			ctx,
			workerID,
			defaultChannelAIReplyBatchSize,
		); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.Log.Error("Channel AI reply batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ProcessChannelAIReplyBatch discovers only tenant IDs outside RLS, then
// claims and processes every scheduled job through database.WithTenant.
func (w *Worker) ProcessChannelAIReplyBatch(
	ctx context.Context,
	workerID string,
	limit int,
) (int, error) {
	if w == nil || w.DB == nil {
		return 0, errors.New("channel AI reply worker requires a database")
	}
	if strings.TrimSpace(workerID) == "" {
		return 0, errors.New("channel AI reply worker ID is required")
	}
	if limit <= 0 || limit > 100 {
		limit = defaultChannelAIReplyBatchSize
	}

	now := time.Now().UTC()
	organizationIDs, err := w.listReadyChannelAIReplyOrganizations(
		limit,
		now,
		now.Add(-defaultChannelAIReplyLease),
	)
	if err != nil {
		return 0, err
	}

	processed := 0
	var batchErrors []error
	for _, organizationID := range organizationIDs {
		if processed >= limit || ctx.Err() != nil {
			break
		}
		jobID, claimed, claimErr := w.claimChannelAIReplyJob(organizationID, workerID)
		if claimErr != nil {
			batchErrors = append(
				batchErrors,
				fmt.Errorf("claim tenant %s: %w", organizationID, claimErr),
			)
			continue
		}
		if !claimed {
			continue
		}
		processed++
		if processErr := w.processChannelAIReplyJob(
			ctx,
			organizationID,
			jobID,
			workerID,
		); processErr != nil {
			batchErrors = append(
				batchErrors,
				fmt.Errorf("process channel AI reply %s: %w", jobID, processErr),
			)
		}
	}
	return processed, errors.Join(batchErrors...)
}

func (w *Worker) listReadyChannelAIReplyOrganizations(
	limit int,
	now, staleBefore time.Time,
) ([]uuid.UUID, error) {
	cursor := uuid.New()
	var organizationIDs []uuid.UUID
	if w.Config != nil && w.Config.Database.RLSEnabled {
		if err := w.DB.Raw(
			"SELECT * FROM public.rereply_ready_channel_ai_reply_orgs(?, ?, ?)",
			cursor,
			limit,
			staleBefore,
		).Scan(&organizationIDs).Error; err != nil {
			return nil, fmt.Errorf("list channel AI reply tenants: %w", err)
		}
		return organizationIDs, nil
	}

	if err := w.DB.Raw(`
		WITH ready AS (
			SELECT DISTINCT organization_id
			FROM scheduled_jobs
			WHERE deleted_at IS NULL
			  AND kind = ?
			  AND run_at <= ?
			  AND (
			    status = ?
			    OR (
			      status = ?
			      AND (locked_at IS NULL OR locked_at < ?)
			    )
			  )
		)
		SELECT organization_id
		FROM ready
		ORDER BY (organization_id > ?) DESC, organization_id
		LIMIT ?
	`,
		models.ScheduledJobKindChannelAIReply,
		now,
		models.ScheduledJobStatusPending,
		models.ScheduledJobStatusProcessing,
		staleBefore,
		cursor,
		limit,
	).Scan(&organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list development channel AI reply tenants: %w", err)
	}
	return organizationIDs, nil
}

func (w *Worker) claimChannelAIReplyJob(
	organizationID uuid.UUID,
	workerID string,
) (uuid.UUID, bool, error) {
	var jobID uuid.UUID
	claimed := false
	err := database.WithTenant(w.DB, organizationID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		staleBefore := now.Add(-defaultChannelAIReplyLease)
		var candidate struct {
			ID uuid.UUID
		}
		if err := tx.Raw(`
			SELECT id
			FROM scheduled_jobs
			WHERE organization_id = ?
			  AND deleted_at IS NULL
			  AND kind = ?
			  AND run_at <= ?
			  AND (
			    status = ?
			    OR (
			      status = ?
			      AND (locked_at IS NULL OR locked_at < ?)
			    )
			  )
			ORDER BY run_at, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		`,
			organizationID,
			models.ScheduledJobKindChannelAIReply,
			now,
			models.ScheduledJobStatusPending,
			models.ScheduledJobStatusProcessing,
			staleBefore,
		).Scan(&candidate).Error; err != nil {
			return err
		}
		if candidate.ID == uuid.Nil {
			return nil
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ?",
				candidate.ID,
				organizationID,
				models.ScheduledJobKindChannelAIReply,
			).
			Updates(map[string]any{
				"status":     models.ScheduledJobStatusProcessing,
				"attempts":   gorm.Expr("attempts + 1"),
				"locked_at":  now,
				"locked_by":  workerID,
				"last_error": "",
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel AI reply claim was lost")
		}
		jobID = candidate.ID
		claimed = true
		return nil
	})
	return jobID, claimed, err
}

func (w *Worker) processChannelAIReplyJob(
	ctx context.Context,
	organizationID, jobID uuid.UUID,
	workerID string,
) error {
	check, err := w.loadChannelAIReplyCheck(organizationID, jobID, workerID, true)
	if err != nil {
		return err
	}
	if check.AlreadyQueued {
		return w.settleChannelAIReplyJob(
			organizationID,
			jobID,
			workerID,
			models.ScheduledJobStatusCompleted,
			"",
		)
	}
	if check.CancelReason != "" {
		return w.settleChannelAIReplyJob(
			organizationID,
			jobID,
			workerID,
			models.ScheduledJobStatusCancelled,
			check.CancelReason,
		)
	}

	baseURL := qwenapi.DefaultBaseURL
	if w.Config != nil && w.Config.AI.QwenBaseURL != "" {
		baseURL = w.Config.AI.QwenBaseURL
	}
	response, err := qwenapi.Generate(ctx, w.QwenHTTP, qwenapi.Options{
		APIKey:      check.Snapshot.APIKey,
		BaseURL:     baseURL,
		Model:       check.Snapshot.Settings.AI.Model,
		MaxTokens:   check.Snapshot.MaxTokens,
		Temperature: check.Snapshot.Settings.AI.Temperature,
		Messages:    check.Snapshot.Messages,
	})
	if err != nil {
		return w.failChannelAIReplyJob(
			organizationID,
			&check.Snapshot.Job,
			workerID,
			err,
		)
	}
	response, err = normalizeChannelAIReply(response)
	if err != nil {
		return w.failChannelAIReplyJob(
			organizationID,
			&check.Snapshot.Job,
			workerID,
			err,
		)
	}
	return w.finalizeChannelAIReply(
		organizationID,
		jobID,
		workerID,
		response,
	)
}

func (w *Worker) loadChannelAIReplyCheck(
	organizationID, jobID uuid.UUID,
	workerID string,
	buildPrompt bool,
) (channelAIReplyCheck, error) {
	var check channelAIReplyCheck
	err := database.WithTenant(w.DB, organizationID, func(tx *gorm.DB) error {
		var job models.ScheduledJob
		if err := tx.Where(
			"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
			jobID,
			organizationID,
			models.ScheduledJobKindChannelAIReply,
			models.ScheduledJobStatusProcessing,
			workerID,
		).First(&job).Error; err != nil {
			return err
		}
		resolved, err := w.checkChannelAIReplyEligibility(
			tx,
			organizationID,
			&job,
			buildPrompt,
		)
		if err != nil {
			return err
		}
		check = resolved
		return nil
	})
	return check, err
}

func (w *Worker) checkChannelAIReplyEligibility(
	tx *gorm.DB,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	buildPrompt bool,
) (channelAIReplyCheck, error) {
	check := channelAIReplyCheck{}
	if tx == nil || job == nil {
		return check, errors.New("channel AI reply eligibility requires a job transaction")
	}
	check.Snapshot.Job = *job

	var payload models.ChannelAIReplyJobPayload
	if err := decodeChannelAIReplyJSON(job.Payload, &payload); err != nil {
		check.CancelReason = "invalid_job_payload"
		return check, nil
	}
	if payload.OrganizationID != organizationID ||
		payload.ChannelAccountID == uuid.Nil ||
		payload.ConversationID == uuid.Nil ||
		payload.InboundMessageID == uuid.Nil ||
		job.OrganizationID != organizationID ||
		job.AggregateType != models.ChannelAIReplyAggregateType ||
		job.AggregateID == nil ||
		*job.AggregateID != payload.InboundMessageID ||
		job.IdempotencyKey != models.ChannelAIReplyIdempotencyKey(payload.InboundMessageID) {
		check.CancelReason = "tenant_binding_mismatch"
		return check, nil
	}
	if payload.ServiceWindowAt.IsZero() {
		check.CancelReason = "invalid_job_service_window_time"
		return check, nil
	}

	var organization models.Organization
	if err := tx.Where("id = ?", organizationID).First(&organization).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			check.CancelReason = "organization_unavailable"
			return check, nil
		}
		return check, err
	}

	now := time.Now().UTC()
	var account models.ChannelAccount
	if err := tx.Where(
		"id = ? AND organization_id = ?",
		payload.ChannelAccountID,
		organizationID,
	).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			check.CancelReason = "channel_account_unavailable"
			return check, nil
		}
		return check, err
	}
	if account.OrganizationID != organizationID ||
		(account.Channel != models.ChannelInstagram &&
			account.Channel != models.ChannelMessenger) ||
		account.Provider != channelapi.RelayProvider {
		check.CancelReason = "channel_account_binding_mismatch"
		return check, nil
	}
	if account.Status != models.ChannelAccountStatusActive ||
		!channelOutboxBool(account.Config, "outbound_enabled") {
		check.CancelReason = "channel_account_outbound_disabled"
		return check, nil
	}
	if !channelAIReplyBoolValue(account.Config["ai_reply_enabled"]) {
		check.CancelReason = "channel_ai_disabled"
		return check, nil
	}
	jobWindowEndsAt := channelapi.InboundServiceWindowEndsAt(
		account.Channel,
		payload.ServiceWindowAt,
	)
	if jobWindowEndsAt == nil || !jobWindowEndsAt.After(now) {
		check.CancelReason = "inbound_service_window_expired"
		return check, nil
	}
	// Freeze the deadline opened by this job's originating inbound message.
	// Conversation state may advance after a newer customer message, but that
	// later window must never extend delivery eligibility for this older reply.
	check.Snapshot.ServiceWindowEndsAt = jobWindowEndsAt.UTC()

	var credentialCount int64
	if err := tx.Model(&models.ChannelCredential{}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
			organizationID,
			account.ID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			now,
		).
		Count(&credentialCount).Error; err != nil {
		return check, err
	}
	if credentialCount == 0 {
		check.CancelReason = "channel_credentials_unavailable"
		return check, nil
	}

	var conversation models.InboxConversation
	if err := tx.Where(
		"id = ? AND organization_id = ? AND channel_account_id = ?",
		payload.ConversationID,
		organizationID,
		account.ID,
	).First(&conversation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			check.CancelReason = "conversation_unavailable"
			return check, nil
		}
		return check, err
	}
	if conversation.OrganizationID != organizationID ||
		conversation.ChannelAccountID != account.ID ||
		conversation.Channel != account.Channel {
		check.CancelReason = "conversation_binding_mismatch"
		return check, nil
	}
	if conversation.ServiceWindowEndsAt == nil ||
		!conversation.ServiceWindowEndsAt.After(now) {
		check.CancelReason = "service_window_expired"
		return check, nil
	}
	if channelAIReplyBoolValue(conversation.Config[models.ConversationConfigAIPaused]) {
		check.CancelReason = "conversation_ai_paused"
		return check, nil
	}

	var inbound models.Message
	if err := tx.Where(
		"id = ? AND organization_id = ? AND inbox_conversation_id = ? AND contact_id = ?",
		payload.InboundMessageID,
		organizationID,
		conversation.ID,
		conversation.ContactID,
	).First(&inbound).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			check.CancelReason = "inbound_message_unavailable"
			return check, nil
		}
		return check, err
	}
	if inbound.OrganizationID != organizationID ||
		inbound.Direction != models.DirectionIncoming ||
		inbound.InboxConversationID == nil ||
		*inbound.InboxConversationID != conversation.ID {
		check.CancelReason = "inbound_message_binding_mismatch"
		return check, nil
	}
	userText, err := channelAIReplyMessageText(tx, organizationID, conversation.ID, &inbound)
	if err != nil {
		return check, err
	}
	if userText == "" {
		check.CancelReason = "inbound_message_has_no_text"
		return check, nil
	}

	var handoverCount int64
	if err := tx.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			organizationID,
			conversation.ContactID,
			models.TransferStatusActive,
		).
		Count(&handoverCount).Error; err != nil {
		return check, err
	}
	if handoverCount > 0 {
		check.CancelReason = "human_handover_active"
		return check, nil
	}

	var newerHumanReplyCount int64
	if err := tx.Model(&models.Message{}).
		Where(
			"organization_id = ? AND inbox_conversation_id = ? AND direction = ? AND sent_by_user_id IS NOT NULL AND created_at > ?",
			organizationID,
			conversation.ID,
			models.DirectionOutgoing,
			payload.ServiceWindowAt,
		).
		Count(&newerHumanReplyCount).Error; err != nil {
		return check, err
	}
	if newerHumanReplyCount > 0 {
		check.CancelReason = "newer_human_reply_exists"
		return check, nil
	}

	settings, settingsErr := resolveChannelAIReplySettings(
		tx,
		organizationID,
		account.Name,
	)
	if settingsErr != nil {
		if errors.Is(settingsErr, gorm.ErrRecordNotFound) {
			check.CancelReason = "channel_ai_profile_unavailable"
			return check, nil
		}
		return check, settingsErr
	}
	if !settings.IsEnabled ||
		!settings.AI.Enabled ||
		settings.AI.Provider != models.AIProviderQwen {
		check.CancelReason = "channel_ai_settings_disabled"
		return check, nil
	}
	encryptionKey := ""
	if w.Config != nil {
		encryptionKey = w.Config.App.EncryptionKey
	}
	apiKey, decryptErr := appcrypto.Decrypt(settings.AI.APIKey, encryptionKey)
	if decryptErr != nil || strings.TrimSpace(apiKey) == "" {
		check.CancelReason = "channel_ai_credentials_invalid"
		return check, nil
	}

	entitled, entitlementErr := channelapi.HasDurableOmnichannelEntitlement(
		tx,
		organizationID,
		now,
	)
	if entitlementErr != nil {
		return check, fmt.Errorf("evaluate omnichannel entitlement: %w", entitlementErr)
	}
	if !entitled {
		check.CancelReason = "omnichannel_not_entitled"
		return check, nil
	}
	consentAllowed, consentReason, consentErr := channelapi.OutboundConsentAllowed(
		tx,
		organizationID,
		conversation.ContactID,
		account.ID,
		account.Channel,
		models.ChannelPreferencePurposeService,
		now,
	)
	if consentErr != nil {
		return check, fmt.Errorf("evaluate channel AI reply consent: %w", consentErr)
	}
	if !consentAllowed {
		check.CancelReason = "consent_" + consentReason
		return check, nil
	}

	idempotencyKey := models.ChannelAIReplyIdempotencyKey(inbound.ID)
	var duplicateCount int64
	if err := tx.Unscoped().Model(&models.OutboxJob{}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
			organizationID,
			account.ID,
			idempotencyKey,
		).
		Count(&duplicateCount).Error; err != nil {
		return check, err
	}
	if duplicateCount == 0 {
		deterministicMessageID := channelAIReplyMessageID(idempotencyKey)
		if err := tx.Unscoped().Model(&models.Message{}).
			Where(
				"organization_id = ? AND id = ?",
				organizationID,
				deterministicMessageID,
			).
			Count(&duplicateCount).Error; err != nil {
			return check, err
		}
	}
	if duplicateCount > 0 {
		check.AlreadyQueued = true
		return check, nil
	}

	var contact models.Contact
	if err := tx.Where(
		"id = ? AND organization_id = ?",
		conversation.ContactID,
		organizationID,
	).First(&contact).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			check.CancelReason = "contact_unavailable"
			return check, nil
		}
		return check, err
	}
	var identity *models.ContactIdentity
	if conversation.ContactIdentityID != nil {
		var loaded models.ContactIdentity
		if err := tx.Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND contact_id = ?",
			*conversation.ContactIdentityID,
			organizationID,
			account.ID,
			contact.ID,
		).First(&loaded).Error; err == nil {
			identity = &loaded
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return check, err
		}
	}

	maxTokens := settings.AI.MaxTokens
	if maxTokens <= 0 || maxTokens > maxChannelAIReplyTokens {
		maxTokens = maxChannelAIReplyTokens
	}
	check.Snapshot.Account = account
	check.Snapshot.Conversation = conversation
	check.Snapshot.Inbound = inbound
	check.Snapshot.Contact = contact
	check.Snapshot.Identity = identity
	check.Snapshot.Settings = settings
	check.Snapshot.UserText = userText
	check.Snapshot.APIKey = apiKey
	check.Snapshot.MaxTokens = maxTokens
	if buildPrompt {
		messages, promptErr := buildChannelAIReplyMessages(
			tx,
			organization,
			account,
			conversation,
			contact,
			inbound,
			settings,
			userText,
		)
		if promptErr != nil {
			return check, promptErr
		}
		check.Snapshot.Messages = messages
	}
	return check, nil
}

func buildChannelAIReplyMessages(
	tx *gorm.DB,
	organization models.Organization,
	account models.ChannelAccount,
	conversation models.InboxConversation,
	contact models.Contact,
	inbound models.Message,
	settings models.ChatbotSettings,
	userText string,
) ([]qwenapi.Message, error) {
	historyLimit := settings.AI.HistoryLimit
	if historyLimit <= 0 {
		historyLimit = 4
	}
	if historyLimit > 10 {
		historyLimit = 10
	}
	var history []models.Message
	if settings.AI.IncludeHistory {
		if err := tx.Where(
			"organization_id = ? AND inbox_conversation_id = ? AND id <> ? AND created_at <= ?",
			organization.ID,
			conversation.ID,
			inbound.ID,
			inbound.CreatedAt,
		).
			Order("created_at DESC, id DESC").
			Limit(historyLimit).
			Find(&history).Error; err != nil {
			return nil, err
		}
	}

	var contexts []models.AIContext
	contextQuery := tx.Where(
		"organization_id = ? AND is_enabled = ?",
		organization.ID,
		true,
	)
	if settings.WhatsAppAccount == "" {
		contextQuery = contextQuery.Where("whats_app_account = ''")
	} else {
		contextQuery = contextQuery.Where(
			"(whats_app_account = ? OR whats_app_account = '')",
			settings.WhatsAppAccount,
		)
	}
	if err := contextQuery.
		Order("priority DESC, created_at").
		Limit(20).
		Find(&contexts).Error; err != nil {
		return nil, err
	}

	contextParts := []string{
		"Organization: " + strings.TrimSpace(organization.Name),
		"Channel profile: " + strings.TrimSpace(account.Name),
		"Customer: " + firstChannelAIReplyValue(
			contact.ProfileName,
			"Customer",
		),
	}
	for _, configured := range contexts {
		content := strings.TrimSpace(configured.StaticContent)
		if content == "" {
			continue
		}
		contextParts = append(
			contextParts,
			fmt.Sprintf("%s:\n%s", configured.Name, content),
		)
	}
	tenantContext := truncateChannelAIRunes(
		strings.Join(contextParts, "\n\n"),
		maxChannelAIContextRunes,
	)
	systemParts := make([]string, 0, 3)
	if configured := strings.TrimSpace(settings.AI.SystemPrompt); configured != "" {
		systemParts = append(systemParts, configured)
	}
	systemParts = append(systemParts,
		"Use only the current tenant context below. Do not claim actions were taken unless the context says so.\n\n"+
			tenantContext,
		"Reply directly to the customer in plain text only. Do not return JSON, Markdown, code fences, tool calls, analysis, or internal notes. Keep the reply concise.",
		channelAISocialSafetySuffix,
	)

	messages := []qwenapi.Message{{
		Role:    "system",
		Content: strings.Join(systemParts, "\n\n"),
	}}
	for index := len(history) - 1; index >= 0; index-- {
		text, err := channelAIReplyMessageText(
			tx,
			organization.ID,
			conversation.ID,
			&history[index],
		)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		role := "user"
		if history[index].Direction == models.DirectionOutgoing {
			role = "assistant"
		}
		messages = append(messages, qwenapi.Message{Role: role, Content: text})
	}
	messages = append(messages, qwenapi.Message{Role: "user", Content: userText})
	return messages, nil
}

// resolveChannelAIReplySettings uses deterministic, fail-closed precedence:
// an exact social profile, then an organization-global profile, then the
// newest explicitly enabled Qwen profile already used by another account.
// A present exact/global disabled or non-Qwen profile is authoritative and is
// rejected by the caller rather than silently bypassed.
func resolveChannelAIReplySettings(
	tx *gorm.DB,
	organizationID uuid.UUID,
	socialAccountName string,
) (models.ChatbotSettings, error) {
	return channelapi.ResolveAIReplySettings(tx, organizationID, socialAccountName)
}

func (w *Worker) finalizeChannelAIReply(
	organizationID, jobID uuid.UUID,
	workerID, response string,
) error {
	return database.WithTenant(w.DB, organizationID, func(tx *gorm.DB) error {
		var job models.ScheduledJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				jobID,
				organizationID,
				models.ScheduledJobKindChannelAIReply,
				models.ScheduledJobStatusProcessing,
				workerID,
			).
			First(&job).Error; err != nil {
			return err
		}
		check, err := w.checkChannelAIReplyEligibility(
			tx,
			organizationID,
			&job,
			false,
		)
		if err != nil {
			return err
		}
		if check.AlreadyQueued {
			return settleChannelAIReplyJobTx(
				tx,
				organizationID,
				job.ID,
				workerID,
				models.ScheduledJobStatusCompleted,
				"",
			)
		}
		if check.CancelReason != "" {
			return settleChannelAIReplyJobTx(
				tx,
				organizationID,
				job.ID,
				workerID,
				models.ScheduledJobStatusCancelled,
				check.CancelReason,
			)
		}

		snapshot := check.Snapshot
		idempotencyKey := models.ChannelAIReplyIdempotencyKey(snapshot.Inbound.ID)
		messageID := channelAIReplyMessageID(idempotencyKey)
		now := time.Now().UTC()
		frozenServiceWindowEndsAt := snapshot.ServiceWindowEndsAt.UTC()
		parts := []channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: response,
		}}
		outbound := channelapi.OutboundMessage{
			OrganizationID: organizationID,
			MessageID:      messageID,
			IdempotencyKey: idempotencyKey,
			Purpose:        models.ChannelPreferencePurposeService,
			Conversation: channelapi.ConversationRef{
				ID:         snapshot.Conversation.ID,
				ExternalID: snapshot.Conversation.ExternalConversationID,
				Subject:    snapshot.Conversation.Subject,
			},
			Recipient: channelapi.Participant{
				ID:         snapshot.Contact.ID,
				ExternalID: channelAIReplyIdentityValue(snapshot.Identity, "external_id"),
				Address:    channelAIReplyIdentityValue(snapshot.Identity, "address"),
				DisplayName: firstChannelAIReplyValue(
					channelAIReplyIdentityValue(snapshot.Identity, "display_name"),
					snapshot.Contact.ProfileName,
				),
				Role: models.ConversationParticipantRoleCustomer,
			},
			Parts:               parts,
			ServiceWindowEndsAt: &frozenServiceWindowEndsAt,
			Metadata: map[string]any{
				"sender_role":        models.ConversationParticipantRoleBot,
				"ai_generated":       true,
				"ai_settings_id":     snapshot.Settings.ID,
				"inbound_message_id": snapshot.Inbound.ID,
				"scheduled_job_id":   job.ID,
			},
		}
		payload, digest, err := channelAIReplyOutboxPayload(outbound)
		if err != nil {
			return err
		}
		message := models.Message{
			BaseModel: models.BaseModel{
				ID: messageID,
			},
			OrganizationID:      organizationID,
			WhatsAppAccount:     snapshot.Account.Name,
			ContactID:           snapshot.Contact.ID,
			ConversationID:      snapshot.Conversation.ExternalConversationID,
			InboxConversationID: &snapshot.Conversation.ID,
			Direction:           models.DirectionOutgoing,
			MessageType:         models.MessageTypeText,
			Content:             response,
			Status:              models.MessageStatusPending,
			Metadata: models.JSONB{
				"channel":            snapshot.Account.Channel,
				"provider":           snapshot.Account.Provider,
				"idempotency_key":    idempotencyKey,
				"payload_digest":     digest,
				"purpose":            models.ChannelPreferencePurposeService,
				"sender_role":        models.ConversationParticipantRoleBot,
				"ai_generated":       true,
				"ai_settings_id":     snapshot.Settings.ID.String(),
				"inbound_message_id": snapshot.Inbound.ID.String(),
				"scheduled_job_id":   job.ID.String(),
			},
		}
		messageParts := []models.MessagePart{{
			BaseModel: models.BaseModel{
				ID: uuid.NewSHA1(messageID, []byte("part:0")),
			},
			OrganizationID: organizationID,
			MessageID:      messageID,
			ConversationID: snapshot.Conversation.ID,
			Position:       0,
			Type:           models.MessagePartTypeText,
			Status:         models.MessagePartStatusReady,
			Text:           response,
			Payload:        models.JSONB{},
		}}
		outbox := models.OutboxJob{
			OrganizationID:   organizationID,
			ChannelAccountID: snapshot.Account.ID,
			ConversationID:   snapshot.Conversation.ID,
			MessageID:        &messageID,
			IdempotencyKey:   idempotencyKey,
			PayloadDigest:    digest,
			Purpose:          models.ChannelPreferencePurposeService,
			Status:           models.OutboxJobStatusPending,
			Priority:         snapshot.Conversation.Priority,
			AvailableAt:      now,
			MaxAttempts:      8,
			Payload:          payload,
		}
		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		if err := tx.Create(&messageParts).Error; err != nil {
			return err
		}
		if err := tx.Create(&outbox).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.InboxConversation{}).
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ?",
				snapshot.Conversation.ID,
				organizationID,
				snapshot.Account.ID,
			).
			Updates(map[string]any{
				"last_message_at":      now,
				"last_outbound_at":     now,
				"last_message_preview": response,
				"updated_at":           now,
			}).Error; err != nil {
			return err
		}
		return settleChannelAIReplyJobTx(
			tx,
			organizationID,
			job.ID,
			workerID,
			models.ScheduledJobStatusCompleted,
			"",
		)
	})
}

func (w *Worker) settleChannelAIReplyJob(
	organizationID, jobID uuid.UUID,
	workerID string,
	status models.ScheduledJobStatus,
	reason string,
) error {
	return database.WithTenant(w.DB, organizationID, func(tx *gorm.DB) error {
		return settleChannelAIReplyJobTx(
			tx,
			organizationID,
			jobID,
			workerID,
			status,
			reason,
		)
	})
}

func settleChannelAIReplyJobTx(
	tx *gorm.DB,
	organizationID, jobID uuid.UUID,
	workerID string,
	status models.ScheduledJobStatus,
	reason string,
) error {
	now := time.Now().UTC()
	result := tx.Model(&models.ScheduledJob{}).
		Where(
			"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
			jobID,
			organizationID,
			models.ScheduledJobKindChannelAIReply,
			models.ScheduledJobStatusProcessing,
			workerID,
		).
		Updates(map[string]any{
			"status":       status,
			"completed_at": now,
			"last_error":   reason,
			"locked_at":    nil,
			"locked_by":    "",
			"updated_at":   now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("channel AI reply settlement lost its lease")
	}
	return nil
}

func (w *Worker) failChannelAIReplyJob(
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	workerID string,
	processErr error,
) error {
	if job == nil {
		return processErr
	}
	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	deadLetter := job.Attempts >= maxAttempts
	nextStatus := models.ScheduledJobStatusPending
	if deadLetter {
		nextStatus = models.ScheduledJobStatusFailed
	}
	errorMessage := channelOutboxErrorMessage(processErr)
	persistErr := database.WithTenant(w.DB, organizationID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		updates := map[string]any{
			"status":     nextStatus,
			"last_error": errorMessage,
			"locked_at":  nil,
			"locked_by":  "",
			"updated_at": now,
		}
		if deadLetter {
			updates["completed_at"] = now
		} else {
			updates["run_at"] = now.Add(channelAIReplyBackoff(job.Attempts))
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID,
				organizationID,
				models.ScheduledJobKindChannelAIReply,
				models.ScheduledJobStatusProcessing,
				workerID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel AI reply failure update lost its lease")
		}
		return nil
	})
	if persistErr != nil {
		return errors.Join(processErr, persistErr)
	}
	return nil
}

func channelAIReplyBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		attempt = 20
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func channelAIReplyMessageText(
	tx *gorm.DB,
	organizationID, conversationID uuid.UUID,
	message *models.Message,
) (string, error) {
	if message == nil {
		return "", nil
	}
	if text := strings.TrimSpace(message.Content); text != "" {
		return text, nil
	}
	var parts []models.MessagePart
	if err := tx.Where(
		"organization_id = ? AND conversation_id = ? AND message_id = ?",
		organizationID,
		conversationID,
		message.ID,
	).Order("position").Find(&parts).Error; err != nil {
		return "", err
	}
	for _, part := range parts {
		if text := strings.TrimSpace(firstChannelAIReplyValue(part.Text, part.Caption)); text != "" {
			return text, nil
		}
	}
	return "", nil
}

func channelAIReplyOutboxPayload(
	outbound channelapi.OutboundMessage,
) (models.JSONB, string, error) {
	encoded, err := json.Marshal(outbound)
	if err != nil {
		return nil, "", fmt.Errorf("encode channel AI reply outbox payload: %w", err)
	}
	var payload models.JSONB
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, "", fmt.Errorf("decode channel AI reply outbox payload: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return payload, hex.EncodeToString(digest[:]), nil
}

func decodeChannelAIReplyJSON(payload models.JSONB, destination any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}

func normalizeChannelAIReply(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if strings.HasPrefix(value, "```") && strings.HasSuffix(value, "```") {
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "```"), "```"))
		if newline := strings.IndexByte(value, '\n'); newline >= 0 {
			firstLine := strings.TrimSpace(value[:newline])
			if !strings.Contains(firstLine, " ") {
				value = strings.TrimSpace(value[newline+1:])
			}
		}
	}
	if value == "" {
		return "", errors.New("qwen returned an empty customer reply")
	}
	if utf8.RuneCountInString(value) > maxChannelAIReplyRunes {
		value = truncateChannelAIRunes(value, maxChannelAIReplyRunes)
	}
	return value, nil
}

func channelAIReplyMessageID(idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("rereply:"+idempotencyKey))
}

func channelAIReplyBoolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized == "true" || normalized == "1" || normalized == "yes"
	default:
		return false
	}
}

func channelAIReplyIdentityValue(identity *models.ContactIdentity, field string) string {
	if identity == nil {
		return ""
	}
	switch field {
	case "external_id":
		return identity.ExternalID
	case "address":
		return identity.Address
	case "display_name":
		return identity.DisplayName
	default:
		return ""
	}
}

func firstChannelAIReplyValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateChannelAIRunes(value string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:maxRunes]))
}
