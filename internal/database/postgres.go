package database

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// NewPostgres creates a new PostgreSQL connection
func NewPostgres(cfg *config.DatabaseConfig, debug bool) (*gorm.DB, error) {
	return NewPostgresWithContext(context.Background(), cfg, debug)
}

// NewPostgresWithContext bounds both connection establishment and the initial
// PostgreSQL health check. It disables GORM's implicit unbounded ping and
// closes the pool if the explicit context-bound ping fails.
func NewPostgresWithContext(ctx context.Context, cfg *config.DatabaseConfig, debug bool) (*gorm.DB, error) {
	if cfg == nil {
		return nil, errors.New("database configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dsn := postgresDSN(cfg)

	logLevel := logger.Silent
	if debug {
		logLevel = logger.Info
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:               logger.Default.LogMode(logLevel),
		DisableAutomaticPing: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get database instance: %w", err)
	}

	// Configure connection pool
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return db, nil
}

func postgresDSN(cfg *config.DatabaseConfig) string {
	if cfg.URL != "" {
		return cfg.URL
	}

	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Name, cfg.SSLMode,
	)
}

// MigrationModel holds model info for migration progress
type MigrationModel struct {
	Name  string
	Model any
}

// GetMigrationModels returns all models to migrate with their names
func GetMigrationModels() []MigrationModel {
	return []MigrationModel{
		// Core models
		{"Reseller", &models.Reseller{}},
		{"Organization", &models.Organization{}},
		{"Permission", &models.Permission{}},
		{"CustomRole", &models.CustomRole{}},
		{"User", &models.User{}},
		{"ResellerMember", &models.ResellerMember{}},
		{"UserOrganization", &models.UserOrganization{}},
		{"Team", &models.Team{}},
		{"TeamMember", &models.TeamMember{}},
		{"APIKey", &models.APIKey{}},
		{"SSOProvider", &models.SSOProvider{}},
		{"Webhook", &models.Webhook{}},
		{"CustomAction", &models.CustomAction{}},
		{"WhatsAppAccount", &models.WhatsAppAccount{}},
		{"WhatsAppCoexistenceState", &models.WhatsAppCoexistenceState{}},
		{"MetaAnalyticsSnapshot", &models.MetaAnalyticsSnapshot{}},
		{"Contact", &models.Contact{}},
		{"WhatsAppIdentityReviewHold", &models.WhatsAppIdentityReviewHold{}},
		{"WhatsAppIdentityReviewMember", &models.WhatsAppIdentityReviewMember{}},
		{"Tag", &models.Tag{}},
		{"Message", &models.Message{}},
		{"Template", &models.Template{}},
		{"WhatsAppFlow", &models.WhatsAppFlow{}},

		// Bulk & Notifications
		{"BulkMessageCampaign", &models.BulkMessageCampaign{}},
		{"BulkMessageRecipient", &models.BulkMessageRecipient{}},
		{"NotificationRule", &models.NotificationRule{}},

		// Chatbot models
		{"ChatbotSettings", &models.ChatbotSettings{}},
		{"KeywordRule", &models.KeywordRule{}},
		{"ChatbotFlow", &models.ChatbotFlow{}},
		// ChatbotFlowStep table is no longer managed by AutoMigrate — the
		// v2 graph runner uses ChatbotFlow.Graph exclusively. The model
		// type is retained only so BackfillChatbotFlowGraph can read
		// existing rows once during startup, then the table can be
		// dropped in a future maintenance migration.
		{"ChatbotSession", &models.ChatbotSession{}},
		{"ChatbotSessionMessage", &models.ChatbotSessionMessage{}},
		{"AIContext", &models.AIContext{}},
		{"AgentTransfer", &models.AgentTransfer{}},

		// User tracking
		{"UserAvailabilityLog", &models.UserAvailabilityLog{}},

		// Canned responses
		{"CannedResponse", &models.CannedResponse{}},

		// Catalogs
		{"Catalog", &models.Catalog{}},
		{"CatalogProduct", &models.CatalogProduct{}},

		// Dashboard
		{"Widget", &models.Widget{}},

		// Conversation Notes
		{"ConversationNote", &models.ConversationNote{}},

		// Calling / IVR
		{"CallLog", &models.CallLog{}},
		{"IVRFlow", &models.IVRFlow{}},
		{"CallTransfer", &models.CallTransfer{}},
		{"CallPermission", &models.CallPermission{}},
		{"AuditLog", &models.AuditLog{}},

		// Commercial control plane
		{"Plan", &models.Plan{}},
		{"PlanPrice", &models.PlanPrice{}},
		{"PlanEntitlement", &models.PlanEntitlement{}},
		{"BillingAccount", &models.BillingAccount{}},
		{"Subscription", &models.Subscription{}},
		{"Invoice", &models.Invoice{}},
		{"BillingWebhookEvent", &models.BillingWebhookEvent{}},
		{"EntitlementOverride", &models.EntitlementOverride{}},
		{"UsageEvent", &models.UsageEvent{}},
		{"BillingUsageRollup", &models.BillingUsageRollup{}},

		// Onboarding and workspace templates
		{"OrganizationOnboarding", &models.OrganizationOnboarding{}},
		{"ProvisioningRun", &models.ProvisioningRun{}},
		{"WorkspaceTemplate", &models.WorkspaceTemplate{}},
		{"WorkspaceTemplateVersion", &models.WorkspaceTemplateVersion{}},
		{"WorkspaceTemplateApplication", &models.WorkspaceTemplateApplication{}},
		{"WorkspaceTemplateResourceMap", &models.WorkspaceTemplateResourceMap{}},

		// Privacy, compliance, and support
		{"ConsentEvent", &models.ConsentEvent{}},
		{"ConsentState", &models.ConsentState{}},
		{"RetentionPolicy", &models.RetentionPolicy{}},
		{"PrivacyRequest", &models.PrivacyRequest{}},
		{"PrivacyRequestEvent", &models.PrivacyRequestEvent{}},
		{"PrivacyJob", &models.PrivacyJob{}},
		{"LegalHold", &models.LegalHold{}},
		{"BreachIncident", &models.BreachIncident{}},
		{"SupportAccessGrant", &models.SupportAccessGrant{}},
		{"SupportCase", &models.SupportCase{}},
		{"RecoveryCheckpoint", &models.RecoveryCheckpoint{}},

		// CRM, scheduling, commerce, and Qwen Copilot
		{"ScheduledJob", &models.ScheduledJob{}},
		{"OutboxEvent", &models.OutboxEvent{}},
		{"CRMPipeline", &models.CRMPipeline{}},
		{"CRMPipelineStage", &models.CRMPipelineStage{}},
		{"CRMLead", &models.CRMLead{}},
		{"CustomerActivityEvent", &models.CustomerActivityEvent{}},
		{"AutomationPolicy", &models.AutomationPolicy{}},
		{"AutomationPolicyVersion", &models.AutomationPolicyVersion{}},
		{"AutomationPolicyActivation", &models.AutomationPolicyActivation{}},
		{"AutomationExecution", &models.AutomationExecution{}},
		{"AutomationExecutionStep", &models.AutomationExecutionStep{}},
		{"AutomationEventReceipt", &models.AutomationEventReceipt{}},
		{"AutomationDispatchState", &models.AutomationDispatchState{}},
		{"CRMStageHistory", &models.CRMStageHistory{}},
		{"FollowUpTask", &models.FollowUpTask{}},
		{"BookingService", &models.BookingService{}},
		{"BookingResource", &models.BookingResource{}},
		{"BookingServiceResource", &models.BookingServiceResource{}},
		{"AvailabilityRule", &models.AvailabilityRule{}},
		{"ResourceTimeOff", &models.ResourceTimeOff{}},
		{"BookingEvent", &models.BookingEvent{}},
		{"Booking", &models.Booking{}},
		{"PackageDefinition", &models.PackageDefinition{}},
		{"PackageEntitlement", &models.PackageEntitlement{}},
		{"ContactPackage", &models.ContactPackage{}},
		{"CreditBalance", &models.CreditBalance{}},
		{"CreditLedgerEntry", &models.CreditLedgerEntry{}},
		{"CommerceInvoice", &models.CommerceInvoice{}},
		{"InvoiceLine", &models.InvoiceLine{}},
		{"PaymentProviderAccount", &models.PaymentProviderAccount{}},
		{"PaymentIntent", &models.PaymentIntent{}},
		{"PaymentTransaction", &models.PaymentTransaction{}},
		{"PaymentWebhookEvent", &models.PaymentWebhookEvent{}},
		{"CopilotSettings", &models.CopilotSettings{}},
		{"CopilotRun", &models.CopilotRun{}},
		{"CopilotFeedback", &models.CopilotFeedback{}},

		// Provider-neutral omnichannel inbox
		{"ProviderIntegration", &models.ProviderIntegration{}},
		{"GoogleSearchConsoleProperty", &models.GoogleSearchConsoleProperty{}},
		{"ChannelAccount", &models.ChannelAccount{}},
		{"ThreadsPlatformBinding", &models.ThreadsPlatformBinding{}},
		{"ThreadsPlatformEventJournal", &models.ThreadsPlatformEventJournal{}},
		{"ChannelCredential", &models.ChannelCredential{}},
		{"MetaDeauthorizationEvent", &models.MetaDeauthorizationEvent{}},
		{"MetaInstagramDataDeletionEvent", &models.MetaInstagramDataDeletionEvent{}},
		{"ContactIdentity", &models.ContactIdentity{}},
		{"InboxConversation", &models.InboxConversation{}},
		{"ConversationParticipant", &models.ConversationParticipant{}},
		{"ConversationRead", &models.ConversationRead{}},
		{"MessagePart", &models.MessagePart{}},
		{"InboundEvent", &models.InboundEvent{}},
		{"MessageEvent", &models.MessageEvent{}},
		{"OutboxJob", &models.OutboxJob{}},
		{"ContactChannelPreference", &models.ContactChannelPreference{}},
	}
}

// AutoMigrate runs auto migration for all models (silent mode)
func AutoMigrate(db *gorm.DB) error {
	return withMigrationSession(db, autoMigrateOnSession)
}

func autoMigrateOnSession(db *gorm.DB) error {
	if err := PrepareProviderIntegrationManagementMode(db); err != nil {
		return err
	}
	if err := PrepareMessageIngestionOrder(db); err != nil {
		return err
	}
	migrationModels := GetMigrationModels()
	for _, m := range migrationModels {
		if m.Name == "MetaInstagramDataDeletionEvent" {
			if err := PrepareMetaInstagramDeletionJournalTenant(db); err != nil {
				return err
			}
		}
		if err := db.AutoMigrate(m.Model); err != nil {
			return err
		}
	}
	if err := InstallMessageIngestionOrderTrigger(db); err != nil {
		return err
	}
	return BackfillProviderIntegrationBindings(db)
}

const migrationAdvisoryLockNamespace int32 = 1380270905

// rlsMigrationPhase is compile-time release authority. It is intentionally
// package-private and is never read from configuration, the environment, a
// command-line flag, or linker input. Each reviewed release source changes the
// literal below as part of its immutable source tree.
type rlsMigrationPhase uint8

const (
	rlsMigrationPhaseBaseline rlsMigrationPhase = iota
	rlsMigrationPhaseBridge
	rlsMigrationPhaseBackend
	rlsMigrationPhaseUI
)

// Release construction exports the same coordinator with exactly one reviewed
// phase literal in each immutable source tree.
const compiledRLSMigrationPhase = rlsMigrationPhaseBridge

type rlsMigrationAction uint8

const (
	rlsMigrationPrepareLegacy rlsMigrationAction = iota
	rlsMigrationPrepareBridge
	rlsMigrationVerifyFuture
)

// legacyRLSCatalogState is a finite release-time state machine. Only an exact
// pre-additive predecessor or an exact published legacy contract with a
// missing additive tenant policy may reach migration code. Query failures and
// every other catalog shape are quarantined before mutation.
type legacyRLSCatalogState uint8

const (
	legacyRLSCatalogQuarantined legacyRLSCatalogState = iota
	legacyRLSCatalogPreAdditive
	legacyRLSCatalogPreparing
	legacyRLSCatalogRepairable
	legacyRLSCatalogComplete
)

const rlsMigrationPrepareClaimPrefix = "prepare:v1:"

// rlsMigrationPreparationLabels binds the durable retry claim to every
// non-model step that can run before tenant-policy publication. Keep this in
// execution order: changing, inserting, or reordering a preparer deliberately
// changes the claim digest and quarantines a claim made by another source.
var rlsMigrationPreparationLabels = []string{
	"PrepareProviderIntegrationManagementMode",
	"PrepareMessageIngestionOrder",
	"PrepareMetaInstagramDeletionJournalTenant",
	"AutoMigrate",
	"InstallMessageIngestionOrderTrigger",
	"BackfillProviderIntegrationBindings",
	"SeedPermissionsAndRoles",
	"SeedSystemRolesForAllOrgs",
	"MigrateUserOrganizations",
	"CreateDefaultAdmin",
	"EnsurePlatformReseller",
	"EnsureReReplyProductCatalog",
	"SeedDefaultWidgets",
	"BackfillLastInboundAt",
}

type rlsMigrationPreparePlan struct {
	Protocol         string   `json:"protocol"`
	Phase            string   `json:"phase"`
	Database         string   `json:"database"`
	RuntimeRole      string   `json:"runtime_role"`
	RuntimeRoleOID   int64    `json:"runtime_role_oid"`
	ModelDescriptors []string `json:"model_descriptors"`
	PreparerLabels   []string `json:"preparer_labels"`
	IndexStatements  []string `json:"index_statements"`
}

func rlsMigrationPhaseLabel(phase rlsMigrationPhase) (string, error) {
	switch phase {
	case rlsMigrationPhaseBaseline:
		return "baseline", nil
	case rlsMigrationPhaseBridge:
		return "bridge", nil
	case rlsMigrationPhaseBackend:
		return "backend", nil
	case rlsMigrationPhaseUI:
		return "ui", nil
	default:
		return "", errors.New("compiled RLS migration phase is invalid")
	}
}

func orderedMigrationModelDescriptors(migrations []MigrationModel) ([]string, error) {
	descriptors := make([]string, 0, len(migrations))
	seenNames := make(map[string]struct{}, len(migrations))
	for index, migration := range migrations {
		if strings.TrimSpace(migration.Name) != migration.Name || migration.Name == "" {
			return nil, fmt.Errorf("migration model %d has an invalid name", index)
		}
		if _, duplicate := seenNames[migration.Name]; duplicate {
			return nil, fmt.Errorf("migration model name %q is duplicated", migration.Name)
		}
		seenNames[migration.Name] = struct{}{}
		modelType := reflect.TypeOf(migration.Model)
		if modelType == nil || modelType.Kind() != reflect.Ptr || modelType.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("migration model %q is not a pointer to a struct", migration.Name)
		}
		modelType = modelType.Elem()
		if modelType.PkgPath() == "" || modelType.Name() == "" {
			return nil, fmt.Errorf("migration model %q has no stable Go type", migration.Name)
		}
		descriptors = append(
			descriptors,
			migration.Name+"="+modelType.PkgPath()+"."+modelType.Name(),
		)
	}
	return descriptors, nil
}

func rlsMigrationPrepareClaimFromPlan(plan rlsMigrationPreparePlan) (string, error) {
	if plan.Protocol != "prepare:v1" || plan.Phase == "" || plan.Database == "" ||
		plan.RuntimeRole == "" || plan.RuntimeRoleOID == 0 ||
		len(plan.ModelDescriptors) == 0 || len(plan.PreparerLabels) == 0 ||
		len(plan.IndexStatements) == 0 {
		return "", errors.New("RLS migration prepare plan is incomplete")
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode RLS migration prepare plan: %w", err)
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%s%x", rlsMigrationPrepareClaimPrefix, digest), nil
}

func rlsMigrationPrepareClaim(
	db *gorm.DB,
	runtimeRole string,
	phase rlsMigrationPhase,
) (string, error) {
	if phase != rlsMigrationPhaseBaseline {
		return "", errors.New("RLS migration prepare claims are valid only in the baseline phase")
	}
	phaseLabel, err := rlsMigrationPhaseLabel(phase)
	if err != nil {
		return "", err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return "", fmt.Errorf("bind prepare claim to runtime role: %w", err)
	}
	var databaseName string
	if err := db.Raw("SELECT pg_catalog.current_database()").Scan(&databaseName).Error; err != nil {
		return "", fmt.Errorf("bind prepare claim to database: %w", err)
	}
	if databaseName == "" {
		return "", errors.New("bind prepare claim to database: current database is empty")
	}
	descriptors, err := orderedMigrationModelDescriptors(GetMigrationModels())
	if err != nil {
		return "", err
	}
	return rlsMigrationPrepareClaimFromPlan(rlsMigrationPreparePlan{
		Protocol:         "prepare:v1",
		Phase:            phaseLabel,
		Database:         databaseName,
		RuntimeRole:      runtimeRole,
		RuntimeRoleOID:   runtimeRoleOID,
		ModelDescriptors: descriptors,
		PreparerLabels:   append([]string(nil), rlsMigrationPreparationLabels...),
		IndexStatements:  append([]string(nil), getIndexes()...),
	})
}

var ErrRLSMigrationCatalogQuarantined = errors.New(
	"RLS migration catalog state is quarantined",
)

var ErrRLSMigrationCommitOutcomeIndeterminate = errors.New(
	"RLS migration commit outcome is indeterminate",
)

func classifyRLSPolicyApplyOutcome(applyErr, reconcileErr error) error {
	if applyErr == nil || reconcileErr == nil {
		return nil
	}
	return errors.Join(
		ErrRLSMigrationCommitOutcomeIndeterminate,
		fmt.Errorf("apply tenant RLS: %w", applyErr),
		fmt.Errorf("read-only policy reconciliation failed: %w", reconcileErr),
	)
}

func classifyRLSActivationOutcome(
	activationErr error,
	postProfile platformComplianceIdentityReviewTriggerProfile,
	profileErr error,
	ownerErr error,
	runtimeErr error,
) error {
	if activationErr == nil {
		return nil
	}
	if profileErr == nil {
		switch postProfile {
		case platformComplianceLegacyIdentityReviewTriggerProfile:
			return fmt.Errorf("activate exact future migration profile: %w", activationErr)
		case platformComplianceFutureIdentityReviewTriggerProfile:
			if ownerErr == nil && runtimeErr == nil {
				return nil
			}
		}
	}
	return errors.Join(
		ErrRLSMigrationCommitOutcomeIndeterminate,
		fmt.Errorf("activate exact future migration profile: %w", activationErr),
		profileErr,
		ownerErr,
		runtimeErr,
	)
}

func decideRLSMigrationAction(
	phase rlsMigrationPhase,
	profile platformComplianceIdentityReviewTriggerProfile,
) (rlsMigrationAction, error) {
	switch profile {
	case platformCompliancePreCursorIdentityReviewTriggerProfile:
		switch phase {
		case rlsMigrationPhaseBaseline:
			return rlsMigrationPrepareLegacy, nil
		case rlsMigrationPhaseBridge:
			return 0, errors.New("bridge migration requires the exact legacy message-cursor profile")
		case rlsMigrationPhaseBackend, rlsMigrationPhaseUI:
			return 0, errors.New("backend and UI migrations require the exact future database profile")
		default:
			return 0, errors.New("compiled RLS migration phase is invalid")
		}
	case platformComplianceLegacyIdentityReviewTriggerProfile:
		switch phase {
		case rlsMigrationPhaseBaseline:
			return rlsMigrationPrepareLegacy, nil
		case rlsMigrationPhaseBridge:
			return rlsMigrationPrepareBridge, nil
		case rlsMigrationPhaseBackend, rlsMigrationPhaseUI:
			return 0, errors.New("backend and UI migrations require the exact future database profile")
		default:
			return 0, errors.New("compiled RLS migration phase is invalid")
		}
	case platformComplianceFutureIdentityReviewTriggerProfile:
		if phase < rlsMigrationPhaseBaseline || phase > rlsMigrationPhaseUI {
			return 0, errors.New("compiled RLS migration phase is invalid")
		}
		return rlsMigrationVerifyFuture, nil
	default:
		return 0, errors.New("database migration profile is invalid")
	}
}

func classifyLegacyRLSCatalog(
	db *gorm.DB,
	runtimeRole string,
	phase rlsMigrationPhase,
) (legacyRLSCatalogState, error) {
	allTables := protectedTenantTableNames()
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(allTables)
	if err != nil {
		return quarantineLegacyRLSCatalog("partition protected tenant tables", err)
	}
	if len(additiveTables) != len(tenantRLSAdditiveProfileV1) {
		return quarantineLegacyRLSCatalog(
			"validate additive tenant table inventory",
			fmt.Errorf("got %d table(s), expected %d", len(additiveTables), len(tenantRLSAdditiveProfileV1)),
		)
	}

	presence, err := readLegacyAdditiveRelationPresence(db, additiveTables)
	if err != nil {
		return quarantineLegacyRLSCatalog("inspect additive tenant relations", err)
	}
	functionCount, err := publicFunctionNameCount(db, "rereply_tenant_policy_additive_fingerprint_v1")
	if err != nil {
		return quarantineLegacyRLSCatalog("inspect additive tenant fingerprint inventory", err)
	}
	functionSource := ""
	if functionCount == 1 {
		var found bool
		functionSource, found, err = publicFunctionSource(
			db,
			tenantAdditivePolicyFingerprintSignature,
		)
		if err != nil {
			return quarantineLegacyRLSCatalog("inspect additive tenant fingerprint source", err)
		}
		if !found {
			return quarantineLegacyRLSCatalog(
				"inspect additive tenant fingerprint signature",
				errors.New("the sole named additive tenant fingerprint has the wrong signature"),
			)
		}
	}
	allAbsent := true
	allPresent := true
	for _, table := range additiveTables {
		allAbsent = allAbsent && !presence[table]
		allPresent = allPresent && presence[table]
	}

	if allAbsent && functionCount == 0 {
		if err := verifyPreAdditiveConstraintNameInventory(db, additiveTables); err != nil {
			return quarantineLegacyRLSCatalog("verify pre-additive constraint name inventory", err)
		}
		if err := verifyPlatformComplianceCoreLegacyGuards(db, runtimeRole); err != nil {
			return quarantineLegacyRLSCatalog("verify pre-additive platform contract", err)
		}
		if err := verifyPlatformComplianceCoreScanAuthority(db); err != nil {
			return quarantineLegacyRLSCatalog("verify pre-additive scan authority", err)
		}
		if _, err := verifyExactLegacyTenantPolicyContract(
			db,
			coreTables,
			nil,
			runtimeRole,
		); err != nil {
			return quarantineLegacyRLSCatalog("verify pre-additive tenant contract", err)
		}
		return legacyRLSCatalogPreAdditive, nil
	}
	if strings.HasPrefix(strings.TrimSpace(functionSource), "SELECT '"+rlsMigrationPrepareClaimPrefix) {
		if functionCount != 1 {
			return quarantineLegacyRLSCatalog(
				"verify RLS migration prepare claim inventory",
				fmt.Errorf("got %d named function(s), expected exactly one", functionCount),
			)
		}
		if phase != rlsMigrationPhaseBaseline {
			return quarantineLegacyRLSCatalog(
				"reject cross-phase RLS migration prepare claim",
				errors.New("a baseline prepare claim cannot authorize another release phase"),
			)
		}
		expectedClaim, err := rlsMigrationPrepareClaim(db, runtimeRole, phase)
		if err != nil {
			return quarantineLegacyRLSCatalog("derive exact RLS migration prepare claim", err)
		}
		if err := verifyLegacyRLSPreparingCatalog(
			db,
			coreTables,
			additiveTables,
			presence,
			runtimeRole,
			expectedClaim,
		); err != nil {
			return quarantineLegacyRLSCatalog("verify claimed RLS migration prefix", err)
		}
		return legacyRLSCatalogPreparing, nil
	}
	if !allPresent || functionCount != 1 {
		return quarantineLegacyRLSCatalog(
			"reject partial additive catalog",
			errors.New("additive relations and fingerprint are not an exact all-present or all-absent set"),
		)
	}
	if err := verifyLegacyAdditiveSchemaContract(db); err != nil {
		return quarantineLegacyRLSCatalog("verify additive schema contract", err)
	}

	if err := verifyPlatformComplianceGuardsWithIdentityReviewRequirement(
		db,
		runtimeRole,
		false,
	); err != nil {
		return quarantineLegacyRLSCatalog("verify published legacy platform contract", err)
	}
	if err := VerifyPlatformComplianceScanAuthority(db); err != nil {
		return quarantineLegacyRLSCatalog("verify published legacy scan authority", err)
	}
	missing, err := verifyExactLegacyTenantPolicyContract(
		db,
		coreTables,
		additiveTables,
		runtimeRole,
	)
	if err != nil {
		return quarantineLegacyRLSCatalog("verify published legacy tenant contract", err)
	}
	if missing > 0 {
		return legacyRLSCatalogRepairable, nil
	}
	return legacyRLSCatalogComplete, nil
}

func verifyPreAdditiveConstraintNameInventory(db *gorm.DB, additiveTables []string) error {
	additive := make(map[string]struct{}, len(additiveTables))
	for _, table := range additiveTables {
		additive[table] = struct{}{}
	}
	names := make([]string, 0, len(coexistenceCheckConstraintTables))
	for _, constraint := range coexistenceCheckConstraintTables {
		if _, absent := additive[constraint.table]; absent {
			names = append(names, constraint.name)
		}
	}
	if len(names) != 12 {
		return fmt.Errorf("pre-additive constraint name inventory has %d entries; expected 12", len(names))
	}
	// The installer checks conname across the whole catalog. Since the intended
	// additive relations are absent, any such name (including a domain or another
	// schema) would suppress installation. Core-table constraint names stay valid.
	var collision bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_constraint
			WHERE conname IN ?
		)
	`, names).Scan(&collision).Error; err != nil {
		return fmt.Errorf("read reserved additive constraint names: %w", err)
	}
	if collision {
		return errors.New("constraint name reserved to an absent additive relation already exists")
	}
	return nil
}

func verifyCompleteLegacyRLSPostcondition(db *gorm.DB, runtimeRole string) error {
	profile, err := detectPlatformComplianceIdentityReviewTriggerProfile(db)
	if err != nil {
		return fmt.Errorf("inspect exact legacy trigger profile: %w", err)
	}
	if profile != platformComplianceLegacyIdentityReviewTriggerProfile {
		return errors.New("completed legacy RLS postcondition unexpectedly contains the future trigger profile")
	}
	if err := verifyLegacyAdditiveSchemaContract(db); err != nil {
		return fmt.Errorf("verify additive schema contract: %w", err)
	}
	if err := verifyPlatformComplianceGuardsWithIdentityReviewRequirement(
		db,
		runtimeRole,
		false,
	); err != nil {
		return fmt.Errorf("verify legacy platform contract: %w", err)
	}
	if err := VerifyPlatformComplianceScanAuthority(db); err != nil {
		return fmt.Errorf("verify legacy platform scan authority: %w", err)
	}
	allTables := protectedTenantTableNames()
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(allTables)
	if err != nil {
		return err
	}
	missing, err := verifyExactLegacyTenantPolicyContract(
		db,
		coreTables,
		additiveTables,
		runtimeRole,
	)
	if err != nil {
		return err
	}
	if missing != 0 {
		return fmt.Errorf("legacy tenant policy contract is missing %d additive policies", missing)
	}
	return nil
}

func verifyFutureRLSPostcondition(db *gorm.DB, runtimeRole string) error {
	if err := verifyLegacyAdditiveSchemaContract(db); err != nil {
		return fmt.Errorf("verify additive schema contract: %w", err)
	}
	if err := verifyPlatformComplianceGuardsWithIdentityReviewRequirement(
		db,
		runtimeRole,
		true,
	); err != nil {
		return fmt.Errorf("verify future platform contract: %w", err)
	}
	if err := VerifyPlatformComplianceScanAuthority(db); err != nil {
		return fmt.Errorf("verify future platform scan authority: %w", err)
	}
	tables := protectedTenantTableNames()
	if err := verifyCanonicalTenantPolicyInventory(
		db,
		tables,
		runtimeRole,
	); err != nil {
		return fmt.Errorf("verify future tenant policy contract: %w", err)
	}
	if err := verifyExactProtectedRuntimeTablePrivileges(db, tables, runtimeRole); err != nil {
		return fmt.Errorf("verify future runtime table privileges: %w", err)
	}
	if err := verifyTenantPolicyFingerprint(db, tables, runtimeRole); err != nil {
		return fmt.Errorf("verify future tenant policy fingerprint: %w", err)
	}
	return nil
}

func quarantineLegacyRLSCatalog(
	operation string,
	cause error,
) (legacyRLSCatalogState, error) {
	if cause == nil {
		cause = errors.New("catalog state is not recognized")
	}
	return legacyRLSCatalogQuarantined, errors.Join(
		ErrRLSMigrationCatalogQuarantined,
		fmt.Errorf("%s: %w", operation, cause),
	)
}

func readLegacyAdditiveRelationPresence(
	db *gorm.DB,
	tables []string,
) (map[string]bool, error) {
	presence := make(map[string]bool, len(tables))
	for _, table := range tables {
		exists, err := publicTableExists(db, table)
		if err != nil {
			return nil, err
		}
		presence[table] = exists
	}
	return presence, nil
}

func publicFunctionNameCount(db *gorm.DB, function string) (int64, error) {
	if err := validateIdentifier(function); err != nil {
		return 0, err
	}
	var count int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		WHERE namespace.nspname = 'public' AND procedure.proname = CAST(? AS text)
	`, function).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func publicFunctionSource(db *gorm.DB, signature string) (string, bool, error) {
	var source string
	result := db.Raw(`
		SELECT procedure.prosrc
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		WHERE procedure.oid = pg_catalog.to_regprocedure(CAST(? AS text))
		  AND namespace.nspname = 'public'
		  AND procedure.prokind = 'f'
	`, signature).Scan(&source)
	if result.Error != nil {
		return "", false, result.Error
	}
	if result.RowsAffected == 0 {
		return "", false, nil
	}
	if result.RowsAffected != 1 {
		return "", false, fmt.Errorf("function signature %q resolved to %d rows", signature, result.RowsAffected)
	}
	return source, true, nil
}

