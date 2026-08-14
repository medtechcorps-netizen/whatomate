package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/tts"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// App holds all dependencies for handlers
type App struct {
	Config            *config.Config
	DB                *gorm.DB
	Redis             *redis.Client
	Log               logf.Logger
	WhatsApp          *whatsapp.Client
	WSHub             *websocket.Hub
	Queue             queue.Queue
	CampaignSubCancel context.CancelFunc
	// HTTPClient is a shared HTTP client with connection pooling for external API calls
	HTTPClient *http.Client
	// Assigner provides shared team-based agent assignment (used by both chat and call transfers)
	Assigner *assignment.Assigner
	// CallManager handles WebRTC call sessions (nil when calling is disabled)
	CallManager *calling.Manager
	// TTS generates audio from text for IVR greetings (nil when not configured)
	TTS *tts.PiperTTS
	// S3Client for serving call recording presigned URLs (nil when not configured)
	S3Client *storage.S3Client
	// ObjectStore persists tenant media independently of any app replica.
	ObjectStore storage.ObjectStore
	// wg tracks background goroutines for graceful shutdown
	wg sync.WaitGroup
	// root points from a request-scoped clone back to the long-lived App.
	// tenantOrgID is non-zero only when DB is bound to an RLS transaction.
	root        *App
	tenantOrgID uuid.UUID
	// inboundContinuation is set only on a per-job App clone. It gives
	// chatbot sends a stable WAMID-derived action key without sharing mutable
	// execution state across concurrent webhook jobs.
	inboundContinuation *inboundContinuationExecution
}

// whatsAppClient returns the application's shared client when available. The
// fallback keeps independently constructed Apps (primarily focused tests) on
// the configured Graph API base URL instead of silently reverting to Meta.
func (a *App) whatsAppClient() *whatsapp.Client {
	if a.WhatsApp != nil {
		return a.WhatsApp
	}
	if a.Config != nil {
		if baseURL := strings.TrimSpace(a.Config.WhatsApp.BaseURL); baseURL != "" {
			return whatsapp.NewWithBaseURL(a.Log, strings.TrimRight(baseURL, "/"))
		}
	}
	return whatsapp.New(a.Log)
}

// WaitForBackgroundTasks blocks until all background goroutines complete.
// Call this during graceful shutdown to ensure all async work finishes.
func (a *App) WaitForBackgroundTasks() {
	a.rootApp().wg.Wait()
}

// getOrgID extracts organization ID from request context (set by auth middleware)
// Super admins can override the org by passing X-Organization-ID header
// Super admins MUST select an organization - no "all organizations" view
func (a *App) getOrgID(r *fastglue.Request) (uuid.UUID, error) {
	// Get user's default organization ID from JWT
	var defaultOrgID uuid.UUID
	orgIDVal := r.RequestCtx.UserValue("organization_id")
	if orgIDVal == nil {
		return uuid.Nil, errors.New("organization_id not found in context")
	}
	switch v := orgIDVal.(type) {
	case uuid.UUID:
		defaultOrgID = v
	case string:
		parsed, err := uuid.Parse(v)
		if err != nil {
			return uuid.Nil, errors.New("organization_id is not a valid UUID")
		}
		defaultOrgID = parsed
	default:
		return uuid.Nil, errors.New("organization_id is not a valid UUID")
	}

	// Check for X-Organization-ID header to switch organizations
	userID, _ := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	overrideOrgID := string(r.RequestCtx.Request.Header.Peek("X-Organization-ID"))
	if overrideOrgID != "" {
		parsedOrgID, err := uuid.Parse(overrideOrgID)
		if err == nil && parsedOrgID != defaultOrgID {
			if a.IsSuperAdmin(userID) {
				// Super admins can access any org
				var count int64
				if err := a.DB.Table("organizations").Where("id = ?", parsedOrgID).Count(&count).Error; err == nil && count > 0 {
					return parsedOrgID, nil
				}
			} else {
				// Non-super-admins can switch if they have membership
				if _, ok := access.OrganizationMembership(a.DB, userID, parsedOrgID); ok {
					return parsedOrgID, nil
				}
			}
		}
	}

	return defaultOrgID, nil
}

var (
	errExplicitOrganizationRequired    = errors.New("explicit organization header is required")
	errExplicitOrganizationInvalid     = errors.New("explicit organization header is invalid")
	errExplicitOrganizationUnavailable = errors.New("explicit organization is unavailable")
)

