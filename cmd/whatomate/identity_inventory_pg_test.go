package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

// These small generated tables exercise defensive malformed-state handling.
// They are not presented as an installed-schema fixture; the separate complete
// BootstrapEmptyDatabase control below provides that compatibility boundary.
func newIdentityInventoryFixture(t *testing.T) (*migrationPlanFixture, config.DatabaseConfig, catalogSnapshotArgs) {
	t.Helper()
	f := newMigrationPlanFixture(t, false)
	cfg := f.cfg
	cfg.URL = cfg.MigrationURL
	cfg.MigrationURL = "must-not-be-used"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, cfg.URL)
	planPGOK(t, err, "connect synthetic identity owner")
	defer func() { planPGOK(t, owner.Close(ctx), "close synthetic identity owner") }()
	_, err = owner.Exec(ctx, `CREATE TABLE public.organizations(id uuid PRIMARY KEY,deleted_at timestamptz);
CREATE TABLE public.whatsapp_accounts(id uuid PRIMARY KEY,organization_id uuid,phone_id text,status text,is_smb boolean,updated_at timestamptz,deleted_at timestamptz,access_token text,app_secret text);
CREATE TABLE public.whatsapp_coexistence_states(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,version bigint,lifecycle_status text,updated_at timestamptz);
CREATE TABLE public.whatsapp_identity_review_holds(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,protocol_version integer,supported boolean,direct_primary_bsuid text,parent_bsuid text,phone text,principal_generation bigint,semantic_claim_digest text,selector_body_digest text,verified_event_digest text,verified_event_provenance text,member_count bigint,member_digest text,version bigint,disposition text);
CREATE TABLE public.whatsapp_identity_review_members(organization_id uuid,hold_id uuid,contact_id uuid,selector_reasons integer);
CREATE TABLE public.channel_accounts(id uuid PRIMARY KEY,organization_id uuid,channel text,provider text,external_account_id text,metadata jsonb,deleted_at timestamptz);
CREATE TABLE public.inbound_events(id uuid PRIMARY KEY,organization_id uuid,channel_account_id uuid,review_hold_id uuid,protocol text,event_type text,status text,signature_valid boolean,deleted_at timestamptz,payload jsonb,headers jsonb,error_message text);
CREATE INDEX identity_fixture_holds_scope ON public.whatsapp_identity_review_holds(organization_id,whats_app_account_id);
CREATE INDEX identity_fixture_members_hold ON public.whatsapp_identity_review_members(organization_id,hold_id);
CREATE INDEX identity_fixture_receipts_hold ON public.inbound_events(organization_id,review_hold_id);
CREATE INDEX identity_fixture_receipts_account ON public.inbound_events(organization_id,channel_account_id);`)
	planPGOK(t, err, "create defensive identity projection tables")
	for _, table := range identityInventoryFixtureTables()[1:] {
		name := pgx.Identifier{"public", table}.Sanitize()
		_, err = owner.Exec(ctx, "ALTER TABLE "+name+" ENABLE ROW LEVEL SECURITY; ALTER TABLE "+name+" FORCE ROW LEVEL SECURITY; CREATE POLICY rereply_migration_access ON "+name+" TO "+pgx.Identifier{f.owner}.Sanitize()+" USING(true) WITH CHECK(true); CREATE POLICY rereply_tenant_isolation ON "+name+" TO "+pgx.Identifier{f.runtime}.Sanitize()+" USING(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid) WITH CHECK(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid)")
		planPGOK(t, err, "install canonical synthetic tenant policy")
	}
	args, err := parseCatalogSnapshotArgs(identityInventoryArguments())
	require.NoError(t, err)
	_, err = owner.Exec(ctx, "INSERT INTO public.organizations(id) VALUES($1::uuid)", args.organizationID)
	planPGOK(t, err, "insert identity scope organization")
	_, err = owner.Exec(ctx, "INSERT INTO public.whatsapp_accounts(id,organization_id,phone_id,status,is_smb,updated_at,access_token,app_secret) VALUES($1::uuid,$2::uuid,'private-phone-id','active',true,now(),'never-observe-credential','never-observe-secret')", args.accountID, args.organizationID)
	planPGOK(t, err, "insert identity scope account")
	_, err = owner.Exec(ctx, "INSERT INTO public.whatsapp_coexistence_states(id,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status,updated_at) VALUES($1,$2::uuid,$3::uuid,2,1,'connected',now())", uuid.New(), args.organizationID, args.accountID)
	planPGOK(t, err, "insert identity scope state")
	return f, cfg, args
}

func identityInventoryFixtureTables() []string {
	return []string{"organizations", "whatsapp_accounts", "whatsapp_coexistence_states", "whatsapp_identity_review_holds", "whatsapp_identity_review_members", "inbound_events", "channel_accounts"}
}

func identityInventoryExec(t *testing.T, f *migrationPlanFixture, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, query, args...)
	planPGOK(t, err, "prepare synthetic identity evidence")
}

func identityInventoryHold(t *testing.T, f *migrationPlanFixture, args catalogSnapshotArgs, supported bool, direct, parent, phone string, cycle int64, members int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	generation := int64(0)
	if supported {
		generation = 1
	}
	identityInventoryExec(t, f, `INSERT INTO public.whatsapp_identity_review_holds(id,organization_id,whats_app_account_id,onboarding_cycle,protocol_version,supported,direct_primary_bsuid,parent_bsuid,phone,principal_generation,semantic_claim_digest,selector_body_digest,verified_event_digest,verified_event_provenance,member_count,member_digest,version,disposition)
 VALUES($1,$2::uuid,$3::uuid,$4,1,$5,$6,$7,$8,$9,repeat('a',64),repeat('b',64),repeat('c',64),'meta_signed_webhook',$10,repeat('d',64),1,'open')`, id, args.organizationID, args.accountID, cycle, supported, direct, parent, phone, generation, members)
	return id
}

