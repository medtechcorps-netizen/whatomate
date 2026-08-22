package handlers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// TenantRequestHandler is a handler method expression that can be rebound to a
// request-scoped App whose DB is a tenant transaction.
type TenantRequestHandler func(*App, *fastglue.Request) error

var errTenantResponseRollback = errors.New("tenant handler returned an error response")

var errPlatformComplianceInstagramTenantRequired = errors.New("platform compliance Instagram tenant is required")

// Tenant wraps an authenticated handler in a transaction-local PostgreSQL
// tenant context. Purpose admission is always checked; when RLS is disabled,
// only the transaction-local tenant binding is skipped.
func (a *App) Tenant(handler TenantRequestHandler) fastglue.FastRequestHandler {
	return func(r *fastglue.Request) error {
		if handler == nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Handler is not configured",
				nil,
				"",
			)
		}
		organizationID, err := a.getOrgID(r)
		if err != nil || organizationID == uuid.Nil {
			return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		}
		if !a.rlsEnabled() {
			return handler(a, r)
		}

		var handlerErr error
		err = database.WithTenant(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			scoped := a.scopedApp(tx, organizationID)
			handlerErr = handler(scoped, r)
			if handlerErr != nil {
				return handlerErr
			}
			if r.RequestCtx.Response.StatusCode() >= fasthttp.StatusBadRequest {
				return errTenantResponseRollback
			}
			return nil
		})
		if errors.Is(err, errTenantResponseRollback) {
			return nil
		}
		if err != nil {
			if handlerErr != nil {
				return handlerErr
			}
			a.Log.Error("Tenant database transaction failed",
				"error", err,
				"organization_id", organizationID,
				"path", string(r.RequestCtx.Path()),
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Database request failed",
				nil,
				"",
			)
		}
		return handlerErr
	}
}

func requireOrdinaryTenantOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	purpose, err := database.IsPlatformComplianceOrganization(db, organizationID)
	if err != nil {
		return err
	}
	if purpose {
		return database.ErrPlatformComplianceTenant
	}
	return nil
}

func requirePlatformComplianceInstagramOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	purpose, err := database.IsPlatformComplianceOrganization(db, organizationID)
	if err != nil {
		return err
	}
	if !purpose {
		return errPlatformComplianceInstagramTenantRequired
	}
	var eligible bool
	result := db.Raw(`
		SELECT true
		FROM public.organizations AS organization
		JOIN public.resellers AS reseller ON reseller.id = organization.reseller_id
		WHERE organization.id = ?
		  AND organization.deleted_at IS NULL
		  AND reseller.deleted_at IS NULL
		  AND reseller.status = 'active'
		  AND reseller.slug = ?
		  AND pg_catalog.jsonb_typeof(
			organization.settings -> CAST(? AS text)
		  ) = 'boolean'
		  AND organization.settings -> CAST(? AS text) = 'true'::jsonb
		FOR SHARE OF organization
	`,
		organizationID,
		database.PlatformResellerSlug,
		database.PlatformComplianceInstagramMarkerKey,
		database.PlatformComplianceInstagramMarkerKey,
	).Scan(&eligible)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 || !eligible {
		return errPlatformComplianceInstagramTenantRequired
	}
	return nil
}

func (a *App) rlsEnabled() bool {
	return a != nil && a.Config != nil && a.Config.Database.RLSEnabled
}

func (a *App) rootApp() *App {
	if a != nil && a.root != nil {
		return a.root
	}
	return a
}

func (a *App) scopedApp(tx *gorm.DB, organizationID uuid.UUID) *App {
	root := a.rootApp()
	scoped := &App{
		Config:              root.Config,
		DB:                  tx,
		Redis:               root.Redis,
		Log:                 root.Log,
		WhatsApp:            root.WhatsApp,
		WSHub:               root.WSHub,
		Queue:               root.Queue,
		CampaignSubCancel:   root.CampaignSubCancel,
		HTTPClient:          root.HTTPClient,
		CallManager:         root.CallManager,
		TTS:                 root.TTS,
		S3Client:            root.S3Client,
		ObjectStore:         root.ObjectStore,
		root:                root,
		tenantOrgID:         organizationID,
		inboundContinuation: a.inboundContinuation,
	}
	if root.Assigner != nil {
		scoped.Assigner = root.Assigner.WithDB(tx)
	}
	return scoped
}

// WithTenantApp is the ordinary-tenant entry point for background jobs and
// webhook goroutines. It rejects reserved compliance tenants before invoking
// the callback and always starts from the root connection pool, so it never
// reuses an HTTP transaction after that request has completed.
func (a *App) WithTenantApp(organizationID uuid.UUID, fn func(*App) error) error {
	if a == nil || fn == nil {
		return errors.New("tenant app callback is required")
	}
	if organizationID == uuid.Nil {
		return database.ErrMissingTenant
	}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("database connection is required")
	}
	if !a.rlsEnabled() {
		if err := requireOrdinaryTenantOrganization(root.DB, organizationID); err != nil {
			return err
		}
		return fn(root)
	}
	return database.WithTenant(root.DB, organizationID, func(tx *gorm.DB) error {
		if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
			return err
		}
		return fn(root.scopedApp(tx, organizationID))
	})
}

