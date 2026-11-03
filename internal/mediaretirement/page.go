// Package mediaretirement defines private, read-only attachment projections.
// It does not authorize SQL execution, account attribution, or any deletion.
package mediaretirement

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	PageRows        = 100
	RowBytes        = 16 * 1024
	PageBytes       = 1024 * 1024
	MaxPrivateBytes = 8 * 1024 * 1024
)

type Family string

const (
	Messages          Family = "messages"
	MessageParts      Family = "message_parts"
	CampaignPreviews  Family = "campaign_previews"
	Recordings        Family = "recordings"
	InboundMedia      Family = "inbound_media"
	IVRAudio          Family = "ivr_audio"
	OrganizationAudio Family = "organization_audio"
	CatalogArtwork    Family = "catalog_artwork"
	IdentityAvatars   Family = "identity_avatars"
)

var ErrIncomplete = errors.New("media reference inventory incomplete")

// Plan contains a closed SQL statement and the relations whose native owner,
// RLS policy/dependency and all-row visibility must be proved before execution.
// Arguments are organization UUID, exclusive cursor UUID (or empty), and the
// remaining private-byte budget. Queries include soft-deleted rows and every
// account in the selected organization. They do not infer historical account
// attribution from the current account name.
type Plan struct {
	SQL    string
	Tables []string
}

type Record struct {
	ID               string                     `json:"id"`
	OrganizationID   string                     `json:"organization_id"`
	AccountName      string                     `json:"account_name"`
	ChannelAccountID string                     `json:"channel_account_id"`
	ParentID         string                     `json:"parent_id"`
	UpdatedAt        string                     `json:"updated_at"`
	DeletedAt        *string                    `json:"deleted_at"`
	Protected        bool                       `json:"protected"`
	Fields           map[string]json.RawMessage `json:"fields"`
}

type Page struct {
	Version  int      `json:"version"`
	Family   Family   `json:"family"`
	Records  []Record `json:"records"`
	More     bool     `json:"more"`
	LastID   string   `json:"last_id"`
	Rejected bool     `json:"rejected"`
}

type projection struct {
	table, account, channel, parent, fields, shape string
	protected                                      bool
	fieldTypes                                     map[string]string
}

