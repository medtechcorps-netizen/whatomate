import { expect, test, type Page } from "@playwright/test";

const organizationId = "81111111-1111-4111-8111-111111111111";
const selectedOrganizationId = "81222222-2222-4222-8222-222222222222";

const permission = (resource: string, action = "read") => ({
  id: `${resource}-${action}`,
  resource,
  action,
});

const adminUser = {
  id: "82222222-2222-4222-8222-222222222222",
  email: "integration-admin@example.test",
  full_name: "Integration Admin",
  organization_id: organizationId,
  organization_name: "Klinik Relive",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "83333333-3333-4333-8333-333333333333",
    name: "admin",
    description: "Workspace administrator",
    is_system: true,
    permissions: [
      permission("settings.integrations"),
      permission("settings.integrations", "write"),
      permission("settings.general"),
      permission("settings.chatbot"),
    ],
  },
};

const deniedUser = {
  ...adminUser,
  id: "84444444-4444-4444-8444-444444444444",
  email: "settings-reader@example.test",
  full_name: "Settings Reader",
  role: {
    ...adminUser.role,
    id: "85555555-5555-4555-8555-555555555555",
    name: "settings-reader",
    is_system: false,
    permissions: [permission("settings.general")],
  },
};

const viewOnlyUser = {
  ...adminUser,
  id: "86666666-6666-4666-8666-666666666666",
  email: "integration-reader@example.test",
  full_name: "Integration Reader",
  role: {
    ...adminUser.role,
    id: "87777777-7777-4777-8777-777777777777",
    name: "integration-reader",
    is_system: false,
    permissions: [permission("settings.integrations")],
  },
};

const superAdminUser = {
  ...adminUser,
  id: "89999999-9999-4999-8999-999999999999",
  email: "platform-owner@example.test",
  full_name: "Platform Owner",
  is_super_admin: true,
};

const connection = (accountCount = 0, activeCount = 0) => ({
  account_count: accountCount,
  active_count: activeCount,
  pending_count: Math.max(0, accountCount - activeCount),
});

interface MockIntegration {
  provider: string;
  display_name: string;
  status: string;
  enabled: boolean;
  configured: boolean;
  read_only: boolean;
  config: Record<string, unknown>;
  credentials: Record<
    string,
    {
      configured: boolean;
      updated_at?: string;
      source?: string;
    }
  >;
  connection: ReturnType<typeof connection>;
  channel_connections?: Record<string, ReturnType<typeof connection>>;
  intended_channels?: string[];
  oauth: { supported: boolean; available: boolean; mode?: string };
  test_supported: boolean;
  message?: string;
  required_scopes?: string[];
}

function integrationFixture(): MockIntegration[] {
  return [
    {
      provider: "meta",
      display_name: "Meta / WhatsApp",
      status: "configured",
      enabled: true,
      configured: true,
      read_only: false,
      config: {
        app_id: "123456789012345",
        config_id: "987654321098765",
        api_version: "v21.0",
        management_mode: "workspace",
        webhook_callback_path: `/api/webhook?workspace=${organizationId}`,
      },
      credentials: {
        app_secret: {
          configured: true,
          updated_at: "2026-07-29T08:30:00Z",
          source: "workspace",
        },
        webhook_verify_token: {
          configured: true,
          updated_at: "2026-07-29T08:30:00Z",
          source: "workspace",
        },
      },
      connection: connection(2, 2),
      channel_connections: {
        whatsapp: connection(1, 1),
        instagram: connection(1, 1),
        messenger: connection(),
      },
      intended_channels: ["whatsapp", "instagram", "messenger"],
      oauth: { supported: true, available: true, mode: "embedded_signup" },
      test_supported: true,
    },
    {
      provider: "threads",
      display_name: "Threads",
      status: "configured",
      enabled: true,
      configured: true,
      read_only: false,
      config: {
        app_id: "234567890123456",
        redirect_uri:
          "https://app.example.test/api/integrations/threads/callback",
        app_review_status: "approved",
      },
      credentials: {
        app_secret: { configured: true, source: "workspace" },
        webhook_verify_token: { configured: true, source: "workspace" },
      },
      connection: connection(),
      oauth: { supported: true, available: true, mode: "oauth" },
      test_supported: false,
      message:
        "Public replies and mentions only. Threads direct messages and standalone posts are not supported.",
      required_scopes: [
        "threads_basic",
        "threads_read_replies",
        "threads_manage_replies",
        "threads_content_publish",
        "threads_manage_mentions",
      ],
    },
    {
      provider: "tiktok",
      display_name: "TikTok Business",
      status: "approval_required",
      enabled: false,
      configured: false,
      read_only: false,
      config: {
        client_id: "",
        redirect_uri:
          "https://app.example.test/api/integrations/tiktok/callback",
        approval_status: "pending",
      },
      credentials: {
        client_secret: { configured: false, source: "workspace" },
      },
      connection: connection(),
      oauth: { supported: true, available: false, mode: "oauth" },
      test_supported: false,
      message:
        "TikTok Business Messaging approval is required before activation.",
      required_scopes: [
        "message.list.read",
        "message.list.send",
        "message.list.manage",
      ],
    },
    {
      provider: "qwen",
      display_name: "Qwen Copilot",
      status: "connected",
      enabled: true,
      configured: true,
      read_only: false,
      config: {
        model: "qwen-plus",
        max_tokens: 700,
        temperature: 0.3,
        endpoint_region: "singapore",
        base_url: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
      },
      credentials: {
        api_key: { configured: true, source: "copilot" },
      },
      connection: connection(),
      oauth: { supported: false, available: false },
      test_supported: true,
    },
    {
      provider: "google_search_console",
      display_name: "Google Search Console",
      status: "connected",
      enabled: true,
      configured: true,
      read_only: false,
      config: {
        platform_configured: true,
        operations_available: true,
        property_count: 2,
        selected_property_count: 1,
      },
      credentials: {
        refresh_token: {
          configured: true,
          updated_at: "2026-07-30T03:00:00Z",
          source: "workspace",
        },
      },
      connection: connection(2, 1),
      oauth: { supported: true, available: true, mode: "oauth" },
      test_supported: true,
      message:
        "One verified website property is available in Search Visibility.",
      required_scopes: ["https://www.googleapis.com/auth/webmasters.readonly"],
    },
    {
      provider: "email",
      display_name: "Email",
      status: "connected",
      enabled: true,
      configured: true,
      read_only: true,
      config: {},
      credentials: {},
      connection: connection(2, 2),
      oauth: { supported: false, available: false },
      test_supported: false,
      message:
        "Connections for this channel are managed in the Omnichannel Inbox.",
    },
    {
      provider: "webchat",
      display_name: "Webchat",
      status: "configured",
      enabled: true,
      configured: true,
      read_only: true,
      config: {},
      credentials: {},
      connection: connection(1, 0),
      oauth: { supported: false, available: false },
      test_supported: false,
      message:
        "Connections for this channel are managed in the Omnichannel Inbox.",
    },
  ];
}

