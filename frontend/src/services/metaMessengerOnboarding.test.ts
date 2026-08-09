// @vitest-environment happy-dom

import { afterEach, describe, expect, it, vi } from "vitest";
import {
  facebookSDKLoadTimeoutMs,
  facebookLoginForBusinessOptions,
  metaMessengerProviderRequestTimeoutMs,
  messengerPageDisabledReason,
  messengerPageSelectable,
  messengerPageSelectionKey,
  messengerPlatformDisplayName,
  prepareMessengerFacebookLogin,
  type FacebookLoginForBusinessSDK,
  type MessengerOnboardingPage,
  type MessengerOnboardingPublicConfig,
} from "@/services/metaMessengerOnboarding";

afterEach(() => {
  vi.useRealTimers();
  document.getElementById("facebook-jssdk")?.remove();
  delete (window as Window & { FB?: unknown }).FB;
});

const publicConfig: MessengerOnboardingPublicConfig = {
  app_id: "123456789",
  config_id: "messenger-login-config",
  graph_api_version: "v24.0",
  response_type: "code",
  override_default_response_type: true,
};

function page(
  overrides: Partial<MessengerOnboardingPage> = {},
): MessengerOnboardingPage {
  return {
    business_id: "business-1",
    business_name: "Medtech Healthcare",
    page_id: "page-1",
    page_name: "Klinik Insan Ampang",
    ownership: "owned",
    selectable: true,
    tasks: ["MESSAGING"],
    ...overrides,
  };
}

describe("Messenger Facebook Login for Business", () => {
  it("keeps provider requests alive beyond the backend's 90-second deadline", () => {
    expect(metaMessengerProviderRequestTimeoutMs).toBeGreaterThan(90_000);
  });

  it("uses the configuration ID code flow without a client-supplied scope", () => {
    const options = facebookLoginForBusinessOptions(publicConfig);

    expect(options).toEqual({
      config_id: "messenger-login-config",
      response_type: "code",
      override_default_response_type: true,
    });
    expect(options).not.toHaveProperty("scope");
  });

  it("opens Facebook synchronously after preparation without another await", async () => {
    let userClickStackActive = true;
    const callback = vi.fn();
    const login = vi.fn(
      (receivedCallback: (response: { status: string }) => void) => {
        expect(userClickStackActive).toBe(true);
        receivedCallback({ status: "connected" });
      },
    );
    const init = vi.fn();
    (window as Window & { FB?: FacebookLoginForBusinessSDK }).FB = {
      init,
      login,
    };

    const prepared = await prepareMessengerFacebookLogin(publicConfig);
    prepared.login(callback);
    userClickStackActive = false;

    expect(init).toHaveBeenCalledTimes(1);
    expect(login).toHaveBeenCalledWith(
      callback,
      facebookLoginForBusinessOptions(publicConfig),
    );
    expect(callback).toHaveBeenCalledWith({ status: "connected" });
  });

  it("allows only an owned Page that the server marked selectable", () => {
    expect(messengerPageSelectable(page())).toBe(true);
    expect(
      messengerPageSelectable(
        page({ ownership: "client_access", selectable: true }),
      ),
    ).toBe(false);
    expect(
      messengerPageSelectable(
        page({ ownership: "unverified", selectable: true }),
      ),
    ).toBe(false);
    expect(messengerPageSelectable(page({ selectable: false }))).toBe(false);
  });

  it("keeps Page choices unique per business and explains client-only blocks", () => {
    expect(messengerPageSelectionKey(page())).toBe("business-1:page-1");
    expect(
      messengerPageDisabledReason(
        page({
          ownership: "client_access",
          selectable: true,
          disabled_reason: "Client-only asset",
        }),
      ),
    ).toBe("Client-only asset");
    expect(
      messengerPageDisabledReason(
        page({ ownership: "client_access", selectable: true }),
      ),
    ).toContain("Client-access Pages cannot be connected");
  });

  it("labels a nameless Business system user without inventing a person", () => {
    expect(
      messengerPlatformDisplayName({
        user_id: "900000000000001",
        name: "",
        token_kind: "SYSTEM_USER",
        client_business_id: "200000000000001",
      }),
    ).toBe("Business system user 900000000000001");
    expect(
      messengerPlatformDisplayName({
        user_id: "facebook-user-10",
        name: "Arham Zakaria",
        token_kind: "USER",
      }),
    ).toBe("Arham Zakaria");
  });

  it("fails within a bounded time when an already-loaded SDK tag has no FB global", async () => {
    vi.useFakeTimers();
    const existingScript = document.createElement("script");
    existingScript.id = "facebook-jssdk";
    document.body.appendChild(existingScript);

    const pendingPreparation = prepareMessengerFacebookLogin(publicConfig);
    const rejection = expect(pendingPreparation).rejects.toThrow(
      "Facebook Login could not be loaded",
    );
    await vi.advanceTimersByTimeAsync(facebookSDKLoadTimeoutMs);

    await rejection;
  });

  it("rejects promptly when Facebook SDK initialization fails", async () => {
    const existingScript = document.createElement("script");
    existingScript.id = "facebook-jssdk";
    document.body.appendChild(existingScript);

    const pendingPreparation = prepareMessengerFacebookLogin(publicConfig);
    (window as Window & { FB?: FacebookLoginForBusinessSDK }).FB = {
      init: () => {
        throw new Error("invalid app configuration");
      },
      login: vi.fn(),
    };
    existingScript.dispatchEvent(new Event("load"));

    await expect(pendingPreparation).rejects.toThrow(
      "Facebook Login could not be initialized",
    );
  });
});
