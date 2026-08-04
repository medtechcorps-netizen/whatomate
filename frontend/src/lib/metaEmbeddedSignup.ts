export const META_EMBEDDED_SIGNUP_CHANNEL_WAIT_MS = 10_000;

export const META_EMBEDDED_SIGNUP_FINISH = "FINISH";
export const META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH =
  "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING";

export interface MetaEmbeddedSignupResult {
  code: string;
  phoneNumberId?: string;
  wabaId: string;
  signupEvent:
    | typeof META_EMBEDDED_SIGNUP_FINISH
    | typeof META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH;
}

export type MetaEmbeddedSignupAbortReason = "cancelled" | "error";

interface MetaEmbeddedSignupSessionOptions {
  expectedSignupEvent: MetaEmbeddedSignupResult["signupEvent"];
  onComplete: (result: MetaEmbeddedSignupResult) => void;
  onAbort: (reason: MetaEmbeddedSignupAbortReason, detail?: string) => void;
  onSettled?: () => void;
  isContextCurrent?: () => boolean;
  onContextChanged?: () => void;
  channelWaitMs?: number;
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
      (hostname === "facebook.com" || hostname.endsWith(".facebook.com"))
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
    event: payload.event.trim().toUpperCase(),
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
 * Correlates Meta's two independent Embedded Signup result channels: the
 * exchangeable code returned to FB.login and the selected assets posted in a
 * WA_EMBEDDED_SIGNUP window message. It never completes from a code alone.
 */
export function createMetaEmbeddedSignupSession(
  options: MetaEmbeddedSignupSessionOptions,
): MetaEmbeddedSignupSession {
  let code: string | undefined;
  let selection: Omit<MetaEmbeddedSignupResult, "code"> | undefined;
  let channelTimer: ReturnType<typeof setTimeout> | undefined;
  let settled = false;

  const clearChannelTimer = () => {
    if (channelTimer !== undefined) {
      clearTimeout(channelTimer);
      channelTimer = undefined;
    }
  };

  const settle = (): boolean => {
    if (settled) return false;
    settled = true;
    clearChannelTimer();
    options.onSettled?.();
    return true;
  };

  const ensureContextCurrent = (): boolean => {
    if (settled) return false;
    if (!options.isContextCurrent || options.isContextCurrent()) return true;
    if (settle()) options.onContextChanged?.();
    return false;
  };

  const abort = (reason: MetaEmbeddedSignupAbortReason, detail?: string) => {
    if (!settle()) return;
    options.onAbort(reason, detail);
  };

  const complete = () => {
    if (!ensureContextCurrent() || !code || !selection || !settle()) return;
    options.onComplete({ code, ...selection });
  };

  const scheduleMissingChannelTimeout = () => {
    if (settled || channelTimer !== undefined || (!code && !selection)) return;
    channelTimer = setTimeout(() => {
      abort(
        "error",
        "Meta did not return complete signup information. Please restart the connection.",
      );
    }, options.channelWaitMs ?? META_EMBEDDED_SIGNUP_CHANNEL_WAIT_MS);
  };

  const handleMessage = (event: Pick<MessageEvent, "data" | "origin">) => {
    if (!ensureContextCurrent()) return;
    const message = parseMetaEmbeddedSignupMessage(event);
    if (!message) return;

    const wabaId = nonEmptyString(message.data?.waba_id);
    const phoneNumberId = nonEmptyString(message.data?.phone_number_id);
    const isSupportedFinish =
      message.event === META_EMBEDDED_SIGNUP_FINISH ||
      message.event === META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH;
    if (isSupportedFinish && message.event !== options.expectedSignupEvent) {
      abort(
        "error",
        "Meta returned a different signup mode than the one selected. Please restart the connection.",
      );
      return;
    }

    switch (message.event) {
      case META_EMBEDDED_SIGNUP_FINISH:
        if (!wabaId || !phoneNumberId) {
          abort(
            "error",
            "Meta completed signup without the selected WABA and phone number IDs. Please reconnect.",
          );
          return;
        }
        selection = {
          wabaId,
          phoneNumberId,
          signupEvent: META_EMBEDDED_SIGNUP_FINISH,
        };
        complete();
        scheduleMissingChannelTimeout();
        return;

      case META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH:
        if (!wabaId) {
          abort(
            "error",
            "Meta completed coexistence signup without a WABA ID. Please reconnect.",
          );
          return;
        }
        selection = {
          wabaId,
          phoneNumberId,
          signupEvent: META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH,
        };
        complete();
        scheduleMissingChannelTimeout();
        return;

      case "CANCEL": {
        const errorDetail =
          nonEmptyString(message.data?.error_message) ||
          nonEmptyString(message.data?.message);
        if (errorDetail || message.data?.error_code !== undefined) {
          abort("error", errorDetail || "Meta reported an onboarding error.");
        } else {
          abort("cancelled", nonEmptyString(message.data?.current_step));
        }
        return;
      }

      case "ERROR":
        abort(
          "error",
          nonEmptyString(message.data?.error_message) ||
            nonEmptyString(message.data?.message) ||
            "Meta reported an onboarding error.",
        );
        return;

      default:
        if (message.event.startsWith("FINISH")) {
          abort(
            "error",
            `ReReply does not support Meta signup result ${message.event}.`,
          );
        }
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
      complete();
      scheduleMissingChannelTimeout();
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
