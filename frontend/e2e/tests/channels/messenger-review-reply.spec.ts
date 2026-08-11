import { expect, test, type Page } from "@playwright/test";

const organizationId = "b1111111-1111-4111-8111-111111111111";
const accountId = "b2222222-2222-4222-8222-222222222222";
const conversationId = "b3333333-3333-4333-8333-333333333333";
const contactId = "b4444444-4444-4444-8444-444444444444";
const pageId = "1038752885977372";

const reviewer = {
  id: "b5555555-5555-4555-8555-555555555555",
  email: "meta-reviewer@example.test",
  full_name: "Meta App Reviewer",
  organization_id: organizationId,
  organization_name: "Klinik Insan Ampang",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "b6666666-6666-4666-8666-666666666666",
    name: "Meta App Reviewer — 2026-08",
    is_system: false,
    permissions: [
      { id: "p1", resource: "channel_accounts", action: "read" },
      { id: "p2", resource: "contacts", action: "read" },
      { id: "p3", resource: "conversations", action: "read" },
    ],
  },
};

const reviewAccount = {
  id: accountId,
  channel: "messenger",
  provider: "relay",
  name: "Klinik Insan Ampang",
  external_account_id: pageId,
  status: "pending",
  capabilities: { text: true, replies: true },
  config: {
    onboarding_state: "review_relay_ready",
    review_only: true,
    registry_recognized: false,
    outbound_enabled: false,
    ai_reply_enabled: false,
    identity_confirmed_id: pageId,
  },
  has_credentials: true,
  outbox_pending: 0,
  outbox_failed: 0,
};

const conversation = {
  id: conversationId,
  channel_account_id: accountId,
  contact_id: contactId,
  channel: "messenger",
  external_conversation_id: "server-held-recipient",
  status: "open",
  last_message_preview: "META-REVIEW-INBOUND-01",
  last_message_at: "2026-08-11T03:20:00Z",
  unread_count: 1,
  ai_paused: true,
  ai_pause_reason: "review_only",
  contact_identity: { display_name: "Meta reviewer test user" },
};

function eligibleResponse(overrides: Record<string, unknown> = {}) {
  return {
    eligible: true,
    reason_code: "eligible",
    attestation_id: "review-attestation-1",
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    page_id: pageId,
    recipient_label: "••••4821",
    constraints: {
      text_only: true,
      max_length: 2_000,
      manual_confirmation_required: true,
      ai_disabled: true,
      mark_read_disabled: true,
    },
    ...overrides,
  };
}

async function mockReviewWorkspace(
  page: Page,
  eligibility: Record<string, unknown>,
  options: {
    sendDelayMs?: number;
    sendStatus?: number;
    sendMessage?: string;
  } = {},
) {
  let eligibilityCalls = 0;
  let reviewSendCalls = 0;
  let genericSendCalls = 0;
  let markReadCalls = 0;
  let reviewPayload: Record<string, unknown> | null = null;

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "light");
  }, reviewer);

  // Register fallback first because Playwright gives later routes priority.
  await page.route("**/api/**", (route) =>
    route.fulfill({ json: { data: {} } }),
  );
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: reviewer } }),
  );
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: "licensed",
          entitlements: { "omnichannel.enabled": true },
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { accounts: [reviewAccount] } } }),
  );
  await page.route(/\/api\/conversations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: { data: { conversations: [conversation], total: 1 } },
    }),
  );
  await page.route(
    new RegExp(`/api/conversations/${conversationId}/messages(?:\\?.*)?$`),
    (route) => {
      if (route.request().method() === "POST") {
        genericSendCalls += 1;
        return route.fulfill({ status: 500, json: { message: "forbidden" } });
      }
      return route.fulfill({
        json: {
          data: {
            messages: [
              {
                message: {
                  id: "message-incoming-1",
                  direction: "incoming",
                  message_type: "text",
                  content: "META-REVIEW-INBOUND-01",
                  status: "received",
                  created_at: "2026-08-11T03:20:00Z",
                },
              },
            ],
            total: 1,
          },
        },
      });
    },
  );
  await page.route(
    new RegExp(
      `/api/conversations/${conversationId}/meta-review-reply(?:\\?.*)?$`,
    ),
    async (route) => {
      if (route.request().method() === "GET") {
        eligibilityCalls += 1;
        return route.fulfill({ json: { data: eligibility } });
      }
      reviewSendCalls += 1;
      reviewPayload = route.request().postDataJSON();
      if (options.sendDelayMs && options.sendDelayMs > 0) {
        await new Promise((resolve) =>
          setTimeout(resolve, options.sendDelayMs),
        );
      }
      if (options.sendStatus) {
        return route.fulfill({
          status: options.sendStatus,
          json: {
            message:
              options.sendMessage ??
              "The review attempt needs audit verification.",
          },
        });
      }
      return route.fulfill({
        json: {
          data: {
            message: {
              id: "message-outgoing-1",
              direction: "outgoing",
              message_type: "text",
              content: "META-REVIEW-OUTBOUND-01",
              status: "sent",
              created_at: "2026-08-11T03:22:00Z",
            },
            parts: [{ type: "text", text: "META-REVIEW-OUTBOUND-01" }],
            audit: {
              id: "audit-review-1",
              sent_at: "2026-08-11T03:22:00Z",
              page_id: pageId,
              recipient_label: "••••4821",
            },
          },
        },
      });
    },
  );
  await page.route(
    new RegExp(`/api/conversations/${conversationId}/read(?:\\?.*)?$`),
    (route) => {
      markReadCalls += 1;
      return route.fulfill({ json: { data: {} } });
    },
  );

  return {
    eligibilityCalls: () => eligibilityCalls,
    reviewSendCalls: () => reviewSendCalls,
    genericSendCalls: () => genericSendCalls,
    markReadCalls: () => markReadCalls,
    reviewPayload: () => reviewPayload,
  };
}

