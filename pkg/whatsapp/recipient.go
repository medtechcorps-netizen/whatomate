package whatsapp

import "strings"

// Recipient identifies a WhatsApp user by phone number and/or BSUID.
// Meta accepts both: phone number via "to" and BSUID via "recipient".
// When both are provided, phone number takes precedence.
type Recipient struct {
	Phone string // Phone number (e.g., "16505551234")
	BSUID string // Business-Scoped User ID (e.g., "US.13491208655302741918")
}

// placeholderAddressPrefixes are the non-dialable contact identities the
// application stores in Contact.PhoneNumber when Meta identifies a user only
// by a business-scoped user ID or username (for example "bsuid:<hash>").
// They are local lookup keys, never WhatsApp addresses.
var placeholderAddressPrefixes = []string{"bsuid:", "user:", "event:", "id:"}

// IsPlaceholderAddress reports whether phone is a stored, non-dialable
// identity placeholder rather than a WhatsApp address. Only the known
// placeholder prefixes match: other non-digit values (for example group JIDs)
// remain valid "to" addresses.
func IsPlaceholderAddress(phone string) bool {
	phone = strings.ToLower(strings.TrimSpace(phone))
	for _, prefix := range placeholderAddressPrefixes {
		if strings.HasPrefix(phone, prefix) {
			return true
		}
	}
	return false
}

// SetOnPayload sets the "to" and/or "recipient" fields on a message payload.
// A placeholder phone is never sent as "to"; delivery then relies on the
// BSUID alone.
func (r Recipient) SetOnPayload(payload map[string]any) {
	if r.Phone != "" && !IsPlaceholderAddress(r.Phone) {
		payload["to"] = r.Phone
	}
	if r.BSUID != "" {
		payload["recipient"] = r.BSUID
	}
}
