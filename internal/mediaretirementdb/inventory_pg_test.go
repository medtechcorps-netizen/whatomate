package mediaretirementdb

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

const mediaFixtureOrg = "10000000-0000-0000-0000-000000000001"
const mediaFixtureAccount = "60000000-0000-0000-0000-000000000001"

// Fixture DDL is confined to a newly created database in the disposable,
// Unix-socket-only Build Cloud cluster. No host or application DSN is read.
type mediaGatewayFixture struct {
	admin              *pgx.Conn
	db, owner, runtime string
}

func mediaPGOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("synthetic media fixture operation failed: %v", err)
	}
}

func mediaFixtureConfig(t *testing.T, db, user string) *pgx.ConnConfig {
	t.Helper()
	c, err := pgx.ParseConfig("host=/tmp/pg-media-retirement port=5432 user=postgres dbname=media_retirement_fixture sslmode=disable")
	mediaPGOK(t, err)
	c.Database, c.User, c.Password = db, user, ""
	c.Fallbacks = nil
	c.ConnectTimeout = 3 * time.Second
	c.RuntimeParams = map[string]string{}
	return c
}

func newMediaGatewayFixture(t *testing.T) *mediaGatewayFixture {
	t.Helper()
	if os.Getenv("MEDIA_RETIREMENT_PG_FIXTURE") != "1" {
		t.Skip("isolated PostgreSQL fixture only")
	}
	if os.Getenv("PGHOST") != "/tmp/pg-media-retirement" || os.Getenv("PGDATABASE") != "media_retirement_fixture" {
		t.Fatal("refusing non-fixture environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cluster, err := pgx.ConnectConfig(ctx, mediaFixtureConfig(t, "media_retirement_fixture", "postgres"))
	mediaPGOK(t, err)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	f := &mediaGatewayFixture{db: "media_db_" + suffix, owner: "media_owner_" + suffix, runtime: "media_runtime_" + suffix}
	ownerCreated, runtimeCreated, databaseCreated := false, false, false
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if f.admin != nil {
			mediaPGOK(t, f.admin.Close(ctx))
		}
		if databaseCreated {
			_, err := cluster.Exec(ctx, "DROP DATABASE "+pgx.Identifier{f.db}.Sanitize()+" WITH (FORCE)")
			mediaPGOK(t, err)
		}
		if runtimeCreated {
			_, err := cluster.Exec(ctx, "DROP ROLE "+pgx.Identifier{f.runtime}.Sanitize())
			mediaPGOK(t, err)
		}
		if ownerCreated {
			_, err := cluster.Exec(ctx, "DROP ROLE "+pgx.Identifier{f.owner}.Sanitize())
			mediaPGOK(t, err)
		}
		mediaPGOK(t, cluster.Close(ctx))
	})
	for _, role := range []string{f.owner, f.runtime} {
		_, err := cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION")
		mediaPGOK(t, err)
		if role == f.owner {
			ownerCreated = true
		} else {
			runtimeCreated = true
		}
	}
	_, err = cluster.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{f.db}.Sanitize()+" OWNER "+pgx.Identifier{f.owner}.Sanitize())
	mediaPGOK(t, err)
	databaseCreated = true
	f.admin, err = pgx.ConnectConfig(ctx, mediaFixtureConfig(t, f.db, "postgres"))
	mediaPGOK(t, err)
	owner, err := pgx.ConnectConfig(ctx, mediaFixtureConfig(t, f.db, f.owner))
	mediaPGOK(t, err)
	defer func() { mediaPGOK(t, owner.Close(context.Background())) }()
	_, err = owner.Exec(ctx, mediaGatewayFixtureDDL)
	mediaPGOK(t, err)
	for _, table := range mediaInventoryTables[1:] {
		if table == "resellers" {
			continue
		}
		name := pgx.Identifier{"public", table}.Sanitize()
		_, err := owner.Exec(ctx, "ALTER TABLE "+name+" ENABLE ROW LEVEL SECURITY; ALTER TABLE "+name+" FORCE ROW LEVEL SECURITY; CREATE POLICY rereply_migration_access ON "+name+" TO "+pgx.Identifier{f.owner}.Sanitize()+" USING(true) WITH CHECK(true); CREATE POLICY rereply_tenant_isolation ON "+name+" TO "+pgx.Identifier{f.runtime}.Sanitize()+" USING(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid) WITH CHECK(organization_id=NULLIF(pg_catalog.current_setting('app.current_organization_id',true),'')::uuid)")
		mediaPGOK(t, err)
	}
	f.exec(t, "GRANT SELECT ON ALL TABLES IN SCHEMA public TO "+pgx.Identifier{f.runtime}.Sanitize())
	f.exec(t, "INSERT INTO public.organizations VALUES($1::uuid,now(),NULL,'{\"private\":\"DO_NOT_TRANSFER\"}');", mediaFixtureOrg)
	f.exec(t, "INSERT INTO public.whatsapp_accounts VALUES($1::uuid,$2::uuid,'current name','private-phone','connected',true,now(),NULL,'DO_NOT_TRANSFER')", mediaFixtureAccount, mediaFixtureOrg)
	return f
}

