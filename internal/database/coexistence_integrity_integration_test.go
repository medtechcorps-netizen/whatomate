package database_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/config"
	databasepkg "github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func identityReviewTestDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type tenantPolicyFingerprintTestRecord struct {
	Schema          string `gorm:"column:schema_name" json:"schema"`
	Table           string `gorm:"column:table_name" json:"table"`
	Policy          string `gorm:"column:policy_name" json:"policy"`
	Command         string `gorm:"column:command" json:"command"`
	Permissive      bool   `gorm:"column:permissive" json:"permissive"`
	Roles           string `gorm:"column:roles" json:"roles"`
	UsingExpression string `gorm:"column:using_expression" json:"using_expression"`
	CheckExpression string `gorm:"column:check_expression" json:"check_expression"`
}

func currentCoreTenantPolicyFingerprint(t *testing.T, db *gorm.DB) string {
	t.Helper()
	additive := map[string]struct{}{
		"whatsapp_coexistence_states":      {},
		"whatsapp_identity_review_holds":   {},
		"whatsapp_identity_review_members": {},
	}
	tables := make([]string, 0, len(databasepkg.DirectTenantTables)+len(databasepkg.RelatedTenantTables))
	for _, table := range databasepkg.DirectTenantTables {
		if _, excluded := additive[table]; !excluded {
			tables = append(tables, table)
		}
	}
	for table := range databasepkg.RelatedTenantTables {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	placeholders := make([]string, len(tables))
	arguments := make([]any, len(tables))
	for index, table := range tables {
		placeholders[index] = "?"
		arguments[index] = table
	}
	records := make([]tenantPolicyFingerprintTestRecord, 0, len(tables))
	require.NoError(t, db.Raw(fmt.Sprintf(`
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
	`, strings.Join(placeholders, ", ")), arguments...).Scan(&records).Error)
	require.Len(t, records, len(tables))
	payload, err := json.Marshal(records)
	require.NoError(t, err)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func rewriteCoreTenantPolicyFingerprintForCurrentPolicies(t *testing.T, db *gorm.DB) {
	t.Helper()
	fingerprint := currentCoreTenantPolicyFingerprint(t, db)
	require.NoError(t, db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_fingerprint()
		RETURNS text LANGUAGE sql IMMUTABLE
		SET search_path = pg_catalog, public
		AS $function$ SELECT '%s'::text $function$
	`, fingerprint)).Error)
}

func requireIdentityReviewSQLState(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.NotEqual(t, "40P01", pgErr.Code, "contention must never depend on deadlock detection")
	require.Equal(t, code, pgErr.Code)
	return pgErr
}

var identityReviewOldCoreTriggers = []struct {
	name  string
	table string
}{
	{name: "rereply_identity_review_contact_selector_fence", table: "contacts"},
	{name: "rereply_identity_review_message_wamid_owner", table: "messages"},
	{name: "rereply_identity_review_event_wamid_owner", table: "inbound_events"},
	{name: "trg_inbound_events_identity_review_guard", table: "inbound_events"},
}

func dropIdentityReviewOldCoreTriggers(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, trigger := range identityReviewOldCoreTriggers {
		require.NoError(t, db.Exec(fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON public.%s",
			trigger.name,
			trigger.table,
		)).Error)
	}
}

func dropPreAdditiveIdentityReviewRelations(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`
		ALTER TABLE public.inbound_events
		DROP CONSTRAINT fk_inbound_events_identity_review_hold_tenant
	`).Error)
	require.NoError(t, db.Exec(`
		DROP TABLE
			public.whatsapp_identity_review_members,
			public.whatsapp_identity_review_holds,
			public.whatsapp_coexistence_states
		RESTRICT
	`).Error)
}

func setupIsolatedRLSMigrationTest(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db, _, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	return db, runtimeRole
}

func setupPreAdditiveRLSMigrationTest(
	t *testing.T,
) (*gorm.DB, *gorm.DB, string) {
	t.Helper()
	db, isolatedAdminDB, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	dropPreAdditiveIdentityReviewRelations(t, db)
	return db, isolatedAdminDB, runtimeRole
}

type migrationIndexLifecycleTestState struct {
	OID        int64  `gorm:"column:index_oid"`
	Definition string `gorm:"column:definition"`
	Valid      bool   `gorm:"column:valid"`
	Ready      bool   `gorm:"column:ready"`
	Live       bool   `gorm:"column:live"`
}

func readMigrationIndexLifecycleTestState(
	t *testing.T,
	db *gorm.DB,
	indexName string,
) (migrationIndexLifecycleTestState, bool) {
	t.Helper()
	var state migrationIndexLifecycleTestState
	result := db.Raw(`
		SELECT index_state.indexrelid::bigint AS index_oid,
			pg_catalog.pg_get_indexdef(index_state.indexrelid, 0, true) AS definition,
			index_state.indisvalid AS valid,
			index_state.indisready AS ready,
			index_state.indislive AS live
		FROM pg_catalog.pg_index AS index_state
		WHERE index_state.indexrelid = pg_catalog.to_regclass(CAST(? AS text))
	`, "public."+indexName).Scan(&state)
	require.NoError(t, result.Error)
	if result.RowsAffected == 0 {
		return migrationIndexLifecycleTestState{}, false
	}
	require.EqualValues(t, 1, result.RowsAffected)
	require.Positive(t, state.OID)
	return state, true
}

func waitForMigrationIndexLifecycleTestState(
	t *testing.T,
	db *gorm.DB,
	indexName string,
	want func(migrationIndexLifecycleTestState, bool) bool,
) migrationIndexLifecycleTestState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, exists := readMigrationIndexLifecycleTestState(t, db, indexName)
		if want(state, exists) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("index %s did not reach the expected lifecycle state (exists=%t valid=%t ready=%t live=%t)",
				indexName, exists, state.Valid, state.Ready, state.Live)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func leaveInterruptedConcurrentIndexBuildTestState(
	t *testing.T,
	db *gorm.DB,
	observer *gorm.DB,
	indexName string,
	createSQL string,
) migrationIndexLifecycleTestState {
	t.Helper()
	blocker := observer.Begin()
	require.NoError(t, blocker.Error)
	defer blocker.Rollback()
	require.NoError(t, blocker.Exec(
		"LOCK TABLE public.messages IN ROW EXCLUSIVE MODE",
	).Error)

	createContext, cancelCreate := context.WithCancel(context.Background())
	defer cancelCreate()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	createConn, err := sqlDB.Conn(context.Background())
	require.NoError(t, err)
	defer createConn.Close()
	var createBackendPID int
	require.NoError(t, createConn.QueryRowContext(
		context.Background(), "SELECT pg_catalog.pg_backend_pid()",
	).Scan(&createBackendPID))
	createResult := make(chan error, 1)
	go func() {
		_, createErr := createConn.ExecContext(createContext, createSQL)
		createResult <- createErr
	}()
	waitForMigrationIndexLifecycleTestState(t, observer, indexName,
		func(state migrationIndexLifecycleTestState, exists bool) bool {
			return exists && !state.Valid
		})
	testutil.RequirePostgresBackendWaitingForLock(t, observer, createBackendPID)
	var canceled bool
	require.NoError(t, observer.Raw(
		"SELECT pg_catalog.pg_cancel_backend(?)", createBackendPID,
	).Scan(&canceled).Error)
	require.True(t, canceled, "PostgreSQL must accept cancellation of the blocked concurrent build")
	select {
	case createErr := <-createResult:
		require.Error(t, createErr)
	case <-time.After(10 * time.Second):
		t.Fatalf("canceled concurrent build for %s did not return", indexName)
	}
	// Keep the blocker until Exec confirms PostgreSQL processed the server-side
	// cancellation; releasing it earlier races the publication commit against
	// cancellation and can incorrectly leave a valid 111 index.
	require.NoError(t, blocker.Rollback().Error)
	return waitForMigrationIndexLifecycleTestState(t, observer, indexName,
		func(state migrationIndexLifecycleTestState, exists bool) bool {
			return exists && !state.Valid
		})
}

func identityReviewOldCoreTriggerBindings(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var bindings []string
	require.NoError(t, db.Raw(`
		SELECT trigger.tgname || '@' || relation.relname
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE NOT trigger.tgisinternal
		  AND namespace.nspname = 'public'
		  AND trigger.tgname IN (
			'rereply_identity_review_contact_selector_fence',
			'rereply_identity_review_message_wamid_owner',
			'rereply_identity_review_event_wamid_owner',
			'trg_inbound_events_identity_review_guard'
		  )
		ORDER BY trigger.tgname, relation.relname
	`).Scan(&bindings).Error)
	return bindings
}

func expectedIdentityReviewOldCoreTriggerBindings() []string {
	bindings := make([]string, 0, len(identityReviewOldCoreTriggers))
	for _, trigger := range identityReviewOldCoreTriggers {
		bindings = append(bindings, trigger.name+"@"+trigger.table)
	}
	sort.Strings(bindings)
	return bindings
}

func identityReviewTestMemberDigest(members []models.WhatsAppIdentityReviewMember) string {
	copyMembers := append([]models.WhatsAppIdentityReviewMember(nil), members...)
	sort.Slice(copyMembers, func(i, j int) bool { return copyMembers[i].ContactID.String() < copyMembers[j].ContactID.String() })
	parts := make([]string, 0, len(copyMembers))
	for _, member := range copyMembers {
		parts = append(parts, fmt.Sprintf("%s:%d", member.ContactID, member.SelectorReasons))
	}
	return identityReviewTestDigest(strings.Join(parts, "\n"))
}

func createIdentityReviewTestHold(
	t *testing.T,
	db *gorm.DB,
	organizationID, accountID uuid.UUID,
	members []models.WhatsAppIdentityReviewMember,
) models.WhatsAppIdentityReviewHold {
	return createIdentityReviewTestHoldForCycle(t, db, organizationID, accountID, 1, members)
}

func createIdentityReviewTestHoldForCycle(
	t *testing.T,
	db *gorm.DB,
	organizationID, accountID uuid.UUID,
	onboardingCycle uint64,
	members []models.WhatsAppIdentityReviewMember,
) models.WhatsAppIdentityReviewHold {
	t.Helper()
	holdID := uuid.New()
	for index := range members {
		members[index].OrganizationID = organizationID
		members[index].HoldID = holdID
	}
	hold := models.WhatsAppIdentityReviewHold{
		ID: holdID, OrganizationID: organizationID, WhatsAppAccountID: accountID,
		OnboardingCycle: onboardingCycle, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion,
		Supported: true, DirectPrimaryBSUID: "direct-" + holdID.String(), PrincipalGeneration: 1,
		SemanticClaimDigest:     identityReviewTestDigest("semantic:" + holdID.String()),
		SelectorBodyDigest:      identityReviewTestDigest("selector:" + holdID.String()),
		VerifiedEventDigest:     identityReviewTestDigest("event:" + holdID.String()),
		VerifiedEventProvenance: "meta_signed_webhook",
		MemberCount:             uint32(len(members)), MemberDigest: identityReviewTestMemberDigest(members),
		Version: 1, Disposition: models.WhatsAppIdentityReviewDispositionOpen,
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&hold).Error; err != nil {
			return err
		}
		if len(members) != 0 {
			return tx.Create(&members).Error
		}
		return nil
	}))
	return hold
}

func TestWhatsAppIdentityReviewOpenHoldsAreSupersededByLaterOnboardingCycle(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	oldContact := testutil.CreateTestContact(t, db, organization.ID)
	currentContact := testutil.CreateTestContact(t, db, organization.ID)
	state := models.WhatsAppCoexistenceState{
		ID:                     uuid.New(),
		OrganizationID:         organization.ID,
		WhatsAppAccountID:      account.ID,
		OnboardingStatus:       models.CoexistenceOnboardingStatusSyncing,
		OnboardingCycle:        1,
		SyncStatus:             models.CoexistenceSyncStatusInProgress,
		ContactSyncStatus:      models.CoexistenceSyncStatusRequested,
		HistoryConsent:         models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus:      models.CoexistenceSyncStatusRequested,
		LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata:      models.JSONB{},
		HistoryProgressPercent: 0,
		Version:                1,
	}
	require.NoError(t, db.Create(&state).Error)

	oldHold := createIdentityReviewTestHoldForCycle(t, db, organization.ID, account.ID, 1, []models.WhatsAppIdentityReviewMember{{
		ContactID: oldContact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	blocked, err := databasepkg.ContactHasBlockingIdentityReviewHold(db, organization.ID, oldContact.ID)
	require.NoError(t, err)
	require.True(t, blocked)

	require.NoError(t, db.Model(&models.WhatsAppCoexistenceState{}).
		Where("organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID).
		Update("onboarding_cycle", 2).Error)

	var superseded models.WhatsAppIdentityReviewHold
	require.NoError(t, db.Where("organization_id = ? AND id = ?", organization.ID, oldHold.ID).First(&superseded).Error)
	assert.Equal(t, uint64(2), superseded.Version)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionSupersededByCycle, superseded.Disposition)
	assert.NotNil(t, superseded.CycleSupersededAt)
	require.NotNil(t, superseded.SupersededByOnboardingCycle)
	assert.Equal(t, uint64(2), *superseded.SupersededByOnboardingCycle)
	assert.Nil(t, superseded.DecisionTargetContactID)
	assert.Nil(t, superseded.DecisionResolvedByID)
	assert.Nil(t, superseded.DecisionResolvedAt)
	assert.Nil(t, superseded.DecisionRequestID)
	assert.Empty(t, superseded.DecisionRequestDigest)
	assert.Empty(t, superseded.DecisionChainDigest)
	assert.Nil(t, superseded.SupersededByHoldID)
	assert.Nil(t, superseded.SupersededByGeneration)

	blocked, err = databasepkg.ContactHasBlockingIdentityReviewHold(db, organization.ID, oldContact.ID)
	require.NoError(t, err)
	assert.False(t, blocked)

	currentHold := createIdentityReviewTestHoldForCycle(t, db, organization.ID, account.ID, 2, []models.WhatsAppIdentityReviewMember{{
		ContactID: currentContact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, currentHold.Disposition)
	blocked, err = databasepkg.ContactHasBlockingIdentityReviewHold(db, organization.ID, currentContact.ID)
	require.NoError(t, err)
	assert.True(t, blocked)

	require.NoError(t, db.Model(&models.WhatsAppCoexistenceState{}).
		Where("organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID).
		Update("onboarding_cycle", 2).Error, "an idempotent same-cycle write remains valid")
	_ = requireIdentityReviewSQLState(t, db.Model(&models.WhatsAppCoexistenceState{}).
		Where("organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID).
		Update("onboarding_cycle", 1).Error, "23514")
	_ = requireIdentityReviewSQLState(t, db.Model(&models.WhatsAppCoexistenceState{}).
		Where("organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID).
		Update("onboarding_cycle", 4).Error, "23514")
	var durableState models.WhatsAppCoexistenceState
	require.NoError(t, db.Where(
		"organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID,
	).First(&durableState).Error)
	assert.Equal(t, uint64(2), durableState.OnboardingCycle)
	var durableCurrentHold models.WhatsAppIdentityReviewHold
	require.NoError(t, db.Where(
		"organization_id = ? AND id = ?", organization.ID, currentHold.ID,
	).First(&durableCurrentHold).Error)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, durableCurrentHold.Disposition,
		"a rejected decrement or jump cannot supersede the current-cycle hold")
}

func TestWhatsAppIdentityReviewTriggerBodiesIgnoreRuntimeTemporaryShadows(t *testing.T) {
	t.Run("cycle supersession uses public holds", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		organization := createGuardTestOrganization(t, db, false)
		account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
		contact := testutil.CreateTestContact(t, db, organization.ID)
		state := models.WhatsAppCoexistenceState{
			ID:                     uuid.New(),
			OrganizationID:         organization.ID,
			WhatsAppAccountID:      account.ID,
			OnboardingStatus:       models.CoexistenceOnboardingStatusSyncing,
			OnboardingCycle:        1,
			SyncStatus:             models.CoexistenceSyncStatusInProgress,
			ContactSyncStatus:      models.CoexistenceSyncStatusRequested,
			HistoryConsent:         models.CoexistenceHistoryConsentUnknown,
			HistorySyncStatus:      models.CoexistenceSyncStatusRequested,
			LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
			LifecycleMetadata:      models.JSONB{},
			HistoryProgressPercent: 0,
			Version:                1,
		}
		require.NoError(t, db.Create(&state).Error)
		hold := createIdentityReviewTestHoldForCycle(t, db, organization.ID, account.ID, 1, []models.WhatsAppIdentityReviewMember{{
			ContactID: contact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
		}})

		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
				return err
			}
			if err := databasepkg.SetTenantContext(tx, organization.ID); err != nil {
				return err
			}
			if err := tx.Exec(`CREATE TEMP TABLE pg_temp.whatsapp_identity_review_holds (
				organization_id uuid, whats_app_account_id uuid, onboarding_cycle bigint,
				version bigint, disposition text, cycle_superseded_at timestamptz,
				superseded_by_onboarding_cycle bigint, updated_at timestamptz
			) ON COMMIT DROP`).Error; err != nil {
				return err
			}
			return tx.Exec(`UPDATE public.whatsapp_coexistence_states
				SET onboarding_cycle = 2
				WHERE organization_id = ? AND whats_app_account_id = ?`, organization.ID, account.ID).Error
		}))

		var stored models.WhatsAppIdentityReviewHold
		require.NoError(t, db.Where("organization_id = ? AND id = ?", organization.ID, hold.ID).First(&stored).Error)
		assert.Equal(t, models.WhatsAppIdentityReviewDispositionSupersededByCycle, stored.Disposition)
		require.NotNil(t, stored.SupersededByOnboardingCycle)
		assert.Equal(t, uint64(2), *stored.SupersededByOnboardingCycle)
	})

	t.Run("deferred completeness uses public membership", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		organization := createGuardTestOrganization(t, db, false)
		account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
		contact := testutil.CreateTestContact(t, db, organization.ID)
		holdID := uuid.New()
		member := models.WhatsAppIdentityReviewMember{
			OrganizationID: organization.ID, HoldID: holdID, ContactID: contact.ID,
			SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
		}
		memberDigest := identityReviewTestMemberDigest([]models.WhatsAppIdentityReviewMember{member})

		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
				return err
			}
			if err := databasepkg.SetTenantContext(tx, organization.ID); err != nil {
				return err
			}
			if err := tx.Exec(`CREATE TEMP TABLE pg_temp.whatsapp_identity_review_holds (
				organization_id uuid, id uuid, member_count bigint, member_digest text
			) ON COMMIT DROP`).Error; err != nil {
				return err
			}
			if err := tx.Exec(`CREATE TEMP TABLE pg_temp.whatsapp_identity_review_members (
				organization_id uuid, hold_id uuid, contact_id uuid, selector_reasons integer
			) ON COMMIT DROP`).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO pg_temp.whatsapp_identity_review_holds
				(organization_id, id, member_count, member_digest) VALUES (?, ?, 1, ?)`,
				organization.ID, holdID, memberDigest).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO pg_temp.whatsapp_identity_review_members
				(organization_id, hold_id, contact_id, selector_reasons) VALUES (?, ?, ?, ?)`,
				organization.ID, holdID, contact.ID, member.SelectorReasons).Error; err != nil {
				return err
			}
			return tx.Exec(`INSERT INTO public.whatsapp_identity_review_holds (
				id, organization_id, whats_app_account_id, onboarding_cycle, protocol_version,
				supported, direct_primary_bsuid, parent_bsuid, phone, principal_generation,
				semantic_claim_digest, selector_body_digest, verified_event_digest,
				verified_event_provenance, member_count, member_digest, version, disposition,
				decision_request_digest, decision_chain_digest
			) VALUES (?, ?, ?, 1, ?, TRUE, ?, '', '', 1, ?, ?, ?, ?, 1, ?, 1, 'open', '', '')`,
				holdID, organization.ID, account.ID, models.WhatsAppIdentityReviewProtocolVersion,
				"direct-"+holdID.String(), identityReviewTestDigest("semantic:"+holdID.String()),
				identityReviewTestDigest("selector:"+holdID.String()), identityReviewTestDigest("event:"+holdID.String()),
				"meta_signed_webhook", memberDigest).Error
		})
		pgErr := requireIdentityReviewSQLState(t, err, "P0001")
		assert.Contains(t, pgErr.Message, "membership is incomplete or noncanonical")
		var count int64
		require.NoError(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
			Where("organization_id = ? AND id = ?", organization.ID, holdID).Count(&count).Error)
		assert.Zero(t, count)
	})
}

func TestWhatsAppCoexistenceIntegrityRejectsCrossTenantAndInvalidState(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organizationA := testutil.CreateTestOrganization(t, db)
	organizationB := testutil.CreateTestOrganization(t, db)
	accountA := testutil.CreateTestWhatsAppAccount(t, db, organizationA.ID)
	accountB := testutil.CreateTestWhatsAppAccount(t, db, organizationB.ID)

	state := models.WhatsAppCoexistenceState{
		ID:                     uuid.New(),
		OrganizationID:         organizationA.ID,
		WhatsAppAccountID:      accountA.ID,
		OnboardingStatus:       models.CoexistenceOnboardingStatusSyncing,
		SyncStatus:             models.CoexistenceSyncStatusInProgress,
		ContactSyncStatus:      models.CoexistenceSyncStatusRequested,
		HistoryConsent:         models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus:      models.CoexistenceSyncStatusRequested,
		HistoryProgressPercent: 25,
		LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata:      models.JSONB{},
		Version:                1,
	}
	require.NoError(t, db.Create(&state).Error)

	contact := testutil.CreateTestContactWith(
		t,
		db,
		organizationA.ID,
		testutil.WithContactAccount(accountA.Name),
	)
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationA.ID,
		WhatsAppAccount:   accountA.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.coexistence-unique",
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	duplicateMessage := message
	duplicateMessage.ID = uuid.New()
	require.Error(t, db.Create(&duplicateMessage).Error,
		"one tenant must not store the same provider WAMID twice")

	crossTenant := state
	crossTenant.ID = uuid.New()
	crossTenant.OrganizationID = organizationA.ID
	crossTenant.WhatsAppAccountID = accountB.ID
	require.Error(t, db.Create(&crossTenant).Error,
		"an organization must not reference another tenant's WhatsApp account")

	require.Error(t, db.Model(&models.WhatsAppCoexistenceState{}).
		Where("id = ?", state.ID).
		Update("whats_app_account_id", accountB.ID).Error,
		"an existing state must not be movable to another tenant's account")

	invalidUpdates := []struct {
		column string
		value  any
	}{
		{column: "onboarding_status", value: "impossible"},
		{column: "contact_sync_attempts", value: -1},
		{column: "history_sync_attempts", value: -1},
		{column: "history_progress_percent", value: 101},
		{column: "history_last_phase", value: -1},
		{column: "history_last_chunk_order", value: -1},
		{column: "version", value: 0},
	}
	for _, invalid := range invalidUpdates {
		t.Run(invalid.column, func(t *testing.T) {
			require.Error(t, db.Model(&models.WhatsAppCoexistenceState{}).
				Where("id = ?", state.ID).
				Update(invalid.column, invalid.value).Error)
		})
	}
}

func TestWhatsAppIdentityReviewIntegrityRequiresCompleteImmutableMembership(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	first := testutil.CreateTestContact(t, db, organization.ID)
	second := testutil.CreateTestContact(t, db, organization.ID)
	members := []models.WhatsAppIdentityReviewMember{
		{ContactID: first.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID},
		{ContactID: second.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone},
	}
	hold := createIdentityReviewTestHold(t, db, organization.ID, account.ID, members)

	blocked, err := databasepkg.ContactHasBlockingIdentityReviewHold(db, organization.ID, first.ID)
	require.NoError(t, err)
	assert.True(t, blocked)

	late := models.WhatsAppIdentityReviewMember{
		OrganizationID:  organization.ID,
		HoldID:          hold.ID,
		ContactID:       testutil.CreateTestContact(t, db, organization.ID).ID,
		SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone,
	}
	require.Error(t, db.Create(&late).Error, "a committed complete set cannot accept a late member")
	require.Error(t, db.Model(&models.WhatsAppIdentityReviewMember{}).
		Where("organization_id = ? AND hold_id = ? AND contact_id = ?", organization.ID, hold.ID, first.ID).
		Update("selector_reasons", models.WhatsAppIdentityReviewSelectorParentBSUID).Error)
	require.Error(t, db.Delete(&models.WhatsAppIdentityReviewMember{},
		"organization_id = ? AND hold_id = ? AND contact_id = ?", organization.ID, hold.ID, first.ID).Error)
	require.Error(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ?", organization.ID, hold.ID).
		Update("phone", "60111111111").Error)
	require.Error(t, db.Delete(&models.WhatsAppIdentityReviewHold{},
		"organization_id = ? AND id = ?", organization.ID, hold.ID).Error)
}

func TestWhatsAppIdentityReviewIntegrityRejectsIncompleteAndCrossTenantSets(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organizationA := testutil.CreateTestOrganization(t, db)
	organizationB := testutil.CreateTestOrganization(t, db)
	accountA := testutil.CreateTestWhatsAppAccount(t, db, organizationA.ID)
	accountB := testutil.CreateTestWhatsAppAccount(t, db, organizationB.ID)
	contactA := testutil.CreateTestContact(t, db, organizationA.ID)
	contactB := testutil.CreateTestContact(t, db, organizationB.ID)

	missingMember := models.WhatsAppIdentityReviewMember{
		ContactID: contactA.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}
	holdID := uuid.New()
	missingMember.OrganizationID = organizationA.ID
	missingMember.HoldID = holdID
	incomplete := models.WhatsAppIdentityReviewHold{
		ID: holdID, OrganizationID: organizationA.ID, WhatsAppAccountID: accountA.ID,
		OnboardingCycle: 1, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion,
		Supported: true, DirectPrimaryBSUID: "direct-incomplete", PrincipalGeneration: 1,
		SemanticClaimDigest:     identityReviewTestDigest("semantic-incomplete"),
		SelectorBodyDigest:      identityReviewTestDigest("selector-incomplete"),
		VerifiedEventDigest:     identityReviewTestDigest("event-incomplete"),
		VerifiedEventProvenance: "meta_signed_webhook",
		MemberCount:             1, MemberDigest: identityReviewTestMemberDigest([]models.WhatsAppIdentityReviewMember{missingMember}),
		Version: 1, Disposition: models.WhatsAppIdentityReviewDispositionOpen,
	}
	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&incomplete).Error
	}), "the deferred guard must reject a header without its complete set")

	crossAccount := incomplete
	crossAccount.ID = uuid.New()
	crossAccount.WhatsAppAccountID = accountB.ID
	crossAccount.SemanticClaimDigest = identityReviewTestDigest("cross-account")
	crossAccount.DirectPrimaryBSUID = "direct-cross-account"
	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&crossAccount).Error
	}), "a hold cannot reference another tenant's account")

	crossMember := models.WhatsAppIdentityReviewMember{
		OrganizationID:  organizationA.ID,
		HoldID:          uuid.New(),
		ContactID:       contactB.ID,
		SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}
	crossHold := incomplete
	crossHold.ID = crossMember.HoldID
	crossHold.SemanticClaimDigest = identityReviewTestDigest("cross-member")
	crossHold.DirectPrimaryBSUID = "direct-cross-member"
	crossHold.MemberDigest = identityReviewTestMemberDigest([]models.WhatsAppIdentityReviewMember{crossMember})
	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&crossHold).Error; err != nil {
			return err
		}
		return tx.Create(&crossMember).Error
	}), "membership cannot reference another tenant's contact")
}

func TestWhatsAppIdentityReviewReservedInboundEventRejectsSQLNullAuthorityFields(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	hold := createIdentityReviewTestHold(t, db, organization.ID, account.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: contact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Channel: models.ChannelWhatsApp, Provider: "meta_legacy", Name: "null-authority",
		ExternalAccountID: "null-authority-" + uuid.NewString(), Status: models.ChannelAccountStatusActive,
		Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channel).Error)

	insertReserved := func(providerEventID, errorCode, errorMessage any, dedupeKey string) error {
		return db.Exec(`INSERT INTO public.inbound_events (
			id, organization_id, channel_account_id, dedupe_key, provider_event_id,
			event_type, status, signature_valid, received_at, attempt_count,
			error_code, error_message, protocol, review_hold_id, headers, payload
		) VALUES (?, ?, ?, ?, ?, 'review_pending', 'pending', TRUE, clock_timestamp(), 0,
			?, ?, 'whatsapp_identity_review_v1', ?, '{}'::jsonb,
			'{"schema_version":1,"message_type":"text","content":"held"}'::jsonb)`,
			uuid.New(), organization.ID, channel.ID, dedupeKey, providerEventID,
			errorCode, errorMessage, hold.ID).Error
	}

	for _, testCase := range []struct {
		name                          string
		providerEventID, code, detail any
		dedupeKey                     string
	}{
		{name: "first null provider event ID", providerEventID: nil, code: "", detail: "", dedupeKey: "identity-review:null-provider-a"},
		{name: "second null provider event ID", providerEventID: nil, code: "", detail: "", dedupeKey: "identity-review:null-provider-b"},
		{name: "null error code", providerEventID: "wamid.null-code-" + uuid.NewString(), code: nil, detail: ""},
		{name: "null error message", providerEventID: "wamid.null-message-" + uuid.NewString(), code: "", detail: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dedupeKey := testCase.dedupeKey
			if dedupeKey == "" {
				dedupeKey = "identity-review:" + testCase.providerEventID.(string)
			}
			pgErr := requireIdentityReviewSQLState(t,
				insertReserved(testCase.providerEventID, testCase.code, testCase.detail, dedupeKey), "23514")
			assert.Equal(t, "chk_inbound_events_identity_review_shape", pgErr.ConstraintName)
		})
	}
	var count int64
	require.NoError(t, db.Model(&models.InboundEvent{}).
		Where("organization_id = ? AND protocol = ?", organization.ID, models.WhatsAppIdentityReviewInboundProtocol).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestWhatsAppIdentityReviewIntegrityAllowsOnlyExactFirstDecision(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	target := testutil.CreateTestContact(t, db, organization.ID)
	nonMember := testutil.CreateTestContact(t, db, organization.ID)
	hold := createIdentityReviewTestHold(t, db, organization.ID, account.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: target.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})

	requestID := uuid.New()
	resolverID := uuid.New()
	resolvedAt := time.Now().UTC()
	require.NoError(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ?", organization.ID, hold.ID).
		Updates(map[string]any{
			"version": 2, "disposition": models.WhatsAppIdentityReviewDispositionFutureRouting,
			"decision_target_contact_id": target.ID, "decision_resolved_by_id": resolverID,
			"decision_resolved_at": resolvedAt, "decision_request_id": requestID,
			"decision_request_digest": identityReviewTestDigest("decision-request"),
			"decision_chain_digest":   identityReviewTestDigest("decision-chain"),
		}).Error)

	blocked, err := databasepkg.ContactHasBlockingIdentityReviewHold(db, organization.ID, target.ID)
	require.NoError(t, err)
	assert.False(t, blocked)
	require.Error(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ?", organization.ID, hold.ID).
		Updates(map[string]any{"version": 1, "disposition": models.WhatsAppIdentityReviewDispositionOpen}).Error)
	require.Error(t, db.Delete(&models.WhatsAppIdentityReviewHold{},
		"organization_id = ? AND id = ?", organization.ID, hold.ID).Error)

	other := createIdentityReviewTestHold(t, db, organization.ID, account.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: target.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	require.Error(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ?", organization.ID, other.ID).
		Updates(map[string]any{
			"version": 2, "disposition": models.WhatsAppIdentityReviewDispositionFutureRouting,
			"decision_target_contact_id": nonMember.ID, "decision_resolved_by_id": resolverID,
			"decision_resolved_at": resolvedAt, "decision_request_id": uuid.New(),
			"decision_request_digest": identityReviewTestDigest("bad-decision-request"),
			"decision_chain_digest":   identityReviewTestDigest("bad-decision-chain"),
		}).Error, "a future route must target an exact immutable member")

	nextCycle := uint64(2)
	terminalAt := time.Now().UTC()
	fabricatedTerminal := models.WhatsAppIdentityReviewHold{
		ID: uuid.New(), OrganizationID: organization.ID, WhatsAppAccountID: account.ID,
		OnboardingCycle: 1, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion,
		Supported: true, DirectPrimaryBSUID: "direct-fabricated-terminal", PrincipalGeneration: 99,
		SemanticClaimDigest:     identityReviewTestDigest("fabricated-terminal-semantic"),
		SelectorBodyDigest:      identityReviewTestDigest("fabricated-terminal-selector"),
		VerifiedEventDigest:     identityReviewTestDigest("fabricated-terminal-event"),
		VerifiedEventProvenance: "meta_signed_webhook",
		MemberCount:             0, MemberDigest: identityReviewTestDigest(""), Version: 2,
		Disposition:                 models.WhatsAppIdentityReviewDispositionSupersededByCycle,
		CycleSupersededAt:           &terminalAt,
		SupersededByOnboardingCycle: &nextCycle,
	}
	pgErr := requireIdentityReviewSQLState(t, db.Create(&fabricatedTerminal).Error, "23514")
	assert.Contains(t, pgErr.Message, "holds must be inserted in the exact open state",
		"terminal audit history must be reachable only through the guarded open transition")
}

func TestWhatsAppIdentityReviewReservedInboundEventIsTenantBoundImmutableAndFirstWinner(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organizationA := testutil.CreateTestOrganization(t, db)
	organizationB := testutil.CreateTestOrganization(t, db)
	accountA := testutil.CreateTestWhatsAppAccount(t, db, organizationA.ID)
	accountB := testutil.CreateTestWhatsAppAccount(t, db, organizationB.ID)
	contactA := testutil.CreateTestContactWith(t, db, organizationA.ID, testutil.WithContactAccount(accountA.Name))
	contactB := testutil.CreateTestContact(t, db, organizationB.ID)
	holdA := createIdentityReviewTestHold(t, db, organizationA.ID, accountA.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: contactA.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	holdB := createIdentityReviewTestHold(t, db, organizationB.ID, accountB.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: contactB.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})

	newChannel := func(suffix string) models.ChannelAccount {
		return models.ChannelAccount{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
			Channel: models.ChannelWhatsApp, Provider: "meta_legacy", Name: "review-" + suffix,
			ExternalAccountID: "review-" + suffix + "-" + uuid.NewString(),
			Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
		}
	}
	channelA := newChannel("a")
	channelB := newChannel("b")
	require.NoError(t, db.Create(&channelA).Error)
	require.NoError(t, db.Create(&channelB).Error)

	claimed, err := databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, "wamid.review.staged")
	require.NoError(t, err)
	assert.False(t, claimed)

	event := models.InboundEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
		ChannelAccountID: channelA.ID, DedupeKey: "identity-review:wamid.review.staged",
		ProviderEventID: "wamid.review.staged", EventType: models.WhatsAppIdentityReviewPendingEvent,
		Status: models.InboundEventStatusPending, SignatureValid: true, ReceivedAt: time.Now().UTC(),
		Protocol: models.WhatsAppIdentityReviewInboundProtocol, ReviewHoldID: &holdA.ID,
		Headers: models.JSONB{}, Payload: models.JSONB{
			"schema_version": 1, "message_type": "text", "content": "held content",
		},
	}
	require.NoError(t, db.Create(&event).Error)
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, event.ProviderEventID)
	require.NoError(t, err)
	assert.True(t, claimed)
	conversation := models.InboxConversation{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
		ChannelAccountID: channelA.ID, ContactID: contactA.ID, Channel: models.ChannelWhatsApp,
		ExternalConversationID: "identity-review-owner-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
		Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&conversation).Error)
	linkedAfterEvent := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
		WhatsAppAccount: accountA.Name, ContactID: contactA.ID,
		WhatsAppMessageID: event.ProviderEventID, InboxConversationID: &conversation.ID,
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
		Status: models.MessageStatusReceived, Metadata: models.JSONB{},
	}
	require.Error(t, db.Create(&linkedAfterEvent).Error,
		"a linked channel message cannot steal an identity-review event WAMID")

	linkedMessageOwner := linkedAfterEvent
	linkedMessageOwner.ID = uuid.New()
	linkedMessageOwner.WhatsAppMessageID = "wamid.review.linked-message-first"
	require.NoError(t, db.Create(&linkedMessageOwner).Error)
	linkedMessageDuplicate := linkedMessageOwner
	linkedMessageDuplicate.ID = uuid.New()
	require.Error(t, db.Create(&linkedMessageDuplicate).Error,
		"two linked messages cannot own one tenant WAMID")
	eventAfterLinkedMessage := event
	eventAfterLinkedMessage.ID = uuid.New()
	eventAfterLinkedMessage.ProviderEventID = linkedMessageOwner.WhatsAppMessageID
	eventAfterLinkedMessage.DedupeKey = "identity-review:" + linkedMessageOwner.WhatsAppMessageID
	require.Error(t, db.Create(&eventAfterLinkedMessage).Error,
		"an identity-review event cannot steal a linked channel message WAMID")
	require.NoError(t, db.Delete(&linkedMessageOwner).Error)
	softDeletedMessageDuplicate := linkedMessageOwner
	softDeletedMessageDuplicate.ID = uuid.New()
	softDeletedMessageDuplicate.DeletedAt = gorm.DeletedAt{}
	require.Error(t, db.Create(&softDeletedMessageDuplicate).Error,
		"a soft-deleted linked message remains the same-store historical WAMID owner")
	require.Error(t, db.Create(&eventAfterLinkedMessage).Error,
		"a soft-deleted linked message remains the historical first WAMID owner")
	var durableLinkedOwner models.Message
	require.NoError(t, db.Unscoped().Where("organization_id = ? AND id = ?", organizationA.ID, linkedMessageOwner.ID).
		First(&durableLinkedOwner).Error)
	assert.Equal(t, true, durableLinkedOwner.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey])
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ? AND id = ?", organizationA.ID, conversation.ID).
		Update("channel", models.ChannelInstagram).Error)
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, linkedMessageOwner.WhatsAppMessageID)
	require.NoError(t, err)
	assert.True(t, claimed, "conversation metadata changes cannot demote historical WhatsApp ownership")
	require.Error(t, db.Create(&eventAfterLinkedMessage).Error,
		"conversation metadata changes cannot expose a claimed WAMID")
	withoutMarker := models.JSONB{}
	for key, value := range durableLinkedOwner.Metadata {
		if key != databasepkg.WhatsAppWAMIDOwnerMetadataKey {
			withoutMarker[key] = value
		}
	}
	require.NoError(t, db.Unscoped().Model(&models.Message{}).
		Where("organization_id = ? AND id = ?", organizationA.ID, linkedMessageOwner.ID).
		Update("metadata", withoutMarker).Error)
	require.NoError(t, db.Unscoped().Where("organization_id = ? AND id = ?", organizationA.ID, linkedMessageOwner.ID).
		First(&durableLinkedOwner).Error)
	assert.Equal(t, true, durableLinkedOwner.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey],
		"the row trigger must restore the immutable ownership marker")
	pgErr := requireIdentityReviewSQLState(t, db.Unscoped().Where(
		"organization_id = ? AND id = ?", organizationA.ID, linkedMessageOwner.ID,
	).Delete(&models.Message{}).Error, "23514")
	assert.Empty(t, pgErr.ConstraintName)

	channelOtherTenant := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationB.ID,
		Channel: models.ChannelWhatsApp, Provider: "meta_legacy", Name: "review-other-tenant",
		ExternalAccountID: "review-other-tenant-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channelOtherTenant).Error)
	crossTenantSameWAMID := eventAfterLinkedMessage
	crossTenantSameWAMID.ID = uuid.New()
	crossTenantSameWAMID.OrganizationID = organizationB.ID
	crossTenantSameWAMID.ChannelAccountID = channelOtherTenant.ID
	crossTenantSameWAMID.ReviewHoldID = &holdB.ID
	require.NoError(t, db.Create(&crossTenantSameWAMID).Error,
		"the same WAMID in an independent tenant must not conflict")

	duplicate := event
	duplicate.ID = uuid.New()
	duplicate.ChannelAccountID = channelB.ID
	duplicate.DedupeKey = "identity-review:" + event.ProviderEventID
	require.Error(t, db.Create(&duplicate).Error, "two account shadows cannot stage one tenant WAMID twice")
	unsanitized := event
	unsanitized.ID = uuid.New()
	unsanitized.ProviderEventID = "wamid.review.unsanitized"
	unsanitized.DedupeKey = "identity-review:" + unsanitized.ProviderEventID
	unsanitized.Payload = models.JSONB{
		"schema_version": 1, "message_type": "text", "content": "held content",
		"raw_webhook": "must not be persisted",
	}
	require.Error(t, db.Create(&unsanitized).Error, "reserved payload rejects non-allowlisted raw fields")
	audio := event
	audio.ID = uuid.New()
	audio.ProviderEventID = "wamid.review.audio"
	audio.DedupeKey = "identity-review:" + audio.ProviderEventID
	audio.Payload = models.JSONB{
		"schema_version": 1, "message_type": "audio", "content": "",
		"media_id": "audio-object", "media_sha256": "", "media_generation": 1,
		"media_status": "pending", "media_revision": identityReviewTestDigest("audio-revision"),
	}
	require.NoError(t, db.Create(&audio).Error, "audio may bind an exact empty provider SHA while retaining a revision digest")
	readyPayload := models.JSONB{
		"schema_version": 1, "message_type": "audio", "content": "",
		"media_id": "audio-object", "media_sha256": "", "media_generation": 1,
		"media_status": "ready", "media_revision": identityReviewTestDigest("audio-revision"),
		"media_url": "tenant/review/audio-object", "media_hydrated_id": "audio-object",
	}
	directReady := audio
	directReady.ID = uuid.New()
	directReady.ProviderEventID = "wamid.review.direct-ready"
	directReady.DedupeKey = "identity-review:" + directReady.ProviderEventID
	directReady.Payload = readyPayload
	require.Error(t, db.Create(&directReady).Error, "ready media cannot bypass the pending receipt transition")
	require.NoError(t, db.Model(&models.InboundEvent{}).
		Where("organization_id = ? AND id = ?", organizationA.ID, audio.ID).
		Update("payload", readyPayload).Error, "the single exact pending-to-ready hydration transition is allowed")
	tamperedReady := models.JSONB{}
	for key, value := range readyPayload {
		tamperedReady[key] = value
	}
	tamperedReady["content"] = "changed after staging"
	require.Error(t, db.Model(&models.InboundEvent{}).
		Where("organization_id = ? AND id = ?", organizationA.ID, audio.ID).
		Update("payload", tamperedReady).Error, "ready media content remains immutable")
	badMediaHash := audio
	badMediaHash.ID = uuid.New()
	badMediaHash.ProviderEventID = "wamid.review.bad-media-hash"
	badMediaHash.DedupeKey = "identity-review:" + badMediaHash.ProviderEventID
	badMediaHash.Payload = models.JSONB{}
	for key, value := range audio.Payload {
		badMediaHash.Payload[key] = value
	}
	badMediaHash.Payload["media_sha256"] = "not-a-digest"
	require.Error(t, db.Create(&badMediaHash).Error, "non-empty media SHA must be lowercase SHA-256")
	require.Error(t, db.Model(&models.InboundEvent{}).Where("id = ?", event.ID).
		Update("status", models.InboundEventStatusProcessed).Error)
	require.Error(t, db.Delete(&models.InboundEvent{}, "id = ?", event.ID).Error)

	crossTenant := event
	crossTenant.ID = uuid.New()
	crossTenant.ProviderEventID = "wamid.review.cross-tenant"
	crossTenant.DedupeKey = "identity-review:" + crossTenant.ProviderEventID
	crossTenant.ReviewHoldID = &holdB.ID
	require.Error(t, db.Create(&crossTenant).Error, "reserved event cannot reference another tenant's hold")

	generic := models.InboundEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
		ChannelAccountID: channelA.ID, DedupeKey: "generic:" + uuid.NewString(),
		ProviderEventID: "generic-event", EventType: "message", Status: models.InboundEventStatusPending,
		SignatureValid: true, ReceivedAt: time.Now().UTC(), Headers: models.JSONB{}, Payload: models.JSONB{},
	}
	require.NoError(t, db.Create(&generic).Error)
	require.NoError(t, db.Model(&generic).Update("status", models.InboundEventStatusProcessed).Error)
	require.NoError(t, db.Delete(&generic).Error, "ordinary inbound event semantics must remain unchanged")

	// The legacy column also stores provider IDs for linked non-WhatsApp inbox
	// messages. Their conversation-scoped identifiers are not WhatsApp WAMID
	// authority and may legitimately collide across providers/conversations.
	instagramAccount := newChannel("instagram")
	instagramAccount.Channel = models.ChannelInstagram
	instagramAccount.Provider = "meta_graph"
	messengerAccount := newChannel("messenger")
	messengerAccount.Channel = models.ChannelMessenger
	messengerAccount.Provider = "meta_graph"
	require.NoError(t, db.Create(&instagramAccount).Error)
	require.NoError(t, db.Create(&messengerAccount).Error)
	newProviderConversation := func(channelAccount models.ChannelAccount) models.InboxConversation {
		return models.InboxConversation{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
			ChannelAccountID: channelAccount.ID, ContactID: contactA.ID, Channel: channelAccount.Channel,
			ExternalConversationID: "provider-collision-" + uuid.NewString(),
			Status:                 models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
			Config: models.JSONB{}, Metadata: models.JSONB{},
		}
	}
	instagramConversation := newProviderConversation(instagramAccount)
	messengerConversation := newProviderConversation(messengerAccount)
	require.NoError(t, db.Create(&instagramConversation).Error)
	require.NoError(t, db.Create(&messengerConversation).Error)
	providerScopedWAMID := "provider-scoped-not-whatsapp"
	newProviderMessage := func(conversation models.InboxConversation) models.Message {
		return models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
			WhatsAppAccount: accountA.Name, ContactID: contactA.ID,
			WhatsAppMessageID: providerScopedWAMID, InboxConversationID: &conversation.ID,
			Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
			Status: models.MessageStatusReceived, Metadata: models.JSONB{},
		}
	}
	instagramMessage := newProviderMessage(instagramConversation)
	messengerMessage := newProviderMessage(messengerConversation)
	require.NoError(t, db.Create(&instagramMessage).Error)
	require.NoError(t, db.Create(&messengerMessage).Error,
		"non-WhatsApp external message IDs remain conversation-scoped")
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, providerScopedWAMID)
	require.NoError(t, err)
	assert.False(t, claimed, "non-WhatsApp provider IDs are not WAMID owners")
	providerCollisionReview := event
	providerCollisionReview.ID = uuid.New()
	providerCollisionReview.ProviderEventID = providerScopedWAMID
	providerCollisionReview.DedupeKey = "identity-review:" + providerScopedWAMID
	require.NoError(t, db.Create(&providerCollisionReview).Error,
		"a reserved WhatsApp receipt may coexist with unrelated provider IDs")

	message := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationA.ID,
		WhatsAppAccount: accountA.Name, ContactID: contactA.ID, WhatsAppMessageID: "wamid.review.message",
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
		Status: models.MessageStatusReceived, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, message.WhatsAppMessageID)
	require.NoError(t, err)
	assert.True(t, claimed)
	require.NoError(t, db.Delete(&message).Error)
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organizationA.ID, message.WhatsAppMessageID)
	require.NoError(t, err)
	assert.True(t, claimed, "a soft-deleted first Message winner must never be revived in another store")
}

func TestWhatsAppIdentityReviewWAMIDMigrationNormalizesHistoricalOwnership(t *testing.T) {
	db, _, _, _ := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	newChannel := func(channel models.Channel, provider, suffix string) models.ChannelAccount {
		return models.ChannelAccount{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			Channel: channel, Provider: provider, Name: "migration-" + suffix,
			ExternalAccountID: "migration-" + suffix + "-" + uuid.NewString(),
			Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
		}
	}
	legacyChannel := newChannel(models.ChannelWhatsApp, "meta_legacy", "legacy")
	providerNeutralChannel := newChannel(models.ChannelMessenger, "meta_graph", "neutral")
	require.NoError(t, db.Create(&legacyChannel).Error)
	require.NoError(t, db.Create(&providerNeutralChannel).Error)
	newConversation := func(channelAccount models.ChannelAccount, suffix string) models.InboxConversation {
		return models.InboxConversation{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			ChannelAccountID: channelAccount.ID, ContactID: contact.ID, Channel: channelAccount.Channel,
			ExternalConversationID: "migration-" + suffix + "-" + uuid.NewString(),
			Status:                 models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
			Config: models.JSONB{}, Metadata: models.JSONB{},
		}
	}
	legacyConversation := newConversation(legacyChannel, "legacy")
	providerNeutralConversation := newConversation(providerNeutralChannel, "neutral")
	require.NoError(t, db.Create(&legacyConversation).Error)
	require.NoError(t, db.Create(&providerNeutralConversation).Error)

	dropIdentityReviewOldCoreTriggers(t, db)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db),
		"the compatibility profile must be exact before activation")
	newMessage := func(wamid string, conversationID uuid.UUID, metadata models.JSONB) models.Message {
		return models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			WhatsAppAccount: account.Name, ContactID: contact.ID, WhatsAppMessageID: wamid,
			InboxConversationID: &conversationID, Direction: models.DirectionIncoming,
			MessageType: models.MessageTypeText, Status: models.MessageStatusReceived, Metadata: metadata,
		}
	}
	legacyOwner := newMessage("wamid.migration.legacy-owner", legacyConversation.ID, models.JSONB{})
	providerNeutral := newMessage("provider.migration.not-a-wamid", providerNeutralConversation.ID, models.JSONB{
		databasepkg.WhatsAppWAMIDOwnerMetadataKey: true,
	})
	require.NoError(t, db.Create(&legacyOwner).Error)
	require.NoError(t, db.Create(&providerNeutral).Error)
	migrationErr := databasepkg.CreateIndexes(db)
	require.Error(t, migrationErr)
	assert.Contains(t, migrationErr.Error(), "reserved WhatsApp WAMID owner marker predates its authority trigger")
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db),
		"a failed atomic activation must leave the complete legacy profile visible")
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND id = ?", organization.ID, providerNeutral.ID).
		Update("metadata", models.JSONB{}).Error)
	require.NoError(t, databasepkg.CreateIndexes(db), "the locked migration must normalize existing marker state")
	assert.Equal(t, expectedIdentityReviewOldCoreTriggerBindings(), identityReviewOldCoreTriggerBindings(t, db),
		"a successful activation must publish all four old-core triggers together")

	var durableLegacy, durableNeutral models.Message
	require.NoError(t, db.Unscoped().First(&durableLegacy, "id = ?", legacyOwner.ID).Error)
	require.NoError(t, db.Unscoped().First(&durableNeutral, "id = ?", providerNeutral.ID).Error)
	assert.Equal(t, true, durableLegacy.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey],
		"an exact preexisting linked legacy WhatsApp owner is backfilled")
	assert.NotContains(t, durableNeutral.Metadata, databasepkg.WhatsAppWAMIDOwnerMetadataKey,
		"a preexisting provider-neutral row cannot spoof the reserved owner marker")
	claimed, err := databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organization.ID, providerNeutral.WhatsAppMessageID)
	require.NoError(t, err)
	assert.False(t, claimed)
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ? AND id = ?", organization.ID, legacyConversation.ID).
		Update("channel", models.ChannelInstagram).Error)
	require.NoError(t, databasepkg.CreateIndexes(db), "an idempotent migration must preserve historical owner markers")
	require.NoError(t, db.Unscoped().First(&durableLegacy, "id = ?", legacyOwner.ID).Error)
	assert.Equal(t, true, durableLegacy.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey],
		"rerunning the migration after mutable conversation reclassification cannot demote an owner")

	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("organization_id = ? AND id = ?", organization.ID, providerNeutralChannel.ID).
		Updates(map[string]any{"channel": models.ChannelWhatsApp, "provider": "meta_legacy"}).Error)
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ? AND id = ?", organization.ID, providerNeutralConversation.ID).
		Update("channel", models.ChannelWhatsApp).Error)
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND id = ?", organization.ID, providerNeutral.ID).
		Update("status", models.MessageStatusRead).Error)
	require.NoError(t, db.Unscoped().First(&durableNeutral, "id = ?", providerNeutral.ID).Error)
	assert.NotContains(t, durableNeutral.Metadata, databasepkg.WhatsAppWAMIDOwnerMetadataKey,
		"indirect mutable channel metadata cannot retroactively promote an existing provider identifier")
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organization.ID, providerNeutral.WhatsAppMessageID)
	require.NoError(t, err)
	assert.False(t, claimed)
	require.NoError(t, databasepkg.CreateIndexes(db),
		"an idempotent future-profile migration cannot reclassify rows from mutable channel metadata")
	require.NoError(t, db.Unscoped().First(&durableNeutral, "id = ?", providerNeutral.ID).Error)
	assert.NotContains(t, durableNeutral.Metadata, databasepkg.WhatsAppWAMIDOwnerMetadataKey,
		"a future-profile rerun must skip first-install marker classification")
	claimed, err = databasepkg.WhatsAppIdentityReviewWAMIDClaimed(db, organization.ID, providerNeutral.WhatsAppMessageID)
	require.NoError(t, err)
	assert.False(t, claimed)

	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ? AND id = ?", organization.ID, legacyConversation.ID).
		Update("channel", models.ChannelWhatsApp).Error)
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND id = ?", organization.ID, providerNeutral.ID).
		Update("inbox_conversation_id", legacyConversation.ID).Error)
	require.NoError(t, db.Unscoped().First(&durableNeutral, "id = ?", providerNeutral.ID).Error)
	assert.Equal(t, true, durableNeutral.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey],
		"an explicit relink into the exact legacy WhatsApp boundary is an admission event")
}

func TestBaselineRLSMigrationPreservesLegacyProfileAndIsIdempotent(t *testing.T) {
	db, isolatedAdminDB, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.whatsapp_coexistence_states",
	).Error)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db))

	adminCfg := &config.DefaultAdminConfig{}
	backfillCalls := 0
	verificationCalls := 0
	backfill := func(session *gorm.DB) error {
		backfillCalls++
		var ownsInterlock bool
		require.NoError(t, session.Raw(`
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_locks
				WHERE pid = pg_catalog.pg_backend_pid()
				  AND locktype = 'advisory'
				  AND granted
			)
		`).Scan(&ownsInterlock).Error)
		assert.True(t, ownsInterlock, "baseline backfills must remain inside the pinned migration interlock")
		return nil
	}
	verifyRuntime := func() error {
		verificationCalls++
		return errors.Join(
			databasepkg.VerifyPlatformComplianceIdentityReviewCompatibilityForTest(db, runtimeRole),
			databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole),
		)
	}

	const retryIndexName = "idx_messages_org_inbox_ingested"
	const exactRetryIndex = `CREATE INDEX CONCURRENTLY idx_messages_org_inbox_ingested
		ON public.messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id)
		WHERE inbox_conversation_id IS NOT NULL AND deleted_at IS NULL`
	require.NoError(t, db.Exec("DROP INDEX CONCURRENTLY public."+retryIndexName).Error)
	require.NoError(t, db.Exec("CREATE TABLE public."+retryIndexName+" (id bigint)").Error)
	var collisionOID int64
	require.NoError(t, isolatedAdminDB.Raw(`
		SELECT relation.oid::bigint
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS relation_namespace
		  ON relation_namespace.oid = relation.relnamespace
		WHERE relation_namespace.nspname = 'public' AND relation.relname = ?
	`, retryIndexName).Scan(&collisionOID).Error)
	require.Positive(t, collisionOID)
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db, adminCfg, runtimeRole, "baseline", backfill, verifyRuntime,
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not an exact retry artifact")
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)
	var preservedCollisionOID int64
	require.NoError(t, isolatedAdminDB.Raw(`
		SELECT relation.oid::bigint
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS relation_namespace
		  ON relation_namespace.oid = relation.relnamespace
		WHERE relation_namespace.nspname = 'public' AND relation.relname = ?
	`, retryIndexName).Scan(&preservedCollisionOID).Error)
	require.Equal(t, collisionOID, preservedCollisionOID,
		"repairable admission must preserve a colliding non-index relation")
	require.NoError(t, db.Exec("DROP TABLE public."+retryIndexName).Error)
	invalidState := leaveInterruptedConcurrentIndexBuildTestState(
		t, db, isolatedAdminDB, retryIndexName, exactRetryIndex,
	)
	require.False(t, invalidState.Valid)

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		backfill,
		verifyRuntime,
	))
	assert.Equal(t, 1, backfillCalls)
	assert.Equal(t, 1, verificationCalls)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db),
		"baseline must repair the legacy contract without publishing future triggers")
	var tenantPolicyRestored bool
	require.NoError(t, db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_policies
			WHERE schemaname = 'public'
			  AND tablename = 'whatsapp_coexistence_states'
			  AND policyname = 'rereply_tenant_isolation'
		)
	`).Scan(&tenantPolicyRestored).Error)
	require.True(t, tenantPolicyRestored, "baseline must restore the exact missing tenant policy")
	rebuiltState, exists := readMigrationIndexLifecycleTestState(t, isolatedAdminDB, retryIndexName)
	require.True(t, exists)
	require.True(t, rebuiltState.Valid && rebuiltState.Ready && rebuiltState.Live)
	require.NotEqual(t, invalidState.OID, rebuiltState.OID,
		"repairable admission must drop and rebuild the exact invalid retry artifact")

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			return errors.New("idempotent baseline replay reached the mutation callback")
		},
		verifyRuntime,
	))
	assert.Equal(t, 1, backfillCalls, "the completed legacy baseline must not replay backfills")
	assert.Equal(t, 2, verificationCalls)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db))
}

