import { expect, test, type Page } from "@playwright/test";

const organizationId = "91111111-1111-4111-8111-111111111111";
const otherOrganizationId = "90000000-0000-4000-8000-000000000001";
const accountId = "92222222-2222-4222-8222-222222222222";

const accountReader = {
  id: "93333333-3333-4333-8333-333333333333",
  email: "account-reader@example.test",
  full_name: "Account Reader",
  organization_id: organizationId,
  organization_name: "Klinik Relive",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "94444444-4444-4444-8444-444444444444",
    name: "account-reader",
    description: "Read-only account access",
    is_system: false,
    permissions: [
      { id: "accounts-read", resource: "accounts", action: "read" },
    ],
  },
};

const integrationReader = {
  ...accountReader,
  id: "95555555-5555-4555-8555-555555555555",
  email: "integration-reader@example.test",
  full_name: "Integration Reader",
  role: {
    ...accountReader.role,
    id: "96666666-6666-4666-8666-666666666666",
    name: "integration-reader",
    description: "Read-only account and integration access",
    permissions: [
      ...accountReader.role.permissions,
      {
        id: "settings-integrations-read",
        resource: "settings.integrations",
        action: "read",
      },
    ],
  },
};

const accountWriter = {
  ...integrationReader,
  id: "97777777-7777-4777-8777-777777777777",
  email: "account-writer@example.test",
  full_name: "Account Writer",
  role: {
    ...integrationReader.role,
    id: "98888888-8888-4888-8888-888888888888",
    name: "account-writer",
    description: "Account write and integration read access",
    permissions: [
      ...integrationReader.role.permissions,
      { id: "accounts-write", resource: "accounts", action: "write" },
    ],
  },
};

const accountIntegrationWriter = {
  ...accountWriter,
  id: "97999999-9999-4999-8999-999999999999",
  email: "account-integration-writer@example.test",
  full_name: "Account Integration Writer",
  role: {
    ...accountWriter.role,
    id: "98000000-0000-4000-8000-000000000000",
    name: "account-integration-writer",
    description: "Account and integration write access",
    permissions: [
      ...accountWriter.role.permissions,
      {
        id: "settings-integrations-write",
        resource: "settings.integrations",
        action: "write",
      },
    ],
  },
};

const accountRecord = {
  id: accountId,
  name: "Klinik Relive WhatsApp",
  phone_id: "101010101010101",
  business_id: "202020202020202",
  api_version: "v21.0",
  is_default_incoming: true,
  is_default_outgoing: true,
  auto_read_receipt: true,
  business_calling_enabled: false,
  status: "active",
  has_access_token: true,
  created_at: "2026-07-29T08:00:00Z",
  updated_at: "2026-07-29T08:30:00Z",
};

