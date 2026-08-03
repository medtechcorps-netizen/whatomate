import { expect, test } from "@playwright/test";

test("raw app response identifies ReReply and explains its purpose without JavaScript", async ({
  request,
}) => {
  const response = await request.get("/about");

  expect(response.ok()).toBe(true);
  expect(response.headers()["content-type"]).toContain("text/html");

  const html = await response.text();
  expect(html).toMatch(/<h1[^>]*>\s*ReReply\s*<\/h1>/);
  expect(html).toContain("customer relationship management platform");
  expect(html).toContain(
    "customer conversations, CRM records, follow-up workflows",
  );
  expect(html).toContain("read-only Google Search Console reporting");
  expect(html).toContain("verified website properties");
  expect(html).toMatch(/<a[^>]+href="\/about"[^>]*>About ReReply<\/a>/);
  expect(html).toMatch(/<a[^>]+href="\/privacy"[^>]*>Privacy Policy<\/a>/);
  expect(html).toMatch(/<a[^>]+href="\/terms"[^>]*>Terms of Service<\/a>/);
});