type TestUser = typeof adminUser;

async function installIntegrationMocks(
  page: Page,
  user: TestUser = adminUser,
  options: {
    platformMeta?: boolean;
    threadsEntitlementEnabled?: boolean;
    entitlementUnavailable?: boolean;
    failEntitlementRefreshAfterGrant?: boolean;
    failEntitlementRefreshAfterRevoke?: boolean;
    entitlementRemainsEnabledAfterRevoke?: boolean;
    supportOverrideActive?: boolean;
    supportStatusUnavailable?: boolean;
    supportStatusOrganizationId?: string;
    failSupportStatusRefreshAfterGrant?: boolean;
    failSupportStatusRefreshAfterRevoke?: boolean;
    revokeConflict?: boolean;
    selectedOrganizationId?: string;
  } = {},
) {
  const integrations = integrationFixture();
  let threadsEntitlementEnabled = options.threadsEntitlementEnabled ?? true;
  let threadsSupportOverrideActive = options.supportOverrideActive ?? false;
  let threadsSupportOverrideId = "8aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
  const entitlementTargetOrganizationId =
    options.selectedOrganizationId ?? organizationId;
  const googleProperties = [
    {
      id: "88888888-8888-4888-8888-888888888881",
      site_url: "sc-domain:klinikrelive.com",
      display_name: "klinikrelive.com",
      property_type: "domain",
      permission_level: "siteOwner",
      selected: true,
      last_synced_at: "2026-07-30T03:00:00Z",
    },
    {
      id: "88888888-8888-4888-8888-888888888882",
      site_url: "https://www.klinikrelive.com/services/",
      display_name: "https://www.klinikrelive.com/services/",
      property_type: "url_prefix",
      permission_level: "siteFullUser",
      selected: false,
      last_synced_at: "2026-07-30T03:00:00Z",
    },
  ];
  if (options.platformMeta) {
    const meta = integrations.find((item) => item.provider === "meta")!;
    meta.config.management_mode = "platform";
    meta.config.webhook_callback_path = "/api/webhook";
    meta.credentials.app_secret.source = "platform";
    meta.credentials.webhook_verify_token.source = "platform";
  }
  const traffic = {
    listReads: 0,
    entitlementReads: 0,
    threadsEntitlementRequests: [] as Array<{
      method: string;
      url: string;
      body: Record<string, unknown>;
      organizationHeader?: string;
    }>,
    threadsEntitlementRevocationRequests: [] as Array<{
      method: string;
      url: string;
      body: Record<string, unknown>;
      organizationHeader?: string;
    }>,
    threadsSupportStatusRequests: [] as Array<{
      method: string;
      url: string;
      organizationHeader?: string;
    }>,
    updates: [] as Array<Record<string, unknown>>,
    threadsUpdates: [] as Array<Record<string, unknown>>,
    threadsEnabledAfterUpdates: [] as boolean[],
    threadsConnects: 0,
    qwenUpdates: [] as Array<Record<string, unknown>>,
    credentialClears: [] as string[],
    googlePropertyUpdates: [] as string[][],
    googleDisconnects: 0,
    googleDisconnectMethods: [] as string[],
  };

  await page.addInitScript(
    ({ storedUser, activeOrganizationId }) => {
      window.localStorage.setItem("user", JSON.stringify(storedUser));
      window.localStorage.setItem("locale", "en");
      if (activeOrganizationId) {
        window.localStorage.setItem(
          "selected_organization_id",
          activeOrganizationId,
        );
      }
    },
    {
      storedUser: user,
      activeOrganizationId: options.selectedOrganizationId,
    },
  );

  // Register the catch-all first. Playwright gives the later, specific routes
  // priority, keeping unrelated layout requests deterministic.
  await page.route("**/api/**", (route) =>
    route.fulfill({ json: { data: {} } }),
  );
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: user } }),
  );
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, async (route) => {
    traffic.entitlementReads += 1;
    if (
      options.entitlementUnavailable ||
      (options.failEntitlementRefreshAfterGrant &&
        traffic.threadsEntitlementRequests.length > 0) ||
      (options.failEntitlementRefreshAfterRevoke &&
        traffic.threadsEntitlementRevocationRequests.length > 0)
    ) {
      await route.fulfill({
        status: 503,
        json: { error: "Entitlements temporarily unavailable" },
      });
      return;
    }
    await route.fulfill({
      json: {
        data: {
          mode: "licensed",
          plan_code: "rereply-growth",
          entitlements: {
            "omnichannel.enabled": true,
            "threads.public_engagement.enabled": threadsEntitlementEnabled,
          },
        },
      },
    });
  });
  await page.route(
    new RegExp(
      `/api/admin/organizations/${entitlementTargetOrganizationId}/entitlements/threads-public-engagement/support-status(?:\\?.*)?$`,
    ),
    async (route) => {
      const request = route.request();
      traffic.threadsSupportStatusRequests.push({
        method: request.method(),
        url: request.url(),
        organizationHeader: request.headers()["x-organization-id"],
      });
      if (
        options.supportStatusUnavailable ||
        (options.failSupportStatusRefreshAfterGrant &&
          traffic.threadsEntitlementRequests.length > 0) ||
        (options.failSupportStatusRefreshAfterRevoke &&
          traffic.threadsEntitlementRevocationRequests.length > 0)
      ) {
        await route.fulfill({
          status: 503,
          json: { error: "Support override status temporarily unavailable" },
        });
        return;
      }
      await route.fulfill({
        json: {
          data: {
            organization_id:
              options.supportStatusOrganizationId ??
              entitlementTargetOrganizationId,
            entitlement_key: "threads.public_engagement.enabled",
            active: threadsSupportOverrideActive,
            ...(threadsSupportOverrideActive
              ? {
                  override_id: threadsSupportOverrideId,
                  source: "support",
                  starts_at: "2026-08-03T09:00:00Z",
                }
              : {}),
          },
        },
      });
    },
  );
  await page.route(
    new RegExp(
      `/api/admin/organizations/${entitlementTargetOrganizationId}/entitlements/threads-public-engagement/enable(?:\\?.*)?$`,
    ),
    async (route) => {
      const request = route.request();
      const body = request.postDataJSON() as Record<string, unknown>;
      traffic.threadsEntitlementRequests.push({
        method: request.method(),
        url: request.url(),
        body,
        organizationHeader: request.headers()["x-organization-id"],
      });
      threadsEntitlementEnabled = true;
      threadsSupportOverrideActive = true;
      await route.fulfill({
        json: {
          data: {
            organization_id: entitlementTargetOrganizationId,
            entitlement_key: "threads.public_engagement.enabled",
            override_id: threadsSupportOverrideId,
            source: "support",
            starts_at: "2026-08-03T09:00:00Z",
            created: true,
            effective_enabled: true,
            plan_code: "rereply-growth",
            subscription_status: "trialing",
          },
        },
      });
    },
  );
  await page.route(
    new RegExp(
      `/api/admin/organizations/${entitlementTargetOrganizationId}/entitlements/threads-public-engagement/revoke-support(?:\\?.*)?$`,
    ),
    async (route) => {
      const request = route.request();
      const body = request.postDataJSON() as Record<string, unknown>;
      traffic.threadsEntitlementRevocationRequests.push({
        method: request.method(),
        url: request.url(),
        body,
        organizationHeader: request.headers()["x-organization-id"],
      });
      if (
        options.revokeConflict &&
        traffic.threadsEntitlementRevocationRequests.length === 1
      ) {
        threadsSupportOverrideId = "8bbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
        await route.fulfill({
          status: 409,
          json: { error: "The active support grant changed" },
        });
        return;
      }
      threadsSupportOverrideActive = false;
      threadsEntitlementEnabled =
        options.entitlementRemainsEnabledAfterRevoke ?? false;
      await route.fulfill({
        json: {
          data: {
            organization_id: entitlementTargetOrganizationId,
            entitlement_key: "threads.public_engagement.enabled",
            override_id: threadsSupportOverrideId,
            source: "support",
            revoked_at: "2026-08-03T10:00:00Z",
            revoked: true,
            effective_enabled: threadsEntitlementEnabled,
          },
        },
      });
    },
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/org\/settings(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          name: "Klinik Relive",
          settings: { timezone: "Asia/Kuala_Lumpur" },
        },
      },
    }),
  );
  await page.route(
    /\/api\/integrations\/meta\/credentials(?:\?.*)?$/,
    async (route) => {
      traffic.credentialClears.push("meta");
      const meta = integrations.find((item) => item.provider === "meta")!;
      meta.enabled = false;
      meta.configured = false;
      meta.status = "disabled";
      meta.credentials.app_secret = { configured: false, source: "workspace" };
      meta.credentials.webhook_verify_token = {
        configured: false,
        source: "workspace",
      };
      await route.fulfill({ json: { data: meta } });
    },
  );
  await page.route(/\/api\/integrations\/meta(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "PUT") {
      await route.fallback();
      return;
    }
    const body = route.request().postDataJSON() as Record<string, unknown>;
    traffic.updates.push(body);
    const meta = integrations.find((item) => item.provider === "meta")!;
    if (typeof body.enabled === "boolean") meta.enabled = body.enabled;
    if (body.config && typeof body.config === "object") {
      meta.config = {
        ...meta.config,
        ...(body.config as Record<string, unknown>),
      };
    }
    if (body.credentials && typeof body.credentials === "object") {
      const credentials = body.credentials as Record<string, unknown>;
      if (typeof credentials.app_secret === "string") {
        meta.credentials.app_secret = {
          configured: true,
          source: "workspace",
        };
      }
      if (typeof credentials.webhook_verify_token === "string") {
        meta.credentials.webhook_verify_token = {
          configured: true,
          source: "workspace",
        };
      }
    }
    await route.fulfill({ json: { data: meta } });
  });
  await page.route(
    /\/api\/integrations\/threads\/connect(?:\?.*)?$/,
    async (route) => {
      traffic.threadsConnects += 1;
      await route.fulfill({
        json: {
          data: {
            provider: "threads",
            ready: true,
            mode: "oauth",
            authorization_url: "/mock-threads-oauth?state=server-issued",
          },
        },
      });
    },
  );
  await page.route(/\/api\/integrations\/threads(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "PUT") {
      await route.fallback();
      return;
    }
    const body = route.request().postDataJSON() as Record<string, unknown>;
    traffic.threadsUpdates.push(body);
    const threads = integrations.find((item) => item.provider === "threads")!;
    if (typeof body.enabled === "boolean") threads.enabled = body.enabled;
    if (body.config && typeof body.config === "object") {
      threads.config = {
        ...threads.config,
        ...(body.config as Record<string, unknown>),
      };
    }
    traffic.threadsEnabledAfterUpdates.push(threads.enabled);
    await route.fulfill({ json: { data: threads } });
  });
  await page.route("**/mock-threads-oauth**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<html><body>Threads authorization</body></html>",
    }),
  );
  await page.route(/\/api\/integrations\/qwen(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "PUT") {
      await route.fallback();
      return;
    }
    const body = route.request().postDataJSON() as Record<string, unknown>;
    traffic.qwenUpdates.push(body);
    const qwen = integrations.find((item) => item.provider === "qwen")!;
    if (typeof body.enabled === "boolean") qwen.enabled = body.enabled;
    if (body.config && typeof body.config === "object") {
      qwen.config = {
        ...qwen.config,
        ...(body.config as Record<string, unknown>),
      };
    }
    await route.fulfill({ json: { data: qwen } });
  });
  await page.route(
    /\/api\/integrations\/google_search_console\/properties(?:\?.*)?$/,
    async (route) => {
      if (route.request().method() === "PUT") {
        const body = route.request().postDataJSON() as {
          property_ids?: string[];
        };
        const selectedIDs = body.property_ids ?? [];
        traffic.googlePropertyUpdates.push(selectedIDs);
        for (const property of googleProperties) {
          property.selected = selectedIDs.includes(property.id);
        }
        const google = integrations.find(
          (item) => item.provider === "google_search_console",
        )!;
        google.connection.active_count = selectedIDs.length;
        google.config.selected_property_count = selectedIDs.length;
      }
      await route.fulfill({
        json: {
          data: {
            properties: googleProperties,
            selected_count: googleProperties.filter(
              (property) => property.selected,
            ).length,
          },
        },
      });
    },
  );
  await page.route(
    /\/api\/integrations\/google_search_console\/properties\/refresh(?:\?.*)?$/,
    (route) =>
      route.fulfill({
        json: {
          data: {
            properties: googleProperties,
            selected_count: googleProperties.filter(
              (property) => property.selected,
            ).length,
          },
        },
      }),
  );
  await page.route(
    /\/api\/integrations\/google_search_console\/connection(?:\?.*)?$/,
    async (route) => {
      traffic.googleDisconnectMethods.push(route.request().method());
      if (route.request().method() !== "DELETE") {
        await route.fulfill({
          status: 405,
          json: { error: "Method not allowed" },
        });
        return;
      }
      traffic.googleDisconnects += 1;
      const google = integrations.find(
        (item) => item.provider === "google_search_console",
      )!;
      google.status = "not_configured";
      google.enabled = false;
      google.configured = false;
      google.credentials.refresh_token = { configured: false };
      google.connection = connection();
      await route.fulfill({ json: { data: google } });
    },
  );
  await page.route(
    /\/api\/integrations\/google_search_console\/test(?:\?.*)?$/,
    (route) =>
      route.fulfill({
        json: {
          data: {
            provider: "google_search_console",
            success: true,
            status: "connected",
            message: "Connection test succeeded",
          },
        },
      }),
  );
  await page.route(/\/api\/integrations(?:\?.*)?$/, (route) => {
    traffic.listReads += 1;
    return route.fulfill({ json: { data: { integrations } } });
  });

  return traffic;
}

