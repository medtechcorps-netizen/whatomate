import { describe, expect, it } from "vitest";

import {
  classifyMetaReviewDeprovisionFailure,
  META_REVIEW_DEPROVISION_TIMEOUT_MS,
} from "./metaReviewDeprovision";

describe("classifyMetaReviewDeprovisionFailure", () => {
  it.each([502, 503])("keeps cleanup status %s retryable", (status) => {
    expect(
      classifyMetaReviewDeprovisionFailure(
        status,
        "The review connection is quarantined; Meta cleanup must be retried",
      ),
    ).toBe("retryable");
  });

  it.each([
    "An earlier Messenger review reply remains permanently fenced",
    "Audited operator reconciliation is required before deprovisioning",
  ])("requires an operator for permanent 503: %s", (message) => {
    expect(classifyMetaReviewDeprovisionFailure(503, message)).toBe(
      "operator_required",
    );
  });

  it("requires an operator when Meta cleanup has no usable Page token", () => {
    expect(
      classifyMetaReviewDeprovisionFailure(
        502,
        "The review connection is quarantined; Meta cleanup requires operator action",
      ),
    ).toBe("operator_required");
  });

  it("treats a missing response as an ambiguous outcome", () => {
    expect(classifyMetaReviewDeprovisionFailure(null, "Network Error")).toBe(
      "ambiguous",
    );
  });

  it("allows the backend cleanup deadline to finish before the client timeout", () => {
    expect(META_REVIEW_DEPROVISION_TIMEOUT_MS).toBeGreaterThan(30_000);
  });

  it("does not infer operator repair from an unrelated failure", () => {
    expect(
      classifyMetaReviewDeprovisionFailure(409, "Connection conflict"),
    ).toBe("failed");
  });
});
