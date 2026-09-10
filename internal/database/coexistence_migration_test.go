package database

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// RunRLSMigrationCoordinatorForTest exercises the phase-parameterized policy
// without creating a runtime selector in production.
func RunRLSMigrationCoordinatorForTest(
	db *gorm.DB,
	adminCfg *config.DefaultAdminConfig,
	runtimeRole, phase string,
	backfill func(*gorm.DB) error,
	verifyRuntime func() error,
) error {
	phaseByName := map[string]rlsMigrationPhase{
		"baseline": rlsMigrationPhaseBaseline,
		"bridge":   rlsMigrationPhaseBridge,
		"backend":  rlsMigrationPhaseBackend,
		"ui":       rlsMigrationPhaseUI,
	}
	selected, ok := phaseByName[phase]
	if !ok {
		return errors.New("test RLS migration phase is invalid")
	}
	return runRLSMigrationCoordinatorForPhase(
		db,
		adminCfg,
		runtimeRole,
		selected,
		backfill,
		verifyRuntime,
	)
}

// VerifyPlatformComplianceIdentityReviewCompatibilityForTest exposes only the
// phase-baseline compatibility verifier to the external integration test
// package. Production entry points remain strict.
func VerifyPlatformComplianceIdentityReviewCompatibilityForTest(
	db *gorm.DB,
	runtimeRole string,
) error {
	return verifyPlatformComplianceGuardsWithIdentityReviewRequirement(db, runtimeRole, false)
}

// VerifyPlatformComplianceIdentityReviewFutureForTest exercises the immutable
// future-only startup contract without depending on a phase source's one
// compile-time literal.
func VerifyPlatformComplianceIdentityReviewFutureForTest(
	db *gorm.DB,
	runtimeRole string,
) error {
	return verifyPlatformComplianceGuardsWithIdentityReviewRequirement(db, runtimeRole, true)
}

// ExecuteCoexistenceActivationForTest exercises the same bounded executor used
// by both production migration entry points without exposing it to runtime
// packages.
func ExecuteCoexistenceActivationForTest(db *gorm.DB, statement string) error {
	return executeMigrationIndexStatement(db, statement, true)
}

func TestWhatsAppCoexistenceStateIsMigratedAndTenantProtected(t *testing.T) {
	t.Parallel()

	var migrated bool
	for _, migration := range GetMigrationModels() {
		if _, ok := migration.Model.(*models.WhatsAppCoexistenceState); ok {
			migrated = true
			assert.Equal(t, "WhatsAppCoexistenceState", migration.Name)
			break
		}
	}
	require.True(t, migrated, "coexistence state must be included in automatic migrations")
	assert.Contains(t, DirectTenantTables, "whatsapp_coexistence_states")
}

func TestWhatsAppIdentityReviewModelsAreMigratedAndTenantProtected(t *testing.T) {
	t.Parallel()

	wanted := map[string]bool{
		"WhatsAppIdentityReviewHold":   false,
		"WhatsAppIdentityReviewMember": false,
	}
	for _, migration := range GetMigrationModels() {
		switch migration.Model.(type) {
		case *models.WhatsAppIdentityReviewHold:
			assert.Equal(t, "WhatsAppIdentityReviewHold", migration.Name)
			wanted["WhatsAppIdentityReviewHold"] = true
		case *models.WhatsAppIdentityReviewMember:
			assert.Equal(t, "WhatsAppIdentityReviewMember", migration.Name)
			wanted["WhatsAppIdentityReviewMember"] = true
		}
	}
	for model, found := range wanted {
		assert.True(t, found, "%s must be included in automatic migrations", model)
	}
	assert.Contains(t, DirectTenantTables, "whatsapp_identity_review_holds")
	assert.Contains(t, DirectTenantTables, "whatsapp_identity_review_members")
}