func TestBaselineRLSMigrationAcceptsExactPreAdditiveLegacyPredecessor(t *testing.T) {
	db, isolatedAdminDB, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	dropPreAdditiveIdentityReviewRelations(t, db)
	readCoreConstraintOIDs := func() []int64 {
		t.Helper()
		var oids []int64
		require.NoError(t, db.Raw(`
			SELECT constraint_state.oid::bigint
			FROM pg_catalog.pg_constraint AS constraint_state
			JOIN pg_catalog.pg_class AS relation ON relation.oid = constraint_state.conrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND constraint_state.contype = 'c'
			  AND (
				(constraint_state.conname = 'chk_messages_whatsapp_message_id_trimmed' AND relation.relname = 'messages')
				OR (constraint_state.conname = 'chk_inbound_events_identity_review_shape' AND relation.relname = 'inbound_events')
			  )
			ORDER BY constraint_state.conname
		`).Scan(&oids).Error)
		require.Len(t, oids, 2, "the legitimate core constraints must remain installed")
		return oids
	}
	coreConstraintOIDs := readCoreConstraintOIDs()
	require.Error(t, databasepkg.VerifyPlatformComplianceIdentityReviewCompatibilityForTest(
		db,
		runtimeRole,
	), "the public/full verifier must remain strict when additive relations are absent")

	backfillCalls := 0
	verificationCalls := 0
	requireRuntimeSelect := func(table string, allowed bool) {
		t.Helper()
		tx := isolatedAdminDB.Begin()
		require.NoError(t, tx.Error)
		require.NoError(t, tx.Exec("SET LOCAL ROLE "+runtimeRole).Error)
		var rows int64
		err := tx.Raw("SELECT COUNT(*) FROM public." + table).Scan(&rows).Error
		if allowed {
			require.NoError(t, err)
		} else {
			_ = requireIdentityReviewSQLState(t, err, "42501")
		}
		require.NoError(t, tx.Rollback().Error)
	}
	verifyRuntime := func() error {
		verificationCalls++
		return errors.Join(
			databasepkg.VerifyPlatformComplianceIdentityReviewCompatibilityForTest(db, runtimeRole),
			databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole),
		)
	}
	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(session *gorm.DB) error {
			backfillCalls++
			for _, table := range []string{
				"whatsapp_coexistence_states",
				"whatsapp_identity_review_holds",
				"whatsapp_identity_review_members",
			} {
				requireRuntimeSelect(table, false)
				var rows int64
				require.NoError(t, session.Raw("SELECT COUNT(*) FROM public."+table).Scan(&rows).Error)
			}
			return nil
		},
		verifyRuntime,
	))
	assert.Equal(t, 1, backfillCalls)
	assert.Equal(t, 1, verificationCalls)
	require.Equal(t, coreConstraintOIDs, readCoreConstraintOIDs(),
		"pre-additive preparation must preserve the legitimate core constraint bindings")
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db))
	for _, table := range []string{
		"whatsapp_coexistence_states",
		"whatsapp_identity_review_holds",
		"whatsapp_identity_review_members",
	} {
		requireRuntimeSelect(table, true)
	}

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			return errors.New("completed predecessor replay reached the mutation callback")
		},
		verifyRuntime,
	))
	assert.Equal(t, 1, backfillCalls)
	assert.Equal(t, 2, verificationCalls)
}

