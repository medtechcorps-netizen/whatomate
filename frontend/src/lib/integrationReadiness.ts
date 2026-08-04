import type {
  IntegrationChannel,
  IntegrationProvider,
  IntegrationState,
} from "@/services/integrations";

export type IntegrationReadinessState =
  | "ready"
  | "prepared"
  | "missing"
  | "blocked"
  | "managed";

export interface IntegrationReadinessItem {
  key: string;
  label: string;
  state: IntegrationReadinessState;
  detail: string;
}

function hasConfigValue(integration: IntegrationState, key: string) {
  const value = integration.config?.[key];
  if (typeof value === "string") return value.trim().length > 0;
  return value !== undefined && value !== null;
}

function credentialState(
  integration: IntegrationState,
  credential: string,
): IntegrationReadinessState {
  if (!integration.credentials?.[credential]?.configured) return "missing";
  const unavailable = `${integration.message ?? ""} ${
    integration.connection?.last_error ?? ""
  }`.toLowerCase();
  return integration.status === "degraded" &&
    /(credential|secret|api key|decrypt|unavailable)/.test(unavailable)
    ? "blocked"
    : "ready";
}

function configRequirement(
  integration: IntegrationState,
  key: string,
  label: string,
  readyDetail: string,
  missingDetail: string,
  readyState: IntegrationReadinessState = "ready",
): IntegrationReadinessItem {
  const ready = hasConfigValue(integration, key);
  return {
    key,
    label,
    state: ready ? readyState : "missing",
    detail: ready ? readyDetail : missingDetail,
  };
}

function httpsRedirectRequirement(
  integration: IntegrationState,
  key: string,
  label: string,
): IntegrationReadinessItem {
  const value = String(integration.config?.[key] ?? "").trim();
  if (!value) {
    return {
      key,
      label,
      state: "missing",
      detail: "Add the exact HTTPS callback registered with the provider.",
    };
  }
  if (!/^https:\/\/[^\s]+$/i.test(value)) {
    return {
      key,
      label,
      state: "blocked",
      detail:
        "The callback must be an absolute HTTPS URL and must exactly match the provider configuration.",
    };
  }
  return {
    key,
    label,
    state: "ready",
    detail: `Register this exact callback with the provider: ${value}`,
  };
}

function secretRequirement(
  integration: IntegrationState,
  key: string,
  label: string,
  missingDetail: string,
  readyDetail = "Stored securely and never returned to the browser.",
  readyState: IntegrationReadinessState = "ready",
): IntegrationReadinessItem {
  const state = credentialState(integration, key);
  return {
    key,
    label,
    state: state === "ready" ? readyState : state,
    detail:
      state === "ready"
        ? readyDetail
        : state === "blocked"
          ? integration.message ||
            "The stored credential cannot be used by this server. Replace it only after server-side encryption is available."
          : missingDetail,
  };
}

function metaWebhookCredentialRequirement(
  integration: IntegrationState,
): IntegrationReadinessItem {
  const credential = integration.credentials?.webhook_verify_token;
  if (credential?.configured && credential.source === "platform") {
    return {
      key: "webhook_verify_token",
      label: "Platform webhook verify token",
      state: "managed",
      detail:
        "The shared platform Meta app uses its deployment-managed verify token. It is read-only for this workspace.",
    };
  }
  if (credential?.configured && credential.source === "legacy_account") {
    return {
      key: "webhook_verify_token",
      label: "Workspace webhook verify token",
      state: "managed",
      detail:
        "A legacy per-account token still verifies existing callbacks. Add the write-only workspace token here, then update Meta to the workspace-specific callback URL; ReReply will not rotate or delete the legacy value.",
    };
  }
  return secretRequirement(
    integration,
    "webhook_verify_token",
    "Workspace webhook verify token",
    "Add the write-only verify token used in Meta for Developers.",
    "Stored centrally, encrypted, and never returned to the browser.",
  );
}