async function installAccountReaderMocks(
  page: Page,
  user = accountReader,
  accountStatus = accountRecord.status,
) {
  const mutatingRequests: string[] = [];
  const resolvedAccount = { ...accountRecord, status: accountStatus };

  await page.addInitScript((user) => {
    window.localStorage.setItem("user", JSON.stringify(user));
    window.localStorage.setItem("locale", "en");
  }, user);

  await page.route("**/api/**", async (route) => {
    if (route.request().method() !== "GET") {
      mutatingRequests.push(
        `${route.request().method()} ${route.request().url()}`,
      );
    }
    await route.fulfill({ json: { data: {} } });
  });
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
  await page.route(/\/api\/accounts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { accounts: [resolvedAccount] } } }),
  );
  await page.route(
    new RegExp(`/api/accounts/${accountId}(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: {
          data: resolvedAccount,
        },
      }),
  );

  return mutatingRequests;
}

test("account readers never receive duplicate webhook credential controls", async ({
  page,
}) => {
  const mutatingRequests = await installAccountReaderMocks(page);
  await page.goto(`/settings/accounts/${accountId}`);

  await expect(
    page.getByRole("heading", { name: "Klinik Relive WhatsApp" }),
  ).toBeVisible();
  await expect(
    page.getByTestId("meta-integration-management-notice"),
  ).toContainText(
    "App ID, Config ID, App Secret and Webhook Verify Token are managed centrally by an administrator.",
  );
  await expect(
    page.getByTestId("meta-integration-management-link"),
  ).toHaveCount(0);
  await expect(page.getByText("Meta App ID", { exact: true })).toHaveCount(0);
  await expect(page.getByText("App Secret", { exact: true })).toHaveCount(0);
  await expect(page.locator('input[type="password"]')).toHaveCount(1);
  await expect(page.getByText("Verify Token", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText("The verification token is hidden for your role.", {
      exact: true,
    }),
  ).toHaveCount(0);

  await expect(
    page.getByRole("button", { name: "Subscribe", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Profile", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Save", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);
  expect(mutatingRequests).toEqual([]);
});

test("account readers cannot register a phone awaiting recovery", async ({
  page,
}) => {
  const mutatingRequests = await installAccountReaderMocks(
    page,
    accountReader,
    "pending_registration",
  );
  await page.goto(`/settings/accounts/${accountId}`);

  await expect(
    page.getByRole("button", { name: "Register phone", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Subscribe", exact: true }),
  ).toHaveCount(0);
  expect(mutatingRequests).toEqual([]);
});

test("account writers confirm recovery once without sending or rendering a PIN", async ({
  page,
}) => {
  await installAccountReaderMocks(
    page,
    accountWriter,
    "pending_registration",
  );

  let accountStatus = "pending_registration";
  const registrationRequests: Array<{
    body: string | null;
    organizationId: string | undefined;
  }> = [];
  let releaseRegistration: (() => void) | undefined;
  const registrationGate = new Promise<void>((resolve) => {
    releaseRegistration = resolve;
  });

  await page.route(
    new RegExp(`/api/accounts/${accountId}(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: { data: { ...accountRecord, status: accountStatus } },
      }),
  );
  await page.route(
    new RegExp(`/api/accounts/${accountId}/register(?:\\?.*)?$`),
    async (route) => {
      registrationRequests.push({
        body: route.request().postData(),
        organizationId:
          route.request().headers()["x-organization-id"],
      });
      await registrationGate;
      accountStatus = "pending_subscription";
      await route.fulfill({
        json: {
          data: {
            success: true,
            // A stale server shape must still never become UI content.
            pin: "483920",
          },
        },
      });
    },
  );

  await page.goto(`/settings/accounts/${accountId}`);

  const openRegistration = page.getByRole("button", {
    name: "Register phone",
    exact: true,
  });
  await expect(openRegistration).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Subscribe", exact: true }),
  ).toHaveCount(0);

  await openRegistration.click();
  const confirmation = page.getByRole("alertdialog");
  await expect(confirmation).toContainText("Klinik Relive WhatsApp");
  expect(registrationRequests).toHaveLength(0);

  const confirmRegistration = confirmation.getByTestId(
    "confirm-phone-registration",
  );
  await confirmRegistration.click();
  await expect(confirmRegistration).toBeDisabled();
  await expect(confirmRegistration).toContainText("Registering...");
  await expect.poll(() => registrationRequests).toHaveLength(1);

  releaseRegistration?.();

  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "Phone registration completed successfully.",
    }),
  ).toBeVisible();
  expect(registrationRequests).toEqual([
    { body: null, organizationId },
  ]);
  await expect(openRegistration).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Subscribe", exact: true }),
  ).toBeVisible();
  await expect(page.locator("body")).not.toContainText("483920");
});

test("phone recovery failure stays generic and does not expose provider details", async ({
  page,
}) => {
  await installAccountReaderMocks(
    page,
    accountWriter,
    "pending_registration",
  );
  let registrationRequests = 0;
  await page.route(
    new RegExp(`/api/accounts/${accountId}/register(?:\\?.*)?$`),
    async (route) => {
      registrationRequests += 1;
      await route.fulfill({
        status: 400,
        json: { message: "Provider rejected two-step PIN 654321" },
      });
    },
  );

  await page.goto(`/settings/accounts/${accountId}`);
  await page
    .getByRole("button", { name: "Register phone", exact: true })
    .click();
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "Register phone", exact: true })
    .click();

  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "Phone registration could not be completed. Please try again.",
    }),
  ).toBeVisible();
  expect(registrationRequests).toBe(1);
  await expect(page.locator("body")).not.toContainText("654321");
  await expect(page.locator("body")).not.toContainText(
    "Provider rejected two-step PIN",
  );
});

