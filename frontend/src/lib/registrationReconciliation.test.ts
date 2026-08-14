import { describe, expect, it } from "vitest";
import { canOfferRegistrationReconciliation } from "./registrationReconciliation";

describe("registration reconciliation action", () => {
  const eligible = {
    canWrite: true,
    isNew: false,
    activeOrganizationId: "org-lifelab",
    loadedOrganizationId: "org-lifelab",
    status: "pending_registration",
  };

  it("is offered only for the tenant-pinned pending registration row", () => {
    expect(canOfferRegistrationReconciliation(eligible)).toBe(true);
  });

  it.each([
    ["read-only operator", { canWrite: false }],
    ["new account", { isNew: true }],
    ["different selected tenant", { activeOrganizationId: "org-other" }],
    ["unloaded tenant", { loadedOrganizationId: null }],
    ["active account", { status: "active" }],
    ["subscription recovery", { status: "pending_subscription" }],
  ])("is hidden for %s", (_label, override) => {
    expect(
      canOfferRegistrationReconciliation({ ...eligible, ...override }),
    ).toBe(false);
  });
});
