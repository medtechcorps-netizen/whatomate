package qwen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultBaseURL  = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
	DefaultModel    = "qwen3.7-plus"
	RegionSingapore = "singapore"
	RegionUS        = "us"
	RegionBeijing   = "china_beijing"

	maxResponseBytes = 4 << 20
)

// BaseURLForRegion maps the only tenant-selectable Qwen endpoint regions to
// Alibaba Cloud's official shared OpenAI-compatible endpoints. Arbitrary URLs
// are intentionally not accepted because every request carries a tenant API
// key and customer conversation content.
func BaseURLForRegion(region string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case RegionSingapore:
		return DefaultBaseURL, nil
	case RegionUS:
		return "https://dashscope-us.aliyuncs.com/compatible-mode/v1", nil
	case RegionBeijing:
		return "https://dashscope.aliyuncs.com/compatible-mode/v1", nil
	default:
		return "", errors.New("unsupported Qwen endpoint region")
	}
}

// Message is one OpenAI-compatible chat-completion turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options contains the tenant-resolved settings needed for a Qwen request.
// Callers are responsible for applying product-specific token limits before
// invoking Generate.
type Options struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	Messages    []Message
}

// Generate calls Alibaba Cloud Model Studio's OpenAI-compatible Qwen endpoint.
// Thinking is disabled because customer-service replies must be short and
// predictable. The caller controls the transaction boundary; this function
// performs network I/O and must never be called from a database transaction.
func Generate(ctx context.Context, client *http.Client, options Options) (string, error) {
	if ctx == nil {
		return "", errors.New("qwen request context is required")
	}
	if strings.TrimSpace(options.APIKey) == "" {
		return "", errors.New("qwen API key is required")
	}
	if len(options.Messages) == 0 {
		return "", errors.New("qwen messages are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	baseURL := strings.TrimSpace(options.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	model := strings.TrimSpace(options.Model)
	if model == "" {
		model = DefaultModel
	}
	payload := map[string]any{
		"model":           model,
		"messages":        options.Messages,
		"max_tokens":      options.MaxTokens,
		"enable_thinking": false,
	}
	if options.Temperature > 0 {
		payload["temperature"] = options.Temperature
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode Qwen request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/chat/completions",
		bytes.NewReader(encoded),
	)
	if err != nil {
		return "", errors.New("create Qwen request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+options.APIKey)

	requestClient := *client
	// Never follow redirects. Replaying a bearer credential or tenant prompt
	// to a redirect target is not an acceptable provider fallback.
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := requestClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("qwen request failed: %w", ctx.Err())
		}
		return "", errors.New("qwen request failed")
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", errors.New("read Qwen response failed")
	}
	if len(body) > maxResponseBytes {
		return "", errors.New("qwen response exceeded the size limit")
	}
	if response.StatusCode != http.StatusOK {
		// Provider bodies can echo prompts or account data. Status is enough
		// for classification and is the only response detail exposed.
		return "", fmt.Errorf("qwen API returned status %d", response.StatusCode)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return "", errors.New("parse Qwen response failed")
	}
	if len(completion.Choices) == 0 {
		return "", errors.New("no response from Qwen")
	}
	text := strings.TrimSpace(completion.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("qwen returned an empty response")
	}
	return text, nil
}