async function openReviewConversation(page: Page) {
  await page.setViewportSize({ width: 1500, height: 950 });
  await page.goto("/inbox");
  await page.getByRole("button", { name: /Meta reviewer test user/ }).click();
}

test("keeps every review reply control hidden when the server says ineligible", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, {
    ...eligibleResponse(),
    eligible: false,
    reason_code: "window_closed",
    reason: "The App Review reply window is closed.",
  });

  await openReviewConversation(page);

  await expect(page.getByText("Review reply locked")).toBeVisible();
  await expect(page.getByTestId("meta-review-reply-composer")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Send reply" })).toHaveCount(0);
  expect(calls.eligibilityCalls()).toBe(1);
  expect(calls.reviewSendCalls()).toBe(0);
  expect(calls.genericSendCalls()).toBe(0);
  expect(calls.markReadCalls()).toBe(0);
});

test("fails closed on a mismatched Page attestation without generic fallback", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(
    page,
    eligibleResponse({ page_id: "another-page" }),
  );

  await openReviewConversation(page);

  await expect(page.getByText("Reply controls unavailable")).toBeVisible();
  await expect(page.getByTestId("meta-review-reply-composer")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Send reply" })).toHaveCount(0);
  expect(calls.eligibilityCalls()).toBe(1);
  expect(calls.markReadCalls()).toBe(0);
});

test("requires confirmation, sends once, freezes, and shows the audit receipt", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, eligibleResponse(), {
    sendDelayMs: 250,
  });

  await openReviewConversation(page);

  const composer = page.getByTestId("meta-review-reply-composer");
  await expect(composer).toBeVisible();
  await expect(page.getByRole("button", { name: "Send reply" })).toHaveCount(0);
  await expect(page.getByTestId("meta-review-reply-page-id")).toHaveText(
    pageId,
  );
  await expect(
    page.getByTestId("meta-review-reply-recipient-label"),
  ).toHaveText("••••4821");
  await expect(composer).not.toContainText("server-held-recipient");
  expect(calls.markReadCalls()).toBe(0);

  const text = page.getByTestId("meta-review-reply-text");
  const confirmation = page.getByTestId("meta-review-reply-confirmation");
  const send = page.getByTestId("meta-review-reply-send");
  await expect(text).toHaveAttribute("maxlength", "2000");
  await expect(send).toBeDisabled();
  await text.fill("META-REVIEW-OUTBOUND-01");
  await expect(send).toBeDisabled();
  await confirmation.check();
  await expect(send).toBeEnabled();

  await send.click();
  await expect(send).toBeDisabled();
  await expect(text).toBeDisabled();
  await expect(send).toContainText("Sending once");
  await expect(page.getByTestId("meta-review-reply-success")).toContainText(
    "audit-review-1",
  );
  await expect(send).toBeDisabled();
  await expect(text).toBeDisabled();
  await expect(confirmation).not.toBeChecked();
  const deliveredText = page.getByText("META-REVIEW-OUTBOUND-01", {
    exact: true,
  });
  await expect(deliveredText).toHaveCount(2);
  await expect(deliveredText.last()).toBeVisible();

  expect(calls.reviewSendCalls()).toBe(1);
  expect(calls.genericSendCalls()).toBe(0);
  expect(calls.markReadCalls()).toBe(0);
  expect(calls.reviewPayload()).toEqual({
    attestation_id: "review-attestation-1",
    idempotency_key: expect.stringMatching(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    ),
    text: "META-REVIEW-OUTBOUND-01",
    manual_confirmation: true,
  });
});

test("freezes after a 409 without retrying or falling back to generic actions", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, eligibleResponse(), {
    sendStatus: 409,
    sendMessage:
      "This review attempt is already terminal. Verify the audit before refreshing.",
  });

  await openReviewConversation(page);

  const text = page.getByTestId("meta-review-reply-text");
  const confirmation = page.getByTestId("meta-review-reply-confirmation");
  const send = page.getByTestId("meta-review-reply-send");
  await text.fill("META-REVIEW-OUTBOUND-409");
  await confirmation.check();
  await send.click();

  await expect(page.getByTestId("meta-review-reply-error")).toContainText(
    "already terminal",
  );
  await expect(page.getByTestId("meta-review-reply-error")).toContainText(
    "Attempt key:",
  );
  await expect(send).toBeDisabled();
  await expect(text).toBeDisabled();
  await expect(confirmation).toBeDisabled();
  await expect(page.getByRole("button", { name: "Send reply" })).toHaveCount(0);
  await page.waitForTimeout(100);

  expect(calls.reviewSendCalls()).toBe(1);
  expect(calls.genericSendCalls()).toBe(0);
  expect(calls.markReadCalls()).toBe(0);
});