func identityInventoryShadow(t *testing.T, f *migrationPlanFixture, args catalogSnapshotArgs) uuid.UUID {
	t.Helper()
	id := uuid.New()
	identityInventoryExec(t, f, "INSERT INTO public.channel_accounts(id,organization_id,channel,provider,external_account_id,metadata) VALUES($1,$2::uuid,'whatsapp','meta',$3,jsonb_build_object('legacy_account_id',$4::text))", id, args.organizationID, "legacy-account:"+args.accountID, args.accountID)
	return id
}

func identityInventoryReceipt(t *testing.T, f *migrationPlanFixture, args catalogSnapshotArgs, hold, shadow uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	identityInventoryExec(t, f, `INSERT INTO public.inbound_events(id,organization_id,channel_account_id,review_hold_id,protocol,event_type,status,signature_valid,payload,headers,error_message)
 VALUES($1,$2::uuid,$3,$4,'whatsapp_identity_review_v1','review_pending','pending',true,'{"private":"never-observe-payload"}','{"private":"never-observe-header"}','never-observe-error')`, id, args.organizationID, shadow, hold)
	return id
}

func readIdentityInventory(t *testing.T, cfg config.DatabaseConfig, args catalogSnapshotArgs) database.IdentityInventoryReport {
	t.Helper()
	raw, err := collectIdentityInventory(context.Background(), cfg, args)
	planPGOK(t, err, "collect complete native identity report")
	var output struct {
		database.IdentityInventoryReport
		FreshScopeMatch         bool `json:"fresh_scope_match"`
		SessionBeforeAfterMatch bool `json:"session_before_after_match"`
		SessionDisposed         bool `json:"session_disposed"`
	}
	require.NoError(t, json.Unmarshal(raw, &output))
	require.True(t, output.FreshScopeMatch)
	require.True(t, output.SessionBeforeAfterMatch)
	require.True(t, output.SessionDisposed)
	for _, private := range []string{args.organizationID, args.accountID, "private-phone-id", "never-observe-credential", "never-observe-secret", "never-observe-payload", "never-observe-header", "never-observe-error"} {
		require.NotContains(t, string(raw), private)
	}
	return output.IdentityInventoryReport
}

func identityInventoryOpen(t *testing.T, parent context.Context, cfg config.DatabaseConfig) (context.Context, context.CancelFunc, planDeadline, *pgx.Conn, pgx.Tx) {
	t.Helper()
	d, ok := newPlanDeadline(parent, time.Now())
	require.True(t, ok)
	ctx, cancel := context.WithDeadline(parent, d.workAt)
	parsed, err := pgx.ParseConfig(cfg.URL)
	planPGOK(t, err, "parse identity observer config")
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	planPGOK(t, err, "configure identity observer")
	observer.RuntimeParams["application_name"] = database.IdentityInventoryApplication
	conn, tx, err := openPlanSession(ctx, observer)
	planPGOK(t, err, "open identity observer")
	return ctx, cancel, d, conn, tx
}

