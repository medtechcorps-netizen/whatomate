/* eslint-env node */

import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import http from "node:http";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import {
  UI_CHECKS,
  executeCanary,
  validateFixtureDescriptor,
  validateLogin,
} from "./runner.mjs";

const REQUEST_KEYS = Object.freeze([
  "schema_version",
  "authority",
  "phase",
  "nonce",
  "idempotency_key",
  "control_sha",
  "change_receipt_sha256",
  "driver_version_sha256",
  "fixture_descriptor_sha256",
]);
const RESPONSE_KEYS = Object.freeze([
  "schema_version",
  "authority",
  "phase",
  "nonce",
  "idempotency_key",
  "change_receipt_sha256",
  "driver_version_sha256",
  "fixture_descriptor_sha256",
  "observed_at",
  "execution_count",
  "checks",
]);
const SHA1_RE = /^[0-9a-f]{40}$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const UTC_SECONDS_RE =
  /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$/;
const MAX_REQUEST_BYTES = 65_536;
const MAX_CLOCK_SKEW_MS = 300_000;
const GENERIC_ERROR_BODY = '{"error":"request rejected"}';
const REQUEST_HMAC_DOMAIN = Buffer.from(
  "rereply-crm-canary-request-v1",
  "ascii",
);

export class DriverRequestError extends Error {
  constructor(message, status = 400) {
    super(message);
    this.name = "DriverRequestError";
    this.status = status;
  }
}

function fail(message, status = 400) {
  throw new DriverRequestError(message, status);
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
    value.length > maximum ||
    value.includes("\u0000") ||
    value.includes("\r") ||
    value.includes("\n") ||
    (pattern && !pattern.test(value))
  ) {
    fail(label + " is invalid");
  }
  return value;
}

function normalizeForCanonicalJson(value) {
  if (value === null || typeof value === "boolean" || typeof value === "string")
    return value;
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) fail("JSON number is invalid");
    return value;
  }
  if (Array.isArray(value)) return value.map(normalizeForCanonicalJson);
  if (!isPlainObject(value)) fail("JSON value is invalid");
  const normalized = {};
  for (const key of Object.keys(value).sort()) {
    normalized[key] = normalizeForCanonicalJson(value[key]);
  }
  return normalized;
}

function escapeNonAscii(raw) {
  return raw.replace(/[\u007f-\uffff]/gu, (character) => {
    return "\\u" + character.charCodeAt(0).toString(16).padStart(4, "0");
  });
}

export function canonicalJson(value) {
  return escapeNonAscii(JSON.stringify(normalizeForCanonicalJson(value)));
}

export function parseCanonicalJson(raw, label = "JSON") {
  let text;
  try {
    text =
      typeof raw === "string"
        ? raw
        : new TextDecoder("utf-8", { fatal: true }).decode(raw);
  } catch {
    fail(label + " is not UTF-8");
  }
  if (
    text.length === 0 ||
    Buffer.byteLength(text, "utf8") > MAX_REQUEST_BYTES
  ) {
    fail(label + " has an invalid size");
  }
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    fail(label + " is malformed");
  }
  if (canonicalJson(value) !== text) fail(label + " is not canonical");
  return value;
}

function sha256(raw) {
  return createHash("sha256").update(raw).digest("hex");
}

function hmacSha256(key, raw) {
  return createHmac("sha256", key).update(raw).digest("hex");
}

export function requestHmacPayload(timestamp, rawBody) {
  const normalizedTimestamp = exactString(
    timestamp,
    "request timestamp",
    UTC_SECONDS_RE,
    20,
  );
  parseTimestamp(normalizedTimestamp, "request timestamp");
  const body = Buffer.isBuffer(rawBody) ? rawBody : Buffer.from(rawBody);
  return Buffer.concat([
    REQUEST_HMAC_DOMAIN,
    Buffer.from([0]),
    Buffer.from(normalizedTimestamp, "ascii"),
    Buffer.from([0]),
    body,
  ]);
}

function secureEqualHex(left, right) {
  if (!SHA256_RE.test(left) || !SHA256_RE.test(right)) return false;
  return timingSafeEqual(Buffer.from(left, "hex"), Buffer.from(right, "hex"));
}