func definition(f Family) (projection, bool) {
	p := projection{account: "''", channel: "''", parent: "''", shape: "true"}
	switch f {
	case Messages:
		p.table = "messages"
		p.account = "COALESCE(t.whats_app_account,'')"
		p.shape = "COALESCE(pg_catalog.jsonb_typeof(t.metadata)='object',false)"
		p.fields = "'media_url',t.media_url,'provider_media_id',t.metadata->'coexistence_media_id','hydrated_media_id',t.metadata->'coexistence_media_hydrated_id','revoked',t.metadata->'coexistence_revoked'"
		p.fieldTypes = map[string]string{"media_url": "string", "provider_media_id": "string", "hydrated_media_id": "string", "revoked": "boolean"}
	case MessageParts:
		p.table = "message_parts"
		p.parent = "t.message_id::text"
		p.fields = "'media_url',t.media_url,'storage_key',t.storage_key,'provider_media_ref',t.provider_media_ref"
		p.fieldTypes = map[string]string{"media_url": "string", "storage_key": "string", "provider_media_ref": "string"}
	case CampaignPreviews:
		p.table = "bulk_message_campaigns"
		p.account = "COALESCE(t.whats_app_account,'')"
		p.fields = "'preview_path',t.header_media_local_path,'provider_media_id',t.header_media_id"
		p.fieldTypes = map[string]string{"preview_path": "string", "provider_media_id": "string"}
	case Recordings:
		p.table = "call_logs"
		p.account = "COALESCE(t.whatsapp_account,'')"
		p.protected = true
		p.fields = "'recording_key',t.recording_s3_key"
		p.fieldTypes = map[string]string{"recording_key": "string"}
	case InboundMedia:
		p.table = "inbound_events"
		p.channel = "t.channel_account_id::text"
		p.parent = "COALESCE(t.review_hold_id::text,'')"
		p.protected = true
		p.shape = "COALESCE(pg_catalog.jsonb_typeof(t.payload)='object',false)"
		p.fields = "'media_url',t.payload->'media_url','provider_media_id',t.payload->'media_id','hydrated_media_id',t.payload->'media_hydrated_id','media_status',t.payload->'media_status','media_generation',t.payload->'media_generation','media_revision',t.payload->'media_revision','revoked',t.payload->'media_revoked','protocol',t.protocol,'event_type',t.event_type,'status',t.status"
		p.fieldTypes = map[string]string{"media_url": "string", "provider_media_id": "string", "hydrated_media_id": "string", "media_status": "string", "media_generation": "number", "media_revision": "string", "revoked": "boolean", "protocol": "string", "event_type": "string", "status": "string"}
	case IVRAudio:
		p.table = "ivr_flows"
		p.account = "COALESCE(t.whatsapp_account,'')"
		p.protected = true
		// Every present node/config must be an object; unexpected shapes refuse
		// the page instead of silently hiding an audio reference.
		p.shape = "COALESCE(pg_catalog.jsonb_typeof(t.menu)='object',false) AND CASE WHEN t.menu->'nodes' IS NULL THEN true WHEN pg_catalog.jsonb_typeof(t.menu->'nodes')='array' THEN pg_catalog.jsonb_array_length(t.menu->'nodes')<=256 AND NOT EXISTS(SELECT 1 FROM pg_catalog.jsonb_array_elements(t.menu->'nodes') n WHERE pg_catalog.jsonb_typeof(n)<>'object' OR (n->'config' IS NOT NULL AND pg_catalog.jsonb_typeof(n->'config')<>'object')) ELSE false END"
		p.fields = "'welcome_audio_url',t.welcome_audio_url,'local_audio_files',CASE WHEN pg_catalog.jsonb_typeof(t.menu->'nodes')='array' AND pg_catalog.jsonb_array_length(t.menu->'nodes')<=256 THEN (SELECT COALESCE(pg_catalog.jsonb_agg(n->'config'->'audio_file' ORDER BY ord),'[]'::jsonb) FROM pg_catalog.jsonb_array_elements(t.menu->'nodes') WITH ORDINALITY AS nodes(n,ord)) ELSE '[]'::jsonb END"
		p.fieldTypes = map[string]string{"welcome_audio_url": "string", "local_audio_files": "strings"}
	case OrganizationAudio:
		p.table = "organizations"
		p.protected = true
		p.shape = "COALESCE(pg_catalog.jsonb_typeof(t.settings)='object',false)"
		p.fields = "'hold_music_file',t.settings->'hold_music_file','ringback_file',t.settings->'ringback_file'"
		p.fieldTypes = map[string]string{"hold_music_file": "string", "ringback_file": "string"}
	case CatalogArtwork:
		p.table = "catalog_products"
		p.parent = "t.catalog_id::text"
		p.protected = true
		p.fields = "'image_url',t.image_url"
		p.fieldTypes = map[string]string{"image_url": "string"}
	case IdentityAvatars:
		p.table = "contact_identities"
		p.channel = "t.channel_account_id::text"
		p.parent = "t.contact_id::text"
		p.protected = true
		p.fields = "'avatar_url',t.avatar_url"
		p.fieldTypes = map[string]string{"avatar_url": "string"}
	default:
		return projection{}, false
	}
	return p, true
}

