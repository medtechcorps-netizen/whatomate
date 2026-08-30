import { createHmac } from "node:crypto";
import { lookup as defaultDnsLookup } from "node:dns/promises";
import { isIP } from "node:net";

export const UI_CHECKS = Object.freeze([
  "klinik_whatsapp_outbound",
  "klinik_whatsapp_inbound",
  "omnichannel_outbound_realtime_without_reload",
  "omnichannel_inbound_realtime_without_reload",
  "navbar_unread_increment",
  "navbar_unread_clear",
  "omnichannel_conversation_switch_autoscroll",
  "omnichannel_late_layout_autoscroll",
  "native_chat_realtime_without_reload",
  "native_chat_conversation_switch_autoscroll",
  "native_chat_late_layout_autoscroll",
  "non_klinik_send_denied",
  "cross_organization_send_denied",
]);

const CHECK_EXECUTION_ORDER = Object.freeze([
  "klinik_whatsapp_inbound",
  "omnichannel_inbound_realtime_without_reload",
  "native_chat_realtime_without_reload",
  "klinik_whatsapp_outbound",
  "omnichannel_outbound_realtime_without_reload",
  "navbar_unread_increment",
  "navbar_unread_clear",
  "omnichannel_conversation_switch_autoscroll",
  "omnichannel_late_layout_autoscroll",
  "native_chat_conversation_switch_autoscroll",
  "native_chat_late_layout_autoscroll",
  "non_klinik_send_denied",
  "cross_organization_send_denied",
]);

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const DIGITS_RE = /^[1-9][0-9]{5,31}$/;
const SAFE_LABEL_RE = /^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,95}$/;
const FIXTURE_NAMESPACE_RE = /^[a-z0-9](?:[a-z0-9-]{1,46}[a-z0-9])?$/;
const MAX_API_BODY_BYTES = 262_144;
const DEFAULT_TIMEOUT_MS = 45_000;
// Leave a 30-second cleanup/response margin below the verifier's 240-second
// driver-only socket timeout and remain below its 300-second signed freshness.
const DRIVER_EXECUTION_TIMEOUT_MS = 210_000;
const DEADLINE_CLEANUP_GRACE_MS = 1_000;
const BOTTOM_TOLERANCE_PX = 12;
const MAX_WEBHOOK_RESPONSE_BYTES = 4096;
const PRODUCT_ORIGIN = "https://app.rereply.app";

export class SyntheticCanaryFailure extends Error {
  constructor(message) {
    super(message);
    this.name = "SyntheticCanaryFailure";
  }
}

function fail(message) {
  throw new SyntheticCanaryFailure(message);
}

function isPlainObject(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function exactKeys(value, expected, label) {
  if (!isPlainObject(value)) fail(label + " is invalid");
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (
    actual.length !== wanted.length ||
    actual.some((key, index) => key !== wanted[index])
  ) {
    fail(label + " keys differ");
  }
  return value;
}

function exactString(value, label, pattern, maximum = 4096) {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > maximum
  ) {
    fail(label + " is invalid");
  }
  if (
    value.includes("\u0000") ||
    value.includes("\r") ||
    value.includes("\n") ||
    (pattern && !pattern.test(value))
  ) {
    fail(label + " is invalid");
  }
  return value;
}

function validateOrigin(value) {
  const raw = exactString(value, "product origin", undefined, 2048);
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    fail("product origin is invalid");
  }
  if (
    raw !== PRODUCT_ORIGIN ||
    parsed.protocol !== "https:" ||
    parsed.username ||
    parsed.password ||
    parsed.search ||
    parsed.hash ||
    parsed.port ||
    parsed.pathname !== "/" ||
    parsed.origin !== raw ||
    parsed.hostname === "localhost" ||
    parsed.hostname.endsWith(".localhost") ||
    parsed.hostname.endsWith(".local") ||
    parsed.hostname.endsWith(".internal") ||
    parsed.hostname.endsWith(".lan") ||
    isIP(parsed.hostname) !== 0
  ) {
    fail("product origin is invalid");
  }
  return parsed.origin;
}

export function isExactProductUrl(value, origin, pathname) {
  let parsed;
  try {
    parsed = value instanceof URL ? value : new URL(value);
  } catch {
    return false;
  }
  return (
    parsed.origin === origin &&
    parsed.pathname === pathname &&
    parsed.search === "" &&
    parsed.hash === ""
  );
}

function publicIPv4(address) {
  const parts = address.split(".").map(Number);
  if (
    parts.length !== 4 ||
    parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)
  ) {
    return false;
  }
  const [a, b, c] = parts;
  return !(
    a === 0 ||
    a === 10 ||
    a === 127 ||
    a >= 224 ||
    (a === 100 && b >= 64 && b <= 127) ||
    (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && b === 168) ||
    (a === 192 && b === 0 && (c === 0 || c === 2)) ||
    (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) ||
    (a === 203 && b === 0 && c === 113)
  );
}

function publicIPAddress(address) {
  const version = isIP(address);
  if (version === 4) return publicIPv4(address);
  if (version !== 6) return false;
  const value = address.toLowerCase();
  if (value.includes("%") || value.includes(".")) return false;
  const firstHextet = Number.parseInt(value.split(":", 1)[0], 16);
  return (
    Number.isInteger(firstHextet) &&
    firstHextet >= 0x2000 &&
    firstHextet <= 0x3fff &&
    !value.startsWith("2001:db8:")
  );
}

export async function validatePublicProductOrigin(
  origin,
  lookup = defaultDnsLookup,
) {
  const normalized = validateOrigin(origin);
  const hostname = new URL(normalized).hostname;
  let records;
  try {
    records = await lookup(hostname, { all: true, verbatim: true });
  } catch {
    fail("product origin DNS is unavailable");
  }
  if (
    !Array.isArray(records) ||
    records.length === 0 ||
    records.some(
      (record) =>
        !isPlainObject(record) ||
        typeof record.address !== "string" ||
        !publicIPAddress(record.address),
    )
  ) {
    fail("product origin does not resolve only to public addresses");
  }
  return normalized;
}

