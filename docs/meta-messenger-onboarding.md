# Managed Messenger onboarding

ReReply uses Meta Facebook Login for Business (FLfB) to stage Messenger Pages.
This flow prevents a workspace from connecting a similarly named Page, a Page
that it can only access as a client, or a Page owned by a different Meta
Business Portfolio.

The feature is disabled by default. Enabling it replaces manual creation of
`messenger` + `relay` accounts with the three authenticated endpoints below.
It does not publish the Meta app, submit App Review, add an account to the
protected relay registry, or enable outbound messaging.

For the single-Page, inbound-only staging path used while an app is under Meta
review, follow the
[staging-only Messenger review relay runbook](meta-messenger-review-relay.md).
Its pending review account must never be treated as a connected production
account or promoted by changing flags in place.

## Production prerequisites

This repository does not yet contain the privileged operator job that copies a
staged account's encrypted Page token and relay credentials into the deployment
secret manager, updates both protected relay inventories, deploys the relay,
and performs the matching audited deprovisioning sequence. The browser API
intentionally never returns those secrets. Until that reviewed, idempotent
provision/deprovision job exists, keep
`WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED=false` in every production
environment. The managed endpoints are staging-only; a successfully staged row
is not a connected channel.

Do not enable the flow until all of these facts are true and independently
recorded:

1. The configured Meta app and FLfB configuration belong to the intended Tech
   Provider Business Portfolio.
2. The Meta app is Live and has Advanced Access for `public_profile` (required
   for external FLfB users) plus the exact five Messenger onboarding
   permissions configured for this flow:
   `business_management`, `pages_manage_metadata`, `pages_messaging`,
   `pages_read_engagement`, and `pages_show_list`.
3. The public privacy, terms, and data-deletion URLs identify the correct legal
   provider entity.
4. The relay webhook app secret and verify token are held in the deployment's
   secret manager. They are not tenant settings.
5. ReReply and the relay share the same provider-proof key and protected
   expected-account inventory contract described in
   [meta-relay-preflight.md](meta-relay-preflight.md).
6. A deployment-owned recurring job revalidates each managed Page with its
   Page token: the Page `business` field still equals the protected Business,
   the read-only Page `conversations` check still proves the `MESSAGING` task
   and required permissions, and the intended app remains subscribed to
   `messages`. Ownership evidence must have a bounded maximum age. Any failure
   quarantines inbound and outbound delivery and starts the audited
   unsubscribe/deprovision path.

## Server configuration

Configure the ReReply service with deployment-controlled values. Never expose
the app secret through a frontend variable or tenant configuration.

```text
WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED=true
WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID=<numeric Meta app ID>
WHATOMATE_META_MESSENGER_ONBOARDING__CONFIG_ID=<numeric FLfB configuration ID>
WHATOMATE_META_MESSENGER_ONBOARDING__OWNER_BUSINESS_ID=<Tech Provider Business ID>
WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET=<secret>
WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_API_VERSION=v25.0
WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_BASE_URL=https://graph.facebook.com
WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL=<canonical relay base URL>
```

An enabled but incomplete configuration fails startup. Production Graph and
relay endpoints must use HTTPS. The app ID, configuration ID, owner Business
ID, Graph version, app secret, and both base URLs are included in the
one-time-session fingerprint, so changing any of them invalidates an in-flight
onboarding attempt.

## Browser and API contract

The browser first calls:

```text
POST /api/integrations/meta/messenger/onboarding/start
```

The response contains a one-time nonce and public Meta values only. The
browser invokes the Meta JavaScript SDK with:

```javascript
FB.login(callback, {
  config_id: publicConfig.config_id,
  response_type: "code",
  override_default_response_type: true,
});
```

Do not add a browser-supplied `scope`. FLfB permissions are fixed in the
deployment-controlled Meta configuration.

The browser passes the returned code and nonce to:

```text
POST /api/integrations/meta/messenger/onboarding/exchange
```

The server consumes the nonce once, exchanges the code, validates that the
token belongs to the configured app, accepts only Meta's exact `USER` or
`SYSTEM_USER` token types, and requires all five Messenger permissions. The
identity and inventory branch is determined only from the inspected token
type:

- `USER`: request `/me?fields=id,name`, then discover `/me/accounts`,
  `/me/businesses`, and each Business `/owned_pages` and `/client_pages`;
- `SYSTEM_USER` (the Business Integration System User mode used by the
  provider-owned FLfB configuration): request
  `/me?fields=id,client_business_id`, treat that
  single `client_business_id` as the canonical Business, read actual Page
  assignment, tasks, and Page tokens from
  `/{system-user-id}/assigned_pages`, and intersect those results with the
  canonical Business's `/owned_pages` and `/client_pages`.

The BISU branch never calls `/me/businesses` and never lets a client Business
or a browser value substitute for `client_business_id`.

