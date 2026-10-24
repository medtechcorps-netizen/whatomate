import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { ssoExchangeInterrupted } from '@/services/api'
import { routes } from './routes'

// Permission-based route meta type
declare module 'vue-router' {
  interface RouteMeta {
    requiresAuth?: boolean
    permission?: string // Resource permission required (e.g., 'analytics', 'chat')
    permissions?: string[] // Every listed read permission is required.
    anyPermissions?: string[] // At least one listed read permission is required.
    entitlement?: string // Product feature entitlement required.
  }
}

// Get base path from server-injected config or fallback to Vite's BASE_URL
const injectedBasePath = (window as any).__BASE_PATH__
const viteBasePath = import.meta.env.BASE_URL === './' ? '/' : import.meta.env.BASE_URL
const basePath = injectedBasePath ?? viteBasePath ?? '/'
const normalizedBasePath = basePath.endsWith('/') ? basePath : basePath + '/'

const router = createRouter({
  history: createWebHistory(normalizedBasePath),
  routes,
})

// Navigation items with permissions in priority order (matches AppLayout.vue)
// Used to find the first accessible route for a user
interface NavigationCandidate {
  path: string
  permission?: string
  permissions?: string[]
  anyPermissions?: string[]
  entitlement?: string
  childPaths?: Array<{ path: string; permission: string }>
}

const navigationOrder: NavigationCandidate[] = [
  { path: '/resellers', permission: 'resellers' },
  { path: '/upgrade-workspace', permission: 'billing' },
  { path: '/launchpad', permission: 'onboarding' },
  { path: '/', permission: 'analytics' },
  { path: '/inbox', permissions: ['conversations', 'channel_accounts'], entitlement: 'omnichannel.enabled' },
  { path: '/chat', permission: 'chat' },
  { path: '/crm/pipeline', permissions: ['crm.leads', 'crm.pipelines'], entitlement: 'crm.enabled' },
  { path: '/crm/tasks', permission: 'tasks', entitlement: 'crm.enabled' },
  {
    path: '/crm/insights',
    anyPermissions: ['crm.leads', 'tasks', 'bookings', 'packages', 'payments'],
    entitlement: 'crm.enabled'
  },
  { path: '/crm/automations', permission: 'crm.automations', entitlement: 'crm.enabled' },
  { path: '/calendar', permissions: ['bookings', 'booking.settings'], entitlement: 'bookings.enabled' },
  { path: '/commerce', anyPermissions: ['packages', 'payments'], entitlement: 'commerce.enabled' },
  { path: '/copilot', permission: 'copilot', entitlement: 'copilot.enabled' },
  { path: '/chatbot', permission: 'settings.chatbot', childPaths: [
    { path: '/chatbot/keywords', permission: 'chatbot.keywords' },
    { path: '/chatbot/flows', permission: 'flows.chatbot' },
    { path: '/chatbot/ai', permission: 'chatbot.ai' }
  ]},
  { path: '/chatbot/transfers', permission: 'transfers' },
  { path: '/analytics/agents', permission: 'analytics.agents' },
  { path: '/analytics/meta-insights', permission: 'analytics' },
  { path: '/analytics/search-visibility', permission: 'analytics' },
  { path: '/templates', permission: 'templates' },
  { path: '/flows', permission: 'flows.whatsapp' },
  { path: '/campaigns', permission: 'campaigns' },
  { path: '/calling/logs', permission: 'call_logs', childPaths: [
    { path: '/calling/ivr-flows', permission: 'ivr_flows' },
    { path: '/calling/transfers', permission: 'call_transfers' }
  ]},
  { path: '/settings', permission: 'settings.general', childPaths: [
    { path: '/settings/integrations', permission: 'settings.integrations' },
    { path: '/settings/chatbot', permission: 'settings.chatbot' },
    { path: '/settings/accounts', permission: 'accounts' },
    { path: '/settings/canned-responses', permission: 'canned_responses' },
    { path: '/settings/contacts', permission: 'contacts' },
    { path: '/settings/tags', permission: 'tags' },
    { path: '/settings/teams', permission: 'teams' },
    { path: '/settings/users', permission: 'users' },
    { path: '/settings/roles', permission: 'roles' },
    { path: '/settings/api-keys', permission: 'api_keys' },
    { path: '/settings/webhooks', permission: 'webhooks' },
    { path: '/settings/custom-actions', permission: 'custom_actions' },
    { path: '/settings/sso', permission: 'settings.sso' },
    { path: '/settings/audit-logs', permission: 'audit_logs' },
    { path: '/settings/privacy', permission: 'privacy.settings' },
    { path: '/settings/privacy', permission: 'privacy.requests' },
    { path: '/settings/support', permission: 'support' }
  ]}
]

// Find the first accessible route for the user
function getFirstAccessibleRoute(authStore: ReturnType<typeof useAuthStore>): string {
  for (const item of navigationOrder) {
    if (item.entitlement && !authStore.hasProductEntitlement(item.entitlement)) {
      continue
    }
    const requiredPermissions = item.permissions ??
      (item.permission ? [item.permission] : [])
    const hasAllRequired = requiredPermissions.every(
      permission => authStore.hasPermission(permission, 'read')
    )
    const hasAnyRequired = !item.anyPermissions?.length ||
      item.anyPermissions.some(permission => authStore.hasPermission(permission, 'read'))
    const hasDirectRequirement = requiredPermissions.length > 0 ||
      Boolean(item.anyPermissions?.length)

    if (hasDirectRequirement && hasAllRequired && hasAnyRequired) {
      return item.path
    }
    // Check child paths if available
    if (item.childPaths) {
      for (const child of item.childPaths) {
        if (authStore.hasPermission(child.permission, 'read')) {
          return child.path
        }
      }
    }
  }
  // Fallback to profile (always accessible)
  return '/profile'
}

// Navigation guard
router.beforeEach(async (to, _from, next) => {
  const authStore = useAuthStore()

  // Check if route requires auth
  if (to.meta.requiresAuth !== false) {
    if (!authStore.isAuthenticated) {
      // An SSO exchange this tab sent was cut off (a reload while it was out):
      // the stored identity may not be the jar's session any more. Nothing is
      // restored in this tab until a sign-in settles it.
      if (ssoExchangeInterrupted()) {
        return next({ name: 'login', query: { redirect: to.fullPath } })
      }
      // Try to restore session from localStorage
      const restored = authStore.restoreSession()
      if (!restored) {
        return next({ name: 'login', query: { redirect: to.fullPath } })
      }
    }

    await authStore.ensureProductEntitlements()

    // Check permission-based access
    const requiredPermissions = to.meta.permissions ??
      (to.meta.permission ? [to.meta.permission] : [])
    const missingRequiredPermission = requiredPermissions.some(
      permission => !authStore.hasPermission(permission, 'read')
    )
    const missingAnyPermission = Boolean(
      to.meta.anyPermissions?.length &&
      !to.meta.anyPermissions.some(permission => authStore.hasPermission(permission, 'read'))
    )
    if (missingRequiredPermission || missingAnyPermission) {
      // Redirect to first accessible page
      return next({ path: getFirstAccessibleRoute(authStore) })
    }
    if (to.meta.entitlement && !authStore.hasProductEntitlement(to.meta.entitlement)) {
      return next({ path: getFirstAccessibleRoute(authStore) })
    }
  } else {
    // Redirect to appropriate page if already logged in
    if (authStore.isAuthenticated && (to.name === 'login' || to.name === 'register')) {
      return next({ path: getFirstAccessibleRoute(authStore) })
    }
  }

  next()
})

export default router
