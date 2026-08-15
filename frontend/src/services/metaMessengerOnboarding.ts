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

type FacebookWindow = {
  FB?: {
    init(options: Record<string, unknown>): void;
    login(
      callback: (response: {
        authResponse?: { code?: string };
        status?: string;
      }) => void,
      options: Record<string, unknown>,
    ): void;
  };
  fbAsyncInit?: () => void;
};

const facebookWindow = window as unknown as FacebookWindow;
let sdkPromise: Promise<NonNullable<FacebookWindow["FB"]>> | null = null;
const providerRequestTimeout = 120_000;
const facebookAuthorizationTimeout = 90_000;

function loadFacebookSDK(appId: string, version: string) {
  if (facebookWindow.FB) {
    facebookWindow.FB.init({ appId, cookie: true, xfbml: false, version });
    return Promise.resolve(facebookWindow.FB);
  }
  if (sdkPromise) return sdkPromise;
  const pending = new Promise<NonNullable<FacebookWindow["FB"]>>(
    (resolve, reject) => {
      const timeout = window.setTimeout(
        () => reject(new Error("Facebook login took too long to load")),
        15_000,
      );
      const previousInit = facebookWindow.fbAsyncInit;
      facebookWindow.fbAsyncInit = () => {
        previousInit?.();
        const fb = facebookWindow.FB;
        if (!fb) {
          window.clearTimeout(timeout);
          reject(new Error("Facebook login is unavailable"));
          return;
        }
        fb.init({ appId, cookie: true, xfbml: false, version });
        window.clearTimeout(timeout);
        resolve(fb);
      };
      const existing = document.getElementById("facebook-jssdk");
      if (existing) return;
      const script = document.createElement("script");
      script.id = "facebook-jssdk";
      script.async = true;
      script.defer = true;
      script.crossOrigin = "anonymous";
      script.src = "https://connect.facebook.net/en_US/sdk.js";
      script.onerror = () => {
        window.clearTimeout(timeout);
        reject(new Error("Facebook login could not be loaded"));
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
            new MetaMessengerAuthorizationCancelledError(
              "Facebook authorization timed out or the popup was closed",
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
        const value = response.authResponse?.code?.trim();
        if (value) finish(() => resolve(value));
        else
          finish(() =>
            reject(
              new MetaMessengerAuthorizationCancelledError(
                "Facebook authorization was cancelled or did not return a code",
              ),
            ),
          );
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
