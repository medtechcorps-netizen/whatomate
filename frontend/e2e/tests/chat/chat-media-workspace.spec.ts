import { expect, test, type Page } from "@playwright/test";
import { deflateSync } from "node:zlib";

// Customer media must load in whichever workspace is selected, not only in the
// login (default) workspace. Native <img src="/api/media/{id}"> requests send
// the auth cookie but cannot send X-Organization-ID, so the server looked the
// message up in the login workspace and answered 404. The mocked media
// endpoint below mirrors that: it only serves the image when the request is
// pinned to the workspace the transcript was loaded from.

const loginWorkspaceId = "f1111111-1111-4111-8111-111111111111";
const selectedWorkspaceId = "f2222222-2222-4222-8222-222222222222";
const contactId = "f3333333-3333-4333-8333-333333333333";
const imageMessageId = "f4444444-4444-4444-8444-444444444444";
const documentMessageId = "f8888888-8888-4888-8888-888888888888";

// A customer document that would run script if it rendered in the CRM origin.
const customerHtml =
  "<!doctype html><title>Visit summary</title>" +
  "<script>localStorage.setItem('chat-media-script', 'inline')</script>" +
  "<img src=\"missing.png\" onerror=\"localStorage.setItem('chat-media-script', 'handler')\">";

const user = {
  id: "f5555555-5555-4555-8555-555555555555",
  email: "media-reviewer@example.test",
  full_name: "Media Reviewer",
  organization_id: loginWorkspaceId,
  organization_name: "Synthetic Login Workspace",
  is_super_admin: true,
  is_reseller_admin: false,
};

const imageWidth = 240;
const imageHeight = 320;

