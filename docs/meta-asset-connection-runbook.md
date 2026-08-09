# Meta asset connection diagnosis and rollout runbook

Use this runbook when one ReReply workspace receives Meta conversations but a
similarly configured workspace does not. A visible Facebook, Messenger, or
Instagram conversation is not by itself proof that ReReply is connected.

## ReAlign Kajang versus Klinik Insan: evidence-based comparison

The observed ReAlign Kajang behaviour proves that its effective runtime path
can route provider webhooks into the intended ReReply workspace. The Klinik
Insan screenshots prove only that test messages were sent successfully in
Facebook and Instagram. They do **not** prove that the intended Klinik Insan
Page or Instagram account was authorized to the Medtech app, subscribed to the
correct webhook, registered in the protected relay inventory, or routed to a
Klinik Insan ReReply account.

The Klinik Insan test also exposed a high-risk ambiguity: more than one Meta
Page used a similar Klinik Insan name. Sending to the other Page changed which
Meta conversation displayed, but it did not connect the intended asset to
ReReply. Human-readable names, usernames, profile URLs, and message text must
never be used as routing identity.

Tech Provider verification is a provider-level prerequisite, not an
organization connection. Likewise, adding permissions to an App Review draft
does not grant them to external organizations, and an unpublished app cannot be
treated as a live multi-tenant connection.

Therefore the current diagnosis is an incomplete or mismatched authorization
and relay-registration chain, not an omnichannel inbox rendering problem. Do
not label Klinik Insan connected until the complete proof sequence below
passes.

## The identity tuple

Every Meta channel must be bound to an exact, deployment-protected tuple:

```text
ReReply organization ID
Meta owning Business Portfolio ID
Meta app ID
channel type
external asset ID (Page, Instagram account, Threads account, or phone number ID)
```

The ReReply channel-account ID and relay credential versions are added after
staging. The tuple must match in the application database, both protected relay
inventories, provider subscription, and deployment secret manager.

## Connection proof sequence

For every new organization:

1. Confirm the legal provider entity, verified Tech Provider Business, app
   owner, and public legal URLs are consistent.
2. Use a deployment-controlled Facebook Login for Business configuration. Do
   not accept browser-supplied permissions, app IDs, Business IDs, or relay
   origins.
3. Inspect the token server-side and accept only the expected app, exact token
   type, required permissions, and unexpired data access.
4. Discover stable numeric IDs from Meta. An owned asset may be selectable only
   after an authoritative ownership intersection and required task check.
   Client, tokenless, scope-restricted, or unverified assets remain visible only
   for diagnosis.
5. Show the operator the exact workspace, Business ID, Page/account ID, and
   platform identity before confirmation.
6. Create a globally unique **pending** account with outbound and AI disabled.
   A database constraint must prevent the same routable asset from being bound
   to two workspaces.
7. Provision the exact encrypted credentials and account tuple into the
   deployment secret manager and both protected relay inventories through an
   audited, idempotent operator job. Never copy plaintext through the browser,
   logs, chat, tickets, command arguments, or temporary files.
8. Verify the provider subscription and protected relay health, then run the
   ReReply **Test** action.
9. After Test, send a new text DM from a genuinely external customer account.
   Confirm that it arrives in the intended ReReply workspace. An outgoing
   message visible only in Meta does not satisfy this gate.
10. Obtain explicit workspace approval before enabling outbound delivery, then
    enable AI replies separately if requested.
11. Revalidate ownership, task/permission eligibility, token identity, and app
    subscription on a bounded schedule. Quarantine the account immediately on
    loss or transfer and use the audited deprovision path.

## Channel-specific minimums

- **Messenger:** owning Business plus Page ID, `MESSAGING` task, Page token,
  exact app subscription to `messages`, protected relay mapping, and a fresh
  external inbound DM.
- **Instagram:** exact Instagram professional account ID and its owning/linked
  Meta assets; a username alone is not identity. Prove a new inbound Instagram
  message after subscription.
- **Threads:** exact Threads account/app binding and the approved Threads
  permissions; Instagram authorization does not automatically authorize
  Threads.
- **WhatsApp:** exact WABA and phone-number ID, verified webhook, approved
  template/account state, and a fresh inbound message; a display number alone
  is not identity.

## Release rule

If any stage is missing, the account stays pending, outbound stays disabled,
and the UI must state the missing gate. A channel becomes “connected” only
after current ownership, provider subscription, protected relay mapping,
health Test, and fresh external inbound proof all agree on the same tuple.