func TestWhatsAppCoexistenceIntegrityStatements(t *testing.T) {
	t.Parallel()

	statements := coexistenceIntegrityStatements()
	joined := strings.Join(statements, "\n")
	for _, required := range []string{
		"uq_whatsapp_accounts_id_org",
		"ON whatsapp_accounts(id, organization_id)",
		"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_whatsapp_accounts_id_org",
		"uq_messages_live_wamid",
		"NOT indisvalid",
		"ON messages(organization_id, (BTRIM(whats_app_message_id)))",
		"inbox_conversation_id IS NULL",
		"resolve duplicate WAMIDs before retrying",
		"chk_messages_whatsapp_message_id_trimmed",
		"AND NOT COALESCE(metadata, '{}'::jsonb) @>",
		"fk_whatsapp_coexistence_account_tenant",
		"FOREIGN KEY (whats_app_account_id, organization_id)",
		"REFERENCES whatsapp_accounts(id, organization_id)",
		"ON DELETE CASCADE",
		"chk_whatsapp_coexistence_onboarding_status",
		"chk_whatsapp_coexistence_sync_status",
		"chk_whatsapp_coexistence_contact_status",
		"chk_whatsapp_coexistence_history_status",
		"chk_whatsapp_coexistence_history_consent",
		"chk_whatsapp_coexistence_lifecycle_status",
		"contact_sync_attempts >= 0",
		"history_sync_attempts >= 0",
		"history_progress_percent BETWEEN 0 AND 100",
		"history_last_phase IS NULL OR history_last_phase >= 0",
		"history_last_chunk_order IS NULL OR history_last_chunk_order >= 0",
		"version >= 1",
		"onboarding_cycle >= 1",
		"uq_whatsapp_identity_review_unsupported_semantic_claim",
		") WHERE NOT supported",
		"uq_whatsapp_identity_review_generation",
		"uq_whatsapp_identity_review_decision_request",
		"uq_inbound_events_identity_review_wamid",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbound_events_protocol",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbound_events_review_hold_id",
		"LOCK TABLE public.contacts, public.channel_accounts, public.inbox_conversations,",
		whatsappIdentityReviewContactSelectorFenceNamespace,
		"rereply_lock_whatsapp_identity_review_contact_selector",
		"CREATE TRIGGER rereply_identity_review_contact_selector_fence",
		"pg_try_advisory_xact_lock",
		"FOR UPDATE NOWAIT",
		"OLD.id IS NOT DISTINCT FROM NEW.id",
		"OLD.phone_number IS NOT DISTINCT FROM NEW.phone_number",
		"OLD.bs_uid IS NOT DISTINCT FROM NEW.bs_uid",
		"OLD.merged_into_id IS NOT DISTINCT FROM NEW.merged_into_id",
		"OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at",
		"uq_whatsapp_wamid_cross_store_owner",
		"public.messages, public.inbound_events IN EXCLUSIVE MODE NOWAIT",
		"target_is_whatsapp",
		"owner_conversation.channel = 'whatsapp'",
		"owner_account.provider = 'meta_legacy'",
		"message.inbox_conversation_id IS NULL",
		WhatsAppWAMIDOwnerMetadataKey,
		"UPDATE public.messages AS message",
		"WhatsApp WAMID owners cannot be hard deleted",
		"cannot enforce WhatsApp message ownership; resolve duplicate tenant WAMIDs before retrying",
		"cannot enforce cross-store WhatsApp admission ownership",
		whatsappIdentityReviewWAMIDFenceNamespace,
		"FOR SHARE NOWAIT",
		"uq_whatsapp_wamid_message_owner",
		"hashtextextended",
		"rereply_guard_message_identity_review_wamid_owner",
		"rereply_guard_identity_review_event_wamid_owner",
		"CREATE TRIGGER rereply_identity_review_message_wamid_owner",
		"CREATE TRIGGER rereply_identity_review_event_wamid_owner",
		"chk_whatsapp_identity_review_hold_authority",
		"chk_whatsapp_identity_review_hold_decision",
		"chk_whatsapp_identity_review_member_reason",
		"chk_inbound_events_identity_review_shape",
		"dedupe_key = 'identity-review:' || provider_event_id",
		"payload ?& ARRAY['schema_version', 'message_type', 'content']",
		"payload - ARRAY[",
		"NOT (payload ?| ARRAY[",
		"fk_whatsapp_identity_review_hold_account_tenant",
		"fk_whatsapp_identity_review_member_hold_tenant",
		"fk_whatsapp_identity_review_member_contact_tenant",
		"fk_whatsapp_identity_review_target_member",
		"fk_whatsapp_identity_review_superseded_tenant",
		"fk_inbound_events_identity_review_hold_tenant",
		"NOT VALID",
		"ALTER TABLE public.inbound_events VALIDATE CONSTRAINT fk_inbound_events_identity_review_hold_tenant",
		"rereply_guard_whatsapp_identity_review_hold",
		"rereply_guard_whatsapp_identity_review_member",
		"rereply_verify_whatsapp_identity_review_complete",
		"rereply_guard_whatsapp_identity_review_inbound_event",
		"rereply_supersede_whatsapp_identity_review_prior_cycles",
		"CREATE TRIGGER trg_whatsapp_coexistence_identity_review_cycle",
		"superseded_by_onboarding_cycle = NEW.onboarding_cycle",
		"CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_holds_complete",
		"CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_members_complete",
		"DEFERRABLE INITIALLY DEFERRED",
		"IF TG_OP = 'DELETE' THEN",
		"target_organization_id := OLD.organization_id",
		"target_organization_id := NEW.organization_id",
		"sha256(convert_to",
		"member.selector_reasons::text",
		"LOCK TABLE public.whatsapp_identity_review_holds, public.whatsapp_identity_review_members",
		"IN SHARE ROW EXCLUSIVE MODE NOWAIT",
		"cannot install WhatsApp identity-review completeness guards; repair incomplete or noncanonical membership first",
		"CREATE TRIGGER trg_inbound_events_identity_review_guard",
		"BEFORE INSERT OR UPDATE OR DELETE ON public.inbound_events",
		"reserved whatsapp identity-review events must be inserted before media hydration",
		"NEW.payload -> 'media_status' = '\"ready\"'::jsonb",
		"NEW.payload ->> 'media_hydrated_id' = OLD.payload ->> 'media_id'",
	} {
		assert.Contains(t, joined, required)
	}

	candidateKeyPosition := strings.Index(joined, "uq_whatsapp_accounts_id_org")
	foreignKeyPosition := strings.Index(joined, "fk_whatsapp_coexistence_account_tenant")
	require.NotEqual(t, -1, candidateKeyPosition)
	require.NotEqual(t, -1, foreignKeyPosition)
	assert.Less(t, candidateKeyPosition, foreignKeyPosition)
	allIndexes := strings.Join(getIndexes(), "\n")
	assert.Contains(t, allIndexes, "fk_whatsapp_coexistence_account_tenant",
		"production index migration must install coexistence integrity")
	require.NotEmpty(t, getIndexes())
	assert.Contains(t, getIndexes()[0],
		"LOCK TABLE public.contacts, public.channel_accounts, public.inbox_conversations,",
		"hot-table readiness must fail before any other index or constraint DDL can wait")
	assert.Contains(t, getIndexes()[0], "IN EXCLUSIVE MODE NOWAIT")
	activation := getIndexes()[len(getIndexes())-1]
	assert.Contains(t, activation, "owner_trigger_installed boolean",
		"the atomic old-core activation must remain the final migration statement")
	for _, required := range []string{
		"identity_review_trigger_count integer",
		"correctly_bound_trigger_count integer",
		"correctly_bound_trigger_count NOT IN (0, 4)",
		"identity-review old-core trigger activation requires an exact absent or complete profile",
		"owner_trigger_installed := correctly_bound_trigger_count = 4",
	} {
		assert.Contains(t, activation, required,
			"the activation boundary must reject every partial or misbound trigger profile before backfill")
	}
	for _, required := range []string{
		"CREATE TRIGGER rereply_identity_review_contact_selector_fence",
		"CREATE TRIGGER rereply_identity_review_message_wamid_owner",
		"CREATE TRIGGER rereply_identity_review_event_wamid_owner",
		"CREATE TRIGGER trg_inbound_events_identity_review_guard",
	} {
		assert.Equal(t, 1, strings.Count(activation, required),
			"each old-core trigger must be created exactly once in the atomic activation")
		assert.Equal(t, 1, strings.Count(allIndexes, required),
			"no preparatory migration may create an old-core trigger")
	}
	assert.Less(t,
		strings.Index(allIndexes, "VALIDATE CONSTRAINT chk_inbound_events_identity_review_shape"),
		strings.LastIndex(allIndexes, "owner_trigger_installed boolean"),
		"historical-row validation must finish before atomic trigger activation",
	)
	assert.Less(t,
		strings.Index(allIndexes, "VALIDATE CONSTRAINT fk_inbound_events_identity_review_hold_tenant"),
		strings.LastIndex(allIndexes, "owner_trigger_installed boolean"),
		"the live-inbox foreign key must be validated before atomic trigger activation",
	)
	assert.Equal(t, "1s", coexistenceActivationStatementTimeout,
		"the final hot-table cutover must have a hard bounded statement lifetime")
	assert.Contains(t, activation, "tgenabled = 'O'")
	assert.Contains(t, activation, "trigger_function.proname = 'rereply_guard_message_identity_review_wamid_owner'")
	assert.Contains(t, activation, "tgargs = ''::bytea")
	assert.Contains(t, activation, "tgqual IS NULL")
	assert.Contains(t, activation, "IF NOT owner_trigger_installed THEN",
		"historical classification and marker backfill must run only on the exact absent profile")
	for _, fieldName := range []string{"Protocol", "ReviewHoldID"} {
		field, ok := reflect.TypeOf(models.InboundEvent{}).FieldByName(fieldName)
		require.True(t, ok)
		assert.NotContains(t, field.Tag.Get("gorm"), "index",
			"live inbound-event indexes must be installed concurrently, never by AutoMigrate")
	}
	for _, statement := range coexistenceIntegrityStatements()[:len(coexistenceIntegrityStatements())-1] {
		assert.NotContains(t, statement, "CREATE TRIGGER rereply_identity_review_contact_selector_fence")
		assert.NotContains(t, statement, "CREATE TRIGGER rereply_identity_review_message_wamid_owner")
		assert.NotContains(t, statement, "CREATE TRIGGER rereply_identity_review_event_wamid_owner")
		assert.NotContains(t, statement, "CREATE TRIGGER trg_inbound_events_identity_review_guard")
	}
	assert.Contains(t, whatsappIdentityReviewMessageWAMIDOwnerFunctionBody, "target_conversation.channel = 'whatsapp'",
		"only linked WhatsApp conversations extend legacy WAMID ownership")
	assert.Contains(t, whatsappIdentityReviewMessageWAMIDOwnerFunctionBody,
		"OLD.inbox_conversation_id IS NOT DISTINCT FROM NEW.inbox_conversation_id",
		"mutable account/conversation metadata alone cannot retroactively promote an existing provider identifier")
	assert.Contains(t, joined, "owner_trigger_installed boolean")
	assert.Contains(t, joined, "reserved WhatsApp WAMID owner marker predates its authority trigger",
		"the first migration must reject a preexisting spoofed reserved marker")
	assert.Contains(t, whatsappIdentityReviewEventWAMIDOwnerFunctionBody, WhatsAppWAMIDOwnerMetadataKey,
		"identity-review events must consume the trigger-maintained historical owner marker")
	assert.Less(t,
		strings.Index(whatsappIdentityReviewMessageWAMIDOwnerFunctionBody, "FROM public.organizations"),
		strings.Index(whatsappIdentityReviewMessageWAMIDOwnerFunctionBody, "pg_try_advisory_xact_lock"),
		"message writers must acquire the shared organization row before the advisory WAMID fence",
	)
	assert.Less(t,
		strings.Index(whatsappIdentityReviewEventWAMIDOwnerFunctionBody, "FROM public.organizations"),
		strings.Index(whatsappIdentityReviewEventWAMIDOwnerFunctionBody, "pg_try_advisory_xact_lock"),
		"event writers must acquire the shared organization row before the advisory WAMID fence",
	)
	assert.Less(t,
		strings.Index(whatsappIdentityReviewContactSelectorFenceFunctionBody, "FROM public.organizations"),
		strings.Index(whatsappIdentityReviewContactSelectorFenceFunctionBody, "pg_try_advisory_xact_lock"),
		"contact writers must acquire the organization row before the selector advisory fence",
	)
	assert.Less(t, "rereply_identity_review_contact_selector_fence", platformComplianceWriteTrigger,
		"the selector fence must run before the platform organization-row guard")
	assert.Less(t, "rereply_identity_review_message_wamid_owner", platformComplianceWriteTrigger,
		"the WAMID fence must run before the platform organization-row guard")
	assert.Less(t, "rereply_identity_review_message_wamid_owner", "trg_messages_ingestion_order",
		"the WAMID fence must run before the conversation ingestion-order trigger")

	var atomicMembershipInstall string
	for _, statement := range statements {
		if strings.Contains(statement, "invalid_membership boolean") {
			require.Empty(t, atomicMembershipInstall, "membership authority must have one atomic installer")
			atomicMembershipInstall = statement
		}
	}
	require.NotEmpty(t, atomicMembershipInstall)
	for _, required := range []string{
		"LOCK TABLE public.whatsapp_identity_review_holds, public.whatsapp_identity_review_members",
		"CREATE OR REPLACE FUNCTION rereply_guard_whatsapp_identity_review_hold()",
		"CREATE OR REPLACE FUNCTION rereply_guard_whatsapp_identity_review_member()",
		"CREATE OR REPLACE FUNCTION rereply_verify_whatsapp_identity_review_complete()",
		"DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_holds_guard",
		"CREATE TRIGGER trg_whatsapp_identity_review_holds_guard",
		"DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_members_guard",
		"CREATE TRIGGER trg_whatsapp_identity_review_members_guard",
		"DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_holds_complete",
		"CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_holds_complete",
		"DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_members_complete",
		"CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_members_complete",
	} {
		assert.Contains(t, atomicMembershipInstall, required,
			"preflight, function replacement, and trigger replacement must stay in one locked transaction")
	}
}

