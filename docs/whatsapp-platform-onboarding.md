# Shared WhatsApp onboarding for new workspaces

New workspaces do not inherit another workspace's Meta settings, connected
accounts, or authorization tokens. Their **More tools > Accounts** page uses
the platform's WhatsApp app configuration until an administrator supplies
workspace-specific settings. The **Connect with Facebook** button requires
account-write permission and a resolved App ID, Embedded Signup configuration
ID, and server-side App Secret. **Sync with Mobile App** is inside that flow.

An empty platform configuration leaves new workspaces unable to start signup.
Saving credentials in **Settings > Integrations** affects only the selected
workspace, including when the operator is a super administrator.

## Platform configuration

The web process reads `[whatsapp]` in its TOML configuration and then applies
environment overrides. Use a double underscore between the section and field:

| Web-service environment key | TOML field | Type | Scope |
| --- | --- | --- | --- |
| `WHATOMATE_WHATSAPP__APP_ID` | `whatsapp.app_id` | General | Runtime |
| `WHATOMATE_WHATSAPP__CONFIG_ID` | `whatsapp.config_id` | General | Runtime |
| `WHATOMATE_WHATSAPP__APP_SECRET` | `whatsapp.app_secret` | Secret | Runtime |

On the production App Platform topology these keys belong only to
`omnitech-web`. They do not belong on the relay services or migration job.
Preserve `WHATOMATE_WHATSAPP__WEBHOOK_VERIFY_TOKEN` and its existing Meta callback
configuration. Restart/redeploy the web process through the release owner's
reviewed procedure; configuration is loaded at startup.

Obtain all three values from the same approved, platform-owned WhatsApp Meta
application. Verify the app's business ownership, Live status, WhatsApp
permissions for external customers, Embedded Signup configuration and supported
version, allowed domain, and callback before enabling signup. Do not substitute
Messenger credentials, another customer's access token, or a secret recovered
from a tenant's settings. Keep the App Secret out of Git, reports and logs.

## Callback routing prerequisite

Embedded Signup currently subscribes the app to the WABA without setting or
verifying an alternate callback, then requests the initial coexistence sync.
The phone webhook override action is a separate operation; signup does not
call it automatically. A later manual override cannot establish that the
initial sync arrived at ReReply. If the selected
Meta app's default callback serves another product, preserve that callback.
Before enabling general self-service onboarding, establish and review a route
that delivers the required phone and WABA/coexistence events to ReReply while
preserving the other product's traffic. A successful app subscription or a
visible **Connect with Facebook** button does not prove event delivery.

Acceptance must include authenticated inbound messages, a reply, and the
required mobile-app echo, history and account-lifecycle events for the newly
connected customer. Meta's [webhook override documentation](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/override/)
supports overrides for messages, mobile-app echoes, contact sync and history,
but account and template lifecycle events still use the app's default callback.
It also states that subscribing with no post body removes an existing WABA
alternate callback. Preserve existing routing and verify the selected design
with the release owner before the configuration transition.

## Existing-account compatibility

Deploy and verify the account-credential compatibility change before adding
platform credentials to a deployment that previously had none. A different
platform app must not replace the app identity or webhook signer of an existing
legacy account.

The account resolver preserves an unmanaged legacy account's ID/secret pair
when it cannot establish that the account uses the platform app. It does not
combine an unrelated legacy ID or secret with platform credentials. When the
IDs match, the current platform secret takes precedence so same-app rotation
continues to work. Explicit workspace settings and managed Integration Center
state remain authoritative, including disabled state and decryption failures.

Before rollout, inventory configuration *presence and ownership*, without
exporting secrets: platform-managed workspaces, complete workspace overrides,
partial overrides, managed-disabled providers, and legacy account credentials.
Partial workspace overrides need explicit resolution before enabling shared
defaults: the existing workspace resolver can combine individual override
fields with platform values. Do not automatically fill or migrate them.

## Coordinated production rollout

1. Finish any release validation already bound to a particular source commit.
   Review the compatibility patch separately and validate the eventual merged
   source through staging. Keep the three new runtime keys absent while the
   older production binary is running.
2. Release the compatible binary through the normal image-only production
   process and verify its source and health.
3. Review a separate configuration-only transition. Bind it to the actual
   production app, active deployment and a fresh complete spec. Preserve every
   unrelated field, image digest and existing environment entry. The only
   proposed additions are the three web-service runtime keys above.
4. Serialize the transition with release work and reject a changed pre-state.
   Preserve the exact previous configuration privately for restoration, apply
   once, and verify the resulting deployment, configuration and health.
   Ordinary image rollback does **not** remove or restore environment entries.
5. Establish a fresh release baseline/dry-run after the settled configuration
   change. Do not add environment entries to `ship.yml`'s image-only PUT or
   weaken its comparison checks.
6. Verify Greentact or the affected workspace and a newly provisioned test
   workspace return their own organisation ID, the expected public App ID and
   configuration ID, and `has_app_secret=true`. Their public responses must
   contain no secret or access token. Confirm **Connect with Facebook** opens
   the mobile-app choice for an authorised administrator.
7. Confirm existing customers' credentials and provider state are unchanged.
   Recheck existing connections for representative platform, workspace-owned,
   and legacy app identities. Complete an actual customer connection only with
   that customer's Meta authorization; visibility of the button alone does
   not prove successful onboarding.

## Focused regression checks

Run the handler tests with disposable local PostgreSQL and Redis, never against
a customer database. The fresh-organisation test exercises actual creation and
the public signup endpoint with and without shared defaults; it also verifies
that source-workspace settings and connections are not copied. Account-resolver
and webhook tests cover legacy identities, same-app secret rotation, managed
authority and incomplete credentials.

This is configuration and credential-selection work. It requires no schema
migration, no customer-account deletion, and no bulk creation of tenant
integration records.