func TestBaselineRLSMigrationPrepareClaimSurvivesFailureAndIsStrict(t *testing.T) {
	db, _, runtimeRole := setupPreAdditiveRLSMigrationTest(t)
	adminCfg := &config.DefaultAdminConfig{}
	forcedBackfillFailure := errors.New("forced baseline backfill failure")
	backfillCalls := 0
	verificationCalls := 0

	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return forcedBackfillFailure
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, forcedBackfillFailure)
	require.Equal(t, 1, backfillCalls)
	require.Zero(t, verificationCalls)

	var claim string
	require.NoError(t, db.Raw(
		"SELECT public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Scan(&claim).Error)
	require.True(t, strings.HasPrefix(claim, "prepare:v1:"))
	require.Len(t, strings.TrimPrefix(claim, "prepare:v1:"), sha256.Size*2)
	var runtimeExecute bool
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.has_function_privilege(
			CAST(? AS text),
			'public.rereply_tenant_policy_additive_fingerprint_v1()'::pg_catalog.regprocedure,
			'EXECUTE'
		)
	`, runtimeRole).Scan(&runtimeExecute).Error)
	require.False(t, runtimeExecute, "the transient claim must remain owner-only")
	var contactIndexConstraintCount int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_constraint
		WHERE conindid = 'public.uq_contacts_id_org'::pg_catalog.regclass
	`).Scan(&contactIndexConstraintCount).Error)
	require.Greater(t, contactIndexConstraintCount, int64(0),
		"the exact published unique index must retain its inbound foreign-key references during retry")

	for _, phase := range []string{"bridge"} {
		err = databasepkg.RunRLSMigrationCoordinatorForTest(
			db,
			adminCfg,
			runtimeRole,
			phase,
			func(*gorm.DB) error {
				backfillCalls++
				return nil
			},
			func() error {
				verificationCalls++
				return nil
			},
		)
		require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
		require.ErrorContains(t, err, "cross-phase")
	}
	require.Equal(t, 1, backfillCalls)
	require.Zero(t, verificationCalls)

	require.NoError(t, db.Exec(
		"GRANT EXECUTE ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() TO "+runtimeRole,
	).Error)
	err = databasepkg.RunRLSMigrationCoordinatorForTest(
		db, adminCfg, runtimeRole, "baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not exact")
	require.Equal(t, 1, backfillCalls)
	require.Zero(t, verificationCalls)
	require.NoError(t, db.Exec(
		"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM "+runtimeRole,
	).Error)

	const wrongClaim = "prepare:v1:0000000000000000000000000000000000000000000000000000000000000000"
	require.NoError(t, db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()
		RETURNS text LANGUAGE sql IMMUTABLE
		SET search_path = pg_catalog, public
		AS $function$ SELECT '%s'::text $function$
	`, wrongClaim)).Error)
	err = databasepkg.RunRLSMigrationCoordinatorForTest(
		db, adminCfg, runtimeRole, "baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not exact")
	require.Equal(t, 1, backfillCalls)
	require.Zero(t, verificationCalls)

	require.NoError(t, db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()
		RETURNS text LANGUAGE sql IMMUTABLE
		SET search_path = pg_catalog, public
		AS $function$ SELECT '%s'::text $function$
	`, claim)).Error)
	require.NoError(t, db.Exec(
		"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM PUBLIC",
	).Error)
	require.NoError(t, db.Exec(
		"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM "+runtimeRole,
	).Error)
	// A crash can leave only an ordered prefix of the additive model loop.
	// Materialize that catalog-equivalent state and prove the exact claim can
	// resume it without weakening markerless-partial quarantine.
	require.NoError(t, db.Exec(
		"DROP TABLE public.whatsapp_identity_review_members CASCADE",
	).Error)
	var identityReviewMembersExists bool
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.to_regclass('public.whatsapp_identity_review_members') IS NOT NULL
	`).Scan(&identityReviewMembersExists).Error)
	require.False(t, identityReviewMembersExists)

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return errors.Join(
				databasepkg.VerifyPlatformComplianceIdentityReviewCompatibilityForTest(db, runtimeRole),
				databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole),
			)
		},
	))
	require.Equal(t, 2, backfillCalls)
	require.Equal(t, 1, verificationCalls)
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.to_regclass('public.whatsapp_identity_review_members') IS NOT NULL
	`).Scan(&identityReviewMembersExists).Error)
	require.True(t, identityReviewMembersExists, "claimed ordered additive prefix must be completed on retry")
	var finalFingerprint string
	require.NoError(t, db.Raw(
		"SELECT public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Scan(&finalFingerprint).Error)
	require.True(t, strings.HasPrefix(finalFingerprint, "v1:"))
	require.False(t, strings.HasPrefix(finalFingerprint, "prepare:"))
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.has_function_privilege(
			CAST(? AS text),
			'public.rereply_tenant_policy_additive_fingerprint_v1()'::pg_catalog.regprocedure,
			'EXECUTE'
		)
	`, runtimeRole).Scan(&runtimeExecute).Error)
	require.True(t, runtimeExecute, "final publication must replace the claim and authorize runtime verification")
}

func TestBaselineRLSMigrationReconcilesOnlyExactInterruptedConcurrentIndexes(t *testing.T) {
	db, isolatedAdminDB, runtimeRole := setupPreAdditiveRLSMigrationTest(t)
	adminCfg := &config.DefaultAdminConfig{}
	forcedBackfillFailure := errors.New("force durable prepare claim before concurrent-index retry")
	require.ErrorIs(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { return forcedBackfillFailure },
		func() error { return nil },
	), forcedBackfillFailure)

	const indexName = "idx_messages_org_inbox_ingested"
	const exactCreate = `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_ingested
		ON messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id)
		WHERE inbox_conversation_id IS NOT NULL AND deleted_at IS NULL`
	require.NoError(t, db.Exec("DROP INDEX CONCURRENTLY public."+indexName).Error)

	// A table, view, sequence, or other pg_class row with a pinned index name
	// must never be treated as absence by the pg_index-specific inspection.
	require.NoError(t, db.Exec("CREATE TABLE public."+indexName+" (id bigint)").Error)
	var collidingRelation struct {
		OID  int64  `gorm:"column:relation_oid"`
		Kind string `gorm:"column:relation_kind"`
	}
	require.NoError(t, isolatedAdminDB.Raw(`
		SELECT relation.oid::bigint AS relation_oid, relation.relkind::text AS relation_kind
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS relation_namespace
		  ON relation_namespace.oid = relation.relnamespace
		WHERE relation_namespace.nspname = 'public' AND relation.relname = ?
	`, indexName).Scan(&collidingRelation).Error)
	require.Equal(t, "r", collidingRelation.Kind)
	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not an exact retry artifact")
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)
	var preservedCollisionOID int64
	require.NoError(t, isolatedAdminDB.Raw(`
		SELECT relation.oid::bigint
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS relation_namespace
		  ON relation_namespace.oid = relation.relnamespace
		WHERE relation_namespace.nspname = 'public' AND relation.relname = ?
	`, indexName).Scan(&preservedCollisionOID).Error)
	require.Equal(t, collidingRelation.OID, preservedCollisionOID)
	require.NoError(t, db.Exec("DROP TABLE public."+indexName).Error)

	// Leave a real invalid catalog row with the right pinned name/table but a
	// wrong definition. The retry classifier must preserve and quarantine it,
	// never normalize arbitrary DDL merely because its name is familiar.
	wrongState := leaveInterruptedConcurrentIndexBuildTestState(
		t,
		db,
		isolatedAdminDB,
		indexName,
		"CREATE INDEX CONCURRENTLY "+indexName+" ON public.messages(id)",
	)
	require.Contains(t, wrongState.Definition, "USING btree (id)")

	err = databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not an exact retry artifact")
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)
	preservedWrongState, exists := readMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName)
	require.True(t, exists)
	require.Equal(t, wrongState.OID, preservedWrongState.OID)
	require.Equal(t, wrongState.Definition, preservedWrongState.Definition)
	require.False(t, preservedWrongState.Valid)

	require.NoError(t, db.Exec("DROP INDEX CONCURRENTLY public."+indexName).Error)
	require.NoError(t, db.Exec(exactCreate).Error)
	publishedState, exists := readMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName)
	require.True(t, exists)
	require.True(t, publishedState.Valid && publishedState.Ready && publishedState.Live)
	highwaterState, exists := readMigrationIndexLifecycleTestState(
		t, isolatedAdminDB, "idx_messages_org_inbox_ingested_highwater",
	)
	require.True(t, exists)
	require.True(t, highwaterState.Valid && highwaterState.Ready && highwaterState.Live)
	require.Contains(t, highwaterState.Definition, "ingested_at DESC",
		"valid claimed retries must accept the pinned descending high-water index")

	// Materialize PostgreSQL's real 000 state: the first blocker holds the DROP
	// after invalidation; a second locker, admitted after that wait set is taken,
	// holds the next phase after PostgreSQL marks the index dead.
	firstBlocker := isolatedAdminDB.Begin()
	require.NoError(t, firstBlocker.Error)
	defer firstBlocker.Rollback()
	var messageCount int64
	require.NoError(t, firstBlocker.Raw("SELECT COUNT(*) FROM public.messages").Scan(&messageCount).Error)
	dropContext, cancelDrop := context.WithCancel(context.Background())
	defer cancelDrop()
	dropSQLDB, err := db.DB()
	require.NoError(t, err)
	dropConn, err := dropSQLDB.Conn(context.Background())
	require.NoError(t, err)
	defer dropConn.Close()
	var dropBackendPID int
	require.NoError(t, dropConn.QueryRowContext(
		context.Background(), "SELECT pg_catalog.pg_backend_pid()",
	).Scan(&dropBackendPID))
	dropResult := make(chan error, 1)
	go func() {
		_, dropErr := dropConn.ExecContext(
			dropContext, "DROP INDEX CONCURRENTLY public."+indexName,
		)
		dropResult <- dropErr
	}()
	waitForMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName,
		func(state migrationIndexLifecycleTestState, exists bool) bool {
			return exists && !state.Valid && state.Live
		})
	testutil.RequirePostgresBackendWaitingForLock(t, isolatedAdminDB, dropBackendPID)
	secondBlocker := isolatedAdminDB.Begin()
	require.NoError(t, secondBlocker.Error)
	defer secondBlocker.Rollback()
	require.NoError(t, secondBlocker.Raw("SELECT COUNT(*) FROM public.messages").Scan(&messageCount).Error)
	require.NoError(t, firstBlocker.Rollback().Error)
	deadState := waitForMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName,
		func(state migrationIndexLifecycleTestState, exists bool) bool {
			return exists && !state.Valid && !state.Ready && !state.Live
		})
	var dropCanceled bool
	require.NoError(t, isolatedAdminDB.Raw(
		"SELECT pg_catalog.pg_cancel_backend(?)", dropBackendPID,
	).Scan(&dropCanceled).Error)
	require.True(t, dropCanceled, "PostgreSQL must accept cancellation of the blocked concurrent drop")
	select {
	case dropErr := <-dropResult:
		require.Error(t, dropErr)
	case <-time.After(10 * time.Second):
		t.Fatal("canceled dead-index concurrent drop did not return")
	}
	// Do not release the second blocker until the server confirms cancellation;
	// otherwise DROP can win the race and remove the retained 000 artifact.
	require.NoError(t, secondBlocker.Rollback().Error)
	deadState, exists = readMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName)
	require.True(t, exists)
	require.False(t, deadState.Valid)
	require.False(t, deadState.Ready)
	require.False(t, deadState.Live)

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error {
			verificationCalls++
			return errors.Join(
				databasepkg.VerifyPlatformComplianceIdentityReviewCompatibilityForTest(db, runtimeRole),
				databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole),
			)
		},
	))
	require.Equal(t, 1, backfillCalls)
	require.Equal(t, 1, verificationCalls)
	rebuiltState, exists := readMigrationIndexLifecycleTestState(t, isolatedAdminDB, indexName)
	require.True(t, exists)
	require.True(t, rebuiltState.Valid && rebuiltState.Ready && rebuiltState.Live)
	require.NotEqual(t, deadState.OID, rebuiltState.OID)
	require.NotEqual(t, wrongState.OID, rebuiltState.OID)
}

func TestBaselineRLSMigrationPinsPublicBeforePreAdditivePreparation(t *testing.T) {
	db, _, ownerRole, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	dropPreAdditiveIdentityReviewRelations(t, db)
	require.NoError(t, db.Exec("CREATE SCHEMA "+ownerRole+" AUTHORIZATION "+ownerRole).Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	require.NoError(t, db.Exec(`SET search_path = "$user", public`).Error)
	var initialSchema string
	require.NoError(t, db.Raw("SELECT pg_catalog.current_schema()").Scan(&initialSchema).Error)
	require.Equal(t, ownerRole, initialSchema)

	backfillCalls := 0
	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(session *gorm.DB) error {
			backfillCalls++
			var currentSchema string
			require.NoError(t, session.Raw("SELECT pg_catalog.current_schema()").Scan(&currentSchema).Error)
			require.Equal(t, "public", currentSchema)
			return nil
		},
		func() error { return nil },
	))
	require.Equal(t, 1, backfillCalls)

	var decoyRelations int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = ?
	`, ownerRole).Scan(&decoyRelations).Error)
	require.Zero(t, decoyRelations, "migration must not create relations in the owner-named schema")
	for _, table := range []string{
		"whatsapp_identity_review_members",
		"whatsapp_identity_review_holds",
		"whatsapp_coexistence_states",
	} {
		var inPublic bool
		require.NoError(t, db.Raw(
			"SELECT pg_catalog.to_regclass(?) IS NOT NULL",
			"public."+table,
		).Scan(&inPublic).Error)
		require.True(t, inPublic, "%s must be created in public", table)
	}
}

