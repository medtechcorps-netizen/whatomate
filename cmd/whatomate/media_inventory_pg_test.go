package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

// This fixture creates only synthetic tables/rows in the existing guarded,
// generated loopback PG17 database. It never adopts production data or access.
func newMediaInventoryFixture(t *testing.T) (*migrationPlanFixture, config.DatabaseConfig, catalogSnapshotArgs) {
	t.Helper()
	f := newMigrationPlanFixture(t, false)
	cfg := f.cfg
	cfg.URL = cfg.MigrationURL
	cfg.MigrationURL = "must-not-be-used"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, cfg.URL)
	planPGOK(t, err, "connect generated inventory owner")
	defer func() { planPGOK(t, owner.Close(ctx), "close generated inventory owner") }()
	_, err = owner.Exec(ctx, `CREATE TABLE public.organizations(id uuid PRIMARY KEY,deleted_at timestamptz);
CREATE TABLE public.whatsapp_accounts(id uuid PRIMARY KEY,organization_id uuid,name text,phone_id text,is_smb boolean,status text,access_token_expires_at timestamptz,deleted_at timestamptz,access_token text,app_secret text);
CREATE TABLE public.messages(id uuid PRIMARY KEY,organization_id uuid,created_at timestamptz,updated_at timestamptz,deleted_at timestamptz,ingested_at timestamptz,whats_app_account text,contact_id uuid,whats_app_message_id text,inbox_conversation_id uuid,direction text,message_type text,content text,media_url text,media_mime_type text,media_filename text,metadata jsonb);
CREATE TABLE public.scheduled_jobs(id uuid PRIMARY KEY,organization_id uuid,created_at timestamptz,updated_at timestamptz,deleted_at timestamptz,kind text,aggregate_type text,aggregate_id uuid,status text,attempts integer,max_attempts integer,idempotency_key text,payload jsonb,version bigint,last_error text);
CREATE TABLE public.contacts(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,phone_number text,bsuid text,merged_into_id uuid);
CREATE TABLE public.customer_activity_events(id uuid PRIMARY KEY,organization_id uuid,contact_id uuid,event_type text,category text,source_object_type text,source_object_id uuid,idempotency_key text);
CREATE TABLE public.channel_accounts(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,channel text,provider text,external_account_id text,metadata jsonb);
CREATE TABLE public.inbox_conversations(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,channel_account_id uuid,contact_id uuid,contact_identity_id uuid,channel text,external_conversation_id text);
CREATE TABLE public.contact_identities(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,contact_id uuid,channel_account_id uuid,channel text,external_id text);
CREATE TABLE public.whatsapp_coexistence_states(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,lifecycle_status text,version bigint);
CREATE TABLE public.inbound_events(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,channel_account_id uuid,provider_event_id text,event_type text,status text,signature_valid boolean,received_at timestamptz,protocol text,review_hold_id uuid,payload jsonb,headers jsonb,error_message text);
CREATE TABLE public.whatsapp_identity_review_holds(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,protocol_version integer,supported boolean,principal_generation bigint,member_count integer,member_digest text,version bigint,disposition text);
CREATE TABLE public.whatsapp_identity_review_members(organization_id uuid,hold_id uuid,contact_id uuid,selector_reasons integer,PRIMARY KEY(organization_id,hold_id,contact_id));`)
	planPGOK(t, err, "create generated media relations")
	for _, table := range mediaInventoryFixtureTables()[1:] {
		quoted := pgx.Identifier{"public", table}.Sanitize()
		_, err = owner.Exec(ctx, "ALTER TABLE "+quoted+" ENABLE ROW LEVEL SECURITY; ALTER TABLE "+quoted+" FORCE ROW LEVEL SECURITY; CREATE POLICY rereply_migration_access ON "+quoted+" TO "+pgx.Identifier{f.owner}.Sanitize()+" USING(true) WITH CHECK(true); CREATE POLICY rereply_tenant_isolation ON "+quoted+" TO "+pgx.Identifier{f.runtime}.Sanitize()+" USING(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid) WITH CHECK(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid)")
		planPGOK(t, err, "create generated canonical tenant policy")
	}
	args, err := parseCatalogSnapshotArgs(mediaInventoryTestArguments())
	require.NoError(t, err)
	org, account := args.organizationID, args.accountID
	_, err = owner.Exec(ctx, "INSERT INTO public.organizations(id) VALUES($1::uuid)", org)
	planPGOK(t, err, "insert generated inventory organization")
	_, err = owner.Exec(ctx, "INSERT INTO public.whatsapp_accounts(id,organization_id,name,phone_id,is_smb,status,access_token,app_secret) VALUES($1::uuid,$2::uuid,'private-account','private-phone',true,'active','private-credential','private-secret')", account, org)
	planPGOK(t, err, "insert generated inventory account")
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0700))
		args.privateManifest = filepath.Join(dir, "inventory.json")
	}
	return f, cfg, args
}

