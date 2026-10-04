package graphstub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/logf"
)

// The tests in this file use the product's own code against the stub; only
// test binaries import it, so the stub itself stays standard-library only.

// TestProductClientAgainstStub drives the stub with pkg/whatsapp configured
// the way staging configures the product: whatsapp.base_url is the stub.
func TestProductClientAgainstStub(t *testing.T) {
	h := newHarness(t, func(config *Config) { config.StatusSequence = []string{"sent", "delivered"} })
	client := whatsapp.NewWithBaseURL(logf.New(logf.Opts{Writer: io.Discard, Level: logf.FatalLevel}), h.url)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account := &whatsapp.Account{PhoneID: testPhone, BusinessID: testWABA, AppID: testAppID, APIVersion: "v21.0", AccessToken: testToken}

	validation, err := client.ValidateCredentials(ctx, testPhone, testWABA, testToken, account.APIVersion)
	if err != nil || validation.PhoneNumber != "+1 555-0101" || validation.VerifiedName != "Stub Clinic" || validation.IsTestNumber || validation.Warning != "" {
		t.Fatalf("ValidateCredentials: %+v, %v", validation, err)
	}
	if _, err := client.ValidateCredentials(ctx, testPhone, testWABA2, testToken, account.APIVersion); err == nil {
		t.Fatal("a phone validated against a WABA it does not belong to")
	}
	if _, err := client.ValidateCredentials(ctx, testPhone, testWABA, "not-a-configured-token", account.APIVersion); err == nil {
		t.Fatal("an unknown token validated")
	}
	if info, err := client.GetPhoneNumberInfo(ctx, testPhone, testToken, account.APIVersion); err != nil || info.PlatformType != "CLOUD_API" {
		t.Fatalf("GetPhoneNumberInfo: %+v, %v", info, err)
	}
	if phones, err := client.GetWABAPhoneNumbers(ctx, testWABA, testToken, account.APIVersion); err != nil || len(phones.Data) != 2 {
		t.Fatalf("GetWABAPhoneNumbers: %+v, %v", phones, err)
	}

	recipient := whatsapp.Recipient{Phone: testCustomer}
	wamid, err := client.SendTextMessage(ctx, account, recipient, "synthetic hello")
	if err != nil {
		t.Fatalf("SendTextMessage: %v", err)
	}
	for _, want := range []string{"sent", "delivered"} {
		webhook := h.product.next(t)
		payload, err := whatsapp.ParseWebhook(webhook.Body)
		if err != nil {
			t.Fatal(err)
		}
		statuses := payload.ExtractStatuses()
		if len(statuses) != 1 || statuses[0].MessageID != wamid || statuses[0].Status != want || payload.GetPhoneNumberID() != testPhone {
			t.Fatalf("status webhook %+v", statuses)
		}
	}
	if err := client.MarkMessageRead(ctx, account, "wamid.inbound-1"); err != nil {
		t.Fatalf("MarkMessageRead: %v", err)
	}

	// The product sends its bearer only to a download URL on the configured
	// base origin, so a successful download proves the URL is same-origin.
	payload := []byte("synthetic media bytes")
	mediaID, err := client.UploadMedia(ctx, account, payload, "image/png", "sample.png")
	if err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	if _, err := client.SendImageMessage(ctx, account, recipient, mediaID, "caption"); err != nil {
		t.Fatalf("SendImageMessage: %v", err)
	}
	mediaURL, err := client.GetMediaURL(ctx, mediaID, account)
	if err != nil {
		t.Fatalf("GetMediaURL: %v", err)
	}
	if downloaded, err := client.DownloadMedia(ctx, mediaURL, testToken); err != nil || !bytes.Equal(downloaded, payload) {
		t.Fatalf("DownloadMedia: %q, %v", downloaded, err)
	}

	templateID, err := client.SubmitTemplate(ctx, account, &whatsapp.TemplateSubmission{
		Name: "visit_reminder", Language: "en", Category: "UTILITY", BodyContent: "Your visit is on {{1}}",
		SampleValues: []any{"Monday"},
	})
	if err != nil || templateID == "" {
		t.Fatalf("SubmitTemplate: %q, %v", templateID, err)
	}
	if _, err := client.SubmitTemplate(ctx, account, &whatsapp.TemplateSubmission{
		MetaTemplateID: templateID, Name: "visit_reminder", Language: "en", Category: "UTILITY", BodyContent: "Your visit is on {{1}}.",
		SampleValues: []any{"Monday"},
	}); err != nil {
		t.Fatalf("SubmitTemplate update: %v", err)
	}
	h.mustControl(http.MethodPost, "/_control/templates", map[string]any{"business_account_id": testWABA, "name": "visit_reminder", "status": "APPROVED"})
	h.product.next(t)
	templates, err := client.FetchTemplates(ctx, account)
	if err != nil || len(templates) != 1 || templates[0].ID != templateID || templates[0].Status != "APPROVED" || templates[0].Components[0].Text != "Your visit is on {{1}}." {
		t.Fatalf("FetchTemplates: %+v, %v", templates, err)
	}
	if _, err := client.SendTemplateMessage(ctx, account, recipient, "visit_reminder", "en", nil); err != nil {
		t.Fatalf("SendTemplateMessage: %v", err)
	}
	if err := client.DeleteTemplate(ctx, account, "visit_reminder"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	if subscribed, err := client.IsAppSubscribed(ctx, testWABA, testAppID, testToken, account.APIVersion); err != nil || subscribed {
		t.Fatalf("IsAppSubscribed before: %v, %v", subscribed, err)
	}
	if err := client.SubscribeApp(ctx, account); err != nil {
		t.Fatalf("SubscribeApp: %v", err)
	}
	if subscribed, err := client.IsAppSubscribed(ctx, testWABA, testAppID, testToken, account.APIVersion); err != nil || !subscribed {
		t.Fatalf("IsAppSubscribed after: %v, %v", subscribed, err)
	}
	if err := client.ConfigurePhoneWebhookOverride(ctx, account, "https://product.example.test/api/webhook?workspace=1", "verify-token-for-tests"); err != nil {
		t.Fatalf("ConfigurePhoneWebhookOverride: %v", err)
	}
	if err := client.RegisterPhoneNumber(ctx, testPhone, "123456", testToken, account.APIVersion); err != nil {
		t.Fatalf("RegisterPhoneNumber: %v", err)
	}
	if err := client.UpdateBusinessProfile(ctx, account, whatsapp.BusinessProfileInput{About: "Open daily", Websites: []string{"https://clinic.example.test"}}); err != nil {
		t.Fatalf("UpdateBusinessProfile: %v", err)
	}
	if profile, err := client.GetBusinessProfile(ctx, account); err != nil || profile.About != "Open daily" || len(profile.Websites) != 1 {
		t.Fatalf("GetBusinessProfile: %+v, %v", profile, err)
	}

	h.mustControl(http.MethodPost, "/_control/oauth/codes", map[string]any{"code": "synthetic-signup-code", "access_token": testToken2})
	token, err := client.ExchangeCodeForToken(ctx, "synthetic-signup-code", testAppID, testAppSecret, account.APIVersion)
	if err != nil || token != testToken2 {
		t.Fatalf("ExchangeCodeForToken: %v", err)
	}
	debug, err := client.GetTokenDebugInfo(ctx, token, testAppID+"|"+testAppSecret)
	if err != nil || !debug.IsValid || debug.AppID != testAppID || len(debug.GranularScopes) != 2 {
		t.Fatalf("GetTokenDebugInfo: %+v, %v", debug, err)
	}

	// A Graph fault reaches the product as a parsed Meta error.
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeMessages, "status": 400, "code": 131047, "message": "Re-engagement message"})
	_, err = client.SendTextMessage(ctx, account, recipient, "outside the window")
	var metaErr *whatsapp.MetaHTTPError
	if !errors.As(err, &metaErr) || metaErr.StatusCode != http.StatusBadRequest || metaErr.MetaCode != 131047 {
		t.Fatalf("the fault reached the client as %v", err)
	}
}

