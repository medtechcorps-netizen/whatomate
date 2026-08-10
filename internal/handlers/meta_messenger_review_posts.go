package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const metaMessengerReviewPagePostLimit = 3

type metaMessengerReviewPagePost struct {
	ID           string `json:"id"`
	Message      string `json:"message,omitempty"`
	CreatedTime  string `json:"created_time"`
	PermalinkURL string `json:"permalink_url"`
}

type metaMessengerReviewPagePostsResponse struct {
	PageID    string                        `json:"page_id"`
	PageName  string                        `json:"page_name"`
	Posts     []metaMessengerReviewPagePost `json:"posts"`
	FetchedAt time.Time                     `json:"fetched_at"`
}

type metaMessengerReviewPagePostsGraphResponse struct {
	Data []metaMessengerReviewPagePost `json:"data"`
}

type metaMessengerReviewPagePostCredential struct {
	EncryptedAccessToken string
	EncryptedTokenDigest [sha256.Size]byte
	CredentialID         uuid.UUID
	CredentialVersion    int
	PageName             string
	Tuple                metareview.ProvisionTuple
}

// GetMetaMessengerReviewPagePosts exposes one reviewer-visible,
// pages_read_engagement-backed preview. It is intentionally unavailable for
// production, ordinary tenants, and every account other than the exact
// deployment-pinned, review-ready Messenger account.
func (a *App) GetMetaMessengerReviewPagePosts(r *fastglue.Request) error {
	setMetaMessengerNoStoreHeaders(r)
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}

	credential, authErr, err := a.loadMetaMessengerReviewPagePostCredential(
		requestContext(r),
		orgID,
		userID,
		accountID,
		r,
	)
	if authErr != nil {
		return nil
	}
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) ||
			errors.Is(err, errMetaMessengerReviewUnavailable) {
			return r.SendErrorEnvelope(
				fasthttp.StatusNotFound,
				"Messenger review Page preview not found",
				nil,
				"",
			)
		}
		a.Log.Error(
			"Failed to load Messenger review Page preview binding",
			"error",
			err,
			"organization_id",
			orgID,
			"channel_account_id",
			accountID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Messenger review Page preview is unavailable",
			nil,
			"",
		)
	}

	// The encrypted value leaves the database transaction, but plaintext never
	// does. It exists only for the provider request below and is never returned
	// or included in diagnostics.
	pageToken, err := appcrypto.Decrypt(
		credential.EncryptedAccessToken,
		a.integrationEncryptionKey(),
	)
	if err != nil || strings.TrimSpace(pageToken) == "" ||
		strings.TrimSpace(pageToken) != pageToken {
		return r.SendErrorEnvelope(
			fasthttp.StatusNotFound,
			"Messenger review Page preview not found",
			nil,
			"",
		)
	}

	var graphResponse metaMessengerReviewPagePostsGraphResponse
	err = a.doMetaMessengerGraphJSON(
		requestContext(r),
		http.MethodGet,
		url.PathEscape(credential.Tuple.PageID)+"/posts",
		url.Values{
			"fields": {"id,message,created_time,permalink_url"},
			"limit":  {strconv.Itoa(metaMessengerReviewPagePostLimit)},
		},
		pageToken,
		&graphResponse,
	)
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"Meta could not load the review Page posts",
			nil,
			"",
		)
	}
	posts, err := normalizeMetaMessengerReviewPagePosts(graphResponse.Data)
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"Meta returned an invalid review Page post response",
			nil,
			"",
		)
	}
	if err := a.revalidateMetaMessengerReviewPagePostCredential(
		requestContext(r),
		orgID,
		accountID,
		credential,
	); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) ||
			errors.Is(err, errMetaMessengerReviewUnavailable) {
			return r.SendErrorEnvelope(
				fasthttp.StatusNotFound,
				"Messenger review Page preview not found",
				nil,
				"",
			)
		}
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Messenger review Page preview is unavailable",
			nil,
			"",
		)
	}
	return r.SendEnvelope(metaMessengerReviewPagePostsResponse{
		PageID:    credential.Tuple.PageID,
		PageName:  credential.PageName,
		Posts:     posts,
		FetchedAt: time.Now().UTC(),
	})
}

