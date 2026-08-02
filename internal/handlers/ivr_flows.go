package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// IVRFlowRequest represents the request body for creating/updating an IVR flow
type IVRFlowRequest struct {
	WhatsAppAccount string       `json:"whatsapp_account"`
	Name            string       `json:"name"`
	Description     string       `json:"description"`
	IsActive        bool         `json:"is_active"`
	IsCallStart     bool         `json:"is_call_start"`
	IsOutgoingEnd   bool         `json:"is_outgoing_end"`
	Menu            models.JSONB `json:"menu"`
	WelcomeAudioURL string       `json:"welcome_audio_url"`
}

// ListIVRFlows returns all IVR flows for the organization
func (a *App) ListIVRFlows(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)
	account := string(r.RequestCtx.QueryArgs().Peek("account"))

	query := a.DB.Where("organization_id = ?", orgID).Order("created_at DESC")
	if account != "" {
		query = query.Where("whatsapp_account = ?", account)
	}

	var total int64
	a.DB.Model(&models.IVRFlow{}).Where("organization_id = ?", orgID).Count(&total)

	var flows []models.IVRFlow
	if err := pg.Apply(query).Find(&flows).Error; err != nil {
		a.Log.Error("Failed to fetch IVR flows", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch IVR flows", nil, "")
	}
	for index := range flows {
		flows[index] = redactIVRFlowForResponse(flows[index])
	}

	return r.SendEnvelope(listEnvelope("ivr_flows", flows, total, pg))
}

// GetIVRFlow returns a single IVR flow by ID
func (a *App) GetIVRFlow(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionRead)
	if err != nil {
		return nil
	}

	flowID, err := parsePathUUID(r, "id", "IVR flow")
	if err != nil {
		return nil
	}

	flow, err := findByIDAndOrg[models.IVRFlow](a.DB.Preload("CreatedBy").Preload("UpdatedBy"), r, flowID, orgID, "IVR Flow")
	if err != nil {
		return nil
	}

	return r.SendEnvelope(redactIVRFlowForResponse(*flow))
}

// CreateIVRFlow creates a new IVR flow
func (a *App) CreateIVRFlow(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req IVRFlowRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if req.Name == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Name is required", nil, "")
	}
	if req.WhatsAppAccount == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "WhatsApp account is required", nil, "")
	}

	// Validate all request-controlled flow data before changing the existing
	// call-start/outgoing-end routing selection.
	if req.Menu != nil {
		if err := validateFlowGraph(req.Menu); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid flow graph: "+err.Error(), nil, "")
		}
		protectedMenu, err := calling.ProtectIVRCallbackHeaders(req.Menu, nil, a.ivrEncryptionKey())
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid IVR callback headers: "+err.Error(), nil, "")
		}
		req.Menu = protectedMenu
		if a.TTS == nil {
			if menuHasGreetingText(req.Menu) {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
					"Text-to-speech is not configured on this server. Please upload audio files instead.", nil, "")
			}
		} else {
			if err := a.generateIVRAudio(req.Menu); err != nil {
				a.Log.Error("TTS generation failed", "error", err)
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
					"Text-to-speech generation failed: "+err.Error(), nil, "")
			}
		}
	}

	// If marking this as call start, unset others for the same account
	if req.IsCallStart {
		a.DB.Model(&models.IVRFlow{}).
			Where("organization_id = ? AND whatsapp_account = ? AND is_call_start = ?", orgID, req.WhatsAppAccount, true).
			Update("is_call_start", false)
	}

	// If marking this as outgoing end, unset others for the same account
	if req.IsOutgoingEnd {
		a.DB.Model(&models.IVRFlow{}).
			Where("organization_id = ? AND whatsapp_account = ? AND is_outgoing_end = ?", orgID, req.WhatsAppAccount, true).
			Update("is_outgoing_end", false)
	}

	flow := models.IVRFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		WhatsAppAccount: req.WhatsAppAccount,
		Name:            req.Name,
		Description:     req.Description,
		IsActive:        req.IsActive,
		IsCallStart:     req.IsCallStart,
		IsOutgoingEnd:   req.IsOutgoingEnd,
		Menu:            req.Menu,
		WelcomeAudioURL: req.WelcomeAudioURL,
		CreatedByID:     &userID,
		UpdatedByID:     &userID,
	}

	if err := a.DB.Create(&flow).Error; err != nil {
		a.Log.Error("Failed to create IVR flow", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create IVR flow", nil, "")
	}

	if a.CallManager != nil {
		a.CallManager.InvalidateIVRFlowCache(flow.ID, flow.OrganizationID, flow.WhatsAppAccount)
	}

	auditFlow := redactIVRFlowForResponse(flow)
	a.logAudit(orgID, userID,
		ivrFlowAuditResourceType, flow.ID, models.AuditActionCreated, nil, &auditFlow)

	return r.SendEnvelope(auditFlow)
}