func TestWhatsAppIdentityReviewPolicyReadersFailClosedOnInvalidAuthority(t *testing.T) {
	t.Parallel()

	blocked, err := ContactHasBlockingIdentityReviewHold(nil, uuid.New(), uuid.New())
	assert.True(t, blocked)
	require.Error(t, err)

	claimed, err := WhatsAppIdentityReviewWAMIDClaimed(nil, uuid.New(), "wamid.invalid")
	assert.True(t, claimed)
	require.Error(t, err)
	claimed, err = WhatsAppIdentityReviewWAMIDClaimed(nil, uuid.Nil, "")
	assert.True(t, claimed)
	require.Error(t, err)
}

func TestWhatsAppIdentityReviewPlatformComplianceTriggerContract(t *testing.T) {
	t.Parallel()

	contacts := platformComplianceFutureIdentityReviewTriggers["contacts"]
	require.Len(t, contacts, 1)
	assert.Equal(t, "rereply_identity_review_contact_selector_fence", contacts[0].name)
	assert.Equal(t, "rereply_lock_whatsapp_identity_review_contact_selector", contacts[0].function)
	assert.Equal(t, 31, contacts[0].typeBits)
	assert.Empty(t, contacts[0].updateColumns)
	assert.Equal(t, "bb7b6fcf2b2dc9775a83a1fade3fd7eaf4e7c882a1df60ca2eacebd44ea3b99e",
		platformComplianceProductTriggerBodySHA256(contacts[0]))

	cycle := platformComplianceProductTriggers["whatsapp_coexistence_states"]
	require.Len(t, cycle, 1)
	assert.Equal(t, "trg_whatsapp_coexistence_identity_review_cycle", cycle[0].name)
	assert.Equal(t, "rereply_supersede_whatsapp_identity_review_prior_cycles", cycle[0].function)
	assert.Equal(t, 17, cycle[0].typeBits)
	assert.Equal(t, []string{"onboarding_cycle"}, cycle[0].updateColumns)
	assert.False(t, cycle[0].constraint)
	assert.Equal(t,
		normalizePlatformComplianceFunctionBody(whatsappIdentityReviewCycleSupersessionFunctionBody),
		normalizePlatformComplianceFunctionBody(cycle[0].functionBody),
	)
	assert.Contains(t, cycle[0].functionBody, "NEW.onboarding_cycle <> OLD.onboarding_cycle + 1")
	assert.Contains(t, cycle[0].functionBody, "WhatsApp onboarding cycle must advance by exactly one")
	assert.Contains(t, cycle[0].functionBody, "disposition = 'superseded_by_cycle'")
	assert.Contains(t, cycle[0].functionBody, "superseded_by_onboarding_cycle = NEW.onboarding_cycle")
	assert.Contains(t, cycle[0].functionBody, "AND disposition = 'open'")

	holds := platformComplianceProductTriggers["whatsapp_identity_review_holds"]
	require.Len(t, holds, 2)
	assert.Equal(t, "trg_whatsapp_identity_review_holds_guard", holds[0].name)
	assert.False(t, holds[0].constraint)
	assert.Equal(t, 31, holds[0].typeBits)
	assert.Equal(t, "trg_whatsapp_identity_review_holds_complete", holds[1].name)
	assert.True(t, holds[1].constraint)
	assert.True(t, holds[1].deferrable)
	assert.True(t, holds[1].initiallyDeferred)
	assert.Equal(t,
		normalizePlatformComplianceFunctionBody(whatsappIdentityReviewCompletenessFunctionBody),
		normalizePlatformComplianceFunctionBody(holds[1].functionBody),
	)
	assert.Contains(t, holds[0].functionBody, "NEW.disposition = 'superseded_by_cycle'")
	assert.Contains(t, holds[0].functionBody, "holds must be inserted in the exact open state")
	assert.Contains(t, holds[0].functionBody, "NEW.decision_resolved_by_id IS NULL")
	assert.Contains(t, holds[0].functionBody, "NEW.superseded_by_onboarding_cycle > NEW.onboarding_cycle")

	members := platformComplianceProductTriggers["whatsapp_identity_review_members"]
	require.Len(t, members, 2)
	assert.Equal(t, 29, members[1].typeBits)
	assert.True(t, members[1].constraint)

	messages := platformComplianceFutureIdentityReviewTriggers["messages"]
	require.Len(t, messages, 1)
	assert.Equal(t, "rereply_identity_review_message_wamid_owner", messages[0].name)
	assert.Equal(t, "rereply_guard_message_identity_review_wamid_owner", messages[0].function)
	assert.Equal(t, 31, messages[0].typeBits)
	assert.Equal(t, "304ccdaf324e50476d475e37032e3c29925a8711f1fd28fd5d66662356e39430",
		platformComplianceProductTriggerBodySHA256(messages[0]))

	inbound := platformComplianceFutureIdentityReviewTriggers["inbound_events"]
	require.Len(t, inbound, 2)
	assert.Equal(t, "trg_inbound_events_identity_review_guard", inbound[0].name)
	assert.Equal(t, 31, inbound[0].typeBits)
	assert.Equal(t, "rereply_identity_review_event_wamid_owner", inbound[1].name)
	assert.Equal(t, "rereply_guard_identity_review_event_wamid_owner", inbound[1].function)
	assert.Equal(t, 23, inbound[1].typeBits)
	assert.Equal(t, "b053ad0a31e1e21a03492fdd5808ac25bb2427afac719811fbd89624a315c2a4",
		platformComplianceProductTriggerBodySHA256(inbound[1]))
	assert.Equal(t, "16717c2e3f66c5226cae2d8d4352351dc333aa18fbcd9e7ba99b6d7192f1ac66",
		platformComplianceProductTriggerBodySHA256(inbound[0]))
}