func (f *mediaGatewayFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, sql, args...)
	mediaPGOK(t, err)
}

func (f *mediaGatewayFixture) connection(t *testing.T, role string, change func(*pgx.ConnConfig)) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := mediaFixtureConfig(t, f.db, role)
	c.RuntimeParams = map[string]string{"application_name": MediaInventoryApplication, "search_path": "pg_catalog", "statement_timeout": "3s", "lock_timeout": "1s", "idle_in_transaction_session_timeout": "5s"}
	if change != nil {
		change(c)
	}
	conn, err := pgx.ConnectConfig(ctx, c)
	mediaPGOK(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		mediaPGOK(t, conn.Close(ctx))
	})
	return conn
}

func (f *mediaGatewayFixture) rows(t *testing.T, count int, width int) {
	f.exec(t, "INSERT INTO public.messages SELECT ('20000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,$1::uuid,now(),CASE WHEN i=1 THEN now() ELSE NULL END,'old unmatched name',repeat('k',$3::integer),'{}','DO_NOT_TRANSFER' FROM generate_series(1,$2::integer) i", mediaFixtureOrg, count, width)
}

func mediaCollect(t *testing.T, conn *pgx.Conn) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return CollectMediaReferenceInventory(ctx, conn, MediaInventoryScope{mediaFixtureOrg, mediaFixtureAccount})
}

func mediaRefused(t *testing.T, conn *pgx.Conn, raw []byte, err error) {
	t.Helper()
	if err != mediaretirement.ErrIncomplete || raw != nil || !conn.IsClosed() {
		t.Fatal("refusal must discard output and dispose the owned connection")
	}
}

