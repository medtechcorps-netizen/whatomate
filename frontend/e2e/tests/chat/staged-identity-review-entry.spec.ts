import { expect, test, type Page } from "@playwright/test";

// A Coexistence number connected to an empty workspace used to hold its first
// messages contact-free with no way to see them: the protected staged queue
// was reachable only from an existing contact's Identity review dialog. The
// workspace-level notice must surface the count and open the queue in Chat,
// the Omnichannel Inbox and Contacts, using only the protected endpoints.

const organizationId = "e1111111-1111-4111-8111-111111111111";
const stagedId = "e2222222-2222-4222-8222-222222222222";
const holdId = "e3333333-3333-4333-8333-333333333333";

// A workspace admin as real admins sign in: not a super admin, so the notice
// is shown through the same role-permission check the server enforces
// (contacts:write + contacts.identity_review:write).
const reviewer = {
  id: "e4444444-4444-4444-8444-444444444444",
  email: "staged-reviewer@example.test",
  full_name: "Staged Reviewer",
  organization_id: organizationId,
  organization_name: "Synthetic Coexistence Workspace",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "e7777777-7777-4777-8777-777777777777",
    name: "admin",
    permissions: [
      { resource: "chat", action: "read" },
      { resource: "conversations", action: "read" },
      { resource: "channel_accounts", action: "read" },
      { resource: "settings.general", action: "read" },
      { resource: "contacts", action: "read" },
      { resource: "contacts", action: "write" },
      { resource: "contacts.identity_review", action: "write" },
    ],
  },
};

const agent = {
  ...reviewer,
  id: "e5555555-5555-4555-8555-555555555555",
  email: "staged-agent@example.test",
  full_name: "Chat Agent",
  role: {
    id: "e6666666-6666-4666-8666-666666666666",
    name: "agent",
    permissions: [
      { resource: "chat", action: "read" },
      { resource: "contacts", action: "read" },
      { resource: "contacts", action: "write" },
    ],
  },
};

const item = {
  id: stagedId,
  hold_id: holdId,
  protocol_version: 1,
  status: "pending",
  message_type: "text",
  received_at: "2026-01-15T08:30:00Z",
  read_only: true,
  eligibility_known: true,
  decision_available: false,
  eligibility_reason: "no_eligible_member",
};

async function mockApi(page: Page, user: typeof reviewer | typeof agent, stagedRequests: string[], options: { unavailable?: () => boolean; unknownDetail?: boolean; beforeList?: () => Promise<void> } = {}) {
  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "dark");
  }, user);

  // Fallback first: Playwright gives later routes priority.
  await page.route("**/api/**", (route) => route.fulfill({ json: { data: {} } }));
  await page.route(/\/api\/me(?:\?.*)?$/, (route) => route.fulfill({ json: { data: user } }));
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: { data: { mode: "licensed", entitlements: { "omnichannel.enabled": true, "crm.enabled": true } } },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) => route.fulfill({ json: { data: { token: "" } } }));
  await page.route(/\/api\/contacts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { contacts: [], total: 0 } } }),
  );
  await page.route(/\/api\/identity-reviews\/staged(?:\?.*)?$/, async (route) => {
    stagedRequests.push(route.request().url());
    expect(route.request().headers()["x-organization-id"]).toBe(organizationId);
    await options.beforeList?.();
    if (options.unavailable?.()) return route.fulfill({ status: 503, json: { error_type: "identity_review_observation_unavailable", message: "Review availability unavailable" } });
    const query = new URL(route.request().url()).searchParams;
    const pageNumber = Number(query.get("page") || "1");
    const limit = Number(query.get("limit") || "100");
    return route.fulfill({ json: { data: { reviews: pageNumber === 1 ? [item] : [], total: 1, read_only_total: 1, actionable_total: 0, actionable_chain_total: 0, eligibility_known: true, page: pageNumber, limit } } });
  });
  await page.route(new RegExp(`/api/identity-reviews/staged/${stagedId}(?:\\?.*)?$`), (route) => {
    stagedRequests.push(route.request().url());
    expect(route.request().headers()["x-organization-id"]).toBe(organizationId);
    expect(new URL(route.request().url()).searchParams.get("eligibility")).toBe("current-v1");
    return route.fulfill({
      json: { data: { ...item, ...(options.unknownDetail ? { eligibility_known: false, decision_available: false, eligibility_reason: "unavailable", read_only: null } : {}), content: "Saw your ad, how much is it?", media_available: false } },
    });
  });
}