// productWebhookStatus feeds one webhook the stub delivered to the product's
// own handler, which checks X-Hub-Signature-256 before anything else.
func productWebhookStatus(t *testing.T, app *handlers.App, webhook capturedWebhook) int {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(http.MethodPost)
	request.RequestCtx.Request.Header.SetContentType("application/json")
	request.RequestCtx.Request.Header.Set(SignatureHeader, webhook.Signature)
	request.RequestCtx.Request.SetBody(webhook.Body)
	if err := app.WebhookHandler(request); err != nil {
		t.Fatalf("WebhookHandler: %v", err)
	}
	return testutil.GetResponseStatusCode(request)
}

// TestProductWebhookVerifierAcceptsStubSignatures proves the product's real
// verifier (internal/handlers/webhook.go) accepts what the stub signs, for a
// phone-scoped status and a WABA-scoped template update, and still refuses a
// wrong secret or a changed body. It needs the PostgreSQL and Redis services
// the go-race CI job provides and skips without them.
func TestProductWebhookVerifierAcceptsStubSignatures(t *testing.T) {
	db := testutil.SetupTestDB(t)
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	app := &handlers.App{
		Config: &config.Config{
			App:      config.AppConfig{EncryptionKey: "graph-stub-verifier-test-encryption-key"},
			JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7},
			WhatsApp: config.WhatsAppConfig{BaseURL: "http://127.0.0.1:9", APIVersion: "v21.0"},
		},
		DB:         db,
		Log:        testutil.NopLogger(),
		Redis:      redisClient,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	phoneID, wabaID := testutil.NewTestGraphObjectID(), testutil.NewTestGraphObjectID()
	organization := testutil.CreateTestOrganization(t, db)
	if err := db.Create(&models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		Name:               "graph-stub-" + phoneID[len(phoneID)-6:],
		PhoneID:            phoneID,
		BusinessID:         wabaID,
		AccessToken:        testToken,
		AppSecret:          testAppSecret,
		WebhookVerifyToken: "verify-token-for-tests",
		APIVersion:         "v21.0",
		Status:             "active",
	}).Error; err != nil {
		t.Fatalf("create account: %v", err)
	}

	h := newHarness(t, func(config *Config) {
		config.Accounts = []Account{{BusinessAccountID: wabaID, PhoneNumberID: phoneID, DisplayPhoneNumber: "+1 555-0109"}}
		config.StatusSequence = []string{"sent"}
	})
	if status, body := h.sendText(testToken, phoneID, "verifier status"); status != http.StatusOK {
		t.Fatalf("send: %d %v", status, body)
	}
	statusWebhook := h.product.next(t)
	if status, body := h.graph(http.MethodPost, v+"/"+wabaID+"/message_templates", testToken, map[string]any{
		"name": "verifier_check", "language": "en", "category": "UTILITY",
		"components": []any{map[string]any{"type": "BODY", "text": "hello"}},
	}); status != http.StatusOK {
		t.Fatalf("create template: %d %v", status, body)
	}
	h.mustControl(http.MethodPost, "/_control/templates", map[string]any{"business_account_id": wabaID, "name": "verifier_check", "status": "PAUSED"})
	templateWebhook := h.product.next(t)
	h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{"phone_number_id": phoneID, "from": testCustomer, "text": "verifier inbound"})
	inboundWebhook := h.product.next(t)

	for _, tc := range []struct {
		name    string
		webhook capturedWebhook
		// exact is the status the product must answer; zero accepts any
		// status except 403, the answer to a signature it refuses.
		exact int
	}{
		{"inbound message", inboundWebhook, fasthttp.StatusOK},
		{"template status", templateWebhook, fasthttp.StatusOK},
		// The product has no outbound row for the stub's WAMID, so after the
		// signature check it asks Meta to retry.
		{"message status", statusWebhook, 0},
	} {
		webhook := tc.webhook
		t.Run(tc.name, func(t *testing.T) {
			got := productWebhookStatus(t, app, webhook)
			if got == fasthttp.StatusForbidden || (tc.exact != 0 && got != tc.exact) {
				t.Fatalf("the product answered %d to the stub's webhook", got)
			}
			wrongSecret := webhook
			wrongSecret.Signature = SignWebhook("another-app-secret-value", webhook.Body)
			if got := productWebhookStatus(t, app, wrongSecret); got != fasthttp.StatusForbidden {
				t.Fatalf("a wrong secret was answered %d", got)
			}
			// Still valid JSON, so only the signature check can refuse it.
			changed := webhook
			changed.Body = append([]byte("{ "), webhook.Body[1:]...)
			if got := productWebhookStatus(t, app, changed); got != fasthttp.StatusForbidden {
				t.Fatalf("a changed body was answered %d", got)
			}
		})
	}
}