// loadMetaMessengerReviewPagePostCredential is the complete database phase.
// It authorizes channel_accounts:read and returns only the encrypted current
// OAuth token after every deployment/account/credential binding matches.
// Provider network I/O and decryption happen only after this transaction ends.
func (a *App) loadMetaMessengerReviewPagePostCredential(
	ctx context.Context,
	organizationID, userID, accountID uuid.UUID,
	r *fastglue.Request,
) (metaMessengerReviewPagePostCredential, error, error) {
	result := metaMessengerReviewPagePostCredential{}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return result, nil, errors.New("review Page preview database is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var authErr error
	err := database.WithTenantReadCommitted(
		root.DB.WithContext(ctx),
		organizationID,
		func(tx *gorm.DB) error {
			scoped := root.scopedApp(tx, organizationID)
			authorizedOrgID, authorizedUserID, scopedAuthErr := scoped.requireAuth(
				r,
				models.ResourceChannelAccounts,
				models.ActionRead,
			)
			authErr = scopedAuthErr
			if authErr != nil {
				return nil
			}
			if authorizedOrgID != organizationID || authorizedUserID != userID {
				return errMetaMessengerReviewUnavailable
			}

			_, tuple, settingsErr := root.metaMessengerReviewSettings(time.Now().UTC())
			if settingsErr != nil || tuple.OrganizationID != organizationID.String() ||
				tuple.ChannelAccountID != accountID.String() {
				return errMetaMessengerReviewUnavailable
			}
			var account models.ChannelAccount
			if err := tx.Where(
				"id = ? AND organization_id = ?",
				accountID,
				organizationID,
			).First(&account).Error; err != nil {
				return err
			}
			if !root.readyMetaMessengerReviewAccount(&account) {
				return errMetaMessengerReviewUnavailable
			}

			now := time.Now().UTC()
			var credentials []models.ChannelCredential
			if err := tx.Model(&models.ChannelCredential{}).
				Select("id, organization_id, channel_account_id, kind, version, credential_blob, status, expires_at, key_version, metadata").
				Where(
					"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
					organizationID,
					accountID,
					models.ChannelCredentialKindOAuth,
					[]models.ChannelCredentialStatus{
						models.ChannelCredentialStatusActive,
						models.ChannelCredentialStatusExpiring,
					},
					now,
				).
				Order("version DESC, id ASC").
				Find(&credentials).Error; err != nil {
				return err
			}
			if len(credentials) != 1 {
				return errMetaMessengerReviewUnavailable
			}
			current := credentials[0]
			encryptedToken, tokenOK := current.CredentialBlob["access_token"].(string)
			if current.OrganizationID != organizationID ||
				current.ChannelAccountID != accountID ||
				current.Kind != models.ChannelCredentialKindOAuth ||
				current.Version <= 0 ||
				current.KeyVersion != "app:v1" ||
				!tokenOK || strings.TrimSpace(encryptedToken) != encryptedToken ||
				!appcrypto.IsEncrypted(encryptedToken) ||
				stringConfigValue(current.Metadata, "app_id") != tuple.MetaAppID ||
				stringConfigValue(current.Metadata, "page_id") != tuple.PageID ||
				stringConfigValue(current.Metadata, "meta_business_id") != tuple.MetaBusinessID ||
				stringConfigValue(current.Metadata, "token_type") != "page" {
				return errMetaMessengerReviewUnavailable
			}
			result = metaMessengerReviewPagePostCredential{
				EncryptedAccessToken: strings.TrimSpace(encryptedToken),
				EncryptedTokenDigest: sha256.Sum256([]byte(encryptedToken)),
				CredentialID:         current.ID,
				CredentialVersion:    current.Version,
				PageName:             strings.TrimSpace(account.Name),
				Tuple:                tuple,
			}
			return nil
		},
	)
	return result, authErr, err
}

// revalidateMetaMessengerReviewPagePostCredential is a second short database
// fence after Meta responds. It prevents Page content from being returned if
// deprovisioning, credential rotation, deployment-generation replacement, or
// any other local revocation raced the provider request.
func (a *App) revalidateMetaMessengerReviewPagePostCredential(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	expected metaMessengerReviewPagePostCredential,
) error {
	root := a.rootApp()
	if root == nil || root.DB == nil || expected.CredentialID == uuid.Nil ||
		expected.CredentialVersion <= 0 {
		return errMetaMessengerReviewUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return database.WithTenantReadCommitted(
		root.DB.WithContext(ctx),
		organizationID,
		func(tx *gorm.DB) error {
			_, tuple, settingsErr := root.metaMessengerReviewSettings(time.Now().UTC())
			if settingsErr != nil || tuple != expected.Tuple ||
				tuple.OrganizationID != organizationID.String() ||
				tuple.ChannelAccountID != accountID.String() {
				return errMetaMessengerReviewUnavailable
			}

			var account models.ChannelAccount
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", accountID, organizationID).
				First(&account).Error; err != nil {
				return err
			}
			if !root.readyMetaMessengerReviewAccount(&account) {
				return errMetaMessengerReviewUnavailable
			}

			now := time.Now().UTC()
			var credentials []models.ChannelCredential
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
					organizationID,
					accountID,
					models.ChannelCredentialKindOAuth,
					[]models.ChannelCredentialStatus{
						models.ChannelCredentialStatusActive,
						models.ChannelCredentialStatusExpiring,
					},
					now,
				).
				Order("version DESC, id ASC").
				Find(&credentials).Error; err != nil {
				return err
			}
			if len(credentials) != 1 {
				return errMetaMessengerReviewUnavailable
			}
			current := credentials[0]
			encryptedToken, tokenOK := current.CredentialBlob["access_token"].(string)
			digest := sha256.Sum256([]byte(encryptedToken))
			if current.ID != expected.CredentialID ||
				current.OrganizationID != organizationID ||
				current.ChannelAccountID != accountID ||
				current.Kind != models.ChannelCredentialKindOAuth ||
				current.Version != expected.CredentialVersion ||
				current.KeyVersion != "app:v1" ||
				!tokenOK || strings.TrimSpace(encryptedToken) != encryptedToken ||
				!appcrypto.IsEncrypted(encryptedToken) ||
				subtle.ConstantTimeCompare(
					digest[:],
					expected.EncryptedTokenDigest[:],
				) != 1 ||
				stringConfigValue(current.Metadata, "app_id") != tuple.MetaAppID ||
				stringConfigValue(current.Metadata, "page_id") != tuple.PageID ||
				stringConfigValue(current.Metadata, "meta_business_id") != tuple.MetaBusinessID ||
				stringConfigValue(current.Metadata, "token_type") != "page" {
				return errMetaMessengerReviewUnavailable
			}
			return nil
		},
	)
}

func normalizeMetaMessengerReviewPagePosts(
	posts []metaMessengerReviewPagePost,
) ([]metaMessengerReviewPagePost, error) {
	if len(posts) > metaMessengerReviewPagePostLimit {
		return nil, errors.New("Meta returned too many review Page posts")
	}
	normalized := make([]metaMessengerReviewPagePost, 0, len(posts))
	for _, post := range posts {
		post.ID = strings.TrimSpace(post.ID)
		post.Message = strings.TrimSpace(post.Message)
		post.CreatedTime = strings.TrimSpace(post.CreatedTime)
		post.PermalinkURL = strings.TrimSpace(post.PermalinkURL)
		if post.ID == "" || len(post.ID) > 512 || len(post.Message) > 1<<20 ||
			!validMetaMessengerReviewPostTime(post.CreatedTime) ||
			!validMetaMessengerReviewPostPermalink(post.PermalinkURL) {
			return nil, errors.New("Meta returned a malformed review Page post")
		}
		normalized = append(normalized, post)
	}
	return normalized, nil
}

func validMetaMessengerReviewPostTime(value string) bool {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700"} {
		if parsed, err := time.Parse(layout, value); err == nil && !parsed.IsZero() {
			return true
		}
	}
	return false
}

func validMetaMessengerReviewPostPermalink(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		parsed.Hostname() == "" || parsed.Host != parsed.Hostname() ||
		parsed.Path == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "facebook.com" || strings.HasSuffix(host, ".facebook.com")
}