func TestMediaGatewayPGCompleteSnapshotAndDisposal(t *testing.T) {
	f := newMediaGatewayFixture(t)
	f.rows(t, 100, 20)
	f.exec(t, "INSERT INTO public.messages VALUES('50000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000002',now(),NULL,'foreign','FOREIGN_REFERENCE','{}','DO_NOT_TRANSFER')")
	conn := f.connection(t, f.owner, nil)
	pid := conn.PgConn().PID()
	raw, err := mediaCollect(t, conn)
	mediaPGOK(t, err)
	var m mediaInventoryManifest
	mediaPGOK(t, json.Unmarshal(raw, &m))
	if !conn.IsClosed() || !m.SessionDisposed || !m.SameSnapshot || m.HistoricalAttributionComplete || m.Version != 2 || !m.GlobalProtection.Complete || m.GlobalProtection.ExactOverlapRequired || m.GlobalProtection.ResellerLogoRows != 0 || m.GlobalProtection.RecoveryCheckpointRows != 0 || len(m.CompleteFamilies) != 9 || len(m.Pages) != 10 || m.Counts[mediaretirement.Messages] != 100 || m.Counts[mediaretirement.OrganizationAudio] != 1 {
		t.Fatal("incomplete or overstated inventory")
	}
	for _, value := range []string{"DO_NOT_TRANSFER", "FOREIGN_REFERENCE", "private-phone"} {
		if strings.Contains(string(raw), value) {
			t.Fatal("unrequested content transferred")
		}
	}
	var first, terminal mediaretirement.Page
	mediaPGOK(t, json.Unmarshal(m.Pages[0], &first))
	mediaPGOK(t, json.Unmarshal(m.Pages[1], &terminal))
	if len(first.Records) != 100 || first.Records[0].DeletedAt == nil || first.Records[0].AccountName != "old unmatched name" || !first.More || terminal.More || len(terminal.Records) != 0 {
		t.Fatal("soft-deleted/history-name/exact-page terminal omitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var remaining int
	mediaPGOK(t, f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid=$1", pid).Scan(&remaining))
	if remaining != 0 {
		t.Fatal("backend survived manifest publication")
	}
}

func TestMediaGatewayPGRefusesIneligibleSession(t *testing.T) {
	cases := []struct{ name, role, param, value string }{
		{"runtime", "runtime", "", ""}, {"superuser", "postgres", "", ""}, {"wrong-application", "owner", "application_name", "other"},
		{"unsafe-search-path", "owner", "search_path", "public"}, {"unbounded-statement", "owner", "statement_timeout", "0"},
		{"unbounded-lock", "owner", "lock_timeout", "0"}, {"unbounded-idle", "owner", "idle_in_transaction_session_timeout", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaGatewayFixture(t)
			role := f.owner
			if tc.role == "runtime" {
				role = f.runtime
			}
			if tc.role == "postgres" {
				role = "postgres"
			}
			conn := f.connection(t, role, func(c *pgx.ConnConfig) {
				if tc.param != "" {
					c.RuntimeParams[tc.param] = tc.value
				}
			})
			raw, err := mediaCollect(t, conn)
			mediaRefused(t, conn, raw, err)
		})
	}
}

func TestMediaGatewayPGRefusesVisibilityAndLateMalformedPage(t *testing.T) {
	cases := []struct{ name, mutation string }{
		{"missing-owner-policy", "DROP POLICY rereply_migration_access ON public.messages"},
		{"changed-runtime-policy", "ALTER POLICY rereply_tenant_isolation ON public.message_parts USING(true)"},
		{"extra-policy", "CREATE POLICY extra ON public.call_logs USING(true)"},
		{"missing-table", "DROP TABLE public.catalog_products"},
		{"late-malformed-reference", `INSERT INTO public.inbound_events VALUES('30000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000001',now(),NULL,'40000000-0000-0000-0000-000000000001',NULL,'{"media_url":{"private":"DO_NOT_TRANSFER"}}','','','pending')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaGatewayFixture(t)
			f.rows(t, 101, 20)
			f.exec(t, tc.mutation)
			conn := f.connection(t, f.owner, nil)
			raw, err := mediaCollect(t, conn)
			mediaRefused(t, conn, raw, err)
		})
	}
}

type mediaGatewayTrace struct {
	match  string
	fired  bool
	action func()
}
type mediaGatewayTraceKey struct{}

func (trace *mediaGatewayTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, mediaGatewayTraceKey{}, strings.Contains(d.SQL, trace.match))
}
func (trace *mediaGatewayTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if match, _ := ctx.Value(mediaGatewayTraceKey{}).(bool); match && !trace.fired {
		trace.fired = true
		trace.action()
	}
}

func TestMediaGatewayPGCancellationAndExistingTransaction(t *testing.T) {
	for _, phase := range []string{"LOCK TABLE", "FROM public.messages", "FROM ONLY public.resellers", "rollback"} {
		t.Run(phase, func(t *testing.T) {
			f := newMediaGatewayFixture(t)
			f.rows(t, 101, 20)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			trace := &mediaGatewayTrace{match: phase, action: cancel}
			conn := f.connection(t, f.owner, func(c *pgx.ConnConfig) { c.Tracer = trace })
			raw, err := CollectMediaReferenceInventory(ctx, conn, MediaInventoryScope{mediaFixtureOrg, mediaFixtureAccount})
			if !trace.fired {
				t.Fatal("cancellation seam was not reached")
			}
			mediaRefused(t, conn, raw, err)
		})
	}
	t.Run("existing-writable-transaction", func(t *testing.T) {
		f := newMediaGatewayFixture(t)
		conn := f.connection(t, f.owner, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := conn.Begin(ctx)
		mediaPGOK(t, err)
		raw, err := mediaCollect(t, conn)
		mediaRefused(t, conn, raw, err)
	})
	t.Run("no-parent-deadline", func(t *testing.T) {
		f := newMediaGatewayFixture(t)
		conn := f.connection(t, f.owner, nil)
		raw, err := CollectMediaReferenceInventory(context.Background(), conn, MediaInventoryScope{mediaFixtureOrg, mediaFixtureAccount})
		mediaRefused(t, conn, raw, err)
	})
}

func TestMediaGatewayPGConcurrentHydrationStaysInSnapshot(t *testing.T) {
	f := newMediaGatewayFixture(t)
	f.rows(t, 101, 20)
	trace := &mediaGatewayTrace{match: "FROM public.messages", action: func() {
		f.exec(t, "UPDATE public.messages SET media_url='HYDRATED_AFTER_SNAPSHOT' WHERE id='20000000-0000-0000-0000-000000000101'")
		f.exec(t, "UPDATE public.whatsapp_accounts SET name='renamed after snapshot' WHERE id=$1::uuid", mediaFixtureAccount)
	}}
	conn := f.connection(t, f.owner, func(c *pgx.ConnConfig) { c.Tracer = trace })
	raw, err := mediaCollect(t, conn)
	mediaPGOK(t, err)
	if !trace.fired || strings.Contains(string(raw), "HYDRATED_AFTER_SNAPSHOT") || strings.Contains(string(raw), "renamed after snapshot") {
		t.Fatal("concurrent committed state mixed into repeatable snapshot")
	}
	var m mediaInventoryManifest
	mediaPGOK(t, json.Unmarshal(raw, &m))
	if m.Counts[mediaretirement.Messages] != 101 || m.Scope.AccountName != "current name" {
		t.Fatal("snapshot incomplete")
	}
	// Demonstrate why the read-only inventory cannot authorize a later mutation.
	var latest string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	mediaPGOK(t, f.admin.QueryRow(ctx, "SELECT media_url FROM public.messages WHERE id='20000000-0000-0000-0000-000000000101'").Scan(&latest))
	if latest != "HYDRATED_AFTER_SNAPSHOT" {
		t.Fatal("concurrent fixture mutation did not commit")
	}
}

func TestMediaGatewayPGTotalBudgetAndDDLBound(t *testing.T) {
	t.Run("aggregate-budget", func(t *testing.T) {
		f := newMediaGatewayFixture(t)
		f.rows(t, 1200, 8000)
		conn := f.connection(t, f.owner, nil)
		raw, err := mediaCollect(t, conn)
		mediaRefused(t, conn, raw, err)
	})
	t.Run("blocked-relation-lock", func(t *testing.T) {
		f := newMediaGatewayFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := f.admin.Begin(ctx)
		mediaPGOK(t, err)
		defer func() { mediaPGOK(t, tx.Rollback(context.Background())) }()
		_, err = tx.Exec(ctx, "LOCK TABLE public.messages IN ACCESS EXCLUSIVE MODE")
		mediaPGOK(t, err)
		conn := f.connection(t, f.owner, nil)
		start := time.Now()
		raw, err := mediaCollect(t, conn)
		mediaRefused(t, conn, raw, err)
		if time.Since(start) > 4*time.Second {
			t.Fatal("relation lock exceeded timeout/cleanup bound")
		}
	})
}

const mediaGatewayFixtureDDL = `CREATE TABLE public.messages(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whats_app_account text,media_url text,metadata jsonb,content text);
CREATE TABLE public.message_parts(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,message_id uuid,media_url text,storage_key text,provider_media_ref text,caption text);
CREATE TABLE public.bulk_message_campaigns(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whats_app_account text,header_media_local_path text,header_media_id text);
CREATE TABLE public.call_logs(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,recording_s3_key text);
CREATE TABLE public.inbound_events(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,channel_account_id uuid,review_hold_id uuid,payload jsonb,protocol text,event_type text,status text);
CREATE TABLE public.ivr_flows(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,menu jsonb,welcome_audio_url text);
CREATE TABLE public.organizations(id uuid PRIMARY KEY,updated_at timestamptz,deleted_at timestamptz,settings jsonb);
CREATE TABLE public.catalog_products(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,catalog_id uuid,image_url text);
CREATE TABLE public.contact_identities(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,channel_account_id uuid,contact_id uuid,avatar_url text);
CREATE TABLE public.whatsapp_accounts(id uuid PRIMARY KEY,organization_id uuid,name text,phone_id text,status text,is_smb boolean,updated_at timestamptz,deleted_at timestamptz,access_token text);
CREATE TABLE public.channel_accounts(id uuid PRIMARY KEY,organization_id uuid,channel text,provider text,external_account_id text,metadata jsonb,updated_at timestamptz,deleted_at timestamptz);
CREATE TABLE public.whatsapp_coexistence_states(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,version bigint,lifecycle_status text,updated_at timestamptz);
CREATE TABLE public.resellers(id uuid PRIMARY KEY,logo_url text,deleted_at timestamptz);
CREATE TABLE public.recovery_checkpoints(id uuid PRIMARY KEY,organization_id uuid,deleted_at timestamptz,storage_provider text,storage_bucket text,storage_object_key text,encryption_key_ref text);`

func TestMediaGatewayPGGlobalProtectionCountsWithoutPrivateValues(t *testing.T) {
	f := newMediaGatewayFixture(t)
	// Empty and NULL logos are unset; whitespace, active and deleted logos all
	// remain protected. Every checkpoint counts, even with no coordinates.
	f.exec(t, "INSERT INTO public.resellers VALUES ('80000000-0000-0000-0000-000000000001','',NULL),('80000000-0000-0000-0000-000000000002',NULL,NULL),('80000000-0000-0000-0000-000000000003','GLOBAL_BRANDING_DO_NOT_TRANSFER',NULL),('80000000-0000-0000-0000-000000000004','DELETED_BRANDING_DO_NOT_TRANSFER',now()),('80000000-0000-0000-0000-000000000005',' ',NULL)")
	f.exec(t, "INSERT INTO public.recovery_checkpoints VALUES ('90000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000002',now(),'PRIVATE_PROVIDER','PRIVATE_BUCKET','PRIVATE_BACKUP_KEY','PRIVATE_ENCRYPTION_REF'),('90000000-0000-0000-0000-000000000002',$1::uuid,NULL,NULL,NULL,NULL,NULL)", mediaFixtureOrg)
	conn := f.connection(t, f.owner, nil)
	raw, err := mediaCollect(t, conn)
	mediaPGOK(t, err)
	var m mediaInventoryManifest
	mediaPGOK(t, json.Unmarshal(raw, &m))
	if m.Version != 2 || !m.SessionDisposed || !m.GlobalProtection.Complete || !m.GlobalProtection.ExactOverlapRequired || m.GlobalProtection.ResellerLogoRows != 3 || m.GlobalProtection.RecoveryCheckpointRows != 2 {
		t.Fatal("global protected references omitted or permission overstated")
	}
	for _, value := range []string{"DO_NOT_TRANSFER", "PRIVATE_", "80000000-", "90000000-", "10000000-0000-0000-0000-000000000002"} {
		if strings.Contains(string(raw), value) {
			t.Fatal("global row identity, backup coordinates or private content transferred")
		}
	}
}

func TestMediaGatewayPGGlobalProtectionVisibilityRefusal(t *testing.T) {
	cases := []struct{ name, mutation string }{
		{"missing-resellers", "DROP TABLE public.resellers"},
		{"missing-recovery", "DROP TABLE public.recovery_checkpoints"},
		{"reseller-rls", "ALTER TABLE public.resellers ENABLE ROW LEVEL SECURITY"},
		{"recovery-owner-policy", "DROP POLICY rereply_migration_access ON public.recovery_checkpoints"},
		{"recovery-tenant-policy", "ALTER POLICY rereply_tenant_isolation ON public.recovery_checkpoints USING(true)"},
		{"recovery-extra-policy", "CREATE POLICY extra ON public.recovery_checkpoints USING(true)"},
		{"global-column-unavailable", "ALTER TABLE public.resellers DROP COLUMN logo_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaGatewayFixture(t)
			f.exec(t, tc.mutation)
			conn := f.connection(t, f.owner, nil)
			raw, err := mediaCollect(t, conn)
			mediaRefused(t, conn, raw, err)
		})
	}
}
