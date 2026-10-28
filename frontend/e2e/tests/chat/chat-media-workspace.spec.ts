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
const olderImageId = (index: number) => `f7000000-0000-4000-8000-${String(index).padStart(12, "0")}`;
const textMessageId = (index: number) => `f6000000-0000-4000-8000-${String(index).padStart(12, "0")}`;
const threadImageId = (index: number) => `f7100000-0000-4000-8000-${String(index).padStart(12, "0")}`;
const replyMessageId = "f9999999-9999-4999-8999-999999999999";

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
  // Image messages placed before the text history, far above the newest one.
  olderImages?: number;
  // An HTML document from the customer as the newest message.
  htmlDocument?: boolean;
  pdfDocument?: Buffer;
  // Replaces the history with this many text messages, each followed by an
  // image, and ends with a reply that quotes one of the texts.
  thread?: { pairs: number; quotedText: number };
};

async function mockApi(page: Page, options: MockOptions = {}) {
  const mediaRequests: MediaRequest[] = [];
  let inFlight = 0;
  let maxInFlight = 0;
  let releaseMedia!: () => void;
  const mediaGate = new Promise<void>((resolve) => {
    releaseMedia = resolve;
  });
  const png = solidPng(imageWidth, imageHeight);
  const olderImages = options.olderImages ?? 0;
  const thread = options.thread;

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
  const textMessage = (index: number, minute = index) => ({
    id: textMessageId(index),
    contact_id: contactId,
    direction: index % 2 === 0 ? "incoming" : "outgoing",
    message_type: "text",
    content: { body: longText(index) },
    status: "delivered",
    created_at: at(minute),
    updated_at: at(minute),
  });
  const imageMessage = (id: string, minute: number) => ({
    id,
    contact_id: contactId,
    direction: "incoming",
    message_type: "image",
    content: { body: "" },
    media_url: `organizations/${selectedWorkspaceId}/messages/images/${id}.png`,
    media_mime_type: "image/png",
    status: "received",
    created_at: at(minute),
    updated_at: at(minute),
  });
  const replyMessage = (quotedText: number, minute: number) => ({
    ...textMessage(quotedText, minute),
    id: replyMessageId,
    direction: "outgoing",
    content: { body: "Yes, those hours are right." },
    is_reply: true,
    reply_to_message_id: textMessageId(quotedText),
    reply_to_message: {
      id: textMessageId(quotedText),
      direction: "incoming",
      message_type: "text",
      content: { body: longText(quotedText) },
    },
  });
  const documentMessage = {
    id: documentMessageId,
    contact_id: contactId,
    direction: "incoming",
    message_type: "document",
    content: { body: "" },
    media_url: `organizations/${selectedWorkspaceId}/messages/documents/${options.pdfDocument ? 'mrc-scan.pdf' : 'visit-summary.html'}`,
    media_mime_type: options.pdfDocument ? "application/pdf" : "text/html",
    media_filename: options.pdfDocument ? "mrc-scan.pdf" : "visit-summary.html",
    status: "received",
    created_at: at(textCount + 1),
    updated_at: at(textCount + 1),
  };
  const messages = thread
    ? [
        ...Array.from({ length: thread.pairs }, (_, index) => [
          textMessage(index, index * 2),
          imageMessage(threadImageId(index), index * 2 + 1),
        ]).flat(),
        replyMessage(thread.quotedText, thread.pairs * 2),
      ]
    : [
        ...Array.from({ length: olderImages }, (_, index) => imageMessage(olderImageId(index), index - olderImages)),
        ...Array.from({ length: textCount }, (_, index) => textMessage(index)),
        imageMessage(imageMessageId, textCount),
        ...(options.htmlDocument || options.pdfDocument ? [documentMessage] : []),
      ];
  const servedImages = new Set([
    imageMessageId,
    ...Array.from({ length: olderImages }, (_, index) => olderImageId(index)),
    ...Array.from({ length: thread?.pairs ?? 0 }, (_, index) => threadImageId(index)),
  ]);

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
    const known = servedImages.has(messageId) || ((options.htmlDocument || options.pdfDocument) && messageId === documentMessageId);
    if (!known || organizationId !== selectedWorkspaceId) {
      // Without the header the server resolves the login workspace, where
      // this message does not exist.
      await route.fulfill({ status: 404, json: { status: "error", message: "Message not found" } });
      return;
    }
    if (messageId === documentMessageId) {
      await route.fulfill({
        status: 200,
        contentType: options.pdfDocument ? "application/pdf" : "text/html",
        body: options.pdfDocument ?? customerHtml,
      });
      return;
    }
    inFlight++;
    maxInFlight = Math.max(maxInFlight, inFlight);
    await mediaGate;
    await route.fulfill({ status: 200, contentType: "image/png", body: png });
    inFlight--;
  });

  return { mediaRequests, releaseMedia, maxInFlight: () => maxInFlight };
}

