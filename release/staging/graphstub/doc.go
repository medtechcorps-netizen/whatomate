// Package graphstub is a synthetic Meta Graph API for local runs, CI and
// staging. The product uses it unchanged: whatsapp.base_url points at the
// stub, and the stub posts signed webhooks back to the product.
//
// It uses the Go standard library only and lives under release/, outside the
// schema guard's scanned roots (release/deployment/schema_change.py) and
// outside every release binary: placement_test.go proves that ./cmd/whatomate,
// ./cmd/meta-relay and ./cmd/gmail-relay, the only packages the release
// Dockerfiles build, depend on nothing under release/. Only
// docker/staging/graph-stub.Dockerfile builds the stub.
//
// # Graph endpoints
//
// Paths work with or without a /v{n}.{m} prefix. Every endpoint except the
// two token endpoints needs "Authorization: Bearer <token>" with a token from
// STUB_ACCESS_TOKENS.
//
//	POST   /{phone}/messages                 send (new WAMID, then scheduled status webhooks) or read receipt
//	POST   /{phone}/media                    multipart upload, returns a media ID
//	GET    /{media}                          media metadata with a same-origin download URL
//	DELETE /{media}                          delete media
//	GET    /_media/{media}                   download (the URL GET /{media} returns)
//	GET    /{phone}                          phone fields, including webhook_configuration
//	POST   /{phone}                          webhook_configuration.override_callback_uri
//	POST   /{phone}/register                 two-step PIN registration
//	GET    /{phone}/whatsapp_business_profile
//	POST   /{phone}/whatsapp_business_profile
//	GET    /{waba}                           WABA fields
//	GET    /{waba}/phone_numbers             paged with limit and after
//	GET    /{waba}/message_templates         paged with limit and after
//	POST   /{waba}/message_templates         create (PENDING until the control API reviews it)
//	DELETE /{waba}/message_templates?name=   delete every language of a template
//	GET    /{template}                       template fields
//	POST   /{template}                       edit components
//	GET    /{waba}/subscribed_apps           POST subscribes, DELETE unsubscribes
//	GET|POST /oauth/access_token             client_credentials, or a code from the control API
//	GET    /debug_token?input_token=         bearer is the app token app_id|app_secret
//
// Errors use Meta's {"error":{...}} envelope. Anything else answers 400 with
// Graph error 100, and the journal records the route as "unsupported".
//
// # Control API
//
// /_control/* drives the stub from tests. Each request is authenticated with
// an HMAC (see ControlMAC and SignControlRequest) over the path the stub
// receives: a missing header, a wrong key, a timestamp more than 60 s away or
// a reused nonce is answered 401. The staging ingress exposes it only under
// its /_stub prefix, which the ingress strips before the stub sees a request.
//
//	GET    /_control/journal?after=&limit=   journal entries after a sequence number
//	GET    /_control/accounts                POST registers a WABA and phone (idempotent)
//	POST   /_control/inbound                 deliver a signed inbound customer message now
//	POST   /_control/status                  deliver one status webhook for an accepted send
//	POST   /_control/media                   store media for an inbound media message
//	POST   /_control/templates               set a template's review status and notify
//	POST   /_control/oauth/codes             issue a single-use Embedded Signup code
//	POST   /_control/faults                  queue a fault; DELETE clears the queue
//	POST   /_control/reset                   back to the startup state
//
// # Environment
//
//	STUB_ENVIRONMENT      required: local or staging; anything else refuses to start
//	STUB_CONTROL_KEY      required: 32+ printable characters, the control API HMAC key
//	STUB_ACCESS_TOKENS    required: comma-separated Graph bearer tokens (16+ characters each)
//	STUB_APP_ID           required: numeric synthetic Meta app ID
//	STUB_APP_SECRET       required: 16+ characters; signs every webhook (X-Hub-Signature-256)
//	STUB_CALLBACK_ORIGIN  required: http(s) origin of the product; the only address the stub dials
//	STUB_LISTEN_ADDR      default :8090
//	STUB_ACCOUNTS         optional JSON array of {business_account_id, phone_number_id,
//	                      display_phone_number, verified_name}
//	STUB_STATUS_SEQUENCE  default sent,delivered; none disables scheduled statuses
//	STUB_STATUS_INTERVAL  default 250ms between scheduled statuses
//
// # Safety
//
// The stub refuses to start outside local and staging, without a control key,
// or with a callback on rereply.app, facebook.com, fbsbx.com, instagram.com or
// googleapis.com (or a subdomain). It answers 421 to requests addressed to
// those hosts and refuses webhook overrides on them. Webhooks go only to
// STUB_CALLBACK_ORIGIN: a phone's override contributes its path and query,
// never its host, and the dialer refuses any other address. State lives in
// memory; the journal keeps the last 10,000 requests and webhooks, and
// neither the journal nor the logs hold a body, token or secret.
package graphstub