func TestClassifyPlatformComplianceIdentityReviewTriggerProfile(t *testing.T) {
	t.Parallel()

	legacy, err := classifyPlatformComplianceIdentityReviewTriggerProfile(
		3,
		"rereply_platform_compliance_write_guard",
		"rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
		"rereply_platform_compliance_write_guard",
	)
	require.NoError(t, err)
	assert.Equal(t, platformComplianceLegacyIdentityReviewTriggerProfile, legacy)

	future, err := classifyPlatformComplianceIdentityReviewTriggerProfile(
		3,
		"rereply_identity_review_contact_selector_fence,rereply_platform_compliance_write_guard",
		"rereply_identity_review_message_wamid_owner,rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
		"rereply_identity_review_event_wamid_owner,rereply_platform_compliance_write_guard,trg_inbound_events_identity_review_guard",
	)
	require.NoError(t, err)
	assert.Equal(t, platformComplianceFutureIdentityReviewTriggerProfile, future)

	for name, profile := range map[string][3]string{
		"partial contact": {
			"rereply_identity_review_contact_selector_fence,rereply_platform_compliance_write_guard",
			"rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
			"rereply_platform_compliance_write_guard",
		},
		"partial message": {
			"rereply_platform_compliance_write_guard",
			"rereply_identity_review_message_wamid_owner,rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
			"rereply_platform_compliance_write_guard",
		},
		"partial inbound": {
			"rereply_platform_compliance_write_guard",
			"rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
			"rereply_identity_review_event_wamid_owner,rereply_platform_compliance_write_guard,trg_inbound_events_identity_review_guard",
		},
		"extra": {
			"rereply_platform_compliance_write_guard,zz_extra",
			"rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
			"rereply_platform_compliance_write_guard",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := classifyPlatformComplianceIdentityReviewTriggerProfile(
				3, profile[0], profile[1], profile[2],
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "neither exact legacy nor exact future")
		})
	}
	_, err = classifyPlatformComplianceIdentityReviewTriggerProfile(
		2,
		"rereply_platform_compliance_write_guard",
		"rereply_platform_compliance_write_guard,trg_messages_cleanup_read_cursors,trg_messages_ingestion_order",
		"",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "relations are incomplete")
}

