package handlers

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	chatExportBatchSize         = 200
	chatExportPageSize          = 40
	chatExportMaxMessages       = 100000
	chatExportMaxBytes    int64 = 64 << 20
	chatExportMaxSelected       = 2000
	chatExportMaxDuration       = 2 * time.Minute
)

var (
	errChatExportDenied       = errors.New("chat export access denied")
	errChatExportUnavailable  = errors.New("chat export permission is not installed")
	errChatExportTooLarge     = errors.New("export exceeds the safe size limit; choose a smaller date range or account")
	errChatExportScopeChanged = errors.New("conversation access or selection changed; preview the export again")
	errChatExportCancelled    = errors.New("export cancelled")
)

// The vocabulary addition is a separately gated seed/data change. Do not add
// runtime seeding or a contacts:export/admin-only fallback here.
const chatExportPermission = "chat:export"

type chatExportRequest struct {
	Scope     string     `json:"scope"`
	Selected  []string   `json:"selected,omitempty"`
	Search    string     `json:"search,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	Assigned  string     `json:"assigned,omitempty"`
	Unread    bool       `json:"unread,omitempty"`
	Account   string     `json:"account,omitempty"`
	From      *time.Time `json:"from,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	After     string     `json:"after,omitempty"`
	RequestID uuid.UUID  `json:"request_id,omitempty"`
}

func (r *chatExportRequest) validate() error {
	if r.Scope != "selected" && r.Scope != "filtered" && r.Scope != "all" {
		return errors.New("choose selected, filtered or all conversations")
	}
	if r.Assigned != "" && r.Assigned != "me" && r.Assigned != "unassigned" {
		return errors.New("invalid assignment filter")
	}
	if len(r.Search) > 1000 || len(r.Tags) > 50 || len(r.Selected) > chatExportMaxSelected {
		return errors.New("selection or filters are too large")
	}
	for _, tag := range r.Tags {
		if len(tag) > 100 {
			return errors.New("tag is too long")
		}
	}
	if r.From != nil && r.Until != nil && !r.From.Before(*r.Until) {
		return errors.New("end date must be after start date")
	}
	if r.Account != "" && !chatExportValidAccount(r.Account) {
		return errors.New("invalid account selection")
	}
	if r.After != "" && !chatExportValidGroup(r.After) {
		return errors.New("invalid conversation cursor")
	}
	seen := make(map[string]bool)
	for _, key := range r.Selected {
		if !chatExportValidGroup(key) || seen[key] {
			return errors.New("invalid or duplicate selected conversation")
		}
		seen[key] = true
	}
	if r.Scope == "selected" && len(r.Selected) == 0 {
		return errors.New("select at least one conversation")
	}
	if r.Scope != "selected" {
		r.Selected = nil
	}
	if r.Scope != "filtered" {
		r.Search = ""
		r.Tags = nil
		r.Assigned = ""
		r.Unread = false
	}
	sort.Strings(r.Selected)
	return nil
}

func chatExportValidAccount(key string) bool {
	if strings.HasPrefix(key, "channel:") {
		id, err := uuid.Parse(strings.TrimPrefix(key, "channel:"))
		return err == nil && id != uuid.Nil
	}
	if strings.HasPrefix(key, "whatsapp:") {
		b, err := hex.DecodeString(strings.TrimPrefix(key, "whatsapp:"))
		return err == nil && len(b) > 0 && len(b) <= 100
	}
	return false
}

func chatExportValidGroup(key string) bool {
	if strings.HasPrefix(key, "inbox:") {
		id, err := uuid.Parse(strings.TrimPrefix(key, "inbox:"))
		return err == nil && id != uuid.Nil
	}
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "whatsapp" {
		return false
	}
	id, err := uuid.Parse(parts[1])
	if err != nil || id == uuid.Nil {
		return false
	}
	return chatExportValidAccount("whatsapp:" + parts[2])
}

