// Package platformcompliance contains the operator-only control plane used to
// atomically create a dedicated first-party compliance organization and manage
// its social-platform feature markers. It is intentionally separate from
// tenant HTTP handlers and startup mutations.
package platformcompliance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

type Feature string

type Operation string

const (
	FeatureInstagram Feature = "instagram"
	FeatureThreads   Feature = "threads"

	OperationEnable Operation = "enable"
	OperationRemove Operation = "remove"

	// PurposeMarkerKey is deliberately independent from the two feature
	// markers. Only the reviewed migration-owner atomic creator may establish
	// it; a committed ordinary organization can never transition to purpose.
	PurposeMarkerKey   = database.PlatformCompliancePurposeMarkerKey
	InstagramMarkerKey = database.PlatformComplianceInstagramMarkerKey
	ThreadsMarkerKey   = database.PlatformComplianceThreadsMarkerKey

	CommandTimeout    = 30 * time.Second
	LockTimeout       = 5 * time.Second
	StatementTimeout  = 20 * time.Second
	auditResourceType = "platform_compliance"
)

var (
	ErrInvalidOrganizationID       = errors.New("organization ID must be one canonical non-nil UUID")
	ErrInvalidFeature              = errors.New("feature must be instagram or threads")
	ErrDuplicateFeature            = errors.New("each feature may be selected only once per operation")
	ErrConflictingFeatureOperation = errors.New("a feature cannot be enabled and removed in the same operation")
	ErrNoFeatures                  = errors.New("at least one atomic-creation or compliance feature operation is required")
	ErrInvalidOperatorRunID        = errors.New("operator run ID must be 8-96 safe characters")
	ErrInvalidRuntimeRole          = errors.New("database runtime role must be one safe PostgreSQL identifier")
	ErrClinicOrganizationCollision = errors.New("compliance organization is included in a configured clinic scope")
	ErrOrganizationNotFound        = errors.New("compliance organization does not exist")
	ErrOrganizationIdentityUsed    = errors.New("compliance organization UUID already exists or was previously used")
	ErrOrganizationInactive        = errors.New("compliance organization is archived")
	ErrPlatformOwnershipInvalid    = errors.New("compliance organization must belong to the active platform-direct reseller")
	ErrCompliancePurposeRequired   = errors.New("organization is not an atomically created dedicated platform compliance tenant")
	ErrClinicDataFootprint         = errors.New("organization has a clinic, channel, or customer data footprint")
	ErrFootprintValidationFailed   = errors.New("cannot prove the organization has no clinic, channel, or customer data footprint")
	ErrMigrationAuthorityRequired  = errors.New("database.migration_url must use the migration/table-owner authority")
	ErrRemovalStillConfigured      = errors.New("compliance feature removal is blocked while release configuration still names the organization")
	ErrInstagramEvidenceRetained   = errors.New("instagram compliance feature removal is permanently blocked by retained privacy evidence")
	ErrCommitOutcomeIndeterminate  = errors.New("platform compliance apply commit outcome is indeterminate")
	ErrDryRunFinalizationFailed    = errors.New("platform compliance dry-run transaction finalization failed")
	ErrCreateWithRemoval           = errors.New("atomic purpose creation cannot be combined with feature removal")

	operatorRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$`)
	runtimeRolePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// ClinicScope describes deployment-owned clinic routing configuration. An
// allow-all scope may explicitly exclude a separately configured compliance
// organization (managed Threads does this); otherwise allow-all necessarily
// collides with every candidate compliance organization.
type ClinicScope struct {
	Name                    string
	OrganizationIDs         []string
	AllowAll                bool
	ExcludedOrganizationIDs []string
}

// ReleaseReference records a non-secret, feature-specific organization UUID
// from the current deployment configuration. A marker cannot be removed while
// its active release configuration still points at the organization.
type ReleaseReference struct {
	Name           string
	Feature        Feature
	OrganizationID string
}

// Options contains only non-secret operator input and the clinic routing
// scopes derived from the already validated deployment configuration.
type Options struct {
	OrganizationID    string
	CreatePurpose     bool
	Features          []Feature
	RemoveFeatures    []Feature
	OperatorRunID     string
	RuntimeRole       string
	ClinicScopes      []ClinicScope
	ReleaseReferences []ReleaseReference
	Apply             bool
}

type MarkerResult struct {
	Feature   Feature
	Key       string
	Operation Operation
}

type Report struct {
	ApplyRequested   bool
	PurposeCreated   bool
	PurposeUnchanged bool
	Changed          []MarkerResult
	Unchanged        []MarkerResult
	AuditWritten     bool
}

type validatedOptions struct {
	organizationID    uuid.UUID
	createPurpose     bool
	features          []Feature
	removeFeatures    []Feature
	operatorRunID     string
	runtimeRole       string
	clinicScopes      []validatedClinicScope
	releaseReferences []validatedReleaseReference
	apply             bool
}

type validatedClinicScope struct {
	name                    string
	organizationIDs         []uuid.UUID
	allowAll                bool
	excludedOrganizationIDs map[uuid.UUID]struct{}
}

type validatedReleaseReference struct {
	name           string
	feature        Feature
	organizationID uuid.UUID
}

type migrationAuthority struct {
	CurrentUser          string `gorm:"column:current_user_name"`
	CurrentUserOID       int64  `gorm:"column:current_user_oid"`
	SessionUser          string `gorm:"column:session_user_name"`
	AuthenticatedUser    string `gorm:"column:authenticated_user_name"`
	AuthenticatedUserOID int64  `gorm:"column:authenticated_user_oid"`
	BackendRows          int64  `gorm:"column:backend_rows"`
	Superuser            bool   `gorm:"column:superuser"`
	OwnsOrganizations    bool   `gorm:"column:owns_organizations"`
	OwnsAuditLogs        bool   `gorm:"column:owns_audit_logs"`
	OwnsIdentityRegistry bool   `gorm:"column:owns_identity_registry"`
}

type controlPlaneOrganization struct {
	ID         uuid.UUID      `gorm:"column:id"`
	ResellerID *uuid.UUID     `gorm:"column:reseller_id"`
	Name       string         `gorm:"column:name"`
	Slug       string         `gorm:"column:slug"`
	Settings   models.JSONB   `gorm:"column:settings;type:jsonb"`
	DeletedAt  gorm.DeletedAt `gorm:"column:deleted_at"`
}

type controlPlaneReseller struct {
	ID        uuid.UUID      `gorm:"column:id"`
	Slug      string         `gorm:"column:slug"`
	Status    string         `gorm:"column:status"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at"`
}