func mediaInventoryFixtureTables() []string {
	return []string{"organizations", "whatsapp_accounts", "messages", "scheduled_jobs", "contacts", "customer_activity_events", "channel_accounts", "inbox_conversations", "contact_identities", "whatsapp_coexistence_states", "inbound_events", "whatsapp_identity_review_holds", "whatsapp_identity_review_members"}
}

func mediaInventoryFixtureRows(t *testing.T, f *migrationPlanFixture) map[string][32]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(map[string][32]byte)
	for _, table := range mediaInventoryFixtureTables() {
		var raw string
		err := f.admin.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM public."+pgx.Identifier{table}.Sanitize()+" t").Scan(&raw)
		planPGOK(t, err, "fingerprint generated media rows")
		result[table] = sha256.Sum256([]byte(raw))
	}
	return result
}

func mediaInventoryFixtureMessage(t *testing.T, f *migrationPlanFixture, args catalogSnapshotArgs, at *time.Time, jobAt *time.Time) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	message, contact, job := uuid.New(), uuid.New(), uuid.New()
	_, err := f.admin.Exec(ctx, "INSERT INTO public.contacts(id,organization_id,phone_number,bsuid) VALUES($1,$2::uuid,'601122334455','')", contact, args.organizationID)
	planPGOK(t, err, "insert generated media contact")
	_, err = f.admin.Exec(ctx, `INSERT INTO public.messages(id,organization_id,created_at,ingested_at,whats_app_account,contact_id,whats_app_message_id,direction,message_type,content,media_url,media_mime_type,media_filename,metadata)
 VALUES($1,$2::uuid,'2026-01-01T12:00:00Z',$3,'private-account',$4,$5,'incoming','image','private-body','','image/jpeg','private-filename','{}')`, message, args.organizationID, at, contact, "private-wamid-"+message.String())
	planPGOK(t, err, "insert generated media message")
	if jobAt != nil {
		wamid := "private-wamid-" + message.String()
		account := uuid.MustParse(args.accountID)
		payload := map[string]any{"message_id": message.String(), "wamid": wamid, "phone_number_id": "private-phone", "message": map[string]any{"id": wamid, "from": "601122334455", "type": "image", "image": map[string]any{"id": "private-media-id"}}}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = f.admin.Exec(ctx, `INSERT INTO public.scheduled_jobs(id,organization_id,created_at,kind,aggregate_type,aggregate_id,status,attempts,max_attempts,idempotency_key,payload,version,last_error)
 VALUES($1,$2::uuid,$3,'inbound_message.continuation','message',$4,'completed',1,8,$5,$6::jsonb,1,'private-error')`, job, args.organizationID, jobAt, message, "inbound-message-continuation:"+uuid.NewSHA1(account, []byte(wamid)).String(), raw)
		planPGOK(t, err, "insert generated continuation proof")
	}
	return message
}

func mediaInventoryFixtureSnapshot(t *testing.T, cfg config.DatabaseConfig, args catalogSnapshotArgs) (database.MediaInventorySnapshot, error) {
	t.Helper()
	ctx := context.Background()
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	work, cancel := context.WithDeadline(ctx, d.workAt)
	defer cancel()
	parsed, err := pgx.ParseConfig(cfg.URL)
	require.NoError(t, err)
	observer, err := seedDiagnosticConfig(parsed, d, work)
	require.NoError(t, err)
	observer.RuntimeParams["application_name"] = database.MediaInventoryApplication
	from, _ := time.Parse(time.RFC3339, args.from)
	to, _ := time.Parse(time.RFC3339, args.to)
	return captureMediaInventory(work, d, observer, database.MediaInventoryScope{OrganizationID: args.organizationID, AccountID: args.accountID, From: from, To: to, Limit: args.mediaLimit})
}

