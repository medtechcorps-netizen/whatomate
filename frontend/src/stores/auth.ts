import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/services/api'
import {
  clearBrowserOrganizationIdentity,
  setSelectedOrganizationId,
} from '@/lib/browserIdentity'

export interface UserSettings {
  email_notifications?: boolean
  new_message_alerts?: boolean
  campaign_updates?: boolean
}

export interface Permission {
  id: string
  resource: string
  action: string
  description?: string
}

export interface UserRole {
  id: string
  name: string
  description?: string
  is_system: boolean
  permissions?: Permission[]
}

export interface User {
  id: string
  email: string
  full_name: string
  role_id?: string
  role?: UserRole
  organization_id: string
  organization_name?: string
  settings?: UserSettings
  is_available?: boolean
  is_super_admin?: boolean
  is_reseller_admin?: boolean
}

export interface AuthState {
  user: User | null
}

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(null)
  const breakStartedAt = ref<string | null>(null)
  const productEntitlements = ref<Record<string, unknown>>({})
  const productEntitlementMode = ref('unlicensed')
  const productEntitlementsLoaded = ref(false)
  let productEntitlementsRequest: Promise<boolean> | null = null
  let identityGeneration = 0
  let authenticationRequestGeneration = 0
  let userRequestGeneration = 0
  let productEntitlementRequestGeneration = 0

  const isAuthenticated = computed(() => !!user.value)
  const userRole = computed(() => user.value?.role?.name || 'agent')
  const organizationId = computed(() => user.value?.organization_id || '')
  const userSettings = computed(() => user.value?.settings || {})
  const isAvailable = computed(() => user.value?.is_available ?? true)

  function safeSetLocalStorage(key: string, value: string) {
    try {
      localStorage.setItem(key, value)
    } catch {
      // In-memory identity remains authoritative when browser storage is denied.
    }
  }

  function safeGetLocalStorage(key: string) {
    try {
      return localStorage.getItem(key)
    } catch {
      return null
    }
  }

  function safeRemoveLocalStorage(key: string) {
    try {
      localStorage.removeItem(key)
      if (localStorage.getItem(key) === null) return
    } catch {
      // Fall through to a non-sensitive tombstone when removal is denied.
    }
    try {
      localStorage.setItem(key, '')
    } catch {
      // Continue clearing every other in-memory/browser identity surface.
    }
  }

  function applyAuth(authData: { user: User }, clearBreak = true) {
    identityGeneration++
    userRequestGeneration++
    user.value = authData.user
    resetProductEntitlements()
    if (clearBreak) {
      breakStartedAt.value = null
      safeRemoveLocalStorage('break_started_at')
    }
    safeSetLocalStorage('user', JSON.stringify(authData.user))
  }

  function setAuth(authData: { user: User }) {
    authenticationRequestGeneration++
    applyAuth(authData)
  }

  function clearPersistedIdentity() {
    // Run the dependency-free organization/attempt cleanup first so a throwing
    // localStorage implementation cannot skip tenant isolation.
    clearBrowserOrganizationIdentity()
    safeRemoveLocalStorage('user')
    safeRemoveLocalStorage('auth_token')
    safeRemoveLocalStorage('refresh_token')
    safeRemoveLocalStorage('break_started_at')
  }

  function clearAuth() {
    authenticationRequestGeneration++
    identityGeneration++
    userRequestGeneration++
    user.value = null
    breakStartedAt.value = null
    resetProductEntitlements()
    clearPersistedIdentity()
  }

  function beginAuthenticationAttempt() {
    const requestGeneration = ++authenticationRequestGeneration
    identityGeneration++
    userRequestGeneration++
    user.value = null
    breakStartedAt.value = null
    resetProductEntitlements()
    clearPersistedIdentity()
    return requestGeneration
  }

  /**
   * Restore session from localStorage (synchronous, no API calls).
   * Returns true if a valid user object was found in localStorage.
   * Does NOT verify the session with the server — the API interceptor
   * handles 401s and token refresh automatically.
   */
  function restoreSession(): boolean {
    const storedUser = safeGetLocalStorage('user')

    // Remove legacy token keys if present
    if (safeGetLocalStorage('auth_token')) {
      safeRemoveLocalStorage('auth_token')
    }
    if (safeGetLocalStorage('refresh_token')) {
      safeRemoveLocalStorage('refresh_token')
    }

    if (storedUser) {
      try {
        const parsed = JSON.parse(storedUser)
        if (!parsed || typeof parsed !== 'object' || !parsed.id || !parsed.email) {
          clearAuth()
          return false
        }
        authenticationRequestGeneration++
        applyAuth({ user: parsed }, false)
        return true
      } catch {
        clearAuth()
        return false
      }
    }
    return false
  }

  // Fetch fresh user data from API (including updated permissions)
  async function refreshUserData(): Promise<boolean> {
    const requestGeneration = ++userRequestGeneration
    const identity = identityGeneration
    const expectedUserId = user.value?.id ?? null
    const expectedOrganizationId = user.value?.organization_id ?? null
    try {
      const response = await api.get('/me')
      if (
        requestGeneration !== userRequestGeneration ||
        identity !== identityGeneration ||
        user.value?.id !== expectedUserId ||
        user.value?.organization_id !== expectedOrganizationId
      ) return false
      const freshUser = response.data.data
      if (
        freshUser?.id !== expectedUserId ||
        freshUser?.organization_id !== expectedOrganizationId
      ) return false
      user.value = freshUser
      safeSetLocalStorage('user', JSON.stringify(freshUser))
      return true
    } catch {
      // If unauthorized, clear auth
      return false
    }
  }

  async function login(email: string, password: string): Promise<void> {
    const requestGeneration = beginAuthenticationAttempt()
    const response = await api.post('/auth/login', { email, password })
    if (requestGeneration !== authenticationRequestGeneration) return
    // Server sets cookies; response body has { user, expires_in }
    applyAuth({ user: response.data.data.user })
  }

  async function register(data: {
    email: string
    password: string
    full_name: string
    invitation_token: string
  }): Promise<void> {
    const requestGeneration = beginAuthenticationAttempt()
    const response = await api.post('/auth/register', data)
    if (requestGeneration !== authenticationRequestGeneration) return
    applyAuth({ user: response.data.data.user })
  }

  async function switchOrg(organizationId: string): Promise<void> {
    const requestGeneration = ++authenticationRequestGeneration
    const identity = identityGeneration
    const response = await api.post('/auth/switch-org', { organization_id: organizationId })
    if (
      requestGeneration !== authenticationRequestGeneration ||
      identity !== identityGeneration
    ) return
    applyAuth({ user: response.data.data.user })
    // Update localStorage org override
    setSelectedOrganizationId(organizationId)
  }

  async function logout(): Promise<void> {
    try {
      await api.post('/auth/logout', {})
    } catch {
      // Ignore logout errors
    } finally {
      clearAuth()
    }
  }

  function setAvailability(available: boolean, breakStart?: string | null) {
    if (user.value) {
      user.value = { ...user.value, is_available: available }
      safeSetLocalStorage('user', JSON.stringify(user.value))
    }
    // Track break start time
    if (!available && breakStart) {
      breakStartedAt.value = breakStart
      safeSetLocalStorage('break_started_at', breakStart)
    } else if (available) {
      breakStartedAt.value = null
      safeRemoveLocalStorage('break_started_at')
    }
  }

  function restoreBreakTime() {
    const stored = safeGetLocalStorage('break_started_at')
    if (stored && !isAvailable.value) {
      breakStartedAt.value = stored
    }
  }

  // Check if user has a specific permission
  function hasPermission(resource: string, action: string = 'read'): boolean {
    // Super admins have all permissions
    if (user.value?.is_super_admin) {
      return true
    }
    if (resource === 'resellers' && user.value?.is_reseller_admin) {
      return true
    }

    const permissions = user.value?.role?.permissions
    if (!permissions || permissions.length === 0) {
      return false
    }

    return permissions.some(p => p.resource === resource && p.action === action)
  }

  function resetProductEntitlements() {
    productEntitlementRequestGeneration++
    productEntitlements.value = {}
    productEntitlementMode.value = 'unlicensed'
    productEntitlementsLoaded.value = false
    productEntitlementsRequest = null
  }

  function entitlementValueAllows(value: unknown): boolean {
    if (typeof value === 'boolean') return value
    if (typeof value === 'number') return Number.isFinite(value) && value > 0
    if (typeof value === 'string') {
      return ['true', 'enabled', 'yes', '1'].includes(value.trim().toLowerCase())
    }
    return false
  }

  function hasProductEntitlement(key?: string): boolean {
    if (!key) return true
    if (!productEntitlementsLoaded.value) return false
    return entitlementValueAllows(productEntitlements.value[key])
  }

  async function fetchProductEntitlements(): Promise<boolean> {
    const requestGeneration = ++productEntitlementRequestGeneration
    const identity = identityGeneration
    const expectedUserId = user.value?.id ?? null
    const expectedOrganizationId = user.value?.organization_id ?? null
    try {
      const response = await api.get('/product/entitlements')
      if (
        requestGeneration !== productEntitlementRequestGeneration ||
        identity !== identityGeneration ||
        user.value?.id !== expectedUserId ||
        user.value?.organization_id !== expectedOrganizationId
      ) return false
      const payload = response.data?.data ?? response.data ?? {}
      productEntitlements.value =
        payload.entitlements && typeof payload.entitlements === 'object'
          ? payload.entitlements
          : {}
      productEntitlementMode.value =
        typeof payload.mode === 'string' ? payload.mode : 'unlicensed'
      productEntitlementsLoaded.value = true
      return true
    } catch {
      if (
        requestGeneration !== productEntitlementRequestGeneration ||
        identity !== identityGeneration ||
        user.value?.id !== expectedUserId ||
        user.value?.organization_id !== expectedOrganizationId
      ) return false
      // Licensing is fail-closed. Core modules remain usable, while routes
      // that require an explicit product entitlement stay unavailable.
      productEntitlements.value = {}
      productEntitlementMode.value = 'unavailable'
      productEntitlementsLoaded.value = true
      return false
    }
  }

  async function ensureProductEntitlements(): Promise<boolean> {
    if (productEntitlementsLoaded.value) return true
    if (!productEntitlementsRequest) {
      const request = fetchProductEntitlements()
      productEntitlementsRequest = request
      void request.finally(() => {
        if (productEntitlementsRequest === request) {
          productEntitlementsRequest = null
        }
      })
    }
    return productEntitlementsRequest
  }

  return {
    user,
    breakStartedAt,
    isAuthenticated,
    userRole,
    organizationId,
    userSettings,
    isAvailable,
    productEntitlements,
    productEntitlementMode,
    productEntitlementsLoaded,
    setAuth,
    clearAuth,
    restoreSession,
    restoreBreakTime,
    refreshUserData,
    login,
    register,
    switchOrg,
    logout,
    setAvailability,
    hasPermission,
    hasProductEntitlement,
    fetchProductEntitlements,
    ensureProductEntitlements
  }
})
