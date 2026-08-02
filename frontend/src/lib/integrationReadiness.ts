import type {
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
        "Verify business_management, whatsapp_business_management and whatsapp_business_messaging access in Meta before onboarding customers.",
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
    connectionRequirement(
      integration,
      "Active Meta channel connection",
      "Enable Meta, then connect WhatsApp with Embedded Signup or configure an approved Meta relay channel.",
    ),
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
      label: "Instagram & Messenger relay credentials",
      state: "managed",
      detail:
        "Instagram and Messenger do not reuse the central WhatsApp app secret. Their operator-managed app secret, verify token and signed relay belong to each channel connection in the Omnichannel Inbox.",
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
      "The future OAuth application is identified.",
      "Add the Threads App ID from Meta for Developers.",
      "prepared",
    ),
    configRequirement(
      integration,
      "redirect_uri",
      "OAuth redirect URI",
      "Stored for preparation only; it is not a live ReReply callback.",
      "Record the future redirect URI required by Meta. ReReply does not expose a live Threads callback until an approved adapter is installed.",
      "prepared",
    ),
    secretRequirement(
      integration,
      "app_secret",
      "Server-side app secret",
      "Add the Threads App Secret for future server-side token exchange.",
      "Stored write-only for pre-approval preparation; it does not create a live connection.",
      "prepared",
    ),
    reviewRequirement(
      integration,
      "app_review_status",
      "Meta App Review",
      "Meta",
      "prepared",
    ),
    adapterRequirement(integration, "Threads public engagement"),
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