func TestIdentityInventoryPGNativeAdmissionAndVisibility(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	require.Zero(t, readIdentityInventory(t, cfg, args).Counts.Holds)
	runtimeCfg := cfg
	runtimeCfg.URL = f.cfg.URL
	raw, err := collectIdentityInventory(context.Background(), runtimeCfg, args)
	require.Error(t, err)
	require.Nil(t, raw, "runtime login is not the native database owner")
	for _, scenario := range []string{"superuser", "set_role", "read_write", "read_committed", "wrong_marker", "managed_native_owner"} {
		t.Run(scenario, func(t *testing.T) {
			parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			d, ok := newPlanDeadline(parent, time.Now())
			require.True(t, ok)
			ctx, cancel := context.WithDeadline(parent, d.workAt)
			defer cancel()
			parsed, err := pgx.ParseConfig(cfg.URL)
			planPGOK(t, err, "parse native admission fixture")
			if scenario == "superuser" || scenario == "set_role" {
				parsed = f.admin.Config()
			}
			if scenario == "managed_native_owner" {
				identityInventoryExec(t, f, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEROLE BYPASSRLS REPLICATION")
			}
			observer, err := seedDiagnosticConfig(parsed, d, ctx)
			planPGOK(t, err, "configure native admission fixture")
			observer.RuntimeParams["application_name"] = database.IdentityInventoryApplication
			if scenario == "wrong_marker" {
				observer.RuntimeParams["application_name"] = database.SeedDiagnosticApplication
			}
			conn, err := pgx.ConnectConfig(ctx, observer)
			planPGOK(t, err, "connect native admission fixture")
			opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
			if scenario == "read_write" {
				opts.AccessMode = pgx.ReadWrite
			}
			if scenario == "read_committed" {
				opts.IsoLevel = pgx.ReadCommitted
			}
			tx, err := conn.BeginTx(ctx, opts)
			planPGOK(t, err, "begin native admission fixture")
			defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
			if scenario == "set_role" {
				_, err = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{f.owner}.Sanitize())
				planPGOK(t, err, "set fixture impersonation")
			}
			report, _, err := database.ObserveIdentityInventory(ctx, tx, database.IdentityInventoryScope{OrganizationID: args.organizationID, AccountID: args.accountID})
			if scenario != "managed_native_owner" {
				require.Error(t, err)
				require.False(t, report.CompleteVisibility)
				return
			}
			planPGOK(t, err, "admit native managed owner")
			require.True(t, report.CompleteVisibility)
			_, err = database.ObservePlanSeedsOnHeldOwner(ctx, tx, database.PlanSeedHeldOwner{})
			require.Error(t, err, "diagnostic cannot manufacture planner witness")
			_, err = readPlanSelf(ctx, tx, database.IdentityInventoryApplication, f.runtime, false)
			require.Error(t, err, "different expected login is not this physical session")
		})
	}
	for _, change := range []struct{ name, before, after string }{
		{"hidden_policy", "ALTER POLICY rereply_migration_access ON public.whatsapp_identity_review_holds USING(false) WITH CHECK(false)", "ALTER POLICY rereply_migration_access ON public.whatsapp_identity_review_holds USING(true) WITH CHECK(true)"},
		{"force_rls", "ALTER TABLE public.inbound_events NO FORCE ROW LEVEL SECURITY", "ALTER TABLE public.inbound_events FORCE ROW LEVEL SECURITY"},
		{"tenant_policy", "ALTER POLICY rereply_tenant_isolation ON public.inbound_events USING(true) WITH CHECK(true)", "ALTER POLICY rereply_tenant_isolation ON public.inbound_events USING(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid) WITH CHECK(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid)"},
		{"global_rls", "ALTER TABLE public.organizations ENABLE ROW LEVEL SECURITY", "ALTER TABLE public.organizations DISABLE ROW LEVEL SECURITY"},
		{"inheritance", "CREATE TABLE public.identity_fixture_child() INHERITS(public.whatsapp_identity_review_holds)", "DROP TABLE public.identity_fixture_child"},
		{"missing_relation", "ALTER TABLE public.whatsapp_identity_review_members RENAME TO private_missing_members", "ALTER TABLE public.private_missing_members RENAME TO whatsapp_identity_review_members"},
		{"owner", "ALTER TABLE public.channel_accounts OWNER TO " + pgx.Identifier{f.runtime}.Sanitize(), "ALTER TABLE public.channel_accounts OWNER TO " + pgx.Identifier{f.owner}.Sanitize()},
		{"database_owner", "ALTER DATABASE " + pgx.Identifier{f.database}.Sanitize() + " OWNER TO " + pgx.Identifier{f.runtime}.Sanitize(), "ALTER DATABASE " + pgx.Identifier{f.database}.Sanitize() + " OWNER TO " + pgx.Identifier{f.owner}.Sanitize()},
	} {
		t.Run(change.name, func(t *testing.T) {
			identityInventoryExec(t, f, change.before)
			defer identityInventoryExec(t, f, change.after)
			raw, err := collectIdentityInventory(context.Background(), cfg, args)
			require.Error(t, err)
			require.Nil(t, raw)
		})
	}
	t.Run("full_installed_schema_empty_positive", func(t *testing.T) {
		full := newMigrationPlanFixture(t, true)
		fullCfg := full.cfg
		fullCfg.URL = fullCfg.MigrationURL
		fullCfg.MigrationURL = "must-not-be-used"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var org string
		planPGOK(t, full.admin.QueryRow(ctx, "SELECT id::text FROM public.organizations WHERE deleted_at IS NULL AND reseller_id IS NOT NULL ORDER BY id LIMIT 1").Scan(&org), "select seeded reseller-bound organization")
		account := uuid.New()
		identityInventoryExec(t, full, "INSERT INTO public.whatsapp_accounts(id,created_at,updated_at,organization_id,name,phone_id,business_id,access_token,is_smb,status) VALUES($1,now(),now(),$2::uuid,'identity-native-fixture','identity-native-phone','identity-native-business','synthetic-secret',true,'active')", account, org)
		identityInventoryExec(t, full, "INSERT INTO public.whatsapp_coexistence_states(id,created_at,updated_at,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status) VALUES($1,now(),now(),$2::uuid,$3,1,1,'connected')", uuid.New(), org, account)
		fullArgs := args
		fullArgs.organizationID = org
		fullArgs.accountID = account.String()
		report := readIdentityInventory(t, fullCfg, fullArgs)
		require.Zero(t, report.Counts.Holds)
		require.Zero(t, report.Counts.ReceiptRows)
		// Installed FK/shape guards remain enabled: an impossible state must be
		// refused by the database, not used as a fake valid observer fixture.
		_, err := full.admin.Exec(ctx, "INSERT INTO public.whatsapp_coexistence_states(id,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status) VALUES($1,$2::uuid,$3,1,1,'connected')", uuid.New(), org, uuid.New())
		require.Error(t, err)
	})
}