func TestBaselineRLSMigrationReadinessFailsBeforePrepareClaim(t *testing.T) {
	db, isolatedAdminDB, runtimeRole := setupPreAdditiveRLSMigrationTest(t)
	blocker := isolatedAdminDB.Begin()
	require.NoError(t, blocker.Error)
	defer blocker.Rollback()
	require.NoError(t, blocker.Exec(
		"LOCK TABLE public.contacts IN ROW EXCLUSIVE MODE",
	).Error)

	backfillCalls := 0
	verificationCalls := 0
	started := time.Now()
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.Error(t, err)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)

	var claimCount int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_proc AS function
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = function.pronamespace
		WHERE namespace.nspname = 'public'
		  AND function.proname = 'rereply_tenant_policy_additive_fingerprint_v1'
	`).Scan(&claimCount).Error)
	require.Zero(t, claimCount, "NOWAIT readiness must fail before claim creation")
	for _, table := range []string{
		"whatsapp_coexistence_states",
		"whatsapp_identity_review_holds",
		"whatsapp_identity_review_members",
	} {
		var exists bool
		require.NoError(t, db.Raw(
			"SELECT pg_catalog.to_regclass(CAST(? AS text)) IS NOT NULL",
			"public."+table,
		).Scan(&exists).Error)
		require.False(t, exists, "%s must remain absent when readiness fails", table)
	}
}

func TestBaselineRLSMigrationRejectsDisabledForeignKeyEnforcementBeforeCallbacks(t *testing.T) {
	db, isolatedAdminDB, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.whatsapp_coexistence_states",
	).Error)
	require.NoError(t, isolatedAdminDB.Exec(`
		DO $block$
		DECLARE enforcement_trigger name;
		BEGIN
			SELECT trigger.tgname
			  INTO STRICT enforcement_trigger
			  FROM pg_catalog.pg_trigger AS trigger
			  JOIN pg_catalog.pg_constraint AS constraint_state
			    ON constraint_state.oid = trigger.tgconstraint
			  JOIN pg_catalog.pg_proc AS function ON function.oid = trigger.tgfoid
			 WHERE constraint_state.conname = 'fk_inbound_events_account_tenant'
			   AND constraint_state.conrelid = 'public.inbound_events'::pg_catalog.regclass
			   AND function.proname = 'RI_FKey_check_ins';
			EXECUTE pg_catalog.format(
				'ALTER TABLE public.inbound_events DISABLE TRIGGER %I',
				enforcement_trigger
			);
		END
		$block$;
	`).Error)

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "fk_inbound_events_account_tenant")
	require.ErrorContains(t, err, "non-canonical enforcement trigger")
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
}

func TestBaselineRLSMigrationRejectsSettableDefaultPrivilegeBeforeTableCreation(t *testing.T) {
	db, isolatedAdminDB, ownerRole, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	dropPreAdditiveIdentityReviewRelations(t, db)

	groupRole := "rereply_scaffold_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, isolatedAdminDB.Exec(
		"CREATE ROLE "+groupRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
	).Error)
	t.Cleanup(func() {
		if err := db.Exec(
			"ALTER DEFAULT PRIVILEGES FOR ROLE " + ownerRole + " REVOKE SELECT ON TABLES FROM " + groupRole,
		).Error; err != nil {
			t.Errorf("revoke synthetic default table privilege: %v", err)
		}
		if err := isolatedAdminDB.Exec("REVOKE " + groupRole + " FROM " + runtimeRole).Error; err != nil {
			t.Errorf("revoke synthetic runtime membership: %v", err)
		}
		if err := isolatedAdminDB.Exec("DROP ROLE " + groupRole).Error; err != nil {
			t.Errorf("drop synthetic scaffold-reader role: %v", err)
		}
	})
	require.NoError(t, isolatedAdminDB.Exec("GRANT "+groupRole+" TO "+runtimeRole).Error)
	require.NoError(t, db.Exec(
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+ownerRole+" GRANT SELECT ON TABLES TO "+groupRole,
	).Error)

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "effective default table privilege")
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
	for _, table := range []string{
		"whatsapp_identity_review_members",
		"whatsapp_identity_review_holds",
		"whatsapp_coexistence_states",
	} {
		var absent bool
		require.NoError(t, db.Raw(
			"SELECT pg_catalog.to_regclass(?) IS NULL",
			"public."+table,
		).Scan(&absent).Error)
		require.True(t, absent, "default-privilege drift must stop before creating %s", table)
	}
}

func TestBaselineRLSMigrationQuarantinesMalformedCatalogBeforeCallbacks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, string)
	}{
		{
			name: "missing core tenant policy",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"DROP POLICY rereply_tenant_isolation ON public.contacts",
				).Error)
			},
		},
		{
			name: "noncanonical extant additive tenant policy",
			mutate: func(t *testing.T, db *gorm.DB, runtimeRole string) {
				require.NoError(t, db.Exec(
					"DROP POLICY rereply_tenant_isolation ON public.whatsapp_identity_review_holds",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE POLICY rereply_tenant_isolation ON public.whatsapp_identity_review_holds TO "+
						runtimeRole+" USING (true) WITH CHECK (true)",
				).Error)
			},
		},
		{
			name: "missing related-table migration policy",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"DROP POLICY rereply_migration_access ON public.chatbot_flow_steps",
				).Error)
			},
		},
		{
			name: "related table no longer forces RLS",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"ALTER TABLE public.chatbot_flow_steps NO FORCE ROW LEVEL SECURITY",
				).Error)
			},
		},
		{
			name: "protected relation is unlogged",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				t.Cleanup(func() {
					_ = db.Exec("ALTER TABLE public.chatbot_flow_steps SET LOGGED").Error
				})
				require.NoError(t, db.Exec(
					"ALTER TABLE public.chatbot_flow_steps SET UNLOGGED",
				).Error)
			},
		},
		{
			name: "unexpected extra core policy",
			mutate: func(t *testing.T, db *gorm.DB, runtimeRole string) {
				t.Cleanup(func() {
					_ = db.Exec("DROP POLICY IF EXISTS unexpected_update_policy ON public.contacts").Error
				})
				require.NoError(t, db.Exec(
					"CREATE POLICY unexpected_update_policy ON public.contacts AS RESTRICTIVE FOR UPDATE TO "+
						runtimeRole+" USING (true) WITH CHECK (true)",
				).Error)
			},
		},
		{
			name: "core fingerprint wrapper",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				var fingerprint string
				require.NoError(t, db.Raw(
					"SELECT public.rereply_tenant_policy_fingerprint()",
				).Scan(&fingerprint).Error)
				require.NoError(t, db.Exec(fmt.Sprintf(`
					CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_fingerprint()
					RETURNS text LANGUAGE sql IMMUTABLE
					SET search_path = pg_catalog, public
					AS $function$ SELECT '%s'::text || ''::text $function$
				`, fingerprint)).Error)
			},
		},
		{
			name: "coordinated related policy and core fingerprint drift",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER POLICY rereply_tenant_isolation ON public.chatbot_flow_steps
					USING (flow_id IS NOT NULL) WITH CHECK (flow_id IS NOT NULL)
				`).Error)
				rewriteCoreTenantPolicyFingerprintForCurrentPolicies(t, db)
			},
		},
		{
			name: "near-equivalent tenant setting literal and matching fingerprint",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER POLICY rereply_tenant_isolation ON public.contacts
					USING (organization_id = NULLIF(current_setting('app. current_organization_id', true), '')::uuid)
					WITH CHECK (organization_id = NULLIF(current_setting('app. current_organization_id', true), '')::uuid)
				`).Error)
				rewriteCoreTenantPolicyFingerprintForCurrentPolicies(t, db)
			},
		},
		{
			name: "public function binding and matching fingerprint",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				t.Cleanup(func() {
					_ = db.Exec("DROP FUNCTION IF EXISTS public.current_setting(text, boolean) CASCADE").Error
				})
				require.NoError(t, db.Exec(`
					CREATE FUNCTION public.current_setting(text, boolean)
					RETURNS text LANGUAGE sql IMMUTABLE
					AS $function$ SELECT ''::text $function$
				`).Error)
				require.NoError(t, db.Exec(`
					SET search_path = public, pg_catalog
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER POLICY rereply_tenant_isolation ON public.contacts
					USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
					WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
				`).Error)
				rewriteCoreTenantPolicyFingerprintForCurrentPolicies(t, db)
			},
		},
		{
			name: "same-name weakened check constraint",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_coexistence_states
					DROP CONSTRAINT chk_whatsapp_coexistence_version
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_coexistence_states
					ADD CONSTRAINT chk_whatsapp_coexistence_version CHECK (true)
				`).Error)
			},
		},
		{
			name: "check constraint literal mimics catalog qualifier",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				var definition string
				require.NoError(t, db.Raw(`
					SELECT pg_catalog.pg_get_expr(constraint_state.conbin, constraint_state.conrelid, false)
					FROM pg_catalog.pg_constraint AS constraint_state
					JOIN pg_catalog.pg_class AS relation ON relation.oid = constraint_state.conrelid
					JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
					WHERE namespace.nspname = 'public'
					  AND relation.relname = 'inbound_events'
					  AND constraint_state.conname = 'chk_inbound_events_identity_review_shape'
				`).Scan(&definition).Error)
				const originalLiteral = "'review_pending'::text"
				const changedLiteral = "'pg_catalog.review_pending'::text"
				require.GreaterOrEqual(t, strings.Count(definition, originalLiteral), 2)
				weakened := strings.Replace(definition, originalLiteral, changedLiteral, 1)
				require.NotEqual(t, definition, weakened)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					DROP CONSTRAINT chk_inbound_events_identity_review_shape
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					ADD CONSTRAINT chk_inbound_events_identity_review_shape CHECK (`+weakened+`)
				`).Error)
			},
		},
		{
			name: "not-valid identity-review check constraint",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					DROP CONSTRAINT chk_inbound_events_identity_review_shape
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					ADD CONSTRAINT chk_inbound_events_identity_review_shape CHECK (true) NOT VALID
				`).Error)
			},
		},
		{
			name: "missing inbound account tenant foreign key",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					DROP CONSTRAINT fk_inbound_events_account_tenant
				`).Error)
			},
		},
		{
			name: "inbound account tenant foreign key rebound to contacts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					DROP CONSTRAINT fk_inbound_events_account_tenant
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					ADD CONSTRAINT fk_inbound_events_account_tenant
					FOREIGN KEY (channel_account_id, organization_id)
					REFERENCES public.contacts(id, organization_id)
					ON DELETE RESTRICT
				`).Error)
			},
		},
		{
			name: "unexpected cascading inbound account tenant foreign key",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					ADD CONSTRAINT unexpected_inbound_events_account_tenant_cascade
					FOREIGN KEY (channel_account_id, organization_id)
					REFERENCES public.channel_accounts(id, organization_id)
					ON UPDATE CASCADE ON DELETE CASCADE
				`).Error)
			},
		},
		{
			name: "identity-review foreign key rebound to contacts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					DROP CONSTRAINT fk_inbound_events_identity_review_hold_tenant
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.inbound_events
					ADD CONSTRAINT fk_inbound_events_identity_review_hold_tenant
					FOREIGN KEY (review_hold_id, organization_id)
					REFERENCES public.contacts(id, organization_id)
					ON DELETE RESTRICT
				`).Error)
			},
		},
		{
			name: "identity-review member foreign key cascades",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_members
					DROP CONSTRAINT fk_whatsapp_identity_review_member_contact_tenant
				`).Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_members
					ADD CONSTRAINT fk_whatsapp_identity_review_member_contact_tenant
					FOREIGN KEY (contact_id, organization_id)
					REFERENCES public.contacts(id, organization_id)
					ON DELETE CASCADE
				`).Error)
			},
		},
		{
			name: "missing identity-review index",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"DROP INDEX public.idx_inbound_events_review_hold_id",
				).Error)
			},
		},
		{
			name: "weakened identity-review partial index",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"DROP INDEX public.uq_inbound_events_identity_review_wamid",
				).Error)
				require.NoError(t, db.Exec(`
					CREATE UNIQUE INDEX uq_inbound_events_identity_review_wamid
					ON public.inbound_events(organization_id, provider_event_id)
					WHERE protocol = 'whatsapp_identity_review_v1'
				`).Error)
			},
		},
		{
			name: "additive column loses not-null contract",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_holds
					ALTER COLUMN semantic_claim_digest DROP NOT NULL
				`).Error)
			},
		},
		{
			name: "additive column default drifts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_holds
					ALTER COLUMN supported SET DEFAULT true
				`).Error)
			},
		},
		{
			name: "additive column type drifts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_members
					ALTER COLUMN selector_reasons TYPE bigint
				`).Error)
			},
		},
		{
			name: "additive column uses public shadow domain",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec("CREATE DOMAIN public.jsonb AS pg_catalog.jsonb").Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_coexistence_states
					ALTER COLUMN lifecycle_metadata TYPE public.jsonb
					USING lifecycle_metadata::pg_catalog.jsonb
				`).Error)
			},
		},
		{
			name: "additive default binds public shadow function",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					CREATE FUNCTION public.gen_random_uuid()
					RETURNS uuid LANGUAGE sql VOLATILE
					AS $function$ SELECT pg_catalog.gen_random_uuid() $function$
				`).Error)
				require.NoError(t, db.Exec("SET search_path = public, pg_catalog").Error)
				require.NoError(t, db.Exec(`
					ALTER TABLE public.whatsapp_identity_review_holds
					ALTER COLUMN id SET DEFAULT gen_random_uuid()
				`).Error)
			},
		},
		{
			name: "platform contract function volatility drifts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER FUNCTION public.rereply_platform_compliance_guard_contract() STABLE
				`).Error)
			},
		},
		{
			name: "platform contract function search path drifts",
			mutate: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(`
					ALTER FUNCTION public.rereply_platform_compliance_guard_contract()
					SET search_path = public, pg_catalog
				`).Error)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, runtimeRole := setupIsolatedRLSMigrationTest(t)
			dropIdentityReviewOldCoreTriggers(t, db)
			require.NoError(t, db.Exec(
				"DROP POLICY rereply_tenant_isolation ON public.whatsapp_coexistence_states",
			).Error)
			test.mutate(t, db, runtimeRole)

			backfillCalls := 0
			verificationCalls := 0
			err := databasepkg.RunRLSMigrationCoordinatorForTest(
				db,
				&config.DefaultAdminConfig{},
				runtimeRole,
				"baseline",
				func(*gorm.DB) error {
					backfillCalls++
					return nil
				},
				func() error {
					verificationCalls++
					return nil
				},
			)
			require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
			assert.Zero(t, backfillCalls)
			assert.Zero(t, verificationCalls)
			var missingPolicy bool
			require.NoError(t, db.Raw(`
				SELECT NOT EXISTS (
					SELECT 1 FROM pg_catalog.pg_policies
					WHERE schemaname = 'public'
					  AND tablename = 'whatsapp_coexistence_states'
					  AND policyname = 'rereply_tenant_isolation'
				)
			`).Scan(&missingPolicy).Error)
			assert.True(t, missingPolicy, "quarantine must occur before policy repair")
		})
	}
}

