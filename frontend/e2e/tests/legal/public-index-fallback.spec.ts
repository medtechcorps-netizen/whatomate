import { expect, test } from "@playwright/test";

const legalEntity = "Medtech Healthcare Sdn Bhd";
const legacyLegalEntity = /Medtech Healthcare(?! Sdn Bhd)|Medtech Softwarehouse/;

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

const rawLegalPages = [
  {
    path: "/privacy",
    key: "privacy",
    title: "Privacy Policy",
    section: "scope",
    markers: [
      legalEntity,
      "DigitalOcean as a cloud infrastructure service provider and data processor",
      "current ReReply staging deployment uses DigitalOcean",
      "Singapore (SGP) region",
      "DigitalOcean is not necessarily the only service provider",
      "The Customer Organisation is the data controller",
      "ReReply does not sell personal data",
      "Retention, deletion and your rights",
    ],
    absent: "These Terms are governed by the laws of Malaysia",
  },
  {
    path: "/terms",
    key: "terms",
    title: "Terms of Service",
    section: "agreement",
    markers: [
      legalEntity,
      "Messaging and channel rules",
      "Customers retain ownership of data",
      "These Terms are governed by the laws of Malaysia",
    ],
    absent: "acknowledge requests within seven calendar days",
  },
  {
    path: "/data-deletion",
    key: "data-deletion",
    title: "Data Deletion Instructions",
    section: "before",
    markers: [
      legalEntity,
      "ReReply Data Deletion Request",
      "acknowledge requests within seven calendar days",
      "encrypted or access-restricted backups",
    ],
    absent: "Search Console access is read-only",
  },
];

for (const legalPage of rawLegalPages) {
  test(`raw ${legalPage.path} response contains substantive route-specific legal content before JavaScript`, async ({
    request,
  }) => {
    const response = await request.get(legalPage.path);

    expect(response.ok()).toBe(true);
    expect(response.headers()["content-type"]).toContain("text/html");

    const html = await response.text();
    expect(html).toContain(`data-rereply-raw-document="${legalPage.key}"`);
    expect(html).toContain(
      `<h1 id="legal-fallback-title">${legalPage.title}</h1>`,
    );
    expect(html).toContain(`<title>${legalPage.title} · ReReply</title>`);
    expect(html).toContain(`href="${legalPage.path}#${legalPage.section}"`);
    expect(html).toContain("mailto:medtechcorps@gmail.com");
    expect(html).not.toMatch(legacyLegalEntity);
    for (const marker of legalPage.markers) {
      expect(html).toContain(marker);
    }
    expect(html).not.toContain(legalPage.absent);

    // APIRequestContext returns the server response without executing the
    // module, so substantive content here proves it is present before Vue runs.
    expect(html).toContain('<script type="module"');
  });
}