func verifyLegacyRLSPreparingCatalog(
	db *gorm.DB,
	coreTables []string,
	additiveTables []string,
	presence map[string]bool,
	runtimeRole string,
	expectedClaim string,
) error {
	expectedTables := append([]string(nil), coreTables...)
	absentSeen := false
	for _, table := range additiveTables {
		present := presence[table]
		if !present {
			absentSeen = true
			continue
		}
		if absentSeen {
			return fmt.Errorf("additive relation %q is outside the ordered migration prefix", table)
		}
		expectedTables = append(expectedTables, table)
	}
	sort.Strings(expectedTables)
	if err := requireExactExistingProtectedTenantTables(db, expectedTables); err != nil {
		return err
	}
	if err := verifyPlatformComplianceCoreLegacyGuards(db, runtimeRole); err != nil {
		return fmt.Errorf("verify preparing core platform contract: %w", err)
	}
	if err := verifyPlatformComplianceCoreScanAuthority(db); err != nil {
		return fmt.Errorf("verify preparing core scan authority: %w", err)
	}
	_, _, owner, err := verifyExactLegacyTenantCorePolicyContract(db, coreTables, runtimeRole)
	if err != nil {
		return fmt.Errorf("verify preparing core tenant contract: %w", err)
	}
	if _, err := verifyExactLegacyTenantPrepareClaim(
		db,
		expectedClaim,
		runtimeRole,
		&owner,
	); err != nil {
		return err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return fmt.Errorf("inspect preparing runtime role: %w", err)
	}
	if err := verifyRuntimeDefaultTablePrivilegesRevoked(db, owner.OID, runtimeRoleOID); err != nil {
		return err
	}
	for _, table := range additiveTables {
		if !presence[table] {
			continue
		}
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return fmt.Errorf("inspect preparing additive relation %q: %w", table, err)
		}
		if state.RelationKind != "r" || state.Persistence != "p" ||
			!state.OwnerIsCurrent || state.HasHierarchy || !state.SelectPrivilege ||
			state.RowSecurity || state.ForceRowSecurity || state.PolicyCount != 0 {
			return fmt.Errorf("unpublished additive relation %q is not an exact migration prefix", table)
		}
		if err := verifyNoRuntimeAdditiveScaffoldPrivileges(db, table, runtimeRole); err != nil {
			return err
		}
	}
	return nil
}

func verifyExactLegacyTenantPolicyContract(
	db *gorm.DB,
	coreTables []string,
	additiveTables []string,
	runtimeRole string,
) (int, error) {
	expectedTables := append(append([]string(nil), coreTables...), additiveTables...)
	sort.Strings(expectedTables)
	if err := requireExactExistingProtectedTenantTables(db, expectedTables); err != nil {
		return 0, err
	}

	if err := verifyExactProtectedRuntimeTablePrivileges(db, expectedTables, runtimeRole); err != nil {
		return 0, err
	}
	coreRecords, runtimeRoleOID, owner, err := verifyExactLegacyTenantCorePolicyContract(
		db,
		coreTables,
		runtimeRole,
	)
	if err != nil {
		return 0, err
	}

	if len(additiveTables) == 0 {
		return 0, nil
	}
	var reference tenantPolicyFingerprintRecord
	for _, record := range coreRecords {
		if record.Table == "contacts" {
			reference = record
			break
		}
	}
	if reference.Table == "" {
		return 0, errors.New("canonical direct-tenant policy template is missing")
	}
	actual, err := readTenantPolicyFingerprintRecords(db, additiveTables)
	if err != nil {
		return 0, fmt.Errorf("read additive tenant policies: %w", err)
	}
	actualByTable := make(map[string]tenantPolicyFingerprintRecord, len(actual))
	for _, record := range actual {
		if _, duplicate := actualByTable[record.Table]; duplicate {
			return 0, fmt.Errorf("duplicate additive tenant policy record for %q", record.Table)
		}
		actualByTable[record.Table] = record
	}

	candidate := make([]tenantPolicyFingerprintRecord, 0, len(additiveTables))
	missing := 0
	for _, table := range additiveTables {
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return 0, fmt.Errorf("inspect additive tenant policy on %s: %w", table, err)
		}
		record, present := actualByTable[table]
		if err := state.requireExactProtected(table, !present); err != nil {
			return 0, err
		}
		expected := reference
		expected.Table = table
		if present {
			if err := verifyCanonicalTenantPolicyRecord(db, record, table, runtimeRoleOID); err != nil {
				return 0, err
			}
			if err := requireExactDirectTenantPolicyRepresentation(record, reference); err != nil {
				return 0, err
			}
			candidate = append(candidate, record)
			continue
		}
		missing++
		candidate = append(candidate, expected)
	}

	expectedFingerprint, err := tenantPolicyFingerprintFromRecords(
		candidate,
		fmt.Sprintf("v%d:", tenantRLSAdditiveProfileVersion),
	)
	if err != nil {
		return 0, fmt.Errorf("encode additive tenant fingerprint candidate: %w", err)
	}
	if _, err := verifyExactLegacyTenantFingerprintFunction(
		db,
		tenantAdditivePolicyFingerprintSignature,
		"rereply_tenant_policy_additive_fingerprint_v1",
		expectedFingerprint,
		runtimeRole,
		&owner,
		true,
	); err != nil {
		return 0, fmt.Errorf("verify additive tenant fingerprint: %w", err)
	}
	return missing, nil
}

func verifyExactLegacyTenantCorePolicyContract(
	db *gorm.DB,
	coreTables []string,
	runtimeRole string,
) ([]tenantPolicyFingerprintRecord, int64, databaseRoleReference, error) {
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return nil, 0, databaseRoleReference{}, fmt.Errorf("inspect tenant-policy runtime role: %w", err)
	}
	if err := verifyExactProtectedRuntimeTablePrivileges(db, coreTables, runtimeRole); err != nil {
		return nil, 0, databaseRoleReference{}, err
	}
	coreRecords, err := readTenantPolicyFingerprintRecords(db, coreTables)
	if err != nil {
		return nil, 0, databaseRoleReference{}, fmt.Errorf("read legacy core tenant policies: %w", err)
	}
	if len(coreRecords) != len(coreTables) {
		return nil, 0, databaseRoleReference{}, fmt.Errorf(
			"legacy core tenant policy count is %d; expected %d",
			len(coreRecords),
			len(coreTables),
		)
	}
	for index, table := range coreTables {
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return nil, 0, databaseRoleReference{}, fmt.Errorf("inspect core tenant policy on %s: %w", table, err)
		}
		if err := state.requireExactProtected(table, false); err != nil {
			return nil, 0, databaseRoleReference{}, err
		}
		if coreRecords[index].Table != table {
			return nil, 0, databaseRoleReference{}, fmt.Errorf(
				"legacy core tenant policy %d is for %q; expected %q",
				index,
				coreRecords[index].Table,
				table,
			)
		}
		if err := verifyCanonicalTenantPolicyRecord(db, coreRecords[index], table, runtimeRoleOID); err != nil {
			return nil, 0, databaseRoleReference{}, err
		}
	}
	coreFingerprint, err := tenantPolicyFingerprintFromRecords(coreRecords, "")
	if err != nil {
		return nil, 0, databaseRoleReference{}, fmt.Errorf("compute legacy core tenant fingerprint: %w", err)
	}
	owner, err := verifyExactLegacyTenantFingerprintFunction(
		db,
		tenantPolicyFingerprintSignature,
		"rereply_tenant_policy_fingerprint",
		coreFingerprint,
		runtimeRole,
		nil,
		true,
	)
	if err != nil {
		return nil, 0, databaseRoleReference{}, fmt.Errorf("verify legacy core tenant fingerprint: %w", err)
	}
	if err := verifyNoDangerousRuntimeDefaultTablePrivileges(
		db,
		owner.OID,
		runtimeRoleOID,
	); err != nil {
		return nil, 0, databaseRoleReference{}, err
	}
	return coreRecords, runtimeRoleOID, owner, nil
}

func exactDatabaseRoleOID(db *gorm.DB, role string) (int64, error) {
	if err := validateIdentifier(role); err != nil {
		return 0, err
	}
	var state struct {
		Count       int64 `gorm:"column:role_count"`
		OID         int64 `gorm:"column:role_oid"`
		Superuser   bool  `gorm:"column:superuser"`
		CreateRole  bool  `gorm:"column:create_role"`
		BypassRLS   bool  `gorm:"column:bypass_rls"`
		Replication bool  `gorm:"column:replication"`
	}
	if err := db.Raw(`
		SELECT
			COUNT(*) AS role_count,
			COALESCE(pg_catalog.min(oid::bigint), 0) AS role_oid,
			COALESCE(pg_catalog.bool_or(rolsuper), false) AS superuser,
			COALESCE(pg_catalog.bool_or(rolcreaterole), false) AS create_role,
			COALESCE(pg_catalog.bool_or(rolbypassrls), false) AS bypass_rls,
			COALESCE(pg_catalog.bool_or(rolreplication), false) AS replication
		FROM pg_catalog.pg_roles
		WHERE rolname = CAST(? AS text)
	`, role).Scan(&state).Error; err != nil {
		return 0, err
	}
	if state.Count != 1 || state.OID == 0 || state.Superuser ||
		state.CreateRole || state.BypassRLS || state.Replication {
		return 0, fmt.Errorf("database role %q is not exact", role)
	}
	canDisableTriggers, err := roleCanSetSessionReplicationRole(db, state.OID)
	if err != nil {
		return 0, fmt.Errorf("inspect database role %q parameter authority: %w", role, err)
	}
	if canDisableTriggers {
		return 0, fmt.Errorf("database role %q can SET session_replication_role", role)
	}
	return state.OID, nil
}

func verifyProtectedRuntimeTablePrivileges(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
	requireDML bool,
) error {
	if err := validateIdentifier(runtimeRole); err != nil {
		return err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if err := validateIdentifier(table); err != nil {
			return err
		}
		var state struct {
			Count              int64 `gorm:"column:relation_count"`
			SelectPrivilege    bool  `gorm:"column:select_privilege"`
			InsertPrivilege    bool  `gorm:"column:insert_privilege"`
			UpdatePrivilege    bool  `gorm:"column:update_privilege"`
			DeletePrivilege    bool  `gorm:"column:delete_privilege"`
			DangerousACLGrants int64 `gorm:"column:dangerous_acl_grants"`
		}
		if err := db.Raw(`
			WITH relation AS (
				SELECT relation.*
				FROM pg_catalog.pg_class AS relation
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = CAST(? AS text)
				  AND relation.relkind = 'r'
				  AND relation.relpersistence = 'p'
			), dangerous_acl AS (
				SELECT grant_state.privilege_type
				FROM relation
				CROSS JOIN LATERAL pg_catalog.aclexplode(
					COALESCE(relation.relacl, pg_catalog.acldefault('r', relation.relowner))
				) AS grant_state
				WHERE grant_state.privilege_type IN ('TRUNCATE', 'REFERENCES', 'TRIGGER', 'MAINTAIN')
				  AND CASE
					WHEN grant_state.grantee = 0 THEN true
					ELSE pg_catalog.pg_has_role(
						CAST(? AS pg_catalog.oid), grant_state.grantee, 'MEMBER'
					)
				  END
				UNION ALL
				SELECT grant_state.privilege_type
				FROM relation
				JOIN pg_catalog.pg_attribute AS attribute
				  ON attribute.attrelid = relation.oid
				 AND attribute.attnum > 0
				 AND NOT attribute.attisdropped
				 AND attribute.attacl IS NOT NULL
				CROSS JOIN LATERAL pg_catalog.aclexplode(
					attribute.attacl
				) AS grant_state
				WHERE grant_state.privilege_type = 'REFERENCES'
				  AND CASE
					WHEN grant_state.grantee = 0 THEN true
					ELSE pg_catalog.pg_has_role(
						CAST(? AS pg_catalog.oid), grant_state.grantee, 'MEMBER'
					)
				  END
			)
			SELECT
				COUNT(*) AS relation_count,
				COALESCE(pg_catalog.bool_and(
					pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'SELECT')
				), false) AS select_privilege,
				COALESCE(pg_catalog.bool_and(
					pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'INSERT')
				), false) AS insert_privilege,
				COALESCE(pg_catalog.bool_and(
					pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'UPDATE')
				), false) AS update_privilege,
				COALESCE(pg_catalog.bool_and(
					pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'DELETE')
				), false) AS delete_privilege,
				(SELECT COUNT(*) FROM dangerous_acl) AS dangerous_acl_grants
			FROM relation
		`, table, runtimeRoleOID, runtimeRoleOID,
			runtimeRole, runtimeRole, runtimeRole, runtimeRole).Scan(&state).Error; err != nil {
			return fmt.Errorf("inspect runtime table privileges on %q: %w", table, err)
		}
		if state.Count != 1 || state.DangerousACLGrants != 0 {
			return fmt.Errorf("runtime role has dangerous table authority on protected table %q", table)
		}
		if requireDML && (!state.SelectPrivilege || !state.InsertPrivilege ||
			!state.UpdatePrivilege || !state.DeletePrivilege) {
			return fmt.Errorf("runtime DML privileges on protected table %q are not exact", table)
		}
	}
	return nil
}

func verifyExactProtectedRuntimeTablePrivileges(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	return verifyProtectedRuntimeTablePrivileges(db, tables, runtimeRole, true)
}

func verifyNoDangerousProtectedRuntimeTablePrivileges(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	return verifyProtectedRuntimeTablePrivileges(db, tables, runtimeRole, false)
}

func verifyNoDangerousRuntimeDefaultTablePrivileges(
	db *gorm.DB,
	ownerOID int64,
	runtimeRoleOID int64,
) error {
	if ownerOID == 0 || runtimeRoleOID == 0 {
		return errors.New("default table privilege owner binding is missing")
	}
	var dangerous int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_default_acl AS defaults
		CROSS JOIN LATERAL pg_catalog.aclexplode(defaults.defaclacl) AS grant_state
		WHERE defaults.defaclrole = CAST(? AS pg_catalog.oid)
		  AND defaults.defaclobjtype = 'r'
		  AND (
			defaults.defaclnamespace = 0
			OR defaults.defaclnamespace = 'public'::pg_catalog.regnamespace
		  )
		  AND grant_state.privilege_type IN ('TRUNCATE', 'REFERENCES', 'TRIGGER', 'MAINTAIN')
		  AND (
			grant_state.grantee = 0
			OR grant_state.grantee = CAST(? AS pg_catalog.oid)
			OR pg_catalog.pg_has_role(
				CAST(? AS pg_catalog.oid), grant_state.grantee, 'MEMBER'
			)
		  )
	`, ownerOID, runtimeRoleOID, runtimeRoleOID).Scan(&dangerous).Error; err != nil {
		return fmt.Errorf("inspect runtime default table authority: %w", err)
	}
	if dangerous != 0 {
		return fmt.Errorf("runtime role has %d dangerous default table privilege grant(s)", dangerous)
	}
	return nil
}

// roleCanSetSessionReplicationRole rejects PostgreSQL 17 parameter ACLs that
// let a nominally unprivileged runtime role switch to replica mode and bypass
// every ordinary product and foreign-key enforcement trigger. PostgreSQL 14
// predates parameter ACLs, so the PG17-only function must never be parsed on
// that release.
func roleCanSetSessionReplicationRole(db *gorm.DB, roleOID int64) (bool, error) {
	major, err := supportedPostgresMajor(db)
	if err != nil {
		return false, err
	}
	var configured bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_db_role_setting AS role_setting
			CROSS JOIN LATERAL pg_catalog.pg_options_to_table(role_setting.setconfig)
			  AS option(option_name, option_value)
			WHERE role_setting.setrole IN (0::oid, CAST(? AS oid))
			  AND role_setting.setdatabase IN (
				0::oid,
				(SELECT database.oid FROM pg_catalog.pg_database AS database
				 WHERE database.datname = pg_catalog.current_database())
			  )
			  AND option.option_name = 'session_replication_role'
			  AND option.option_value <> 'origin'
		)
	`, roleOID).Scan(&configured).Error; err != nil {
		return false, err
	}
	if configured || major == 14 {
		return configured, nil
	}
	var allowed bool
	if err := db.Raw(`
		WITH RECURSIVE settable_role(role_oid, path, can_set) AS (
			SELECT role.oid, ARRAY[role.oid]::oid[], true
			FROM pg_catalog.pg_roles AS role
			WHERE role.oid = CAST(? AS oid)
			UNION ALL
			SELECT membership.roleid,
				pg_catalog.array_append(settable_role.path, membership.roleid),
				settable_role.can_set AND COALESCE(
					(pg_catalog.to_jsonb(membership)->>'set_option')::boolean,
					true
				)
			FROM settable_role
			JOIN pg_catalog.pg_auth_members AS membership
			  ON membership.member = settable_role.role_oid
			WHERE NOT membership.roleid = ANY(settable_role.path)
		)
		SELECT EXISTS (
			SELECT 1
			FROM settable_role
			WHERE can_set
			  AND (
				pg_catalog.has_parameter_privilege(
					CAST(role_oid AS oid),
					'session_replication_role',
					'SET'
				)
				OR pg_catalog.has_parameter_privilege(
					CAST(role_oid AS oid),
					'session_replication_role',
					'ALTER SYSTEM'
				)
			  )
		)
	`, roleOID).Scan(&allowed).Error; err != nil {
		return false, err
	}
	return allowed, nil
}

func tenantPolicyFingerprintFromRecords(
	records []tenantPolicyFingerprintRecord,
	prefix string,
) (string, error) {
	payload, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%s%x", prefix, digest), nil
}

func requireExactDirectTenantPolicyRepresentation(
	record tenantPolicyFingerprintRecord,
	reference tenantPolicyFingerprintRecord,
) error {
	expected := reference
	expected.Table = record.Table
	if record != expected {
		return fmt.Errorf(
			"additive tenant policy on %q does not match the canonical direct-tenant representation",
			record.Table,
		)
	}
	return nil
}

func verifyCanonicalTenantPolicyRecord(
	db *gorm.DB,
	record tenantPolicyFingerprintRecord,
	table string,
	runtimeRoleOID int64,
) error {
	if err := verifyCanonicalTenantPolicyDependencies(db, table); err != nil {
		return fmt.Errorf("verify tenant policy dependencies on %q: %w", table, err)
	}
	expression, err := canonicalTenantPolicyExpression(table)
	if err != nil {
		return err
	}
	usingExpression, err := canonicalizeTenantPolicyExpression(record.UsingExpression)
	if err != nil {
		return fmt.Errorf("parse tenant policy USING expression on %q: %w", table, err)
	}
	checkExpression, err := canonicalizeTenantPolicyExpression(record.CheckExpression)
	if err != nil {
		return fmt.Errorf("parse tenant policy WITH CHECK expression on %q: %w", table, err)
	}
	expectedRoles := fmt.Sprintf("{%d}", runtimeRoleOID)
	if record.Schema != "public" || record.Table != table ||
		record.Policy != "rereply_tenant_isolation" || record.Command != "*" ||
		!record.Permissive || record.Roles != expectedRoles ||
		usingExpression != expression || checkExpression != expression {
		return fmt.Errorf(
			"tenant policy on %q does not match the code-derived canonical contract (metadata=%t roles=%t using=%t check=%t)",
			table,
			record.Schema == "public" && record.Table == table &&
				record.Policy == "rereply_tenant_isolation" && record.Command == "*" && record.Permissive,
			record.Roles == expectedRoles,
			usingExpression == expression,
			checkExpression == expression,
		)
	}
	return nil
}

func canonicalTenantPolicyRelationNames(table string) ([]string, error) {
	relations := map[string]struct{}{table: {}}
	expression, related := RelatedTenantTables[table]
	if !related {
		return []string{table}, nil
	}
	tokens, err := scanTenantPolicyExpression(expression)
	if err != nil {
		return nil, fmt.Errorf("parse related tenant policy for %q: %w", table, err)
	}
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].kind != 'i' || tokens[index].value != "public" ||
			tokens[index+1].value != "." || tokens[index+2].kind != 'i' {
			continue
		}
		relation := tokens[index+2].value
		if !isCanonicalTenantPolicyRelation(relation) {
			return nil, fmt.Errorf("related tenant policy for %q references unexpected public relation %q", table, relation)
		}
		relations[relation] = struct{}{}
	}
	if len(relations) != 2 {
		return nil, fmt.Errorf("related tenant policy for %q does not identify exactly one parent relation", table)
	}
	result := make([]string, 0, len(relations))
	for relation := range relations {
		result = append(result, relation)
	}
	sort.Strings(result)
	return result, nil
}

func verifyCanonicalTenantPolicyDependencies(db *gorm.DB, table string) error {
	if db == nil {
		return errors.New("database is required")
	}
	if err := validateIdentifier(table); err != nil {
		return err
	}
	relations, err := canonicalTenantPolicyRelationNames(table)
	if err != nil {
		return err
	}
	placeholders := make([]string, len(relations))
	arguments := make([]any, 0, len(relations)+1)
	arguments = append(arguments, table)
	for index, relation := range relations {
		placeholders[index] = "?"
		arguments = append(arguments, relation)
	}
	var state struct {
		PolicyCount                   int64 `gorm:"column:policy_count"`
		ProcedureDependencyCount      int64 `gorm:"column:procedure_dependency_count"`
		CurrentSettingDependencyCount int64 `gorm:"column:current_setting_dependency_count"`
		UnexpectedProcedureCount      int64 `gorm:"column:unexpected_procedure_count"`
		RelationDependencyCount       int64 `gorm:"column:relation_dependency_count"`
		ExpectedRelationCount         int64 `gorm:"column:expected_relation_count"`
		UnexpectedRelationCount       int64 `gorm:"column:unexpected_relation_count"`
		OperatorDependencyCount       int64 `gorm:"column:operator_dependency_count"`
		UnexpectedOperatorCount       int64 `gorm:"column:unexpected_operator_count"`
		UnexpectedTypeCount           int64 `gorm:"column:unexpected_type_count"`
	}
	query := fmt.Sprintf(`
		WITH target_policy AS (
			SELECT policy.oid
			FROM pg_catalog.pg_policy AS policy
			JOIN pg_catalog.pg_class AS relation ON relation.oid = policy.polrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
			  AND policy.polname = 'rereply_tenant_isolation'
		), dependency AS (
			SELECT dependency.*
			FROM pg_catalog.pg_depend AS dependency
			JOIN target_policy ON target_policy.oid = dependency.objid
			WHERE dependency.classid = 'pg_catalog.pg_policy'::pg_catalog.regclass
		)
		SELECT
			(SELECT pg_catalog.count(*) FROM target_policy) AS policy_count,
			pg_catalog.count(DISTINCT procedure.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass)
				AS procedure_dependency_count,
			pg_catalog.count(DISTINCT procedure.oid)
				FILTER (WHERE procedure.oid = pg_catalog.to_regprocedure('pg_catalog.current_setting(text,boolean)'))
				AS current_setting_dependency_count,
			pg_catalog.count(DISTINCT procedure.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
					AND procedure.oid <> pg_catalog.to_regprocedure('pg_catalog.current_setting(text,boolean)'))
				AS unexpected_procedure_count,
			pg_catalog.count(DISTINCT relation.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass)
				AS relation_dependency_count,
			pg_catalog.count(DISTINCT relation.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
					AND relation_namespace.nspname = 'public'
					AND relation.relname IN (%s))
				AS expected_relation_count,
			pg_catalog.count(DISTINCT relation.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
					AND NOT (relation_namespace.nspname = 'public' AND relation.relname IN (%s)))
				AS unexpected_relation_count,
			pg_catalog.count(DISTINCT operator.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass)
				AS operator_dependency_count,
			pg_catalog.count(DISTINCT operator.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
					AND (operator_namespace.nspname <> 'pg_catalog' OR operator.oprname <> '='))
				AS unexpected_operator_count,
			pg_catalog.count(DISTINCT referenced_type.oid)
				FILTER (WHERE dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
					AND NOT (type_namespace.nspname = 'pg_catalog'
						AND referenced_type.typname IN ('bool', 'text', 'uuid')))
				AS unexpected_type_count
		FROM dependency
		LEFT JOIN pg_catalog.pg_proc AS procedure
		  ON dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
		 AND procedure.oid = dependency.refobjid
		LEFT JOIN pg_catalog.pg_class AS relation
		  ON dependency.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
		 AND relation.oid = dependency.refobjid
		LEFT JOIN pg_catalog.pg_namespace AS relation_namespace
		  ON relation_namespace.oid = relation.relnamespace
		LEFT JOIN pg_catalog.pg_operator AS operator
		  ON dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
		 AND operator.oid = dependency.refobjid
		LEFT JOIN pg_catalog.pg_namespace AS operator_namespace
		  ON operator_namespace.oid = operator.oprnamespace
		LEFT JOIN pg_catalog.pg_type AS referenced_type
		  ON dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
		 AND referenced_type.oid = dependency.refobjid
		LEFT JOIN pg_catalog.pg_namespace AS type_namespace
		  ON type_namespace.oid = referenced_type.typnamespace
	`, strings.Join(placeholders, ", "), strings.Join(placeholders, ", "))
	arguments = append(arguments, arguments[1:]...)
	result := db.Raw(query, arguments...).Scan(&state)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 || state.PolicyCount != 1 ||
		state.ProcedureDependencyCount != state.CurrentSettingDependencyCount ||
		state.UnexpectedProcedureCount != 0 ||
		state.RelationDependencyCount != int64(len(relations)) ||
		state.ExpectedRelationCount != int64(len(relations)) ||
		state.UnexpectedRelationCount != 0 ||
		state.UnexpectedOperatorCount != 0 || state.UnexpectedTypeCount != 0 {
		return fmt.Errorf("tenant policy dependency graph is not canonical: %+v", state)
	}
	return nil
}

func canonicalTenantPolicyExpression(table string) (string, error) {
	for _, direct := range DirectTenantTables {
		if direct == table {
			return canonicalizeTenantPolicyExpression(
				"organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid",
			)
		}
	}
	if expression, related := RelatedTenantTables[table]; related {
		return canonicalizeTenantPolicyExpression(expression)
	}
	return "", fmt.Errorf("protected tenant table %q has no canonical policy expression", table)
}

type tenantPolicyExpressionToken struct {
	kind  byte
	value string
}

func canonicalizeTenantPolicyExpression(expression string) (string, error) {
	tokens, err := scanTenantPolicyExpression(expression)
	if err != nil {
		return "", err
	}
	canonical := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.kind == 'i' && token.value == "pg_catalog" &&
			index+2 < len(tokens) && tokens[index+1].value == "." &&
			tokens[index+2].kind == 'i' && tokens[index+2].value == "current_setting" {
			continue
		}
		if token.value == "." && index > 0 && tokens[index-1].kind == 'i' &&
			tokens[index-1].value == "pg_catalog" && index+1 < len(tokens) &&
			tokens[index+1].kind == 'i' && tokens[index+1].value == "current_setting" {
			continue
		}
		if token.kind == 'i' && token.value == "public" &&
			index+2 < len(tokens) && tokens[index+1].value == "." &&
			tokens[index+2].kind == 'i' && isCanonicalTenantPolicyRelation(tokens[index+2].value) &&
			(index+3 >= len(tokens) || tokens[index+3].value != "(") {
			continue
		}
		if token.value == "." && index > 0 && tokens[index-1].kind == 'i' &&
			tokens[index-1].value == "public" && index+1 < len(tokens) &&
			tokens[index+1].kind == 'i' && isCanonicalTenantPolicyRelation(tokens[index+1].value) &&
			(index+2 >= len(tokens) || tokens[index+2].value != "(") {
			continue
		}
		if token.value == "::" && index+1 < len(tokens) &&
			tokens[index+1].kind == 'i' && tokens[index+1].value == "text" &&
			len(canonical) > 0 && strings.HasPrefix(canonical[len(canonical)-1], "s:") {
			index++
			continue
		}
		if token.value == "(" || token.value == ")" {
			continue
		}
		value := token.value
		if token.kind == 'i' && index+1 < len(tokens) && tokens[index+1].value == "(" &&
			(token.value == "current_setting" || token.value == "nullif") {
			value = "call:" + value
		}
		canonical = append(canonical, fmt.Sprintf("%c:%s", token.kind, value))
	}
	return strings.Join(canonical, "\x1f"), nil
}

func isCanonicalTenantPolicyRelation(value string) bool {
	for _, table := range DirectTenantTables {
		if table == value {
			return true
		}
	}
	_, related := RelatedTenantTables[value]
	return related
}

