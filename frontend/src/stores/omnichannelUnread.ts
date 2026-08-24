import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { channelsService } from '@/services/productSuite'

function normalizeOrganizationId(id: string | null) {
  const normalized = id?.trim() ?? ''
  return normalized || null
}

function normalizeUserId(id: string | null | undefined) {
  const normalized = id?.trim() ?? ''
  return normalized || null
}

function isAbortError(error: unknown) {
  return (
    (typeof DOMException !== 'undefined' &&
      error instanceof DOMException &&
      error.name === 'AbortError') ||
    (typeof error === 'object' &&
      error !== null &&
      'code' in error &&
      (error as { code?: unknown }).code === 'ERR_CANCELED')
  )
}

function isAccessDenied(error: unknown) {
  if (typeof error !== 'object' || error === null || !('response' in error)) return false
  const response = (error as { response?: { status?: unknown } }).response
  return response?.status === 401 || response?.status === 402 || response?.status === 403
}

function unreadCountFromResponse(response: unknown) {
  const envelope = response as {
    data?: {
      data?: { unread_conversations?: unknown }
      unread_conversations?: unknown
    }
  }
  const payload = envelope?.data?.data ?? envelope?.data
  const count = payload?.unread_conversations
  if (!Number.isSafeInteger(count) || (count as number) < 0) {
    throw new Error('Invalid omnichannel attention summary')
  }
  return count as number
}

export const useOmnichannelUnreadStore = defineStore('omnichannelUnread', () => {
  const organizationId = ref<string | null>(null)
  const userId = ref<string | null>(null)
  const unreadConversationCount = ref<number | null>(null)
  const loading = ref(false)
  const stale = ref(true)

  const hasUnread = computed(() => (unreadConversationCount.value ?? 0) > 0)
  const displayCount = computed(() => {
    const count = unreadConversationCount.value
    if (count === null) return ''
    return count > 99 ? '99+' : String(count)
  })

  let identityGeneration = 0
  let refreshGeneration = 0
  let refreshController: AbortController | null = null

  function cancelRefresh() {
    refreshGeneration++
    refreshController?.abort()
    refreshController = null
    loading.value = false
  }

  function setIdentity(id: string | null, authenticatedUserId: string | null | undefined) {
    const nextOrganizationId = normalizeOrganizationId(id)
    const nextUserId = normalizeUserId(authenticatedUserId)
    if (organizationId.value === nextOrganizationId && userId.value === nextUserId) return

    identityGeneration++
    cancelRefresh()
    organizationId.value = nextOrganizationId
    userId.value = nextUserId
    unreadConversationCount.value = null
    stale.value = true
  }

  async function refresh(
    id: string | null,
    authenticatedUserId: string | null | undefined,
  ): Promise<boolean> {
    const requestedOrganizationId = normalizeOrganizationId(id)
    const requestedUserId = normalizeUserId(authenticatedUserId)
    setIdentity(requestedOrganizationId, requestedUserId)
    if (!requestedOrganizationId || !requestedUserId) return false

    const identity = identityGeneration
    const requestGeneration = ++refreshGeneration
    refreshController?.abort()
    const controller = new AbortController()
    refreshController = controller
    loading.value = true

    try {
      const response = await channelsService.attentionSummary(
        requestedOrganizationId,
        controller.signal,
      )
      if (
        controller.signal.aborted ||
        identity !== identityGeneration ||
        requestGeneration !== refreshGeneration ||
        organizationId.value !== requestedOrganizationId ||
        userId.value !== requestedUserId
      ) {
        return false
      }

      unreadConversationCount.value = unreadCountFromResponse(response)
      stale.value = false
      return true
    } catch (error) {
      if (
        !controller.signal.aborted &&
        !isAbortError(error) &&
        identity === identityGeneration &&
        requestGeneration === refreshGeneration &&
        organizationId.value === requestedOrganizationId &&
        userId.value === requestedUserId
      ) {
        // Never retain a protected tenant's count after access is revoked.
        // For transient failures, keep the last known value until retry.
        if (isAccessDenied(error)) unreadConversationCount.value = null
        stale.value = true
      }
      return false
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === refreshGeneration &&
        refreshController === controller
      ) {
        refreshController = null
        loading.value = false
      }
    }
  }

  function resetForIdentityChange() {
    identityGeneration++
    cancelRefresh()
    organizationId.value = null
    userId.value = null
    unreadConversationCount.value = null
    stale.value = true
  }

  return {
    organizationId,
    userId,
    unreadConversationCount,
    loading,
    stale,
    hasUnread,
    displayCount,
    setIdentity,
    refresh,
    resetForIdentityChange,
  }
})