func TestMediaInventoryPGArrivalCohortPrivacyAndNoWrites(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	inside := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	before := inside.Add(-24 * time.Hour)
	upper := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	first := mediaInventoryFixtureMessage(t, f, args, &inside, &inside)
	unknown := mediaInventoryFixtureMessage(t, f, args, nil, &inside)
	mediaInventoryFixtureMessage(t, f, args, &before, nil) // provider CreatedAt lies inside; server arrival does not.
	mediaInventoryFixtureMessage(t, f, args, &upper, nil)  // exclusive upper bound.
	fingerprints := mediaInventoryFixtureRows(t, f)
	snapshot, err := mediaInventoryFixtureSnapshot(t, cfg, args)
	planPGOK(t, err, "observe generated media cohort")
	require.Len(t, snapshot.Candidates, 2)
	for _, row := range snapshot.Rows {
		for _, excluded := range []string{"private-credential", "private-secret", "private-error"} {
			require.NotContains(t, string(row.Data), excluded, "credential/error columns must never enter the private snapshot")
		}
	}
	require.ElementsMatch(t, []string{first.String(), unknown.String()}, []string{snapshot.Candidates[0].ID, snapshot.Candidates[1].ID})
	public, private, err := classifyMediaInventory(context.Background(), snapshot)
	require.NoError(t, err)
	for _, secret := range []string{"private-credential", "private-secret", "private-error", "private-body", "private-filename", "private-phone", "private-wamid", "601122334455"} {
		require.NotContains(t, string(public), secret)
		require.NotContains(t, string(private), secret)
	}
	require.Contains(t, string(public), `"ordinary_blank_url_exact_continuation_proof":2`)
	require.Contains(t, string(private), `"message_ingestion_unknown":true`)
	require.Equal(t, fingerprints, mediaInventoryFixtureRows(t, f))
	if runtime.GOOS != "windows" {
		report, err := collectMediaInventory(context.Background(), cfg, args)
		planPGOK(t, err, "collect complete private inventory")
		require.NotContains(t, string(report), first.String())
		stat, err := os.Stat(args.privateManifest)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), stat.Mode().Perm())
		report, err = collectMediaInventory(context.Background(), cfg, args)
		require.Error(t, err)
		require.Nil(t, report)
	}
}

