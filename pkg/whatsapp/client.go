package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zerodha/logf"
)

const (
	// DefaultTimeout for HTTP requests
	DefaultTimeout = 30 * time.Second
	// BaseURL for Meta Graph API
	BaseURL = "https://graph.facebook.com"
)

var (
	// Meta Graph object IDs are decimal strings. Validate provider-controlled
	// account fields before they are interpolated into a URL path so a malformed
	// stored value cannot alter the Graph endpoint being called.
	graphObjectIDPattern   = regexp.MustCompile(`^[0-9]{1,32}$`)
	graphAPIVersionPattern = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,3}$`)
)

func normalizeGraphObjectID(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if !graphObjectIDPattern.MatchString(value) {
		return "", fmt.Errorf("invalid %s", label)
	}
	return value, nil
}

func normalizeGraphAPIVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !graphAPIVersionPattern.MatchString(value) {
		return "", errors.New("invalid WhatsApp API version")
	}
	return value, nil
}

// Client is the WhatsApp Cloud API client
type Client struct {
	HTTPClient *http.Client
	Log        logf.Logger
	baseURL    string // For testing with mock servers
}

// New creates a new WhatsApp client
func New(log logf.Logger) *Client {
	return &Client{
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		Log:     log,
		baseURL: BaseURL,
	}
}

// NewWithTimeout creates a new WhatsApp client with custom timeout
func NewWithTimeout(log logf.Logger, timeout time.Duration) *Client {
	return &Client{
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
		Log:     log,
		baseURL: BaseURL,
	}
}

// NewWithBaseURL creates a new WhatsApp client with a custom base URL (for testing)
func NewWithBaseURL(log logf.Logger, baseURL string) *Client {
	return &Client{
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		Log:     log,
		baseURL: baseURL,
	}
}

// getBaseURL returns the base URL for API requests
func (c *Client) getBaseURL() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return BaseURL
}

// doHTTP prevents Meta credentials, authorization codes and signed payloads
// from being replayed to an HTTP redirect target. The Graph API endpoints used
// by this client are final destinations; callers must explicitly re-resolve a
// new trusted URL instead of following a provider response automatically.
func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	if c == nil || c.HTTPClient == nil {
		return nil, errors.New("WhatsApp HTTP client is not configured")
	}
	requestClient := *c.HTTPClient
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return requestClient.Do(req)
}

// doRequest performs an HTTP request to the Meta API
func (c *Client) doRequest(ctx context.Context, method, url string, body any, accessToken string) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, ParseMetaAPIError(resp.StatusCode, respBody)
	}

	return respBody, nil
}

// doJSON performs an HTTP request to the Meta API and unmarshals the JSON
// response body into a value of type T. It is a thin generic wrapper over
// doRequest for the common "request then decode" pattern.
func doJSON[T any](ctx context.Context, c *Client, method, url string, body any, accessToken string) (T, error) {
	var result T
	respBody, err := c.doRequest(ctx, method, url, body, accessToken)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return result, fmt.Errorf("failed to parse response: %w", err)
	}
	return result, nil
}

// parseMessageID extracts the WhatsApp message ID from a successful send
// response body, returning an error if the body is malformed or carries no ID.
func parseMessageID(respBody []byte) (string, error) {
	var resp MetaAPIResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if len(resp.Messages) == 0 {
		return "", fmt.Errorf("no message ID in response")
	}
	return resp.Messages[0].ID, nil
}

// CredentialsValidationResult contains the result of credentials validation
type CredentialsValidationResult struct {
	PhoneNumber            string
	VerifiedName           string
	AccountMode            string
	IsTestNumber           bool
	QualityRating          string
	CodeVerificationStatus string
	Warning                string
	IsOnBizApp             bool
	PlatformType           string
	IsSMB                  bool
}

// ValidateCredentials validates WhatsApp account credentials with Meta API
// It checks the phone number endpoint, business account endpoint, and verifies
// that the phone number belongs to the specified business account
func (c *Client) ValidateCredentials(ctx context.Context, phoneID, businessID, accessToken, apiVersion string) (*CredentialsValidationResult, error) {
	var err error
	phoneID, err = normalizeGraphObjectID(phoneID, "phone_id")
	if err != nil {
		return nil, err
	}
	businessID, err = normalizeGraphObjectID(businessID, "business_id")
	if err != nil {
		return nil, err
	}
	apiVersion, err = normalizeGraphAPIVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	// 1. Validate PhoneID
	phoneURL := fmt.Sprintf("%s/%s/%s?fields=display_phone_number,verified_name,code_verification_status,account_mode,quality_rating,is_on_biz_app,platform_type",
		c.getBaseURL(), apiVersion, phoneID)
	phoneBody, err := c.doRequest(ctx, http.MethodGet, phoneURL, nil, accessToken)
	if err != nil {
		return nil, fmt.Errorf("invalid phone_id or access_token: %w", err)
	}

	var phoneResult struct {
		DisplayPhoneNumber     string `json:"display_phone_number"`
		VerifiedName           string `json:"verified_name"`
		AccountMode            string `json:"account_mode"`
		CodeVerificationStatus string `json:"code_verification_status"`
		QualityRating          string `json:"quality_rating"`
		IsOnBizApp             bool   `json:"is_on_biz_app"`
		PlatformType           string `json:"platform_type"`
	}
	if err := json.Unmarshal(phoneBody, &phoneResult); err != nil {
		return nil, fmt.Errorf("failed to parse phone response: %w", err)
	}

	// Check verification status (skip for sandbox/test numbers and SMB accounts)
	isTestNumber := phoneResult.AccountMode == "SANDBOX" || phoneResult.VerifiedName == "Test Number"
	isSMB := phoneResult.IsOnBizApp || phoneResult.PlatformType == "SMB" || phoneResult.PlatformType == "SMB_CLOUD_API"

	var warning string
	if !isTestNumber && !isSMB {
		if phoneResult.CodeVerificationStatus == "NOT_VERIFIED" {
			return nil, fmt.Errorf("phone number is not verified. Please register it at: https://business.facebook.com/wa/manage/phone-numbers/")
		}
		if phoneResult.CodeVerificationStatus == "EXPIRED" {
			warning = "Phone verification has expired. Consider re-verifying at: https://business.facebook.com/wa/manage/phone-numbers/"
		}
	}

	// 2. Validate BusinessID
	businessURL := fmt.Sprintf("%s/%s/%s?fields=id,name", c.getBaseURL(), apiVersion, businessID)
	if _, err := c.doRequest(ctx, http.MethodGet, businessURL, nil, accessToken); err != nil {
		return nil, fmt.Errorf("invalid business_id: %w", err)
	}

	// 3. Verify phone belongs to business account. The edge is paginated; an
	// exact phone can be on any page.
	phonesResult, err := c.GetWABAPhoneNumbers(ctx, businessID, accessToken, apiVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to verify phone-business relationship: %w", err)
	}

	phoneFound := false
	for _, phone := range phonesResult.Data {
		if phone.ID == phoneID {
			phoneFound = true
			break
		}
	}
	if !phoneFound {
		return nil, fmt.Errorf("phone_id '%s' does not belong to business_id '%s'. Please verify your configuration", phoneID, businessID)
	}

	return &CredentialsValidationResult{
		PhoneNumber:            phoneResult.DisplayPhoneNumber,
		VerifiedName:           phoneResult.VerifiedName,
		AccountMode:            phoneResult.AccountMode,
		IsTestNumber:           isTestNumber,
		QualityRating:          phoneResult.QualityRating,
		CodeVerificationStatus: phoneResult.CodeVerificationStatus,
		Warning:                warning,
		IsOnBizApp:             phoneResult.IsOnBizApp,
		PlatformType:           phoneResult.PlatformType,
		IsSMB:                  isSMB,
	}, nil
}

// buildMessagesURL builds the messages endpoint URL
func (c *Client) buildMessagesURL(account *Account) string {
	return fmt.Sprintf("%s/%s/%s/messages", c.getBaseURL(), account.APIVersion, account.PhoneID)
}

// buildTemplatesURL builds the message_templates endpoint URL
func (c *Client) buildTemplatesURL(account *Account) string {
	return fmt.Sprintf("%s/%s/%s/message_templates", c.getBaseURL(), account.APIVersion, account.BusinessID)
}

// MediaURLResponse represents the response from Meta's media endpoint
type MediaURLResponse struct {
	URL              string `json:"url"`
	MimeType         string `json:"mime_type"`
	SHA256           string `json:"sha256"`
	FileSize         int64  `json:"file_size"`
	MessagingProduct string `json:"messaging_product"`
}

// GetMediaURL retrieves the download URL for a media file from Meta's API
func (c *Client) GetMediaURL(ctx context.Context, mediaID string, account *Account) (string, error) {
	url := fmt.Sprintf("%s/%s/%s", c.getBaseURL(), account.APIVersion, mediaID)

	respBody, err := c.doRequest(ctx, http.MethodGet, url, nil, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to get media URL: %w", err)
	}

	var mediaResp MediaURLResponse
	if err := json.Unmarshal(respBody, &mediaResp); err != nil {
		return "", fmt.Errorf("failed to parse media response: %w", err)
	}

	if mediaResp.URL == "" {
		return "", fmt.Errorf("no URL in media response")
	}

	return mediaResp.URL, nil
}

// DownloadMedia downloads media content from Meta's CDN URL
func (c *Client) DownloadMedia(ctx context.Context, mediaURL string, accessToken string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create download request: %w", err)
	}

	// Meta requires Bearer token for media download
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download media: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media download failed with status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read media content: %w", err)
	}

	return data, nil
}

// UploadMediaResponse represents the response from uploading media
type UploadMediaResponse struct {
	ID string `json:"id"`
}

// UploadMedia uploads media to WhatsApp's servers and returns the media ID
func (c *Client) UploadMedia(ctx context.Context, account *Account, data []byte, mimeType, filename string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s/media", c.getBaseURL(), account.APIVersion, account.PhoneID)

	// Create multipart form body
	body := &bytes.Buffer{}
	boundary := "----WebKitFormBoundary7MA4YWxkTrZu0gW"

	// Build multipart body manually
	fmt.Fprintf(body, "--%s\r\n", boundary)
	body.WriteString("Content-Disposition: form-data; name=\"messaging_product\"\r\n\r\n")
	body.WriteString("whatsapp\r\n")

	fmt.Fprintf(body, "--%s\r\n", boundary)
	fmt.Fprintf(body, "Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename)
	fmt.Fprintf(body, "Content-Type: %s\r\n\r\n", mimeType)
	body.Write(data)
	body.WriteString("\r\n")

	fmt.Fprintf(body, "--%s--\r\n", boundary)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("failed to create upload request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	req.Header.Set("Content-Type", fmt.Sprintf("multipart/form-data; boundary=%s", boundary))

	resp, err := c.doHTTP(req)
	if err != nil {
		return "", fmt.Errorf("failed to upload media: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("media upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var uploadResp UploadMediaResponse
	if err := json.Unmarshal(respBody, &uploadResp); err != nil {
		return "", fmt.Errorf("failed to parse upload response: %w", err)
	}

	if uploadResp.ID == "" {
		return "", fmt.Errorf("no media ID in upload response")
	}

	c.Log.Info("Media uploaded", "media_id", uploadResp.ID)
	return uploadResp.ID, nil
}

// sendMediaMessage is the shared implementation for all media message types.
func (c *Client) sendMediaMessage(ctx context.Context, account *Account, rcpt Recipient, mediaType string, mediaFields map[string]any) (string, error) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"type":              mediaType,
		mediaType:           mediaFields,
	}
	rcpt.SetOnPayload(payload)

	url := c.buildMessagesURL(account)
	c.Log.Debug("Sending media message", "type", mediaType, "phone", rcpt.Phone, "media_id", mediaFields["id"])

	respBody, err := c.doRequest(ctx, "POST", url, payload, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to send %s message: %w", mediaType, err)
	}

	messageID, err := parseMessageID(respBody)
	if err != nil {
		return "", err
	}
	c.Log.Info("Media message sent", "type", mediaType, "message_id", messageID, "phone", rcpt.Phone)
	return messageID, nil
}

// SendImageMessage sends an image message using a media ID
func (c *Client) SendImageMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, caption string) (string, error) {
	return c.sendMediaMessage(ctx, account, rcpt, "image", map[string]any{
		"id": mediaID, "caption": caption,
	})
}

// SendDocumentMessage sends a document message using a media ID
func (c *Client) SendDocumentMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, filename, caption string) (string, error) {
	return c.sendMediaMessage(ctx, account, rcpt, "document", map[string]any{
		"id": mediaID, "filename": filename, "caption": caption,
	})
}

// SendVideoMessage sends a video message using a media ID
func (c *Client) SendVideoMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, caption string) (string, error) {
	return c.sendMediaMessage(ctx, account, rcpt, "video", map[string]any{
		"id": mediaID, "caption": caption,
	})
}

// SendAudioMessage sends an audio message using a media ID
func (c *Client) SendAudioMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID string) (string, error) {
	return c.sendMediaMessage(ctx, account, rcpt, "audio", map[string]any{
		"id": mediaID,
	})
}

// MarkMessageRead sends a read receipt for a message
func (c *Client) MarkMessageRead(ctx context.Context, account *Account, messageID string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"status":            "read",
		"message_id":        messageID,
	}

	url := c.buildMessagesURL(account)
	c.Log.Debug("Sending read receipt", "message_id", messageID)

	_, err := c.doRequest(ctx, "POST", url, payload, account.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to send read receipt: %w", err)
	}

	c.Log.Debug("Read receipt sent", "message_id", messageID)
	return nil
}

// ResumableUploadResponse represents response from creating upload session
type ResumableUploadResponse struct {
	ID string `json:"id"` // Upload session ID
}

// ResumableUploadFinishResponse represents response from completing upload
type ResumableUploadFinishResponse struct {
	Handle string `json:"h"` // File handle for use in templates
}

// ResumableUpload performs a resumable upload to get a file handle for template media samples.
// This is required for IMAGE, VIDEO, DOCUMENT header types in templates.
// Returns a handle (like "4::aW1hZ2...") that can be used in template creation.
func (c *Client) ResumableUpload(ctx context.Context, account *Account, data []byte, mimeType, filename string) (string, error) {
	if account.AppID == "" {
		return "", fmt.Errorf("app_id is required for resumable upload")
	}

	// Step 1: Create upload session
	sessionURL := fmt.Sprintf("%s/%s/%s/uploads", c.getBaseURL(), account.APIVersion, account.AppID)

	sessionPayload := map[string]any{
		"file_length": len(data),
		"file_type":   mimeType,
		"file_name":   filename,
	}

	c.Log.Info("Creating upload session", "url", sessionURL, "file_size", len(data), "mime_type", mimeType)

	sessionResp, err := c.doRequest(ctx, http.MethodPost, sessionURL, sessionPayload, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to create upload session: %w", err)
	}

	var uploadSession ResumableUploadResponse
	if err := json.Unmarshal(sessionResp, &uploadSession); err != nil {
		return "", fmt.Errorf("failed to parse upload session response: %w", err)
	}

	if uploadSession.ID == "" {
		return "", fmt.Errorf("no session ID in upload response")
	}

	c.Log.Info("Upload session created", "session_id", uploadSession.ID)

	// Step 2: Upload file data to session
	uploadURL := fmt.Sprintf("%s/%s/%s", c.getBaseURL(), account.APIVersion, uploadSession.ID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to create upload request: %w", err)
	}

	req.Header.Set("Authorization", "OAuth "+account.AccessToken)
	req.Header.Set("file_offset", "0")
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.doHTTP(req)
	if err != nil {
		return "", fmt.Errorf("failed to upload file data: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var finishResp ResumableUploadFinishResponse
	if err := json.Unmarshal(respBody, &finishResp); err != nil {
		return "", fmt.Errorf("failed to parse upload finish response: %w", err)
	}

	if finishResp.Handle == "" {
		return "", fmt.Errorf("no handle in upload response")
	}

	c.Log.Info("Resumable upload completed", "handle", finishResp.Handle[:20]+"...")
	return finishResp.Handle, nil
}

// BusinessProfileResponse represents the response containing business profile
type BusinessProfileResponse struct {
	Data []BusinessProfile `json:"data"`
}

// GetBusinessProfile retrieves the business profile settings
func (c *Client) GetBusinessProfile(ctx context.Context, account *Account) (*BusinessProfile, error) {
	// Requesting specific fields to optimize performance
	fields := "about,address,description,email,profile_picture_url,websites,vertical,messaging_product"
	url := fmt.Sprintf("%s/%s/%s/whatsapp_business_profile?fields=%s", c.getBaseURL(), account.APIVersion, account.PhoneID, fields)

	respBody, err := c.doRequest(ctx, http.MethodGet, url, nil, account.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get business profile: %w", err)
	}

	var response BusinessProfileResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse business profile response: %w", err)
	}

	if len(response.Data) == 0 {
		return nil, fmt.Errorf("no business profile found")
	}

	return &response.Data[0], nil
}

// UpdateBusinessProfile updates the business profile settings
func (c *Client) UpdateBusinessProfile(ctx context.Context, account *Account, input BusinessProfileInput) error {
	url := fmt.Sprintf("%s/%s/%s/whatsapp_business_profile", c.getBaseURL(), account.APIVersion, account.PhoneID)

	// Ensure messaging_product is set
	input.MessagingProduct = "whatsapp"

	_, err := c.doRequest(ctx, http.MethodPost, url, input, account.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to update business profile: %w", err)
	}

	return nil
}

// SubscribeAppResponse represents the response from subscribing an app to webhooks
type SubscribeAppResponse struct {
	Success bool `json:"success"`
}

// SubscribeApp subscribes the app to webhooks for the WhatsApp Business Account.
// This is required after phone number registration to receive incoming messages.
// Calls POST /{api_version}/{waba_id}/subscribed_apps
func (c *Client) SubscribeApp(ctx context.Context, account *Account) error {
	url := fmt.Sprintf("%s/%s/%s/subscribed_apps", c.getBaseURL(), account.APIVersion, account.BusinessID)

	respBody, err := c.doRequest(ctx, http.MethodPost, url, nil, account.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to subscribe app to webhooks: %w", err)
	}

	var resp SubscribeAppResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("failed to parse subscribe response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("subscription was not successful")
	}

	c.Log.Info("App subscribed to webhooks", "business_id", account.BusinessID)
	return nil
}

// ConfigurePhoneWebhookOverride configures an alternate webhook callback for
// exactly one WhatsApp phone number. It deliberately targets account.PhoneID,
// never the WABA or app-level callback. Meta must read back the exact callback
// from webhook_configuration.phone_number before this function returns success.
//
// Callers must supply credentials from a trusted server-side account record;
// this method does not log or return either secret.
func (c *Client) ConfigurePhoneWebhookOverride(
	ctx context.Context,
	account *Account,
	callbackURL string,
	verifyToken string,
) error {
	endpoint, err := c.phoneGraphEndpoint(account)
	if err != nil {
		return err
	}
	if err := validateWebhookOverrideInputs(callbackURL, verifyToken); err != nil {
		return err
	}

	type webhookConfiguration struct {
		OverrideCallbackURI string `json:"override_callback_uri"`
		VerifyToken         string `json:"verify_token"`
	}
	type setOverrideRequest struct {
		WebhookConfiguration webhookConfiguration `json:"webhook_configuration"`
	}
	type setOverrideResponse struct {
		Success *bool `json:"success"`
	}

	responseBody, err := c.doRequest(ctx, http.MethodPost, endpoint, setOverrideRequest{
		WebhookConfiguration: webhookConfiguration{
			OverrideCallbackURI: callbackURL,
			VerifyToken:         verifyToken,
		},
	}, account.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to set phone webhook override: %w", err)
	}

	// Meta normally returns {"success":true}. A missing success field is allowed
	// so the required readback remains authoritative, but an explicit false is
	// never treated as a successful configuration.
	var setResponse setOverrideResponse
	if err := json.Unmarshal(responseBody, &setResponse); err == nil &&
		setResponse.Success != nil && !*setResponse.Success {
		return fmt.Errorf("meta did not accept the phone webhook override")
	}

	type readbackResponse struct {
		WebhookConfiguration struct {
			PhoneNumber string `json:"phone_number"`
		} `json:"webhook_configuration"`
	}
	readbackBody, err := c.doRequest(
		ctx,
		http.MethodGet,
		endpoint+"?fields=webhook_configuration",
		nil,
		account.AccessToken,
	)
	if err != nil {
		return fmt.Errorf("failed to verify phone webhook override: %w", err)
	}

	var readback readbackResponse
	if err := json.Unmarshal(readbackBody, &readback); err != nil {
		return fmt.Errorf("failed to parse phone webhook override verification: %w", err)
	}
	if readback.WebhookConfiguration.PhoneNumber != callbackURL {
		return fmt.Errorf("phone webhook override verification did not report the expected callback")
	}

	return nil
}

func (c *Client) phoneGraphEndpoint(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("WhatsApp account is required")
	}
	phoneID := strings.TrimSpace(account.PhoneID)
	apiVersion := strings.TrimSpace(account.APIVersion)
	if !graphObjectIDPattern.MatchString(phoneID) {
		return "", fmt.Errorf("invalid WhatsApp phone ID")
	}
	if !graphAPIVersionPattern.MatchString(apiVersion) {
		return "", fmt.Errorf("invalid WhatsApp API version")
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(c.getBaseURL(), "/"), apiVersion, url.PathEscape(phoneID)), nil
}

func validateWebhookOverrideInputs(callbackURL, verifyToken string) error {
	if strings.TrimSpace(verifyToken) == "" {
		return fmt.Errorf("webhook verify token is required")
	}
	parsedCallback, err := url.Parse(callbackURL)
	if err != nil || parsedCallback.Scheme != "https" || parsedCallback.Host == "" ||
		parsedCallback.User != nil || parsedCallback.Fragment != "" {
		return fmt.Errorf("invalid webhook callback URL")
	}
	return nil
}

// TokenExchangeResponse represents the response from OAuth token exchange
type TokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// ExchangeCodeForToken exchanges a Facebook authorization code for a permanent access token
func (c *Client) ExchangeCodeForToken(ctx context.Context, code, appID, appSecret, apiVersion string) (string, error) {
	var err error
	apiVersion, err = normalizeGraphAPIVersion(apiVersion)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/%s/oauth/access_token", c.getBaseURL(), apiVersion)
	form := url.Values{
		"client_id":     []string{appID},
		"client_secret": []string{appSecret},
		"code":          []string{code},
	}

	// Keep credentials out of the request URL and intermediary access logs.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.doHTTP(req)
	if err != nil {
		return "", fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Parse Meta error using same pattern as doRequest
		var metaErr MetaAPIError
		if json.Unmarshal(respBody, &metaErr) == nil && metaErr.Error.Message != "" {
			return "", fmt.Errorf("token exchange failed: %s", metaErr.Error.Message)
		}
		return "", fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp TokenExchangeResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("no access token in response")
	}

	c.Log.Info("Token exchange successful")
	return tokenResp.AccessToken, nil
}

// PhoneNumberInfo represents phone number information from Meta
type PhoneNumberInfo struct {
	VerifiedName       string `json:"verified_name"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	QualityRating      string `json:"quality_rating"`
	IsOnBizApp         bool   `json:"is_on_biz_app"`
	PlatformType       string `json:"platform_type"`
}

