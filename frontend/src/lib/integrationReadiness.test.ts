import { describe, expect, it } from "vitest";

import type { IntegrationState } from "../services/integrations";
import { integrationReadiness } from "./integrationReadiness";

function integrationState(
  provider: IntegrationState["provider"],
  overrides: Partial<IntegrationState> = {},
): IntegrationState {
  return {
    provider,
    display_name: provider,
    status: "not_configured",
    enabled: false,
    configured: false,
    config: {},
    credentials: {},
    connection: {
      account_count: 0,
      active_count: 0,
      pending_count: 0,
    },
    oauth: { supported: false, available: false },
    test_supported: false,
    ...overrides,
  };
}

describe("integrationReadiness", () => {
  it("reports WhatsApp, Instagram and Messenger connections separately", () => {
    const readiness = integrationReadiness(
      integrationState("meta", {
        status: "connected",
        enabled: true,
        configured: true,
        config: {
          app_id: "meta-app",
          config_id: "embedded-signup",
          webhook_callback_path: "/api/webhook",
          management_mode: "platform",
        },
        credentials: {
          app_secret: { configured: true, source: "platform" },
          webhook_verify_token: { configured: true, source: "platform" },
        },
        connection: {
          account_count: 1,
          active_count: 1,
          pending_count: 0,
        },
        channel_connections: {
          whatsapp: { account_count: 1, active_count: 1, pending_count: 0 },
          instagram: { account_count: 0, active_count: 0, pending_count: 0 },
          messenger: { account_count: 1, active_count: 0, pending_count: 1 },
        },
        oauth: { supported: true, available: true, mode: "embedded_signup" },
        test_supported: true,
      }),
    );
    const byKey = Object.fromEntries(readiness.map((item) => [item.key, item]));

    expect(byKey.active_whatsapp_connection?.state).toBe("ready");
    expect(byKey.active_instagram_connection?.state).toBe("missing");
    expect(byKey.active_instagram_connection?.detail).toContain(
      "this workspace's Instagram",
    );
    expect(byKey.active_messenger_connection?.state).toBe("blocked");
    expect(byKey.active_messenger_connection?.detail).toContain(
      "none are active",
    );
  });

  it("never names another organization in the Threads setup guidance", () => {
    const readiness = integrationReadiness(
      integrationState("threads", {
        config: {
          app_id: "threads-app",
          redirect_uri:
            "https://app.example.test/api/integrations/threads/callback",
          app_review_status: "not_submitted",
        },
        oauth: { supported: true, available: false, mode: "oauth" },
      }),
    );
    const connection = readiness.find(
      (item) => item.key === "active_connection",
    );

    expect(connection?.detail).toBe(
      "Authorize this workspace's Threads account.",
    );
    expect(connection?.detail).not.toContain("ReAlign");
  });

  it("blocks a partially healthy channel instead of hiding its error", () => {
    const readiness = integrationReadiness(
      integrationState("meta", {
        channel_connections: {
          whatsapp: { account_count: 1, active_count: 1, pending_count: 0 },
          instagram: {
            account_count: 2,
            active_count: 1,
            pending_count: 1,
            last_error: "Connection needs attention",
          },
          messenger: { account_count: 0, active_count: 0, pending_count: 0 },
        },
      }),
    );
    const instagram = readiness.find(
      (item) => item.key === "active_instagram_connection",
    );

    expect(instagram?.state).toBe("blocked");
    expect(instagram?.detail).toContain("run Test again");
  });

  it("requires only the Meta channels declared for this workspace", () => {
    const readiness = integrationReadiness(
      integrationState("meta", {
        intended_channels: ["whatsapp"],
        channel_connections: {
          whatsapp: { account_count: 1, active_count: 1, pending_count: 0 },
          instagram: { account_count: 0, active_count: 0, pending_count: 0 },
          messenger: { account_count: 0, active_count: 0, pending_count: 0 },
        },
      }),
    );
    const keys = readiness.map((item) => item.key);

    expect(keys).toContain("active_whatsapp_connection");
    expect(keys).not.toContain("active_instagram_connection");
    expect(keys).not.toContain("active_messenger_connection");
  });
});
