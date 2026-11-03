package mediaretirement

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These fixtures run only in the disposable, Unix-socket-only Build Cloud
// database. They do not borrow application/database credentials from the host.
func pgFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("MEDIA_RETIREMENT_PG_FIXTURE") != "1" {
		t.Skip("isolated PostgreSQL fixture only")
	}
	if os.Getenv("PGHOST") != "/tmp/pg-media-retirement" || os.Getenv("PGDATABASE") != "media_retirement_fixture" {
		t.Fatal("refusing non-fixture database")
	}
	if pg(t, "SELECT current_database() || ':' || (current_setting('server_version_num')::int/10000)::text") != "media_retirement_fixture:17" {
		t.Fatal("wrong fixture database")
	}
	pg(t, `CREATE TABLE IF NOT EXISTS public.messages(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,media_url text,metadata jsonb,content text);
CREATE TABLE IF NOT EXISTS public.message_parts(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,message_id uuid,media_url text,storage_key text,provider_media_ref text,caption text);
CREATE TABLE IF NOT EXISTS public.bulk_message_campaigns(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,header_media_local_path text,header_media_id text);
CREATE TABLE IF NOT EXISTS public.call_logs(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,recording_s3_key text);
CREATE TABLE IF NOT EXISTS public.inbound_events(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,channel_account_id uuid,review_hold_id uuid,payload jsonb,protocol text,event_type text,status text);
CREATE TABLE IF NOT EXISTS public.ivr_flows(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,whatsapp_account text,menu jsonb,welcome_audio_url text);
CREATE TABLE IF NOT EXISTS public.organizations(id uuid PRIMARY KEY,updated_at timestamptz,deleted_at timestamptz,settings jsonb);
CREATE TABLE IF NOT EXISTS public.catalog_products(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,catalog_id uuid,image_url text);
CREATE TABLE IF NOT EXISTS public.contact_identities(id uuid PRIMARY KEY,organization_id uuid,updated_at timestamptz,deleted_at timestamptz,channel_account_id uuid,contact_id uuid,avatar_url text);
TRUNCATE public.messages,public.message_parts,public.bulk_message_campaigns,public.call_logs,public.inbound_events,public.ivr_flows,public.organizations,public.catalog_products,public.contact_identities;`)
}