func TestRLSMigrationPhasePolicyIsCompileTimeAndFailClosed(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		phase   rlsMigrationPhase
		profile platformComplianceIdentityReviewTriggerProfile
		want    rlsMigrationAction
		wantErr string
	}{
		{
			name: "baseline prepares exact legacy without activation", phase: rlsMigrationPhaseBaseline,
			profile: platformComplianceLegacyIdentityReviewTriggerProfile, want: rlsMigrationPrepareLegacy,
		},
		{
			name: "baseline future is read only", phase: rlsMigrationPhaseBaseline,
			profile: platformComplianceFutureIdentityReviewTriggerProfile, want: rlsMigrationVerifyFuture,
		},
		{
			name: "bridge activates exact legacy", phase: rlsMigrationPhaseBridge,
			profile: platformComplianceLegacyIdentityReviewTriggerProfile, want: rlsMigrationPrepareBridge,
		},
		{
			name: "bridge future is idempotent", phase: rlsMigrationPhaseBridge,
			profile: platformComplianceFutureIdentityReviewTriggerProfile, want: rlsMigrationVerifyFuture,
		},
		{
			name: "backend rejects legacy", phase: rlsMigrationPhaseBackend,
			profile: platformComplianceLegacyIdentityReviewTriggerProfile, wantErr: "require the exact future",
		},
		{
			name: "ui rejects legacy", phase: rlsMigrationPhaseUI,
			profile: platformComplianceLegacyIdentityReviewTriggerProfile, wantErr: "require the exact future",
		},
		{
			name: "backend accepts future read only", phase: rlsMigrationPhaseBackend,
			profile: platformComplianceFutureIdentityReviewTriggerProfile, want: rlsMigrationVerifyFuture,
		},
		{
			name: "ui accepts future read only", phase: rlsMigrationPhaseUI,
			profile: platformComplianceFutureIdentityReviewTriggerProfile, want: rlsMigrationVerifyFuture,
		},
		{
			name: "unknown phase rejects", phase: rlsMigrationPhase(255),
			profile: platformComplianceFutureIdentityReviewTriggerProfile, wantErr: "phase is invalid",
		},
		{
			name: "unknown profile rejects", phase: rlsMigrationPhaseBridge,
			profile: platformComplianceIdentityReviewTriggerProfile(255), wantErr: "profile is invalid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			action, err := decideRLSMigrationAction(test.phase, test.profile)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, action)
		})
	}

	_, err := decideRLSMigrationAction(
		compiledRLSMigrationPhase,
		platformComplianceFutureIdentityReviewTriggerProfile,
	)
	require.NoError(t, err,
		"every immutable source must carry one valid literal compile-time phase authority")
}

