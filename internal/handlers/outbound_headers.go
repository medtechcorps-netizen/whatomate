package handlers

import (
	"errors"
	"fmt"
	"strings"

	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"golang.org/x/net/http/httpguts"
)

const outboundHeaderMask = "********"

var (
	errOutboundHeaderEncryptionUnavailable = errors.New("outbound credential encryption is unavailable")
	errOutboundHeaderMaskWithoutExisting   = errors.New("masked outbound header has no existing value")
)

func outboundHeaderStrings(raw any) (map[string]string, error) {
	result := make(map[string]string)
	switch headers := raw.(type) {
	case nil:
		return result, nil
	case map[string]string:
		for key, value := range headers {
			result[key] = value
		}
		return result, nil
	case models.JSONB:
		for key, value := range headers {
			stringValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("outbound header %q must have a string value", key)
			}
			result[key] = stringValue
		}
		return result, nil
	case map[string]any:
		for key, value := range headers {
			stringValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("outbound header %q must have a string value", key)
			}
			result[key] = stringValue
		}
		return result, nil
	default:
		return nil, errors.New("outbound headers must be an object of string values")
	}
}

func outboundHeaderJSONB(headers map[string]string) models.JSONB {
	result := make(models.JSONB, len(headers))
	for key, value := range headers {
		result[key] = value
	}
	return result
}

func (a *App) outboundHeaderEncryptionKey() string {
	if a == nil || a.Config == nil {
		return ""
	}
	if strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
		return ""
	}
	return a.Config.App.EncryptionKey
}

// protectOutboundHeaders prepares an entire header map for storage. Every
// non-empty value is protected because third-party credentials can be carried
// under arbitrary custom names. The map is deliberately treated as a
// replacement: keys omitted by the caller are deleted. A masked value
// preserves the matching existing value case-insensitively, allowing ordinary
// edit round-trips without exposing or rotating credentials.
func (a *App) protectOutboundHeaders(incoming map[string]string, existing models.JSONB) (models.JSONB, error) {
	existingHeaders, err := outboundHeaderStrings(existing)
	if err != nil {
		return nil, err
	}
	existingByName := make(map[string]string, len(existingHeaders))
	for key, value := range existingHeaders {
		canonicalName := strings.ToLower(strings.TrimSpace(key))
		if _, duplicate := existingByName[canonicalName]; duplicate {
			return nil, fmt.Errorf("outbound header %q is duplicated case-insensitively", key)
		}
		existingByName[canonicalName] = value
	}

	key := a.outboundHeaderEncryptionKey()
	protected := make(map[string]string, len(incoming))
	seenNames := make(map[string]struct{}, len(incoming))
	for headerName, incomingValue := range incoming {
		if !httpguts.ValidHeaderFieldName(headerName) {
			return nil, fmt.Errorf("outbound header name %q is invalid", headerName)
		}
		canonicalName := strings.ToLower(headerName)
		if _, duplicate := seenNames[canonicalName]; duplicate {
			return nil, fmt.Errorf("outbound header %q is duplicated case-insensitively", headerName)
		}
		seenNames[canonicalName] = struct{}{}
		if !httpguts.ValidHeaderFieldValue(incomingValue) {
			return nil, fmt.Errorf("outbound header %q contains an invalid value", headerName)
		}
		existingValue, hasExisting := existingByName[canonicalName]
		if incomingValue == outboundHeaderMask {
			if !hasExisting {
				return nil, fmt.Errorf("%w: %s", errOutboundHeaderMaskWithoutExisting, headerName)
			}
			if existingValue == "" || appcrypto.IsEncrypted(existingValue) || key == "" {
				protected[headerName] = existingValue
				continue
			}
			// An edit of a legacy plaintext value is a safe opportunity to
			// migrate it without asking the user to re-enter the credential.
			incomingValue = existingValue
		} else if hasExisting && incomingValue == existingValue {
			if existingValue == "" || appcrypto.IsEncrypted(existingValue) || key == "" {
				protected[headerName] = existingValue
				continue
			}
		}

		if incomingValue == "" {
			protected[headerName] = ""
			continue
		}
		if appcrypto.IsEncrypted(incomingValue) {
			return nil, fmt.Errorf("outbound header %q must be supplied as plaintext or the mask", headerName)
		}
		if key == "" {
			return nil, fmt.Errorf("%w: %s", errOutboundHeaderEncryptionUnavailable, headerName)
		}
		encrypted, encryptErr := appcrypto.Encrypt(incomingValue, key)
		if encryptErr != nil || !appcrypto.IsEncrypted(encrypted) {
			return nil, fmt.Errorf("failed to encrypt outbound header %q: %w", headerName, encryptErr)
		}
		protected[headerName] = encrypted
	}

	return outboundHeaderJSONB(protected), nil
}