// UpdateIVRFlow updates an existing IVR flow
func (a *App) UpdateIVRFlow(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionWrite)
	if err != nil {
		return nil
	}

	flowID, err := parsePathUUID(r, "id", "IVR flow")
	if err != nil {
		return nil
	}

	flow, err := findByIDAndOrg[models.IVRFlow](a.DB, r, flowID, orgID, "IVR Flow")
	if err != nil {
		return nil
	}

	oldFlow := *flow // value copy for audit

	var req IVRFlowRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Validate all request-controlled flow data before changing the existing
	// call-start/outgoing-end routing selection.
	if req.Menu != nil {
		if err := validateFlowGraph(req.Menu); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid flow graph: "+err.Error(), nil, "")
		}
		protectedMenu, err := calling.ProtectIVRCallbackHeaders(req.Menu, flow.Menu, a.ivrEncryptionKey())
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid IVR callback headers: "+err.Error(), nil, "")
		}
		req.Menu = protectedMenu
		if a.TTS == nil {
			if menuHasGreetingText(req.Menu) {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
					"Text-to-speech is not configured on this server. Please upload audio files instead.", nil, "")
			}
		} else {
			if err := a.generateIVRAudio(req.Menu); err != nil {
				a.Log.Error("TTS generation failed", "error", err)
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
					"Text-to-speech generation failed: "+err.Error(), nil, "")
			}
		}
	}

	// If marking this as call start, unset others for the same account
	if req.IsCallStart && !flow.IsCallStart {
		a.DB.Model(&models.IVRFlow{}).
			Where("organization_id = ? AND whatsapp_account = ? AND is_call_start = ? AND id != ?",
				orgID, flow.WhatsAppAccount, true, flowID).
			Update("is_call_start", false)
	}

	// If marking this as outgoing end, unset others for the same account
	if req.IsOutgoingEnd && !flow.IsOutgoingEnd {
		a.DB.Model(&models.IVRFlow{}).
			Where("organization_id = ? AND whatsapp_account = ? AND is_outgoing_end = ? AND id != ?",
				orgID, flow.WhatsAppAccount, true, flowID).
			Update("is_outgoing_end", false)
	}

	// Only update fields that were actually provided (non-zero) to support
	// partial updates like toggling is_active without wiping the menu.
	updates := map[string]any{
		"is_active":       req.IsActive,
		"is_call_start":   req.IsCallStart,
		"is_outgoing_end": req.IsOutgoingEnd,
		"updated_by_id":   userID,
	}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.Description != "" || req.Name != "" {
		// Include description when saving from the editor (name is always sent)
		updates["description"] = req.Description
	}
	if req.Menu != nil {
		updates["menu"] = req.Menu
	}
	if req.WelcomeAudioURL != "" {
		updates["welcome_audio_url"] = req.WelcomeAudioURL
	}
	if req.WhatsAppAccount != "" {
		updates["whatsapp_account"] = req.WhatsAppAccount
	}

	if err := a.DB.Model(flow).Updates(updates).Error; err != nil {
		a.Log.Error("Failed to update IVR flow", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update IVR flow", nil, "")
	}

	// Reload for response
	a.DB.Preload("CreatedBy").Preload("UpdatedBy").First(flow, flowID)

	if a.CallManager != nil {
		if oldFlow.WhatsAppAccount != flow.WhatsAppAccount {
			a.CallManager.InvalidateIVRFlowCache(flow.ID, flow.OrganizationID, oldFlow.WhatsAppAccount)
		}
		a.CallManager.InvalidateIVRFlowCache(flow.ID, flow.OrganizationID, flow.WhatsAppAccount)
	}

	// Compare IVR menu nodes for audit
	var extraChanges []map[string]any
	auditOldFlow := redactIVRFlowForResponse(oldFlow)
	auditFlow := redactIVRFlowForResponse(*flow)
	if req.Menu != nil {
		extraChanges = diffIVRMenuNodes(a.DB, auditOldFlow.Menu, auditFlow.Menu)
	}

	a.logAudit(orgID, userID,
		ivrFlowAuditResourceType, flow.ID, models.AuditActionUpdated, &auditOldFlow, &auditFlow, extraChanges...)

	return r.SendEnvelope(auditFlow)
}