function utcSeconds(value) {
  const date = value instanceof Date ? value : new Date(value);
  if (!Number.isFinite(date.getTime())) fail("timestamp is invalid");
  return date.toISOString().replace(/\.[0-9]{3}Z$/u, "Z");
}

function parseTimestamp(value, label) {
  const raw = exactString(value, label, UTC_SECONDS_RE, 20);
  const milliseconds = Date.parse(raw);
  if (
    !Number.isFinite(milliseconds) ||
    utcSeconds(new Date(milliseconds)) !== raw
  ) {
    fail(label + " is invalid");
  }
  return milliseconds;
}

function decodeHmacKey(value) {
  const encoded = exactString(
    value,
    "driver HMAC key",
    /^[A-Za-z0-9+/]+={0,2}$/u,
    256,
  );
  let key;
  try {
    key = Buffer.from(encoded, "base64");
  } catch {
    fail("driver HMAC key is invalid", 500);
  }
  if (key.length !== 32 || key.toString("base64") !== encoded) {
    fail("driver HMAC key is invalid", 500);
  }
  return key;
}

function parseEnvironmentJson(raw, label) {
  if (typeof raw !== "string" || raw.length === 0)
    fail(label + " is unavailable", 500);
  return parseCanonicalJson(raw, label);
}

function validateDatabaseUrl(value) {
  const raw = exactString(value, "ledger database URL", undefined, 4096);
  if (/[\u0000-\u0020\u007f]/u.test(raw)) {
    fail("ledger database URL is invalid", 500);
  }
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    fail("ledger database URL is invalid", 500);
  }
  if (
    !["postgres:", "postgresql:"].includes(parsed.protocol) ||
    !parsed.hostname ||
    !parsed.username ||
    !parsed.password ||
    !parsed.pathname ||
    parsed.hash
  ) {
    fail("ledger database URL is invalid", 500);
  }
  // libpq-style query parameters can override connection authority, client
  // credentials, and TLS files. Accept only DigitalOcean's canonical
  // libpq-compatible TLS policy; hostname, port, and credentials come only
  // from the URL authority installed in the protected driver environment.
  const sslModes = parsed.searchParams.getAll("sslmode");
  const libpqCompat = parsed.searchParams.getAll("uselibpqcompat");
  const exactQuery = "?sslmode=require&uselibpqcompat=true";
  if (
    [...parsed.searchParams.keys()].length !== 2 ||
    sslModes.length !== 1 ||
    sslModes[0] !== "require" ||
    libpqCompat.length !== 1 ||
    libpqCompat[0] !== "true" ||
    parsed.search !== exactQuery ||
    raw.indexOf("?") !== raw.length - exactQuery.length ||
    raw.slice(-exactQuery.length) !== exactQuery
  ) {
    fail("ledger database TLS policy is invalid", 500);
  }
  return raw;
}

export function loadRuntimeConfig(environment = process.env) {
  const descriptor = validateFixtureDescriptor(
    parseEnvironmentJson(
      environment.CRM_CANARY_FIXTURE_DESCRIPTOR_JSON,
      "fixture descriptor",
    ),
  );
  const klinikLogin = validateLogin(
    parseEnvironmentJson(
      environment.CRM_CANARY_KLINIK_LOGIN_JSON,
      "Klinik login",
    ),
    "Klinik login",
  );
  const nonKlinikLogin = validateLogin(
    parseEnvironmentJson(
      environment.CRM_CANARY_NON_KLINIK_LOGIN_JSON,
      "non-Klinik login",
    ),
    "non-Klinik login",
  );
  const metaAppSecret = exactString(
    environment.CRM_CANARY_META_APP_SECRET,
    "Meta app secret",
    /^[\x21-\x7e]+$/u,
    256,
  );
  if (metaAppSecret.length < 16) fail("Meta app secret is invalid", 500);
  const portRaw = environment.PORT || "8080";
  if (!/^[1-9][0-9]{0,4}$/u.test(portRaw)) fail("driver port is invalid", 500);
  const port = Number(portRaw);
  if (port > 65535) fail("driver port is invalid", 500);
  return {
    hmacKey: decodeHmacKey(environment.CRM_CANARY_HMAC_KEY_BASE64),
    driverVersionSha256: exactString(
      environment.CRM_CANARY_DRIVER_VERSION_SHA256,
      "driver version",
      SHA256_RE,
      64,
    ),
    fixtureDescriptorSha256: sha256(canonicalJson(descriptor)),
    ledgerDatabaseUrl: validateDatabaseUrl(
      environment.CRM_CANARY_LEDGER_DATABASE_URL,
    ),
    descriptor,
    klinikLogin,
    nonKlinikLogin,
    metaAppSecret,
    port,
  };
}

