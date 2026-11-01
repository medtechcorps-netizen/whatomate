package identityreview

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func holdProjectionFixture() (Scope, string, string, []HoldProjectionInput, HoldProjectionLimits) {
	scope := Scope{OrganizationID: "11111111-1111-4111-8111-111111111111", AccountID: "22222222-2222-4222-8222-222222222222", Cycle: 4}
	actor := "33333333-3333-4333-8333-333333333333"
	preview := "44444444-4444-4444-8444-444444444444"
	holds := []HoldProjectionInput{
		{Scope: scope, ID: "55555555-5555-4555-8555-555555555555", Generation: 1, Version: 1, SemanticHash: strings.Repeat("a", 64), Address: Address{Kind: DirectBSUID, Value: "private-sender"}, ReceiptCount: 0,
			Members: []HoldProjectionMember{{ContactID: "77777777-7777-4777-8777-777777777777", Reasons: 4}}},
		{Scope: scope, ID: "66666666-6666-4666-8666-666666666666", Generation: 2, Version: 1, SemanticHash: strings.Repeat("b", 64), Address: Address{Kind: DirectBSUID, Value: "private-sender"}, ReceiptCount: 97,
			Members: []HoldProjectionMember{{ContactID: "88888888-8888-4888-8888-888888888888", Reasons: 1}, {ContactID: "77777777-7777-4777-8777-777777777777", Reasons: 2}}},
	}
	return scope, actor, preview, holds, HoldProjectionLimits{MaxHolds: 4, MaxMembers: 8, MaxItems: 4}
}

func TestHoldProjectionIncludesEveryObservedGenerationBeforePaging(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	if len(m.Items) != 1 || m.ReceiptCount != 97 || len(m.Items[0].Holds) != 2 {
		t.Fatalf("receipt/endpoint/chain counts: %+v", m)
	}
	if len(m.Items[0].Members) != 2 || m.Items[0].Members[0].Reasons != 6 {
		t.Fatal("older zero-receipt member was lost or masks were not unioned")
	}
	reversed := []HoldProjectionInput{holds[1], holds[0]}
	other, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, reversed, limits)
	if err != nil || !reflect.DeepEqual(m, other) {
		t.Fatal("ordering changed the sealed manifest")
	}
}

func TestHoldProjectionRefusesIncompleteOrMixedInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Scope, *[]HoldProjectionInput, *HoldProjectionLimits, *uint64)
	}{
		{"receipt_undercount", func(_ *Scope, _ *[]HoldProjectionInput, _ *HoldProjectionLimits, count *uint64) { *count = 98 }},
		{"foreign_org", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Scope.OrganizationID = "99999999-9999-4999-8999-999999999999"
		}},
		{"foreign_account", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Scope.AccountID = "99999999-9999-4999-8999-999999999999"
		}},
		{"stale_cycle", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) { (*h)[0].Scope.Cycle-- }},
		{"sql_cycle_overflow", func(s *Scope, _ *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			s.Cycle = uint64(1) << 63
		}},
		{"duplicate_hold", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) { (*h)[1].ID = (*h)[0].ID }},
		{"duplicate_member", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Members = append((*h)[0].Members, (*h)[0].Members[0])
		}},
		{"unknown_selector_bit", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Members[0].Reasons = 8
		}},
		{"zero_version", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) { (*h)[0].Version = 0 }},
		{"unbounded_limits", func(_ *Scope, _ *[]HoldProjectionInput, l *HoldProjectionLimits, _ *uint64) {
			*l = HoldProjectionLimits{}
		}},
		{"global_member_cap", func(_ *Scope, _ *[]HoldProjectionInput, l *HoldProjectionLimits, _ *uint64) { l.MaxMembers = 2 }},
		{"hold_cap", func(_ *Scope, _ *[]HoldProjectionInput, l *HoldProjectionLimits, _ *uint64) { l.MaxHolds = 1 }},
		{"unknown_refusal", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Refusal = "pretend_safe"
			(*h)[0].Address = Address{}
		}},
		{"refusal_with_guessed_endpoint", func(_ *Scope, h *[]HoldProjectionInput, _ *HoldProjectionLimits, _ *uint64) {
			(*h)[0].Refusal = "historical_phone_evidence_incomplete"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, actor, id, holds, limits := holdProjectionFixture()
			count := uint64(97)
			tc.change(&scope, &holds, &limits, &count)
			m, err := BuildHoldProjection(context.Background(), scope, actor, id, count, holds, limits)
			if err == nil || m != nil {
				t.Fatal("incomplete input produced a usable partial manifest")
			}
		})
	}
}

func TestHoldProjectionKeepsUnprovenPhoneHoldsSeparate(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	for i := range holds {
		holds[i].Address = Address{}
		holds[i].Refusal = "historical_phone_evidence_incomplete"
	}
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil || len(m.Items) != 2 {
		t.Fatal("unproven sender evidence was coalesced")
	}
	for _, item := range m.Items {
		if item.Refusal != "historical_phone_evidence_incomplete" {
			t.Fatal("individual refusal lost")
		}
	}
}

