/* eslint-env node */

import assert from "node:assert/strict";
import { createHash, createHmac } from "node:crypto";
import { EventEmitter } from "node:events";
import http from "node:http";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

import {
  DriverRequestError,
  canonicalJson,
  createHttpHandler,
  loadRuntimeConfig,
  parseCanonicalJson,
  processAuthenticatedRequest,
  requestHmacPayload,
  startDriver,
} from "./index.mjs";
import {
  UI_CHECKS,
  LiveProductScenario,
  SyntheticCanaryFailure,
  buildSignedWebhook,
  executeCheckPlan,
  executeCheckPlanWithinDeadline,
  isExactProductUrl,
  validateLiveFixtureConversation,
  validateFixtureDescriptor,
  validatePublicProductOrigin,
  validateRefreshedServiceWindow,
} from "./runner.mjs";

const NOW = new Date("2026-08-30T01:02:03Z");
const HMAC_KEY = Buffer.alloc(32, 0x5a);
const DRIVER_VERSION = "1".repeat(64);
const CONTROL_SHA = "2".repeat(40);
const RECEIPT_SHA = "3".repeat(64);
const NONCE = "4".repeat(64);

const descriptor = {
  schema_version: 1,
  product_origin: "https://app.rereply.app",
  fixture_namespace: "rereply-canary",
  klinik: {
    organization_id: "11111111-1111-4111-8111-111111111111",
    conversations: {
      a: {
        conversation_id: "22222222-2222-4222-8222-222222222222",
        contact_id: "33333333-3333-4333-8333-333333333333",
        display_name: "rereply-canary-contact-a",
        sender_wa_id: "60111111111",
      },
      b: {
        conversation_id: "44444444-4444-4444-8444-444444444444",
        contact_id: "55555555-5555-4555-8555-555555555555",
        display_name: "rereply-canary-contact-b",
        sender_wa_id: "60222222222",
      },
    },
    meta: {
      business_account_id: "100000000001",
      phone_number_id: "100000000002",
      display_phone_number: "60333333333",
      channel_account_id: "77777777-7777-4777-8777-777777777777",
      legacy_account_id: "88888888-8888-4888-8888-888888888888",
      legacy_account_name: "Rereply Canary",
    },
  },
  non_klinik: {
    organization_id: "66666666-6666-4666-8666-666666666666",
  },
};
const FIXTURE_DESCRIPTOR_SHA = createHash("sha256")
  .update(canonicalJson(descriptor))
  .digest("hex");

function liveConversation(fixture, unreadCount = 0) {
  const meta = descriptor.klinik.meta;
  return {
    id: fixture.conversation_id,
    organization_id: descriptor.klinik.organization_id,
    channel_account_id: meta.channel_account_id,
    channel_account: {
      id: meta.channel_account_id,
      organization_id: descriptor.klinik.organization_id,
      channel: "whatsapp",
      provider: "meta_legacy",
      name: "WhatsApp Rereply Canary [" + meta.legacy_account_id + "]",
      external_account_id: "legacy-account:" + meta.legacy_account_id,
      status: "active",
      has_credentials: false,
      capabilities: {
        text: true,
        replies: true,
        service_window: true,
        legacy_text_reply_endpoint: true,
      },
      config: {
        legacy_read_only: true,
        outbound_enabled: false,
        reply_route: "chat",
      },
    },
    contact_id: fixture.contact_id,
    contact: {
      id: fixture.contact_id,
      organization_id: descriptor.klinik.organization_id,
      phone_number: fixture.sender_wa_id,
      profile_name: fixture.display_name,
      whatsapp_account: meta.legacy_account_name,
    },
    contact_identity: {
      organization_id: descriptor.klinik.organization_id,
      contact_id: fixture.contact_id,
      channel_account_id: meta.channel_account_id,
      channel: "whatsapp",
      external_id: "legacy-contact:" + fixture.contact_id,
      address: fixture.sender_wa_id,
      normalized_address: fixture.sender_wa_id,
      display_name: fixture.display_name,
      is_primary: true,
      is_verified: true,
    },
    channel: "whatsapp",
    external_conversation_id: "legacy-contact:" + fixture.contact_id,
    unread_count: unreadCount,
    service_window_ends_at: "2026-08-31T01:02:03Z",
  };
}

const klinikLogin = {
  schema_version: 1,
  email: "klinik-canary@example.invalid",
  password: "Klinik-only-password!",
};

const nonKlinikLogin = {
  schema_version: 1,
  email: "non-klinik-canary@example.invalid",
  password: "Non-klinik-password!",
};

function requestValue(overrides = {}) {
  return {
    schema_version: 1,
    authority: "rereply-controlled-synthetic-crm-request",
    phase: "ui",
    nonce: NONCE,
    idempotency_key: NONCE,
    control_sha: CONTROL_SHA,
    change_receipt_sha256: RECEIPT_SHA,
    driver_version_sha256: DRIVER_VERSION,
    fixture_descriptor_sha256: FIXTURE_DESCRIPTOR_SHA,
    ...overrides,
  };
}

function requestParts(
  value = requestValue(),
  timestamp = "2026-08-30T01:02:03Z",
) {
  const rawBody = canonicalJson(value);
  return {
    rawBody,
    headers: {
      "content-type": "application/json",
      "x-rereply-canary-timestamp": timestamp,
      "x-rereply-canary-signature": createHmac("sha256", HMAC_KEY)
        .update(requestHmacPayload(timestamp, rawBody))
        .digest("hex"),
    },
  };
}

function passingChecks() {
  return Object.fromEntries(UI_CHECKS.map((key) => [key, true]));
}

function runtimeConfig() {
  return {
    hmacKey: HMAC_KEY,
    driverVersionSha256: DRIVER_VERSION,
    fixtureDescriptorSha256: FIXTURE_DESCRIPTOR_SHA,
    descriptor,
    klinikLogin,
    nonKlinikLogin,
    metaAppSecret: "dedicated-synthetic-meta-secret",
  };
}

class MemoryLedger {
  constructor() {
    this.records = new Map();
  }

  async claim(idempotencyKey, requestSha256) {
    const existing = this.records.get(idempotencyKey);
    if (!existing) {
      this.records.set(idempotencyKey, {
        requestSha256,
        state: "running",
        responseBody: null,
      });
      return { kind: "new" };
    }
    if (existing.requestSha256 !== requestSha256) return { kind: "mismatch" };
    if (existing.state === "complete") {
      return { kind: "complete", responseBody: existing.responseBody };
    }
    return { kind: existing.state };
  }

