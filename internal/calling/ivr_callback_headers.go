package calling

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
)

// IVRCallbackHeaderMask is the only header value exposed by IVR flow APIs.
// Header values are credentials regardless of their names, so all values are
// encrypted at rest and all are masked at the API boundary.
const IVRCallbackHeaderMask = "********"

var (
	ErrIVRCallbackHeaderEncryptionUnavailable = errors.New("IVR callback header encryption is unavailable")
	ErrIVRCallbackHeaderMaskWithoutExisting   = errors.New("masked IVR callback header has no existing value")
)

type ivrCallbackHeaderSlot struct {
	nodeID string
	event  string
	values map[string]any
	set    func(map[string]any)
}

func cloneIVRMenu(menu models.JSONB) (models.JSONB, error) {
	if menu == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(menu)
	if err != nil {
		return nil, fmt.Errorf("copy IVR flow menu: %w", err)
	}
	var cloned models.JSONB
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, fmt.Errorf("copy IVR flow menu: %w", err)
	}
	return cloned, nil
}

func ivrMap(value any) (map[string]any, bool) {
	switch mapped := value.(type) {
	case map[string]any:
		return mapped, true
	case models.JSONB:
		return map[string]any(mapped), true
	case map[string]string:
		result := make(map[string]any, len(mapped))
		for key, value := range mapped {
			result[key] = value
		}
		return result, true
	default:
		return nil, false
	}
}

// ivrCallbackHeaderSlots returns only the three credential-bearing locations
// supported by IVR: a direct http_callback and a transfer's on_waiting and
// on_connect callbacks. The menu is expected to be a private clone because the
// setters mutate it in place.
func ivrCallbackHeaderSlots(menu models.JSONB, strict bool) ([]ivrCallbackHeaderSlot, error) {
	if menu == nil {
		return nil, nil
	}
	rawNodes, exists := menu["nodes"]
	if !exists {
		return nil, nil
	}
	nodes, ok := rawNodes.([]any)
	if !ok {
		if strict {
			return nil, errors.New("IVR flow nodes must be an array")
		}
		return nil, nil
	}

	var slots []ivrCallbackHeaderSlot
	for _, rawNode := range nodes {
		node, ok := ivrMap(rawNode)
		if !ok {
			continue
		}
		nodeID, _ := node["id"].(string)
		nodeType, _ := node["type"].(string)
		config, ok := ivrMap(node["config"])
		if !ok {
			continue
		}

		appendSlot := func(event string, callback map[string]any) error {
			rawHeaders, hasHeaders := callback["headers"]
			if !hasHeaders || rawHeaders == nil {
				return nil
			}
			headers, ok := ivrMap(rawHeaders)
			if !ok {
				if strict {
					return fmt.Errorf("node %q %s callback headers must be an object", nodeID, event)
				}
				// A malformed legacy value must never be echoed to an API response.
				callback["headers"] = map[string]any{}
				return nil
			}
			slots = append(slots, ivrCallbackHeaderSlot{
				nodeID: nodeID,
				event:  event,
				values: headers,
				set: func(replacement map[string]any) {
					callback["headers"] = replacement
				},
			})
			return nil
		}

		switch nodeType {
		case string(IVRNodeHTTPCallback):
			if err := appendSlot("http_callback", config); err != nil {
				return nil, err
			}
		case string(IVRNodeTransfer):
			for _, event := range []string{"on_waiting", "on_connect"} {
				callback, ok := ivrMap(config[event])
				if !ok {
					continue
				}
				if err := appendSlot(event, callback); err != nil {
					return nil, err
				}
			}
		}
	}
	return slots, nil
}

func ivrHeaderLocationKey(nodeID, event string) string {
	return nodeID + "\x00" + event
}

func canonicalIVRHeaderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func validIVRHeaderName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func encryptIVRHeaderValue(value, encryptionKey, headerName string) (string, error) {
	if strings.TrimSpace(encryptionKey) == "" {
		return "", fmt.Errorf("%w: %s", ErrIVRCallbackHeaderEncryptionUnavailable, headerName)
	}
	encrypted, err := appcrypto.Encrypt(value, encryptionKey)
	if err != nil || !appcrypto.IsEncrypted(encrypted) {
		return "", fmt.Errorf("failed to encrypt IVR callback header %q", headerName)
	}
	return encrypted, nil
}

// ProtectIVRCallbackHeaders prepares a complete IVR menu for persistence. Each
// incoming header map is a replacement, so omitted keys are deleted. A masked
// value preserves the matching stored value by node ID, callback event, and
// case-insensitive header name. Editing a legacy plaintext value migrates it to
// ciphertext without requiring the user to re-enter it.
func ProtectIVRCallbackHeaders(incoming, existing models.JSONB, encryptionKey string) (models.JSONB, error) {
	protected, err := cloneIVRMenu(incoming)
	if err != nil {
		return nil, err
	}
	existingClone, err := cloneIVRMenu(existing)
	if err != nil {
		return nil, err
	}

	existingByLocation := make(map[string]map[string]string)
	existingSlots, err := ivrCallbackHeaderSlots(existingClone, true)
	if err != nil {
		return nil, err
	}
	for _, slot := range existingSlots {
		location := ivrHeaderLocationKey(slot.nodeID, slot.event)
		if _, duplicate := existingByLocation[location]; duplicate {
			return nil, fmt.Errorf("stored IVR callback location %q/%s is duplicated", slot.nodeID, slot.event)
		}
		values := make(map[string]string, len(slot.values))
		for headerName, rawValue := range slot.values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("stored IVR callback header %q must have a string value", headerName)
			}
			canonicalName := canonicalIVRHeaderName(headerName)
			if _, duplicate := values[canonicalName]; duplicate {
				return nil, fmt.Errorf("stored IVR callback header %q is duplicated with different casing", headerName)
			}
			values[canonicalName] = value
		}
		existingByLocation[location] = values
	}

	incomingSlots, err := ivrCallbackHeaderSlots(protected, true)
	if err != nil {
		return nil, err
	}
	for _, slot := range incomingSlots {
		location := existingByLocation[ivrHeaderLocationKey(slot.nodeID, slot.event)]
		replacement := make(map[string]any, len(slot.values))
		seenNames := make(map[string]struct{}, len(slot.values))
		for headerName, rawValue := range slot.values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("IVR callback header %q must have a string value", headerName)
			}
			// The editor uses one blank row while a header is being added. It is
			// not a real header and must never be persisted.
			if strings.TrimSpace(headerName) == "" && value == "" {
				continue
			}
			if !validIVRHeaderName(headerName) {
				return nil, fmt.Errorf("IVR callback header name %q is invalid", headerName)
			}
			if strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("IVR callback header %q contains an invalid line break", headerName)
			}
			canonicalName := canonicalIVRHeaderName(headerName)
			if _, duplicate := seenNames[canonicalName]; duplicate {
				return nil, fmt.Errorf("IVR callback header %q is duplicated with different casing", headerName)
			}
			seenNames[canonicalName] = struct{}{}

			// Empty values are treated as deletion. This keeps every value that
			// reaches storage encrypted while preserving the editor's UX.
			if value == "" {
				continue
			}
			if value == IVRCallbackHeaderMask {
				existingValue, found := location[canonicalName]
				if !found || existingValue == "" {
					return nil, fmt.Errorf("%w: %s", ErrIVRCallbackHeaderMaskWithoutExisting, headerName)
				}
				if appcrypto.IsEncrypted(existingValue) {
					replacement[headerName] = existingValue
					continue
				}
				migrated, encryptErr := encryptIVRHeaderValue(existingValue, encryptionKey, headerName)
				if encryptErr != nil {
					return nil, encryptErr
				}
				replacement[headerName] = migrated
				continue
			}
			if appcrypto.IsEncrypted(value) {
				return nil, fmt.Errorf("IVR callback header %q must be supplied as plaintext or the mask", headerName)
			}
			encrypted, encryptErr := encryptIVRHeaderValue(value, encryptionKey, headerName)
			if encryptErr != nil {
				return nil, encryptErr
			}
			replacement[headerName] = encrypted
		}
		slot.set(replacement)
	}
	return protected, nil
}