function validateHeaders(headers, now) {
  const contentType = exactString(
    headers["content-type"],
    "content type",
    undefined,
    64,
  );
  if (contentType.toLowerCase() !== "application/json")
    fail("content type differs");
  const timestamp = exactString(
    headers["x-rereply-canary-timestamp"],
    "request timestamp",
    UTC_SECONDS_RE,
    20,
  );
  const timestampMs = parseTimestamp(timestamp, "request timestamp");
  if (Math.abs(now.getTime() - timestampMs) > MAX_CLOCK_SKEW_MS) {
    fail("request timestamp is stale or future-dated");
  }
  return {
    timestamp,
    signature: exactString(
      headers["x-rereply-canary-signature"],
      "request signature",
      SHA256_RE,
      64,
    ),
  };
}

function validateRequest(value, config) {
  const request = exactKeys(value, REQUEST_KEYS, "synthetic CRM request");
  if (
    request.schema_version !== 1 ||
    request.authority !== "rereply-controlled-synthetic-crm-request" ||
    request.phase !== "ui"
  ) {
    fail("synthetic CRM request authority differs");
  }
  const nonce = exactString(request.nonce, "request nonce", SHA256_RE, 64);
  if (request.idempotency_key !== nonce)
    fail("request idempotency binding differs");
  exactString(request.control_sha, "control SHA", SHA1_RE, 40);
  exactString(
    request.change_receipt_sha256,
    "change receipt hash",
    SHA256_RE,
    64,
  );
  if (
    request.driver_version_sha256 !== config.driverVersionSha256 ||
    request.fixture_descriptor_sha256 !== config.fixtureDescriptorSha256
  ) {
    fail("driver request binding differs");
  }
  return request;
}

function validateChecks(value) {
  const checks = exactKeys(value, UI_CHECKS, "synthetic CRM checks");
  if (UI_CHECKS.some((key) => checks[key] !== true))
    fail("synthetic CRM checks failed", 503);
  return Object.fromEntries(UI_CHECKS.map((key) => [key, true]));
}

function buildSignedResponse(request, checks, config, observedAt) {
  const signed = {
    schema_version: 1,
    authority: "rereply-controlled-synthetic-crm-result",
    phase: "ui",
    nonce: request.nonce,
    idempotency_key: request.idempotency_key,
    change_receipt_sha256: request.change_receipt_sha256,
    driver_version_sha256: config.driverVersionSha256,
    fixture_descriptor_sha256: config.fixtureDescriptorSha256,
    observed_at: utcSeconds(observedAt),
    execution_count: 1,
    checks: validateChecks(checks),
  };
  exactKeys(signed, RESPONSE_KEYS, "synthetic CRM response");
  const hmac = hmacSha256(config.hmacKey, canonicalJson(signed));
  return canonicalJson({ ...signed, hmac_sha256: hmac });
}

function validateStoredResponse(raw, request, config) {
  const response = parseCanonicalJson(raw, "stored response");
  const value = exactKeys(
    response,
    [...RESPONSE_KEYS, "hmac_sha256"],
    "stored response",
  );
  const signature = exactString(
    value.hmac_sha256,
    "stored response HMAC",
    SHA256_RE,
    64,
  );
  const signed = { ...value };
  delete signed.hmac_sha256;
  if (
    !secureEqualHex(
      signature,
      hmacSha256(config.hmacKey, canonicalJson(signed)),
    )
  ) {
    fail("stored response authentication failed", 503);
  }
  if (
    value.schema_version !== 1 ||
    value.authority !== "rereply-controlled-synthetic-crm-result" ||
    value.phase !== "ui" ||
    value.nonce !== request.nonce ||
    value.idempotency_key !== request.idempotency_key ||
    value.change_receipt_sha256 !== request.change_receipt_sha256 ||
    value.driver_version_sha256 !== config.driverVersionSha256 ||
    value.fixture_descriptor_sha256 !== config.fixtureDescriptorSha256 ||
    value.execution_count !== 1
  ) {
    fail("stored response binding differs", 503);
  }
  parseTimestamp(value.observed_at, "stored response timestamp");
  validateChecks(value.checks);
  return raw;
}