// diffIVRMenuNodes compares old and new IVR menu JSONB to find node-level changes
func diffIVRMenuNodes(db *gorm.DB, oldMenu, newMenu models.JSONB) []map[string]any {
	var changes []map[string]any

	type ivrNode struct {
		ID     string         `json:"id"`
		Type   string         `json:"type"`
		Label  string         `json:"label"`
		Config map[string]any `json:"config"`
	}

	extractNodes := func(menu models.JSONB) map[string]ivrNode {
		result := make(map[string]ivrNode)
		nodesRaw, ok := menu["nodes"]
		if !ok {
			return result
		}
		nodesSlice, ok := nodesRaw.([]any)
		if !ok {
			return result
		}
		for _, raw := range nodesSlice {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			b, err := json.Marshal(m)
			if err != nil {
				continue
			}
			var n ivrNode
			_ = json.Unmarshal(b, &n)
			if n.ID != "" {
				result[n.ID] = n
			}
		}
		return result
	}

	oldNodes := extractNodes(oldMenu)
	newNodes := extractNodes(newMenu)

	// Detect added nodes
	for id, n := range newNodes {
		if _, exists := oldNodes[id]; !exists {
			changes = append(changes, map[string]any{
				"field": "node_added", "old_value": nil, "new_value": n.Label + " (" + n.Type + ")",
			})
		}
	}

	// Detect removed nodes
	for id, n := range oldNodes {
		if _, exists := newNodes[id]; !exists {
			changes = append(changes, map[string]any{
				"field": "node_removed", "old_value": n.Label + " (" + n.Type + ")", "new_value": nil,
			})
		}
	}

	// Detect modified nodes
	for id, newN := range newNodes {
		oldN, exists := oldNodes[id]
		if !exists {
			continue
		}
		if oldN.Label != newN.Label {
			changes = append(changes, map[string]any{
				"field": newN.Label + " → label", "old_value": oldN.Label, "new_value": newN.Label,
			})
		}
		// Compare config fields — drill into nested maps for readable diffs
		label := newN.Label
		if label == "" {
			label = id
		}
		for key, newVal := range newN.Config {
			oldVal := oldN.Config[key]
			oldJSON, _ := json.Marshal(oldVal)
			newJSON, _ := json.Marshal(newVal)
			if string(oldJSON) == string(newJSON) {
				continue
			}
			// Try to diff nested maps (e.g. options: {"1": {"label": "Sales"}})
			oldMap, oldIsMap := oldVal.(map[string]any)
			newMap, newIsMap := newVal.(map[string]any)
			if oldIsMap && newIsMap {
				for subKey, subNew := range newMap {
					subOld := oldMap[subKey]
					sOldJSON, _ := json.Marshal(subOld)
					sNewJSON, _ := json.Marshal(subNew)
					if string(sOldJSON) != string(sNewJSON) {
						// Extract readable value from nested object
						oldLabel := extractLabel(subOld)
						newLabel := extractLabel(subNew)
						changes = append(changes, map[string]any{
							"field": label + " → " + key + "[" + subKey + "]", "old_value": oldLabel, "new_value": newLabel,
						})
					}
				}
				// Check for removed keys
				for subKey, subOld := range oldMap {
					if _, exists := newMap[subKey]; !exists {
						changes = append(changes, map[string]any{
							"field": label + " → " + key + "[" + subKey + "]", "old_value": extractLabel(subOld), "new_value": nil,
						})
					}
				}
			} else {
				displayOld := oldVal
				displayNew := newVal
				// Resolve team_id UUIDs to team names
				if key == "team_id" {
					displayOld = resolveTeamName(db, fmt.Sprintf("%v", oldVal))
					displayNew = resolveTeamName(db, fmt.Sprintf("%v", newVal))
				}
				changes = append(changes, map[string]any{
					"field": label + " → " + key, "old_value": displayOld, "new_value": displayNew,
				})
			}
		}
	}

	return changes
}

