package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
)

// Chatbot API-context and flow-node headers are intentionally all treated as
// credentials. A harmless-looking custom header can carry an API key just as
// easily as Authorization, so deciding sensitivity from the name would leave
// an avoidable plaintext-storage gap in these tenant-authored configurations.
func (a *App) protectChatbotHeaders(incoming map[string]string, existing any) (models.JSONB, error) {
	existingHeaders, err := outboundHeaderStrings(existing)
	if err != nil {
		return nil, err
	}
	existingByName := make(map[string]string, len(existingHeaders))
	for name, value := range existingHeaders {
		existingByName[strings.ToLower(strings.TrimSpace(name))] = value
	}

	encryptionKey := a.outboundHeaderEncryptionKey()
	protected := make(map[string]string, len(incoming))
	seenNames := make(map[string]struct{}, len(incoming))
	for name, incomingValue := range incoming {
		if !validChatbotHeaderName(name) {
			return nil, fmt.Errorf("invalid chatbot outbound header name %q", name)
		}
		normalizedName := strings.ToLower(name)
		if _, duplicate := seenNames[normalizedName]; duplicate {
			return nil, fmt.Errorf("duplicate chatbot outbound header name %q", name)
		}
		seenNames[normalizedName] = struct{}{}
		existingValue, hasExisting := existingByName[normalizedName]
		if incomingValue == outboundHeaderMask {
			if !hasExisting {
				return nil, fmt.Errorf("%w: %s", errOutboundHeaderMaskWithoutExisting, name)
			}
			if existingValue == "" || appcrypto.IsEncrypted(existingValue) {
				protected[name] = existingValue
				continue
			}
			// Migrate a legacy plaintext value whenever its masked form is
			// submitted by an editor.
			incomingValue = existingValue
		} else if hasExisting && incomingValue == existingValue && appcrypto.IsEncrypted(existingValue) {
			protected[name] = existingValue
			continue
		}

		if incomingValue == "" {
			protected[name] = ""
			continue
		}
		if !validChatbotHeaderValue(incomingValue) {
			return nil, fmt.Errorf("chatbot outbound header %q contains an invalid value", name)
		}
		if strings.TrimSpace(encryptionKey) == "" {
			return nil, fmt.Errorf("%w: %s", errOutboundHeaderEncryptionUnavailable, name)
		}
		encrypted, encryptErr := appcrypto.Encrypt(incomingValue, encryptionKey)
		if encryptErr != nil {
			return nil, fmt.Errorf("failed to encrypt chatbot outbound header %q: %w", name, encryptErr)
		}
		if !appcrypto.IsEncrypted(encrypted) {
			return nil, fmt.Errorf("failed to encrypt chatbot outbound header %q", name)
		}
		protected[name] = encrypted
	}

	return outboundHeaderJSONB(protected), nil
}