func scanTenantPolicyExpression(expression string) ([]tenantPolicyExpressionToken, error) {
	tokens := make([]tenantPolicyExpressionToken, 0, len(expression)/3)
	for index := 0; index < len(expression); {
		character := expression[index]
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			index++
			continue
		}
		if character == '\'' {
			start := index
			index++
			closed := false
			for index < len(expression) {
				if expression[index] != '\'' {
					index++
					continue
				}
				if index+1 < len(expression) && expression[index+1] == '\'' {
					index += 2
					continue
				}
				index++
				closed = true
				break
			}
			if !closed {
				return nil, errors.New("unterminated SQL string literal")
			}
			tokens = append(tokens, tenantPolicyExpressionToken{kind: 's', value: expression[start:index]})
			continue
		}
		if character == '"' {
			start := index
			index++
			closed := false
			for index < len(expression) {
				if expression[index] != '"' {
					index++
					continue
				}
				if index+1 < len(expression) && expression[index+1] == '"' {
					index += 2
					continue
				}
				index++
				closed = true
				break
			}
			if !closed {
				return nil, errors.New("unterminated quoted SQL identifier")
			}
			tokens = append(tokens, tenantPolicyExpressionToken{kind: 'q', value: expression[start:index]})
			continue
		}
		if (character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') || character == '_' {
			start := index
			index++
			for index < len(expression) {
				next := expression[index]
				if (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') ||
					(next >= '0' && next <= '9') || next == '_' || next == '$' {
					index++
					continue
				}
				break
			}
			tokens = append(tokens, tenantPolicyExpressionToken{
				kind: 'i', value: strings.ToLower(expression[start:index]),
			})
			continue
		}
		if character >= '0' && character <= '9' {
			start := index
			for index < len(expression) && expression[index] >= '0' && expression[index] <= '9' {
				index++
			}
			tokens = append(tokens, tenantPolicyExpressionToken{kind: 'n', value: expression[start:index]})
			continue
		}
		if character == ':' && index+1 < len(expression) && expression[index+1] == ':' {
			tokens = append(tokens, tenantPolicyExpressionToken{kind: 'o', value: "::"})
			index += 2
			continue
		}
		if strings.ContainsRune("(),.=<>!+-*/", rune(character)) {
			tokens = append(tokens, tenantPolicyExpressionToken{kind: 'o', value: string(character)})
			index++
			continue
		}
		return nil, fmt.Errorf("unsupported SQL expression byte 0x%02x", character)
	}
	return tokens, nil
}

func requireExactExistingProtectedTenantTables(db *gorm.DB, expected []string) error {
	actual, err := existingProtectedTenantTables(db)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("protected tenant relation count is %d; expected %d", len(actual), len(expected))
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return fmt.Errorf("protected tenant relation %d is %q; expected %q", index, actual[index], expected[index])
		}
	}
	return nil
}

type legacyAdditiveModelContract struct {
	table           string
	model           any
	requiredColumns []string
	exactColumns    bool
}

var legacyAdditiveModelContracts = []legacyAdditiveModelContract{
	{table: "whatsapp_coexistence_states", model: &models.WhatsAppCoexistenceState{}, exactColumns: true},
	{table: "whatsapp_identity_review_holds", model: &models.WhatsAppIdentityReviewHold{}, exactColumns: true},
	{table: "whatsapp_identity_review_members", model: &models.WhatsAppIdentityReviewMember{}, exactColumns: true},
	{
		table: "whatsapp_accounts", model: &models.WhatsAppAccount{},
		requiredColumns: []string{"id", "organization_id"},
	},
	{
		table: "contacts", model: &models.Contact{},
		requiredColumns: []string{
			"id", "organization_id", "phone_number", "bs_uid", "merged_into_id", "deleted_at",
		},
	},
	{
		table: "organizations", model: &models.Organization{},
		requiredColumns: []string{"id"},
	},
	{
		table: "channel_accounts", model: &models.ChannelAccount{},
		requiredColumns: []string{"id", "organization_id", "channel", "provider"},
	},
	{
		table: "inbox_conversations", model: &models.InboxConversation{},
		requiredColumns: []string{"id", "organization_id", "channel", "channel_account_id"},
	},
	{
		table: "messages", model: &models.Message{},
		requiredColumns: []string{
			"id", "organization_id", "whats_app_message_id", "inbox_conversation_id",
			"metadata", "deleted_at",
		},
	},
	{
		table: "inbound_events", model: &models.InboundEvent{},
		requiredColumns: []string{
			"id", "organization_id", "provider_event_id", "protocol", "channel_account_id", "review_hold_id",
			"event_type", "status", "dedupe_key", "signature_valid", "headers",
			"payload", "processing_started_at", "processed_at", "next_attempt_at",
			"attempt_count", "error_code", "error_message",
		},
	},
}

// PostgreSQL's node-to-SQL deparser adds casts and parentheses that are absent
// from the migration source. Pin the normalized pg_get_expr result for both
// release majors rather than weakening the check with a substring parser. The
// disposable PG17 -> PG14 packet supplies these reviewed digests.
var legacyCheckDefinitionSHA256 = map[int]map[string]string{
	14: {
		"chk_inbound_events_identity_review_shape":    "0468bdecb12a0537f9fcba10cf336e1cb717bf5452b83a5339857079c93fb609",
		"chk_messages_whatsapp_message_id_trimmed":    "e9697ca9e9153d41eced44574886fd03d11f84468f61267e881cb2b511a786e8",
		"chk_whatsapp_coexistence_attempts":           "3cefa6a9a6dd6ab3e0d4f5215d3513844ed63907c5c411df9ce5c5020acb7177",
		"chk_whatsapp_coexistence_contact_status":     "dc0754a22b645c9099c98b323ae3257dcbf48b109a6fbf9d7d887f6f3529001b",
		"chk_whatsapp_coexistence_history_consent":    "fc7a37389ed6560060f83c7153445b41e85bc7f2c00418cd12d63436608fc16c",
		"chk_whatsapp_coexistence_history_progress":   "e792743c4d550001a36b5fe263cf31a2cce40a8f809308d90df46918958c37ff",
		"chk_whatsapp_coexistence_history_status":     "6ed3cd19bffd208b0460a0ccc4cff3ba40f86315b1fde50a7dd1412f7c50aac6",
		"chk_whatsapp_coexistence_lifecycle_status":   "c26e14da8c5f3d2db6107602315d4a8ef83b7b8511e978e5bcb73247f914370b",
		"chk_whatsapp_coexistence_onboarding_status":  "623afda89eafac3c70ac049f0323dd3ce645878d7c8b64436706c69f2b64bf1e",
		"chk_whatsapp_coexistence_sync_status":        "92a434e9b0debaab888d46f0ba8e733f3cc3c9e1d0ae316429d6663eec883076",
		"chk_whatsapp_coexistence_version":            "57cfb848478ead2d6a93c0f28cc4b91c2134a9f0c18e743a625a1279361ae80a",
		"chk_whatsapp_identity_review_hold_authority": "f5cbb77afb507863fb7be8ed77e05aa7cab71816b587e85a79acf1465eedf05d",
		"chk_whatsapp_identity_review_hold_decision":  "567539f21235f83c0b5a8509ccccdd6e15a4a0e921361d03265ad94e141581d1",
		"chk_whatsapp_identity_review_member_reason":  "fdafcdac55ad57abd040d8416c7e669f2c37e5cc775fde3a5bee1247ac4af59f",
	},
	17: {
		"chk_inbound_events_identity_review_shape":    "0468bdecb12a0537f9fcba10cf336e1cb717bf5452b83a5339857079c93fb609",
		"chk_messages_whatsapp_message_id_trimmed":    "e9697ca9e9153d41eced44574886fd03d11f84468f61267e881cb2b511a786e8",
		"chk_whatsapp_coexistence_attempts":           "3cefa6a9a6dd6ab3e0d4f5215d3513844ed63907c5c411df9ce5c5020acb7177",
		"chk_whatsapp_coexistence_contact_status":     "dc0754a22b645c9099c98b323ae3257dcbf48b109a6fbf9d7d887f6f3529001b",
		"chk_whatsapp_coexistence_history_consent":    "fc7a37389ed6560060f83c7153445b41e85bc7f2c00418cd12d63436608fc16c",
		"chk_whatsapp_coexistence_history_progress":   "e792743c4d550001a36b5fe263cf31a2cce40a8f809308d90df46918958c37ff",
		"chk_whatsapp_coexistence_history_status":     "6ed3cd19bffd208b0460a0ccc4cff3ba40f86315b1fde50a7dd1412f7c50aac6",
		"chk_whatsapp_coexistence_lifecycle_status":   "c26e14da8c5f3d2db6107602315d4a8ef83b7b8511e978e5bcb73247f914370b",
		"chk_whatsapp_coexistence_onboarding_status":  "623afda89eafac3c70ac049f0323dd3ce645878d7c8b64436706c69f2b64bf1e",
		"chk_whatsapp_coexistence_sync_status":        "92a434e9b0debaab888d46f0ba8e733f3cc3c9e1d0ae316429d6663eec883076",
		"chk_whatsapp_coexistence_version":            "57cfb848478ead2d6a93c0f28cc4b91c2134a9f0c18e743a625a1279361ae80a",
		"chk_whatsapp_identity_review_hold_authority": "f5cbb77afb507863fb7be8ed77e05aa7cab71816b587e85a79acf1465eedf05d",
		"chk_whatsapp_identity_review_hold_decision":  "567539f21235f83c0b5a8509ccccdd6e15a4a0e921361d03265ad94e141581d1",
		"chk_whatsapp_identity_review_member_reason":  "fdafcdac55ad57abd040d8416c7e669f2c37e5cc775fde3a5bee1247ac4af59f",
	},
}

type legacyAdditiveForeignKeyContract struct {
	name, childTable, childColumns, parentTable, parentColumns, updateAction, deleteAction string
}

var legacyAdditiveForeignKeyContracts = []legacyAdditiveForeignKeyContract{
	{
		name: "fk_whatsapp_coexistence_account_tenant", childTable: "whatsapp_coexistence_states",
		childColumns: "whats_app_account_id,organization_id", parentTable: "whatsapp_accounts",
		parentColumns: "id,organization_id", updateAction: "a", deleteAction: "c",
	},
	{
		name: "fk_whatsapp_coexistence_states_organization", childTable: "whatsapp_coexistence_states",
		childColumns: "organization_id", parentTable: "organizations",
		parentColumns: "id", updateAction: "c", deleteAction: "c",
	},
	{
		name: "fk_whatsapp_coexistence_states_whats_app_account", childTable: "whatsapp_coexistence_states",
		childColumns: "whats_app_account_id", parentTable: "whatsapp_accounts",
		parentColumns: "id", updateAction: "c", deleteAction: "c",
	},
	{
		name: "fk_whatsapp_identity_review_hold_account_tenant", childTable: "whatsapp_identity_review_holds",
		childColumns: "whats_app_account_id,organization_id", parentTable: "whatsapp_accounts",
		parentColumns: "id,organization_id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_whatsapp_identity_review_member_hold_tenant", childTable: "whatsapp_identity_review_members",
		childColumns: "organization_id,hold_id", parentTable: "whatsapp_identity_review_holds",
		parentColumns: "organization_id,id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_whatsapp_identity_review_member_contact_tenant", childTable: "whatsapp_identity_review_members",
		childColumns: "contact_id,organization_id", parentTable: "contacts",
		parentColumns: "id,organization_id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_whatsapp_identity_review_target_member", childTable: "whatsapp_identity_review_holds",
		childColumns: "organization_id,id,decision_target_contact_id", parentTable: "whatsapp_identity_review_members",
		parentColumns: "organization_id,hold_id,contact_id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_whatsapp_identity_review_superseded_tenant", childTable: "whatsapp_identity_review_holds",
		childColumns: "organization_id,superseded_by_hold_id", parentTable: "whatsapp_identity_review_holds",
		parentColumns: "organization_id,id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_inbound_events_account_tenant", childTable: "inbound_events",
		childColumns: "channel_account_id,organization_id", parentTable: "channel_accounts",
		parentColumns: "id,organization_id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_inbound_events_channel_account", childTable: "inbound_events",
		childColumns: "channel_account_id", parentTable: "channel_accounts",
		parentColumns: "id", updateAction: "c", deleteAction: "r",
	},
	{
		name: "fk_inbound_events_identity_review_hold_tenant", childTable: "inbound_events",
		childColumns: "organization_id,review_hold_id", parentTable: "whatsapp_identity_review_holds",
		parentColumns: "organization_id,id", updateAction: "a", deleteAction: "r",
	},
	{
		name: "fk_inbound_events_organization", childTable: "inbound_events",
		childColumns: "organization_id", parentTable: "organizations",
		parentColumns: "id", updateAction: "c", deleteAction: "c",
	},
}

type legacyAdditiveIndexContract struct {
	name, table, columns, predicate, dependencyColumns string
	tableDependencyCount                               int64
	unique                                             bool
}

var legacyAdditiveIndexContracts = []legacyAdditiveIndexContract{
	{
		name: "uq_whatsapp_accounts_id_org", table: "whatsapp_accounts",
		columns: "id,organization_id", dependencyColumns: "id,organization_id", unique: true,
	},
	{
		name: "uq_messages_live_wamid", table: "messages",
		columns:           "organization_id,btrim(whats_app_message_id::text)",
		predicate:         "deleted_at IS NULL AND inbox_conversation_id IS NULL AND btrim(whats_app_message_id::text) <> ''::text",
		dependencyColumns: "deleted_at,inbox_conversation_id,organization_id,whats_app_message_id",
		unique:            true,
	},
	{
		name: "uq_contacts_id_org", table: "contacts",
		columns: "id,organization_id", dependencyColumns: "id,organization_id", unique: true,
	},
	{
		name: "idx_whatsapp_coexistence_org_account", table: "whatsapp_coexistence_states",
		columns: "organization_id,whats_app_account_id", dependencyColumns: "organization_id,whats_app_account_id", unique: true,
	},
	{
		name: "uq_whatsapp_identity_review_holds_org_id", table: "whatsapp_identity_review_holds",
		columns: "organization_id,id", dependencyColumns: "id,organization_id", unique: true,
	},
	{
		name: "uq_whatsapp_identity_review_unsupported_semantic_claim", table: "whatsapp_identity_review_holds",
		columns:           "organization_id,whats_app_account_id,onboarding_cycle,protocol_version,semantic_claim_digest",
		predicate:         "NOT supported",
		dependencyColumns: "onboarding_cycle,organization_id,protocol_version,semantic_claim_digest,supported,whats_app_account_id",
		unique:            true,
	},
	{
		name: "uq_whatsapp_identity_review_generation", table: "whatsapp_identity_review_holds",
		columns:           "organization_id,whats_app_account_id,onboarding_cycle,direct_primary_bsuid,principal_generation",
		predicate:         "supported",
		dependencyColumns: "direct_primary_bsuid,onboarding_cycle,organization_id,principal_generation,supported,whats_app_account_id",
		unique:            true,
	},
	{
		name: "uq_whatsapp_identity_review_decision_request", table: "whatsapp_identity_review_holds",
		columns:           "organization_id,decision_request_id",
		predicate:         "decision_request_id IS NOT NULL AND disposition = 'future_routing'",
		dependencyColumns: "decision_request_id,disposition,organization_id",
		unique:            true,
	},
	{
		name: "idx_whatsapp_identity_review_member_contact", table: "whatsapp_identity_review_members",
		columns: "organization_id,contact_id,hold_id", dependencyColumns: "contact_id,hold_id,organization_id",
	},
	{
		name: "uq_inbound_events_identity_review_wamid", table: "inbound_events",
		columns:           "organization_id,btrim(provider_event_id::text)",
		predicate:         "protocol::text = 'whatsapp_identity_review_v1'::text",
		dependencyColumns: "organization_id,protocol,provider_event_id",
		unique:            true,
	},
	{
		name: "idx_inbound_events_protocol", table: "inbound_events",
		columns: "protocol", dependencyColumns: "protocol",
	},
	{
		name: "idx_inbound_events_review_hold_id", table: "inbound_events",
		columns: "review_hold_id", dependencyColumns: "review_hold_id",
	},
}

func canonicalizeLegacyAdditiveIndexPredicate(expression string) (string, error) {
	tokens, err := scanTenantPolicyExpression(expression)
	if err != nil {
		return "", err
	}
	unqualified := make([]tenantPolicyExpressionToken, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		if tokens[index].kind == 'i' && tokens[index].value == "pg_catalog" &&
			index+2 < len(tokens) && tokens[index+1].value == "." && tokens[index+2].kind == 'i' {
			unqualified = append(unqualified, tokens[index+2])
			index += 2
			continue
		}
		unqualified = append(unqualified, tokens[index])
	}
	tokens = unqualified
	canonical := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.value == "(" || token.value == ")" {
			continue
		}
		if token.value == "::" && index+1 < len(tokens) &&
			tokens[index+1].kind == 'i' && tokens[index+1].value == "text" {
			index++
			continue
		}
		canonical = append(canonical, fmt.Sprintf("%c:%s", token.kind, token.value))
	}
	return strings.Join(canonical, "\x1f"), nil
}

func canonicalLegacyColumnType(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	value = strings.ReplaceAll(value, "varchar(", "character varying(")
	value = strings.ReplaceAll(value, "timestamptz(", "timestamp with time zone(")
	if value == "timestamptz" {
		return "timestamp with time zone"
	}
	return value
}

func trimBalancedOuterParentheses(value string) string {
	for len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' {
		depth := 0
		quoted := false
		encloses := true
		for index := 0; index < len(value); index++ {
			character := value[index]
			if character == '\'' {
				if quoted && index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				quoted = !quoted
				continue
			}
			if quoted {
				continue
			}
			switch character {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && index != len(value)-1 {
					encloses = false
				}
			}
		}
		if !encloses || depth != 0 || quoted {
			break
		}
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func canonicalLegacyColumnDefault(value string) string {
	value = trimBalancedOuterParentheses(strings.TrimSpace(value))
	for _, suffix := range []string{
		"::character varying", "::text", "::jsonb", "::boolean",
		"::smallint", "::integer", "::bigint", "::uuid",
	} {
		if strings.HasSuffix(strings.ToLower(value), suffix) {
			value = strings.TrimSpace(value[:len(value)-len(suffix)])
			value = trimBalancedOuterParentheses(value)
		}
	}
	var result strings.Builder
	quoted := false
	pendingSpace := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\'' {
			if pendingSpace && result.Len() > 0 {
				result.WriteByte(' ')
				pendingSpace = false
			}
			result.WriteByte(character)
			if quoted && index+1 < len(value) && value[index+1] == '\'' {
				result.WriteByte(value[index+1])
				index++
				continue
			}
			quoted = !quoted
			continue
		}
		if !quoted && (character == ' ' || character == '\t' || character == '\r' || character == '\n') {
			pendingSpace = true
			continue
		}
		if pendingSpace && result.Len() > 0 {
			result.WriteByte(' ')
			pendingSpace = false
		}
		if !quoted && character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		result.WriteByte(character)
	}
	return strings.TrimSpace(result.String())
}

func expectedLegacyModelColumn(
	db *gorm.DB,
	fieldName string,
	statement *gorm.Statement,
) (dataType string, notNull bool, defaultValue string, err error) {
	field := statement.Schema.FieldsByDBName[fieldName]
	if field == nil || field.IgnoreMigration {
		return "", false, "", fmt.Errorf("model column %q is unavailable", fieldName)
	}
	fullType := strings.TrimSpace(db.Migrator().FullDataTypeOf(field).SQL)
	upperType := strings.ToUpper(fullType)
	cut := len(fullType)
	for _, marker := range []string{" NOT NULL", " DEFAULT "} {
		if index := strings.Index(upperType, marker); index >= 0 && index < cut {
			cut = index
		}
	}
	dataType = canonicalLegacyColumnType(fullType[:cut])
	notNull = field.NotNull || field.PrimaryKey
	if index := strings.Index(upperType, " DEFAULT "); index >= 0 {
		defaultValue = canonicalLegacyColumnDefault(fullType[index+len(" DEFAULT "):])
	}
	return dataType, notNull, defaultValue, nil
}

func verifyLegacyColumnDefaultDependencies(
	db *gorm.DB,
	defaultOID, relationOID int64,
	attributeNumber int16,
) error {
	if defaultOID == 0 {
		return nil
	}
	var dependencyState struct {
		RelationCount   int64 `gorm:"column:relation_count"`
		ExactCount      int64 `gorm:"column:exact_count"`
		UnexpectedCount int64 `gorm:"column:unexpected_count"`
	}
	if err := db.Raw(`
		WITH dependency AS (
			SELECT dependency.*
			FROM pg_catalog.pg_depend AS dependency
			WHERE dependency.classid = 'pg_catalog.pg_attrdef'::pg_catalog.regclass
			  AND dependency.objid = ?
		), classified AS (
			SELECT
				dependency.*,
				procedure_namespace.nspname AS procedure_namespace,
				operator_namespace.nspname AS operator_namespace,
				type_namespace.nspname AS type_namespace,
				collation_namespace.nspname AS collation_namespace
			FROM dependency
			LEFT JOIN pg_catalog.pg_proc AS procedure
			  ON dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
			 AND procedure.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS procedure_namespace
			  ON procedure_namespace.oid = procedure.pronamespace
			LEFT JOIN pg_catalog.pg_operator AS operator
			  ON dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
			 AND operator.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS operator_namespace
			  ON operator_namespace.oid = operator.oprnamespace
			LEFT JOIN pg_catalog.pg_type AS referenced_type
			  ON dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
			 AND referenced_type.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS type_namespace
			  ON type_namespace.oid = referenced_type.typnamespace
			LEFT JOIN pg_catalog.pg_collation AS collation_state
			  ON dependency.refclassid = 'pg_catalog.pg_collation'::pg_catalog.regclass
			 AND collation_state.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS collation_namespace
			  ON collation_namespace.oid = collation_state.collnamespace
		)
		SELECT
			pg_catalog.count(*) FILTER (
				WHERE refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
			) AS relation_count,
			pg_catalog.count(*) FILTER (
				WHERE refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
				  AND refobjid = ? AND refobjsubid = ? AND deptype = 'a'
			) AS exact_count,
			pg_catalog.count(*) FILTER (WHERE
				CASE
					WHEN refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
						THEN NOT (refobjid = ? AND refobjsubid = ? AND deptype = 'a')
					WHEN refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
						THEN procedure_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
						THEN operator_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
						THEN type_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_collation'::pg_catalog.regclass
						THEN collation_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					ELSE true
				END
			) AS unexpected_count
		FROM classified
	`, defaultOID, relationOID, attributeNumber, relationOID, attributeNumber).Scan(&dependencyState).Error; err != nil {
		return err
	}
	if dependencyState.RelationCount != 1 || dependencyState.ExactCount != 1 ||
		dependencyState.UnexpectedCount != 0 {
		return errors.New("column default dependencies are not exact")
	}
	return nil
}

func verifyLegacyAdditiveModelShape(db *gorm.DB, contract legacyAdditiveModelContract) error {
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(contract.model); err != nil {
		return fmt.Errorf("derive model schema for %q: %w", contract.table, err)
	}
	if statement.Schema == nil || statement.Schema.Table != contract.table {
		return fmt.Errorf("model schema for %q is unavailable", contract.table)
	}
	expectedColumns := append([]string(nil), contract.requiredColumns...)
	if contract.exactColumns {
		expectedColumns = append([]string(nil), statement.Schema.DBNames...)
	}
	sort.Strings(expectedColumns)
	type columnState struct {
		Name            string `gorm:"column:column_name"`
		DataType        string `gorm:"column:data_type"`
		NotNull         bool   `gorm:"column:not_null"`
		DefaultSQL      string `gorm:"column:default_sql"`
		Identity        string `gorm:"column:identity_kind"`
		Generated       string `gorm:"column:generated_kind"`
		ExactCollation  bool   `gorm:"column:exact_collation"`
		ExactType       bool   `gorm:"column:exact_type"`
		DefaultACL      bool   `gorm:"column:default_acl"`
		RelationOID     int64  `gorm:"column:relation_oid"`
		AttributeNumber int16  `gorm:"column:attribute_number"`
		DefaultOID      int64  `gorm:"column:default_oid"`
	}
	allColumns := make([]columnState, 0, len(statement.Schema.DBNames))
	result := db.Raw(`
		SELECT
			attribute.attname AS column_name,
			pg_catalog.format_type(attribute.atttypid, attribute.atttypmod) AS data_type,
			attribute.attnotnull AS not_null,
			COALESCE(pg_catalog.pg_get_expr(default_value.adbin, default_value.adrelid, false), '') AS default_sql,
			attribute.attidentity::text AS identity_kind,
			attribute.attgenerated::text AS generated_kind,
			relation.oid::bigint AS relation_oid,
			attribute.attnum::smallint AS attribute_number,
			COALESCE(default_value.oid, 0)::bigint AS default_oid,
			(type_namespace.nspname = 'pg_catalog' AND data_type.typtype <> 'd') AS exact_type,
			(
				attribute.attcollation = 0 OR (
					collation_namespace.nspname = 'pg_catalog'
					AND collation_state.collname = 'default'
				)
			) AS exact_collation,
			attribute.attacl IS NULL AS default_acl
		FROM pg_catalog.pg_attribute AS attribute
		JOIN pg_catalog.pg_class AS relation ON relation.oid = attribute.attrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		LEFT JOIN pg_catalog.pg_attrdef AS default_value
		  ON default_value.adrelid = attribute.attrelid AND default_value.adnum = attribute.attnum
		JOIN pg_catalog.pg_type AS data_type ON data_type.oid = attribute.atttypid
		JOIN pg_catalog.pg_namespace AS type_namespace ON type_namespace.oid = data_type.typnamespace
		LEFT JOIN pg_catalog.pg_collation AS collation_state ON collation_state.oid = attribute.attcollation
		LEFT JOIN pg_catalog.pg_namespace AS collation_namespace ON collation_namespace.oid = collation_state.collnamespace
		WHERE namespace.nspname = 'public'
		  AND relation.relname = CAST(? AS text)
		  AND relation.relkind = 'r'
		  AND relation.relpersistence = 'p'
		  AND attribute.attnum > 0
		  AND NOT attribute.attisdropped
		ORDER BY attribute.attname
	`, contract.table).Scan(&allColumns)
	if result.Error != nil {
		return fmt.Errorf("read identity-review model columns for %q: %w", contract.table, result.Error)
	}
	actualColumns := allColumns
	if !contract.exactColumns {
		byName := make(map[string]columnState, len(allColumns))
		for _, column := range allColumns {
			byName[column.Name] = column
		}
		actualColumns = make([]columnState, 0, len(expectedColumns))
		for _, name := range expectedColumns {
			if column, ok := byName[name]; ok {
				actualColumns = append(actualColumns, column)
			}
		}
	}
	if len(actualColumns) != len(expectedColumns) {
		return fmt.Errorf("identity-review model %q column count is %d; expected %d", contract.table, len(actualColumns), len(expectedColumns))
	}
	for index := range expectedColumns {
		actual := actualColumns[index]
		if actual.Name != expectedColumns[index] {
			return fmt.Errorf("identity-review model %q column %d is %q; expected %q", contract.table, index, actual.Name, expectedColumns[index])
		}
		expectedType, expectedNotNull, expectedDefault, err := expectedLegacyModelColumn(db, actual.Name, statement)
		if err != nil {
			return fmt.Errorf("derive identity-review model %q column %q: %w", contract.table, actual.Name, err)
		}
		if canonicalLegacyColumnType(actual.DataType) != expectedType ||
			actual.NotNull != expectedNotNull ||
			canonicalLegacyColumnDefault(actual.DefaultSQL) != expectedDefault ||
			actual.Identity != "" || actual.Generated != "" ||
			!actual.ExactType || !actual.ExactCollation || !actual.DefaultACL {
			return fmt.Errorf("identity-review model %q column %q is not exact", contract.table, actual.Name)
		}
		if err := verifyLegacyColumnDefaultDependencies(
			db,
			actual.DefaultOID,
			actual.RelationOID,
			actual.AttributeNumber,
		); err != nil {
			return fmt.Errorf("identity-review model %q column %q default binding is not exact: %w", contract.table, actual.Name, err)
		}
	}
	if !contract.exactColumns {
		return nil
	}

	expectedPrimaryColumns := make([]string, 0, len(statement.Schema.PrimaryFields))
	for _, field := range statement.Schema.PrimaryFields {
		expectedPrimaryColumns = append(expectedPrimaryColumns, field.DBName)
	}
	var primary struct {
		Count   int64  `gorm:"column:primary_count"`
		Columns string `gorm:"column:columns"`
		Valid   bool   `gorm:"column:valid"`
	}
	if err := db.Raw(`
		SELECT
			pg_catalog.count(*) AS primary_count,
			COALESCE(pg_catalog.min(primary_index.columns), '') AS columns,
			COALESCE(pg_catalog.bool_and(primary_index.valid), false) AS valid
		FROM (
			SELECT
				(
					SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY key.ordinality)
					FROM pg_catalog.unnest(index_state.indkey::smallint[])
						WITH ORDINALITY AS key(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute
					  ON attribute.attrelid = relation.oid AND attribute.attnum = key.attnum
				) AS columns,
				index_relation.relkind = 'i' AND index_relation.relpersistence = 'p'
					AND index_relation.reloptions IS NULL AND index_relation.relacl IS NULL
					AND index_namespace.nspname = 'public' AND access_method.amname = 'btree'
					AND index_state.indisunique AND index_state.indisvalid AND index_state.indisready
					AND index_state.indislive AND index_state.indimmediate
					AND index_state.indisprimary AND NOT index_state.indisexclusion
					AND NOT index_state.indisclustered AND NOT index_state.indisreplident
					AND index_state.indexprs IS NULL AND index_state.indpred IS NULL
					AND index_state.indnkeyatts = index_state.indnatts
					AND NOT COALESCE((pg_catalog.to_jsonb(index_state)->>'indnullsnotdistinct')::boolean, false)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(index_state.indoption::smallint[]) AS option(value)
						WHERE option.value <> 0
					)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(index_state.indclass::oid[]) AS class(oid)
						JOIN pg_catalog.pg_opclass AS operator_class ON operator_class.oid = class.oid
						JOIN pg_catalog.pg_namespace AS class_namespace ON class_namespace.oid = operator_class.opcnamespace
						WHERE NOT operator_class.opcdefault OR class_namespace.nspname <> 'pg_catalog'
					)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(index_state.indcollation::oid[]) AS index_collation(oid)
						LEFT JOIN pg_catalog.pg_collation AS resolved ON resolved.oid = index_collation.oid
						LEFT JOIN pg_catalog.pg_namespace AS resolved_namespace ON resolved_namespace.oid = resolved.collnamespace
						WHERE index_collation.oid <> 0
						  AND (resolved_namespace.nspname <> 'pg_catalog' OR resolved.collname <> 'default')
					)
					AND primary_constraint.contype = 'p' AND primary_constraint.convalidated
					AND primary_constraint.conislocal AND primary_constraint.coninhcount = 0
					AND primary_constraint.conparentid = 0
					AND NOT primary_constraint.condeferrable AND NOT primary_constraint.condeferred
					AND primary_constraint.connoinherit AS valid
			FROM pg_catalog.pg_index AS index_state
			JOIN pg_catalog.pg_class AS relation ON relation.oid = index_state.indrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			JOIN pg_catalog.pg_class AS index_relation ON index_relation.oid = index_state.indexrelid
			JOIN pg_catalog.pg_namespace AS index_namespace ON index_namespace.oid = index_relation.relnamespace
			JOIN pg_catalog.pg_am AS access_method ON access_method.oid = index_relation.relam
			JOIN pg_catalog.pg_constraint AS primary_constraint
			  ON primary_constraint.conrelid = relation.oid
			 AND primary_constraint.conindid = index_state.indexrelid
			WHERE namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
			  AND index_state.indisprimary
		) AS primary_index
	`, contract.table).Scan(&primary).Error; err != nil {
		return fmt.Errorf("read identity-review model primary key for %q: %w", contract.table, err)
	}
	if primary.Count != 1 || !primary.Valid || primary.Columns != strings.Join(expectedPrimaryColumns, ",") {
		return fmt.Errorf(
			"identity-review model %q primary key is not exact (count=%d valid=%t columns=%q expected=%q)",
			contract.table,
			primary.Count,
			primary.Valid,
			primary.Columns,
			strings.Join(expectedPrimaryColumns, ","),
		)
	}
	return nil
}

