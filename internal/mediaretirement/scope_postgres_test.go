package mediaretirement

import (
	"encoding/json"
	"fmt"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"strings"
	"testing"
)

const scopeAccount = "60000000-0000-0000-0000-000000000001"
const scopeChannel = "70000000-0000-0000-0000-000000000001"

func scopeFixture(t *testing.T) {
	t.Helper()
	pgFixture(t)
	pg(t, `CREATE TABLE IF NOT EXISTS public.whatsapp_accounts(id uuid PRIMARY KEY,organization_id uuid,name text,phone_id text,status text,is_smb boolean,updated_at timestamptz,deleted_at timestamptz,access_token text);
CREATE TABLE IF NOT EXISTS public.channel_accounts(id uuid PRIMARY KEY,organization_id uuid,channel text,provider text,external_account_id text,metadata jsonb,updated_at timestamptz,deleted_at timestamptz);
CREATE TABLE IF NOT EXISTS public.whatsapp_coexistence_states(id uuid PRIMARY KEY,organization_id uuid,whats_app_account_id uuid,onboarding_cycle bigint,version bigint,lifecycle_status text,updated_at timestamptz);
TRUNCATE public.whatsapp_accounts,public.channel_accounts,public.whatsapp_coexistence_states;`)
	pg(t, fmt.Sprintf(`INSERT INTO public.organizations VALUES('%s','2026-11-03T00:00:00Z',NULL,'{"private":"DO_NOT_TRANSFER"}');
INSERT INTO public.whatsapp_accounts VALUES('%s','%s','current name','phone-id','connected',true,'2026-11-03T00:00:00Z',NULL,'DO_NOT_TRANSFER');
INSERT INTO public.channel_accounts VALUES('%s','%s','whatsapp','%s','legacy-account:%s','{"legacy_account_id":"%s","access_token":"DO_NOT_TRANSFER"}','2026-11-03T00:00:00Z',NULL);
INSERT INTO public.whatsapp_coexistence_states VALUES('80000000-0000-0000-0000-000000000001','%s','%s',1,2,'connected','2026-11-03T00:00:00Z');`, org, scopeAccount, org, scopeChannel, org, channelapi.LegacyMetaProvider, scopeAccount, scopeAccount, org, scopeAccount))
}

func scopeRaw(t *testing.T) []byte {
	t.Helper()
	return []byte(pg(t, fmt.Sprintf("PREPARE fixed_scope(uuid,uuid) AS %s; EXECUTE fixed_scope('%s','%s');", ScopeProjection().SQL, org, scopeAccount)))
}

func TestScopePostgresCurrentBindingsAndPrivateProjection(t *testing.T) {
	scopeFixture(t)
	raw := scopeRaw(t)
	if strings.Contains(string(raw), "DO_NOT_TRANSFER") || strings.Contains(string(raw), "phone-id") {
		t.Fatal("unrequested selector or credential content transferred")
	}
	w, err := DecodeScope(raw, org, scopeAccount)
	if err != nil || w.AccountName != "current name" || w.ChannelAccountID != scopeChannel {
		t.Fatalf("valid current scope refused: %s", raw)
	}
	pg(t, "UPDATE public.whatsapp_accounts SET name='renamed' WHERE id='"+scopeAccount+"'")
	renamed, err := DecodeScope(scopeRaw(t), org, scopeAccount)
	if err != nil || renamed.Digest == w.Digest || renamed.AccountName != "renamed" {
		t.Fatal("same-timestamp account rename was not bound")
	}
	pg(t, "UPDATE public.whatsapp_coexistence_states SET version=version+1")
	version, err := DecodeScope(scopeRaw(t), org, scopeAccount)
	if err != nil || version.Digest == renamed.Digest {
		t.Fatal("coexistence generation was not bound")
	}
	pg(t, "TRUNCATE public.channel_accounts,public.whatsapp_coexistence_states")
	absent, err := DecodeScope(scopeRaw(t), org, scopeAccount)
	if err != nil || absent.ChannelAccountID != "" || absent.Digest == version.Digest {
		t.Fatal("observed absence was not explicit in witness")
	}
}