// Projection returns only closed, parameterized SELECT statements. It projects
// no content/caption/headers or whole metadata/settings/payload objects. Rows
// without references are retained for complete pagination and shape checks.
// Global reseller branding and recovery-service overlap require separate
// protection checks; these nine families are not provider-wide closure proof.
func Projection(f Family) (Plan, error) {
	p, ok := definition(f)
	if !ok {
		return Plan{}, ErrIncomplete
	}
	// Reject malformed selected JSON values on the server before transferring
	// any reference page. Client-side rejection alone is too late to prevent
	// an unexpected object from carrying content in a reference-shaped field.
	switch f {
	case Messages:
		for _, key := range []string{"coexistence_media_id", "coexistence_media_hydrated_id"} {
			p.shape += " AND " + nullableJSONType("t.metadata", key, "string")
		}
		p.shape += " AND " + nullableJSONType("t.metadata", "coexistence_revoked", "boolean")
	case InboundMedia:
		for _, key := range []string{"media_url", "media_id", "media_hydrated_id", "media_status", "media_revision"} {
			p.shape += " AND " + nullableJSONType("t.payload", key, "string")
		}
		p.shape += " AND " + nullableJSONType("t.payload", "media_generation", "number")
		p.shape += " AND " + nullableJSONType("t.payload", "media_revoked", "boolean")
	case OrganizationAudio:
		for _, key := range []string{"hold_music_file", "ringback_file"} {
			p.shape += " AND " + nullableJSONType("t.settings", key, "string")
		}
	case IVRAudio:
		p.shape += " AND CASE WHEN t.menu->'nodes' IS NULL THEN true WHEN pg_catalog.jsonb_typeof(t.menu->'nodes')<>'array' THEN false WHEN pg_catalog.jsonb_array_length(t.menu->'nodes')>256 THEN false ELSE NOT EXISTS(SELECT 1 FROM pg_catalog.jsonb_array_elements(t.menu->'nodes') n WHERE n->'config'->'audio_file' IS NOT NULL AND pg_catalog.jsonb_typeof(n->'config'->'audio_file') NOT IN ('string','null')) END"
	}
	org := "t.organization_id"
	if f == OrganizationAudio {
		org = "t.id"
	}
	protected := "false"
	if p.protected {
		protected = "true"
	}
	sql := `WITH page AS MATERIALIZED (
 SELECT t.id, (` + p.shape + `) AS shape_ok,
 pg_catalog.jsonb_build_object('id',t.id,'organization_id',` + org + `,
 'account_name',` + p.account + `,'channel_account_id',` + p.channel + `,'parent_id',` + p.parent + `,
 'updated_at',t.updated_at,'deleted_at',t.deleted_at,'protected',` + protected + `,
 'fields',CASE WHEN (` + p.shape + `) THEN pg_catalog.jsonb_build_object(` + p.fields + `) ELSE NULL END) AS body
 FROM public.` + p.table + ` t WHERE ` + org + `=$1::uuid
 AND ($2::text='' OR t.id>NULLIF($2::text,'')::uuid)
 ORDER BY t.id LIMIT 100
), sized AS MATERIALIZED (SELECT *,pg_catalog.octet_length(body::text) AS bytes FROM page),
 guard AS (SELECT COALESCE(pg_catalog.bool_and(shape_ok AND bytes<=16384),true)
 AND COALESCE(pg_catalog.sum(bytes),0)+4096<=LEAST($3::bigint,1048576)
 AND $3::bigint BETWEEN 4096 AND 8388608 AS ok FROM sized)
 SELECT CASE WHEN guard.ok THEN pg_catalog.jsonb_build_object(
 'version',1,'family','` + string(f) + `','records',COALESCE((SELECT pg_catalog.jsonb_agg(body ORDER BY id) FROM sized),'[]'::jsonb),
 'more',(SELECT count(*)=100 FROM sized),'last_id',COALESCE((SELECT id::text FROM sized ORDER BY id DESC LIMIT 1),$2::text),'rejected',false)
 ELSE pg_catalog.jsonb_build_object('version',1,'family','` + string(f) + `','records','[]'::jsonb,'more',false,'last_id',$2::text,'rejected',true) END::text
 FROM guard`
	return Plan{SQL: sql, Tables: []string{p.table}}, nil
}

// Inputs are exclusively fixed literals in Projection's closed family switch.
func nullableJSONType(column, key, kind string) string {
	expression := column + "->'" + key + "'"
	return "(" + expression + " IS NULL OR pg_catalog.jsonb_typeof(" + expression + ") IN ('" + kind + "','null'))"
}

