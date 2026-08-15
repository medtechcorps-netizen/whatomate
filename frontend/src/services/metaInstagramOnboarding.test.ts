// @vitest-environment happy-dom

import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "@/services/api";
import {
  type MetaInstagramAvailabilityState,
  MetaInstagramOffPilotError,
  MetaInstagramOrganizationChangedError,
  metaInstagramOAuthAvailable,
  metaInstagramOnboarding,
  metaInstagramReconciliationAvailable,
  metaInstagramStaticCreationAvailable,
  metaInstagramTeardownAvailable,
  type MetaInstagramStart,
} from "@/services/metaInstagramOnboarding";

vi.mock("@/services/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

const organizationA = "11111111-1111-4111-8111-111111111111";
const organizationB = "22222222-2222-4222-8222-222222222222";
const start: MetaInstagramStart = {
  provider: "meta",
  channel: "instagram",
  mode: "instagram_login",
  authorization_url:
    "https://www.instagram.example.test/oauth/authorize?state=synthetic-state",
  expires_at: "2026-08-15T08:00:00Z",
};

function response<T>(data: T) {
  return Promise.resolve({ data: { data } });
}

describe("managed Instagram onboarding client", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("never falls back when the explicit organization pin is missing", async () => {
    await expect(metaInstagramOnboarding.status("")).rejects.toBeInstanceOf(
      MetaInstagramOrganizationChangedError,
    );
    await expect(metaInstagramOnboarding.begin(" ")).rejects.toBeInstanceOf(
      MetaInstagramOrganizationChangedError,
    );
    expect(api.get).not.toHaveBeenCalled();
    expect(api.post).not.toHaveBeenCalled();
  });

  it("pins status and rejects a response for a different organization", async () => {
    vi.mocked(api.get).mockReturnValue(
      response({
        organization_id: organizationB,
        configured: true,
        enabled: true,
        quarantine_only: false,
        app_review_status: "approved",
      }) as any,
    );
    await expect(
      metaInstagramOnboarding.status(organizationA),
    ).rejects.toBeInstanceOf(MetaInstagramOrganizationChangedError);
    expect(api.get).toHaveBeenCalledWith(
      "/integrations/meta/instagram/onboarding/status",
      {
        headers: { "X-Organization-ID": organizationA },
      },
    );
  });

  it("keeps only teardown available while the configured deployment is quarantined", () => {
    const status = {
      organization_id: organizationA,
      configured: true,
      enabled: false,
      quarantine_only: true,
      app_review_status: "rejected" as const,
    };
    expect(metaInstagramOAuthAvailable(status)).toBe(false);
    expect(metaInstagramTeardownAvailable(status)).toBe(true);
    expect(metaInstagramReconciliationAvailable(status, "subscribed")).toBe(
      false,
    );
    expect(metaInstagramReconciliationAvailable(status, "unsubscribed")).toBe(
      true,
    );
    expect(
      metaInstagramTeardownAvailable({ ...status, configured: false }),
    ).toBe(false);
  });

  it.each([
    ["loading", "loading", null, false],
    ["failed", "error", null, false],
    [
      "quarantine-only",
      "managed",
      {
        organization_id: organizationA,
        configured: true,
        enabled: false,
        quarantine_only: true,
        app_review_status: "approved",
      },
      false,
    ],
    ["off-pilot", "off_pilot", null, true],
  ] as const)(
    "allows static Instagram creation only for an exact %s state",
    (_label, availability, status, expected) => {
      const state: MetaInstagramAvailabilityState = {
        availability,
        organizationId: organizationA,
        status,
      };
      expect(metaInstagramStaticCreationAvailable(state, organizationA)).toBe(
        expected,
      );
      expect(metaInstagramStaticCreationAvailable(state, organizationB)).toBe(
        false,
      );
    },
  );

  it("recognizes only the exact off-pilot status response", async () => {
    vi.mocked(api.get).mockRejectedValueOnce({
      isAxiosError: true,
      response: {
        status: 404,
        data: {
          message:
            "Managed Instagram onboarding is not available for this workspace",
        },
      },
    });
    await expect(
      metaInstagramOnboarding.status(organizationA),
    ).rejects.toBeInstanceOf(MetaInstagramOffPilotError);

    const unavailable = {
      isAxiosError: true,
      response: { status: 404, data: { message: "Route not found" } },
    };
    vi.mocked(api.get).mockRejectedValueOnce(unavailable);
    await expect(metaInstagramOnboarding.status(organizationA)).rejects.toBe(
      unavailable,
    );
  });

  it("accepts only an exact HTTPS Instagram Login authorization binding", async () => {
    vi.mocked(api.post).mockReturnValue(response(start) as any);
    await expect(metaInstagramOnboarding.begin(organizationA)).resolves.toEqual(
      start,
    );
    expect(api.post).toHaveBeenCalledWith(
      "/integrations/meta/instagram/onboarding/start",
      {},
      {
        headers: { "X-Organization-ID": organizationA },
        timeout: 120_000,
      },
    );

    vi.mocked(api.post).mockReturnValue(
      response({ ...start, mode: "facebook_login" }) as any,
    );
    await expect(metaInstagramOnboarding.begin(organizationA)).rejects.toThrow(
      "invalid provider binding",
    );

    vi.mocked(api.post).mockReturnValue(
      response({
        ...start,
        authorization_url: "http://provider.example.test/oauth",
      }) as any,
    );
    await expect(metaInstagramOnboarding.begin(organizationA)).rejects.toThrow(
      "authorization URL is invalid",
    );
  });

  it("pins every approval, repair, reconnect, and disconnect mutation", async () => {
    vi.mocked(api.post).mockImplementation((requestURL: string) => {
      if (requestURL.endsWith("/reconnect")) return response(start) as any;
      return response({}) as any;
    });

    await metaInstagramOnboarding.approve("synthetic-account", organizationA);
    await metaInstagramOnboarding.reconcile("synthetic-account", organizationA);
    await metaInstagramOnboarding.disconnect(
      "synthetic-account",
      "700000000000101",
      organizationA,
    );
    await metaInstagramOnboarding.reconnect("synthetic-account", organizationA);

    for (const call of vi.mocked(api.post).mock.calls) {
      expect(call[2]).toEqual({
        headers: { "X-Organization-ID": organizationA },
        timeout: 120_000,
      });
    }
    expect(vi.mocked(api.post).mock.calls[0]?.[1]).toEqual({ approve: true });
    expect(vi.mocked(api.post).mock.calls[2]?.[1]).toEqual({
      confirm_account_id: "700000000000101",
    });
  });
});
