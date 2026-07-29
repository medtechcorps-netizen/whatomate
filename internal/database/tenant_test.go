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
				accountB.PhoneID,
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

	// Verify that the rejected mutations did not change either clinic's data.
	var persistedA, persistedB models.Contact
	require.NoError(t, adminDB.First(&persistedA, "id = ?", contactA.ID).Error)
	require.NoError(t, adminDB.First(&persistedB, "id = ?", contactB.ID).Error)
	require.Equal(t, orgA.ID, persistedA.OrganizationID)
	require.Equal(t, orgB.ID, persistedB.OrganizationID)
}
