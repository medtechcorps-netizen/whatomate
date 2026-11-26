import { expect, test, type BrowserContext, type Page } from "@playwright/test";

const workspaceA = { id: "2c111111-1111-4111-8111-111111111111", name: "Synthetic workspace A" };
const workspaceB = { id: "2c222222-2222-4222-8222-222222222222", name: "Synthetic workspace B" };
const sessionMarker = "synthetic-template-sync-session";
const csrf = "synthetic-template-sync-csrf";
const syncPath = "/api/templates/sync";
const syncAccountsPath = "/api/templates/sync/accounts";
const refreshPath = "/api/auth/refresh";
const selectedKey = "selected_organization_id";
const sessionKey = "whm_session_id";
const epochKey = "whm_auth_epoch";
const templateLabel = (scope: string) =>
  scope === workspaceA.id ? "Workspace A template" : "Workspace B template";

async function mockApi(
  context: BrowserContext,
  page: Page,
  baseURL: string | undefined,
  scenario: { switcher?: boolean; holdRefresh?: Promise<void>; allowReplay?: boolean } = {},
) {
  if (!baseURL) throw new Error("The template sync fixture needs an owned frontend");
  const origin = new URL(baseURL).origin;
  const viewer = {
    id: "2c333333-3333-4333-8333-333333333333",
    email: "template-affinity@example.test",
    full_name: "Synthetic template reviewer",
    organization_id: workspaceA.id,
    organization_name: workspaceA.name,
    is_super_admin: scenario.switcher === true,
    is_reseller_admin: false,
    role: {
      id: "2c444444-4444-4444-8444-444444444444",
      name: "template-reviewer",
      is_system: false,
      permissions: [
        { id: "templates-read", resource: "templates", action: "read" },
        { id: "templates-sync", resource: "templates", action: "sync" },
      ],
    },
  };
  const accountReads: Array<{ path: string; organization: string | undefined; primary: boolean }> = [];
  const templateReads: Array<{
    organization: string | undefined;
    account: string | null;
    primary: boolean;
  }> = [];
  const syncs: Array<{
    organization: string | undefined;
    csrf: string | undefined;
    body: unknown;
  }> = [];
  const refreshes: Array<string | undefined> = [];
  const unexpected: string[] = [];

  await context.addCookies([{ name: "whm_csrf", value: csrf, url: origin }]);
  await context.addInitScript(
    ({ viewer, sessionKey, sessionMarker }) => {
      if (!localStorage.getItem("user")) {
        localStorage.setItem("user", JSON.stringify(viewer));
      }
      if (!localStorage.getItem(sessionKey)) {
        localStorage.setItem(sessionKey, sessionMarker);
      }
      localStorage.setItem("locale", "en");
    },
    { viewer, sessionKey, sessionMarker },
  );
  await context.routeWebSocket("**", (socket) => socket.close());
  await context.route("**/*", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== origin) return route.abort("blockedbyclient");
    if (!url.pathname.startsWith("/api/")) return route.continue();
    const method = request.method();
    const headers = request.headers();
    const organization = headers["x-organization-id"];

    if (method === "POST" && url.pathname === syncPath) {
      syncs.push({
        organization,
        csrf: headers["x-csrf-token"],
        body: request.postDataJSON(),
      });
      if (syncs.length !== 1 && !(scenario.allowReplay && syncs.length === 2)) {
        unexpected.push("Replayed template sync");
        return route.fulfill({ status: 409, json: { message: "Unexpected replay" } });
      }
      if (scenario.holdRefresh && syncs.length === 1) {
        return route.fulfill({ status: 401, json: { message: "Synthetic access expiry" } });
      }
      return route.fulfill({ json: { data: { count: 1 } } });
    }

    if (method === "POST" && url.pathname === refreshPath && scenario.holdRefresh) {
      refreshes.push(headers["x-csrf-token"]);
      await scenario.holdRefresh;
      return route.fulfill({ json: { data: { expires_in: 3600 } } });
    }

    if (method !== "GET") {
      unexpected.push(method + " " + url.pathname);
      return route.abort("blockedbyclient");
    }

    const accountRead = url.pathname === "/api/accounts" || url.pathname === syncAccountsPath;
    if (accountRead || url.pathname === "/api/templates") {
      const primary = request.frame().page() === page;
      if (accountRead) accountReads.push({ path: url.pathname, organization, primary });
      else templateReads.push({ organization, account: url.searchParams.get("account"), primary });
      if (organization !== workspaceA.id && organization !== workspaceB.id) {
        return route.fulfill({ status: 403, json: { message: "Explicit workspace required" } });
      }

      if (accountRead) {
        // Only the synthetic super-admin can use the full management endpoint.
        if (url.pathname === "/api/accounts" && !viewer.is_super_admin) {
          return route.fulfill({ status: 403, json: { message: "Insufficient permissions" } });
        }
        const accounts = organization === workspaceA.id
          ? [
              { id: "a-all", name: "all" },
              { id: "a-other", name: "Alternate A account" },
            ]
          : [{ id: "b-other", name: "Alternate B account" }];
        return route.fulfill({ json: { data: { accounts } } });
      }
      return route.fulfill({
        json: {
          data: {
            templates: [{
              id: organization + "-template",
              name: organization === workspaceA.id ? "workspace_a_template" : "workspace_b_template",
              display_name: templateLabel(organization),
              whatsapp_account: organization === workspaceA.id ? "all" : "Alternate B account",
              status: "APPROVED",
              language: "en",
              category: "UTILITY",
            }],
            total: 1,
          },
        },
      });
    }

    switch (url.pathname) {
      case "/api/me":
        return route.fulfill({ json: { data: viewer } });
      case "/api/product/entitlements":
        return route.fulfill({ json: { data: { mode: "licensed", entitlements: {} } } });
      case "/api/auth/ws-token":
        return route.fulfill({ json: { data: { token: "" } } });
      case "/api/me/organizations":
        return route.fulfill({ json: { data: { organizations: [] } } });
      case "/api/organizations":
        return route.fulfill({ json: { data: { organizations: [workspaceA, workspaceB] } } });
      default:
        return route.fulfill({ json: { data: {} } });
    }
  });
  return { accountReads, templateReads, syncs, refreshes, unexpected };
}