func pg(t *testing.T, sql string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture SQL failed: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func pgPage(t *testing.T, f Family, cursor string, budget int) []byte {
	t.Helper()
	p, err := Projection(f)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "" && !uuidOK(cursor) {
		t.Fatal("bad fixture cursor")
	}
	return []byte(pg(t, fmt.Sprintf("PREPARE fixed_page(uuid,text,bigint) AS %s; EXECUTE fixed_page('%s','%s',%d);", p.SQL, org, cursor, budget)))
}

func TestPagePostgresNineFamiliesExcludeContent(t *testing.T) {
	pgFixture(t)
	const id = "20000000-0000-0000-0000-000000000001"
	const channel = "30000000-0000-0000-0000-000000000001"
	const parent = "40000000-0000-0000-0000-000000000001"
	common := fmt.Sprintf("'%s','%s','2026-11-03T00:00:00Z','2026-11-03T01:00:00Z'", id, org)
	pg(t, "INSERT INTO public.messages VALUES("+common+`, 'old name','opaque +%/reference','{"coexistence_media_id":"provider-id","access_token":"DO_NOT_TRANSFER"}','DO_NOT_TRANSFER');`+
		"INSERT INTO public.message_parts VALUES("+common+",'"+parent+`','opaque +%/reference','organizations/key','provider-id','DO_NOT_TRANSFER');`+
		"INSERT INTO public.bulk_message_campaigns VALUES("+common+`, 'old name','legacy/preview','provider-id');`+
		"INSERT INTO public.call_logs VALUES("+common+`, 'old name','recordings/key.ogg');`+
		"INSERT INTO public.inbound_events VALUES("+common+",'"+channel+"','"+parent+`','{"media_url":"opaque +%/reference","media_generation":18446744073709551615,"content":"DO_NOT_TRANSFER"}','whatsapp_identity_review_v1','review_pending','pending');`+
		"INSERT INTO public.ivr_flows VALUES("+common+`, 'old name','{"nodes":[{"config":{"audio_file":"local.wav","content":"DO_NOT_TRANSFER"}},{"config":{}}]}','welcome-url');`+
		"INSERT INTO public.organizations VALUES('"+org+`','2026-11-03T00:00:00Z',NULL,'{"hold_music_file":"hold.wav","ringback_file":"ring.wav","access_token":"DO_NOT_TRANSFER"}');`+
		"INSERT INTO public.catalog_products VALUES("+common+",'"+parent+`','artwork-url');`+
		"INSERT INTO public.contact_identities VALUES("+common+",'"+channel+"','"+parent+`','avatar-url');`)
	for _, f := range []Family{Messages, MessageParts, CampaignPreviews, Recordings, InboundMedia, IVRAudio, OrganizationAudio, CatalogArtwork, IdentityAvatars} {
		t.Run(string(f), func(t *testing.T) {
			raw := pgPage(t, f, "", MaxPrivateBytes)
			if strings.Contains(string(raw), "DO_NOT_TRANSFER") {
				t.Fatal("unrequested content transferred")
			}
			page, err := DecodePage(raw, f, org, "", MaxPrivateBytes)
			if err != nil || len(page.Records) != 1 || page.More {
				t.Fatalf("native projection refused: %s", raw)
			}
			if f != OrganizationAudio && page.Records[0].DeletedAt == nil {
				t.Fatal("soft-deleted row missing")
			}
		})
	}
}

func TestPagePostgresScopePaginationAndBounds(t *testing.T) {
	pgFixture(t)
	pg(t, "INSERT INTO public.messages SELECT ('20000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,'"+org+`',now(),CASE WHEN i=1 THEN now() ELSE NULL END,'old name','organizations/key','{}','DO_NOT_TRANSFER' FROM generate_series(1,101) i;
INSERT INTO public.messages VALUES('50000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000002',now(),NULL,'other account','other-key','{}','DO_NOT_TRANSFER');`)
	first, err := DecodePage(pgPage(t, Messages, "", MaxPrivateBytes), Messages, org, "", MaxPrivateBytes)
	if err != nil || len(first.Records) != 100 || !first.More {
		t.Fatal("wrong native first page")
	}
	second, err := DecodePage(pgPage(t, Messages, first.LastID, MaxPrivateBytes), Messages, org, first.LastID, MaxPrivateBytes)
	if err != nil || len(second.Records) != 1 || second.More {
		t.Fatal("wrong native continuation")
	}
	last, err := DecodePage(pgPage(t, Messages, second.LastID, MaxPrivateBytes), Messages, org, second.LastID, MaxPrivateBytes)
	if err != nil || len(last.Records) != 0 || last.More {
		t.Fatal("other organization leaked or terminal failed")
	}
	var rejected Page
	if err := json.Unmarshal(pgPage(t, Messages, "", 4096), &rejected); err != nil || !rejected.Rejected || len(rejected.Records) != 0 {
		t.Fatal("server total-byte limit failed")
	}
	pg(t, "TRUNCATE public.messages; INSERT INTO public.messages VALUES('20000000-0000-0000-0000-000000000001','"+org+`',now(),NULL,'old name',repeat('x',17000),'{}','DO_NOT_TRANSFER');`)
	if err := json.Unmarshal(pgPage(t, Messages, "", MaxPrivateBytes), &rejected); err != nil || !rejected.Rejected || len(rejected.Records) != 0 {
		t.Fatal("server row-byte limit failed")
	}
}

func TestPagePostgresRejectsMalformedValuesBeforeTransfer(t *testing.T) {
	pgFixture(t)
	common := "'20000000-0000-0000-0000-000000000001','" + org + "',now(),NULL"
	cases := []struct {
		name   string
		family Family
		insert string
	}{
		{"message-object", Messages, "INSERT INTO public.messages VALUES(" + common + `,'name','key','{"coexistence_media_id":{"content":"DO_NOT_TRANSFER"}}',NULL)`},
		{"message-root-array", Messages, "INSERT INTO public.messages VALUES(" + common + `,'name','key','["DO_NOT_TRANSFER"]',NULL)`},
		{"inbound-object", InboundMedia, "INSERT INTO public.inbound_events VALUES(" + common + `,'30000000-0000-0000-0000-000000000001',NULL,'{"media_url":{"content":"DO_NOT_TRANSFER"}}','','','pending')`},
		{"organization-object", OrganizationAudio, "INSERT INTO public.organizations VALUES('" + org + `',now(),NULL,'{"hold_music_file":{"content":"DO_NOT_TRANSFER"}}')`},
		{"ivr-audio-object", IVRAudio, "INSERT INTO public.ivr_flows VALUES(" + common + `,'name','{"nodes":[{"config":{"audio_file":{"content":"DO_NOT_TRANSFER"}}}]}','url')`},
		{"ivr-nodes-scalar", IVRAudio, "INSERT INTO public.ivr_flows VALUES(" + common + `,'name','{"nodes":"DO_NOT_TRANSFER"}','url')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := Projection(c.family)
			pg(t, "TRUNCATE public."+p.Tables[0]+"; "+c.insert)
			raw := pgPage(t, c.family, "", MaxPrivateBytes)
			if strings.Contains(string(raw), "DO_NOT_TRANSFER") {
				t.Fatal("malformed reference content transferred")
			}
			var page Page
			if err := json.Unmarshal(raw, &page); err != nil || !page.Rejected || len(page.Records) != 0 {
				t.Fatalf("server did not refuse malformed projection: %s", raw)
			}
		})
	}
}
