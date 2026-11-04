package mediaretirement

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ScopeWitness binds current selectors only. It cannot prove which account
// owned a legacy name when an older attachment was created. Use it only inside
// an owner-validated physical snapshot; no connection admission happens here.
type ScopeWitness struct {
	OrganizationID   string `json:"organization_id"`
	AccountID        string `json:"account_id"`
	AccountName      string `json:"account_name"`
	ChannelAccountID string `json:"channel_account_id"`
	Digest           string `json:"digest"`
}

// ScopeProjection binds organization/account UUIDs, the current legacy name,
// all same-name claims (including deleted claims), exact channel bridge and
// any coexistence generation. Every cohort is limited to two: ambiguity
// refuses the complete result. Foreign-organization bridge/state claims also
// refuse. Only the fixed, bounded selector envelope leaves PostgreSQL.
func ScopeProjection() Plan {
	return Plan{Tables: []string{"organizations", "whatsapp_accounts", "channel_accounts", "whatsapp_coexistence_states"}, SQL: `WITH
 o AS MATERIALIZED (SELECT id,updated_at,deleted_at FROM public.organizations WHERE id=$1::uuid LIMIT 2),
 a AS MATERIALIZED (SELECT id,organization_id,name,phone_id,status,is_smb,updated_at,deleted_at FROM public.whatsapp_accounts WHERE id=$2::uuid LIMIT 2),
 n AS MATERIALIZED (SELECT id,organization_id,name,updated_at,deleted_at FROM public.whatsapp_accounts WHERE organization_id=$1::uuid AND name=(SELECT name FROM a) ORDER BY id LIMIT 2),
 c AS MATERIALIZED (SELECT id,organization_id,channel,provider,external_account_id,metadata->'legacy_account_id' AS legacy_account_id,updated_at,deleted_at
 FROM public.channel_accounts WHERE external_account_id='legacy-account:'||$2::text OR metadata->>'legacy_account_id'=$2::text ORDER BY id LIMIT 2),
 s AS MATERIALIZED (SELECT id,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status,updated_at
 FROM public.whatsapp_coexistence_states WHERE whats_app_account_id=$2::uuid ORDER BY id LIMIT 2),
 guard AS (SELECT (SELECT count(*) FROM o)=1 AND (SELECT count(*) FROM a)=1 AND (SELECT count(*) FROM n)=1
 AND (SELECT count(*) FROM c)<=1 AND (SELECT count(*) FROM s)<=1
 AND EXISTS(SELECT 1 FROM o WHERE deleted_at IS NULL AND updated_at IS NOT NULL)
 AND EXISTS(SELECT 1 FROM a WHERE organization_id=$1::uuid AND deleted_at IS NULL AND updated_at IS NOT NULL
 AND name IS NOT NULL AND octet_length(name) BETWEEN 1 AND 400 AND btrim(name)<>''
 AND phone_id IS NOT NULL AND octet_length(phone_id)<=255 AND status IS NOT NULL AND octet_length(status)<=64 AND is_smb IS NOT NULL)
 AND EXISTS(SELECT 1 FROM n WHERE id=$2::uuid AND deleted_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM c WHERE id='00000000-0000-0000-0000-000000000000'::uuid
 OR organization_id IS DISTINCT FROM $1::uuid OR channel IS DISTINCT FROM 'whatsapp' OR provider IS DISTINCT FROM 'meta_legacy'
 OR external_account_id IS DISTINCT FROM 'legacy-account:'||$2::text OR legacy_account_id IS DISTINCT FROM pg_catalog.to_jsonb($2::text)
 OR updated_at IS NULL OR deleted_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM s WHERE id='00000000-0000-0000-0000-000000000000'::uuid OR organization_id IS DISTINCT FROM $1::uuid
 OR onboarding_cycle IS NULL OR onboarding_cycle<=0 OR version IS NULL OR version<=0 OR updated_at IS NULL
 OR lifecycle_status IS NULL OR lifecycle_status NOT IN ('unknown','connected','disconnected','offboarded')) AS ok),
 witness AS (SELECT CASE WHEN ok THEN pg_catalog.jsonb_build_object('version',1,'organization_id',$1::text,'account_id',$2::text,
 'account_name',(SELECT name FROM a),'channel_account_id',COALESCE((SELECT id::text FROM c),''),'historical_attribution_complete',false,
 'digest',pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(pg_catalog.jsonb_build_array(
 (SELECT pg_catalog.to_jsonb(o) FROM o),(SELECT pg_catalog.to_jsonb(a) FROM a),(SELECT pg_catalog.to_jsonb(n) FROM n),
 (SELECT pg_catalog.to_jsonb(c) FROM c),(SELECT pg_catalog.to_jsonb(s) FROM s))::text,'UTF8')),'hex')) END AS body FROM guard)
 SELECT CASE WHEN pg_catalog.octet_length(body::text)<=2048 THEN body::text END FROM witness`}
}

// DecodeScope returns one fixed error and no partial selector on any refusal.
// Digest equality is meaningful only in the caller's admitted snapshot;
// mutation requires a new transaction with fresh concurrency checks.
func DecodeScope(raw []byte, organization, account string) (ScopeWitness, error) {
	var empty ScopeWitness
	if !uuidOK(organization) || !uuidOK(account) || len(raw) == 0 || len(raw) > 2048 || !utf8.Valid(raw) || !uniqueJSON(raw) ||
		!exactObject(raw, "version", "organization_id", "account_id", "account_name", "channel_account_id", "digest", "historical_attribution_complete") {
		return empty, ErrIncomplete
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !bytes.Equal(bytes.TrimSpace(fields["version"]), []byte("1")) ||
		!bytes.Equal(bytes.TrimSpace(fields["historical_attribution_complete"]), []byte("false")) {
		return empty, ErrIncomplete
	}
	for _, key := range []string{"organization_id", "account_id", "account_name", "channel_account_id", "digest"} {
		if !stringJSON(fields[key]) {
			return empty, ErrIncomplete
		}
	}
	var w ScopeWitness
	if json.Unmarshal(raw, &w) != nil || w.OrganizationID != organization || w.AccountID != account ||
		len(w.AccountName) > 400 || strings.TrimSpace(w.AccountName) == "" || strings.ContainsRune(w.AccountName, 0) ||
		(w.ChannelAccountID != "" && !uuidOK(w.ChannelAccountID)) || len(w.Digest) != 64 || strings.Trim(w.Digest, "0123456789abcdef") != "" {
		return empty, ErrIncomplete
	}
	return w, nil
}