const syncButton = (page: Page) =>
  page.locator("header").getByRole("button", { name: "Sync from Meta", exact: true });
const filter = (page: Page) =>
  page.locator('button[role="combobox"]:not([data-testid="organization-switcher"])');
const result = (page: Page) => page.getByLabel("Template sync results");
const expectedSync = {
  organization: workspaceA.id,
  csrf,
  body: { whatsapp_account: "all" },
};

async function openTemplates(page: Page) {
  await page.goto("/templates", { waitUntil: "domcontentloaded" });
  await expect(page.getByRole("heading", { name: "Message Templates", exact: true })).toBeVisible();
}

async function selectLiteralAll(page: Page) {
  await filter(page).click();
  await page.getByRole("option", { name: "all", exact: true }).click();
  await expect(filter(page)).toHaveText("all");
  await expect(syncButton(page)).toBeEnabled();
}

test("syncs only the selected literal all account under its explicit workspace", async ({
  page, context, baseURL,
}) => {
  const wire = await mockApi(context, page, baseURL);
  await openTemplates(page);
  await expect(page.getByText("Workspace A template", { exact: true })).toBeVisible();
  await expect(filter(page)).toContainText("All Accounts");
  await expect(syncButton(page)).toBeDisabled();
  expect(wire.accountReads).toEqual([{ path: syncAccountsPath, organization: workspaceA.id, primary: true }]);
  expect(wire.templateReads).toEqual([
    { organization: workspaceA.id, account: null, primary: true },
  ]);
  expect(wire.syncs).toEqual([]);

  await selectLiteralAll(page);
  await expect.poll(() => wire.templateReads.at(-1)).toEqual({
    organization: workspaceA.id, account: "all", primary: true,
  });
  // No ambient storage header can account for these explicit A headers.
  expect(await page.evaluate((key) => localStorage.getItem(key), selectedKey)).toBeNull();
  expect(wire.syncs).toEqual([]);
  await syncButton(page).click();
  await expect(result(page)).toContainText("all: Synced");
  expect(wire.syncs).toEqual([expectedSync]);
  expect(wire.refreshes).toEqual([]);
  expect(wire.unexpected).toEqual([]);
});

