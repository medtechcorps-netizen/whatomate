package handlers

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ivrCallbackValidationMenu(nodeType string, config map[string]any) models.JSONB {
	return models.JSONB{
		"version":    2,
		"entry_node": "callback-node",
		"nodes": []any{
			map[string]any{
				"id":     "callback-node",
				"type":   nodeType,
				"config": config,
			},
		},
		"edges": []any{},
	}
}

func TestValidateFlowGraphAcceptsPublicHTTPSIVRCallbacks(t *testing.T) {
	t.Parallel()

	direct := ivrCallbackValidationMenu("http_callback", map[string]any{
		"url": "https://crm.example.test/ivr?call_id={{call_id}}",
	})
	require.NoError(t, validateFlowGraph(direct))

	transfer := ivrCallbackValidationMenu("transfer", map[string]any{
		"team_id": "example-team",
		"on_waiting": map[string]any{
			"url": "https://crm.example.test/calls/waiting",
		},
		"on_connect": map[string]any{
			"url": "https://crm.example.test/calls/connect?source={{source}}",
		},
	})
	require.NoError(t, validateFlowGraph(transfer))
}

func TestValidateFlowGraphRejectsUnsafeHTTPCallbackNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "missing URL", url: ""},
		{name: "plain HTTP", url: "http://crm.example.test/ivr"},
		{name: "loopback", url: "https://127.0.0.1/ivr"},
		{name: "private address", url: "https://10.0.0.2/ivr"},
		{name: "URL credentials", url: "https://user:password@crm.example.test/ivr"},
		{name: "host interpolation", url: "https://{{callback_host}}/ivr"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			menu := ivrCallbackValidationMenu("http_callback", map[string]any{"url": test.url})
			err := validateFlowGraph(menu)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "callback-node")
			if test.url != "" {
				assert.Contains(t, err.Error(), "public HTTPS URL")
			}
		})
	}
}

func TestValidateFlowGraphRejectsOutOfRangeHTTPCallbackTimeouts(t *testing.T) {
	t.Parallel()

	for name, timeout := range map[string]any{
		"zero":       float64(0),
		"negative":   float64(-1),
		"too large":  float64(31),
		"fractional": float64(1.5),
		"text":       "10",
	} {
		t.Run(name, func(t *testing.T) {
			menu := ivrCallbackValidationMenu("http_callback", map[string]any{
				"url":             "https://crm.example.test/ivr",
				"timeout_seconds": timeout,
			})
			err := validateFlowGraph(menu)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "timeout_seconds")
		})
	}

	for _, timeout := range []any{float64(1), float64(10), float64(30)} {
		menu := ivrCallbackValidationMenu("http_callback", map[string]any{
			"url":             "https://crm.example.test/ivr",
			"timeout_seconds": timeout,
		})
		require.NoError(t, validateFlowGraph(menu))
	}
}

func TestValidateFlowGraphRejectsUnsafeCallbackMethods(t *testing.T) {
	t.Parallel()

	direct := ivrCallbackValidationMenu("http_callback", map[string]any{
		"url":    "https://crm.example.test/ivr",
		"method": "CONNECT",
	})
	require.ErrorContains(t, validateFlowGraph(direct), "GET or POST")

	transfer := ivrCallbackValidationMenu("transfer", map[string]any{
		"on_connect": map[string]any{
			"url":    "https://crm.example.test/calls/connect",
			"method": "TRACE",
		},
	})
	require.ErrorContains(t, validateFlowGraph(transfer), "GET or POST")
}

func TestValidateFlowGraphRejectsUnsafeTransferCallbacks(t *testing.T) {
	t.Parallel()

	for _, event := range []string{"on_waiting", "on_connect"} {
		t.Run(event, func(t *testing.T) {
			menu := ivrCallbackValidationMenu("transfer", map[string]any{
				event: map[string]any{"url": "http://crm.example.test/call"},
			})
			err := validateFlowGraph(menu)
			require.Error(t, err)
			assert.Contains(t, err.Error(), event)
			assert.Contains(t, err.Error(), "public HTTPS URL")
		})
	}
}
