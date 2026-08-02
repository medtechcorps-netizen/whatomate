package calling

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ivrHeaderTestKey = "synthetic-ivr-header-vault-test-key"

func testIVRCallbackMenu(direct, waiting, connected map[string]any) models.JSONB {
	return models.JSONB{
		"version":    2,
		"entry_node": "direct",
		"nodes": []any{
			map[string]any{
				"id":   "direct",
				"type": "http_callback",
				"config": map[string]any{
					"url":     "https://crm.example.test/direct",
					"headers": direct,
				},
			},
			map[string]any{
				"id":   "transfer",
				"type": "transfer",
				"config": map[string]any{
					"on_waiting": map[string]any{
						"url":     "https://crm.example.test/waiting",
						"headers": waiting,
					},
					"on_connect": map[string]any{
						"url":     "https://crm.example.test/connected",
						"headers": connected,
					},
				},
			},
		},
		"edges": []any{},
	}
}

func callbackHeadersAt(t *testing.T, menu models.JSONB, nodeID, event string) map[string]any {
	t.Helper()
	cloned, err := cloneIVRMenu(menu)
	require.NoError(t, err)
	slots, err := ivrCallbackHeaderSlots(cloned, true)
	require.NoError(t, err)
	for _, slot := range slots {
		if slot.nodeID == nodeID && slot.event == event {
			return slot.values
		}
	}
	t.Fatalf("callback slot %s/%s was not found", nodeID, event)
	return nil
}

func requireNoIVRSecretLeak(t *testing.T, value any, secrets ...string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	serialized := string(encoded)
	for _, secret := range secrets {
		assert.NotContains(t, serialized, secret)
	}
	assert.NotContains(t, serialized, "enc:", "ciphertext must never be exposed")
	return serialized
}

func TestProtectIVRCallbackHeadersEncryptsAndMasksAllThreeLocations(t *testing.T) {
	t.Parallel()

	menu := testIVRCallbackMenu(
		map[string]any{"Authorization": "Bearer synthetic-direct"},
		map[string]any{"X-Wait-Key": "synthetic-waiting"},
		map[string]any{"Cookie": "session=synthetic-connected"},
	)
	protected, err := ProtectIVRCallbackHeaders(menu, nil, ivrHeaderTestKey)
	require.NoError(t, err)

	for _, location := range []struct {
		nodeID string
		event  string
		name   string
		want   string
	}{
		{nodeID: "direct", event: "http_callback", name: "Authorization", want: "Bearer synthetic-direct"},
		{nodeID: "transfer", event: "on_waiting", name: "X-Wait-Key", want: "synthetic-waiting"},
		{nodeID: "transfer", event: "on_connect", name: "Cookie", want: "session=synthetic-connected"},
	} {
		stored := callbackHeadersAt(t, protected, location.nodeID, location.event)
		ciphertext, ok := stored[location.name].(string)
		require.True(t, ok)
		assert.True(t, appcrypto.IsEncrypted(ciphertext))
		resolved, resolveErr := ResolveIVRCallbackHeaders(stored, ivrHeaderTestKey)
		require.NoError(t, resolveErr)
		assert.Equal(t, location.want, resolved[location.name])
	}

	redacted := RedactIVRCallbackHeaders(protected)
	serialized := requireNoIVRSecretLeak(t, redacted,
		"synthetic-direct", "synthetic-waiting", "synthetic-connected")
	assert.Equal(t, 3, strings.Count(serialized, IVRCallbackHeaderMask))

	// The caller-owned input is never mutated into ciphertext.
	assert.Equal(t, "Bearer synthetic-direct", callbackHeadersAt(t, menu, "direct", "http_callback")["Authorization"])
}

func TestProtectIVRCallbackHeadersPreservesMasksCaseInsensitivelyAndDeletesOmissions(t *testing.T) {
	t.Parallel()

	existing, err := ProtectIVRCallbackHeaders(testIVRCallbackMenu(
		map[string]any{"Authorization": "Bearer keep", "X-Delete": "remove-me"},
		map[string]any{"X-Wait-Key": "wait-keep"},
		map[string]any{"X-Connect-Key": "connect-delete"},
	), nil, ivrHeaderTestKey)
	require.NoError(t, err)
	directCiphertext := callbackHeadersAt(t, existing, "direct", "http_callback")["Authorization"]
	waitCiphertext := callbackHeadersAt(t, existing, "transfer", "on_waiting")["X-Wait-Key"]

	incoming := testIVRCallbackMenu(
		map[string]any{"authorization": IVRCallbackHeaderMask},
		map[string]any{"x-wait-key": IVRCallbackHeaderMask},
		map[string]any{},
	)
	updated, err := ProtectIVRCallbackHeaders(incoming, existing, "")
	require.NoError(t, err, "masked ciphertext preservation does not need to decrypt or rotate the value")

	direct := callbackHeadersAt(t, updated, "direct", "http_callback")
	assert.Equal(t, directCiphertext, direct["authorization"])
	assert.NotContains(t, direct, "X-Delete", "omitted keys must be deleted")
	waiting := callbackHeadersAt(t, updated, "transfer", "on_waiting")
	assert.Equal(t, waitCiphertext, waiting["x-wait-key"])
	assert.Empty(t, callbackHeadersAt(t, updated, "transfer", "on_connect"))
}

