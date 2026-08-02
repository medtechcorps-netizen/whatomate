package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ivrResponseTestKey = "synthetic-ivr-response-redaction-test-key"

func ivrResponseTestMenu(directValue, waitingValue, connectedValue string) models.JSONB {
	return models.JSONB{
		"version":    2,
		"entry_node": "direct",
		"nodes": []any{
			map[string]any{
				"id":   "direct",
				"type": "http_callback",
				"config": map[string]any{
					"url":     "https://crm.example.test/direct",
					"headers": map[string]any{"Authorization": directValue},
				},
			},
			map[string]any{
				"id":   "transfer",
				"type": "transfer",
				"config": map[string]any{
					"on_waiting": map[string]any{
						"url":     "https://crm.example.test/waiting",
						"headers": map[string]any{"X-Wait-Key": waitingValue},
					},
					"on_connect": map[string]any{
						"url":     "https://crm.example.test/connected",
						"headers": map[string]any{"Cookie": connectedValue},
					},
				},
			},
		},
		"edges": []any{},
	}
}

func assertNoIVRResponseLeak(t *testing.T, value any, secrets ...string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	serialized := string(encoded)
	for _, secret := range secrets {
		assert.NotContains(t, serialized, secret)
	}
	assert.NotContains(t, serialized, "enc:")
	return serialized
}

func TestIVRFlowAndCallLogResponsesMaskEveryCallbackHeader(t *testing.T) {
	t.Parallel()

	menu := ivrResponseTestMenu("Bearer response-direct", "response-waiting", "response-connected")
	protected, err := calling.ProtectIVRCallbackHeaders(menu, nil, ivrResponseTestKey)
	require.NoError(t, err)
	flow := models.IVRFlow{BaseModel: models.BaseModel{ID: uuid.New()}, Menu: protected}

	redactedFlow := redactIVRFlowForResponse(flow)
	serialized := assertNoIVRResponseLeak(t, redactedFlow,
		"response-direct", "response-waiting", "response-connected")
	assert.Equal(t, 3, strings.Count(serialized, calling.IVRCallbackHeaderMask))

	callLog := models.CallLog{IVRFlow: &flow}
	callLog.IVRFlow = redactIVRFlowPointerForResponse(callLog.IVRFlow)
	serialized = assertNoIVRResponseLeak(t, callLog,
		"response-direct", "response-waiting", "response-connected")
	assert.Equal(t, 3, strings.Count(serialized, calling.IVRCallbackHeaderMask))

	// Redaction operates on private copies; the persistence model remains
	// ciphertext so caches and runtime never receive API masks.
	storedJSON, err := json.Marshal(flow)
	require.NoError(t, err)
	assert.Contains(t, string(storedJSON), "enc:")
}

func TestIVRAuditDiffsAndHistoricalResponsesNeverLeakHeaders(t *testing.T) {
	t.Parallel()

	oldMenu, err := calling.ProtectIVRCallbackHeaders(
		ivrResponseTestMenu("Bearer old-direct", "old-waiting", "old-connected"),
		nil,
		ivrResponseTestKey,
	)
	require.NoError(t, err)
	newMenu, err := calling.ProtectIVRCallbackHeaders(
		ivrResponseTestMenu("Bearer new-direct", "new-waiting", "new-connected"),
		nil,
		ivrResponseTestKey,
	)
	require.NoError(t, err)

	oldRedacted := calling.RedactIVRCallbackHeaders(oldMenu)
	newRedacted := calling.RedactIVRCallbackHeaders(newMenu)
	diffs := diffIVRMenuNodes(nil, oldRedacted, newRedacted)
	assertNoIVRResponseLeak(t, diffs,
		"old-direct", "old-waiting", "old-connected",
		"new-direct", "new-waiting", "new-connected")

	ciphertext, err := appcrypto.Encrypt("legacy-audit-ciphertext", ivrResponseTestKey)
	require.NoError(t, err)
	legacyChanges := models.JSONBArray{
		map[string]any{
			"field":     "Callback -> headers[Authorization]",
			"old_value": "Bearer legacy-audit-plain",
			"new_value": ciphertext,
		},
		map[string]any{
			"field":     "Transfer -> on_waiting",
			"old_value": nil,
			"new_value": map[string]any{
				"url": "https://crm.example.test/waiting",
				"headers": map[string]any{
					"X-API-Key": "legacy-nested-plain",
				},
			},
		},
	}
	redactedChanges := auditChangesForResponse(ivrFlowAuditResourceType, legacyChanges)
	serialized := assertNoIVRResponseLeak(t, redactedChanges,
		"legacy-audit-plain", "legacy-audit-ciphertext", "legacy-nested-plain")
	assert.Contains(t, serialized, calling.IVRCallbackHeaderMask)

	// The response scrubber never mutates an audit row loaded from storage.
	originalJSON, err := json.Marshal(legacyChanges)
	require.NoError(t, err)
	assert.Contains(t, string(originalJSON), "legacy-audit-plain")
}