func TestFutureRLSMigrationRejectsSchemaDriftBeforeCallbacks(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	require.NoError(t, db.Exec(`
		ALTER TABLE public.whatsapp_identity_review_holds
		ALTER COLUMN supported SET DEFAULT true
	`).Error)

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"ui",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema contract")
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
	var defaultExpression string
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.pg_get_expr(default_value.adbin, default_value.adrelid, false)
		FROM pg_catalog.pg_attrdef AS default_value
		JOIN pg_catalog.pg_class AS relation ON relation.oid = default_value.adrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		JOIN pg_catalog.pg_attribute AS attribute
		  ON attribute.attrelid = relation.oid AND attribute.attnum = default_value.adnum
		WHERE namespace.nspname = 'public'
		  AND relation.relname = 'whatsapp_identity_review_holds'
		  AND attribute.attname = 'supported'
	`).Scan(&defaultExpression).Error)
	assert.Equal(t, "true", defaultExpression, "future verification must not repair catalog drift")
}

func TestBaselineRLSMigrationRejectsWrongTableConstraintBeforeCallbacks(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	dropPreAdditiveIdentityReviewRelations(t, db)

	requirePreAdditiveAbsent := func(t *testing.T) {
		t.Helper()
		var relations int64
		require.NoError(t, db.Raw(`
			SELECT COUNT(*)
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname IN (
				'whatsapp_identity_review_members',
				'whatsapp_identity_review_holds',
				'whatsapp_coexistence_states'
			  )
		`).Scan(&relations).Error)
		require.Zero(t, relations, "quarantine must not prepare additive relations")
		var functions int64
		require.NoError(t, db.Raw(`
			SELECT COUNT(*)
			FROM pg_catalog.pg_proc AS function
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = function.pronamespace
			WHERE namespace.nspname = 'public'
			  AND function.proname = 'rereply_tenant_policy_additive_fingerprint_v1'
		`).Scan(&functions).Error)
		require.Zero(t, functions, "quarantine must not publish the additive fingerprint")
	}
	const reservedName = "chk_whatsapp_coexistence_onboarding_status"
	type constraintIdentity struct {
		OID          int64  `gorm:"column:oid"`
		NamespaceOID int64  `gorm:"column:namespace_oid"`
		RelationOID  int64  `gorm:"column:relation_oid"`
		DomainOID    int64  `gorm:"column:domain_oid"`
		Type         string `gorm:"column:constraint_type"`
		Definition   string `gorm:"column:definition"`
	}
	readConstraint := func(t *testing.T) constraintIdentity {
		t.Helper()
		var state constraintIdentity
		result := db.Raw(`
			SELECT oid::bigint AS oid, connamespace::bigint AS namespace_oid,
				conrelid::bigint AS relation_oid, contypid::bigint AS domain_oid,
				contype::text AS constraint_type,
				pg_catalog.pg_get_constraintdef(oid, false) AS definition
			FROM pg_catalog.pg_constraint
			WHERE conname = ?
		`, reservedName).Scan(&state)
		require.NoError(t, result.Error)
		require.EqualValues(t, 1, result.RowsAffected)
		require.Positive(t, state.OID)
		require.Equal(t, "c", state.Type)
		require.NotEmpty(t, state.Definition)
		return state
	}
	requireNoCollision := func(t *testing.T) {
		t.Helper()
		var count int64
		require.NoError(t, db.Raw(
			"SELECT COUNT(*) FROM pg_catalog.pg_constraint WHERE conname = ?", reservedName,
		).Scan(&count).Error)
		require.Zero(t, count, "each scenario must start and finish without a reserved-name collision")
	}

	for _, scenario := range []struct {
		name      string
		create    []string
		cleanup   []string
		relation  string
		domain    string
		namespace string
	}{
		{
			name: "wrong_public_table",
			create: []string{
				"ALTER TABLE public.organizations ADD CONSTRAINT " + reservedName + " CHECK (true)",
			},
			cleanup: []string{
				"ALTER TABLE public.organizations DROP CONSTRAINT IF EXISTS " + reservedName,
			},
			relation: "public.organizations", namespace: "public",
		},
		{
			name: "other_schema_table",
			create: []string{
				"CREATE SCHEMA rereply_constraint_collision",
				"CREATE TABLE rereply_constraint_collision.probe (id integer CONSTRAINT " + reservedName + " CHECK (true))",
			},
			cleanup: []string{
				"DROP TABLE IF EXISTS rereply_constraint_collision.probe",
				"DROP SCHEMA IF EXISTS rereply_constraint_collision",
			},
			relation: "rereply_constraint_collision.probe", namespace: "rereply_constraint_collision",
		},
		{
			name: "domain_constraint",
			create: []string{
				"CREATE SCHEMA rereply_constraint_collision",
				"CREATE DOMAIN rereply_constraint_collision.probe AS integer CONSTRAINT " + reservedName + " CHECK (VALUE >= 0)",
			},
			cleanup: []string{
				"DROP DOMAIN IF EXISTS rereply_constraint_collision.probe",
				"DROP SCHEMA IF EXISTS rereply_constraint_collision",
			},
			domain: "rereply_constraint_collision.probe", namespace: "rereply_constraint_collision",
		},
	} {
		if !t.Run(scenario.name, func(t *testing.T) {
			requirePreAdditiveAbsent(t)
			requireNoCollision(t)
			t.Cleanup(func() {
				for _, statement := range scenario.cleanup {
					if err := db.Exec(statement).Error; err != nil {
						t.Errorf("remove synthetic collision object: %v", err)
					}
				}
				requireNoCollision(t)
			})
			for _, statement := range scenario.create {
				require.NoError(t, db.Exec(statement).Error)
			}
			before := readConstraint(t)
			var expectedNamespace int64
			require.NoError(t, db.Raw(
				"SELECT oid::bigint FROM pg_catalog.pg_namespace WHERE nspname = ?", scenario.namespace,
			).Scan(&expectedNamespace).Error)
			require.Equal(t, expectedNamespace, before.NamespaceOID)
			if scenario.relation != "" {
				var expectedRelation int64
				require.NoError(t, db.Raw(
					"SELECT CAST(? AS regclass)::oid::bigint", scenario.relation,
				).Scan(&expectedRelation).Error)
				require.Equal(t, expectedRelation, before.RelationOID)
				require.Zero(t, before.DomainOID)
			} else {
				var expectedDomain int64
				require.NoError(t, db.Raw(
					"SELECT CAST(? AS regtype)::oid::bigint", scenario.domain,
				).Scan(&expectedDomain).Error)
				require.Equal(t, expectedDomain, before.DomainOID)
				require.Zero(t, before.RelationOID)
			}

			backfillCalls := 0
			verificationCalls := 0
			err := databasepkg.RunRLSMigrationCoordinatorForTest(
				db, &config.DefaultAdminConfig{}, runtimeRole, "baseline",
				func(*gorm.DB) error { backfillCalls++; return nil },
				func() error { verificationCalls++; return nil },
			)
			require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
			require.Zero(t, backfillCalls)
			require.Zero(t, verificationCalls)
			requirePreAdditiveAbsent(t)
			require.Equal(t, before, readConstraint(t),
				"quarantine must preserve the exact suspicious constraint and its binding")
		}) {
			return
		}
	}
}

func TestRLSMigrationFingerprintVerificationNeverExecutesStoredBody(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.whatsapp_coexistence_states",
	).Error)
	require.NoError(t, db.Exec("CREATE SEQUENCE public.rereply_fingerprint_execution_probe").Error)
	require.NoError(t, db.Exec(`
		CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_fingerprint()
		RETURNS text LANGUAGE plpgsql VOLATILE
		SET search_path = pg_catalog, public
		AS $function$
		BEGIN
			PERFORM pg_catalog.nextval('public.rereply_fingerprint_execution_probe'::pg_catalog.regclass);
			RETURN 'forged';
		END
		$function$
	`).Error)

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
	var sequenceCalled bool
	require.NoError(t, db.Raw(`
		SELECT is_called
		FROM public.rereply_fingerprint_execution_probe
	`).Scan(&sequenceCalled).Error)
	assert.False(t, sequenceCalled, "catalog verification must never invoke a stored fingerprint function")
}

func TestRLSMigrationCallbackScanAuthorityRejectsNoncanonicalAdditivePolicy(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.whatsapp_identity_review_holds",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE POLICY rereply_tenant_isolation ON public.whatsapp_identity_review_holds AS RESTRICTIVE TO "+
			runtimeRole+" USING (false) WITH CHECK (false)",
	).Error)

	err := databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenant policy")
}

func TestRLSMigrationCallbackScanAuthorityRejectsRawDistinctCanonicalEquivalentPolicy(t *testing.T) {
	const reference = "organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid"
	const candidate = "(organization_id = NULLIF(current_setting('app.current_organization_id'::text, true), ''::text)::uuid)"

	require.NoError(t, databasepkg.VerifyDirectTenantPolicyRepresentationForTest(reference, reference))
	err := databasepkg.VerifyDirectTenantPolicyRepresentationForTest(reference, candidate)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical direct-tenant representation")
}

func TestRLSMigrationCallbackScanAuthorityRejectsNoncanonicalRelatedPolicy(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.chatbot_flow_steps",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE POLICY rereply_tenant_isolation ON public.chatbot_flow_steps AS RESTRICTIVE TO "+
			runtimeRole+" USING (false) WITH CHECK (false)",
	).Error)

	err := databasepkg.VerifyRLSMigrationCallbackScanAuthorityForTest(db, runtimeRole)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenant policy")
}

func TestVerifyTenantRLSRejectsCoordinatedPolicyAndFingerprintDrift(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	require.NoError(t, databasepkg.CreateIndexes(db))
	require.NoError(t, db.Exec(`
		ALTER POLICY rereply_tenant_isolation ON public.chatbot_flow_steps
		USING (flow_id IS NOT NULL) WITH CHECK (flow_id IS NOT NULL)
	`).Error)
	rewriteCoreTenantPolicyFingerprintForCurrentPolicies(t, db)

	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	err := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical tenant policy inventory")
}

func TestApplyTenantRLSRevokesPublicSchemaCreateFromPublic(t *testing.T) {
	type schemaGrant struct {
		Grantor       int64  `gorm:"column:grantor"`
		Grantee       int64  `gorm:"column:grantee"`
		PrivilegeType string `gorm:"column:privilege_type"`
		IsGrantable   bool   `gorm:"column:is_grantable"`
	}
	type roleIdentity struct {
		CurrentRole string `gorm:"column:current_role"`
		SessionRole string `gorm:"column:session_role"`
		OID         int64  `gorm:"column:role_oid"`
		Superuser   bool   `gorm:"column:superuser"`
		CreateRole  bool   `gorm:"column:create_role"`
		BypassRLS   bool   `gorm:"column:bypass_rls"`
		Replication bool   `gorm:"column:replication"`
	}
	type schemaFixture struct {
		DB          *gorm.DB
		AdminDB     *gorm.DB
		OwnerRole   string
		RuntimeRole string
		OwnerOID    int64
		AdminOID    int64
		RuntimeOID  int64
	}
	readIdentity := func(t *testing.T, db *gorm.DB) roleIdentity {
		t.Helper()
		var identity roleIdentity
		require.NoError(t, db.Raw(`
			SELECT current_user::text AS current_role,
			       session_user::text AS session_role,
			       role.oid::bigint AS role_oid,
			       role.rolsuper AS superuser,
			       role.rolcreaterole AS create_role,
			       role.rolbypassrls AS bypass_rls,
			       role.rolreplication AS replication
			FROM pg_catalog.pg_roles AS role
			WHERE role.rolname = current_user
		`).Scan(&identity).Error)
		require.Positive(t, identity.OID)
		require.Equal(t, identity.CurrentRole, identity.SessionRole)
		return identity
	}
	requireSchemaOwner := func(t *testing.T, db *gorm.DB, ownerOID int64) {
		t.Helper()
		var state struct {
			Count    int64 `gorm:"column:namespace_count"`
			OwnerOID int64 `gorm:"column:owner_oid"`
		}
		require.NoError(t, db.Raw(`
			SELECT COUNT(*) AS namespace_count,
			       COALESCE(min(nspowner::bigint), 0) AS owner_oid
			FROM pg_catalog.pg_namespace
			WHERE nspname = 'public'
		`).Scan(&state).Error)
		require.EqualValues(t, 1, state.Count)
		require.Equal(t, ownerOID, state.OwnerOID)
	}
	readACL := func(t *testing.T, db *gorm.DB) []schemaGrant {
		t.Helper()
		grants := make([]schemaGrant, 0)
		require.NoError(t, db.Raw(`
			SELECT privilege.grantor::bigint AS grantor,
			       privilege.grantee::bigint AS grantee,
			       privilege.privilege_type,
			       privilege.is_grantable
			FROM pg_catalog.pg_namespace AS namespace,
			LATERAL pg_catalog.aclexplode(
				COALESCE(namespace.nspacl, pg_catalog.acldefault('n', namespace.nspowner))
			) AS privilege
			WHERE namespace.nspname = 'public'
			ORDER BY privilege.grantor::bigint, privilege.grantee::bigint,
			         privilege.privilege_type, privilege.is_grantable
		`).Scan(&grants).Error)
		return grants
	}
	createGrantsFor := func(grants []schemaGrant, grantee int64) []schemaGrant {
		selected := make([]schemaGrant, 0)
		for _, grant := range grants {
			if grant.Grantee == grantee && grant.PrivilegeType == "CREATE" {
				selected = append(selected, grant)
			}
		}
		return selected
	}
	requirePrivilege := func(t *testing.T, db *gorm.DB, role, privilege string, expected bool) {
		t.Helper()
		var granted bool
		require.NoError(t, db.Raw(
			"SELECT pg_catalog.has_schema_privilege(?::name, 'public', ?)", role, privilege,
		).Scan(&granted).Error)
		require.Equal(t, expected, granted)
	}
	newFixture := func(t *testing.T) schemaFixture {
		t.Helper()
		db, adminDB, ownerRole, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
		owner := readIdentity(t, db)
		require.Equal(t, ownerRole, owner.CurrentRole)
		require.False(t, owner.Superuser)
		require.False(t, owner.CreateRole)
		require.False(t, owner.BypassRLS)
		require.False(t, owner.Replication)
		admin := readIdentity(t, adminDB)
		require.NotEqual(t, owner.OID, admin.OID)
		var runtimeOID int64
		require.NoError(t, db.Raw(
			"SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname = ?", runtimeRole,
		).Scan(&runtimeOID).Error)
		require.Positive(t, runtimeOID)
		require.NotEqual(t, owner.OID, runtimeOID)
		requireSchemaOwner(t, db, owner.OID)
		return schemaFixture{db, adminDB, ownerRole, runtimeRole, owner.OID, admin.OID, runtimeOID}
	}
	prepareForeignOwner := func(t *testing.T, fixture schemaFixture, grantOption bool) {
		t.Helper()
		require.NoError(t, fixture.AdminDB.Exec("ALTER SCHEMA public OWNER TO CURRENT_USER").Error)
		requireSchemaOwner(t, fixture.DB, fixture.AdminOID)
		require.NoError(t, fixture.AdminDB.Exec("REVOKE CREATE ON SCHEMA public FROM PUBLIC").Error)
		require.NoError(t, fixture.AdminDB.Exec("REVOKE CREATE ON SCHEMA public FROM "+fixture.OwnerRole).Error)
		require.NoError(t, fixture.AdminDB.Exec("REVOKE CREATE ON SCHEMA public FROM "+fixture.RuntimeRole).Error)
		require.NoError(t, fixture.AdminDB.Exec("GRANT USAGE ON SCHEMA public TO "+fixture.OwnerRole).Error)
		grant := "GRANT CREATE ON SCHEMA public TO " + fixture.OwnerRole
		if grantOption {
			grant += " WITH GRANT OPTION"
		}
		require.NoError(t, fixture.AdminDB.Exec(grant).Error)
		require.NoError(t, fixture.AdminDB.Exec("GRANT CREATE ON SCHEMA public TO PUBLIC").Error)
		requirePrivilege(t, fixture.DB, fixture.OwnerRole, "CREATE", true)
		requirePrivilege(t, fixture.DB, fixture.OwnerRole, "CREATE WITH GRANT OPTION", grantOption)
	}
	const postconditionError = "apply tenant RLS: public schema CREATE revocation postcondition failed"

	t.Run("schema_owner_revokes_seeded_public_and_runtime_grants", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.DB.Exec("REVOKE CREATE ON SCHEMA public FROM PUBLIC").Error)
		require.NoError(t, fixture.DB.Exec("REVOKE CREATE ON SCHEMA public FROM "+fixture.RuntimeRole).Error)
		require.NoError(t, fixture.DB.Exec("GRANT CREATE ON SCHEMA public TO PUBLIC").Error)
		require.NoError(t, fixture.DB.Exec("GRANT CREATE ON SCHEMA public TO "+fixture.RuntimeRole).Error)
		before := readACL(t, fixture.DB)
		require.Equal(t, []schemaGrant{{fixture.OwnerOID, 0, "CREATE", false}}, createGrantsFor(before, 0))
		require.Equal(t, []schemaGrant{{fixture.OwnerOID, fixture.RuntimeOID, "CREATE", false}}, createGrantsFor(before, fixture.RuntimeOID))
		requirePrivilege(t, fixture.DB, fixture.RuntimeRole, "CREATE", true)

		require.NoError(t, databasepkg.ApplyTenantRLS(fixture.DB, fixture.RuntimeRole))

		after := readACL(t, fixture.DB)
		require.Empty(t, createGrantsFor(after, 0), "PUBLIC must not retain CREATE on schema public")
		require.Empty(t, createGrantsFor(after, fixture.RuntimeOID))
		requirePrivilege(t, fixture.DB, fixture.RuntimeRole, "CREATE", false)
		requireSchemaOwner(t, fixture.DB, fixture.OwnerOID)
	})

	t.Run("foreign_owner_without_grant_option_is_rejected_without_acl_change", func(t *testing.T) {
		fixture := newFixture(t)
		prepareForeignOwner(t, fixture, false)
		require.NoError(t, fixture.AdminDB.Exec("GRANT CREATE ON SCHEMA public TO "+fixture.RuntimeRole).Error)
		before := readACL(t, fixture.DB)
		require.Equal(t, []schemaGrant{{fixture.AdminOID, 0, "CREATE", false}}, createGrantsFor(before, 0))
		require.Equal(t, []schemaGrant{{fixture.AdminOID, fixture.RuntimeOID, "CREATE", false}}, createGrantsFor(before, fixture.RuntimeOID))

		err := databasepkg.ApplyTenantRLS(fixture.DB, fixture.RuntimeRole)

		require.EqualError(t, err, postconditionError)
		require.Equal(t, before, readACL(t, fixture.DB))
		requireSchemaOwner(t, fixture.DB, fixture.AdminOID)
	})

	t.Run("foreign_public_grant_rejection_rolls_back_owner_issued_runtime_revoke", func(t *testing.T) {
		fixture := newFixture(t)
		prepareForeignOwner(t, fixture, true)
		require.NoError(t, fixture.DB.Exec("GRANT CREATE ON SCHEMA public TO "+fixture.RuntimeRole).Error)
		before := readACL(t, fixture.DB)
		require.Equal(t, []schemaGrant{{fixture.AdminOID, 0, "CREATE", false}}, createGrantsFor(before, 0))
		ownerIssuedRuntimeGrant := []schemaGrant{{fixture.OwnerOID, fixture.RuntimeOID, "CREATE", false}}
		require.Equal(t, ownerIssuedRuntimeGrant, createGrantsFor(before, fixture.RuntimeOID))

		err := databasepkg.ApplyTenantRLS(fixture.DB, fixture.RuntimeRole)

		require.EqualError(t, err, postconditionError)
		after := readACL(t, fixture.DB)
		require.Equal(t, before, after)
		// Effective CREATE would remain true through PUBLIC even if the direct
		// grant were lost; the exact grantor-bound ACL row proves restoration.
		require.Equal(t, ownerIssuedRuntimeGrant, createGrantsFor(after, fixture.RuntimeOID))
		requireSchemaOwner(t, fixture.DB, fixture.AdminOID)
	})
}

func TestSetTenantContextCannotBindPublicShadowFunction(t *testing.T) {
	db, _ := setupIsolatedRLSMigrationTest(t)
	require.NoError(t, db.Exec("CREATE SEQUENCE public.rereply_set_config_execution_probe").Error)
	require.NoError(t, db.Exec(`
		CREATE FUNCTION public.set_config(text, text, boolean)
		RETURNS text LANGUAGE plpgsql VOLATILE
		AS $function$
		BEGIN
			PERFORM pg_catalog.nextval('public.rereply_set_config_execution_probe'::pg_catalog.regclass);
			RETURN 'forged';
		END
		$function$
	`).Error)
	require.NoError(t, db.Exec("SET search_path = public, pg_catalog").Error)

	organizationID := uuid.New()
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	require.NoError(t, databasepkg.SetTenantContext(tx, organizationID))
	var tenant string
	require.NoError(t, tx.Raw(`
		SELECT pg_catalog.current_setting('app.current_organization_id', true)
	`).Scan(&tenant).Error)
	assert.Equal(t, organizationID.String(), tenant)
	var sequenceCalled bool
	require.NoError(t, tx.Raw(`
		SELECT is_called
		FROM public.rereply_set_config_execution_probe
	`).Scan(&sequenceCalled).Error)
	assert.False(t, sequenceCalled, "tenant binding must call pg_catalog.set_config directly")
}

func TestVerifyTenantRLSRejectsSettablePublicSchemaCreator(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	creatorRole := "rereply_public_creator_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	require.NoError(t, db.Exec(
		"CREATE ROLE "+creatorRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
	).Error)
	t.Cleanup(func() {
		_ = db.Exec("REVOKE " + creatorRole + " FROM " + runtimeRole).Error
		_ = db.Exec("REVOKE CREATE ON SCHEMA public FROM " + creatorRole).Error
		_ = db.Exec("DROP ROLE IF EXISTS " + creatorRole).Error
	})
	require.NoError(t, db.Exec("GRANT CREATE ON SCHEMA public TO "+creatorRole).Error)
	require.NoError(t, db.Exec("GRANT "+creatorRole+" TO "+runtimeRole).Error)

	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	err := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SET ROLE to a public-schema creator")
}

func TestTenantRLSRejectsCreateRoleRuntimeAuthority(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	require.NoError(t, db.Exec("ALTER ROLE "+runtimeRole+" CREATEROLE").Error)

	applyErr := databasepkg.ApplyTenantRLS(db, runtimeRole)
	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "NOCREATEROLE")

	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	verifyErr := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.Error(t, verifyErr)
	assert.Contains(t, verifyErr.Error(), "NOCREATEROLE")
}

func TestVerifyTenantRLSAcceptsExactSeparatedRuntimeAuthority(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	require.NoError(t, databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole))
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
}

func TestVerifyTenantRLSRejectsPublicBtrimRoutingIndexShadow(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	restored := false
	t.Cleanup(func() {
		_ = runtimeDB.Exec("RESET search_path").Error
		if restored {
			return
		}
		_ = db.Exec("DROP INDEX IF EXISTS public.uq_whatsapp_accounts_live_phone_id").Error
		_ = db.Exec("DROP FUNCTION IF EXISTS public.btrim(text)").Error
		_ = db.Exec(`
			CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_accounts_live_phone_id
			ON public.whatsapp_accounts(pg_catalog.btrim(phone_id))
			WHERE deleted_at IS NULL
		`).Error
	})

	require.NoError(t, db.Exec("DROP INDEX public.uq_whatsapp_accounts_live_phone_id").Error)
	require.NoError(t, db.Exec(`
		CREATE FUNCTION public.btrim(text)
		RETURNS text
		LANGUAGE sql
		IMMUTABLE STRICT PARALLEL SAFE
		AS $function$ SELECT pg_catalog.btrim($1) $function$
	`).Error)
	require.NoError(t, db.Exec(`
		CREATE UNIQUE INDEX uq_whatsapp_accounts_live_phone_id
		ON public.whatsapp_accounts(public.btrim(phone_id))
		WHERE deleted_at IS NULL
	`).Error)
	require.NoError(t, runtimeDB.Exec("SET search_path = public, pg_catalog").Error)
	err := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.Error(t, err)
	require.ErrorContains(t, err, "Phone ID routing index")

	require.NoError(t, runtimeDB.Exec("RESET search_path").Error)
	require.NoError(t, db.Exec("DROP INDEX public.uq_whatsapp_accounts_live_phone_id").Error)
	require.NoError(t, db.Exec("DROP FUNCTION public.btrim(text)").Error)
	require.NoError(t, db.Exec(`
		CREATE UNIQUE INDEX uq_whatsapp_accounts_live_phone_id
		ON public.whatsapp_accounts(pg_catalog.btrim(phone_id))
		WHERE deleted_at IS NULL
	`).Error)
	restored = true
	require.NoError(t, databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole))
}

func TestTenantRLSRejectsSessionReplicationRoleParameterAuthority(t *testing.T) {
	db := testutil.SetupTestDB(t)
	var major int
	require.NoError(t, db.Raw(
		"SELECT pg_catalog.current_setting('server_version_num')::integer / 10000",
	).Scan(&major).Error)
	if major != 17 {
		t.Skip("PostgreSQL 14 predates parameter ACLs")
	}

	for _, privilege := range []string{"SET", "ALTER SYSTEM"} {
		t.Run(strings.ToLower(strings.ReplaceAll(privilege, " ", "_")), func(t *testing.T) {
			ownerDB, runtimeRole := setupPlatformComplianceGuardTest(t)
			require.NoError(t, ownerDB.Exec(
				"GRANT "+privilege+" ON PARAMETER session_replication_role TO "+runtimeRole,
			).Error)
			t.Cleanup(func() {
				if err := ownerDB.Exec(
					"REVOKE " + privilege + " ON PARAMETER session_replication_role FROM " + runtimeRole,
				).Error; err != nil {
					t.Errorf("revoke synthetic parameter privilege: %v", err)
				}
			})

			applyErr := databasepkg.ApplyTenantRLS(ownerDB, runtimeRole)
			require.ErrorContains(t, applyErr, "session_replication_role")
			runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
			verifyErr := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
			require.ErrorContains(t, verifyErr, "session_replication_role")

			backfillCalls := 0
			verificationCalls := 0
			coordinatorErr := databasepkg.RunRLSMigrationCoordinatorForTest(
				ownerDB,
				&config.DefaultAdminConfig{},
				runtimeRole,
				"baseline",
				func(*gorm.DB) error { backfillCalls++; return nil },
				func() error { verificationCalls++; return nil },
			)
			require.Error(t, coordinatorErr)
			require.ErrorContains(t, coordinatorErr, "session_replication_role")
			require.Zero(t, backfillCalls)
			require.Zero(t, verificationCalls)
		})
	}
}

func TestTenantRLSRejectsPersistedReplicaSessionDefault(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	require.NoError(t, db.Exec(
		"ALTER ROLE "+runtimeRole+" SET session_replication_role = replica",
	).Error)
	t.Cleanup(func() {
		if err := db.Exec("ALTER ROLE " + runtimeRole + " RESET session_replication_role").Error; err != nil {
			t.Errorf("reset synthetic session replication default: %v", err)
		}
	})

	applyErr := databasepkg.ApplyTenantRLS(db, runtimeRole)
	require.ErrorContains(t, applyErr, "session_replication_role")
	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	verifyErr := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.ErrorContains(t, verifyErr, "session_replication_role")
}

func TestApplyTenantRLSRejectsReplicaMigrationSession(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	require.NoError(t, tx.Exec("SET LOCAL session_replication_role = replica").Error)

	err := databasepkg.ApplyTenantRLS(tx, runtimeRole)
	require.ErrorContains(t, err, "migration session_replication_role")
	require.NoError(t, tx.Rollback().Error)
}

func futureRLSAuthorityCatalogSnapshot(t *testing.T, db *gorm.DB, runtimeRole string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.md5(pg_catalog.string_agg(item, E'\n' ORDER BY item))
		FROM (
			SELECT 'relation:' || namespace.nspname || '.' || relation.relname || ':' ||
				relation.relowner::text || ':' || relation.relrowsecurity::text || ':' ||
				relation.relforcerowsecurity::text || ':' || COALESCE(relation.relacl::text, '<null>') AS item
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND relation.relkind IN ('r', 'p')
			UNION ALL
			SELECT 'default:' || defaults.defaclrole::text || ':' || defaults.defaclnamespace::text || ':' ||
				defaults.defaclobjtype::text || ':' || COALESCE(defaults.defaclacl::text, '<null>')
			FROM pg_catalog.pg_default_acl AS defaults
			WHERE defaults.defaclnamespace IN (0::oid, 'public'::pg_catalog.regnamespace)
			UNION ALL
			SELECT 'membership:' || membership.roleid::text || ':' || membership.member::text || ':' ||
				membership.grantor::text || ':' || membership.admin_option::text
			FROM pg_catalog.pg_auth_members AS membership
			JOIN pg_catalog.pg_roles AS granted_role ON granted_role.oid = membership.roleid
			JOIN pg_catalog.pg_roles AS member_role ON member_role.oid = membership.member
			WHERE granted_role.rolname = CAST(? AS text) OR member_role.rolname = CAST(? AS text)
			UNION ALL
			SELECT 'trigger:' || pg_catalog.pg_get_triggerdef(trigger.oid, true)
			FROM pg_catalog.pg_trigger AS trigger
			JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND NOT trigger.tgisinternal
			UNION ALL
			SELECT 'policy:' || policy.schemaname || '.' || policy.tablename || ':' ||
				policy.policyname || ':' || COALESCE(policy.qual, '') || ':' || COALESCE(policy.with_check, '')
			FROM pg_catalog.pg_policies AS policy
			WHERE policy.schemaname = 'public'
			UNION ALL
			SELECT 'function:' || procedure.oid::pg_catalog.regprocedure::text || ':' ||
				pg_catalog.pg_get_functiondef(procedure.oid)
			FROM pg_catalog.pg_proc AS procedure
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
			WHERE namespace.nspname = 'public' AND procedure.proname LIKE 'rereply_%'
		) AS catalog
	`, runtimeRole, runtimeRole).Scan(&snapshot).Error)
	require.NotEmpty(t, snapshot)
	return snapshot
}