func TestProtectIVRCallbackHeadersMigratesLegacyPlaintextOnEdit(t *testing.T) {
	t.Parallel()

	legacy := testIVRCallbackMenu(
		map[string]any{"Authorization": "Bearer legacy-direct"},
		map[string]any{"X-Wait-Key": "legacy-waiting"},
		map[string]any{"Cookie": "legacy-connected"},
	)
	incoming := testIVRCallbackMenu(
		map[string]any{"authorization": IVRCallbackHeaderMask},
		map[string]any{"x-wait-key": IVRCallbackHeaderMask},
		map[string]any{"cookie": IVRCallbackHeaderMask},
	)

	migrated, err := ProtectIVRCallbackHeaders(incoming, legacy, ivrHeaderTestKey)
	require.NoError(t, err)
	assert.True(t, IVRCallbackHeadersAreEncrypted(migrated))
	serialized, err := json.Marshal(migrated)
	require.NoError(t, err)
	assert.NotContains(t, string(serialized), "legacy-direct")
	assert.NotContains(t, string(serialized), "legacy-waiting")
	assert.NotContains(t, string(serialized), "legacy-connected")

	_, err = ProtectIVRCallbackHeaders(incoming, legacy, "")
	require.ErrorIs(t, err, ErrIVRCallbackHeaderEncryptionUnavailable)
}

func TestIVRCallbackHeaderVaultFailsClosedForMissingAndWrongKeys(t *testing.T) {
	t.Parallel()

	menu := testIVRCallbackMenu(map[string]any{"Authorization": "Bearer new-secret"}, nil, nil)
	_, err := ProtectIVRCallbackHeaders(menu, nil, "")
	require.ErrorIs(t, err, ErrIVRCallbackHeaderEncryptionUnavailable)

	masked := testIVRCallbackMenu(map[string]any{"Authorization": IVRCallbackHeaderMask}, nil, nil)
	_, err = ProtectIVRCallbackHeaders(masked, nil, ivrHeaderTestKey)
	require.ErrorIs(t, err, ErrIVRCallbackHeaderMaskWithoutExisting)

	protected, err := ProtectIVRCallbackHeaders(menu, nil, ivrHeaderTestKey)
	require.NoError(t, err)
	stored := callbackHeadersAt(t, protected, "direct", "http_callback")
	_, err = ResolveIVRCallbackHeaders(stored, "")
	require.ErrorIs(t, err, ErrIVRCallbackHeaderEncryptionUnavailable)
	_, err = ResolveIVRCallbackHeaders(stored, "wrong-synthetic-key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be decrypted")

	legacyResolved, err := ResolveIVRCallbackHeaders(map[string]any{"Authorization": "Bearer legacy"}, "")
	require.NoError(t, err)
	assert.Equal(t, "Bearer legacy", legacyResolved["Authorization"])
}

func TestIVRFlowCachePayloadContainsOnlyCiphertext(t *testing.T) {
	t.Parallel()

	legacyMenu := testIVRCallbackMenu(
		map[string]any{"Authorization": "Bearer cache-direct"},
		map[string]any{"X-Wait-Key": "cache-waiting"},
		map[string]any{"Cookie": "cache-connected"},
	)
	assert.False(t, IVRCallbackHeadersAreEncrypted(legacyMenu))

	manager := &Manager{encryptionKey: ivrHeaderTestKey}
	payload, ok := manager.ivrFlowCachePayload(models.IVRFlow{Menu: legacyMenu})
	require.True(t, ok)
	assert.NotContains(t, string(payload), "cache-direct")
	assert.NotContains(t, string(payload), "cache-waiting")
	assert.NotContains(t, string(payload), "cache-connected")

	var cached models.IVRFlow
	require.NoError(t, json.Unmarshal(payload, &cached))
	assert.True(t, IVRCallbackHeadersAreEncrypted(cached.Menu))
	assert.False(t, IVRCallbackHeadersAreEncrypted(RedactIVRCallbackHeaders(cached.Menu)), "masked API data must never be accepted as a cache payload")

	withoutKey := &Manager{}
	_, ok = withoutKey.ivrFlowCachePayload(models.IVRFlow{Menu: legacyMenu})
	assert.False(t, ok, "legacy plaintext must remain uncached when it cannot be encrypted")
}

type recordingRedisHook struct {
	mu       sync.Mutex
	commands [][]string
}