func TestMediaInventoryPGCapsAndQuerySpecificFailure(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mediaInventoryFixtureMessage(t, f, args, &at, &at)
	mediaInventoryFixtureMessage(t, f, args, &at, &at)
	args.mediaLimit = 1
	snapshot, err := mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	require.Empty(t, snapshot.Rows)
	args.mediaLimit = 100
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = f.admin.Exec(ctx, "UPDATE public.scheduled_jobs SET payload=payload||jsonb_build_object('oversize',repeat('x',65537))")
	planPGOK(t, err, "prepare oversized fixture proof")
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	require.Empty(t, snapshot.Rows)
	_, err = f.admin.Exec(ctx, `UPDATE public.scheduled_jobs SET payload=payload-'oversize';
 INSERT INTO public.scheduled_jobs(id,organization_id,created_at,kind,aggregate_type,aggregate_id,status,attempts,max_attempts,idempotency_key,payload,version)
 SELECT md5('inventory-fanout-'||n)::uuid,j.organization_id,j.created_at,j.kind,j.aggregate_type,j.aggregate_id,j.status,1,8,'fanout-'||n,j.payload,1
 FROM generate_series(1,817) n CROSS JOIN LATERAL (SELECT * FROM public.scheduled_jobs ORDER BY id LIMIT 1) j`)
	planPGOK(t, err, "prepare bounded proof fanout overflow")
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	require.Empty(t, snapshot.Rows)
	_, err = f.admin.Exec(ctx, `DELETE FROM public.scheduled_jobs WHERE idempotency_key LIKE 'fanout-%';
 INSERT INTO public.scheduled_jobs(id,organization_id,created_at,kind,aggregate_type,aggregate_id,status,attempts,max_attempts,idempotency_key,payload,version)
 SELECT md5('inventory-byte-fanout-'||n)::uuid,j.organization_id,j.created_at,j.kind,j.aggregate_type,j.aggregate_id,j.status,1,8,'byte-fanout-'||n,
 j.payload||jsonb_build_object('bounded_extra',repeat('x',40000)),1
 FROM generate_series(1,220) n CROSS JOIN LATERAL (SELECT * FROM public.scheduled_jobs ORDER BY id LIMIT 1) j`)
	planPGOK(t, err, "prepare many valid-sized proofs exceeding cumulative byte cap")
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err, "each row is below 64KiB and the row count is below 816, but cumulative proof must refuse")
	require.Empty(t, snapshot.Rows)
	_, err = f.admin.Exec(ctx, "DELETE FROM public.scheduled_jobs WHERE idempotency_key LIKE 'byte-fanout-%'; ALTER TABLE public.messages RENAME COLUMN media_url TO unavailable_media_url")
	planPGOK(t, err, "prepare query-specific fixture failure")
	var healthy int
	planPGOK(t, f.admin.QueryRow(ctx, "SELECT 1").Scan(&healthy), "healthy independent query")
	require.Equal(t, 1, healthy)
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	require.Empty(t, snapshot.Rows)
}

func TestMediaInventoryPGConflictingActivityIsObserved(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	message := mediaInventoryFixtureMessage(t, f, args, &at, &at)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, `INSERT INTO public.customer_activity_events(id,organization_id,contact_id,event_type,category,source_object_type,source_object_id,idempotency_key)
 VALUES($1,$2::uuid,$3,'unrelated.event','unrelated','message',$4,'unrelated-key')`, uuid.New(), args.organizationID, uuid.New(), message)
	planPGOK(t, err, "insert contradictory source-object claim with unrelated key and type")
	snapshot, err := mediaInventoryFixtureSnapshot(t, cfg, args)
	planPGOK(t, err, "observe contradictory activity source-object claim")
	public, private, err := classifyMediaInventory(context.Background(), snapshot)
	require.NoError(t, err)
	require.Contains(t, string(public), `"ordinary_blank_url_exact_continuation_proof":0`)
	require.Contains(t, string(public), `"ordinary_blank_url_unproven_or_conflicting":1`)
	require.Contains(t, string(private), `"reason":"conflicting_activity"`)
}

func TestMediaInventoryPGKeyOnlyContinuationConflictIsObserved(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	message := mediaInventoryFixtureMessage(t, f, args, &at, &at)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The production key constraint normally prevents this state. The native
	// diagnostic's positive claim must not depend on assuming that constraint.
	_, err := f.admin.Exec(ctx, `INSERT INTO public.scheduled_jobs(id,organization_id,created_at,kind,aggregate_type,aggregate_id,status,attempts,max_attempts,idempotency_key,payload,version)
 SELECT $1,organization_id,'2025-01-01T00:00:00Z','wrong.kind','message',$2,'completed',1,8,idempotency_key,'{}',1
 FROM public.scheduled_jobs WHERE aggregate_id=$3`, uuid.New(), uuid.New(), message)
	planPGOK(t, err, "insert key-only contradictory continuation claim")
	snapshot, err := mediaInventoryFixtureSnapshot(t, cfg, args)
	planPGOK(t, err, "observe key-only contradictory continuation claim")
	public, private, err := classifyMediaInventory(context.Background(), snapshot)
	require.NoError(t, err)
	require.Contains(t, string(public), `"ordinary_blank_url_exact_continuation_proof":0`)
	require.Contains(t, string(private), `"reason":"missing_or_ambiguous_continuation"`)
	// With only the rewritten key remaining, the observer cannot discover that
	// row by its payload/aggregate. It must still refuse exact provenance; an
	// undiscovered job is never represented as proof of absence or safe replay.
	_, err = f.admin.Exec(ctx, "DELETE FROM public.scheduled_jobs WHERE aggregate_id=$1", message)
	planPGOK(t, err, "leave sole rewritten key-only claim")
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	planPGOK(t, err, "observe message without an independently discovered valid proof")
	public, private, err = classifyMediaInventory(context.Background(), snapshot)
	require.NoError(t, err)
	require.Contains(t, string(public), `"ordinary_blank_url_exact_continuation_proof":0`)
	require.Contains(t, string(private), `"reason":"missing_or_ambiguous_continuation"`)
}