// ConfiguredClinicScopes extracts only organization routing identifiers. It
// never copies application secrets, tokens, or review evidence from cfg.
func ConfiguredClinicScopes(cfg *config.Config) []ClinicScope {
	if cfg == nil {
		return nil
	}

	threadsExclusions := configuredIDs(cfg.ThreadsManaged.ComplianceOrganizationID)
	instagramActive := append(
		configuredIDs(cfg.MetaInstagram.AllowedOrganizationID),
		configuredIDs(cfg.MetaInstagram.AllowedOrganizationIDs)...,
	)
	return []ClinicScope{
		{
			Name:            "messenger-active",
			OrganizationIDs: configuredIDs(cfg.MetaMessenger.AllowedOrganizationIDs),
			AllowAll:        cfg.MetaMessenger.AllowAllOrganizations,
		},
		{
			Name:            "instagram-active",
			OrganizationIDs: instagramActive,
		},
		{
			Name:            "instagram-quarantined",
			OrganizationIDs: configuredIDs(cfg.MetaInstagram.QuarantinedOrganizationIDs),
		},
		{
			Name:                    "threads-active",
			OrganizationIDs:         configuredIDs(cfg.ThreadsManaged.AllowedOrganizationIDs),
			AllowAll:                cfg.ThreadsManaged.AllowAllOrganizations,
			ExcludedOrganizationIDs: threadsExclusions,
		},
		{
			Name:            "threads-development",
			OrganizationIDs: configuredIDs(cfg.ThreadsAppReview.DevelopmentOrganizationID),
		},
	}
}