function legacyChannelAccountName(name, accountID) {
  const prefix = "WhatsApp ";
  const suffix = " [" + accountID + "]";
  return prefix + name.slice(0, 100 - prefix.length - suffix.length) + suffix;
}

function validateConversation(value, label, namespace) {
  const source = exactKeys(
    value,
    ["conversation_id", "contact_id", "display_name", "sender_wa_id"],
    label,
  );
  const displayName = exactString(
    source.display_name,
    label + " display name",
    SAFE_LABEL_RE,
    96,
  );
  if (!displayName.toLowerCase().startsWith(namespace + "-")) {
    fail(label + " is not bound to the synthetic namespace");
  }
  return {
    conversation_id: exactString(
      source.conversation_id,
      label + " conversation ID",
      UUID_RE,
      36,
    ),
    contact_id: exactString(
      source.contact_id,
      label + " contact ID",
      UUID_RE,
      36,
    ),
    display_name: displayName,
    sender_wa_id: exactString(
      source.sender_wa_id,
      label + " sender identity",
      DIGITS_RE,
      32,
    ),
  };
}

export function validateFixtureDescriptor(value) {
  const source = exactKeys(
    value,
    [
      "schema_version",
      "product_origin",
      "fixture_namespace",
      "klinik",
      "non_klinik",
    ],
    "fixture descriptor",
  );
  if (source.schema_version !== 1) fail("fixture descriptor schema differs");
  const namespace = exactString(
    source.fixture_namespace,
    "fixture namespace",
    FIXTURE_NAMESPACE_RE,
    48,
  );
  if (namespace !== "rereply-canary") {
    fail("fixture namespace differs");
  }
  const klinik = exactKeys(
    source.klinik,
    ["organization_id", "conversations", "meta"],
    "Klinik fixture",
  );
  const conversations = exactKeys(
    klinik.conversations,
    ["a", "b"],
    "conversation fixtures",
  );
  const conversationA = validateConversation(
    conversations.a,
    "conversation A",
    namespace,
  );
  const conversationB = validateConversation(
    conversations.b,
    "conversation B",
    namespace,
  );
  const meta = exactKeys(
    klinik.meta,
    [
      "business_account_id",
      "phone_number_id",
      "display_phone_number",
      "channel_account_id",
      "legacy_account_id",
      "legacy_account_name",
    ],
    "Meta fixture",
  );
  const nonKlinik = exactKeys(
    source.non_klinik,
    ["organization_id"],
    "non-Klinik fixture",
  );
  const result = {
    schema_version: 1,
    product_origin: validateOrigin(source.product_origin),
    fixture_namespace: namespace,
    klinik: {
      organization_id: exactString(
        klinik.organization_id,
        "Klinik organization ID",
        UUID_RE,
        36,
      ),
      conversations: { a: conversationA, b: conversationB },
      meta: {
        business_account_id: exactString(
          meta.business_account_id,
          "Meta business account ID",
          DIGITS_RE,
          32,
        ),
        phone_number_id: exactString(
          meta.phone_number_id,
          "Meta phone ID",
          DIGITS_RE,
          32,
        ),
        display_phone_number: exactString(
          meta.display_phone_number,
          "Meta display phone",
          DIGITS_RE,
          32,
        ),
        channel_account_id: exactString(
          meta.channel_account_id,
          "Meta shadow channel account ID",
          UUID_RE,
          36,
        ),
        legacy_account_id: exactString(
          meta.legacy_account_id,
          "legacy WhatsApp account ID",
          UUID_RE,
          36,
        ),
        legacy_account_name: exactString(
          meta.legacy_account_name,
          "legacy WhatsApp account name",
          SAFE_LABEL_RE,
          96,
        ),
      },
    },
    non_klinik: {
      organization_id: exactString(
        nonKlinik.organization_id,
        "non-Klinik organization ID",
        UUID_RE,
        36,
      ),
    },
  };
  if (result.klinik.organization_id === result.non_klinik.organization_id) {
    fail("synthetic organizations are not isolated");
  }
  if (
    conversationA.conversation_id === conversationB.conversation_id ||
    conversationA.contact_id === conversationB.contact_id ||
    conversationA.sender_wa_id === conversationB.sender_wa_id ||
    conversationA.display_name === conversationB.display_name
  ) {
    fail("synthetic conversations are not distinct");
  }
  return result;
}

export function validateLogin(value, label) {
  const source = exactKeys(
    value,
    ["schema_version", "email", "password"],
    label,
  );
  if (source.schema_version !== 1) fail(label + " schema differs");
  const email = exactString(
    source.email,
    label + " email",
    /^[\x21-\x7e]+$/u,
    254,
  );
  if (!email.includes("@")) fail(label + " email is invalid");
  const password = exactString(
    source.password,
    label + " password",
    /^[\x20-\x7e]+$/u,
    256,
  );
  if (password.length < 8) fail(label + " password is invalid");
  return { schema_version: 1, email, password };
}

function metaTextMessage(descriptor, conversation, wamid, body, now) {
  return {
    object: "whatsapp_business_account",
    entry: [
      {
        id: descriptor.klinik.meta.business_account_id,
        changes: [
          {
            field: "messages",
            value: {
              messaging_product: "whatsapp",
              metadata: {
                display_phone_number:
                  descriptor.klinik.meta.display_phone_number,
                phone_number_id: descriptor.klinik.meta.phone_number_id,
              },
              contacts: [
                {
                  profile: { name: conversation.display_name },
                  wa_id: conversation.sender_wa_id,
                },
              ],
              messages: [
                {
                  from: conversation.sender_wa_id,
                  id: wamid,
                  timestamp: String(Math.floor(now.getTime() / 1000)),
                  text: { body },
                  type: "text",
                },
              ],
            },
          },
        ],
      },
    ],
  };
}

