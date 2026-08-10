import { api } from "@/services/api";
import type { ChannelAccount } from "@/services/productSuite";

export const facebookLoginForBusinessMode =
  "facebook_login_for_business" as const;
export const awaitingMessengerRelayRegistryState =
  "awaiting_relay_registry" as const;
export const messengerReviewRelayReadyState = "review_relay_ready" as const;
export const facebookSDKLoadTimeoutMs = 15_000;
// Backend discovery/selection has a 90-second provider deadline because it can
// traverse several Business portfolios. Keep the browser request alive beyond
// that deadline so it does not report a false failure while the server is
// still safely staging the Page.
export const metaMessengerProviderRequestTimeoutMs = 100_000;

export interface MessengerOnboardingPublicConfig {
  app_id: string;
  config_id: string;
  graph_api_version: string;
  response_type: "code";
  override_default_response_type: true;
}

export interface MessengerOnboardingStart {
  provider: "meta";
  channel: "messenger";
  mode: typeof facebookLoginForBusinessMode;
  nonce: string;
  expires_at: string;
  public_config: MessengerOnboardingPublicConfig;
}

export interface MessengerOnboardingPlatform {
  user_id: string;
  name?: string;
  token_kind?: string;
  client_business_id?: string;
}

export interface MessengerOnboardingWorkspace {
  organization_id: string;
  organization_name: string;
}

export interface MessengerOnboardingBusiness {
  id: string;
  name: string;
}

export interface MessengerOnboardingPage {
  business_id?: string;
  business_name?: string;
  page_id: string;
  page_name: string;
  ownership: "owned" | "client_access" | "unverified";
  selectable: boolean;
  disabled_reason?: string;
  tasks?: string[];
}

export interface MessengerOnboardingInventory {
  session_id: string;
  expires_at: string;
  platform: MessengerOnboardingPlatform;
  workspace: MessengerOnboardingWorkspace;
  businesses: MessengerOnboardingBusiness[];
  pages: MessengerOnboardingPage[];
}

export interface MessengerOnboardingSelection {
  account: ChannelAccount;
  onboarding_state:
    | typeof awaitingMessengerRelayRegistryState
    | typeof messengerReviewRelayReadyState;
  subscription_verified: boolean;
  registry_recognized: false;
}

/**
 * Accepts the normal pending-registry response and the deliberately narrower
 * staging review response. A review response is safe only when every browser-
 * visible fail-closed marker agrees that this is a pending, inbound-only relay
 * account. This must never reinterpret review readiness as production registry
 * recognition or outbound approval.
 */
export function messengerOnboardingSelectionIsSafe(
  selection: MessengerOnboardingSelection,
) {
  const account = selection.account;
  if (
    !account?.id ||
    selection.subscription_verified !== true ||
    selection.registry_recognized !== false
  ) {
    return false;
  }

  if (selection.onboarding_state === awaitingMessengerRelayRegistryState) {
    return true;
  }
  if (selection.onboarding_state !== messengerReviewRelayReadyState) {
    return false;
  }

  return (
    account.channel === "messenger" &&
    account.provider === "relay" &&
    account.status === "pending" &&
    account.config?.onboarding_state === messengerReviewRelayReadyState &&
    account.config?.review_only === true &&
    account.config?.registry_recognized === false &&
    account.config?.outbound_enabled === false &&
    account.config?.ai_reply_enabled === false
  );
}

export interface FacebookLoginForBusinessResponse {
  authResponse?: {
    code?: string;
  };
  status?: string;
  error?: {
    message?: string;
  };
}

export interface FacebookLoginForBusinessSDK {
  init(options: {
    appId: string;
    cookie: boolean;
    xfbml: boolean;
    version: string;
  }): void;
  login(
    callback: (response: FacebookLoginForBusinessResponse) => void,
    options: FacebookLoginForBusinessOptions,
  ): void;
}

export interface FacebookLoginForBusinessOptions {
  config_id: string;
  response_type: "code";
  override_default_response_type: true;
}

export interface PreparedMessengerFacebookLogin {
  app_id: string;
  config_id: string;
  graph_api_version: string;
  login(callback: (response: FacebookLoginForBusinessResponse) => void): void;
}

export function facebookLoginForBusinessOptions(
  config: MessengerOnboardingPublicConfig,
): FacebookLoginForBusinessOptions {
  return {
    config_id: config.config_id,
    response_type: "code",
    override_default_response_type: true,
  };
}

export function messengerPageSelectionKey(page: MessengerOnboardingPage) {
  return `${page.business_id ?? ""}:${page.page_id}`;
}

