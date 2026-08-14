import { describe, expect, it } from "vitest";
import { resolveOrganizationHeader } from "./organizationHeaders";

describe("resolveOrganizationHeader", () => {
  it("preserves an explicitly pinned organization", () => {
    expect(resolveOrganizationHeader("org-at-launch", "org-current")).toBe(
      "org-at-launch",
    );
  });

  it("uses the selected organization when no explicit pin exists", () => {
    expect(resolveOrganizationHeader(undefined, "org-current")).toBe(
      "org-current",
    );
    expect(resolveOrganizationHeader("", "org-current")).toBe("org-current");
  });

  it("omits the header when neither organization is available", () => {
    expect(resolveOrganizationHeader(undefined, null)).toBeUndefined();
  });
});