type chatExportAccess struct {
	AllContacts             bool
	NativeInbox             bool
	CurrentConversationOnly bool
	MaskPhones              bool
}

func (a *App) chatExportAccess(orgID, userID uuid.UUID) (chatExportAccess, error) {
	var result chatExportAccess
	var installed int64
	if err := a.DB.Model(&models.Permission{}).Where("resource = ? AND action = ?", models.ResourceChat, models.ActionExport).Count(&installed).Error; err != nil {
		return result, err
	}
	if installed != 1 {
		return result, errChatExportUnavailable
	}
	// Export admission must observe actual grants, not a six-hour cache. The
	// request-specific scoped App has Redis=nil; no permission cache is written.
	permissions, err := a.getUserPermissionsCached(userID, orgID)
	if err != nil {
		return result, errChatExportDenied
	}
	has := func(key string) bool {
		if permissions.IsSuperAdmin {
			return true
		}
		for _, value := range permissions.Permissions {
			if value == key {
				return true
			}
		}
		return false
	}
	if !has(chatExportPermission) || !has("chat:read") {
		return result, errChatExportDenied
	}
	result.AllContacts = has("contacts:read")
	result.NativeInbox = has("conversations:read")
	if result.NativeInbox {
		allowed, entitlementErr := a.HasProductEntitlement(userID, orgID, "omnichannel.enabled")
		if entitlementErr != nil {
			return result, entitlementErr
		}
		result.NativeInbox = allowed
	}
	if !result.AllContacts {
		var settings models.ChatbotSettings
		err = a.DB.Select("id", "agent_current_conversation_only").Where("organization_id = ? AND whats_app_account = ''", orgID).First(&settings).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return result, err
		}
		result.CurrentConversationOnly = settings.AgentAssignment.CurrentConversationOnly
	}
	var organization models.Organization
	if err := a.DB.Select("id", "settings").Where("id = ?", orgID).First(&organization).Error; err != nil {
		return result, err
	}
	result.MaskPhones, _ = organization.Settings["mask_phone_numbers"].(bool)
	return result, nil
}

// A bridge links the original messages row in place. Reading messages once,
// with one-to-one joins, avoids exporting its legacy and inbox views twice.
// A provider-neutral link must agree on org, contact and account before use.
const chatExportLegacySQL = `(m.whats_app_account <> '' AND (m.inbox_conversation_id IS NULL OR ca.provider = 'meta_legacy'))`
const chatExportGroupSQL = `CASE WHEN ` + chatExportLegacySQL + ` THEN 'whatsapp:' || m.contact_id::text || ':' || encode(convert_to(m.whats_app_account, 'UTF8'), 'hex') ELSE 'inbox:' || ic.id::text END`
const chatExportAccountSQL = `CASE WHEN ` + chatExportLegacySQL + ` THEN 'whatsapp:' || encode(convert_to(m.whats_app_account, 'UTF8'), 'hex') ELSE 'channel:' || ca.id::text END`
const chatExportAssignedSQL = `(c.assigned_user_id = ? OR EXISTS (SELECT 1 FROM agent_transfers atx WHERE atx.organization_id = c.organization_id AND atx.contact_id = c.id AND atx.deleted_at IS NULL AND atx.status = 'active' AND atx.agent_id = ?))`
const chatExportCutoffSQL = `GREATEST((SELECT MAX(cs.started_at) FROM chatbot_sessions cs WHERE cs.organization_id = c.organization_id AND cs.contact_id = c.id AND cs.deleted_at IS NULL), (SELECT MAX(atx.transferred_at) FROM agent_transfers atx WHERE atx.organization_id = c.organization_id AND atx.contact_id = c.id AND atx.deleted_at IS NULL AND atx.status = 'active' AND atx.agent_id = ?))`
const chatExportBridgeSQL = `(m.inbox_conversation_id IS NULL OR ca.provider <> 'meta_legacy' OR (ic.channel = 'whatsapp' AND EXISTS (SELECT 1 FROM whatsapp_accounts wa WHERE wa.organization_id = m.organization_id AND wa.id::text = ca.metadata->>'legacy_account_id' AND ca.external_account_id = 'legacy-account:' || wa.id::text AND wa.name = m.whats_app_account AND ca.metadata->>'legacy_account_name' = wa.name AND wa.deleted_at IS NULL)))`