// GetPhoneNumberInfo retrieves information about a phone number
func (c *Client) GetPhoneNumberInfo(ctx context.Context, phoneID, accessToken, apiVersion string) (*PhoneNumberInfo, error) {
	var err error
	phoneID, err = normalizeGraphObjectID(phoneID, "phone_id")
	if err != nil {
		return nil, err
	}
	apiVersion, err = normalizeGraphAPIVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/%s/%s?fields=verified_name,display_phone_number,quality_rating,is_on_biz_app,platform_type",
		c.getBaseURL(), apiVersion, phoneID)

	respBody, err := c.doRequest(ctx, http.MethodGet, url, nil, accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get phone number info: %w", err)
	}

	var info PhoneNumberInfo
	if err := json.Unmarshal(respBody, &info); err != nil {
		return nil, fmt.Errorf("failed to parse phone number info: %w", err)
	}

	return &info, nil
}

// RegisterPhoneNumber registers a phone number with Two-Step Verification PIN
func (c *Client) RegisterPhoneNumber(ctx context.Context, phoneID, pin, accessToken, apiVersion string) error {
	var err error
	phoneID, err = normalizeGraphObjectID(phoneID, "phone_id")
	if err != nil {
		return err
	}
	apiVersion, err = normalizeGraphAPIVersion(apiVersion)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/%s/register", c.getBaseURL(), apiVersion, phoneID)

	payload := map[string]string{
		"messaging_product": "whatsapp",
		"pin":               pin,
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, url, payload, accessToken)
	if err != nil {
		return fmt.Errorf("phone registration failed: %w", err)
	}
	var registrationResponse struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(respBody, &registrationResponse); err != nil || !registrationResponse.Success {
		// A 200 without an explicit success acknowledgement is ambiguous. Do
		// not expose the provider body and do not let callers advance local state.
		return errors.New("phone registration response could not be confirmed")
	}

	c.Log.Info("Phone number registered successfully", "phone_id", phoneID)
	return nil
}

