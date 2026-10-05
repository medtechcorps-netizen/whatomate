export const META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS = 5_000;
// Meta's exchangeable code lives for 30 seconds. Coexistence waits at most half
// of that for the selected-asset message, so a code-only fallback still reaches
// the server early enough to redeem the code.
export const META_COEXISTENCE_SELECTION_TIMEOUT_MS = 15_000;
export const META_COEXISTENCE_SESSION_INFO_VERSION = "3";

export type MetaEmbeddedSignupMode = "coexistence" | "classic";

const META_EMBEDDED_SIGNUP_MESSAGE_HOSTS = new Set([
  "www.facebook.com",
  "web.facebook.com",
  "business.facebook.com",
  "signup.business.facebook.com",
]);

export interface MetaEmbeddedSignupResult {
  code: string;
  mode: MetaEmbeddedSignupMode;
  phoneNumberId?: string;
  wabaId?: string;
  /**
   * Coexistence only: the E.164 digits the operator entered, present only
   * when no phone ID is sent. The server uses it solely to choose among the
   * phones Meta lists under the selected WhatsApp Business Account.
   */
  phoneNumberHint?: string;
  /**
   * Privacy-safe summary of the Meta signup messages this session observed
   * and of what it sent. It never contains an ID, code, token or number.
   */
  diagnostics: string;
}

export type MetaEmbeddedSignupAbortReason = "cancelled" | "error";

interface MetaEmbeddedSignupSessionOptions {
  mode: MetaEmbeddedSignupMode;
  onComplete: (result: MetaEmbeddedSignupResult) => void;
  onAbort: (
    reason: MetaEmbeddedSignupAbortReason,
    detail: string | undefined,
    diagnostics: string,
  ) => void;
  onSettled?: () => void;
  isContextCurrent?: () => boolean;
  onContextChanged?: () => void;
  codeFallbackMs?: number;
  /**
   * Coexistence only: the operator's number, already normalized by
   * normalizeCoexistencePhoneNumber. Meta's Coexistence completion carries no
   * phone ID, so the number tells the server which listed phone to connect.
   * It is sent only when no phone ID is sent, and never in classic mode.
   */
  phoneNumberHint?: string;
  /**
   * Coexistence only: the phone ID of the workspace account the operator
   * chose to reconnect. It is sent when Meta's completion has no phone ID;
   * a different phone ID from Meta aborts the session without an exchange.
   */
  reconnectPhoneNumberId?: string;
}

// The operator's number for a Coexistence signup, as E.164 digits. The rules
// match the server's normalizeEmbeddedSignupPhoneNumberHint: digits with
// spaces, hyphens, dots, parentheses and one leading +; 7 to 15 digits; a
// country code never starts with 0; no Malaysian number has a 0 after +60
// (country codes are prefix-free, so 60 is only Malaysia); and a number
// starting with 1 is a US or Canada number, which always has 11 digits.
// Only the browser folds pasted look-alike characters first (see
// foldPastedPhoneNumber); the server then receives ASCII digits only.
const COEXISTENCE_PHONE_NUMBER_MAX_LENGTH = 32;
const COEXISTENCE_PHONE_NUMBER_MIN_DIGITS = 7;
const COEXISTENCE_PHONE_NUMBER_MAX_DIGITS = 15;
const MALAYSIA_TRUNK_ZERO_PREFIX = "600";
const NANP_DIGITS = 11;

export type CoexistencePhoneNumberProblem =
  | "empty"
  | "missing_country_code"
  | "zero_after_country_code"
  | "us_canada_length"
  | "invalid";

export type CoexistencePhoneNumber =
  | { digits: string; problem?: undefined }
  | { digits?: undefined; problem: CoexistencePhoneNumberProblem };

// Look-alike digits a phone keyboard or contacts app may produce: full-width
// (U+FF10), Arabic-Indic (U+0660) and Extended Arabic-Indic (U+06F0).
const PHONE_DIGIT_ZEROS = [0xff10, 0x0660, 0x06f0];
const PHONE_LOOKALIKE_PUNCTUATION: Record<string, string> = {
  "\uff0b": "+", // full-width plus
  "\uff08": "(",
  "\uff09": ")",
  "\uff0e": ".",
  "\u2212": "-", // minus sign
};

