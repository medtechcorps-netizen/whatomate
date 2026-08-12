import { expect, test, type Page } from "@playwright/test";

const organizationId = "a1111111-1111-4111-8111-111111111111";
const templateId = "a2222222-2222-4222-8222-222222222222";

const templateRecord = {
  id: templateId,
  whatsapp_account: "Medtech Bot Test Number",
  meta_template_id: "",
  name: "review_utility_template",
  display_name: "Review Utility Template",
  language: "en",
  category: "UTILITY",
  status: "DRAFT",
  header_type: "NONE",
  header_content: "",
  body_content: "Your review-safe test message is ready.",
  footer_content: "",
  buttons: [],
  sample_values: [],
  quality_rating: "UNKNOWN",
  created_at: "2026-08-12T00:00:00Z",
  updated_at: "2026-08-12T00:00:00Z",
};

const accountRecord = {
  id: "a3333333-3333-4333-8333-333333333333",
  name: "Medtech Bot Test Number",
  phone_id: "1000000000000003",
};

function templateUser(actions: string[]) {
  return {
    id: "a4444444-4444-4444-8444-444444444444",
    email: `template-${actions.join("-")}@example.test`,
    full_name: `Template ${actions.join(" ")}`,
    organization_id: organizationId,
    organization_name: "ReReply",
    is_super_admin: false,
    is_reseller_admin: false,
    role: {
      id: "a5555555-5555-4555-8555-555555555555",
      name: `template-${actions.join("-")}`,
      description: "Focused template permissions",
      is_system: false,
      permissions: actions.map((action) => ({
        id: `templates-${action}`,
        resource: "templates",
        action,
      })),
    },
  };
}

async function installTemplateMocks(page: Page, actions: string[]) {
  const user = templateUser(actions);
  const mutatingRequests: string[] = [];

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
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
    route.fulfill({ json: { data: { accounts: [accountRecord] } } }),
  );
  await page.route(/\/api\/templates(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          templates: [templateRecord],
          total: 1,
          page: 1,
          limit: 20,
        },
      },
    }),
  );
  await page.route(
    new RegExp(`/api/templates/${templateId}(?:\\?.*)?$`),
    (route) => route.fulfill({ json: { data: templateRecord } }),
  );

  return mutatingRequests;
}

test("template readers cannot see create, publish, sync, or delete controls", async ({
  page,
}) => {
  const mutatingRequests = await installTemplateMocks(page, ["read"]);

  await page.goto("/templates");
  await expect(
    page.getByRole("heading", { name: "Message Templates" }),
  ).toBeVisible();
  await expect(
    page.getByText("Review Utility Template", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Create Template" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Sync from Meta" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);

  await page.goto(`/templates/${templateId}`);
  await expect(
    page.getByRole("heading", { name: "Review Utility Template" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: /Publish|Republish/ }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);
  expect(mutatingRequests).toEqual([]);
});

test("template writers see create and publish but not sync or delete controls", async ({
  page,
}) => {
  await installTemplateMocks(page, ["read", "write"]);

  await page.goto("/templates");
  await expect(
    page.getByRole("button", { name: "Create Template" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Sync from Meta" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);

  await page.goto(`/templates/${templateId}`);
  await expect(
    page.getByRole("button", { name: "Publish", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);
});

test("template sync and delete controls use their dedicated permissions", async ({
  page,
}) => {
  await installTemplateMocks(page, ["read", "sync"]);

  await page.goto("/templates");
  await expect(
    page.getByRole("button", { name: "Sync from Meta" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Create Template" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toHaveCount(0);

  await installTemplateMocks(page, ["read", "delete"]);
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Delete", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Sync from Meta" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Create Template" }),
  ).toHaveCount(0);
});