func validChatbotHeaderName(name string) bool {
	if name == "" {
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

func validChatbotHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (a *App) resolveChatbotHeaders(raw any) (map[string]string, error) {
	stored, err := outboundHeaderStrings(raw)
	if err != nil {
		return nil, err
	}

	encryptionKey := a.outboundHeaderEncryptionKey()
	resolved := make(map[string]string, len(stored))
	for name, storedValue := range stored {
		if !appcrypto.IsEncrypted(storedValue) {
			// Read-only compatibility for legacy rows. New and updated values
			// always pass through protectChatbotHeaders first.
			resolved[name] = storedValue
			continue
		}
		if strings.TrimSpace(encryptionKey) == "" {
			return nil, fmt.Errorf("%w: %s", errOutboundHeaderEncryptionUnavailable, name)
		}
		plaintext, decryptErr := appcrypto.Decrypt(storedValue, encryptionKey)
		if decryptErr != nil || appcrypto.IsEncrypted(plaintext) {
			return nil, fmt.Errorf("chatbot outbound header %q could not be decrypted", name)
		}
		resolved[name] = plaintext
	}
	return resolved, nil
}

func redactChatbotHeaders(raw any) map[string]string {
	stored, err := outboundHeaderStrings(raw)
	if err != nil {
		return map[string]string{}
	}
	redacted := make(map[string]string, len(stored))
	for name := range stored {
		redacted[name] = outboundHeaderMask
	}
	return redacted
}

func cloneChatbotJSONB(raw models.JSONB) (models.JSONB, error) {
	if raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var cloned models.JSONB
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func chatbotConfigMap(raw any) (map[string]any, error) {
	switch value := raw.(type) {
	case nil:
		return map[string]any{}, nil
	case models.JSONB:
		return map[string]any(value), nil
	case map[string]any:
		return value, nil
	default:
		return nil, errors.New("chatbot outbound configuration must be an object")
	}
}

func validateConfiguredChatbotURL(config map[string]any) error {
	rawURL, exists := config["url"]
	if !exists || rawURL == nil {
		return nil
	}
	apiURL, ok := rawURL.(string)
	if !ok {
		return errors.New("chatbot outbound URL must be a string")
	}
	if strings.TrimSpace(apiURL) == "" {
		return nil
	}
	if err := validateWebhookURL(apiURL); err != nil {
		return fmt.Errorf("invalid chatbot outbound URL: %w", err)
	}
	return nil
}

func requireConfiguredChatbotURL(config models.JSONB, owner string) error {
	rawURL, ok := config["url"].(string)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("%s URL is required", owner)
	}
	return validateConfiguredChatbotURL(map[string]any(config))
}

func validateChatbotCompletionAction(action string, config models.JSONB) error {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "none":
		return nil
	case "webhook":
		return requireConfiguredChatbotURL(config, "completion webhook")
	default:
		return errors.New("on_complete_action must be none or webhook")
	}
}

func validateAIContextOutboundConfig(contextType models.ContextType, config models.JSONB) error {
	switch contextType {
	case models.ContextTypeStatic:
		return nil
	case models.ContextTypeAPI:
		return requireConfiguredChatbotURL(config, "API context")
	default:
		return errors.New("context_type must be static or api")
	}
}

func configuredChatbotMethod(config map[string]any) (string, error) {
	rawMethod, exists := config["method"]
	if !exists || rawMethod == nil {
		return http.MethodGet, nil
	}
	method, ok := rawMethod.(string)
	if !ok {
		return "", errors.New("chatbot outbound method must be a string")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return http.MethodGet, nil
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch:
		return method, nil
	default:
		return "", errors.New("chatbot outbound method must be GET, POST, PUT, or PATCH")
	}
}

func (a *App) protectChatbotAPIConfig(incoming, existing models.JSONB) (models.JSONB, error) {
	protected, err := cloneChatbotJSONB(incoming)
	if err != nil {
		return nil, fmt.Errorf("invalid chatbot API configuration: %w", err)
	}
	if protected == nil {
		return nil, nil
	}
	if err := validateConfiguredChatbotURL(map[string]any(protected)); err != nil {
		return nil, err
	}
	method, err := configuredChatbotMethod(map[string]any(protected))
	if err != nil {
		return nil, err
	}
	if _, hasMethod := protected["method"]; hasMethod {
		protected["method"] = method
	}

	rawHeaders, hasHeaders := protected["headers"]
	if !hasHeaders {
		return protected, nil
	}
	incomingHeaders, err := outboundHeaderStrings(rawHeaders)
	if err != nil {
		return nil, err
	}
	var existingHeaders any
	if existing != nil {
		existingHeaders = existing["headers"]
	}
	protectedHeaders, err := a.protectChatbotHeaders(incomingHeaders, existingHeaders)
	if err != nil {
		return nil, err
	}
	protected["headers"] = protectedHeaders
	return protected, nil
}

func redactChatbotAPIConfig(config models.JSONB) models.JSONB {
	if config == nil {
		return nil
	}
	redacted, err := cloneChatbotJSONB(config)
	if err != nil {
		return models.JSONB{}
	}
	if rawHeaders, ok := redacted["headers"]; ok {
		redacted["headers"] = redactChatbotHeaders(rawHeaders)
	}
	return redacted
}

func chatbotFlowNodeConfigKey(nodeType, nodeID string) string {
	return nodeType + "\x00" + nodeID
}

func chatbotFlowNodeConfigs(graph models.JSONB) (map[string]models.JSONB, error) {
	configs := make(map[string]models.JSONB)
	if graph == nil {
		return configs, nil
	}
	normalized, err := cloneChatbotJSONB(graph)
	if err != nil {
		return nil, err
	}
	nodes, ok := normalized["nodes"].([]any)
	if !ok {
		if normalized["nodes"] == nil {
			return configs, nil
		}
		return nil, errors.New("chatbot flow graph nodes must be an array")
	}
	for _, rawNode := range nodes {
		node, err := chatbotConfigMap(rawNode)
		if err != nil {
			return nil, err
		}
		nodeType, _ := node["type"].(string)
		if nodeType != string(ChatNodeAPICall) && nodeType != string(ChatNodeWebhook) {
			continue
		}
		nodeID, _ := node["id"].(string)
		if strings.TrimSpace(nodeID) == "" {
			return nil, errors.New("chatbot outbound flow node must have an ID")
		}
		config, err := chatbotConfigMap(node["config"])
		if err != nil {
			return nil, err
		}
		configKey := chatbotFlowNodeConfigKey(nodeType, nodeID)
		if _, duplicate := configs[configKey]; duplicate {
			return nil, fmt.Errorf("duplicate chatbot outbound flow node ID %q", nodeID)
		}
		configs[configKey] = models.JSONB(config)
	}
	return configs, nil
}

func (a *App) protectChatbotFlowGraph(incoming, existing models.JSONB) (models.JSONB, error) {
	protected, err := cloneChatbotJSONB(incoming)
	if err != nil {
		return nil, fmt.Errorf("invalid chatbot flow graph: %w", err)
	}
	if protected == nil {
		return nil, nil
	}
	existingConfigs, err := chatbotFlowNodeConfigs(existing)
	if err != nil {
		return nil, err
	}
	nodes, ok := protected["nodes"].([]any)
	if !ok {
		if protected["nodes"] == nil {
			return protected, nil
		}
		return nil, errors.New("chatbot flow graph nodes must be an array")
	}
	for _, rawNode := range nodes {
		node, err := chatbotConfigMap(rawNode)
		if err != nil {
			return nil, err
		}
		nodeType, _ := node["type"].(string)
		if nodeType != string(ChatNodeAPICall) && nodeType != string(ChatNodeWebhook) {
			continue
		}
		nodeID, _ := node["id"].(string)
		if strings.TrimSpace(nodeID) == "" {
			return nil, errors.New("chatbot outbound flow node must have an ID")
		}
		config, err := chatbotConfigMap(node["config"])
		if err != nil {
			return nil, err
		}
		configKey := chatbotFlowNodeConfigKey(nodeType, nodeID)
		protectedConfig, err := a.protectChatbotAPIConfig(models.JSONB(config), existingConfigs[configKey])
		if err != nil {
			return nil, fmt.Errorf("chatbot flow node %q: %w", nodeID, err)
		}
		node["config"] = protectedConfig
	}
	return protected, nil
}

func redactChatbotFlowGraph(graph models.JSONB) models.JSONB {
	if graph == nil {
		return nil
	}
	redacted, err := cloneChatbotJSONB(graph)
	if err != nil {
		return models.JSONB{}
	}
	nodes, ok := redacted["nodes"].([]any)
	if !ok {
		return redacted
	}
	for _, rawNode := range nodes {
		node, err := chatbotConfigMap(rawNode)
		if err != nil {
			continue
		}
		nodeType, _ := node["type"].(string)
		if nodeType != string(ChatNodeAPICall) && nodeType != string(ChatNodeWebhook) {
			continue
		}
		config, err := chatbotConfigMap(node["config"])
		if err != nil {
			continue
		}
		node["config"] = redactChatbotAPIConfig(models.JSONB(config))
	}
	return redacted
}

func redactChatbotFlow(flow models.ChatbotFlow) models.ChatbotFlow {
	flow.Graph = redactChatbotFlowGraph(flow.Graph)
	flow.CompletionConfig = redactChatbotAPIConfig(flow.CompletionConfig)
	return flow
}

// chatbotOutboundConfigsCacheSafe prevents legacy plaintext header values from
// being copied into Redis. Legacy database rows remain executable, but are not
// cached until an ordinary edit migrates them to encrypted storage.
func chatbotOutboundConfigsCacheSafe(configs ...models.JSONB) bool {
	for _, config := range configs {
		if config == nil {
			continue
		}
		headers, err := outboundHeaderStrings(config["headers"])
		if err != nil {
			return false
		}
		for _, value := range headers {
			if value != "" && !appcrypto.IsEncrypted(value) {
				return false
			}
		}
	}
	return true
}

func chatbotFlowCacheSafe(flow models.ChatbotFlow) bool {
	configs, err := chatbotFlowNodeConfigs(flow.Graph)
	if err != nil || !chatbotOutboundConfigsCacheSafe(flow.CompletionConfig) {
		return false
	}
	for _, config := range configs {
		if !chatbotOutboundConfigsCacheSafe(config) {
			return false
		}
	}
	return true
}

func (a *App) chatbotRequestClient() (*http.Client, error) {
	if a == nil || a.HTTPClient == nil {
		return nil, errors.New("chatbot outbound HTTP client is unavailable")
	}
	if a.HTTPClient.Transport == nil {
		return nil, errors.New("chatbot outbound SSRF-safe transport is unavailable")
	}
	// Copying the injected client retains its SSRF-safe transport and pooling,
	// while making the no-redirect policy local to tenant-authored requests.
	client := *a.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client, nil
}
