import { expect, test, type Page } from "@playwright/test";

const organizationId = "b1111111-1111-4111-8111-111111111111";
const otherOrganizationId = "b9999999-9999-4999-8999-999999999999";

const accountWriter = {
  id: "b2222222-2222-4222-8222-222222222222",
  email: "embedded-signup-writer@example.test",
  full_name: "Embedded Signup Writer",
  organization_id: organizationId,
  organization_name: "ReReply",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "b3333333-3333-4333-8333-333333333333",
    name: "embedded-signup-writer",
    description: "Account writer for embedded signup testing",
    is_system: false,
    permissions: [
      { id: "accounts-read", resource: "accounts", action: "read" },
      { id: "accounts-write", resource: "accounts", action: "write" },
    ],
  },
};

type MetaLoginMode = "complete" | "pending";

interface EmbeddedSignupMockOptions {
  configOrganizationId?: string;
  exchangeWarning?: string;
  metaLoginMode?: MetaLoginMode;
  exposeMultipleOrganizations?: boolean;
}

interface EmbeddedSignupRequestCapture {
  accountOrganizationIds: Array<string | undefined>;
  configOrganizationIds: Array<string | undefined>;
  exchangeOrganizationIds: Array<string | undefined>;
  exchangeRequests: number;
  facebookSDKRequests: number;
}

async function installEmbeddedSignupMocks(
  page: Page,
  options: EmbeddedSignupMockOptions = {},
) {
  const capture: EmbeddedSignupRequestCapture = {
    accountOrganizationIds: [],
    configOrganizationIds: [],
    exchangeOrganizationIds: [],
    exchangeRequests: 0,
    facebookSDKRequests: 0,
  };

  await page.addInitScript((user) => {
    const observedWindow = window as typeof window & {
      __observedXHRTimeouts?: number[];
    };
    observedWindow.__observedXHRTimeouts = [];
    const timeoutDescriptor = Object.getOwnPropertyDescriptor(
      XMLHttpRequest.prototype,
      "timeout",
    );
    if (timeoutDescriptor?.get && timeoutDescriptor.set) {
      Object.defineProperty(XMLHttpRequest.prototype, "timeout", {
        configurable: timeoutDescriptor.configurable,
        enumerable: timeoutDescriptor.enumerable,
        get: timeoutDescriptor.get,
        set(value: number) {
          observedWindow.__observedXHRTimeouts?.push(value);
          timeoutDescriptor.set?.call(this, value);
        },
      });
    }

    window.localStorage.setItem("user", JSON.stringify(user));
    window.localStorage.setItem("locale", "en");
    // Simulate stale browser state from another workspace. Requests that pin
    // their launch workspace must win over this interceptor fallback.
    window.localStorage.setItem(
      "selected_organization_id",
      "b9999999-9999-4999-8999-999999999999",
    );
  }, accountWriter);

  await page.route("https://connect.facebook.net/**", (route) => {
    capture.facebookSDKRequests += 1;
    const loginImplementation =
      options.metaLoginMode === "pending"
        ? "window.__embeddedSignupLoginCallback = callback;"
        : `
            window.dispatchEvent(new MessageEvent('message', {
              origin: 'https://www.facebook.com',
              data: {
                type: 'WA_EMBEDDED_SIGNUP',
                event: 'FINISH',
                data: {
                  phone_number_id: '1000000000000003',
                  waba_id: '1000000000000004'
                }
              }
            }));
            callback({ authResponse: { code: 'review-safe-code' } });
          `;

    return route.fulfill({
      contentType: "application/javascript",
      body: `
        window.FB = {
          init: function () {},
          login: function (callback) {
            ${loginImplementation}
          }
        };
      `,
    });
  });

  await page.route("**/api/**", (route) =>
    route.fulfill({ json: { data: {} } }),
  );
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: accountWriter } }),
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
  await page.route(/\/api\/me\/organizations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          organizations: options.exposeMultipleOrganizations
            ? [
                {
                  organization_id: organizationId,
                  name: "ReReply",
                  slug: "rereply",
                  role_name: "embedded-signup-writer",
                  is_default: true,
                },
                {
                  organization_id: otherOrganizationId,
                  name: "Other Clinic",
                  slug: "other-clinic",
                  role_name: "embedded-signup-writer",
                  is_default: false,
                },
              ]
            : [],
        },
      },
    }),
  );
  await page.route(/\/api\/embedded-signup\/config(?:\?.*)?$/, (route) => {
    capture.configOrganizationIds.push(
      route.request().headers()["x-organization-id"],
    );
    return route.fulfill({
      json: {
        data: {
          organization_id: options.configOrganizationId ?? organizationId,
          whatsapp_app_id: "1000000000000001",
          whatsapp_config_id: "1000000000000002",
          whatsapp_api_version: "v24.0",
          has_app_secret: true,
        },
      },
    });
  });
  await page.route(/\/api\/accounts(?:\?.*)?$/, (route) => {
    capture.accountOrganizationIds.push(
      route.request().headers()["x-organization-id"],
    );
    return route.fulfill({ json: { data: { accounts: [] } } });
  });
  await page.route(/\/api\/accounts\/exchange-token(?:\?.*)?$/, (route) => {
    capture.exchangeRequests += 1;
    capture.exchangeOrganizationIds.push(
      route.request().headers()["x-organization-id"],
    );
    return route.fulfill({
      json: {
        data: {
          account: {
            id: "b4444444-4444-4444-8444-444444444444",
            name: "Synthetic WhatsApp Test Number",
            status: "active",
          },
          warning: options.exchangeWarning,
          // Legacy servers returned this secret. The UI must ignore it even
          // if a stale response shape appears during a rolling deployment.
          pin: "483920",
        },
      },
    });
  });

  return capture;
}

