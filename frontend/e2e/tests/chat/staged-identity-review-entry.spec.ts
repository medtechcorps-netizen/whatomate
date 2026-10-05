import { expect, test, type Page } from "@playwright/test";

// A Coexistence number connected to an empty workspace used to hold its first
// messages contact-free with no way to see them: the protected staged queue
// was reachable only from an existing contact's Identity review dialog. The
// workspace-level notice must surface the count and open the queue in Chat,
// the Omnichannel Inbox and Contacts, using only the protected endpoints.

const organizationId = "e1111111-1111-4111-8111-111111111111";
const stagedId = "e2222222-2222-4222-8222-222222222222";
const holdId = "e3333333-3333-4333-8333-333333333333";

const reviewer = {
  id: "e4444444-4444-4444-8444-444444444444",
  email: "staged-reviewer@example.test",
  full_name: "Staged Reviewer",
  organization_id: organizationId,
  organization_name: "Synthetic Coexistence Workspace",
  is_super_admin: true,
  is_reseller_admin: false,
};

const agent = {
  ...reviewer,
  id: "e5555555-5555-4555-8555-555555555555",
  email: "staged-agent@example.test",
  full_name: "Chat Agent",
  is_super_admin: false,
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
};

async function mockApi(page: Page, user: typeof reviewer | typeof agent, stagedRequests: string[]) {
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
  await page.route(/\/api\/identity-reviews\/staged(?:\?.*)?$/, (route) => {
    stagedRequests.push(route.request().url());
    return route.fulfill({ json: { data: { reviews: [item], total: 1, page: 1, limit: 100 } } });
  });
  await page.route(new RegExp(`/api/identity-reviews/staged/${stagedId}(?:\\?.*)?$`), (route) => {
    stagedRequests.push(route.request().url());
    return route.fulfill({
      json: { data: { ...item, content: "Saw your ad, how much is it?", media_available: false } },
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
    await expect(notice).toContainText("1 WhatsApp message is held for identity review");

    await page.getByTestId("staged-identity-review-open").click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Held WhatsApp messages");
    await expect(dialog).not.toContainText("Contact review");
    await expect(dialog.getByTestId("staged-pagination-status")).toContainText("Showing 1–1 of 1");

    await dialog.getByRole("button", { name: /2026-01-15T08:30:00Z/ }).click();
    await expect(dialog).toContainText("Saw your ad, how much is it?");
    await expect(dialog.getByTestId("staged-identity-review-guidance")).toContainText(
      "never moved into a conversation",
    );
    expect(stagedRequests.some((url) => url.includes(`/identity-reviews/staged/${stagedId}`))).toBe(true);
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
