import { expect, test, type Page } from "@playwright/test";

const organizationId = "b1111111-1111-4111-8111-111111111111";

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

async function installEmbeddedSignupMocks(page: Page) {
  await page.addInitScript((user) => {
    window.localStorage.setItem("user", JSON.stringify(user));
    window.localStorage.setItem("locale", "en");
  }, accountWriter);

  await page.route("https://connect.facebook.net/**", (route) =>
    route.fulfill({
      contentType: "application/javascript",
      body: `
        window.FB = {
          init: function () {},
          login: function (callback) {
            callback({
              authResponse: {
                code: 'review-safe-code',
                phone_number_id: '1000000000000003',
                waba_id: '1000000000000004'
              }
            });
          }
        };
      `,
    }),
  );

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
  await page.route(/\/api\/embedded-signup\/config(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          whatsapp_app_id: "1000000000000001",
          whatsapp_config_id: "1000000000000002",
          whatsapp_api_version: "v24.0",
          has_app_secret: true,
        },
      },
    }),
  );
  await page.route(/\/api\/accounts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { accounts: [] } } }),
  );
  await page.route(/\/api\/accounts\/exchange-token(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          account: {
            id: "b4444444-4444-4444-8444-444444444444",
            name: "Medtech Bot Test Number",
            status: "active",
          },
          pin: "483920",
        },
      },
    }),
  );
}

test("embedded signup never renders the generated two-step verification PIN in a toast", async ({
  page,
}) => {
  await installEmbeddedSignupMocks(page);
  await page.goto("/settings/accounts");

  const connectButton = page
    .getByRole("button", { name: "Connect with Facebook" })
    .first();
  await expect(connectButton).toBeEnabled();
  await connectButton.click();
  await page.getByText("Direct Cloud API (Classic)", { exact: true }).click();

  const successToast = page.locator("[data-sonner-toast]").filter({
    hasText:
      "WhatsApp account connected and two-step verification configured successfully!",
  });
  await expect(successToast).toBeVisible();
  await expect(page.locator("[data-sonner-toast]")).not.toContainText("483920");
  await expect(page.getByText(/Your 2FA PIN/i)).toHaveCount(0);
});