func resolveTeamName(db *gorm.DB, teamID string) string {
	if teamID == "" || teamID == "<nil>" {
		return "—"
	}
	var name string
	db.Model(&models.Team{}).Where("id = ?", teamID).Pluck("name", &name)
	if name == "" {
		return teamID
	}
	return name
}

// extractLabel returns a readable string from a value — if it's a map with a "label" key, return that
func extractLabel(val any) any {
	if m, ok := val.(map[string]any); ok {
		if label, exists := m["label"]; exists {
			return label
		}
	}
	return val
}

// DeleteIVRFlow soft-deletes an IVR flow
func (a *App) DeleteIVRFlow(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionDelete)
	if err != nil {
		return nil
	}

	flowID, err := parsePathUUID(r, "id", "IVR flow")
	if err != nil {
		return nil
	}

	flow, err := findByIDAndOrg[models.IVRFlow](a.DB, r, flowID, orgID, "IVR Flow")
	if err != nil {
		return nil
	}

	if err := a.DB.Delete(flow).Error; err != nil {
		a.Log.Error("Failed to delete IVR flow", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete IVR flow", nil, "")
	}

	if a.CallManager != nil {
		a.CallManager.InvalidateIVRFlowCache(flow.ID, flow.OrganizationID, flow.WhatsAppAccount)
	}

	auditFlow := redactIVRFlowForResponse(*flow)
	a.logAudit(orgID, userID,
		ivrFlowAuditResourceType, flow.ID, models.AuditActionDeleted, &auditFlow, nil)

	return r.SendEnvelope(map[string]string{"message": "IVR flow deleted"})
}

// getAudioDir returns the configured audio directory path.
func (a *App) getAudioDir() string {
	dir := a.Config.Calling.AudioDir
	if dir == "" {
		dir = "./audio"
	}
	return dir
}

