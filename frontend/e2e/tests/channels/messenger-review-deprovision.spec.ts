import { expect, test, type Page } from "@playwright/test";

const organizationID = "b1111111-1111-4111-8111-111111111111";
const accountID = "b2222222-2222-4222-8222-222222222222";
const pageID = "1038752885977372";
const connectionName = "Klinik Insan Ampang";

function reviewAccount(reviewOnly = true) {
  return {
    id: accountID,
    channel: "messenger",
    provider: "relay",
    name: connectionName,
    external_account_id: pageID,
    status: "pending",
    capabilities: { text: true, replies: true },
    config: {
      onboarding_state: reviewOnly
        ? "review_relay_ready"
        : "awaiting_relay_registry",
      registry_recognized: false,
      identity_confirmed_id: pageID,
      outbound_enabled: false,
      ai_reply_enabled: false,
      ...(reviewOnly ? { review_only: true } : {}),
    },
    has_credentials: true,
    outbox_pending: 0,
    outbox_failed: 0,
  };
}

async function mockReviewWorkspace(
  page: Page,
  options: {
    canDelete?: boolean;
    reviewOnly?: boolean;
    dedicatedStatuses?: number[];
    dedicatedMessages?: string[];
    networkFailureCalls?: number[];
    networkFailureRemovesAccount?: boolean;
  } = {},
) {
  const canDelete = options.canDelete ?? true;
  let account: ReturnType<typeof reviewAccount> | null = reviewAccount(
    options.reviewOnly ?? true,
  );
  let accountListCalls = 0;
  let dedicatedDeleteCalls = 0;
  let genericDeleteCalls = 0;
  const dedicatedRequests: Array<{ method: string; url: string }> = [];
  const user = {
    id: "b3333333-3333-4333-8333-333333333333",
    email: "review-admin@example.test",
    full_name: "ReReply Review Administrator",
    organization_id: organizationID,
    organization_name: "ReReply",
    is_super_admin: canDelete,
    is_reseller_admin: false,
    role: canDelete
      ? undefined
      : {
          id: "role-without-delete",
          name: "Channel manager",
          permissions: [
            { resource: "channel_accounts", action: "read" },
            { resource: "channel_accounts", action: "write" },
            { resource: "conversations", action: "read" },
            { resource: "conversations", action: "write" },
          ],
        },
  };

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem("user", JSON.stringify(mockUser));
    window.localStorage.setItem("color-mode", "light");
  }, user);

  // Register the catch-all first; Playwright gives later routes priority.
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
          entitlements: { "omnichannel.enabled": true },
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/conversations(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { conversations: [], total: 0 } } }),
  );
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, (route) => {
    accountListCalls += 1;
    return route.fulfill({
      json: { data: { accounts: account ? [account] : [] } },
    });
  });
  await page.route(
    new RegExp(`/api/channel-accounts/${accountID}$`),
    (route) => {
      if (route.request().method() === "DELETE") genericDeleteCalls += 1;
      return route.fulfill({
        status: 409,
        json: { message: "Generic deletion must never be used" },
      });
    },
  );
  await page.route(
    new RegExp(`/api/integrations/meta/messenger/review/${accountID}$`),
    (route) => {
      dedicatedDeleteCalls += 1;
      dedicatedRequests.push({
        method: route.request().method(),
        url: new URL(route.request().url()).pathname,
      });
      if (options.networkFailureCalls?.includes(dedicatedDeleteCalls)) {
        if (options.networkFailureRemovesAccount) {
          account = null;
        } else if (account) {
          account = {
            ...account,
            status: "disconnected",
            config: {
              ...account.config,
              onboarding_state: "review_remote_cleanup_pending",
              outbound_enabled: false,
              ai_reply_enabled: false,
            },
          };
        }
        return route.abort("timedout");
      }
      const status =
        options.dedicatedStatuses?.[dedicatedDeleteCalls - 1] ?? 200;
      if (status === 200) {
        account = null;
        return route.fulfill({
          status,
          json: {
            data: {
              message: "Staging Messenger review connection deprovisioned",
            },
          },
        });
      }
      if (account) {
        account = {
          ...account,
          status: "disconnected",
          config: {
            ...account.config,
            onboarding_state: "review_remote_cleanup_pending",
            outbound_enabled: false,
            ai_reply_enabled: false,
          },
        };
      }
      return route.fulfill({
        status,
        json: {
          message:
            options.dedicatedMessages?.[dedicatedDeleteCalls - 1] ??
            "The review connection is quarantined; Meta cleanup must be retried",
        },
      });
    },
  );

  return {
    accountListCalls: () => accountListCalls,
    dedicatedDeleteCalls: () => dedicatedDeleteCalls,
    genericDeleteCalls: () => genericDeleteCalls,
    dedicatedRequests: () => dedicatedRequests,
  };
}

async function openReviewConnectionSettings(page: Page) {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/inbox");
  await page
    .getByRole("button", { name: `Manage ${connectionName}`, exact: true })
    .click();
}

test("deprovisions the exact review Page only through the dedicated audited route", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page);
  await openReviewConnectionSettings(page);

  await expect(page.getByTestId("meta-review-deprovision-open")).toBeVisible();
  await page.getByTestId("meta-review-deprovision-open").click();

  const confirm = page.getByTestId("meta-review-deprovision-confirm");
  await expect(confirm).toBeDisabled();
  await page.getByTestId("meta-review-deprovision-name").fill("Wrong Page");
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);
  await expect(confirm).toBeDisabled();
  await page.getByTestId("meta-review-deprovision-name").fill(connectionName);
  await expect(confirm).toBeEnabled();
  await confirm.click();

  await expect(page.getByTestId("meta-review-deprovision-dialog")).toHaveCount(
    0,
  );
  await expect.poll(calls.accountListCalls).toBeGreaterThan(1);
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);
  expect(calls.dedicatedRequests()).toEqual([
    {
      method: "DELETE",
      url: `/api/integrations/meta/messenger/review/${accountID}`,
    },
  ]);
});