test("admin sees every provider and saving a blank secret preserves it", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");

  await expect(page.getByTestId("integrations-page")).toBeVisible();
  for (const provider of [
    "meta",
    "threads",
    "tiktok",
    "qwen",
    "google_search_console",
    "email",
    "webchat",
  ]) {
    await expect(
      page.getByTestId(`integration-card-${provider}`),
    ).toBeVisible();
  }

  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "Configure" })
    .click();

  const secretInput = page.locator("#meta-app_secret");
  const webhookTokenInput = page.locator("#meta-webhook_verify_token");
  await expect(secretInput).toHaveAttribute("type", "password");
  await expect(secretInput).toHaveValue("");
  await expect(secretInput).toHaveAttribute("placeholder", /Stored securely/);
  await expect(webhookTokenInput).toHaveAttribute("type", "password");
  await expect(webhookTokenInput).toHaveValue("");
  await expect(webhookTokenInput).toHaveAttribute(
    "placeholder",
    /Stored securely/,
  );
  await expect(page.locator("#meta-webhook_callback_path")).toHaveValue(
    new RegExp(`/api/webhook\\?workspace=${organizationId}$`),
  );
  await expect(page.getByTestId("meta-webhook-callback-copy")).toBeVisible();
  await expect(
    page.getByText("Write-only credentials", { exact: true }),
  ).toBeVisible();

  await page.getByTestId("integration-save-meta").click();
  await expect.poll(() => traffic.updates.length).toBe(1);

  expect(traffic.updates[0]).toEqual({
    enabled: true,
    config: {
      app_id: "123456789012345",
      config_id: "987654321098765",
    },
  });
  expect(traffic.updates[0]).not.toHaveProperty("credentials");
  await expect(secretInput).toHaveValue("");
  await expect(webhookTokenInput).toHaveValue("");
});