export function buildSignedWebhook(
  descriptor,
  conversation,
  wamid,
  body,
  metaAppSecret,
  now,
) {
  exactString(wamid, "synthetic WAMID", /^[A-Za-z0-9._:-]+$/u, 128);
  exactString(body, "synthetic message body", /^[\x20-\x7e]+$/u, 4096);
  exactString(metaAppSecret, "Meta app secret", /^[\x21-\x7e]+$/u, 256);
  if (metaAppSecret.length < 16) fail("Meta app secret is invalid");
  const payload = JSON.stringify(
    metaTextMessage(descriptor, conversation, wamid, body, now),
  );
  return {
    payload,
    signature:
      "sha256=" +
      createHmac("sha256", metaAppSecret).update(payload, "utf8").digest("hex"),
  };
}

function unwrapData(value) {
  return isPlainObject(value) &&
    Object.prototype.hasOwnProperty.call(value, "data")
    ? value.data
    : value;
}

function unwrapList(value, key) {
  const first = unwrapData(value);
  const second = unwrapData(first);
  if (isPlainObject(second) && Array.isArray(second[key])) return second[key];
  if (isPlainObject(first) && Array.isArray(first[key])) return first[key];
  if (Array.isArray(second)) return second;
  fail("product list response differs");
}

async function pageFetch(page, path, init = {}) {
  if (
    typeof path !== "string" ||
    !path.startsWith("/api/") ||
    path.includes("\n")
  ) {
    fail("product API path is invalid");
  }
  const request = {
    path,
    method: init.method || "GET",
    headers: init.headers || {},
    body: init.body === undefined ? null : JSON.stringify(init.body),
    maximum: MAX_API_BODY_BYTES,
    timeout: DEFAULT_TIMEOUT_MS,
  };
  return page.evaluate(async (input) => {
    const csrfMatch = document.cookie.match(/(?:^|; )whm_csrf=([^;]*)/u);
    const headers = { Accept: "application/json", ...input.headers };
    if (input.body !== null) {
      headers["Content-Type"] = "application/json";
      if (csrfMatch) headers["X-CSRF-Token"] = decodeURIComponent(csrfMatch[1]);
    }
    try {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), input.timeout);
      let response;
      try {
        response = await fetch(input.path, {
          method: input.method,
          headers,
          body: input.body,
          credentials: "include",
          redirect: "error",
          cache: "no-store",
          signal: controller.signal,
        });
        const declared = response.headers.get("content-length");
        if (
          declared !== null &&
          (!/^[0-9]+$/u.test(declared) || Number(declared) > input.maximum)
        ) {
          await response.body?.cancel();
          return { status: response.status, invalid: true, json: null };
        }
        if (!response.body) {
          return { status: response.status, invalid: true, json: null };
        }
        const reader = response.body.getReader();
        const chunks = [];
        let total = 0;
        let complete = false;
        while (!complete) {
          const { done, value } = await reader.read();
          complete = done;
          if (complete) break;
          total += value.byteLength;
          if (total > input.maximum) {
            await reader.cancel();
            return { status: response.status, invalid: true, json: null };
          }
          chunks.push(value);
        }
        if (total === 0) {
          return { status: response.status, invalid: true, json: null };
        }
        const raw = new Uint8Array(total);
        let offset = 0;
        for (const chunk of chunks) {
          raw.set(chunk, offset);
          offset += chunk.byteLength;
        }
        const text = new TextDecoder("utf-8", { fatal: true }).decode(raw);
        return {
          status: response.status,
          invalid: false,
          json: JSON.parse(text),
        };
      } finally {
        clearTimeout(timer);
      }
    } catch {
      return {
        status: 0,
        invalid: true,
        json: null,
      };
    }
  }, request);
}

export function validateLiveFixtureConversation(value, descriptor, fixture) {
  if (!isPlainObject(value)) fail("live fixture conversation is invalid");
  const account = value.channel_account;
  const contact = value.contact;
  const identity = value.contact_identity;
  const meta = descriptor.klinik.meta;
  const expectedExternalContact = "legacy-contact:" + fixture.contact_id;
  const expectedExternalAccount = "legacy-account:" + meta.legacy_account_id;
  if (
    value.id !== fixture.conversation_id ||
    value.organization_id !== descriptor.klinik.organization_id ||
    value.channel_account_id !== meta.channel_account_id ||
    value.contact_id !== fixture.contact_id ||
    value.channel !== "whatsapp" ||
    value.external_conversation_id !== expectedExternalContact ||
    !isPlainObject(account) ||
    account.id !== meta.channel_account_id ||
    account.organization_id !== descriptor.klinik.organization_id ||
    account.channel !== "whatsapp" ||
    account.provider !== "meta_legacy" ||
    account.name !==
      legacyChannelAccountName(
        meta.legacy_account_name,
        meta.legacy_account_id,
      ) ||
    account.external_account_id !== expectedExternalAccount ||
    account.status !== "active" ||
    account.has_credentials !== false ||
    !isPlainObject(account.capabilities) ||
    account.capabilities.text !== true ||
    account.capabilities.replies !== true ||
    account.capabilities.service_window !== true ||
    account.capabilities.legacy_text_reply_endpoint !== true ||
    !isPlainObject(account.config) ||
    account.config.legacy_read_only !== true ||
    account.config.outbound_enabled !== false ||
    account.config.reply_route !== "chat" ||
    !isPlainObject(contact) ||
    contact.id !== fixture.contact_id ||
    contact.organization_id !== descriptor.klinik.organization_id ||
    contact.phone_number !== fixture.sender_wa_id ||
    contact.profile_name !== fixture.display_name ||
    contact.whatsapp_account !== meta.legacy_account_name ||
    !isPlainObject(identity) ||
    identity.organization_id !== descriptor.klinik.organization_id ||
    identity.contact_id !== fixture.contact_id ||
    identity.channel_account_id !== meta.channel_account_id ||
    identity.channel !== "whatsapp" ||
    identity.external_id !== expectedExternalContact ||
    identity.address !== fixture.sender_wa_id ||
    identity.normalized_address !== fixture.sender_wa_id ||
    identity.display_name !== fixture.display_name ||
    identity.is_primary !== true ||
    identity.is_verified !== true
  ) {
    fail("live fixture identity differs from the protected descriptor");
  }
  if (!Number.isSafeInteger(value.unread_count) || value.unread_count < 0) {
    fail("live fixture unread state is invalid");
  }
  return value;
}