export class PostgresExecutionLedger {
  constructor(pool) {
    this.pool = pool;
  }

  async initialize() {
    await this.pool.query(`
      CREATE TABLE IF NOT EXISTS crm_canary_execution_ledger (
        idempotency_key CHAR(64) PRIMARY KEY,
        request_sha256 CHAR(64) NOT NULL,
        state TEXT NOT NULL CHECK (state IN ('running', 'complete', 'failed')),
        response_body TEXT,
        created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
        completed_at TIMESTAMPTZ,
        CHECK (
          (state = 'complete' AND response_body IS NOT NULL AND completed_at IS NOT NULL)
          OR (state <> 'complete' AND response_body IS NULL)
        )
      )
    `);
  }

  async claim(idempotencyKey, requestSha256) {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      const inserted = await client.query(
        `INSERT INTO crm_canary_execution_ledger
           (idempotency_key, request_sha256, state)
         VALUES ($1, $2, 'running')
         ON CONFLICT (idempotency_key) DO NOTHING
         RETURNING idempotency_key`,
        [idempotencyKey, requestSha256],
      );
      if (inserted.rowCount === 1) {
        await client.query("COMMIT");
        return { kind: "new" };
      }
      const existing = await client.query(
        `SELECT request_sha256, state, response_body
           FROM crm_canary_execution_ledger
          WHERE idempotency_key = $1
          FOR UPDATE`,
        [idempotencyKey],
      );
      await client.query("COMMIT");
      if (existing.rowCount !== 1)
        fail("execution ledger is inconsistent", 503);
      const row = existing.rows[0];
      if (row.request_sha256 !== requestSha256) return { kind: "mismatch" };
      if (row.state === "complete")
        return { kind: "complete", responseBody: row.response_body };
      return { kind: row.state };
    } catch (error) {
      await client.query("ROLLBACK").catch(() => {});
      throw error;
    } finally {
      client.release();
    }
  }

  async complete(idempotencyKey, requestSha256, responseBody) {
    const result = await this.pool.query(
      `UPDATE crm_canary_execution_ledger
          SET state = 'complete', response_body = $3, completed_at = clock_timestamp()
        WHERE idempotency_key = $1 AND request_sha256 = $2 AND state = 'running'`,
      [idempotencyKey, requestSha256, responseBody],
    );
    if (result.rowCount !== 1) fail("execution ledger completion failed", 503);
  }

  async fail(idempotencyKey, requestSha256) {
    await this.pool.query(
      `UPDATE crm_canary_execution_ledger
          SET state = 'failed'
        WHERE idempotency_key = $1 AND request_sha256 = $2 AND state = 'running'`,
      [idempotencyKey, requestSha256],
    );
  }
}

export async function processAuthenticatedRequest({
  rawBody,
  headers,
  config,
  ledger,
  executor = executeCanary,
  clock = () => new Date(),
}) {
  const startedAt = clock();
  const normalizedHeaders = Object.fromEntries(
    Object.entries(headers).map(([key, value]) => [key.toLowerCase(), value]),
  );
  const authority = validateHeaders(normalizedHeaders, startedAt);
  const raw = Buffer.isBuffer(rawBody) ? rawBody : Buffer.from(rawBody);
  const request = validateRequest(
    parseCanonicalJson(raw, "synthetic CRM request"),
    config,
  );
  const expectedSignature = hmacSha256(
    config.hmacKey,
    requestHmacPayload(authority.timestamp, raw),
  );
  if (!secureEqualHex(authority.signature, expectedSignature)) {
    fail("request authentication failed", 401);
  }
  const requestSha256 = sha256(raw);
  const claim = await ledger.claim(request.idempotency_key, requestSha256);
  if (claim.kind === "mismatch") fail("idempotency key reuse differs", 409);
  if (claim.kind === "running" || claim.kind === "failed") {
    fail("idempotent execution is unavailable", 409);
  }
  if (claim.kind === "complete") {
    return {
      status: 200,
      body: validateStoredResponse(claim.responseBody, request, config),
      replayed: true,
    };
  }
  if (claim.kind !== "new") fail("execution ledger state differs", 503);

  try {
    const checks = await executor({
      descriptor: config.descriptor,
      klinikLogin: config.klinikLogin,
      nonKlinikLogin: config.nonKlinikLogin,
      metaAppSecret: config.metaAppSecret,
      nonce: request.nonce,
      now: startedAt,
    });
    const responseBody = buildSignedResponse(request, checks, config, clock());
    await ledger.complete(request.idempotency_key, requestSha256, responseBody);
    return { status: 200, body: responseBody, replayed: false };
  } catch (error) {
    await ledger.fail(request.idempotency_key, requestSha256).catch(() => {});
    if (error instanceof DriverRequestError) throw error;
    fail("synthetic CRM execution failed", 503);
  }
}