func supportedPostgresMajor(db *gorm.DB) (int, error) {
	var version int
	if err := db.Raw(
		"SELECT pg_catalog.current_setting('server_version_num')::integer / 10000",
	).Scan(&version).Error; err != nil {
		return 0, err
	}
	if version != 14 && version != 17 {
		return 0, fmt.Errorf("PostgreSQL major %d is not release-authorized", version)
	}
	return version, nil
}

func canonicalSchemaDefinition(value string) string {
	const catalogQualifier = "pg_catalog."
	var result strings.Builder
	result.Grow(len(value))
	inSingleQuote := false
	inDoubleQuote := false
	escapeString := false
	pendingSpace := false
	identifierByte := func(value byte) bool {
		return value == '_' || value == '$' || value >= '0' && value <= '9' ||
			value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
	}
	writePendingSpace := func() {
		if pendingSpace && result.Len() > 0 {
			result.WriteByte(' ')
		}
		pendingSpace = false
	}

	for index := 0; index < len(value); {
		character := value[index]
		if inSingleQuote {
			result.WriteByte(character)
			index++
			if escapeString && character == '\\' && index < len(value) {
				result.WriteByte(value[index])
				index++
				continue
			}
			if character == '\'' {
				if index < len(value) && value[index] == '\'' {
					result.WriteByte(value[index])
					index++
					continue
				}
				inSingleQuote = false
				escapeString = false
			}
			continue
		}
		if inDoubleQuote {
			result.WriteByte(character)
			index++
			if character == '"' {
				if index < len(value) && value[index] == '"' {
					result.WriteByte(value[index])
					index++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}

		switch character {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			pendingSpace = true
			index++
			continue
		case '\'':
			writePendingSpace()
			inSingleQuote = true
			escapeString = index > 0 && (value[index-1] == 'E' || value[index-1] == 'e') &&
				(index == 1 || !identifierByte(value[index-2]))
			result.WriteByte(character)
			index++
			continue
		case '"':
			writePendingSpace()
			inDoubleQuote = true
			result.WriteByte(character)
			index++
			continue
		}

		writePendingSpace()
		if strings.HasPrefix(value[index:], catalogQualifier) &&
			(index == 0 || !identifierByte(value[index-1])) &&
			index+len(catalogQualifier) < len(value) &&
			(identifierByte(value[index+len(catalogQualifier)]) || value[index+len(catalogQualifier)] == '"') {
			index += len(catalogQualifier)
			continue
		}
		result.WriteByte(character)
		index++
	}
	return result.String()
}

func schemaDefinitionSHA256(value string) string {
	digest := sha256.Sum256([]byte(canonicalSchemaDefinition(value)))
	return fmt.Sprintf("%x", digest)
}

func isLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func verifyLegacyCheckDefinitionManifest(major int) error {
	definitions, ok := legacyCheckDefinitionSHA256[major]
	if !ok {
		return fmt.Errorf("PostgreSQL major %d has no check-constraint digest manifest", major)
	}
	expected := make(map[string]struct{}, len(coexistenceCheckConstraintTables))
	for _, constraint := range coexistenceCheckConstraintTables {
		if _, duplicate := expected[constraint.name]; duplicate {
			return fmt.Errorf("duplicate expected identity-review check constraint %q", constraint.name)
		}
		expected[constraint.name] = struct{}{}
		digest, present := definitions[constraint.name]
		if !present || !isLowerHexSHA256(digest) {
			return fmt.Errorf("identity-review check constraint %q has no exact PostgreSQL %d digest", constraint.name, major)
		}
	}
	if len(definitions) != len(expected) {
		return fmt.Errorf("PostgreSQL %d check-constraint digest count is %d; expected %d", major, len(definitions), len(expected))
	}
	for name := range definitions {
		if _, present := expected[name]; !present {
			return fmt.Errorf("PostgreSQL %d check-constraint digest manifest contains unexpected name %q", major, name)
		}
	}
	return nil
}

func verifyLegacyCheckConstraint(db *gorm.DB, major int, table, name string) error {
	var state struct {
		Count      int64  `gorm:"column:constraint_count"`
		Expression string `gorm:"column:expression"`
		Valid      bool   `gorm:"column:valid"`
		Dependency bool   `gorm:"column:dependencies_are_pinned"`
	}
	if err := db.Raw(`
		SELECT
			pg_catalog.count(*) AS constraint_count,
			COALESCE(pg_catalog.min(pg_catalog.pg_get_expr(constraint_state.conbin, constraint_state.conrelid, false)), '') AS expression,
			COALESCE(pg_catalog.bool_and(
				constraint_state.contype = 'c' AND constraint_state.convalidated
				AND constraint_state.conislocal AND constraint_state.coninhcount = 0
				AND constraint_state.conparentid = 0
				AND NOT constraint_state.condeferrable AND NOT constraint_state.condeferred
				AND NOT constraint_state.connoinherit
			), false) AS valid,
			NOT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_constraint AS checked
				JOIN pg_catalog.pg_depend AS dependency
				  ON dependency.classid = 'pg_catalog.pg_constraint'::pg_catalog.regclass
				 AND dependency.objid = checked.oid
				LEFT JOIN pg_catalog.pg_proc AS procedure
				  ON dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
				 AND procedure.oid = dependency.refobjid
				LEFT JOIN pg_catalog.pg_namespace AS procedure_namespace
				  ON procedure_namespace.oid = procedure.pronamespace
				LEFT JOIN pg_catalog.pg_operator AS operator
				  ON dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
				 AND operator.oid = dependency.refobjid
				LEFT JOIN pg_catalog.pg_namespace AS operator_namespace
				  ON operator_namespace.oid = operator.oprnamespace
				LEFT JOIN pg_catalog.pg_type AS referenced_type
				  ON dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
				 AND referenced_type.oid = dependency.refobjid
				LEFT JOIN pg_catalog.pg_namespace AS type_namespace
				  ON type_namespace.oid = referenced_type.typnamespace
				WHERE checked.conrelid = relation.oid
				  AND checked.conname = CAST(? AS text)
				  AND (
					(dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
					 AND procedure_namespace.nspname <> 'pg_catalog')
					OR (dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
					 AND operator_namespace.nspname <> 'pg_catalog')
					OR (dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
					 AND type_namespace.nspname <> 'pg_catalog')
				  )
			) AS dependencies_are_pinned
		FROM pg_catalog.pg_constraint AS constraint_state
		JOIN pg_catalog.pg_class AS relation ON relation.oid = constraint_state.conrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public'
		  AND relation.relname = CAST(? AS text)
		  AND constraint_state.conname = CAST(? AS text)
		GROUP BY relation.oid
	`, name, table, name).Scan(&state).Error; err != nil {
		return fmt.Errorf("read identity-review check constraint %q: %w", name, err)
	}
	expected := legacyCheckDefinitionSHA256[major][name]
	actual := schemaDefinitionSHA256(state.Expression)
	if state.Count != 1 || !state.Valid || !state.Dependency || !isLowerHexSHA256(expected) || actual != expected {
		return fmt.Errorf("identity-review check constraint %q is not exact (definition sha256 %s)", name, actual)
	}
	return nil
}

func verifyLegacyAdditiveForeignKey(db *gorm.DB, contract legacyAdditiveForeignKeyContract) error {
	var state struct {
		Count              int64  `gorm:"column:constraint_count"`
		ConstraintOID      int64  `gorm:"column:constraint_oid"`
		ChildRelationOID   int64  `gorm:"column:child_relation_oid"`
		ParentRelationOID  int64  `gorm:"column:parent_relation_oid"`
		ReferencedIndexOID int64  `gorm:"column:referenced_index_oid"`
		ChildColumns       string `gorm:"column:child_columns"`
		ParentTable        string `gorm:"column:parent_table"`
		ParentColumns      string `gorm:"column:parent_columns"`
		DeleteAction       string `gorm:"column:delete_action"`
		Valid              bool   `gorm:"column:valid"`
	}
	if err := db.Raw(`
		SELECT
			pg_catalog.count(*) AS constraint_count,
			COALESCE(pg_catalog.min(candidate.constraint_oid), 0) AS constraint_oid,
			COALESCE(pg_catalog.min(candidate.child_relation_oid), 0) AS child_relation_oid,
			COALESCE(pg_catalog.min(candidate.parent_relation_oid), 0) AS parent_relation_oid,
			COALESCE(pg_catalog.min(candidate.referenced_index_oid), 0) AS referenced_index_oid,
			COALESCE(pg_catalog.min(candidate.child_columns), '') AS child_columns,
			COALESCE(pg_catalog.min(candidate.parent_table), '') AS parent_table,
			COALESCE(pg_catalog.min(candidate.parent_columns), '') AS parent_columns,
			COALESCE(pg_catalog.min(candidate.delete_action), '') AS delete_action,
			COALESCE(pg_catalog.bool_and(candidate.valid), false) AS valid
		FROM (
			SELECT
				constraint_state.oid::bigint AS constraint_oid,
				constraint_state.conrelid::bigint AS child_relation_oid,
				constraint_state.confrelid::bigint AS parent_relation_oid,
				constraint_state.conindid::bigint AS referenced_index_oid,
				(SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY key.ordinality)
				 FROM pg_catalog.unnest(constraint_state.conkey) WITH ORDINALITY AS key(attnum, ordinality)
				 JOIN pg_catalog.pg_attribute AS attribute
				   ON attribute.attrelid = constraint_state.conrelid AND attribute.attnum = key.attnum) AS child_columns,
				parent_relation.relname::text AS parent_table,
				(SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY key.ordinality)
				 FROM pg_catalog.unnest(constraint_state.confkey) WITH ORDINALITY AS key(attnum, ordinality)
				 JOIN pg_catalog.pg_attribute AS attribute
				   ON attribute.attrelid = constraint_state.confrelid AND attribute.attnum = key.attnum) AS parent_columns,
				constraint_state.confdeltype::text AS delete_action,
				constraint_state.contype = 'f' AND constraint_state.convalidated
					AND NOT constraint_state.condeferrable AND NOT constraint_state.condeferred
					AND constraint_state.connoinherit
					AND constraint_state.confmatchtype = 's'
					AND constraint_state.confupdtype::text = CAST(? AS text)
					AND constraint_state.conislocal AND constraint_state.coninhcount = 0
					AND constraint_state.conparentid = 0
					AND constraint_state.conindid <> 0
					AND pg_catalog.cardinality(constraint_state.conpfeqop) = pg_catalog.cardinality(constraint_state.conkey)
					AND pg_catalog.cardinality(constraint_state.conppeqop) = pg_catalog.cardinality(constraint_state.conkey)
					AND pg_catalog.cardinality(constraint_state.conffeqop) = pg_catalog.cardinality(constraint_state.conkey)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(
							constraint_state.conpfeqop || constraint_state.conppeqop || constraint_state.conffeqop
						) AS equality(operator_oid)
						LEFT JOIN pg_catalog.pg_operator AS operator ON operator.oid = equality.operator_oid
						LEFT JOIN pg_catalog.pg_namespace AS operator_namespace ON operator_namespace.oid = operator.oprnamespace
						WHERE operator.oprname IS DISTINCT FROM '='
						   OR operator_namespace.nspname IS DISTINCT FROM 'pg_catalog'
					)
					AND EXISTS (
						SELECT 1
						FROM pg_catalog.pg_index AS referenced_index
						JOIN pg_catalog.pg_class AS referenced_index_relation
						  ON referenced_index_relation.oid = referenced_index.indexrelid
						JOIN pg_catalog.pg_namespace AS referenced_index_namespace
						  ON referenced_index_namespace.oid = referenced_index_relation.relnamespace
						JOIN pg_catalog.pg_am AS referenced_access_method
						  ON referenced_access_method.oid = referenced_index_relation.relam
						WHERE referenced_index.indexrelid = constraint_state.conindid
						  AND referenced_index.indrelid = constraint_state.confrelid
						  AND (
							SELECT pg_catalog.array_agg(key.attnum ORDER BY key.ordinality)
							FROM pg_catalog.unnest(referenced_index.indkey::smallint[])
								WITH ORDINALITY AS key(attnum, ordinality)
							WHERE key.ordinality <= referenced_index.indnkeyatts
						  ) = constraint_state.confkey
						  AND referenced_index.indnkeyatts = pg_catalog.cardinality(constraint_state.confkey)
						  AND referenced_index.indnatts = referenced_index.indnkeyatts
						  AND referenced_index.indisunique AND referenced_index.indisvalid
						  AND referenced_index.indisready AND referenced_index.indislive
						  AND referenced_index.indimmediate AND NOT referenced_index.indisexclusion
						  AND NOT referenced_index.indisclustered AND NOT referenced_index.indisreplident
						  AND referenced_index.indexprs IS NULL AND referenced_index.indpred IS NULL
						  AND NOT COALESCE((pg_catalog.to_jsonb(referenced_index)->>'indnullsnotdistinct')::boolean, false)
						  AND referenced_index_relation.relkind = 'i'
						  AND referenced_index_relation.relpersistence = 'p'
						  AND referenced_index_relation.reloptions IS NULL
						  AND referenced_index_relation.relacl IS NULL
						  AND referenced_index_namespace.nspname = 'public'
						  AND referenced_access_method.amname = 'btree'
						  AND NOT EXISTS (
							SELECT 1
							FROM pg_catalog.unnest(referenced_index.indoption::smallint[]) AS option(value)
							WHERE option.value <> 0
						  )
						  AND NOT EXISTS (
							SELECT 1
							FROM pg_catalog.unnest(referenced_index.indclass::oid[]) AS class(oid)
							JOIN pg_catalog.pg_opclass AS operator_class ON operator_class.oid = class.oid
							JOIN pg_catalog.pg_namespace AS class_namespace ON class_namespace.oid = operator_class.opcnamespace
							WHERE NOT operator_class.opcdefault OR class_namespace.nspname <> 'pg_catalog'
						  )
						  AND NOT EXISTS (
							SELECT 1
							FROM pg_catalog.unnest(referenced_index.indcollation::oid[]) AS index_collation(oid)
							LEFT JOIN pg_catalog.pg_collation AS resolved ON resolved.oid = index_collation.oid
							LEFT JOIN pg_catalog.pg_namespace AS resolved_namespace ON resolved_namespace.oid = resolved.collnamespace
							WHERE index_collation.oid <> 0
							  AND (resolved_namespace.nspname <> 'pg_catalog' OR resolved.collname <> 'default')
						  )
					)
					AND child_namespace.nspname = 'public' AND parent_namespace.nspname = 'public' AS valid
			FROM pg_catalog.pg_constraint AS constraint_state
			JOIN pg_catalog.pg_class AS child_relation ON child_relation.oid = constraint_state.conrelid
			JOIN pg_catalog.pg_namespace AS child_namespace ON child_namespace.oid = child_relation.relnamespace
			JOIN pg_catalog.pg_class AS parent_relation ON parent_relation.oid = constraint_state.confrelid
			JOIN pg_catalog.pg_namespace AS parent_namespace ON parent_namespace.oid = parent_relation.relnamespace
			WHERE child_namespace.nspname = 'public'
			  AND child_relation.relname = CAST(? AS text)
			  AND constraint_state.conname = CAST(? AS text)
		) AS candidate
	`, contract.updateAction, contract.childTable, contract.name).Scan(&state).Error; err != nil {
		return fmt.Errorf("read additive foreign key %q: %w", contract.name, err)
	}
	if state.Count != 1 || !state.Valid || state.ChildColumns != contract.childColumns ||
		state.ParentTable != contract.parentTable || state.ParentColumns != contract.parentColumns ||
		state.DeleteAction != contract.deleteAction || contract.updateAction == "" {
		return fmt.Errorf("additive foreign key %q is not exact", contract.name)
	}
	if err := verifyLegacyAdditiveForeignKeyTriggers(
		db,
		contract,
		state.ConstraintOID,
		state.ChildRelationOID,
		state.ParentRelationOID,
		state.ReferencedIndexOID,
	); err != nil {
		return err
	}
	return nil
}

func verifyLegacyAdditiveForeignKeyInventory(db *gorm.DB) error {
	tableSet := make(map[string]struct{})
	expected := make([]string, 0, len(legacyAdditiveForeignKeyContracts))
	for _, contract := range legacyAdditiveForeignKeyContracts {
		tableSet[contract.childTable] = struct{}{}
		expected = append(expected, contract.childTable+"\x00"+contract.name)
	}
	tables := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	sort.Strings(expected)
	placeholders := make([]string, len(tables))
	arguments := make([]any, len(tables))
	for index, table := range tables {
		placeholders[index] = "?"
		arguments[index] = table
	}
	var records []struct {
		ChildTable string `gorm:"column:child_table"`
		Name       string `gorm:"column:constraint_name"`
	}
	query := fmt.Sprintf(`
		SELECT
			child_relation.relname::text AS child_table,
			constraint_state.conname::text AS constraint_name
		FROM pg_catalog.pg_constraint AS constraint_state
		JOIN pg_catalog.pg_class AS child_relation ON child_relation.oid = constraint_state.conrelid
		JOIN pg_catalog.pg_namespace AS child_namespace ON child_namespace.oid = child_relation.relnamespace
		WHERE child_namespace.nspname = 'public'
		  AND child_relation.relname IN (%s)
		  AND constraint_state.contype = 'f'
		ORDER BY child_relation.relname, constraint_state.conname
	`, strings.Join(placeholders, ", "))
	if err := db.Raw(query, arguments...).Scan(&records).Error; err != nil {
		return fmt.Errorf("read identity-review foreign-key inventory: %w", err)
	}
	actual := make([]string, 0, len(records))
	for _, record := range records {
		actual = append(actual, record.ChildTable+"\x00"+record.Name)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf(
			"identity-review foreign-key inventory is not exact (got %d, expected %d)",
			len(actual),
			len(expected),
		)
	}
	return nil
}

func verifyLegacyAdditiveForeignKeyTriggers(
	db *gorm.DB,
	contract legacyAdditiveForeignKeyContract,
	constraintOID, childRelationOID, parentRelationOID, referencedIndexOID int64,
) error {
	type triggerState struct {
		RelationOID           int64  `gorm:"column:relation_oid"`
		ConstraintRelationOID int64  `gorm:"column:constraint_relation_oid"`
		TypeBits              int    `gorm:"column:type_bits"`
		FunctionNamespace     string `gorm:"column:function_namespace"`
		FunctionName          string `gorm:"column:function_name"`
		MetadataExact         bool   `gorm:"column:metadata_exact"`
	}
	var triggers []triggerState
	if err := db.Raw(`
		SELECT
			trigger.tgrelid::bigint AS relation_oid,
			trigger.tgconstrrelid::bigint AS constraint_relation_oid,
			trigger.tgtype::integer AS type_bits,
			function_namespace.nspname::text AS function_namespace,
			function.proname::text AS function_name,
			trigger.tgisinternal
				AND trigger.tgenabled = 'O'
				AND trigger.tgparentid = 0
				AND trigger.tgnargs = 0
				AND pg_catalog.octet_length(trigger.tgargs) = 0
				AND COALESCE(pg_catalog.cardinality(trigger.tgattr::smallint[]), 0) = 0
				AND trigger.tgqual IS NULL
				AND trigger.tgoldtable IS NULL
				AND trigger.tgnewtable IS NULL
				AND NOT trigger.tgdeferrable
				AND NOT trigger.tginitdeferred
				AND trigger.tgconstrindid = CAST(? AS pg_catalog.oid)
				AS metadata_exact
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_proc AS function ON function.oid = trigger.tgfoid
		JOIN pg_catalog.pg_namespace AS function_namespace ON function_namespace.oid = function.pronamespace
		WHERE trigger.tgconstraint = CAST(? AS pg_catalog.oid)
	`, referencedIndexOID, constraintOID).Scan(&triggers).Error; err != nil {
		return fmt.Errorf("read additive foreign key %q enforcement triggers: %w", contract.name, err)
	}

	deleteFunction := map[string]string{
		"a": "RI_FKey_noaction_del",
		"c": "RI_FKey_cascade_del",
		"r": "RI_FKey_restrict_del",
	}[contract.deleteAction]
	if deleteFunction == "" {
		return fmt.Errorf("additive foreign key %q has unsupported delete action %q", contract.name, contract.deleteAction)
	}
	updateFunction := map[string]string{
		"a": "RI_FKey_noaction_upd",
		"c": "RI_FKey_cascade_upd",
		"r": "RI_FKey_restrict_upd",
	}[contract.updateAction]
	if updateFunction == "" {
		return fmt.Errorf("additive foreign key %q has unsupported update action %q", contract.name, contract.updateAction)
	}
	type triggerIdentity struct {
		relationOID, constraintRelationOID int64
		typeBits                           int
		functionNamespace, functionName    string
	}
	expected := map[triggerIdentity]int{
		{childRelationOID, parentRelationOID, 5, "pg_catalog", "RI_FKey_check_ins"}:  1,
		{childRelationOID, parentRelationOID, 17, "pg_catalog", "RI_FKey_check_upd"}: 1,
		{parentRelationOID, childRelationOID, 9, "pg_catalog", deleteFunction}:       1,
		{parentRelationOID, childRelationOID, 17, "pg_catalog", updateFunction}:      1,
	}
	actual := make(map[triggerIdentity]int, len(triggers))
	for _, trigger := range triggers {
		if !trigger.MetadataExact {
			return fmt.Errorf("additive foreign key %q has a non-canonical enforcement trigger", contract.name)
		}
		actual[triggerIdentity{
			trigger.RelationOID,
			trigger.ConstraintRelationOID,
			trigger.TypeBits,
			trigger.FunctionNamespace,
			trigger.FunctionName,
		}]++
	}
	if len(triggers) != 4 || !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("additive foreign key %q enforcement trigger inventory is not exact", contract.name)
	}
	return nil
}

func verifyLegacyIndexDependencies(
	db *gorm.DB,
	indexOID, tableOID int64,
	expectedColumns string,
	expectedTableDependencyCount int64,
) error {
	var state struct {
		RelationCount        int64  `gorm:"column:relation_count"`
		TableDependencyCount int64  `gorm:"column:table_dependency_count"`
		DependencyFields     string `gorm:"column:dependency_fields"`
		UnexpectedCount      int64  `gorm:"column:unexpected_count"`
	}
	if err := db.Raw(`
		WITH dependency AS (
			SELECT dependency.*
			FROM pg_catalog.pg_depend AS dependency
			WHERE dependency.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND dependency.objid = ?
		), classified AS (
			SELECT
				dependency.*,
				attribute.attname AS relation_attribute,
				procedure_namespace.nspname AS procedure_namespace,
				operator_namespace.nspname AS operator_namespace,
				type_namespace.nspname AS type_namespace,
				collation_namespace.nspname AS collation_namespace,
				opclass_namespace.nspname AS opclass_namespace
			FROM dependency
			LEFT JOIN pg_catalog.pg_attribute AS attribute
			  ON dependency.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
			 AND attribute.attrelid = dependency.refobjid
			 AND attribute.attnum = dependency.refobjsubid
			LEFT JOIN pg_catalog.pg_proc AS procedure
			  ON dependency.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
			 AND procedure.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS procedure_namespace
			  ON procedure_namespace.oid = procedure.pronamespace
			LEFT JOIN pg_catalog.pg_operator AS operator
			  ON dependency.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
			 AND operator.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS operator_namespace
			  ON operator_namespace.oid = operator.oprnamespace
			LEFT JOIN pg_catalog.pg_type AS referenced_type
			  ON dependency.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
			 AND referenced_type.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS type_namespace
			  ON type_namespace.oid = referenced_type.typnamespace
			LEFT JOIN pg_catalog.pg_collation AS collation_state
			  ON dependency.refclassid = 'pg_catalog.pg_collation'::pg_catalog.regclass
			 AND collation_state.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS collation_namespace
			  ON collation_namespace.oid = collation_state.collnamespace
			LEFT JOIN pg_catalog.pg_opclass AS operator_class
			  ON dependency.refclassid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
			 AND operator_class.oid = dependency.refobjid
			LEFT JOIN pg_catalog.pg_namespace AS opclass_namespace
			  ON opclass_namespace.oid = operator_class.opcnamespace
		)
		SELECT
			pg_catalog.count(DISTINCT refobjsubid) FILTER (
				WHERE refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
				  AND refobjid = ? AND refobjsubid > 0 AND deptype = 'a'
			) AS relation_count,
			pg_catalog.count(*) FILTER (
				WHERE refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
				  AND refobjid = ? AND refobjsubid = 0 AND deptype = 'a'
			) AS table_dependency_count,
			COALESCE(pg_catalog.string_agg(
				DISTINCT relation_attribute,
				',' ORDER BY relation_attribute
			) FILTER (
				WHERE refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
				  AND refobjid = ? AND refobjsubid > 0 AND deptype = 'a'
			), '') AS dependency_fields,
			pg_catalog.count(*) FILTER (WHERE
				CASE
					WHEN refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
						THEN NOT (refobjid = ? AND deptype = 'a' AND (
							refobjsubid = 0 OR (refobjsubid > 0 AND relation_attribute IS NOT NULL)
						))
					WHEN refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
						THEN procedure_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
						THEN operator_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
						THEN type_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_collation'::pg_catalog.regclass
						THEN collation_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					WHEN refclassid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
						THEN opclass_namespace IS DISTINCT FROM 'pg_catalog' OR deptype <> 'n'
					ELSE true
				END
			) AS unexpected_count
		FROM classified
	`, indexOID, tableOID, tableOID, tableOID, tableOID).Scan(&state).Error; err != nil {
		return err
	}
	expectedCount := int64(0)
	if expectedColumns != "" {
		expectedCount = int64(len(strings.Split(expectedColumns, ",")))
	}
	if state.RelationCount != expectedCount ||
		state.TableDependencyCount != expectedTableDependencyCount ||
		state.DependencyFields != expectedColumns ||
		state.UnexpectedCount != 0 {
		return fmt.Errorf(
			"index dependencies are not exact (relation_count=%d expected_count=%d table_dependencies=%d expected_table_dependencies=%d fields=%q expected_fields=%q unexpected=%d)",
			state.RelationCount,
			expectedCount,
			state.TableDependencyCount,
			expectedTableDependencyCount,
			state.DependencyFields,
			expectedColumns,
			state.UnexpectedCount,
		)
	}
	return nil
}

func verifyLegacyAdditiveIndex(db *gorm.DB, contract legacyAdditiveIndexContract) error {
	var state struct {
		IndexOID  int64  `gorm:"column:index_oid"`
		TableOID  int64  `gorm:"column:table_oid"`
		Columns   string `gorm:"column:columns"`
		Predicate string `gorm:"column:predicate"`
		Unique    bool   `gorm:"column:unique_index"`
		Valid     bool   `gorm:"column:valid"`
	}
	result := db.Raw(`
		SELECT
			index_relation.oid::bigint AS index_oid,
			table_relation.oid::bigint AS table_oid,
			COALESCE((
				SELECT pg_catalog.string_agg(
					CASE WHEN key.attnum = 0
						THEN pg_catalog.pg_get_indexdef(index_state.indexrelid, key.ordinality::integer, true)
						ELSE attribute.attname
					END,
					',' ORDER BY key.ordinality
				)
				FROM pg_catalog.unnest(index_state.indkey::smallint[]) WITH ORDINALITY AS key(attnum, ordinality)
				LEFT JOIN pg_catalog.pg_attribute AS attribute
				  ON attribute.attrelid = table_relation.oid AND attribute.attnum = key.attnum
			), '') AS columns,
			COALESCE(pg_catalog.pg_get_expr(index_state.indpred, index_state.indrelid, false), '') AS predicate,
			index_state.indisunique AS unique_index,
			index_relation.relkind = 'i' AND index_relation.relpersistence = 'p'
				AND access_method.amname = 'btree' AND index_state.indisvalid
				AND index_state.indisready AND index_state.indislive AND index_state.indimmediate
				AND NOT index_state.indisprimary AND NOT index_state.indisexclusion
				AND NOT index_state.indisclustered AND NOT index_state.indisreplident
				AND index_state.indnkeyatts = index_state.indnatts
				AND index_relation.reloptions IS NULL AND index_relation.relacl IS NULL
				AND NOT COALESCE((pg_catalog.to_jsonb(index_state)->>'indnullsnotdistinct')::boolean, false)
				AND NOT EXISTS (
					SELECT 1
					FROM pg_catalog.unnest(index_state.indoption::smallint[]) AS option(value)
					WHERE option.value <> 0
				)
				AND NOT EXISTS (
					SELECT 1
					FROM pg_catalog.unnest(index_state.indclass::oid[]) AS class(oid)
					JOIN pg_catalog.pg_opclass AS operator_class ON operator_class.oid = class.oid
					JOIN pg_catalog.pg_namespace AS class_namespace ON class_namespace.oid = operator_class.opcnamespace
					WHERE NOT operator_class.opcdefault OR class_namespace.nspname <> 'pg_catalog'
				)
				AND NOT EXISTS (
					SELECT 1
					FROM pg_catalog.unnest(index_state.indcollation::oid[]) AS index_collation(oid)
					LEFT JOIN pg_catalog.pg_collation AS resolved ON resolved.oid = index_collation.oid
					LEFT JOIN pg_catalog.pg_namespace AS resolved_namespace ON resolved_namespace.oid = resolved.collnamespace
					WHERE index_collation.oid <> 0
					  AND (resolved_namespace.nspname <> 'pg_catalog' OR resolved.collname <> 'default')
				) AS valid
		FROM pg_catalog.pg_class AS index_relation
		JOIN pg_catalog.pg_namespace AS index_namespace ON index_namespace.oid = index_relation.relnamespace
		JOIN pg_catalog.pg_index AS index_state ON index_state.indexrelid = index_relation.oid
		JOIN pg_catalog.pg_class AS table_relation ON table_relation.oid = index_state.indrelid
		JOIN pg_catalog.pg_namespace AS table_namespace ON table_namespace.oid = table_relation.relnamespace
		JOIN pg_catalog.pg_am AS access_method ON access_method.oid = index_relation.relam
		WHERE index_namespace.nspname = 'public'
		  AND index_relation.relname = CAST(? AS text)
		  AND table_namespace.nspname = 'public'
		  AND table_relation.relname = CAST(? AS text)
	`, contract.name, contract.table).Scan(&state)
	if result.Error != nil {
		return fmt.Errorf("read identity-review index %q: %w", contract.name, result.Error)
	}
	actualColumns, err := canonicalizeLegacyAdditiveIndexPredicate(state.Columns)
	if err != nil {
		return fmt.Errorf("parse identity-review index %q columns: %w", contract.name, err)
	}
	expectedColumns, err := canonicalizeLegacyAdditiveIndexPredicate(contract.columns)
	if err != nil {
		return fmt.Errorf("parse expected identity-review index %q columns: %w", contract.name, err)
	}
	actualPredicate, err := canonicalizeLegacyAdditiveIndexPredicate(state.Predicate)
	if err != nil {
		return fmt.Errorf("parse identity-review index %q predicate: %w", contract.name, err)
	}
	expectedPredicate, err := canonicalizeLegacyAdditiveIndexPredicate(contract.predicate)
	if err != nil {
		return fmt.Errorf("parse expected identity-review index %q predicate: %w", contract.name, err)
	}
	if result.RowsAffected != 1 || !state.Valid || state.Unique != contract.unique ||
		actualColumns != expectedColumns || actualPredicate != expectedPredicate {
		return fmt.Errorf("identity-review index %q is not exact", contract.name)
	}
	if err := verifyLegacyIndexDependencies(
		db,
		state.IndexOID,
		state.TableOID,
		contract.dependencyColumns,
		contract.tableDependencyCount,
	); err != nil {
		return fmt.Errorf("identity-review index %q dependency binding is not exact: %w", contract.name, err)
	}
	return nil
}

func verifyLegacyAdditiveSchemaContract(db *gorm.DB) error {
	major, err := supportedPostgresMajor(db)
	if err != nil {
		return fmt.Errorf("read identity-review schema PostgreSQL version: %w", err)
	}
	if err := verifyLegacyCheckDefinitionManifest(major); err != nil {
		return err
	}
	for _, contract := range legacyAdditiveModelContracts {
		if err := verifyLegacyAdditiveModelShape(db, contract); err != nil {
			return err
		}
	}
	for _, constraint := range coexistenceCheckConstraintTables {
		if err := verifyLegacyCheckConstraint(db, major, constraint.table, constraint.name); err != nil {
			return err
		}
	}
	for _, contract := range legacyAdditiveForeignKeyContracts {
		if err := verifyLegacyAdditiveForeignKey(db, contract); err != nil {
			return err
		}
	}
	if err := verifyLegacyAdditiveForeignKeyInventory(db); err != nil {
		return err
	}
	for _, contract := range legacyAdditiveIndexContracts {
		if err := verifyLegacyAdditiveIndex(db, contract); err != nil {
			return err
		}
	}
	return nil
}

type legacyTenantRelationPolicyState struct {
	Exists              bool   `gorm:"column:exists"`
	RelationKind        string `gorm:"column:relation_kind"`
	Persistence         string `gorm:"column:persistence"`
	OwnerOID            int64  `gorm:"column:owner_oid"`
	OwnerIsCurrent      bool   `gorm:"column:owner_is_current"`
	HasHierarchy        bool   `gorm:"column:has_hierarchy"`
	SelectPrivilege     bool   `gorm:"column:select_privilege"`
	RowSecurity         bool   `gorm:"column:row_security"`
	ForceRowSecurity    bool   `gorm:"column:force_row_security"`
	PolicyCount         int64  `gorm:"column:policy_count"`
	MigrationNamedCount int64  `gorm:"column:migration_named_count"`
	MigrationExactCount int64  `gorm:"column:migration_exact_count"`
	MigrationOwnerExact int64  `gorm:"column:migration_owner_exact_count"`
	TenantNamedCount    int64  `gorm:"column:tenant_named_count"`
}

func readLegacyTenantRelationPolicyState(
	db *gorm.DB,
	table string,
) (legacyTenantRelationPolicyState, error) {
	if err := validateIdentifier(table); err != nil {
		return legacyTenantRelationPolicyState{}, err
	}
	var state legacyTenantRelationPolicyState
	result := db.Raw(`
		WITH relation AS (
			SELECT relation.*
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND relation.relname = CAST(? AS text)
		), current_role_oid AS (
			SELECT role.oid FROM pg_catalog.pg_roles AS role WHERE role.rolname = current_user
		), relation_policy AS (
			SELECT policy.* FROM pg_catalog.pg_policy AS policy
			JOIN relation ON relation.oid = policy.polrelid
		)
		SELECT
			true AS exists,
			relation.relkind::text AS relation_kind,
			relation.relpersistence::text AS persistence,
			relation.relowner::bigint AS owner_oid,
			relation.relowner = (SELECT oid FROM current_role_oid) AS owner_is_current,
			EXISTS (
				SELECT 1 FROM pg_catalog.pg_inherits AS inheritance
				WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
			) AS has_hierarchy,
			pg_catalog.has_table_privilege(current_user, relation.oid, 'SELECT') AS select_privilege,
			relation.relrowsecurity AS row_security,
			relation.relforcerowsecurity AS force_row_security,
			(SELECT COUNT(*) FROM relation_policy) AS policy_count,
			(SELECT COUNT(*) FROM relation_policy WHERE polname = 'rereply_migration_access') AS migration_named_count,
			(SELECT COUNT(*) FROM relation_policy
			 WHERE polname = 'rereply_migration_access'
			   AND polroles = ARRAY[(SELECT oid FROM current_role_oid)]::oid[]
			   AND polpermissive AND polcmd = '*'
			   AND pg_catalog.pg_get_expr(polqual, polrelid, false) = 'true'
			   AND pg_catalog.pg_get_expr(polwithcheck, polrelid, false) = 'true') AS migration_exact_count,
			(SELECT COUNT(*) FROM relation_policy
			 WHERE polname = 'rereply_migration_access'
			   AND polroles = ARRAY[relation.relowner]::oid[]
			   AND polpermissive AND polcmd = '*'
			   AND pg_catalog.pg_get_expr(polqual, polrelid, false) = 'true'
			   AND pg_catalog.pg_get_expr(polwithcheck, polrelid, false) = 'true') AS migration_owner_exact_count,
			(SELECT COUNT(*) FROM relation_policy WHERE polname = 'rereply_tenant_isolation') AS tenant_named_count
		FROM relation
	`, table).Scan(&state)
	if result.Error != nil {
		return legacyTenantRelationPolicyState{}, result.Error
	}
	if result.RowsAffected != 1 || !state.Exists {
		return legacyTenantRelationPolicyState{}, fmt.Errorf("public.%s is missing", table)
	}
	return state, nil
}

func (state legacyTenantRelationPolicyState) requireExactProtected(
	table string,
	missingTenantPolicy bool,
) error {
	expectedPolicies := int64(2)
	expectedTenantPolicies := int64(1)
	if missingTenantPolicy {
		expectedPolicies = 1
		expectedTenantPolicies = 0
	}
	if state.RelationKind != "r" || state.Persistence != "p" ||
		!state.OwnerIsCurrent || state.HasHierarchy ||
		!state.SelectPrivilege || !state.RowSecurity || !state.ForceRowSecurity ||
		state.PolicyCount != expectedPolicies || state.MigrationNamedCount != 1 ||
		state.MigrationExactCount != 1 || state.TenantNamedCount != expectedTenantPolicies {
		return fmt.Errorf("protected tenant policy inventory on %q is not exact", table)
	}
	return nil
}

func (state legacyTenantRelationPolicyState) requireExactRuntimeProtected(table string) error {
	if state.RelationKind != "r" || state.Persistence != "p" || state.OwnerOID == 0 ||
		state.HasHierarchy || !state.SelectPrivilege || !state.RowSecurity ||
		!state.ForceRowSecurity || state.PolicyCount != 2 ||
		state.MigrationNamedCount != 1 || state.MigrationOwnerExact != 1 ||
		state.TenantNamedCount != 1 {
		return fmt.Errorf("protected runtime tenant policy inventory on %q is not exact", table)
	}
	return nil
}

func verifyExactLegacyTenantFingerprintFunction(
	db *gorm.DB,
	signature string,
	functionName string,
	expectedValue string,
	runtimeRole string,
	expectedOwner *databaseRoleReference,
	requireCurrentOwner bool,
) (databaseRoleReference, error) {
	return verifyExactLegacyTenantFingerprintFunctionAccess(
		db,
		signature,
		functionName,
		expectedValue,
		runtimeRole,
		expectedOwner,
		requireCurrentOwner,
		true,
	)
}

func verifyExactLegacyTenantPrepareClaim(
	db *gorm.DB,
	expectedValue string,
	runtimeRole string,
	expectedOwner *databaseRoleReference,
) (databaseRoleReference, error) {
	if !strings.HasPrefix(expectedValue, rlsMigrationPrepareClaimPrefix) ||
		!isLowerHexSHA256(strings.TrimPrefix(expectedValue, rlsMigrationPrepareClaimPrefix)) {
		return databaseRoleReference{}, errors.New("RLS migration prepare claim value is invalid")
	}
	return verifyExactLegacyTenantFingerprintFunctionAccess(
		db,
		tenantAdditivePolicyFingerprintSignature,
		"rereply_tenant_policy_additive_fingerprint_v1",
		expectedValue,
		runtimeRole,
		expectedOwner,
		true,
		false,
	)
}

func verifyExactLegacyTenantFingerprintFunctionAccess(
	db *gorm.DB,
	signature string,
	functionName string,
	expectedValue string,
	runtimeRole string,
	expectedOwner *databaseRoleReference,
	requireCurrentOwner bool,
	requireRuntimeExecute bool,
) (databaseRoleReference, error) {
	var state struct {
		NameCount         int64  `gorm:"column:name_count"`
		Source            string `gorm:"column:source"`
		OwnerOID          int64  `gorm:"column:owner_oid"`
		OwnerName         string `gorm:"column:owner_name"`
		OwnerIsCurrent    bool   `gorm:"column:owner_is_current"`
		Language          string `gorm:"column:language"`
		Volatility        string `gorm:"column:volatility"`
		Parallel          string `gorm:"column:parallel"`
		SecurityDefiner   bool   `gorm:"column:security_definer"`
		Leakproof         bool   `gorm:"column:leakproof"`
		Strict            bool   `gorm:"column:strict"`
		ReturnsText       bool   `gorm:"column:returns_text"`
		ReturnsSet        bool   `gorm:"column:returns_set"`
		ExactSearchPath   bool   `gorm:"column:exact_search_path"`
		OwnerExecute      bool   `gorm:"column:owner_execute"`
		RuntimeExecute    bool   `gorm:"column:runtime_execute"`
		PublicExecute     bool   `gorm:"column:public_execute"`
		UnexpectedExecute bool   `gorm:"column:unexpected_execute"`
	}
	result := db.Raw(`
		SELECT
			(SELECT COUNT(*) FROM pg_catalog.pg_proc AS named
			 JOIN pg_catalog.pg_namespace AS named_namespace ON named_namespace.oid = named.pronamespace
			 WHERE named_namespace.nspname = 'public' AND named.proname = CAST(? AS text)) AS name_count,
			procedure.prosrc AS source,
			owner.oid::bigint AS owner_oid,
			owner.rolname::text AS owner_name,
			owner.rolname = current_user AS owner_is_current,
			language.lanname::text AS language,
			procedure.provolatile::text AS volatility,
			procedure.proparallel::text AS parallel,
			procedure.prosecdef AS security_definer,
			procedure.proleakproof AS leakproof,
			procedure.proisstrict AS strict,
			procedure.prorettype = 'text'::pg_catalog.regtype AS returns_text,
			procedure.proretset AS returns_set,
			procedure.proconfig = ARRAY['search_path=pg_catalog, public']::text[] AS exact_search_path,
			pg_catalog.has_function_privilege(owner.oid, procedure.oid, 'EXECUTE') AS owner_execute,
			pg_catalog.has_function_privilege(CAST(? AS text), procedure.oid, 'EXECUTE') AS runtime_execute,
			EXISTS (
				SELECT 1 FROM pg_catalog.aclexplode(COALESCE(
					procedure.proacl, pg_catalog.acldefault('f', procedure.proowner)
				)) AS privilege
				WHERE privilege.privilege_type = 'EXECUTE' AND privilege.grantee = 0
			) AS public_execute,
			EXISTS (
				SELECT 1 FROM pg_catalog.aclexplode(COALESCE(
					procedure.proacl, pg_catalog.acldefault('f', procedure.proowner)
				)) AS privilege
				LEFT JOIN pg_catalog.pg_roles AS grantee ON grantee.oid = privilege.grantee
				WHERE privilege.privilege_type = 'EXECUTE'
				  AND privilege.grantee <> procedure.proowner
				  AND COALESCE(grantee.rolname <> CAST(? AS text), true)
			) AS unexpected_execute
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
		JOIN pg_catalog.pg_language AS language ON language.oid = procedure.prolang
		WHERE procedure.oid = pg_catalog.to_regprocedure(CAST(? AS text))
		  AND namespace.nspname = 'public'
		  AND procedure.prokind = 'f'
		  AND procedure.pronargs = 0
	`, functionName, runtimeRole, runtimeRole, signature).Scan(&state)
	if result.Error != nil {
		return databaseRoleReference{}, result.Error
	}
	if result.RowsAffected != 1 || state.NameCount != 1 ||
		strings.TrimSpace(state.Source) != "SELECT '"+expectedValue+"'::text" ||
		(requireCurrentOwner && !state.OwnerIsCurrent) || state.OwnerOID == 0 || state.OwnerName == "" ||
		state.Language != "sql" || state.Volatility != "i" || state.Parallel != "u" ||
		state.SecurityDefiner || state.Leakproof || state.Strict || !state.ReturnsText ||
		state.ReturnsSet || !state.ExactSearchPath || !state.OwnerExecute ||
		state.RuntimeExecute != requireRuntimeExecute || state.PublicExecute || state.UnexpectedExecute {
		return databaseRoleReference{}, fmt.Errorf("tenant fingerprint function %q is not exact", functionName)
	}
	owner := databaseRoleReference{OID: state.OwnerOID, Name: state.OwnerName}
	if expectedOwner != nil && owner != *expectedOwner {
		return databaseRoleReference{}, errors.New("tenant fingerprint functions have different owners")
	}
	member, err := roleHasMembership(db, runtimeRole, owner.OID)
	if err != nil {
		return databaseRoleReference{}, fmt.Errorf("inspect runtime membership in tenant fingerprint owner: %w", err)
	}
	if runtimeRole == owner.Name || member {
		return databaseRoleReference{}, errors.New("runtime role controls the tenant fingerprint owner")
	}
	return owner, nil
}

func readTenantPolicyFingerprintRecords(
	db *gorm.DB,
	tables []string,
) ([]tenantPolicyFingerprintRecord, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(tables))
	arguments := make([]any, len(tables))
	for index, table := range tables {
		placeholders[index] = "?"
		arguments[index] = table
	}
	records := make([]tenantPolicyFingerprintRecord, 0, len(tables))
	query := fmt.Sprintf(`
		SELECT
			policy_schema.nspname AS schema_name,
			policy_table.relname AS table_name,
			policy.polname AS policy_name,
			policy.polcmd::text AS command,
			policy.polpermissive AS permissive,
			policy.polroles::text AS roles,
			COALESCE(pg_catalog.pg_get_expr(policy.polqual, policy.polrelid, false), '') AS using_expression,
			COALESCE(pg_catalog.pg_get_expr(policy.polwithcheck, policy.polrelid, false), '') AS check_expression
		FROM pg_catalog.pg_policy AS policy
		JOIN pg_catalog.pg_class AS policy_table ON policy_table.oid = policy.polrelid
		JOIN pg_catalog.pg_namespace AS policy_schema ON policy_schema.oid = policy_table.relnamespace
		WHERE policy_schema.nspname = 'public'
		  AND policy.polname = 'rereply_tenant_isolation'
		  AND policy_table.relname IN (%s)
		ORDER BY policy_schema.nspname, policy_table.relname, policy.polname
	`, strings.Join(placeholders, ", "))
	if err := db.Raw(query, arguments...).Scan(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func verifyCanonicalTenantPolicyInventory(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	expectedTables := append([]string(nil), tables...)
	sort.Strings(expectedTables)
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return err
	}
	records, err := readTenantPolicyFingerprintRecords(db, expectedTables)
	if err != nil {
		return err
	}
	if len(records) != len(expectedTables) {
		return fmt.Errorf("canonical tenant policy count is %d; expected %d", len(records), len(expectedTables))
	}
	for index, table := range expectedTables {
		if records[index].Table != table {
			return fmt.Errorf("canonical tenant policy %d is for %q; expected %q", index, records[index].Table, table)
		}
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return fmt.Errorf("inspect canonical tenant relation %q: %w", table, err)
		}
		if err := state.requireExactProtected(table, false); err != nil {
			return err
		}
		if err := verifyCanonicalTenantPolicyRecord(db, records[index], table, runtimeRoleOID); err != nil {
			return err
		}
	}
	return nil
}