// ConfiguredReleaseReferences extracts only feature/organization routing
// identifiers. It intentionally excludes all provider secrets and tokens.
func ConfiguredReleaseReferences(cfg *config.Config) []ReleaseReference {
	if cfg == nil {
		return nil
	}
	references := make([]ReleaseReference, 0, 2)
	// Use the raw validated release field rather than the convenience reader,
	// which deliberately returns an empty string when some other Instagram set
	// invariant is invalid. Removal safety must never turn malformed release
	// configuration into evidence that no reference exists.
	if organizationID := strings.TrimSpace(cfg.MetaInstagram.DataDeletionComplianceOrganizationID); organizationID != "" {
		references = append(references, ReleaseReference{
			Name:           "instagram-data-deletion-compliance",
			Feature:        FeatureInstagram,
			OrganizationID: organizationID,
		})
	}
	if organizationID := strings.TrimSpace(cfg.ThreadsManaged.ComplianceOrganizationID); organizationID != "" {
		references = append(references, ReleaseReference{
			Name:           "threads-managed-compliance",
			Feature:        FeatureThreads,
			OrganizationID: organizationID,
		})
	}
	return references
}

// Bootstrap validates and, when Apply is true, atomically changes only the
// selected marker keys in organizations.settings. Dry runs use a PostgreSQL
// read-only transaction and take no row locks. Apply uses READ COMMITTED with
// explicit row locks so a scan after waiting on a child writer has a fresh
// snapshot; its audit row commits or rolls back with the settings mutation.
func Bootstrap(
	ctx context.Context,
	db *gorm.DB,
	options Options,
) (Report, error) {
	if db == nil {
		return Report{}, errors.New("database connection is required")
	}
	validated, err := validateOptions(options)
	if err != nil {
		return Report{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	boundedContext, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()

	txOptions := &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	}
	if validated.apply {
		txOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	var report Report
	bodyCompleted := false
	err = db.WithContext(boundedContext).Transaction(func(tx *gorm.DB) error {
		// The search path pin must remain the first statement. Every subsequent
		// control-plane query is also public-schema-qualified.
		if pinErr := pinTransaction(tx); pinErr != nil {
			return pinErr
		}
		var runErr error
		report, runErr = bootstrapTransaction(tx, validated)
		if runErr == nil {
			bodyCompleted = true
		}
		return runErr
	}, txOptions)
	if transactionErr := classifyBootstrapTransactionError(err, validated.apply, bodyCompleted); transactionErr != nil {
		return Report{}, transactionErr
	}
	return report, nil
}

func classifyBootstrapTransactionError(err error, apply, bodyCompleted bool) error {
	if err == nil {
		return nil
	}
	if apply && bodyCompleted {
		return ErrCommitOutcomeIndeterminate
	}
	if bodyCompleted {
		return ErrDryRunFinalizationFailed
	}
	return err
}

func pinTransaction(tx *gorm.DB) error {
	if tx == nil {
		return errors.New("compliance bootstrap transaction is required")
	}
	if err := tx.Exec("SET LOCAL search_path = pg_catalog, public").Error; err != nil {
		return fmt.Errorf("pin compliance transaction search path: %w", err)
	}
	if err := tx.Exec("SET LOCAL lock_timeout = '5s'").Error; err != nil {
		return fmt.Errorf("set compliance transaction lock timeout: %w", err)
	}
	if err := tx.Exec("SET LOCAL statement_timeout = '20s'").Error; err != nil {
		return fmt.Errorf("set compliance transaction statement timeout: %w", err)
	}
	return nil
}

func validateOptions(options Options) (validatedOptions, error) {
	rawOrganizationID := options.OrganizationID
	organizationID, err := uuid.Parse(rawOrganizationID)
	if err != nil || organizationID == uuid.Nil || organizationID.String() != rawOrganizationID {
		return validatedOptions{}, ErrInvalidOrganizationID
	}
	if !operatorRunIDPattern.MatchString(options.OperatorRunID) {
		return validatedOptions{}, ErrInvalidOperatorRunID
	}
	if !runtimeRolePattern.MatchString(options.RuntimeRole) {
		return validatedOptions{}, ErrInvalidRuntimeRole
	}
	if !options.CreatePurpose && len(options.Features)+len(options.RemoveFeatures) == 0 {
		return validatedOptions{}, ErrNoFeatures
	}
	if options.CreatePurpose && len(options.RemoveFeatures) != 0 {
		return validatedOptions{}, ErrCreateWithRemoval
	}

	features, err := validateFeatureSet(options.Features)
	if err != nil {
		return validatedOptions{}, err
	}
	removeFeatures, err := validateFeatureSet(options.RemoveFeatures)
	if err != nil {
		return validatedOptions{}, err
	}
	enabled := make(map[Feature]struct{}, len(features))
	for _, feature := range features {
		enabled[feature] = struct{}{}
	}
	for _, feature := range removeFeatures {
		if _, conflict := enabled[feature]; conflict {
			return validatedOptions{}, ErrConflictingFeatureOperation
		}
	}

	clinicScopes := make([]validatedClinicScope, 0, len(options.ClinicScopes))
	for _, scope := range options.ClinicScopes {
		excluded := make(map[uuid.UUID]struct{}, len(scope.ExcludedOrganizationIDs))
		for _, rawID := range scope.ExcludedOrganizationIDs {
			parsed, parseErr := parseConfiguredOrganizationID(rawID)
			if parseErr != nil {
				return validatedOptions{}, fmt.Errorf("invalid %s exclusion: %w", safeScopeName(scope.Name), parseErr)
			}
			excluded[parsed] = struct{}{}
		}
		organizationIDs := make([]uuid.UUID, 0, len(scope.OrganizationIDs))
		for _, rawID := range scope.OrganizationIDs {
			parsed, parseErr := parseConfiguredOrganizationID(rawID)
			if parseErr != nil {
				return validatedOptions{}, fmt.Errorf("invalid %s organization: %w", safeScopeName(scope.Name), parseErr)
			}
			organizationIDs = append(organizationIDs, parsed)
		}
		clinicScopes = append(clinicScopes, validatedClinicScope{
			name:                    safeScopeName(scope.Name),
			organizationIDs:         organizationIDs,
			allowAll:                scope.AllowAll,
			excludedOrganizationIDs: excluded,
		})
	}
	releaseReferences := make([]validatedReleaseReference, 0, len(options.ReleaseReferences))
	for _, reference := range options.ReleaseReferences {
		if _, err := validateFeatureSet([]Feature{reference.Feature}); err != nil {
			return validatedOptions{}, fmt.Errorf("invalid %s release reference: %w", safeScopeName(reference.Name), err)
		}
		parsed, parseErr := parseConfiguredOrganizationID(reference.OrganizationID)
		if parseErr != nil {
			return validatedOptions{}, fmt.Errorf("invalid %s release reference: %w", safeScopeName(reference.Name), parseErr)
		}
		releaseReferences = append(releaseReferences, validatedReleaseReference{
			name: safeScopeName(reference.Name), feature: reference.Feature, organizationID: parsed,
		})
	}

	return validatedOptions{
		organizationID:    organizationID,
		createPurpose:     options.CreatePurpose,
		features:          features,
		removeFeatures:    removeFeatures,
		operatorRunID:     options.OperatorRunID,
		runtimeRole:       options.RuntimeRole,
		clinicScopes:      clinicScopes,
		releaseReferences: releaseReferences,
		apply:             options.Apply,
	}, nil
}

func validateFeatureSet(raw []Feature) ([]Feature, error) {
	seen := make(map[Feature]struct{}, len(raw))
	for _, feature := range raw {
		switch feature {
		case FeatureInstagram, FeatureThreads:
		default:
			return nil, ErrInvalidFeature
		}
		if _, duplicate := seen[feature]; duplicate {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateFeature, feature)
		}
		seen[feature] = struct{}{}
	}
	features := make([]Feature, 0, len(seen))
	for _, feature := range []Feature{FeatureInstagram, FeatureThreads} {
		if _, selected := seen[feature]; selected {
			features = append(features, feature)
		}
	}
	return features, nil
}

func validateClinicSeparation(
	organizationID uuid.UUID,
	clinicScopes []validatedClinicScope,
) error {
	for _, scope := range clinicScopes {
		for _, configuredID := range scope.organizationIDs {
			if configuredID == organizationID {
				return fmt.Errorf("%w: %s", ErrClinicOrganizationCollision, scope.name)
			}
		}
		if scope.allowAll {
			if _, explicitlyExcluded := scope.excludedOrganizationIDs[organizationID]; !explicitlyExcluded {
				return fmt.Errorf("%w: %s allow-all", ErrClinicOrganizationCollision, scope.name)
			}
		}
	}
	return nil
}

func parseConfiguredOrganizationID(raw string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed == uuid.Nil || parsed.String() != raw {
		return uuid.Nil, ErrInvalidOrganizationID
	}
	return parsed, nil
}

func safeScopeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed-clinic-scope"
	}
	name = strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z':
			return character
		case character >= 'A' && character <= 'Z':
			return character
		case character >= '0' && character <= '9':
			return character
		case character == '-', character == '_':
			return character
		default:
			return '-'
		}
	}, name)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func configuredIDs(raw string) []string {
	values := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func bootstrapTransaction(
	tx *gorm.DB,
	options validatedOptions,
) (Report, error) {
	_, err := verifyMigrationAuthority(tx)
	if err != nil {
		return Report{}, err
	}
	if err := database.VerifyPlatformComplianceGuardContract(tx, options.runtimeRole); err != nil {
		return Report{}, fmt.Errorf("verify platform compliance guard contract: %w", err)
	}
	if err := validateClinicSeparation(options.organizationID, options.clinicScopes); err != nil {
		return Report{}, err
	}
	if options.createPurpose {
		return createPurposeTransaction(tx, options)
	}
	return changeFeatureMarkersTransaction(tx, options)
}

func createPurposeTransaction(
	tx *gorm.DB,
	options validatedOptions,
) (Report, error) {
	var identityState struct {
		OrganizationExists bool `gorm:"column:organization_exists"`
		IdentityClaimed    bool `gorm:"column:identity_claimed"`
	}
	if err := tx.Raw(`
		SELECT
			EXISTS (
				SELECT 1 FROM public.organizations WHERE id = ?
			) AS organization_exists,
			EXISTS (
				SELECT 1 FROM public.organization_identity_registry
				WHERE organization_id = ?
			) AS identity_claimed
	`, options.organizationID, options.organizationID).Scan(&identityState).Error; err != nil {
		return Report{}, fmt.Errorf("inspect platform compliance organization identity: %w", err)
	}
	if identityState.OrganizationExists {
		idempotent, report, err := atomicCreationAlreadyApplied(tx, options)
		if err != nil {
			return Report{}, err
		}
		if idempotent {
			return report, nil
		}
		return Report{}, ErrOrganizationIdentityUsed
	}
	if identityState.IdentityClaimed {
		return Report{}, ErrOrganizationIdentityUsed
	}
	if err := validateAtomicPlatformReseller(tx); err != nil {
		return Report{}, err
	}
	source, err := database.FindPlatformComplianceAnyFootprint(tx, options.organizationID)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrFootprintValidationFailed, err)
	}
	if source != "" {
		return Report{}, fmt.Errorf("%w: %s", ErrClinicDataFootprint, source)
	}

	report := Report{ApplyRequested: options.apply, PurposeCreated: true}
	for _, feature := range options.features {
		report.Changed = append(report.Changed, MarkerResult{
			Feature: feature, Key: markerForFeature(feature), Operation: OperationEnable,
		})
	}
	if !options.apply {
		return report, nil
	}
	enableInstagram := containsFeature(options.features, FeatureInstagram)
	enableThreads := containsFeature(options.features, FeatureThreads)
	if err := database.CreatePlatformComplianceOrganization(
		tx, options.organizationID, options.operatorRunID,
		enableInstagram, enableThreads,
	); err != nil {
		return Report{}, err
	}
	report.AuditWritten = true
	return report, nil
}

