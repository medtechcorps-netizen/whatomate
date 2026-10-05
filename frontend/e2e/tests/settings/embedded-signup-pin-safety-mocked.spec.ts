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

type MetaLoginMode =
  | "complete"
  | "pending"
  | "wrong_coexistence_signals"
  | "coexistence_waba_only";

interface EmbeddedSignupMockOptions {
  accounts?: Array<Record<string, unknown>>;
  configOrganizationId?: string;
  coexistenceRetryWarning?: string;
  exchangeWarning?: string;
  exchangeDelayMs?: number;
  exchangeErrorMessage?: string;
  exchangeXHRTimeoutMs?: number;
  metaLoginMode?: MetaLoginMode;
  exposeMultipleOrganizations?: boolean;
}

interface EmbeddedSignupRequestCapture {
  accountOrganizationIds: Array<string | undefined>;
  configOrganizationIds: Array<string | undefined>;
  exchangeOrganizationIds: Array<string | undefined>;
  exchangeBodies: Array<Record<string, unknown>>;
  exchangeRequests: number;
  exchangeRoutesSettled: number;
  facebookSDKRequests: number;
  coexistenceRetryOrganizationIds: Array<string | undefined>;
  coexistenceRetryRequests: number;
}

async function installEmbeddedSignupMocks(
  page: Page,
  options: EmbeddedSignupMockOptions = {},
) {
  const capture: EmbeddedSignupRequestCapture = {
    accountOrganizationIds: [],
    configOrganizationIds: [],
    exchangeOrganizationIds: [],
    exchangeBodies: [],
    exchangeRequests: 0,
    exchangeRoutesSettled: 0,
    facebookSDKRequests: 0,
    coexistenceRetryOrganizationIds: [],
    coexistenceRetryRequests: 0,
  };

  await page.addInitScript(
    ({ user, exchangeXHRTimeoutMs }) => {
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
            timeoutDescriptor.set?.call(
              this,
              value === 90_000 && exchangeXHRTimeoutMs !== undefined
                ? exchangeXHRTimeoutMs
                : value,
            );
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
    },
    { user: accountWriter, exchangeXHRTimeoutMs: options.exchangeXHRTimeoutMs },
  );

  await page.route("https://connect.facebook.net/**", (route) => {
    capture.facebookSDKRequests += 1;
    let loginImplementation: string;
    if (options.metaLoginMode === "pending") {
      loginImplementation = "window.__embeddedSignupLoginCallback = callback;";
    } else if (options.metaLoginMode === "coexistence_waba_only") {
      // Meta's documented Coexistence completion names only the WABA.
      loginImplementation = `
            window.dispatchEvent(new MessageEvent('message', {
              origin: 'https://www.facebook.com',
              data: {
                type: 'WA_EMBEDDED_SIGNUP',
                event: 'FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING',
                version: 3,
                data: { waba_id: '1000000000000004' }
              }
            }));
            callback({ authResponse: { code: 'review-safe-code' } });
          `;
    } else if (options.metaLoginMode === "wrong_coexistence_signals") {
      loginImplementation = `
            window.dispatchEvent(new MessageEvent('message', {
              origin: 'https://www.facebook.com',
              data: {
                type: 'WA_EMBEDDED_SIGNUP',
                event: 'FINISH',
                version: 2,
                data: {
                  phone_number_id: 'wrong-mode-phone',
                  waba_id: 'wrong-mode-waba'
                }
              }
            }));
            window.dispatchEvent(new MessageEvent('message', {
              origin: 'https://www.facebook.com',
              data: {
                type: 'WA_EMBEDDED_SIGNUP',
                event: 'FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING',
                version: 2,
                data: { waba_id: 'wrong-version-waba' }
              }
            }));
            callback({ authResponse: { code: 'review-safe-code' } });
          `;
    } else {
      loginImplementation = `
            window.dispatchEvent(new MessageEvent('message', {
              origin: 'https://www.facebook.com',
              data: {
                type: 'WA_EMBEDDED_SIGNUP',
                event: loginOptions?.extras?.featureType === 'whatsapp_business_app_onboarding'
                  ? 'FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING'
                  : 'FINISH',
                version: loginOptions?.extras?.featureType === 'whatsapp_business_app_onboarding'
                  ? 3
                  : undefined,
                data: {
                  phone_number_id: '1000000000000003',
                  waba_id: '1000000000000004'
                }
              }
            }));
            callback({ authResponse: { code: 'review-safe-code' } });
          `;
    }

    return route.fulfill({
      contentType: "application/javascript",
      body: `
        window.FB = {
          init: function () {},
          login: function (callback, loginOptions) {
            window.__embeddedSignupLoginOptions = loginOptions;
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
    return route.fulfill({
      json: { data: { accounts: options.accounts ?? [] } },
    });
  });
  await page.route(
    /\/api\/accounts\/[^/?]+\/coexistence-sync(?:\?.*)?$/,
    (route) => {
      capture.coexistenceRetryRequests += 1;
      capture.coexistenceRetryOrganizationIds.push(
        route.request().headers()["x-organization-id"],
      );
      return route.fulfill({
        json: {
          data: {
            success: true,
            coexistence: {
              onboarding_status: "syncing",
              sync_status: "in_progress",
              contact_sync_status: "requested",
              history_consent: "unknown",
              history_sync_status: "requested",
              lifecycle_status: "connected",
              history_progress_percent: 0,
              updated_at: "2026-09-04T01:05:00Z",
            },
            warning: options.coexistenceRetryWarning,
          },
        },
      });
    },
  );
  await page.route(
    /\/api\/accounts\/exchange-token(?:\?.*)?$/,
    async (route) => {
      capture.exchangeRequests += 1;
      capture.exchangeBodies.push(
        route.request().postDataJSON() as Record<string, unknown>,
      );
      capture.exchangeOrganizationIds.push(
        route.request().headers()["x-organization-id"],
      );
      if (options.exchangeDelayMs) {
        await new Promise((resolve) =>
          setTimeout(resolve, options.exchangeDelayMs),
        );
      }
      if (options.exchangeErrorMessage) {
        capture.exchangeRoutesSettled += 1;
        return route.fulfill({
          status: 400,
          json: { status: "error", message: options.exchangeErrorMessage },
        });
      }
      try {
        await route.fulfill({
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
      } catch (error) {
        // A deliberate browser timeout can abort the intercepted response.
        if (options.exchangeXHRTimeoutMs === undefined) throw error;
      } finally {
        capture.exchangeRoutesSettled += 1;
      }
    },
  );

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

async function openConnectionMethodDialog(page: Page) {
  const connectButton = page
    .getByRole("button", { name: "Connect with Facebook" })
    .first();
  await expect(connectButton).toBeEnabled();
  await connectButton.click();
}

// Synthetic number. Meta's Coexistence completion names only the WABA, so the
// operator enters the number being connected before Meta's login opens.
const syntheticCoexistenceNumber = "+60 12-345 6789";

async function continueCoexistenceSignup(
  page: Page,
  phoneNumber = syntheticCoexistenceNumber,
) {
  await page.getByRole("button", { name: /Sync with Mobile App/i }).click();
  await page
    .getByLabel("WhatsApp Business app number", { exact: true })
    .fill(phoneNumber);
  await page.getByRole("button", { name: "Continue with Facebook" }).click();
}

async function metaLoginOptions(page: Page) {
  return page.evaluate(
    () =>
      (
        window as typeof window & {
          __embeddedSignupLoginOptions?: Record<string, unknown>;
        }
      ).__embeddedSignupLoginOptions,
  );
}

test("launches Coexistence with Meta's Business App onboarding contract", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page);
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);

  const coexistenceOption = page.getByRole("button", {
    name: /Sync with Mobile App/i,
  });
  await expect(coexistenceOption).toContainText(
    "WhatsApp Business app 2.24.17 or newer is required.",
  );
  await expect(coexistenceOption).toContainText(
    "You choose in Meta whether to share up to 6 months of 1-to-1 chat history.",
  );
  await expect(coexistenceOption).toContainText(
    "Group chats do not sync. Cloud API traffic is limited to 20 messages/second.",
  );
  await expect(coexistenceOption).toContainText(
    "Up to 4 companion devices can remain linked",
  );

  // A real button makes the full-card choice keyboard operable.
  await coexistenceOption.press("Enter");
  const phoneNumber = page.getByLabel("WhatsApp Business app number", {
    exact: true,
  });
  await expect(phoneNumber).toBeVisible();
  expect(await metaLoginOptions(page)).toBeUndefined();
  await phoneNumber.fill(syntheticCoexistenceNumber);
  // Submitting with Enter keeps Meta's login inside the user gesture.
  await phoneNumber.press("Enter");
  await expect.poll(() => capture.exchangeRequests).toBe(1);

  const loginOptions = await metaLoginOptions(page);
  expect(loginOptions).toEqual({
    config_id: "1000000000000002",
    response_type: "code",
    override_default_response_type: true,
    extras: {
      setup: {},
      featureType: "whatsapp_business_app_onboarding",
      sessionInfoVersion: "3",
    },
  });
  expect(capture.exchangeOrganizationIds).toEqual([organizationId]);
  // Meta named the phone, so the entered number is not sent.
  expect(capture.exchangeBodies).toEqual([
    {
      code: "review-safe-code",
      signup_mode: "coexistence",
      phone_id: "1000000000000003",
      waba_id: "1000000000000004",
    },
  ]);
});

test("Coexistence sends the entered number's digits when Meta names only the WABA", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "coexistence_waba_only",
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await continueCoexistenceSignup(page);

  await expect.poll(() => capture.exchangeRequests).toBe(1);
  expect(capture.exchangeOrganizationIds).toEqual([organizationId]);
  expect(capture.exchangeBodies).toEqual([
    {
      code: "review-safe-code",
      signup_mode: "coexistence",
      waba_id: "1000000000000004",
      phone_number_hint: "60123456789",
    },
  ]);
  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "WhatsApp account connected successfully!",
    }),
  ).toBeVisible();
});

test("Coexistence refuses a number without its country code before Meta's login", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "coexistence_waba_only",
    // The server refuses a number that is not listed under the WABA and names
    // at most its last four digits.
    exchangeErrorMessage:
      "the number ending in 6789 is not listed in the selected WhatsApp Business Account; check the number and its country code, then restart Sync with Mobile App",
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await page.getByRole("button", { name: /Sync with Mobile App/i }).click();

  const phoneNumber = page.getByLabel("WhatsApp Business app number", {
    exact: true,
  });
  const continueButton = page.getByRole("button", {
    name: "Continue with Facebook",
  });
  await continueButton.click();
  await expect(
    page.getByText("Enter the number you are connecting."),
  ).toBeVisible();
  await phoneNumber.fill("012-345 6789");
  await continueButton.click();
  await expect(
    page.getByText(
      "Start with the country code instead of 0, for example +60 12-345 6789.",
    ),
  ).toBeVisible();
  await expect(phoneNumber).toHaveAttribute("aria-invalid", "true");
  await expect(phoneNumber).toBeFocused();
  expect(await metaLoginOptions(page)).toBeUndefined();
  expect(capture.exchangeRequests).toBe(0);

  // Back returns to the mode choice; the dialog keeps the typed number.
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Back", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: /Direct Cloud API \(Classic\)/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: /Sync with Mobile App/i }).click();
  await expect(phoneNumber).toHaveValue("012-345 6789");
  await phoneNumber.fill(syntheticCoexistenceNumber);
  await continueButton.click();

  await expect.poll(() => capture.exchangeRequests).toBe(1);
  expect(capture.exchangeBodies[0]?.phone_number_hint).toBe("60123456789");
  const refusal = page
    .locator("[data-sonner-toast]")
    .filter({ hasText: "the number ending in 6789 is not listed" });
  await expect(refusal).toContainText(
    "Meta signup diagnostics: FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=3 waba_id=present waba_ids=absent phone_number_id=absent (before code); coexistence sent code+waba_id+phone_number_hint",
  );
  await expect(refusal).not.toContainText("12-345");
  await expect(refusal).not.toContainText("60123456789");
  await expect(refusal).not.toContainText("review-safe-code");
});

test("Coexistence reconnects a workspace account by its phone ID without a number", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "coexistence_waba_only",
    accounts: [
      {
        id: "b4444444-4444-4444-8444-444444444445",
        name: "Existing classic number",
        phone_id: "1000000000000003",
        business_id: "1000000000000004",
        api_version: "v21.0",
        status: "active",
        is_smb: false,
        has_access_token: true,
        created_at: "2026-09-04T01:00:00Z",
      },
    ],
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await page.getByRole("button", { name: /Sync with Mobile App/i }).click();

  const reconnect = page.getByLabel(
    "Or reconnect a number already in this workspace",
  );
  await reconnect.selectOption({
    label: "Existing classic number (Phone Number ID 1000000000000003)",
  });
  await expect(
    page.getByLabel("WhatsApp Business app number", { exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Continue with Facebook" }).click();

  await expect.poll(() => capture.exchangeRequests).toBe(1);
  expect(capture.exchangeBodies).toEqual([
    {
      code: "review-safe-code",
      signup_mode: "coexistence",
      phone_id: "1000000000000003",
      waba_id: "1000000000000004",
    },
  ]);
});

test("Coexistence never sends IDs from unsupported finish versions and falls back to a code-only exchange", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "wrong_coexistence_signals",
    // The server refuses a code-only exchange whose token grants several
    // WABAs. The UI adds only privacy-safe diagnostics to that refusal.
    exchangeErrorMessage:
      "embedded signup token grants access to multiple WhatsApp Business Accounts; reconnect and select exactly one account",
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await page.clock.install();
  await continueCoexistenceSignup(page);

  await page.clock.fastForward(5_001);
  expect(capture.exchangeRequests).toBe(0);
  await page.clock.fastForward(10_000);
  // The server accepts a code-only exchange only when the token grants
  // exactly one WABA and refuses ambiguity. The entered number still picks
  // the phone if the token's single WABA lists several.
  await expect.poll(() => capture.exchangeRequests).toBe(1);
  expect(capture.exchangeBodies).toEqual([
    {
      code: "review-safe-code",
      signup_mode: "coexistence",
      phone_number_hint: "60123456789",
    },
  ]);

  const refusal = page
    .locator("[data-sonner-toast]")
    .filter({ hasText: "multiple WhatsApp Business Accounts" });
  await expect(refusal).toContainText(
    "Meta signup diagnostics: " +
      "FINISH v=2 waba_id=present waba_ids=absent phone_number_id=present (before code); " +
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=2 waba_id=present waba_ids=absent phone_number_id=absent (before code); " +
      "coexistence sent code+phone_number_hint after a 15s wait",
  );
  await expect(refusal).not.toContainText("wrong-");
  await expect(refusal).not.toContainText("review-safe-code");
  await expect(refusal).not.toContainText("60123456789");
  // The diagnostic toast stays until the operator closes it.
  await page.clock.fastForward(60_000);
  await expect(refusal).toBeVisible();
});

test("Coexistence refuses an ambiguous Meta selection with privacy-safe diagnostics", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "pending",
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await continueCoexistenceSignup(page);
  await page.evaluate(() => {
    (
      window as typeof window & {
        __embeddedSignupLoginCallback?: (response: unknown) => void;
      }
    ).__embeddedSignupLoginCallback?.({
      authResponse: { code: "review-safe-code" },
    });
    window.dispatchEvent(
      new MessageEvent("message", {
        origin: "https://www.facebook.com",
        data: JSON.stringify({
          type: "WA_EMBEDDED_SIGNUP",
          event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
          version: 3,
          data: { waba_ids: ["1000000000000004", "1000000000000006"] },
        }),
      }),
    );
  });

  const refusal = page
    .locator("[data-sonner-toast]")
    .filter({ hasText: "more than one selected WhatsApp account" });
  await expect(refusal).toContainText(
    "Meta signup diagnostics: FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=3 waba_id=absent waba_ids=2 listed phone_number_id=absent (0s after code); coexistence sent nothing",
  );
  await expect(refusal).not.toContainText("1000000000000");
  await expect(refusal).not.toContainText("review-safe-code");
  expect(capture.exchangeRequests).toBe(0);
});

test("Coexistence preserves the selected account from a delayed standard finish without a version", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    metaLoginMode: "pending",
  });
  await page.goto("/settings/accounts");
  await openConnectionMethodDialog(page);
  await page.clock.install();
  await continueCoexistenceSignup(page);
  await page.evaluate(() => {
    const loginCallback = (
      window as typeof window & {
        __embeddedSignupLoginCallback?: (response: unknown) => void;
      }
    ).__embeddedSignupLoginCallback;
    loginCallback?.({ authResponse: { code: "review-safe-code" } });
  });
  await page.clock.fastForward(5_001);
  expect(capture.exchangeRequests).toBe(0);
  await page.evaluate(() => {
    window.dispatchEvent(
      new MessageEvent("message", {
        origin: "https://www.facebook.com",
        data: {
          type: "WA_EMBEDDED_SIGNUP",
          event: "FINISH",
          data: {
            waba_id: "1000000000000004",
            phone_number_id: "1000000000000003",
          },
        },
      }),
    );
  });
  await expect.poll(() => capture.exchangeRequests).toBe(1);
  expect(capture.exchangeOrganizationIds).toEqual([organizationId]);
  expect(capture.exchangeBodies).toEqual([
    {
      code: "review-safe-code",
      signup_mode: "coexistence",
      phone_id: "1000000000000003",
      waba_id: "1000000000000004",
    },
  ]);
});

test("renders lifecycle-safe Coexistence states and retries only an eligible sync", async ({
  page,
}) => {
  const baseAccount = {
    phone_id: "1000000000000003",
    business_id: "1000000000000004",
    api_version: "v24.0",
    is_default_incoming: false,
    is_default_outgoing: false,
    status: "active",
    is_smb: true,
    has_access_token: true,
    created_at: "2026-09-04T01:00:00Z",
  };
  const capture = await installEmbeddedSignupMocks(page, {
    accounts: [
      {
        ...baseAccount,
        id: "b4444444-4444-4444-8444-444444444441",
        name: "Clinic retry",
        coexistence: {
          onboarding_status: "failed",
          sync_status: "failed",
          contact_sync_status: "failed",
          history_consent: "unknown",
          history_sync_status: "requested",
          lifecycle_status: "connected",
          history_progress_percent: 0,
          sync_deadline_at: "2099-09-05T01:00:00Z",
          updated_at: "2026-09-04T01:05:00Z",
        },
      },
      {
        ...baseAccount,
        id: "b4444444-4444-4444-8444-444444444442",
        name: "Clinic disconnected",
        // ACCOUNT_RECONNECTED updates lifecycle state, but the account remains
        // disconnected until a fresh Embedded Signup credential exchange.
        status: "disconnected",
        coexistence: {
          onboarding_status: "ready",
          sync_status: "completed",
          contact_sync_status: "completed",
          history_consent: "granted",
          history_sync_status: "completed",
          lifecycle_status: "connected",
          history_progress_percent: 100,
          updated_at: "2026-09-04T01:05:00Z",
        },
      },
      {
        ...baseAccount,
        id: "b4444444-4444-4444-8444-444444444443",
        name: "Clinic expired",
        coexistence: {
          onboarding_status: "syncing",
          sync_status: "pending",
          contact_sync_status: "pending",
          history_consent: "unknown",
          history_sync_status: "pending",
          lifecycle_status: "connected",
          history_progress_percent: 0,
          sync_deadline_at: "2020-09-05T01:00:00Z",
          updated_at: "2026-09-04T01:05:00Z",
        },
      },
    ],
  });
  await page.goto("/settings/accounts");

  await expect(page.getByText("Mobile sync needs attention")).toBeVisible();
  await expect(page.getByText("Reconnect required")).toBeVisible();
  await expect(page.getByText("Initial sync window expired")).toBeVisible();
  await expect(page.getByText("Mobile sync complete")).toHaveCount(0);

  const retryButton = page.getByRole("button", {
    name: "Retry mobile-app sync for Clinic retry",
  });
  await expect(retryButton).toBeEnabled();
  await expect(
    page.getByRole("button", {
      name: "Retry mobile-app sync for Clinic disconnected",
    }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", {
      name: "Retry mobile-app sync for Clinic expired",
    }),
  ).toHaveCount(0);

  await retryButton.click();
  await expect.poll(() => capture.coexistenceRetryRequests).toBe(1);
  expect(capture.coexistenceRetryOrganizationIds).toEqual([organizationId]);
  await expect(
    page.locator("[data-sonner-toast]").filter({
      hasText: "Initial mobile-app sync request sent to Meta.",
    }),
  ).toBeVisible();
});

test("embedded signup never renders the generated two-step verification PIN in a toast", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page);
  await page.goto("/settings/accounts");

  await startDirectCloudSignup(page);

  const successToast = page.locator("[data-sonner-toast]").filter({
    hasText: "WhatsApp account connected successfully!",
  });
  await expect(successToast).toBeVisible();
  await expect(page.locator("[data-sonner-toast]")).not.toContainText("483920");
  await expect(page.getByText(/Your 2FA PIN/i)).toHaveCount(0);
  expect(capture.exchangeBodies[0]?.signup_mode).toBe("classic");
  expect(capture.exchangeBodies[0]).not.toHaveProperty("phone_number_hint");
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

test("waits for delayed signup settlement without replaying the exchange", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    exchangeDelayMs: 250,
  });
  await page.goto("/settings/accounts");
  await startDirectCloudSignup(page);
  await expect.poll(() => capture.exchangeRequests).toBe(1);
  await expect(
    page
      .locator("[data-sonner-toast]")
      .filter({ hasText: "WhatsApp account connected successfully!" }),
  ).toBeVisible();
  expect(capture.exchangeRequests).toBe(1);
  expect(capture.exchangeRoutesSettled).toBe(1);
});

test("a client timeout refreshes durable accounts and warns against replay", async ({
  page,
}) => {
  const capture = await installEmbeddedSignupMocks(page, {
    exchangeDelayMs: 300,
    // Exercise the browser's actual XHR timeout without waiting 90 seconds.
    exchangeXHRTimeoutMs: 25,
    accounts: [
      {
        id: "b4444444-4444-4444-8444-444444444444",
        name: "Pending connection for reconciliation",
        status: "pending_registration",
        phone_id: "1000000000000003",
        // The accounts API always returns created_at, and the row renders it.
        created_at: "2026-09-04T01:00:00Z",
      },
    ],
  });
  await page.goto("/settings/accounts");
  await expect.poll(() => capture.accountOrganizationIds.length).toBe(1);
  await startDirectCloudSignup(page);
  await expect(
    page
      .locator("[data-sonner-toast]")
      .filter({ hasText: "Connection result could not be confirmed." }),
  ).toBeVisible();
  await expect.poll(() => capture.accountOrganizationIds.length).toBe(2);
  await expect.poll(() => capture.exchangeRoutesSettled).toBe(1);
  expect(capture.exchangeRequests).toBe(1);
  expect(capture.accountOrganizationIds).toEqual([
    organizationId,
    organizationId,
  ]);
  await expect(
    page
      .locator("[data-sonner-toast]")
      .filter({ hasText: "WhatsApp account connected successfully!" }),
  ).toHaveCount(0);
  await expect(
    page.getByText("Pending connection for reconciliation", { exact: true }),
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
