package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

var outboundURLTemplateValue = regexp.MustCompile(`^\{\{\s*[A-Za-z0-9_.-]+\s*\}\}$`)

// validateWebhookRuntimeURL performs structural validation of a resolved
// outbound URL. It blocks known-internal hostnames and private IP literals.
// Runtime SSRF protection (including DNS rebinding) is handled by
// SSRFSafeDialer.
func validateWebhookRuntimeURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be https")
	}
	if u.User != nil {
		return fmt.Errorf("URL must not contain user credentials")
	}
	if u.Fragment != "" {
		return fmt.Errorf("URL must not contain a fragment")
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("URL must have a hostname")
	}

	// Block obvious internal hostnames
	lower := strings.ToLower(hostname)
	if lower == "localhost" || lower == "0.0.0.0" || strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("URL must not point to internal addresses")
	}

	// Block private/loopback IP literals (e.g. http://127.0.0.1, http://[::1])
	if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("URL must not point to internal addresses")
		}
	}

	return nil
}

// validateWebhookURL validates a stored URL template. Literal query values are
// forbidden because third-party API keys are commonly placed in query strings,
// where they would otherwise be persisted and returned as ordinary config.
// Dynamic query values remain supported through {{variable}} placeholders;
// static credentials belong in the encrypted outbound-header vault.
func validateWebhookURL(rawURL string) error {
	if err := validateWebhookRuntimeURL(rawURL); err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.RawQuery == "" {
		return nil
	}
	values, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return fmt.Errorf("URL query parameters are invalid")
	}
	for name, candidates := range values {
		if strings.TrimSpace(name) == "" || len(candidates) == 0 {
			return fmt.Errorf("URL query parameters are invalid")
		}
		for _, value := range candidates {
			if !outboundURLTemplateValue.MatchString(value) {
				return fmt.Errorf("URL query values must use {{variable}} placeholders; store credentials in encrypted headers")
			}
		}
	}
	return nil
}

// validateEventWebhookURL validates generic event-delivery webhooks. These
// webhooks have no URL rendering context, so accepting template-looking query
// values would send them literally and mislead administrators. Routing data
// belongs in the path; credentials belong in encrypted headers.
func validateEventWebhookURL(rawURL string) error {
	if err := validateWebhookRuntimeURL(rawURL); err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("event webhook URLs must not contain query parameters; store credentials in encrypted headers")
	}
	return nil
}

// SSRFSafeDialer returns a DialContext function that blocks connections to
// private/loopback IPs after DNS resolution. Use this in http.Transport
// for webhook and custom action HTTP calls.
func SSRFSafeDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}

		for _, ipStr := range ips {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				return nil, fmt.Errorf("connection to private address %s is not allowed", ipStr)
			}
		}

		// Connect to first resolved IP
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
	}
}

// WebhookRequest represents the request body for creating/updating a webhook
type WebhookRequest struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Events      []string          `json:"events"`
	Headers     map[string]string `json:"headers"`
	Secret      string            `json:"secret"`
	ClearSecret bool              `json:"clear_secret"`
	IsActive    *bool             `json:"is_active"`
}

