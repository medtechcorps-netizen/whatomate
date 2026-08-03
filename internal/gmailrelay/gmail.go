package gmailrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxGmailResponseBytes = int64(16 << 20)

// AccessTokenProvider supplies a short-lived Google access token without
// exposing the refresh token to the Gmail transport.
type AccessTokenProvider interface {
	AccessToken(context.Context) (string, error)
}

// GmailProfile is the users.getProfile subset used for account binding and
// initial history baselining.
type GmailProfile struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	ThreadsTotal  int64  `json:"threadsTotal"`
	HistoryID     string `json:"historyId"`
}

type GmailMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type GmailHistoryRecord struct {
	ID            string `json:"id"`
	MessagesAdded []struct {
		Message GmailMessageRef `json:"message"`
	} `json:"messagesAdded,omitempty"`
	LabelsAdded []struct {
		Message  GmailMessageRef `json:"message"`
		LabelIDs []string        `json:"labelIds"`
	} `json:"labelsAdded,omitempty"`
}

type GmailHistoryPage struct {
	History       []GmailHistoryRecord `json:"history,omitempty"`
	NextPageToken string               `json:"nextPageToken,omitempty"`
	HistoryID     string               `json:"historyId"`
}

type GmailMessageList struct {
	Messages      []GmailMessageRef `json:"messages,omitempty"`
	NextPageToken string            `json:"nextPageToken,omitempty"`
}

type GmailThread struct {
	ID        string             `json:"id"`
	HistoryID string             `json:"historyId"`
	Messages  []GmailRESTMessage `json:"messages"`
}

type GmailSendResponse struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

// GmailAPIError deliberately excludes Google's response body because OAuth
// errors and message fragments can contain credentials or customer content.
type GmailAPIError struct {
	Operation  string
	StatusCode int
	RetryAfter time.Duration
}

func (e *GmailAPIError) Error() string {
	if e == nil {
		return "Gmail API request failed"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("Gmail API %s returned HTTP %d", e.Operation, e.StatusCode)
	}
	return fmt.Sprintf("Gmail API %s transport failed", e.Operation)
}

// GmailClient is a fixed-origin REST client. Callers can supply resource IDs
// and query values, but can never redirect requests to an arbitrary host.
type GmailClient struct {
	baseURL string
	tokens  AccessTokenProvider
	client  *http.Client
	now     func() time.Time
}

