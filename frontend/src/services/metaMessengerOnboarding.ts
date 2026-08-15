import { api } from "@/services/api";
import { unwrapItemResponse } from "@/lib/api-utils";
import type { AxiosResponse } from "axios";

export interface MetaMessengerStart {
  provider: "meta";
  channel: "messenger";
  mode: "facebook_login_for_business";
  nonce: string;
  expires_at: string;
  public_config: {
    app_id: string;
    config_id: string;
    graph_api_version: string;
    response_type: "code";
    override_default_response_type: true;
  };
}

export interface MetaMessengerPage {
  business_id?: string;
  business_name?: string;
  page_id: string;
  page_name: string;
  ownership: string;
  selectable: boolean;
  disabled_reason?: string;
  tasks?: string[];
}

export interface MetaMessengerSelection {
  session_id: string;
  expires_at: string;
  workspace: { organization_id: string; organization_name: string };
  pages: MetaMessengerPage[];
  verified_scopes: string[];
}

export interface MetaMessengerStatus {
  organization_id: string;
  enabled: boolean;
}

export class MetaMessengerOrganizationChangedError extends Error {
  constructor() {
    super("The active workspace changed during Messenger onboarding");
    this.name = "MetaMessengerOrganizationChangedError";
  }
}

export class MetaMessengerAuthorizationCancelledError extends Error {
  constructor(message = "Messenger authorization was cancelled") {
    super(message);
    this.name = "MetaMessengerAuthorizationCancelledError";
  }
}

export class MetaMessengerAuthorizationFailedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MetaMessengerAuthorizationFailedError";
  }
}

type FacebookLoginResponse = {
  authResponse?: { code?: unknown } | null;
  status?: unknown;
  error?: unknown;
  error_type?: unknown;
  error_code?: unknown;
  error_reason?: unknown;
  error_description?: unknown;
  error_message?: unknown;
  message?: unknown;
};

type FacebookWindow = {
  FB?: {
    init(options: Record<string, unknown>): void;
    login(
      callback: (response: FacebookLoginResponse | null | undefined) => void,
      options: Record<string, unknown>,
    ): void;
  };
  fbAsyncInit?: () => void;
};

const facebookWindow = window as unknown as FacebookWindow;
let sdkPromise: Promise<NonNullable<FacebookWindow["FB"]>> | null = null;
const providerRequestTimeout = 120_000;
const facebookAuthorizationTimeout = 90_000;
const facebookSDKLoadTimeout = 15_000;
const facebookSDKElementID = "facebook-jssdk";

const safeProviderDetailPattern = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$/;

function safeProviderDetail(value: unknown) {
  if (typeof value !== "string" && typeof value !== "number") return "";
  const candidate = String(value).trim();
  return safeProviderDetailPattern.test(candidate) ? candidate : "";
}

function providerErrorRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function hasProviderErrorMarker(response: FacebookLoginResponse) {
  return [
    "error",
    "error_type",
    "error_code",
    "error_reason",
    "error_description",
    "error_message",
    "message",
  ].some((key) => Object.prototype.hasOwnProperty.call(response, key));
}

function isFacebookAuthorizationCancellation(response: FacebookLoginResponse) {
  return (
    response.authResponse === null &&
    safeProviderDetail(response.status).toLowerCase() === "unknown" &&
    !hasProviderErrorMarker(response)
  );
}

function facebookAuthorizationFailure(response: FacebookLoginResponse) {
  const providerError = providerErrorRecord(response.error);
  const status = safeProviderDetail(response.status);
  const errorType =
    safeProviderDetail(response.error_type) ||
    safeProviderDetail(providerError?.type) ||
    safeProviderDetail(response.error_reason) ||
    (typeof response.error === "string"
      ? safeProviderDetail(response.error)
      : "");
  const errorCode =
    safeProviderDetail(response.error_code) ||
    safeProviderDetail(providerError?.code);
  const details = [
    status ? `status: ${status}` : "",
    errorType ? `error type: ${errorType}` : "",
    errorCode ? `error code: ${errorCode}` : "",
  ].filter(Boolean);
  const suffix = details.length > 0 ? ` (${details.join("; ")})` : "";
  return new MetaMessengerAuthorizationFailedError(
    "Facebook completed without returning the required authorization code. " +
      "Retry the connection; if it happens again, ask an administrator to verify the Login for Business configuration." +
      suffix,
  );
}

