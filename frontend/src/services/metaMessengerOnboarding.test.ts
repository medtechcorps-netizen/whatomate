// @vitest-environment happy-dom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/services/api";
import {
  MetaMessengerAuthorizationCancelledError,
  MetaMessengerAuthorizationFailedError,
  MetaMessengerOrganizationChangedError,
  metaMessengerOnboarding,
  type MetaMessengerSelection,
  type MetaMessengerStart,
} from "@/services/metaMessengerOnboarding";

vi.mock("@/services/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

const organizationA = "11111111-1111-4111-8111-111111111111";
const organizationB = "22222222-2222-4222-8222-222222222222";

const start: MetaMessengerStart = {
  provider: "meta",
  channel: "messenger",
  mode: "facebook_login_for_business",
  nonce: "one-time-nonce",
  expires_at: "2026-08-15T08:00:00Z",
  public_config: {
    app_id: "100000000000001",
    config_id: "200000000000001",
    graph_api_version: "v25.0",
    response_type: "code",
    override_default_response_type: true,
  },
};

function selection(organizationId = organizationA): MetaMessengerSelection {
  return {
    session_id: "selection-session",
    expires_at: "2026-08-15T08:00:00Z",
    workspace: {
      organization_id: organizationId,
      organization_name: "Clinic",
    },
    pages: [],
    verified_scopes: [],
  };
}

function response<T>(data: T) {
  return Promise.resolve({ data: { data } });
}

function installFacebookLogin(afterCallback?: () => void) {
  installFacebookLoginResponse(
    { authResponse: { code: "authorization-code" }, status: "connected" },
    afterCallback,
  );
}

function installFacebookLoginResponse(
  loginResponse: unknown,
  afterCallback?: () => void,
) {
  (window as any).FB = {
    init: vi.fn(),
    login: vi.fn((callback: (value: unknown) => void) => {
      callback(loginResponse);
      afterCallback?.();
    }),
  };
}

describe("metaMessengerOnboarding organization pinning", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.setItem("selected_organization_id", organizationB);
    document.getElementById("facebook-jssdk")?.remove();
    installFacebookLogin();
  });

  it("never falls back to the default organization when the explicit pin is missing", async () => {
    await expect(metaMessengerOnboarding.status("")).rejects.toBeInstanceOf(
      MetaMessengerOrganizationChangedError,
    );
    expect(api.get).not.toHaveBeenCalled();
  });

  it("pins status, start, and callback to the launch organization", async () => {
    vi.mocked(api.get).mockReturnValue(
      response({
        organization_id: organizationA,
        enabled: true,
      }) as any,
    );
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      if (url.endsWith("/callback")) return response(selection()) as any;
      throw new Error(`unexpected request ${url}`);
    });

    await expect(metaMessengerOnboarding.status(organizationA)).resolves.toBe(
      true,
    );
    await expect(
      metaMessengerOnboarding.begin(organizationA, () => true),
    ).resolves.toEqual(selection());

    expect(api.get).toHaveBeenCalledWith(
      "/integrations/meta/messenger/onboarding/status",
      { headers: { "X-Organization-ID": organizationA } },
    );
    for (const call of vi.mocked(api.post).mock.calls) {
      expect(call[2]).toEqual({
        headers: { "X-Organization-ID": organizationA },
        timeout: 120_000,
      });
    }
    expect(vi.mocked(api.post).mock.calls[1]?.[1]).toEqual({
      code: "authorization-code",
      nonce: start.nonce,
    });
  });

  it("treats Meta's explicit unknown/null response as authorization cancellation", async () => {
    installFacebookLoginResponse({ authResponse: null, status: "unknown" });
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      throw new Error("the callback must not be sent after cancellation");
    });

    await expect(
      metaMessengerOnboarding.begin(organizationA, () => true),
    ).rejects.toBeInstanceOf(MetaMessengerAuthorizationCancelledError);
    expect(api.post).toHaveBeenCalledTimes(1);
  });

  it("surfaces a sanitized provider failure when Meta completes without a code", async () => {
    installFacebookLoginResponse({
      authResponse: {},
      status: "connected",
      error_type: "OAuthException",
      error_code: 200,
      error_message: "private provider detail <token-like-value>",
    });
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      throw new Error("the callback must not be sent without a code");
    });

    const failure = await metaMessengerOnboarding
      .begin(organizationA, () => true)
      .catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(MetaMessengerAuthorizationFailedError);
    expect((failure as Error).message).toContain(
      "Facebook completed without returning the required authorization code",
    );
    expect((failure as Error).message).toContain("status: connected");
    expect((failure as Error).message).toContain("error type: OAuthException");
    expect((failure as Error).message).toContain("error code: 200");
    expect((failure as Error).message).not.toContain("private provider detail");
    expect((failure as Error).message).not.toContain("token-like-value");
    expect(api.post).toHaveBeenCalledTimes(1);
  });

  it("detaches after a workspace switch before sending the authorization code", async () => {
    let current = true;
    installFacebookLogin(() => {
      current = false;
    });
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      throw new Error("the callback must not be sent after a workspace switch");
    });

    await expect(
      metaMessengerOnboarding.begin(organizationA, () => current),
    ).rejects.toBeInstanceOf(MetaMessengerOrganizationChangedError);
    expect(api.post).toHaveBeenCalledTimes(1);
  });

  it("rejects a stale callback response from another organization", async () => {
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      if (url.endsWith("/callback"))
        return response(selection(organizationB)) as any;
      throw new Error(`unexpected request ${url}`);
    });

    await expect(
      metaMessengerOnboarding.begin(organizationA, () => true),
    ).rejects.toBeInstanceOf(MetaMessengerOrganizationChangedError);
    expect(vi.mocked(api.post).mock.calls[1]?.[2]).toEqual({
      headers: { "X-Organization-ID": organizationA },
      timeout: 120_000,
    });
  });

  it("pins selection, approval, reconnect, and disconnect mutations", async () => {
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.includes("/reconnect")) return response(start) as any;
      if (url.endsWith("/callback")) return response(selection()) as any;
      return response({}) as any;
    });

    await metaMessengerOnboarding.select(
      organizationA,
      "session",
      "300000000000001",
      "400000000000001",
    );
    await metaMessengerOnboarding.approve("account-id", organizationA);
    await metaMessengerOnboarding.reconcile("account-id", organizationA);
    await metaMessengerOnboarding.disconnect(
      "account-id",
      "400000000000001",
      organizationA,
    );
    await metaMessengerOnboarding.reconnect(
      "account-id",
      organizationA,
      () => true,
    );

    for (const call of vi.mocked(api.post).mock.calls) {
      expect(call[2]).toEqual({
        headers: { "X-Organization-ID": organizationA },
        timeout: 120_000,
      });
    }
  });

  it("keeps provider-bearing requests alive beyond the global 30 second timeout", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(api.post).mockImplementation((url: string) => {
        if (url.endsWith("/start")) return response(start) as any;
        if (url.endsWith("/callback")) {
          return new Promise((resolve) => {
            window.setTimeout(
              () => resolve({ data: { data: selection() } }),
              31_000,
            );
          }) as any;
        }
        throw new Error(`unexpected request ${url}`);
      });

      const result = metaMessengerOnboarding.begin(organizationA, () => true);
      await vi.advanceTimersByTimeAsync(31_000);
      await expect(result).resolves.toEqual(selection());
      expect(vi.mocked(api.post).mock.calls[1]?.[2]).toEqual({
        headers: { "X-Organization-ID": organizationA },
        timeout: 120_000,
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("accepts a Facebook callback after 90 seconds without a stale timeout rejection", async () => {
    vi.useFakeTimers();
    try {
      let facebookCallback: ((value: unknown) => void) | undefined;
      (window as any).FB = {
        init: vi.fn(),
        login: vi.fn((callback: (value: unknown) => void) => {
          facebookCallback = callback;
        }),
      };
      vi.mocked(api.post).mockImplementation((url: string) => {
        if (url.endsWith("/start")) return response(start) as any;
        if (url.endsWith("/callback")) return response(selection()) as any;
        throw new Error(`unexpected request ${url}`);
      });

      const result = metaMessengerOnboarding.begin(
        organizationA,
        () => true,
      );
      await vi.advanceTimersByTimeAsync(0);
      expect(facebookCallback).toBeTypeOf("function");

      await vi.advanceTimersByTimeAsync(120_000);
      facebookCallback?.({
        authResponse: { code: "authorization-code-after-finish" },
        status: "connected",
      });
      await expect(result).resolves.toEqual(selection());

      // Cross the original eight-minute deadline. A successful callback must
      // have cleared that timer instead of producing a stale rejection.
      await vi.advanceTimersByTimeAsync(360_001);
      expect(api.post).toHaveBeenCalledTimes(2);
      expect(vi.mocked(api.post).mock.calls[1]?.[1]).toEqual({
        code: "authorization-code-after-finish",
        nonce: start.nonce,
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("times out a lost Facebook popup callback before the server nonce expires", async () => {
    vi.useFakeTimers();
    try {
      let facebookCallback: ((value: unknown) => void) | undefined;
      (window as any).FB = {
        init: vi.fn(),
        login: vi.fn((callback: (value: unknown) => void) => {
          facebookCallback = callback;
        }),
      };
      vi.mocked(api.post).mockImplementation((url: string) => {
        if (url.endsWith("/start")) return response(start) as any;
        throw new Error("the callback must not be sent after popup timeout");
      });

      const result = metaMessengerOnboarding.begin(organizationA, () => true);
      const rejection = expect(result).rejects.toBeInstanceOf(
        MetaMessengerAuthorizationFailedError,
      );
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(8 * 60_000);
      await rejection;
      facebookCallback?.({ authResponse: { code: "stale-code" } });
      await Promise.resolve();
      expect(api.post).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("cancels authorization and ignores a late Facebook callback", async () => {
    let facebookCallback: ((value: unknown) => void) | undefined;
    (window as any).FB = {
      init: vi.fn(),
      login: vi.fn((callback: (value: unknown) => void) => {
        facebookCallback = callback;
      }),
    };
    vi.mocked(api.post).mockImplementation((url: string) => {
      if (url.endsWith("/start")) return response(start) as any;
      throw new Error("the callback must not be sent after cancellation");
    });
    const controller = new AbortController();

    const result = metaMessengerOnboarding.begin(
      organizationA,
      () => true,
      controller.signal,
    );
    await vi.waitFor(() => expect(facebookCallback).toBeTypeOf("function"));
    controller.abort();
    await expect(result).rejects.toBeInstanceOf(
      MetaMessengerAuthorizationCancelledError,
    );
    facebookCallback?.({ authResponse: { code: "late-code" } });
    await Promise.resolve();
    expect(api.post).toHaveBeenCalledTimes(1);
  });

  it("removes a stale SDK element after timeout so a retry can load cleanly", async () => {
    vi.useFakeTimers();
    let appendSpy: ReturnType<typeof vi.spyOn> | undefined;
    try {
      delete (window as any).FB;
      const staleScript = document.createElement("script");
      staleScript.id = "facebook-jssdk";
      document.head.appendChild(staleScript);
      vi.mocked(api.post).mockImplementation((url: string) => {
        if (url.endsWith("/start")) return response(start) as any;
        if (url.endsWith("/callback")) return response(selection()) as any;
        throw new Error(`unexpected request ${url}`);
      });

      const firstAttempt = metaMessengerOnboarding.begin(
        organizationA,
        () => true,
      );
      const firstRejection = expect(firstAttempt).rejects.toThrow(
        "Facebook login took too long to load",
      );
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(15_000);
      await firstRejection;
      expect(document.getElementById("facebook-jssdk")).toBeNull();

      const appendedSDK: { current: HTMLScriptElement | null } = {
        current: null,
      };
      appendSpy = vi
        .spyOn(document.head, "appendChild")
        .mockImplementation((node: Node) => {
          appendedSDK.current = node as HTMLScriptElement;
          return node;
        });
      const retry = metaMessengerOnboarding.begin(organizationA, () => true);
      await Promise.resolve();
      await Promise.resolve();
      expect(appendedSDK.current?.id).toBe("facebook-jssdk");
      installFacebookLogin();
      (window as any).fbAsyncInit?.();
      await expect(retry).resolves.toEqual(selection());
    } finally {
      appendSpy?.mockRestore();
      document.getElementById("facebook-jssdk")?.remove();
      vi.useRealTimers();
    }
  });
});
