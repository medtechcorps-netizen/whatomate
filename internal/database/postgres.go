package database

import (
	"errors"
	"fmt"
	"os"
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
	dsn := postgresDSN(cfg)

	logLevel := logger.Silent
	if debug {
		logLevel = logger.Info
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
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
		{"MetaAnalyticsSnapshot", &models.MetaAnalyticsSnapshot{}},
		{"Contact", &models.Contact{}},
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
		{"ChannelAccount", &models.ChannelAccount{}},
		{"ChannelCredential", &models.ChannelCredential{}},
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
	migrationModels := GetMigrationModels()
	for _, m := range migrationModels {
		if err := db.AutoMigrate(m.Model); err != nil {
			return err
		}
	}
	return nil
}

// RunMigrationWithProgress runs migrations with a progress bar display
func RunMigrationWithProgress(db *gorm.DB, adminCfg *config.DefaultAdminConfig) error {
	// Silence GORM logging during migration
	silentDB := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})

	migrationModels := GetMigrationModels()
	indexes := getIndexes()

	// Total steps: models + indexes + default admin check
	totalSteps := len(migrationModels) + len(indexes) + 1
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

	// Migrate models
	for _, m := range migrationModels {
		printProgress(currentStep, totalSteps)
		if err := silentDB.AutoMigrate(m.Model); err != nil {
			fmt.Printf("\n  \033[31m✗ Migration failed: %s\033[0m\n\n", m.Name)
			return fmt.Errorf("failed to migrate %s: %w", m.Name, err)
		}
		currentStep++
	}

	// Create indexes
	for _, idx := range indexes {
		printProgress(currentStep, totalSteps)
		if err := silentDB.Exec(idx).Error; err != nil {
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
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_whatsapp_accounts_org_phone ON whatsapp_accounts(organization_id, phone_id)`,
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
		`CREATE INDEX IF NOT EXISTS idx_inbox_conversations_org_queue ON inbox_conversations(organization_id, status, priority DESC, last_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_inbound_events_processing ON inbound_events(status, next_attempt_at, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_processing ON outbox_jobs(status, available_at, priority DESC)`,
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
	return append(indexes, productIntegrityStatements()...)
}

// CreateIndexes creates additional indexes not handled by GORM tags
func CreateIndexes(db *gorm.DB) error {
	for _, idx := range getIndexes() {
		if err := db.Exec(idx).Error; err != nil {
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

	// Find any existing organization, or create the ReReply organization if none exist
	var org models.Organization
	if err := db.First(&org).Error; err != nil {
		// No organizations exist, create default one
		org = models.Organization{
			BaseModel: models.BaseModel{ID: uuid.New()},
			Name:      "ReReply",
			Settings:  models.JSONB{},
		}
		if err := db.Create(&org).Error; err != nil {
			return fmt.Errorf("failed to create default organization: %w", err)
		}
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

const platformResellerSlug = "platform-direct"

// EnsurePlatformReseller creates the first-party reseller, assigns every
// legacy organization to it, and records platform super administrators as
// its owners. The operation is idempotent for both fresh installs and
// upgrades.
func EnsurePlatformReseller(db *gorm.DB) error {
	if db == nil {
		return errors.New("database is required")
	}

	var reseller models.Reseller
	err := db.Unscoped().Where("slug = ?", platformResellerSlug).First(&reseller).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		reseller = models.Reseller{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			Name:             "Platform Direct",
			Slug:             platformResellerSlug,
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
	if err := db.Find(&orgs).Error; err != nil {
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
	if err := db.Find(&orgs).Error; err != nil {
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
