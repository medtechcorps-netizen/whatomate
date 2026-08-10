import { expect, test, type Page } from "@playwright/test";

const organizationId = "a1111111-1111-4111-8111-111111111111";
const accountId = "a2222222-2222-4222-8222-222222222222";
const user = {
  id: "a3333333-3333-4333-8333-333333333333",
  email: "messenger-owner@example.test",
  full_name: "Messenger Workspace Owner",
  organization_id: organizationId,
  organization_name: "Klinik Insan Ampang",
  is_super_admin: true,
  is_reseller_admin: false,
};

const ownedPage = {
  business_id: "business-100",
  business_name: "Medtech Healthcare",
  page_id: "page-200",
  page_name: "Klinik Insan Ampang",
  ownership: "owned" as const,
  selectable: true,
  tasks: ["MESSAGING", "MANAGE"],
};

const clientPage = {
  business_id: "business-100",
  business_name: "Medtech Healthcare",
  page_id: "page-300",
  page_name: "Client Clinic Page",
  ownership: "client_access" as const,
  // The browser must still fail closed if an inconsistent response marks a
  // client-access Page selectable.
  selectable: true,
  disabled_reason: "Client-only Page access cannot authorize this workspace.",
  tasks: ["MESSAGING"],
};

const businessSystemUser = {
  user_id: "900000000000001",
  name: "",
  token_kind: "SYSTEM_USER",
  client_business_id: "business-100",
};

