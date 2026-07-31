import { expect, test, type Page } from "@playwright/test";

const organizationId = "91111111-1111-4111-8111-111111111111";
const accountId = "92222222-2222-4222-8222-222222222222";
const conversationId = "93333333-3333-4333-8333-333333333333";
const contactId = "94444444-4444-4444-8444-444444444444";
const secondConversationId = "97777777-7777-4777-8777-777777777777";
const secondContactId = "98888888-8888-4888-8888-888888888888";

const user = {
  id: "95555555-5555-4555-8555-555555555555",
  email: "mobile-workspace@example.test",
  full_name: "Mobile Workspace Reviewer",
  organization_id: organizationId,
  organization_name: "Klinik Relive",
  is_super_admin: true,
  is_reseller_admin: false,
};

const dashboardWidgets = [
  {
    id: "mobile-total-messages",
    name: "Total messages",
    description: "All inbound and outbound messages",
    data_source: "messages",
    metric: "count",
    field: "",
    filters: [],
    display_type: "number",
    chart_type: "",
    group_by_field: "",
    show_change: true,
    color: "blue",
    size: "small",
    display_order: 0,
    grid_x: 0,
    grid_y: 0,
    grid_w: 3,
    grid_h: 3,
    config: {},
    is_shared: true,
    is_default: true,
    is_owner: true,
    created_by: user.id,
    created_at: "2026-07-30T08:00:00Z",
    updated_at: "2026-07-30T08:00:00Z",
  },
  {
    id: "mobile-active-contacts",
    name: "Active contacts",
    description: "Contacts active in this period",
    data_source: "contacts",
    metric: "count",
    field: "",
    filters: [],
    display_type: "number",
    chart_type: "",
    group_by_field: "",
    show_change: true,
    color: "green",
    size: "small",
    display_order: 1,
    grid_x: 3,
    grid_y: 0,
    grid_w: 3,
    grid_h: 3,
    config: {},
    is_shared: true,
    is_default: true,
    is_owner: true,
    created_by: user.id,
    created_at: "2026-07-30T08:00:00Z",
    updated_at: "2026-07-30T08:00:00Z",
  },
];

const account = {
  id: accountId,
  channel: "whatsapp",
  provider: "mock_fixture",
  name: "Klinik Relive WhatsApp",
  external_account_id: "klinik-relive-mobile",
  status: "active",
  capabilities: { text: true, replies: true },
  config: { outbound_enabled: true },
  has_credentials: true,
  outbox_pending: 0,
  outbox_failed: 0,
};

const conversation = {
  id: conversationId,
  channel_account_id: accountId,
  contact_id: contactId,
  channel: "whatsapp",
  external_conversation_id: "mobile-conversation-1",
  subject: "Appointment availability",
  status: "open",
  last_message_preview: "Can I book a Saturday appointment?",
  last_message_at: "2026-07-30T08:00:00Z",
  unread_count: 1,
  contact: {
    id: contactId,
    profile_name: "Mobile Patient",
  },
};

const secondConversation = {
  ...conversation,
  id: secondConversationId,
  contact_id: secondContactId,
  external_conversation_id: "mobile-conversation-2",
  subject: "Package renewal",
  last_message_preview: "I would like to renew my package.",
  last_message_at: "2026-07-30T08:05:00Z",
  unread_count: 0,
  contact: {
    id: secondContactId,
    profile_name: "Second Patient",
  },
};