// getExplicitOrgID resolves the exact workspace pinned by a sensitive client
// flow. Unlike getOrgID, it never falls back to the JWT/default organization
// when the header is missing, malformed, or inaccessible.
func (a *App) getExplicitOrgID(r *fastglue.Request) (uuid.UUID, error) {
	raw := strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek("X-Organization-ID")))
	if raw == "" {
		return uuid.Nil, errExplicitOrganizationRequired
	}
	requested, err := uuid.Parse(raw)
	if err != nil || requested == uuid.Nil {
		return uuid.Nil, errExplicitOrganizationInvalid
	}
	resolved, err := a.getOrgID(r)
	if err != nil || resolved != requested {
		return uuid.Nil, errExplicitOrganizationUnavailable
	}
	// getOrgID proves access, but a stale membership (or a super-admin lookup
	// using Table) can outlive a soft-deleted workspace. Re-resolve the exact
	// organization through the normal soft-delete scope on the root control
	// plane before any provider request or tenant write.
	var activeOrganization models.Organization
	root := a.rootApp()
	if root == nil || root.DB == nil ||
		root.DB.Select("id").Where("id = ?", requested).First(&activeOrganization).Error != nil {
		return uuid.Nil, errExplicitOrganizationUnavailable
	}
	return requested, nil
}

// requireExplicitOrganization writes a stable client response for exact
// workspace binding failures without disclosing whether another workspace
// exists or which access check failed.
func (a *App) requireExplicitOrganization(r *fastglue.Request) (uuid.UUID, error) {
	organizationID, err := a.getExplicitOrgID(r)
	if err == nil {
		return organizationID, nil
	}
	if errors.Is(err, errExplicitOrganizationRequired) || errors.Is(err, errExplicitOrganizationInvalid) {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "X-Organization-ID must identify the selected organization", nil, "")
	} else {
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
	}
	return uuid.Nil, errEnvelopeSent
}

// HealthCheck returns server health status
func (a *App) HealthCheck(r *fastglue.Request) error {
	return r.SendEnvelope(map[string]string{
		"status":  "ok",
		"service": "rereply",
	})
}

// ReadyCheck returns server readiness status
func (a *App) ReadyCheck(r *fastglue.Request) error {
	// Check database connection
	sqlDB, err := a.DB.DB()
	if err != nil {
		a.Log.Error("Database connection error", "error", err)
		return r.SendErrorEnvelope(500, "Database connection error", nil, "")
	}
	if err := sqlDB.Ping(); err != nil {
		a.Log.Error("Database ping failed", "error", err)
		return r.SendErrorEnvelope(500, "Database ping failed", nil, "")
	}

	// Check Redis connection
	if a.Redis == nil {
		a.Log.Error("Redis client is unavailable")
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Redis connection unavailable",
			nil,
			"",
		)
	}
	readinessContext := context.Background()
	if err := a.Redis.Ping(readinessContext).Err(); err != nil {
		a.Log.Error("Redis connection error", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Redis connection error",
			nil,
			"",
		)
	}

	heartbeatValue, err := a.Redis.Get(readinessContext, queue.WorkerHeartbeatKey).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			a.Log.Error("Worker heartbeat lookup failed", "error", err)
		}
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Worker heartbeat unavailable",
			nil,
			"",
		)
	}
	heartbeatAt, err := time.Parse(time.RFC3339Nano, heartbeatValue)
	if err != nil || time.Since(heartbeatAt) > queue.WorkerHeartbeatTTL {
		a.Log.Error("Worker heartbeat is invalid or stale", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Worker heartbeat stale",
			nil,
			"",
		)
	}

	return r.SendEnvelope(map[string]string{
		"status": "ready",
		"worker": "ready",
	})
}