// resolveOutboundHeaders returns the values to put on an outbound request.
// Legacy plaintext values remain readable; ciphertext is decrypted only here.
func (a *App) resolveOutboundHeaders(raw any) (map[string]string, error) {
	stored, err := outboundHeaderStrings(raw)
	if err != nil {
		return nil, err
	}
	key := a.outboundHeaderEncryptionKey()
	resolved := make(map[string]string, len(stored))
	seenNames := make(map[string]struct{}, len(stored))
	for headerName, storedValue := range stored {
		if !httpguts.ValidHeaderFieldName(headerName) {
			return nil, fmt.Errorf("stored outbound header name %q is invalid", headerName)
		}
		canonicalName := strings.ToLower(headerName)
		if _, duplicate := seenNames[canonicalName]; duplicate {
			return nil, fmt.Errorf("stored outbound header %q is duplicated case-insensitively", headerName)
		}
		seenNames[canonicalName] = struct{}{}
		if !appcrypto.IsEncrypted(storedValue) {
			if !httpguts.ValidHeaderFieldValue(storedValue) {
				return nil, fmt.Errorf("stored outbound header %q contains an invalid value", headerName)
			}
			resolved[headerName] = storedValue
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("%w: %s", errOutboundHeaderEncryptionUnavailable, headerName)
		}
		plaintext, decryptErr := appcrypto.Decrypt(storedValue, key)
		if decryptErr != nil || appcrypto.IsEncrypted(plaintext) {
			return nil, fmt.Errorf("outbound header %q could not be decrypted", headerName)
		}
		if !httpguts.ValidHeaderFieldValue(plaintext) {
			return nil, fmt.Errorf("outbound header %q contains an invalid value", headerName)
		}
		resolved[headerName] = plaintext
	}
	return resolved, nil
}

// redactOutboundHeaders produces an API-safe view. Every non-empty value uses
// one stable mask, so neither legacy plaintext nor ciphertext can leave the
// service regardless of the custom header name selected by an administrator.
func redactOutboundHeaders(raw any) map[string]string {
	stored, err := outboundHeaderStrings(raw)
	if err != nil {
		return map[string]string{}
	}
	redacted := make(map[string]string, len(stored))
	for headerName, storedValue := range stored {
		if storedValue != "" {
			redacted[headerName] = outboundHeaderMask
			continue
		}
		redacted[headerName] = storedValue
	}
	return redacted
}

func cloneActionConfig(config map[string]any) map[string]any {
	cloned := make(map[string]any, len(config))
	for key, value := range config {
		cloned[key] = value
	}
	return cloned
}

func (a *App) protectWebhookActionConfig(incoming map[string]any, existing models.JSONB) (models.JSONB, error) {
	protected := cloneActionConfig(incoming)
	rawHeaders, hasHeaders := incoming["headers"]
	if !hasHeaders {
		return models.JSONB(protected), nil
	}

	incomingHeaders, err := outboundHeaderStrings(rawHeaders)
	if err != nil {
		return nil, err
	}
	var existingHeaders models.JSONB
	if existing != nil {
		parsedExisting, parseErr := outboundHeaderStrings(existing["headers"])
		if parseErr != nil {
			return nil, parseErr
		}
		existingHeaders = outboundHeaderJSONB(parsedExisting)
	}
	storedHeaders, err := a.protectOutboundHeaders(incomingHeaders, existingHeaders)
	if err != nil {
		return nil, err
	}
	protected["headers"] = storedHeaders
	return models.JSONB(protected), nil
}

func redactCustomActionConfig(config models.JSONB) map[string]any {
	redacted := cloneActionConfig(map[string]any(config))
	if rawHeaders, ok := config["headers"]; ok {
		redacted["headers"] = redactOutboundHeaders(rawHeaders)
	}
	return redacted
}