// verifyRuntimeCanonicalTenantPolicyInventory validates the same immutable
// policy catalogue from the authenticated runtime connection. Unlike the
// migration classifier it must not require current_user to own the tables or
// be the migration-policy grantee; the exact table owner is that grantee and
// later startup checks prove the runtime cannot assume any owner role.
func verifyRuntimeCanonicalTenantPolicyInventory(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	expectedTables := append([]string(nil), tables...)
	sort.Strings(expectedTables)
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return err
	}
	records, err := readTenantPolicyFingerprintRecords(db, expectedTables)
	if err != nil {
		return err
	}
	if len(records) != len(expectedTables) {
		return fmt.Errorf("canonical tenant policy count is %d; expected %d", len(records), len(expectedTables))
	}
	if err := verifyExactProtectedRuntimeTablePrivileges(db, expectedTables, runtimeRole); err != nil {
		return err
	}
	for index, table := range expectedTables {
		if records[index].Table != table {
			return fmt.Errorf("canonical tenant policy %d is for %q; expected %q", index, records[index].Table, table)
		}
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return fmt.Errorf("inspect canonical runtime tenant relation %q: %w", table, err)
		}
		if err := state.requireExactRuntimeProtected(table); err != nil {
			return err
		}
		if err := verifyCanonicalTenantPolicyRecord(db, records[index], table, runtimeRoleOID); err != nil {
			return err
		}
	}
	return nil
}

func verifyMigrationCallbackScanAuthority(db *gorm.DB, runtimeRole string) error {
	if err := verifyLegacyAdditiveSchemaContract(db); err != nil {
		return fmt.Errorf("verify migration-callback additive schema: %w", err)
	}
	if err := verifyPlatformComplianceCoreLegacyGuards(db, runtimeRole); err != nil {
		return err
	}
	if err := verifyPlatformComplianceCoreScanAuthority(db); err != nil {
		return err
	}
	allTables := protectedTenantTableNames()
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(allTables)
	if err != nil {
		return err
	}
	if err := requireExactExistingProtectedTenantTables(db, allTables); err != nil {
		return err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return fmt.Errorf("inspect migration-callback runtime role: %w", err)
	}
	coreRecords, err := readTenantPolicyFingerprintRecords(db, coreTables)
	if err != nil {
		return fmt.Errorf("read migration-callback core tenant policies: %w", err)
	}
	if len(coreRecords) != len(coreTables) {
		return fmt.Errorf("migration-callback core tenant policy count is %d; expected %d", len(coreRecords), len(coreTables))
	}
	if err := verifyExactProtectedRuntimeTablePrivileges(db, coreTables, runtimeRole); err != nil {
		return err
	}
	var directReference tenantPolicyFingerprintRecord
	for _, record := range coreRecords {
		if record.Table == "contacts" {
			directReference = record
			break
		}
	}
	if directReference.Table == "" {
		return errors.New("migration-callback canonical direct-tenant policy template is missing")
	}
	additiveRecords, err := readTenantPolicyFingerprintRecords(db, additiveTables)
	if err != nil {
		return fmt.Errorf("read migration-callback additive tenant policies: %w", err)
	}
	additiveByTable := make(map[string]tenantPolicyFingerprintRecord, len(additiveRecords))
	for _, record := range additiveRecords {
		if _, duplicate := additiveByTable[record.Table]; duplicate {
			return fmt.Errorf("duplicate migration-callback additive tenant policy record for %q", record.Table)
		}
		additiveByTable[record.Table] = record
	}
	for index, table := range coreTables {
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return err
		}
		if err := state.requireExactProtected(table, false); err != nil {
			return err
		}
		if coreRecords[index].Table != table {
			return fmt.Errorf("migration-callback core tenant policy %d is for %q; expected %q", index, coreRecords[index].Table, table)
		}
		if err := verifyCanonicalTenantPolicyRecord(db, coreRecords[index], table, runtimeRoleOID); err != nil {
			return err
		}
	}
	for _, table := range additiveTables {
		state, err := readLegacyTenantRelationPolicyState(db, table)
		if err != nil {
			return err
		}
		record, present := additiveByTable[table]
		if !state.RowSecurity && !state.ForceRowSecurity && state.PolicyCount == 0 &&
			state.RelationKind == "r" && state.Persistence == "p" &&
			state.OwnerIsCurrent && !state.HasHierarchy &&
			state.SelectPrivilege {
			if present {
				return fmt.Errorf("unprotected additive scaffold %q has a tenant policy record", table)
			}
			if err := verifyNoRuntimeAdditiveScaffoldPrivileges(db, table, runtimeRole); err != nil {
				return err
			}
			continue
		}
		missingTenantPolicy := !present
		if err := state.requireExactProtected(table, missingTenantPolicy); err != nil {
			return err
		}
		if err := verifyExactProtectedRuntimeTablePrivileges(db, []string{table}, runtimeRole); err != nil {
			return err
		}
		if present {
			if err := verifyCanonicalTenantPolicyRecord(db, record, table, runtimeRoleOID); err != nil {
				return err
			}
			if err := requireExactDirectTenantPolicyRepresentation(record, directReference); err != nil {
				return fmt.Errorf(
					"migration-callback tenant policy representation: %w",
					err,
				)
			}
		}
	}
	return nil
}

func verifyNoRuntimeAdditiveScaffoldPrivileges(db *gorm.DB, table, runtimeRole string) error {
	if err := validateIdentifier(table); err != nil {
		return err
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return fmt.Errorf("inspect runtime role for additive scaffold %q: %w", table, err)
	}
	var state struct {
		Count int64 `gorm:"column:relation_count"`
		Exact bool  `gorm:"column:privileges_absent"`
	}
	if err := db.Raw(`
		WITH relation AS (
			SELECT relation.*
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
			  AND relation.relkind = 'r'
			  AND relation.relpersistence = 'p'
		), unexpected_acl_grant AS (
			SELECT grant_state.privilege_type
			FROM relation
			CROSS JOIN LATERAL pg_catalog.aclexplode(
				COALESCE(relation.relacl, pg_catalog.acldefault('r', relation.relowner))
			) AS grant_state
			WHERE CASE
				WHEN grant_state.grantee = 0 THEN true
				ELSE pg_catalog.pg_has_role(
					CAST(? AS pg_catalog.oid), grant_state.grantee, 'MEMBER'
				)
			  END
			UNION ALL
			SELECT grant_state.privilege_type
			FROM relation
			JOIN pg_catalog.pg_attribute AS attribute
			  ON attribute.attrelid = relation.oid
			 AND attribute.attnum > 0
			 AND NOT attribute.attisdropped
			 AND attribute.attacl IS NOT NULL
			CROSS JOIN LATERAL pg_catalog.aclexplode(
				attribute.attacl
			) AS grant_state
			WHERE CASE
				WHEN grant_state.grantee = 0 THEN true
				ELSE pg_catalog.pg_has_role(
					CAST(? AS pg_catalog.oid), grant_state.grantee, 'MEMBER'
				)
			  END
		)
		SELECT
			COUNT(*) AS relation_count,
			COALESCE(pg_catalog.bool_and(
				NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'SELECT')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'INSERT')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'UPDATE')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'DELETE')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'TRUNCATE')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'REFERENCES')
				AND NOT pg_catalog.has_table_privilege(CAST(? AS text), relation.oid, 'TRIGGER')
				AND NOT pg_catalog.has_any_column_privilege(CAST(? AS text), relation.oid, 'SELECT')
				AND NOT pg_catalog.has_any_column_privilege(CAST(? AS text), relation.oid, 'INSERT')
				AND NOT pg_catalog.has_any_column_privilege(CAST(? AS text), relation.oid, 'UPDATE')
				AND NOT pg_catalog.has_any_column_privilege(CAST(? AS text), relation.oid, 'REFERENCES')
				AND NOT EXISTS (SELECT 1 FROM unexpected_acl_grant)
			), false) AS privileges_absent
		FROM relation
	`, table, runtimeRoleOID, runtimeRoleOID,
		runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole,
		runtimeRole, runtimeRole, runtimeRole, runtimeRole).Scan(&state).Error; err != nil {
		return fmt.Errorf("inspect runtime privileges on additive scaffold %q: %w", table, err)
	}
	if state.Count != 1 || !state.Exact {
		return fmt.Errorf("runtime role has effective privileges on unprotected additive scaffold %q", table)
	}
	return nil
}

func revokeRuntimeDefaultTablePrivilegesBeforeAdditivePreparation(
	db *gorm.DB,
	runtimeRole string,
) error {
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return fmt.Errorf("inspect runtime role before additive preparation: %w", err)
	}
	var migrationRole string
	if err := db.Raw("SELECT current_user").Scan(&migrationRole).Error; err != nil {
		return fmt.Errorf("read migration role before additive preparation: %w", err)
	}
	if err := validateIdentifier(migrationRole); err != nil {
		return fmt.Errorf("invalid migration role before additive preparation: %w", err)
	}
	if strings.EqualFold(migrationRole, runtimeRole) {
		return errors.New("runtime role must differ from migration role during additive preparation")
	}
	var migrationRoleOID int64
	if err := db.Raw(`
		SELECT COALESCE(pg_catalog.min(role.oid::bigint), 0)
		FROM pg_catalog.pg_roles AS role
		WHERE role.rolname = CAST(? AS text)
	`, migrationRole).Scan(&migrationRoleOID).Error; err != nil {
		return fmt.Errorf("inspect migration role before additive preparation: %w", err)
	}
	if migrationRoleOID == 0 {
		return fmt.Errorf("migration role %q does not exist", migrationRole)
	}
	runtimeIsMigrationMember, err := roleHasMembership(db, runtimeRole, migrationRoleOID)
	if err != nil {
		return fmt.Errorf("inspect runtime membership before additive preparation: %w", err)
	}
	if runtimeIsMigrationMember {
		return fmt.Errorf("runtime role %q must not be a member of migration role %q", runtimeRole, migrationRole)
	}
	migrator := quoteIdentifier(migrationRole)
	runtime := quoteIdentifier(runtimeRole)
	statements := []string{
		fmt.Sprintf(
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM %s",
			migrator,
			runtime,
		),
		fmt.Sprintf(
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM PUBLIC",
			migrator,
		),
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("revoke additive-preparation default table privileges: %w", err)
			}
		}
		return verifyRuntimeDefaultTablePrivilegesRevoked(tx, migrationRoleOID, runtimeRoleOID)
	}); err != nil {
		return err
	}
	return nil
}

