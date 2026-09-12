package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CoexistenceSyncType identifies one of Meta's one-time WhatsApp Business App
// data synchronizations. The resulting data is delivered asynchronously to
// the matching webhook field.
type CoexistenceSyncType string

const (
	// CoexistenceSyncContacts requests the Business App contact/address-book
	// state exposed through the smb_app_state_sync webhook field.
	CoexistenceSyncContacts CoexistenceSyncType = "smb_app_state_sync"
	// CoexistenceSyncHistory requests the Business App message history exposed
	// through the history webhook field.
	CoexistenceSyncHistory CoexistenceSyncType = "history"
)

// CoexistenceSyncResponse is Meta's acknowledgement that a Business App data
// synchronization request was accepted. History progress is reported by
// webhook. Contact delivery uses the ongoing smb_app_state_sync delta stream,
// which has no request-correlated completion marker, so this acknowledgement
// is the contact stage's terminal success. RequestID is persisted so onboarding
// remains resumable.
type CoexistenceSyncResponse struct {
	MessagingProduct string `json:"messaging_product"`
	RequestID        string `json:"request_id"`
}

type coexistenceSyncRequest struct {
	MessagingProduct string              `json:"messaging_product"`
	SyncType         CoexistenceSyncType `json:"sync_type"`
}

// RequestCoexistenceContactSync asks Meta to send the WhatsApp Business App's
// contacts through smb_app_state_sync webhooks.
func (c *Client) RequestCoexistenceContactSync(ctx context.Context, account *Account) (*CoexistenceSyncResponse, error) {
	return c.RequestCoexistenceSync(ctx, account, CoexistenceSyncContacts)
}

// RequestCoexistenceHistorySync asks Meta to send the WhatsApp Business App's
// available message history through history webhooks.
func (c *Client) RequestCoexistenceHistorySync(ctx context.Context, account *Account) (*CoexistenceSyncResponse, error) {
	return c.RequestCoexistenceSync(ctx, account, CoexistenceSyncHistory)
}

// RequestCoexistenceSync requests one supported Business App synchronization.
// This call only acknowledges the request; callers track history progress and
// completion separately. The method deliberately emits no logs because the
// account carries an access token and provider responses can contain customer
// or diagnostic data.
func (c *Client) RequestCoexistenceSync(ctx context.Context, account *Account, syncType CoexistenceSyncType) (*CoexistenceSyncResponse, error) {
	endpoint, err := c.coexistenceSyncEndpoint(account)
	if err != nil {
		return nil, err
	}
	if syncType != CoexistenceSyncContacts && syncType != CoexistenceSyncHistory {
		return nil, errors.New("invalid coexistence sync type")
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return nil, errors.New("WhatsApp access token is required")
	}

	response, err := doJSON[CoexistenceSyncResponse](ctx, c, http.MethodPost, endpoint, coexistenceSyncRequest{
		MessagingProduct: "whatsapp",
		SyncType:         syncType,
	}, account.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("request coexistence %s sync: %w", syncType, err)
	}
	response.RequestID = strings.TrimSpace(response.RequestID)
	if response.RequestID == "" {
		return nil, errors.New("meta accepted coexistence sync without a request_id")
	}
	if response.MessagingProduct != "whatsapp" {
		return nil, errors.New("meta returned an unexpected messaging product for coexistence sync")
	}
	return &response, nil
}

func (c *Client) coexistenceSyncEndpoint(account *Account) (string, error) {
	if c == nil {
		return "", errors.New("WhatsApp client is required")
	}
	if account == nil {
		return "", errors.New("WhatsApp account is required")
	}
	apiVersion, err := normalizeGraphAPIVersion(account.APIVersion)
	if err != nil || apiVersion != account.APIVersion {
		return "", errors.New("invalid WhatsApp API version")
	}
	phoneID, err := normalizeGraphObjectID(account.PhoneID, "phone_id")
	if err != nil || phoneID != account.PhoneID {
		return "", errors.New("invalid phone_id")
	}
	return fmt.Sprintf("%s/%s/%s/smb_app_data", strings.TrimRight(c.getBaseURL(), "/"), apiVersion, phoneID), nil
}