// RedactIVRCallbackHeaders returns a private API-safe copy of a menu. Every
// value in all three callback header locations is replaced by the same mask;
// malformed legacy header objects are replaced with an empty object.
func RedactIVRCallbackHeaders(menu models.JSONB) models.JSONB {
	redacted, err := cloneIVRMenu(menu)
	if err != nil {
		return nil
	}
	slots, err := ivrCallbackHeaderSlots(redacted, false)
	if err != nil {
		return nil
	}
	for _, slot := range slots {
		masked := make(map[string]any, len(slot.values))
		for headerName := range slot.values {
			masked[headerName] = IVRCallbackHeaderMask
		}
		slot.set(masked)
	}
	return redacted
}

// ResolveIVRCallbackHeaders decrypts a stored header map immediately before a
// request is interpolated and sent. Legacy plaintext remains runtime-compatible.
// Ciphertext fails closed when the key is absent, wrong, or double-wrapped.
func ResolveIVRCallbackHeaders(raw any, encryptionKey string) (map[string]string, error) {
	headers, ok := ivrMap(raw)
	if raw == nil {
		return map[string]string{}, nil
	}
	if !ok {
		return nil, errors.New("IVR callback headers must be an object")
	}
	resolved := make(map[string]string, len(headers))
	for headerName, rawValue := range headers {
		value, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf("IVR callback header %q must have a string value", headerName)
		}
		if !appcrypto.IsEncrypted(value) {
			resolved[headerName] = value
			continue
		}
		if strings.TrimSpace(encryptionKey) == "" {
			return nil, fmt.Errorf("%w: %s", ErrIVRCallbackHeaderEncryptionUnavailable, headerName)
		}
		plaintext, err := appcrypto.Decrypt(value, encryptionKey)
		if err != nil || appcrypto.IsEncrypted(plaintext) {
			return nil, fmt.Errorf("IVR callback header %q could not be decrypted", headerName)
		}
		resolved[headerName] = plaintext
	}
	return resolved, nil
}

// EncryptIVRCallbackHeadersForCache makes a cache-only copy in which legacy
// plaintext headers are encrypted. It never mutates or backfills the database.
func EncryptIVRCallbackHeadersForCache(menu models.JSONB, encryptionKey string) (models.JSONB, error) {
	prepared, err := cloneIVRMenu(menu)
	if err != nil {
		return nil, err
	}
	slots, err := ivrCallbackHeaderSlots(prepared, true)
	if err != nil {
		return nil, err
	}
	for _, slot := range slots {
		replacement := make(map[string]any, len(slot.values))
		for headerName, rawValue := range slot.values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("IVR callback header %q must have a string value", headerName)
			}
			if value == "" {
				continue
			}
			if appcrypto.IsEncrypted(value) {
				replacement[headerName] = value
				continue
			}
			encrypted, encryptErr := encryptIVRHeaderValue(value, encryptionKey, headerName)
			if encryptErr != nil {
				return nil, encryptErr
			}
			replacement[headerName] = encrypted
		}
		slot.set(replacement)
	}
	return prepared, nil
}

// IVRCallbackHeadersAreEncrypted rejects stale caches produced before header
// vaulting. Headerless flows are valid. Every stored value must be ciphertext.
func IVRCallbackHeadersAreEncrypted(menu models.JSONB) bool {
	cloned, err := cloneIVRMenu(menu)
	if err != nil {
		return false
	}
	slots, err := ivrCallbackHeaderSlots(cloned, true)
	if err != nil {
		return false
	}
	for _, slot := range slots {
		for _, rawValue := range slot.values {
			value, ok := rawValue.(string)
			if !ok || !appcrypto.IsEncrypted(value) {
				return false
			}
		}
	}
	return true
}