test("Google Search Console selects verified properties and disconnects through the safe endpoint", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");

  await expect(
    page.getByTestId("integration-card-google_search_console"),
  ).toBeVisible();

  const providerOrder = await page
    .locator('[data-testid^="integration-card-"]')
    .evaluateAll((cards) =>
      cards.map((card) => card.getAttribute("data-testid")),
    );
  expect(providerOrder.indexOf("integration-card-google_search_console")).toBe(
    providerOrder.indexOf("integration-card-qwen") + 1,
  );

  await page
    .getByTestId("integration-card-google_search_console")
    .getByRole("button", { name: "Configure" })
    .click();
  const dialog = page.getByTestId("integration-dialog-google_search_console");
  await expect(dialog).toBeVisible();
  await expect(
    dialog.getByText("klinikrelive.com", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByText("Domain property", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByText(/does not report website visits or sessions/i),
  ).toBeVisible();
  await expect(
    dialog.getByRole("link", { name: /Open dashboard/ }),
  ).toHaveCount(0);

  const propertyCheckboxes = dialog.getByRole("checkbox");
  await expect(propertyCheckboxes).toHaveCount(2);
  await propertyCheckboxes.nth(1).click();
  await dialog.getByTestId("gsc-save-properties").click();
  await expect.poll(() => traffic.googlePropertyUpdates.length).toBe(1);
  expect(traffic.googlePropertyUpdates[0]).toEqual([
    "88888888-8888-4888-8888-888888888881",
    "88888888-8888-4888-8888-888888888882",
  ]);

  await dialog.getByRole("button", { name: "Disconnect", exact: true }).click();
  await page
    .getByRole("button", { name: "Disconnect Google", exact: true })
    .click();
  await expect.poll(() => traffic.googleDisconnects).toBe(1);
  expect(traffic.googleDisconnectMethods).toEqual(["DELETE"]);
  await expect(dialog).toHaveCount(0);
});

test("Google OAuth callback opens property setup and removes the one-time URL marker", async ({
  page,
}) => {
  await installIntegrationMocks(page);
  await page.goto(
    "/settings/integrations?google_search_console=connected&keep=1",
  );

  await expect(
    page.getByTestId("integration-dialog-google_search_console"),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/settings\/integrations\?keep=1$/);
  await expect(
    page.getByText("Google Search Console connected", { exact: true }),
  ).toBeVisible();
});

test("workspace webhook token is submitted write-only and cleared from the form after save", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "Configure" })
    .click();

  const webhookTokenInput = page.locator("#meta-webhook_verify_token");
  await webhookTokenInput.fill("synthetic-workspace-webhook-verify-token");
  await page.getByTestId("integration-save-meta").click();

  await expect.poll(() => traffic.updates.length).toBe(1);
  expect(traffic.updates[0]).toMatchObject({
    credentials: {
      webhook_verify_token: "synthetic-workspace-webhook-verify-token",
    },
  });
  await expect(webhookTokenInput).toHaveValue("");
  await expect(page.getByTestId("integration-dialog-meta")).not.toContainText(
    "synthetic-workspace-webhook-verify-token",
  );
});