func atomicCreationAlreadyApplied(
	tx *gorm.DB,
	options validatedOptions,
) (bool, Report, error) {
	purpose, err := database.IsPlatformComplianceOrganization(tx, options.organizationID)
	if err != nil {
		return false, Report{}, fmt.Errorf("inspect existing organization purpose state: %w", err)
	}
	if !purpose {
		return false, Report{}, nil
	}
	if err := database.VerifyPlatformComplianceAtomicCreation(
		tx,
		options.organizationID,
		options.operatorRunID,
		containsFeature(options.features, FeatureInstagram),
		containsFeature(options.features, FeatureThreads),
		options.apply,
	); err != nil {
		if errors.Is(err, database.ErrPlatformComplianceCreationMismatch) {
			return false, Report{}, nil
		}
		return false, Report{}, fmt.Errorf("verify atomic creation idempotence: %w", err)
	}
	report := Report{
		ApplyRequested:   options.apply,
		PurposeUnchanged: true,
	}
	for _, feature := range options.features {
		report.Unchanged = append(report.Unchanged, MarkerResult{
			Feature: feature, Key: markerForFeature(feature), Operation: OperationEnable,
		})
	}
	return true, report, nil
}

func changeFeatureMarkersTransaction(
	tx *gorm.DB,
	options validatedOptions,
) (Report, error) {
	organization, err := loadOrganization(tx, options.organizationID, options.apply)
	if err != nil {
		return Report{}, err
	}
	if err := validatePlatformOwnership(tx, &organization, options.apply); err != nil {
		return Report{}, err
	}
	if !markerEnabled(organization.Settings, PurposeMarkerKey) {
		return Report{}, ErrCompliancePurposeRequired
	}
	if err := validateNoClinicDataFootprint(tx, options.organizationID); err != nil {
		return Report{}, err
	}
	if err := validateFeatureRemovalSafety(tx, options); err != nil {
		return Report{}, err
	}

	report := Report{ApplyRequested: options.apply, PurposeUnchanged: true}
	patch := make(map[string]bool, len(options.features))
	removeKeys := make([]string, 0, len(options.removeFeatures))
	for _, feature := range options.features {
		marker := markerForFeature(feature)
		result := MarkerResult{Feature: feature, Key: marker, Operation: OperationEnable}
		if markerEnabled(organization.Settings, marker) {
			report.Unchanged = append(report.Unchanged, result)
			continue
		}
		report.Changed = append(report.Changed, result)
		patch[marker] = true
	}
	for _, feature := range options.removeFeatures {
		marker := markerForFeature(feature)
		result := MarkerResult{Feature: feature, Key: marker, Operation: OperationRemove}
		if !markerExists(organization.Settings, marker) {
			report.Unchanged = append(report.Unchanged, result)
			continue
		}
		report.Changed = append(report.Changed, result)
		removeKeys = append(removeKeys, marker)
	}
	if !options.apply || len(report.Changed) == 0 {
		return report, nil
	}

	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return Report{}, fmt.Errorf("encode compliance marker patch: %w", err)
	}
	removeJSON, err := json.Marshal(removeKeys)
	if err != nil {
		return Report{}, fmt.Errorf("encode compliance marker removals: %w", err)
	}
	operation := operationSummary(report.Changed)
	if err := database.SetPlatformComplianceOperatorIntent(
		tx, options.operatorRunID, operation,
	); err != nil {
		return Report{}, err
	}
	now := time.Now().UTC()
	updated := tx.Exec(`
		UPDATE public.organizations
		SET settings =
			(COALESCE(settings, '{}'::jsonb) || CAST(? AS jsonb))
			- ARRAY(
				SELECT pg_catalog.jsonb_array_elements_text(CAST(? AS jsonb))
			  ),
			updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`, string(patchJSON), string(removeJSON), now, organization.ID)
	if updated.Error != nil {
		return Report{}, fmt.Errorf("change compliance organization markers: %w", updated.Error)
	}
	if updated.RowsAffected != 1 {
		return Report{}, ErrOrganizationInactive
	}
	// The verified organization trigger emits the exact transition evidence in
	// this transaction. A missing/tampered audit path makes the UPDATE fail.
	report.AuditWritten = true
	return report, nil
}