func uuidOK(s string) bool {
	if len(s) != 36 || s == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// DecodePage validates the exact private wire projection, ordering, scope and
// byte budget. It returns no partial page on refusal. A successful page is an
// observation, never a complete inventory or deletion decision.
func DecodePage(raw []byte, f Family, organization, cursor string, remaining int) (Page, error) {
	p, ok := definition(f)
	if !ok || !uuidOK(organization) || (cursor != "" && !uuidOK(cursor)) || remaining < 4096 || remaining > MaxPrivateBytes || len(raw) == 0 || len(raw) > PageBytes || len(raw) > remaining || !utf8.Valid(raw) {
		return Page{}, ErrIncomplete
	}
	if !uniqueJSON(raw) {
		return Page{}, ErrIncomplete
	}
	if !exactObject(raw, "version", "family", "records", "more", "last_id", "rejected") {
		return Page{}, ErrIncomplete
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || !boolJSON(root["more"]) || !bytes.Equal(bytes.TrimSpace(root["rejected"]), []byte("false")) || !stringJSON(root["last_id"]) {
		return Page{}, ErrIncomplete
	}
	var envelope struct {
		Records []json.RawMessage `json:"records"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return Page{}, ErrIncomplete
	}
	for _, record := range envelope.Records {
		if !exactObject(record, "id", "organization_id", "account_name", "channel_account_id", "parent_id", "updated_at", "deleted_at", "protected", "fields") {
			return Page{}, ErrIncomplete
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(record, &fields) != nil || !boolJSON(fields["protected"]) {
			return Page{}, ErrIncomplete
		}
		for _, key := range []string{"id", "organization_id", "account_name", "channel_account_id", "parent_id", "updated_at"} {
			if !stringJSON(fields[key]) {
				return Page{}, ErrIncomplete
			}
		}
	}
	var page Page
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&page) != nil || page.Version != 1 || page.Family != f || page.Rejected || page.Records == nil || len(page.Records) > PageRows || page.More != (len(page.Records) == PageRows) {
		return Page{}, ErrIncomplete
	}
	previous := cursor
	for _, r := range page.Records {
		if !uuidOK(r.ID) || r.ID <= previous || r.OrganizationID != organization || r.Protected != p.protected || len(r.AccountName) > 400 || strings.ContainsRune(r.AccountName, 0) {
			return Page{}, ErrIncomplete
		}
		if r.ChannelAccountID != "" && !uuidOK(r.ChannelAccountID) || r.ParentID != "" && !uuidOK(r.ParentID) {
			return Page{}, ErrIncomplete
		}
		if (p.channel == "''" && r.ChannelAccountID != "") || (p.channel != "''" && r.ChannelAccountID == "") {
			return Page{}, ErrIncomplete
		}
		if p.parent == "''" && r.ParentID != "" || p.parent != "''" && f != InboundMedia && r.ParentID == "" {
			return Page{}, ErrIncomplete
		}
		if p.account == "''" && r.AccountName != "" || f == OrganizationAudio && r.ID != organization {
			return Page{}, ErrIncomplete
		}
		if _, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err != nil {
			return Page{}, ErrIncomplete
		}
		if r.DeletedAt != nil {
			if _, err := time.Parse(time.RFC3339Nano, *r.DeletedAt); err != nil {
				return Page{}, ErrIncomplete
			}
		}
		if len(r.Fields) != len(p.fieldTypes) {
			return Page{}, ErrIncomplete
		}
		for key, kind := range p.fieldTypes {
			value, found := r.Fields[key]
			if !found || !fieldOK(value, kind) {
				return Page{}, ErrIncomplete
			}
		}
		encoded, err := json.Marshal(r)
		if err != nil || len(encoded) > RowBytes {
			return Page{}, ErrIncomplete
		}
		previous = r.ID
	}
	if page.LastID != previous {
		return Page{}, ErrIncomplete
	}
	return page, nil
}

func exactObject(raw []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func boolJSON(raw []byte) bool {
	value := string(bytes.TrimSpace(raw))
	return value == "true" || value == "false"
}

func stringJSON(raw []byte) bool {
	value := bytes.TrimSpace(raw)
	return len(value) >= 2 && value[0] == '"'
}

func fieldOK(raw json.RawMessage, kind string) bool {
	if bytes.Equal(raw, []byte("null")) {
		return kind != "strings"
	}
	switch kind {
	case "string":
		var value string
		return json.Unmarshal(raw, &value) == nil && !strings.ContainsRune(value, 0)
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "number":
		// Generations are exact nonnegative uint64 integers, not floats.
		var value uint64
		return json.Unmarshal(raw, &value) == nil
	case "strings":
		var values []*string
		if json.Unmarshal(raw, &values) != nil || values == nil || len(values) > 256 {
			return false
		}
		for _, value := range values {
			if value != nil && strings.ContainsRune(*value, 0) {
				return false
			}
		}
		return true
	}
	return false
}

func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 12 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		opening, composite := tok.(json.Delim)
		if !composite {
			return true
		}
		switch opening {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
