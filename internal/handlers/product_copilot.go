package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const copilotPromptVersion = "rereply-copilot-v1"

type runCopilotRequest struct {
	Instruction    string `json:"instruction,omitempty"`
	Tone           string `json:"tone,omitempty"`
	Language       string `json:"language,omitempty"`
	MessageLimit   int    `json:"message_limit,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type createCopilotFeedbackRequest struct {
	Rating         models.CopilotFeedbackRating `json:"rating,omitempty"`
	Accepted       *bool                        `json:"accepted,omitempty"`
	FinalMessageID *uuid.UUID                   `json:"final_message_id,omitempty"`
	EditDistance   *float64                     `json:"edit_distance,omitempty"`
	FinalText      string                       `json:"final_text,omitempty"`
	Reason         string                       `json:"reason,omitempty"`
	Metadata       models.JSONB                 `json:"metadata,omitempty"`
}

type copilotRunResponse struct {
	ID               uuid.UUID               `json:"id"`
	ContactID        uuid.UUID               `json:"contact_id"`
	RequestedByID    uuid.UUID               `json:"requested_by_id"`
	TaskType         models.CopilotTaskType  `json:"task_type"`
	Status           models.CopilotRunStatus `json:"status"`
	ResultText       string                  `json:"result_text,omitempty"`
	StructuredResult models.JSONB            `json:"structured_result"`
	SourceNames      models.StringArray      `json:"source_names"`
	SafetyWarnings   models.StringArray      `json:"safety_warnings"`
	SafetyLabels     models.StringArray      `json:"safety_labels"`
	LatencyMS        int64                   `json:"latency_ms"`
	ErrorCode        string                  `json:"error_code,omitempty"`
	CreatedAt        time.Time               `json:"created_at"`
	ExpiresAt        *time.Time              `json:"expires_at,omitempty"`
}

// RunContactCopilot invokes Qwen for one human-requested, grounded CRM task.
// It deliberately never sends a message; the returned draft must be reviewed
// and sent through an explicit messaging action.
func (a *App) RunContactCopilot(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCopilot, models.ActionExecute)
	if err != nil {
		return nil
	}
	if !a.hasCopilotContextAccess(userID, orgID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Copilot requires read access to contacts, conversations, and AI context",
			nil,
			"",
		)
	}
	contactID, err := parsePathUUID(r, "id", "contact")
	if err != nil {
		return nil
	}
	task, ok := parseCopilotTask(stringPathValue(r, "task"))
	if !ok {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Task must be reply, summary, qualify, or extract_actions",
			nil,
			"",
		)
	}

	var request runCopilotRequest
	if len(r.RequestCtx.PostBody()) > 0 {
		if err := a.decodeRequest(r, &request); err != nil {
			return nil
		}
	}
	request.Instruction = strings.TrimSpace(request.Instruction)
	request.Tone = strings.TrimSpace(request.Tone)
	request.Language = strings.TrimSpace(request.Language)
	if utf8.RuneCountInString(request.Instruction) > 2000 ||
		utf8.RuneCountInString(request.Tone) > 80 ||
		utf8.RuneCountInString(request.Language) > 80 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Copilot instructions are too long", nil, "")
	}
	if request.MessageLimit == 0 {
		request.MessageLimit = 24
	}
	if request.MessageLimit < 5 || request.MessageLimit > 50 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "message_limit must be between 5 and 50", nil, "")
	}

	var contact models.Contact
	if err := a.DB.Where("id = ? AND organization_id = ?", contactID, orgID).First(&contact).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}

	settings, err := a.resolveCopilotQwenSettings(orgID, contact.WhatsAppAccount)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"AI Copilot is not configured for this organization",
				nil,
				"",
			)
		}
		a.Log.Error("Failed to resolve Qwen Copilot settings", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load Copilot settings", nil, "")
	}

	var recent []models.Message
	if err := a.DB.
		Where("organization_id = ? AND contact_id = ?", orgID, contactID).
		Order("created_at DESC").
		Limit(request.MessageLimit).
		Find(&recent).Error; err != nil {
		a.Log.Error("Failed to load Copilot conversation context", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare Copilot context", nil, "")
	}
	reverseMessages(recent)

	var contexts []models.AIContext
	if err := a.DB.
		Where("organization_id = ? AND is_enabled = ? AND context_type = ? AND (whats_app_account = ? OR whats_app_account = '')",
			orgID, true, models.ContextTypeStatic, contact.WhatsAppAccount).
		Order("priority DESC, created_at ASC").
		Limit(10).
		Find(&contexts).Error; err != nil {
		a.Log.Error("Failed to load Copilot knowledge context", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare Copilot context", nil, "")
	}

	messageIDs := make(models.StringArray, 0, len(recent))
	for i := range recent {
		messageIDs = append(messageIDs, recent[i].ID.String())
	}
	sourceIDs := make(models.StringArray, 0, len(contexts))
	sourceNames := make(models.StringArray, 0, len(contexts)+1)
	sourceNames = append(sourceNames, "Recent conversation")
	for i := range contexts {
		sourceIDs = append(sourceIDs, contexts[i].ID.String())
		sourceNames = append(sourceNames, contexts[i].Name)
	}

	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek("Idempotency-Key")))
	}
	if utf8.RuneCountInString(idempotencyKey) > 255 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Idempotency key is too long", nil, "")
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}

	contextText := buildCopilotContext(&contact, recent, contexts)
	userPrompt := buildCopilotTaskPrompt(task, request)
	inputDigest := sha256.Sum256([]byte(string(task) + "\x00" + userPrompt + "\x00" + contextText))
	inputHash := hex.EncodeToString(inputDigest[:])
	var existing models.CopilotRun
	if err := a.DB.
		Where("organization_id = ? AND idempotency_key = ?", orgID, idempotencyKey).
		First(&existing).Error; err == nil {
		if existing.ContactID != contactID ||
			existing.TaskType != task ||
			existing.InputHash != inputHash {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"Idempotency key was already used for a different Copilot request",
				nil,
				"",
			)
		}
		return r.SendEnvelope(copilotRunToResponse(&existing))
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start Copilot", nil, "")
	}

	now := time.Now().UTC()
	retentionDays := settings.retentionDays
	expiresAt := now.Add(time.Duration(retentionDays) * 24 * time.Hour)
	run := models.CopilotRun{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgID,
		ContactID:          contactID,
		RequestedByID:      userID,
		TaskType:           task,
		Status:             models.CopilotRunStatusRunning,
		Model:              settings.chatbot.AI.Model,
		PromptVersion:      copilotPromptVersion,
		InputMessageIDs:    messageIDs,
		InputHash:          inputHash,
		ResultData:         models.JSONB{},
		ContextSourceIDs:   sourceIDs,
		ContextSourceNames: sourceNames,
		SafetyLabels:       models.StringArray{"human_review_required", "no_auto_send"},
		Warnings:           models.StringArray{"Review this output before use. Copilot never sends messages automatically."},
		IdempotencyKey:     idempotencyKey,
		ExpiresAt:          &expiresAt,
		Version:            1,
	}
	if err := a.DB.Create(&run).Error; err != nil {
		if isUniqueConstraintError(err) {
			if lookupErr := a.DB.
				Where("organization_id = ? AND idempotency_key = ?", orgID, idempotencyKey).
				First(&existing).Error; lookupErr == nil {
				if existing.ContactID != contactID ||
					existing.TaskType != task ||
					existing.InputHash != inputHash {
					return r.SendErrorEnvelope(
						fasthttp.StatusConflict,
						"Idempotency key was already used for a different Copilot request",
						nil,
						"",
					)
				}
				return r.SendEnvelope(copilotRunToResponse(&existing))
			}
		}
		a.Log.Error("Failed to persist Copilot run", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start Copilot", nil, "")
	}

	started := time.Now()
	result, generationErr := a.generateQwenResponse(
		settings.chatbot,
		nil,
		userPrompt,
		contextText,
	)
	latencyMS := time.Since(started).Milliseconds()
	if generationErr != nil {
		a.Log.Error("Qwen Copilot request failed", "error", generationErr, "run_id", run.ID)
		run.Status = models.CopilotRunStatusFailed
		run.ErrorCode = "provider_error"
		run.ErrorMessage = "AI request failed"
		run.LatencyMS = latencyMS
		run.Version++
		if err := a.DB.Save(&run).Error; err != nil {
			a.Log.Error("Failed to record Copilot error", "error", err, "run_id", run.ID)
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "AI Copilot is temporarily unavailable", nil, "")
	}

	run.Status = models.CopilotRunStatusCompleted
	run.ResultText = strings.TrimSpace(result)
	run.ResultData = parseCopilotStructuredResult(task, result)
	run.LatencyMS = latencyMS
	run.Version++
	if err := a.DB.Save(&run).Error; err != nil {
		a.Log.Error("Failed to save Copilot result", "error", err, "run_id", run.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save Copilot result", nil, "")
	}
	a.logAudit(orgID, userID, "copilot_run", run.ID, models.AuditActionCreated, nil, map[string]any{
		"task_type": task,
		"model":     run.Model,
		"status":    run.Status,
	})
	return r.SendEnvelope(copilotRunToResponse(&run))
}

func (a *App) hasCopilotContextAccess(userID, orgID uuid.UUID) bool {
	for _, permission := range []struct {
		resource string
		action   string
	}{
		{models.ResourceContacts, models.ActionRead},
		{models.ResourceChat, models.ActionRead},
		{models.ResourceChatbotAI, models.ActionRead},
	} {
		if !a.HasPermission(userID, permission.resource, permission.action, orgID) {
			return false
		}
	}
	return true
}

// ListCopilotRuns returns a tenant-scoped audit history without model secrets
// or prompt bodies.
func (a *App) ListCopilotRuns(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCopilot, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.CopilotRun{}).Where("organization_id = ?", orgID)
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("contact_id"))); raw != "" {
		contactID, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact ID", nil, "")
		}
		query = query.Where("contact_id = ?", contactID)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("task_type"))); raw != "" {
		task, valid := parseCopilotTask(raw)
		if !valid {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid Copilot task type", nil, "")
		}
		query = query.Where("task_type = ?", task)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list Copilot runs", nil, "")
	}
	var runs []models.CopilotRun
	if err := pg.Apply(query).Order("created_at DESC").Find(&runs).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list Copilot runs", nil, "")
	}
	response := make([]copilotRunResponse, len(runs))
	for i := range runs {
		response[i] = copilotRunToResponse(&runs[i])
	}
	return r.SendEnvelope(listEnvelope("runs", response, total, pg))
}

// CreateCopilotFeedback records human review and can link the message the user
// explicitly chose to send. It never performs the send itself.
func (a *App) CreateCopilotFeedback(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCopilot, models.ActionExecute)
	if err != nil {
		return nil
	}
	runID, err := parsePathUUID(r, "id", "Copilot run")
	if err != nil {
		return nil
	}
	var request createCopilotFeedbackRequest
	if err := a.decodeRequest(r, &request); err != nil {
		return nil
	}
	if request.Rating != "" &&
		request.Rating != models.CopilotFeedbackRatingHelpful &&
		request.Rating != models.CopilotFeedbackRatingNotHelpful {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid feedback rating", nil, "")
	}
	if request.EditDistance != nil && (*request.EditDistance < 0 || *request.EditDistance > 1) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Edit distance must be between 0 and 1", nil, "")
	}
	if utf8.RuneCountInString(strings.TrimSpace(request.Reason)) > 2000 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Feedback reason is too long", nil, "")
	}
	request.FinalText = strings.TrimSpace(request.FinalText)
	if utf8.RuneCountInString(request.FinalText) > 12000 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Reviewed final text is too long", nil, "")
	}

	var run models.CopilotRun
	if err := a.DB.Where("id = ? AND organization_id = ?", runID, orgID).First(&run).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Copilot run not found", nil, "")
	}
	if request.FinalMessageID != nil {
		var count int64
		if err := a.DB.Model(&models.Message{}).
			Where("id = ? AND organization_id = ? AND contact_id = ?", *request.FinalMessageID, orgID, run.ContactID).
			Count(&count).Error; err != nil || count != 1 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Final message is not part of this contact", nil, "")
		}
	}

	feedback := models.CopilotFeedback{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		RunID:          runID,
		UserID:         userID,
		Rating:         request.Rating,
		Accepted:       request.Accepted,
		FinalMessageID: request.FinalMessageID,
		EditDistance:   request.EditDistance,
		Reason:         strings.TrimSpace(request.Reason),
		Metadata:       request.Metadata,
		Version:        1,
	}
	if feedback.Metadata == nil {
		feedback.Metadata = models.JSONB{}
	}
	if request.FinalText != "" {
		feedback.Metadata["reviewed_final_text"] = request.FinalText
	}
	err = a.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "run_id"},
			{Name: "user_id"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"rating":           feedback.Rating,
			"accepted":         feedback.Accepted,
			"final_message_id": feedback.FinalMessageID,
			"edit_distance":    feedback.EditDistance,
			"reason":           feedback.Reason,
			"metadata":         feedback.Metadata,
			"version":          gorm.Expr("copilot_feedback.version + 1"),
			"updated_at":       time.Now().UTC(),
		}),
	}).Create(&feedback).Error
	if err != nil {
		a.Log.Error("Failed to save Copilot feedback", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save Copilot feedback", nil, "")
	}
	a.logAudit(orgID, userID, "copilot_feedback", feedback.ID, models.AuditActionCreated, nil, map[string]any{
		"run_id":   runID,
		"rating":   feedback.Rating,
		"accepted": feedback.Accepted,
	})
	return r.SendEnvelope(map[string]any{"feedback": feedback})
}

type resolvedCopilotSettings struct {
	chatbot       *models.ChatbotSettings
	retentionDays int
}

func (a *App) resolveCopilotQwenSettings(orgID uuid.UUID, account string) (*resolvedCopilotSettings, error) {
	var dedicated models.CopilotSettings
	err := a.DB.
		Where(
			"organization_id = ? AND is_enabled = ? AND (whats_app_account = ? OR whats_app_account = '')",
			orgID,
			true,
			account,
		).
		Order(gorm.Expr("CASE WHEN whats_app_account = ? THEN 0 ELSE 1 END", account)).
		First(&dedicated).Error
	if err == nil {
		if dedicated.Provider != "" && dedicated.Provider != models.AIProviderQwen {
			return nil, gorm.ErrRecordNotFound
		}
		apiKey := a.decryptStoredSecret(dedicated.APIKeyEncrypted)
		if apiKey == "" {
			return nil, gorm.ErrRecordNotFound
		}
		maxTokens := dedicated.MaxTokens
		if maxTokens < 64 || maxTokens > 4000 {
			maxTokens = 700
		}
		model := strings.TrimSpace(dedicated.Model)
		if model == "" {
			model = "qwen3.7-plus"
		}
		retentionDays := dedicated.RetentionDays
		if retentionDays < 1 || retentionDays > 365 {
			retentionDays = 30
		}
		return &resolvedCopilotSettings{
			chatbot: &models.ChatbotSettings{AI: models.AIConfig{
				Enabled:      true,
				Provider:     models.AIProviderQwen,
				APIKey:       apiKey,
				Model:        model,
				MaxTokens:    maxTokens,
				Temperature:  clampCopilotTemperature(dedicated.Temperature),
				SystemPrompt: dedicated.SystemPrompt,
			}},
			retentionDays: retentionDays,
		}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var chatbot models.ChatbotSettings
	err = a.DB.
		Where("organization_id = ? AND ai_enabled = ? AND ai_provider = ? AND (whats_app_account = ? OR whats_app_account = '')",
			orgID, true, models.AIProviderQwen, account).
		Order(gorm.Expr("CASE WHEN whats_app_account = ? THEN 0 ELSE 1 END", account)).
		First(&chatbot).Error
	if err != nil {
		return nil, err
	}
	chatbot.AI.APIKey = a.decryptStoredSecret(chatbot.AI.APIKey)
	if chatbot.AI.APIKey == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if strings.TrimSpace(chatbot.AI.Model) == "" {
		chatbot.AI.Model = "qwen3.7-plus"
	}
	if chatbot.AI.MaxTokens < 64 || chatbot.AI.MaxTokens > 4000 {
		chatbot.AI.MaxTokens = 700
	}
	chatbot.AI.Temperature = clampCopilotTemperature(chatbot.AI.Temperature)
	chatbot.AI.IncludeHistory = false
	return &resolvedCopilotSettings{chatbot: &chatbot, retentionDays: 30}, nil
}

func buildCopilotContext(contact *models.Contact, messages []models.Message, contexts []models.AIContext) string {
	var builder strings.Builder
	builder.WriteString("GROUNDING RULES\n")
	builder.WriteString("- Use only the supplied organization knowledge and conversation.\n")
	builder.WriteString("- If information is absent, state that it is unknown; do not invent it.\n")
	builder.WriteString("- This is customer-service assistance, not medical diagnosis or prescribing.\n")
	builder.WriteString("- Flag urgent health or safety concerns for immediate human escalation.\n")
	builder.WriteString("- Never claim an appointment, payment, refund, or message was completed.\n")
	builder.WriteString("- A human agent must review every output before acting.\n\n")
	builder.WriteString("CONTACT\nName: ")
	builder.WriteString(truncateRunes(strings.TrimSpace(contact.ProfileName), 255))
	builder.WriteString("\n\nRECENT CONVERSATION\n")
	for i := range messages {
		role := "Customer"
		if messages[i].Direction == models.DirectionOutgoing {
			role = "Business"
		}
		builder.WriteString(role)
		builder.WriteString(": ")
		content := strings.TrimSpace(messages[i].Content)
		if content == "" {
			content = "[" + string(messages[i].MessageType) + " message]"
		}
		builder.WriteString(truncateRunes(content, 2000))
		builder.WriteByte('\n')
	}
	if len(messages) == 0 {
		builder.WriteString("[No recent messages]\n")
	}

	builder.WriteString("\nORGANIZATION KNOWLEDGE\n")
	for i := range contexts {
		builder.WriteString("### ")
		builder.WriteString(truncateRunes(contexts[i].Name, 255))
		builder.WriteByte('\n')
		builder.WriteString(truncateRunes(contexts[i].StaticContent, 4000))
		builder.WriteByte('\n')
	}
	if len(contexts) == 0 {
		builder.WriteString("[No approved knowledge sources]\n")
	}
	return builder.String()
}

func buildCopilotTaskPrompt(task models.CopilotTaskType, request runCopilotRequest) string {
	taskInstruction := map[models.CopilotTaskType]string{
		models.CopilotTaskTypeReply:          "Draft one concise reply for a human agent to review. Return only the proposed reply.",
		models.CopilotTaskTypeSummary:        "Summarize the conversation with customer intent, key facts, unresolved questions, and next step.",
		models.CopilotTaskTypeQualify:        "Qualify this lead. Return JSON with keys summary, intent, urgency, fit, objections, and recommended_next_step.",
		models.CopilotTaskTypeExtractActions: "Extract follow-up actions. Return JSON with an actions array; each action may include title, owner_hint, due_hint, and evidence.",
	}[task]
	var builder strings.Builder
	builder.WriteString(taskInstruction)
	if request.Language != "" {
		builder.WriteString("\nOutput language: ")
		builder.WriteString(request.Language)
	}
	if request.Tone != "" {
		builder.WriteString("\nTone: ")
		builder.WriteString(request.Tone)
	}
	if request.Instruction != "" {
		builder.WriteString("\nAdditional human instruction: ")
		builder.WriteString(request.Instruction)
	}
	return builder.String()
}

func parseCopilotStructuredResult(task models.CopilotTaskType, result string) models.JSONB {
	if task != models.CopilotTaskTypeQualify && task != models.CopilotTaskTypeExtractActions {
		return models.JSONB{}
	}
	candidate := strings.TrimSpace(result)
	if strings.HasPrefix(candidate, "```") {
		lines := strings.Split(candidate, "\n")
		if len(lines) >= 3 {
			candidate = strings.Join(lines[1:len(lines)-1], "\n")
			candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "json"))
		}
	}
	var decoded any
	if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
		return models.JSONB{}
	}
	if object, ok := decoded.(map[string]any); ok {
		return models.JSONB(object)
	}
	return models.JSONB{"value": decoded}
}