func validateAtomicPlatformReseller(tx *gorm.DB) error {
	var count int64
	if err := tx.Raw(`
		SELECT pg_catalog.count(*)
		FROM public.resellers
		WHERE slug = 'platform-direct'
		  AND status = 'active'
		  AND deleted_at IS NULL
	`).Scan(&count).Error; err != nil {
		return fmt.Errorf("read active platform-direct reseller: %w", err)
	}
	if count != 1 {
		return ErrPlatformOwnershipInvalid
	}
	return nil
}

func containsFeature(features []Feature, wanted Feature) bool {
	for _, feature := range features {
		if feature == wanted {
			return true
		}
	}
	return false
}

func atomicOrganizationName(organizationID uuid.UUID) string {
	return "Platform Compliance " + organizationID.String()
}

func atomicOrganizationSlug(organizationID uuid.UUID) string {
	return "platform-compliance-" + organizationID.String()
}

func verifyMigrationAuthority(tx *gorm.DB) (migrationAuthority, error) {
	if tx == nil {
		return migrationAuthority{}, ErrMigrationAuthorityRequired
	}
	var authority migrationAuthority
	result := tx.Raw(`
		WITH backend_identity AS (
			SELECT
				activity.usesysid::bigint AS authenticated_user_oid,
				activity.usename::text AS authenticated_user_name
			FROM pg_catalog.pg_stat_activity AS activity
			WHERE activity.pid = pg_catalog.pg_backend_pid()
		)
		SELECT
			current_user::text AS current_user_name,
			role.oid::bigint AS current_user_oid,
			session_user::text AS session_user_name,
			COALESCE((SELECT authenticated_user_name FROM backend_identity), '')
				AS authenticated_user_name,
			COALESCE((SELECT authenticated_user_oid FROM backend_identity), 0)
				AS authenticated_user_oid,
			(SELECT COUNT(*)::bigint FROM backend_identity) AS backend_rows,
			role.rolsuper AS superuser,
			COALESCE((
				SELECT relation.relowner = role.oid
				FROM pg_catalog.pg_class AS relation
				JOIN pg_catalog.pg_namespace AS namespace
				  ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'organizations'
			), false) AS owns_organizations,
			COALESCE((
				SELECT relation.relowner = role.oid
				FROM pg_catalog.pg_class AS relation
				JOIN pg_catalog.pg_namespace AS namespace
				  ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'audit_logs'
			), false) AS owns_audit_logs,
			COALESCE((
				SELECT relation.relowner = role.oid
				FROM pg_catalog.pg_class AS relation
				JOIN pg_catalog.pg_namespace AS namespace
				  ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'organization_identity_registry'
			), false) AS owns_identity_registry
		FROM pg_catalog.pg_roles AS role
		WHERE role.rolname = current_user
	`).Scan(&authority)
	if result.Error != nil {
		return migrationAuthority{}, fmt.Errorf("verify migration authority: %w", result.Error)
	}
	if result.RowsAffected != 1 || !validMigrationAuthority(authority) {
		return migrationAuthority{}, ErrMigrationAuthorityRequired
	}
	return authority, nil
}