test("shared platform Meta webhook token is read-only and uses the shared callback", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, adminUser, {
    platformMeta: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "Configure" })
    .click();

  await expect(page.locator("#meta-webhook_verify_token")).toBeDisabled();
  await expect(page.locator("#meta-webhook_verify_token")).toHaveAttribute(
    "placeholder",
    "Managed by the shared platform Meta app",
  );
  await expect(page.locator("#meta-webhook_callback_path")).toHaveValue(
    /\/api\/webhook$/,
  );
  await expect(
    page.getByRole("button", { name: "Remove credentials", exact: true }),
  ).toHaveCount(0);

  await page.getByTestId("integration-save-meta").click();
  await expect.poll(() => traffic.updates.length).toBe(1);
  expect(traffic.updates[0]).toEqual({ enabled: true, config: {} });
  expect(traffic.updates[0]).not.toHaveProperty("credentials");
});

test("credential removal is a separate confirmed destructive action", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "Configure" })
    .click();

  await page
    .getByRole("button", { name: "Remove credentials", exact: true })
    .click();
  expect(traffic.credentialClears).toHaveLength(0);

  const confirmation = page.getByRole("alertdialog");
  await expect(
    confirmation.getByText("Remove stored credentials?", { exact: true }),
  ).toBeVisible();
  await confirmation
    .getByRole("button", { name: "Remove credentials", exact: true })
    .click();

  await expect.poll(() => traffic.credentialClears).toEqual(["meta"]);
  await expect(page.locator("#meta-app_secret")).toHaveValue("");
});

test("Qwen region selection is wired to the allowlisted backend config without resending secrets or endpoint URLs", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-qwen")
    .getByRole("button", { name: "Configure" })
    .click();

  await page.locator("#qwen-endpoint_region").click();
  await page.getByRole("option", { name: "United States" }).click();
  await expect(page.locator("#qwen-api_key")).toHaveValue("");
  await page.getByTestId("integration-save-qwen").click();

  await expect.poll(() => traffic.qwenUpdates.length).toBe(1);
  expect(traffic.qwenUpdates[0]).toEqual({
    enabled: true,
    config: {
      model: "qwen-plus",
      max_tokens: 700,
      temperature: 0.3,
      endpoint_region: "us",
    },
  });
  expect(traffic.qwenUpdates[0]).not.toHaveProperty("credentials");
  expect(
    (traffic.qwenUpdates[0].config as Record<string, unknown>).base_url,
  ).toBeUndefined();
});

test("Threads credential save preserves a live enabled integration during an entitlement outage", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, adminUser, {
    entitlementUnavailable: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const enabledSwitch = page.locator("#integration-enabled");
  await expect(enabledSwitch).toBeDisabled();
  await expect(enabledSwitch).toHaveAttribute("data-state", "checked");
  await page
    .locator("#threads-webhook_verify_token")
    .fill("synthetic-threads-outage-webhook-token");
  await page.getByTestId("integration-save-threads").click();

  await expect.poll(() => traffic.threadsUpdates.length).toBe(1);
  expect(traffic.threadsUpdates[0]).not.toHaveProperty("enabled");
  expect(traffic.threadsUpdates[0]).toMatchObject({
    credentials: {
      webhook_verify_token: "synthetic-threads-outage-webhook-token",
    },
  });
  expect(traffic.threadsEnabledAfterUpdates).toEqual([true]);
  await expect(enabledSwitch).toHaveAttribute("data-state", "checked");
});