func TestIdentityInventoryPGQueryOneAndIndependentGrains(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	current := identityInventoryHold(t, f, args, false, "", "", "601122334455", 2, 2)
	identityInventoryHold(t, f, args, false, "", "parent-only", "", 1, 1)
	identityInventoryHold(t, f, args, false, "", "", "60119999999", 3, 3)
	identityInventoryHold(t, f, args, true, "direct-supported", "", "", 2, 2)
	identityInventoryHold(t, f, args, false, "", "", "", 2, 0)
	closed := identityInventoryHold(t, f, args, false, "legacy-direct", "", "", 2, 4)
	identityInventoryExec(t, f, "UPDATE public.whatsapp_identity_review_holds SET disposition='superseded_by_latest' WHERE id=$1", closed)
	shadow := identityInventoryShadow(t, f, args)
	for i := 0; i < 5; i++ {
		identityInventoryReceipt(t, f, args, current, shadow)
	}
	for i := 0; i < 2; i++ {
		identityInventoryExec(t, f, "INSERT INTO public.whatsapp_identity_review_members(organization_id,hold_id,contact_id,selector_reasons) VALUES($1::uuid,$2,$3,4)", args.organizationID, current, uuid.New())
	}
	foreign := args
	foreign.accountID = uuid.NewString()
	identityInventoryHold(t, f, foreign, false, "foreign-account-selector", "", "", 2, 999)
	foreign.organizationID = uuid.NewString()
	identityInventoryHold(t, f, foreign, false, "foreign-tenant-selector", "", "", 2, 999)
	report := readIdentityInventory(t, cfg, args)
	require.EqualValues(t, 6, report.Counts.Holds)
	require.EqualValues(t, 2, report.Counts.MemberRows)
	require.EqualValues(t, 5, report.Counts.ReceiptRows)
	require.EqualValues(t, 5, report.Counts.ReceiptRecordedCopyShape)
	require.EqualValues(t, 4, report.Counts.MemberCountMismatchHolds)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := f.admin.Query(ctx, `SELECT h.onboarding_cycle,s.onboarding_cycle AS current_cycle,count(*) AS holds,sum(h.member_count) AS member_rows
 FROM public.whatsapp_identity_review_holds h JOIN public.whatsapp_coexistence_states s ON s.organization_id=h.organization_id AND s.whats_app_account_id=h.whats_app_account_id
 WHERE NOT h.supported AND h.disposition='open' AND h.member_count>0 AND h.organization_id=$1::uuid AND h.whats_app_account_id=$2::uuid GROUP BY 1,2 ORDER BY 1`, args.organizationID, args.accountID)
	planPGOK(t, err, "execute retained scoped query one reference")
	defer rows.Close()
	index := 0
	for rows.Next() {
		var cycle, currentCycle, holds, members int64
		planPGOK(t, rows.Scan(&cycle, &currentCycle, &holds, &members), "scan reference aggregate")
		require.Less(t, index, len(report.QueryOne))
		actual := report.QueryOne[index]
		require.EqualValues(t, index+1, actual.Ordinal)
		require.Equal(t, holds, actual.Holds)
		require.Equal(t, members, actual.StoredMemberRows)
		relation := "future_invalid"
		if cycle == currentCycle {
			relation = "current"
		} else if cycle > 0 && cycle < currentCycle {
			relation = "older"
		}
		require.Equal(t, relation, actual.CycleRelation)
		index++
	}
	planPGOK(t, rows.Err(), "complete reference aggregate")
	require.Len(t, report.QueryOne, index)
	require.Equal(t, 3, index)
	for _, mutation := range []struct{ before, after string }{
		{"UPDATE public.whatsapp_accounts SET deleted_at=now()", "UPDATE public.whatsapp_accounts SET deleted_at=NULL"},
		{"UPDATE public.whatsapp_coexistence_states SET onboarding_cycle=0", "UPDATE public.whatsapp_coexistence_states SET onboarding_cycle=2"},
		{"UPDATE public.whatsapp_accounts SET organization_id='33333333-3333-4333-8333-333333333333'", "UPDATE public.whatsapp_accounts SET organization_id='11111111-1111-4111-8111-111111111111'"},
	} {
		identityInventoryExec(t, f, mutation.before)
		raw, err := collectIdentityInventory(context.Background(), cfg, args)
		require.Error(t, err)
		require.Nil(t, raw)
		identityInventoryExec(t, f, mutation.after)
	}
	identityInventoryExec(t, f, "INSERT INTO public.whatsapp_coexistence_states SELECT $1,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status,updated_at FROM public.whatsapp_coexistence_states", uuid.New())
	raw, err := collectIdentityInventory(context.Background(), cfg, args)
	require.Error(t, err)
	require.Nil(t, raw)
}