// WebhookResponse represents the API response for a webhook
type WebhookResponse struct {
	ID        uuid.UUID         `json:"id"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Events    []string          `json:"events"`
	Headers   map[string]string `json:"headers"`
	IsActive  bool              `json:"is_active"`
	HasSecret bool              `json:"has_secret"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

// AvailableWebhookEvents returns the list of available webhook event types
var AvailableWebhookEvents = []map[string]string{
	{"value": string(models.WebhookEventMessageIncoming), "label": "Message Incoming", "description": "When a new message is received from a contact"},
	{"value": string(models.WebhookEventMessageSent), "label": "Message Sent", "description": "When an agent sends a message"},
	{"value": string(models.WebhookEventMessageOutgoing), "label": "Message Outgoing", "description": "When a message is sent to a contact (includes echoes)"},
	{"value": string(models.WebhookEventContactCreated), "label": "Contact Created", "description": "When a new contact is created"},
	{"value": string(models.WebhookEventContactUpdated), "label": "Contact Updated", "description": "When a customer profile is updated"},
	{"value": string(models.WebhookEventContactMerged), "label": "Contact Merged", "description": "When a duplicate customer is merged into a canonical profile"},
	{"value": string(models.WebhookEventCRMLeadCreated), "label": "Journey Created", "description": "When a CRM lead or customer journey is created"},
	{"value": string(models.WebhookEventCRMLeadUpdated), "label": "Journey Updated", "description": "When a CRM lead or customer journey is updated"},
	{"value": string(models.WebhookEventCRMStageMoved), "label": "Journey Stage Moved", "description": "When a CRM lead or customer journey changes stage"},
	{"value": string(models.WebhookEventTaskCreated), "label": "Task Created", "description": "When a customer follow-up task is created"},
	{"value": string(models.WebhookEventTaskCompleted), "label": "Task Completed", "description": "When a customer follow-up task is completed"},
	{"value": string(models.WebhookEventBookingCreated), "label": "Booking Created", "description": "When a customer booking is created"},
	{"value": string(models.WebhookEventBookingStatus), "label": "Booking Status Changed", "description": "When a customer booking changes status"},
	{"value": string(models.WebhookEventPackageSold), "label": "Package Sold", "description": "When a package is sold or assigned to a customer"},
	{"value": string(models.WebhookEventPackageLow), "label": "Package Balance Low", "description": "When a customer package has few credits remaining"},
	{"value": string(models.WebhookEventPackageExpiring), "label": "Package Expiring", "description": "When a customer package is nearing expiry"},
	{"value": string(models.WebhookEventInvoiceCreated), "label": "Invoice Created", "description": "When a customer invoice is issued"},
	{"value": string(models.WebhookEventInvoiceOverdue), "label": "Invoice Overdue", "description": "When a customer invoice becomes overdue"},
	{"value": string(models.WebhookEventInvoicePaid), "label": "Invoice Paid", "description": "When a customer invoice is fully paid"},
	{"value": string(models.WebhookEventPaymentRecorded), "label": "Payment Recorded", "description": "When a customer payment is recorded"},
	{"value": string(models.WebhookEventTransferCreated), "label": "Transfer Created", "description": "When a transfer to human agent is requested"},
	{"value": string(models.WebhookEventTransferAssigned), "label": "Transfer Assigned", "description": "When a transfer is assigned to an agent"},
	{"value": string(models.WebhookEventTransferResumed), "label": "Transfer Resumed", "description": "When chatbot is resumed (transfer closed)"},
}

// ListWebhooks returns all webhooks for the organization
func (a *App) ListWebhooks(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	query := a.DB.Where("organization_id = ?", orgID)

	// Apply search filter - search by name or URL (case-insensitive)
	if search != "" {
		searchPattern := "%" + search + "%"
		query = query.Where("name ILIKE ? OR url ILIKE ?", searchPattern, searchPattern)
	}

	var total int64
	query.Model(&models.Webhook{}).Count(&total)

	var webhooks []models.Webhook
	if err := pg.Apply(query.Model(&models.Webhook{}).Order("created_at DESC")).
		Find(&webhooks).Error; err != nil {
		a.Log.Error("Failed to list webhooks", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list webhooks", nil, "")
	}

	result := make([]WebhookResponse, len(webhooks))
	for i, wh := range webhooks {
		result[i] = webhookToResponse(wh)
	}

	return r.SendEnvelope(map[string]any{
		"webhooks":         result,
		"available_events": AvailableWebhookEvents,
		"total":            total,
		"page":             pg.Page,
		"limit":            pg.Limit,
	})
}

// GetWebhook returns a single webhook by ID
func (a *App) GetWebhook(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionRead)
	if err != nil {
		return nil
	}

	webhookID, err := parsePathUUID(r, "id", "webhook")
	if err != nil {
		return nil
	}

	webhook, err := findByIDAndOrg[models.Webhook](a.DB, r, webhookID, orgID, "Webhook")
	if err != nil {
		return nil
	}

	return r.SendEnvelope(webhookToResponse(*webhook))
}

// CreateWebhook creates a new webhook
func (a *App) CreateWebhook(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req WebhookRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if req.Name == "" || req.URL == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "name and url are required", nil, "")
	}

	if err := validateEventWebhookURL(req.URL); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	if len(req.Events) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "at least one event must be selected", nil, "")
	}
	if req.ClearSecret {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "clear_secret is only valid when updating a webhook", nil, "")
	}

	headers, err := a.protectOutboundHeaders(req.Headers, nil)
	if err != nil {
		if errors.Is(err, errOutboundHeaderEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Webhook credential storage is unavailable", nil, "")
		}
		if errors.Is(err, errOutboundHeaderMaskWithoutExisting) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Masked header values can only preserve an existing credential", nil, "")
		}
		a.Log.Error("Failed to protect webhook headers", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create webhook", nil, "")
	}

	// HMAC signing is optional. Do not generate an undisclosed secret: a
	// receiver could never verify signatures made with a value it was not
	// given. When supplied, the customer-owned secret is write-only and
	// encrypted before storage.
	encryptedSecret := ""
	if secret := strings.TrimSpace(req.Secret); secret != "" {
		if a.Config == nil || strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Webhook credential storage is unavailable", nil, "")
		}
		encryptedSecret, err = appcrypto.Encrypt(secret, a.Config.App.EncryptionKey)
		if err != nil || !appcrypto.IsEncrypted(encryptedSecret) {
			a.Log.Error("Failed to encrypt webhook secret", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create webhook", nil, "")
		}
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	webhook := models.Webhook{
		OrganizationID: orgID,
		Name:           req.Name,
		URL:            req.URL,
		Events:         req.Events,
		Headers:        headers,
		Secret:         encryptedSecret,
		IsActive:       isActive,
	}

	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&webhook).Error; err != nil {
			return err
		}
		// GORM applies the model's default:true tag when a bool is false during
		// Create. Force the explicit API value in the same transaction so a
		// newly paused webhook can never become briefly or permanently active.
		if req.IsActive != nil && !*req.IsActive {
			if err := tx.Model(&models.Webhook{}).
				Where("id = ? AND organization_id = ?", webhook.ID, orgID).
				Update("is_active", false).Error; err != nil {
				return err
			}
			webhook.IsActive = false
		}
		return nil
	}); err != nil {
		a.Log.Error("Failed to create webhook", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create webhook", nil, "")
	}

	// Invalidate cache
	a.InvalidateWebhooksCache(orgID)

	a.logAudit(orgID, userID,
		"webhook", webhook.ID, models.AuditActionCreated, nil, webhookAuditSnapshot(&webhook))

	return r.SendEnvelope(webhookToResponse(webhook))
}

