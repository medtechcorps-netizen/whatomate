package threadsplatform

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrStoreUnavailable      = errors.New("managed Threads store is unavailable")
	ErrInvalidBindingClaim   = errors.New("managed Threads binding claim is invalid")
	ErrIntegrationNotManaged = errors.New("threads integration is not configured for the requested managed platform app")
	ErrBindingClaimConflict  = errors.New("managed Threads subject or asset is already claimed")
	ErrBindingNotFound       = errors.New("managed Threads binding was not found")
	ErrBindingReconnectFence = errors.New("managed Threads reconnect no longer matches the claimed binding")
	ErrInvalidJournalReceipt = errors.New("managed Threads journal receipt is invalid")

	lowerHexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reasonCodePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,79}$`)
)

type BindingClaim struct {
	OrganizationID          uuid.UUID
	IntegrationID           uuid.UUID
	ChannelAccountID        *uuid.UUID
	PlatformAppKey          string
	PlatformAppID           string
	OAuthSubjectID          string
	AuthorityAssetID        string
	ConfigurationGeneration uint64
	AuthorizationGeneration uint64
	ClaimedAt               time.Time
}

// BindingReconnect is a compare-and-swap authorization rotation. Every
// ownership field is repeated deliberately: a reconnect can refresh only the
// exact existing tenant claim and can never rekey, alias, or transfer it.
type BindingReconnect struct {
	OrganizationID                  uuid.UUID
	BindingID                       uuid.UUID
	IntegrationID                   uuid.UUID
	ChannelAccountID                uuid.UUID
	PlatformAppKey                  string
	PlatformAppID                   string
	OAuthSubjectID                  string
	AuthorityAssetID                string
	ConfigurationGeneration         uint64
	ExpectedAuthorizationGeneration uint64
}

type JournalReceipt struct {
	PlatformAppKey          string
	PlatformAppID           string
	EventDigest             string
	EventType               string
	SubjectDigest           string
	ConfigurationGeneration uint64
	ReceivedAt              time.Time
}

// BindingStore is the compile-safe boundary for later OAuth and callback
// phases. Phase 1 deliberately supplies no provider-facing handler.
type BindingStore interface {
	ClaimBinding(context.Context, BindingClaim) (*models.ThreadsPlatformBinding, error)
	ReleaseBinding(context.Context, uuid.UUID, uuid.UUID, string) error
	RecordJournalReceipt(context.Context, JournalReceipt) (bool, error)
}

type PostgresStore struct {
	db *gorm.DB
}

var _ BindingStore = (*PostgresStore)(nil)

func NewPostgresStore(db *gorm.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// ClaimBinding creates a new pending claim in a tenant-scoped transaction.
// Partial unique indexes arbitrate cross-tenant races. ON CONFLICT never
// updates the existing owner, so a duplicate cannot become an automatic
// profile transfer or disclose the other tenant.
func (store *PostgresStore) ClaimBinding(
	ctx context.Context,
	claim BindingClaim,
) (*models.ThreadsPlatformBinding, error) {
	if store == nil || store.db == nil {
		return nil, ErrStoreUnavailable
	}
	if err := validateBindingClaim(claim); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var binding *models.ThreadsPlatformBinding
	err := database.WithTenant(store.db.WithContext(ctx), claim.OrganizationID, func(tx *gorm.DB) error {
		var claimErr error
		binding, claimErr = ClaimBindingTx(tx, claim)
		return claimErr
	})
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// ClaimBindingTx performs the global claim inside the caller's tenant
// transaction so account, encrypted credential, and ownership become durable
// atomically. Partial unique indexes remain the global race arbiter.
func ClaimBindingTx(tx *gorm.DB, claim BindingClaim) (*models.ThreadsPlatformBinding, error) {
	if tx == nil {
		return nil, ErrStoreUnavailable
	}
	if err := validateBindingClaim(claim); err != nil {
		return nil, err
	}
	if err := validateManagedIntegrationTx(tx, claim.OrganizationID, claim.IntegrationID, claim.PlatformAppKey); err != nil {
		return nil, err
	}
	if claim.ChannelAccountID != nil {
		if err := validateManagedAccountTx(
			tx, claim.OrganizationID, *claim.ChannelAccountID, claim.AuthorityAssetID,
		); err != nil {
			return nil, err
		}
	}
	claimedAt := claim.ClaimedAt.UTC()
	if claimedAt.IsZero() {
		claimedAt = time.Now().UTC()
	}
	authorizationGeneration := claim.AuthorizationGeneration
	if authorizationGeneration == 0 {
		authorizationGeneration = 1
	}
	binding := &models.ThreadsPlatformBinding{
		BaseModel:               models.BaseModel{ID: uuid.New()},
		OrganizationID:          claim.OrganizationID,
		IntegrationID:           claim.IntegrationID,
		ChannelAccountID:        claim.ChannelAccountID,
		PlatformAppKey:          claim.PlatformAppKey,
		PlatformAppID:           claim.PlatformAppID,
		OAuthSubjectID:          claim.OAuthSubjectID,
		AuthorityAssetID:        claim.AuthorityAssetID,
		ConfigurationGeneration: claim.ConfigurationGeneration,
		AuthorizationGeneration: authorizationGeneration,
		Status:                  models.ThreadsPlatformBindingStatusPending,
		ClaimedAt:               claimedAt,
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(binding)
	if result.Error != nil {
		return nil, fmt.Errorf("claim managed Threads binding: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, ErrBindingClaimConflict
	}
	return binding, nil
}

// ReconnectBindingTx advances only the exact immutable claim captured at OAuth
// start. It has no release or transfer branch and never mutates app, subject,
// profile, tenant, integration, or account ownership fields.
func ReconnectBindingTx(tx *gorm.DB, reconnect BindingReconnect) (*models.ThreadsPlatformBinding, error) {
	if tx == nil {
		return nil, ErrStoreUnavailable
	}
	claim := BindingClaim{
		OrganizationID: reconnect.OrganizationID, IntegrationID: reconnect.IntegrationID,
		ChannelAccountID: &reconnect.ChannelAccountID, PlatformAppKey: reconnect.PlatformAppKey,
		PlatformAppID: reconnect.PlatformAppID, OAuthSubjectID: reconnect.OAuthSubjectID,
		AuthorityAssetID:        reconnect.AuthorityAssetID,
		ConfigurationGeneration: reconnect.ConfigurationGeneration,
		AuthorizationGeneration: reconnect.ExpectedAuthorizationGeneration,
	}
	if reconnect.BindingID == uuid.Nil || reconnect.ChannelAccountID == uuid.Nil ||
		reconnect.ExpectedAuthorizationGeneration == 0 || validateBindingClaim(claim) != nil {
		return nil, ErrInvalidBindingClaim
	}
	if err := validateManagedIntegrationTx(
		tx, reconnect.OrganizationID, reconnect.IntegrationID, reconnect.PlatformAppKey,
	); err != nil {
		return nil, err
	}
	if err := validateManagedAccountTx(
		tx, reconnect.OrganizationID, reconnect.ChannelAccountID, reconnect.AuthorityAssetID,
	); err != nil {
		return nil, err
	}
	var binding models.ThreadsPlatformBinding
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND integration_id = ? AND channel_account_id = ?",
		reconnect.BindingID, reconnect.OrganizationID, reconnect.IntegrationID, reconnect.ChannelAccountID,
	).First(&binding).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBindingReconnectFence
		}
		return nil, fmt.Errorf("lock managed Threads reconnect binding: %w", err)
	}
	if binding.PlatformAppKey != reconnect.PlatformAppKey ||
		binding.PlatformAppID != reconnect.PlatformAppID ||
		binding.OAuthSubjectID != reconnect.OAuthSubjectID ||
		binding.AuthorityAssetID != reconnect.AuthorityAssetID ||
		binding.ConfigurationGeneration != reconnect.ConfigurationGeneration ||
		binding.AuthorizationGeneration != reconnect.ExpectedAuthorizationGeneration ||
		(binding.Status != models.ThreadsPlatformBindingStatusPending &&
			binding.Status != models.ThreadsPlatformBindingStatusActive &&
			binding.Status != models.ThreadsPlatformBindingStatusQuarantined) {
		return nil, ErrBindingReconnectFence
	}
	result := tx.Model(&models.ThreadsPlatformBinding{}).Where(
		"id = ? AND organization_id = ? AND authorization_generation = ?",
		binding.ID, reconnect.OrganizationID, reconnect.ExpectedAuthorizationGeneration,
	).Updates(map[string]any{
		"authorization_generation": reconnect.ExpectedAuthorizationGeneration + 1,
		"status":                   models.ThreadsPlatformBindingStatusPending,
	})
	if result.Error != nil {
		return nil, fmt.Errorf("advance managed Threads authorization generation: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, ErrBindingReconnectFence
	}
	binding.AuthorizationGeneration++
	binding.Status = models.ThreadsPlatformBindingStatusPending
	return &binding, nil
}

func validateManagedIntegrationTx(
	tx *gorm.DB,
	organizationID, integrationID uuid.UUID,
	platformAppKey string,
) error {
	var integration models.ProviderIntegration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND provider = ?", integrationID, organizationID, "threads",
	).First(&integration).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrIntegrationNotManaged
		}
		return fmt.Errorf("lock managed Threads integration: %w", err)
	}
	if err := ValidateIntegrationManagement(&integration); err != nil ||
		EffectiveManagementMode(&integration) != models.ThreadsManagementModePlatformManaged ||
		integration.PlatformAppKey == nil || *integration.PlatformAppKey != platformAppKey {
		return ErrIntegrationNotManaged
	}
	return nil
}

func validateManagedAccountTx(
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
	authorityAssetID string,
) error {
	var account models.ChannelAccount
	if err := tx.Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
		accountID, organizationID, models.ChannelThreads, "threads", authorityAssetID,
	).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInvalidBindingClaim
		}
		return fmt.Errorf("validate managed Threads channel account: %w", err)
	}
	return nil
}

// ReleaseBinding is an explicit owner action. There is intentionally no
// transfer method and ClaimBinding never calls ReleaseBinding automatically.
func (store *PostgresStore) ReleaseBinding(
	ctx context.Context,
	organizationID, bindingID uuid.UUID,
	reasonCode string,
) error {
	if store == nil || store.db == nil {
		return ErrStoreUnavailable
	}
	if organizationID == uuid.Nil || bindingID == uuid.Nil || !reasonCodePattern.MatchString(reasonCode) {
		return ErrInvalidBindingClaim
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return database.WithTenant(store.db.WithContext(ctx), organizationID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&models.ThreadsPlatformBinding{}).
			Where(
				"id = ? AND organization_id = ? AND status IN ?",
				bindingID,
				organizationID,
				[]string{
					models.ThreadsPlatformBindingStatusPending,
					models.ThreadsPlatformBindingStatusActive,
					models.ThreadsPlatformBindingStatusQuarantined,
				},
			).
			Updates(map[string]any{
				"status":              models.ThreadsPlatformBindingStatusRevoked,
				"released_at":         now,
				"release_reason_code": reasonCode,
			})
		if result.Error != nil {
			return fmt.Errorf("release managed Threads binding: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrBindingNotFound
		}
		return nil
	})
}

// RecordJournalReceipt inserts a global digest-only receipt idempotently.
func (store *PostgresStore) RecordJournalReceipt(
	ctx context.Context,
	receipt JournalReceipt,
) (bool, error) {
	if store == nil || store.db == nil {
		return false, ErrStoreUnavailable
	}
	if err := validateJournalReceipt(receipt); err != nil {
		return false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	receivedAt := receipt.ReceivedAt.UTC()
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	event := models.ThreadsPlatformEventJournal{
		BaseModel:               models.BaseModel{ID: uuid.New()},
		PlatformAppKey:          receipt.PlatformAppKey,
		PlatformAppID:           receipt.PlatformAppID,
		EventDigest:             receipt.EventDigest,
		EventType:               receipt.EventType,
		SubjectDigest:           receipt.SubjectDigest,
		ConfigurationGeneration: receipt.ConfigurationGeneration,
		RoutingState:            models.ThreadsPlatformRoutingReceived,
		ReceivedAt:              receivedAt,
	}
	result := store.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&event)
	if result.Error != nil {
		return false, fmt.Errorf("record managed Threads journal receipt: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func validateBindingClaim(claim BindingClaim) error {
	if claim.OrganizationID == uuid.Nil || claim.IntegrationID == uuid.Nil ||
		!platformAppKeyPattern.MatchString(claim.PlatformAppKey) ||
		!canonicalNumericID(claim.PlatformAppID) ||
		!canonicalNumericID(claim.OAuthSubjectID) ||
		!canonicalNumericID(claim.AuthorityAssetID) ||
		claim.ConfigurationGeneration == 0 ||
		(claim.ChannelAccountID != nil && *claim.ChannelAccountID == uuid.Nil) {
		return ErrInvalidBindingClaim
	}
	return nil
}

func validateJournalReceipt(receipt JournalReceipt) error {
	if !platformAppKeyPattern.MatchString(receipt.PlatformAppKey) ||
		!canonicalNumericID(receipt.PlatformAppID) ||
		!lowerHexDigestPattern.MatchString(receipt.EventDigest) ||
		!lowerHexDigestPattern.MatchString(receipt.SubjectDigest) ||
		receipt.ConfigurationGeneration == 0 {
		return ErrInvalidJournalReceipt
	}
	switch receipt.EventType {
	case models.ThreadsPlatformEventWebhook,
		models.ThreadsPlatformEventDeauthorization,
		models.ThreadsPlatformEventDataDeletion:
		return nil
	default:
		return ErrInvalidJournalReceipt
	}
}

func canonicalNumericID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
