import type { ChannelAccount } from "@/services/productSuite";

export type MetaAccountReadinessKey =
  | "identity"
  | "permissions"
  | "relay_mapping"
  | "webhook_subscription"
  | "outbound_approval";

export type MetaAccountReadinessState = "complete" | "required" | "blocked";

export interface MetaAccountReadinessItem {
  key: MetaAccountReadinessKey;
  label: string;
  state: MetaAccountReadinessState;
  detail: string;
}

export interface MetaAccountReadiness {
  items: MetaAccountReadinessItem[];
  preflightReady: boolean;
  completedCount: number;
  requiredCount: number;
  hasLegacyOutboundApproval: boolean;
}

function configString(account: ChannelAccount, key: string) {
  const value = account.config?.[key];
  return typeof value === "string" ? value.trim() : "";
}

function identityLabel(account: ChannelAccount) {
  return account.channel === "messenger"
    ? "Selected Facebook Page"
    : "Selected Instagram identity";
}

function externalIdentityLabel(account: ChannelAccount) {
  return account.channel === "messenger" ? "Page ID" : "Instagram Graph ID";
}

export function isMetaRelayAccount(account: ChannelAccount) {
  return (
    account.provider === "relay" &&
    (account.channel === "instagram" || account.channel === "messenger")
  );
}

export function isManagedMessengerAccount(account: ChannelAccount) {
  return (
    account.channel === "messenger" &&
    configString(account, "onboarding_state") !== ""
  );
}

/**
 * Limits the browser deprovision control to the staging App Review account
 * identified by the server-owned review projection. The dedicated DELETE
 * handler remains authoritative and rechecks the full immutable deployment
 * tuple before it changes any state.
 */
export function isStagingMessengerReviewAccount(account: ChannelAccount) {
  const onboardingState = configString(account, "onboarding_state");
  return (
    account.channel === "messenger" &&
    account.provider === "relay" &&
    account.config?.review_only === true &&
    (account.external_account_id?.trim() ?? "") !== "" &&
    (onboardingState === "review_relay_ready" ||
      onboardingState === "review_relay_unavailable" ||
      onboardingState === "review_deprovisioning" ||
      onboardingState === "review_remote_cleanup_pending")
  );
}

export function messengerRelayRegistryRecognized(account: ChannelAccount) {
  if (!isManagedMessengerAccount(account)) return true;
  return (
    account.config?.registry_recognized === true &&
    configString(account, "onboarding_state") !== "awaiting_relay_registry"
  );
}

export function messengerReviewRelayReady(account: ChannelAccount) {
  return (
    isManagedMessengerAccount(account) &&
    configString(account, "onboarding_state") === "review_relay_ready" &&
    account.config?.review_only === true &&
    account.config?.registry_recognized === false &&
    account.config?.outbound_enabled === false &&
    account.config?.ai_reply_enabled === false &&
    account.status === "pending"
  );
}

export function messengerAwaitingRelayRegistry(account: ChannelAccount) {
  return (
    isManagedMessengerAccount(account) &&
    !messengerRelayRegistryRecognized(account)
  );
}

export const META_RECERTIFICATION_SEQUENCE =
  "Test pass -> provider-proven new incoming text DM from a genuine external customer profile not listed as a Meta app admin/developer/tester, strictly after Test -> refresh -> admin approval.";

export function metaAccountTestStopsDelivery(account: ChannelAccount) {
  return (
    isMetaRelayAccount(account) &&
    (account.config?.outbound_enabled === true ||
      account.config?.ai_reply_enabled === true)
  );
}

export function metaAccountTestActionLabel(account: ChannelAccount) {
  return metaAccountTestStopsDelivery(account) ? "Re-certify" : "Test";
}

export function metaAccountSettingsRequireRetest(
  account: ChannelAccount,
  relayURL: string,
  identityConfirmation: string,
  rotatesCredential: boolean,
) {
  return (
    isMetaRelayAccount(account) &&
    (configString(account, "relay_url") !== relayURL.trim() ||
      configString(account, "identity_confirmed_id") !==
        identityConfirmation.trim() ||
      rotatesCredential)
  );
}

export function confirmMetaAccountTest(
  account: ChannelAccount,
  confirm: (message: string) => boolean,
) {
  if (!metaAccountTestStopsDelivery(account)) return true;
  return confirm(
    `Re-certify ${account.name}?\n\nThis stops new outbound delivery and automatic AI replies. A provider request already dispatching across the network may still complete. New delivery can resume only after: ${META_RECERTIFICATION_SEQUENCE}\n\nContinue?`,
  );
}

export function confirmMetaAccountOutboundApproval(
  account: ChannelAccount,
  confirm: (message: string) => boolean,
) {
  if (!isMetaRelayAccount(account)) return true;
  return confirm(
    `Approve outbound for ${account.name}?\n\nConfirm that the provider-proven message was a new incoming text DM strictly after the latest Test, sent by a genuine external customer profile that is not listed as an admin, developer or tester on either Meta app. A photo, sticker or reaction alone does not qualify.`,
  );
}

/**
 * Builds the operator-facing proof required before a Meta relay can be used for
 * outbound delivery. Every signal is scoped to the channel account returned by
 * the current organization, and unknown evidence deliberately fails closed.
 */