func requireFutureRLSCoordinatorQuarantined(
	t *testing.T,
	db *gorm.DB,
	runtimeRole string,
	want string,
) {
	t.Helper()
	before := futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole)
	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, want)
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)
	require.Equal(t, before, futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole),
		"future-profile quarantine must not rewrite catalog authority")
}

func TestTenantRLSRejectsDangerousRuntimeTableAuthority(t *testing.T) {
	t.Run("valid future profile is accepted without mutation", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
		before := futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole)
		backfillCalls := 0
		verificationCalls := 0
		require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
			db,
			&config.DefaultAdminConfig{},
			runtimeRole,
			"baseline",
			func(*gorm.DB) error {
				backfillCalls++
				return errors.New("valid future profile reached the mutation callback")
			},
			func() error {
				verificationCalls++
				return databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
			},
		))
		require.Zero(t, backfillCalls)
		require.Equal(t, 1, verificationCalls)
		require.Equal(t, before, futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole),
			"future-profile verification must be read-only")
	})

	t.Run("direct dangerous table grant", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		require.NoError(t, db.Exec("GRANT TRUNCATE ON TABLE public.contacts TO "+runtimeRole).Error)
		t.Cleanup(func() {
			if err := db.Exec("REVOKE TRUNCATE ON TABLE public.contacts FROM " + runtimeRole).Error; err != nil {
				t.Errorf("revoke synthetic truncate privilege: %v", err)
			}
		})

		before := futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole)
		applyErr := databasepkg.ApplyTenantRLS(db, runtimeRole)
		require.ErrorContains(t, applyErr, "dangerous table authority")
		runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
		verifyErr := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
		require.ErrorContains(t, verifyErr, "dangerous table authority")
		require.Equal(t, before, futureRLSAuthorityCatalogSnapshot(t, db, runtimeRole))
		requireFutureRLSCoordinatorQuarantined(t, db, runtimeRole, "dangerous table authority")
	})

	t.Run("settable dangerous table grant", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		groupRole := "rereply_table_truncate_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		require.NoError(t, db.Exec(
			"CREATE ROLE "+groupRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
		).Error)
		require.NoError(t, db.Exec("GRANT "+groupRole+" TO "+runtimeRole).Error)
		require.NoError(t, db.Exec("GRANT TRUNCATE ON TABLE public.contacts TO "+groupRole).Error)
		t.Cleanup(func() {
			if err := db.Exec("REVOKE TRUNCATE ON TABLE public.contacts FROM " + groupRole).Error; err != nil {
				t.Errorf("revoke synthetic settable truncate privilege: %v", err)
			}
			if err := db.Exec("REVOKE " + groupRole + " FROM " + runtimeRole).Error; err != nil {
				t.Errorf("revoke synthetic runtime membership: %v", err)
			}
			if err := db.Exec("DROP ROLE " + groupRole).Error; err != nil {
				t.Errorf("drop synthetic table-authority role: %v", err)
			}
		})

		requireFutureRLSCoordinatorQuarantined(t, db, runtimeRole, "dangerous table authority")
	})

	t.Run("direct dangerous default table grant", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		require.NoError(t, db.Exec(
			"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public GRANT TRUNCATE ON TABLES TO "+runtimeRole,
		).Error)
		t.Cleanup(func() {
			if err := db.Exec(
				"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public REVOKE TRUNCATE ON TABLES FROM " + runtimeRole,
			).Error; err != nil {
				t.Errorf("revoke synthetic direct default truncate privilege: %v", err)
			}
		})

		requireFutureRLSCoordinatorQuarantined(t, db, runtimeRole, "dangerous default table privilege")
	})

	t.Run("settable dangerous default table grant", func(t *testing.T) {
		db, runtimeRole := setupPlatformComplianceGuardTest(t)
		groupRole := "rereply_default_truncate_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		require.NoError(t, db.Exec(
			"CREATE ROLE "+groupRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
		).Error)
		require.NoError(t, db.Exec("GRANT "+groupRole+" TO "+runtimeRole).Error)
		require.NoError(t, db.Exec(
			"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public GRANT TRUNCATE ON TABLES TO "+groupRole,
		).Error)
		t.Cleanup(func() {
			if err := db.Exec(
				"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public REVOKE TRUNCATE ON TABLES FROM " + groupRole,
			).Error; err != nil {
				t.Errorf("revoke synthetic settable default truncate privilege: %v", err)
			}
			if err := db.Exec("REVOKE " + groupRole + " FROM " + runtimeRole).Error; err != nil {
				t.Errorf("revoke synthetic default privilege membership: %v", err)
			}
			if err := db.Exec("DROP ROLE " + groupRole).Error; err != nil {
				t.Errorf("drop synthetic default privilege role: %v", err)
			}
		})

		requireFutureRLSCoordinatorQuarantined(t, db, runtimeRole, "dangerous default table privilege")
	})
}