function reviewRequirement(
  integration: IntegrationState,
  key: string,
  label: string,
  provider: string,
  approvedState: IntegrationReadinessState = "ready",
): IntegrationReadinessItem {
  const review = String(integration.config?.[key] ?? "not_submitted")
    .trim()
    .toLowerCase();
  if (review === "approved") {
    return {
      key,
      label,
      state: approvedState,
      detail: `${provider} approval is recorded for this workspace.`,
    };
  }
  if (review === "not_submitted" || review === "") {
    return {
      key,
      label,
      state: "missing",
      detail: `Submit the required ${provider} permissions for provider review.`,
    };
  }
  if (review === "rejected") {
    return {
      key,
      label,
      state: "blocked",
      detail: `${provider} rejected the permission request; resolve the review feedback before activation.`,
    };
  }
  return {
    key,
    label,
    state: "blocked",
    detail: `${provider} approval is still pending, so live authorization remains disabled.`,
  };
}

function activationRequirement(
  integration: IntegrationState,
): IntegrationReadinessItem {
  return {
    key: "provider_enabled",
    label: "Provider activation",
    state: integration.enabled ? "ready" : "blocked",
    detail: integration.enabled
      ? "Enabled for this workspace."
      : integration.configured
        ? "Configuration is stored, but an administrator must enable the provider."
        : "Complete the required configuration before enabling the provider.",
  };
}

function connectionRequirement(
  integration: IntegrationState,
  label: string,
  emptyDetail: string,
): IntegrationReadinessItem {
  if (integration.connection.active_count > 0) {
    return {
      key: "active_connection",
      label,
      state: "ready",
      detail: `${integration.connection.active_count} of ${integration.connection.account_count} connections are active.`,
    };
  }
  if (integration.connection.account_count > 0) {
    return {
      key: "active_connection",
      label,
      state: "blocked",
      detail: `${integration.connection.account_count} connections exist, but none are active. Open the Omnichannel Inbox and resolve their health or approval state.`,
    };
  }
  return {
    key: "active_connection",
    label,
    state: "missing",
    detail: emptyDetail,
  };
}

function channelConnectionRequirement(
  integration: IntegrationState,
  channel: IntegrationChannel,
  label: string,
  emptyDetail: string,
): IntegrationReadinessItem {
  const explicitConnections = integration.channel_connections;
  const connection =
    explicitConnections?.[channel] ??
    (channel === "whatsapp" && !explicitConnections
      ? integration.connection
      : { account_count: 0, active_count: 0, pending_count: 0 });
  const key = `active_${channel}_connection`;
  if (connection.last_error) {
    return {
      key,
      label,
      state: "blocked",
      detail: `${label} needs attention. Resolve the reported connection error and run Test again before go-live.`,
    };
  }
  if (
    connection.active_count > 0 &&
    ((connection.pending_count ?? 0) > 0 ||
      connection.active_count < connection.account_count)
  ) {
    return {
      key,
      label,
      state: "blocked",
      detail: `${connection.active_count} of ${connection.account_count} ${channel} connections are active for this workspace; resolve the remaining connection before go-live.`,
    };
  }
  if (connection.active_count > 0) {
    return {
      key,
      label,
      state: "ready",
      detail: `${connection.active_count} of ${connection.account_count} ${channel} connections are active for this workspace.`,
    };
  }
  if (connection.account_count > 0) {
    return {
      key,
      label,
      state: "blocked",
      detail: `${connection.account_count} ${channel} connections exist for this workspace, but none are active. Open the Omnichannel Inbox and resolve their health or authorization state.`,
    };
  }
  return {
    key,
    label,
    state: "missing",
    detail: emptyDetail,
  };
}