func TestHoldProjectionDoesNotCoalescePhoneAndBSUID(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	holds[0].Address = Address{Kind: VerifiedPhone, Value: "60123456789"}
	holds[1].Address = Address{Kind: DirectBSUID, Value: "60123456789"}
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil || len(m.Items) != 2 {
		t.Fatal("text equality was treated as identity evolution")
	}
	limits.MaxItems = 1
	if result, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits); err == nil || result != nil {
		t.Fatal("item cap returned partial success")
	}
}

func TestHoldProjectionDigestBindsActorAndProjection(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, changeActor := range []bool{false, true} {
		a, p := actor, id
		if changeActor {
			a = "99999999-9999-4999-8999-999999999999"
		} else {
			p = "99999999-9999-4999-8999-999999999999"
		}
		n, err := BuildHoldProjection(context.Background(), scope, a, p, 97, holds, limits)
		if err != nil || n.ProjectionDigest == m.ProjectionDigest {
			t.Fatal("cross-actor/projection substitution preserved the seal")
		}
	}
}

func TestHoldProjectionPageOwnsCopiesAndRejectsStaleTokens(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	holds[0].Address = Address{Kind: VerifiedPhone, Value: "60123456789"}
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil {
		t.Fatal(err)
	}
	first, next, err := PageHoldProjection(m, actor, m.ProjectionDigest, "", 1, 2)
	if err != nil || len(first) != 1 || next == "" {
		t.Fatal("first page")
	}
	second, end, err := PageHoldProjection(m, actor, m.ProjectionDigest, next, 1, 2)
	if err != nil || len(second) != 1 || end != "" || first[0].Key == second[0].Key {
		t.Fatal("complete pages overlap or truncate")
	}
	first[0].Members[0].Reasons = 7
	first[0].Holds[0].Version = 99
	if _, _, err := PageHoldProjection(m, actor, m.ProjectionDigest, "", 1, 2); err != nil {
		t.Fatal("returned page mutated sealed storage")
	}
	for _, cursor := range []string{strings.Repeat("f", 64) + ":1", m.ProjectionDigest + ":01", m.ProjectionDigest + ":-1", m.ProjectionDigest + ":999999999999999999999999", m.ProjectionDigest + ":3"} {
		if p, _, err := PageHoldProjection(m, actor, m.ProjectionDigest, cursor, 1, 2); err == nil || p != nil {
			t.Fatal("accepted invalid cursor")
		}
	}
	if _, _, err := PageHoldProjection(m, "99999999-9999-4999-8999-999999999999", m.ProjectionDigest, "", 1, 2); err == nil {
		t.Fatal("foreign actor")
	}
	m.Items[0].ReceiptCount++
	if _, _, err := PageHoldProjection(m, actor, m.ProjectionDigest, "", 1, 2); err == nil {
		t.Fatal("stored data changed without resealing")
	}
	m.Items[0].ReceiptCount--
	m.Version++
	m.ProjectionDigest, err = holdProjectionDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := PageHoldProjection(m, actor, m.ProjectionDigest, "", 1, 2); err == nil {
		t.Fatal("different hold-projection protocol accepted")
	}
}

func TestHoldProjectionCancellationAndKnownZero(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m, err := BuildHoldProjection(ctx, scope, actor, id, 97, holds, limits); !errors.Is(err, context.Canceled) || m != nil {
		t.Fatal("cancelled build succeeded")
	}
	if m, err := BuildHoldProjection(nil, scope, actor, id, 97, holds, limits); err == nil || m != nil { //nolint:staticcheck // SA1012: deliberately verify nil context refuses with no projection.
		t.Fatal("missing context")
	}
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 0, nil, limits)
	if err != nil || m == nil || len(m.Items) != 0 || m.ReceiptCount != 0 {
		t.Fatal("complete empty observation is not known zero")
	}
	if m, err := BuildHoldProjection(context.Background(), scope, actor, id, 1, nil, limits); err == nil || m != nil {
		t.Fatal("missing rows masquerade as known zero")
	}
}

// The former private preview domain must not survive the semantic narrowing.
func TestHoldProjectionRejectsFormerPreviewDigestDomain(t *testing.T) {
	scope, actor, id, holds, limits := holdProjectionFixture()
	m, err := BuildHoldProjection(context.Background(), scope, actor, id, 97, holds, limits)
	if err != nil {
		t.Fatal(err)
	}
	current := m.ProjectionDigest
	m.ProjectionDigest, err = digest(struct {
		Domain   string                  `json:"domain"`
		Manifest *HoldProjectionManifest `json:"manifest"`
	}{"rereply.identity_review.observed_preview", m})
	if err != nil {
		t.Fatal(err)
	}
	if m.ProjectionDigest == current {
		t.Fatal("old full-preview label retained as projection domain")
	}
	if page, _, err := PageHoldProjection(m, actor, m.ProjectionDigest, "", 1, 2); err == nil || page != nil {
		t.Fatal("former preview domain accepted as current hold projection")
	}
	m.ProjectionDigest = current
	if _, _, err := PageHoldProjection(m, actor, current, "", 1, 2); err != nil {
		t.Fatal(err)
	}
}