func validMigrationAuthority(authority migrationAuthority) bool {
	return authority.CurrentUser != "" && authority.CurrentUserOID != 0 &&
		authority.BackendRows == 1 &&
		authority.AuthenticatedUser == authority.CurrentUser &&
		authority.AuthenticatedUserOID == authority.CurrentUserOID &&
		!authority.Superuser && authority.SessionUser == authority.CurrentUser &&
		authority.OwnsOrganizations &&
		authority.OwnsAuditLogs && authority.OwnsIdentityRegistry
}

func loadOrganization(
	tx *gorm.DB,
	organizationID uuid.UUID,
	lock bool,
) (controlPlaneOrganization, error) {
	locking := ""
	if lock {
		locking = " FOR UPDATE"
	}
	var organization controlPlaneOrganization
	result := tx.Raw(`
		SELECT id, reseller_id, name, slug, settings, deleted_at
		FROM public.organizations
		WHERE id = ?`+locking,
		organizationID,
	).Scan(&organization)
	if result.Error != nil {
		return controlPlaneOrganization{}, fmt.Errorf("read compliance organization: %w", result.Error)
	}
	if result.RowsAffected != 1 || organization.ID == uuid.Nil {
		return controlPlaneOrganization{}, ErrOrganizationNotFound
	}
	if organization.DeletedAt.Valid {
		return controlPlaneOrganization{}, ErrOrganizationInactive
	}
	return organization, nil
}