  async complete(idempotencyKey, requestSha256, responseBody) {
    const record = this.records.get(idempotencyKey);
    assert.equal(record?.requestSha256, requestSha256);
    assert.equal(record?.state, "running");
    record.state = "complete";
    record.responseBody = responseBody;
  }

  async fail(idempotencyKey, requestSha256) {
    const record = this.records.get(idempotencyKey);
    if (record?.requestSha256 === requestSha256 && record.state === "running") {
      record.state = "failed";
    }
  }
}

async function assertRequestRejects(parts, expectedStatus) {
  await assert.rejects(
    processAuthenticatedRequest({
      ...parts,
      config: runtimeConfig(),
      ledger: new MemoryLedger(),
      executor: async () => passingChecks(),
      clock: () => NOW,
    }),
    (error) => {
      assert.ok(error instanceof DriverRequestError);
      if (expectedStatus) assert.equal(error.status, expectedStatus);
      return true;
    },
  );
}

test("canonical JSON is stable and strict", () => {
  const value = { z: ["é", 1, true], a: { y: null, x: "ok" } };
  const raw = '{"a":{"x":"ok","y":null},"z":["\\u00e9",1,true]}';
  assert.equal(canonicalJson(value), raw);
  assert.deepEqual(parseCanonicalJson(raw), value);
  assert.throws(() => parseCanonicalJson(' {"a":1}'), DriverRequestError);
  assert.throws(() => parseCanonicalJson('{"a":1,"a":1}'), DriverRequestError);
  assert.throws(() => parseCanonicalJson('{"a":1.0}'), DriverRequestError);
});

test("request HMAC has an exact cross-language timestamp-bound vector", () => {
  const timestamp = "2026-08-30T01:02:03Z";
  const body = '{"schema_version":1}';
  assert.equal(
    createHmac("sha256", Buffer.from([...Array(32).keys()]))
      .update(requestHmacPayload(timestamp, body))
      .digest("hex"),
    "cb84422660014e889aa1de606126338f4eab813db4bc5355c8ec29cb7e239a37",
  );
});

test("fixture descriptor is exact, isolated, and synthetic", () => {
  assert.deepEqual(validateFixtureDescriptor(descriptor), descriptor);
  assert.throws(
    () =>
      validateFixtureDescriptor({
        ...descriptor,
        fixture_namespace: "customer-production",
      }),
    SyntheticCanaryFailure,
  );
  assert.throws(
    () => validateFixtureDescriptor({ ...descriptor, extra: true }),
    SyntheticCanaryFailure,
  );
  assert.throws(
    () =>
      validateFixtureDescriptor({
        ...descriptor,
        klinik: {
          ...descriptor.klinik,
          conversations: {
            ...descriptor.klinik.conversations,
            b: {
              ...descriptor.klinik.conversations.b,
              conversation_id:
                descriptor.klinik.conversations.a.conversation_id,
            },
          },
        },
      }),
    SyntheticCanaryFailure,
  );
  assert.throws(
    () =>
      validateFixtureDescriptor({
        ...descriptor,
        klinik: {
          ...descriptor.klinik,
          conversations: {
            ...descriptor.klinik.conversations,
            a: {
              ...descriptor.klinik.conversations.a,
              display_name: "Customer One",
            },
          },
        },
      }),
    SyntheticCanaryFailure,
  );
});

test("product origin is compiled, port-443-only, and public", async () => {
  const publicLookup = async () => [
    { address: "203.0.114.10", family: 4 },
    { address: "2606:4700::6810:1234", family: 6 },
  ];
  assert.equal(
    await validatePublicProductOrigin(descriptor.product_origin, publicLookup),
    descriptor.product_origin,
  );
  await assert.rejects(
    validatePublicProductOrigin("https://example.com", publicLookup),
    SyntheticCanaryFailure,
  );
  await assert.rejects(
    validatePublicProductOrigin(descriptor.product_origin, async () => [
      { address: "127.0.0.1", family: 4 },
    ]),
    SyntheticCanaryFailure,
  );
  await assert.rejects(
    validatePublicProductOrigin(descriptor.product_origin, async () => [
      { address: "fe80::1", family: 6 },
    ]),
    SyntheticCanaryFailure,
  );
  await assert.rejects(
    validatePublicProductOrigin(descriptor.product_origin, async () => [
      { address: "0:0:0:0:0:0:0:1", family: 6 },
    ]),
    SyntheticCanaryFailure,
  );
  await assert.rejects(
    validatePublicProductOrigin(descriptor.product_origin, async () => [
      { address: "::ffff:127.0.0.1", family: 6 },
    ]),
    SyntheticCanaryFailure,
  );
});

test("browser product URL binding rejects a cross-origin or decorated URL", () => {
  const path = "/api/conversations/fixture/read";
  assert.equal(
    isExactProductUrl(
      descriptor.product_origin + path,
      descriptor.product_origin,
      path,
    ),
    true,
  );
  assert.equal(
    isExactProductUrl(
      "https://attacker.invalid" + path,
      descriptor.product_origin,
      path,
    ),
    false,
  );
  assert.equal(
    isExactProductUrl(
      descriptor.product_origin + path + "?next=1",
      descriptor.product_origin,
      path,
    ),
    false,
  );
});

test("live fixture identity is independently bound before mutation", () => {
  const fixture = descriptor.klinik.conversations.a;
  const live = liveConversation(fixture);
  assert.equal(
    validateLiveFixtureConversation(live, descriptor, fixture),
    live,
  );
  for (const mutation of [
    { contact: { ...live.contact, phone_number: "60999999999" } },
    { channel_account: { ...live.channel_account, status: "degraded" } },
    {
      contact_identity: {
        ...live.contact_identity,
        channel_account_id: "99999999-9999-4999-8999-999999999999",
      },
    },
  ]) {
    assert.throws(
      () =>
        validateLiveFixtureConversation(
          { ...live, ...mutation },
          descriptor,
          fixture,
        ),
      SyntheticCanaryFailure,
    );
  }
});

