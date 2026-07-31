<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { RouterLink, RouterView, useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  ChevronLeft,
  ChevronRight,
  Menu,
  X,
  Zap
} from 'lucide-vue-next'
import { wsService } from '@/services/websocket'
import { authService } from '@/services/api'
import OrganizationSwitcher from './OrganizationSwitcher.vue'
import UserMenu from './UserMenu.vue'
import ActiveCallPanel from '@/components/calling/ActiveCallPanel.vue'
import { ScrollToTop } from '@/components/shared'
import { navigationSections, type NavSection } from './navigation'
import ReReplyLogo from '@/components/brand/ReReplyLogo.vue'

useI18n() // Enable $t() in template

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const organizationsStore = useOrganizationsStore()
const isCollapsed = ref(false)
const isMobileMenuOpen = ref(false)

// Refresh user data and connect WebSocket on mount
onMounted(() => {
  if (authStore.isAuthenticated) {
    // Fetch fresh permissions in background (non-destructive — interceptor handles 401)
    authStore.refreshUserData()
    authStore.ensureProductEntitlements()

    wsService.connect(async () => {
      try {
        const resp = await authService.getWSToken()
        return resp.data.data.token
      } catch {
        return null
      }
    })
  }
})

function filterItems(items: NavSection['items']) {
  const canAccessItem = (item: NavSection['items'][number]) => {
    if (item.entitlement && !authStore.hasProductEntitlement(item.entitlement)) {
      return false
    }
    if (item.requiredPermissions && !item.requiredPermissions.every(p => authStore.hasPermission(p, 'read'))) {
      return false
    }
    if (item.anyPermissions && !item.anyPermissions.some(p => authStore.hasPermission(p, 'read'))) {
      return false
    }
    if (item.childPermissions) {
      return item.childPermissions.some(p => authStore.hasPermission(p, 'read'))
    }
    return !item.permission || authStore.hasPermission(item.permission, 'read')
  }

  return items
    .filter(canAccessItem)
    .map(item => {
      const filteredChildren = item.children?.filter(canAccessItem)

      let effectivePath = item.path
      if (item.childPermissions && item.permission && !authStore.hasPermission(item.permission, 'read') && filteredChildren?.length) {
        effectivePath = filteredChildren[0].path
      }

      const originalPath = item.path
      const matchesPath = (path: string, exact = false) =>
        route.path === path || (!exact && route.path.startsWith(`${path}/`))
      const childIsActive = filteredChildren?.some(child =>
        matchesPath(child.path, child.exact)
      ) ?? false
      const isActive = originalPath === '/'
        ? route.name === 'dashboard'
        : originalPath === '/chat'
          ? route.name === 'chat' || route.name === 'chat-conversation'
          : childIsActive || matchesPath(originalPath, item.exact)

      return {
        ...item,
        path: effectivePath,
        active: isActive,
        children: filteredChildren
      }
    })
}

// Filter navigation sections based on user permissions
const navSections = computed(() => {
  return navigationSections
    .map(section => ({
      ...section,
      items: filterItems(section.items)
    }))
    .filter(section => section.items.length > 0)
})

const mainSections = computed(() => navSections.value.filter(s => !s.pinBottom))
const bottomSections = computed(() => navSections.value.filter(s => s.pinBottom))

// Mobile is intentionally a focused companion experience. Keep the full
// workspace available on desktop while exposing only the three workflows that
// are useful on a small screen.
const mobileNavItems = computed(() => {
  const items = navSections.value.flatMap(section => section.items)
  const dashboard = items.find(item => item.path === '/')
  const inbox = items.find(item => item.path === '/inbox')
  const analyticsOptions = items.filter(item => item.path.startsWith('/analytics/'))
  const currentAnalytics = analyticsOptions.find(
    item => item.path === route.path && item.path.startsWith('/analytics/'),
  )
  const analytics = currentAnalytics
    ?? analyticsOptions.find(item => item.path === '/analytics/agents')
    ?? analyticsOptions.find(item => item.path === '/analytics/meta-insights')

  const focusedItems = []
  if (dashboard) focusedItems.push(dashboard)
  if (analytics) {
    focusedItems.push({
      ...analytics,
      name: 'nav.analytics',
      active: route.path.startsWith('/analytics/'),
      children: analyticsOptions,
    })
  }
  if (inbox) focusedItems.push(inbox)

  return focusedItems
})
const canManageWorkspaceLicense = computed(() =>
  authStore.hasPermission('billing', 'read')
)
const activeOrganizationId = computed(() =>
  authStore.user?.is_super_admin
    ? organizationsStore.selectedOrgId || authStore.organizationId
    : authStore.organizationId
)
const workspaceUpgradeRoute = computed(() => ({
  path: '/upgrade-workspace',
  query: activeOrganizationId.value
    ? { organization_id: activeOrganizationId.value }
    : undefined
}))