func TestIdentityInventoryPGRecordedClaimAndSenderUnits(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	identityInventoryHold(t, f, args, true, "direct-A", "parent-A", "60111111111", 2, 0)
	secondGeneration := identityInventoryHold(t, f, args, true, "direct-A", "parent-B", "60222222222", 2, 0)
	identityInventoryExec(t, f, "UPDATE public.whatsapp_identity_review_holds SET principal_generation=2 WHERE id=$1", secondGeneration)
	identityInventoryHold(t, f, args, true, "direct-B", "", "60111111111", 2, 0)
	identityInventoryHold(t, f, args, false, "direct-legacy", "", "", 2, 0)
	identityInventoryHold(t, f, args, false, "", "", "60111111111", 2, 0)
	identityInventoryHold(t, f, args, false, "", "", "60111111111", 2, 0)
	identityInventoryHold(t, f, args, false, "", "parent-only", "", 2, 0)
	identityInventoryHold(t, f, args, false, "", "", "123", 2, 0)
	identityInventoryHold(t, f, args, true, "direct-A", "", "", 1, 0)
	base := readIdentityInventory(t, cfg, args)
	require.EqualValues(t, 4, base.Counts.DirectSenderUnits)
	require.EqualValues(t, 1, base.Counts.PhoneSenderUnits)
	require.EqualValues(t, 1, base.Counts.UnsupportedWithDirect)
	require.EqualValues(t, 2, base.Counts.UnknownSenderHoldIncidences)
	require.EqualValues(t, 1, base.OverlappingHistograms.DirectUnitsWithMultiplePhones)
	require.EqualValues(t, 1, base.OverlappingHistograms.DirectUnitsWithMultipleParents)
	require.EqualValues(t, 1, base.OverlappingHistograms.PhoneGroupsWithMultipleDirectUnits)
	require.EqualValues(t, 1, base.OverlappingHistograms.PhoneOnlyUnitsAlsoUsedByDirect)
	// Real PostgreSQL predicates must agree with Go Unicode TrimSpace and byte
	// boundaries. BOM is not Unicode White_Space and is intentionally retained.
	for _, tc := range []struct {
		direct string
		valid  bool
	}{
		{"\u0085edge", false}, {"edge\u3000", false}, {"\u2007edge", false}, {"\ufeffedge", true},
		{strings.Repeat("a", 255), true}, {strings.Repeat("a", 256), false},
		{strings.Repeat("é", 127) + "a", true}, {strings.Repeat("é", 128), false},
	} {
		id := identityInventoryHold(t, f, args, true, tc.direct, "", "", 2, 0)
		current := readIdentityInventory(t, cfg, args)
		if tc.valid {
			require.EqualValues(t, base.Counts.RecordedVerifiedClaimShape+1, current.Counts.RecordedVerifiedClaimShape)
		} else {
			require.EqualValues(t, base.Counts.ClaimShapeUnknown+1, current.Counts.ClaimShapeUnknown)
		}
		identityInventoryExec(t, f, "DELETE FROM public.whatsapp_identity_review_holds WHERE id=$1", id)
	}
	for _, space := range "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" {
		direct := string(space) + "unicode-boundary" + string(space)
		require.NotEqual(t, direct, strings.TrimSpace(direct), "Go normalization reference must reject this stored form")
		id := identityInventoryHold(t, f, args, true, direct, "", "", 2, 0)
		current := readIdentityInventory(t, cfg, args)
		require.EqualValues(t, base.Counts.ClaimShapeUnknown+1, current.Counts.ClaimShapeUnknown)
		identityInventoryExec(t, f, "DELETE FROM public.whatsapp_identity_review_holds WHERE id=$1", id)
	}
	for _, length := range []int{6, 7, 15, 16, 50, 51} {
		id := identityInventoryHold(t, f, args, false, "", "", strings.Repeat("6", length), 2, 0)
		current := readIdentityInventory(t, cfg, args)
		if length >= 7 && length <= 15 {
			require.EqualValues(t, base.Counts.PhoneCandidate+1, current.Counts.PhoneCandidate)
		} else if length <= 50 {
			require.EqualValues(t, base.Counts.NoAddressCandidate+1, current.Counts.NoAddressCandidate)
		} else {
			require.EqualValues(t, base.Counts.ClaimShapeUnknown+1, current.Counts.ClaimShapeUnknown)
		}
		identityInventoryExec(t, f, "DELETE FROM public.whatsapp_identity_review_holds WHERE id=$1", id)
	}
	for _, set := range []string{"verified_event_digest='INVALID'", "verified_event_digest=repeat('A',64)", "verified_event_digest=repeat('a',64)||E'\\n'", "verified_event_provenance='untrusted'", "phone='+60111111111'", "phone='１２３４５６７'", "principal_generation=0", "protocol_version=2", "parent_bsuid=E' spaced\\t'", "supported=NULL"} {
		id := identityInventoryHold(t, f, args, true, "invalid-claim-control", "", "", 2, 0)
		identityInventoryExec(t, f, "UPDATE public.whatsapp_identity_review_holds SET "+set+" WHERE id=$1", id)
		current := readIdentityInventory(t, cfg, args)
		require.EqualValues(t, base.Counts.ClaimShapeUnknown+1, current.Counts.ClaimShapeUnknown)
		identityInventoryExec(t, f, "DELETE FROM public.whatsapp_identity_review_holds WHERE id=$1", id)
	}
}