function loadFacebookSDK(appId: string, version: string) {
  if (facebookWindow.FB) {
    facebookWindow.FB.init({ appId, cookie: true, xfbml: false, version });
    return Promise.resolve(facebookWindow.FB);
  }
  if (sdkPromise) return sdkPromise;
  const pending = new Promise<NonNullable<FacebookWindow["FB"]>>(
    (resolve, reject) => {
      const previousInit = facebookWindow.fbAsyncInit;
      let settled = false;
      let timeout = 0;
      const restoreInit = () => {
        if (facebookWindow.fbAsyncInit === initHandler) {
          facebookWindow.fbAsyncInit = previousInit;
        }
      };
      const finish = (action: () => void) => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timeout);
        restoreInit();
        action();
      };
      const removeStaleSDKElement = () => {
        if (!facebookWindow.FB) {
          // A previously loaded or failed SDK element will never fire
          // fbAsyncInit again. Remove only this reserved element so the next
          // attempt can perform a fresh load instead of repeating the same
          // timeout forever.
          document.getElementById(facebookSDKElementID)?.remove();
        }
      };
      const initHandler = () => {
        previousInit?.();
        const fb = facebookWindow.FB;
        if (!fb) {
          removeStaleSDKElement();
          finish(() => reject(new Error("Facebook login is unavailable")));
          return;
        }
        fb.init({ appId, cookie: true, xfbml: false, version });
        finish(() => resolve(fb));
      };
      facebookWindow.fbAsyncInit = initHandler;
      timeout = window.setTimeout(() => {
        removeStaleSDKElement();
        finish(() => reject(new Error("Facebook login took too long to load")));
      }, facebookSDKLoadTimeout);
      const existing = document.getElementById(facebookSDKElementID);
      if (existing) return;
      const script = document.createElement("script");
      script.id = facebookSDKElementID;
      script.async = true;
      script.defer = true;
      script.crossOrigin = "anonymous";
      script.src = "https://connect.facebook.net/en_US/sdk.js";
      script.onerror = () => {
        removeStaleSDKElement();
        finish(() => reject(new Error("Facebook login could not be loaded")));
      };
      document.head.appendChild(script);
    },
  );
  sdkPromise = pending.catch((error) => {
    sdkPromise = null;
    throw error;
  });
  return sdkPromise;
}

async function unwrap<T>(request: Promise<AxiosResponse>): Promise<T> {
  const response = await request;
  return unwrapItemResponse<T>(response);
}

function organizationHeaders(organizationId: string) {
  const value = organizationId.trim();
  if (!value) throw new MetaMessengerOrganizationChangedError();
  return { "X-Organization-ID": value };
}

function requireCurrentOrganization(
  organizationId: string,
  isCurrent?: () => boolean,
  signal?: AbortSignal,
) {
  if (signal?.aborted) throw new MetaMessengerAuthorizationCancelledError();
  if (!organizationId.trim() || (isCurrent && !isCurrent())) {
    throw new MetaMessengerOrganizationChangedError();
  }
}

function providerRequestConfig(organizationId: string, signal?: AbortSignal) {
  const config = {
    headers: organizationHeaders(organizationId),
    timeout: providerRequestTimeout,
  };
  return signal ? { ...config, signal } : config;
}