test("expired fixture is accepted before inbound but refreshed before outbound", () => {
  const fixture = descriptor.klinik.conversations.a;
  const expired = {
    ...liveConversation(fixture),
    service_window_ends_at: "2026-08-29T01:02:03Z",
  };
  assert.equal(
    validateLiveFixtureConversation(expired, descriptor, fixture),
    expired,
  );
  assert.throws(
    () => validateRefreshedServiceWindow(expired, NOW),
    SyntheticCanaryFailure,
  );
  assert.equal(
    validateRefreshedServiceWindow(liveConversation(fixture), NOW),
    true,
  );
  const inboundMethod = LiveProductScenario.prototype.klinik_whatsapp_inbound;
  assert.match(inboundMethod.toString(), /loadLiveFixtureConversation/u);
  assert.match(inboundMethod.toString(), /validateRefreshedServiceWindow/u);
});

test("signed webhook is exact and carries only the dedicated fixture", () => {
  const signed = buildSignedWebhook(
    descriptor,
    descriptor.klinik.conversations.a,
    "wamid.crmcanary.example",
    "synthetic body",
    "dedicated-synthetic-meta-secret",
    NOW,
  );
  const expected =
    "sha256=" +
    createHmac("sha256", "dedicated-synthetic-meta-secret")
      .update(signed.payload)
      .digest("hex");
  assert.equal(signed.signature, expected);
  const payload = JSON.parse(signed.payload);
  assert.equal(payload.entry[0].id, descriptor.klinik.meta.business_account_id);
  assert.equal(
    payload.entry[0].changes[0].value.metadata.phone_number_id,
    descriptor.klinik.meta.phone_number_id,
  );
  assert.equal(
    payload.entry[0].changes[0].value.messages[0].from,
    descriptor.klinik.conversations.a.sender_wa_id,
  );
});

test("mocked product boundary executes every exact CRM check once", async () => {
  const calls = [];
  const scenario = {
    async prepare() {
      calls.push("prepare");
    },
    async close() {
      calls.push("close");
    },
  };
  for (const key of UI_CHECKS) {
    scenario[key] = async () => {
      calls.push(key);
      return true;
    };
  }
  const checks = await executeCheckPlan(scenario);
  assert.deepEqual(checks, passingChecks());
  assert.equal(calls[0], "prepare");
  assert.equal(calls.at(-1), "close");
  assert.deepEqual(new Set(calls.slice(1, -1)), new Set(UI_CHECKS));
  assert.equal(calls.length, UI_CHECKS.length + 2);
});

test("live product scenario implements every exact CRM method", () => {
  const methods = new Set(
    Object.getOwnPropertyNames(LiveProductScenario.prototype).filter(
      (name) => typeof LiveProductScenario.prototype[name] === "function",
    ),
  );
  for (const method of ["prepare", "abort", "close", ...UI_CHECKS]) {
    assert.equal(methods.has(method), true, method);
  }
});

test("global driver deadline closes blocked browser work", async () => {
  let rejectBlocked;
  let closeCount = 0;
  let abortCount = 0;
  const scenario = {
    async prepare() {},
    abort() {
      abortCount += 1;
      rejectBlocked?.(new SyntheticCanaryFailure("browser aborted"));
    },
    async close() {
      closeCount += 1;
    },
  };
  for (const key of UI_CHECKS) scenario[key] = async () => true;
  scenario.klinik_whatsapp_inbound = () =>
    new Promise((_, reject) => {
      rejectBlocked = reject;
    });
  await assert.rejects(
    executeCheckPlanWithinDeadline(scenario, 10),
    SyntheticCanaryFailure,
  );
  assert.equal(abortCount, 1);
  assert.ok(closeCount >= 1);
});

test("global driver deadline rejects even when prepare and cleanup never settle", async () => {
  const never = new Promise(() => {});
  let aborted = false;
  const scenario = {
    prepare: () => never,
    abort() {
      aborted = true;
    },
    close: () => never,
  };
  const unhandled = [];
  const captureUnhandled = (error) => unhandled.push(error);
  process.on("unhandledRejection", captureUnhandled);
  const started = performance.now();
  try {
    await assert.rejects(
      executeCheckPlanWithinDeadline(scenario, 10),
      SyntheticCanaryFailure,
    );
    assert.ok(performance.now() - started < 250);
    assert.equal(aborted, true);
    await new Promise((resolve) => setTimeout(resolve, 25));
    assert.deepEqual(unhandled, []);
  } finally {
    process.removeListener("unhandledRejection", captureUnhandled);
  }
});

test("mocked product boundary fails closed and still closes", async () => {
  let closed = false;
  const scenario = {
    async prepare() {},
    async close() {
      closed = true;
    },
  };
  for (const key of UI_CHECKS) scenario[key] = async () => true;
  scenario.navbar_unread_increment = async () => false;
  await assert.rejects(executeCheckPlan(scenario), SyntheticCanaryFailure);
  assert.equal(closed, true);
});

test("authenticated request returns the exact signed response", async () => {
  const ledger = new MemoryLedger();
  const result = await processAuthenticatedRequest({
    ...requestParts(),
    config: runtimeConfig(),
    ledger,
    executor: async () => passingChecks(),
    clock: () => NOW,
  });
  assert.equal(result.status, 200);
  assert.equal(result.replayed, false);
  const response = parseCanonicalJson(result.body);
  const signature = response.hmac_sha256;
  const signed = { ...response };
  delete signed.hmac_sha256;
  assert.equal(
    signature,
    createHmac("sha256", HMAC_KEY).update(canonicalJson(signed)).digest("hex"),
  );
  assert.deepEqual(response.checks, passingChecks());
  assert.equal(response.execution_count, 1);
  assert.equal(response.nonce, NONCE);
  assert.equal(response.change_receipt_sha256, RECEIPT_SHA);
  assert.equal(response.fixture_descriptor_sha256, FIXTURE_DESCRIPTOR_SHA);
  assert.equal(response.observed_at, "2026-08-30T01:02:03Z");
});

test("same authenticated request replays the durable result without executing twice", async () => {
  const ledger = new MemoryLedger();
  let executions = 0;
  const invoke = () =>
    processAuthenticatedRequest({
      ...requestParts(),
      config: runtimeConfig(),
      ledger,
      executor: async () => {
        executions += 1;
        return passingChecks();
      },
      clock: () => NOW,
    });
  const first = await invoke();
  const second = await invoke();
  assert.equal(executions, 1);
  assert.equal(second.replayed, true);
  assert.equal(second.body, first.body);
  assert.equal(parseCanonicalJson(second.body).execution_count, 1);
});

