export interface RegistrationReconciliationAccess {
  canWrite: boolean;
  isNew: boolean;
  activeOrganizationId: string | null;
  loadedOrganizationId: string | null;
  status: string | null | undefined;
}

// Keep the safety action tenant-pinned and unavailable for every state except
// the one the backend can reconcile with its strict status CAS.
export function canOfferRegistrationReconciliation(
  access: RegistrationReconciliationAccess,
): boolean {
  return Boolean(
    access.canWrite &&
    !access.isNew &&
    access.activeOrganizationId &&
    access.loadedOrganizationId === access.activeOrganizationId &&
    access.status === "pending_registration",
  );
}