async function mockMessengerWorkspace(
  page: Page,
  options: {
    featureDisabled?: boolean;
    preparationDelayMs?: number;
    expiryByCallMs?: number[];
    reviewReady?: boolean;
  } = {},
) {
  let connectedAccount: Record<string, unknown> | null = null;
  let startCalls = 0;
  let manualCreateCalls = 0;
  let exchangePayload: unknown = null;
  let selectPayload: unknown = null;

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "light");
    (window as any).__messengerFBLoginCalls = [];
    (window as any).__messengerFBLoginSynchronous = [];
    (window as any).__messengerFBInitCalls = [];
    (window as any).__messengerContinueClickActive = false;
    document.addEventListener(
      "click",
      (event) => {
        const target = event.target as HTMLElement | null;
        if (target?.closest('[data-testid="messenger-onboarding-continue"]')) {
          (window as any).__messengerContinueClickActive = true;
        }
      },
      true,
    );
    document.addEventListener("click", (event) => {
      const target = event.target as HTMLElement | null;
      if (target?.closest('[data-testid="messenger-onboarding-continue"]')) {
        (window as any).__messengerContinueClickActive = false;
      }
    });
    (window as any).FB = {
      init: (settings: unknown) => {
        (window as any).__messengerFBInitCalls.push(settings);
      },
      login: (callback: (response: unknown) => void, settings: unknown) => {
        (window as any).__messengerFBLoginCalls.push(settings);
        (window as any).__messengerFBLoginSynchronous.push(
          (window as any).__messengerContinueClickActive,
        );
        callback({ authResponse: { code: "facebook-code-1" } });
      },
    };
  }, user);

  // Register fallback first because Playwright gives later routes priority.
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
          entitlements: { "omnichannel.enabled": true },
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/conversations(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { conversations: [], total: 0 } } }),
  );
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, (route) => {
    if (route.request().method() === "POST") {
      manualCreateCalls += 1;
      return route.fulfill({
        status: 500,
        json: { message: "manual creation is forbidden" },
      });
    }
    return route.fulfill({
      json: {
        data: { accounts: connectedAccount ? [connectedAccount] : [] },
      },
    });
  });
  await page.route(
    /\/api\/integrations\/meta\/messenger\/onboarding\/start$/,
    async (route) => {
      startCalls += 1;
      if (options.preparationDelayMs) {
        await new Promise((resolve) =>
          setTimeout(resolve, options.preparationDelayMs),
        );
      }
      if (options.featureDisabled) {
        return route.fulfill({
          status: 503,
          json: {
            message: "Messenger onboarding is disabled for this deployment.",
          },
        });
      }
      const expiresInMs = options.expiryByCallMs?.[startCalls - 1] ?? 60_000;
      return route.fulfill({
        json: {
          data: {
            provider: "meta",
            channel: "messenger",
            mode: "facebook_login_for_business",
            nonce: "nonce-1",
            expires_at: new Date(Date.now() + expiresInMs).toISOString(),
            public_config: {
              app_id: "facebook-app-1",
              config_id: "messenger-config-1",
              graph_api_version: "v24.0",
              response_type: "code",
              override_default_response_type: true,
            },
          },
        },
      });
    },
  );
  await page.route(
    /\/api\/integrations\/meta\/messenger\/onboarding\/exchange$/,
    async (route) => {
      exchangePayload = route.request().postDataJSON();
      return route.fulfill({
        json: {
          data: {
            session_id: "session-1",
            expires_at: "2026-08-06T04:20:00Z",
            platform: businessSystemUser,
            workspace: {
              organization_id: organizationId,
              organization_name: "Klinik Insan Ampang",
            },
            businesses: [{ id: "business-100", name: "Medtech Healthcare" }],
            pages: [ownedPage, clientPage],
          },
        },
      });
    },
  );
  await page.route(
    /\/api\/integrations\/meta\/messenger\/onboarding\/select$/,
    async (route) => {
      selectPayload = route.request().postDataJSON();
      const onboardingState = options.reviewReady
        ? "review_relay_ready"
        : "awaiting_relay_registry";
      connectedAccount = {
        id: accountId,
        channel: "messenger",
        provider: "relay",
        name: ownedPage.page_name,
        external_account_id: ownedPage.page_id,
        status: "pending",
        capabilities: { text: true, replies: true },
        config: {
          onboarding_state: onboardingState,
          registry_recognized: false,
          identity_confirmed_id: ownedPage.page_id,
          outbound_enabled: false,
          ai_reply_enabled: false,
          ...(options.reviewReady ? { review_only: true } : {}),
        },
        has_credentials: true,
        outbox_pending: 0,
        outbox_failed: 0,
      };
      return route.fulfill({
        json: {
          data: {
            account: connectedAccount,
            onboarding_state: onboardingState,
            subscription_verified: true,
            registry_recognized: false,
          },
        },
      });
    },
  );

  return {
    startCalls: () => startCalls,
    manualCreateCalls: () => manualCreateCalls,
    exchangePayload: () => exchangePayload,
    selectPayload: () => selectPayload,
  };
}

