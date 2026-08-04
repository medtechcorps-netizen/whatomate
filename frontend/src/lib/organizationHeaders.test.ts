import { describe, expect, it } from "vitest";
import { resolveOrganizationHeader } from "./organizationHeaders";

describe("resolveOrganizationHeader", () => {
  it("preserves an explicitly pinned organization", () => {
    expect(resolveOrganizationHeader("org-at-launch", "org-current")).toBe(
      "org-at-launch",
    );
  });

  it("falls back to the currently selected organization", () => {
    expect(resolveOrganizationHeader(undefined, "org-current")).toBe(
      "org-current",
    );
    expect(resolveOrganizationHeader("", "org-current")).toBe("org-current");
  });
});