func verifyRuntimeDefaultTablePrivilegesRevoked(
	db *gorm.DB,
	migrationRoleOID int64,
	runtimeRoleOID int64,
) error {
	if migrationRoleOID == 0 || runtimeRoleOID == 0 {
		return errors.New("additive-preparation default privilege role binding is missing")
	}
	var unexpectedDefaults int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_default_acl AS defaults
		CROSS JOIN LATERAL pg_catalog.aclexplode(defaults.defaclacl) AS grant_state
		WHERE defaults.defaclrole = CAST(? AS pg_catalog.oid)
		  AND defaults.defaclobjtype = 'r'
		  AND (
			defaults.defaclnamespace = 0
			OR defaults.defaclnamespace = 'public'::pg_catalog.regnamespace
		  )
		  AND CASE
			WHEN grant_state.grantee = 0 THEN true
			ELSE pg_catalog.pg_has_role(
				CAST(? AS pg_catalog.oid),
				grant_state.grantee,
				'MEMBER'
			)
		  END
	`, migrationRoleOID, runtimeRoleOID).Scan(&unexpectedDefaults).Error; err != nil {
		return fmt.Errorf("inspect additive-preparation default table privileges: %w", err)
	}
	if unexpectedDefaults != 0 {
		return fmt.Errorf(
			"runtime role has %d effective default table privilege grant(s) before additive preparation",
			unexpectedDefaults,
		)
	}
	return nil
}

func createLegacyRLSMigrationPrepareClaim(
	db *gorm.DB,
	runtimeRole string,
	claim string,
) error {
	if !strings.HasPrefix(claim, rlsMigrationPrepareClaimPrefix) ||
		!isLowerHexSHA256(strings.TrimPrefix(claim, rlsMigrationPrepareClaimPrefix)) {
		return errors.New("RLS migration prepare claim value is invalid")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := revokeRuntimeDefaultTablePrivilegesBeforeAdditivePreparation(tx, runtimeRole); err != nil {
			return err
		}
		createStatement := fmt.Sprintf(`CREATE FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()
			RETURNS text
			LANGUAGE sql
			IMMUTABLE
			SET search_path = pg_catalog, public
			AS $function$
			  SELECT '%s'::text
			$function$`, claim)
		for _, statement := range []string{
			createStatement,
			"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM PUBLIC",
			fmt.Sprintf(
				"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM %s",
				quoteIdentifier(runtimeRole),
			),
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("create owner-only RLS migration prepare claim: %w", err)
			}
		}
		coreTables, _, err := tenantPolicyFingerprintProfiles(protectedTenantTableNames())
		if err != nil {
			return err
		}
		_, runtimeRoleOID, owner, err := verifyExactLegacyTenantCorePolicyContract(
			tx,
			coreTables,
			runtimeRole,
		)
		if err != nil {
			return err
		}
		if _, err := verifyExactLegacyTenantPrepareClaim(
			tx,
			claim,
			runtimeRole,
			&owner,
		); err != nil {
			return err
		}
		return verifyRuntimeDefaultTablePrivilegesRevoked(tx, owner.OID, runtimeRoleOID)
	})
}

// VerifyRLSMigrationCallbackScanAuthorityForTest exposes the final read-only
// fence immediately before release backfills. Production callers use the same
// private helper through runRLSMigrationCoordinator.
func VerifyRLSMigrationCallbackScanAuthorityForTest(db *gorm.DB, runtimeRole string) error {
	return verifyMigrationCallbackScanAuthority(db, runtimeRole)
}

// VerifyDirectTenantPolicyRepresentationForTest proves that the raw catalog
// representation fence rejects two source expressions that normalize to the
// same semantic policy. Production callers reach the same comparison through
// the migration classifier and callback scan authority.
func VerifyDirectTenantPolicyRepresentationForTest(referenceExpression, candidateExpression string) error {
	referenceCanonical, err := canonicalizeTenantPolicyExpression(referenceExpression)
	if err != nil {
		return err
	}
	candidateCanonical, err := canonicalizeTenantPolicyExpression(candidateExpression)
	if err != nil {
		return err
	}
	if referenceCanonical != candidateCanonical {
		return errors.New("test tenant policy expressions are not canonically equivalent")
	}
	reference := tenantPolicyFingerprintRecord{
		Schema:          "public",
		Table:           "contacts",
		Policy:          "rereply_tenant_isolation",
		Command:         "*",
		Permissive:      true,
		Roles:           "{1}",
		UsingExpression: referenceExpression,
		CheckExpression: referenceExpression,
	}
	candidate := reference
	candidate.Table = "whatsapp_identity_review_holds"
	candidate.UsingExpression = candidateExpression
	candidate.CheckExpression = candidateExpression
	return requireExactDirectTenantPolicyRepresentation(candidate, reference)
}

// withMigrationSession pins every migration statement to one PostgreSQL
// connection, takes a database-scoped session advisory lock, and applies a
// bounded lock timeout. Pinning matters because CREATE INDEX CONCURRENTLY
// cannot run inside a transaction, while a transaction-scoped advisory lock
// would otherwise be the only straightforward way to keep one connection.
// The database-name hash keeps disposable test databases and independent
// deployments on the same PostgreSQL cluster from blocking each other.
func withMigrationSession(db *gorm.DB, migrate func(*gorm.DB) error) error {
	if db == nil {
		return errors.New("database is required")
	}
	if migrate == nil {
		return errors.New("migration callback is required")
	}
	if db.Name() != "postgres" {
		return migrate(db.Session(&gorm.Session{NewDB: true}))
	}

	return db.Connection(func(connection *gorm.DB) (returnErr error) {
		// Connection returns a handle whose Statement can be reused in place.
		// Keep a clone-on-use wrapper around its pinned ConnPool so Raw().Scan(),
		// migrators, and callbacks cannot leak model/table state into each other.
		session := connection.Session(&gorm.Session{NewDB: true})
		var previousSearchPath string
		if err := session.Raw("SHOW search_path").Scan(&previousSearchPath).Error; err != nil {
			return fmt.Errorf("read database migration search path: %w", err)
		}
		var configuredSearchPath string
		if err := session.Raw(
			"SELECT pg_catalog.set_config('search_path', 'public, pg_temp', false)",
		).Scan(&configuredSearchPath).Error; err != nil {
			return fmt.Errorf("set database migration search path: %w", err)
		}
		defer func() {
			var restoredSearchPath string
			if err := session.Raw(
				"SELECT pg_catalog.set_config('search_path', ?, false)",
				previousSearchPath,
			).Scan(&restoredSearchPath).Error; err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore database migration search path: %w", err))
			}
		}()
		var pathState struct {
			CurrentSchema string `gorm:"column:current_schema"`
			SearchPath    string `gorm:"column:search_path"`
		}
		if err := session.Raw(`
			SELECT
				pg_catalog.current_schema()::text AS current_schema,
				pg_catalog.current_setting('search_path') AS search_path
		`).Scan(&pathState).Error; err != nil {
			return fmt.Errorf("verify database migration search path: %w", err)
		}
		if pathState.CurrentSchema != "public" || pathState.SearchPath != "public, pg_temp" ||
			configuredSearchPath != "public, pg_temp" {
			return fmt.Errorf(
				"database migration search path is %q with current schema %q; expected public, pg_temp",
				pathState.SearchPath,
				pathState.CurrentSchema,
			)
		}
		var sessionReplicationRole string
		if err := session.Raw(
			"SELECT pg_catalog.current_setting('session_replication_role')",
		).Scan(&sessionReplicationRole).Error; err != nil {
			return fmt.Errorf("inspect database migration session_replication_role: %w", err)
		}
		if sessionReplicationRole != "origin" {
			return fmt.Errorf(
				"database migration session_replication_role is %q; expected origin",
				sessionReplicationRole,
			)
		}
		var acquired bool
		if err := session.Raw(
			"SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtext(pg_catalog.current_database()), ?)",
			migrationAdvisoryLockNamespace,
		).Scan(&acquired).Error; err != nil {
			return fmt.Errorf("acquire database migration advisory lock: %w", err)
		}
		if !acquired {
			return errors.New("another database migrator already owns the ReReply migration lock")
		}
		defer func() {
			var unlocked bool
			if err := session.Raw(
				"SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtext(pg_catalog.current_database()), ?)",
				migrationAdvisoryLockNamespace,
			).Scan(&unlocked).Error; err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("release database migration advisory lock: %w", err))
			} else if !unlocked {
				returnErr = errors.Join(returnErr, errors.New("database migration advisory lock was not owned at release"))
			}
		}()

		var previousLockTimeout string
		if err := session.Raw("SHOW lock_timeout").Scan(&previousLockTimeout).Error; err != nil {
			return fmt.Errorf("read database migration lock timeout: %w", err)
		}
		var configuredLockTimeout string
		if err := session.Raw(
			"SELECT pg_catalog.set_config('lock_timeout', '5s', false)",
		).Scan(&configuredLockTimeout).Error; err != nil {
			return fmt.Errorf("set database migration lock timeout: %w", err)
		}
		defer func() {
			var restoredLockTimeout string
			if err := session.Raw(
				"SELECT pg_catalog.set_config('lock_timeout', ?, false)",
				previousLockTimeout,
			).Scan(&restoredLockTimeout).Error; err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore database migration lock timeout: %w", err))
			}
		}()

		return migrate(session)
	})
}

// RunRLSMigrationCoordinator runs the immutable source phase's complete
// pre-deploy database authority. The callback receives the same pinned
// migration connection and executes the non-database-package backfills between
// schema preparation and policy publication. The verifier must open/use the
// runtime-role connection and is deliberately invoked while the migration
// session still owns the singleton advisory lock.
func RunRLSMigrationCoordinator(
	db *gorm.DB,
	adminCfg *config.DefaultAdminConfig,
	runtimeRole string,
	backfill func(*gorm.DB) error,
	verifyRuntime func() error,
) error {
	return runRLSMigrationCoordinatorForPhase(
		db,
		adminCfg,
		runtimeRole,
		compiledRLSMigrationPhase,
		backfill,
		verifyRuntime,
	)
}

func runRLSMigrationCoordinatorForPhase(
	db *gorm.DB,
	adminCfg *config.DefaultAdminConfig,
	runtimeRole string,
	phase rlsMigrationPhase,
	backfill func(*gorm.DB) error,
	verifyRuntime func() error,
) error {
	if db == nil {
		return errors.New("database is required")
	}
	if db.Name() != "postgres" {
		return errors.New("RLS migration coordinator requires PostgreSQL session locking")
	}
	if adminCfg == nil {
		return errors.New("default administrator configuration is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	if backfill == nil {
		return errors.New("migration backfill callback is required")
	}
	if verifyRuntime == nil {
		return errors.New("runtime verification callback is required")
	}

	return withMigrationSession(db, func(session *gorm.DB) error {
		profile, err := detectPlatformComplianceIdentityReviewTriggerProfile(session)
		if err != nil {
			// Classification is the first database operation inside the singleton
			// boundary. A partial/misbound catalog therefore fails before any
			// preparer, seed, backfill, policy, or activation can run.
			return fmt.Errorf("classify RLS migration profile before mutation: %w", err)
		}
		action, err := decideRLSMigrationAction(phase, profile)
		if err != nil {
			return err
		}

		// Inspect the additive fingerprint before any future-profile early return.
		// A transient baseline claim must never be mistaken for a published final
		// fingerprint merely because another actor changed the trigger profile.
		catalogState, err := classifyLegacyRLSCatalog(session, runtimeRole, phase)
		if err != nil {
			return err
		}
		if action == rlsMigrationVerifyFuture {
			if catalogState == legacyRLSCatalogPreparing {
				return errors.Join(
					ErrRLSMigrationCatalogQuarantined,
					errors.New("future migration profile retains a transient baseline prepare claim"),
				)
			}
			if err := verifyFutureRLSPostcondition(session, runtimeRole); err != nil {
				return errors.Join(
					ErrRLSMigrationCatalogQuarantined,
					fmt.Errorf("verify future migration profile without mutation: %w", err),
				)
			}
			if err := verifyRuntime(); err != nil {
				return fmt.Errorf("verify future runtime database contract without mutation: %w", err)
			}
			return nil
		}
		if action == rlsMigrationPrepareLegacy && catalogState == legacyRLSCatalogComplete {
			if err := verifyCompleteLegacyRLSPostcondition(session, runtimeRole); err != nil {
				return fmt.Errorf("verify completed baseline migration profile without mutation: %w", err)
			}
			if err := verifyRuntime(); err != nil {
				return fmt.Errorf("verify completed baseline runtime database contract without mutation: %w", err)
			}
			return nil
		}

		readiness, _, _, err := splitRLSMigrationPlan(getIndexes())
		if err != nil {
			return err
		}
		if err := executeMigrationIndexStatement(session, readiness, false); err != nil {
			return fmt.Errorf("verify pre-mutation RLS migration readiness: %w", err)
		}

		// Preparation commits independently of policy activation. Establish its
		// schema boundary before even the first durable claim or repair, and
		// reject previously planted overloads before any application SQL runs.
		if err := establishMigrationSchemaTrust(session, runtimeRole); err != nil {
			return errors.Join(
				ErrRLSMigrationCatalogQuarantined,
				fmt.Errorf("establish pre-preparation schema trust: %w", err),
			)
		}

		if catalogState == legacyRLSCatalogPreAdditive {
			claim, err := rlsMigrationPrepareClaim(session, runtimeRole, phase)
			if err != nil {
				return errors.Join(
					ErrRLSMigrationCatalogQuarantined,
					fmt.Errorf("derive additive preparation claim: %w", err),
				)
			}
			if err := createLegacyRLSMigrationPrepareClaim(session, runtimeRole, claim); err != nil {
				return errors.Join(
					ErrRLSMigrationCatalogQuarantined,
					fmt.Errorf("atomically fence and claim additive preparation: %w", err),
				)
			}
			catalogState = legacyRLSCatalogPreparing
		}
		// Every admitted path below replays preparation. Reconcile its exact
		// concurrent-index artifacts regardless of whether admission came from a
		// transient claim, a repairable legacy policy profile, or a bridge from a
		// complete legacy profile. Future and completed-baseline paths returned
		// above without entering this mutation boundary.
		if err := reconcileInvalidConcurrentMigrationIndexes(session, getIndexes()); err != nil {
			return errors.Join(
				ErrRLSMigrationCatalogQuarantined,
				fmt.Errorf("reconcile concurrent-index retry artifacts: %w", err),
			)
		}
		if err := runMigrationPreparationWithProgressOnSession(session, adminCfg); err != nil {
			return fmt.Errorf("prepare schema under RLS migration interlock: %w", err)
		}
		if err := verifyMigrationCallbackScanAuthority(session, runtimeRole); err != nil {
			return errors.Join(
				ErrRLSMigrationCatalogQuarantined,
				fmt.Errorf("verify migration callback scan authority: %w", err),
			)
		}
		if err := backfill(session); err != nil {
			return fmt.Errorf("run migration backfills under RLS migration interlock: %w", err)
		}

		if err := ApplyTenantRLS(session, runtimeRole); err != nil {
			// A lost COMMIT acknowledgement must never trigger a blind second
			// policy write. Reconcile only the exact owner-visible postcondition.
			reconcileErr := verifyCompleteLegacyRLSPostcondition(session, runtimeRole)
			if outcomeErr := classifyRLSPolicyApplyOutcome(err, reconcileErr); outcomeErr != nil {
				return outcomeErr
			}
		}
		if err := verifyCompleteLegacyRLSPostcondition(session, runtimeRole); err != nil {
			return errors.Join(
				ErrRLSMigrationCommitOutcomeIndeterminate,
				fmt.Errorf("verify exact tenant policy postcondition: %w", err),
			)
		}

		if action == rlsMigrationPrepareLegacy {
			if err := verifyRuntime(); err != nil {
				return errors.Join(
					ErrRLSMigrationCommitOutcomeIndeterminate,
					fmt.Errorf("verify baseline runtime database contract after mutation: %w", err),
				)
			}
			return nil
		}

		_, activation, err := splitMigrationIndexes(getIndexes())
		if err != nil {
			return err
		}
		if activationErr := executeMigrationIndexStatement(session, activation, true); activationErr != nil {
			// The activation is one atomic PostgreSQL statement in one
			// transaction. An exact legacy profile proves rollback; an exact
			// future profile is accepted only after both independent read-only
			// verifiers prove the committed postcondition.
			postProfile, profileErr := detectPlatformComplianceIdentityReviewTriggerProfile(session)
			var ownerErr error
			var runtimeErr error
			if profileErr == nil && postProfile == platformComplianceFutureIdentityReviewTriggerProfile {
				ownerErr = verifyFutureRLSPostcondition(session, runtimeRole)
				runtimeErr = verifyRuntime()
			}
			return classifyRLSActivationOutcome(
				activationErr,
				postProfile,
				profileErr,
				ownerErr,
				runtimeErr,
			)
		}

		if err := verifyFutureRLSPostcondition(session, runtimeRole); err != nil {
			return errors.Join(
				ErrRLSMigrationCommitOutcomeIndeterminate,
				fmt.Errorf("verify activated future migration profile: %w", err),
			)
		}
		if err := verifyRuntime(); err != nil {
			return errors.Join(
				ErrRLSMigrationCommitOutcomeIndeterminate,
				fmt.Errorf("verify future runtime database contract after activation: %w", err),
			)
		}
		return nil
	})
}

// PrepareMessageIngestionOrder adds the nullable rollout column before GORM
// sees the Message model. Adding the column without a default is a metadata-only
// change for existing rows; setting the default afterwards does not rewrite or
// backfill them. Old rows remain NULL and are read through COALESCE(created_at),
// while every old- or new-binary insert after this preparation receives the
// database clock. A later maintenance migration can batch-backfill and enforce
// NOT NULL after all replicas have upgraded.
func PrepareMessageIngestionOrder(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	// Keep the additive columns, defaults, and rolling-version trigger contracts
	// in one PostgreSQL transaction. No old replica can insert a normalized
	// message or advance a legacy cursor in a partially prepared schema between
	// these statements.
	return db.Transaction(func(tx *gorm.DB) error {
		// Fail fast rather than queue an ACCESS EXCLUSIVE schema lock behind a
		// long-running production transaction and stall all following traffic on
		// the same relations. The migration can be retried by the singleton
		// migrator after the blocker is cleared.
		if tx.Name() == "postgres" {
			if err := tx.Exec("SET LOCAL lock_timeout = '5s'").Error; err != nil {
				return fmt.Errorf("set message-ingestion migration lock timeout: %w", err)
			}
		}
		if tx.Migrator().HasTable(&models.Message{}) {
			if err := tx.Exec(`
				ALTER TABLE messages
				ADD COLUMN IF NOT EXISTS ingested_at timestamptz
			`).Error; err != nil {
				return fmt.Errorf("add nullable message ingestion timestamp: %w", err)
			}
			if err := tx.Exec(`
				ALTER TABLE messages
				ALTER COLUMN ingested_at SET DEFAULT clock_timestamp()
			`).Error; err != nil {
				return fmt.Errorf("set message ingestion timestamp default: %w", err)
			}
		}
		if tx.Migrator().HasTable(&models.ConversationRead{}) {
			if err := tx.Exec(`
				ALTER TABLE conversation_reads
				ADD COLUMN IF NOT EXISTS last_read_ingested_at timestamptz
			`).Error; err != nil {
				return fmt.Errorf("add nullable conversation-read ingestion cursor: %w", err)
			}
		}
		return InstallMessageIngestionOrderTrigger(tx)
	})
}

// InstallMessageIngestionOrderTrigger makes the conversation row the shared
// commit-order serialization point for message visibility and read cursors.
// The trigger also protects writes from older replicas during the rolling
// deployment. clock_timestamp() is assigned only after the conversation lock
// is acquired, so a message transaction that commits after a completed read
// cannot retain an older cursor position.
func InstallMessageIngestionOrderTrigger(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&models.Message{}) ||
		!db.Migrator().HasTable(&models.InboxConversation{}) {
		return nil
	}
	if err := db.Exec(messageIngestionOrderFunctionSQL).Error; err != nil {
		return fmt.Errorf("create message ingestion-order trigger function: %w", err)
	}
	if err := db.Exec(`
		DO $block$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger
				WHERE tgname = 'trg_messages_ingestion_order'
				  AND tgrelid = 'messages'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER trg_messages_ingestion_order
				BEFORE INSERT OR UPDATE OF inbox_conversation_id ON messages
				FOR EACH ROW
				EXECUTE FUNCTION rereply_set_message_ingestion_order();
			END IF;
		END
		$block$
	`).Error; err != nil {
		return fmt.Errorf("install message ingestion-order trigger: %w", err)
	}
	if !db.Migrator().HasTable(&models.ConversationRead{}) ||
		!db.Migrator().HasColumn(&models.ConversationRead{}, "LastReadIngestedAt") {
		return nil
	}
	if err := db.Exec(conversationReadIngestionOrderFunctionSQL).Error; err != nil {
		return fmt.Errorf("create conversation-read ingestion-order trigger function: %w", err)
	}
	if err := db.Exec(`
		DO $block$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger
				WHERE tgname = 'trg_conversation_reads_ingestion_order'
				  AND tgrelid = 'conversation_reads'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER trg_conversation_reads_ingestion_order
				BEFORE INSERT OR UPDATE OF last_read_message_id, last_read_at
				ON conversation_reads
				FOR EACH ROW
				EXECUTE FUNCTION rereply_set_conversation_read_ingestion_order();
			END IF;
		END
		$block$
	`).Error; err != nil {
		return fmt.Errorf("install conversation-read ingestion-order trigger: %w", err)
	}
	if err := db.Exec(deletedMessageReadCursorCleanupFunctionSQL).Error; err != nil {
		return fmt.Errorf("create deleted-message cursor cleanup function: %w", err)
	}
	if err := db.Exec(`
		DO $block$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger
				WHERE tgname = 'trg_messages_cleanup_read_cursors'
				  AND tgrelid = 'messages'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER trg_messages_cleanup_read_cursors
				BEFORE DELETE ON messages
				FOR EACH ROW
				EXECUTE FUNCTION rereply_cleanup_deleted_message_read_cursors();
			END IF;
		END
		$block$
	`).Error; err != nil {
		return fmt.Errorf("install deleted-message cursor cleanup trigger: %w", err)
	}
	return nil
}

// PrepareProviderIntegrationManagementMode adds the mode discriminator before
// AutoMigrate. Existing and concurrently waiting old-binary inserts receive
// workspace_byo, preserving the exact pre-managed Threads behavior throughout
// a rolling deployment.
func PrepareProviderIntegrationManagementMode(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&models.ProviderIntegration{}) {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			ALTER TABLE provider_integrations
			ADD COLUMN IF NOT EXISTS management_mode varchar(32)
		`).Error; err != nil {
			return fmt.Errorf("add provider integration management mode: %w", err)
		}
		if err := tx.Exec(`
			UPDATE provider_integrations
			SET management_mode = 'workspace_byo'
			WHERE management_mode IS NULL OR pg_catalog.btrim(management_mode::text) = ''
		`).Error; err != nil {
			return fmt.Errorf("backfill provider integration management mode: %w", err)
		}
		if err := tx.Exec(`
			ALTER TABLE provider_integrations
			ALTER COLUMN management_mode SET DEFAULT 'workspace_byo',
			ALTER COLUMN management_mode SET NOT NULL
		`).Error; err != nil {
			return fmt.Errorf("enforce provider integration management mode default: %w", err)
		}
		return nil
	})
}

// PrepareMetaInstagramDeletionJournalTenant safely upgrades the unreleased
// global journal shape to tenant ownership before AutoMigrate makes
// organization_id NOT NULL. Completed rows can be recovered from their
// tenant-owned privacy request. An unresolved row without that durable link
// has no trustworthy tenant evidence, so it is removed rather than assigned
// across a tenant boundary; an authentic callback replay recreates it inside
// the deployment-configured tenant.
func PrepareMetaInstagramDeletionJournalTenant(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&models.MetaInstagramDataDeletionEvent{}) {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		// The first unreleased journal schema had no tenant column at all. Add it
		// nullable before examining rows; AutoMigrate enforces NOT NULL only after
		// every retained row has trustworthy tenant evidence.
		if err := tx.Exec(`
			ALTER TABLE meta_instagram_data_deletion_events
			ADD COLUMN IF NOT EXISTS organization_id uuid
		`).Error; err != nil {
			return fmt.Errorf("add Instagram deletion journal tenant column: %w", err)
		}
		if tx.Migrator().HasTable(&models.PrivacyRequest{}) &&
			tx.Migrator().HasColumn(&models.MetaInstagramDataDeletionEvent{}, "privacy_request_id") {
			if err := tx.Exec(`
				UPDATE meta_instagram_data_deletion_events AS event
				SET organization_id = request.organization_id
				FROM privacy_requests AS request
				WHERE event.organization_id IS NULL
				  AND event.privacy_request_id = request.id
			`).Error; err != nil {
				return fmt.Errorf("backfill Instagram deletion journal tenant: %w", err)
			}
		}
		if err := tx.Exec(`
			DELETE FROM meta_instagram_data_deletion_events
			WHERE organization_id IS NULL
		`).Error; err != nil {
			return fmt.Errorf("remove unowned Instagram deletion journal rows: %w", err)
		}
		return nil
	})
}

