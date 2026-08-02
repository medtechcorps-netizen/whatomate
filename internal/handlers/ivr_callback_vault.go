package handlers

import (
	"encoding/json"
	"strings"

	"github.com/shridarpatil/whatomate/internal/calling"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
)

const ivrFlowAuditResourceType = "ivr_flow"

func (a *App) ivrEncryptionKey() string {
	if a == nil || a.Config == nil {
		return ""
	}
	return a.Config.App.EncryptionKey
}

func redactIVRFlowForResponse(flow models.IVRFlow) models.IVRFlow {
	flow.Menu = calling.RedactIVRCallbackHeaders(flow.Menu)
	return flow
}

func redactIVRFlowPointerForResponse(flow *models.IVRFlow) *models.IVRFlow {
	if flow == nil {
		return nil
	}
	redacted := redactIVRFlowForResponse(*flow)
	return &redacted
}

// redactIVRAuditChanges is also applied when reading historical audit rows, so
// legacy plaintext and any ciphertext written by an older build cannot leave
// the service through the audit API.
func redactIVRAuditChanges(changes models.JSONBArray) models.JSONBArray {
	encoded, err := json.Marshal(changes)
	if err != nil {
		return models.JSONBArray{}
	}
	var cloned models.JSONBArray
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return models.JSONBArray{}
	}

	for index, rawChange := range cloned {
		change, ok := rawChange.(map[string]any)
		if !ok {
			cloned[index] = redactIVRAuditValue(rawChange)
			continue
		}
		field, _ := change["field"].(string)
		if strings.Contains(strings.ToLower(field), "header") {
			if _, exists := change["old_value"]; exists && change["old_value"] != nil {
				change["old_value"] = calling.IVRCallbackHeaderMask
			}
			if _, exists := change["new_value"]; exists && change["new_value"] != nil {
				change["new_value"] = calling.IVRCallbackHeaderMask
			}
			continue
		}
		if oldValue, exists := change["old_value"]; exists {
			change["old_value"] = redactIVRAuditValue(oldValue)
		}
		if newValue, exists := change["new_value"]; exists {
			change["new_value"] = redactIVRAuditValue(newValue)
		}
	}
	return cloned
}

func redactIVRAuditValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, nested := range typed {
			if strings.EqualFold(strings.TrimSpace(key), "headers") {
				result[key] = redactIVRHeaderObject(nested)
				continue
			}
			result[key] = redactIVRAuditValue(nested)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, nested := range typed {
			result[index] = redactIVRAuditValue(nested)
		}
		return result
	case string:
		if appcrypto.IsEncrypted(typed) {
			return calling.IVRCallbackHeaderMask
		}
		return typed
	default:
		return value
	}
}

func redactIVRHeaderObject(value any) any {
	headers, ok := value.(map[string]any)
	if !ok {
		return calling.IVRCallbackHeaderMask
	}
	redacted := make(map[string]any, len(headers))
	for name := range headers {
		redacted[name] = calling.IVRCallbackHeaderMask
	}
	return redacted
}
