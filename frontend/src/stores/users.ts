import { defineStore } from 'pinia'
import { ref } from 'vue'
import { usersService } from '@/services/api'

export interface UserRole {
  id: string
  name: string
  description?: string
  is_system: boolean
}

export interface User {
  id: string
  email: string
  full_name: string
  role_id?: string
  role?: UserRole
  is_active: boolean
  is_super_admin?: boolean
  is_member?: boolean
  organization_id: string
  created_at: string
  updated_at: string
}

export interface CreateUserData {
  email: string
  password: string
  full_name: string
  role_id?: string
  is_super_admin?: boolean
}

export interface UpdateUserData {
  email?: string
  password?: string
  full_name?: string
  role_id?: string
  is_active?: boolean
  is_super_admin?: boolean
}

export interface FetchUsersParams {
  search?: string
  page?: number
  limit?: number
  role_id?: string
  online_only?: boolean
}

export interface FetchUsersResponse {
  users: User[]
  total: number
  page: number
  limit: number
  online_count: number
}

export const useUsersStore = defineStore('users', () => {
  const users = ref<User[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let identityGeneration = 0
  let fetchGeneration = 0
  let fetchController: AbortController | null = null

  function resetForIdentityChange() {
    identityGeneration++
    fetchGeneration++
    fetchController?.abort()
    fetchController = null
    users.value = []
    loading.value = false
    error.value = null
  }

  async function fetchUsers(params?: FetchUsersParams): Promise<FetchUsersResponse> {
    const identity = identityGeneration
    const requestGeneration = ++fetchGeneration
    fetchController?.abort()
    const controller = new AbortController()
    fetchController = controller
    loading.value = true
    error.value = null
    try {
      const response = await usersService.list(params, controller.signal)
      const data = response.data.data || response.data
      const fetchedUsers = data.users || []
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        users.value = fetchedUsers
      }
      return {
        users: fetchedUsers,
        total: data.total ?? fetchedUsers.length,
        page: data.page ?? 1,
        limit: data.limit ?? 50,
        online_count: data.online_count ?? 0
      }
    } catch (err: any) {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch users'
      }
      throw err
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        loading.value = false
        if (fetchController === controller) fetchController = null
      }
    }
  }

  async function fetchUser(id: string): Promise<User> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await usersService.get(id)
      return response.data.data || response.data
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to fetch user'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function createUser(data: CreateUserData): Promise<User> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await usersService.create(data)
      const newUser = response.data.data
      if (identity === identityGeneration) users.value.unshift(newUser)
      return newUser
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to create user'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function updateUser(id: string, data: UpdateUserData): Promise<User> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await usersService.update(id, data)
      const updatedUser = response.data.data
      if (identity === identityGeneration) {
        const index = users.value.findIndex(u => u.id === id)
        if (index !== -1) {
          users.value[index] = updatedUser
        }
      }
      return updatedUser
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to update user'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function deleteUser(id: string): Promise<void> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      await usersService.delete(id)
      if (identity === identityGeneration) {
        users.value = users.value.filter(u => u.id !== id)
      }
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to delete user'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  return {
    users,
    loading,
    error,
    fetchUsers,
    fetchUser,
    createUser,
    updateUser,
    deleteUser,
    resetForIdentityChange,
  }
})
