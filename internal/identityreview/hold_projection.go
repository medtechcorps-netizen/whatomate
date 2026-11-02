package identityreview

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// HoldProjectionVersion identifies IMMUTABLE HOLD DATA ONLY. It preserves the
// recovered ProtocolVersion=1 Subject/Intent encoding. A ProjectionDigest MUST
// NOT be used as Intent.SnapshotDigest or as complete preview/grant authority.
// Current owners, families, routability and authorization are not inputs here.
const HoldProjectionVersion = 1

// HoldProjectionLimits are explicit observation/memory limits, never write quotas or
// permissions. There are deliberately no production defaults. A DB adapter must
// enforce these while expanding its query, before retaining rows in this input.
type HoldProjectionLimits struct {
	MaxHolds   int
	MaxMembers int
	MaxItems   int
}

// HoldProjectionMember is an immutable hold-member input. IDs and reason masks are
// facts from the held-connection observer, never request-supplied candidates.
type HoldProjectionMember struct {
	ContactID string `json:"contact_id"`
	Reasons   uint8  `json:"reasons"`
}

// HoldProjectionInput describes one immutable generation, including zero-receipt
// generations needed by the complete closing-chain union. Complete enumeration,
// provenance, current owner/routability, and authorization are adapter duties.
// Before this call the adapter must verify EACH hold member count/digest and
// semantic digest. This helper cannot prove omitted rows or current authority.
// Never insert derived current family members into these immutable Members.
type HoldProjectionInput struct {
	Scope        Scope
	ID           string
	Generation   uint64
	Version      uint64
	SemanticHash string
	Address      Address
	Refusal      string
	ReceiptCount uint64
	Members      []HoldProjectionMember
}

type HoldProjectionRef struct {
	ID           string `json:"id"`
	Generation   uint64 `json:"generation"`
	Version      uint64 `json:"version"`
	SemanticHash string `json:"semantic_hash"`
	ReceiptCount uint64 `json:"receipt_count"`
}

// HoldProjectionItem contains no message/media/profile body. The endpoint key and
// contact IDs are private, not anonymized or suitable for public logs.
type HoldProjectionItem struct {
	Key          string                 `json:"key"`
	Refusal      string                 `json:"refusal"`
	ReceiptCount uint64                 `json:"receipt_count"`
	Holds        []HoldProjectionRef    `json:"holds"`
	Members      []HoldProjectionMember `json:"members"`
}

type HoldProjectionManifest struct {
	Version          int                  `json:"version"`
	Scope            Scope                `json:"scope"`
	ActorID          string               `json:"actor_id"`
	ProjectionID     string               `json:"projection_id"`
	ReceiptCount     uint64               `json:"receipt_count"`
	Items            []HoldProjectionItem `json:"items"`
	ProjectionDigest string               `json:"-"`
}

// BuildHoldProjection canonicalizes only a trusted immutable hold projection.
// It provides no complete current observation, eligibility or authorization.
// A future versioned SnapshotDigest must additionally bind the separately
// bounded current family/owner/routability/authority facts and candidate union.
// expectedReceipts is an independently aggregated same-statement total; any
// mismatch refuses the entire result. The adapter must separately attest that
// all immutable generations/member rows were bounded during collection.
// Current selector/family facts belong to a SEPARATE observation relation.
func BuildHoldProjection(ctx context.Context, scope Scope, actorID, projectionID string, expectedReceipts uint64, holds []HoldProjectionInput, limits HoldProjectionLimits) (*HoldProjectionManifest, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, ErrInvalidIntent
	}
	if !validScope(scope) || scope.Cycle > uint64(1<<63-1) || expectedReceipts > uint64(1<<63-1) || !canonicalUUID(actorID) || !canonicalUUID(projectionID) ||
		limits.MaxHolds < 1 || limits.MaxMembers < 1 || limits.MaxItems < 1 || len(holds) > limits.MaxHolds {
		return nil, ErrInvalidIntent
	}
	seen := make(map[string]bool)
	items := make(map[string]*HoldProjectionItem)
	unions := make(map[string]map[string]uint8)
	totalMembers := 0
	var totalReceipts uint64
	for _, hold := range holds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hold.Scope != scope || !canonicalUUID(hold.ID) || seen[hold.ID] || hold.Version == 0 ||
			hold.Version > uint64(1<<63-1) || hold.Generation > uint64(1<<63-1) || !lowerDigest(hold.SemanticHash) ||
			len(hold.Members) > limits.MaxMembers-totalMembers {
			return nil, ErrInvalidIntent
		}
		seen[hold.ID] = true
		totalMembers += len(hold.Members)
		if hold.ReceiptCount > ^uint64(0)-totalReceipts {
			return nil, ErrInvalidIntent
		}
		totalReceipts += hold.ReceiptCount
		var key string
		if hold.Refusal != "" {
			if !holdProjectionRefusal(hold.Refusal) || hold.Address != (Address{}) {
				return nil, ErrInvalidIntent
			}
			// An unproven endpoint cannot coalesce separate receipts by phone,
			// display name or guessed identity. Preserve its hold-level reason.
			key = "hold:" + hold.ID
		} else {
			var err error
			key, err = SubjectKey(scope, hold.Address)
			if err != nil {
				return nil, err
			}
		}
		item := items[key]
		if item == nil {
			if len(items) >= limits.MaxItems {
				return nil, ErrInvalidIntent
			}
			item = &HoldProjectionItem{Key: key, Refusal: hold.Refusal, Holds: []HoldProjectionRef{}, Members: []HoldProjectionMember{}}
			items[key] = item
			unions[key] = make(map[string]uint8)
		}
		item.ReceiptCount += hold.ReceiptCount // Bounded by the checked global sum.
		item.Holds = append(item.Holds, HoldProjectionRef{hold.ID, hold.Generation, hold.Version, hold.SemanticHash, hold.ReceiptCount})
		perHold := make(map[string]bool)
		for _, member := range hold.Members {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Current immutable member mask is direct/parent/phone = 1/2/4.
			if !canonicalUUID(member.ContactID) || member.Reasons == 0 || member.Reasons&^uint8(7) != 0 || perHold[member.ContactID] {
				return nil, ErrInvalidIntent
			}
			perHold[member.ContactID] = true
			unions[key][member.ContactID] |= member.Reasons
		}
	}
	if totalReceipts != expectedReceipts {
		return nil, ErrInvalidIntent
	}
	manifest := &HoldProjectionManifest{Version: HoldProjectionVersion, Scope: scope, ActorID: actorID, ProjectionID: projectionID, ReceiptCount: totalReceipts, Items: []HoldProjectionItem{}}
	for key, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for id, reasons := range unions[key] {
			item.Members = append(item.Members, HoldProjectionMember{id, reasons})
		}
		sort.Slice(item.Members, func(i, j int) bool { return item.Members[i].ContactID < item.Members[j].ContactID })
		sort.Slice(item.Holds, func(i, j int) bool {
			if item.Holds[i].Generation != item.Holds[j].Generation {
				return item.Holds[i].Generation < item.Holds[j].Generation
			}
			return item.Holds[i].ID < item.Holds[j].ID
		})
		manifest.Items = append(manifest.Items, *item)
	}
	sort.Slice(manifest.Items, func(i, j int) bool { return manifest.Items[i].Key < manifest.Items[j].Key })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	manifest.ProjectionDigest, err = holdProjectionDigest(manifest)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return manifest, nil
}

