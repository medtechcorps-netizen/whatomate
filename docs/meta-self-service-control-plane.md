# Meta self-service control plane foundation

This split-A foundation defines how a later lifecycle release can remove
per-clinic relay environment edits and deployments. It is deliberately
impossible to enable in production today: both ReReply and the relay reject
dynamic registry credentials during startup. Existing static mappings remain
the only supported production path.

## Trust boundaries

1. ReReply's approved Messenger parent app and separate Instagram Login app
   remain deployment-owned. Their app secrets and verify tokens stay on the
   relay.
2. A clinic authorizes assets through those platform apps. The future OAuth
   completion handler must verify the token, scopes, Page/professional-account
   ownership, and selected Business before marking a binding active.
3. ReReply stores the asset as a tenant-owned `channel_accounts` row. Provider
   access tokens use an encrypted, versioned OAuth `channel_credentials` row;
   ReReply inbound/outbound HMAC values use a separate encrypted, versioned
   webhook credential row.
4. The isolated relay obtains only a short binding lease through the private
   signed broker. It has no database credential and no application encryption
   key.

## Required persisted state

The OAuth completion transaction must set these non-secret values:

- `channel_accounts.provider = relay`;
- `config.meta_registry_managed = true`;
- `config.meta_management_mode = platform_oauth`;
- `config.rereply_webhook_url` to the exact account webhook;
- `config.instagram_api_mode` to `instagram_login` or `facebook_login` for
  Instagram (and omit it for Messenger);
- `metadata.meta_ownership_state = verified`;
- `metadata.meta_ownership_checked_at` as UTC RFC3339Nano; and
- the provider app key, verified scopes, Business ID, and authorizing Meta user
  as non-secret metadata needed for later revalidation/deauthorization.

The transaction must create both credentials, write a redacted audit event,
and rely on `uq_channel_accounts_global_routable_identity` to prevent the same
Meta asset from being owned by two workspaces.

The generic channel API reserves these managed markers regardless of the value
a caller tries to inject. It cannot create a managed Meta row, replace its
routing/capability configuration, rotate its credentials, or delete it. Normal
profile/default changes remain available. Outbound approval and AI opt-in also
require a successful relay+Graph health result no older than 15 minutes;
disconnect and credential changes must go through the version-fenced Meta
lifecycle.

## Remaining product work

- Merge or reimplement the reviewed Messenger Facebook Login for Business flow
  against this production registry instead of stopping at
  `awaiting_relay_registry`.
- Add the equivalent Instagram Login OAuth flow and token exchange.
- Add a scheduled Graph revalidator for ownership, required scopes, expiry,
  subscribed-app state, and token binding. It should call the version-fenced
  internal revalidation transition.
- Add provider-signed deauthorization callbacks on the relay. After validating
  the platform app signature, the relay calls the version-fenced internal
  revoke transition.
- Add onboarding UI, safe reconnect/rotation, disconnect confirmation, and
  health explanations. No flow may accept a clinic app secret or bypass the
  platform app boundary.

All items above are release blockers, not optional follow-ups. Lifecycle B must
also provide a real backend caller for OAuth completion/reconnect/disconnect,
keep a new account outbound-disabled until a fresh relay+Graph health test and
explicit administrator approval, revalidate ownership/scopes/token binding and
`subscribed_apps` before the ownership age expires, and verify provider-signed
deauthorization before revoking both credential versions. Only after those
tests pass may a code change remove the two startup gates.

The unexported provisioning helper in split A is therefore a persistence
contract and test fixture, not an activation endpoint. Lifecycle B must own the
real pending -> health-verified -> explicitly approved -> active transition;
it must not expose the helper directly or treat its provisional active row as
customer-ready. The private broker must also receive service-edge rate limits
before its production gate is removed.

The later rollout must deploy registry-aware queue readers everywhere before
enabling its producer. Dynamic jobs use a credential fence and queue schema 2;
old workers must be drained before enablement and may not be reintroduced until
no registry-fenced jobs remain.

`origin/codex/meta-channel-safe-reconnect` is useful reference code for the
Messenger authorization and review evidence flow, but it is not a drop-in
production registry: its documented production endpoint still requires an
operator-maintained relay registry, its dynamic broker is staging-review-only,
and it has no Instagram OAuth implementation.