func NewGmailClient(config *Config, tokens AccessTokenProvider, client *http.Client) (*GmailClient, error) {
	if config == nil || tokens == nil {
		return nil, errors.New("gmail client configuration is incomplete")
	}
	if err := validateEndpoint(config.GmailAPIBaseURL, true); err != nil {
		return nil, errors.New("gmail API base URL is invalid")
	}
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	if clientCopy.Timeout <= 0 {
		clientCopy.Timeout = config.HTTPTimeout
	}
	if clientCopy.CheckRedirect == nil {
		clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &GmailClient{
		baseURL: strings.TrimRight(config.GmailAPIBaseURL, "/"),
		tokens:  tokens,
		client:  &clientCopy,
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

func (c *GmailClient) Profile(ctx context.Context) (GmailProfile, error) {
	var profile GmailProfile
	err := c.doJSON(ctx, http.MethodGet, "/users/me/profile", nil, nil, &profile, "profile")
	if err != nil {
		return GmailProfile{}, err
	}
	if strings.TrimSpace(profile.EmailAddress) == "" || strings.TrimSpace(profile.HistoryID) == "" {
		return GmailProfile{}, errors.New("gmail profile response is incomplete")
	}
	return profile, nil
}

func (c *GmailClient) History(ctx context.Context, startHistoryID, pageToken string) (GmailHistoryPage, error) {
	if strings.TrimSpace(startHistoryID) == "" {
		return GmailHistoryPage{}, errors.New("gmail history cursor is required")
	}
	query := url.Values{
		"startHistoryId": {startHistoryID},
		"historyTypes":   {"messageAdded", "labelAdded"},
		"labelId":        {"INBOX"},
		"maxResults":     {"100"},
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	var page GmailHistoryPage
	err := c.doJSON(ctx, http.MethodGet, "/users/me/history", query, nil, &page, "history")
	return page, err
}

func (c *GmailClient) GetMessage(ctx context.Context, messageID string) (GmailRESTMessage, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return GmailRESTMessage{}, errors.New("gmail message ID is required")
	}
	query := url.Values{
		"format": {"full"},
	}
	var message GmailRESTMessage
	err := c.doJSON(
		ctx,
		http.MethodGet,
		"/users/me/messages/"+url.PathEscape(messageID),
		query,
		nil,
		&message,
		"message_get",
	)
	return message, err
}

func (c *GmailClient) GetThread(ctx context.Context, threadID string) (GmailThread, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return GmailThread{}, errors.New("gmail thread ID is required")
	}
	query := url.Values{
		"format": {"metadata"},
		"metadataHeaders": {
			"From",
			"Reply-To",
			"Subject",
			"Message-ID",
			"In-Reply-To",
			"References",
		},
	}
	var thread GmailThread
	err := c.doJSON(
		ctx,
		http.MethodGet,
		"/users/me/threads/"+url.PathEscape(threadID),
		query,
		nil,
		&thread,
		"thread_get",
	)
	return thread, err
}

func (c *GmailClient) SearchMessages(ctx context.Context, queryValue string, maxResults int) (GmailMessageList, error) {
	return c.SearchMessagePage(ctx, queryValue, maxResults, "")
}

func (c *GmailClient) SearchMessagePage(
	ctx context.Context,
	queryValue string,
	maxResults int,
	pageToken string,
) (GmailMessageList, error) {
	queryValue = strings.TrimSpace(queryValue)
	if queryValue == "" {
		return GmailMessageList{}, errors.New("gmail search query is required")
	}
	if maxResults <= 0 || maxResults > 100 {
		maxResults = 10
	}
	query := url.Values{
		"q":          {queryValue},
		"maxResults": {strconv.Itoa(maxResults)},
	}
	if strings.TrimSpace(pageToken) != "" {
		query.Set("pageToken", strings.TrimSpace(pageToken))
	}
	var result GmailMessageList
	err := c.doJSON(ctx, http.MethodGet, "/users/me/messages", query, nil, &result, "message_search")
	return result, err
}

func (c *GmailClient) SendRaw(ctx context.Context, threadID string, raw []byte) (GmailSendResponse, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || len(raw) == 0 {
		return GmailSendResponse{}, errors.New("gmail send request is incomplete")
	}
	payload := struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}{
		Raw:      EncodeBase64URL(raw),
		ThreadID: threadID,
	}
	var result GmailSendResponse
	err := c.doJSON(ctx, http.MethodPost, "/users/me/messages/send", nil, payload, &result, "message_send")
	if err != nil {
		return GmailSendResponse{}, err
	}
	if strings.TrimSpace(result.ID) == "" || strings.TrimSpace(result.ThreadID) == "" {
		return GmailSendResponse{}, errors.New("gmail send response is incomplete")
	}
	return result, nil
}

func (c *GmailClient) doJSON(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	payload any,
	result any,
	operation string,
) error {
	if c == nil || c.tokens == nil || c.client == nil {
		return errors.New("gmail client is unavailable")
	}
	token, err := c.tokens.AccessToken(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return errors.New("gmail authorization is unavailable")
	}

	var body io.Reader
	if payload != nil {
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return errors.New("encode Gmail request")
		}
		body = bytes.NewReader(raw)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("build Gmail request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Gmail-Relay/1.0")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return &GmailAPIError{Operation: operation}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &GmailAPIError{
			Operation:  operation,
			StatusCode: response.StatusCode,
			RetryAfter: parseProviderRetryAfter(response.Header.Get("Retry-After"), c.now()),
		}
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxGmailResponseBytes))
	if err := decoder.Decode(result); err != nil {
		return errors.New("decode Gmail response")
	}
	return nil
}

func parseProviderRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Hour)
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return min(parsed.Sub(now), time.Hour)
	}
	return 0
}