test("concurrent identical requests admit only one execution", async () => {
  const ledger = new MemoryLedger();
  let executions = 0;
  let releaseExecution;
  const blocked = new Promise((resolve) => {
    releaseExecution = resolve;
  });
  const invoke = () =>
    processAuthenticatedRequest({
      ...requestParts(),
      config: runtimeConfig(),
      ledger,
      executor: async () => {
        executions += 1;
        await blocked;
        return passingChecks();
      },
      clock: () => NOW,
    });
  const first = invoke();
  await new Promise((resolve) => setImmediate(resolve));
  await assert.rejects(
    invoke(),
    (error) => error instanceof DriverRequestError && error.status === 409,
  );
  releaseExecution();
  const result = await first;
  assert.equal(result.replayed, false);
  assert.equal(executions, 1);
});

test("post-commit crash replays the durable signed result without reexecution", async () => {
  class CommitThenThrowLedger extends MemoryLedger {
    constructor() {
      super();
      this.injectCrash = true;
    }

    async complete(idempotencyKey, requestSha256, responseBody) {
      await super.complete(idempotencyKey, requestSha256, responseBody);
      if (this.injectCrash) {
        this.injectCrash = false;
        throw new Error("synthetic post-commit transport failure");
      }
    }
  }
  const ledger = new CommitThenThrowLedger();
  let executions = 0;
  const invoke = () =>
    processAuthenticatedRequest({
      ...requestParts(),
      config: runtimeConfig(),
      ledger,
      executor: async () => {
        executions += 1;
        return passingChecks();
      },
      clock: () => NOW,
    });
  await assert.rejects(
    invoke(),
    (error) => error instanceof DriverRequestError && error.status === 503,
  );
  const replay = await invoke();
  assert.equal(replay.replayed, true);
  assert.equal(executions, 1);
  assert.deepEqual(parseCanonicalJson(replay.body).checks, passingChecks());
});

test("idempotency collision and in-flight replay fail closed", async () => {
  const ledger = new MemoryLedger();
  const original = requestParts();
  await processAuthenticatedRequest({
    ...original,
    config: runtimeConfig(),
    ledger,
    executor: async () => passingChecks(),
    clock: () => NOW,
  });
  const collision = requestParts(requestValue({ control_sha: "5".repeat(40) }));
  await assert.rejects(
    processAuthenticatedRequest({
      ...collision,
      config: runtimeConfig(),
      ledger,
      executor: async () => passingChecks(),
      clock: () => NOW,
    }),
    (error) => error instanceof DriverRequestError && error.status === 409,
  );

  const runningLedger = new MemoryLedger();
  const body = requestParts();
  await runningLedger.claim(NONCE, createHashForTest(body.rawBody));
  await assert.rejects(
    processAuthenticatedRequest({
      ...body,
      config: runtimeConfig(),
      ledger: runningLedger,
      executor: async () => passingChecks(),
      clock: () => NOW,
    }),
    (error) => error instanceof DriverRequestError && error.status === 409,
  );
});

test("a swapped fixture descriptor is rejected before execution", async () => {
  const swappedDescriptor = {
    ...descriptor,
    klinik: {
      ...descriptor.klinik,
      organization_id: "99999999-9999-4999-8999-999999999999",
    },
  };
  let executed = false;
  await assert.rejects(
    processAuthenticatedRequest({
      ...requestParts(),
      config: {
        ...runtimeConfig(),
        descriptor: swappedDescriptor,
        fixtureDescriptorSha256: createHash("sha256")
          .update(canonicalJson(swappedDescriptor))
          .digest("hex"),
      },
      ledger: new MemoryLedger(),
      executor: async () => {
        executed = true;
        return passingChecks();
      },
      clock: () => NOW,
    }),
    DriverRequestError,
  );
  assert.equal(executed, false);
});

function createHashForTest(raw) {
  return createHash("sha256").update(raw).digest("hex");
}

test("request schema, HMAC, freshness, and canonical encoding fail closed", async () => {
  await assertRequestRejects(requestParts(requestValue({ extra: true })));
  await assertRequestRejects(
    {
      ...requestParts(),
      headers: {
        ...requestParts().headers,
        "x-rereply-canary-signature": "0".repeat(64),
      },
    },
    401,
  );
  await assertRequestRejects(
    requestParts(requestValue(), "2026-08-30T00:50:00Z"),
  );
  const timestampRebound = requestParts();
  timestampRebound.headers["x-rereply-canary-timestamp"] =
    "2026-08-30T01:02:02Z";
  await assertRequestRejects(timestampRebound, 401);

  const duplicate =
    '{"authority":"rereply-controlled-synthetic-crm-request",' +
    '"authority":"rereply-controlled-synthetic-crm-request",' +
    '"change_receipt_sha256":"' +
    RECEIPT_SHA +
    '",' +
    '"control_sha":"' +
    CONTROL_SHA +
    '",' +
    '"driver_version_sha256":"' +
    DRIVER_VERSION +
    '",' +
    '"idempotency_key":"' +
    NONCE +
    '",' +
    '"nonce":"' +
    NONCE +
    '","phase":"ui","schema_version":1}';
  const duplicateHeaders = requestParts().headers;
  duplicateHeaders["x-rereply-canary-signature"] = createHmac(
    "sha256",
    HMAC_KEY,
  )
    .update(
      requestHmacPayload(
        duplicateHeaders["x-rereply-canary-timestamp"],
        duplicate,
      ),
    )
    .digest("hex");
  await assertRequestRejects({ rawBody: duplicate, headers: duplicateHeaders });

  const whitespace = " " + requestParts().rawBody;
  const whitespaceHeaders = requestParts().headers;
  whitespaceHeaders["x-rereply-canary-signature"] = createHmac(
    "sha256",
    HMAC_KEY,
  )
    .update(
      requestHmacPayload(
        whitespaceHeaders["x-rereply-canary-timestamp"],
        whitespace,
      ),
    )
    .digest("hex");
  await assertRequestRejects({
    rawBody: whitespace,
    headers: whitespaceHeaders,
  });
});