func TestMediaInventoryPGNativeVisibilityAndSameSnapshot(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mediaInventoryFixtureMessage(t, f, args, &at, &at)
	runtimeConfig := cfg
	runtimeConfig.URL = f.cfg.URL
	_, err := mediaInventoryFixtureSnapshot(t, runtimeConfig, args)
	require.Error(t, err, "configured runtime role cannot silently borrow migration owner")
	superuserConfig := cfg
	superuserConfig.URL = f.admin.Config().ConnString()
	_, err = mediaInventoryFixtureSnapshot(t, superuserConfig, args)
	require.Error(t, err, "native diagnostic refuses superuser")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = f.admin.Exec(ctx, "ALTER TABLE public.messages NO FORCE ROW LEVEL SECURITY")
	planPGOK(t, err, "remove fixture visibility proof")
	_, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	_, err = f.admin.Exec(ctx, "ALTER TABLE public.messages FORCE ROW LEVEL SECURITY")
	planPGOK(t, err, "restore fixture visibility proof")
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	work, done := context.WithDeadline(ctx, d.workAt)
	defer done()
	parsed, err := pgx.ParseConfig(cfg.URL)
	require.NoError(t, err)
	observer, err := seedDiagnosticConfig(parsed, d, work)
	require.NoError(t, err)
	observer.RuntimeParams["application_name"] = database.MediaInventoryApplication
	conn, tx, err := openPlanSession(work, observer)
	planPGOK(t, err, "open generated media snapshot")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	var count int
	planPGOK(t, tx.QueryRow(work, "SELECT count(*) FROM public.messages").Scan(&count), "establish repeatable snapshot")
	require.Equal(t, 1, count)
	mediaInventoryFixtureMessage(t, f, args, &at, &at) // separate connection commits after snapshot.
	from, _ := time.Parse(time.RFC3339, args.from)
	to, _ := time.Parse(time.RFC3339, args.to)
	scope := database.MediaInventoryScope{OrganizationID: args.organizationID, AccountID: args.accountID, From: from, To: to, Limit: 100}
	snapshot, err := database.ObserveMediaInventory(work, tx, scope)
	planPGOK(t, err, "observe stable physical snapshot")
	require.Len(t, snapshot.Candidates, 1)
	_, err = tx.Exec(work, "UPDATE public.messages SET media_url='must-not-write'")
	require.Error(t, err, "native inventory transaction is read-only")
}

func TestMediaInventoryPGLockCancellationAndCleanup(t *testing.T) {
	f, cfg, args := newMediaInventoryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := f.admin.Begin(ctx)
	planPGOK(t, err, "open fixture blocker")
	_, err = blocker.Exec(ctx, "LOCK TABLE public.messages IN ACCESS EXCLUSIVE MODE")
	planPGOK(t, err, "hold fixture table lock")
	start := time.Now()
	snapshot, err := mediaInventoryFixtureSnapshot(t, cfg, args)
	require.Error(t, err)
	require.Empty(t, snapshot.Rows)
	require.Less(t, time.Since(start), 17*time.Second)
	planPGOK(t, blocker.Rollback(ctx), "release fixture table lock")
	var sessions int
	planPGOK(t, f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1", database.MediaInventoryApplication).Scan(&sessions), "check diagnostic disposal")
	require.Zero(t, sessions)
	snapshot, err = mediaInventoryFixtureSnapshot(t, cfg, args)
	planPGOK(t, err, "observe after released lock")
	require.Empty(t, snapshot.Candidates)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	raw, err := collectMediaInventory(cancelled, cfg, args)
	require.Error(t, err)
	require.Nil(t, raw)
}