func TestTenantRLSRejectsDangerousRuntimeDefaultTableAuthority(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	groupRole := "rereply_default_truncate_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, db.Exec(
		"CREATE ROLE "+groupRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
	).Error)
	require.NoError(t, db.Exec("GRANT "+groupRole+" TO "+runtimeRole).Error)
	require.NoError(t, db.Exec(
		"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public GRANT TRUNCATE ON TABLES TO "+groupRole,
	).Error)
	t.Cleanup(func() {
		if err := db.Exec(
			"ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER IN SCHEMA public REVOKE TRUNCATE ON TABLES FROM " + groupRole,
		).Error; err != nil {
			t.Errorf("revoke synthetic default truncate privilege: %v", err)
		}
		if err := db.Exec("REVOKE " + groupRole + " FROM " + runtimeRole).Error; err != nil {
			t.Errorf("revoke synthetic default privilege membership: %v", err)
		}
		if err := db.Exec("DROP ROLE " + groupRole).Error; err != nil {
			t.Errorf("drop synthetic default privilege role: %v", err)
		}
	})

	applyErr := databasepkg.ApplyTenantRLS(db, runtimeRole)
	require.ErrorContains(t, applyErr, "dangerous default table privilege")
	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	verifyErr := databasepkg.VerifyTenantRLS(runtimeDB, runtimeRole)
	require.ErrorContains(t, verifyErr, "dangerous default table privilege")
}

func TestBaselineRLSMigrationQuarantinesPartialPreAdditiveCatalog(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	require.NoError(t, db.Exec(`
		ALTER TABLE public.inbound_events
		DROP CONSTRAINT fk_inbound_events_identity_review_hold_tenant
	`).Error)
	require.NoError(t, db.Exec(`
		DROP TABLE
			public.whatsapp_identity_review_members,
			public.whatsapp_identity_review_holds
		RESTRICT
	`).Error)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_tenant_isolation ON public.whatsapp_coexistence_states",
	).Error)
	require.NoError(t, db.Exec(
		"DROP POLICY rereply_migration_access ON public.whatsapp_coexistence_states",
	).Error)
	require.NoError(t, db.Exec(
		"ALTER TABLE public.whatsapp_coexistence_states DISABLE ROW LEVEL SECURITY",
	).Error)

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
	var membersAbsent bool
	require.NoError(t, db.Raw(
		"SELECT pg_catalog.to_regclass('public.whatsapp_identity_review_members') IS NULL",
	).Scan(&membersAbsent).Error)
	assert.True(t, membersAbsent, "quarantine must occur before schema preparation")
}

func TestBaselineRLSMigrationQuarantinesMarkerlessFullUnpublishedCatalog(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, db.Exec(
		"DROP FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()",
	).Error)
	for _, table := range []string{
		"whatsapp_coexistence_states",
		"whatsapp_identity_review_holds",
		"whatsapp_identity_review_members",
	} {
		require.NoError(t, db.Exec(
			"DROP POLICY rereply_tenant_isolation ON public."+table,
		).Error)
		require.NoError(t, db.Exec(
			"DROP POLICY rereply_migration_access ON public."+table,
		).Error)
		require.NoError(t, db.Exec(
			"ALTER TABLE public."+table+" DISABLE ROW LEVEL SECURITY",
		).Error)
	}

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error { backfillCalls++; return nil },
		func() error { verificationCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "partial additive catalog")
	require.Zero(t, backfillCalls)
	require.Zero(t, verificationCalls)
	for _, table := range []string{
		"whatsapp_coexistence_states",
		"whatsapp_identity_review_holds",
		"whatsapp_identity_review_members",
	} {
		var rowSecurity bool
		require.NoError(t, db.Raw(`
			SELECT relation.relrowsecurity
			FROM pg_catalog.pg_class AS relation
			WHERE relation.oid = pg_catalog.to_regclass(CAST(? AS text))
		`, "public."+table).Scan(&rowSecurity).Error)
		require.False(t, rowSecurity, "markerless quarantine must not publish policy state on %s", table)
	}
}

func TestBaselineRLSMigrationReadFailureCannotReachCallbacks(t *testing.T) {
	db, runtimeRole := setupIsolatedRLSMigrationTest(t)
	dropIdentityReviewOldCoreTriggers(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	backfillCalls := 0
	verificationCalls := 0
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db.WithContext(ctx),
		&config.DefaultAdminConfig{},
		runtimeRole,
		"baseline",
		func(*gorm.DB) error {
			backfillCalls++
			return nil
		},
		func() error {
			verificationCalls++
			return nil
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, backfillCalls)
	assert.Zero(t, verificationCalls)
}

func TestRLSMigrationCoordinatorRejectsFutureOnlyPhasesAndRecoversAfterLateBridgeFailure(t *testing.T) {
	db, isolatedAdminDB, _, runtimeRole := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, databasepkg.ApplyTenantRLS(db, runtimeRole))
	dropIdentityReviewOldCoreTriggers(t, db)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db))

	adminCfg := &config.DefaultAdminConfig{}
	mutationCalls := 0
	verifyCalls := 0
	for _, phase := range []string{"backend", "ui"} {
		err := databasepkg.RunRLSMigrationCoordinatorForTest(
			db,
			adminCfg,
			runtimeRole,
			phase,
			func(*gorm.DB) error {
				mutationCalls++
				return nil
			},
			func() error {
				verifyCalls++
				return nil
			},
		)
		require.ErrorContains(t, err, "require the exact future")
	}
	assert.Zero(t, mutationCalls, "future-only sources must reject legacy before any callback")
	assert.Zero(t, verifyCalls, "a rejected legacy profile is not runtime authority")

	const retryIndexName = "idx_messages_org_inbox_ingested"
	const exactRetryIndex = `CREATE INDEX CONCURRENTLY idx_messages_org_inbox_ingested
		ON public.messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id)
		WHERE inbox_conversation_id IS NOT NULL AND deleted_at IS NULL`
	require.NoError(t, db.Exec("DROP INDEX CONCURRENTLY public."+retryIndexName).Error)
	wrongState := leaveInterruptedConcurrentIndexBuildTestState(
		t,
		db,
		isolatedAdminDB,
		retryIndexName,
		"CREATE INDEX CONCURRENTLY "+retryIndexName+" ON public.messages(id)",
	)
	err := databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"bridge",
		func(*gorm.DB) error { mutationCalls++; return nil },
		func() error { verifyCalls++; return nil },
	)
	require.ErrorIs(t, err, databasepkg.ErrRLSMigrationCatalogQuarantined)
	require.ErrorContains(t, err, "not an exact retry artifact")
	require.Zero(t, mutationCalls)
	require.Zero(t, verifyCalls)
	preservedWrongState, exists := readMigrationIndexLifecycleTestState(
		t, isolatedAdminDB, retryIndexName,
	)
	require.True(t, exists)
	require.Equal(t, wrongState.OID, preservedWrongState.OID)
	require.Equal(t, wrongState.Definition, preservedWrongState.Definition)
	require.False(t, preservedWrongState.Valid)
	require.NoError(t, db.Exec("DROP INDEX CONCURRENTLY public."+retryIndexName).Error)
	invalidBridgeState := leaveInterruptedConcurrentIndexBuildTestState(
		t, db, isolatedAdminDB, retryIndexName, exactRetryIndex,
	)
	require.False(t, invalidBridgeState.Valid)

	forcedLateFailure := errors.New("forced post-schema bridge backfill failure")
	err = databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"bridge",
		func(session *gorm.DB) error {
			mutationCalls++
			var ownsInterlock bool
			require.NoError(t, session.Raw(`
				SELECT EXISTS (
					SELECT 1 FROM pg_catalog.pg_locks
					WHERE pid = pg_catalog.pg_backend_pid()
					  AND locktype = 'advisory'
					  AND granted
				)
			`).Scan(&ownsInterlock).Error)
			assert.True(t, ownsInterlock, "backfills must remain inside the pinned migration interlock")

			contenderErr := databasepkg.CreateIndexes(db)
			require.ErrorContains(t, contenderErr, "another database migrator already owns")
			return forcedLateFailure
		},
		func() error {
			verifyCalls++
			return nil
		},
	)
	require.ErrorIs(t, err, forcedLateFailure)
	assert.Equal(t, 1, mutationCalls)
	assert.Zero(t, verifyCalls)
	require.Empty(t, identityReviewOldCoreTriggerBindings(t, db),
		"a late pre-activation failure must preserve the exact legacy profile")
	rebuiltBridgeState, exists := readMigrationIndexLifecycleTestState(
		t, isolatedAdminDB, retryIndexName,
	)
	require.True(t, exists)
	require.True(t, rebuiltBridgeState.Valid && rebuiltBridgeState.Ready && rebuiltBridgeState.Live)
	require.NotEqual(t, invalidBridgeState.OID, rebuiltBridgeState.OID,
		"bridge-from-complete must repair the exact interrupted index before backfill")

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"bridge",
		func(session *gorm.DB) error {
			mutationCalls++
			var ownsInterlock bool
			require.NoError(t, session.Raw(`
				SELECT EXISTS (
					SELECT 1 FROM pg_catalog.pg_locks
					WHERE pid = pg_catalog.pg_backend_pid()
					  AND locktype = 'advisory'
					  AND granted
				)
			`).Scan(&ownsInterlock).Error)
			assert.True(t, ownsInterlock)
			return nil
		},
		func() error {
			verifyCalls++
			return databasepkg.VerifyPlatformComplianceIdentityReviewFutureForTest(db, runtimeRole)
		},
	))
	assert.Equal(t, 2, mutationCalls)
	assert.Equal(t, 1, verifyCalls)
	assert.Equal(t, expectedIdentityReviewOldCoreTriggerBindings(), identityReviewOldCoreTriggerBindings(t, db))

	require.NoError(t, databasepkg.RunRLSMigrationCoordinatorForTest(
		db,
		adminCfg,
		runtimeRole,
		"bridge",
		func(*gorm.DB) error {
			return errors.New("future-profile bridge replay reached the mutation callback")
		},
		func() error {
			verifyCalls++
			return databasepkg.VerifyPlatformComplianceIdentityReviewFutureForTest(db, runtimeRole)
		},
	))
	assert.Equal(t, 2, mutationCalls, "a completed bridge must not replay backfills")
	assert.Equal(t, 2, verifyCalls)
}

func TestWhatsAppIdentityReviewMigrationRejectsPartialOldCoreTriggerProfileBeforeBackfill(t *testing.T) {
	db, _, _, _ := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)

	dropIdentityReviewOldCoreTriggers(t, db)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.migration.partial-profile",
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	require.NoError(t, db.Exec(`
		CREATE TRIGGER rereply_identity_review_contact_selector_fence
		BEFORE INSERT OR UPDATE OR DELETE ON public.contacts
		FOR EACH ROW EXECUTE FUNCTION rereply_lock_whatsapp_identity_review_contact_selector()
	`).Error)

	err := databasepkg.CreateIndexes(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"identity-review old-core trigger activation requires an exact absent or complete profile")
	assert.Equal(t,
		[]string{"rereply_identity_review_contact_selector_fence@contacts"},
		identityReviewOldCoreTriggerBindings(t, db),
		"the rejected partial profile must remain unchanged instead of being silently completed")
	var durable models.Message
	require.NoError(t, db.Unscoped().First(&durable, "id = ?", message.ID).Error)
	assert.NotContains(t, durable.Metadata, databasepkg.WhatsAppWAMIDOwnerMetadataKey,
		"profile preflight must fail before the historical marker backfill")

	dropIdentityReviewOldCoreTriggers(t, db)
	require.NoError(t, databasepkg.CreateIndexes(db),
		"the exact absent profile must retry into the complete future profile")
	assert.Equal(t, expectedIdentityReviewOldCoreTriggerBindings(), identityReviewOldCoreTriggerBindings(t, db))
	require.NoError(t, db.Unscoped().First(&durable, "id = ?", message.ID).Error)
	assert.Equal(t, true, durable.Metadata[databasepkg.WhatsAppWAMIDOwnerMetadataKey])

	require.NoError(t, db.Exec(`ALTER TABLE public.messages
		DISABLE TRIGGER rereply_identity_review_message_wamid_owner`).Error)
	err = databasepkg.CreateIndexes(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"identity-review old-core trigger activation requires an exact absent or complete profile",
		"four correct names and relations are not complete when one authority trigger is disabled")
	require.NoError(t, db.Exec(`ALTER TABLE public.messages
		ENABLE TRIGGER rereply_identity_review_message_wamid_owner`).Error)
	require.NoError(t, databasepkg.CreateIndexes(db),
		"the exact enabled future profile must remain idempotent after rejected catalog drift")
}

func TestWhatsAppIdentityReviewCrossStoreWAMIDFenceSeesOppositeStoreAsRuntime(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	organization := createGuardTestOrganization(t, db, false)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hold := createIdentityReviewTestHold(t, db, organization.ID, account.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: contact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Channel: models.ChannelWhatsApp, Provider: "meta_legacy", Name: "runtime-cross-store",
		ExternalAccountID: "runtime-cross-store-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channel).Error)
	newEvent := func(wamid string) models.InboundEvent {
		return models.InboundEvent{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			ChannelAccountID: channel.ID, DedupeKey: "identity-review:" + wamid,
			ProviderEventID: wamid, EventType: models.WhatsAppIdentityReviewPendingEvent,
			Status: models.InboundEventStatusPending, SignatureValid: true, ReceivedAt: time.Now().UTC(),
			Protocol: models.WhatsAppIdentityReviewInboundProtocol, ReviewHoldID: &hold.ID,
			Headers: models.JSONB{}, Payload: models.JSONB{
				"schema_version": 1, "message_type": "text", "content": "runtime fence",
			},
		}
	}
	newMessage := func(wamid string) models.Message {
		return models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			WhatsAppAccount: account.Name, ContactID: contact.ID, WhatsAppMessageID: wamid,
			Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
			Status: models.MessageStatusReceived, Metadata: models.JSONB{},
		}
	}
	asRuntime := func(create func(*gorm.DB) error) error {
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
				return err
			}
			if err := databasepkg.SetTenantContext(tx, organization.ID); err != nil {
				return err
			}
			return create(tx)
		})
	}

	eventFirst := newEvent("wamid.runtime.event-first")
	require.NoError(t, db.Create(&eventFirst).Error)
	messageAfterEvent := newMessage(eventFirst.ProviderEventID)
	err := asRuntime(func(tx *gorm.DB) error { return tx.Create(&messageAfterEvent).Error })
	pgErr := requireIdentityReviewSQLState(t, err, "23505")
	assert.Equal(t, "uq_whatsapp_wamid_cross_store_owner", pgErr.ConstraintName)

	messageFirst := newMessage("wamid.runtime.message-first")
	require.NoError(t, db.Create(&messageFirst).Error)
	eventAfterMessage := newEvent(messageFirst.WhatsAppMessageID)
	err = asRuntime(func(tx *gorm.DB) error { return tx.Create(&eventAfterMessage).Error })
	pgErr = requireIdentityReviewSQLState(t, err, "23505")
	assert.Equal(t, "uq_whatsapp_wamid_cross_store_owner", pgErr.ConstraintName)

	require.NoError(t, db.Delete(&messageFirst).Error)
	softDeletedOwner := newEvent(messageFirst.WhatsAppMessageID)
	err = asRuntime(func(tx *gorm.DB) error { return tx.Create(&softDeletedOwner).Error })
	pgErr = requireIdentityReviewSQLState(t, err, "23505")
	assert.Equal(t, "uq_whatsapp_wamid_cross_store_owner", pgErr.ConstraintName)
}

