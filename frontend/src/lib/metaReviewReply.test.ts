import { describe, expect, it } from "vitest";
import {
  attestedMetaReviewReplyEligibility,
  isExplicitlyIneligibleMetaReviewReply,
} from "@/lib/metaReviewReply";

const now = Date.parse("2026-08-11T03:00:00Z");

function eligibleResponse(overrides: Record<string, unknown> = {}) {
  return {
    eligible: true,
    reason_code: "eligible",
    attestation_id: "review-attestation-1",
    expires_at: "2026-08-11T03:10:00Z",
    page_id: "page-200",
    recipient_label: "••••0300",
    constraints: {
      text_only: true,
      max_length: 2_000,
      manual_confirmation_required: true,
      ai_disabled: true,
      mark_read_disabled: true,
    },
    ...overrides,
  };
}

describe("Meta App Review reply attestation", () => {
  it("accepts only a current server eligibility for the exact Page", () => {
    const result = attestedMetaReviewReplyEligibility(
      eligibleResponse({ recipient_id: "raw-psid-must-be-dropped" }),
      "page-200",
      now,
    );

    expect(result).toMatchObject({
      eligible: true,
      attestation_id: "review-attestation-1",
      page_id: "page-200",
      recipient_label: "••••0300",
    });
    expect(result).not.toHaveProperty("recipient_id");
  });

  it.each([
    ["server says no", { eligible: false }],
    ["reason code is not eligible", { reason_code: "unavailable" }],
    ["attestation is missing", { attestation_id: "" }],
    ["attestation expired", { expires_at: "2026-08-11T02:59:59Z" }],
    ["Page differs", { page_id: "page-elsewhere" }],
    [
      "manual confirmation is not required",
      {
        constraints: {
          text_only: true,
          max_length: 2_000,
          manual_confirmation_required: false,
          ai_disabled: true,
          mark_read_disabled: true,
        },
      },
    ],
    [
      "mark-read is not disabled",
      {
        constraints: {
          text_only: true,
          max_length: 2_000,
          manual_confirmation_required: true,
          ai_disabled: true,
          mark_read_disabled: false,
        },
      },
    ],
    [
      "text limit exceeds the supported review boundary",
      {
        constraints: {
          text_only: true,
          max_length: 2_001,
          manual_confirmation_required: true,
          ai_disabled: true,
          mark_read_disabled: true,
        },
      },
    ],
  ])("fails closed when %s", (_label, overrides) => {
    expect(
      attestedMetaReviewReplyEligibility(
        eligibleResponse(overrides),
        "page-200",
        now,
      ),
    ).toBeNull();
  });

  it("distinguishes an explicit ineligible response from malformed state", () => {
    expect(isExplicitlyIneligibleMetaReviewReply({ eligible: false })).toBe(
      true,
    );
    expect(isExplicitlyIneligibleMetaReviewReply({ eligible: true })).toBe(
      false,
    );
    expect(isExplicitlyIneligibleMetaReviewReply(undefined)).toBe(false);
  });
});
