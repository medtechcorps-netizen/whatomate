import { computed, onScopeDispose, ref, watch } from 'vue'
import { contactsService, usersService } from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'

export interface AssignableUser {
  id: string
  full_name: string
  email: string
  role_name?: string
}

const PAGE_SIZE = 100
const SEARCH_DELAY_MS = 250

// Both authorized directories are paged. Keep the picker's results private:
// the shared settings users store can contain another query or workspace.
export function useAssignableUsers() {
  const authStore = useAuthStore()
  const organizationsStore = useOrganizationsStore()
  const users = ref<AssignableUser[]>([])
  const search = ref('')
  const loading = ref(false)
  const failed = ref(false)
  const hasMore = ref(false)
  let nextPage = 1
  let directoryTotal: number | null = null
  let seenPeopleIds = new Set<string>()
  let loaded = false
  let disposed = false
  let generation = 0
  let controller: AbortController | null = null
  let searchTimer: ReturnType<typeof setTimeout> | null = null

  const organizationId = computed(() => organizationsStore.selectedOrgId || authStore.organizationId || '')
  const directory = computed(() => {
    if (!authStore.user?.id || !authStore.hasPermission('contacts', 'write')) return ''
    if (authStore.hasPermission('users', 'read')) return 'users'
    if (authStore.hasPermission('organizations', 'read')) return 'members'
    return ''
  })

  function invalidate() {
    generation++
    controller?.abort()
    controller = null
    if (searchTimer !== null) clearTimeout(searchTimer)
    searchTimer = null
    users.value = []
    nextPage = 1
    directoryTotal = null
    seenPeopleIds = new Set()
    hasMore.value = false
    failed.value = false
    loading.value = false
  }

  async function fetchPage() {
    const org = organizationId.value
    const source = directory.value
    if (disposed || !org || !source) return
    const requestGeneration = generation
    const page = nextPage
    const params = { page, limit: PAGE_SIZE, search: search.value.trim() || undefined }
    const requestController = new AbortController()
    controller = requestController
    loading.value = true
    failed.value = false
    try {
      const response = source === 'users'
        ? await usersService.list(params, requestController.signal, org)
        : await contactsService.assignableMembers(params, requestController.signal, org)
      if (requestGeneration !== generation || requestController.signal.aborted) return
      const data = response.data?.data ?? response.data
      // A null members slice is the server's valid empty-page representation.
      const rows = data?.[source] ?? (data?.total === 0 ? [] : undefined)
      if (!Array.isArray(rows) || data.page !== page || data.limit !== PAGE_SIZE ||
        !Number.isSafeInteger(data.total) || data.total < 0 || rows.length > PAGE_SIZE ||
        (directoryTotal !== null && data.total !== directoryTotal) ||
        rows.length !== Math.min(PAGE_SIZE, Math.max(0, data.total - (page - 1) * PAGE_SIZE))) {
        throw new Error('Incomplete assignment directory page')
      }
      const people = new Map(users.value.map(user => [user.id, user]))
      const pageIds = new Set(seenPeopleIds)
      for (const row of rows) {
        const id = source === 'users' ? row?.id : row?.user_id
        if (typeof id !== 'string' || !id) continue
        // Offset pages can shift after a concurrent insertion/removal. A
        // duplicate would hide a missing person if merely deduplicated.
        if (pageIds.has(id)) throw new Error('Assignment directory pages overlap')
        pageIds.add(id)
        if (row.is_active !== true || typeof row.full_name !== 'string' || !row.full_name.trim()) {
          continue
        }
        people.set(id, {
          id,
          full_name: row.full_name,
          email: typeof row.email === 'string' ? row.email : '',
          role_name: (source === 'users' ? row.role?.name : row.role_name) ?? undefined,
        })
      }
      users.value = [...people.values()]
      directoryTotal = data.total
      seenPeopleIds = pageIds
      hasMore.value = page * PAGE_SIZE < data.total
      nextPage = page + 1
    } catch {
      if (requestGeneration !== generation || requestController.signal.aborted) return
      // Never leave a failed partial directory looking complete. Retry starts
      // at page one, since offset pagination may have changed in the meantime.
      users.value = []
      hasMore.value = false
      nextPage = 1
      directoryTotal = null
      seenPeopleIds = new Set()
      failed.value = true
    } finally {
      if (requestGeneration === generation) {
        loading.value = false
        if (controller === requestController) controller = null
      }
    }
  }

  async function load() {
    if (disposed) return
    loaded = true
    invalidate()
    await fetchPage()
  }

  async function loadMore() {
    if (loading.value || failed.value || !hasMore.value) return
    await fetchPage()
  }

  async function ensureLoaded() {
    if (!loaded) await load()
  }

  function nameOf(userId: string | null | undefined) {
    return userId ? users.value.find(user => user.id === userId)?.full_name : undefined
  }

  watch(
    () => [organizationId.value, authStore.user, directory.value],
    () => {
      invalidate()
      if (loaded) void fetchPage()
    },
    { flush: 'sync' },
  )
  watch(
    () => search.value.trim(),
    () => {
      invalidate()
      if (!loaded || !organizationId.value || !directory.value) return
      // Clear old matches immediately, including during the debounce window.
      loading.value = true
      searchTimer = setTimeout(() => {
        searchTimer = null
        void fetchPage()
      }, SEARCH_DELAY_MS)
    },
    { flush: 'sync' },
  )
  onScopeDispose(() => {
    disposed = true
    invalidate()
  })

  return { users, search, loading, failed, hasMore, load, loadMore, ensureLoaded, nameOf }
}
