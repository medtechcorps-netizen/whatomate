export const META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS = 5_000;

const META_EMBEDDED_SIGNUP_MESSAGE_HOSTS = new Set([
  "www.facebook.com",
  "web.facebook.com",
  "business.facebook.com",
  "signup.business.facebook.com",
]);

export interface MetaEmbeddedSignupResult {
  code: string;
  phoneNumberId?: string;
  wabaId?: string;
}

export type MetaEmbeddedSignupAbortReason = "cancelled" | "error";

interface MetaEmbeddedSignupSessionOptions {
  onComplete: (result: MetaEmbeddedSignupResult) => void;
  onAbort: (reason: MetaEmbeddedSignupAbortReason, detail?: string) => void;
  onSettled?: () => void;
  isContextCurrent?: () => boolean;
  onContextChanged?: () => void;
  codeFallbackMs?: number;
}

export interface MetaEmbeddedSignupSession {
  handleLoginResponse: (response: unknown) => void;
  handleMessage: (event: Pick<MessageEvent, "data" | "origin">) => void;
  cancel: () => void;
}

interface EmbeddedSignupMessage {
  type: "WA_EMBEDDED_SIGNUP";
  event: string;
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
    data: isRecord(payload.data) ? payload.data : undefined,
  };
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

  const complete = (allowMissingWabaId: boolean) => {
    if (
      !ensureContextCurrent() ||
      !code ||
      (!allowMissingWabaId && !wabaId) ||
      !settle()
    ) {
      return;
    }

    options.onComplete({ code, phoneNumberId, wabaId });
  };

  const scheduleCodeOnlyFallback = () => {
    if (settled || !code || fallbackTimer !== undefined) return;
    fallbackTimer = setTimeout(
      () => complete(true),
      options.codeFallbackMs ?? META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS,
    );
  };

  const abort = (reason: MetaEmbeddedSignupAbortReason, detail?: string) => {
    if (!settle()) return;
    options.onAbort(reason, detail);
  };

  const handleMessage = (event: Pick<MessageEvent, "data" | "origin">) => {
    if (!ensureContextCurrent()) return;
    const message = parseMetaEmbeddedSignupMessage(event);
    if (!message) return;

    switch (message.event.toUpperCase()) {
      case "FINISH":
      case "FINISH_ONLY_WABA":
      case "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING":
        phoneNumberId = nonEmptyString(message.data?.phone_number_id);
        wabaId = nonEmptyString(message.data?.waba_id);
        complete(false);
        break;
      case "CANCEL":
        abort("cancelled", nonEmptyString(message.data?.current_step));
        break;
      case "ERROR":
        abort(
          "error",
          nonEmptyString(message.data?.error_message) ||
            nonEmptyString(message.data?.message),
        );
        break;
    }
  };

  const handleLoginResponse = (response: unknown) => {
    if (!ensureContextCurrent()) return;

    if (isRecord(response) && isRecord(response.authResponse)) {
      code = nonEmptyString(response.authResponse.code);
      if (!code) {
        abort("error", "Meta did not return an authorization code.");
        return;
      }

      complete(false);
      scheduleCodeOnlyFallback();
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