func (a *App) chatExportQuery(orgID, userID uuid.UUID, req chatExportRequest, access chatExportAccess) *gorm.DB {
	q := a.DB.Table("messages AS m").
		Joins("JOIN contacts c ON c.id = m.contact_id AND c.organization_id = m.organization_id AND c.deleted_at IS NULL").
		Joins("LEFT JOIN inbox_conversations ic ON ic.id = m.inbox_conversation_id AND ic.organization_id = m.organization_id AND ic.contact_id = m.contact_id AND ic.deleted_at IS NULL").
		Joins("LEFT JOIN channel_accounts ca ON ca.id = ic.channel_account_id AND ca.organization_id = m.organization_id AND ca.channel = ic.channel AND ca.deleted_at IS NULL").
		Where("m.organization_id = ? AND m.deleted_at IS NULL", orgID).
		Where("(m.inbox_conversation_id IS NULL OR (ic.id IS NOT NULL AND ca.id IS NOT NULL))").Where(chatExportBridgeSQL)
	if access.NativeInbox {
		q = q.Where("(" + chatExportLegacySQL + " OR (ic.id IS NOT NULL AND ca.id IS NOT NULL))")
	} else {
		q = q.Where(chatExportLegacySQL)
	}
	if !access.AllContacts {
		q = q.Where(chatExportAssignedSQL, userID, userID)
	}
	if access.CurrentConversationOnly {
		q = q.Where("m.created_at >= "+chatExportCutoffSQL, userID)
	}
	if req.Scope == "selected" {
		q = q.Where(chatExportGroupSQL+" IN ?", req.Selected)
	}
	if req.Account != "" {
		q = q.Where(chatExportAccountSQL+" = ?", req.Account)
	}
	if req.From != nil {
		q = q.Where("m.created_at >= ?", req.From.UTC())
	}
	if req.Until != nil {
		q = q.Where("m.created_at < ?", req.Until.UTC())
	}
	if req.Scope == "filtered" {
		switch req.Assigned {
		case "me":
			q = q.Where(chatExportAssignedSQL, userID, userID)
		case "unassigned":
			q = q.Where("c.assigned_user_id IS NULL AND NOT EXISTS (SELECT 1 FROM agent_transfers atx WHERE atx.organization_id = c.organization_id AND atx.contact_id = c.id AND atx.deleted_at IS NULL AND atx.status = 'active' AND atx.agent_id IS NOT NULL)")
		}
		if req.Unread {
			q = q.Where(`(c.metadata @> '{"manual_unread":true}'::jsonb OR EXISTS (SELECT 1 FROM messages um WHERE um.organization_id = c.organization_id AND um.contact_id = c.id AND um.deleted_at IS NULL AND um.direction = 'incoming' AND um.status <> 'read'))`)
		}
		if req.Search != "" {
			pattern := "%" + req.Search + "%"
			q = q.Where("(c.phone_number LIKE ? OR c.profile_name ILIKE ?)", pattern, pattern)
		}
		if len(req.Tags) > 0 {
			conditions := []string{}
			args := []any{}
			for _, tag := range req.Tags {
				b, _ := json.Marshal([]string{tag})
				conditions = append(conditions, "c.tags @> ?::jsonb")
				args = append(args, string(b))
			}
			q = q.Where("("+strings.Join(conditions, " OR ")+")", args...)
		}
	}
	return q
}

type chatExportConversation struct {
	Key       string    `json:"key"`
	ContactID uuid.UUID `json:"contact_id"`
	Name      string    `json:"name"`
	Account   string    `json:"account"`
	Channel   string    `json:"channel"`
	Messages  int64     `json:"messages"`
}

