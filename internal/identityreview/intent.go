// Package identityreview defines inert, versioned future-review intent values.
// It is not imported by production handlers. None of these helpers grants
// permission, authenticates an event, resolves a contact, or authorizes routing.
package identityreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion     = 1
	SignedDirectMessage = "meta_signed_regular_message"
	DirectBSUID         = "direct_bsuid"
	VerifiedPhone       = "verified_phone"
	AllowFuture         = "allow_future"
	ExistingContact     = "existing_contact"
	NewContact          = "new_contact"
)

var ErrInvalidIntent = errors.New("invalid identity-review intent")

// Scope is the immutable routing context. Account names are never identifiers.
type Scope struct {
	OrganizationID string `json:"organization_id"`
	AccountID      string `json:"account_id"`
	Cycle          uint64 `json:"onboarding_cycle"`
}

// Claim is a projection from server-verified direct-message evidence, never an
// HTTP request body. Source is a discriminator, not proof of authenticity: the
// storage adapter must validate signature provenance and immutable receipt
// bindings before calling ClassifyAddress. Sidecar/user/profile names cannot
// supply a missing direct address.
type Claim struct {
	Source               string
	DirectBSUID          string
	ParentBSUID          string
	Phone                string
	PhoneSidecarConflict bool
}

// Address identifies an authenticated endpoint, not a permanent human person.
// In particular, a phone approval cannot detect a silent number recycling.
type Address struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ClassifyAddress returns an addressable endpoint or a stable refusal reason.
// A direct BSUID is sufficient to identify the sender even when its phone is
// disputed; target selection and foreign-history checks remain mandatory.
// This deliberately does not create a phone alias for a BSUID-bearing claim.
func ClassifyAddress(claim Claim) (Address, string) {
	if claim.Source != SignedDirectMessage {
		return Address{}, "direct_message_evidence_required"
	}
	if claim.DirectBSUID != "" {
		if !validBSUID(claim.DirectBSUID) {
			return Address{}, "invalid_direct_bsuid"
		}
		return Address{Kind: DirectBSUID, Value: claim.DirectBSUID}, ""
	}
	if claim.ParentBSUID != "" {
		return Address{}, "parent_without_direct_primary"
	}
	if claim.PhoneSidecarConflict {
		return Address{}, "conflicting_phone_evidence"
	}
	if !validPhone(claim.Phone) {
		return Address{}, "verified_phone_required"
	}
	return Address{Kind: VerifiedPhone, Value: claim.Phone}, ""
}

// SubjectKey deduplicates exact endpoints within a tenant/account/cycle. It
// must stay private: it is an unkeyed digest and is not an anonymization token.
// Aliases and cross-cycle continuity require a separate verified DB operation.
func SubjectKey(scope Scope, address Address) (string, error) {
	if !validScope(scope) || !validAddress(address) {
		return "", ErrInvalidIntent
	}
	return digest(struct {
		Domain  string  `json:"domain"`
		Version int     `json:"version"`
		Scope   Scope   `json:"scope"`
		Address Address `json:"address"`
	}{"rereply.identity_review.subject", ProtocolVersion, scope, address})
}

// Intent binds one preview item to one exact decision. SnapshotDigest commits
// to the server's complete hold/candidate/owner/routability snapshot; accepting
// its syntax does not prove that snapshot current. Apply must rebuild it under
// the existing policy/selector/account/state locks and reauthorize the caller.
type Intent struct {
	Scope           Scope  `json:"scope"`
	RequestID       string `json:"request_id"`
	PreviewID       string `json:"preview_id"`
	SubjectKey      string `json:"subject_key"`
	SnapshotDigest  string `json:"snapshot_digest"`
	Action          string `json:"action"`
	TargetMode      string `json:"target_mode"`
	TargetContactID string `json:"target_contact_id"`
}

// IntentDigest is an equality check for idempotency, never an authorization
// signature. A repeated request key with a different digest must conflict.
// Other review outcomes, including "not this person", need their own explicit
// lifecycle contract; this initial intent must not silently reinterpret them.
func IntentDigest(intent Intent) (string, error) {
	if !validScope(intent.Scope) || !canonicalUUID(intent.RequestID) ||
		!canonicalUUID(intent.PreviewID) || !lowerDigest(intent.SubjectKey) ||
		!lowerDigest(intent.SnapshotDigest) {
		return "", ErrInvalidIntent
	}
	if intent.Action != AllowFuture {
		return "", ErrInvalidIntent
	}
	switch intent.TargetMode {
	case ExistingContact:
		if !canonicalUUID(intent.TargetContactID) {
			return "", ErrInvalidIntent
		}
	case NewContact:
		if intent.TargetContactID != "" {
			return "", ErrInvalidIntent
		}
	default:
		return "", ErrInvalidIntent
	}
	return digest(struct {
		Domain  string `json:"domain"`
		Version int    `json:"version"`
		Intent  Intent `json:"intent"`
	}{"rereply.identity_review.intent", ProtocolVersion, intent})
}

func validScope(scope Scope) bool {
	return canonicalUUID(scope.OrganizationID) && canonicalUUID(scope.AccountID) && scope.Cycle != 0
}

func validAddress(address Address) bool {
	switch address.Kind {
	case DirectBSUID:
		return validBSUID(address.Value)
	case VerifiedPhone:
		return validPhone(address.Value)
	default:
		return false
	}
}

func validBSUID(value string) bool {
	// Contact.BSUID currently has a 150-character limit. The byte bound is
	// conservative and avoids accepting an endpoint the adapter cannot store.
	if value == "" || len(value) > 150 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}

func validPhone(value string) bool {
	if len(value) < 7 || len(value) > 15 || value[0] == '0' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func lowerDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func digest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