func validatePlatformOwnership(
	tx *gorm.DB,
	organization *controlPlaneOrganization,
	lock bool,
) error {
	if organization == nil || organization.ResellerID == nil || *organization.ResellerID == uuid.Nil {
		return ErrPlatformOwnershipInvalid
	}
	locking := ""
	if lock {
		locking = " FOR SHARE"
	}
	var reseller controlPlaneReseller
	result := tx.Raw(`
		SELECT id, slug, status, deleted_at
		FROM public.resellers
		WHERE id = ?`+locking,
		*organization.ResellerID,
	).Scan(&reseller)
	if result.Error != nil {
		return fmt.Errorf("read compliance organization owner: %w", result.Error)
	}
	if result.RowsAffected != 1 || reseller.ID == uuid.Nil || reseller.DeletedAt.Valid ||
		reseller.Status != string(models.ResellerStatusActive) ||
		reseller.Slug != database.PlatformResellerSlug {
		return ErrPlatformOwnershipInvalid
	}
	return nil
}

func validateNoClinicDataFootprint(tx *gorm.DB, organizationID uuid.UUID) error {
	if tx == nil {
		return ErrFootprintValidationFailed
	}
	source, err := database.FindPlatformComplianceDisallowedFootprint(tx, organizationID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFootprintValidationFailed, err)
	}
	if source != "" {
		return fmt.Errorf("%w: %s", ErrClinicDataFootprint, source)
	}
	return nil
}