export function metaAccountReadiness(
  account: ChannelAccount,
): MetaAccountReadiness {
  const externalID = account.external_account_id?.trim() ?? "";
  const confirmedID = configString(account, "identity_confirmed_id");
  const identityConfirmed = externalID !== "" && confirmedID === externalID;
  const healthCheckedAt = Date.parse(account.last_health_check_at ?? "");
  const inboundReceivedAt = Date.parse(account.last_inbound_at ?? "");
  const hasParseableHealthCheck = Number.isFinite(healthCheckedAt);
  const hasCurrentProviderProof = account.meta_provider_proof_version === "v1";
  const relayRegistryRecognized = messengerRelayRegistryRecognized(account);
  // The relay health endpoint only succeeds after its runtime App Review gate
  // is approved and its Graph checks confirm the exact external identity plus
  // subscribed_apps/messages. The protected deployment preflight separately
  // requires the broader future-organization Advanced Access profile. Do not
  // replace either server-side gate with a browser checkbox.
  const providerVerified = Boolean(
    relayRegistryRecognized &&
    account.has_credentials &&
    account.status === "active" &&
    hasParseableHealthCheck &&
    hasCurrentProviderProof &&
    !account.last_error_at &&
    !account.last_error,
  );
  const hasParseableInbound = Number.isFinite(inboundReceivedAt);
  const webhookDelivered =
    hasParseableInbound &&
    hasParseableHealthCheck &&
    inboundReceivedAt > healthCheckedAt;
  const webhookReady = providerVerified && webhookDelivered;
  const preflightReady = identityConfirmed && providerVerified && webhookReady;
  const outboundEnabled = account.config?.outbound_enabled === true;

  const items: MetaAccountReadinessItem[] = [
    {
      key: "identity",
      label: identityLabel(account),
      state: identityConfirmed ? "complete" : "required",
      detail: identityConfirmed
        ? `${account.name}: ${externalIdentityLabel(account)} ${externalID} is locked to this connection. Portfolio ownership is separately enforced at runtime by ReReply's deployment-held protected inventory.`
        : `Type the exact ${externalIdentityLabel(account)} to lock this connection to that immutable ID. Typing the ID does not prove portfolio ownership; ReReply's deployment-held protected inventory enforces ownership separately.`,
    },
    {
      key: "permissions",
      label: "Runtime permissions & App Review",
      state: providerVerified ? "complete" : "blocked",
      detail: providerVerified
        ? "The signed relay passed its approved runtime App Review gate and Graph access checks. ReReply matched its protected runtime inventory, while deployment preflight separately required the full future-organization Advanced Access profile."
        : !relayRegistryRecognized
          ? "Test remains locked until ReReply's protected runtime relay registry recognizes this workspace, business and Page authorization."
          : !hasCurrentProviderProof
            ? "Run a new Test on the provider-proof release. Older health and inbound timestamps cannot prove this deployment. Production also requires the future-organization Advanced Access profile."
            : "Test remains blocked until ReReply's protected runtime inventory matches and the relay reports approved runtime App Review plus valid Graph access. Production also requires the future-organization Advanced Access profile.",
    },
    {
      key: "relay_mapping",
      label: "Relay mapping & health",
      state: providerVerified ? "complete" : "blocked",
      detail: providerVerified
        ? `The relay verified ${externalIdentityLabel(account)} ${externalID} and the current HMAC credentials.`
        : !relayRegistryRecognized
          ? `Awaiting runtime relay registry recognition for ${externalIdentityLabel(account)} ${externalID || "shown above"}. Test and outbound approval remain unavailable.`
          : `Run Test. The relay must verify ${externalIdentityLabel(account)} ${externalID || "shown above"}, the matching HMAC credentials and provider-proof v1.`,
    },
    {
      key: "webhook_subscription",
      label: "Webhook subscription & inbound test",
      state: webhookReady ? "complete" : "blocked",
      detail: !providerVerified
        ? "Run Test. The relay must prove subscribed_apps/messages for this exact asset before webhook delivery can be trusted."
        : webhookDelivered
          ? "The relay proved the messages subscription and this connection received a provider-proven new incoming text DM after Test."
          : account.last_inbound_at && !hasParseableInbound
            ? "The incoming-customer timestamp is invalid, so delivery cannot be proven. Send a new incoming text DM from a genuine external customer profile that is not an app admin, developer or tester, then refresh."
            : hasParseableInbound && inboundReceivedAt <= healthCheckedAt
              ? "The last provider-proven incoming text DM does not postdate the latest relay Test. Send a new incoming text DM from a genuine external customer profile that is not an app admin, developer or tester."
              : "The messages subscription is verified, but no provider-proven incoming text DM has reached this connection since the latest Test. Send a new incoming text DM from a genuine external customer profile that is not an app admin, developer or tester.",
    },
    {
      key: "outbound_approval",
      label: "Outbound approval",
      state:
        outboundEnabled && preflightReady
          ? "complete"
          : preflightReady
            ? "required"
            : "blocked",
      detail:
        outboundEnabled && !preflightReady
          ? "Outbound was approved before this proof checklist existed. Review the missing evidence; changing these settings will disable outbound until re-approved."
          : outboundEnabled
            ? "An administrator explicitly approved outbound delivery after every prerequisite passed."
            : preflightReady
              ? "Every prerequisite passed. An administrator may now approve outbound delivery."
              : "Approval stays locked until identity, permissions, relay mapping and webhook delivery are all verified.",
    },
  ];

  return {
    items,
    preflightReady,
    completedCount: items.filter((item) => item.state === "complete").length,
    requiredCount: items.length,
    hasLegacyOutboundApproval: outboundEnabled && !preflightReady,
  };
}
