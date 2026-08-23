import { clearLegacyWhatsAppReplyAttemptNamespace } from './legacyWhatsAppReplyAttempts'

export const SELECTED_ORGANIZATION_STORAGE_KEY = 'selected_organization_id'

let selectedOrganizationIdentityCleared = false

export function readSelectedOrganizationId() {
  if (selectedOrganizationIdentityCleared) return null
  try {
    return localStorage.getItem(SELECTED_ORGANIZATION_STORAGE_KEY)
  } catch {
    return null
  }
}

export function setSelectedOrganizationId(organizationId: string) {
  try {
    localStorage.setItem(SELECTED_ORGANIZATION_STORAGE_KEY, organizationId)
    if (localStorage.getItem(SELECTED_ORGANIZATION_STORAGE_KEY) === organizationId) {
      selectedOrganizationIdentityCleared = false
    }
  } catch {
    // Keep the in-memory suppression active; omitting an override is safer
    // than sending a request with an unverified tenant identifier.
  }
}

export function clearBrowserOrganizationIdentity() {
  selectedOrganizationIdentityCleared = true
  try {
    localStorage.removeItem(SELECTED_ORGANIZATION_STORAGE_KEY)
  } catch {
    // A revoked storage backend must not block auth teardown.
  }
  try {
    if (localStorage.getItem(SELECTED_ORGANIZATION_STORAGE_KEY) !== null) {
      localStorage.setItem(SELECTED_ORGANIZATION_STORAGE_KEY, '')
    }
  } catch {
    // The in-memory suppression still prevents stale request headers.
  }
  clearLegacyWhatsAppReplyAttemptNamespace()
}
