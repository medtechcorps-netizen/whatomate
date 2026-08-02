import { expect, test, type Page } from "@playwright/test";

const organizationId = "81111111-1111-4111-8111-111111111111";

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
      oauth: { supported: true, available: true, mode: "embedded_signup" },
      test_supported: true,
    },
    {
      provider: "threads",
      display_name: "Threads",
      status: "adapter_unavailable",
      enabled: false,
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
      },
      connection: connection(),
      oauth: { supported: true, available: false, mode: "oauth" },
      test_supported: false,
      message:
        "Threads public replies and mentions remain disabled until an approved adapter is installed. Direct messages are not supported.",
      required_scopes: [
        "threads_basic",
        "threads_read_replies",
        "threads_manage_replies",
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
  options: { platformMeta?: boolean } = {},
) {
  const integrations = integrationFixture();
  if (options.platformMeta) {
    const meta = integrations.find((item) => item.provider === "meta")!;
    meta.config.management_mode = "platform";
    meta.config.webhook_callback_path = "/api/webhook";
    meta.credentials.app_secret.source = "platform";
    meta.credentials.webhook_verify_token.source = "platform";
  }
  const traffic = {
    listReads: 0,
    updates: [] as Array<Record<string, unknown>>,
    qwenUpdates: [] as Array<Record<string, unknown>>,
    credentialClears: [] as string[],
  };

  await page.addInitScript((storedUser) => {
    window.localStorage.setItem("user", JSON.stringify(storedUser));
    window.localStorage.setItem("locale", "en");
  }, user);

  // Register the catch-all first. Playwright gives the later, specific routes
  // priority, keeping unrelated layout requests deterministic.
  await page.route("**/api/**", (route) =>
    route.fulfill({ json: { data: {} } }),
  );
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: user } }),
  );
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: "licensed",
          plan_code: "rereply-growth",
          entitlements: {},
        },
      },
    }),
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

test("unavailable adapters are visibly fail-closed and cannot be enabled or connected", async ({
  page,
}) => {
  await installIntegrationMocks(page);
  await page.goto("/settings/integrations");

  const threadsCard = page.getByTestId("integration-card-threads");
  await expect(threadsCard).toContainText("Adapter pending");
  await expect(threadsCard).toContainText(
    "disabled until an approved adapter is installed",
  );
  await threadsCard.getByRole("button", { name: "Configure" }).click();

  const dialog = page.getByTestId("integration-dialog-threads");
  await expect(dialog).toContainText(
    "disabled until an approved adapter is installed",
  );
  await expect(page.getByTestId("integration-preparation-only")).toBeVisible();
  await expect(page.locator("#integration-enabled")).toBeDisabled();
  await expect(page.getByTestId("integration-connect-threads")).toHaveCount(0);
  await expect(page.getByTestId("integration-test-threads")).toHaveCount(0);
  await expect(
    page.getByTestId("integration-dialog-readiness-threads-approved_adapter"),
  ).toContainText("Blocked");
  await expect(
    page.getByTestId("integration-dialog-readiness-threads-approved_adapter"),
  ).toContainText("Direct messages are not supported");
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
  ).toContainText("Instagram & Messenger relay credentials");

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
  ).toContainText("do not reuse the central WhatsApp app secret");
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("integration-dialog-meta")).toHaveCount(0);

  await expect(
    page.getByTestId("integration-readiness-threads-approved_adapter"),
  ).toContainText("Blocked");
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