func TestIdentityInventoryPGReceiptAndMemberAnomalies(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	hold := identityInventoryHold(t, f, args, false, "", "", "601122334455", 2, 2)
	shadow := identityInventoryShadow(t, f, args)
	identityInventoryReceipt(t, f, args, hold, shadow)
	orphan := identityInventoryReceipt(t, f, args, uuid.New(), shadow)
	wrong := identityInventoryReceipt(t, f, args, hold, uuid.New())
	bad := identityInventoryReceipt(t, f, args, hold, shadow)
	identityInventoryExec(t, f, "UPDATE public.inbound_events SET protocol='unknown',status='unknown',signature_valid=false,deleted_at=now() WHERE id=$1", bad)
	member := uuid.New()
	for _, reason := range []int{4, 8} {
		identityInventoryExec(t, f, "INSERT INTO public.whatsapp_identity_review_members(organization_id,hold_id,contact_id,selector_reasons) VALUES($1::uuid,$2,$3,$4)", args.organizationID, hold, member, reason)
	}
	report := readIdentityInventory(t, cfg, args)
	require.EqualValues(t, 4, report.Counts.ReceiptRows)
	require.EqualValues(t, 3, report.Counts.ReceiptLinkedToHold)
	require.EqualValues(t, 1, report.Counts.ReceiptAccountOnly)
	require.EqualValues(t, 1, report.Counts.ReceiptRecordedCopyShape)
	require.EqualValues(t, 3, report.Counts.ReceiptShapeUnknown)
	require.EqualValues(t, 1, report.Counts.ReceiptWrongAccount)
	require.EqualValues(t, 1, report.Counts.ReceiptDeleted)
	require.EqualValues(t, 1, report.Counts.MemberDuplicateRows)
	require.EqualValues(t, 1, report.Counts.MemberSelectorInvalid)
	require.EqualValues(t, 1, report.DerivedReasons.MemberCountOrSelectorShapeInconsistent)
	identityInventoryExec(t, f, "UPDATE public.inbound_events SET organization_id=$1 WHERE id=$2", uuid.New(), orphan)
	require.EqualValues(t, 3, readIdentityInventory(t, cfg, args).Counts.ReceiptRows, "foreign tenant rows are outside this scoped census, not globally absent")
	identityInventoryExec(t, f, "UPDATE public.channel_accounts SET deleted_at=now() WHERE id=$1", shadow)
	report = readIdentityInventory(t, cfg, args)
	require.Equal(t, "deleted_exact", report.ShadowState)
	require.Zero(t, report.Counts.ReceiptRecordedCopyShape)
	identityInventoryExec(t, f, "UPDATE public.channel_accounts SET deleted_at=NULL,provider='wrong' WHERE id=$1", shadow)
	report = readIdentityInventory(t, cfg, args)
	require.Equal(t, "contradictory", report.ShadowState)
	require.Zero(t, report.Counts.ReceiptRecordedCopyShape)
	identityInventoryExec(t, f, "UPDATE public.channel_accounts SET provider='meta',metadata=jsonb_build_object('legacy_account_id',$1::text) WHERE id=$2", uuid.NewString(), shadow)
	raw, err := collectIdentityInventory(context.Background(), cfg, args)
	require.Error(t, err)
	require.Nil(t, raw, "one row with competing account selectors is ambiguous")
	identityInventoryExec(t, f, "UPDATE public.channel_accounts SET metadata=jsonb_build_object('legacy_account_id',$1::text) WHERE id=$2", args.accountID, shadow)
	duplicate := identityInventoryShadow(t, f, args)
	identityInventoryExec(t, f, "UPDATE public.channel_accounts SET deleted_at=now() WHERE id=$1", duplicate)
	raw, err = collectIdentityInventory(context.Background(), cfg, args)
	require.Error(t, err)
	require.Nil(t, raw, "soft-deleted predecessor remains a competing claim")
	require.NotEqual(t, uuid.Nil, wrong)
}

func TestIdentityInventoryPGCapsAllOrRefuse(t *testing.T) {
	for _, kind := range []string{"holds", "members", "receipts", "cycles"} {
		t.Run(kind, func(t *testing.T) {
			f, cfg, args := newIdentityInventoryFixture(t)
			hold := identityInventoryHold(t, f, args, false, "", "", "601122334455", 2, 0)
			shadow := identityInventoryShadow(t, f, args)
			switch kind {
			case "holds":
				identityInventoryExec(t, f, `INSERT INTO public.whatsapp_identity_review_holds SELECT md5('identity-cap-'||n)::uuid,organization_id,whats_app_account_id,onboarding_cycle,protocol_version,supported,direct_primary_bsuid,parent_bsuid,phone,principal_generation,semantic_claim_digest,selector_body_digest,verified_event_digest,verified_event_provenance,member_count,member_digest,version,disposition FROM public.whatsapp_identity_review_holds CROSS JOIN generate_series(1,9999) n WHERE id=$1`, hold)
				require.EqualValues(t, 10000, readIdentityInventory(t, cfg, args).Counts.Holds)
				identityInventoryHold(t, f, args, false, "", "", "", 2, 0)
			case "members":
				identityInventoryExec(t, f, "INSERT INTO public.whatsapp_identity_review_members SELECT $1::uuid,$2::uuid,md5('identity-member-'||n)::uuid,4 FROM generate_series(1,100000) n", args.organizationID, hold)
				require.EqualValues(t, 100000, readIdentityInventory(t, cfg, args).Counts.MemberRows)
				identityInventoryExec(t, f, "INSERT INTO public.whatsapp_identity_review_members VALUES($1::uuid,$2,$3,4)", args.organizationID, hold, uuid.New())
			case "receipts":
				identityInventoryExec(t, f, "INSERT INTO public.inbound_events(id,organization_id,channel_account_id,review_hold_id,protocol,event_type,status,signature_valid) SELECT md5('identity-receipt-'||n)::uuid,$1::uuid,$2::uuid,$3::uuid,'whatsapp_identity_review_v1','review_pending','pending',true FROM generate_series(1,20000) n", args.organizationID, shadow, hold)
				require.EqualValues(t, 20000, readIdentityInventory(t, cfg, args).Counts.ReceiptRows)
				identityInventoryReceipt(t, f, args, hold, shadow)
			case "cycles":
				identityInventoryExec(t, f, "UPDATE public.whatsapp_identity_review_holds SET onboarding_cycle=1")
				identityInventoryExec(t, f, `INSERT INTO public.whatsapp_identity_review_holds SELECT md5('identity-cycle-'||n)::uuid,organization_id,whats_app_account_id,n,protocol_version,supported,direct_primary_bsuid,parent_bsuid,phone,principal_generation,semantic_claim_digest,selector_body_digest,verified_event_digest,verified_event_provenance,1,member_digest,version,disposition FROM public.whatsapp_identity_review_holds CROSS JOIN generate_series(2,256) n WHERE id=$1`, hold)
				require.EqualValues(t, 256, readIdentityInventory(t, cfg, args).Counts.Holds)
				identityInventoryHold(t, f, args, false, "", "", "", 257, 1)
			}
			raw, err := collectIdentityInventory(context.Background(), cfg, args)
			require.Error(t, err)
			require.Nil(t, raw, "overflow never returns a truncated aggregate")
		})
	}
	// The512 aggregate-row cap is a conservative second fence: this report has
	// at most256 cycle rows plus three fixed projections.257 cycles is rejected
	// first; a database fixture cannot honestly reach512 successful output rows.
	// The decoder group covers synthetic oversized arrays and exact64KiB bytes.
}

