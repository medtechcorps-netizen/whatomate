package identityreview

import (
	"strings"
	"testing"
)

func testScope() Scope {
	return Scope{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", 7}
}

func TestClassifyAddressUsesOnlyDirectEvidence(t *testing.T) {
	tests := []struct {
		name         string
		claim        Claim
		kind, reason string
	}{
		{"phone", Claim{Source: SignedDirectMessage, Phone: "60123456789"}, VerifiedPhone, ""},
		{"direct", Claim{Source: SignedDirectMessage, DirectBSUID: "bsuid-A", Phone: "60123456789"}, DirectBSUID, ""},
		{"disputed phone does not change direct principal", Claim{Source: SignedDirectMessage, DirectBSUID: "bsuid-A", PhoneSidecarConflict: true}, DirectBSUID, ""},
		{"history is not direct evidence", Claim{Source: "history", Phone: "60123456789"}, "", "direct_message_evidence_required"},
		{"missing source", Claim{Phone: "60123456789"}, "", "direct_message_evidence_required"},
		{"parent only", Claim{Source: SignedDirectMessage, ParentBSUID: "parent-A", Phone: "60123456789"}, "", "parent_without_direct_primary"},
		{"sidecar conflict", Claim{Source: SignedDirectMessage, Phone: "60123456789", PhoneSidecarConflict: true}, "", "conflicting_phone_evidence"},
		{"no address", Claim{Source: SignedDirectMessage}, "", "verified_phone_required"},
		{"display value is not phone", Claim{Source: SignedDirectMessage, Phone: "Jane Smith"}, "", "verified_phone_required"},
		{"leading zero", Claim{Source: SignedDirectMessage, Phone: "0123456789"}, "", "verified_phone_required"},
		{"plus not canonical", Claim{Source: SignedDirectMessage, Phone: "+60123456789"}, "", "verified_phone_required"},
		{"unicode digits", Claim{Source: SignedDirectMessage, Phone: "６０１２３４５６７８９"}, "", "verified_phone_required"},
		{"invalid direct cannot downgrade to phone", Claim{Source: SignedDirectMessage, DirectBSUID: " bad ", Phone: "60123456789"}, "", "invalid_direct_bsuid"},
		{"too long direct", Claim{Source: SignedDirectMessage, DirectBSUID: strings.Repeat("a", 151)}, "", "invalid_direct_bsuid"},
		{"control in direct", Claim{Source: SignedDirectMessage, DirectBSUID: "a\x00b"}, "", "invalid_direct_bsuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, reason := ClassifyAddress(tt.claim)
			if address.Kind != tt.kind || reason != tt.reason {
				t.Fatalf("got kind=%q reason=%q", address.Kind, reason)
			}
			if reason != "" && address != (Address{}) {
				t.Fatal("refusal retained an address")
			}
		})
	}
}

func TestSubjectKeyHasExactRoutingScopeAndNoImplicitAlias(t *testing.T) {
	scope, address := testScope(), Address{VerifiedPhone, "60123456789"}
	base, err := SubjectKey(scope, address)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := SubjectKey(scope, address)
	if base != again {
		t.Fatal("same subject changed key")
	}
	variants := []struct {
		scope   Scope
		address Address
	}{
		{Scope{"33333333-3333-4333-8333-333333333333", scope.AccountID, scope.Cycle}, address},
		{Scope{scope.OrganizationID, "33333333-3333-4333-8333-333333333333", scope.Cycle}, address},
		{Scope{scope.OrganizationID, scope.AccountID, scope.Cycle + 1}, address},
		{scope, Address{VerifiedPhone, "60123456780"}},
		{scope, Address{DirectBSUID, address.Value}},
	}
	seen := map[string]bool{base: true}
	for _, v := range variants {
		key, err := SubjectKey(v.scope, v.address)
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatal("distinct scope/address collapsed")
		}
		seen[key] = true
	}
}