test("replays one expired sync when the selected workspace stays current", async ({
  page, context, baseURL,
}) => {
  const wire = await mockApi(context, page, baseURL, {
    holdRefresh: Promise.resolve(),
    allowReplay: true,
  });
  await openTemplates(page);
  await expect(page.getByText("Workspace A template", { exact: true })).toBeVisible();
  await selectLiteralAll(page);

  await syncButton(page).click();
  await expect(result(page)).toContainText("all: Synced");
  expect(wire.syncs).toEqual([expectedSync, expectedSync]);
  expect(wire.refreshes).toEqual([csrf]);
  expect(wire.unexpected).toEqual([]);
});

test("a second tab's workspace switch blocks replay after an actual Axios 401 refresh", async ({
  page, context, baseURL,
}) => {
  let releaseRefresh!: () => void;
  const refreshGate = new Promise<void>((resolve) => { releaseRefresh = resolve; });
  const wire = await mockApi(context, page, baseURL, {
    switcher: true,
    holdRefresh: refreshGate,
  });
  const secondTab = await context.newPage();

  try {
    await openTemplates(page);
    await expect(page.getByTestId("organization-switcher")).toContainText(workspaceA.name);
    await expect(page.getByText("Workspace A template", { exact: true })).toBeVisible();
    await selectLiteralAll(page);
    await openTemplates(secondTab);
    await expect(secondTab.getByTestId("organization-switcher")).toContainText(workspaceA.name);

    const beforeEpoch = await page.evaluate((key) => Number(localStorage.getItem(key)), epochKey);
    await syncButton(page).click();
    await expect.poll(() => wire.refreshes).toEqual([csrf]);
    expect(wire.syncs).toEqual([expectedSync]);
    await expect(syncButton(page)).toBeDisabled();

    // The real super-admin switcher persists B and reloads only this second tab.
    await secondTab.getByTestId("organization-switcher").click();
    await secondTab.getByRole("option", { name: workspaceB.name, exact: true }).click();
    await expect(secondTab.getByTestId("organization-switcher")).toContainText(workspaceB.name);
    await expect(secondTab.getByText("Workspace B template", { exact: true })).toBeVisible();
    await expect(page.getByText("Workspace A template", { exact: true })).toHaveCount(0);
    await expect(syncButton(page)).toBeDisabled();
    expect(await page.evaluate((key) => localStorage.getItem(key), selectedKey))
      .toBe(workspaceB.id);
    expect(await page.evaluate((key) => localStorage.getItem(key), sessionKey))
      .toBe(sessionMarker);
    const firstTabReads = wire.templateReads.filter((entry) => entry.primary).length;

    releaseRefresh();
    // The real refresh advances the epoch while retaining the same logical session.
    await expect.poll(() => page.evaluate((key) => Number(localStorage.getItem(key)), epochKey))
      .toBeGreaterThan(beforeEpoch);
    await page.evaluate(async () => {
      // Queue behind the same refresh lock, then drain the renderer's continuations
      // before treating absence of a second wire request as an observation.
      await navigator.locks.request("whm-token-refresh", async () => {});
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    });

    expect(wire.syncs).toEqual([expectedSync]);
    expect(wire.refreshes).toEqual([csrf]);
    expect(wire.templateReads.filter((entry) => entry.primary)).toHaveLength(firstTabReads);
    expect(wire.unexpected).toEqual([]);
    await expect(page.getByRole("heading", { name: "Message Templates", exact: true })).toBeVisible();
    await expect(result(page)).toHaveCount(0);
    await expect(syncButton(page)).toBeDisabled();
  } finally {
    releaseRefresh();
    await secondTab.close();
  }
});