const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(bytes: Buffer) {
  let crc = 0xffffffff;
  for (const byte of bytes) crc = crcTable[(crc ^ byte) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer) {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([length, body, crc]);
}

// A synthetic solid-colour photo, so the test needs no binary fixture.
function solidPng(width: number, height: number) {
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header[8] = 8; // bit depth
  header[9] = 2; // truecolour RGB
  const row = Buffer.alloc(1 + width * 3);
  for (let x = 0; x < width; x++) row.set([46, 125, 50], 1 + x * 3);
  const pixels = Buffer.concat(Array.from({ length: height }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", header),
    pngChunk("IDAT", deflateSync(pixels)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

const longText = (index: number) =>
  `Message ${index + 1}: ` +
  "Could you confirm the clinic hours and what to bring for the follow-up visit? ".repeat(4);

const textCount = 20;
const baseTime = Date.parse("2026-08-01T08:00:00Z");
const at = (index: number) => new Date(baseTime + index * 60_000).toISOString();

type MediaRequest = { messageId: string; organizationId: string | null };

type MockOptions = {
  // An HTML document from the customer as the newest message.
  htmlDocument?: boolean;
};

async function mockApi(page: Page, options: MockOptions = {}) {
  const mediaRequests: MediaRequest[] = [];
  let releaseMedia!: () => void;
  const mediaGate = new Promise<void>((resolve) => {
    releaseMedia = resolve;
  });
  const png = solidPng(imageWidth, imageHeight);

  await page.addInitScript(
    ({ mockUser, workspaceId }) => {
      window.localStorage.setItem("user", JSON.stringify(mockUser));
      window.localStorage.setItem("selected_organization_id", workspaceId);
      window.localStorage.setItem("color-mode", "light");
    },
    { mockUser: user, workspaceId: selectedWorkspaceId },
  );

  // Fallback first: Playwright gives later routes priority.
  await page.route("**/api/**", (route) => route.fulfill({ json: { data: {} } }));
  await page.route(/\/api\/me(?:\?.*)?$/, (route) => route.fulfill({ json: { data: user } }));
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { mode: "licensed", entitlements: {} } } }),
  );
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: "" } } }),
  );

  const contact = {
    id: contactId,
    phone_number: "60000000001",
    name: "Media Patient",
    profile_name: "Media Patient",
    status: "active",
    tags: [],
    metadata: {},
    unread_count: 0,
    whatsapp_account: "synthetic-media",
    identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
    last_message_at: at(textCount),
    created_at: at(0),
    updated_at: at(textCount),
  };
  const messages = [
    ...Array.from({ length: textCount }, (_, index) => ({
      id: `f6000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
      contact_id: contactId,
      direction: index % 2 === 0 ? "incoming" : "outgoing",
      message_type: "text",
      content: { body: longText(index) },
      status: "delivered",
      created_at: at(index),
      updated_at: at(index),
    })),
    {
      id: imageMessageId,
      contact_id: contactId,
      direction: "incoming",
      message_type: "image",
      content: { body: "" },
      media_url: `organizations/${selectedWorkspaceId}/messages/images/synthetic.png`,
      media_mime_type: "image/png",
      status: "received",
      created_at: at(textCount),
      updated_at: at(textCount),
    },
    ...(options.htmlDocument
      ? [
          {
            id: documentMessageId,
            contact_id: contactId,
            direction: "incoming",
            message_type: "document",
            content: { body: "" },
            media_url: `organizations/${selectedWorkspaceId}/messages/documents/visit-summary.html`,
            media_mime_type: "text/html",
            media_filename: "visit-summary.html",
            status: "received",
            created_at: at(textCount + 1),
            updated_at: at(textCount + 1),
          },
        ]
      : []),
  ];

  await page.route(/\/api\/contacts(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { contacts: [contact], total: 1 } } }),
  );
  await page.route(new RegExp(`/api/contacts/${contactId}(?:\\?.*)?$`), (route) =>
    route.fulfill({ json: { data: contact } }),
  );
  await page.route(new RegExp(`/api/contacts/${contactId}/messages(?:\\?.*)?$`), (route) =>
    route.fulfill({ json: { data: { messages, has_more: false } } }),
  );
  await page.route(/\/api\/media\/[^/?]+(?:\?.*)?$/, async (route) => {
    const request = route.request();
    const messageId = new URL(request.url()).pathname.split("/").pop() ?? "";
    const organizationId = await request.headerValue("x-organization-id");
    mediaRequests.push({ messageId, organizationId });
    const known = messageId === imageMessageId || (options.htmlDocument && messageId === documentMessageId);
    if (!known || organizationId !== selectedWorkspaceId) {
      // Without the header the server resolves the login workspace, where
      // this message does not exist.
      await route.fulfill({ status: 404, json: { status: "error", message: "Message not found" } });
      return;
    }
    if (messageId === documentMessageId) {
      await route.fulfill({ status: 200, contentType: "text/html", body: customerHtml });
      return;
    }
    await mediaGate;
    await route.fulfill({ status: 200, contentType: "image/png", body: png });
  });

  return { mediaRequests, releaseMedia };
}

const scroller = '[data-reka-scroll-area-viewport]:has([data-testid="chat-message-list"])';

async function distanceFromBottom(page: Page) {
  return page.evaluate((target) => {
    const element = document.querySelector(target);
    if (!(element instanceof HTMLElement)) return Number.POSITIVE_INFINITY;
    return element.scrollHeight - element.clientHeight - element.scrollTop;
  }, scroller);
}

function messageMedia(page: Page, messageId: string) {
  return page.locator(
    `[data-testid="chat-message"][data-message-id="${messageId}"] [data-testid="chat-message-media"]`,
  );
}

function imageMedia(page: Page) {
  return messageMedia(page, imageMessageId);
}

async function naturalWidth(page: Page) {
  return imageMedia(page)
    .locator("img")
    .evaluate((image) => (image instanceof HTMLImageElement && image.complete ? image.naturalWidth : 0));
}

test.describe("Chat media in a non-default workspace", () => {
  test("loads a customer image pinned to the selected workspace and keeps the pane at the newest message", async ({
    page,
  }) => {
    const { mediaRequests, releaseMedia } = await mockApi(page);
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);

    const media = imageMedia(page);
    await expect(media).toHaveAttribute("data-media-status", "loading");
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);

    // The image arrives after the initial render; its bubble grows by
    // roughly the image height while the reader is following the thread.
    releaseMedia();
    await expect(media).toHaveAttribute("data-media-status", "ready");
    await expect.poll(() => naturalWidth(page)).toBe(imageWidth);
    await expect(media.locator("img")).toHaveAttribute("src", /^blob:/);
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);

    expect(mediaRequests.length).toBeGreaterThan(0);
    for (const request of mediaRequests) {
      expect(request).toEqual({ messageId: imageMessageId, organizationId: selectedWorkspaceId });
    }

    // Clicking the image opens the already-authorized bytes in a new tab.
    const popupPromise = page.waitForEvent("popup");
    await media.getByRole("button").click();
    const popup = await popupPromise;
    expect(popup.url()).toMatch(/^blob:/);
    await expect
      .poll(() =>
        popup.evaluate(() => {
          const image = document.querySelector("img");
          return image instanceof HTMLImageElement && image.complete ? image.naturalWidth : 0;
        }),
      )
      .toBe(imageWidth);
    await popup.close();
  });

  test("downloads a customer HTML document instead of running it in the CRM origin", async ({ page }) => {
    const { mediaRequests } = await mockApi(page, { htmlDocument: true });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);

    const documentMedia = messageMedia(page, documentMessageId);
    await expect(documentMedia).toHaveAttribute("data-media-status", "idle");
    const downloadPromise = page.waitForEvent("download");
    await documentMedia.getByRole("button", { name: "visit-summary.html" }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toBe("visit-summary.html");
    expect(mediaRequests.filter((request) => request.messageId === documentMessageId)).toEqual([
      { messageId: documentMessageId, organizationId: selectedWorkspaceId },
    ]);

    // "Open link in new tab" on the attachment: the object URL shares this
    // origin, so the browser must download the bytes, not render the page.
    const href = await documentMedia.locator("a[download]").getAttribute("href");
    expect(href).toMatch(/^blob:/);
    const newTab = await page.context().newPage();
    const newTabDownload = newTab.waitForEvent("download");
    // Playwright reports a navigation that turns into a download as an error.
    const opened = await newTab.goto(href ?? "").then(
      () => "rendered as a page",
      (error: Error) => error.message,
    );
    expect(opened).toContain("Download is starting");
    await newTabDownload;
    await page.waitForTimeout(300);
    expect(await page.evaluate(() => localStorage.getItem("chat-media-script"))).toBeNull();
    await newTab.close();
  });

  test("does not pull a reader who scrolled up when an image finishes loading", async ({ page }) => {
    const { releaseMedia } = await mockApi(page);
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);

    const media = imageMedia(page);
    await expect(media).toHaveAttribute("data-media-status", "loading");
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);

    await page.evaluate((target) => {
      const element = document.querySelector(target);
      if (element instanceof HTMLElement) element.scrollTop = 0;
    }, scroller);
    await page.waitForTimeout(300);

    releaseMedia();
    await expect.poll(() => naturalWidth(page)).toBe(imageWidth);
    await page.waitForTimeout(300);

    const scrollTop = await page.evaluate((target) => {
      const element = document.querySelector(target);
      return element instanceof HTMLElement ? element.scrollTop : -1;
    }, scroller);
    expect(scrollTop).toBeLessThan(5);
    expect(await distanceFromBottom(page)).toBeGreaterThan(200);
  });
});
