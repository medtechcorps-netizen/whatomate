export type MetaReviewDeprovisionFailureKind =
  | "retryable"
  | "operator_required"
  | "ambiguous"
  | "failed";

export const META_REVIEW_DEPROVISION_TIMEOUT_MS = 45_000;

export function classifyMetaReviewDeprovisionFailure(
  status: number | null,
  message: string,
): MetaReviewDeprovisionFailureKind {
  const normalizedMessage = message.toLowerCase();
  if (
    (status === 503 &&
      (normalizedMessage.includes("permanently fenced") ||
        normalizedMessage.includes("audited operator reconciliation"))) ||
    (status === 502 && normalizedMessage.includes("requires operator action"))
  ) {
    return "operator_required";
  }
  if (status === null) return "ambiguous";
  if (status === 502 || status === 503) return "retryable";
  return "failed";
}