func TestRLSMigrationLostAcknowledgementClassificationIsReadOnlyAndFailClosed(t *testing.T) {
	t.Parallel()

	lostAcknowledgement := errors.New("connection closed while reading commit acknowledgement")
	verificationFailure := errors.New("read-only verification failed")

	assert.NoError(t, classifyRLSPolicyApplyOutcome(lostAcknowledgement, nil),
		"an exact read-only policy postcondition resolves the ambiguous acknowledgement")
	policyErr := classifyRLSPolicyApplyOutcome(lostAcknowledgement, verificationFailure)
	require.ErrorIs(t, policyErr, ErrRLSMigrationCommitOutcomeIndeterminate)
	assert.ErrorContains(t, policyErr, lostAcknowledgement.Error())
	assert.ErrorContains(t, policyErr, verificationFailure.Error())

	assert.NoError(t, classifyRLSActivationOutcome(
		lostAcknowledgement,
		platformComplianceFutureIdentityReviewTriggerProfile,
		nil,
		nil,
		nil,
	), "the exact future profile plus both read-only verifiers resolves a lost acknowledgement")

	rolledBack := classifyRLSActivationOutcome(
		lostAcknowledgement,
		platformComplianceLegacyIdentityReviewTriggerProfile,
		nil,
		nil,
		nil,
	)
	require.Error(t, rolledBack)
	assert.NotErrorIs(t, rolledBack, ErrRLSMigrationCommitOutcomeIndeterminate)
	assert.ErrorIs(t, rolledBack, lostAcknowledgement)

	for name, outcomeErr := range map[string]error{
		"mixed catalog": classifyRLSActivationOutcome(
			lostAcknowledgement,
			platformComplianceIdentityReviewTriggerProfile(255),
			verificationFailure,
			nil,
			nil,
		),
		"owner verification failure": classifyRLSActivationOutcome(
			lostAcknowledgement,
			platformComplianceFutureIdentityReviewTriggerProfile,
			nil,
			verificationFailure,
			nil,
		),
		"runtime verification failure": classifyRLSActivationOutcome(
			lostAcknowledgement,
			platformComplianceFutureIdentityReviewTriggerProfile,
			nil,
			nil,
			verificationFailure,
		),
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, outcomeErr, ErrRLSMigrationCommitOutcomeIndeterminate)
			assert.ErrorContains(t, outcomeErr, verificationFailure.Error())
		})
	}
}