// Numbers copied from a phone or contacts app can carry invisible format
// characters (bidi marks such as U+202A/U+202C, zero-width spaces, a BOM),
// no-break or thin spaces, typographic dashes and look-alike digits. They
// look correct, so refusing them as "invalid" would confuse the operator.
// Only the resulting ASCII digits ever leave the browser, so folding them
// here never widens what the server accepts.
function foldPastedPhoneNumber(value: string): string {
  let folded = "";
  for (const character of value.replace(/\p{Cf}/gu, "")) {
    const codePoint = character.codePointAt(0) ?? 0;
    const zero = PHONE_DIGIT_ZEROS.find(
      (start) => codePoint >= start && codePoint <= start + 9,
    );
    if (zero !== undefined) {
      folded += String(codePoint - zero);
    } else if (/\p{Zs}/u.test(character)) {
      folded += " ";
    } else if (/\p{Pd}/u.test(character)) {
      folded += "-";
    } else {
      folded += PHONE_LOOKALIKE_PUNCTUATION[character] ?? character;
    }
  }
  return folded;
}

export function normalizeCoexistencePhoneNumber(
  value: string,
): CoexistencePhoneNumber {
  const trimmed = foldPastedPhoneNumber(value).trim();
  if (!trimmed) return { problem: "empty" };
  if (trimmed.length > COEXISTENCE_PHONE_NUMBER_MAX_LENGTH) {
    return { problem: "invalid" };
  }
  let digits = "";
  for (let index = 0; index < trimmed.length; index += 1) {
    const character = trimmed[index];
    if (character >= "0" && character <= "9") {
      digits += character;
    } else if (character === "+" && index === 0) {
      continue;
    } else if (!" -.()".includes(character)) {
      return { problem: "invalid" };
    }
  }
  if (digits.startsWith("0")) return { problem: "missing_country_code" };
  if (digits.startsWith(MALAYSIA_TRUNK_ZERO_PREFIX)) {
    return { problem: "zero_after_country_code" };
  }
  if (
    digits.length < COEXISTENCE_PHONE_NUMBER_MIN_DIGITS ||
    digits.length > COEXISTENCE_PHONE_NUMBER_MAX_DIGITS
  ) {
    return { problem: "invalid" };
  }
  if (digits.startsWith("1") && digits.length !== NANP_DIGITS) {
    return { problem: "us_canada_length" };
  }
  return { digits };
}

export interface MetaEmbeddedSignupSession {
  handleLoginResponse: (response: unknown) => void;
  handleMessage: (event: Pick<MessageEvent, "data" | "origin">) => void;
  cancel: () => void;
}