export function messengerPlatformDisplayName(
  platform: MessengerOnboardingPlatform,
) {
  const providerName = platform.name?.trim();
  if (providerName) return providerName;

  const identityID = platform.user_id.trim();
  const tokenKind = platform.token_kind?.trim().toUpperCase();
  if (tokenKind === "SYSTEM_USER") {
    return identityID
      ? `Business system user ${identityID}`
      : "Business system user";
  }
  return identityID ? `Facebook identity ${identityID}` : "Facebook identity";
}

export function messengerPageSelectable(page: MessengerOnboardingPage) {
  return (
    page.ownership === "owned" &&
    page.selectable === true &&
    Boolean(page.business_id?.trim()) &&
    Boolean(page.page_id.trim())
  );
}

export function messengerPageDisabledReason(page: MessengerOnboardingPage) {
  if (messengerPageSelectable(page)) return "";
  if (page.ownership === "client_access") {
    return (
      page.disabled_reason ||
      "Client-access Pages cannot be connected. Select a Page owned by this business portfolio."
    );
  }
  return (
    page.disabled_reason ||
    "This Page is not eligible for Messenger onboarding."
  );
}

function initializeFacebookSDK(
  sdk: FacebookLoginForBusinessSDK,
  config: MessengerOnboardingPublicConfig,
) {
  // The SDK singleton is also used by WhatsApp Embedded Signup. Initialize it
  // during the short-lived preparation immediately before the login button is
  // enabled, so a previously visited setup page cannot leave stale app/version
  // settings behind. The browser never supplies an app ID.
  sdk.init({
    appId: config.app_id,
    cookie: true,
    xfbml: false,
    version: config.graph_api_version,
  });
  return sdk;
}

function facebookSDKFromWindow() {
  return (window as Window & { FB?: FacebookLoginForBusinessSDK }).FB;
}

async function loadFacebookSDK(
  config: MessengerOnboardingPublicConfig,
): Promise<FacebookLoginForBusinessSDK> {
  const loadedSDK = facebookSDKFromWindow();
  if (loadedSDK) {
    return initializeFacebookSDK(loadedSDK, config);
  }

  const scriptID = "facebook-jssdk";
  const existingScript = document.getElementById(
    scriptID,
  ) as HTMLScriptElement | null;
  const script = existingScript ?? document.createElement("script");

  if (!existingScript) {
    script.id = scriptID;
    script.src = "https://connect.facebook.net/en_US/sdk.js";
    script.async = true;
    script.defer = true;
  }

  return new Promise((resolve, reject) => {
    let settled = false;
    let timeoutID = 0;
    const cleanup = () => {
      window.clearTimeout(timeoutID);
      script.removeEventListener("load", finish);
      script.removeEventListener("error", fail);
    };
    const rejectLoad = () => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(new Error("Facebook Login could not be loaded"));
    };
    const finish = () => {
      if (settled) return;
      const sdk = facebookSDKFromWindow();
      if (!sdk) {
        rejectLoad();
        return;
      }
      try {
        const initializedSDK = initializeFacebookSDK(sdk, config);
        settled = true;
        cleanup();
        resolve(initializedSDK);
      } catch {
        settled = true;
        cleanup();
        reject(new Error("Facebook Login could not be initialized"));
      }
    };
    const fail = () => rejectLoad();

    script.addEventListener("load", finish);
    script.addEventListener("error", fail);
    timeoutID = window.setTimeout(rejectLoad, facebookSDKLoadTimeoutMs);
    if (!existingScript) document.body.appendChild(script);
  });
}

export async function prepareMessengerFacebookLogin(
  config: MessengerOnboardingPublicConfig,
): Promise<PreparedMessengerFacebookLogin> {
  const sdk = await loadFacebookSDK(config);
  const options = facebookLoginForBusinessOptions(config);

  return Object.freeze({
    app_id: config.app_id,
    config_id: config.config_id,
    graph_api_version: config.graph_api_version,
    login(callback: (response: FacebookLoginForBusinessResponse) => void) {
      // This method intentionally has no asynchronous boundary. The caller
      // invokes it directly from the user's Continue click so browsers treat
      // Meta's authorization window as a user-initiated popup. Permissions
      // remain entirely controlled by the backend-issued FLfB configuration;
      // never add a browser-supplied `scope` here.
      sdk.login(callback, options);
    },
  });
}

export const metaMessengerOnboardingService = {
  start: () => api.post("/integrations/meta/messenger/onboarding/start", {}),
  exchange: (data: { code: string; nonce: string }) =>
    api.post("/integrations/meta/messenger/onboarding/exchange", data, {
      timeout: metaMessengerProviderRequestTimeoutMs,
    }),
  select: (data: {
    session_id: string;
    business_id: string;
    page_id: string;
  }) =>
    api.post("/integrations/meta/messenger/onboarding/select", data, {
      timeout: metaMessengerProviderRequestTimeoutMs,
    }),
};