export function validateRefreshedServiceWindow(value, now) {
  const checkedAt = now instanceof Date ? now : new Date(now);
  const serviceWindow = Date.parse(value?.service_window_ends_at || "");
  if (
    !Number.isFinite(checkedAt.getTime()) ||
    !Number.isFinite(serviceWindow) ||
    serviceWindow <= checkedAt.getTime()
  ) {
    fail("synthetic outbound fixture service window is unavailable");
  }
  return true;
}

async function loadLiveFixtureConversation(page, descriptor, fixture) {
  const response = await pageFetch(
    page,
    "/api/conversations?limit=100&search=" +
      encodeURIComponent(fixture.sender_wa_id),
    {
      headers: {
        "X-Organization-ID": descriptor.klinik.organization_id,
      },
    },
  );
  if (response.status !== 200 || response.invalid) {
    fail("live fixture inventory is unavailable");
  }
  const matches = unwrapList(response.json, "conversations").filter(
    (conversation) =>
      isPlainObject(conversation) &&
      conversation.id === fixture.conversation_id,
  );
  if (matches.length !== 1) fail("live fixture inventory differs");
  return validateLiveFixtureConversation(matches[0], descriptor, fixture);
}

async function login(page, origin, loginValue, organizationID, timeout) {
  await page.addInitScript(() => {
    const key = "__rereply_canary_document_count";
    const next = Number(sessionStorage.getItem(key) || "0") + 1;
    sessionStorage.setItem(key, String(next));
  });
  await page.goto(origin + "/login", {
    waitUntil: "domcontentloaded",
    timeout,
  });
  await page.locator("#email").fill(loginValue.email);
  await page.locator("#password").fill(loginValue.password);
  await Promise.all([
    page.waitForURL(
      (url) => url.origin === origin && url.pathname !== "/login",
      { timeout },
    ),
    page.getByTestId("password-sign-in").click(),
  ]);
  const me = await pageFetch(page, "/api/me");
  if (me.status !== 200 || me.invalid) fail("synthetic login failed");
  const user = unwrapData(me.json);
  if (!isPlainObject(user) || user.organization_id !== organizationID) {
    fail("synthetic login organization differs");
  }
}

async function documentCount(page) {
  return page.evaluate(() =>
    Number(sessionStorage.getItem("__rereply_canary_document_count") || "0"),
  );
}

async function selectOmnichannelConversation(page, conversationID, timeout) {
  const target = page.locator(
    '[data-testid="omnichannel-conversation"][data-conversation-id="' +
      conversationID +
      '"]',
  );
  await target.first().waitFor({ state: "visible", timeout });
  if ((await target.count()) !== 1)
    fail("synthetic conversation binding differs");
  await target.click({ timeout });
  await page
    .getByTestId("omnichannel-message-viewport")
    .waitFor({ state: "visible", timeout });
}

async function selectNativeContact(page, origin, contactID, timeout) {
  const target = page.locator(
    '[data-testid="chat-contact"][data-contact-id="' + contactID + '"]',
  );
  await target.first().waitFor({ state: "visible", timeout });
  if ((await target.count()) !== 1) fail("synthetic contact binding differs");
  await target.click({ timeout });
  await page.waitForURL(
    (url) => isExactProductUrl(url, origin, "/chat/" + contactID),
    { timeout },
  );
  await page
    .getByTestId("chat-message-list")
    .waitFor({ state: "visible", timeout });
}

async function scrollMetrics(locator) {
  return locator.evaluate((root) => {
    const ancestors = [];
    let parent = root.parentElement;
    while (parent && ancestors.length < 6) {
      ancestors.push(parent);
      parent = parent.parentElement;
    }
    const candidates = [root, ...ancestors, ...root.querySelectorAll("*")];
    const scrollable = candidates.find((node) => {
      if (!(node instanceof HTMLElement)) return false;
      const style = getComputedStyle(node);
      return /(auto|scroll)/u.test(style.overflowY) && node.clientHeight > 0;
    });
    const node = scrollable instanceof HTMLElement ? scrollable : root;
    if (!(node instanceof HTMLElement))
      throw new Error("scroll viewport is unavailable");
    return {
      clientHeight: node.clientHeight,
      scrollHeight: node.scrollHeight,
      scrollTop: node.scrollTop,
    };
  });
}