// UpdateWebhook updates an existing webhook
func (a *App) UpdateWebhook(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionWrite)
	if err != nil {
		return nil
	}

	webhookID, err := parsePathUUID(r, "id", "webhook")
	if err != nil {
		return nil
	}

	webhook, err := findByIDAndOrg[models.Webhook](a.DB, r, webhookID, orgID, "Webhook")
	if err != nil {
		return nil
	}

	oldSnap := webhookAuditSnapshot(webhook)

	var req WebhookRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if req.Name != "" {
		webhook.Name = req.Name
	}
	if req.URL != "" {
		if err := validateEventWebhookURL(req.URL); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		webhook.URL = req.URL
	}
	if len(req.Events) > 0 {
		webhook.Events = req.Events
	}

	// Update headers if provided
	if req.Headers != nil {
		headers, protectErr := a.protectOutboundHeaders(req.Headers, webhook.Headers)
		if protectErr != nil {
			if errors.Is(protectErr, errOutboundHeaderEncryptionUnavailable) {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Webhook credential storage is unavailable", nil, "")
			}
			if errors.Is(protectErr, errOutboundHeaderMaskWithoutExisting) {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Masked header values can only preserve an existing credential", nil, "")
			}
			a.Log.Error("Failed to protect webhook headers", "error", protectErr)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update webhook", nil, "")
		}
		webhook.Headers = headers
	}

	if req.ClearSecret && strings.TrimSpace(req.Secret) != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "secret and clear_secret cannot be supplied together", nil, "")
	}
	// Replace the write-only secret only when a new value is supplied, or clear
	// it explicitly. An omitted/empty secret otherwise preserves the value.
	if req.ClearSecret {
		webhook.Secret = ""
	} else if strings.TrimSpace(req.Secret) != "" {
		if a.Config == nil || strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Webhook credential storage is unavailable", nil, "")
		}
		encryptedSecret, encryptErr := appcrypto.Encrypt(req.Secret, a.Config.App.EncryptionKey)
		if encryptErr != nil || !appcrypto.IsEncrypted(encryptedSecret) {
			a.Log.Error("Failed to encrypt webhook secret", "error", encryptErr)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update webhook", nil, "")
		}
		webhook.Secret = encryptedSecret
	}

	if req.IsActive != nil {
		webhook.IsActive = *req.IsActive
	}

	if err := a.DB.Save(webhook).Error; err != nil {
		a.Log.Error("Failed to update webhook", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update webhook", nil, "")
	}

	// Invalidate cache
	a.InvalidateWebhooksCache(orgID)

	a.logAudit(orgID, userID,
		"webhook", webhook.ID, models.AuditActionUpdated, oldSnap, webhookAuditSnapshot(webhook))

	return r.SendEnvelope(webhookToResponse(*webhook))
}