async function mockMobileWorkspace(page: Page) {
  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "light");
  }, user);

  // Register the fallback first because Playwright gives later routes priority.
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
          entitlements: {
            "omnichannel.enabled": true,
            "crm.enabled": true,
            "bookings.enabled": true,
            "commerce.enabled": true,
            "copilot.enabled": true,
          },
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/analytics\/meta\/accounts(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          accounts: [
            {
              id: "meta-mobile-account",
              name: "Klinik Relive Meta",
              phone_id: "meta-mobile-phone",
              is_mock: true,
            },
          ],
          demo_data: true,
        },
      },
    }),
  );
  await page.route(/\/api\/analytics\/meta(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          accounts: [
            {
              account_id: "meta-mobile-account",
              account_name: "Klinik Relive Meta",
              is_mock: true,
              template_names: { "template-mobile": "Appointment reminder" },
              data: {
                id: "meta-mobile-account",
                analytics: {
                  granularity: "DAY",
                  data_points: [
                    {
                      start: 1785369600,
                      end: 1785456000,
                      sent: 120,
                      delivered: 114,
                    },
                  ],
                },
                template_analytics: {
                  granularity: "DAY",
                  data_points: [
                    {
                      start: 1785369600,
                      end: 1785456000,
                      template_id: "template-mobile",
                      sent: 80,
                      delivered: 76,
                      read: 68,
                      replied: 24,
                      clicked: [
                        {
                          type: "quick_reply_button",
                          button_content: "Confirm",
                          count: 12,
                        },
                      ],
                      cost: [{ type: "amount_spent", value: 4.2 }],
                    },
                  ],
                },
                call_analytics: {
                  granularity: "DAY",
                  data_points: [
                    {
                      start: 1785369600,
                      end: 1785456000,
                      count: 12,
                      cost: 1.8,
                      average_duration: 95,
                      direction: "USER_INITIATED",
                    },
                  ],
                },
              },
            },
          ],
          cached: false,
          demo_data: true,
        },
      },
    }),
  );

  await page.route(/\/api\/widgets(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { widgets: dashboardWidgets } } }),
  );
  await page.route(/\/api\/widgets\/data-sources(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          data_sources: [],
          metrics: [],
          display_types: [],
          operators: [],
        },
      },
    }),
  );
  await page.route(/\/api\/widgets\/data(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          data: {
            "mobile-total-messages": {
              widget_id: "mobile-total-messages",
              value: 1284,
              change: 12.5,
              prev_value: 1141,
              chart_data: [],
              data_points: [],
            },
            "mobile-active-contacts": {
              widget_id: "mobile-active-contacts",
              value: 317,
              change: 4.8,
              prev_value: 302,
              chart_data: [],
              data_points: [],
            },
          },
          errors: {},
        },
      },
    }),
  );

  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { accounts: [account] } } }),
  );
  await page.route(/\/api\/conversations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          conversations: [conversation, secondConversation],
          total: 2,
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
            messages: [
              {
                message: {
                  id: "96666666-6666-4666-8666-666666666666",
                  direction: "incoming",
                  message_type: "text",
                  content: "Can I book a Saturday appointment?",
                  status: "delivered",
                  created_at: "2026-07-30T08:00:00Z",
                },
                parts: [
                  {
                    type: "text",
                    text: "Can I book a Saturday appointment?",
                  },
                ],
              },
            ],
            total: 1,
          },
        },
      }),
  );
  await page.route(
    new RegExp(`/api/contacts/${contactId}/workspace(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: {
          data: {
            contact: conversation.contact,
            capabilities: {},
            identities: [],
            journeys: [],
            tasks: [],
            bookings: [],
            packages: [],
            invoices: [],
            payments: [],
            summary: { pipeline_value: [], outstanding: [], collected: [] },
            timeline: [],
          },
        },
      }),
  );
}

test.describe("Focused mobile workspace", () => {
  test.beforeEach(async ({ page }) => {
    await mockMobileWorkspace(page);
  });

  for (const viewport of [
    { width: 390, height: 844 },
    { width: 768, height: 1024 },
    { width: 1023, height: 900 },
  ]) {
    test(`shows only the three focused destinations at ${viewport.width}px`, async ({
      page,
    }) => {
      await page.setViewportSize(viewport);
      await page.goto("/");

      await page
        .getByRole("button", { name: "Open mobile workspace menu" })
        .click();
      const mobileNavigation = page.getByRole("navigation", {
        name: "Mobile workspace",
      });
      await expect(mobileNavigation).toBeVisible();

      const links = mobileNavigation.getByRole("link");
      await expect(links).toHaveCount(3);
      expect(
        (await links.allTextContents()).map((label) => label.trim()),
      ).toEqual(["Dashboard", "Analytics", "Omnichannel Inbox"]);
      await expect(mobileNavigation.getByText("Lead Pipeline")).toHaveCount(0);
      await expect(
        mobileNavigation.getByText("Settings", { exact: true }),
      ).toHaveCount(0);
    });
  }

  test("keeps both permitted analytics views inside the focused Analytics function", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/");

    await page
      .getByRole("button", { name: "Open mobile workspace menu" })
      .click();
    const mobileNavigation = page.getByRole("navigation", {
      name: "Mobile workspace",
    });
    await mobileNavigation
      .getByRole("link", { name: "Analytics", exact: true })
      .click();
    await expect(page).toHaveURL(/\/analytics\/agents$/);

    await page
      .getByRole("button", { name: "Open mobile workspace menu" })
      .click();
    const analyticsViews = mobileNavigation.locator(
      '[aria-label="Analytics views"]',
    );
    await expect(
      analyticsViews.getByRole("link", { name: "Agent Analytics" }),
    ).toBeVisible();
    await expect(
      analyticsViews.getByRole("link", { name: "Meta Insights" }),
    ).toBeVisible();

    await analyticsViews.getByRole("link", { name: "Meta Insights" }).click();
    await expect(page).toHaveURL(/\/analytics\/meta-insights$/);
  });

  test("stacks dashboard widgets without horizontal overflow on phone and tablet", async ({
    page,
  }) => {
    for (const viewport of [
      { width: 390, height: 844 },
      { width: 768, height: 1024 },
      { width: 1023, height: 900 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto("/");

      await expect(
        page.getByText("Total messages", { exact: true }),
      ).toBeVisible();
      const items = page
        .locator(".vgl-layout > .vgl-item")
        .filter({ has: page.locator(".card-depth") });
      await expect(items).toHaveCount(2);

      await expect
        .poll(async () => {
          const [first, second] = await items.evaluateAll((elements) =>
            elements.map((element) => {
              const rect = element.getBoundingClientRect();
              return { left: rect.left, top: rect.top, bottom: rect.bottom };
            }),
          );
          return {
            aligned: Math.abs(first.left - second.left) <= 2,
            gap: Math.round(second.top - first.bottom),
          };
        })
        .toMatchObject({ aligned: true, gap: 16 });

      const boxes = await items.evaluateAll((elements) =>
        elements.map((element) => {
          const rect = element.getBoundingClientRect();
          return {
            left: rect.left,
            right: rect.right,
            top: rect.top,
            width: rect.width,
          };
        }),
      );
      const layoutBox = await page.locator(".vgl-layout").boundingBox();
      expect(layoutBox).not.toBeNull();
      expect(boxes[1].top).toBeGreaterThan(boxes[0].top);
      for (const box of boxes) {
        expect(box.width).toBeLessThanOrEqual(layoutBox!.width + 1);
        expect(box.right).toBeLessThanOrEqual(
          layoutBox!.x + layoutBox!.width + 1,
        );
      }

      const hasHorizontalOverflow = await page.evaluate(
        () =>
          document.documentElement.scrollWidth >
          document.documentElement.clientWidth + 1,
      );
      expect(hasHorizontalOverflow).toBe(false);
    }
  });

  test("uses a list-to-conversation flow with a Back control in the inbox", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/inbox");

    const search = page.getByPlaceholder("Search conversations");
    await expect(search).toBeVisible();
    await page.getByRole("button", { name: /Mobile Patient/ }).click();

    await expect(
      page.getByRole("button", { name: "Back to conversations" }),
    ).toBeVisible();
    const detail = page
      .getByRole("button", { name: "Back to conversations" })
      .locator("xpath=ancestor::main[1]");
    await expect(
      detail.getByText("Can I book a Saturday appointment?", { exact: true }),
    ).toBeVisible();
    await expect(search).toBeHidden();

    await page.getByRole("button", { name: "Back to conversations" }).click();
    await expect(search).toBeVisible();
    await expect(
      page.getByRole("button", { name: /Mobile Patient/ }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Back to conversations" }),
    ).toHaveCount(0);
  });

  test("ignores a late message response after switching conversations", async ({
    page,
  }) => {
    let releaseFirstRequest!: () => void;
    let markFirstRequestStarted!: () => void;
    let markFirstRequestFinished!: () => void;
    const firstRequestGate = new Promise<void>((resolve) => {
      releaseFirstRequest = resolve;
    });
    const firstRequestStarted = new Promise<void>((resolve) => {
      markFirstRequestStarted = resolve;
    });
    const firstRequestFinished = new Promise<void>((resolve) => {
      markFirstRequestFinished = resolve;
    });

    await page.route(
      /\/api\/conversations\/[^/]+\/messages(?:\?.*)?$/,
      async (route) => {
        const isFirst = route.request().url().includes(conversationId);
        if (isFirst) {
          markFirstRequestStarted();
          await firstRequestGate;
        }
        const content = isFirst
          ? "Late response for Mobile Patient"
          : "Current response for Second Patient";
        await route.fulfill({
          json: {
            data: {
              messages: [
                {
                  message: {
                    id: isFirst
                      ? "99999999-9999-4999-8999-999999999991"
                      : "99999999-9999-4999-8999-999999999992",
                    direction: "incoming",
                    message_type: "text",
                    content,
                    status: "delivered",
                    created_at: "2026-07-30T08:10:00Z",
                  },
                  parts: [{ type: "text", text: content }],
                },
              ],
              total: 1,
            },
          },
        });
        if (isFirst) markFirstRequestFinished();
      },
    );

    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/inbox");
    await page.getByRole("button", { name: /Mobile Patient/ }).click();
    await firstRequestStarted;
    await page.getByRole("button", { name: "Back to conversations" }).click();
    await page.getByRole("button", { name: /Second Patient/ }).click();

    const detail = page
      .getByRole("button", { name: "Back to conversations" })
      .locator("xpath=ancestor::main[1]");
    await expect(
      detail.getByText("Current response for Second Patient", { exact: true }),
    ).toBeVisible();

    releaseFirstRequest();
    await firstRequestFinished;
    await page.evaluate(
      () =>
        new Promise<void>((resolve) =>
          requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
        ),
    );

    await expect(
      detail.getByText("Current response for Second Patient", { exact: true }),
    ).toBeVisible();
    await expect(
      detail.getByText("Late response for Mobile Patient", { exact: true }),
    ).toHaveCount(0);
  });

  test("keeps agent analytics readable without phone-width overflow", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/analytics/agents");

    await expect(
      page.getByRole("heading", { name: "Agent Analytics" }),
    ).toBeVisible();
    await expect(
      page.getByText("Transfers Handled", { exact: true }),
    ).toBeVisible();

    const hasHorizontalOverflow = await page.evaluate(
      () =>
        document.documentElement.scrollWidth >
        document.documentElement.clientWidth + 1,
    );
    expect(hasHorizontalOverflow).toBe(false);
  });

  test("wraps Meta Insights cards cleanly on phones and tablets", async ({
    page,
  }) => {
    for (const viewport of [
      { width: 390, height: 844, expectedColumns: 1 },
      { width: 768, height: 1024, expectedColumns: 2 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto("/analytics/meta-insights");

      await expect(
        page.getByRole("heading", { name: "Meta Insights" }),
      ).toBeVisible();
      await expect(page.getByRole("tab", { name: "Messaging" })).toBeVisible();

      for (const analyticsType of [
        { tab: "Templates", expectedCards: 6 },
        { tab: "Calls", expectedCards: 5 },
      ]) {
        await page.getByRole("tab", { name: analyticsType.tab }).click();
        const cards = page.locator('[role="tabpanel"]:visible .card-depth');
        await expect(cards).toHaveCount(analyticsType.expectedCards);

        await expect
          .poll(async () => {
            const boxes = await cards.evaluateAll((elements) =>
              elements.slice(0, 3).map((element) => {
                const rect = element.getBoundingClientRect();
                return { left: rect.left, top: rect.top };
              }),
            );

            if (boxes.length !== 3) return false;
            if (viewport.expectedColumns === 1) {
              return boxes[1].top > boxes[0].top && boxes[2].top > boxes[1].top;
            }

            return (
              Math.abs(boxes[1].top - boxes[0].top) <= 2 &&
              boxes[1].left > boxes[0].left &&
              boxes[2].top > boxes[0].top
            );
          })
          .toBe(true);
      }

      const hasHorizontalOverflow = await page.evaluate(
        () =>
          document.documentElement.scrollWidth >
          document.documentElement.clientWidth + 1,
      );
      expect(hasHorizontalOverflow).toBe(false);
    }
  });

  test("keeps the full workspace navigation and controls on desktop", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1024, height: 900 });
    await page.goto("/");

    await expect(
      page.getByRole("button", { name: "Open mobile workspace menu" }),
    ).toBeHidden();
    const desktopNavigation = page.getByRole("menubar");
    await expect(
      desktopNavigation.getByRole("menuitem", { name: "Lead Pipeline" }),
    ).toBeVisible();
    await expect(
      desktopNavigation.getByRole("menuitem", { name: "Omnichannel Inbox" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Add Widget" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Edit Layout" }),
    ).toBeVisible();

    await page.setViewportSize({ width: 1280, height: 900 });

    const dashboardItems = page
      .locator(".vgl-layout > .vgl-item")
      .filter({ has: page.locator(".card-depth") });
    await expect(dashboardItems).toHaveCount(2);
    await expect
      .poll(async () => {
        const [first, second] = await dashboardItems.evaluateAll((elements) =>
          elements.map((element) => {
            const rect = element.getBoundingClientRect();
            return { left: rect.left, top: rect.top };
          }),
        );
        return {
          sameRow: Math.abs(first.top - second.top) <= 2,
          separated: second.left > first.left + 20,
        };
      })
      .toEqual({ sameRow: true, separated: true });
  });
});