func validateFeatureRemovalSafety(tx *gorm.DB, options validatedOptions) error {
	if len(options.removeFeatures) == 0 {
		return nil
	}
	for _, feature := range options.removeFeatures {
		for _, reference := range options.releaseReferences {
			if reference.feature == feature && reference.organizationID == options.organizationID {
				return fmt.Errorf("%w: %s", ErrRemovalStillConfigured, reference.name)
			}
		}
	}

	removingInstagram := false
	for _, feature := range options.removeFeatures {
		if feature == FeatureInstagram {
			removingInstagram = true
		}
	}
	if !removingInstagram {
		return nil
	}

	// Any admitted Instagram privacy evidence permanently binds the feature
	// marker in this release. These rows are append-only and retained to serve
	// signed status obligations, so a terminal-looking journal is not removal
	// authority. Audit rows are intentionally excluded so a pristine staging
	// organization can still exercise marker removal before its first receipt.
	queries := []struct {
		name  string
		query string
	}{
		{
			name: "meta_instagram_data_deletion_events",
			query: `SELECT EXISTS (
				SELECT 1 FROM public.meta_instagram_data_deletion_events
				WHERE organization_id = ?
			)`,
		},
		{
			name: "privacy_requests",
			query: `SELECT EXISTS (
				SELECT 1 FROM public.privacy_requests
				WHERE organization_id = ?
			)`,
		},
		{
			name: "privacy_request_events",
			query: `SELECT EXISTS (
				SELECT 1 FROM public.privacy_request_events
				WHERE organization_id = ?
			)`,
		},
	}
	for _, obligation := range queries {
		var exists bool
		if err := tx.Raw(obligation.query, options.organizationID).Scan(&exists).Error; err != nil {
			return fmt.Errorf("verify %s removal obligations: %w", obligation.name, err)
		}
		if exists {
			return fmt.Errorf("%w: %s", ErrInstagramEvidenceRetained, obligation.name)
		}
	}
	return nil
}

func operationSummary(changed []MarkerResult) string {
	parts := make([]string, 0, len(changed))
	for _, result := range changed {
		parts = append(parts, string(result.Operation)+":"+string(result.Feature))
	}
	return strings.Join(parts, ",")
}

func markerForFeature(feature Feature) string {
	switch feature {
	case FeatureInstagram:
		return InstagramMarkerKey
	case FeatureThreads:
		return ThreadsMarkerKey
	default:
		return ""
	}
}

func markerExists(settings models.JSONB, marker string) bool {
	_, exists := settings[marker]
	return exists
}

func markerEnabled(settings models.JSONB, marker string) bool {
	value, exists := settings[marker]
	enabled, boolean := value.(bool)
	return exists && boolean && enabled
}