test("Threads entitlement support action is hidden from workspace admins while activation stays locked", async ({
  page,
}) => {
  await installIntegrationMocks(page, adminUser, {
    threadsEntitlementEnabled: false,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const dialog = page.getByTestId("integration-dialog-threads");
  await expect(dialog).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-support")).toHaveCount(0);
  await expect(page.locator("#integration-enabled")).toBeDisabled();
});

test("platform owner sees plan-only Threads access without a support revocation action", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: false,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  await expect
    .poll(() => traffic.threadsSupportStatusRequests.length)
    .toBe(1);
  const statusRequest = traffic.threadsSupportStatusRequests[0];
  expect(statusRequest.method).toBe("GET");
  expect(new URL(statusRequest.url).pathname).toBe(
    `/api/admin/organizations/${selectedOrganizationId}/entitlements/threads-public-engagement/support-status`,
  );
  expect(statusRequest.organizationHeader).toBe(selectedOrganizationId);
  await expect(
    page.getByTestId("threads-entitlement-no-support-override"),
  ).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(page.locator("#integration-enabled")).toBeEnabled();
});

test("Threads support actions fail closed when support status is unavailable", async ({
  page,
}) => {
  await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: false,
    supportStatusUnavailable: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  await expect(
    page.getByTestId("threads-entitlement-support-unavailable"),
  ).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(page.locator("#integration-enabled")).toBeDisabled();
});

test("Threads support actions fail closed on a stale workspace status response", async ({
  page,
}) => {
  await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    supportStatusOrganizationId: organizationId,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  await expect(
    page.getByTestId("threads-entitlement-support-unavailable"),
  ).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(page.locator("#integration-enabled")).toBeEnabled();
});

test("platform owner grants the exact Threads entitlement with an audited reason and refreshes state", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: false,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const support = page.getByTestId("threads-entitlement-support");
  const reason = page.locator("#threads-entitlement-support-reason");
  const enable = page.getByTestId("threads-entitlement-enable");
  await expect(support).toBeVisible();
  await expect(reason).toBeVisible();
  await expect(enable).toBeDisabled();
  await reason.fill("   ");
  await expect(enable).toBeDisabled();
  await reason.fill("  SR-2041 approved by platform support  ");
  await expect(enable).toBeEnabled();
  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await enable.click();

  await expect.poll(() => traffic.threadsEntitlementRequests.length).toBe(1);
  const request = traffic.threadsEntitlementRequests[0];
  expect(request.method).toBe("POST");
  expect(new URL(request.url).pathname).toBe(
    `/api/admin/organizations/${selectedOrganizationId}/entitlements/threads-public-engagement/enable`,
  );
  expect(request.organizationHeader).toBe(selectedOrganizationId);
  expect(request.body).toEqual({
    reason: "SR-2041 approved by platform support",
  });
  await expect
    .poll(() => traffic.threadsSupportStatusRequests.length)
    .toBeGreaterThanOrEqual(2);
  expect(
    traffic.threadsSupportStatusRequests.every(
      (statusRequest) =>
        statusRequest.method === "GET" &&
        new URL(statusRequest.url).pathname ===
          `/api/admin/organizations/${selectedOrganizationId}/entitlements/threads-public-engagement/support-status` &&
        statusRequest.organizationHeader === selectedOrganizationId,
    ),
  ).toBe(true);
  await expect.poll(() => traffic.entitlementReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => traffic.listReads).toBeGreaterThanOrEqual(2);
  await expect(support).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-revoke")).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeEnabled();
  await expect(
    page.getByText("Threads public engagement entitlement enabled", {
      exact: true,
    }),
  ).toBeVisible();
});

test("platform owner revokes the selected workspace support entitlement with an audited reason", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const support = page.getByTestId("threads-entitlement-support");
  const reason = page.locator("#threads-entitlement-support-revoke-reason");
  const revoke = page.getByTestId("threads-entitlement-revoke");
  await expect(support).toBeVisible();
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(reason).toBeVisible();
  await expect(revoke).toBeDisabled();
  await reason.fill("   ");
  await expect(revoke).toBeDisabled();
  await reason.fill("  SR-2043 customer offboarding approved  ");
  await expect(revoke).toBeEnabled();
  await expect(page.locator("#integration-enabled")).toBeEnabled();
  await revoke.click();

  await expect
    .poll(() => traffic.threadsEntitlementRevocationRequests.length)
    .toBe(1);
  const request = traffic.threadsEntitlementRevocationRequests[0];
  expect(request.method).toBe("POST");
  expect(new URL(request.url).pathname).toBe(
    `/api/admin/organizations/${selectedOrganizationId}/entitlements/threads-public-engagement/revoke-support`,
  );
  expect(request.organizationHeader).toBe(selectedOrganizationId);
  expect(request.body).toEqual({
    override_id: "8aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    reason: "SR-2043 customer offboarding approved",
  });
  await expect
    .poll(() => traffic.threadsSupportStatusRequests.length)
    .toBeGreaterThanOrEqual(2);
  expect(
    traffic.threadsSupportStatusRequests.every(
      (statusRequest) =>
        statusRequest.method === "GET" &&
        new URL(statusRequest.url).pathname ===
          `/api/admin/organizations/${selectedOrganizationId}/entitlements/threads-public-engagement/support-status` &&
        statusRequest.organizationHeader === selectedOrganizationId,
    ),
  ).toBe(true);
  await expect.poll(() => traffic.entitlementReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => traffic.listReads).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-enable")).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await expect(
    page.getByText("Threads support entitlement revoked", { exact: true }),
  ).toBeVisible();
});

test("saving enabled Threads configuration is not blocked by an empty support revocation reason", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const revokeReason = page.locator(
    "#threads-entitlement-support-revoke-reason",
  );
  await expect(page.getByTestId("threads-entitlement-support")).toBeVisible();
  await expect(revokeReason).toBeVisible();
  await expect(revokeReason).toHaveValue("");
  await expect(revokeReason).toHaveAttribute("aria-required", "true");
  await expect(revokeReason).not.toHaveAttribute("required", "");
  await expect(page.getByTestId("threads-entitlement-revoke")).toBeDisabled();
  await expect(page.locator("#integration-enabled")).toHaveAttribute(
    "data-state",
    "checked",
  );

  await page.getByTestId("integration-save-threads").click();

  await expect.poll(() => traffic.threadsUpdates.length).toBe(1);
  expect(traffic.threadsUpdates[0].enabled).toBe(true);
  await expect(revokeReason).toHaveValue("");
  await expect(page.getByTestId("threads-entitlement-revoke")).toBeDisabled();
});