func (hook *recordingRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (hook *recordingRedisHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, command redis.Cmder) error {
		args := command.Args()
		recorded := make([]string, len(args))
		for index, arg := range args {
			recorded[index] = strings.ToLower(strings.TrimSpace(toString(arg)))
		}
		hook.mu.Lock()
		hook.commands = append(hook.commands, recorded)
		hook.mu.Unlock()
		if intCommand, ok := command.(*redis.IntCmd); ok {
			intCommand.SetVal(1)
		}
		return nil
	}
}

func (hook *recordingRedisHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, _ []redis.Cmder) error { return nil }
}

func toString(value any) string {
	if stringValue, ok := value.(string); ok {
		return stringValue
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestInvalidateIVRFlowCacheDeletesIDAndBothRoutingKeys(t *testing.T) {
	t.Parallel()

	hook := &recordingRedisHook{}
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })

	manager := &Manager{redis: client}
	flowID := uuid.New()
	orgID := uuid.New()
	manager.InvalidateIVRFlowCache(flowID, orgID, "clinic-main")

	hook.mu.Lock()
	commands := append([][]string(nil), hook.commands...)
	hook.mu.Unlock()
	require.Len(t, commands, 3)
	assert.ElementsMatch(t, [][]string{
		{"del", strings.ToLower(ivrFlowCachePrefix + orgID.String() + ":" + flowID.String())},
		{"del", strings.ToLower(ivrFlowCfgCachePrefix + orgID.String() + ":clinic-main:call_start")},
		{"del", strings.ToLower(ivrFlowCfgCachePrefix + orgID.String() + ":clinic-main:outgoing_end")},
	}, commands)
}

func TestDirectAndTransferRuntimeDecryptOnlyAtDispatch(t *testing.T) {
	t.Parallel()

	directCiphertext, err := appcrypto.Encrypt("Bearer runtime-{{call_id}}", ivrHeaderTestKey)
	require.NoError(t, err)
	transferCiphertext, err := appcrypto.Encrypt("transfer-{{status}}", ivrHeaderTestKey)
	require.NoError(t, err)

	requests := make(chan *http.Request, 2)
	manager := &Manager{
		log:           testutil.NopLogger(),
		encryptionKey: ivrHeaderTestKey,
		httpClient: &http.Client{Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests <- request.Clone(request.Context())
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})},
	}

	directNode := &IVRNode{Config: map[string]any{
		"url": "https://crm.example.test/direct",
		"headers": map[string]any{
			"Authorization": directCiphertext,
		},
	}}
	outcome := manager.executeHTTPCallback(
		&CallSession{ID: "call-123"},
		directNode,
		&IVRContext{Variables: map[string]string{"call_id": "call-123"}},
	)
	assert.Equal(t, "http:2xx", outcome)
	directRequest := <-requests
	assert.Equal(t, "Bearer runtime-call-123", directRequest.Header.Get("Authorization"))

	manager.fireTransferCallback(
		&CallSession{ID: "call-123"},
		&TransferHTTPCallback{
			URL:     "https://crm.example.test/transfer",
			Headers: map[string]string{"X-Transfer-Key": transferCiphertext},
		},
		map[string]string{"status": "connected"},
	)
	select {
	case transferRequest := <-requests:
		assert.Equal(t, "transfer-connected", transferRequest.Header.Get("X-Transfer-Key"))
	case <-time.After(time.Second):
		t.Fatal("transfer callback was not dispatched")
	}
}

func TestRuntimeWrongKeyNeverDispatchesCredential(t *testing.T) {
	t.Parallel()

	ciphertext, err := appcrypto.Encrypt("Bearer must-not-leave", ivrHeaderTestKey)
	require.NoError(t, err)
	var requestCount atomic.Int32
	requestAttempted := make(chan struct{}, 1)
	manager := &Manager{
		log:           testutil.NopLogger(),
		encryptionKey: "wrong-key",
		httpClient: &http.Client{Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount.Add(1)
			requestAttempted <- struct{}{}
			return nil, assert.AnError
		})},
	}
	node := &IVRNode{Config: map[string]any{
		"url":     "https://crm.example.test/direct",
		"headers": map[string]any{"Authorization": ciphertext},
	}}
	assert.Equal(t, "http:non2xx", manager.executeHTTPCallback(
		&CallSession{ID: "call-closed"}, node, &IVRContext{Variables: map[string]string{}},
	))
	assert.Zero(t, requestCount.Load())

	manager.fireTransferCallback(
		&CallSession{ID: "call-closed"},
		&TransferHTTPCallback{URL: "https://crm.example.test/transfer", Headers: map[string]string{"Authorization": ciphertext}},
		map[string]string{},
	)
	select {
	case <-requestAttempted:
		t.Fatal("transfer callback dispatched with an undecryptable credential")
	case <-time.After(100 * time.Millisecond):
	}
	assert.Zero(t, requestCount.Load())
}
