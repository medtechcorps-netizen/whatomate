package mediaretirement

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const org = "10000000-0000-0000-0000-000000000001"

func fixture(t *testing.T, f Family, n int) []byte {
	t.Helper()
	p, ok := definition(f)
	if !ok {
		t.Fatal("invalid fixture family")
	}
	page := Page{Version: 1, Family: f, Records: []Record{}, More: n == PageRows}
	for i := 1; i <= n; i++ {
		r := Record{ID: fmt.Sprintf("20000000-0000-0000-0000-%012d", i), OrganizationID: org, UpdatedAt: "2026-11-03T00:00:00Z", Protected: p.protected, Fields: map[string]json.RawMessage{}}
		if f == OrganizationAudio {
			r.ID = org
		}
		if p.account != "''" {
			r.AccountName = "retained account"
		}
		if p.channel != "''" {
			r.ChannelAccountID = "30000000-0000-0000-0000-000000000001"
		}
		if p.parent != "''" {
			r.ParentID = "40000000-0000-0000-0000-000000000001"
		}
		for key, kind := range p.fieldTypes {
			switch kind {
			case "string":
				r.Fields[key] = json.RawMessage(`"opaque/reference"`)
			case "number":
				r.Fields[key] = json.RawMessage(`18446744073709551615`)
			case "boolean":
				r.Fields[key] = json.RawMessage(`false`)
			case "strings":
				r.Fields[key] = json.RawMessage(`["local.wav",null]`)
			}
		}
		page.Records = append(page.Records, r)
		page.LastID = r.ID
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPagePreservesFixedReferencesAndSoftDeletion(t *testing.T) {
	for _, f := range []Family{Messages, MessageParts, CampaignPreviews, Recordings, InboundMedia, IVRAudio, OrganizationAudio, CatalogArtwork, IdentityAvatars} {
		t.Run(string(f), func(t *testing.T) {
			raw := fixture(t, f, 1)
			var page Page
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			deleted := "2026-11-03T01:00:00+08:00"
			page.Records[0].DeletedAt = &deleted
			encoded, _ := json.Marshal(page)
			got, err := DecodePage(encoded, f, org, "", MaxPrivateBytes)
			if err != nil || len(got.Records) != 1 || got.Records[0].DeletedAt == nil || *got.Records[0].DeletedAt != deleted {
				t.Fatal("soft-deleted reference lost")
			}
			if got.More || got.LastID != page.LastID {
				t.Fatal("wrong terminal page")
			}
			before, _ := json.Marshal(page.Records[0].Fields)
			after, _ := json.Marshal(got.Records[0].Fields)
			if string(before) != string(after) {
				t.Fatal("opaque reference or provider ID changed")
			}
		})
	}
}

func TestPagePaginationRequiresAdvancingCompleteCursor(t *testing.T) {
	raw := fixture(t, Messages, 100)
	page, err := DecodePage(raw, Messages, org, "", MaxPrivateBytes)
	if err != nil || !page.More || len(page.Records) != 100 {
		t.Fatal("full page refused")
	}
	empty := Page{Version: 1, Family: Messages, Records: []Record{}, LastID: page.LastID}
	last, _ := json.Marshal(empty)
	if got, err := DecodePage(last, Messages, org, page.LastID, MaxPrivateBytes); err != nil || got.More {
		t.Fatal("empty terminal page refused")
	}
	if _, err := DecodePage(raw, Messages, org, page.LastID, MaxPrivateBytes); !errors.Is(err, ErrIncomplete) {
		t.Fatal("replayed/nonadvancing page accepted")
	}
	page.Records[0], page.Records[1] = page.Records[1], page.Records[0]
	bad, _ := json.Marshal(page)
	if _, err := DecodePage(bad, Messages, org, "", MaxPrivateBytes); err == nil {
		t.Fatal("unordered rows accepted")
	}
}

func TestPageRefusesAmbiguousMalformedOrUnscopedWire(t *testing.T) {
	valid := string(fixture(t, Messages, 1))
	cases := map[string]string{
		"duplicate-key":      strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"unknown-root":       strings.Replace(valid, `"version":1`, `"version":1,"content":"private"`, 1),
		"unknown-field":      strings.Replace(valid, `"fields":{`, `"fields":{"content":"private",`, 1),
		"wrong-org":          strings.Replace(valid, org, "10000000-0000-0000-0000-000000000002", 1),
		"missing-flag":       strings.Replace(valid, `,"rejected":false`, "", 1),
		"null-flag":          strings.Replace(valid, `"more":false`, `"more":null`, 1),
		"null-protection":    strings.Replace(valid, `"protected":false`, `"protected":null`, 1),
		"server-rejected":    strings.Replace(valid, `"rejected":false`, `"rejected":true`, 1),
		"false-more":         strings.Replace(valid, `"more":false`, `"more":true`, 1),
		"trailing-value":     valid + `{}`,
		"wrong-value-type":   strings.Replace(valid, `"media_url":"opaque/reference"`, `"media_url":{"content":"private"}`, 1),
		"unexpected-channel": strings.Replace(valid, `"channel_account_id":""`, `"channel_account_id":"30000000-0000-0000-0000-000000000001"`, 1),
		"invalid-utf8":       strings.Replace(valid, "opaque/reference", string([]byte{0xff}), 1),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if bad == valid {
				t.Fatal("fixture mutation missed")
			}
			page, err := DecodePage([]byte(bad), Messages, org, "", MaxPrivateBytes)
			if !errors.Is(err, ErrIncomplete) || page.Records != nil {
				t.Fatal("refusal returned a successful or partial page")
			}
		})
	}
}