// BackfillProviderIntegrationBindings upgrades prepared Threads integrations
// created before the dedicated-app column existed. The unique index on
// threads_app_id makes an unsafe historical duplicate fail the deployment
// instead of silently routing one Meta app to multiple workspaces.
func BackfillProviderIntegrationBindings(db *gorm.DB) error {
	if err := db.Exec(`
		UPDATE provider_integrations
		SET threads_app_id = NULLIF(pg_catalog.btrim((config->>'app_id')::text), '')
		WHERE provider = 'threads'
		  AND threads_app_id IS NULL
		  AND NULLIF(pg_catalog.btrim((config->>'app_id')::text), '') IS NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("backfill dedicated Threads app bindings: %w", err)
	}
	return nil
}

// RunMigrationWithProgress runs migrations with a progress bar display
func RunMigrationWithProgress(db *gorm.DB, adminCfg *config.DefaultAdminConfig) error {
	return withMigrationSession(db, func(session *gorm.DB) error {
		return runMigrationWithProgressOnSession(session, adminCfg)
	})
}

func runMigrationWithProgressOnSession(db *gorm.DB, adminCfg *config.DefaultAdminConfig) error {
	return runMigrationWithProgressOnSessionUsingIndexes(db, adminCfg, getIndexes(), true)
}

func runMigrationPreparationWithProgressOnSession(
	db *gorm.DB,
	adminCfg *config.DefaultAdminConfig,
) error {
	_, preparation, _, err := splitRLSMigrationPlan(getIndexes())
	if err != nil {
		return err
	}
	return runMigrationWithProgressOnSessionUsingIndexes(db, adminCfg, preparation, false)
}

func runMigrationWithProgressOnSessionUsingIndexes(
	db *gorm.DB,
	adminCfg *config.DefaultAdminConfig,
	indexes []string,
	activateLast bool,
) error {
	// Silence GORM logging during migration
	// Start from a fresh statement while preserving the pinned ConnPool. The
	// connection callback otherwise carries scalar Raw().Scan() state into ORM
	// seed operations, leaving GORM without a valid model/table.
	silentDB := db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
		NewDB:  true,
	})

	migrationModels := GetMigrationModels()

	// Total steps: models + provider binding backfill + indexes + default admin check
	totalSteps := len(migrationModels) + len(indexes) + 2
	currentStep := 0
	barWidth := 40

	printProgress := func(step int, total int) {
		percent := float64(step) / float64(total)
		filled := int(percent * float64(barWidth))
		empty := barWidth - filled

		bar := repeatChar("█", filled) + "\033[90m" + repeatChar("░", empty) + "\033[0m"
		fmt.Printf("\r  Running migrations  %s %3d%%", bar, int(percent*100))
		_ = os.Stdout.Sync()
	}

	fmt.Println()
	if err := PrepareProviderIntegrationManagementMode(silentDB); err != nil {
		fmt.Printf("\n  \033[31mProvider management-mode preparation failed\033[0m\n\n")
		return err
	}
	if err := PrepareMessageIngestionOrder(silentDB); err != nil {
		fmt.Printf("\n  \033[31mMessage ingestion-order preparation failed\033[0m\n\n")
		return err
	}

	// Migrate models
	for _, m := range migrationModels {
		printProgress(currentStep, totalSteps)
		if m.Name == "MetaInstagramDataDeletionEvent" {
			if err := PrepareMetaInstagramDeletionJournalTenant(silentDB); err != nil {
				fmt.Printf("\n  \033[31mInstagram deletion journal tenant migration failed\033[0m\n\n")
				return err
			}
		}
		if err := silentDB.AutoMigrate(m.Model); err != nil {
			fmt.Printf("\n  \033[31m✗ Migration failed: %s\033[0m\n\n", m.Name)
			return fmt.Errorf("failed to migrate %s: %w", m.Name, err)
		}
		currentStep++
	}
	if err := InstallMessageIngestionOrderTrigger(silentDB); err != nil {
		fmt.Printf("\n  \033[31mMessage ingestion-order trigger installation failed\033[0m\n\n")
		return err
	}

	printProgress(currentStep, totalSteps)
	if err := BackfillProviderIntegrationBindings(silentDB); err != nil {
		fmt.Printf("\n  \033[31mProvider binding backfill failed\033[0m\n\n")
		return err
	}
	currentStep++

	// Create indexes
	for index, idx := range indexes {
		printProgress(currentStep, totalSteps)
		if err := executeMigrationIndexStatement(
			silentDB,
			idx,
			activateLast && index == len(indexes)-1,
		); err != nil {
			fmt.Printf("\n  \033[31m✗ Index creation failed\033[0m\n\n")
			return fmt.Errorf("failed to create index: %w", err)
		}
		currentStep++
	}

	// Seed permissions (always run, will skip if already seeded)
	printProgress(currentStep, totalSteps)
	if err := SeedPermissionsAndRoles(silentDB); err != nil {
		fmt.Printf("\n  \033[31m✗ Failed to seed permissions\033[0m\n\n")
		return err
	}

	// Fix existing organizations - link permissions to system roles if missing
	if err := SeedSystemRolesForAllOrgs(silentDB); err != nil {
		fmt.Printf("\n  \033[31m✗ Failed to fix existing role permissions\033[0m\n\n")
		return err
	}

	// Backfill user_organizations from existing users
	if err := MigrateUserOrganizations(silentDB); err != nil {
		fmt.Printf("\n  \033[31m✗ Failed to backfill user organizations\033[0m\n\n")
		return err
	}

	// Create default admin (only runs if no users exist)
	printProgress(currentStep, totalSteps)
	if err := CreateDefaultAdmin(silentDB, adminCfg); err != nil {
		fmt.Printf("\n  \033[31m✗ Setup failed\033[0m\n\n")
		return err
	}
	currentStep++

	// Initialize the reseller control plane only after the first organization
	// and platform administrator have been created.
	if err := EnsurePlatformReseller(silentDB); err != nil {
		return fmt.Errorf("failed to initialize reseller control plane: %w", err)
	}

	// Apply the versioned first-party workspace catalog backfill after the
	// commercial tables exist. It keeps legacy subscription price identities
	// intact and does not undo later control-plane retirements on restart.
	if err := EnsureReReplyProductCatalog(silentDB); err != nil {
		return fmt.Errorf("failed to initialize ReReply product catalog: %w", err)
	}

	// Seed default widgets for all organizations
	printProgress(currentStep, totalSteps)
	if err := SeedDefaultWidgets(silentDB); err != nil {
		fmt.Printf("\n  \033[31m✗ Failed to seed widgets\033[0m\n\n")
		return err
	}

	// Backfill last_inbound_at from existing messages
	if err := BackfillLastInboundAt(silentDB); err != nil {
		fmt.Printf("\n  \033[31m✗ Failed to backfill last_inbound_at\033[0m\n\n")
		return err
	}

	printProgress(currentStep, totalSteps)
	fmt.Printf("\n  \033[32m✓ Migration completed\033[0m\n\n")

	return nil
}

// repeatChar repeats a character n times
func repeatChar(char string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += char
	}
	return result
}

// getIndexes returns all index creation SQL statements
func getIndexes() []string {
	indexes := []string{
		// Expand phone_number columns to support group JIDs (e.g., 120363422675615917@g.us)
		`ALTER TABLE contacts ALTER COLUMN phone_number TYPE varchar(50)`,
		`ALTER TABLE chatbot_sessions ALTER COLUMN phone_number TYPE varchar(50)`,
		`ALTER TABLE agent_transfers ALTER COLUMN phone_number TYPE varchar(50)`,
		`ALTER TABLE bulk_message_recipients ALTER COLUMN phone_number TYPE varchar(50)`,
		// Indexes
		`CREATE INDEX IF NOT EXISTS idx_messages_contact_created ON messages(contact_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_contacts_org_phone ON contacts(organization_id, phone_number)`,
		`CREATE INDEX IF NOT EXISTS idx_contacts_assigned_read ON contacts(assigned_user_id, is_read)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_phone_status ON chatbot_sessions(organization_id, phone_number, status)`,
		`CREATE INDEX IF NOT EXISTS idx_keyword_rules_priority ON keyword_rules(organization_id, is_enabled, priority DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_transfers_active ON agent_transfers(organization_id, phone_number, status)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_transfers_org_contact ON agent_transfers(organization_id, contact_id, status)`,
		// Preserve the newest active transfer and close historical duplicates
		// before installing the concurrency invariant.
		`WITH ranked_active_transfers AS (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY organization_id, contact_id
				ORDER BY transferred_at DESC, created_at DESC, id DESC
			) AS duplicate_rank
			FROM agent_transfers
			WHERE status = 'active' AND deleted_at IS NULL
		)
		UPDATE agent_transfers AS duplicate
		SET status = 'expired',
			updated_at = NOW(),
			expires_at = COALESCE(duplicate.expires_at, NOW())
		FROM ranked_active_transfers AS ranked
		WHERE duplicate.id = ranked.id
		  AND ranked.duplicate_rank > 1`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_transfers_org_contact_active
			ON agent_transfers(organization_id, contact_id)
			WHERE status = 'active' AND deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_agent_transfers_agent_active ON agent_transfers(agent_id, status) WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS idx_agent_transfers_team ON agent_transfers(team_id, status) WHERE team_id IS NOT NULL`,
		// A WhatsApp Phone Number ID is a global webhook-routing identity, not a
		// tenant-local label. Refuse to guess an owner when legacy live rows
		// collide after normalization; operators must resolve those assignments
		// explicitly before retrying the migration.
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM whatsapp_accounts
				WHERE deleted_at IS NULL
				GROUP BY pg_catalog.btrim(phone_id::text)
				HAVING COUNT(*) > 1
			) THEN
				RAISE EXCEPTION 'cannot enforce unique live WhatsApp Phone IDs; resolve duplicate assignments before retrying';
			END IF;
		END $$`,
		// The duplicate preflight above makes this normalization lossless. Store
		// the canonical identity before adding the expression index so the prior
		// exact-match resolver remains usable during a one-click binary rollback.
		`UPDATE whatsapp_accounts
			SET phone_id = pg_catalog.btrim(phone_id::text)
			WHERE phone_id <> pg_catalog.btrim(phone_id::text)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_accounts_live_phone_id
			ON whatsapp_accounts(pg_catalog.btrim(phone_id::text))
			WHERE deleted_at IS NULL`,
		`DROP INDEX IF EXISTS idx_whatsapp_accounts_org_phone`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_templates_account_name_lang ON templates(whats_app_account, name, language)`,
		`CREATE INDEX IF NOT EXISTS idx_keyword_rules_account ON keyword_rules(whats_app_account, is_enabled, priority DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_chatbot_flows_account ON chatbot_flows(whats_app_account, is_enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_contexts_account ON ai_contexts(whats_app_account, is_enabled, priority DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_bulk_campaigns_account ON bulk_message_campaigns(whats_app_account, status)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_rules_account ON notification_rules(whats_app_account, is_enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_account ON messages(whats_app_account, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_contacts_account ON contacts(whats_app_account)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_canned_responses_org_name ON canned_responses(organization_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_canned_responses_active ON canned_responses(organization_id, is_active, usage_count DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_webhooks_org_active ON webhooks(organization_id, is_active)`,
		`CREATE INDEX IF NOT EXISTS idx_availability_logs_user_time ON user_availability_logs(user_id, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_availability_logs_org_time ON user_availability_logs(organization_id, started_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sso_providers_org_provider ON sso_providers(organization_id, provider)`,
		// Teams indexes
		`CREATE INDEX IF NOT EXISTS idx_teams_org_active ON teams(organization_id, is_active)`,
		// Create partial unique index (soft-deleted members)
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_team_members_unique ON team_members(team_id, user_id) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id)`,
		// Custom roles indexes
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_roles_org_name ON custom_roles(organization_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_custom_roles_org_system ON custom_roles(organization_id, is_system)`,
		`CREATE INDEX IF NOT EXISTS idx_custom_roles_org_default ON custom_roles(organization_id, is_default) WHERE is_default = true`,
		// GIN index for JSONB tag filtering
		`CREATE INDEX IF NOT EXISTS idx_contacts_tags ON contacts USING GIN (tags)`,
		// User organizations
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_org_unique ON user_organizations(user_id, organization_id) WHERE deleted_at IS NULL`,
		// Conversation notes
		`CREATE INDEX IF NOT EXISTS idx_conversation_notes_contact ON conversation_notes(organization_id, contact_id, created_at DESC)`,
		// Call logs
		`CREATE INDEX IF NOT EXISTS idx_call_logs_org_status ON call_logs(organization_id, status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_call_logs_contact ON call_logs(contact_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_call_logs_wa_call_id ON call_logs(whatsapp_call_id) WHERE whatsapp_call_id != ''`,
		// IVR flows
		`CREATE INDEX IF NOT EXISTS idx_ivr_flows_org_active ON ivr_flows(organization_id, whatsapp_account, is_active)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ivr_flows_org_call_start ON ivr_flows(organization_id, whatsapp_account) WHERE is_call_start = true AND is_active = true AND deleted_at IS NULL`,
		// Commercial subscriptions, onboarding, privacy, and support
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_org_live ON subscriptions(organization_id) WHERE status IN ('incomplete', 'trialing', 'active', 'past_due', 'paused') AND deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_org_number ON invoices(organization_id, number) WHERE number <> '' AND deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_privacy_requests_org_queue ON privacy_requests(organization_id, status, due_at, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_privacy_jobs_org_due ON privacy_jobs(organization_id, status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_support_cases_org_queue ON support_cases(organization_id, status, priority, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_recovery_checkpoints_org_created ON recovery_checkpoints(organization_id, created_at DESC)`,
		// CRM, follow-ups, booking, packages, and payments
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_crm_pipelines_org_default ON crm_pipelines(organization_id) WHERE is_default = true AND is_active = true AND deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_crm_pipeline_stages_order ON crm_pipeline_stages(pipeline_id, display_order) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_crm_leads_org_board ON crm_leads(organization_id, pipeline_id, stage_id, status, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_customer_activity_contact_time ON customer_activity_events(organization_id, contact_id, occurred_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_customer_activity_lead_time ON customer_activity_events(organization_id, lead_id, occurred_at DESC) WHERE lead_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_follow_up_tasks_org_due ON follow_up_tasks(organization_id, status, due_at, priority)`,
		`CREATE INDEX IF NOT EXISTS idx_booking_events_org_window ON booking_events(organization_id, resource_id, starts_at, ends_at)`,
		`CREATE INDEX IF NOT EXISTS idx_bookings_org_event_status ON bookings(organization_id, event_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_packages_org_contact ON contact_packages(organization_id, contact_id, status, expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_commerce_invoices_org_queue ON commerce_invoices(organization_id, status, due_at, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_transactions_org_created ON payment_transactions(organization_id, status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_copilot_runs_org_contact ON copilot_runs(organization_id, contact_id, created_at DESC)`,
		// Provider-neutral inbox
		`CREATE INDEX IF NOT EXISTS idx_messages_inbox_conversation ON messages(inbox_conversation_id, created_at DESC) WHERE inbox_conversation_id IS NOT NULL`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index AS index_state
				JOIN pg_catalog.pg_class AS index_class
				  ON index_class.oid = index_state.indexrelid
				JOIN pg_catalog.pg_namespace AS index_namespace
				  ON index_namespace.oid = index_class.relnamespace
				WHERE index_namespace.nspname = current_schema()
				  AND index_class.relname IN (
					'idx_messages_org_inbox_ingested',
					'idx_messages_org_inbox_ingested_highwater',
					'idx_messages_org_contact_ingested',
					'idx_messages_org_contact_account_ingested'
				  )
				  AND NOT index_state.indisvalid
			) THEN
				RAISE EXCEPTION 'an invalid message ingestion-order pagination index must be dropped concurrently before retrying migration';
			END IF;
		END $$`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_ingested ON messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id) WHERE inbox_conversation_id IS NOT NULL AND deleted_at IS NULL`,
		// The ingestion trigger serializes incoming writes on InboxConversation and
		// reads MAX(ingested_at) while holding that lock. Keep this raw high-water
		// lookup index aligned with its deleted-inclusive, non-null predicate so a
		// large transcript does not turn every inbound insert into a table scan.
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_ingested_highwater ON messages(organization_id, inbox_conversation_id, ingested_at DESC) WHERE ingested_at IS NOT NULL`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_contact_ingested ON messages(organization_id, contact_id, (COALESCE(ingested_at, created_at)), id) WHERE deleted_at IS NULL`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_contact_account_ingested ON messages(organization_id, contact_id, whats_app_account, (COALESCE(ingested_at, created_at)), id) WHERE deleted_at IS NULL`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index AS index_state
				JOIN pg_catalog.pg_class AS index_class
				  ON index_class.oid = index_state.indexrelid
				JOIN pg_catalog.pg_namespace AS index_namespace
				  ON index_namespace.oid = index_class.relnamespace
				WHERE index_namespace.nspname = current_schema()
				  AND index_class.relname = 'idx_messages_org_inbox_incoming_ingested'
				  AND NOT index_state.indisvalid
			) THEN
				RAISE EXCEPTION 'invalid index idx_messages_org_inbox_incoming_ingested must be dropped concurrently before retrying migration';
			END IF;
		END $$`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_incoming_ingested ON messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id) WHERE inbox_conversation_id IS NOT NULL AND direction = 'incoming' AND deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_inbox_conversations_org_queue ON inbox_conversations(organization_id, status, priority DESC, last_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_inbound_events_processing ON inbound_events(status, next_attempt_at, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_processing ON outbox_jobs(status, available_at, priority DESC)`,
		// Management mode is additive and defaults every legacy row to BYO. A
		// platform-managed row may select only a deployment app key and may not
		// retain any tenant-owned Threads app identity or credential material.
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_provider_integrations_management_mode') THEN
				ALTER TABLE provider_integrations
				ADD CONSTRAINT chk_provider_integrations_management_mode CHECK (
					(
						management_mode = 'workspace_byo'
						AND platform_app_key IS NULL
					) OR (
						provider = 'threads'
						AND management_mode = 'platform_managed'
						AND platform_app_key IS NOT NULL
						AND platform_app_key ~ '^[a-z][a-z0-9_-]{0,63}$'
						AND threads_app_id IS NULL
						AND NOT (credential_data ?| ARRAY['app_secret', 'webhook_verify_token'])
						AND NOT (config ?| ARRAY['app_id', 'redirect_uri', 'app_review_status', '_app_review_approval'])
					)
				);
			END IF;
		END $$`,
		// Refuse ambiguous historical ownership instead of choosing a clinic.
		// Quarantined claims remain live and continue to reserve the identity.
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM threads_platform_bindings
				WHERE deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')
				GROUP BY platform_app_id, oauth_subject_id
				HAVING COUNT(*) > 1
			) OR EXISTS (
				SELECT 1
				FROM threads_platform_bindings
				WHERE deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')
				GROUP BY platform_app_id, authority_asset_id
				HAVING COUNT(*) > 1
			) THEN
				RAISE EXCEPTION 'cannot enforce managed Threads identity ownership; resolve duplicate live claims before retrying';
			END IF;
		END $$`,
		// App keys are operator-facing aliases and may be renamed. Remove the
		// early Phase-1 alias-scoped indexes before installing immutable Meta app
		// ID ownership, otherwise a rekey could evade the no-transfer boundary.
		`DROP INDEX IF EXISTS uq_threads_platform_bindings_live_subject`,
		`DROP INDEX IF EXISTS uq_threads_platform_bindings_live_asset`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_threads_platform_bindings_live_app_subject
			ON threads_platform_bindings(platform_app_id, oauth_subject_id)
			WHERE deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_threads_platform_bindings_live_app_asset
			ON threads_platform_bindings(platform_app_id, authority_asset_id)
			WHERE deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_threads_platform_bindings_live_integration
			ON threads_platform_bindings(integration_id)
			WHERE deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_threads_platform_bindings_live_account
			ON threads_platform_bindings(channel_account_id)
			WHERE channel_account_id IS NOT NULL AND deleted_at IS NULL AND status IN ('pending', 'active', 'quarantined')`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_threads_platform_bindings_values') THEN
				ALTER TABLE threads_platform_bindings
				ADD CONSTRAINT chk_threads_platform_bindings_values CHECK (
					platform_app_key ~ '^[a-z][a-z0-9_-]{0,63}$'
					AND platform_app_id ~ '^[0-9]+$'
					AND oauth_subject_id ~ '^[0-9]+$'
					AND authority_asset_id ~ '^[0-9]+$'
					AND configuration_generation > 0
					AND authorization_generation > 0
					AND status IN ('pending', 'active', 'quarantined', 'revoked')
					AND (
						(status = 'revoked' AND released_at IS NOT NULL)
						OR (status <> 'revoked' AND released_at IS NULL)
					)
				);
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_threads_platform_journal_values') THEN
				ALTER TABLE threads_platform_event_journal
				ADD CONSTRAINT chk_threads_platform_journal_values CHECK (
					platform_app_key ~ '^[a-z][a-z0-9_-]{0,63}$'
					AND platform_app_id ~ '^[0-9]+$'
					AND event_digest ~ '^[0-9a-f]{64}$'
					AND subject_digest ~ '^[0-9a-f]{64}$'
					AND configuration_generation > 0
					AND event_type IN ('webhook', 'deauthorization', 'data_deletion')
					AND routing_state IN ('received', 'routed', 'unknown', 'ambiguous', 'quarantined', 'processed')
				);
			END IF;
		END $$`,
		// Provider identities routed outside a tenant-specific webhook path must
		// have exactly one workspace owner. Existing duplicates intentionally
		// fail migration so an operator chooses the correct owner explicitly.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_accounts_global_routable_identity
			ON channel_accounts(channel, provider, external_account_id)
			WHERE deleted_at IS NULL AND (
				(channel IN ('instagram', 'messenger') AND provider = 'relay') OR
				(channel = 'threads' AND provider = 'threads')
			)`,
		// GORM's model-level unique indexes include soft-deleted rows. Install the
		// active-row replacements before removing the legacy indexes so uniqueness
		// remains enforced while allowing soft-deleted accounts to be reconnected.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_accounts_org_name_active
			ON channel_accounts(organization_id, name)
			WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_accounts_org_external_active
			ON channel_accounts(organization_id, channel, provider, external_account_id)
			WHERE deleted_at IS NULL`,
		`DROP INDEX IF EXISTS idx_channel_accounts_org_name`,
		`DROP INDEX IF EXISTS idx_channel_accounts_external`,
		// Domain invariants that GORM's string-backed enums cannot express.
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_booking_events_window') THEN
				ALTER TABLE booking_events ADD CONSTRAINT chk_booking_events_window CHECK (ends_at > starts_at AND capacity > 0);
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_package_definitions_money') THEN
				ALTER TABLE package_definitions ADD CONSTRAINT chk_package_definitions_money CHECK (price_minor >= 0 AND validity_days > 0);
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_commerce_invoices_amounts') THEN
				ALTER TABLE commerce_invoices ADD CONSTRAINT chk_commerce_invoices_amounts CHECK (
					subtotal_minor >= 0 AND discount_minor >= 0 AND tax_minor >= 0 AND
					total_minor >= 0 AND paid_minor >= 0 AND due_minor >= 0
				);
			END IF;
		END $$`,
	}
	coexistenceStatements := coexistenceIntegrityStatements()
	if len(coexistenceStatements) < 2 {
		panic("coexistence integrity migration has no activation boundary")
	}
	coexistenceReadiness := coexistenceStatements[0]
	coexistenceActivation := coexistenceStatements[len(coexistenceStatements)-1]
	// Fail before any unrelated or preparatory DDL can wait on a hot legacy
	// relation. This tiny NOWAIT probe is repeated by the final cutover after all
	// migration work, closing the race without holding the lock during prep.
	indexes = append([]string{coexistenceReadiness}, indexes...)
	indexes = append(indexes, coexistenceStatements[1:len(coexistenceStatements)-1]...)
	indexes = append(indexes, productIntegrityStatements()...)
	// Keep the four old-core identity-review triggers behind every preparatory
	// and independently committed migration statement. Their single PostgreSQL
	// statement is atomic: a failed bridge leaves either the exact legacy
	// profile or the complete future profile, never an unstartable mixture.
	indexes = append(indexes, coexistenceActivation)
	return indexes
}

func splitMigrationIndexes(indexes []string) (preparation []string, activation string, err error) {
	if len(indexes) == 0 {
		return nil, "", errors.New("migration index inventory is empty")
	}
	activation = indexes[len(indexes)-1]
	if !strings.Contains(activation, "owner_trigger_installed boolean") ||
		!strings.Contains(activation, "CREATE TRIGGER rereply_identity_review_contact_selector_fence") {
		return nil, "", errors.New("migration final activation boundary is missing")
	}
	preparation = append([]string(nil), indexes[:len(indexes)-1]...)
	return preparation, activation, nil
}

func splitRLSMigrationPlan(
	indexes []string,
) (readiness string, preparation []string, activation string, err error) {
	preparationWithReadiness, activation, err := splitMigrationIndexes(indexes)
	if err != nil {
		return "", nil, "", err
	}
	if len(preparationWithReadiness) == 0 ||
		!strings.Contains(preparationWithReadiness[0], "IN EXCLUSIVE MODE NOWAIT") ||
		!strings.Contains(preparationWithReadiness[0], "public.contacts") {
		return "", nil, "", errors.New("migration pre-mutation NOWAIT readiness boundary is missing")
	}
	readiness = preparationWithReadiness[0]
	preparation = append([]string(nil), preparationWithReadiness[1:]...)
	return readiness, preparation, activation, nil
}

type concurrentMigrationIndexContract struct {
	Name              string
	Table             string
	Columns           string
	Predicate         string
	DependencyColumns string
	Unique            bool
}

func retryableConcurrentIndexLifecycle(valid, ready, live bool) bool {
	if valid {
		return ready && live
	}
	// CREATE INDEX CONCURRENTLY can leave either an unready/live catalog row
	// (failure before publication) or a ready/live invalid row (failure after
	// the first scan). DROP INDEX CONCURRENTLY can additionally be interrupted
	// after marking the index dead; PostgreSQL represents that as
	// invalid/unready/not-live and explicitly permits the same DROP to resume.
	return live || !ready
}

func retryableConcurrentIndexConstraintBinding(valid bool, constraintCount int64) bool {
	if constraintCount < 0 {
		return false
	}
	// A published unique index can legitimately be the referenced key for
	// foreign-key constraints. It is retained here and those exact constraints
	// are checked by the full post-preparation verifier. An invalid index is a
	// DROP candidate, so no pg_constraint row may still depend on it.
	return valid || constraintCount == 0
}

var concurrentMigrationIndexDependencyColumns = map[string]string{
	"uq_whatsapp_accounts_id_org":               "id,organization_id",
	"uq_messages_live_wamid":                    "deleted_at,inbox_conversation_id,organization_id,whats_app_message_id",
	"uq_contacts_id_org":                        "id,organization_id",
	"uq_inbound_events_identity_review_wamid":   "organization_id,protocol,provider_event_id",
	"idx_inbound_events_protocol":               "protocol",
	"idx_inbound_events_review_hold_id":         "review_hold_id",
	"idx_messages_org_inbox_ingested":           "created_at,deleted_at,id,inbox_conversation_id,ingested_at,organization_id",
	"idx_messages_org_inbox_ingested_highwater": "inbox_conversation_id,ingested_at,organization_id",
	"idx_messages_org_contact_ingested":         "contact_id,created_at,deleted_at,id,ingested_at,organization_id",
	"idx_messages_org_contact_account_ingested": "contact_id,created_at,deleted_at,id,ingested_at,organization_id,whats_app_account",
	"idx_messages_org_inbox_incoming_ingested":  "created_at,deleted_at,direction,id,inbox_conversation_id,ingested_at,organization_id",
}

func splitConcurrentMigrationIndexDefinition(definition string) (string, string, error) {
	if definition == "" || definition[0] != '(' {
		return "", "", errors.New("concurrent migration index column list is missing")
	}
	depth := 0
	inSingleQuote := false
	inDoubleQuote := false
	for index := 0; index < len(definition); index++ {
		character := definition[index]
		if inSingleQuote {
			if character == '\'' {
				if index+1 < len(definition) && definition[index+1] == '\'' {
					index++
					continue
				}
				inSingleQuote = false
			}
			continue
		}
		if inDoubleQuote {
			if character == '"' {
				if index+1 < len(definition) && definition[index+1] == '"' {
					index++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}
		switch character {
		case '\'':
			inSingleQuote = true
		case '"':
			inDoubleQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return "", "", errors.New("concurrent migration index column list is unbalanced")
			}
			if depth == 0 {
				columns := strings.TrimSpace(definition[1:index])
				tail := strings.TrimSpace(definition[index+1:])
				if columns == "" {
					return "", "", errors.New("concurrent migration index has no columns")
				}
				if tail == "" {
					return columns, "", nil
				}
				if !strings.HasPrefix(tail, "WHERE ") {
					return "", "", errors.New("concurrent migration index has an unsupported suffix")
				}
				predicate := strings.TrimSpace(strings.TrimPrefix(tail, "WHERE "))
				if predicate == "" {
					return "", "", errors.New("concurrent migration index predicate is empty")
				}
				return columns, predicate, nil
			}
		}
	}
	return "", "", errors.New("concurrent migration index column list is unterminated")
}

func splitConcurrentMigrationIndexKeys(columns string) ([]string, error) {
	columns = strings.TrimSpace(columns)
	if columns == "" {
		return nil, errors.New("concurrent migration index has no keys")
	}
	keys := make([]string, 0)
	start := 0
	depth := 0
	inSingleQuote := false
	inDoubleQuote := false
	for index := 0; index < len(columns); index++ {
		character := columns[index]
		if inSingleQuote {
			if character == '\'' {
				if index+1 < len(columns) && columns[index+1] == '\'' {
					index++
					continue
				}
				inSingleQuote = false
			}
			continue
		}
		if inDoubleQuote {
			if character == '"' {
				if index+1 < len(columns) && columns[index+1] == '"' {
					index++
					continue
				}
				inDoubleQuote = false
			}
			continue
		}
		switch character {
		case '\'':
			inSingleQuote = true
		case '"':
			inDoubleQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, errors.New("concurrent migration index key expression is unbalanced")
			}
		case ',':
			if depth == 0 {
				key := strings.TrimSpace(columns[start:index])
				if key == "" {
					return nil, errors.New("concurrent migration index has an empty key")
				}
				keys = append(keys, key)
				start = index + 1
			}
		}
	}
	if inSingleQuote || inDoubleQuote || depth != 0 {
		return nil, errors.New("concurrent migration index key expression is unterminated")
	}
	key := strings.TrimSpace(columns[start:])
	if key == "" {
		return nil, errors.New("concurrent migration index has an empty final key")
	}
	return append(keys, key), nil
}

func canonicalConcurrentMigrationIndexKeys(columns string) (string, string, error) {
	keys, err := splitConcurrentMigrationIndexKeys(columns)
	if err != nil {
		return "", "", err
	}
	canonicalKeys := make([]string, 0, len(keys))
	options := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		upper := strings.ToUpper(key)
		nullsFirst := false
		explicitNullOrder := false
		for _, suffix := range []struct {
			Text       string
			NullsFirst bool
		}{
			{Text: " NULLS FIRST", NullsFirst: true},
			{Text: " NULLS LAST", NullsFirst: false},
		} {
			if strings.HasSuffix(upper, suffix.Text) {
				key = strings.TrimSpace(key[:len(key)-len(suffix.Text)])
				upper = strings.ToUpper(key)
				nullsFirst = suffix.NullsFirst
				explicitNullOrder = true
				break
			}
		}
		descending := false
		switch {
		case strings.HasSuffix(upper, " DESC"):
			key = strings.TrimSpace(key[:len(key)-len(" DESC")])
			descending = true
		case strings.HasSuffix(upper, " ASC"):
			key = strings.TrimSpace(key[:len(key)-len(" ASC")])
		}
		if key == "" {
			return "", "", errors.New("concurrent migration index key expression is empty")
		}
		if !explicitNullOrder {
			nullsFirst = descending
		}
		option := 0
		if descending {
			option |= 1
		}
		if nullsFirst {
			option |= 2
		}
		canonical, err := canonicalizeLegacyAdditiveIndexPredicate(key)
		if err != nil {
			return "", "", err
		}
		canonicalKeys = append(canonicalKeys, canonical)
		options = append(options, fmt.Sprintf("%d", option))
	}
	return strings.Join(canonicalKeys, "\x1e"), strings.Join(options, ","), nil
}

func concurrentMigrationIndexContracts(
	indexes []string,
) ([]concurrentMigrationIndexContract, error) {
	contracts := make([]concurrentMigrationIndexContract, 0)
	seen := make(map[string]struct{})
	for _, statement := range indexes {
		normalized := canonicalSchemaDefinition(statement)
		unique := false
		var prefix string
		switch {
		case strings.HasPrefix(normalized, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "):
			prefix = "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "
			unique = true
		case strings.HasPrefix(normalized, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "):
			prefix = "CREATE INDEX CONCURRENTLY IF NOT EXISTS "
		default:
			continue
		}
		remainder := strings.TrimPrefix(normalized, prefix)
		nameEnd := strings.IndexByte(remainder, ' ')
		if nameEnd <= 0 {
			return nil, errors.New("concurrent migration index has no pinned name")
		}
		name := remainder[:nameEnd]
		remainder = strings.TrimSpace(remainder[nameEnd:])
		if !strings.HasPrefix(remainder, "ON ") {
			return nil, fmt.Errorf("concurrent migration index %q has no table binding", name)
		}
		remainder = strings.TrimSpace(strings.TrimPrefix(remainder, "ON "))
		tableEnd := strings.IndexByte(remainder, '(')
		if tableEnd <= 0 {
			return nil, fmt.Errorf("concurrent migration index %q has no column boundary", name)
		}
		table := strings.TrimSpace(remainder[:tableEnd])
		table = strings.TrimPrefix(table, "public.")
		columns, predicate, err := splitConcurrentMigrationIndexDefinition(remainder[tableEnd:])
		if err != nil {
			return nil, fmt.Errorf("parse concurrent migration index %q: %w", name, err)
		}
		if err := validateIdentifier(name); err != nil {
			return nil, fmt.Errorf("invalid concurrent migration index name %q: %w", name, err)
		}
		if err := validateIdentifier(table); err != nil {
			return nil, fmt.Errorf("invalid concurrent migration index table %q: %w", table, err)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("concurrent migration index name %q is duplicated", name)
		}
		dependencyColumns, present := concurrentMigrationIndexDependencyColumns[name]
		if !present {
			return nil, fmt.Errorf("concurrent migration index %q has no dependency manifest", name)
		}
		seen[name] = struct{}{}
		contracts = append(contracts, concurrentMigrationIndexContract{
			Name: name, Table: table, Columns: columns, Predicate: predicate,
			DependencyColumns: dependencyColumns, Unique: unique,
		})
	}
	if len(contracts) == 0 {
		return nil, errors.New("migration plan has no concurrent indexes")
	}
	if len(contracts) != len(concurrentMigrationIndexDependencyColumns) {
		return nil, fmt.Errorf(
			"concurrent migration index inventory has %d entries; expected %d",
			len(contracts),
			len(concurrentMigrationIndexDependencyColumns),
		)
	}
	return contracts, nil
}

func reconcileInvalidConcurrentMigrationIndexes(db *gorm.DB, indexes []string) error {
	contracts, err := concurrentMigrationIndexContracts(indexes)
	if err != nil {
		return err
	}
	for _, contract := range contracts {
		var relation struct {
			OID  int64  `gorm:"column:relation_oid"`
			Kind string `gorm:"column:relation_kind"`
		}
		relationResult := db.Raw(`
			SELECT
				relation.oid::bigint AS relation_oid,
				relation.relkind::text AS relation_kind
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS relation_namespace
			  ON relation_namespace.oid = relation.relnamespace
			WHERE relation_namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
		`, contract.Name).Scan(&relation)
		if relationResult.Error != nil {
			return fmt.Errorf("inspect retryable concurrent relation %q: %w", contract.Name, relationResult.Error)
		}
		if relationResult.RowsAffected == 0 {
			continue
		}
		if relationResult.RowsAffected != 1 || relation.Kind != "i" {
			return fmt.Errorf("pinned concurrent index %q is not an exact retry artifact", contract.Name)
		}

		var state struct {
			IndexOID        int64  `gorm:"column:index_oid"`
			TableOID        int64  `gorm:"column:table_oid"`
			RelationKind    string `gorm:"column:relation_kind"`
			Persistence     string `gorm:"column:persistence"`
			OwnerIsCurrent  bool   `gorm:"column:owner_is_current"`
			AccessMethod    string `gorm:"column:access_method"`
			TableSchema     string `gorm:"column:table_schema"`
			TableName       string `gorm:"column:table_name"`
			Columns         string `gorm:"column:columns"`
			SortOptions     string `gorm:"column:sort_options"`
			Predicate       string `gorm:"column:predicate"`
			Valid           bool   `gorm:"column:valid"`
			Ready           bool   `gorm:"column:ready"`
			Live            bool   `gorm:"column:live"`
			Unique          bool   `gorm:"column:unique_index"`
			MetadataExact   bool   `gorm:"column:metadata_exact"`
			ConstraintCount int64  `gorm:"column:constraint_count"`
		}
		result := db.Raw(`
			SELECT
				index_relation.oid::bigint AS index_oid,
				table_relation.oid::bigint AS table_oid,
				index_relation.relkind::text AS relation_kind,
				index_relation.relpersistence::text AS persistence,
				index_owner.rolname = current_user AS owner_is_current,
				access_method.amname::text AS access_method,
				table_namespace.nspname::text AS table_schema,
				table_relation.relname::text AS table_name,
				COALESCE((
					SELECT pg_catalog.string_agg(
						pg_catalog.pg_get_indexdef(index_state.indexrelid, key.ordinality::integer, true),
						',' ORDER BY key.ordinality
					)
					FROM pg_catalog.unnest(index_state.indkey::smallint[]) WITH ORDINALITY
						AS key(attnum, ordinality)
				), '') AS columns,
				COALESCE(pg_catalog.array_to_string(index_state.indoption::smallint[], ','), '') AS sort_options,
				COALESCE(pg_catalog.pg_get_expr(index_state.indpred, index_state.indrelid, false), '') AS predicate,
				index_state.indisvalid AS valid,
				index_state.indisready AS ready,
				index_state.indislive AS live,
				index_state.indisunique AS unique_index,
				index_state.indimmediate AND NOT index_state.indisprimary AND NOT index_state.indisexclusion
					AND NOT index_state.indisclustered AND NOT index_state.indisreplident
					AND NOT index_state.indcheckxmin
					AND index_state.indnkeyatts = index_state.indnatts
					AND index_relation.reloptions IS NULL AND index_relation.relacl IS NULL
					AND NOT COALESCE(
						(pg_catalog.to_jsonb(index_state)->>'indnullsnotdistinct')::boolean,
						false
					)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(index_state.indclass::oid[]) AS class(oid)
						JOIN pg_catalog.pg_opclass AS operator_class ON operator_class.oid = class.oid
						JOIN pg_catalog.pg_namespace AS class_namespace
						  ON class_namespace.oid = operator_class.opcnamespace
						WHERE NOT operator_class.opcdefault OR class_namespace.nspname <> 'pg_catalog'
					)
					AND NOT EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(index_state.indcollation::oid[]) AS index_collation(oid)
						LEFT JOIN pg_catalog.pg_collation AS resolved ON resolved.oid = index_collation.oid
						LEFT JOIN pg_catalog.pg_namespace AS resolved_namespace
						  ON resolved_namespace.oid = resolved.collnamespace
						WHERE index_collation.oid <> 0 AND (
							resolved_namespace.nspname <> 'pg_catalog' OR resolved.collname <> 'default'
						)
					) AS metadata_exact,
				(SELECT COUNT(*) FROM pg_catalog.pg_constraint AS constraint_state
				 WHERE constraint_state.conindid = index_relation.oid) AS constraint_count
			FROM pg_catalog.pg_class AS index_relation
			JOIN pg_catalog.pg_namespace AS index_namespace
			  ON index_namespace.oid = index_relation.relnamespace
			JOIN pg_catalog.pg_roles AS index_owner ON index_owner.oid = index_relation.relowner
			JOIN pg_catalog.pg_am AS access_method ON access_method.oid = index_relation.relam
			JOIN pg_catalog.pg_index AS index_state ON index_state.indexrelid = index_relation.oid
			JOIN pg_catalog.pg_class AS table_relation ON table_relation.oid = index_state.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
			  ON table_namespace.oid = table_relation.relnamespace
			WHERE index_namespace.nspname = 'public'
			  AND index_relation.relname = CAST(? AS text)
			  AND index_relation.oid::bigint = ?
		`, contract.Name, relation.OID).Scan(&state)
		if result.Error != nil {
			return fmt.Errorf("inspect retryable concurrent index %q: %w", contract.Name, result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("pinned concurrent index %q is not an exact retry artifact", contract.Name)
		}
		actualColumns, _, err := canonicalConcurrentMigrationIndexKeys(state.Columns)
		if err != nil {
			return fmt.Errorf("parse concurrent migration index %q columns: %w", contract.Name, err)
		}
		expectedColumns, expectedSortOptions, err := canonicalConcurrentMigrationIndexKeys(contract.Columns)
		if err != nil {
			return fmt.Errorf("parse expected concurrent migration index %q columns: %w", contract.Name, err)
		}
		actualPredicate, err := canonicalizeLegacyAdditiveIndexPredicate(state.Predicate)
		if err != nil {
			return fmt.Errorf("parse concurrent migration index %q predicate: %w", contract.Name, err)
		}
		expectedPredicate, err := canonicalizeLegacyAdditiveIndexPredicate(contract.Predicate)
		if err != nil {
			return fmt.Errorf("parse expected concurrent migration index %q predicate: %w", contract.Name, err)
		}
		if state.RelationKind != "i" || state.Persistence != "p" ||
			!state.OwnerIsCurrent || state.AccessMethod != "btree" || state.TableSchema != "public" ||
			state.TableName != contract.Table || state.Unique != contract.Unique ||
			!state.MetadataExact ||
			!retryableConcurrentIndexConstraintBinding(state.Valid, state.ConstraintCount) ||
			!retryableConcurrentIndexLifecycle(state.Valid, state.Ready, state.Live) ||
			actualColumns != expectedColumns || expectedSortOptions != state.SortOptions ||
			actualPredicate != expectedPredicate {
			return fmt.Errorf(
				"pinned concurrent index %q is not an exact retry artifact "+
					"(kind=%q persistence=%q owner_current=%t access_method=%q table=%q.%q "+
					"unique=%t metadata_exact=%t constraints=%d lifecycle=%t/%t/%t "+
					"columns=%q expected_columns=%q sort_options=%q expected_sort_options=%q "+
					"predicate=%q expected_predicate=%q)",
				contract.Name,
				state.RelationKind,
				state.Persistence,
				state.OwnerIsCurrent,
				state.AccessMethod,
				state.TableSchema,
				state.TableName,
				state.Unique,
				state.MetadataExact,
				state.ConstraintCount,
				state.Valid,
				state.Ready,
				state.Live,
				state.Columns,
				contract.Columns,
				state.SortOptions,
				expectedSortOptions,
				state.Predicate,
				contract.Predicate,
			)
		}
		if err := verifyLegacyIndexDependencies(
			db,
			state.IndexOID,
			state.TableOID,
			contract.DependencyColumns,
			0,
		); err != nil {
			return fmt.Errorf(
				"concurrent migration index %q dependency binding is not exact: %w",
				contract.Name,
				err,
			)
		}
		if state.Valid {
			continue
		}
		if err := db.Exec(
			"DROP INDEX CONCURRENTLY public." + quoteIdentifier(contract.Name),
		).Error; err != nil {
			return fmt.Errorf("drop invalid concurrent index %q for exact retry: %w", contract.Name, err)
		}
	}
	return nil
}

// CreateIndexes creates additional indexes not handled by GORM tags
func CreateIndexes(db *gorm.DB) error {
	return withMigrationSession(db, createIndexesOnSession)
}

func executeMigrationIndexStatement(db *gorm.DB, statement string, coexistenceActivation bool) error {
	if !coexistenceActivation {
		return db.Exec(statement).Error
	}

	// Keep the complete old-core cutover in one explicit transaction. Its
	// EXCLUSIVE NOWAIT locks fail promptly under live-write contention, while
	// historical verification and backfill are allowed to run for the time
	// required by the database size. Any statement error rolls everything back.
	return db.Transaction(func(tx *gorm.DB) error {
		return tx.Exec(statement).Error
	})
}

func createIndexesOnSession(db *gorm.DB) error {
	indexes := getIndexes()
	for index, idx := range indexes {
		err := executeMigrationIndexStatement(db, idx, index == len(indexes)-1)
		if err != nil {
			return fmt.Errorf("failed to create index: %w", err)
		}
	}
	return nil
}

// CreateDefaultAdmin creates a default admin user only for a completely new
// installation. Existing users, including soft-deleted users, must never be
// replaced or recreated implicitly by a migration.
func CreateDefaultAdmin(db *gorm.DB, cfg *config.DefaultAdminConfig) error {
	var userCount int64
	if err := db.Unscoped().Model(&models.User{}).Count(&userCount).Error; err != nil {
		return fmt.Errorf("failed to check existing users: %w", err)
	}
	if userCount > 0 {
		return nil
	}

	// Find an ordinary organization, or create ReReply when the installation
	// contains only atomic platform-compliance control-plane organizations.
	var org models.Organization
	err := db.Scopes(ExcludePlatformComplianceOrganizations).First(&org).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// No organizations exist, create default one
		org = models.Organization{
			BaseModel: models.BaseModel{ID: uuid.New()},
			Name:      "ReReply",
			Settings:  models.JSONB{},
		}
		if err := db.Create(&org).Error; err != nil {
			return fmt.Errorf("failed to create default organization: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to find an ordinary organization: %w", err)
	}

	// Hash the default password from config
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Seed permissions if not exist
	if err := SeedPermissionsAndRoles(db); err != nil {
		return fmt.Errorf("failed to seed permissions: %w", err)
	}

	// Seed system roles for this organization if not exist
	if err := SeedSystemRolesForOrg(db, org.ID); err != nil {
		return fmt.Errorf("failed to seed system roles: %w", err)
	}

	// Get admin system role for the organization
	var adminRole models.CustomRole
	if err := db.Where("organization_id = ? AND name = ? AND is_system = ?", org.ID, "admin", true).First(&adminRole).Error; err != nil {
		return fmt.Errorf("failed to find admin role: %w", err)
	}

	// Create default admin user (super admin for cross-organization access)
	admin := models.User{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Email:          cfg.Email,
		PasswordHash:   string(passwordHash),
		FullName:       cfg.FullName,
		RoleID:         &adminRole.ID,
		IsActive:       true,
		IsAvailable:    true,
		IsSuperAdmin:   true,
		Settings:       models.JSONB{},
	}
	if err := db.Create(&admin).Error; err != nil {
		return fmt.Errorf("failed to create default admin user: %w", err)
	}

	// Create UserOrganization entry for the default admin
	userOrg := models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         admin.ID,
		OrganizationID: org.ID,
		RoleID:         &adminRole.ID,
		IsDefault:      true,
	}
	if err := db.Create(&userOrg).Error; err != nil {
		return fmt.Errorf("failed to create user organization entry: %w", err)
	}

	return nil
}

// MigrateUserOrganizations backfills user_organizations from existing users
func MigrateUserOrganizations(db *gorm.DB) error {
	return db.Exec(`
		INSERT INTO user_organizations (id, user_id, organization_id, role_id, is_default, created_at, updated_at)
		SELECT gen_random_uuid(), u.id, u.organization_id, u.role_id, true, NOW(), NOW()
		FROM users u
		LEFT JOIN user_organizations uo ON uo.user_id = u.id AND uo.organization_id = u.organization_id AND uo.deleted_at IS NULL
		WHERE uo.id IS NULL AND u.deleted_at IS NULL
	`).Error
}

const PlatformResellerSlug = "platform-direct"

// EnsurePlatformReseller creates the first-party reseller, assigns every
// legacy organization to it, and records platform super administrators as
// its owners. The operation is idempotent for both fresh installs and
// upgrades.
func EnsurePlatformReseller(db *gorm.DB) error {
	if db == nil {
		return errors.New("database is required")
	}

	var reseller models.Reseller
	err := db.Unscoped().Where("slug = ?", PlatformResellerSlug).First(&reseller).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		reseller = models.Reseller{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			Name:             "Platform Direct",
			Slug:             PlatformResellerSlug,
			Status:           models.ResellerStatusActive,
			Plan:             models.ResellerPlanEnterprise,
			MaxOrganizations: 10000,
			BrandName:        "ReReply",
			PrimaryColor:     "#0f766e",
			AccentColor:      "#f59e0b",
			Settings:         models.JSONB{},
		}
		if err := db.Create(&reseller).Error; err != nil {
			return fmt.Errorf("create platform reseller: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("find platform reseller: %w", err)
	} else if reseller.DeletedAt.Valid || reseller.Status != models.ResellerStatusActive {
		if err := db.Unscoped().Model(&reseller).Updates(map[string]any{
			"deleted_at": nil,
			"status":     models.ResellerStatusActive,
		}).Error; err != nil {
			return fmt.Errorf("restore platform reseller: %w", err)
		}
	}

	if err := db.Model(&models.Organization{}).
		Where("reseller_id IS NULL").
		Update("reseller_id", reseller.ID).Error; err != nil {
		return fmt.Errorf("backfill organization resellers: %w", err)
	}

	var owners []models.User
	if err := db.Where("is_super_admin = ? AND is_active = ?", true, true).Find(&owners).Error; err != nil {
		return fmt.Errorf("list platform owners: %w", err)
	}
	for _, owner := range owners {
		var membership models.ResellerMember
		err := db.Unscoped().
			Where("reseller_id = ? AND user_id = ?", reseller.ID, owner.ID).
			First(&membership).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			membership = models.ResellerMember{
				BaseModel:  models.BaseModel{ID: uuid.New()},
				ResellerID: reseller.ID,
				UserID:     owner.ID,
				Role:       models.ResellerRoleOwner,
				IsActive:   true,
			}
			if err := db.Create(&membership).Error; err != nil {
				return fmt.Errorf("create platform owner membership: %w", err)
			}
		case err != nil:
			return fmt.Errorf("find platform owner membership: %w", err)
		default:
			if err := db.Unscoped().Model(&membership).Updates(map[string]any{
				"deleted_at": nil,
				"role":       models.ResellerRoleOwner,
				"is_active":  true,
			}).Error; err != nil {
				return fmt.Errorf("restore platform owner membership: %w", err)
			}
		}
	}

	return nil
}

// BackfillLastInboundAt sets last_inbound_at for existing contacts from their
// most recent incoming message. Only updates contacts where the field is NULL.
func BackfillLastInboundAt(db *gorm.DB) error {
	return db.Exec(`
		UPDATE contacts c
		SET last_inbound_at = sub.max_created
		FROM (
			SELECT contact_id, MAX(created_at) AS max_created
			FROM messages
			WHERE direction = 'incoming' AND deleted_at IS NULL
			GROUP BY contact_id
		) sub
		WHERE c.id = sub.contact_id AND c.last_inbound_at IS NULL AND c.deleted_at IS NULL
	`).Error
}

// SeedPermissionsAndRoles seeds the default permissions and system roles
func SeedPermissionsAndRoles(db *gorm.DB) error {
	// Get all default permissions
	defaultPerms := models.DefaultPermissions()

	// Add any missing permissions
	for _, perm := range defaultPerms {
		var existing models.Permission
		if err := db.Where("resource = ? AND action = ?", perm.Resource, perm.Action).First(&existing).Error; err != nil {
			// Permission doesn't exist, create it
			perm.ID = uuid.New()
			if err := db.Create(&perm).Error; err != nil {
				return fmt.Errorf("failed to create permission %s:%s: %w", perm.Resource, perm.Action, err)
			}
		}
	}

	return nil
}

// SeedSystemRolesForAllOrgs creates system roles for all existing organizations
// This is idempotent - it skips organizations that already have system roles
func SeedSystemRolesForAllOrgs(db *gorm.DB) error {
	var orgs []models.Organization
	if err := db.Scopes(ExcludePlatformComplianceOrganizations).Find(&orgs).Error; err != nil {
		return fmt.Errorf("failed to fetch organizations: %w", err)
	}

	for _, org := range orgs {
		if err := SeedSystemRolesForOrg(db, org.ID); err != nil {
			return fmt.Errorf("failed to seed roles for org %s: %w", org.ID, err)
		}
	}

	// Fix any system roles that don't have permissions linked
	if err := FixSystemRolePermissions(db); err != nil {
		return fmt.Errorf("failed to fix role permissions: %w", err)
	}

	// Remove the legacy sensitive Settings grants from system manager roles
	// exactly once per organization. The version marker prevents a later
	// explicit super-admin grant from being removed on restart.
	if err := ApplyManagerSettingsPolicyMigration(db); err != nil {
		return fmt.Errorf("failed to apply manager settings policy: %w", err)
	}

	// Migrate existing users from old role column to new role_id
	if err := MigrateExistingUserRoles(db); err != nil {
		return fmt.Errorf("failed to migrate user roles: %w", err)
	}

	// Make admin@admin.com a super admin if exists
	if err := db.Exec("UPDATE users SET is_super_admin = true WHERE email = 'admin@admin.com'").Error; err != nil {
		return fmt.Errorf("failed to set super admin: %w", err)
	}

	return nil
}

// FixSystemRolePermissions links permissions to existing system roles.
//
// Empty roles receive their complete system definition. Existing roles receive
// only expected permissions that were introduced after the role was last
// updated. This upgrades older organizations without restoring permissions a
// platform owner deliberately removed from a system role later.
func FixSystemRolePermissions(db *gorm.DB) error {
	// Get all permissions from database
	var permissions []models.Permission
	if err := db.Find(&permissions).Error; err != nil {
		return fmt.Errorf("failed to fetch permissions: %w", err)
	}

	if len(permissions) == 0 {
		return nil // No permissions to link
	}

	// Create permission map for lookup
	permMap := make(map[string]models.Permission)
	for _, p := range permissions {
		permMap[p.Resource+":"+p.Action] = p
	}

	// Get system role permission mappings
	rolePermissions := models.SystemRolePermissions()

	// Find system roles without permissions
	var systemRoles []models.CustomRole
	if err := db.Where("is_system = ?", true).Find(&systemRoles).Error; err != nil {
		return fmt.Errorf("failed to fetch system roles: %w", err)
	}

	for _, role := range systemRoles {
		permKeys, ok := rolePermissions[role.Name]
		if !ok {
			continue // Unknown role name
		}

		var currentPermissions []models.Permission
		if err := db.Model(&role).
			Association("Permissions").
			Find(&currentPermissions); err != nil {
			return fmt.Errorf("failed to load permissions for role %s: %w", role.Name, err)
		}
		currentPermissionIDs := make(map[uuid.UUID]struct{}, len(currentPermissions))
		for _, permission := range currentPermissions {
			currentPermissionIDs[permission.ID] = struct{}{}
		}

		// An empty role is repaired in full. A populated role receives only
		// permissions created at or after its last explicit update.
		var permsToAdd []models.Permission
		for _, key := range permKeys {
			permission, exists := permMap[key]
			if !exists {
				continue
			}
			if _, exists := currentPermissionIDs[permission.ID]; exists {
				continue
			}
			if len(currentPermissions) == 0 ||
				!permission.CreatedAt.Before(role.UpdatedAt) {
				permsToAdd = append(permsToAdd, permission)
			}
		}

		if len(permsToAdd) > 0 {
			if err := db.Model(&role).Association("Permissions").Append(permsToAdd); err != nil {
				return fmt.Errorf("failed to link permissions to role %s: %w", role.Name, err)
			}
			if err := db.Model(&models.CustomRole{}).
				Where("id = ?", role.ID).
				UpdateColumn("updated_at", time.Now().UTC()).Error; err != nil {
				return fmt.Errorf("failed to record permission upgrade for role %s: %w", role.Name, err)
			}
		}
	}

	return nil
}

// MigrateExistingUserRoles migrates users from the old role column to the new role_id
// This is safe to run on fresh installs - it will simply do nothing if the column doesn't exist
func MigrateExistingUserRoles(db *gorm.DB) error {
	// Check if the old 'role' column exists in the users table
	var columnExists bool
	err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'role'
		)
	`).Scan(&columnExists).Error
	if err != nil {
		return fmt.Errorf("failed to check for role column: %w", err)
	}

	if !columnExists {
		return nil // Fresh install, no old role column
	}

	// Get users who have old role but no role_id assigned
	type UserWithLegacyRole struct {
		ID             uuid.UUID
		OrganizationID uuid.UUID
		LegacyRole     string
	}

	var usersToMigrate []UserWithLegacyRole
	err = db.Raw(`
		SELECT id, organization_id, role as legacy_role
		FROM users
		WHERE role_id IS NULL AND role IS NOT NULL AND role != ''
	`).Scan(&usersToMigrate).Error
	if err != nil {
		return fmt.Errorf("failed to fetch users with legacy roles: %w", err)
	}

	if len(usersToMigrate) == 0 {
		return nil // No users to migrate
	}

	// Get all system roles grouped by organization
	var systemRoles []models.CustomRole
	if err := db.Where("is_system = ?", true).Find(&systemRoles).Error; err != nil {
		return fmt.Errorf("failed to fetch system roles: %w", err)
	}

	// Create lookup: orgID -> roleName -> roleID
	roleMap := make(map[uuid.UUID]map[string]uuid.UUID)
	for _, role := range systemRoles {
		if roleMap[role.OrganizationID] == nil {
			roleMap[role.OrganizationID] = make(map[string]uuid.UUID)
		}
		roleMap[role.OrganizationID][role.Name] = role.ID
	}

	// Migrate each user
	for _, user := range usersToMigrate {
		orgRoles, ok := roleMap[user.OrganizationID]
		if !ok {
			continue // Organization doesn't have system roles yet
		}

		roleID, ok := orgRoles[user.LegacyRole]
		if !ok {
			continue // Role not found (shouldn't happen for admin/manager/agent)
		}

		// Update user's role_id
		if err := db.Exec("UPDATE users SET role_id = ? WHERE id = ?", roleID, user.ID).Error; err != nil {
			return fmt.Errorf("failed to update user %s role: %w", user.ID, err)
		}
	}

	return nil
}

// SeedSystemRolesForOrg creates system roles for an organization
func SeedSystemRolesForOrg(db *gorm.DB, orgID uuid.UUID) error {
	// Check if system roles exist for this org
	var roleCount int64
	if err := db.Model(&models.CustomRole{}).Where("organization_id = ? AND is_system = ?", orgID, true).Count(&roleCount).Error; err != nil {
		return fmt.Errorf("failed to count roles: %w", err)
	}

	if roleCount > 0 {
		return nil // Already seeded
	}

	// Get all permissions from database
	var permissions []models.Permission
	if err := db.Find(&permissions).Error; err != nil {
		return fmt.Errorf("failed to fetch permissions: %w", err)
	}

	// Create permission map for lookup
	permMap := make(map[string]models.Permission)
	for _, p := range permissions {
		permMap[p.Resource+":"+p.Action] = p
	}

	// Get system role permission mappings
	rolePermissions := models.SystemRolePermissions()

	// Create system roles
	systemRoles := []struct {
		Name        string
		Description string
		IsDefault   bool
	}{
		{"admin", "Full system access", false},
		{"manager", "Manage chatbot, campaigns, and team operations", false},
		{"agent", "Handle customer conversations", true},
	}

	for _, sr := range systemRoles {
		role := models.CustomRole{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			Name:           sr.Name,
			Description:    sr.Description,
			IsSystem:       true,
			IsDefault:      sr.IsDefault,
		}

		// Add permissions
		permKeys := rolePermissions[sr.Name]
		for _, key := range permKeys {
			if perm, ok := permMap[key]; ok {
				role.Permissions = append(role.Permissions, perm)
			}
		}

		if err := db.Create(&role).Error; err != nil {
			return fmt.Errorf("failed to create %s role: %w", sr.Name, err)
		}
	}

	// Fresh roles already use the current policy. Mark the organization now so
	// an explicit grant made later is never mistaken for a legacy default.
	var organization models.Organization
	if err := db.Where("id = ?", orgID).First(&organization).Error; err != nil {
		return fmt.Errorf("failed to load organization for manager settings policy: %w", err)
	}
	if err := markManagerSettingsPolicyVersion(db, &organization); err != nil {
		return fmt.Errorf("failed to mark manager settings policy: %w", err)
	}
	return nil
}

// SeedDefaultWidgets creates default dashboard widgets for all organizations
func SeedDefaultWidgets(db *gorm.DB) error {
	// Find a super admin without coupling the seed to a legacy email address.
	var superAdmin models.User
	if err := db.Where("is_super_admin = ?", true).Order("created_at ASC").First(&superAdmin).Error; err != nil {
		// No super admin exists yet, skip widget creation
		return nil
	}

	// Get all organizations
	var orgs []models.Organization
	if err := db.Scopes(ExcludePlatformComplianceOrganizations).Find(&orgs).Error; err != nil {
		return fmt.Errorf("failed to fetch organizations: %w", err)
	}

	for _, org := range orgs {
		// Skip orgs that already have widgets
		var exists int64
		db.Model(&models.Widget{}).Where("organization_id = ?", org.ID).Count(&exists)
		if exists > 0 {
			continue
		}

		if err := SeedDefaultWidgetsForOrg(db, org.ID, superAdmin.ID); err != nil {
			return err
		}
	}

	return nil
}

// SeedDefaultWidgetsForOrg creates default dashboard widgets for a single organization.
// Used when a new organization is created at runtime.
func SeedDefaultWidgetsForOrg(db *gorm.DB, orgID, userID uuid.UUID) error {
	defaultWidgetsData := []struct {
		Name         string
		Description  string
		DataSource   string
		DisplayType  string
		Color        string
		Config       models.JSONB
		DisplayOrder int
		GridX        int
		GridY        int
		GridW        int
		GridH        int
	}{
		{"Total Messages", "Total number of messages sent and received", "messages", "number", "blue", nil, 1, 0, 0, 3, 3},
		{"Active Contacts", "Number of contacts with recent activity", "contacts", "number", "green", nil, 2, 3, 0, 3, 3},
		{"Chatbot Sessions", "Active chatbot conversation sessions", "sessions", "number", "purple", nil, 3, 6, 0, 3, 3},
		{"Total Campaigns", "Number of bulk message campaigns", "campaigns", "number", "orange", nil, 4, 9, 0, 3, 3},
		{"Recent Messages", "Latest conversations from your contacts", "messages", "table", "", nil, 5, 0, 3, 6, 8},
		{"Quick Actions", "Common tasks and shortcuts", "shortcuts", "shortcuts", "", models.JSONB{"shortcuts": []any{"chat", "campaigns", "templates", "chatbot"}}, 6, 6, 3, 6, 8},
	}

	for _, wd := range defaultWidgetsData {
		displayType := wd.DisplayType
		if displayType == "" {
			displayType = "number"
		}
		widget := models.Widget{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			UserID:         &userID,
			Name:           wd.Name,
			Description:    wd.Description,
			DataSource:     wd.DataSource,
			Metric:         "count",
			DisplayType:    displayType,
			ShowChange:     displayType == "number",
			Color:          wd.Color,
			Size:           "small",
			Config:         wd.Config,
			DisplayOrder:   wd.DisplayOrder,
			GridX:          wd.GridX,
			GridY:          wd.GridY,
			GridW:          wd.GridW,
			GridH:          wd.GridH,
			IsShared:       true,
			IsDefault:      true,
		}
		if err := db.Create(&widget).Error; err != nil {
			return fmt.Errorf("failed to create widget %s: %w", wd.Name, err)
		}
	}

	return nil
}
