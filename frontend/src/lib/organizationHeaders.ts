export function resolveOrganizationHeader(
  explicitHeader: unknown,
  selectedOrganizationId: string | null,
): string | undefined {
  if (typeof explicitHeader === "string" && explicitHeader.trim()) {
    return explicitHeader;
  }
  return selectedOrganizationId || undefined;
}