function waitForFacebookAuthorization(
  fb: NonNullable<FacebookWindow["FB"]>,
  start: MetaMessengerStart,
  signal?: AbortSignal,
) {
  return new Promise<string>((resolve, reject) => {
    let settled = false;
    const finish = (action: () => void) => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timeout);
      signal?.removeEventListener("abort", onAbort);
      action();
    };
    const onAbort = () =>
      finish(() => reject(new MetaMessengerAuthorizationCancelledError()));
    const timeout = window.setTimeout(
      () =>
        finish(() =>
          reject(
            new MetaMessengerAuthorizationFailedError(
              "Facebook authorization timed out before a result was returned. Retry and keep the Facebook window open until it finishes.",
            ),
          ),
        ),
      facebookAuthorizationTimeout,
    );
    if (signal?.aborted) {
      onAbort();
      return;
    }
    signal?.addEventListener("abort", onAbort, { once: true });
    fb.login(
      (response) => {
        if (settled || signal?.aborted) return;
        const safeResponse =
          response && typeof response === "object" ? response : {};
        const value =
          typeof safeResponse.authResponse?.code === "string"
            ? safeResponse.authResponse.code.trim()
            : "";
        if (value) finish(() => resolve(value));
        else if (isFacebookAuthorizationCancellation(safeResponse)) {
          finish(() =>
            reject(
              new MetaMessengerAuthorizationCancelledError(
                "Facebook authorization was cancelled",
              ),
            ),
          );
        } else {
          finish(() => reject(facebookAuthorizationFailure(safeResponse)));
        }
      },
      {
        config_id: start.public_config.config_id,
        response_type: start.public_config.response_type,
        override_default_response_type:
          start.public_config.override_default_response_type,
      },
    );
  });
}

async function authorize(
  start: MetaMessengerStart,
  organizationId: string,
  isCurrent: () => boolean,
  signal?: AbortSignal,
): Promise<MetaMessengerSelection> {
  requireCurrentOrganization(organizationId, isCurrent, signal);
  const fb = await loadFacebookSDK(
    start.public_config.app_id,
    start.public_config.graph_api_version,
  );
  requireCurrentOrganization(organizationId, isCurrent, signal);
  const code = await waitForFacebookAuthorization(fb, start, signal);
  requireCurrentOrganization(organizationId, isCurrent, signal);
  const selection = await unwrap<MetaMessengerSelection>(
    api.post(
      "/integrations/meta/messenger/onboarding/callback",
      {
        code,
        nonce: start.nonce,
      },
      providerRequestConfig(organizationId, signal),
    ),
  );
  requireCurrentOrganization(organizationId, isCurrent, signal);
  if (selection.workspace.organization_id !== organizationId) {
    throw new MetaMessengerOrganizationChangedError();
  }
  return selection;
}

export const metaMessengerOnboarding = {
  async status(organizationId: string): Promise<boolean> {
    const result = await unwrap<MetaMessengerStatus>(
      api.get("/integrations/meta/messenger/onboarding/status", {
        headers: organizationHeaders(organizationId),
      }),
    );
    if (result.organization_id !== organizationId) {
      throw new MetaMessengerOrganizationChangedError();
    }
    return result.enabled === true;
  },
  async begin(
    organizationId: string,
    isCurrent: () => boolean,
    signal?: AbortSignal,
  ): Promise<MetaMessengerSelection> {
    requireCurrentOrganization(organizationId, isCurrent, signal);
    const start = await unwrap<MetaMessengerStart>(
      api.post(
        "/integrations/meta/messenger/onboarding/start",
        {},
        providerRequestConfig(organizationId, signal),
      ),
    );
    return authorize(start, organizationId, isCurrent, signal);
  },
  async reconnect(
    accountId: string,
    organizationId: string,
    isCurrent: () => boolean,
    signal?: AbortSignal,
  ): Promise<MetaMessengerSelection> {
    requireCurrentOrganization(organizationId, isCurrent, signal);
    const start = await unwrap<MetaMessengerStart>(
      api.post(
        `/channel-accounts/${encodeURIComponent(accountId)}/meta-messenger/reconnect`,
        {},
        providerRequestConfig(organizationId, signal),
      ),
    );
    return authorize(start, organizationId, isCurrent, signal);
  },
  select(
    organizationId: string,
    sessionId: string,
    businessId: string,
    pageId: string,
  ) {
    return api.post(
      "/integrations/meta/messenger/onboarding/select",
      {
        session_id: sessionId,
        business_id: businessId,
        page_id: pageId,
      },
      providerRequestConfig(organizationId),
    );
  },
  approve(accountId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-messenger/approve`,
      {
        approve: true,
      },
      providerRequestConfig(organizationId),
    );
  },
  reconcile(accountId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-messenger/reconcile`,
      {},
      providerRequestConfig(organizationId),
    );
  },
  disconnect(accountId: string, pageId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-messenger/disconnect`,
      {
        confirm_page_id: pageId,
      },
      providerRequestConfig(organizationId),
    );
  },
};