// WithCommittedTenantApp runs one short ordinary-tenant database phase and
// commits it before returning. It is used when provider I/O must happen between
// durable tenant state transitions. Unlike WithTenantApp, it also opens a real
// transaction when PostgreSQL RLS is disabled.
func (a *App) WithCommittedTenantApp(organizationID uuid.UUID, fn func(*App) error) error {
	if a == nil || fn == nil {
		return errors.New("tenant app callback is required")
	}
	if organizationID == uuid.Nil {
		return database.ErrMissingTenant
	}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("database connection is required")
	}
	if a.rlsEnabled() {
		return database.WithTenant(root.DB, organizationID, func(tx *gorm.DB) error {
			if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
				return err
			}
			return fn(root.scopedApp(tx, organizationID))
		})
	}
	return root.DB.Transaction(func(tx *gorm.DB) error {
		if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
			return err
		}
		return fn(root.scopedApp(tx, organizationID))
	})
}

// withPlatformComplianceInstagramTenantApp is the only tenant-App entry point
// for the managed Instagram compliance callback. It rejects ordinary tenants,
// other compliance features, malformed reserved markers, and inactive or
// non-platform ownership before exposing a scoped transaction.
func (a *App) withPlatformComplianceInstagramTenantApp(
	organizationID uuid.UUID,
	fn func(*App) error,
) error {
	if a == nil || fn == nil {
		return errors.New("platform compliance Instagram callback is required")
	}
	if organizationID == uuid.Nil {
		return database.ErrMissingTenant
	}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("database connection is required")
	}
	run := func(tx *gorm.DB) error {
		if err := requirePlatformComplianceInstagramOrganization(tx, organizationID); err != nil {
			return err
		}
		return fn(root.scopedApp(tx, organizationID))
	}
	return database.WithTenantReadCommitted(root.DB, organizationID, run)
}

func (a *App) hasTenantScope() bool {
	return a != nil && a.tenantOrgID != uuid.Nil
}

func (a *App) resolveWhatsAppOrganization(phoneID string) (uuid.UUID, error) {
	phoneID = strings.TrimSpace(phoneID)
	if phoneID == "" {
		return uuid.Nil, database.ErrMissingTenant
	}
	root := a.rootApp()
	var organizationID uuid.UUID
	if a.rlsEnabled() {
		var organizationIDText string
		if err := root.DB.Raw(
			"SELECT public.rereply_resolve_whatsapp_org(?)::text",
			phoneID,
		).Scan(&organizationIDText).Error; err != nil {
			return uuid.Nil, err
		}
		if organizationIDText == "" {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		var err error
		organizationID, err = uuid.Parse(organizationIDText)
		if err != nil {
			return uuid.Nil, fmt.Errorf("parse WhatsApp tenant: %w", err)
		}
	} else {
		var account models.WhatsAppAccount
		if err := root.DB.
			Select("organization_id").
			Where("BTRIM(phone_id) = BTRIM(?)", phoneID).
			First(&account).Error; err != nil {
			return uuid.Nil, err
		}
		organizationID = account.OrganizationID
	}
	if organizationID == uuid.Nil {
		return uuid.Nil, gorm.ErrRecordNotFound
	}
	return organizationID, nil
}

func (a *App) resolveWebhookOrganization(verifyToken string) (uuid.UUID, error) {
	if verifyToken == "" {
		return uuid.Nil, database.ErrMissingTenant
	}
	root := a.rootApp()
	var organizationID uuid.UUID
	if a.rlsEnabled() {
		var organizationIDText string
		if err := root.DB.Raw(
			"SELECT public.rereply_resolve_webhook_org(?)::text",
			verifyToken,
		).Scan(&organizationIDText).Error; err != nil {
			return uuid.Nil, err
		}
		if organizationIDText == "" {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		var err error
		organizationID, err = uuid.Parse(organizationIDText)
		if err != nil {
			return uuid.Nil, fmt.Errorf("parse webhook tenant: %w", err)
		}
	} else {
		var account models.WhatsAppAccount
		if err := root.DB.
			Select("organization_id").
			Where("webhook_verify_token = ?", verifyToken).
			First(&account).Error; err != nil {
			return uuid.Nil, err
		}
		organizationID = account.OrganizationID
	}
	if organizationID == uuid.Nil {
		return uuid.Nil, gorm.ErrRecordNotFound
	}
	return organizationID, nil
}

func (a *App) resolveWABAOrganizations(businessID string) ([]uuid.UUID, error) {
	if businessID == "" {
		return nil, database.ErrMissingTenant
	}
	root := a.rootApp()
	var organizationIDs []uuid.UUID
	if a.rlsEnabled() {
		var organizationIDTexts []string
		if err := root.DB.Raw(
			"SELECT resolved::text FROM public.rereply_resolve_waba_orgs(?) AS resolved",
			businessID,
		).Scan(&organizationIDTexts).Error; err != nil {
			return nil, err
		}
		for _, organizationIDText := range organizationIDTexts {
			organizationID, err := uuid.Parse(organizationIDText)
			if err != nil {
				return nil, fmt.Errorf("parse WABA tenant: %w", err)
			}
			organizationIDs = append(organizationIDs, organizationID)
		}
	} else {
		var accounts []models.WhatsAppAccount
		if err := root.DB.Model(&models.WhatsAppAccount{}).
			Select("organization_id").
			Distinct("organization_id").
			Where("business_id = ?", businessID).
			Find(&accounts).Error; err != nil {
			return nil, err
		}
		for _, account := range accounts {
			organizationIDs = append(organizationIDs, account.OrganizationID)
		}
	}
	if len(organizationIDs) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return organizationIDs, nil
}

func (a *App) withPhoneTenant(phoneID string, fn func(*App) error) error {
	organizationID, err := a.resolveWhatsAppOrganization(phoneID)
	if err != nil {
		return fmt.Errorf("resolve WhatsApp tenant: %w", err)
	}
	return a.WithTenantApp(organizationID, fn)
}