// A test-only pgx trace barrier intercepts the actual collector's second BEGIN,
// after the first rollback has succeeded. No product hook or query rewrite is
// introduced. The initiating context controls this owned wait.
type identityInventorySecondBegin struct {
	count   int
	reached chan struct{}
	release chan struct{}
}

func (b *identityInventorySecondBegin) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(data.SQL), "begin") {
		b.count++
		if b.count == 2 {
			close(b.reached)
			select {
			case <-b.release:
			case <-ctx.Done():
			}
		}
	}
	return ctx
}
func (*identityInventorySecondBegin) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestIdentityInventoryPGRepeatableSnapshotAndFreshScope(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	identityInventoryHold(t, f, args, false, "", "", "601122334455", 2, 0)
	scope := database.IdentityInventoryScope{OrganizationID: args.organizationID, AccountID: args.accountID}
	func() {
		parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		ctx, cancel, d, conn, tx := identityInventoryOpen(t, parent, cfg)
		defer cancel()
		defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
		first, witness, err := database.ObserveIdentityInventory(ctx, tx, scope)
		planPGOK(t, err, "observe first repeatable identity snapshot")
		identityInventoryHold(t, f, args, false, "new-direct-after-snapshot", "", "", 2, 0)
		again, _, err := database.ObserveIdentityInventory(ctx, tx, scope)
		planPGOK(t, err, "observe unchanged retained snapshot")
		require.Equal(t, first, again)
		before, err := readPlanSelf(ctx, tx, database.IdentityInventoryApplication, conn.Config().User, false)
		planPGOK(t, err, "read first physical identity")
		planPGOK(t, tx.Rollback(ctx), "end first identity snapshot")
		tx = nil
		tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		planPGOK(t, err, "begin fresh identity snapshot")
		planPGOK(t, database.RecheckIdentityInventoryScope(ctx, tx, scope, witness), "hold additions do not masquerade as scope drift")
		after, err := readPlanSelf(ctx, tx, database.IdentityInventoryApplication, conn.Config().User, false)
		planPGOK(t, err, "read fresh physical identity")
		require.True(t, samePlanSelf(before, after))
	}()
	for i, mutation := range []string{
		"UPDATE public.whatsapp_accounts SET phone_id=phone_id||'-changed',updated_at=now()",
		"UPDATE public.whatsapp_coexistence_states SET version=version+1,updated_at=now()",
		"UPDATE public.whatsapp_coexistence_states SET onboarding_cycle=onboarding_cycle+1,updated_at=now()",
		"UPDATE public.whatsapp_accounts SET deleted_at=now()",
	} {
		t.Run([]string{"account_binding", "state_version", "state_cycle", "account_deletion"}[i], func(t *testing.T) {
			workParent, workStop := context.WithTimeout(context.Background(), 20*time.Second)
			defer workStop()
			d, ok := newPlanDeadline(workParent, time.Now())
			require.True(t, ok)
			work, workCancel := context.WithDeadline(workParent, d.workAt)
			defer workCancel()
			parsed, err := pgx.ParseConfig(cfg.URL)
			planPGOK(t, err, "parse traced collector")
			observer, err := seedDiagnosticConfig(parsed, d, work)
			planPGOK(t, err, "configure traced collector")
			observer.RuntimeParams["application_name"] = database.IdentityInventoryApplication
			barrier := &identityInventorySecondBegin{reached: make(chan struct{}), release: make(chan struct{})}
			observer.Tracer = barrier
			type result struct {
				raw []byte
				err error
			}
			done := make(chan result, 1)
			joined := false
			released := false
			go func() { raw, err := captureIdentityInventory(work, d, observer, scope); done <- result{raw, err} }()
			defer func() {
				if !released {
					close(barrier.release)
				}
				workCancel()
				if !joined {
					select {
					case <-done:
						joined = true
					case <-time.After(3 * time.Second):
						t.Error("owned identity capture did not drain")
					}
				}
			}()
			select {
			case <-barrier.reached:
			case <-work.Done():
				t.Fatal("collector did not reach its second transaction")
			}
			identityInventoryExec(t, f, mutation)
			close(barrier.release)
			released = true
			select {
			case outcome := <-done:
				joined = true
				require.Error(t, outcome.err)
				require.Nil(t, outcome.raw)
			case <-work.Done():
				t.Fatal("traced capture did not return")
			}
			identityInventoryExec(t, f, "UPDATE public.whatsapp_accounts SET deleted_at=NULL")
		})
	}
	// Both real initial/fresh phases succeed when the same bounded scope is
	// stable. No claim is made that later hold or account changes are prevented.
	require.True(t, readIdentityInventory(t, cfg, args).CompleteVisibility)
}