// UploadIVRAudio handles multipart audio file uploads for IVR greetings.
func (a *App) UploadIVRAudio(r *fastglue.Request) error {
	_, _, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionWrite)
	if err != nil {
		return nil
	}

	// Parse multipart form
	contentType := string(r.RequestCtx.Request.Header.ContentType())
	a.Log.Debug("IVR audio upload", "content_type", contentType, "body_size", len(r.RequestCtx.Request.Body()))

	form, err := r.RequestCtx.MultipartForm()
	if err != nil {
		a.Log.Error("Multipart parse failed", "error", err, "content_type", contentType)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid multipart form: "+err.Error(), nil, "")
	}

	files := form.File["file"]
	if len(files) == 0 {
		a.Log.Error("No file in multipart form", "form_keys", fmt.Sprintf("%v", form.Value))
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No file provided", nil, "")
	}

	fileHeader := files[0]
	file, err := fileHeader.Open()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Failed to open file", nil, "")
	}
	defer func() { _ = file.Close() }()

	// Read file content (limit to 5MB for IVR prompts)
	const maxAudioSize = 5 << 20 // 5MB
	data, err := io.ReadAll(io.LimitReader(file, maxAudioSize+1))
	if err != nil {
		a.Log.Error("Failed to read IVR audio file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}
	if len(data) > maxAudioSize {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "File too large. Maximum size is 5MB", nil, "")
	}

	// Validate MIME type
	mimeType := fileHeader.Header.Get("Content-Type")
	allowedAudio := map[string]bool{
		"audio/ogg":                true,
		"audio/opus":               true,
		"audio/mpeg":               true,
		"audio/mp3":                true,
		"audio/aac":                true,
		"audio/mp4":                true,
		"audio/wav":                true,
		"audio/x-wav":              true,
		"audio/wave":               true,
		"audio/webm":               true,
		"audio/flac":               true,
		"audio/x-flac":             true,
		"audio/x-m4a":              true,
		"audio/m4a":                true,
		"application/ogg":          true,
		"application/octet-stream": true, // fallback for unknown audio
		"video/ogg":                true, // some browsers report .ogg as video/ogg
	}
	if !allowedAudio[mimeType] {
		a.Log.Error("Unsupported audio MIME type", "mime_type", mimeType, "filename", fileHeader.Filename)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Unsupported audio type: "+mimeType, nil, "")
	}

	// Ensure audio directory exists
	audioDir := a.getAudioDir()
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		a.Log.Error("Failed to create audio directory", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create audio directory", nil, "")
	}

	// Save uploaded file to a temp location for transcoding
	tmpInput, err := os.CreateTemp("", "ivr-audio-input-*")
	if err != nil {
		a.Log.Error("Failed to create IVR temp file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create temp file", nil, "")
	}
	defer func() { _ = os.Remove(tmpInput.Name()) }()

	if _, err := tmpInput.Write(data); err != nil {
		_ = tmpInput.Close()
		a.Log.Error("Failed to write IVR temp file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to write temp file", nil, "")
	}
	_ = tmpInput.Close()

	// Transcode to OGG/Opus 48kHz mono for WebRTC compatibility
	filename := uuid.New().String() + ".ogg"
	filePath := filepath.Join(audioDir, filename)

	if err := transcodeToOpus(tmpInput.Name(), filePath); err != nil {
		a.Log.Error("IVR audio transcoding failed", "error", err, "original_mime", mimeType)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to transcode audio to Opus format", nil, "")
	}

	a.Log.Info("IVR audio uploaded", "filename", filename, "original_mime", mimeType, "size", len(data))

	return r.SendEnvelope(map[string]any{
		"filename":  filename,
		"mime_type": mimeType,
		"size":      len(data),
	})
}