test("keeps a retryable cleanup fail-closed and retries the same dedicated route", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, {
    dedicatedStatuses: [503, 200],
  });
  await openReviewConnectionSettings(page);
  await page.getByTestId("meta-review-deprovision-open").click();
  await page.getByTestId("meta-review-deprovision-name").fill(connectionName);
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);

  const confirm = page.getByTestId("meta-review-deprovision-confirm");
  await confirm.click();
  const error = page.getByTestId("meta-review-deprovision-error");
  await expect(error).toContainText("Cleanup is fail-closed and needs a retry");
  await expect(error).toContainText("Meta cleanup must be retried");
  await expect(confirm).toBeEnabled();
  await expect.poll(calls.accountListCalls).toBeGreaterThan(1);
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);

  await confirm.click();
  await expect(page.getByTestId("meta-review-deprovision-dialog")).toHaveCount(
    0,
  );
  expect(calls.dedicatedDeleteCalls()).toBe(2);
  expect(calls.genericDeleteCalls()).toBe(0);
});

test("confirms completion from the refreshed account list after an ambiguous network response", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, {
    networkFailureCalls: [1],
    networkFailureRemovesAccount: true,
  });
  await openReviewConnectionSettings(page);
  await page.getByTestId("meta-review-deprovision-open").click();
  await page
    .getByTestId("meta-review-deprovision-name")
    .fill(connectionName);
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);
  await page.getByTestId("meta-review-deprovision-confirm").click();

  await expect(
    page.getByTestId("meta-review-deprovision-dialog"),
  ).toHaveCount(0);
  await expect.poll(calls.accountListCalls).toBeGreaterThan(1);
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);
});

test("allows the same dedicated retry when an ambiguous request leaves the exact account", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, {
    networkFailureCalls: [1],
  });
  await openReviewConnectionSettings(page);
  await page.getByTestId("meta-review-deprovision-open").click();
  await page
    .getByTestId("meta-review-deprovision-name")
    .fill(connectionName);
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);

  const confirm = page.getByTestId("meta-review-deprovision-confirm");
  await confirm.click();
  await expect(page.getByTestId("meta-review-deprovision-error")).toContainText(
    "The request outcome is unconfirmed",
  );
  await expect(confirm).toBeEnabled();
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);

  await confirm.click();
  await expect(
    page.getByTestId("meta-review-deprovision-dialog"),
  ).toHaveCount(0);
  expect(calls.dedicatedDeleteCalls()).toBe(2);
  expect(calls.genericDeleteCalls()).toBe(0);
});

test("blocks simple retry when a permanent dispatch fence needs audited reconciliation", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, {
    dedicatedStatuses: [503],
    dedicatedMessages: [
      "An earlier Messenger review reply remains permanently fenced; audited operator reconciliation is required before deprovisioning",
    ],
  });
  await openReviewConnectionSettings(page);
  await page.getByTestId("meta-review-deprovision-open").click();
  await page
    .getByTestId("meta-review-deprovision-name")
    .fill(connectionName);
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);

  const confirm = page.getByTestId("meta-review-deprovision-confirm");
  await confirm.click();
  const error = page.getByTestId("meta-review-deprovision-error");
  await expect(error).toContainText(
    "Audited operator reconciliation is required",
  );
  await expect(error).toContainText("remains permanently fenced");
  await expect(confirm).toBeDisabled();
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);

  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByTestId("meta-review-deprovision-open").click();
  await page
    .getByTestId("meta-review-deprovision-name")
    .fill(connectionName);
  await page.getByTestId("meta-review-deprovision-page-id").fill(pageID);
  await expect(page.getByTestId("meta-review-deprovision-confirm")).toBeDisabled();
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);

  await page.getByTestId("meta-review-deprovision-recheck").click();
  await expect(
    page.getByTestId("meta-review-deprovision-confirm"),
  ).toBeEnabled();
  expect(calls.dedicatedDeleteCalls()).toBe(1);
  expect(calls.genericDeleteCalls()).toBe(0);

  await page.getByTestId("meta-review-deprovision-confirm").click();
  await expect(
    page.getByTestId("meta-review-deprovision-dialog"),
  ).toHaveCount(0);
  expect(calls.dedicatedDeleteCalls()).toBe(2);
  expect(calls.genericDeleteCalls()).toBe(0);
});

test("hides the danger control without channel-account deletion permission", async ({
  page,
}) => {
  await mockReviewWorkspace(page, { canDelete: false });
  await openReviewConnectionSettings(page);

  await expect(page.getByTestId("meta-review-deprovision-open")).toHaveCount(0);
  await expect(page.getByText("Disconnect relay", { exact: true })).toHaveCount(
    0,
  );
});

test("does not offer dedicated review cleanup for a generic managed Messenger account", async ({
  page,
}) => {
  const calls = await mockReviewWorkspace(page, { reviewOnly: false });
  await openReviewConnectionSettings(page);

  await expect(page.getByTestId("meta-review-deprovision-open")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Operator deprovisioning required" }),
  ).toBeDisabled();
  expect(calls.dedicatedDeleteCalls()).toBe(0);
  expect(calls.genericDeleteCalls()).toBe(0);
});
