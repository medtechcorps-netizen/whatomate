import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { organizationsService, usersService, type Organization } from '@/services/api'
import { clearLegacyWhatsAppReplyAttemptNamespace } from '@/lib/legacyWhatsAppReplyAttempts'
import {
  clearBrowserOrganizationIdentity,
  readSelectedOrganizationId,
  setSelectedOrganizationId,
} from '@/lib/browserIdentity'

export const useOrganizationsStore = defineStore('organizations', () => {
  const organizations = ref<Organization[]>([])
  const myOrganizations = ref<Array<{
    organization_id: string
    name: string
    slug: string
    role_name: string
    is_default: boolean
  }>>([])
  const selectedOrgId = ref<string | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)
  const organizationSwitchBlocker = ref<{
    owner: string
    message: string
  } | null>(null)
  let identityGeneration = 0
  let organizationsLoadGeneration = 0
  let myOrganizationsLoadGeneration = 0
  let organizationsLoadController: AbortController | null = null
  let myOrganizationsLoadController: AbortController | null = null

  const selectedOrganization = computed(() => {
    if (!selectedOrgId.value) return null
    return organizations.value.find(org => org.id === selectedOrgId.value) || null
  })

  const isMultiOrg = computed(() => myOrganizations.value.length > 1)

  function isAbortError(err: unknown) {
    return (
      (err instanceof DOMException && err.name === 'AbortError') ||
      (typeof err === 'object' && err !== null && 'code' in err && (err as any).code === 'ERR_CANCELED')
    )
  }

  // Initialize from localStorage
  function init() {
    const stored = readSelectedOrganizationId()
    if (stored) {
      selectedOrgId.value = stored
    }
  }

  async function fetchOrganizations(): Promise<void> {
    const generation = ++organizationsLoadGeneration
    const identity = identityGeneration
    organizationsLoadController?.abort()
    const controller = new AbortController()
    organizationsLoadController = controller
    loading.value = true
    error.value = null
    try {
      const response = await organizationsService.list(controller.signal)
      if (generation !== organizationsLoadGeneration || identity !== identityGeneration) return
      organizations.value = (response.data as any).data?.organizations || response.data?.organizations || []
    } catch (err: any) {
      if (
        generation === organizationsLoadGeneration &&
        identity === identityGeneration &&
        !isAbortError(err)
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch organizations'
        organizations.value = []
      }
    } finally {
      if (generation === organizationsLoadGeneration) {
        loading.value = false
        if (organizationsLoadController === controller) organizationsLoadController = null
      }
    }
  }

  async function fetchMyOrganizations(): Promise<void> {
    const generation = ++myOrganizationsLoadGeneration
    const identity = identityGeneration
    myOrganizationsLoadController?.abort()
    const controller = new AbortController()
    myOrganizationsLoadController = controller
    try {
      const response = await usersService.listMyOrganizations(controller.signal)
      if (generation !== myOrganizationsLoadGeneration || identity !== identityGeneration) return
      myOrganizations.value = (response.data as any).data?.organizations || []
    } catch (err) {
      if (
        generation === myOrganizationsLoadGeneration &&
        identity === identityGeneration &&
        !isAbortError(err)
      ) {
        myOrganizations.value = []
      }
    } finally {
      if (myOrganizationsLoadController === controller) myOrganizationsLoadController = null
    }
  }

  function selectOrganization(orgId: string | null) {
    if (selectedOrgId.value !== orgId) clearLegacyWhatsAppReplyAttemptNamespace()
    selectedOrgId.value = orgId
    if (orgId) {
      setSelectedOrganizationId(orgId)
    } else {
      clearBrowserOrganizationIdentity()
    }
  }

  function resetForIdentityChange() {
    identityGeneration++
    organizationsLoadGeneration++
    myOrganizationsLoadGeneration++
    organizationsLoadController?.abort()
    organizationsLoadController = null
    myOrganizationsLoadController?.abort()
    myOrganizationsLoadController = null
    organizations.value = []
    myOrganizations.value = []
    selectedOrgId.value = null
    loading.value = false
    error.value = null
    organizationSwitchBlocker.value = null
    clearBrowserOrganizationIdentity()
  }

  function blockOrganizationSwitch(owner: string, message: string) {
    organizationSwitchBlocker.value = { owner, message }
  }

  function unblockOrganizationSwitch(owner: string) {
    if (organizationSwitchBlocker.value?.owner === owner) {
      organizationSwitchBlocker.value = null
    }
  }

  async function addMember(data: { email: string; role_id?: string }): Promise<void> {
    const identity = identityGeneration
    try {
      await organizationsService.addMember(data)
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to add member'
      }
      throw err
    }
  }

  async function createInvitation(roleId?: string): Promise<string> {
    const identity = identityGeneration
    try {
      const response = await organizationsService.createInvitation(roleId ? { role_id: roleId } : {})
      if (identity !== identityGeneration) throw new Error('Organization identity changed')
      return response.data.data.token
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to create invitation'
      }
      throw err
    }
  }

  return {
    organizations,
    myOrganizations,
    isMultiOrg,
    selectedOrgId,
    selectedOrganization,
    organizationSwitchBlocker,
    loading,
    error,
    init,
    fetchOrganizations,
    fetchMyOrganizations,
    selectOrganization,
    resetForIdentityChange,
    blockOrganizationSwitch,
    unblockOrganizationSwitch,
    addMember,
    createInvitation
  }
})