func holdProjectionDigest(manifest *HoldProjectionManifest) (string, error) {
	return digest(struct {
		Domain   string                  `json:"domain"`
		Manifest *HoldProjectionManifest `json:"manifest"`
	}{"rereply.identity_review.hold_projection", manifest})
}

func holdProjectionRefusal(reason string) bool {
	switch reason {
	case "direct_message_evidence_required", "invalid_direct_bsuid", "parent_without_direct_primary", "conflicting_phone_evidence", "verified_phone_required",
		"historical_phone_evidence_incomplete", "contradictory_owner", "outbound_address_unproven", "unsupported_claim_shape":
		return true
	default:
		return false
	}
}

// PageHoldProjection pages IMMUTABLE HOLD DATA ONLY, not an authorized preview.
// The adapter pins actor/selected org/account/cycle in storage, bounds cursor
// bytes BEFORE parsing, and reauthorizes the WHOLE immutable AND current union.
// It checks the shared budget before/after this finite non-preemptible hash/copy
// and emits nothing after cancellation. Expiry/lifecycle are adapter duties.
// Cursors and ProjectionDigest are consistency tokens, never grant authority.
func PageHoldProjection(manifest *HoldProjectionManifest, actorID, expectedDigest, cursor string, size, maxPageSize int) ([]HoldProjectionItem, string, error) {
	if manifest == nil || manifest.Version != HoldProjectionVersion || !validScope(manifest.Scope) ||
		manifest.Scope.Cycle > uint64(1<<63-1) || !canonicalUUID(manifest.ProjectionID) ||
		manifest.ActorID != actorID || !canonicalUUID(actorID) || !lowerDigest(expectedDigest) ||
		manifest.ProjectionDigest != expectedDigest || size < 1 || maxPageSize < 1 || size > maxPageSize {
		return nil, "", ErrInvalidIntent
	}
	actual, err := holdProjectionDigest(manifest)
	if err != nil || actual != expectedDigest {
		return nil, "", ErrInvalidIntent
	}
	offset := 0
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 2 || parts[0] != expectedDigest || parts[1] == "" || (len(parts[1]) > 1 && parts[1][0] == '0') {
			return nil, "", ErrInvalidIntent
		}
		for _, c := range parts[1] {
			if c < '0' || c > '9' {
				return nil, "", ErrInvalidIntent
			}
		}
		parsed, err := strconv.Atoi(parts[1])
		if err != nil || parsed < 0 || parsed > len(manifest.Items) {
			return nil, "", ErrInvalidIntent
		}
		offset = parsed
	}
	end := len(manifest.Items)
	if size < end-offset {
		end = offset + size
	}
	// Copy nested slices: a caller changing a page must not change the seal.
	page := make([]HoldProjectionItem, end-offset)
	for i, item := range manifest.Items[offset:end] {
		page[i] = item
		page[i].Holds = append([]HoldProjectionRef{}, item.Holds...)
		page[i].Members = append([]HoldProjectionMember{}, item.Members...)
	}
	next := ""
	if end < len(manifest.Items) {
		next = fmt.Sprintf("%s:%d", expectedDigest, end)
	}
	return page, next, nil
}
