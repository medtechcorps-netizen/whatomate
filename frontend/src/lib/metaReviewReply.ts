import type {
  MetaMessengerReviewReplyEligibility,
  MetaMessengerReviewReplyConstraints,
} from "@/services/productSuite";

const maximumSupportedReviewReplyLength = 2_000;

export type AttestedMetaReviewReplyEligibility = Omit<
  MetaMessengerReviewReplyEligibility,
  | "attestation_id"
  | "expires_at"
  | "page_id"
  | "recipient_label"
  | "constraints"
> & {
  eligible: true;
  attestation_id: string;
  expires_at: string;
  page_id: string;
  recipient_label: string;
  constraints: MetaMessengerReviewReplyConstraints;
};

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function nonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

export function isExplicitlyIneligibleMetaReviewReply(value: unknown) {
  return isObject(value) && value.eligible === false;
}

/**
 * Fail-closed validation for the short-lived server attestation. Client-side
 * account metadata can only narrow an attestation by checking the Page ID; it
 * can never make an otherwise ineligible conversation eligible.
 */
export function attestedMetaReviewReplyEligibility(
  value: unknown,
  expectedPageId: string | undefined,
  now = Date.now(),
): AttestedMetaReviewReplyEligibility | null {
  if (!isObject(value) || value.eligible !== true || !expectedPageId) {
    return null;
  }

  const constraints = value.constraints;
  if (
    !isObject(constraints) ||
    constraints.text_only !== true ||
    constraints.manual_confirmation_required !== true ||
    constraints.ai_disabled !== true ||
    constraints.mark_read_disabled !== true ||
    !Number.isInteger(constraints.max_length) ||
    Number(constraints.max_length) < 1 ||
    Number(constraints.max_length) > maximumSupportedReviewReplyLength
  ) {
    return null;
  }

  if (
    value.reason_code !== "eligible" ||
    !nonEmptyString(value.attestation_id) ||
    !nonEmptyString(value.expires_at) ||
    !nonEmptyString(value.page_id) ||
    !nonEmptyString(value.recipient_label) ||
    value.page_id !== expectedPageId
  ) {
    return null;
  }

  const expiresAt = Date.parse(value.expires_at);
  if (!Number.isFinite(expiresAt) || expiresAt <= now) return null;

  return {
    eligible: true,
    reason_code: value.reason_code,
    ...(nonEmptyString(value.reason) ? { reason: value.reason } : {}),
    attestation_id: value.attestation_id,
    expires_at: value.expires_at,
    page_id: value.page_id,
    recipient_label: value.recipient_label,
    constraints: {
      text_only: true,
      max_length: Number(constraints.max_length),
      manual_confirmation_required: true,
      ai_disabled: true,
      mark_read_disabled: true,
    },
  };
}