// ServeIVRAudio serves audio files from the IVR audio directory.
func (a *App) ServeIVRAudio(r *fastglue.Request) error {
	_, _, err := a.requireAuth(r, models.ResourceIVRFlows, models.ActionRead)
	if err != nil {
		return nil
	}

	filename := r.RequestCtx.UserValue("filename").(string)
	filename = sanitizeFilename(filename)

	// Security: prevent directory traversal and symlink attacks
	audioDir := a.getAudioDir()
	baseDir, err := filepath.Abs(audioDir)
	if err != nil {
		a.Log.Error("Failed to resolve audio directory", "error", err, "audio_dir", audioDir)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Storage configuration error", nil, "")
	}
	fullPath, err := filepath.Abs(filepath.Join(baseDir, filename))
	if err != nil || !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid file path", nil, "")
	}

	// Reject symlinks
	info, err := os.Lstat(fullPath)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "File not found", nil, "")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid file path", nil, "")
	}

	// Read file
	data, err := os.ReadFile(fullPath)
	if err != nil {
		a.Log.Error("Failed to read audio file", "path", fullPath, "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}

	// Determine content type from extension
	ext := strings.ToLower(filepath.Ext(filename))
	contentType := getMimeTypeFromExtension(ext)

	r.RequestCtx.Response.Header.Set("Content-Type", contentType)
	r.RequestCtx.Response.Header.Set("Cache-Control", "private, max-age=3600")
	r.RequestCtx.SetBody(data)

	return nil
}

// UploadOrgAudio handles multipart audio file uploads for org-level hold music and ringback tones.
// The "type" query parameter must be "hold_music" or "ringback".
func (a *App) UploadOrgAudio(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSettingsCalling, models.ActionWrite)
	if err != nil {
		return nil
	}

	audioType := string(r.RequestCtx.QueryArgs().Peek("type"))
	if audioType != "hold_music" && audioType != "ringback" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Query parameter 'type' must be 'hold_music' or 'ringback'", nil, "")
	}

	// Parse multipart form
	form, err := r.RequestCtx.MultipartForm()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid multipart form: "+err.Error(), nil, "")
	}

	files := form.File["file"]
	if len(files) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No file provided", nil, "")
	}

	fileHeader := files[0]
	file, err := fileHeader.Open()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Failed to open file", nil, "")
	}
	defer func() { _ = file.Close() }()

	// Read file content (limit to 5MB)
	const maxAudioSize = 5 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxAudioSize+1))
	if err != nil {
		a.Log.Error("Failed to read org audio file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}
	if len(data) > maxAudioSize {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "File too large. Maximum size is 5MB", nil, "")
	}

	// Validate MIME type
	mimeType := fileHeader.Header.Get("Content-Type")
	allowedAudio := map[string]bool{
		"audio/ogg": true, "audio/opus": true,
		"audio/mpeg": true, "audio/mp3": true,
		"audio/wav": true, "audio/x-wav": true, "audio/wave": true,
		"application/ogg": true, "application/octet-stream": true,
		"video/ogg": true,
	}
	if !allowedAudio[mimeType] {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Unsupported audio type: "+mimeType, nil, "")
	}

	// Ensure audio directory exists
	audioDir := a.getAudioDir()
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		a.Log.Error("Failed to create org audio directory", "error", err, "audio_dir", audioDir)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create audio directory", nil, "")
	}

	// Save uploaded file to a temp location for transcoding
	tmpInput, err := os.CreateTemp("", "org-audio-input-*")
	if err != nil {
		a.Log.Error("Failed to create org temp file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create temp file", nil, "")
	}
	defer func() { _ = os.Remove(tmpInput.Name()) }()

	if _, err := tmpInput.Write(data); err != nil {
		_ = tmpInput.Close()
		a.Log.Error("Failed to write org temp file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to write temp file", nil, "")
	}
	_ = tmpInput.Close()

	// Transcode to OGG/Opus 48kHz mono using ffmpeg
	filename := fmt.Sprintf("org_%s_%s.ogg", orgID.String(), audioType)
	filePath := filepath.Join(audioDir, filename)

	if err := transcodeToOpus(tmpInput.Name(), filePath); err != nil {
		a.Log.Error("Audio transcoding failed", "error", err, "org_id", orgID, "type", audioType)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to transcode audio to Opus format", nil, "")
	}

	settingsKey := audioType + "_file"
	// Serialize this JSONB update with Integration Center and General/Calling
	// settings writes so the audio filename cannot restore a stale credential
	// snapshot after waiting on another transaction.
	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		var org models.Organization
		if queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", orgID).
			First(&org).Error; queryErr != nil {
			return queryErr
		}
		if org.Settings == nil {
			org.Settings = models.JSONB{}
		}
		org.Settings[settingsKey] = filename
		return tx.Model(&models.Organization{}).
			Where("id = ?", orgID).
			Update("settings", org.Settings).Error
	}); err != nil {
		a.Log.Error("Failed to update organization audio settings", "error", err, "org_id", orgID, "audio_type", audioType)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update organization settings", nil, "")
	}

	a.Log.Info("Org audio uploaded", "org_id", orgID, "type", audioType, "filename", filename, "size", len(data))

	return r.SendEnvelope(map[string]any{
		"filename":  filename,
		"type":      audioType,
		"mime_type": mimeType,
		"size":      len(data),
	})
}

