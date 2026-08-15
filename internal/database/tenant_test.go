package database_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestWithTenantReadCommittedOverridesSessionDefault(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organizationID := uuid.New()

	require.NoError(t, db.Connection(func(connection *gorm.DB) error {
		if err := connection.Exec(
			"SET default_transaction_isolation TO 'repeatable read'",
		).Error; err != nil {
			return err
		}
		defer func() {
			_ = connection.Exec("RESET default_transaction_isolation").Error
		}()

		return database.WithTenantReadCommitted(
			connection,
			organizationID,
			func(tx *gorm.DB) error {
				var isolation string
				if err := tx.Raw("SHOW transaction_isolation").
					Scan(&isolation).Error; err != nil {
					return err
				}
				if isolation != "read committed" {
					return fmt.Errorf(
						"transaction isolation is %q; expected read committed",
						isolation,
					)
				}
				return nil
			},
		)
	}))
}

func TestTenantRLS_FailsClosedAcrossOrganizations(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)

	// A unique, non-login role keeps this test isolated from concurrent CI
	// runs. SET LOCAL ROLE exercises the same PostgreSQL permissions and RLS
	// behavior as the production runtime login without putting a password in
	// the test environment.
	runtimeRole := "rereply_rls_" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
	)).Error)

	t.Cleanup(func() {
		// Cleanup is deliberately best-effort so the original assertion remains
		// visible if a policy test fails.
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	orgA := models.Organization{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: &reseller.ID,
		Name:       "Clinic Alpha",
		Slug:       "clinic-alpha-" + uuid.NewString()[:8],
		Settings:   models.JSONB{},
	}
	orgB := models.Organization{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: &reseller.ID,
		Name:       "Clinic Beta",
		Slug:       "clinic-beta-" + uuid.NewString()[:8],
		Settings:   models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&orgA).Error)
	require.NoError(t, adminDB.Create(&orgB).Error)

	accountA := models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgA.ID,
		Name:               "clinic-alpha",
		PhoneID:            "phone-alpha-" + uuid.NewString(),
		BusinessID:         "waba-alpha-" + uuid.NewString(),
		AccessToken:        "encrypted-test-token-a",
		WebhookVerifyToken: "verify-alpha-" + uuid.NewString(),
	}
	accountB := models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgB.ID,
		Name:               "clinic-beta",
		PhoneID:            "phone-beta-" + uuid.NewString(),
		BusinessID:         "waba-beta-" + uuid.NewString(),
		AccessToken:        "encrypted-test-token-b",
		WebhookVerifyToken: "verify-beta-" + uuid.NewString(),
	}
	require.NoError(t, adminDB.Create(&accountA).Error)
	require.NoError(t, adminDB.Create(&accountB).Error)

	contactA := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		PhoneNumber:     "+601100000001",
		ProfileName:     "Alpha Patient",
		WhatsAppAccount: accountA.Name,
	}
	contactB := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		PhoneNumber:     "+601100000002",
		ProfileName:     "Beta Patient",
		WhatsAppAccount: accountB.Name,
	}
	require.NoError(t, adminDB.Create(&contactA).Error)
	require.NoError(t, adminDB.Create(&contactB).Error)

	snapshotPeriodStart := time.Now().UTC().AddDate(0, 0, -7)
	snapshotPeriodEnd := time.Now().UTC()
	snapshotA := models.MetaAnalyticsSnapshot{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		AccountID:      accountA.ID,
		AccountName:    "Alpha logical analytics",
		Dataset:        "rls-test",
		AnalyticsType:  "analytics",
		Granularity:    "DAY",
		PeriodStart:    snapshotPeriodStart,
		PeriodEnd:      snapshotPeriodEnd,
		Payload:        models.JSONB{"id": "alpha"},
		TemplateNames:  models.JSONB{},
		IsMock:         true,
	}
	snapshotB := models.MetaAnalyticsSnapshot{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		AccountID:      accountB.ID,
		AccountName:    "Beta logical analytics",
		Dataset:        "rls-test",
		AnalyticsType:  "analytics",
		Granularity:    "DAY",
		PeriodStart:    snapshotPeriodStart,
		PeriodEnd:      snapshotPeriodEnd,
		Payload:        models.JSONB{"id": "beta"},
		TemplateNames:  models.JSONB{},
		IsMock:         true,
	}
	require.NoError(t, adminDB.Create(&snapshotA).Error)
	require.NoError(t, adminDB.Create(&snapshotB).Error)

	onboardingA := models.OrganizationOnboarding{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		Status:          models.OnboardingStatusInProgress,
		ProgressPercent: 25,
		Checklist:       models.JSONB{},
		Input:           models.JSONB{},
		Metadata:        models.JSONB{},
	}
	onboardingB := models.OrganizationOnboarding{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		Status:          models.OnboardingStatusInProgress,
		ProgressPercent: 50,
		Checklist:       models.JSONB{},
		Input:           models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&onboardingA).Error)
	require.NoError(t, adminDB.Create(&onboardingB).Error)

	pipelineA := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		Name:           "Alpha pipeline",
		IsDefault:      true,
		IsActive:       true,
		Version:        1,
	}
	pipelineB := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		Name:           "Beta pipeline",
		IsDefault:      true,
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, adminDB.Create(&pipelineA).Error)
	require.NoError(t, adminDB.Create(&pipelineB).Error)

	serviceA := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		Name:            "Alpha assessment",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		Version:         1,
	}
	serviceB := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		Name:            "Beta assessment",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 45,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		Version:         1,
	}
	require.NoError(t, adminDB.Create(&serviceA).Error)
	require.NoError(t, adminDB.Create(&serviceB).Error)

	channelA := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgA.ID,
		Channel:           models.ChannelWebChat,
		Provider:          "relay",
		Name:              "Alpha web chat",
		ExternalAccountID: "alpha-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	channelB := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgB.ID,
		Channel:           models.ChannelWebChat,
		Provider:          "relay",
		Name:              "Beta web chat",
		ExternalAccountID: "beta-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&channelA).Error)
	require.NoError(t, adminDB.Create(&channelB).Error)
	metaPlatformAppID := "100000000000001"
	metaAuthorizingUserID := "900000000000001"
	metaChannelB := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgB.ID,
		Channel:           models.ChannelMessenger,
		Provider:          "relay",
		Name:              "Beta Messenger registry",
		ExternalAccountID: "page-beta-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusDegraded,
		Capabilities:      models.JSONB{},
		Config: models.JSONB{
			"meta_registry_managed": true,
			"meta_management_mode":  "platform_oauth",
		},
		Metadata: models.JSONB{
			"meta_platform_app_id":      metaPlatformAppID,
			"meta_authorizing_user_id":  metaAuthorizingUserID,
			"meta_ownership_checked_at": time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano),
		},
	}
	require.NoError(t, adminDB.Create(&metaChannelB).Error)
	threadsChannelB := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgB.ID,
		Channel:           models.ChannelThreads,
		Provider:          "threads",
		Name:              "Beta Threads",
		ExternalAccountID: "threads-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&threadsChannelB).Error)
	threadsExpiry := time.Now().UTC().Add(24 * time.Hour)
	require.NoError(t, adminDB.Create(&models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgB.ID,
		ChannelAccountID: threadsChannelB.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": "rls-test-token"},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test",
		ExpiresAt:        &threadsExpiry,
		Metadata:         models.JSONB{},
	}).Error)
	conversationB := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         orgB.ID,
		ChannelAccountID:       channelB.ID,
		ContactID:              contactB.ID,
		Channel:                channelB.Channel,
		ExternalConversationID: "outbox-test-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&conversationB).Error)
	outboxB := models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgB.ID,
		ChannelAccountID: channelB.ID,
		ConversationID:   conversationB.ID,
		IdempotencyKey:   "rls-outbox-" + uuid.NewString(),
		Status:           models.OutboxJobStatusPending,
		AvailableAt:      time.Now().UTC().Add(-time.Minute),
		MaxAttempts:      8,
		Payload:          models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&outboxB).Error)
	scheduledReplyB := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		Kind:           models.ScheduledJobKindChannelAIReply,
		AggregateType:  models.ChannelAIReplyAggregateType,
		RunAt:          time.Now().UTC().Add(-time.Minute),
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    5,
		IdempotencyKey: "rls-ai-reply-" + uuid.NewString(),
		Payload:        models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&scheduledReplyB).Error)

	flowA := models.ChatbotFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		WhatsAppAccount: accountA.Name,
		Name:            "Alpha Intake",
	}
	flowB := models.ChatbotFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		WhatsAppAccount: accountB.Name,
		Name:            "Beta Intake",
	}
	require.NoError(t, adminDB.Create(&flowA).Error)
	require.NoError(t, adminDB.Create(&flowB).Error)
	stepA := models.ChatbotFlowStep{
		BaseModel: models.BaseModel{ID: uuid.New()},
		FlowID:    flowA.ID,
		StepName:  "alpha-step",
		StepOrder: 1,
		Message:   "Alpha only",
	}
	stepB := models.ChatbotFlowStep{
		BaseModel: models.BaseModel{ID: uuid.New()},
		FlowID:    flowB.ID,
		StepName:  "beta-step",
		StepOrder: 1,
		Message:   "Beta only",
	}
	require.NoError(t, adminDB.Create(&stepA).Error)
	require.NoError(t, adminDB.Create(&stepB).Error)

	ivrA := models.IVRFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		WhatsAppAccount: accountA.Name,
		Name:            "Alpha call start",
		IsActive:        true,
		Menu:            models.JSONB{"version": 2, "nodes": []any{}},
	}
	ivrB := models.IVRFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		WhatsAppAccount: accountB.Name,
		Name:            "Beta call start",
		IsActive:        true,
		Menu:            models.JSONB{"version": 2, "nodes": []any{}},
	}
	require.NoError(t, adminDB.Create(&ivrA).Error)
	require.NoError(t, adminDB.Create(&ivrB).Error)

	callLogA := models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		WhatsAppAccount: accountA.Name,
		ContactID:       contactA.ID,
		WhatsAppCallID:  "rls-call-alpha-" + uuid.NewString(),
		CallerPhone:     contactA.PhoneNumber,
		Direction:       models.CallDirectionIncoming,
		Status:          models.CallStatusRinging,
		IVRFlowID:       &ivrA.ID,
	}
	callLogB := models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		WhatsAppAccount: accountB.Name,
		ContactID:       contactB.ID,
		WhatsAppCallID:  "rls-call-beta-" + uuid.NewString(),
		CallerPhone:     contactB.PhoneNumber,
		Direction:       models.CallDirectionIncoming,
		Status:          models.CallStatusRinging,
		IVRFlowID:       &ivrB.ID,
	}
	require.NoError(t, adminDB.Create(&callLogA).Error)
	require.NoError(t, adminDB.Create(&callLogB).Error)

	permissionA := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		ContactID:       contactA.ID,
		WhatsAppAccount: accountA.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "permission-alpha-" + uuid.NewString(),
	}
	permissionB := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		ContactID:       contactB.ID,
		WhatsAppAccount: accountB.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "permission-beta-" + uuid.NewString(),
	}
	require.NoError(t, adminDB.Create(&permissionA).Error)
	require.NoError(t, adminDB.Create(&permissionB).Error)

	transferA := models.CallTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		CallLogID:       callLogA.ID,
		WhatsAppCallID:  callLogA.WhatsAppCallID,
		CallerPhone:     contactA.PhoneNumber,
		ContactID:       contactA.ID,
		WhatsAppAccount: accountA.Name,
		Status:          models.CallTransferStatusWaiting,
		TransferredAt:   time.Now().UTC(),
	}
	transferB := models.CallTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		CallLogID:       callLogB.ID,
		WhatsAppCallID:  callLogB.WhatsAppCallID,
		CallerPhone:     contactB.PhoneNumber,
		ContactID:       contactB.ID,
		WhatsAppAccount: accountB.Name,
		Status:          models.CallTransferStatusWaiting,
		TransferredAt:   time.Now().UTC(),
	}
	require.NoError(t, adminDB.Create(&transferA).Error)
	require.NoError(t, adminDB.Create(&transferB).Error)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))

	asRuntime := func(fn func(*gorm.DB) error) error {
		return adminDB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
				return err
			}
			return fn(tx)
		})
	}
	asTenant := func(organizationID uuid.UUID, fn func(*gorm.DB) error) error {
		return asRuntime(func(runtimeDB *gorm.DB) error {
			return database.WithTenant(runtimeDB, organizationID, fn)
		})
	}

	t.Run("runtime startup verification accepts only the restricted role", func(t *testing.T) {
		require.Error(t, database.VerifyTenantRLS(adminDB, runtimeRole))
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		}))
	})

	t.Run("optional lifecycle resolvers preserve version five binary rollback", func(t *testing.T) {
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			// This is the exact routing-function tail of VerifyTenantRLS at
			// foundation production SHA 91c75ae. Running it after the Lifecycle B
			// migration proves that binary's v5 startup contract still accepts the
			// additive resolver set (before any managed account/job exists).
			for _, signature := range []string{
				"public.rereply_resolve_whatsapp_org(text)",
				"public.rereply_resolve_webhook_org(text)",
				"public.rereply_resolve_waba_orgs(text)",
				"public.rereply_resolve_channel_org(uuid)",
				"public.rereply_resolve_meta_channel_org(text,text)",
				"public.rereply_ready_channel_outbox_orgs(uuid,integer,timestamp with time zone)",
				"public.rereply_ready_channel_ai_reply_orgs(uuid,integer,timestamp with time zone)",
				"public.rereply_ready_threads_credential_orgs(uuid,integer,timestamp with time zone)",
				"public.rereply_rls_routing_version()",
			} {
				var allowed bool
				if err := runtimeDB.Raw(
					"SELECT has_function_privilege(current_user, ?, 'EXECUTE')", signature,
				).Scan(&allowed).Error; err != nil {
					return err
				}
				require.True(t, allowed, "91c75ae routing contract %s", signature)
			}
			var routingVersion int
			if err := runtimeDB.Raw(
				"SELECT public.rereply_rls_routing_version()",
			).Scan(&routingVersion).Error; err != nil {
				return err
			}
			require.Equal(t, 5, routingVersion)
			// The current verifier additionally checks the new lifecycle resolver
			// signatures while retaining that previous v5 contract.
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		}))
	})

	t.Run("runtime startup rejects stale routing functions", func(t *testing.T) {
		require.NoError(t, adminDB.Exec(`
			CREATE OR REPLACE FUNCTION public.rereply_rls_routing_version()
			RETURNS integer
			LANGUAGE sql
			IMMUTABLE
			SECURITY DEFINER
			SET search_path = pg_catalog, public
			AS $function$
			  SELECT 1
			$function$
		`).Error)
		err := asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "routing version")

		// Restore the current resolver set for the remaining isolation checks.
		require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		}))
	})

	t.Run("runtime startup rejects a missing WhatsApp Phone ID routing index", func(t *testing.T) {
		restored := false
		defer func() {
			if !restored {
				require.NoError(t, adminDB.Exec(`
					CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_accounts_live_phone_id
					ON whatsapp_accounts(BTRIM(phone_id))
					WHERE deleted_at IS NULL
				`).Error)
			}
		}()

		require.NoError(t, adminDB.Exec(
			"DROP INDEX public.uq_whatsapp_accounts_live_phone_id",
		).Error)
		err := asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Phone ID routing index")

		require.NoError(t, adminDB.Exec(`
			CREATE UNIQUE INDEX uq_whatsapp_accounts_live_phone_id
			ON whatsapp_accounts(BTRIM(phone_id))
			WHERE deleted_at IS NULL
		`).Error)
		restored = true
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		}))
	})

	t.Run("runtime startup rejects missing Threads credential resolver grant", func(t *testing.T) {
		require.NoError(t, adminDB.Exec(fmt.Sprintf(
			"REVOKE ALL ON FUNCTION public.rereply_ready_threads_credential_orgs(uuid,integer,timestamptz) FROM %s",
			runtimeRole,
		)).Error)
		restored := false
		defer func() {
			if !restored {
				require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
			}
		}()
		err := asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "rereply_ready_threads_credential_orgs")

		// Restore the resolver grant for the remaining isolation checks.
		require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
		restored = true
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return database.VerifyTenantRLS(runtimeDB, runtimeRole)
		}))
	})

	t.Run("missing tenant context returns no CRM rows", func(t *testing.T) {
		var count int64
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Model(&models.Contact{}).Count(&count).Error
		}))
		require.Zero(t, count)
	})

	t.Run("unfiltered reads only return the active clinic", func(t *testing.T) {
		var contacts []models.Contact
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			// No organization predicate is intentional: PostgreSQL must add it.
			return tenantDB.Order("profile_name").Find(&contacts).Error
		}))
		require.Len(t, contacts, 1)
		require.Equal(t, contactA.ID, contacts[0].ID)
	})

	t.Run("new commercial CRM booking and channel tables are tenant isolated", func(t *testing.T) {
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			checks := []struct {
				model any
				want  uuid.UUID
			}{
				{model: &models.OrganizationOnboarding{}, want: onboardingA.ID},
				{model: &models.CRMPipeline{}, want: pipelineA.ID},
				{model: &models.BookingService{}, want: serviceA.ID},
				{model: &models.ChannelAccount{}, want: channelA.ID},
				{model: &models.MetaAnalyticsSnapshot{}, want: snapshotA.ID},
			}
			for _, check := range checks {
				var ids []uuid.UUID
				if err := tenantDB.Model(check.model).Pluck("id", &ids).Error; err != nil {
					return err
				}
				if len(ids) != 1 || ids[0] != check.want {
					return fmt.Errorf("tenant table exposed IDs %v, expected only %s", ids, check.want)
				}
			}
			return nil
		}))
	})

	t.Run("calling and IVR reads only return the active clinic", func(t *testing.T) {
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			checks := []struct {
				name  string
				model any
				want  uuid.UUID
			}{
				{name: "call_logs", model: &models.CallLog{}, want: callLogA.ID},
				{name: "call_permissions", model: &models.CallPermission{}, want: permissionA.ID},
				{name: "call_transfers", model: &models.CallTransfer{}, want: transferA.ID},
				{name: "ivr_flows", model: &models.IVRFlow{}, want: ivrA.ID},
			}
			for _, check := range checks {
				var ids []uuid.UUID
				if err := tenantDB.Model(check.model).Order("id").Pluck("id", &ids).Error; err != nil {
					return fmt.Errorf("read %s: %w", check.name, err)
				}
				if len(ids) != 1 || ids[0] != check.want {
					return fmt.Errorf("%s exposed IDs %v, expected only %s", check.name, ids, check.want)
				}
			}
			return nil
		}))
	})

	t.Run("calling and IVR tables fail closed without tenant context", func(t *testing.T) {
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			for name, model := range map[string]any{
				"call_logs":        &models.CallLog{},
				"call_permissions": &models.CallPermission{},
				"call_transfers":   &models.CallTransfer{},
				"ivr_flows":        &models.IVRFlow{},
			} {
				var count int64
				if err := runtimeDB.Model(model).Count(&count).Error; err != nil {
					return fmt.Errorf("count %s: %w", name, err)
				}
				if count != 0 {
					return fmt.Errorf("unscoped %s query exposed %d rows", name, count)
				}
			}
			return nil
		}))
	})

	t.Run("calling and IVR cross-clinic mutations are rejected", func(t *testing.T) {
		type mutationCheck struct {
			name        string
			model       any
			ownID       uuid.UUID
			foreignID   uuid.UUID
			updateField string
			updateValue any
			forbidden   func() any
		}
		checks := []mutationCheck{
			{
				name: "call_logs", model: &models.CallLog{}, ownID: callLogA.ID, foreignID: callLogB.ID,
				updateField: "error_message", updateValue: "must remain beta-only",
				forbidden: func() any {
					return &models.CallLog{
						BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
						WhatsAppAccount: accountB.Name, ContactID: contactB.ID,
						WhatsAppCallID: "forbidden-call-" + uuid.NewString(), CallerPhone: contactB.PhoneNumber,
						Direction: models.CallDirectionIncoming, Status: models.CallStatusRinging,
					}
				},
			},
			{
				name: "call_permissions", model: &models.CallPermission{}, ownID: permissionA.ID, foreignID: permissionB.ID,
				updateField: "message_id", updateValue: "must-remain-beta-only",
				forbidden: func() any {
					return &models.CallPermission{
						BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
						ContactID: contactB.ID, WhatsAppAccount: accountB.Name,
						Status: models.CallPermissionPending, MessageID: "forbidden-permission-" + uuid.NewString(),
					}
				},
			},
			{
				name: "call_transfers", model: &models.CallTransfer{}, ownID: transferA.ID, foreignID: transferB.ID,
				updateField: "hold_duration", updateValue: 99,
				forbidden: func() any {
					return &models.CallTransfer{
						BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
						CallLogID: callLogB.ID, WhatsAppCallID: callLogB.WhatsAppCallID,
						CallerPhone: contactB.PhoneNumber, ContactID: contactB.ID,
						WhatsAppAccount: accountB.Name, Status: models.CallTransferStatusWaiting,
						TransferredAt: time.Now().UTC(),
					}
				},
			},
			{
				name: "ivr_flows", model: &models.IVRFlow{}, ownID: ivrA.ID, foreignID: ivrB.ID,
				updateField: "description", updateValue: "must remain beta-only",
				forbidden: func() any {
					return &models.IVRFlow{
						BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
						WhatsAppAccount: accountB.Name, Name: "Forbidden Beta IVR",
						IsActive: false, Menu: models.JSONB{"version": 2, "nodes": []any{}},
					}
				},
			},
		}

		for _, check := range checks {
			check := check
			t.Run(check.name, func(t *testing.T) {
				insertErr := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
					return tenantDB.Create(check.forbidden()).Error
				})
				require.Error(t, insertErr)
				require.Contains(t, insertErr.Error(), "row-level security")

				moveErr := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
					return tenantDB.Model(check.model).
						Where("id = ?", check.ownID).
						Update("organization_id", orgB.ID).Error
				})
				require.Error(t, moveErr)
				require.Contains(t, moveErr.Error(), "row-level security")

				var updatedRows int64
				require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
					result := tenantDB.Model(check.model).
						Where("id = ?", check.foreignID).
						Update(check.updateField, check.updateValue)
					updatedRows = result.RowsAffected
					return result.Error
				}))
				require.Zero(t, updatedRows)

				var deletedRows int64
				require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
					result := tenantDB.Unscoped().Delete(check.model, "id = ?", check.foreignID)
					deletedRows = result.RowsAffected
					return result.Error
				}))
				require.Zero(t, deletedRows)
			})
		}
	})

	t.Run("explicit lookup of another clinic is invisible", func(t *testing.T) {
		var contact models.Contact
		err := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			return tenantDB.First(&contact, "id = ?", contactB.ID).Error
		})
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	})

	t.Run("child tables without organization columns inherit parent isolation", func(t *testing.T) {
		var steps []models.ChatbotFlowStep
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			// chatbot_flow_steps has no organization_id column; its RLS policy
			// must derive the clinic from chatbot_flows.
			return tenantDB.Order("step_name").Find(&steps).Error
		}))
		require.Len(t, steps, 1)
		require.Equal(t, stepA.ID, steps[0].ID)

		err := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			forbidden := models.ChatbotFlowStep{
				BaseModel: models.BaseModel{ID: uuid.New()},
				FlowID:    flowB.ID,
				StepName:  "forbidden-step",
				StepOrder: 2,
				Message:   "Must not be created",
			}
			return tenantDB.Create(&forbidden).Error
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "row-level security")
	})

	t.Run("cross-clinic insert is rejected", func(t *testing.T) {
		crossTenantContact := models.Contact{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgB.ID,
			PhoneNumber:     "+601100000003",
			ProfileName:     "Forbidden Insert",
			WhatsAppAccount: accountB.Name,
		}
		err := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			return tenantDB.Create(&crossTenantContact).Error
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "row-level security")
	})

	t.Run("moving a row to another clinic is rejected", func(t *testing.T) {
		err := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			return tenantDB.Model(&models.Contact{}).
				Where("id = ?", contactA.ID).
				Update("organization_id", orgB.ID).Error
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "row-level security")
	})

	t.Run("Meta analytics snapshots reject cross-clinic writes", func(t *testing.T) {
		crossTenantSnapshot := models.MetaAnalyticsSnapshot{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgB.ID,
			AccountID:      uuid.New(),
			AccountName:    "Forbidden snapshot",
			Dataset:        "rls-test",
			AnalyticsType:  "analytics",
			Granularity:    "DAY",
			PeriodStart:    snapshotPeriodStart,
			PeriodEnd:      snapshotPeriodEnd,
			Payload:        models.JSONB{"id": "forbidden"},
			TemplateNames:  models.JSONB{},
			IsMock:         true,
		}
		insertErr := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			return tenantDB.Create(&crossTenantSnapshot).Error
		})
		require.Error(t, insertErr)
		require.Contains(t, insertErr.Error(), "row-level security")

		updateErr := asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			return tenantDB.Model(&models.MetaAnalyticsSnapshot{}).
				Where("id = ?", snapshotA.ID).
				Update("organization_id", orgB.ID).Error
		})
		require.Error(t, updateErr)
		require.Contains(t, updateErr.Error(), "row-level security")
	})

	t.Run("cross-clinic delete affects no rows", func(t *testing.T) {
		var rowsAffected int64
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			result := tenantDB.Unscoped().Delete(&models.Contact{}, "id = ?", contactB.ID)
			rowsAffected = result.RowsAffected
			return result.Error
		}))
		require.Zero(t, rowsAffected)
	})

	t.Run("webhook router resolves a tenant without exposing account rows", func(t *testing.T) {
		var resolvedText string
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT public.rereply_resolve_whatsapp_org(?)::text",
				"  "+accountB.PhoneID+"  ",
			).Scan(&resolvedText).Error; err != nil {
				return err
			}

			var visibleAccounts int64
			if err := runtimeDB.Model(&models.WhatsAppAccount{}).Count(&visibleAccounts).Error; err != nil {
				return err
			}
			if visibleAccounts != 0 {
				return fmt.Errorf("unscoped account query exposed %d rows", visibleAccounts)
			}
			return nil
		}))
		resolved, err := uuid.Parse(resolvedText)
		require.NoError(t, err)
		require.Equal(t, orgB.ID, resolved)
	})

	t.Run("omnichannel webhook router resolves without exposing channel accounts", func(t *testing.T) {
		var resolvedText string
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT public.rereply_resolve_channel_org(?)::text",
				channelB.ID,
			).Scan(&resolvedText).Error; err != nil {
				return err
			}

			var visibleAccounts int64
			if err := runtimeDB.Model(&models.ChannelAccount{}).Count(&visibleAccounts).Error; err != nil {
				return err
			}
			if visibleAccounts != 0 {
				return fmt.Errorf("unscoped channel account query exposed %d rows", visibleAccounts)
			}
			return nil
		}))
		resolved, err := uuid.Parse(resolvedText)
		require.NoError(t, err)
		require.Equal(t, orgB.ID, resolved)
	})

	t.Run("Meta registry router resolves degraded owner without bypassing RLS", func(t *testing.T) {
		var resolvedText string
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT public.rereply_resolve_meta_channel_org(?, ?)::text",
				"messenger", metaChannelB.ExternalAccountID,
			).Scan(&resolvedText).Error; err != nil {
				return err
			}
			var visibleAccounts int64
			if err := runtimeDB.Model(&models.ChannelAccount{}).Count(&visibleAccounts).Error; err != nil {
				return err
			}
			if visibleAccounts != 0 {
				return fmt.Errorf("unscoped Meta account query exposed %d rows", visibleAccounts)
			}
			return nil
		}))
		resolved, err := uuid.Parse(resolvedText)
		require.NoError(t, err)
		require.Equal(t, orgB.ID, resolved)
	})

	t.Run("Meta lifecycle resolver pages due tenants without exposing accounts", func(t *testing.T) {
		var organizationIDs []uuid.UUID
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT * FROM public.rereply_ready_meta_lifecycle_orgs(?, ?, ?)",
				uuid.Nil,
				20,
				time.Now().UTC().Add(-24*time.Hour),
			).Scan(&organizationIDs).Error; err != nil {
				return err
			}
			var visibleAccounts int64
			if err := runtimeDB.Model(&models.ChannelAccount{}).Count(&visibleAccounts).Error; err != nil {
				return err
			}
			if visibleAccounts != 0 {
				return fmt.Errorf("unscoped lifecycle query exposed %d channel accounts", visibleAccounts)
			}
			return nil
		}))
		require.Equal(t, []uuid.UUID{orgB.ID}, organizationIDs)

		organizationIDs = nil
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Raw(
				"SELECT * FROM public.rereply_ready_meta_lifecycle_orgs(?, ?, ?)",
				orgB.ID,
				20,
				time.Now().UTC(),
			).Scan(&organizationIDs).Error
		}))
		require.Empty(t, organizationIDs)
	})

	t.Run("Meta deauthorization lookup is exact and does not bypass tenant RLS", func(t *testing.T) {
		type deauthTarget struct {
			OrganizationID uuid.UUID `gorm:"column:organization_id"`
			AccountID      uuid.UUID `gorm:"column:account_id"`
		}
		var targets []deauthTarget
		var visibleAccounts int64
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT * FROM public.rereply_meta_deauth_targets(?, ?)",
				metaPlatformAppID,
				metaAuthorizingUserID,
			).Scan(&targets).Error; err != nil {
				return err
			}
			return runtimeDB.Model(&models.ChannelAccount{}).Count(&visibleAccounts).Error
		}))
		require.Equal(t, []deauthTarget{{OrganizationID: orgB.ID, AccountID: metaChannelB.ID}}, targets)
		require.Zero(t, visibleAccounts)

		targets = nil
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Raw(
				"SELECT * FROM public.rereply_meta_deauth_target_page(?, ?, ?, ?)",
				metaPlatformAppID,
				metaAuthorizingUserID,
				uuid.Nil,
				100,
			).Scan(&targets).Error
		}))
		require.Equal(t, []deauthTarget{{OrganizationID: orgB.ID, AccountID: metaChannelB.ID}}, targets)
		targets = nil
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Raw(
				"SELECT * FROM public.rereply_meta_deauth_target_page(?, ?, ?, ?)",
				metaPlatformAppID,
				metaAuthorizingUserID,
				metaChannelB.ID,
				100,
			).Scan(&targets).Error
		}))
		require.Empty(t, targets)

		targets = nil
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Raw(
				"SELECT * FROM public.rereply_meta_deauth_targets(?, ?)",
				"999999999999999",
				metaAuthorizingUserID,
			).Scan(&targets).Error
		}))
		require.Empty(t, targets)
	})

	t.Run("Meta lifecycle and deauthorization resolver limits are NULL-safe and hard-capped", func(t *testing.T) {
		const fixtureCount = 105
		boundedAppID := "888888888888888"
		boundedUserID := "777777777777777"
		due := time.Now().UTC().Add(-48 * time.Hour)
		organizations := make([]models.Organization, 0, fixtureCount)
		accounts := make([]models.ChannelAccount, 0, fixtureCount)
		for index := 0; index < fixtureCount; index++ {
			organizationID := uuid.New()
			organizations = append(organizations, models.Organization{
				BaseModel: models.BaseModel{ID: organizationID},
				Name:      fmt.Sprintf("Bounded resolver %03d", index),
				Slug:      fmt.Sprintf("bounded-resolver-%s", uuid.NewString()[:8]),
			})
			accounts = append(accounts, models.ChannelAccount{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
				Channel: models.ChannelMessenger, Provider: "relay", Name: fmt.Sprintf("Bounded Page %03d", index),
				ExternalAccountID: fmt.Sprintf("880000000%06d", index), Status: models.ChannelAccountStatusActive,
				Config: models.JSONB{"meta_management_mode": "platform_oauth"},
				Metadata: models.JSONB{
					"meta_platform_app_id": boundedAppID, "meta_authorizing_user_id": boundedUserID,
					"meta_ownership_checked_at": due.Format(time.RFC3339Nano),
				},
			})
		}
		require.NoError(t, adminDB.CreateInBatches(&organizations, 50).Error)
		require.NoError(t, adminDB.CreateInBatches(&accounts, 50).Error)

		for _, limit := range []any{nil, 0, -100, 1000000} {
			var ready []uuid.UUID
			require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
				return runtimeDB.Raw(
					"SELECT * FROM public.rereply_ready_meta_lifecycle_orgs(?, CAST(? AS integer), ?)",
					uuid.Nil, limit, time.Now().UTC(),
				).Scan(&ready).Error
			}))
			require.LessOrEqual(t, len(ready), 100, "lifecycle limit %v", limit)

			type deauthTarget struct {
				OrganizationID uuid.UUID `gorm:"column:organization_id"`
				AccountID      uuid.UUID `gorm:"column:account_id"`
			}
			var targets []deauthTarget
			require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
				return runtimeDB.Raw(
					"SELECT * FROM public.rereply_meta_deauth_target_page(?, ?, ?, CAST(? AS integer))",
					boundedAppID, boundedUserID, uuid.Nil, limit,
				).Scan(&targets).Error
			}))
			require.LessOrEqual(t, len(targets), 100, "deauthorization limit %v", limit)
		}

		var compatibilityTargets []struct {
			OrganizationID uuid.UUID `gorm:"column:organization_id"`
			AccountID      uuid.UUID `gorm:"column:account_id"`
		}
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Raw(
				"SELECT * FROM public.rereply_meta_deauth_targets(?, ?)", boundedAppID, boundedUserID,
			).Scan(&compatibilityTargets).Error
		}))
		require.Len(t, compatibilityTargets, 100, "the v5 rollback shim must remain hard-capped")
	})

	t.Run("outbox resolver returns ready tenants without exposing jobs", func(t *testing.T) {
		var organizationIDs []uuid.UUID
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT * FROM public.rereply_ready_channel_outbox_orgs(?, ?, ?)",
				uuid.Nil,
				20,
				time.Now().UTC().Add(-2*time.Minute),
			).Scan(&organizationIDs).Error; err != nil {
				return err
			}

			var visibleJobs int64
			if err := runtimeDB.Model(&models.OutboxJob{}).Count(&visibleJobs).Error; err != nil {
				return err
			}
			if visibleJobs != 0 {
				return fmt.Errorf("unscoped outbox query exposed %d rows", visibleJobs)
			}
			return nil
		}))
		require.Contains(t, organizationIDs, orgB.ID)
	})

	t.Run("AI reply resolver returns tenant IDs without exposing scheduled jobs", func(t *testing.T) {
		var organizationIDs []uuid.UUID
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT * FROM public.rereply_ready_channel_ai_reply_orgs(?, ?, ?)",
				uuid.Nil,
				20,
				time.Now().UTC().Add(-2*time.Minute),
			).Scan(&organizationIDs).Error; err != nil {
				return err
			}

			var visibleJobs int64
			if err := runtimeDB.Model(&models.ScheduledJob{}).Count(&visibleJobs).Error; err != nil {
				return err
			}
			if visibleJobs != 0 {
				return fmt.Errorf("unscoped scheduled job query exposed %d rows", visibleJobs)
			}
			return nil
		}))
		require.Contains(t, organizationIDs, orgB.ID)
	})

	t.Run("Threads credential resolver returns tenant IDs without exposing credentials", func(t *testing.T) {
		var organizationIDs []uuid.UUID
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			if err := runtimeDB.Raw(
				"SELECT * FROM public.rereply_ready_threads_credential_orgs(?, ?, ?)",
				uuid.Nil,
				20,
				time.Now().UTC().Add(7*24*time.Hour),
			).Scan(&organizationIDs).Error; err != nil {
				return err
			}

			var visibleCredentials int64
			if err := runtimeDB.Model(&models.ChannelCredential{}).Count(&visibleCredentials).Error; err != nil {
				return err
			}
			if visibleCredentials != 0 {
				return fmt.Errorf("unscoped credential query exposed %d rows", visibleCredentials)
			}
			return nil
		}))
		require.Contains(t, organizationIDs, orgB.ID)
	})

	t.Run("transaction-local tenant state does not leak to the pool", func(t *testing.T) {
		require.NoError(t, asTenant(orgA.ID, func(tenantDB *gorm.DB) error {
			var count int64
			if err := tenantDB.Model(&models.Contact{}).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("tenant A expected one contact, got %d", count)
			}
			return nil
		}))

		var count int64
		require.NoError(t, asRuntime(func(runtimeDB *gorm.DB) error {
			return runtimeDB.Model(&models.Contact{}).Count(&count).Error
		}))
		require.Zero(t, count)
	})

	t.Run("RLS rollback drops lifecycle resolver functions", func(t *testing.T) {
		require.NoError(t, database.RemoveTenantRLS(adminDB))
		for _, signature := range []string{
			"public.rereply_ready_threads_credential_orgs(uuid,integer,timestamptz)",
			"public.rereply_ready_meta_lifecycle_orgs(uuid,integer,timestamptz)",
			"public.rereply_meta_deauth_targets(text,text)",
			"public.rereply_meta_deauth_target_page(text,text,uuid,integer)",
		} {
			var resolverExists bool
			require.NoError(t, adminDB.Raw(
				"SELECT to_regprocedure(?) IS NOT NULL", signature,
			).Scan(&resolverExists).Error)
			require.False(t, resolverExists, signature)
		}

		// Restore RLS so final data-integrity assertions and cleanup run under
		// the same schema state as the rest of this test.
		require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	})

	// Verify that the rejected mutations did not change either clinic's data.
	var persistedA, persistedB models.Contact
	require.NoError(t, adminDB.First(&persistedA, "id = ?", contactA.ID).Error)
	require.NoError(t, adminDB.First(&persistedB, "id = ?", contactB.ID).Error)
	require.Equal(t, orgA.ID, persistedA.OrganizationID)
	require.Equal(t, orgB.ID, persistedB.OrganizationID)
}