func TestSubjectKeyRejectsMalformedScopeOrAddress(t *testing.T) {
	for _, scope := range []Scope{{}, {testScope().OrganizationID, testScope().AccountID, 0}, {"00000000-0000-0000-0000-000000000000", testScope().AccountID, 1}, {"11111111-1111-4111-8111-11111111111G", testScope().AccountID, 1}} {
		if _, err := SubjectKey(scope, Address{DirectBSUID, "valid"}); err == nil {
			t.Fatal("accepted malformed scope")
		}
	}
	for _, address := range []Address{{}, {"username", "user-A"}, {VerifiedPhone, "123"}, {DirectBSUID, "\xff"}} {
		if _, err := SubjectKey(testScope(), address); err == nil {
			t.Fatal("accepted malformed address")
		}
	}
}

func testIntent(t *testing.T) Intent {
	t.Helper()
	key, err := SubjectKey(testScope(), Address{DirectBSUID, "sender-A"})
	if err != nil {
		t.Fatal(err)
	}
	return Intent{Scope: testScope(), RequestID: "33333333-3333-4333-8333-333333333333", PreviewID: "44444444-4444-4444-8444-444444444444", SubjectKey: key, SnapshotDigest: strings.Repeat("a", 64), Action: AllowFuture, TargetMode: ExistingContact, TargetContactID: "55555555-5555-4555-8555-555555555555"}
}

// These independently calculated canonical JSON vectors freeze protocol v1.
// Changing serialization requires an explicit new protocol, not lost replay
// equality for durable decisions made by an earlier application build.
func TestProtocolV1StableDigests(t *testing.T) {
	intent := testIntent(t)
	if intent.SubjectKey != "ffa9901d52f4053aebf55fd51789dfb60147996910330a58809b0b7ad4935267" {
		t.Fatal("protocol v1 subject digest changed")
	}
	got, err := IntentDigest(intent)
	if err != nil || got != "03337844b7a85994b93fe41daef54fa7509272de35ee3a5b550365031c950474" {
		t.Fatal("protocol v1 intent digest changed")
	}
}

func TestIntentDigestBindsEveryDecisionBoundary(t *testing.T) {
	base := testIntent(t)
	digest, err := IntentDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := IntentDigest(base)
	if digest != again {
		t.Fatal("retry changed digest")
	}
	mutations := []func(*Intent){
		func(i *Intent) { i.Scope.Cycle++ },
		func(i *Intent) { i.Scope.AccountID = "66666666-6666-4666-8666-666666666666" },
		func(i *Intent) { i.RequestID = "66666666-6666-4666-8666-666666666666" },
		func(i *Intent) { i.PreviewID = "66666666-6666-4666-8666-666666666666" },
		func(i *Intent) { i.SubjectKey = strings.Repeat("b", 64) },
		func(i *Intent) { i.SnapshotDigest = strings.Repeat("b", 64) },
		func(i *Intent) { i.TargetContactID = "66666666-6666-4666-8666-666666666666" },
		func(i *Intent) { i.TargetMode = NewContact; i.TargetContactID = "" },
	}
	seen := map[string]bool{digest: true}
	for _, mutate := range mutations {
		current := base
		mutate(&current)
		got, err := IntentDigest(current)
		if err != nil {
			t.Fatal(err)
		}
		if seen[got] {
			t.Fatal("different intent retained digest")
		}
		seen[got] = true
	}
}

func TestIntentRejectsAmbiguousOrForeignHistoryShapes(t *testing.T) {
	mutations := []func(*Intent){
		func(i *Intent) { i.Action = "approve_all_history" },
		func(i *Intent) { i.Action = "not_this_person" },
		func(i *Intent) { i.TargetMode = NewContact },
		func(i *Intent) { i.TargetContactID = "" },
		func(i *Intent) { i.TargetMode = "phone_match" },
		func(i *Intent) { i.SnapshotDigest = strings.Repeat("A", 64) },
		func(i *Intent) { i.RequestID = " 33333333-3333-4333-8333-333333333333" },
	}
	for _, mutate := range mutations {
		current := testIntent(t)
		mutate(&current)
		if _, err := IntentDigest(current); err == nil {
			t.Fatal("accepted invalid intent")
		}
	}
}