func TestScopePostgresRefusesAmbiguousOrContradictoryClaims(t *testing.T) {
	cases := []struct{ name, mutation string }{
		{"deleted-name-claim", "INSERT INTO public.whatsapp_accounts SELECT '60000000-0000-0000-0000-000000000002',organization_id,name,phone_id,status,is_smb,updated_at,now(),access_token FROM public.whatsapp_accounts"},
		{"live-name-claim", "INSERT INTO public.whatsapp_accounts SELECT '60000000-0000-0000-0000-000000000002',organization_id,name,phone_id,status,is_smb,updated_at,NULL,access_token FROM public.whatsapp_accounts"},
		{"selected-account-deleted", "UPDATE public.whatsapp_accounts SET deleted_at=now()"},
		{"wrong-account-organization", "UPDATE public.whatsapp_accounts SET organization_id='10000000-0000-0000-0000-000000000002'"},
		{"organization-deleted", "UPDATE public.organizations SET deleted_at=now()"},
		{"channel-missing-selector", "UPDATE public.channel_accounts SET metadata='{}'"},
		{"channel-object-selector", `UPDATE public.channel_accounts SET metadata='{"legacy_account_id":{"content":"DO_NOT_TRANSFER"}}'`},
		{"channel-wrong-external", "UPDATE public.channel_accounts SET external_account_id='wrong'"},
		{"channel-nonlegacy-meta-provider", "UPDATE public.channel_accounts SET provider='meta'"},
		{"channel-wrong-provider", "UPDATE public.channel_accounts SET provider='other'"},
		{"channel-wrong-organization", "UPDATE public.channel_accounts SET organization_id='10000000-0000-0000-0000-000000000002'"},
		{"channel-deleted", "UPDATE public.channel_accounts SET deleted_at=now()"},
		{"duplicate-channel", "INSERT INTO public.channel_accounts SELECT '70000000-0000-0000-0000-000000000002',organization_id,channel,provider,external_account_id,metadata,updated_at,now() FROM public.channel_accounts"},
		{"state-wrong-organization", "UPDATE public.whatsapp_coexistence_states SET organization_id='10000000-0000-0000-0000-000000000002'"},
		{"state-invalid-generation", "UPDATE public.whatsapp_coexistence_states SET version=0"},
		{"state-invalid-lifecycle", "UPDATE public.whatsapp_coexistence_states SET lifecycle_status=NULL"},
		{"state-duplicate", "INSERT INTO public.whatsapp_coexistence_states SELECT '80000000-0000-0000-0000-000000000002',organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status,updated_at FROM public.whatsapp_coexistence_states"},
		{"oversized-name", "UPDATE public.whatsapp_accounts SET name=repeat('x',401)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scopeFixture(t)
			pg(t, tc.mutation)
			raw := scopeRaw(t)
			if len(raw) != 0 {
				t.Fatalf("server did not refuse complete witness: %s", raw)
			}
			if w, err := DecodeScope(raw, org, scopeAccount); err != ErrIncomplete || w != (ScopeWitness{}) {
				t.Fatal("refusal returned a partial witness")
			}
		})
	}
}

func TestScopeDecoderRejectsMalformedAndHistoricalCompleteness(t *testing.T) {
	good := map[string]any{"version": 1, "organization_id": org, "account_id": scopeAccount, "account_name": "current name", "channel_account_id": scopeChannel, "digest": strings.Repeat("a", 64), "historical_attribution_complete": false}
	for _, key := range []string{"version", "organization_id", "account_id", "account_name", "channel_account_id", "digest", "historical_attribution_complete"} {
		t.Run("null-"+key, func(t *testing.T) {
			copy := make(map[string]any)
			for k, v := range good {
				copy[k] = v
			}
			copy[key] = nil
			raw, _ := json.Marshal(copy)
			if w, err := DecodeScope(raw, org, scopeAccount); err != ErrIncomplete || w != (ScopeWitness{}) {
				t.Fatal("null accepted")
			}
		})
	}
	raw, _ := json.Marshal(good)
	if _, err := DecodeScope(raw, org, scopeAccount); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(raw), `"historical_attribution_complete":false`, `"historical_attribution_complete":true`, 1)),
		[]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)),
		[]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"credentials":"DO_NOT_TRANSFER"`, 1)),
		append(raw, []byte(`{}`)...),
		[]byte(strings.Repeat(" ", 2049)),
	} {
		if w, err := DecodeScope(bad, org, scopeAccount); err != ErrIncomplete || w != (ScopeWitness{}) {
			t.Fatal("malformed/overstated scope accepted")
		}
	}
}