async function startDirectCloudSignup(page: Page) {
  const connectButton = page
    .getByRole("button", { name: "Connect with Facebook" })
    .first();
  await expect(connectButton).toBeEnabled();
  await connectButton.click();
  await page.getByText("Direct Cloud API (Classic)", { exact: true }).click();
}

test("embedded signup never renders the generated two-step verification PIN in a toast", async ({
  page,
}) => {
  await installEmbeddedSignupMocks(page);
  await page.goto("/settings/accounts");

  await startDirectCloudSignup(page);

  const successToast = page.locator("[data-sonner-toast]").filter({
    hasText: "WhatsApp account connected successfully!",
  });
  await expect(successToast).toBeVisible();
  await expect(page.locator("[data-sonner-toast]")).not.toContainText("483920");
  await expect(page.getByText(/Your 2FA PIN/i)).toHaveCount(0);
});

test("an active embedded signup still surfaces a backend recovery warning", async ({
  page,
}) => {
  await installEmbeddedSignupMocks(page, {
    exchangeWarning:
      "Connection completed, but this account still needs operator attention.",
  });
  await page.goto("/settings/accounts");

  await startDirectCloudSignup(page);

  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "WhatsApp account connected successfully!",
    }),
  ).toBeVisible();
  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText:
        "Connection completed, but this account still needs operator attention.",
    }),
  ).toBeVisible();
});

test("pins config, account, and token exchange requests to the launch workspace", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page);
  await page.goto("/settings/accounts");

  await startDirectCloudSignup(page);
  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "WhatsApp account connected successfully!",
    }),
  ).toBeVisible();

  expect(capture.configOrganizationIds).not.toHaveLength(0);
  expect(capture.accountOrganizationIds).not.toHaveLength(0);
  expect(capture.exchangeOrganizationIds).toEqual([organizationId]);
  expect(capture.configOrganizationIds).toEqual(
    capture.configOrganizationIds.map(() => organizationId),
  );
  expect(capture.accountOrganizationIds).toEqual(
    capture.accountOrganizationIds.map(() => organizationId),
  );
  expect(
    await page.evaluate(() =>
      window.localStorage.getItem("selected_organization_id"),
    ),
  ).toBe(otherOrganizationId);
  expect(
    await page.evaluate(
      () =>
        (
          window as typeof window & {
            __observedXHRTimeouts?: number[];
          }
        ).__observedXHRTimeouts,
    ),
  ).toContain(90_000);
});

test("fails closed when the Embedded Signup config belongs to another workspace", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    configOrganizationId: otherOrganizationId,
  });
  await page.goto("/settings/accounts");

  await expect.poll(() => capture.configOrganizationIds.length).toBe(1);
  await expect(
    page.getByRole("button", { name: "Connect with Facebook" }),
  ).toHaveCount(0);
  expect(capture.facebookSDKRequests).toBe(0);
  expect(capture.exchangeRequests).toBe(0);
});

test("operator cancellation detaches pending Meta callbacks and never exchanges a code", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "pending",
  });
  await page.goto("/settings/accounts");
  await startDirectCloudSignup(page);

  const cancelButton = page.getByRole("button", { name: "Cancel connection" });
  await expect(cancelButton).toBeVisible();
  await cancelButton.click();

  await page.evaluate(() => {
    const loginCallback = (
      window as typeof window & {
        __embeddedSignupLoginCallback?: (response: unknown) => void;
      }
    ).__embeddedSignupLoginCallback;
    loginCallback?.({ authResponse: { code: "too-late-code" } });
    window.dispatchEvent(
      new MessageEvent("message", {
        origin: "https://www.facebook.com",
        data: {
          type: "WA_EMBEDDED_SIGNUP",
          event: "FINISH",
          data: {
            phone_number_id: "too-late-phone",
            waba_id: "too-late-waba",
          },
        },
      }),
    );
  });

  await page.waitForTimeout(100);
  expect(capture.exchangeRequests).toBe(0);
  await expect(cancelButton).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Connect with Facebook" }).first(),
  ).toBeEnabled();
});

test("blocks workspace switching and route navigation while Meta signup is pending", async ({
  page,
}) => {
  await installEmbeddedSignupMocks(page, {
    metaLoginMode: "pending",
    exposeMultipleOrganizations: true,
  });
  await page.goto("/settings/accounts");

  const workspaceSwitcher = page.locator("aside").getByRole("combobox").first();
  await expect(workspaceSwitcher).toBeEnabled();
  await startDirectCloudSignup(page);

  await expect(workspaceSwitcher).toBeDisabled();
  await page.getByRole("link", { name: "Add Account" }).first().click();
  await expect(page).toHaveURL(/\/settings\/accounts$/);
  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText:
        "Finish or cancel the WhatsApp connection before switching workspaces.",
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Cancel connection" }),
  ).toBeVisible();
});