const toggleSidebar = () => {
  isCollapsed.value = !isCollapsed.value
}

const handleLogout = async () => {
  await authStore.logout()
  router.push('/login')
}
</script>

<template>
  <div class="flex h-screen bg-[#0a0a0b] light:bg-slate-100">
    <!-- Skip link for accessibility -->
    <a href="#main-content" class="skip-link">{{ $t('nav.skipToMain') }}</a>

    <!-- Mobile header -->
    <header class="fixed top-0 left-0 right-0 z-50 flex h-12 items-center justify-between border-b border-white/[0.08] light:border-slate-300 bg-[#0a0a0b]/95 light:bg-slate-50/95 backdrop-blur-sm px-3 lg:hidden">
      <RouterLink to="/" class="flex items-center gap-2">
        <ReReplyLogo size="sm" tone="light" class="light:hidden" />
        <ReReplyLogo size="sm" tone="dark" class="hidden light:inline-flex" />
      </RouterLink>
      <Button
        variant="ghost"
        size="icon"
        class="h-11 w-11 text-white/70 hover:text-white hover:bg-white/[0.08] light:text-slate-700 light:hover:text-slate-950 light:hover:bg-slate-200"
        aria-label="Open mobile workspace menu"
        :aria-expanded="isMobileMenuOpen"
        @click="isMobileMenuOpen = !isMobileMenuOpen"
      >
        <X v-if="isMobileMenuOpen" class="h-5 w-5" />
        <Menu v-else class="h-5 w-5" />
      </Button>
    </header>

    <!-- Mobile menu overlay -->
    <div
      v-if="isMobileMenuOpen"
      class="fixed inset-0 z-40 bg-black/60 light:bg-black/30 backdrop-blur-sm lg:hidden"
      @click="isMobileMenuOpen = false"
    />

    <!-- Sidebar -->
    <aside
      :class="[
        'flex flex-col border-r border-white/[0.08] light:border-slate-300 bg-[#0a0a0b] light:bg-slate-50 transition-all duration-300',
        'fixed inset-y-0 left-0 z-40 lg:relative',
        'transform lg:transform-none',
        isMobileMenuOpen ? 'translate-x-0' : '-translate-x-full lg:translate-x-0',
        isCollapsed ? 'w-64 lg:w-16' : 'w-64'
      ]"
      role="navigation"
      aria-label="Main navigation"
    >
      <!-- Logo (hidden on mobile, shown in header instead) -->
      <div class="hidden h-12 items-center justify-between border-b border-white/[0.08] px-3 light:border-slate-300 lg:flex">
        <RouterLink to="/" class="flex items-center gap-2">
          <ReReplyLogo :compact="isCollapsed" size="sm" tone="light" class="light:hidden" />
          <ReReplyLogo :compact="isCollapsed" size="sm" tone="dark" class="hidden light:inline-flex" />
        </RouterLink>
        <Button
          variant="ghost"
          size="icon"
          class="h-7 w-7 text-white/50 hover:text-white hover:bg-white/[0.08] light:text-gray-400 light:hover:text-gray-900 light:hover:bg-gray-100"
          :aria-label="isCollapsed ? $t('nav.expandSidebar') : $t('nav.collapseSidebar')"
          :aria-expanded="!isCollapsed"
          @click="toggleSidebar"
        >
          <ChevronLeft v-if="!isCollapsed" class="h-3.5 w-3.5" />
          <ChevronRight v-else class="h-3.5 w-3.5" />
        </Button>
      </div>
      <!-- Mobile logo spacer -->
      <div class="h-12 lg:hidden" />

      <!-- Organization Switcher (Super Admin only) -->
      <OrganizationSwitcher :collapsed="isCollapsed" />

      <!-- Navigation -->
      <ScrollArea class="flex-1 py-2">
        <div class="px-2 lg:hidden" role="navigation" aria-label="Mobile workspace">
          <p class="px-2.5 pb-2 pt-1 text-[10px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-slate-600">
            Mobile workspace
          </p>
          <div class="space-y-1">
            <template v-for="item in mobileNavItems" :key="item.path">
              <RouterLink
                :to="item.path"
                :class="[
                  'nav-active-indicator btn-press flex min-h-11 items-center gap-3 rounded-xl px-3 py-2.5 text-sm font-semibold transition-all duration-200',
                  item.active
                    ? 'bg-white/[0.08] text-white light:bg-slate-200 light:text-slate-950'
                    : 'text-white/55 hover:text-white hover:bg-white/[0.06] light:text-slate-700 light:hover:text-slate-950 light:hover:bg-slate-200/70'
                ]"
                data-mobile-primary
                :data-active="item.active"
                :aria-current="item.active ? 'page' : undefined"
                @click="isMobileMenuOpen = false"
              >
                <component :is="item.icon" class="h-5 w-5 shrink-0" aria-hidden="true" />
                <span>{{ $t(item.name) }}</span>
              </RouterLink>

              <div
                v-if="item.name === 'nav.analytics' && item.active && item.children && item.children.length > 1"
                class="ml-5 space-y-1 border-l border-white/[0.08] pl-3 light:border-slate-300"
                aria-label="Analytics views"
              >
                <RouterLink
                  v-for="child in item.children"
                  :key="child.path"
                  :to="child.path"
                  :class="[
                    'btn-press flex min-h-11 items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors',
                    route.path === child.path
                      ? 'bg-white/[0.06] text-white light:bg-slate-200/80 light:text-slate-950'
                      : 'text-white/45 hover:bg-white/[0.04] hover:text-white/80 light:text-slate-600 light:hover:bg-slate-200/60 light:hover:text-slate-900'
                  ]"
                  :aria-current="route.path === child.path ? 'page' : undefined"
                  @click="isMobileMenuOpen = false"
                >
                  <component :is="child.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
                  <span>{{ $t(child.name) }}</span>
                </RouterLink>
              </div>
            </template>
          </div>
          <p class="px-3 pt-4 text-xs leading-5 text-white/35 light:text-slate-600">
            Advanced workspace tools are available on desktop.
          </p>
        </div>

        <nav class="hidden px-2 lg:block" role="menubar">
          <template v-for="(section, sIdx) in mainSections" :key="section.label">
            <!-- Section header -->
            <div
              v-if="section.label && !isCollapsed"
              :class="['px-2.5 pt-4 pb-1 text-[10px] font-semibold uppercase tracking-wider text-white/30 light:text-gray-400', sIdx === 0 && 'pt-1']"
            >
              {{ $t(section.label) }}
            </div>
            <div v-else-if="sIdx > 0" :class="['my-2 mx-2.5 border-t border-white/[0.06] light:border-gray-200', isCollapsed && 'mx-1']" />

            <!-- Section items -->
            <div class="space-y-0.5">
              <template v-for="item in section.items" :key="item.path">
                <RouterLink
                  :to="item.path"
                  :class="[
                    'nav-active-indicator btn-press flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] font-medium transition-all duration-200',
                    item.active
                      ? 'bg-white/[0.08] text-white light:bg-gray-100 light:text-gray-900'
                      : 'text-white/50 hover:text-white hover:bg-white/[0.06] light:text-gray-500 light:hover:text-gray-900 light:hover:bg-gray-50',
                    isCollapsed && 'lg:justify-center lg:px-2'
                  ]"
                  :data-active="item.active"
                  role="menuitem"
                  :aria-current="item.active ? 'page' : undefined"
                  @click="isMobileMenuOpen = false"
                >
                  <component :is="item.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
                  <span :class="isCollapsed && 'lg:sr-only'">{{ $t(item.name) }}</span>
                </RouterLink>

                <!-- Submenu items -->
                <template v-if="item.children && item.active && !isCollapsed">
                  <RouterLink
                    v-for="child in item.children"
                    :key="child.path"
                    :to="child.path"
                    :class="[
                      'flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-[13px] font-medium transition-all duration-200 ml-4',
                      route.path === child.path
                        ? 'bg-white/[0.06] text-white light:bg-gray-100 light:text-gray-900'
                        : 'text-white/40 hover:text-white/70 hover:bg-white/[0.04] light:text-gray-400 light:hover:text-gray-700 light:hover:bg-gray-50'
                    ]"
                    role="menuitem"
                    :aria-current="route.path === child.path ? 'page' : undefined"
                    @click="isMobileMenuOpen = false"
                  >
                    <component :is="child.icon" class="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
                    <span>{{ $t(child.name) }}</span>
                  </RouterLink>
                </template>
              </template>
            </div>
          </template>
        </nav>
      </ScrollArea>

      <!-- Licensing shortcut follows the billing catalog authorization. -->
      <div
        v-if="canManageWorkspaceLicense"
        class="hidden border-t border-white/[0.06] px-2 py-2 light:border-slate-300 lg:block"
      >
        <RouterLink
          :to="workspaceUpgradeRoute"
          title="Upgrade the selected workspace"
          :class="[
            'btn-press flex items-center gap-2.5 rounded-lg border border-amber-300/25 bg-amber-300/[0.09] px-2.5 py-2 text-[13px] font-semibold text-amber-200 transition-all duration-200 hover:bg-amber-300/[0.14] light:text-amber-800',
            isCollapsed && 'lg:justify-center lg:px-2'
          ]"
          @click="isMobileMenuOpen = false"
        >
          <Zap class="h-4 w-4 shrink-0" aria-hidden="true" />
          <span :class="isCollapsed && 'lg:sr-only'">Upgrade workspace</span>
        </RouterLink>
      </div>

      <!-- Bottom-pinned navigation (Settings) -->
      <div v-if="bottomSections.length > 0" class="hidden border-t border-white/[0.06] px-2 py-2 light:border-slate-300 lg:block">
        <template v-for="section in bottomSections" :key="section.label">
          <template v-for="item in section.items" :key="item.path">
            <RouterLink
              :to="item.path"
              :class="[
                'nav-active-indicator btn-press flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] font-medium transition-all duration-200',
                item.active
                  ? 'bg-white/[0.08] text-white light:bg-gray-100 light:text-gray-900'
                  : 'text-white/50 hover:text-white hover:bg-white/[0.06] light:text-gray-500 light:hover:text-gray-900 light:hover:bg-gray-50',
                isCollapsed && 'lg:justify-center lg:px-2'
              ]"
              :data-active="item.active"
              role="menuitem"
              :aria-current="item.active ? 'page' : undefined"
              @click="isMobileMenuOpen = false"
            >
              <component :is="item.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
              <span :class="isCollapsed && 'lg:sr-only'">{{ $t(item.name) }}</span>
            </RouterLink>

            <template v-if="item.children && item.active && !isCollapsed">
              <RouterLink
                v-for="child in item.children"
                :key="child.path"
                :to="child.path"
                :class="[
                  'flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-[13px] font-medium transition-all duration-200 ml-4',
                  route.path === child.path
                    ? 'bg-white/[0.06] text-white light:bg-gray-100 light:text-gray-900'
                    : 'text-white/40 hover:text-white/70 hover:bg-white/[0.04] light:text-gray-400 light:hover:text-gray-700 light:hover:bg-gray-50'
                ]"
                role="menuitem"
                :aria-current="route.path === child.path ? 'page' : undefined"
                @click="isMobileMenuOpen = false"
              >
                <component :is="child.icon" class="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
                <span>{{ $t(child.name) }}</span>
              </RouterLink>
            </template>
          </template>
        </template>
      </div>

      <!-- User Menu -->
      <UserMenu :collapsed="isCollapsed" @logout="handleLogout" />
    </aside>

    <!-- Main content -->
    <main id="main-content" class="flex-1 overflow-hidden bg-[#0a0a0b] pt-12 light:bg-slate-100 lg:pt-0" role="main">
      <RouterView v-slot="{ Component, route: viewRoute }">
        <Transition name="page" mode="out-in">
          <component :is="Component" :key="viewRoute.meta.stableKey ? String(viewRoute.name) : viewRoute.path" />
        </Transition>
      </RouterView>
      <ActiveCallPanel />
      <ScrollToTop />
    </main>
  </div>
</template>