// TokenDebugInfo represents the response from the debug_token endpoint
type TokenDebugInfo struct {
	AppID               string   `json:"app_id"`
	Type                string   `json:"type"`
	Application         string   `json:"application"`
	DataAccessExpiresAt int64    `json:"data_access_expires_at"`
	ExpiresAt           int64    `json:"expires_at"`
	IsValid             bool     `json:"is_valid"`
	IssuedAt            int64    `json:"issued_at"`
	Scopes              []string `json:"scopes"`
	UserID              string   `json:"user_id"`
	GranularScopes      []struct {
		Scope     string   `json:"scope"`
		TargetIds []string `json:"target_ids,omitempty"`
	} `json:"granular_scopes"`
}

// GetTokenDebugInfo retrieves information about an access token
func (c *Client) GetTokenDebugInfo(ctx context.Context, inputToken, accessToken string) (*TokenDebugInfo, error) {
	url := fmt.Sprintf("%s/debug_token?input_token=%s", c.getBaseURL(), url.QueryEscape(inputToken))

	// debug_token requires an app access token or a user access token
	respBody, err := c.doRequest(ctx, http.MethodGet, url, nil, accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get token debug info: %w", err)
	}

	var resp struct {
		Data TokenDebugInfo `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse token debug info: %w", err)
	}

	return &resp.Data, nil
}

// SharedWABAResponse represents the legacy /me/accounts response shape.
// Deprecated: /me/accounts returns Facebook Pages, not WhatsApp Business Accounts.
type SharedWABAResponse struct {
	Data []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Phone struct {
			Data []struct {
				ID                 string `json:"id"`
				DisplayPhoneNumber string `json:"display_phone_number"`
				VerifiedName       string `json:"verified_name"`
			} `json:"data"`
		} `json:"phone_numbers"`
	} `json:"data"`
}

// GetSharedWABA queries the legacy /me/accounts edge.
// Deprecated: /me/accounts returns Facebook Pages and must not be used for
// WhatsApp Embedded Signup asset discovery. Consume the WA_EMBEDDED_SIGNUP
// browser event, or use whatsapp_business_management granular scope targets.
func (c *Client) GetSharedWABA(ctx context.Context, accessToken string) (*SharedWABAResponse, error) {
	url := fmt.Sprintf("%s/me/accounts?fields=id,name,phone_numbers{id,display_phone_number,verified_name}", c.getBaseURL())

	respBody, err := c.doRequest(ctx, http.MethodGet, url, nil, accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared WABA info: %w", err)
	}

	var resp SharedWABAResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse shared WABA response: %w", err)
	}

	return &resp, nil
}

const maxWABAPhonePages = 100

// WABAPhoneNumber represents one phone returned by a WABA phone_numbers edge.
type WABAPhoneNumber struct {
	ID                 string `json:"id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name"`
	QualityRating      string `json:"quality_rating"`
}

// WABAPhoneNumbersResponse represents all deduplicated phone numbers for a WABA.
type WABAPhoneNumbersResponse struct {
	Data []WABAPhoneNumber `json:"data"`
}

// GetWABAPhoneNumbers retrieves every page of phone numbers associated with a
// WABA. Pagination URLs are always reconstructed against the configured Graph
// base; provider-supplied paging.next URLs are never followed.
func (c *Client) GetWABAPhoneNumbers(ctx context.Context, wabaID, accessToken, apiVersion string) (*WABAPhoneNumbersResponse, error) {
	type phonePage struct {
		Data   []WABAPhoneNumber `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}

	var err error
	wabaID, err = normalizeGraphObjectID(wabaID, "business_id")
	if err != nil {
		return nil, err
	}
	apiVersion, err = normalizeGraphAPIVersion(apiVersion)
	if err != nil {
		return nil, err
	}

	baseEndpoint := fmt.Sprintf(
		"%s/%s/%s/phone_numbers",
		strings.TrimRight(c.getBaseURL(), "/"),
		url.PathEscape(apiVersion),
		url.PathEscape(wabaID),
	)
	result := &WABAPhoneNumbersResponse{Data: make([]WABAPhoneNumber, 0)}
	seenIDs := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	after := ""

	for pageNumber := 0; pageNumber < maxWABAPhonePages; pageNumber++ {
		query := url.Values{}
		query.Set("fields", "id,display_phone_number,verified_name,quality_rating")
		query.Set("limit", "100")
		if after != "" {
			query.Set("after", after)
		}
		pageURL := baseEndpoint + "?" + query.Encode()

		respBody, err := c.doRequest(ctx, http.MethodGet, pageURL, nil, accessToken)
		if err != nil {
			return nil, fmt.Errorf("failed to get WABA phone numbers: %w", err)
		}
		var page phonePage
		if err := json.Unmarshal(respBody, &page); err != nil {
			return nil, fmt.Errorf("failed to parse WABA phone numbers response: %w", err)
		}
		for _, phone := range page.Data {
			phone.ID, err = normalizeGraphObjectID(phone.ID, "phone_id returned by Meta")
			if err != nil {
				return nil, err
			}
			if _, exists := seenIDs[phone.ID]; exists {
				continue
			}
			seenIDs[phone.ID] = struct{}{}
			result.Data = append(result.Data, phone)
		}

		if strings.TrimSpace(page.Paging.Next) == "" {
			return result, nil
		}
		after = strings.TrimSpace(page.Paging.Cursors.After)
		if after == "" {
			return nil, errors.New("Meta phone-number pagination omitted its continuation cursor")
		}
		if _, repeated := seenCursors[after]; repeated {
			return nil, errors.New("Meta phone-number pagination repeated a continuation cursor")
		}
		seenCursors[after] = struct{}{}
	}

	return nil, fmt.Errorf("Meta phone-number pagination exceeded %d pages", maxWABAPhonePages)
}