test("subscribe is available only for subscription recovery states", async ({
  page,
}) => {
  await installAccountReaderMocks(page, accountWriter, "pending_subscription");
  let accountStatus = "pending_subscription";
  const subscriptionOrganizationIds: Array<string | undefined> = [];
  await page.route(
    new RegExp(`/api/accounts/${accountId}(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: { data: { ...accountRecord, status: accountStatus } },
      }),
  );
  await page.route(
    new RegExp(`/api/accounts/${accountId}/subscribe(?:\\?.*)?$`),
    async (route) => {
      subscriptionOrganizationIds.push(
        route.request().headers()["x-organization-id"],
      );
      accountStatus = "active";
      await route.fulfill({ json: { data: { success: true } } });
    },
  );

  await page.goto(`/settings/accounts/${accountId}`);
  const subscribe = page.getByRole("button", {
    name: "Subscribe",
    exact: true,
  });
  await expect(subscribe).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Register phone", exact: true }),
  ).toHaveCount(0);

  await subscribe.click();
  await expect.poll(() => subscriptionOrganizationIds).toEqual([
    organizationId,
  ]);
  await expect(subscribe).toHaveCount(0);

  accountStatus = "subscription_failed";
  await page.reload();
  await expect(subscribe).toBeVisible();

  accountStatus = "active";
  await page.reload();
  await expect(subscribe).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Register phone", exact: true }),
  ).toHaveCount(0);
});

test("stale or missing workspace context cannot trigger recovery mutations", async ({
  page,
}) => {
  await installAccountReaderMocks(
    page,
    accountWriter,
    "pending_registration",
  );
  let accountStatus = "pending_registration";
  const recoveryRequests: string[] = [];

  await page.route(
    new RegExp(`/api/accounts/${accountId}(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: { data: { ...accountRecord, status: accountStatus } },
      }),
  );
  await page.route(
    new RegExp(`/api/accounts/${accountId}/(?:register|subscribe)(?:\\?.*)?$`),
    async (route) => {
      recoveryRequests.push(route.request().url());
      await route.fulfill({ json: { data: { success: true } } });
    },
  );

  const setClientOrganizationContext = async (
    selectedOrganizationId: string | null,
    authOrganizationId: string,
  ) =>
    page.evaluate(
      ({ selectedOrganizationId, authOrganizationId }) => {
        const app = (document.querySelector("#app") as any)?.__vue_app__;
        const pinia = app?.config.globalProperties.$pinia;
        const organizations = pinia?._s.get("organizations");
        const auth = pinia?._s.get("auth");
        organizations?.selectOrganization(selectedOrganizationId);
        if (auth?.user) {
          auth.user = { ...auth.user, organization_id: authOrganizationId };
        }
      },
      { selectedOrganizationId, authOrganizationId },
    );

  await page.goto(`/settings/accounts/${accountId}`);
  const registerButton = page.getByRole("button", {
    name: "Register phone",
    exact: true,
  });
  await expect(registerButton).toBeVisible();
  const staleRegisterButton = await registerButton.elementHandle();

  await setClientOrganizationContext(otherOrganizationId, organizationId);
  await expect(registerButton).toHaveCount(0);
  await staleRegisterButton?.evaluate((element) =>
    (element as HTMLButtonElement).click(),
  );
  await page.waitForTimeout(100);
  expect(recoveryRequests).toEqual([]);

  await setClientOrganizationContext(null, organizationId);
  accountStatus = "pending_subscription";
  await page.reload();
  const subscribeButton = page.getByRole("button", {
    name: "Subscribe",
    exact: true,
  });
  await expect(subscribeButton).toBeVisible();
  const staleSubscribeButton = await subscribeButton.elementHandle();

  await setClientOrganizationContext(null, "");
  await expect(subscribeButton).toHaveCount(0);
  await staleSubscribeButton?.evaluate((element) =>
    (element as HTMLButtonElement).click(),
  );
  await page.waitForTimeout(100);
  expect(recoveryRequests).toEqual([]);
});