test("failed execution is durably fenced from replay", async () => {
  const ledger = new MemoryLedger();
  const parts = requestParts();
  await assert.rejects(
    processAuthenticatedRequest({
      ...parts,
      config: runtimeConfig(),
      ledger,
      executor: async () => {
        throw new Error("private product detail");
      },
      clock: () => NOW,
    }),
    (error) => error instanceof DriverRequestError && error.status === 503,
  );
  assert.equal(ledger.records.get(NONCE).state, "failed");
  await assert.rejects(
    processAuthenticatedRequest({
      ...parts,
      config: runtimeConfig(),
      ledger,
      executor: async () => passingChecks(),
      clock: () => NOW,
    }),
    (error) => error instanceof DriverRequestError && error.status === 409,
  );
});

test("runtime configuration is exact and requires dedicated secrets", () => {
  const environment = {
    CRM_CANARY_HMAC_KEY_BASE64: HMAC_KEY.toString("base64"),
    CRM_CANARY_DRIVER_VERSION_SHA256: DRIVER_VERSION,
    CRM_CANARY_LEDGER_DATABASE_URL:
      "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true",
    CRM_CANARY_FIXTURE_DESCRIPTOR_JSON: canonicalJson(descriptor),
    CRM_CANARY_KLINIK_LOGIN_JSON: canonicalJson(klinikLogin),
    CRM_CANARY_NON_KLINIK_LOGIN_JSON: canonicalJson(nonKlinikLogin),
    CRM_CANARY_META_APP_SECRET: "dedicated-synthetic-meta-secret",
    PORT: "8080",
  };
  const config = loadRuntimeConfig(environment);
  assert.equal(config.port, 8080);
  assert.deepEqual(config.descriptor, descriptor);
  assert.deepEqual(config.klinikLogin, klinikLogin);
  assert.deepEqual(config.nonKlinikLogin, nonKlinikLogin);
  assert.equal(config.fixtureDescriptorSha256, FIXTURE_DESCRIPTOR_SHA);
  assert.throws(() =>
    loadRuntimeConfig({ ...environment, CRM_CANARY_HMAC_KEY_BASE64: "AA==" }),
  );
  assert.throws(() =>
    loadRuntimeConfig({
      ...environment,
      CRM_CANARY_FIXTURE_DESCRIPTOR_JSON: canonicalJson({
        ...descriptor,
        extra: true,
      }),
    }),
  );
  for (const ledgerDatabaseUrl of [
    "postgresql://ledger:password@ledger.invalid/canary",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=disable",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=prefer",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=false",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&uselibpqcompat=true",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&sslmode=require&uselibpqcompat=true",
    "postgresql://ledger:password@ledger.invalid/canary?uselibpqcompat=true&sslmode=require",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=verify-ca&uselibpqcompat=true",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=verify-full&uselibpqcompat=true",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&ssl=false",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&host=attacker.invalid",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&port=5432",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&user=other",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&password=other",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&sslcert=client.crt",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&sslkey=client.key",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&sslrootcert=root.crt",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true&unknown=value",
    "postgresql://ledger@ledger.invalid/canary?sslmode=require&uselibpqcompat=true",
    "postgre\tsql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true",
    "postgresql://ledger:pass\tword@ledger.invalid/canary?sslmode=require&uselibpqcompat=true",
    "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&\tuselibpqcompat=true",
  ]) {
    assert.throws(() =>
      loadRuntimeConfig({
        ...environment,
        CRM_CANARY_LEDGER_DATABASE_URL: ledgerDatabaseUrl,
      }),
    );
  }
});

test("HTTP boundary emits only a constant redacted error", async (t) => {
  const ledger = new MemoryLedger();
  const secrets = [
    klinikLogin.password,
    nonKlinikLogin.password,
    "dedicated-synthetic-meta-secret",
    "postgresql://ledger:password@ledger.invalid/canary",
  ];
  const handler = createHttpHandler({
    config: runtimeConfig(),
    ledger,
    executor: async () => passingChecks(),
    clock: () => NOW,
  });
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const address = server.address();
  assert.ok(address && typeof address === "object");
  const sensitiveBody = canonicalJson({ secret: secrets.join("|") });
  const response = await fetch(
    "http://127.0.0.1:" + address.port + "/v1/execute",
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-ReReply-Canary-Timestamp": "2026-08-30T01:02:03Z",
        "X-ReReply-Canary-Signature": "0".repeat(64),
      },
      body: sensitiveBody,
    },
  );
  const body = await response.text();
  assert.equal(response.status, 400);
  assert.equal(body, '{"error":"request rejected"}');
  for (const secret of secrets) assert.equal(body.includes(secret), false);
  assert.equal(
    JSON.stringify([...ledger.records.values()]).includes("password"),
    false,
  );
});