func TestMigrationActivationIsSeparatedFromPreparation(t *testing.T) {
	t.Parallel()

	preparation, activation, err := splitMigrationIndexes(getIndexes())
	require.NoError(t, err)
	require.NotEmpty(t, preparation)
	require.NotEmpty(t, activation)
	assert.Contains(t, activation, "owner_trigger_installed boolean")
	assert.Contains(t, activation, "CREATE TRIGGER rereply_identity_review_contact_selector_fence")
	assert.NotContains(t, strings.Join(preparation, "\n"),
		"CREATE TRIGGER rereply_identity_review_contact_selector_fence",
		"the old-core publication must occur only after schema, seeds, backfills, and RLS")
	_, _, err = splitMigrationIndexes(nil)
	require.ErrorContains(t, err, "inventory is empty")
	_, _, err = splitMigrationIndexes([]string{"SELECT 1"})
	require.ErrorContains(t, err, "activation boundary is missing")
}

func TestWhatsAppIdentityReviewTablesUseAnExactAdditiveRollbackProfile(t *testing.T) {
	t.Parallel()

	expected := []string{
		"whatsapp_coexistence_states",
		"whatsapp_identity_review_holds",
		"whatsapp_identity_review_members",
	}
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(DirectTenantTables)
	require.NoError(t, err)
	assert.Equal(t, expected, additiveTables)
	for _, table := range expected {
		assert.NotContains(t, coreTables, table)
	}

	rules, err := PlatformComplianceTableRules()
	require.NoError(t, err)
	coreRules, additiveRules, err := platformComplianceRuleProfiles(rules)
	require.NoError(t, err)
	require.Len(t, additiveRules, len(expected))
	for index, rule := range additiveRules {
		assert.Equal(t, expected[index], rule.Table)
		assert.True(t, rule.ForceTenantRLS)
		assert.Empty(t, strings.TrimSpace(rule.AllowedRowPredicate))
	}

	// The durable v7 functions remain the legacy core. Current binaries still
	// verify every additive relation and trigger separately through rules, but a
	// rollback binary must not reject a changed v7 body or fingerprint.
	coreSQL := strings.Join([]string{
		platformComplianceScanGuardSQL(coreRules),
		platformComplianceWriteGuardSQL(coreRules),
		platformComplianceClassificationGuardSQL(coreRules),
		platformComplianceCreatorSQL(coreRules),
	}, "\n")
	for _, table := range expected {
		assert.NotContains(t, coreSQL, table)
	}
}