test("account users with integration read access can open the central Meta settings", async ({
  page,
}) => {
  await installAccountReaderMocks(page, integrationReader);
  await page.goto(`/settings/accounts/${accountId}`);

  const notice = page.getByTestId("meta-integration-management-notice");
  await expect(notice).toContainText(
    "App ID, Config ID, App Secret and Webhook Verify Token are managed centrally in Integration Center.",
  );
  await expect(
    page.getByTestId("meta-integration-management-link"),
  ).toHaveAttribute("href", "/settings/integrations");
});

test("account writers without integration write cannot configure a phone webhook", async ({
  page,
}) => {
  const mutatingRequests = await installAccountReaderMocks(page, accountWriter);
  await page.goto(`/settings/accounts/${accountId}`);

  await expect(
    page.getByRole("button", {
      name: "Configure ReReply webhook",
      exact: true,
    }),
  ).toHaveCount(0);
  expect(mutatingRequests).toEqual([]);
});

test("authorized account and integration writers configure only after confirmation", async ({
  page,
}) => {
  const mutatingRequests = await installAccountReaderMocks(
    page,
    accountIntegrationWriter,
  );
  await page.goto(`/settings/accounts/${accountId}`);

  const action = page.getByRole("button", {
    name: "Configure ReReply webhook",
    exact: true,
  });
  await expect(action).toBeVisible();
  expect(mutatingRequests).toEqual([]);

  await action.click();
  await expect(
    page.getByText(
      "This configures Meta to send inbound events for only this WhatsApp phone account to ReReply. It does not change any other phone number, WhatsApp Business Account, or the app-level webhook.",
      { exact: true },
    ),
  ).toBeVisible();
  expect(mutatingRequests).toEqual([]);

  await page.getByRole("button", { name: "Configure", exact: true }).click();
  await expect.poll(() => mutatingRequests).toHaveLength(1);
  expect(mutatingRequests[0]).toMatch(
    new RegExp(`^POST .*/api/accounts/${accountId}/webhook-override$`),
  );
});

test("the WhatsApp account list no longer exposes an App ID column", async ({
  page,
}) => {
  await installAccountReaderMocks(page);
  await page.goto("/settings/accounts");

  await expect(
    page.getByText("Klinik Relive WhatsApp", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("columnheader", { name: "App ID", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("columnheader", { name: "Phone Number ID", exact: true }),
  ).toBeVisible();
});

test("manual account creation never submits central Meta app credentials", async ({
  page,
}) => {
  await installAccountReaderMocks(page, accountWriter);
  let submittedPayload: Record<string, unknown> | undefined;
  await page.route(/\/api\/accounts(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "POST") {
      await route.fallback();
      return;
    }
    submittedPayload = route.request().postDataJSON();
    await route.fulfill({
      json: {
        data: {
          ...accountRecord,
          id: "99999999-9999-4999-8999-999999999999",
          name: "New Klinik Relive Line",
        },
      },
    });
  });

  await page.goto("/settings/accounts/new");

  const inputFor = (label: string) =>
    page
      .locator("label")
      .filter({ hasText: label })
      .locator("..")
      .locator("input");
  await inputFor("Account Name").fill("New Klinik Relive Line");
  await inputFor("Phone Number ID").fill("303030303030303");
  await inputFor("Business Account ID").fill("404040404040404");
  await page.locator('input[type="password"]').fill("per-account-access-token");
  await page.getByRole("button", { name: "Create", exact: true }).click();

  await expect.poll(() => submittedPayload).toBeTruthy();
  expect(submittedPayload).toMatchObject({
    name: "New Klinik Relive Line",
    phone_id: "303030303030303",
    business_id: "404040404040404",
    access_token: "per-account-access-token",
  });
  expect(submittedPayload).not.toHaveProperty("app_id");
  expect(submittedPayload).not.toHaveProperty("app_secret");
  expect(submittedPayload).not.toHaveProperty("webhook_verify_token");
});