// transcodeToOpus converts any audio file to OGG/Opus 48kHz mono using ffmpeg.
// This ensures the file is compatible with the WebRTC AudioPlayer.
func transcodeToOpus(inputPath, outputPath string) error {
	cmd := exec.Command("ffmpeg",
		"-y",            // overwrite output
		"-i", inputPath, // input file
		"-ac", "1", // mono
		"-ar", "48000", // 48kHz (Opus standard)
		"-c:a", "libopus",
		"-b:a", "48k", // bitrate
		"-application", "audio",
		"-frame_duration", "20", // 20ms frames (matches RTP packetization)
		"-vn", // strip video/cover art
		outputPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// generateIVRAudio iterates the flat v2 nodes array and generates TTS audio
// for any node with a non-empty "greeting_text" in its config. The generated
// audio filename is set as the node's "audio_file" config field.
func (a *App) generateIVRAudio(menu models.JSONB) error {
	nodesRaw, ok := menu["nodes"]
	if !ok {
		return nil
	}

	// toSlice may produce a copy (via re-marshal), so we always write
	// the potentially-modified slice back into menu["nodes"].
	nodesSlice, ok := toSlice(nodesRaw)
	if !ok {
		return nil
	}

	for i, nodeRaw := range nodesSlice {
		nodeMap, ok := nodeRaw.(map[string]any)
		if !ok {
			continue
		}
		configRaw, ok := nodeMap["config"]
		if !ok {
			continue
		}
		config, ok := configRaw.(map[string]any)
		if !ok {
			continue
		}
		greetingText, _ := config["greeting_text"].(string)
		if greetingText == "" {
			continue
		}
		filename, err := a.TTS.Generate(greetingText)
		if err != nil {
			return err
		}
		config["audio_file"] = filename
		nodeMap["config"] = config
		nodesSlice[i] = nodeMap
	}

	// Write the modified nodes back so changes reach the DB.
	menu["nodes"] = nodesSlice
	return nil
}

// menuHasGreetingText checks if any node in the v2 flow graph uses greeting_text.
func menuHasGreetingText(menu models.JSONB) bool {
	nodesRaw, ok := menu["nodes"]
	if !ok {
		return false
	}
	nodesSlice, ok := toSlice(nodesRaw)
	if !ok {
		return false
	}

	for _, nodeRaw := range nodesSlice {
		nodeMap, ok := nodeRaw.(map[string]any)
		if !ok {
			continue
		}
		configRaw, ok := nodeMap["config"]
		if !ok {
			continue
		}
		config, ok := configRaw.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := config["greeting_text"].(string); text != "" {
			return true
		}
	}
	return false
}

// toSlice converts an any to []any, handling JSON re-marshal if needed.
func toSlice(v any) ([]any, bool) {
	if s, ok := v.([]any); ok {
		return s, true
	}
	// Handle case where JSONB was deserialized differently
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var s []any
	if json.Unmarshal(b, &s) == nil {
		return s, true
	}
	return nil, false
}

// validateFlowGraph validates a v2 IVR flow graph for structural correctness.
func validateFlowGraph(menu models.JSONB) error {
	versionRaw := menu["version"]
	var version int
	switch v := versionRaw.(type) {
	case float64:
		version = int(v)
	case int:
		version = v
	}
	if version != 2 {
		return fmt.Errorf("unsupported flow version: %v (expected 2)", versionRaw)
	}

	nodesRaw, ok := menu["nodes"]
	if !ok {
		return fmt.Errorf("missing nodes array")
	}
	nodesSlice, ok := toSlice(nodesRaw)
	if !ok {
		return fmt.Errorf("nodes must be an array")
	}

	// Empty flow (no nodes yet) is valid
	if len(nodesSlice) == 0 {
		return nil
	}

	entryNode, _ := menu["entry_node"].(string)
	if entryNode == "" {
		return fmt.Errorf("missing entry_node (required when nodes exist)")
	}

	// Build node ID set and terminal node set
	nodeIDs := make(map[string]bool, len(nodesSlice))
	terminalNodes := make(map[string]bool)
	terminalTypes := map[string]bool{"goto_flow": true, "hangup": true}

	for _, nodeRaw := range nodesSlice {
		nodeMap, ok := nodeRaw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := nodeMap["id"].(string)
		if id == "" {
			return fmt.Errorf("node missing id")
		}
		if nodeIDs[id] {
			return fmt.Errorf("duplicate node id: %s", id)
		}
		nodeIDs[id] = true

		nodeType, _ := nodeMap["type"].(string)
		if err := validateIVRCallbackNode(id, nodeType, nodeMap["config"]); err != nil {
			return err
		}
		if terminalTypes[nodeType] {
			terminalNodes[id] = true
		}
	}

	if !nodeIDs[entryNode] {
		return fmt.Errorf("entry_node %q does not reference a valid node", entryNode)
	}

	// Validate edges
	edgesRaw := menu["edges"]
	if edgesRaw != nil {
		edgesSlice, ok := toSlice(edgesRaw)
		if !ok {
			return fmt.Errorf("edges must be an array")
		}
		for _, edgeRaw := range edgesSlice {
			edgeMap, ok := edgeRaw.(map[string]any)
			if !ok {
				continue
			}
			from, _ := edgeMap["from"].(string)
			to, _ := edgeMap["to"].(string)
			if !nodeIDs[from] {
				return fmt.Errorf("edge from %q references non-existent node", from)
			}
			if !nodeIDs[to] {
				return fmt.Errorf("edge to %q references non-existent node", to)
			}
			if terminalNodes[from] {
				return fmt.Errorf("terminal node %q must not have outgoing edges", from)
			}
		}
	}

	return nil
}

func validateIVRCallbackNode(nodeID, nodeType string, rawConfig any) error {
	config, configOK := ivrConfigMap(rawConfig)
	switch nodeType {
	case string(calling.IVRNodeHTTPCallback):
		if !configOK {
			return fmt.Errorf("node %q HTTP callback config must be an object", nodeID)
		}
		if err := validateIVRCallbackTimeout(nodeID, config); err != nil {
			return err
		}
		method, _ := config["method"].(string)
		if strings.TrimSpace(method) == "" {
			method = "GET"
		}
		if err := calling.ValidateHTTPCallbackMethod(method); err != nil {
			return fmt.Errorf("node %q: %w", nodeID, err)
		}
		callbackURL, ok := config["url"].(string)
		if !ok || strings.TrimSpace(callbackURL) == "" {
			return fmt.Errorf("node %q HTTP callback URL is required", nodeID)
		}
		if err := calling.ValidateHTTPCallbackTemplateURL(callbackURL); err != nil {
			return fmt.Errorf("node %q: %w", nodeID, err)
		}
	case string(calling.IVRNodeTransfer):
		if !configOK {
			return nil
		}
		for _, event := range []string{"on_waiting", "on_connect"} {
			rawCallback, exists := config[event]
			if !exists || rawCallback == nil {
				continue
			}
			callback, ok := ivrConfigMap(rawCallback)
			if !ok {
				return fmt.Errorf("node %q transfer callback %s must be an object", nodeID, event)
			}
			callbackURL, _ := callback["url"].(string)
			if strings.TrimSpace(callbackURL) == "" {
				continue
			}
			method, _ := callback["method"].(string)
			if strings.TrimSpace(method) == "" {
				method = "POST"
			}
			if err := calling.ValidateHTTPCallbackMethod(method); err != nil {
				return fmt.Errorf("node %q transfer callback %s: %w", nodeID, event, err)
			}
			if err := calling.ValidateHTTPCallbackTemplateURL(callbackURL); err != nil {
				return fmt.Errorf("node %q transfer callback %s: %w", nodeID, event, err)
			}
		}
	}
	return nil
}

func validateIVRCallbackTimeout(nodeID string, config map[string]any) error {
	rawTimeout, configured := config["timeout_seconds"]
	if !configured {
		return nil
	}

	var timeout int64
	switch value := rawTimeout.(type) {
	case int:
		timeout = int64(value)
	case int64:
		timeout = value
	case float64:
		if value != float64(int64(value)) {
			return fmt.Errorf("node %q HTTP callback timeout_seconds must be a whole number from 1 to 30", nodeID)
		}
		timeout = int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return fmt.Errorf("node %q HTTP callback timeout_seconds must be a whole number from 1 to 30", nodeID)
		}
		timeout = parsed
	default:
		return fmt.Errorf("node %q HTTP callback timeout_seconds must be a whole number from 1 to 30", nodeID)
	}
	if timeout < 1 || timeout > 30 {
		return fmt.Errorf("node %q HTTP callback timeout_seconds must be between 1 and 30", nodeID)
	}
	return nil
}

func ivrConfigMap(value any) (map[string]any, bool) {
	switch config := value.(type) {
	case map[string]any:
		return config, true
	case models.JSONB:
		return map[string]any(config), true
	default:
		return nil, false
	}
}