interface EmbeddedSignupMessage {
  type: "WA_EMBEDDED_SIGNUP";
  event: string;
  version?: unknown;
  data?: Record<string, unknown>;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function nonEmptyString(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed || undefined;
}

// Diagnostics. Production builds drop console output (esbuild.drop in
// vite.config.ts), so the summary is shown with the error instead. It holds
// only event names, versions, whether asset IDs were present, timing and what
// was sent. It never holds an ID, code or token, and it never echoes a value
// that could carry one.
const MAX_DIAGNOSTIC_MESSAGES = 6;
const MAX_DIAGNOSTIC_ORIGINS = 3;

function diagnosticEventName(value: string): string {
  const upper = value.trim().toUpperCase();
  // Letters and underscores only, so an echoed name can never carry an ID.
  return /^[A-Z_]{1,48}$/.test(upper) ? upper : "UNRECOGNIZED";
}

function diagnosticVersion(value: unknown): string {
  if (value === undefined) return "none";
  if (value === null) return "null";
  if (
    typeof value === "number" &&
    Number.isInteger(value) &&
    value >= 0 &&
    value < 1000
  ) {
    return String(value);
  }
  if (typeof value === "string" && /^[0-9]{1,3}$/.test(value)) {
    return `"${value}"`;
  }
  return "invalid";
}

function diagnosticPresence(value: unknown): string {
  if (value === undefined) return "absent";
  if (value === null) return "null";
  if (typeof value !== "string") return "invalid";
  return value.trim() ? "present" : "empty";
}

function diagnosticListPresence(value: unknown): string {
  if (value === undefined) return "absent";
  if (value === null) return "null";
  if (!Array.isArray(value)) return "invalid";
  return `${value.length} listed`;
}

function diagnosticOrigin(origin: string): string {
  try {
    const parsed = new URL(origin);
    const hostname = parsed.hostname.toLowerCase();
    if (
      parsed.protocol === "https:" &&
      /^[a-z0-9.-]{1,253}$/.test(hostname) &&
      (hostname === "facebook.com" || hostname.endsWith(".facebook.com"))
    ) {
      return parsed.port ? `${hostname}:${parsed.port}` : hostname;
    }
  } catch {
    // An unparseable origin is reported generically below.
  }
  return "non-Meta origin";
}

function looksLikeEmbeddedSignupPayload(data: unknown): boolean {
  let payload: unknown = data;
  if (typeof payload === "string") {
    if (!payload.includes("WA_EMBEDDED_SIGNUP")) return false;
    try {
      payload = JSON.parse(payload);
    } catch {
      return false;
    }
  }
  return isRecord(payload) && payload.type === "WA_EMBEDDED_SIGNUP";
}

export function isAllowedMetaEmbeddedSignupOrigin(origin: string): boolean {
  if (!origin || origin !== origin.trim()) return false;

  try {
    const parsed = new URL(origin);
    const hostname = parsed.hostname.toLowerCase();
    return (
      parsed.protocol === "https:" &&
      parsed.port === "" &&
      parsed.username === "" &&
      parsed.password === "" &&
      parsed.pathname === "/" &&
      parsed.search === "" &&
      parsed.hash === "" &&
      META_EMBEDDED_SIGNUP_MESSAGE_HOSTS.has(hostname)
    );
  } catch {
    return false;
  }
}

export function parseMetaEmbeddedSignupMessage(
  event: Pick<MessageEvent, "data" | "origin">,
): EmbeddedSignupMessage | null {
  if (!isAllowedMetaEmbeddedSignupOrigin(event.origin)) return null;

  let payload: unknown = event.data;
  if (typeof payload === "string") {
    try {
      payload = JSON.parse(payload);
    } catch {
      return null;
    }
  }

  if (
    !isRecord(payload) ||
    payload.type !== "WA_EMBEDDED_SIGNUP" ||
    typeof payload.event !== "string"
  ) {
    return null;
  }

  return {
    type: "WA_EMBEDDED_SIGNUP",
    event: payload.event,
    version: payload.version,
    data: isRecord(payload.data) ? payload.data : undefined,
  };
}

// Meta's v4 payloads omit the version; Coexistence needs session info 3 or
// newer. A later version keeps the same selection fields, and the server
// still proves the grant, the phone's WABA and the mode, so accepting it
// keeps signup working when Meta raises the version.
function isSupportedCoexistenceSessionVersion(version: unknown): boolean {
  if (version === undefined) return true;
  let parsed = Number.NaN;
  if (typeof version === "number") {
    parsed = version;
  } else if (typeof version === "string" && /^[1-9][0-9]{0,2}$/.test(version)) {
    parsed = Number(version);
  }
  return (
    Number.isInteger(parsed) &&
    parsed >= Number(META_COEXISTENCE_SESSION_INFO_VERSION) &&
    parsed < 1000
  );
}

function isExpectedFinishMessage(
  message: EmbeddedSignupMessage,
  mode: MetaEmbeddedSignupMode,
): boolean {
  const event = message.event.toUpperCase();
  const standardFinish = event === "FINISH" || event === "FINISH_ONLY_WABA";
  if (mode === "coexistence") {
    // Standard completion messages identify selected assets and may omit the
    // session version. They do not change the requested mode: the server
    // independently proves that mode from Meta phone data.
    return (
      (standardFinish || event === "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING") &&
      isSupportedCoexistenceSessionVersion(message.version)
    );
  }
  return standardFinish;
}

// Meta's error text plus its numeric error code. The session_id Meta sends
// with a user-reported error is a support reference and is never shown.
function metaReportedError(
  data: Record<string, unknown> | undefined,
): string | undefined {
  const message = nonEmptyString(data?.error_message);
  if (!message) return undefined;
  const rawCode = data?.error_code;
  let errorCode: string | undefined;
  if (
    typeof rawCode === "number" &&
    Number.isSafeInteger(rawCode) &&
    rawCode >= 0
  ) {
    errorCode = String(rawCode);
  } else if (typeof rawCode === "string" && /^[0-9]{1,10}$/.test(rawCode)) {
    errorCode = rawCode;
  }
  return errorCode ? `${message} (Meta error code ${errorCode})` : message;
}

function loginErrorMessage(
  response: Record<string, unknown>,
): string | undefined {
  const error = response.error;
  if (typeof error === "string") return nonEmptyString(error);
  if (!isRecord(error)) return undefined;
  return nonEmptyString(error.message) || nonEmptyString(error.error_user_msg);
}

/**
 * Coordinates the two independent Meta Embedded Signup v4 result channels:
 * the OAuth code returned to FB.login and the selected WABA/phone IDs posted
 * through WA_EMBEDDED_SIGNUP. Either may arrive first.
 */
export function createMetaEmbeddedSignupSession(
  options: MetaEmbeddedSignupSessionOptions,
): MetaEmbeddedSignupSession {
  let code: string | undefined;
  let phoneNumberId: string | undefined;
  let wabaId: string | undefined;
  let fallbackTimer: ReturnType<typeof setTimeout> | undefined;
  let settled = false;
  let codeReceivedAt: number | undefined;
  const observedMessages: string[] = [];
  let omittedMessages = 0;
  let malformedMessages = 0;
  const unlistedOrigins = new Map<string, number>();

  const recordMessage = (message: EmbeddedSignupMessage) => {
    if (observedMessages.length >= MAX_DIAGNOSTIC_MESSAGES) {
      omittedMessages += 1;
      return;
    }
    const data = message.data;
    const timing =
      codeReceivedAt === undefined
        ? "before code"
        : `${Math.max(0, Math.round((Date.now() - codeReceivedAt) / 1000))}s after code`;
    observedMessages.push(
      `${diagnosticEventName(message.event)} v=${diagnosticVersion(message.version)}` +
        ` waba_id=${diagnosticPresence(data?.waba_id)}` +
        ` waba_ids=${diagnosticListPresence(data?.waba_ids)}` +
        ` phone_number_id=${diagnosticPresence(data?.phone_number_id)}` +
        ` (${timing})`,
    );
  };

  const recordUnparsedMessage = (
    event: Pick<MessageEvent, "data" | "origin">,
  ) => {
    if (!looksLikeEmbeddedSignupPayload(event.data)) return;
    if (isAllowedMetaEmbeddedSignupOrigin(event.origin)) {
      malformedMessages += 1;
      return;
    }
    const origin = diagnosticOrigin(event.origin);
    if (
      unlistedOrigins.has(origin) ||
      unlistedOrigins.size < MAX_DIAGNOSTIC_ORIGINS
    ) {
      unlistedOrigins.set(origin, (unlistedOrigins.get(origin) ?? 0) + 1);
    }
  };

  const diagnostics = (outcome: string): string => {
    const parts = observedMessages.length
      ? [...observedMessages]
      : ["no WA_EMBEDDED_SIGNUP message received"];
    if (omittedMessages) parts.push(`${omittedMessages} more not listed`);
    if (malformedMessages) {
      parts.push(`${malformedMessages} malformed message(s) ignored`);
    }
    for (const [origin, count] of unlistedOrigins) {
      parts.push(`${count} message(s) ignored from unlisted ${origin}`);
    }
    parts.push(outcome);
    return `Meta signup diagnostics: ${parts.join("; ")}`;
  };

  const clearFallback = () => {
    if (fallbackTimer !== undefined) {
      clearTimeout(fallbackTimer);
      fallbackTimer = undefined;
    }
  };

  const settle = (): boolean => {
    if (settled) return false;
    settled = true;
    clearFallback();
    options.onSettled?.();
    return true;
  };

  const ensureContextCurrent = (): boolean => {
    if (settled) return false;
    if (!options.isContextCurrent || options.isContextCurrent()) return true;
    if (settle()) options.onContextChanged?.();
    return false;
  };

  // Meta's Coexistence completion carries only the WABA ID. The operator
  // either chose an existing workspace account to reconnect, whose phone ID is
  // known, or entered the number being connected. Classic ignores both.
  const reconnectPhoneNumberId =
    options.mode === "coexistence"
      ? nonEmptyString(options.reconnectPhoneNumberId)
      : undefined;
  const phoneNumberHint =
    options.mode === "coexistence" && !reconnectPhoneNumberId
      ? nonEmptyString(options.phoneNumberHint)
      : undefined;

  const complete = (allowMissingWabaId: boolean, waitedMs?: number) => {
    if (!ensureContextCurrent() || !code || (!allowMissingWabaId && !wabaId)) {
      return;
    }
    if (
      reconnectPhoneNumberId &&
      phoneNumberId &&
      phoneNumberId !== reconnectPhoneNumberId
    ) {
      abort(
        "error",
        "Meta returned a different phone number from the account you chose to reconnect. Restart the connection and choose the matching account or number.",
      );
      return;
    }
    if (!settle()) return;

    const sent = ["code"];
    if (wabaId) sent.push("waba_id");
    let sentPhoneNumberId = phoneNumberId;
    let sentPhoneNumberHint: string | undefined;
    if (phoneNumberId) {
      sent.push("phone_number_id");
    } else if (reconnectPhoneNumberId) {
      sentPhoneNumberId = reconnectPhoneNumberId;
      sent.push("reconnect_account_phone_id");
    } else if (phoneNumberHint) {
      sentPhoneNumberHint = phoneNumberHint;
      sent.push("phone_number_hint");
    }
    const waited =
      waitedMs === undefined
        ? ""
        : ` after a ${Math.round(waitedMs / 1000)}s wait`;
    const result: MetaEmbeddedSignupResult = {
      code,
      mode: options.mode,
      phoneNumberId: sentPhoneNumberId,
      wabaId,
      diagnostics: diagnostics(
        `${options.mode} sent ${sent.join("+")}${waited}`,
      ),
    };
    if (sentPhoneNumberHint) result.phoneNumberHint = sentPhoneNumberHint;
    options.onComplete(result);
  };

  const scheduleCompletionDeadline = () => {
    if (settled || !code || fallbackTimer !== undefined) return;
    // Without a selected-asset message, submit the code alone. The server
    // uses the token's WABA only when it grants exactly one and refuses more
    // than one, so a code-only exchange never substitutes another granted WABA
    // for the user's selection, and single-WABA signups keep working when
    // Meta's message is missing or unrecognised. Coexistence waits longer for
    // the message because its tokens often grant previously shared WABAs.
    const waitMs =
      options.mode === "coexistence"
        ? META_COEXISTENCE_SELECTION_TIMEOUT_MS
        : (options.codeFallbackMs ?? META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS);
    fallbackTimer = setTimeout(() => complete(true, waitMs), waitMs);
  };

  const abort = (reason: MetaEmbeddedSignupAbortReason, detail?: string) => {
    if (!settle()) return;
    options.onAbort(
      reason,
      detail,
      diagnostics(`${options.mode} sent nothing`),
    );
  };

  const handleMessage = (event: Pick<MessageEvent, "data" | "origin">) => {
    if (!ensureContextCurrent()) return;
    const message = parseMetaEmbeddedSignupMessage(event);
    if (!message) {
      recordUnparsedMessage(event);
      return;
    }
    recordMessage(message);

    const eventName = message.event.toUpperCase();
    if (isExpectedFinishMessage(message, options.mode)) {
      if (options.mode === "classic") {
        phoneNumberId = nonEmptyString(message.data?.phone_number_id);
        wabaId = nonEmptyString(message.data?.waba_id);
        complete(false);
        return;
      }
      // null is treated as absent; any other non-string value is malformed.
      const singularSelection = message.data?.waba_id ?? undefined;
      const pluralSelection = message.data?.waba_ids ?? undefined;
      if (
        (singularSelection !== undefined &&
          typeof singularSelection !== "string") ||
        (pluralSelection !== undefined &&
          (!Array.isArray(pluralSelection) ||
            pluralSelection.some((value) => typeof value !== "string")))
      ) {
        abort(
          "error",
          "Meta returned an invalid WhatsApp account selection. Restart the connection and select exactly one account.",
        );
        return;
      }
      const listedWabaIds = new Set<string>();
      if (Array.isArray(pluralSelection)) {
        for (const value of pluralSelection) {
          const listedWabaId = nonEmptyString(value);
          if (listedWabaId) listedWabaIds.add(listedWabaId);
        }
      }
      // Meta documents waba_id as the flow's WhatsApp account and waba_ids as
      // every account shared in a multi-WABA flow, so waba_id wins when present.
      const singularWabaId = nonEmptyString(singularSelection);
      let selectedWabaId: string | undefined;
      if (singularWabaId) {
        if (listedWabaIds.size > 0 && !listedWabaIds.has(singularWabaId)) {
          abort(
            "error",
            "Meta returned conflicting WhatsApp account selections. Restart the connection and select exactly one account.",
          );
          return;
        }
        selectedWabaId = singularWabaId;
      } else if (listedWabaIds.size === 1) {
        [selectedWabaId] = listedWabaIds;
      } else if (listedWabaIds.size > 1) {
        abort(
          "error",
          "Meta returned more than one selected WhatsApp account. Restart the connection and select exactly one account.",
        );
        return;
      }
      if (wabaId && selectedWabaId && wabaId !== selectedWabaId) {
        abort(
          "error",
          "Meta returned more than one selected WhatsApp account. Restart the connection and select exactly one account.",
        );
        return;
      }
      if (!selectedWabaId) return;
      const selectedPhoneId = nonEmptyString(message.data?.phone_number_id);
      if (
        phoneNumberId &&
        selectedPhoneId &&
        phoneNumberId !== selectedPhoneId
      ) {
        abort(
          "error",
          "Meta returned more than one selected WhatsApp phone number. Restart the connection and select exactly one phone number.",
        );
        return;
      }
      phoneNumberId = selectedPhoneId || phoneNumberId;
      wabaId = selectedWabaId;
      complete(false);
      return;
    }

    switch (eventName) {
      case "CANCEL": {
        // Meta reports an error the user hit in the flow as CANCEL with
        // error_message and error_code, not as an abandoned step.
        const reportedError = metaReportedError(message.data);
        if (reportedError) {
          abort("error", reportedError);
          return;
        }
        abort("cancelled", nonEmptyString(message.data?.current_step));
        return;
      }
      case "ERROR":
        abort(
          "error",
          metaReportedError(message.data) ||
            nonEmptyString(message.data?.message),
        );
        return;
    }
  };

  const handleLoginResponse = (response: unknown) => {
    if (!ensureContextCurrent()) return;

    if (isRecord(response) && isRecord(response.authResponse)) {
      code = nonEmptyString(response.authResponse.code);
      codeReceivedAt = Date.now();
      if (!code) {
        abort("error", "Meta did not return an authorization code.");
        return;
      }

      complete(false);
      scheduleCompletionDeadline();
      return;
    }

    const detail = isRecord(response) ? loginErrorMessage(response) : undefined;
    if (detail) {
      abort("error", detail);
    } else {
      abort("cancelled");
    }
  };

  return {
    handleLoginResponse,
    handleMessage,
    cancel: () => {
      settle();
    },
  };
}