function metaChannelConnectionRequirements(
  integration: IntegrationState,
): IntegrationReadinessItem[] {
  const requirements: Array<{
    channel: Extract<IntegrationChannel, "whatsapp" | "instagram" | "messenger">;
    label: string;
    emptyDetail: string;
  }> = [
    {
      channel: "whatsapp",
      label: "WhatsApp connection",
      emptyDetail:
        "Connect this workspace's WhatsApp Business account through WhatsApp Accounts or Embedded Signup.",
    },
    {
      channel: "instagram",
      label: "Instagram connection",
      emptyDetail:
        "Create this workspace's Instagram signed-relay connection, register the same external account ID and secrets in the Meta relay runtime, then run Test.",
    },
    {
      channel: "messenger",
      label: "Messenger connection",
      emptyDetail:
        "Create this workspace's Messenger signed-relay connection, register the same Page ID and secrets in the Meta relay runtime, then run Test.",
    },
  ];
  const intended = integration.intended_channels;
  return requirements
    .filter(({ channel }) => !intended || intended.includes(channel))
    .map(({ channel, label, emptyDetail }) =>
      channelConnectionRequirement(integration, channel, label, emptyDetail),
    );
}

function healthRequirement(
  integration: IntegrationState,
): IntegrationReadinessItem {
  if (integration.connection.last_error) {
    return {
      key: "connection_health",
      label: "Connection health",
      state: "blocked",
      detail: integration.connection.last_error,
    };
  }
  if (integration.connection.last_health_check_at) {
    return {
      key: "connection_health",
      label: "Connection health",
      state: "ready",
      detail: "The most recent relay health check passed.",
    };
  }
  return {
    key: "connection_health",
    label: "Connection health",
    state: "missing",
    detail: "Run Test on each connection in the Omnichannel Inbox.",
  };
}

function adapterRequirement(
  integration: IntegrationState,
  provider: string,
): IntegrationReadinessItem {
  return {
    key: "approved_adapter",
    label: "Approved messaging adapter",
    state: integration.oauth.available ? "ready" : "blocked",
    detail: integration.oauth.available
      ? `${provider} authorization is available.`
      : integration.message ||
        `An approved ${provider} adapter is not installed, so account creation and OAuth are unavailable.`,
  };
}

const builders: Record<
  IntegrationProvider,
  (integration: IntegrationState) => IntegrationReadinessItem[]