func copilotRunToResponse(run *models.CopilotRun) copilotRunResponse {
	result := run.ResultData
	if result == nil {
		result = models.JSONB{}
	}
	return copilotRunResponse{
		ID:               run.ID,
		ContactID:        run.ContactID,
		RequestedByID:    run.RequestedByID,
		TaskType:         run.TaskType,
		Status:           run.Status,
		ResultText:       run.ResultText,
		StructuredResult: result,
		SourceNames:      run.ContextSourceNames,
		SafetyWarnings:   run.Warnings,
		SafetyLabels:     run.SafetyLabels,
		LatencyMS:        run.LatencyMS,
		ErrorCode:        run.ErrorCode,
		CreatedAt:        run.CreatedAt,
		ExpiresAt:        run.ExpiresAt,
	}
}

func parseCopilotTask(raw string) (models.CopilotTaskType, bool) {
	task := models.CopilotTaskType(strings.ToLower(strings.TrimSpace(raw)))
	switch task {
	case models.CopilotTaskTypeReply,
		models.CopilotTaskTypeSummary,
		models.CopilotTaskTypeQualify,
		models.CopilotTaskTypeExtractActions:
		return task, true
	default:
		return "", false
	}
}

func stringPathValue(r *fastglue.Request, key string) string {
	value := r.RequestCtx.UserValue(key)
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

func reverseMessages(messages []models.Message) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "…"
}

func clampCopilotTemperature(value float64) float64 {
	if value <= 0 {
		return 0.3
	}
	if value > 1 {
		return 1
	}
	return value
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}