test("a revoke conflict refreshes the changed support grant and requires a new reason", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    revokeConflict: true,
    selectedOrganizationId,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const reason = page.locator("#threads-entitlement-support-revoke-reason");
  await reason.fill("SR-2046 revoke the reviewed grant");
  await page.getByTestId("threads-entitlement-revoke").click();

  await expect
    .poll(() => traffic.threadsEntitlementRevocationRequests.length)
    .toBe(1);
  expect(traffic.threadsEntitlementRevocationRequests[0].body).toEqual({
    override_id: "8aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    reason: "SR-2046 revoke the reviewed grant",
  });
  await expect
    .poll(() => traffic.threadsSupportStatusRequests.length)
    .toBeGreaterThanOrEqual(2);
  await expect(reason).toHaveValue("");
  await expect(page.getByTestId("threads-entitlement-revoke")).toBeDisabled();
  await expect(
    page.getByText(
      "The active support grant changed. Its status was refreshed; review it before entering a new revocation reason.",
      { exact: true },
    ),
  ).toBeVisible();

  await reason.fill("SR-2047 revoke the refreshed grant");
  await page.getByTestId("threads-entitlement-revoke").click();
  await expect
    .poll(() => traffic.threadsEntitlementRevocationRequests.length)
    .toBe(2);
  expect(traffic.threadsEntitlementRevocationRequests[1].body).toEqual({
    override_id: "8bbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
    reason: "SR-2047 revoke the refreshed grant",
  });
});

test("support revocation preserves public engagement granted by another entitlement", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    entitlementRemainsEnabledAfterRevoke: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  const reason = page.locator("#threads-entitlement-support-revoke-reason");
  await reason.fill("SR-2045 remove support override only");
  await page.getByTestId("threads-entitlement-revoke").click();

  await expect
    .poll(() => traffic.threadsEntitlementRevocationRequests.length)
    .toBe(1);
  await expect.poll(() => traffic.entitlementReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => traffic.listReads).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(
    page.getByTestId("threads-entitlement-no-support-override"),
  ).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeEnabled();
  await expect(reason).toHaveCount(0);
  await expect(
    page.getByText(
      "Threads support entitlement revoked; public engagement remains enabled by another entitlement.",
      { exact: true },
    ),
  ).toBeVisible();
});

test("Threads activation fails closed when the post-revoke entitlement refresh fails", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: true,
    supportOverrideActive: true,
    failEntitlementRefreshAfterRevoke: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  await page
    .locator("#threads-entitlement-support-revoke-reason")
    .fill("SR-2044 verify fail-closed revocation");
  await page.getByTestId("threads-entitlement-revoke").click();

  await expect
    .poll(() => traffic.threadsEntitlementRevocationRequests.length)
    .toBe(1);
  await expect.poll(() => traffic.entitlementReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => traffic.listReads).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("threads-entitlement-enable")).toHaveCount(0);
  await expect(page.getByTestId("threads-entitlement-revoke")).toHaveCount(0);
  await expect(
    page.getByTestId("threads-entitlement-support-unavailable"),
  ).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await expect(
    page.getByText(
      "The support entitlement was revoked, but its status could not be refreshed. Threads activation remains locked.",
      { exact: true },
    ),
  ).toBeVisible();
});

test("Threads Enabled switch remains locked when the post-grant entitlement refresh fails", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, superAdminUser, {
    threadsEntitlementEnabled: false,
    failEntitlementRefreshAfterGrant: true,
  });
  await page.goto("/settings/integrations");
  await page
    .getByTestId("integration-card-threads")
    .getByRole("button", { name: "Configure" })
    .click();

  await page
    .locator("#threads-entitlement-support-reason")
    .fill("SR-2042 entitlement refresh verification");
  await page.getByTestId("threads-entitlement-enable").click();

  await expect.poll(() => traffic.threadsEntitlementRequests.length).toBe(1);
  await expect.poll(() => traffic.entitlementReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => traffic.listReads).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("threads-entitlement-support")).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await expect(
    page.getByText(
      "The entitlement was enabled, but its status could not be refreshed. The Enabled switch remains locked.",
      { exact: true },
    ),
  ).toBeVisible();
});

test("Threads exposes the exact stored callback and starts live OAuth authorization", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page);
  await page.goto("/settings/integrations");

  const threadsCard = page.getByTestId("integration-card-threads");
  await expect(threadsCard).toContainText("Ready to connect");
  await expect(threadsCard).toContainText("Public replies and mentions");
  await threadsCard.getByRole("button", { name: "Configure" }).click();

  const dialog = page.getByTestId("integration-dialog-threads");
  await expect(dialog).toContainText(
    "Threads direct messages and standalone posts are not supported",
  );
  await expect(page.getByTestId("integration-preparation-only")).toHaveCount(0);
  await expect(page.locator("#integration-enabled")).toBeEnabled();
  const redirectInput = page.locator("#threads-redirect_uri");
  await expect(redirectInput).toHaveValue(
    "https://app.example.test/api/integrations/threads/callback",
  );
  await expect(redirectInput).toBeEditable();
  await expect(page.getByTestId("threads-oauth-callback-copy")).toBeVisible();
  const webhookTokenInput = page.locator("#threads-webhook_verify_token");
  await expect(webhookTokenInput).toHaveAttribute("type", "password");
  await expect(webhookTokenInput).toHaveValue("");
  await expect(webhookTokenInput).toHaveAttribute(
    "placeholder",
    /Stored securely/,
  );
  await expect(
    page.getByTestId("integration-dialog-readiness-threads-redirect_uri"),
  ).toContainText("https://app.example.test/api/integrations/threads/callback");
  await expect(
    page.getByTestId(
      "integration-dialog-readiness-threads-oauth_authorization",
    ),
  ).toContainText("Ready");
  await expect(
    page.getByTestId(
      "integration-dialog-readiness-threads-webhook_verify_token",
    ),
  ).toContainText("Ready");
  await expect(
    page.getByTestId(
      "integration-dialog-readiness-threads-public_engagement_policy",
    ),
  ).toContainText(/direct messages and standalone posts remain unavailable/i);

  await webhookTokenInput.fill("synthetic-threads-webhook-verify-token");
  await page.getByTestId("integration-save-threads").click();
  await expect.poll(() => traffic.threadsUpdates.length).toBe(1);
  expect(traffic.threadsUpdates[0]).toMatchObject({
    enabled: true,
    config: {
      app_id: "234567890123456",
      redirect_uri:
        "https://app.example.test/api/integrations/threads/callback",
      app_review_status: "approved",
    },
    credentials: {
      webhook_verify_token: "synthetic-threads-webhook-verify-token",
    },
  });
  expect(
    (traffic.threadsUpdates[0].credentials as Record<string, unknown>)
      .app_secret,
  ).toBeUndefined();
  await expect(webhookTokenInput).toHaveValue("");
  await expect(dialog).not.toContainText(
    "synthetic-threads-webhook-verify-token",
  );

  await page.getByTestId("integration-connect-threads").click();
  await expect.poll(() => traffic.threadsConnects).toBe(1);
  await expect(page).toHaveURL(/\/mock-threads-oauth\?state=server-issued$/);
});

