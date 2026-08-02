import { expect, test } from "@playwright/test";

test("workspace-scoped SSO discovery carries the selector into initialization", async ({
  page,
}) => {
  const discoverySelectors: string[] = [];
  let initURL = "";

  await page.route(/\/api\/auth\/sso\/providers(?:\?.*)?$/, async (route) => {
    const requestURL = new URL(route.request().url());
    const selector = requestURL.searchParams.get("organization") || "";
    discoverySelectors.push(selector);
    await route.fulfill({
      json: {
        data:
          selector === "klinik-relive-a1b2c3d4"
            ? [{ provider: "google", name: "Google" }]
            : [],
      },
    });
  });
  await page.route(
    /\/api\/auth\/sso\/google\/init(?:\?.*)?$/,
    async (route) => {
      initURL = route.request().url();
      await route.abort();
    },
  );

  await page.goto("/login");
  await expect(page.getByTestId("sso-workspace-discovery")).toBeVisible();
  await page
    .getByTestId("sso-workspace-code")
    .fill("  KLINIK-RELIVE-A1B2C3D4  ");
  await page.getByTestId("sso-discover-button").click();

  await expect
    .poll(() => discoverySelectors)
    .toEqual(["klinik-relive-a1b2c3d4"]);
  await expect(page.getByTestId("sso-provider-google")).toBeVisible();
  await page.getByTestId("sso-provider-google").click();

  await expect.poll(() => initURL).not.toBe("");
  const parsedInitURL = new URL(initURL);
  expect(parsedInitURL.pathname).toBe("/api/auth/sso/google/init");
  expect(parsedInitURL.searchParams.get("organization")).toBe(
    "klinik-relive-a1b2c3d4",
  );
});

test("unknown workspace uses a neutral empty SSO state", async ({ page }) => {
  await page.route(/\/api\/auth\/sso\/providers(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: [] } }),
  );

  await page.goto("/login");
  await page
    .getByTestId("sso-workspace-code")
    .fill("unknown-workspace-a1b2c3d4");
  await page.getByTestId("sso-discover-button").click();

  await expect(page.getByTestId("sso-no-providers")).toHaveText(
    "No SSO sign-in method is available for that workspace code.",
  );
  await expect(page.locator('[data-testid^="sso-provider-"]')).toHaveCount(0);
});