// GetEmbeddedSignupConfig returns public configuration values for the embedded signup flow
func (a *App) GetEmbeddedSignupConfig(r *fastglue.Request) error {
	type EmbeddedSignupConfig struct {
		OrganizationID     uuid.UUID `json:"organization_id"`
		WhatsAppAppID      string    `json:"whatsapp_app_id,omitempty"`
		WhatsAppConfigID   string    `json:"whatsapp_config_id,omitempty"`
		WhatsAppAPIVersion string    `json:"whatsapp_api_version,omitempty"`
		HasAppSecret       bool      `json:"has_app_secret"`
	}

	selectedOrgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	if orgID != selectedOrgID {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
	}

	appID, appSecret, configID, err := a.resolveMetaAppCreds(orgID)
	if err != nil {
		if errors.Is(err, errMetaIntegrationDisabled) {
			return r.SendEnvelope(EmbeddedSignupConfig{OrganizationID: orgID, HasAppSecret: false})
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to resolve credentials", nil, "")
	}

	config := EmbeddedSignupConfig{
		OrganizationID:     orgID,
		WhatsAppAppID:      appID,
		WhatsAppConfigID:   configID,
		WhatsAppAPIVersion: a.metaAPIVersion(),
		HasAppSecret:       strings.TrimSpace(appSecret) != "",
	}

	return r.SendEnvelope(config)
}

// StartCampaignStatsSubscriber starts listening for campaign stats updates from Redis pub/sub
// and broadcasts them via WebSocket
func (a *App) StartCampaignStatsSubscriber() error {
	if a.WSHub == nil {
		a.Log.Warn("WebSocket hub not initialized, skipping campaign stats subscriber")
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.CampaignSubCancel = cancel

	subscriber := queue.NewSubscriber(a.Redis, a.Log)

	err := subscriber.SubscribeCampaignStats(ctx, func(update *queue.CampaignStatsUpdate) {
		a.Log.Debug("Received campaign stats update from Redis",
			"campaign_id", update.CampaignID,
			"status", update.Status,
			"sent", update.SentCount,
		)

		// Broadcast to organization via WebSocket
		a.WSHub.BroadcastToOrg(update.OrganizationID, websocket.WSMessage{
			Type: websocket.TypeCampaignStatsUpdate,
			Payload: map[string]any{
				"campaign_id":     update.CampaignID,
				"status":          update.Status,
				"sent_count":      update.SentCount,
				"delivered_count": update.DeliveredCount,
				"read_count":      update.ReadCount,
				"failed_count":    update.FailedCount,
			},
		})
	})

	if err != nil {
		cancel()
		return err
	}

	a.Log.Info("Campaign stats subscriber started")
	return nil
}

// StopCampaignStatsSubscriber stops the campaign stats subscriber
func (a *App) StopCampaignStatsSubscriber() {
	if a.CampaignSubCancel != nil {
		a.CampaignSubCancel()
	}
}

// getOrgAndUserID extracts both organization ID and user ID from the request context.
// Returns an error if either is missing or invalid.
func (a *App) getOrgAndUserID(r *fastglue.Request) (orgID, userID uuid.UUID, err error) {
	orgID, err = a.getOrgID(r)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}

	userIDVal := r.RequestCtx.UserValue("user_id")
	if userIDVal == nil {
		return uuid.Nil, uuid.Nil, errors.New("user_id not found in context")
	}
	switch v := userIDVal.(type) {
	case uuid.UUID:
		userID = v
	case string:
		userID, err = uuid.Parse(v)
		if err != nil {
			return uuid.Nil, uuid.Nil, errors.New("user_id is not a valid UUID")
		}
	default:
		return uuid.Nil, uuid.Nil, errors.New("user_id is not a valid UUID")
	}

	return orgID, userID, nil
}

// requirePermission checks if the user has the required permission.
// Returns nil if permitted, otherwise sends a 403 error envelope and returns errEnvelopeSent.
// Automatically extracts orgID from the request for org-aware permission checks.
func (a *App) requirePermission(r *fastglue.Request, userID uuid.UUID, resource, action string) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		a.Log.Error("Failed to get organization ID for permission check", "error", err, "user_id", userID)
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Insufficient permissions", nil, "")
		return errEnvelopeSent
	}
	if !a.HasPermission(userID, resource, action, orgID) {
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Insufficient permissions", nil, "")
		return errEnvelopeSent
	}
	return nil
}

// requireAuth extracts the organization ID and user ID from the request and
// verifies the user holds the given permission. On failure it writes the
// appropriate error envelope (401 if unauthenticated, 403 if the permission is
// missing) and returns errEnvelopeSent, so callers should `return nil` early.
func (a *App) requireAuth(r *fastglue.Request, resource, action string) (orgID, userID uuid.UUID, err error) {
	orgID, userID, err = a.getOrgAndUserID(r)
	if err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return uuid.Nil, uuid.Nil, errEnvelopeSent
	}
	if !a.HasPermission(userID, resource, action, orgID) {
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Insufficient permissions", nil, "")
		return uuid.Nil, uuid.Nil, errEnvelopeSent
	}
	if entitlementKey, licensed := ProductEntitlementKeyForResource(resource); licensed {
		allowed, entitlementErr := a.HasProductEntitlement(userID, orgID, entitlementKey)
		if entitlementErr != nil {
			a.Log.Error(
				"Failed to evaluate product entitlement",
				"error",
				entitlementErr,
				"organization_id",
				orgID,
				"resource",
				resource,
				"entitlement",
				entitlementKey,
			)
			_ = r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Product entitlement could not be evaluated",
				nil,
				"",
			)
			return uuid.Nil, uuid.Nil, errEnvelopeSent
		}
		if !allowed {
			_ = r.SendErrorEnvelope(
				fasthttp.StatusPaymentRequired,
				"Feature is not included in the organization's active plan",
				nil,
				"",
			)
			return uuid.Nil, uuid.Nil, errEnvelopeSent
		}
	}
	return orgID, userID, nil
}

// decodeRequest decodes a JSON request body into the provided struct.
// Returns nil on success, otherwise sends a 400 error envelope and returns errEnvelopeSent.
func (a *App) decodeRequest(r *fastglue.Request, v any) error {
	if err := r.Decode(v, "json"); err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	return nil
}