type chatExportAccount struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
}

type chatExportPreview struct {
	AsOf          time.Time                `json:"as_of"`
	Scope         chatExportRequest        `json:"scope"`
	Conversations int64                    `json:"conversations"`
	Messages      int64                    `json:"messages"`
	Rows          []chatExportConversation `json:"rows"`
	Accounts      []chatExportAccount      `json:"accounts"`
	Next          string                   `json:"next,omitempty"`
	CanDownload   bool                     `json:"can_download"`
	LimitReason   string                   `json:"limit_reason,omitempty"`
	MaxMessages   int                      `json:"max_messages"`
	MaxBytes      int64                    `json:"max_bytes"`
}

func (a *App) chatExportPreview(orgID, userID uuid.UUID, req chatExportRequest, access chatExportAccess) (chatExportPreview, error) {
	result := chatExportPreview{AsOf: time.Now().UTC(), Scope: req, Rows: []chatExportConversation{}, Accounts: []chatExportAccount{}, MaxMessages: chatExportMaxMessages, MaxBytes: chatExportMaxBytes}
	var count struct {
		Messages      int64
		Conversations int64
		Bytes         int64
		Largest       int64
	}
	err := a.chatExportQuery(orgID, userID, req, access).Select("COUNT(*) AS messages, COUNT(DISTINCT " + chatExportGroupSQL + ") AS conversations, COALESCE(SUM(COALESCE(octet_length(m.content),0) + 1024),0) AS bytes, COALESCE(MAX(octet_length(m.content)),0) AS largest").Scan(&count).Error
	if err != nil {
		return result, err
	}
	result.Messages = count.Messages
	result.Conversations = count.Conversations
	result.CanDownload = count.Messages > 0 && count.Messages <= chatExportMaxMessages && count.Bytes <= chatExportMaxBytes && count.Largest <= 4<<20
	if count.Messages > chatExportMaxMessages || count.Bytes > chatExportMaxBytes || count.Largest > 4<<20 {
		result.LimitReason = errChatExportTooLarge.Error()
	}
	if req.Scope == "selected" && count.Conversations != int64(len(req.Selected)) {
		return result, errChatExportScopeChanged
	}
	page := a.chatExportQuery(orgID, userID, req, access)
	if req.After != "" {
		page = page.Where(chatExportGroupSQL+" > ?", req.After)
	}
	err = page.Select(chatExportGroupSQL + " AS key, m.contact_id AS contact_id, MAX(c.profile_name) AS name, MAX(CASE WHEN " + chatExportLegacySQL + " THEN m.whats_app_account ELSE ca.name END) AS account, MAX(CASE WHEN " + chatExportLegacySQL + " THEN 'whatsapp' ELSE ic.channel END) AS channel, COUNT(*) AS messages").Group(chatExportGroupSQL + ", m.contact_id").Order("key ASC").Limit(chatExportPageSize + 1).Scan(&result.Rows).Error
	if err != nil {
		return result, err
	}
	if len(result.Rows) > chatExportPageSize {
		result.Rows = result.Rows[:chatExportPageSize]
		result.Next = result.Rows[len(result.Rows)-1].Key
	}
	for i := range result.Rows {
		result.Rows[i].Name = chatExportDisplayName(result.Rows[i].Name, access.MaskPhones)
	}
	accountScope := req
	accountScope.Account = ""
	err = a.chatExportQuery(orgID, userID, accountScope, access).Select(chatExportAccountSQL + " AS key, MAX(CASE WHEN " + chatExportLegacySQL + " THEN m.whats_app_account ELSE ca.name END) AS name, MAX(CASE WHEN " + chatExportLegacySQL + " THEN 'whatsapp' ELSE ic.channel END) AS channel").Group(chatExportAccountSQL).Order("key ASC").Limit(501).Scan(&result.Accounts).Error
	if len(result.Accounts) > 500 {
		return result, fmt.Errorf("too many accounts; narrow the export scope")
	}
	return result, err
}