test("connects an owned Messenger Page through the backend-gated Facebook code flow", async ({
  page,
}) => {
  const calls = await mockMessengerWorkspace(page, { preparationDelayMs: 250 });
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/inbox");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByTestId("channel-connect-type").selectOption("messenger");

  await expect(
    page.getByTestId("channel-connect-identity-confirmation"),
  ).toHaveCount(0);
  await expect(
    page.getByPlaceholder("Provider-issued immutable ID"),
  ).toHaveCount(0);
  await expect(
    page.getByPlaceholder("https://relay.example.com/..."),
  ).toHaveCount(0);

  const continueButton = page.getByTestId("messenger-onboarding-continue");
  await expect(continueButton).toBeDisabled();
  await expect(
    page.getByTestId("messenger-onboarding-preparation-status"),
  ).toContainText("Preparing a short-lived authorization");
  expect(
    await page.evaluate(() => (window as any).__messengerFBLoginCalls.length),
  ).toBe(0);

  await expect(continueButton).toBeEnabled();
  await expect(
    page.getByTestId("messenger-onboarding-preparation-status"),
  ).toContainText("Secure preparation ready");
  await continueButton.click();
  await expect(
    page.getByTestId("messenger-onboarding-inventory"),
  ).toBeVisible();

  expect(calls.startCalls()).toBe(1);
  expect(calls.exchangePayload()).toEqual({
    code: "facebook-code-1",
    nonce: "nonce-1",
  });
  const loginCalls = await page.evaluate(
    () => (window as any).__messengerFBLoginCalls,
  );
  expect(loginCalls).toEqual([
    {
      config_id: "messenger-config-1",
      response_type: "code",
      override_default_response_type: true,
    },
  ]);
  expect(loginCalls[0]).not.toHaveProperty("scope");
  expect(
    await page.evaluate(() => (window as any).__messengerFBLoginSynchronous),
  ).toEqual([true]);
  expect(
    await page.evaluate(() => (window as any).__messengerFBInitCalls),
  ).toEqual([
    {
      appId: "facebook-app-1",
      cookie: true,
      xfbml: false,
      version: "v24.0",
    },
  ]);

  await expect(
    page.getByTestId("messenger-onboarding-inventory"),
  ).toContainText(`Business system user ${businessSystemUser.user_id}`);
  await expect(
    page.getByTestId("messenger-onboarding-inventory"),
  ).toContainText(businessSystemUser.user_id);
  await expect(
    page.getByTestId("messenger-onboarding-inventory"),
  ).toContainText(organizationId);

  const clientOnly = page.getByTestId(`messenger-page-${clientPage.page_id}`);
  await expect(clientOnly).toBeDisabled();
  await expect(clientOnly).toContainText("Client access only");
  await expect(clientOnly).toContainText(clientPage.disabled_reason);

  await page.getByTestId(`messenger-page-${ownedPage.page_id}`).click();
  const summary = page.getByTestId("messenger-exact-connection-summary");
  await expect(summary).toContainText("Platform");
  await expect(summary).toContainText(
    `Business system user ${businessSystemUser.user_id}`,
  );
  await expect(summary).toContainText("Workspace");
  await expect(summary).toContainText("Klinik Insan Ampang");
  await expect(summary).toContainText("Business");
  await expect(summary).toContainText(ownedPage.business_name);
  await expect(summary).toContainText(ownedPage.business_id);
  await expect(summary).toContainText("Page");
  await expect(summary).toContainText(ownedPage.page_name);
  await expect(summary).toContainText(ownedPage.page_id);

  await page.getByTestId("messenger-onboarding-select").click();
  const connectionsSidebar = page
    .locator("aside")
    .filter({ hasText: "Available adapters" });
  await expect(
    connectionsSidebar.getByText("Awaiting relay registry"),
  ).toBeVisible();
  expect(calls.selectPayload()).toEqual({
    session_id: "session-1",
    business_id: ownedPage.business_id,
    page_id: ownedPage.page_id,
  });
  expect(calls.manualCreateCalls()).toBe(0);

  await expect(
    connectionsSidebar.getByRole("button", {
      name: "Test Klinik Insan Ampang is unavailable until the protected runtime relay registry recognizes this Facebook authorization.",
      exact: true,
    }),
  ).toBeDisabled();
});

test("accepts an inbound-only staging review result without unlocking production controls", async ({
  page,
}) => {
  const calls = await mockMessengerWorkspace(page, { reviewReady: true });
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/inbox");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByTestId("channel-connect-type").selectOption("messenger");
  const continueButton = page.getByTestId("messenger-onboarding-continue");
  await expect(continueButton).toBeEnabled();
  await continueButton.click();
  await expect(
    page.getByTestId("messenger-onboarding-inventory"),
  ).toBeVisible();
  await page.getByTestId(`messenger-page-${ownedPage.page_id}`).click();
  await page.getByTestId("messenger-onboarding-select").click();

  await expect(page.getByTestId("messenger-onboarding-inventory")).toHaveCount(
    0,
  );
  const connectionsSidebar = page
    .getByRole("complementary")
    .filter({ hasText: "Available adapters" });
  await expect(
    connectionsSidebar.getByText(
      /^Staging review relay ready - inbound only \| 1\/5 checks$/,
    ),
  ).toBeVisible();
  await expect(
    connectionsSidebar.getByRole("button", {
      name: "Test Klinik Insan Ampang is unavailable until the protected runtime relay registry recognizes this Facebook authorization.",
      exact: true,
    }),
  ).toBeDisabled();
  await expect(
    connectionsSidebar.getByRole("button", { name: "Approve outbound" }),
  ).toHaveCount(0);
  expect(calls.selectPayload()).toEqual({
    session_id: "session-1",
    business_id: ownedPage.business_id,
    page_id: ownedPage.page_id,
  });
  expect(calls.manualCreateCalls()).toBe(0);
});