func TestIdentityInventoryPGLockCancelAndDisposal(t *testing.T) {
	for _, cancelAtWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "server_lock_timeout", true: "parent_cancel"}[cancelAtWait], func(t *testing.T) {
			f, cfg, args := newIdentityInventoryFixture(t)
			identityInventoryHold(t, f, args, false, "", "", "601122334455", 2, 0)
			parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			watch, err := pgx.ConnectConfig(parent, f.admin.Config())
			planPGOK(t, err, "connect independent lock witness")
			defer func() {
				cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
				defer done()
				planPGOK(t, watch.Close(cleanup), "close lock witness")
			}()
			blocker, err := f.admin.Begin(parent)
			planPGOK(t, err, "begin owned identity blocker")
			_, err = blocker.Exec(parent, "LOCK TABLE public.whatsapp_identity_review_holds IN ACCESS EXCLUSIVE MODE")
			planPGOK(t, err, "hold exact identity relation")
			blockerPID := int32(f.admin.PgConn().PID())
			work, cancel := context.WithCancel(parent)
			type result struct {
				raw []byte
				err error
			}
			resultCh := make(chan result, 1)
			joined := false
			rolledBack := false
			start := time.Now()
			go func() { raw, err := collectIdentityInventory(work, cfg, args); resultCh <- result{raw, err} }()
			defer func() {
				cancel()
				var rollbackErr error
				if !rolledBack {
					cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
					rollbackErr = blocker.Rollback(cleanup)
					done()
				}
				if !joined {
					select {
					case <-resultCh:
						joined = true
					case <-time.After(3 * time.Second):
					}
				}
				if rollbackErr != nil {
					t.Errorf("release owned blocker: %v", rollbackErr)
				}
				if !joined {
					t.Error("owned blocked collector did not drain")
				}
			}()
			var observedPID int32
			until := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(until) {
				var pid *int32
				err = watch.QueryRow(parent, `SELECT a.pid FROM pg_catalog.pg_stat_activity a WHERE a.datname=$1 AND a.application_name=$2 AND a.wait_event_type='Lock'
 AND ((a.query LIKE '%h AS MATERIALIZED%' AND a.query LIKE '%FROM public.whatsapp_identity_review_holds%')
 OR a.query LIKE '%COALESCE(pg_catalog.pg_get_expr(tenant.polqual,tenant.polrelid,false)%')
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_locks l WHERE l.pid=a.pid AND NOT l.granted AND l.relation='public.whatsapp_identity_review_holds'::pg_catalog.regclass)
 AND $3::integer=ANY(pg_catalog.pg_blocking_pids(a.pid))`, f.database, database.IdentityInventoryApplication, blockerPID).Scan(&pid)
				if err == nil && pid != nil {
					observedPID = *pid
					break
				}
				if err != nil && err != pgx.ErrNoRows {
					planPGOK(t, err, "observe exact identity lock wait")
				}
				time.Sleep(5 * time.Millisecond)
			}
			require.NotZero(t, observedPID, "must observe the fixed visibility/cohort query waiting on this exact relation and blocker")
			if cancelAtWait {
				cancel()
			}
			select {
			case outcome := <-resultCh:
				joined = true
				require.Error(t, outcome.err)
				require.Nil(t, outcome.raw)
			case <-time.After(4 * time.Second):
				t.Fatal("bounded blocked collector did not finish")
			}
			require.Less(t, time.Since(start), 5*time.Second)
			_, err = watch.Exec(parent, "SELECT pg_catalog.pg_stat_clear_snapshot()")
			planPGOK(t, err, "refresh cleanup witness")
			var remaining int
			planPGOK(t, watch.QueryRow(parent, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid=$1 AND datname=$2", observedPID, f.database).Scan(&remaining), "verify exact observer disposed")
			require.Zero(t, remaining)
			planPGOK(t, blocker.Rollback(parent), "release observed relation blocker")
			rolledBack = true
			require.EqualValues(t, 1, readIdentityInventory(t, cfg, args).Counts.Holds)
		})
	}
}

func TestIdentityInventoryPGNoWritesOrExternalEffects(t *testing.T) {
	f, cfg, args := newIdentityInventoryFixture(t)
	hold := identityInventoryHold(t, f, args, false, "private-direct", "private-parent", "601122334455", 2, 0)
	shadow := identityInventoryShadow(t, f, args)
	identityInventoryReceipt(t, f, args, hold, shadow)
	fingerprint := func() map[string][32]byte {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out := make(map[string][32]byte)
		for _, table := range identityInventoryFixtureTables() {
			var raw string
			planPGOK(t, f.admin.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM public."+pgx.Identifier{table}.Sanitize()+" t").Scan(&raw), "fingerprint private synthetic rows")
			out[table] = sha256.Sum256([]byte(raw))
		}
		var catalog string
		planPGOK(t, f.admin.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(c.relname,c.relowner,c.relrowsecurity,c.relforcerowsecurity,p.polname,p.polroles,pg_get_expr(p.polqual,p.polrelid),pg_get_expr(p.polwithcheck,p.polrelid)) ORDER BY c.relname,p.polname)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_policy p ON p.polrelid=c.oid WHERE n.nspname='public'").Scan(&catalog), "fingerprint private synthetic policy catalog")
		out["catalog"] = sha256.Sum256([]byte(catalog))
		return out
	}
	before := fingerprint()
	raw, err := collectIdentityInventory(context.Background(), cfg, args)
	planPGOK(t, err, "read nonmutating identity diagnostic")
	require.Equal(t, before, fingerprint())
	for _, secret := range []string{"private-direct", "private-parent", "601122334455", hold.String(), shadow.String(), args.organizationID, args.accountID, "never-observe"} {
		require.NotContains(t, string(raw), secret)
	}
	parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	ctx, cancel, d, conn, tx := identityInventoryOpen(t, parent, cfg)
	defer cancel()
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	_, err = tx.Exec(ctx, "UPDATE public.whatsapp_identity_review_holds SET member_count=member_count+1 WHERE id=$1", hold)
	require.Error(t, err, "the actual observer session rejects DML")
	require.Equal(t, before, fingerprint())
}