func TestWhatsAppIdentityReviewWAMIDFenceFailsFastAndSerializesOwners(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hold := createIdentityReviewTestHold(t, db, organization.ID, account.ID, []models.WhatsAppIdentityReviewMember{{
		ContactID: contact.ID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
	}})
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Channel: models.ChannelWhatsApp, Provider: "meta_legacy", Name: "wamid-race",
		ExternalAccountID: "wamid-race-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channel).Error)
	newMessage := func(wamid string) models.Message {
		return models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			WhatsAppAccount: account.Name, ContactID: contact.ID, WhatsAppMessageID: wamid,
			Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
			Status: models.MessageStatusReceived, Metadata: models.JSONB{},
		}
	}
	newEvent := func(wamid string) models.InboundEvent {
		return models.InboundEvent{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			ChannelAccountID: channel.ID, DedupeKey: "identity-review:" + wamid,
			ProviderEventID: wamid, EventType: models.WhatsAppIdentityReviewPendingEvent,
			Status: models.InboundEventStatusPending, SignatureValid: true, ReceivedAt: time.Now().UTC(),
			Protocol: models.WhatsAppIdentityReviewInboundProtocol, ReviewHoldID: &hold.ID,
			Headers: models.JSONB{}, Payload: models.JSONB{
				"schema_version": 1, "message_type": "text", "content": "race",
			},
		}
	}

	t.Run("message transaction wins", func(t *testing.T) {
		const wamid = "wamid.race.message-first"
		winner := db.Begin()
		require.NoError(t, winner.Error)
		defer winner.Rollback()
		message := newMessage(wamid)
		require.NoError(t, winner.Create(&message).Error)

		started := time.Now()
		event := newEvent(wamid)
		err := db.Create(&event).Error
		_ = requireIdentityReviewSQLState(t, err, "55P03")
		assert.Less(t, time.Since(started), 2*time.Second, "the losing writer must fail promptly")
		require.NoError(t, winner.Commit().Error)

		retry := newEvent(wamid)
		_ = requireIdentityReviewSQLState(t, db.Create(&retry).Error, "23505")
		var messageCount, eventCount int64
		require.NoError(t, db.Unscoped().Model(&models.Message{}).
			Where("organization_id = ? AND BTRIM(whats_app_message_id) = ?", organization.ID, wamid).
			Count(&messageCount).Error)
		require.NoError(t, db.Unscoped().Model(&models.InboundEvent{}).
			Where("organization_id = ? AND protocol = ? AND BTRIM(provider_event_id) = ?",
				organization.ID, models.WhatsAppIdentityReviewInboundProtocol, wamid).
			Count(&eventCount).Error)
		assert.EqualValues(t, 1, messageCount)
		assert.Zero(t, eventCount)
	})

	t.Run("review transaction wins", func(t *testing.T) {
		const wamid = "wamid.race.review-first"
		winner := db.Begin()
		require.NoError(t, winner.Error)
		defer winner.Rollback()
		event := newEvent(wamid)
		require.NoError(t, winner.Create(&event).Error)

		started := time.Now()
		message := newMessage(wamid)
		err := db.Create(&message).Error
		_ = requireIdentityReviewSQLState(t, err, "55P03")
		assert.Less(t, time.Since(started), 2*time.Second, "the losing writer must fail promptly")
		require.NoError(t, winner.Commit().Error)

		retry := newMessage(wamid)
		_ = requireIdentityReviewSQLState(t, db.Create(&retry).Error, "23505")
		var messageCount, eventCount int64
		require.NoError(t, db.Unscoped().Model(&models.Message{}).
			Where("organization_id = ? AND BTRIM(whats_app_message_id) = ?", organization.ID, wamid).
			Count(&messageCount).Error)
		require.NoError(t, db.Unscoped().Model(&models.InboundEvent{}).
			Where("organization_id = ? AND protocol = ? AND BTRIM(provider_event_id) = ?",
				organization.ID, models.WhatsAppIdentityReviewInboundProtocol, wamid).
			Count(&eventCount).Error)
		assert.Zero(t, messageCount)
		assert.EqualValues(t, 1, eventCount)
	})

	t.Run("empty message identity is fenced before update", func(t *testing.T) {
		const wamid = "wamid.race.empty-update"
		message := newMessage("")
		require.NoError(t, db.Create(&message).Error)
		holder := db.Begin()
		require.NoError(t, holder.Error)
		defer holder.Rollback()
		require.NoError(t, databasepkg.LockWhatsAppWAMIDScopes(holder, organization.ID, wamid))

		started := time.Now()
		err := db.Model(&models.Message{}).
			Where("organization_id = ? AND id = ?", organization.ID, message.ID).
			Update("whats_app_message_id", wamid).Error
		_ = requireIdentityReviewSQLState(t, err, "55P03")
		assert.Less(t, time.Since(started), 2*time.Second, "the tuple-owning update must fail promptly")
		var unchanged models.Message
		require.NoError(t, db.Unscoped().Where("organization_id = ? AND id = ?", organization.ID, message.ID).
			First(&unchanged).Error)
		assert.Empty(t, unchanged.WhatsAppMessageID)

		require.NoError(t, holder.Rollback().Error)
		require.NoError(t, db.Model(&models.Message{}).
			Where("organization_id = ? AND id = ?", organization.ID, message.ID).
			Update("whats_app_message_id", wamid).Error)
	})
}

func TestWhatsAppIdentityReviewWAMIDBatchLocksUseCanonicalOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)

	first := db.Begin()
	require.NoError(t, first.Error)
	defer first.Rollback()
	require.NoError(t, databasepkg.LockWhatsAppWAMIDScopes(
		first,
		organization.ID,
		"wamid.batch.z",
		"wamid.batch.a",
	))

	second := db.Begin()
	require.NoError(t, second.Error)
	defer second.Rollback()
	result := make(chan error, 1)
	go func() {
		result <- databasepkg.LockWhatsAppWAMIDScopes(
			second,
			organization.ID,
			"wamid.batch.a",
			"wamid.batch.z",
		)
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
		t.Fatal("the second transaction unexpectedly acquired locks before the first released them")
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, first.Commit().Error)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("canonically ordered WAMID batch lock timed out")
	}
	require.NoError(t, second.Commit().Error)
}

func TestWhatsAppIdentityReviewWAMIDFenceBreaksRollingWriterLockInversion(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	const wamid = "wamid.rolling-lock-order"
	selectorKey := databasepkg.WhatsAppIdentityReviewContactSelectorFenceKey(organization.ID)

	// Model the predecessor writer: it already owns the organization selector
	// fence and reaches a Message write before it knows about the WAMID prelock.
	predecessor := db.Begin()
	require.NoError(t, predecessor.Error)
	defer predecessor.Rollback()
	require.NoError(t, predecessor.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))", selectorKey,
	).Error)

	// Model the current writer's canonical order: WAMID first, then the selector
	// fence. It waits on the predecessor rather than taking the locks backwards.
	current := db.Begin()
	require.NoError(t, current.Error)
	defer current.Rollback()
	require.NoError(t, databasepkg.LockWhatsAppWAMIDScopes(current, organization.ID, wamid))
	var currentPID int
	require.NoError(t, current.Raw("SELECT pg_backend_pid()").Scan(&currentPID).Error)
	selectorResult := make(chan error, 1)
	go func() {
		selectorResult <- current.Exec(
			"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))", selectorKey,
		).Error
	}()
	testutil.RequirePostgresBackendWaitingForLock(t, db, currentPID)

	message := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		WhatsAppAccount: account.Name, ContactID: contact.ID, WhatsAppMessageID: wamid,
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
		Status: models.MessageStatusReceived, Metadata: models.JSONB{},
	}
	started := time.Now()
	_ = requireIdentityReviewSQLState(t, predecessor.Create(&message).Error, "55P03")
	assert.Less(t, time.Since(started), 2*time.Second,
		"the trigger must fail the reverse-order predecessor promptly instead of forming a deadlock")
	require.NoError(t, predecessor.Rollback().Error)
	select {
	case err := <-selectorResult:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("current writer did not acquire the selector fence after predecessor rollback")
	}
	require.NoError(t, current.Commit().Error)

	var count int64
	require.NoError(t, db.Unscoped().Model(&models.Message{}).
		Where("organization_id = ? AND BTRIM(whats_app_message_id) = ?", organization.ID, wamid).
		Count(&count).Error)
	assert.Zero(t, count, "the failed predecessor write cannot leave a partial owner")
}

func TestWhatsAppIdentityReviewMigrationRejectsIncompleteHistoricalMembership(t *testing.T) {
	db, _, _, _ := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	require.NoError(t, db.Exec(
		"DROP TRIGGER trg_whatsapp_identity_review_holds_complete ON public.whatsapp_identity_review_holds",
	).Error)
	require.NoError(t, db.Exec(
		"DROP TRIGGER trg_whatsapp_identity_review_holds_guard ON public.whatsapp_identity_review_holds",
	).Error)

	corrupt := models.WhatsAppIdentityReviewHold{
		ID: uuid.New(), OrganizationID: organization.ID, WhatsAppAccountID: account.ID,
		OnboardingCycle: 1, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion,
		Supported: true, DirectPrimaryBSUID: "direct-corrupt", PrincipalGeneration: 1,
		SemanticClaimDigest:     identityReviewTestDigest("semantic-corrupt"),
		SelectorBodyDigest:      identityReviewTestDigest("selector-corrupt"),
		VerifiedEventDigest:     identityReviewTestDigest("event-corrupt"),
		VerifiedEventProvenance: "meta_signed_webhook",
		MemberCount:             1, MemberDigest: identityReviewTestDigest("missing-member"),
		Version: 1, Disposition: models.WhatsAppIdentityReviewDispositionOpen,
	}
	require.NoError(t, db.Create(&corrupt).Error)
	err := databasepkg.CreateIndexes(db)
	pgErr := requireIdentityReviewSQLState(t, err, "23514")
	assert.Contains(t, pgErr.Message,
		"cannot install WhatsApp identity-review completeness guards; repair incomplete or noncanonical membership first")

	require.NoError(t, db.Exec(
		"DELETE FROM public.whatsapp_identity_review_holds WHERE organization_id = ? AND id = ?",
		organization.ID, corrupt.ID,
	).Error)
	require.NoError(t, databasepkg.CreateIndexes(db))
	for _, triggerName := range []string{
		"trg_whatsapp_identity_review_holds_guard",
		"trg_whatsapp_identity_review_holds_complete",
	} {
		var installed bool
		require.NoError(t, db.Raw(`SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_trigger
			WHERE tgrelid = 'public.whatsapp_identity_review_holds'::regclass
			  AND tgname = ? AND NOT tgisinternal
		)`, triggerName).Scan(&installed).Error)
		assert.True(t, installed, "%s must be restored after the repaired retry", triggerName)
	}
}

func TestWhatsAppIdentityReviewInboundIndexBuildDoesNotWaitForLiveWriter(t *testing.T) {
	db, _, _, _ := testutil.OpenIsolatedTestDatabaseOwnedByRole(t)
	require.NoError(t, db.Exec("DROP INDEX IF EXISTS public.uq_inbound_events_identity_review_wamid").Error)

	organization := testutil.CreateTestOrganization(t, db)
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Channel: models.ChannelMessenger, Provider: "meta_graph", Name: "concurrent-index-writer",
		ExternalAccountID: "concurrent-index-writer-" + uuid.NewString(), Status: models.ChannelAccountStatusActive,
		Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channel).Error)

	// An old repeatable-read snapshot holds the concurrent build at its final
	// snapshot fence, giving the regression a deterministic window in which to
	// prove that a new webhook writer is not blocked by index construction.
	snapshot := db.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, snapshot.Error)
	defer snapshot.Rollback()
	var before int64
	require.NoError(t, snapshot.Raw("SELECT COUNT(*) FROM public.inbound_events").Scan(&before).Error)

	result := make(chan error, 1)
	go func() { result <- databasepkg.CreateIndexes(db) }()
	workerDrained := false
	t.Cleanup(func() {
		_ = snapshot.Rollback().Error
		if workerDrained {
			return
		}
		select {
		case <-result:
			workerDrained = true
		case <-time.After(10 * time.Second):
			t.Errorf("concurrent index worker did not stop during cleanup")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var active bool
		require.NoError(t, db.Raw(`SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE datname = current_database()
			  AND query LIKE 'CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_inbound_events_identity_review_wamid%'
			  AND state = 'active'
		)`).Scan(&active).Error)
		if active {
			break
		}
		select {
		case err := <-result:
			workerDrained = true
			t.Fatalf("index build ended before the live-writer probe: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("concurrent index build did not reach its observable scan/snapshot phase")
		}
		time.Sleep(10 * time.Millisecond)
	}

	event := models.InboundEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		ChannelAccountID: channel.ID, DedupeKey: "ordinary-index-writer-" + uuid.NewString(),
		ProviderEventID: "ordinary-index-writer-" + uuid.NewString(), EventType: "message",
		Status: models.InboundEventStatusPending, SignatureValid: true, ReceivedAt: time.Now().UTC(),
		Headers: models.JSONB{}, Payload: models.JSONB{},
	}
	writeContext, cancelWrite := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWrite()
	require.NoError(t, db.WithContext(writeContext).Create(&event).Error,
		"a concurrent index build must not block a newly arriving inbox write")
	require.NoError(t, snapshot.Rollback().Error)

	select {
	case err := <-result:
		workerDrained = true
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("migration did not finish after the old snapshot was released")
	}
	var valid bool
	require.NoError(t, db.Raw(`SELECT COALESCE((SELECT indisvalid FROM pg_catalog.pg_index
		WHERE indexrelid = pg_catalog.to_regclass('public.uq_inbound_events_identity_review_wamid')), FALSE)`,
	).Scan(&valid).Error)
	assert.True(t, valid)
}

func TestWhatsAppIdentityReviewMigrationLocksFailFastAndRetryCleanly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	for _, tableName := range []string{"channel_accounts", "messages"} {
		t.Run(tableName, func(t *testing.T) {
			holder := db.Begin()
			require.NoError(t, holder.Error)
			defer holder.Rollback()
			require.NoError(t, holder.Exec(
				"LOCK TABLE public."+tableName+" IN ROW EXCLUSIVE MODE",
			).Error)

			started := time.Now()
			err := databasepkg.CreateIndexes(db)
			_ = requireIdentityReviewSQLState(t, err, "55P03")
			assert.Less(t, time.Since(started), 2*time.Second,
				"migration lock contention must fail promptly instead of deadlocking")
			require.NoError(t, holder.Rollback().Error)
			require.NoError(t, databasepkg.CreateIndexes(db),
				"the complete migration must succeed after the conflicting writer ends")
		})
	}
}

func TestWhatsAppIdentityReviewActivationAllowsLongHistoricalValidation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	statement := fmt.Sprintf(`DO $block$ BEGIN
		LOCK TABLE public.contacts IN EXCLUSIVE MODE NOWAIT;
		UPDATE public.contacts SET profile_name = 'cutover-after-long-validation'
		WHERE id = '%s'::uuid;
		PERFORM pg_catalog.pg_sleep(1.25);
	END $block$`, contact.ID)

	started := time.Now()
	require.NoError(t, databasepkg.ExecuteCoexistenceActivationForTest(db, statement))
	elapsed := time.Since(started)
	assert.GreaterOrEqual(t, elapsed, time.Second,
		"activation must not impose the former one-second deadline")

	var durable models.Contact
	require.NoError(t, db.Unscoped().First(&durable, "id = ?", contact.ID).Error)
	assert.Equal(t, "cutover-after-long-validation", durable.ProfileName)
}

func TestWhatsAppIdentityReviewActivationErrorRollsBackTheWholeStatement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	originalProfileName := contact.ProfileName
	statement := fmt.Sprintf(`DO $block$ BEGIN
		LOCK TABLE public.contacts IN EXCLUSIVE MODE NOWAIT;
		UPDATE public.contacts SET profile_name = 'cutover-must-roll-back'
		WHERE id = '%s'::uuid;
		RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'deliberate cutover failure';
	END $block$`, contact.ID)

	err := databasepkg.ExecuteCoexistenceActivationForTest(db, statement)
	_ = requireIdentityReviewSQLState(t, err, "P0001")

	var durable models.Contact
	require.NoError(t, db.Unscoped().First(&durable, "id = ?", contact.ID).Error)
	assert.Equal(t, originalProfileName, durable.ProfileName,
		"a cutover error must roll back every write in the activation transaction")
	require.NoError(t, databasepkg.CreateIndexes(db),
		"the production migration path must remain retryable after a rolled-back activation")
}

func TestWhatsAppIdentityReviewMigrationRejectsExistingRowLockWithoutRetainingPrefixLocks(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Channel: models.ChannelMessenger, Provider: "meta_graph", Name: "migration-row-lock",
		ExternalAccountID: "migration-row-lock-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&channel).Error)
	conversation := models.InboxConversation{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		ChannelAccountID: channel.ID, ContactID: contact.ID, Channel: models.ChannelMessenger,
		ExternalConversationID: "migration-row-lock-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
		Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&conversation).Error)
	message := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		WhatsAppAccount: account.Name, ContactID: contact.ID, InboxConversationID: &conversation.ID,
		WhatsAppMessageID: "provider.migration.row-lock", Direction: models.DirectionIncoming,
		MessageType: models.MessageTypeText, Status: models.MessageStatusReceived, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)

	liveWriter := db.Begin()
	require.NoError(t, liveWriter.Error)
	defer liveWriter.Rollback()
	require.NoError(t, liveWriter.Exec("SET LOCAL lock_timeout = '1s'").Error)
	var lockedID string
	require.NoError(t, liveWriter.Raw(
		"SELECT id::text FROM public.messages WHERE organization_id = ? AND id = ? FOR UPDATE",
		organization.ID, message.ID,
	).Scan(&lockedID).Error)
	assert.Equal(t, message.ID.String(), lockedID)

	started := time.Now()
	err := databasepkg.CreateIndexes(db)
	_ = requireIdentityReviewSQLState(t, err, "55P03")
	assert.Less(t, time.Since(started), 2*time.Second,
		"migration must reject an existing row lock before performing its marker backfill")
	require.NoError(t, liveWriter.Model(&models.Message{}).
		Where("organization_id = ? AND id = ?", organization.ID, message.ID).
		Update("status", models.MessageStatusRead).Error,
		"a failed migration attempt must not retain a prefix table lock against the live writer")
	require.NoError(t, liveWriter.Commit().Error)
	require.NoError(t, databasepkg.CreateIndexes(db),
		"the complete migration must succeed after the live writer commits")
}
