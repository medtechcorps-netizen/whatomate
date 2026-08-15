// @vitest-environment happy-dom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/services/api";
import {
  MetaMessengerAuthorizationCancelledError,
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
  (window as any).FB = {
    init: vi.fn(),
    login: vi.fn((callback: (value: unknown) => void) => {
      callback({ authResponse: { code: "authorization-code" } });
      afterCallback?.();
    }),
  };
}

describe("metaMessengerOnboarding organization pinning", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.setItem("selected_organization_id", organizationB);
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

  it("times out a lost Facebook popup callback without posting a code", async () => {
    vi.useFakeTimers();
    try {
      (window as any).FB = {
        init: vi.fn(),
        login: vi.fn(),
      };
      vi.mocked(api.post).mockImplementation((url: string) => {
        if (url.endsWith("/start")) return response(start) as any;
        throw new Error("the callback must not be sent after popup timeout");
      });

      const result = metaMessengerOnboarding.begin(organizationA, () => true);
      const rejection = expect(result).rejects.toBeInstanceOf(
        MetaMessengerAuthorizationCancelledError,
      );
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(90_000);
      await rejection;
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
});
