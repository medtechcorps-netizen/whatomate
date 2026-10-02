import { expect, test, type Page } from "@playwright/test";

// A reader at the newest message must keep following it when the window is
// resized. Browser scroll anchoring used to shift scrollTop during the reflow
// and fire a scroll event before the transcript ResizeObserver ran, which
// turned bottom-following off and left the pane far above the latest message.

const organizationId = "a1111111-1111-4111-8111-111111111111";
const accountId = "a2222222-2222-4222-8222-222222222222";
const conversationId = "a3333333-3333-4333-8333-333333333333";
const contactId = "a4444444-4444-4444-8444-444444444444";

const user = {
  id: "a5555555-5555-4555-8555-555555555555",
  email: "resize-reviewer@example.test",
  full_name: "Resize Reviewer",
  organization_id: organizationId,
  organization_name: "Synthetic Clinic",
  is_super_admin: true,
  is_reseller_admin: false,
};

const longText = (index: number) =>
  `Message ${index + 1}: ` +
  "Could you share the programme schedule, the consultation steps and what to prepare before the first visit? ".repeat(
    4,
  );

const messageCount = 24;
const baseTime = Date.parse("2026-07-30T08:00:00Z");
const at = (index: number) => new Date(baseTime + index * 60_000).toISOString();

async function mockApi(page: Page) {
  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "light");
  }, user);

  // Fallback first: Playwright gives later routes priority.
  await page.route("**/api/**", (route) => route.fulfill({ json: { data: {} } }));
  await page.route(/\/api\/me(?:\?.*)?$/, (route) => route.fulfill({ json: { data: user } }));
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: "licensed",
          entitlements: { "omnichannel.enabled": true, "crm.enabled": true },
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );

  // Omnichannel inbox.
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          accounts: [
            {
              id: accountId,
              channel: "whatsapp",
              provider: "mock_fixture",
              name: "Synthetic WhatsApp",
              external_account_id: "synthetic-resize",
              status: "active",
              capabilities: { text: true, replies: true },
              config: { outbound_enabled: true },
              has_credentials: true,
              outbox_pending: 0,
              outbox_failed: 0,
            },
          ],
        },
      },
    }),
  );
  await page.route(/\/api\/conversations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          conversations: [
            {
              id: conversationId,
              channel_account_id: accountId,
              contact_id: contactId,
              channel: "whatsapp",
              external_conversation_id: "resize-conversation",
              subject: "Programme questions",
              status: "open",
              last_message_preview: "Message preview",
              last_message_at: at(messageCount - 1),
              unread_count: 0,
              contact: { id: contactId, profile_name: "Resize Patient" },
            },
          ],
          total: 1,
        },
      },
    }),
  );
  await page.route(
    new RegExp(`/api/conversations/${conversationId}/messages(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: {
          data: {
            messages: Array.from({ length: messageCount }, (_, index) => ({
              message: {
                id: `b0000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
                direction: index % 2 === 0 ? "incoming" : "outgoing",
                message_type: "text",
                content: longText(index),
                status: "delivered",
                created_at: at(index),
              },
              parts: [{ type: "text", text: longText(index) }],
            })),
            total: messageCount,
          },
        },
      }),
  );

  // Native WhatsApp chat.
  const contact = {
    id: contactId,
    phone_number: "60000000000",
    name: "Resize Patient",
    profile_name: "Resize Patient",
    status: "active",
    tags: [],
    metadata: {},
    unread_count: 0,
    whatsapp_account: "synthetic-resize",
    identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
    last_message_at: at(messageCount - 1),
    created_at: at(0),
    updated_at: at(messageCount - 1),
  };
  await page.route(/\/api\/contacts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { contacts: [contact], total: 1 } } }),
  );
  await page.route(new RegExp(`/api/contacts/${contactId}(?:\\?.*)?$`), (route) =>
    route.fulfill({ json: { data: contact } }),
  );
  await page.route(new RegExp(`/api/contacts/${contactId}/messages(?:\\?.*)?$`), (route) =>
    route.fulfill({
      json: {
        data: {
          messages: Array.from({ length: messageCount }, (_, index) => ({
            id: `c0000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
            contact_id: contactId,
            direction: index % 2 === 0 ? "incoming" : "outgoing",
            message_type: "text",
            content: { body: longText(index) },
            status: "delivered",
            created_at: at(index),
            updated_at: at(index),
          })),
          has_more: false,
        },
      },
    }),
  );
}

async function distanceFromBottom(page: Page, selector: string) {
  return page.evaluate((target) => {
    const element = document.querySelector(target);
    if (!(element instanceof HTMLElement)) return Number.POSITIVE_INFINITY;
    return element.scrollHeight - element.clientHeight - element.scrollTop;
  }, selector);
}

test.describe("Message pane follows the newest message across window resizes", () => {
  test.beforeEach(async ({ page }) => {
    await mockApi(page);
  });

  test("omnichannel inbox stays at the bottom after narrowing the window", async ({ page }) => {
    const viewport = '[data-testid="omnichannel-message-viewport"]';
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto("/inbox");
    await page.getByRole("button", { name: /Resize Patient/ }).click();
    await expect(page.locator(viewport)).toBeVisible();
    await expect.poll(() => distanceFromBottom(page, viewport)).toBeLessThan(2);

    await page.setViewportSize({ width: 1300, height: 900 });
    await page.waitForTimeout(400);
    await expect.poll(() => distanceFromBottom(page, viewport)).toBeLessThan(80);

    await page.setViewportSize({ width: 1600, height: 900 });
    await page.waitForTimeout(400);
    await expect.poll(() => distanceFromBottom(page, viewport)).toBeLessThan(80);
  });

  test("native chat keeps the reader's place when older messages load", async ({ page }) => {
    // With scroll anchoring disabled on the chat viewport, prepending history
    // relies on useInfiniteScroll.preserveScrollPosition alone.
    const nativeMessage = (id: string, index: number, body: string) => ({
      id,
      contact_id: contactId,
      direction: index % 2 === 0 ? "incoming" : "outgoing",
      message_type: "text",
      content: { body },
      status: "delivered",
      created_at: at(index),
      updated_at: at(index),
    });
    const latest = Array.from({ length: messageCount }, (_, index) =>
      nativeMessage(
        `d0000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
        index,
        longText(index),
      ),
    );
    const older = Array.from({ length: 12 }, (_, index) =>
      nativeMessage(
        `e0000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
        index - 100,
        `Earlier message ${index + 1}: ${longText(index)}`,
      ),
    );
    let releaseOlder!: () => void;
    const olderGate = new Promise<void>((resolve) => {
      releaseOlder = resolve;
    });
    await page.route(new RegExp(`/api/contacts/${contactId}/messages(?:\\?.*)?$`), async (route) => {
      const isOlderPage = new URL(route.request().url()).searchParams.has("before_id");
      if (isOlderPage) await olderGate;
      await route.fulfill({
        json: {
          data: isOlderPage
            ? { messages: older, has_more: false }
            : { messages: latest, has_more: true },
        },
      });
    });

    const scroller = '[data-reka-scroll-area-viewport]:has([data-testid="chat-message-list"])';
    const anchorMessage = page.locator(`[data-testid="chat-message"][data-message-id="${latest[0].id}"]`);
    const anchorOffset = () =>
      page.evaluate(
        ({ target, id }) => {
          const viewport = document.querySelector(target);
          const message = document.querySelector(
            `[data-testid="chat-message"][data-message-id="${id}"]`,
          );
          if (!viewport || !message) return Number.NaN;
          return message.getBoundingClientRect().top - viewport.getBoundingClientRect().top;
        },
        { target: scroller, id: latest[0].id },
      );

    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);
    await expect(anchorMessage).toBeAttached();
    await expect.poll(() => distanceFromBottom(page, scroller)).toBeLessThan(2);

    // Scroll to the top to request older history; hold the response so the
    // reader's position can be measured before the prepend lands.
    await page.evaluate((target) => {
      const viewport = document.querySelector(target);
      if (viewport instanceof HTMLElement) viewport.scrollTop = 0;
    }, scroller);
    await page.waitForTimeout(300);
    const before = await anchorOffset();
    expect(Number.isFinite(before)).toBe(true);

    releaseOlder();
    await expect(
      page.locator(`[data-testid="chat-message"][data-message-id="${older[0].id}"]`),
    ).toBeAttached();
    await page.waitForTimeout(400);

    const after = await anchorOffset();
    // The message the reader was looking at stays roughly where it was (the
    // transient "loading older messages" row may account for a few pixels).
    expect(Math.abs(after - before)).toBeLessThan(60);
    // And the newly loaded history sits above it, out of view until scrolled.
    expect(await page.evaluate((target) => {
      const viewport = document.querySelector(target);
      return viewport instanceof HTMLElement ? viewport.scrollTop : -1;
    }, scroller)).toBeGreaterThan(100);
  });

  test("native chat stays at the bottom after narrowing the window", async ({ page }) => {
    const viewport = '[data-testid="chat-message-list"]';
    const scroller = `[data-reka-scroll-area-viewport]:has(${viewport})`;
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);
    await expect(page.locator(viewport)).toBeVisible();
    await expect.poll(() => distanceFromBottom(page, scroller)).toBeLessThan(2);

    await page.setViewportSize({ width: 1300, height: 900 });
    await page.waitForTimeout(400);
    await expect.poll(() => distanceFromBottom(page, scroller)).toBeLessThan(80);
  });
});
