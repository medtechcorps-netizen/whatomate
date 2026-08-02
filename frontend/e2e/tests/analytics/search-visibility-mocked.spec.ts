import { expect, test, type Page } from "@playwright/test";

const organizationId = "91111111-1111-4111-8111-111111111111";
const propertyId = "92222222-2222-4222-8222-222222222222";

function utcDateDaysAgo(days: number) {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() - days);
  return `${date.getUTCFullYear()}-${String(date.getUTCMonth() + 1).padStart(2, "0")}-${String(date.getUTCDate()).padStart(2, "0")}`;
}

const user = {
  id: "93333333-3333-4333-8333-333333333333",
  email: "search-analyst@example.test",
  full_name: "Search Analyst",
  organization_id: organizationId,
  organization_name: "Klinik Relive",
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: "94444444-4444-4444-8444-444444444444",
    name: "search-analyst",
    description: "Search performance analyst",
    is_system: false,
    permissions: [
      { id: "analytics-read", resource: "analytics", action: "read" },
    ],
  },
};

async function installMocks(
  page: Page,
  options: { status?: string; empty?: boolean } = {},
) {
  const analyticsRequests: URL[] = [];
  let setupReads = 0;

  await page.addInitScript((storedUser) => {
    window.localStorage.setItem("user", JSON.stringify(storedUser));
    window.localStorage.setItem("locale", "en");
  }, user);
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
          plan_code: "rereply-growth",
          entitlements: {},
        },
      },
    }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );
  await page.route(/\/api\/org\/settings(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          name: "Klinik Relive",
          settings: { timezone: "Asia/Kuala_Lumpur" },
        },
      },
    }),
  );
  await page.route(
    /\/api\/analytics\/search-visibility\/setup(?:\?.*)?$/,
    (route) => {
      setupReads += 1;
      const connected = options.status !== "not_configured";
      return route.fulfill({
        json: {
          data: {
            status: options.status ?? "connected",
            properties: connected
              ? [
                  {
                    id: propertyId,
                    site_url: "sc-domain:klinikrelive.com",
                    display_name: "klinikrelive.com",
                    property_type: "domain",
                    permission_level: "siteOwner",
                    selected: true,
                    last_synced_at: "2026-07-30T03:00:00Z",
                  },
                ]
              : [],
          },
        },
      });
    },
  );
  await page.route(/\/api\/analytics\/search-visibility(?:\?.*)?$/, (route) => {
    analyticsRequests.push(new URL(route.request().url()));
    const pageFilter =
      analyticsRequests[analyticsRequests.length - 1]?.searchParams.get(
        "page",
      ) ?? "";
    const empty = Boolean(options.empty);
    return route.fulfill({
      json: {
        data: {
          property: {
            id: propertyId,
            site_url: "sc-domain:klinikrelive.com",
            display_name: "klinikrelive.com",
            property_type: "domain",
          },
          start_date: "2026-07-01",
          end_date: "2026-07-28",
          search_type: "web",
          data_state: "final",
          ...(pageFilter ? { page: pageFilter } : {}),
          summary: {
            clicks: empty ? 0 : 1234,
            impressions: empty ? 0 : 45678,
            ctr: empty ? 0 : 0.027015,
            position: empty ? 0 : 4.2,
          },
          trend: empty
            ? []
            : [
                {
                  date: "2026-07-27",
                  clicks: 40,
                  impressions: 1400,
                  ctr: 0.0285,
                  position: 4.1,
                },
                {
                  date: "2026-07-28",
                  clicks: 44,
                  impressions: 1510,
                  ctr: 0.0291,
                  position: 4.0,
                },
              ],
          top_queries: empty
            ? []
            : [
                {
                  query: "klinik near me",
                  clicks: 150,
                  impressions: 1800,
                  ctr: 0.0833,
                  position: 2.3,
                },
              ],
          top_pages: empty
            ? []
            : [
                {
                  page: "https://www.klinikrelive.com/services/weight-loss",
                  clicks: 320,
                  impressions: 5400,
                  ctr: 0.0592,
                  position: 3.1,
                },
                {
                  page: "javascript:alert('not-a-link')",
                  clicks: 1,
                  impressions: 10,
                  ctr: 0.1,
                  position: 8.4,
                },
              ],
        },
      },
    });
  });

  return {
    analyticsRequests,
    setupReads: () => setupReads,
  };
}

test("shows Google clicks—not visits—and applies an exact page filter by property ID", async ({
  page,
}) => {
  const traffic = await installMocks(page);
  await page.goto("/analytics/search-visibility");

  await expect(
    page.getByRole("heading", { name: "Search Visibility" }),
  ).toBeVisible();
  await expect(page.getByText("1,234", { exact: true })).toBeVisible();
  await expect(page.getByText("45,678", { exact: true })).toBeVisible();
  await expect(page.getByText("2.7%", { exact: true })).toBeVisible();
  await expect(page.getByText("4.2", { exact: true })).toBeVisible();
  await expect(
    page.getByText("Google clicks are not website visits."),
  ).toBeVisible();
  await expect(page.getByText(/delayed by roughly 2/)).toBeVisible();
  await expect(page.getByText("klinik near me", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("link", {
      name: /www\.klinikrelive\.com\/services\/weight-loss/,
    }),
  ).toHaveAttribute(
    "href",
    "https://www.klinikrelive.com/services/weight-loss",
  );
  await expect(page.locator('a[href^="javascript:"]')).toHaveCount(0);

  await expect.poll(() => traffic.analyticsRequests.length).toBe(1);
  expect(traffic.setupReads()).toBe(1);
  const initialRequest = traffic.analyticsRequests[0];
  expect(initialRequest.searchParams.get("property_id")).toBe(propertyId);
  expect(initialRequest.searchParams.has("site_url")).toBe(false);
  expect(initialRequest.searchParams.get("end_date")).toBe(utcDateDaysAgo(2));

  await page
    .getByLabel("Exact page URL")
    .fill("https://user:secret@www.klinikrelive.com/services/acne#private");
  await page.getByRole("button", { name: "Apply page" }).click();
  await expect(
    page.getByText(/without credentials or a fragment/i),
  ).toBeVisible();
  expect(traffic.analyticsRequests).toHaveLength(1);

  const exactPage = "https://www.klinikrelive.com/services/acne";
  await page.getByLabel("Exact page URL").fill(exactPage);
  await page.getByRole("button", { name: "Apply page" }).click();
  await expect.poll(() => traffic.analyticsRequests.length).toBe(2);
  expect(traffic.analyticsRequests[1].searchParams.get("page")).toBe(exactPage);
  expect(traffic.analyticsRequests[1].searchParams.get("property_id")).toBe(
    propertyId,
  );
});

test("keeps Search Visibility usable on a phone and explains an empty result", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMocks(page, { empty: true });
  await page.goto("/analytics/search-visibility");

  await expect(
    page.getByRole("heading", { name: "No Google Search data for this view" }),
  ).toBeVisible();
  await expect(page.getByText("—", { exact: true })).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth + 1,
      ),
    )
    .toBe(true);
});

test("does not query properties or analytics before Google is connected", async ({
  page,
}) => {
  const traffic = await installMocks(page, { status: "not_configured" });
  await page.goto("/analytics/search-visibility");

  await expect(
    page.getByRole("heading", { name: "Connect Google Search Console" }),
  ).toBeVisible();
  expect(traffic.setupReads()).toBe(1);
  expect(traffic.analyticsRequests).toHaveLength(0);
});