test.describe("Workspace entry point for held WhatsApp messages", () => {
  test("an empty chat shows the held count and the reviewer can read the message", async ({ page }) => {
    const stagedRequests: string[] = [];
    await mockApi(page, reviewer, stagedRequests);
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/chat");

    const notice = page.getByTestId("staged-identity-review-notice");
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("1 held message");
    await expect(notice.getByTestId("staged-identity-review-read-only")).toContainText("No reviews are available right now");

    await page.getByTestId("staged-identity-review-open").click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Held WhatsApp messages");
    await expect(dialog).not.toContainText("Contact review");
    await expect(dialog.getByTestId("staged-pagination-status")).toContainText("Showing 1–1 of 1");

    await expect(dialog.getByTestId("staged-read-only-badge")).toBeVisible();
    await dialog.getByRole("button", { name: /2026-01-15T08:30:00Z/ }).click();
    await expect(dialog).toContainText("Saw your ad, how much is it?");
    const guidance = dialog.getByTestId("staged-identity-review-guidance");
    await expect(guidance).toContainText("No candidate can currently receive");
    await expect(guidance).toContainText("held copy remains protected");
    await expect(guidance).not.toContainText("onboarded again");
    expect(stagedRequests.some((url) => url.includes(`/identity-reviews/staged/${stagedId}`))).toBe(true);

    // Closing the dialog re-reads the count.
    const listRequests = () => stagedRequests.filter((url) => !url.includes(`/identity-reviews/staged/${stagedId}`)).length;
    const beforeClose = listRequests();
    await dialog.getByRole("button", { name: "Close" }).first().click();
    await expect(dialog).toBeHidden();
    await expect.poll(listRequests).toBeGreaterThan(beforeClose);
    await expect(notice).toBeVisible();
  });

  test("the notice fits a phone-width chat list", async ({ page }) => {
    const stagedRequests: string[] = [];
    await mockApi(page, reviewer, stagedRequests);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/chat");
    const notice = page.getByTestId("staged-identity-review-notice");
    await expect(notice).toBeVisible();
    await expect(page.getByTestId("staged-identity-review-open")).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
  });

  for (const path of ["/settings/contacts", "/inbox"]) {
    test(`${path} shows the same entry point`, async ({ page }) => {
      const stagedRequests: string[] = [];
      await mockApi(page, reviewer, stagedRequests);
      await page.setViewportSize({ width: 1600, height: 900 });
      await page.goto(path);
      await expect(page.getByTestId("staged-identity-review-notice")).toBeVisible();
    });
  }

  test("an agent without identity-review authority never requests the protected queue", async ({ page }) => {
    const stagedRequests: string[] = [];
    await mockApi(page, agent, stagedRequests);
    await page.goto("/chat");
    await expect(page.getByTestId("chat-contact-list")).toBeVisible();
    await page.waitForTimeout(500);
    await expect(page.getByTestId("staged-identity-review-notice")).toHaveCount(0);
    expect(stagedRequests).toEqual([]);
  });
});

test("unknown availability remains visible and Retry obtains a current verdict", async ({ page }) => {
  const requests: string[] = [];
  let unavailable = true;
  await mockApi(page, reviewer, requests, { unavailable: () => unavailable });
  await page.goto("/chat");
  const notice = page.getByTestId("staged-identity-review-notice");
  await expect(notice).toContainText("could not be checked");
  await expect(notice.getByTestId("staged-identity-review-count")).toHaveCount(0);
  unavailable = false;
  await page.getByTestId("staged-identity-review-retry").focus();
  await page.keyboard.press("Enter");
  await expect(notice.getByTestId("staged-identity-review-count")).toHaveText("0");
  await expect(notice).toContainText("1 held message");
  await expect(page.getByTestId("staged-identity-review-open")).toBeVisible();
});

test("current-v1 unknown detail keeps the authorized copy without actionable guidance", async ({ page }) => {
  const requests: string[] = [];
  await mockApi(page, reviewer, requests, { unknownDetail: true });
  await page.goto("/chat");
  await page.getByTestId("staged-identity-review-open").click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: /2026-01-15T08:30:00Z/ }).click();
  await expect(dialog).toContainText("Saw your ad, how much is it?");
  await expect(dialog.getByTestId("staged-identity-review-guidance")).toContainText("could not be checked");
  await expect(dialog.getByTestId("staged-identity-review-guidance")).not.toContainText("choose");
  await expect(dialog.getByTestId("identity-review-decision")).toHaveCount(0);
  expect(requests.some(url => url.includes("eligibility=current-v1"))).toBe(true);
});

test("focus and visibility coalesce while the same count request is pending", async ({ page }) => {
  const requests: string[] = [];
  let release: (() => void) | undefined;
  let hold = false;
  await mockApi(page, reviewer, requests, { beforeList: () => hold ? new Promise<void>(done => { release = done; }) : Promise.resolve() });
  await page.goto("/chat");
  await expect(page.getByTestId("staged-identity-review-count")).toHaveText("0");
  hold = true;
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect.poll(() => requests.length).toBe(2);
  await page.evaluate(() => {
    window.dispatchEvent(new Event("focus"));
    document.dispatchEvent(new Event("visibilitychange"));
  });
  expect(requests).toHaveLength(2);
  hold = false;
  release?.();
  await expect(page.getByTestId("staged-identity-review-count")).toHaveText("0");
  expect(requests).toHaveLength(2);
});

test("a replaced stored workspace cannot publish the old in-flight count", async ({ page }) => {
  const requests: string[] = [];
  let release: (() => void) | undefined;
  await mockApi(page, reviewer, requests, { beforeList: () => new Promise<void>(done => { release = done; }) });
  await page.goto("/chat");
  await expect.poll(() => requests.length).toBe(1);
  await page.evaluate(() => {
    window.localStorage.setItem("selected_organization_id", "e8888888-8888-4888-8888-888888888888");
    window.dispatchEvent(new Event("storage"));
  });
  release?.();
  await expect(page.getByTestId("staged-identity-review-count")).toHaveCount(0);
  await expect(page.getByTestId("staged-identity-review-notice")).not.toContainText("1 held message");
  // Storage disagreement invalidates this view; it never adopts authority
  // from another tab's workspace or dispatches a replacement in that scope.
  expect(requests).toHaveLength(1);
});