> = {
  meta: (integration) => [
    configRequirement(
      integration,
      "app_id",
      "Meta application ID",
      "The shared Meta application is identified.",
      "Add the App ID from Meta for Developers.",
    ),
    configRequirement(
      integration,
      "config_id",
      "Embedded Signup configuration",
      "The Facebook Login for Business configuration is identified.",
      "Add the configuration ID used by Meta Embedded Signup.",
    ),
    secretRequirement(
      integration,
      "app_secret",
      "Server-side app secret",
      "Add the Meta App Secret. It is stored write-only and encrypted.",
    ),
    metaWebhookCredentialRequirement(integration),
    {
      key: "meta_permissions",
      label: "Meta permissions & App Review",
      state: "managed",
      detail:
        "Verify whatsapp_business_management and whatsapp_business_messaging access in Meta before onboarding customers.",
    },
    {
      key: "callback_endpoint",
      label: "Meta webhook callback",
      state: hasConfigValue(integration, "webhook_callback_path")
        ? "ready"
        : "missing",
      detail: hasConfigValue(integration, "webhook_callback_path")
        ? `Use the ${integration.config.management_mode === "workspace" ? "workspace-specific" : "shared platform"} ${String(integration.config.webhook_callback_path)} callback in Meta for Developers.`
        : "The server has not published a Meta webhook callback.",
    },
    activationRequirement(integration),
    ...metaChannelConnectionRequirements(integration),
    {
      key: "account_webhook",
      label: "Per-account webhook subscription",
      state: "managed",
      detail:
        integration.config.management_mode === "workspace"
          ? "Phone number ID and access token stay on each WhatsApp account. Webhook verification is workspace-owned; subscribe and test each WABA under WhatsApp Accounts."
          : "Phone number ID and access token stay on each WhatsApp account. Webhook verification remains deployment-managed for the shared Meta app; subscribe and test each WABA under WhatsApp Accounts.",
    },
    {
      key: "meta_relay_credentials",
      label: "Meta relay registration",
      state: "managed",
      detail:
        "Every Instagram and Messenger account needs its own tenant channel record plus a matching operator-managed relay mapping. Another workspace's tokens, Page IDs and signing secrets cannot satisfy this workspace.",
    },
    {
      key: "business_calling",
      label: "Optional WhatsApp Business Calling",
      state: "managed",
      detail:
        "Calling needs no additional client secret: it reuses the Phone number ID and access token on each WhatsApp account. Enable it per account; manage call behavior under Calling Settings.",
    },
  ],
  threads: (integration) => [
    configRequirement(
      integration,
      "app_id",
      "Threads application ID",
      "A dedicated Threads OAuth application is bound to this workspace and profile.",
      "Add a dedicated Threads App ID from Meta for Developers; App IDs cannot be shared between ReReply workspaces.",
    ),
    httpsRedirectRequirement(integration, "redirect_uri", "OAuth redirect URI"),
    secretRequirement(
      integration,
      "app_secret",
      "Server-side app secret",
      "Add the Threads App Secret for server-side token exchange.",
    ),
    secretRequirement(
      integration,
      "webhook_verify_token",
      "Webhook verify token",
      "Enter the same strong secret of at least 16 characters in the Meta Threads Webhooks callback setup.",
    ),
    reviewRequirement(
      integration,
      "app_review_status",
      "Meta App Review",
      "Meta",
    ),
    {
      key: "oauth_authorization",
      label: "Threads authorization",
      state:
        integration.oauth.supported && integration.oauth.available
          ? "ready"
          : "blocked",
      detail:
        integration.oauth.supported && integration.oauth.available
          ? "OAuth authorization is available for a Threads account."
          : integration.message ||
            "Threads OAuth is unavailable until the server-side integration is configured.",
    },
    activationRequirement(integration),
    connectionRequirement(
      integration,
      "Connected Threads account",
      "Authorize this workspace's Threads account.",
    ),
    healthRequirement(integration),
    {
      key: "public_engagement_policy",
      label: "Public-engagement safety policy",
      state: "managed",
      detail:
        "Replies require an existing public reply or mention target. Threads direct messages and standalone posts remain unavailable.",
    },
  ],
  tiktok: (integration) => [
    configRequirement(
      integration,
      "client_id",
      "TikTok client ID",
      "The future Business Messaging application is identified.",
      "Add the client ID issued to the approved TikTok application.",
      "prepared",
    ),
    configRequirement(
      integration,
      "redirect_uri",
      "OAuth redirect URI",
      "Stored for preparation only; it is not a live ReReply callback.",
      "Record the future redirect URI required by TikTok. ReReply does not expose a live TikTok callback until an approved adapter is installed.",
      "prepared",
    ),
    secretRequirement(
      integration,
      "client_secret",
      "Server-side client secret",
      "Add the TikTok client secret for future server-side token exchange.",
      "Stored write-only for pre-approval preparation; it does not create a live connection.",
      "prepared",
    ),
    reviewRequirement(
      integration,
      "approval_status",
      "Business Messaging approval",
      "TikTok",
      "prepared",
    ),
    adapterRequirement(integration, "TikTok Business Messaging"),
  ],
  qwen: (integration) => [
    configRequirement(
      integration,
      "model",
      "Compatible model",
      "A Qwen-compatible model is selected.",
      "Select the Qwen-compatible model deployed for this workspace.",
    ),
    secretRequirement(
      integration,
      "api_key",
      "Workspace API key",
      "Add a DashScope model API key. Coding Plan and token-plan keys are not compatible with this backend.",
    ),
    configRequirement(
      integration,
      "endpoint_region",
      "Approved endpoint region",
      "An official DashScope region is selected; arbitrary endpoints are not allowed.",
      "Select the platform default, Singapore, United States or China (Beijing) endpoint region.",
    ),
    configRequirement(
      integration,
      "base_url",
      "Computed API endpoint",
      "The backend endpoint and key region must match.",
      "The server has not published a Qwen endpoint; configure it on the deployment.",
    ),
    activationRequirement(integration),
    {
      key: "provider_test",
      label: "Provider health test",
      state:
        integration.status === "degraded"
          ? "blocked"
          : integration.last_tested_at || integration.status === "connected"
            ? "ready"
            : "missing",
      detail:
        integration.status === "degraded"
          ? integration.message || "The most recent provider test failed."
          : integration.last_tested_at || integration.status === "connected"
            ? "The provider has passed a server-side health check."
            : "Save and enable Qwen, then run Test before relying on Copilot.",
    },
    {
      key: "chatbot_policy",
      label: "Chatbot automation policy",
      state: "managed",
      detail:
        "Provider model, endpoint and API key belong here. Chatbot-specific enablement, prompt and output cap remain under Chatbot Settings.",
    },
  ],
  google_search_console: (integration) => {
    const propertyCount = Number(integration.config?.property_count ?? 0);
    const selectedPropertyCount = Number(
      integration.config?.selected_property_count ??
        integration.connection?.active_count ??
        0,
    );
    const isConnected = integration.status === "connected";
    const hasAuthorization = Boolean(
      integration.configured ||
      integration.credentials?.refresh_token?.configured,
    );

    return [
      {
        key: "google_oauth",
        label: "Google authorization",
        state: isConnected
          ? "ready"
          : hasAuthorization
            ? "blocked"
            : integration.oauth.available
              ? "missing"
              : "blocked",
        detail: isConnected
          ? "Read-only Search Console access is authorized for this workspace."
          : hasAuthorization
            ? integration.message ||
              "The stored Google authorization needs attention. Reauthorize the workspace connection."
            : integration.oauth.available
              ? "Connect a Google account with access to at least one verified Search Console property."
              : "Google OAuth is not configured on this deployment. Ask a platform administrator to enable it.",
      },
      {
        key: "verified_properties",
        label: "Verified properties",
        state: !hasAuthorization
          ? "missing"
          : !isConnected
            ? "blocked"
            : selectedPropertyCount > 0
              ? "ready"
              : "missing",
        detail: !hasAuthorization
          ? "Authorize Google before selecting properties."
          : !isConnected
            ? "Restore the Google authorization before using the selected properties."
            : selectedPropertyCount > 0
              ? `${selectedPropertyCount} of ${propertyCount} available properties are selected for analytics.`
              : "Select at least one verified property to start Search Visibility reporting.",
      },
      {
        key: "search_metrics",
        label: "Search performance metrics",
        state: selectedPropertyCount > 0 ? "managed" : "missing",
        detail:
          selectedPropertyCount > 0
            ? "Google clicks, impressions, CTR and average position are available in Search Visibility. Website sessions require a separate Google Analytics integration."
            : "Select a property to enable Google Search performance reporting.",
      },
    ];
  },
  email: (integration) => [
    connectionRequirement(
      integration,
      "Mailbox relay connection",
      "Create an Email signed-relay connection in the Omnichannel Inbox.",
    ),
    {
      key: "relay_signing",
      label: "Inbound & outbound signing",
      state: "managed",
      detail:
        "Relay URL, external mailbox ID and HMAC secrets are configured per connection in the Omnichannel Inbox.",
    },
    healthRequirement(integration),
  ],
  webchat: (integration) => [
    connectionRequirement(
      integration,
      "Webchat relay connection",
      "Create a Webchat signed-relay connection in the Omnichannel Inbox.",
    ),
    {
      key: "relay_signing",
      label: "Inbound & outbound signing",
      state: "managed",
      detail:
        "Relay URL, external site ID and HMAC secrets are configured per connection in the Omnichannel Inbox.",
    },
    healthRequirement(integration),
  ],
};

export function integrationReadiness(
  integration: IntegrationState,
): IntegrationReadinessItem[] {
  return builders[integration.provider](integration);
}