func TestPageRefusesByteRowAndGenerationOverflow(t *testing.T) {
	raw := fixture(t, Messages, 1)
	if _, err := DecodePage(raw, Messages, org, "", 4095); err == nil {
		t.Fatal("insufficient budget accepted")
	}
	large := strings.Replace(string(raw), "opaque/reference", strings.Repeat("x", RowBytes+1), 1)
	if _, err := DecodePage([]byte(large), Messages, org, "", MaxPrivateBytes); err == nil {
		t.Fatal("oversized row accepted")
	}
	if _, err := DecodePage([]byte(strings.Repeat(" ", PageBytes)+string(raw)), Messages, org, "", MaxPrivateBytes); err == nil {
		t.Fatal("oversized transfer accepted")
	}
	badGeneration := strings.Replace(string(fixture(t, InboundMedia, 1)), "18446744073709551615", "18446744073709551616", 1)
	if _, err := DecodePage([]byte(badGeneration), InboundMedia, org, "", MaxPrivateBytes); err == nil {
		t.Fatal("generation overflow accepted")
	}
	if _, err := DecodePage(fixture(t, Messages, 101), Messages, org, "", MaxPrivateBytes); err == nil {
		t.Fatal("row overflow accepted")
	}
}

func TestProjectionRejectsCallerControlledFamily(t *testing.T) {
	for _, f := range []Family{"", `messages; DELETE FROM public.messages`, "recovery_checkpoints", "resellers"} {
		if p, err := Projection(f); !errors.Is(err, ErrIncomplete) || p.SQL != "" || p.Tables != nil {
			t.Fatal("unadmitted family accepted")
		}
	}
	for _, f := range []Family{Messages, MessageParts, CampaignPreviews, Recordings, InboundMedia, IVRAudio, OrganizationAudio, CatalogArtwork, IdentityAvatars} {
		p, err := Projection(f)
		if err != nil || len(p.Tables) != 1 || !strings.Contains(p.SQL, "$1::uuid") || !strings.Contains(p.SQL, "$2::text") || !strings.Contains(p.SQL, "$3::bigint") {
			t.Fatal("closed parameterized projection unavailable")
		}
	}
}
