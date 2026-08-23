import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { rolesService, permissionsService, type Role, type Permission } from '@/services/api'
import { RESOURCE_LABELS } from '@/lib/constants'

export interface CreateRoleData {
  name: string
  description?: string
  is_default?: boolean
  permissions: string[]
}

export interface UpdateRoleData {
  name?: string
  description?: string
  is_default?: boolean
  permissions?: string[]
}

export interface FetchRolesParams {
  search?: string
  page?: number
  limit?: number
}

export interface FetchRolesResponse {
  roles: Role[]
  total: number
  page: number
  limit: number
}

// Group permissions by resource for the UI
export interface PermissionGroup {
  resource: string
  label: string
  permissions: Permission[]
}

export const useRolesStore = defineStore('roles', () => {
  const roles = ref<Role[]>([])
  const permissions = ref<Permission[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let identityGeneration = 0
  let rolesFetchGeneration = 0
  let permissionsFetchGeneration = 0
  let rolesFetchController: AbortController | null = null
  let permissionsFetchController: AbortController | null = null

  function resetForIdentityChange() {
    identityGeneration++
    rolesFetchGeneration++
    permissionsFetchGeneration++
    rolesFetchController?.abort()
    permissionsFetchController?.abort()
    rolesFetchController = null
    permissionsFetchController = null
    roles.value = []
    permissions.value = []
    loading.value = false
    error.value = null
  }

  // Group permissions by resource
  const permissionGroups = computed<PermissionGroup[]>(() => {
    const groups: Record<string, Permission[]> = {}

    for (const perm of permissions.value) {
      if (!groups[perm.resource]) {
        groups[perm.resource] = []
      }
      groups[perm.resource].push(perm)
    }

    return Object.entries(groups)
      .map(([resource, perms]) => ({
        resource,
        label: RESOURCE_LABELS[resource] || resource.charAt(0).toUpperCase() + resource.slice(1),
        permissions: perms.sort((a, b) => a.action.localeCompare(b.action))
      }))
      .sort((a, b) => a.label.localeCompare(b.label))
  })

  async function fetchRoles(params?: FetchRolesParams): Promise<FetchRolesResponse> {
    const identity = identityGeneration
    const requestGeneration = ++rolesFetchGeneration
    rolesFetchController?.abort()
    const controller = new AbortController()
    rolesFetchController = controller
    loading.value = true
    error.value = null
    try {
      const response = await rolesService.list(params, controller.signal)
      const data = (response.data as any).data || response.data
      const fetchedRoles = data.roles || []
      if (
        identity === identityGeneration &&
        requestGeneration === rolesFetchGeneration
      ) {
        roles.value = fetchedRoles
      }
      return {
        roles: fetchedRoles,
        total: data.total ?? fetchedRoles.length,
        page: data.page ?? 1,
        limit: data.limit ?? 50
      }
    } catch (err: any) {
      if (
        identity === identityGeneration &&
        requestGeneration === rolesFetchGeneration
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch roles'
      }
      throw err
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === rolesFetchGeneration
      ) {
        loading.value = false
        if (rolesFetchController === controller) rolesFetchController = null
      }
    }
  }

  async function fetchRole(id: string): Promise<Role> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await rolesService.get(id)
      const data = (response.data as any).data || response.data
      return data
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to fetch role'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function fetchPermissions(): Promise<void> {
    const identity = identityGeneration
    const requestGeneration = ++permissionsFetchGeneration
    permissionsFetchController?.abort()
    const controller = new AbortController()
    permissionsFetchController = controller
    try {
      const response = await permissionsService.list(controller.signal)
      if (
        identity === identityGeneration &&
        requestGeneration === permissionsFetchGeneration
      ) {
        permissions.value = (response.data as any).data?.permissions || response.data?.permissions || []
      }
    } catch (err: any) {
      if (
        identity === identityGeneration &&
        requestGeneration === permissionsFetchGeneration
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch permissions'
      }
      throw err
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === permissionsFetchGeneration &&
        permissionsFetchController === controller
      ) {
        permissionsFetchController = null
      }
    }
  }

  async function createRole(data: CreateRoleData): Promise<Role> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await rolesService.create(data)
      const newRole = (response.data as any).data || response.data
      if (identity === identityGeneration) roles.value.unshift(newRole)
      return newRole
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to create role'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function updateRole(id: string, data: UpdateRoleData): Promise<Role> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await rolesService.update(id, data)
      const updatedRole = (response.data as any).data || response.data
      if (identity === identityGeneration) {
        const index = roles.value.findIndex(r => r.id === id)
        if (index !== -1) {
          roles.value[index] = updatedRole
        }
      }
      return updatedRole
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to update role'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function deleteRole(id: string): Promise<void> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      await rolesService.delete(id)
      if (identity === identityGeneration) {
        roles.value = roles.value.filter(r => r.id !== id)
      }
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to delete role'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  return {
    roles,
    permissions,
    permissionGroups,
    loading,
    error,
    fetchRoles,
    fetchRole,
    fetchPermissions,
    createRole,
    updateRole,
    deleteRole,
    resetForIdentityChange,
  }
})