A Page is selectable only when the same numeric Page ID has both current
authority and current ownership. In the `USER` branch, `/me/accounts` supplies
the actual tasks and Page token and the canonical Business's `/owned_pages`
supplies ownership. In the `SYSTEM_USER` branch,
`/{system-user-id}/assigned_pages` supplies the actual assigned tasks and Page
token and `/{client_business_id}/owned_pages` supplies ownership. Meta's
`permitted_tasks` values describe assignable capabilities and never satisfy
authorization. A client-access-only, unassigned, task-ineligible, tokenless,
granular-scope-restricted, or otherwise unverified Page can be displayed for
diagnosis but is never selectable. Page names, usernames, profile URLs, and
organization display names are not ownership evidence.

The user confirms the exact platform user, ReReply workspace, Meta Business
ID, and Page ID. The browser then calls:

```text
POST /api/integrations/meta/messenger/onboarding/select
```

The selection session is one-time. Immediately before persistence, the server
revalidates the token type, identity, permissions, Business, ownership,
`MESSAGING` task, granular targets, and Page token using the same authoritative
branch. It then validates the Page-token identity and verifies that the
configured app is subscribed to the Page's `messages` field.

Authorization codes and access tokens never enter API responses or logs. The
short-lived selection session contains only encrypted token material and is
deleted on use. The `USER` or BISU authorization token is not persisted after
selection; only the selected Page token is retained as an encrypted OAuth
credential. Token kind and, for BISU, `client_business_id` are retained as
server-controlled audit metadata.

## Staged account state

A successful selection creates a globally unique, tenant-owned
`messenger`/`relay` account with:

- status `pending`;
- management mode `meta_messenger_oauth`;
- ownership evidence `owned_pages_v1` and its verification timestamp;
- the exact Meta app, owner Business, selected Business, and Page IDs;
- an encrypted Page access token;
- distinct encrypted inbound and outbound relay HMAC credentials;
- `outbound_enabled=false` and `ai_reply_enabled=false`;
- onboarding state `awaiting_relay_registry`.

The database's global routable-identity index prevents one active Meta Page ID
from being staged in two workspaces, even with tenant row-level security.

The account remains unusable until an operator adds the exact organization,
Business, Page, and ReReply account IDs to the protected relay inventory. The
runtime trust resolver requires that the Business ID proven by `owned_pages`
matches the deployment inventory; one control plane cannot silently replace
the other.

## Activation checklist

This checklist is an operator design contract, not a currently automated
production path. Do not copy credential plaintext through a browser, shell
argument, log, ticket, chat, or temporary file to work around the missing
provisioning job.

After staging, complete these gates in order:

1. Review and promote the exact account tuple into the protected relay
   inventory and deploy both matching runtime copies.
2. Run the signed deployment preflight and verify the provider-proof key
   fingerprint matches across ReReply and the relay.
3. Run **Test** for the account. This proves current token identity,
   subscription, protected mapping, and relay health.
4. Send a new genuine external customer text DM after the successful Test and
   confirm it arrives in the intended ReReply workspace.
5. Obtain explicit workspace approval before enabling outbound messaging.

Test, inbound proof, and approval are deliberately invalidated by disconnects,
credential changes, protected-inventory changes, or a new health test. A
queued outbound send repeats the trust checks immediately before provider
delivery. Point-in-time onboarding evidence is not sufficient for a broadly
available multi-tenant release; the recurring ownership revalidation in the
production prerequisites is mandatory.

## Recovery

- **Client access only:** move or claim the Page under the intended Business
  Portfolio in Meta, then start a new session. Do not override the UI.
- **Ownership or permission changed:** the one-time selection fails; start
  again after correcting Meta access.
- **Subscription or finalization failed:** the Page remains safely pending with
  outbound and AI disabled. Start a fresh short-lived authorization session for
  the same workspace, Business, and Page. Before protected-registry promotion,
  the server may resume only that exact managed pending row, reuse its valid
  relay HMAC credential, rotate the Page OAuth credential, and retry the
  idempotent subscription/finalization sequence. It must never create a second
  row or resume another workspace's binding.
- **Already bound:** identify the existing workspace by protected operational
  records. Never work around the global uniqueness constraint with another
  Page name or account row.
- **Registry pending:** add the exact tuple through the reviewed deployment
  process. Tenant administrators cannot self-promote it.
- **Disconnect:** generic local deletion is blocked for OAuth-managed
  Messenger accounts. An operator must first remove the relay registry entry,
  unsubscribe/revoke the Meta authorization, stop inbound delivery, and then
  complete an audited local deprovisioning workflow. Soft-deleting only the
  ReReply row is not a valid disconnect.

Keep the feature disabled in a new environment until these failure paths and
the full activation checklist have been exercised with non-production assets.
