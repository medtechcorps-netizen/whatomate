import { computed, ref, watch } from 'vue'
import { contactsService } from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { useUsersStore } from '@/stores/users'

export interface AssignableUser {
  id: string
  full_name: string
  email: string
  role_name?: string
}

// The people the chat Assign dialog offers. Assigning needs contacts:write,
// but the user list behind it needs users:read, which default managers lack.
// Without users:read the organization's member list (organizations:read) is
// used instead; it is membership-aware like the server's assignee check.
export function useAssignableUsers() {
  const authStore = useAuthStore()
  const usersStore = useUsersStore()
  const organizationsStore = useOrganizationsStore()
  const members = ref<AssignableUser[] | null>(null)
  let loaded = false
  let loadGeneration = 0

  const users = computed<AssignableUser[]>(() => {
    if (members.value) return members.value
    return usersStore.users
      .filter(user => user.is_active)
      .map(user => ({
        id: user.id,
        full_name: user.full_name,
        email: user.email,
        role_name: user.role?.name,
      }))
  })

  async function load() {
    loaded = true
    const generation = ++loadGeneration
    if (authStore.hasPermission('users', 'read')) {
      members.value = null
      await usersStore.fetchUsers().catch(() => undefined)
      return
    }
    if (!authStore.hasPermission('organizations', 'read')) {
      members.value = []
      return
    }
    try {
      const response = await contactsService.assignableMembers()
      if (generation !== loadGeneration) return
      const data = (response.data as any)?.data ?? response.data
      members.value = (data?.members ?? [])
        .filter((member: any) => member?.is_active === true && member.user_id && member.full_name)
        .map((member: any) => ({
          id: member.user_id,
          full_name: member.full_name,
          email: member.email ?? '',
          role_name: member.role_name ?? undefined,
        }))
    } catch {
      if (generation === loadGeneration) members.value = []
    }
  }

  function nameOf(userId: string | null | undefined) {
    if (!userId) return undefined
    return users.value.find(user => user.id === userId)?.full_name
  }

  // A workspace switch must never offer the previous organization's people.
  watch(
    () => organizationsStore.selectedOrgId || authStore.organizationId || '',
    (scope, previous) => {
      if (scope === previous) return
      loadGeneration++
      members.value = null
      if (loaded) void load()
    },
  )

  return { users, load, nameOf }
}