const scroller = '[data-reka-scroll-area-viewport]:has([data-testid="chat-message-list"])';

// A real, self-contained MRC-style PDF: low-resolution colour layers plus a
// higher-resolution one-bit grayscale soft mask. All stream lengths and xref
// offsets are exact. Its bounded counterpart must actually draw via pinned
// pdf.js; the large mask must never be sent to that worker.
function mrcPdf(maskSize: number) {
  const stream = (dict: string, data: Buffer) => Buffer.concat([
    Buffer.from("<< " + dict + " /Length " + data.length + " >>\nstream\n"),
    data,
    Buffer.from("\nendstream"),
  ]);
  const rowBytes = Math.ceil(maskSize / 8);
  const mask = Buffer.alloc(rowBytes * maskSize);
  mask.fill(255, 0, rowBytes * Math.floor(maskSize / 2));
  const image = (rgb: number[]) => deflateSync(Buffer.concat(Array.from({ length: 32 * 32 }, () => Buffer.from(rgb))));
  const objects = [
    Buffer.from("<< /Type /Catalog /Pages 2 0 R >>"),
    Buffer.from("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
    Buffer.from("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /XObject << /Bg 5 0 R /Fg 6 0 R >> >> /Contents 4 0 R >>"),
    stream("", Buffer.from("q 200 0 0 200 0 0 cm /Bg Do /Fg Do Q")),
    stream("/Type /XObject /Subtype /Image /Width 32 /Height 32 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", image([255, 255, 255])),
    stream("/Type /XObject /Subtype /Image /Width 32 /Height 32 /ColorSpace /DeviceRGB /BitsPerComponent 8 /SMask 7 0 R /Filter /FlateDecode", image([255, 0, 0])),
    stream("/Type /XObject /Subtype /Image /Width " + maskSize + " /Height " + maskSize + " /ColorSpace /DeviceGray /BitsPerComponent 1 /Filter /FlateDecode", deflateSync(mask)),
  ];
  const parts = [Buffer.from("%PDF-1.7\n")];
  const offsets = [0];
  let length = parts[0].length;
  objects.forEach((body, index) => {
    offsets.push(length);
    const object = Buffer.concat([Buffer.from((index + 1) + " 0 obj\n"), body, Buffer.from("\nendobj\n")]);
    parts.push(object);
    length += object.length;
  });
  parts.push(Buffer.from(
    "xref\n0 " + (objects.length + 1) + "\n0000000000 65535 f \n" +
    offsets.slice(1).map(offset => String(offset).padStart(10, "0") + " 00000 n \n").join("") +
    "trailer\n<< /Size " + (objects.length + 1) + " /Root 1 0 R >>\nstartxref\n" + length + "\n%%EOF\n",
  ));
  return Buffer.concat(parts);
}

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

async function scrollToTop(page: Page) {
  await page.evaluate((target) => {
    const element = document.querySelector(target);
    if (element instanceof HTMLElement) element.scrollTop = 0;
  }, scroller);
}

function readyMedia(page: Page) {
  return page.locator('[data-testid="chat-message-media"][data-media-status="ready"]');
}

async function scrollTop(page: Page) {
  return page.evaluate((target) => {
    const element = document.querySelector(target);
    return element instanceof HTMLElement ? element.scrollTop : -1;
  }, scroller);
}

// Scrolls up a step at a time, as a reader going back through the thread
// does, and reports how far the topmost fully visible message moved while the
// reader paused on each step.
async function readUpward(page: Page, step: number, pauseMs: number) {
  return page.evaluate(
    async ({ target, step, pauseMs }) => {
      const viewport = document.querySelector(target);
      if (!(viewport instanceof HTMLElement)) return [];
      const nextFrame = () => new Promise((resolve) => requestAnimationFrame(resolve));
      const pause = () => new Promise((resolve) => setTimeout(resolve, pauseMs));
      const drifts: number[] = [];
      while (viewport.scrollTop > 0) {
        viewport.scrollTop = Math.max(0, viewport.scrollTop - step);
        await nextFrame();
        const bounds = viewport.getBoundingClientRect();
        const anchor = Array.from(viewport.querySelectorAll('[data-testid="chat-message"]')).find((message) => {
          const rect = message.getBoundingClientRect();
          return rect.top >= bounds.top && rect.bottom <= bounds.bottom;
        });
        if (!anchor) continue;
        const before = anchor.getBoundingClientRect().top;
        await pause();
        drifts.push(Math.round(Math.abs(anchor.getBoundingClientRect().top - before)));
      }
      return drifts;
    },
    { target: scroller, step, pauseMs },
  );
}

async function naturalWidth(page: Page) {
  return imageMedia(page)
    .locator("img")
    .evaluate((image) => (image instanceof HTMLImageElement && image.complete ? image.naturalWidth : 0));
}

type ObservedDownloadBlob = { url: string; type: string; size: number };

async function observeDownloadBlobs(page: Page) {
  await page.addInitScript(() => {
    const blobs: ObservedDownloadBlob[] = [];
    (window as unknown as { __mrcDownloadBlobs: ObservedDownloadBlob[] }).__mrcDownloadBlobs = blobs;
    const create = URL.createObjectURL.bind(URL);
    URL.createObjectURL = object => {
      const url = create(object);
      if (object instanceof Blob) blobs.push({ url, type: object.type, size: object.size });
      return url;
    };
  });
}

async function expectOpaquePdfDownload(page: Page, bytes: Uint8Array) {
  const link = page.getByTestId("chat-media-preview").getByTestId("chat-media-preview-download");
  await expect(link).toHaveAttribute("download", "mrc-scan.pdf");
  const href = await link.getAttribute("href");
  expect(href).toMatch(/^blob:/);
  const observed = await page.evaluate(url =>
    (window as unknown as { __mrcDownloadBlobs: ObservedDownloadBlob[] }).__mrcDownloadBlobs.find(blob => blob.url === url), href);
  expect(observed).toEqual({ url: href, type: "application/octet-stream", size: bytes.length });
  // CSP deliberately forbids fetch(blob:). Exercise the user's native download
  // instead; the observer above calls the original object-URL factory unchanged.
  const downloadEvent = page.waitForEvent("download");
  await link.click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe("mrc-scan.pdf");
  const stream = await download.createReadStream();
  expect(stream).toBeTruthy();
  const chunks: Buffer[] = [];
  let received = 0;
  for await (const chunk of stream!) {
    const data = Buffer.from(chunk);
    received += data.length;
    expect(received).toBeLessThanOrEqual(bytes.length);
    chunks.push(data);
  }
  expect(Buffer.concat(chunks)).toEqual(Buffer.from(bytes));
}

test.describe("Chat media in a non-default workspace", () => {
  test("renders bounded MRC through the pinned PDF worker and preserves opaque download bytes", async ({ page }) => {
    const bytes = mrcPdf(64);
    await observeDownloadBlobs(page);
    const workers: string[] = [];
    page.on("worker", worker => workers.push(worker.url()));
    const { mediaRequests } = await mockApi(page, { pdfDocument: bytes });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto("/chat/" + contactId);
    await messageMedia(page, documentMessageId).getByRole("button", { name: "mrc-scan.pdf" }).click();
    const preview = page.getByTestId("chat-media-preview");
    await expect(preview).toHaveAttribute("data-preview-status", "ready");
    const drawn = preview.getByTestId("chat-media-preview-page");
    await expect(drawn).toHaveAttribute("data-page-state", "rendered");
    const pixels = await drawn.locator("canvas").evaluate(element => {
      if (!(element instanceof HTMLCanvasElement)) throw new Error("Expected PDF canvas");
      const canvas = element;
      const ctx = canvas.getContext("2d")!;
      return [0.25, 0.75].map(y => Array.from(ctx.getImageData(Math.floor(canvas.width / 2), Math.floor(canvas.height * y), 1, 1).data));
    });
    // A blank page or omitted image cannot satisfy both colour-layer samples.
    expect(pixels).toEqual(expect.arrayContaining([[255, 0, 0, 255], [255, 255, 255, 255]]));
    expect(workers.some(url => url.includes("pdf.worker"))).toBe(true);
    await expectOpaquePdfDownload(page, bytes);
    expect(mediaRequests.filter(request => request.messageId === documentMessageId)).toEqual([
      { messageId: documentMessageId, organizationId: selectedWorkspaceId },
    ]);
  });

  test("keeps a legitimate large one-bit MRC as an opaque download without a PDF worker or canvas", async ({ page }) => {
    const bytes = mrcPdf(5000); // 25MP mask exceeds the unchanged combined 4096² cap.
    await observeDownloadBlobs(page);
    const workers: string[] = [];
    page.on("worker", worker => workers.push(worker.url()));
    await mockApi(page, { pdfDocument: bytes });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto("/chat/" + contactId);
    await messageMedia(page, documentMessageId).getByRole("button", { name: "mrc-scan.pdf" }).click();
    const preview = page.getByTestId("chat-media-preview");
    await expect(preview).toHaveAttribute("data-preview-status", "error");
    await expect(preview.getByRole("alert")).toContainText("too large to preview");
    await expect(preview.locator("canvas")).toHaveCount(0);
    expect(workers.some(url => url.includes("pdf.worker"))).toBe(false);
    await expectOpaquePdfDownload(page, bytes);
    await expect(preview.locator("iframe, object, embed")).toHaveCount(0);
  });

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

    // Exactly one request: a refetch on transcript refresh or a duplicate
    // load from the same bubble would show up here.
    expect(mediaRequests).toEqual([{ messageId: imageMessageId, organizationId: selectedWorkspaceId }]);

    // Clicking the image opens the already-authorized bytes in the in-app
    // preview, not a new tab.
    const imageSource = await media.locator("img").getAttribute("src");
    expect(imageSource).toMatch(/^blob:/);
    const popups: string[] = [];
    page.on("popup", (popup) => popups.push(popup.url()));
    await media.getByRole("button").click();
    const preview = page.getByTestId("chat-media-preview");
    await expect(preview).toBeVisible();
    await expect(preview).toHaveAttribute("data-preview-kind", "image");
    const previewImage = preview.locator("img");
    await expect(previewImage).toHaveAttribute("src", imageSource ?? "");
    await expect
      .poll(() => previewImage.evaluate((image) => (image instanceof HTMLImageElement ? image.naturalWidth : 0)))
      .toBe(imageWidth);
    const download = preview.getByTestId("chat-media-preview-download");
    await expect(download).toHaveAttribute("href", imageSource ?? "");
    await expect(download).toHaveAttribute("download", "image.png");

    await page.keyboard.press("Escape");
    await expect(preview).toBeHidden();
    await expect(media.locator("img")).toHaveAttribute("src", imageSource ?? "");
    expect(popups).toEqual([]);
    expect(mediaRequests).toHaveLength(1);

    // "Open image in new tab" still gets an image: the object URL keeps its
    // raster type and nothing else.
    const newTab = await page.context().newPage();
    await newTab.goto(imageSource ?? "");
    expect(await newTab.evaluate(() => document.contentType)).toBe("image/png");
    await newTab.close();
  });

  test("loads the transcript's images newest first, a few at a time", async ({ page }) => {
    const olderImages = 6;
    const { mediaRequests, releaseMedia, maxInFlight } = await mockApi(page, { olderImages });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);

    await expect(imageMedia(page)).toHaveAttribute("data-media-status", "loading");
    await expect.poll(() => mediaRequests.length).toBe(4);
    await page.waitForTimeout(500);
    // While every download is held, no more than four run at once, and the
    // newest images, the ones in view when a chat opens, go first.
    expect(mediaRequests).toHaveLength(4);
    expect(new Set(mediaRequests.map((request) => request.messageId))).toEqual(
      new Set([imageMessageId, olderImageId(5), olderImageId(4), olderImageId(3)]),
    );
    expect(maxInFlight()).toBe(4);

    releaseMedia();
    await expect(readyMedia(page)).toHaveCount(olderImages + 1);
    const requested = mediaRequests.map((request) => request.messageId);
    expect(new Set(requested).size).toBe(requested.length);
    expect(requested).toHaveLength(olderImages + 1);
    for (const request of mediaRequests) expect(request.organizationId).toBe(selectedWorkspaceId);
    expect(maxInFlight()).toBe(4);
    // Older images that load above the newest message do not move it.
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);
  });

  test("keeps the text a reader is on in place while reading back through images", async ({ page }) => {
    const pairs = 12;
    const { releaseMedia } = await mockApi(page, { thread: { pairs, quotedText: 5 } });
    releaseMedia();
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);

    // The whole transcript loads when the chat opens, so its height is final
    // before the reader scrolls back.
    await expect(readyMedia(page)).toHaveCount(pairs);
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);

    const drifts = await readUpward(page, 600, 400);
    expect(drifts.length).toBeGreaterThan(4);
    for (const drift of drifts) expect(drift).toBeLessThanOrEqual(2);
  });

  test("brings a quoted message into view from its reply preview among images", async ({ page }) => {
    const pairs = 12;
    const quotedText = 5;
    const { releaseMedia } = await mockApi(page, { thread: { pairs, quotedText } });
    releaseMedia();
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`/chat/${contactId}`);
    await expect(readyMedia(page)).toHaveCount(pairs);
    await expect.poll(() => distanceFromBottom(page)).toBeLessThan(2);

    const start = await scrollTop(page);
    await page.locator(`[data-testid="chat-message"][data-message-id="${replyMessageId}"] .reply-preview`).click();
    await expect.poll(() => scrollTop(page)).not.toBe(start);
    // Let the smooth scroll finish, then give late layout changes time to show.
    let previous = -1;
    await expect
      .poll(
        async () => {
          const current = await scrollTop(page);
          const settled = current === previous;
          previous = current;
          return settled;
        },
        { intervals: [250] },
      )
      .toBe(true);
    await page.waitForTimeout(500);

    const placement = await page.evaluate(
      ({ target, messageId }) => {
        const viewport = document.querySelector(target);
        const message = document.querySelector(`[data-testid="chat-message"][data-message-id="${messageId}"]`);
        if (!(viewport instanceof HTMLElement) || !(message instanceof HTMLElement)) return null;
        const bounds = viewport.getBoundingClientRect();
        const rect = message.getBoundingClientRect();
        return {
          inView: rect.top >= bounds.top && rect.bottom <= bounds.bottom,
          offsetFromCentre: Math.round(rect.top + rect.height / 2 - (bounds.top + bounds.height / 2)),
        };
      },
      { target: scroller, messageId: textMessageId(quotedText) },
    );
    expect(placement?.inView).toBe(true);
    expect(Math.abs(placement?.offsetFromCentre ?? Number.POSITIVE_INFINITY)).toBeLessThanOrEqual(2);
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

    await scrollToTop(page);
    await page.waitForTimeout(300);

    releaseMedia();
    await expect.poll(() => naturalWidth(page)).toBe(imageWidth);
    await page.waitForTimeout(300);

    expect(await scrollTop(page)).toBeLessThan(5);
    expect(await distanceFromBottom(page)).toBeGreaterThan(200);
  });
});