async function scrollToBottom(locator) {
  await locator.evaluate((root) => {
    const ancestors = [];
    let parent = root.parentElement;
    while (parent && ancestors.length < 6) {
      ancestors.push(parent);
      parent = parent.parentElement;
    }
    const candidates = [root, ...ancestors, ...root.querySelectorAll("*")];
    const scrollable = candidates.find((node) => {
      if (!(node instanceof HTMLElement)) return false;
      const style = getComputedStyle(node);
      return /(auto|scroll)/u.test(style.overflowY) && node.clientHeight > 0;
    });
    const node = scrollable instanceof HTMLElement ? scrollable : root;
    if (!(node instanceof HTMLElement))
      throw new Error("scroll viewport is unavailable");
    node.scrollTop = node.scrollHeight;
    node.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
}

function metricsAtBottom(metrics) {
  return (
    metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight <=
    BOTTOM_TOLERANCE_PX
  );
}

async function requireAtBottom(locator) {
  const metrics = await scrollMetrics(locator);
  if (!metricsAtBottom(metrics))
    fail("message viewport is not at the latest message");
}

async function waitForApiMessage(
  page,
  conversationID,
  wamid,
  direction,
  timeout,
) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const response = await pageFetch(
      page,
      "/api/conversations/" +
        encodeURIComponent(conversationID) +
        "/messages?limit=100",
    );
    if (response.status === 200 && !response.invalid) {
      const records = unwrapList(response.json, "messages");
      for (const record of records) {
        const message =
          isPlainObject(record) && isPlainObject(record.message)
            ? record.message
            : record;
        if (
          isPlainObject(message) &&
          message.whatsapp_message_id === wamid &&
          message.direction === direction &&
          typeof message.id === "string" &&
          UUID_RE.test(message.id)
        ) {
          return message.id;
        }
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  fail("synthetic message did not become durable");
}

async function waitForApiMessageBody(
  page,
  conversationID,
  body,
  direction,
  timeout,
) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const response = await pageFetch(
      page,
      "/api/conversations/" +
        encodeURIComponent(conversationID) +
        "/messages?limit=100",
    );
    if (response.status === 200 && !response.invalid) {
      const records = unwrapList(response.json, "messages");
      for (const record of records) {
        const message =
          isPlainObject(record) && isPlainObject(record.message)
            ? record.message
            : record;
        if (
          isPlainObject(message) &&
          message.content === body &&
          message.direction === direction &&
          typeof message.id === "string" &&
          UUID_RE.test(message.id)
        ) {
          return message.id;
        }
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  fail("synthetic outbound message did not become durable");
}

async function messageIDs(page, conversationID) {
  const response = await pageFetch(
    page,
    "/api/conversations/" +
      encodeURIComponent(conversationID) +
      "/messages?limit=100",
  );
  if (response.status !== 200 || response.invalid)
    fail("message inventory is unavailable");
  return unwrapList(response.json, "messages")
    .map((record) =>
      isPlainObject(record) && isPlainObject(record.message)
        ? record.message
        : record,
    )
    .map((message) => (isPlainObject(message) ? message.id : null))
    .filter((value) => typeof value === "string" && UUID_RE.test(value))
    .sort();
}

async function attentionCount(page, organizationID) {
  const response = await pageFetch(
    page,
    "/api/conversations/attention-summary",
    {
      headers: { "X-Organization-ID": organizationID },
    },
  );
  if (response.status !== 200 || response.invalid)
    fail("unread summary is unavailable");
  const value = unwrapData(response.json);
  if (
    !isPlainObject(value) ||
    !Number.isSafeInteger(value.unread_conversations)
  ) {
    fail("unread summary differs");
  }
  return value.unread_conversations;
}

async function pollValue(read, predicate, timeout, label) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await read();
    if (predicate(value)) return value;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  fail(label);
}

async function readBoundedResponse(response, maximum) {
  const declared = response.headers.get("content-length");
  if (
    declared !== null &&
    (!/^[0-9]+$/u.test(declared) || Number(declared) > maximum)
  ) {
    await response.body?.cancel();
    fail("webhook response is too large");
  }
  if (!response.body) return;
  const reader = response.body.getReader();
  let total = 0;
  let complete = false;
  while (!complete) {
    const { done, value } = await reader.read();
    complete = done;
    if (complete) return;
    total += value.byteLength;
    if (total > maximum) {
      await reader.cancel();
      fail("webhook response is too large");
    }
  }
}

async function sendWebhook(origin, workspaceID, signed, executionSignal) {
  const url = new URL("/api/webhook", origin);
  url.searchParams.set("workspace", workspaceID);
  const timeoutSignal = AbortSignal.timeout(DEFAULT_TIMEOUT_MS);
  const signal = AbortSignal.any([executionSignal, timeoutSignal]);
  const response = await fetch(url, {
    method: "POST",
    redirect: "error",
    cache: "no-store",
    signal,
    headers: {
      "Content-Type": "application/json",
      "X-Hub-Signature-256": signed.signature,
    },
    body: signed.payload,
  });
  await readBoundedResponse(response, MAX_WEBHOOK_RESPONSE_BYTES);
  if (response.status !== 200) fail("signed synthetic webhook was rejected");
}

function responseMessage(value) {
  if (!isPlainObject(value) || typeof value.message !== "string") return "";
  return value.message;
}

export class LiveProductScenario {
  constructor({
    descriptor,
    klinikLogin,
    nonKlinikLogin,
    metaAppSecret,
    nonce,
    now,
    chromium,
  }) {
    this.descriptor = descriptor;
    this.klinikLogin = klinikLogin;
    this.nonKlinikLogin = nonKlinikLogin;
    this.metaAppSecret = metaAppSecret;
    this.nonce = nonce;
    this.now = now;
    this.chromium = chromium;
    this.timeout = DEFAULT_TIMEOUT_MS;
    this.browser = null;
    this.contexts = [];
    this.pages = {};
    this.state = {};
    this.abortController = new AbortController();
    this.closePromise = null;
  }

  async prepare() {
    this.browser = await this.chromium.launch({ headless: true });
    const createPage = async (loginValue, organizationID) => {
      const context = await this.browser.newContext({
        viewport: { width: 1440, height: 900 },
        acceptDownloads: false,
        serviceWorkers: "block",
      });
      this.contexts.push(context);
      const page = await context.newPage();
      page.setDefaultTimeout(this.timeout);
      page.setDefaultNavigationTimeout(this.timeout);
      await login(
        page,
        this.descriptor.product_origin,
        loginValue,
        organizationID,
        this.timeout,
      );
      return page;
    };
    [
      this.pages.omniPrimary,
      this.pages.omniObserver,
      this.pages.native,
      this.pages.nonKlinik,
    ] = await Promise.all([
      createPage(this.klinikLogin, this.descriptor.klinik.organization_id),
      createPage(this.klinikLogin, this.descriptor.klinik.organization_id),
      createPage(this.klinikLogin, this.descriptor.klinik.organization_id),
      createPage(
        this.nonKlinikLogin,
        this.descriptor.non_klinik.organization_id,
      ),
    ]);

    const [liveA, liveB] = await Promise.all([
      loadLiveFixtureConversation(
        this.pages.omniPrimary,
        this.descriptor,
        this.descriptor.klinik.conversations.a,
      ),
      loadLiveFixtureConversation(
        this.pages.omniPrimary,
        this.descriptor,
        this.descriptor.klinik.conversations.b,
      ),
    ]);
    if (liveB.unread_count !== 0) {
      fail("synthetic unread fixture is not initially clear");
    }
    this.state.liveA = liveA;
    this.state.liveB = liveB;

    const conversationA = this.descriptor.klinik.conversations.a;
    await Promise.all([
      this.pages.omniPrimary.goto(this.descriptor.product_origin + "/inbox", {
        waitUntil: "domcontentloaded",
      }),
      this.pages.omniObserver.goto(this.descriptor.product_origin + "/inbox", {
        waitUntil: "domcontentloaded",
      }),
      this.pages.native.goto(
        this.descriptor.product_origin + "/chat/" + conversationA.contact_id,
        {
          waitUntil: "domcontentloaded",
        },
      ),
    ]);
    await Promise.all([
      selectOmnichannelConversation(
        this.pages.omniPrimary,
        conversationA.conversation_id,
        this.timeout,
      ),
      selectOmnichannelConversation(
        this.pages.omniObserver,
        conversationA.conversation_id,
        this.timeout,
      ),
      this.pages.native.getByTestId("chat-message-list").waitFor({
        state: "visible",
        timeout: this.timeout,
      }),
    ]);
    this.state.omniObserverDocument = await documentCount(
      this.pages.omniObserver,
    );
    this.state.nativeDocument = await documentCount(this.pages.native);
  }

  async close() {
    if (!this.closePromise) {
      this.closePromise = (async () => {
        this.abort();
        await Promise.allSettled(
          this.contexts.map((context) => context.close()),
        );
        if (this.browser) await this.browser.close().catch(() => {});
      })();
    }
    await this.closePromise;
  }

  abort() {
    this.abortController.abort();
  }

  async klinik_whatsapp_inbound() {
    const fixture = this.descriptor.klinik.conversations.a;
    const wamid = "wamid.crmcanary." + this.nonce.slice(0, 40) + ".a";
    const body = ("crm canary layout " + this.nonce.slice(0, 12) + " ")
      .repeat(72)
      .trim();
    const signed = buildSignedWebhook(
      this.descriptor,
      fixture,
      wamid,
      body,
      this.metaAppSecret,
      this.now,
    );
    await sendWebhook(
      this.descriptor.product_origin,
      this.descriptor.klinik.organization_id,
      signed,
      this.abortController.signal,
    );
    const messageID = await waitForApiMessage(
      this.pages.omniPrimary,
      fixture.conversation_id,
      wamid,
      "incoming",
      this.timeout,
    );
    const refreshedFixture = await loadLiveFixtureConversation(
      this.pages.omniPrimary,
      this.descriptor,
      fixture,
    );
    validateRefreshedServiceWindow(refreshedFixture, new Date());
    await this.pages.omniPrimary
      .locator(
        '[data-testid="omnichannel-message"][data-message-id="' +
          messageID +
          '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    this.state.inboundAID = messageID;
    return true;
  }

  async omnichannel_inbound_realtime_without_reload() {
    const messageID = this.state.inboundAID;
    if (!messageID) fail("inbound realtime dependency is unavailable");
    await this.pages.omniObserver
      .locator(
        '[data-testid="omnichannel-message"][data-message-id="' +
          messageID +
          '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    if (
      (await documentCount(this.pages.omniObserver)) !==
      this.state.omniObserverDocument
    ) {
      fail("Omnichannel reloaded while receiving an inbound message");
    }
    return true;
  }

  async native_chat_realtime_without_reload() {
    const messageID = this.state.inboundAID;
    if (!messageID) fail("native realtime dependency is unavailable");
    await this.pages.native
      .locator(
        '[data-testid="chat-message"][data-message-id="' + messageID + '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    if (
      (await documentCount(this.pages.native)) !== this.state.nativeDocument
    ) {
      fail("native Chat reloaded while receiving a message");
    }
    return true;
  }

  async klinik_whatsapp_outbound() {
    const fixture = this.descriptor.klinik.conversations.a;
    const live = await loadLiveFixtureConversation(
      this.pages.omniPrimary,
      this.descriptor,
      fixture,
    );
    validateRefreshedServiceWindow(live, new Date());
    const body = "crm-canary-outbound-" + this.nonce.slice(0, 24);
    const responsePromise = this.pages.omniPrimary.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        isExactProductUrl(
          response.url(),
          this.descriptor.product_origin,
          "/api/conversations/" +
            fixture.conversation_id +
            "/legacy-whatsapp-replies",
        ),
      { timeout: this.timeout },
    );
    await this.pages.omniPrimary
      .getByTestId("omnichannel-reply-composer")
      .fill(body);
    await this.pages.omniPrimary.getByTestId("omnichannel-send-reply").click();
    const response = await responsePromise;
    if (response.status() !== 200)
      fail("Klinik WhatsApp outbound request failed");
    const messageID = await waitForApiMessageBody(
      this.pages.omniPrimary,
      fixture.conversation_id,
      body,
      "outgoing",
      this.timeout,
    );
    await this.pages.omniPrimary
      .locator(
        '[data-testid="omnichannel-message"][data-message-id="' +
          messageID +
          '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    this.state.outboundID = messageID;
    return true;
  }

  async omnichannel_outbound_realtime_without_reload() {
    const messageID = this.state.outboundID;
    if (!messageID) fail("outbound realtime dependency is unavailable");
    await this.pages.omniObserver
      .locator(
        '[data-testid="omnichannel-message"][data-message-id="' +
          messageID +
          '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    if (
      (await documentCount(this.pages.omniObserver)) !==
      this.state.omniObserverDocument
    ) {
      fail("Omnichannel reloaded while receiving an outbound message");
    }
    return true;
  }

  async navbar_unread_increment() {
    const fixture = this.descriptor.klinik.conversations.b;
    const before = await attentionCount(
      this.pages.omniObserver,
      this.descriptor.klinik.organization_id,
    );
    const fixtureBefore = await loadLiveFixtureConversation(
      this.pages.omniObserver,
      this.descriptor,
      fixture,
    );
    if (fixtureBefore.unread_count !== 0) {
      fail("synthetic unread fixture is not clear before injection");
    }
    const wamid = "wamid.crmcanary." + this.nonce.slice(0, 40) + ".b";
    const body = "crm-canary-unread-" + this.nonce.slice(0, 24);
    const signed = buildSignedWebhook(
      this.descriptor,
      fixture,
      wamid,
      body,
      this.metaAppSecret,
      this.now,
    );
    await sendWebhook(
      this.descriptor.product_origin,
      this.descriptor.klinik.organization_id,
      signed,
      this.abortController.signal,
    );
    const messageID = await waitForApiMessage(
      this.pages.omniObserver,
      fixture.conversation_id,
      wamid,
      "incoming",
      this.timeout,
    );
    const after = await pollValue(
      () =>
        attentionCount(
          this.pages.omniObserver,
          this.descriptor.klinik.organization_id,
        ),
      (value) => value === before + 1,
      this.timeout,
      "navbar unread count did not increment",
    );
    const fixtureAfter = await pollValue(
      () =>
        loadLiveFixtureConversation(
          this.pages.omniObserver,
          this.descriptor,
          fixture,
        ),
      (value) => value.unread_count === 1,
      this.timeout,
      "exact synthetic conversation unread count did not increment",
    );
    const badge = this.pages.omniObserver.getByTestId(
      "omnichannel-desktop-nav-unread-badge",
    );
    await badge.waitFor({ state: "visible", timeout: this.timeout });
    const expected = after > 99 ? "99+" : String(after);
    await pollValue(
      async () => (await badge.textContent())?.trim(),
      (value) => value === expected,
      this.timeout,
      "navbar unread badge differs",
    );
    this.state.unreadBefore = before;
    this.state.unreadAfter = after;
    this.state.inboundBID = messageID;
    this.state.fixtureUnreadAfter = fixtureAfter.unread_count;
    return true;
  }

  async navbar_unread_clear() {
    const fixture = this.descriptor.klinik.conversations.b;
    const readResponsePromise = this.pages.omniObserver.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        isExactProductUrl(
          response.url(),
          this.descriptor.product_origin,
          "/api/conversations/" + fixture.conversation_id + "/read",
        ),
      { timeout: this.timeout },
    );
    await selectOmnichannelConversation(
      this.pages.omniObserver,
      fixture.conversation_id,
      this.timeout,
    );
    await this.pages.omniObserver
      .locator(
        '[data-testid="omnichannel-message"][data-message-id="' +
          this.state.inboundBID +
          '"]',
      )
      .waitFor({ state: "visible", timeout: this.timeout });
    await scrollToBottom(
      this.pages.omniObserver.getByTestId("omnichannel-message-viewport"),
    );
    const readResponse = await readResponsePromise;
    if (readResponse.status() !== 200)
      fail("synthetic read acknowledgement failed");
    let readRequest;
    try {
      readRequest = readResponse.request().postDataJSON();
    } catch {
      fail("synthetic read request differs");
    }
    const organizationHeader = await readResponse
      .request()
      .headerValue("x-organization-id");
    if (
      !isPlainObject(readRequest) ||
      Object.keys(readRequest).length !== 1 ||
      readRequest.last_visible_message_id !== this.state.inboundBID ||
      organizationHeader !== this.descriptor.klinik.organization_id
    ) {
      fail("synthetic read request differs");
    }
    const cleared = await pollValue(
      () =>
        attentionCount(
          this.pages.omniObserver,
          this.descriptor.klinik.organization_id,
        ),
      (value) => value === this.state.unreadBefore,
      this.timeout,
      "navbar unread count did not clear",
    );
    const fixtureCleared = await pollValue(
      () =>
        loadLiveFixtureConversation(
          this.pages.omniObserver,
          this.descriptor,
          fixture,
        ),
      (value) => value.unread_count === 0,
      this.timeout,
      "exact synthetic conversation unread count did not clear",
    );
    if (
      this.state.fixtureUnreadAfter !== 1 ||
      fixtureCleared.unread_count !== 0
    ) {
      fail("exact synthetic unread transition differs");
    }
    const badge = this.pages.omniObserver.getByTestId(
      "omnichannel-desktop-nav-unread-badge",
    );
    if (cleared === 0) {
      await badge.waitFor({ state: "detached", timeout: this.timeout });
    } else {
      const expected = cleared > 99 ? "99+" : String(cleared);
      await pollValue(
        async () => (await badge.textContent())?.trim(),
        (value) => value === expected,
        this.timeout,
        "navbar unread clear display differs",
      );
    }
    return true;
  }

  async omnichannel_conversation_switch_autoscroll() {
    const a = this.descriptor.klinik.conversations.a;
    const b = this.descriptor.klinik.conversations.b;
    await selectOmnichannelConversation(
      this.pages.omniPrimary,
      b.conversation_id,
      this.timeout,
    );
    await requireAtBottom(
      this.pages.omniPrimary.getByTestId("omnichannel-message-viewport"),
    );
    await selectOmnichannelConversation(
      this.pages.omniPrimary,
      a.conversation_id,
      this.timeout,
    );
    await requireAtBottom(
      this.pages.omniPrimary.getByTestId("omnichannel-message-viewport"),
    );
    return true;
  }

  async omnichannel_late_layout_autoscroll() {
    const viewport = this.pages.omniPrimary.getByTestId(
      "omnichannel-message-viewport",
    );
    await scrollToBottom(viewport);
    const before = await scrollMetrics(viewport);
    await this.pages.omniPrimary.setViewportSize({ width: 860, height: 900 });
    await this.pages.omniPrimary.waitForTimeout(500);
    const after = await scrollMetrics(viewport);
    if (
      after.scrollHeight <= before.scrollHeight + 0.5 ||
      !metricsAtBottom(after)
    ) {
      fail("Omnichannel late layout did not preserve the latest message");
    }
    return true;
  }

  async native_chat_conversation_switch_autoscroll() {
    const a = this.descriptor.klinik.conversations.a;
    const b = this.descriptor.klinik.conversations.b;
    await selectNativeContact(
      this.pages.native,
      this.descriptor.product_origin,
      b.contact_id,
      this.timeout,
    );
    await requireAtBottom(this.pages.native.getByTestId("chat-message-list"));
    await selectNativeContact(
      this.pages.native,
      this.descriptor.product_origin,
      a.contact_id,
      this.timeout,
    );
    await requireAtBottom(this.pages.native.getByTestId("chat-message-list"));
    return true;
  }

  async native_chat_late_layout_autoscroll() {
    const viewport = this.pages.native.getByTestId("chat-message-list");
    await scrollToBottom(viewport);
    const before = await scrollMetrics(viewport);
    await this.pages.native.setViewportSize({ width: 860, height: 900 });
    await this.pages.native.waitForTimeout(500);
    const after = await scrollMetrics(viewport);
    if (
      after.scrollHeight <= before.scrollHeight + 0.5 ||
      !metricsAtBottom(after)
    ) {
      fail("native Chat late layout did not preserve the latest message");
    }
    return true;
  }

  async requireDenied(organizationID, expectedStatus, expectedMessage) {
    const conversationID =
      this.descriptor.klinik.conversations.a.conversation_id;
    const before = await messageIDs(this.pages.omniPrimary, conversationID);
    const response = await pageFetch(
      this.pages.nonKlinik,
      "/api/conversations/" + conversationID + "/legacy-whatsapp-replies",
      {
        method: "POST",
        headers: { "X-Organization-ID": organizationID },
        body: {
          idempotency_key: "crm-canary-denied-" + this.nonce,
          type: "text",
          content: { body: "crm-canary-denied-" + this.nonce.slice(0, 24) },
        },
      },
    );
    if (
      response.status !== expectedStatus ||
      response.invalid ||
      responseMessage(response.json) !== expectedMessage
    ) {
      fail("synthetic denial boundary differs");
    }
    const after = await messageIDs(this.pages.omniPrimary, conversationID);
    if (JSON.stringify(after) !== JSON.stringify(before)) {
      fail("denied request changed the message inventory");
    }
  }

  async non_klinik_send_denied() {
    await this.requireDenied(
      this.descriptor.non_klinik.organization_id,
      404,
      "WhatsApp Omnichannel replies are not enabled",
    );
    return true;
  }

  async cross_organization_send_denied() {
    await this.requireDenied(
      this.descriptor.klinik.organization_id,
      403,
      "Selected organization is not available",
    );
    return true;
  }
}

export async function executeCheckPlan(scenario) {
  if (
    !scenario ||
    typeof scenario.prepare !== "function" ||
    typeof scenario.close !== "function"
  ) {
    fail("synthetic scenario contract differs");
  }
  const checks = Object.fromEntries(UI_CHECKS.map((key) => [key, false]));
  try {
    await scenario.prepare();
    for (const key of CHECK_EXECUTION_ORDER) {
      if (typeof scenario[key] !== "function")
        fail("synthetic scenario contract differs");
      if ((await scenario[key]()) !== true) fail("synthetic CRM check failed");
      checks[key] = true;
    }
  } finally {
    await scenario.close();
  }
  if (
    Object.keys(checks).length !== UI_CHECKS.length ||
    UI_CHECKS.some((key) => checks[key] !== true)
  ) {
    fail("synthetic CRM check set differs");
  }
  return checks;
}

export async function executeCheckPlanWithinDeadline(
  scenario,
  timeoutMilliseconds = DRIVER_EXECUTION_TIMEOUT_MS,
) {
  if (
    !Number.isSafeInteger(timeoutMilliseconds) ||
    timeoutMilliseconds < 1 ||
    timeoutMilliseconds > DRIVER_EXECUTION_TIMEOUT_MS
  ) {
    fail("driver execution deadline is invalid");
  }
  if (
    !scenario ||
    typeof scenario.abort !== "function" ||
    typeof scenario.close !== "function"
  ) {
    fail("synthetic scenario deadline contract differs");
  }
  let deadlineTimer;
  const deadline = new Promise((_, reject) => {
    deadlineTimer = setTimeout(() => {
      try {
        Promise.resolve(scenario.abort()).catch(() => {});
      } catch {
        // Cleanup authority is best effort after the hard deadline. The
        // externally observed rejection must not wait for browser teardown.
      }
      let cleanupTimer;
      const cleanupGrace = new Promise((resolve) => {
        cleanupTimer = setTimeout(resolve, DEADLINE_CLEANUP_GRACE_MS);
        cleanupTimer.unref?.();
      });
      const cleanup = Promise.resolve()
        .then(() => scenario.close())
        .catch(() => {});
      void Promise.race([cleanup, cleanupGrace]).finally(() => {
        clearTimeout(cleanupTimer);
      });
      reject(new SyntheticCanaryFailure("driver deadline exceeded"));
    }, timeoutMilliseconds);
  });
  try {
    return await Promise.race([executeCheckPlan(scenario), deadline]);
  } finally {
    clearTimeout(deadlineTimer);
  }
}

export async function executeCanary(config, dependencies = {}) {
  const descriptor = validateFixtureDescriptor(config.descriptor);
  const klinikLogin = validateLogin(config.klinikLogin, "Klinik login");
  const nonKlinikLogin = validateLogin(
    config.nonKlinikLogin,
    "non-Klinik login",
  );
  const nonce = exactString(config.nonce, "canary nonce", SHA256_RE, 64);
  const metaAppSecret = exactString(
    config.metaAppSecret,
    "Meta app secret",
    /^[\x21-\x7e]+$/u,
    256,
  );
  if (metaAppSecret.length < 16) fail("Meta app secret is invalid");
  const now =
    config.now instanceof Date && Number.isFinite(config.now.getTime())
      ? config.now
      : new Date();
  await validatePublicProductOrigin(
    descriptor.product_origin,
    dependencies.dnsLookup || defaultDnsLookup,
  );
  let chromium = dependencies.chromium;
  if (!chromium) {
    const playwright = await import("playwright");
    chromium = playwright.chromium;
  }
  const scenario = new LiveProductScenario({
    descriptor,
    klinikLogin,
    nonKlinikLogin,
    metaAppSecret,
    nonce,
    now,
    chromium,
  });
  return executeCheckPlanWithinDeadline(scenario);
}