// DeleteWebhook deletes a webhook
func (a *App) DeleteWebhook(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionDelete)
	if err != nil {
		return nil
	}

	webhookID, err := parsePathUUID(r, "id", "webhook")
	if err != nil {
		return nil
	}

	var webhook models.Webhook
	if err := a.DB.Where("id = ? AND organization_id = ?", webhookID, orgID).First(&webhook).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Webhook not found", nil, "")
	}

	if err := a.DB.Delete(&webhook).Error; err != nil {
		a.Log.Error("Failed to delete webhook", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete webhook", nil, "")
	}

	// Invalidate cache
	a.InvalidateWebhooksCache(orgID)

	a.logAudit(orgID, userID,
		"webhook", webhookID, models.AuditActionDeleted, webhookAuditSnapshot(&webhook), nil)

	return r.SendEnvelope(map[string]string{"message": "Webhook deleted successfully"})
}

// TestWebhook sends a test event to a webhook
func (a *App) TestWebhook(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWebhooks, models.ActionWrite)
	if err != nil {
		return nil
	}

	webhookID, err := parsePathUUID(r, "id", "webhook")
	if err != nil {
		return nil
	}

	webhook, err := findByIDAndOrg[models.Webhook](a.DB, r, webhookID, orgID, "Webhook")
	if err != nil {
		return nil
	}

	// Send a test event synchronously
	testData := map[string]any{
		"test":      true,
		"message":   "This is a test webhook from ReReply",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	payload := OutboundWebhookPayload{
		Event:     "test",
		Timestamp: time.Now().UTC(),
		Data:      testData,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		a.Log.Error("Failed to create test payload", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create test payload", nil, "")
	}

	// Use timeout context for test webhook request
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := a.sendWebhookRequest(ctx, *webhook, jsonData); err != nil {
		a.Log.Error("Webhook test failed", "webhook_id", webhook.ID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Webhook test failed", nil, "")
	}

	return r.SendEnvelope(map[string]string{"message": "Test webhook sent successfully"})
}

func webhookAuditSnapshot(wh *models.Webhook) map[string]any {
	if wh == nil {
		return nil
	}
	events := make([]string, len(wh.Events))
	copy(events, wh.Events)
	return map[string]any{
		"name": wh.Name,
		// Paths can contain opaque provider routing material. Audit the origin
		// only; administrators with webhook-read access can inspect live config.
		"url_origin": redactURLForLog(wh.URL),
		"events":     events,
		"is_active":  wh.IsActive,
	}
}

func webhookToResponse(wh models.Webhook) WebhookResponse {
	// Convert events
	events := make([]string, len(wh.Events))
	copy(events, wh.Events)

	headers := redactOutboundHeaders(wh.Headers)

	return WebhookResponse{
		ID:        wh.ID,
		Name:      wh.Name,
		URL:       wh.URL,
		Events:    events,
		Headers:   headers,
		IsActive:  wh.IsActive,
		HasSecret: wh.Secret != "",
		CreatedAt: wh.CreatedAt.Format(time.RFC3339),
		UpdatedAt: wh.UpdatedAt.Format(time.RFC3339),
	}
}