test("driver sources disable sensitive capture and console logging", async () => {
  const indexSource = await readFile(
    new URL("./index.mjs", import.meta.url),
    "utf8",
  );
  const runnerSource = await readFile(
    new URL("./runner.mjs", import.meta.url),
    "utf8",
  );
  for (const source of [indexSource, runnerSource]) {
    assert.equal(/console\s*\./u.test(source), false);
    assert.equal(/\.screenshot\s*\(/u.test(source), false);
    assert.equal(/tracing\s*\./u.test(source), false);
    assert.equal(/recordVideo/u.test(source), false);
  }
  assert.match(runnerSource, /acceptDownloads:\s*false/u);
  assert.match(runnerSource, /serviceWorkers:\s*["']block["']/u);
  assert.match(runnerSource, /AbortSignal\.timeout\(DEFAULT_TIMEOUT_MS\)/u);
  assert.match(runnerSource, /\.body\.getReader\(\)/u);
  assert.match(runnerSource, /DRIVER_EXECUTION_TIMEOUT_MS\s*=\s*210_000/u);
  assert.match(runnerSource, /https:\/\/app\.rereply\.app/u);
  assert.match(
    indexSource,
    /const exactQuery\s*=\s*"\?sslmode=require&uselibpqcompat=true"/u,
  );
  assert.match(indexSource, /raw\.indexOf\("\?"\)\s*!==/u);
  assert.match(indexSource, /\[\\u0000-\\u0020\\u007f\]/u);
  assert.match(
    indexSource,
    /parsed\.searchParams\.getAll\("uselibpqcompat"\)/u,
  );
  assert.equal(
    /DIGITALOCEAN|DO_TOKEN|doctl/u.test(indexSource + runnerSource),
    false,
  );
});

function startupEnvironment() {
  return {
    CRM_CANARY_HMAC_KEY_BASE64: HMAC_KEY.toString("base64"),
    CRM_CANARY_DRIVER_VERSION_SHA256: DRIVER_VERSION,
    CRM_CANARY_LEDGER_DATABASE_URL:
      "postgresql://ledger:password@ledger.invalid/canary?sslmode=require&uselibpqcompat=true",
    CRM_CANARY_FIXTURE_DESCRIPTOR_JSON: canonicalJson(descriptor),
    CRM_CANARY_KLINIK_LOGIN_JSON: canonicalJson(klinikLogin),
    CRM_CANARY_NON_KLINIK_LOGIN_JSON: canonicalJson(nonKlinikLogin),
    CRM_CANARY_META_APP_SECRET: "dedicated-synthetic-meta-secret",
  };
}

const STARTUP_PRIVATE =
  "private-postgresql://user:password@private.invalid/HMAC";

function startupHarness(options = {}) {
  const events = [];
  const lines = [];
  const processLike = new EventEmitter();
  processLike.exitCode = undefined;
  processLike.exit = (code) => events.push(["exit", code]);
  const pool = new EventEmitter();
  pool.query = (...args) => {
    events.push(["query", ...args]);
    return options.query ? options.query() : Promise.resolve({});
  };
  pool.end = () => {
    events.push(["pool.end"]);
    return options.end ? options.end() : Promise.resolve();
  };
  const server = new EventEmitter();
  server.listening = false;
  server.listen = (port, host, callback) => {
    events.push(["listen", port, host]);
    server.finishListen = () => {
      server.listening = true;
      callback();
    };
    if (options.listen) return options.listen(server);
    queueMicrotask(server.finishListen);
  };
  server.close = (callback) => {
    events.push(["server.close"]);
    if (options.close) return options.close(callback, server);
    server.listening = false;
    callback();
  };
  server.closeAllConnections = () =>
    events.push(["server.closeAllConnections"]);
  const dependencies = {
    loadPg: async () => {
      events.push(["loadPg"]);
      if (options.importFailure) throw options.failure;
      return {
        Pool: class {
          constructor(config) {
            events.push(["Pool", config]);
            if (options.poolFailure) throw options.failure;
            return pool;
          }
        },
      };
    },
    createServer: (handler) => {
      events.push(["createServer"]);
      if (options.serverFailure) throw options.failure;
      server.handler = handler;
      return server;
    },
    processLike,
    writeDiagnostic: (line) => {
      lines.push(line);
      if (options.write) return options.write();
    },
    ...(options.timers || {}),
  };
  return { events, lines, processLike, pool, server, dependencies };
}

function startupDiagnostics(harness) {
  const stages = new Set([
    "CONFIG",
    "PG_IMPORT",
    "POOL_CREATE",
    "LEDGER_INITIALIZE",
    "LISTEN",
    "READY",
    "RUNTIME",
    "SERVER_CLEANUP",
    "POOL_CLEANUP",
    "CLEANUP",
  ]);
  const codes = new Set([
    "NONE",
    "UNKNOWN",
    "LEDGER_PERMISSION_DENIED",
    "LEDGER_AUTH_REJECTED",
    "LEDGER_DATABASE_MISSING",
    "TLS_VERIFICATION_FAILED",
    "NAME_RESOLUTION_FAILED",
    "CONNECTION_REFUSED",
    "CONNECTION_TIMEOUT",
  ]);
  return harness.lines.map((line) => {
    assert.equal(line.endsWith("\n"), true);
    const record = JSON.parse(line);
    assert.deepEqual(Object.keys(record), [
      "schema_version",
      "event",
      "stage",
      "outcome",
      "code",
    ]);
    assert.equal(record.schema_version, 1);
    assert.equal(record.event, "crm_canary_driver_startup");
    assert.equal(stages.has(record.stage), true);
    assert.equal(
      ["STARTED", "ERROR", "SUCCESS"].includes(record.outcome),
      true,
    );
    assert.equal(codes.has(record.code), true);
    assert.equal(record.code === "NONE", record.outcome !== "ERROR");
    for (const privateValue of [
      STARTUP_PRIVATE,
      ...Object.values(startupEnvironment()),
      klinikLogin.password,
      nonKlinikLogin.password,
    ])
      assert.equal(line.includes(privateValue), false);
    return [record.stage, record.outcome];
  });
}

async function rejectStartup(harness, environment = startupEnvironment()) {
  await assert.rejects(
    startDriver(environment, harness.dependencies),
    (error) => {
      assert.equal(error instanceof DriverRequestError, true);
      assert.equal(error.message, "driver startup failed");
      assert.equal(error.status, 503);
      assert.equal("cause" in error, false);
      return true;
    },
  );
  assert.equal(
    startupDiagnostics(harness).some(([stage]) => stage === "READY"),
    false,
  );
  assert.equal(harness.processLike.listenerCount("SIGTERM"), 0);
  assert.equal(harness.processLike.listenerCount("SIGINT"), 0);
}

async function startupSettled() {
  // Flush only fake-resource promises; never open a socket or load pg.
  for (let count = 0; count < 30; count += 1) await Promise.resolve();
}

test("startup emits exact stages and stays unready until fake listen completes", async () => {
  const h = startupHarness({ listen: () => {} });
  const started = startDriver(startupEnvironment(), h.dependencies);
  await startupSettled();
  assert.deepEqual(startupDiagnostics(h), [
    ["CONFIG", "STARTED"],
    ["PG_IMPORT", "STARTED"],
    ["POOL_CREATE", "STARTED"],
    ["LEDGER_INITIALIZE", "STARTED"],
    ["LISTEN", "STARTED"],
  ]);
  h.server.finishListen();
  assert.deepEqual(await started, { server: h.server, pool: h.pool });
  assert.deepEqual(startupDiagnostics(h).at(-1), ["READY", "SUCCESS"]);
  assert.deepEqual(
    h.events.map(([event]) => event),
    ["loadPg", "Pool", "query", "createServer", "listen"],
  );
  assert.deepEqual(h.events.at(-1), ["listen", 8080, "0.0.0.0"]);
  assert.match(
    h.events.find(([event]) => event === "query")[1],
    /CREATE TABLE IF NOT EXISTS crm_canary_execution_ledger/u,
  );
  assert.equal(h.processLike.listenerCount("SIGTERM"), 1);
  assert.equal(h.processLike.listenerCount("SIGINT"), 1);
  const response = {
    writeHead: (status) => assert.equal(status, 204),
    end: () => {},
  };
  await h.server.handler({ method: "GET", url: "/healthz" }, response);
  h.processLike.emit("SIGTERM");
  await startupSettled();
  assert.equal(h.processLike.exitCode, 0);
});

for (const [stage, options, environment] of [
  ["CONFIG", {}, {}],
  ["PG_IMPORT", { importFailure: true }, startupEnvironment()],
  ["POOL_CREATE", { poolFailure: true }, startupEnvironment()],
  [
    "LEDGER_INITIALIZE",
    { query: () => Promise.reject(STARTUP_PRIVATE) },
    startupEnvironment(),
  ],
  ["LISTEN", { serverFailure: true }, startupEnvironment()],
]) {
  test("startup failure is closed at " + stage, async () => {
    const h = startupHarness({
      ...options,
      failure: new Error(STARTUP_PRIVATE),
    });
    await rejectStartup(h, environment);
    assert.deepEqual(
      startupDiagnostics(h).filter(([, outcome]) => outcome === "ERROR"),
      [[stage, "ERROR"]],
    );
    assert.equal(
      h.events.some(([event]) => event === "listen"),
      false,
    );
    const hasPool = ["LEDGER_INITIALIZE", "LISTEN"].includes(stage);
    assert.equal(
      h.events.filter(([event]) => event === "pool.end").length,
      hasPool ? 1 : 0,
    );
    if (stage === "CONFIG") assert.deepEqual(h.events, []);
  });
}

test("startup never reads hostile thrown properties or logs thrown strings", async () => {
  let propertyReads = 0;
  const hostile = new Error(STARTUP_PRIVATE);
  for (const key of [
    "message",
    "stack",
    "code",
    "detail",
    "cause",
    "toJSON",
    Symbol.toPrimitive,
    Symbol.for("nodejs.util.inspect.custom"),
  ]) {
    Object.defineProperty(hostile, key, {
      get: () => {
        propertyReads += 1;
        throw STARTUP_PRIVATE;
      },
    });
  }
  for (const failure of [hostile, STARTUP_PRIVATE, null, undefined]) {
    const h = startupHarness({ importFailure: true, failure });
    await rejectStartup(h);
  }
  assert.equal(propertyReads, 0);
});

test("startup handles both synchronous and emitted listener failure", async () => {
  for (const listen of [
    () => {
      throw STARTUP_PRIVATE;
    },
    (server) =>
      queueMicrotask(() => server.emit("error", new Error(STARTUP_PRIVATE))),
  ]) {
    const h = startupHarness({ listen });
    await rejectStartup(h);
    assert.deepEqual(startupDiagnostics(h).at(-1), ["LISTEN", "ERROR"]);
    assert.equal(
      h.events.filter(([event]) => event === "server.close").length,
      1,
    );
    assert.equal(h.events.filter(([event]) => event === "pool.end").length, 1);
  }
});

test("startup pool error interrupts pending initialization without readiness", async () => {
  const h = startupHarness({ query: () => new Promise(() => {}) });
  const started = rejectStartup(h);
  await startupSettled();
  h.pool.emit("error", new Error(STARTUP_PRIVATE));
  await started;
  assert.deepEqual(startupDiagnostics(h).at(-1), [
    "LEDGER_INITIALIZE",
    "ERROR",
  ]);
});

test("startup resource error racing listening callback never emits readiness", async () => {
  const h = startupHarness({
    listen: (server) => {
      server.finishListen();
      server.emit("error", STARTUP_PRIVATE);
    },
  });
  await rejectStartup(h);
  assert.deepEqual(startupDiagnostics(h).at(-1), ["LISTEN", "ERROR"]);
});

test("startup cleans both resources despite close throws and pool rejection", async () => {
  const h = startupHarness({
    listen: () => {
      throw STARTUP_PRIVATE;
    },
    close: () => {
      throw STARTUP_PRIVATE;
    },
    end: () => Promise.reject(new Error(STARTUP_PRIVATE)),
  });
  await rejectStartup(h);
  assert.deepEqual(startupDiagnostics(h).slice(-3), [
    ["LISTEN", "ERROR"],
    ["SERVER_CLEANUP", "ERROR"],
    ["POOL_CLEANUP", "ERROR"],
  ]);
  assert.equal(h.events.filter(([event]) => event === "pool.end").length, 1);
});

test("startup catches synchronous pool cleanup failure", async () => {
  const h = startupHarness({
    query: () => Promise.reject(STARTUP_PRIVATE),
    end: () => {
      throw STARTUP_PRIVATE;
    },
  });
  await rejectStartup(h);
  assert.deepEqual(startupDiagnostics(h).at(-1), ["POOL_CLEANUP", "ERROR"]);
});

test("startup cleanup is bounded and late rejection remains handled", async () => {
  const timers = [];
  const cleared = [];
  let rejectEnd;
  const h = startupHarness({
    listen: () => {
      throw STARTUP_PRIVATE;
    },
    close: () => {},
    end: () =>
      new Promise((_resolve, reject) => {
        rejectEnd = reject;
      }),
    timers: {
      scheduleTimeout: (callback, delay) => {
        assert.equal(delay, 5_000);
        timers.push(callback);
        return callback;
      },
      cancelTimeout: (timer) => cleared.push(timer),
    },
  });
  const started = rejectStartup(h);
  await startupSettled();
  assert.equal(timers.length, 1);
  timers[0]();
  await startupSettled();
  assert.equal(timers.length, 2);
  timers[1]();
  await started;
  assert.deepEqual(cleared, timers);
  rejectEnd(new Error(STARTUP_PRIVATE));
  await startupSettled();
  assert.deepEqual(startupDiagnostics(h).slice(-2), [
    ["SERVER_CLEANUP", "ERROR"],
    ["POOL_CLEANUP", "ERROR"],
  ]);
});

test("startup diagnostic sink throws and rejections never escape", async () => {
  for (const write of [
    () => {
      throw STARTUP_PRIVATE;
    },
    () => Promise.reject(new Error(STARTUP_PRIVATE)),
  ]) {
    const h = startupHarness({
      importFailure: true,
      failure: STARTUP_PRIVATE,
      write,
    });
    await rejectStartup(h);
    await startupSettled();
  }
});

test("startup permanent resource error handlers fail closed after readiness", async () => {
  for (const resource of ["pool", "server"]) {
    const h = startupHarness();
    await startDriver(startupEnvironment(), h.dependencies);
    h[resource].emit("error", new Error(STARTUP_PRIVATE));
    h[resource].emit("error", STARTUP_PRIVATE);
    h.processLike.emit("SIGTERM");
    h.processLike.emit("SIGINT");
    await startupSettled();
    assert.equal(h.processLike.exitCode, 1);
    assert.deepEqual(
      h.events.filter(([event]) => event === "exit"),
      [["exit", 1]],
    );
    assert.equal(h.events.filter(([event]) => event === "pool.end").length, 1);
    assert.equal(
      h.events.filter(([event]) => event === "server.close").length,
      1,
    );
    assert.deepEqual(
      startupDiagnostics(h).filter(([, outcome]) => outcome === "ERROR"),
      [["RUNTIME", "ERROR"]],
    );
  }
});

test("startup duplicate shutdown signals cannot hide cleanup failure", async () => {
  const h = startupHarness({ end: () => Promise.reject(STARTUP_PRIVATE) });
  await startDriver(startupEnvironment(), h.dependencies);
  h.processLike.emit("SIGTERM");
  h.processLike.emit("SIGINT");
  await startupSettled();
  assert.equal(h.processLike.exitCode, 1);
  assert.deepEqual(
    h.events.filter(([event]) => event === "exit"),
    [["exit", 1]],
  );
  assert.equal(h.events.filter(([event]) => event === "pool.end").length, 1);
  assert.deepEqual(startupDiagnostics(h).at(-1), ["POOL_CLEANUP", "ERROR"]);
});

test("startup ledger errors map only finite own string data codes", async () => {
  const cases = [
    ["42501", "LEDGER_PERMISSION_DENIED"],
    ["28P01", "LEDGER_AUTH_REJECTED"],
    ["28000", "LEDGER_AUTH_REJECTED"],
    ["3D000", "LEDGER_DATABASE_MISSING"],
    ...[
      "DEPTH_ZERO_SELF_SIGNED_CERT",
      "SELF_SIGNED_CERT_IN_CHAIN",
      "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
      "CERT_HAS_EXPIRED",
      "CERT_NOT_YET_VALID",
      "ERR_TLS_CERT_ALTNAME_INVALID",
    ].map((code) => [code, "TLS_VERIFICATION_FAILED"]),
    ["ENOTFOUND", "NAME_RESOLUTION_FAILED"],
    ["EAI_AGAIN", "NAME_RESOLUTION_FAILED"],
    ["ECONNREFUSED", "CONNECTION_REFUSED"],
    ["ETIMEDOUT", "CONNECTION_TIMEOUT"],
  ];
  for (const [code, label] of cases) {
    const error = new Error(STARTUP_PRIVATE);
    error.code = code;
    error.detail = STARTUP_PRIVATE;
    const h = startupHarness({ query: () => Promise.reject(error) });
    await rejectStartup(h);
    assert.equal(JSON.parse(h.lines.at(-1)).code, label);
    assert.equal(h.lines.join("").includes(code), false);
  }
});

test("startup ledger codes never invoke getters proxies or coercion", async () => {
  let propertyReads = 0;
  const getter = {};
  Object.defineProperty(getter, "code", {
    get: () => {
      propertyReads += 1;
      throw new Error(STARTUP_PRIVATE);
    },
  });
  const proxy = new Proxy(
    {},
    {
      getOwnPropertyDescriptor: () => {
        propertyReads += 1;
        throw STARTUP_PRIVATE;
      },
      get: () => {
        propertyReads += 1;
        throw STARTUP_PRIVATE;
      },
    },
  );
  const revoked = Proxy.revocable({}, {});
  revoked.revoke();
  const coercible = {
    toString: () => {
      propertyReads += 1;
      throw STARTUP_PRIVATE;
    },
  };
  for (const error of [
    getter,
    proxy,
    revoked.proxy,
    Object.create({ code: "42501" }),
    { code: STARTUP_PRIVATE },
    { code: coercible },
    { code: 42501 },
    { code: "__proto__" },
    { code: "constructor" },
    null,
    STARTUP_PRIVATE,
  ]) {
    const h = startupHarness({ query: () => Promise.reject(error) });
    await rejectStartup(h);
    assert.equal(JSON.parse(h.lines.at(-1)).code, "UNKNOWN");
  }
  assert.equal(propertyReads, 0);
});

test("startup emitted ledger error preserves mapped label without raw error", async () => {
  const h = startupHarness({ query: () => new Promise(() => {}) });
  const started = rejectStartup(h);
  await startupSettled();
  const error = new Error(STARTUP_PRIVATE);
  error.code = "42501";
  h.pool.emit("error", error);
  await started;
  assert.equal(JSON.parse(h.lines.at(-1)).code, "LEDGER_PERMISSION_DENIED");
});

test("startup nonledger errors cannot claim a ledger cause", async () => {
  const error = new Error(STARTUP_PRIVATE);
  error.code = "42501";
  const h = startupHarness({ importFailure: true, failure: error });
  await rejectStartup(h);
  assert.equal(JSON.parse(h.lines.at(-1)).code, "UNKNOWN");
});

test("startup rejects unversioned login payloads before pg import", async () => {
  for (const key of [
    "CRM_CANARY_KLINIK_LOGIN_JSON",
    "CRM_CANARY_NON_KLINIK_LOGIN_JSON",
  ]) {
    const environment = startupEnvironment();
    const login = JSON.parse(environment[key]);
    delete login.schema_version;
    environment[key] = canonicalJson(login);
    const h = startupHarness();
    await rejectStartup(h, environment);
    assert.deepEqual(h.events, []);
    assert.deepEqual(startupDiagnostics(h), [
      ["CONFIG", "STARTED"],
      ["CONFIG", "ERROR"],
    ]);
  }
});
