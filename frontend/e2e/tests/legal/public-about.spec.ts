import { expect, test } from "@playwright/test";

test("ReReply about page is public and explains the Search Console integration", async ({
  page,
}) => {
  await page.goto("/about");

  await expect(page).toHaveURL(/\/about\/?$/);
  await expect(
    page.getByRole("heading", { name: /Every signal\. One reply\./ }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Google Search Console" }),
  ).toBeVisible();
  await expect(page.getByText("No write access")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Privacy Policy" }),
  ).toHaveAttribute("href", "/privacy");
  await expect(
    page.getByRole("link", { name: "Terms of Service" }),
  ).toHaveAttribute("href", "/terms");
  await expect(page.getByRole("link", { name: "Sign in" })).toHaveAttribute(
    "href",
    "/login",
  );
});

test("Public integrations URL resolves to the ReReply about page", async ({
  page,
}) => {
  await page.goto("/integrations");

  await expect(page).toHaveURL(/\/integrations\/?$/);
  await expect(
    page.getByRole("heading", { name: /Every signal\. One reply\./ }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Google Search Console" }),
  ).toBeVisible();
});