test("fails closed before Facebook Login when the backend feature gate is disabled", async ({
  page,
}) => {
  const calls = await mockMessengerWorkspace(page, { featureDisabled: true });
  await page.goto("/inbox");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByTestId("channel-connect-type").selectOption("messenger");

  await expect(page.getByTestId("messenger-onboarding-error")).toContainText(
    "Messenger onboarding is disabled for this deployment.",
  );
  expect(calls.startCalls()).toBe(1);
  await expect(
    page.getByTestId("messenger-onboarding-continue"),
  ).toBeDisabled();
  await expect(page.getByTestId("messenger-onboarding-prepare")).toHaveText(
    "Retry Facebook preparation",
  );
  await page.waitForTimeout(100);
  expect(calls.startCalls()).toBe(1);

  await page.getByTestId("messenger-onboarding-prepare").click();
  await expect.poll(calls.startCalls).toBe(2);
  expect(calls.manualCreateCalls()).toBe(0);
  expect(
    await page.evaluate(() => (window as any).__messengerFBLoginCalls.length),
  ).toBe(0);
  await expect(page.getByTestId("messenger-onboarding-inventory")).toHaveCount(
    0,
  );
});

test("expires an unused preparation and requires an explicit refresh", async ({
  page,
}) => {
  const calls = await mockMessengerWorkspace(page, {
    expiryByCallMs: [60_000, 60_000],
  });
  await page.goto("/inbox");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByTestId("channel-connect-type").selectOption("messenger");

  const continueButton = page.getByTestId("messenger-onboarding-continue");
  await expect(continueButton).toBeEnabled();
  await page.evaluate(() => {
    const realNow = Date.now;
    (window as any).__messengerRealDateNow = realNow;
    const afterNonceExpiry = realNow() + 61_000;
    Date.now = () => afterNonceExpiry;
  });
  await continueButton.click();
  await page.evaluate(() => {
    Date.now = (window as any).__messengerRealDateNow;
  });
  await expect(
    page.getByTestId("messenger-onboarding-preparation-status"),
  ).toContainText("Preparation expired - refresh required");
  await expect(continueButton).toBeDisabled();
  expect(calls.startCalls()).toBe(1);
  expect(
    await page.evaluate(() => (window as any).__messengerFBLoginCalls.length),
  ).toBe(0);

  await page.getByTestId("messenger-onboarding-prepare").click();
  await expect(continueButton).toBeEnabled();
  expect(calls.startCalls()).toBe(2);
});

test("preserves the existing manual Instagram identity and relay safeguards", async ({
  page,
}) => {
  await mockMessengerWorkspace(page);
  await page.goto("/inbox");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByTestId("channel-connect-type").selectOption("instagram");

  await expect(
    page.getByPlaceholder("Provider-issued immutable ID"),
  ).toBeVisible();
  await expect(
    page.getByPlaceholder("https://relay.example.com/..."),
  ).toBeVisible();
  await expect(
    page.getByTestId("channel-connect-identity-confirmation"),
  ).toBeVisible();
  await expect(page.getByTestId("channel-connect-submit")).toBeDisabled();
  await expect(
    page.getByTestId("messenger-facebook-login-connect"),
  ).toHaveCount(0);
});