function singleRequestHeaders(request) {
  const critical = new Map([
    ["content-type", []],
    ["x-rereply-canary-timestamp", []],
    ["x-rereply-canary-signature", []],
  ]);
  for (let index = 0; index < request.rawHeaders.length; index += 2) {
    const key = request.rawHeaders[index].toLowerCase();
    if (critical.has(key))
      critical.get(key).push(request.rawHeaders[index + 1]);
  }
  const result = {};
  for (const [key, values] of critical) {
    if (values.length !== 1) fail("request headers differ");
    result[key] = values[0];
  }
  return result;
}

async function readBody(request) {
  const lengthHeader = request.headers["content-length"];
  if (Array.isArray(lengthHeader)) fail("request length differs");
  if (typeof lengthHeader === "string") {
    if (
      !/^[1-9][0-9]{0,5}$/u.test(lengthHeader) ||
      Number(lengthHeader) > MAX_REQUEST_BYTES
    ) {
      fail("request body is too large", 413);
    }
  }
  const chunks = [];
  let total = 0;
  for await (const chunk of request) {
    total += chunk.length;
    if (total > MAX_REQUEST_BYTES) fail("request body is too large", 413);
    chunks.push(chunk);
  }
  if (total === 0) fail("request body is empty");
  return Buffer.concat(chunks, total);
}

function writeJson(response, status, body) {
  response.writeHead(status, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(body, "utf8"),
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  response.end(body);
}

export function createHttpHandler(dependencies) {
  return async (request, response) => {
    try {
      const url = new URL(request.url || "/", "http://127.0.0.1");
      if (
        request.method === "GET" &&
        url.pathname === "/healthz" &&
        !url.search
      ) {
        response.writeHead(204, { "Cache-Control": "no-store" });
        response.end();
        return;
      }
      if (
        request.method !== "POST" ||
        url.pathname !== "/v1/execute" ||
        url.search
      ) {
        fail("request route differs", 404);
      }
      const rawBody = await readBody(request);
      const result = await processAuthenticatedRequest({
        rawBody,
        headers: singleRequestHeaders(request),
        ...dependencies,
      });
      writeJson(response, result.status, result.body);
    } catch (error) {
      const status = error instanceof DriverRequestError ? error.status : 503;
      writeJson(response, status, GENERIC_ERROR_BODY);
    }
  };
}

export async function main(environment = process.env) {
  const config = loadRuntimeConfig(environment);
  const { Pool } = await import("pg");
  const pool = new Pool({
    connectionString: config.ledgerDatabaseUrl,
    max: 2,
    idleTimeoutMillis: 10_000,
    connectionTimeoutMillis: 10_000,
    application_name: "rereply-crm-canary-driver",
  });
  const ledger = new PostgresExecutionLedger(pool);
  await ledger.initialize();
  const server = http.createServer(createHttpHandler({ config, ledger }));
  server.on("clientError", (_error, socket) => socket.destroy());
  await new Promise((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen(config.port, "0.0.0.0", resolveListen);
  });
  const shutdown = () => {
    server.close(() => {
      pool.end().finally(() => {
        process.exitCode = 0;
      });
    });
  };
  process.once("SIGTERM", shutdown);
  process.once("SIGINT", shutdown);
  return { server, pool };
}

const invokedPath = process.argv[1] ? resolve(process.argv[1]) : "";
if (invokedPath && fileURLToPath(import.meta.url) === invokedPath) {
  main().catch(() => {
    process.exitCode = 1;
  });
}