test("Threads OAuth callback opens the provider and removes its one-time URL marker", async ({
  page,
}) => {
  await installIntegrationMocks(page);
  await page.goto("/settings/integrations?threads=connected&keep=1");

  await expect(page.getByTestId("integration-dialog-threads")).toBeVisible();
  await expect(page).toHaveURL(/\/settings\/integrations\?keep=1$/);
  await expect(
    page.getByText("Threads connected", { exact: true }),
  ).toBeVisible();
});

test("provider preflight assigns callbacks, subscriptions, and relay credentials to the correct owner", async ({
  page,
}) => {
  await installIntegrationMocks(page);
  await page.goto("/settings/integrations");

  await expect(
    page.getByTestId("integration-readiness-meta-callback_endpoint"),
  ).toContainText("Meta webhook callback");
  await expect(
    page.getByTestId("integration-readiness-meta-account_webhook"),
  ).toContainText("Per-account webhook subscription");
  await expect(
    page.getByTestId("integration-readiness-meta-meta_relay_credentials"),
  ).toContainText("Meta relay registration");
  await expect(
    page.getByTestId("integration-readiness-meta-active_whatsapp_connection"),
  ).toContainText("Ready");
  await expect(
    page.getByTestId("integration-readiness-meta-active_instagram_connection"),
  ).toContainText("Ready");
  await expect(
    page.getByTestId("integration-readiness-meta-active_messenger_connection"),
  ).toContainText("Missing");

  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "Configure" })
    .click();
  await expect(
    page.getByTestId("integration-dialog-readiness-meta-callback_endpoint"),
  ).toContainText(`/api/webhook?workspace=${organizationId}`);
  await expect(
    page.getByTestId("integration-dialog-readiness-meta-webhook_verify_token"),
  ).toContainText("Stored centrally, encrypted");
  await expect(
    page.getByTestId("integration-dialog-readiness-meta-account_webhook"),
  ).toContainText(
    "Phone number ID and access token stay on each WhatsApp account",
  );
  await expect(
    page.getByTestId(
      "integration-dialog-readiness-meta-meta_relay_credentials",
    ),
  ).toContainText("Another workspace's tokens");
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("integration-dialog-meta")).toHaveCount(0);

  await expect(
    page.getByTestId("integration-readiness-threads-oauth_authorization"),
  ).toContainText("Ready");
  await expect(
    page.getByTestId("integration-readiness-tiktok-approved_adapter"),
  ).toContainText("Blocked");

  await expect(
    page.getByTestId("integration-readiness-email-relay_signing"),
  ).toContainText("HMAC secrets");
  await expect(
    page.getByTestId("integration-readiness-webchat-relay_signing"),
  ).toContainText("HMAC secrets");
  await expect(
    page.getByTestId("integration-readiness-qwen-endpoint_region"),
  ).toContainText("Approved endpoint region");
  await expect(
    page.getByTestId("integration-readiness-qwen-endpoint_region"),
  ).toContainText("Ready");
});

test("chatbot AI settings defer Qwen provider credentials to the Integration Center", async ({
  page,
}) => {
  await installIntegrationMocks(page);
  await page.goto("/settings/chatbot");
  await page.getByRole("tab", { name: "AI", exact: true }).click();

  const authority = page.getByTestId("chatbot-ai-provider-authority");
  await expect(authority).toBeVisible();
  await expect(authority).toContainText(
    "Provider, model, endpoint and the write-only API key are owned by the Integration Center",
  );
  await expect(page.getByTestId("chatbot-open-qwen-integration")).toBeVisible();

  await page.getByTestId("chatbot-open-qwen-integration").click();
  await expect(page).toHaveURL(/\/settings\/integrations$/);
  await expect(page.getByTestId("integrations-page")).toBeVisible();
});

test("read permission without write permission exposes status but no mutations", async ({
  page,
}) => {
  await installIntegrationMocks(page, viewOnlyUser);
  await page.goto("/settings/integrations");

  await page
    .getByTestId("integration-card-meta")
    .getByRole("button", { name: "View details" })
    .click();

  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await expect(page.locator("#meta-app_id")).not.toBeEditable();
  await expect(page.locator("#meta-app_secret")).toBeDisabled();
  await expect(page.getByTestId("integration-save-meta")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Remove credentials", exact: true }),
  ).toHaveCount(0);
});

test("a user without the integration permission cannot enter the route or see its navigation item", async ({
  page,
}) => {
  const traffic = await installIntegrationMocks(page, deniedUser);
  await page.goto("/settings/integrations");

  await expect(page).toHaveURL(/\/settings$/);
  await expect(page.getByTestId("integrations-page")).toHaveCount(0);
  await expect(
    page.locator('aside a[href="/settings/integrations"]'),
  ).toHaveCount(0);
  expect(traffic.listReads).toBe(0);
});
