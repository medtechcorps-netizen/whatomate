import { useContactsStore } from '@/stores/contacts'
import { useTransfersStore } from '@/stores/transfers'
import { useCallingStore } from '@/stores/calling'
import { useTagsStore } from '@/stores/tags'
import { useUsersStore } from '@/stores/users'
import { useRolesStore } from '@/stores/roles'
import { useTeamsStore } from '@/stores/teams'
import { useAuthStore } from '@/stores/auth'
import { useNotesStore } from '@/stores/notes'
import { useOmnichannelUnreadStore } from '@/stores/omnichannelUnread'
import { toast } from 'vue-sonner'
import router from '@/router'

// Notification sound
let notificationSound: HTMLAudioElement | null = null

function playNotificationSound() {
  if (!notificationSound) {
    notificationSound = new Audio('/notification.mp3')
    notificationSound.volume = 0.5
  }
  notificationSound.currentTime = 0
  notificationSound.play().catch(() => {
    // Ignore autoplay errors (browser may block until user interaction)
  })
}

// Show toast notification with click handler
function showNotification(title: string, body: string, contactId: string) {
  toast.info(title, {
    description: body,
    duration: 5000,
    action: {
      label: 'View',
      onClick: () => {
        router.push(`/chat/${contactId}`)
      },
      actionButtonStyle: {
        background: 'transparent',
        border: '1px solid #e5e7eb',
        color: '#3b82f6',
        fontWeight: '500'
      }
    }
  })
}

// WebSocket message types
const WS_TYPE_AUTH = 'auth'
const WS_TYPE_NEW_MESSAGE = 'new_message'
const WS_TYPE_STATUS_UPDATE = 'status_update'
const WS_TYPE_SET_CONTACT = 'set_contact'
const WS_TYPE_PING = 'ping'
const WS_TYPE_PONG = 'pong'

// Reaction types
const WS_TYPE_REACTION_UPDATE = 'reaction_update'

// Agent transfer types
const WS_TYPE_AGENT_TRANSFER = 'agent_transfer'
const WS_TYPE_AGENT_TRANSFER_RESUME = 'agent_transfer_resume'
const WS_TYPE_AGENT_TRANSFER_ASSIGN = 'agent_transfer_assign'
const WS_TYPE_TRANSFER_ESCALATION = 'transfer_escalation'

// Campaign types
const WS_TYPE_CAMPAIGN_STATS_UPDATE = 'campaign_stats_update'

// Permission types
const WS_TYPE_PERMISSIONS_UPDATED = 'permissions_updated'
const WS_TYPE_CHANNEL_SYNC = 'channel_sync'
const WS_TYPE_REALTIME_SYNC = 'realtime_sync'

// Call types
const WS_TYPE_CALL_INCOMING = 'call_incoming'
const WS_TYPE_CALL_ANSWERED = 'call_answered'
const WS_TYPE_CALL_ENDED = 'call_ended'

// Call transfer types
const WS_TYPE_CALL_TRANSFER_WAITING = 'call_transfer_waiting'
const WS_TYPE_CALL_TRANSFER_CONNECTED = 'call_transfer_connected'
const WS_TYPE_CALL_TRANSFER_COMPLETED = 'call_transfer_completed'
const WS_TYPE_CALL_TRANSFER_ABANDONED = 'call_transfer_abandoned'
const WS_TYPE_CALL_TRANSFER_NO_ANSWER = 'call_transfer_no_answer'

// Outgoing call types
const WS_TYPE_OUTGOING_CALL_INITIATED = 'outgoing_call_initiated'
const WS_TYPE_OUTGOING_CALL_RINGING = 'outgoing_call_ringing'
const WS_TYPE_OUTGOING_CALL_ANSWERED = 'outgoing_call_answered'
const WS_TYPE_OUTGOING_CALL_REJECTED = 'outgoing_call_rejected'
const WS_TYPE_OUTGOING_CALL_ENDED = 'outgoing_call_ended'

// Conversation note types
const WS_TYPE_CONVERSATION_NOTE_CREATED = 'conversation_note_created'
const WS_TYPE_CONVERSATION_NOTE_UPDATED = 'conversation_note_updated'
const WS_TYPE_CONVERSATION_NOTE_DELETED = 'conversation_note_deleted'

interface WSMessage {
  type: string
  payload: any
}

export type InboxActivityType =
  | typeof WS_TYPE_NEW_MESSAGE
  | typeof WS_TYPE_STATUS_UPDATE
  | typeof WS_TYPE_CHANNEL_SYNC
  | typeof WS_TYPE_REALTIME_SYNC

export interface InboxActivityEvent {
  type: InboxActivityType
  payload: any
}

/**
 * True when an inbox activity can change a user's unread-conversation count.
 * Unknown realtime kinds intentionally refresh so a rolling deployment cannot
 * hide a future inbound invalidation; known delivery-only events are skipped.
 */
export function isUnreadRelevantInboxActivity(event: InboxActivityEvent) {
  if (event.type === WS_TYPE_STATUS_UPDATE) return false
  if (event.type === WS_TYPE_NEW_MESSAGE) {
    return event.payload?.direction !== 'outgoing'
  }
  if (event.type === WS_TYPE_REALTIME_SYNC) {
    return event.payload?.kind !== 'message_status_changed'
  }
  return event.type === WS_TYPE_CHANNEL_SYNC
}

/** True when the normalized conversation/message lists need a canonical fetch. */
export function isInboxContentRefreshActivity(event: InboxActivityEvent) {
  if (event.type === WS_TYPE_STATUS_UPDATE) return false
  if (event.type === WS_TYPE_REALTIME_SYNC) {
    return event.payload?.kind !== 'message_status_changed'
  }
  return event.type === WS_TYPE_NEW_MESSAGE || event.type === WS_TYPE_CHANNEL_SYNC
}

export type WebSocketConnectionState =
  | 'disconnected'
  | 'connecting'
  | 'connected'
  | 'reconnecting'

class WebSocketService {
  private ws: WebSocket | null = null
  private reconnectAttempts = 0
  private reconnectDelay = 1000
  private maxReconnectDelay = 30000
  private reconnectTimer: number | null = null
  private connectTimeoutTimer: number | null = null
  private pingInterval: number | null = null
  private heartbeatWatchdogInterval: number | null = null
  private lastPongAt = 0
  private nativeInboxRefreshTimer: number | null = null
  private nativeInboxRefreshTimerGeneration: number | null = null
  private nativeInboxRefreshInFlightGeneration: number | null = null
  private nativeInboxRefreshQueuedGeneration: number | null = null
  private connectInFlightGeneration: number | null = null
  private shouldReconnect = false
  private connectionGeneration = 0
  private isConnected = false
  private hasConnectedBefore = false
  private connectionState: WebSocketConnectionState = 'disconnected'
  private campaignStatsCallbacks: ((payload: any) => void)[] = []
  private channelSyncCallbacks: ((payload: any) => void)[] = []
  private inboxActivityCallbacks: ((event: InboxActivityEvent) => void)[] = []
  private connectionStateCallbacks: ((state: WebSocketConnectionState) => void)[] = []
  private getTokenFn: (() => Promise<string | null>) | null = null

  async connect(getToken?: () => Promise<string | null>) {
    // Store the token function for reconnects
    if (getToken) {
      this.getTokenFn = getToken
    }

    this.shouldReconnect = true
    const generation = this.connectionGeneration

    if (
      this.ws?.readyState === WebSocket.OPEN ||
      this.ws?.readyState === WebSocket.CONNECTING ||
      this.connectInFlightGeneration === generation
    ) {
      return
    }

    if (!this.getTokenFn) {
      this.setConnectionState('disconnected')
      return
    }

    this.clearReconnectTimer()
    this.connectInFlightGeneration = generation
    this.setConnectionState(this.hasConnectedBefore ? 'reconnecting' : 'connecting')

    try {
      // Get a fresh short-lived WS token for every connection attempt.
      const token = await this.getTokenFn()
      if (!this.shouldReconnect || generation !== this.connectionGeneration) {
        return
      }
      if (!token) {
        this.scheduleReconnect()
        return
      }

      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const host = window.location.host
      const basePath = ((window as any).__BASE_PATH__ ?? '').replace(/\/$/, '')
      const url = `${protocol}//${host}${basePath}/ws`
      const socket = new WebSocket(url)
      this.ws = socket
      this.startConnectTimeout(socket, generation)

      socket.onopen = () => {
        if (this.ws !== socket || !this.shouldReconnect) {
          socket.close()
          return
        }
        this.clearConnectTimeout()
        // Send auth message as the first message (token not in URL for security)
        this.send({ type: WS_TYPE_AUTH, payload: { token } })

        const isReconnection = this.hasConnectedBefore
        this.isConnected = true
        this.hasConnectedBefore = true
        this.reconnectAttempts = 0
        this.clearReconnectTimer()
        this.setConnectionState('connected')
        this.startPing()

        // Force refresh data after reconnection to sync any missed updates
        if (isReconnection) {
          this.refreshStaleData()
        }
      }

      socket.onmessage = (event) => {
        if (this.ws !== socket || !this.shouldReconnect) return
        this.handleMessage(event.data)
      }

      socket.onclose = () => {
        if (this.ws !== socket) return
        this.clearConnectTimeout()
        this.ws = null
        this.isConnected = false
        this.stopPing()
        if (this.shouldReconnect) {
          this.scheduleReconnect()
        } else {
          this.setConnectionState('disconnected')
        }
      }

      socket.onerror = () => {
        if (this.ws === socket) this.forceReconnect(socket)
      }
    } catch {
      if (this.shouldReconnect && generation === this.connectionGeneration) {
        this.scheduleReconnect()
      }
    } finally {
      if (this.connectInFlightGeneration === generation) {
        this.connectInFlightGeneration = null
      }
    }
  }

  disconnect() {
    this.shouldReconnect = false
    this.connectionGeneration++
    this.clearReconnectTimer()
    this.clearConnectTimeout()
    this.stopPing()
    this.clearNativeInboxRefresh()
    this.resetTenantInboxState()
    const socket = this.ws
    this.ws = null
    if (socket) {
      socket.onopen = null
      socket.onmessage = null
      socket.onclose = null
      socket.onerror = null
      socket.close()
    }
    this.isConnected = false
    this.hasConnectedBefore = false
    this.reconnectAttempts = 0
    this.getTokenFn = null
    this.setConnectionState('disconnected')
  }

  private handleMessage(data: string) {
    if (typeof data !== 'string') return
    if (data.trim() === WS_TYPE_PONG) {
      this.recordPong()
      return
    }
    try {
      const message: WSMessage = JSON.parse(data)
      const store = useContactsStore()

      switch (message.type) {
        case WS_TYPE_NEW_MESSAGE:
          this.handleNewMessage(store, message.payload)
          this.emitInboxActivity(WS_TYPE_NEW_MESSAGE, message.payload)
          break
        case WS_TYPE_STATUS_UPDATE:
          this.handleStatusUpdate(store, message.payload)
          this.emitInboxActivity(WS_TYPE_STATUS_UPDATE, message.payload)
          break
        case WS_TYPE_AGENT_TRANSFER:
          this.handleAgentTransfer(message.payload)
          break
        case WS_TYPE_AGENT_TRANSFER_RESUME:
          this.handleAgentTransferResume(message.payload)
          break
        case WS_TYPE_AGENT_TRANSFER_ASSIGN:
          this.handleAgentTransferAssign(message.payload)
          break
        case WS_TYPE_TRANSFER_ESCALATION:
          this.handleTransferEscalation(message.payload)
          break
        case WS_TYPE_REACTION_UPDATE:
          this.handleReactionUpdate(store, message.payload)
          break
        case WS_TYPE_PONG:
          this.recordPong()
          break
        case WS_TYPE_CAMPAIGN_STATS_UPDATE:
          this.handleCampaignStatsUpdate(message.payload)
          break
        case WS_TYPE_PERMISSIONS_UPDATED:
          this.handlePermissionsUpdated()
          break
        case WS_TYPE_CHANNEL_SYNC:
          this.emitChannelSync(message.payload)
          this.emitInboxActivity(WS_TYPE_CHANNEL_SYNC, message.payload)
          break
        case WS_TYPE_REALTIME_SYNC:
          this.scheduleNativeInboxRefresh()
          this.emitInboxActivity(WS_TYPE_REALTIME_SYNC, message.payload)
          break
        case WS_TYPE_CALL_INCOMING:
          this.handleCallIncoming(message.payload)
          break
        case WS_TYPE_CALL_ANSWERED:
        case WS_TYPE_CALL_ENDED:
          useCallingStore().handleCallEvent(message.type, message.payload)
          break
        case WS_TYPE_CALL_TRANSFER_WAITING:
          this.handleCallTransferWaiting(message.payload)
          break
        case WS_TYPE_CALL_TRANSFER_CONNECTED:
        case WS_TYPE_CALL_TRANSFER_COMPLETED:
        case WS_TYPE_CALL_TRANSFER_ABANDONED:
        case WS_TYPE_CALL_TRANSFER_NO_ANSWER:
          useCallingStore().handleCallEvent(message.type, message.payload)
          break
        case WS_TYPE_OUTGOING_CALL_INITIATED:
        case WS_TYPE_OUTGOING_CALL_RINGING:
        case WS_TYPE_OUTGOING_CALL_ANSWERED:
        case WS_TYPE_OUTGOING_CALL_REJECTED:
        case WS_TYPE_OUTGOING_CALL_ENDED:
          useCallingStore().handleCallEvent(message.type, message.payload)
          break
        case WS_TYPE_CONVERSATION_NOTE_CREATED:
          useNotesStore().addNote(message.payload)
          break
        case WS_TYPE_CONVERSATION_NOTE_UPDATED:
          useNotesStore().onNoteUpdated(message.payload)
          break
        case WS_TYPE_CONVERSATION_NOTE_DELETED:
          useNotesStore().onNoteDeleted(message.payload.id)
          break
        default:
          // Unknown message type, ignore
          break
      }
    } catch {
      // Failed to parse message, ignore
    }
  }

  private handleNewMessage(store: ReturnType<typeof useContactsStore>, payload: any) {
    // Check if this message is for the current contact
    const currentContact = store.currentContact
    const isViewingThisContact = currentContact && payload.contact_id === currentContact.id

    if (isViewingThisContact) {
      // Add message to the store
      store.addMessage({
        id: payload.id,
        contact_id: payload.contact_id,
        direction: payload.direction,
        message_type: payload.message_type,
        content: payload.content,
        media_url: payload.media_url,
        media_mime_type: payload.media_mime_type,
        media_filename: payload.media_filename,
        interactive_data: payload.interactive_data,
        status: payload.status,
        wamid: payload.wamid,
        error_message: payload.error_message,
        is_reply: payload.is_reply,
        reply_to_message_id: payload.reply_to_message_id,
        reply_to_message: payload.reply_to_message,
        reactions: payload.reactions,
        whatsapp_account: payload.whatsapp_account,
        ingested_at: payload.ingested_at,
        created_at: payload.created_at,
        updated_at: payload.updated_at
      })
    }

    // Show toast notification for incoming messages if:
    // 1. Message is incoming (from customer, not chatbot/agent)
    // 2. Current user is assigned to this contact
    // 3. User has new_message_alerts enabled
    // 4. User is not currently viewing this contact
    if (payload.direction === 'incoming' && !isViewingThisContact) {
      const authStore = useAuthStore()
      const currentUserId = authStore.user?.id
      const settings = authStore.userSettings

      // Check if user is assigned to this contact
      const isAssignedToUser = payload.assigned_user_id === currentUserId

      // Check if new message alerts are enabled (default to true if not set)
      const alertsEnabled = settings.new_message_alerts !== false

      if (isAssignedToUser && alertsEnabled) {
        const senderName = payload.profile_name || 'Unknown'
        const messagePreview = payload.content?.body || 'New message'
        const preview = messagePreview.length > 50
          ? messagePreview.substring(0, 50) + '...'
          : messagePreview
        const contactId = payload.contact_id

        // Play notification sound and show browser notification
        playNotificationSound()
        showNotification(senderName, preview, contactId)
      }
    }

    // Read acknowledgement belongs to ChatView, which owns the actual scroll
    // viewport. Document focus alone cannot prove that the new bubble is on
    // screen (the reader may be inspecting older history).
    store.fetchContacts()
  }

  private handleStatusUpdate(store: ReturnType<typeof useContactsStore>, payload: any) {
    store.updateMessageStatus(payload.message_id, payload.status, payload.error_message)
  }

  private handleReactionUpdate(store: ReturnType<typeof useContactsStore>, payload: any) {
    // Update the message reactions if we're viewing the contact
    const currentContact = store.currentContact
    if (currentContact && payload.contact_id === currentContact.id) {
      store.updateMessageReactions(payload.message_id, payload.reactions)
    }
  }

  private handleAgentTransfer(payload: any) {
    const transfersStore = useTransfersStore()
    const authStore = useAuthStore()

    // Add transfer to store with default SLA values
    transfersStore.addTransfer({
      id: payload.id,
      contact_id: payload.contact_id,
      contact_name: payload.contact_name || payload.phone_number,
      phone_number: payload.phone_number,
      whatsapp_account: payload.whatsapp_account,
      status: payload.status,
      source: payload.source || 'manual',
      agent_id: payload.agent_id,
      team_id: payload.team_id,
      notes: payload.notes,
      transferred_at: payload.transferred_at,
      // Default SLA values - will be updated on next fetch
      sla_breached: false,
      escalation_level: 0
    })

    // Refresh to get complete data including SLA fields
    transfersStore.fetchTransfers({ status: 'active' })

    // Toast only the assigned agent — managers/admins get notification noise
    // for every transfer in the org otherwise. They can keep the Transfers
    // page open if they want live updates.
    const currentUserId = authStore.user?.id
    const isAssignedToMe = payload.agent_id === currentUserId

    if (isAssignedToMe) {
      const contactName = payload.contact_name || payload.phone_number
      toast.info('New Transfer', {
        description: `${contactName} has been transferred to ${isAssignedToMe ? 'you' : 'agent queue'}`,
        duration: 5000,
        action: {
          label: 'View',
          onClick: () => router.push('/chatbot/transfers')
        }
      })
    }
  }

  private handleAgentTransferResume(payload: any) {
    const transfersStore = useTransfersStore()

    const updated = transfersStore.updateTransfer(payload.id, {
      status: payload.status,
      resumed_at: payload.resumed_at,
      resumed_by: payload.resumed_by
    })

    // If transfer wasn't found in store, refresh to get latest data
    if (!updated) {
      transfersStore.fetchTransfers()
    }
  }

  private handleAgentTransferAssign(payload: any) {
    const transfersStore = useTransfersStore()
    const authStore = useAuthStore()

    // Try to update existing transfer
    transfersStore.updateTransfer(payload.id, {
      agent_id: payload.agent_id,
      team_id: payload.team_id
    })

    // Always refresh to ensure UI is in sync (queue counts, etc.)
    transfersStore.fetchTransfers()

    // Notify if assigned to current user
    const currentUserId = authStore.user?.id
    if (payload.agent_id === currentUserId) {
      toast.info('Transfer Assigned', {
        description: 'A transfer has been assigned to you',
        duration: 5000,
        action: {
          label: 'View',
          onClick: () => router.push('/chatbot/transfers')
        }
      })
    }
  }

  private handleTransferEscalation(payload: any) {
    const authStore = useAuthStore()
    const currentUserId = authStore.user?.id

    // Check if current user should be notified
    const notifyIds: string[] = payload.escalation_notify_ids || []
    const shouldNotify = notifyIds.includes(currentUserId || '')

    // Trust the backend's escalation_notify_ids list — it already includes
    // the right people per the escalation rules. Don't broadcast to every
    // manager too; that's just noise and risks burying real alerts.
    if (shouldNotify) {
      const levelName = payload.level_name === 'critical' ? 'Critical' : 'Warning'
      const contactName = payload.contact_name || payload.phone_number

      // Play notification sound
      playNotificationSound()

      // Show urgent toast
      toast.warning(`SLA Escalation: ${levelName}`, {
        description: `${contactName} has been waiting since ${new Date(payload.waiting_since).toLocaleTimeString()}`,
        duration: 10000,
        action: {
          label: 'View',
          onClick: () => router.push('/chatbot/transfers')
        }
      })
    }
  }

  private handleCallIncoming(payload: any) {
    const callingStore = useCallingStore()
    callingStore.handleCallEvent('call_incoming', payload)

    const contactName = payload.caller_phone || 'Unknown'
    playNotificationSound()
    toast.info('Incoming Call', {
      description: `Call from ${contactName}`,
      duration: 5000,
      action: {
        label: 'View',
        onClick: () => router.push('/calling/logs')
      }
    })
  }

  private handleCallTransferWaiting(payload: any) {
    const callingStore = useCallingStore()
    callingStore.handleCallEvent('call_transfer_waiting', payload)

    const contactName = payload.caller_phone || 'Unknown'
    playNotificationSound()
    toast.info('Incoming Call Transfer', {
      description: `Call from ${contactName} waiting for an agent`,
      duration: 10000,
      action: {
        label: 'Accept',
        onClick: () => {
          router.push('/calling/transfers')
        }
      }
    })
  }

  private handleCampaignStatsUpdate(payload: any) {
    // Notify all registered callbacks
    this.campaignStatsCallbacks.forEach(callback => callback(payload))
  }

  private async handlePermissionsUpdated() {
    const authStore = useAuthStore()

    // Refresh user data from server
    const success = await authStore.refreshUserData()

    if (success) {
      toast.info('Permissions Updated', {
        description: 'Your permissions have been updated. The page will refresh.',
        duration: 3000
      })

      // Reload the page after a short delay to apply new permissions
      setTimeout(() => {
        window.location.reload()
      }, 1500)
    }
  }

  onCampaignStatsUpdate(callback: (payload: any) => void) {
    this.campaignStatsCallbacks.push(callback)
    // Return unsubscribe function
    return () => {
      const index = this.campaignStatsCallbacks.indexOf(callback)
      if (index > -1) {
        this.campaignStatsCallbacks.splice(index, 1)
      }
    }
  }

  onChannelSync(callback: (payload: any) => void) {
    this.channelSyncCallbacks.push(callback)
    return () => {
      const index = this.channelSyncCallbacks.indexOf(callback)
      if (index > -1) {
        this.channelSyncCallbacks.splice(index, 1)
      }
    }
  }

  onInboxActivity(callback: (event: InboxActivityEvent) => void) {
    this.inboxActivityCallbacks.push(callback)
    return () => {
      const index = this.inboxActivityCallbacks.indexOf(callback)
      if (index > -1) {
        this.inboxActivityCallbacks.splice(index, 1)
      }
    }
  }

  onConnectionStateChange(callback: (state: WebSocketConnectionState) => void) {
    this.connectionStateCallbacks.push(callback)
    try {
      callback(this.connectionState)
    } catch {
      // A connection-state observer must not break registration or reconnection.
    }
    return () => {
      const index = this.connectionStateCallbacks.indexOf(callback)
      if (index > -1) {
        this.connectionStateCallbacks.splice(index, 1)
      }
    }
  }

  private scheduleReconnect() {
    if (!this.shouldReconnect || this.reconnectTimer !== null) return

    this.reconnectAttempts++
    const delay = Math.min(
      this.reconnectDelay * Math.pow(2, Math.min(this.reconnectAttempts - 1, 10)),
      this.maxReconnectDelay,
    )
    this.setConnectionState('reconnecting')

    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null
      void this.connect()
    }, delay)
  }

  private clearReconnectTimer() {
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  private startConnectTimeout(socket: WebSocket, generation: number) {
    this.clearConnectTimeout()
    this.connectTimeoutTimer = window.setTimeout(() => {
      this.connectTimeoutTimer = null
      if (
        this.shouldReconnect &&
        generation === this.connectionGeneration &&
        this.ws === socket &&
        socket.readyState === WebSocket.CONNECTING
      ) {
        // A proxy/TLS handshake can leave the browser in CONNECTING forever.
        this.forceReconnect(socket)
      }
    }, 15000)
  }

  private clearConnectTimeout() {
    if (this.connectTimeoutTimer !== null) {
      window.clearTimeout(this.connectTimeoutTimer)
      this.connectTimeoutTimer = null
    }
  }

  private forceReconnect(socket: WebSocket) {
    if (this.ws !== socket) return
    this.clearConnectTimeout()
    this.ws = null
    this.isConnected = false
    this.stopPing()
    // Do not wait for a close event from the very transport path we have
    // declared unhealthy. Detach it and schedule the retry immediately.
    socket.onopen = null
    socket.onmessage = null
    socket.onclose = null
    socket.onerror = null
    try {
      socket.close()
    } catch {
      // The retry below is authoritative even if browser teardown throws.
    }
    if (this.shouldReconnect) {
      this.scheduleReconnect()
    } else {
      this.setConnectionState('disconnected')
    }
  }

  private emitInboxActivity(type: InboxActivityType, payload: any) {
    const event: InboxActivityEvent = { type, payload }
    for (const callback of [...this.inboxActivityCallbacks]) {
      try {
        callback(event)
      } catch {
        // A page-level subscriber must not break WebSocket event processing.
      }
    }
  }

  private emitChannelSync(payload: any) {
    for (const callback of [...this.channelSyncCallbacks]) {
      try {
        callback(payload)
      } catch {
        // Preserve newer inbox subscribers if a legacy observer fails.
      }
    }
  }

  private setConnectionState(state: WebSocketConnectionState) {
    if (this.connectionState === state) return
    this.connectionState = state
    for (const callback of [...this.connectionStateCallbacks]) {
      try {
        callback(state)
      } catch {
        // A connection-state observer must not break reconnection.
      }
    }
  }

  private scheduleNativeInboxRefresh() {
    const generation = this.connectionGeneration
    this.nativeInboxRefreshQueuedGeneration = generation
    if (
      this.nativeInboxRefreshTimerGeneration === generation ||
      this.nativeInboxRefreshInFlightGeneration === generation
    ) return

    if (this.nativeInboxRefreshTimer !== null) {
      window.clearTimeout(this.nativeInboxRefreshTimer)
    }

    this.nativeInboxRefreshTimerGeneration = generation
    this.nativeInboxRefreshTimer = window.setTimeout(() => {
      if (this.nativeInboxRefreshTimerGeneration !== generation) return
      this.nativeInboxRefreshTimer = null
      this.nativeInboxRefreshTimerGeneration = null
      void this.refreshNativeInbox(generation)
    }, 150)
  }

  private async refreshNativeInbox(generation: number) {
    if (!this.shouldReconnect || generation !== this.connectionGeneration) {
      if (this.nativeInboxRefreshQueuedGeneration === generation) {
        this.nativeInboxRefreshQueuedGeneration = null
      }
      return
    }
    this.nativeInboxRefreshInFlightGeneration = generation
    if (this.nativeInboxRefreshQueuedGeneration === generation) {
      this.nativeInboxRefreshQueuedGeneration = null
    }
    try {
      const store = useContactsStore()
      await Promise.allSettled([store.fetchContacts(), store.refreshCurrentMessages()])
    } finally {
      if (this.nativeInboxRefreshInFlightGeneration === generation) {
        this.nativeInboxRefreshInFlightGeneration = null
      }
      if (
        this.nativeInboxRefreshQueuedGeneration === generation &&
        this.shouldReconnect &&
        generation === this.connectionGeneration
      ) {
        this.scheduleNativeInboxRefresh()
      }
    }
  }

  private clearNativeInboxRefresh() {
    if (this.nativeInboxRefreshTimer !== null) {
      window.clearTimeout(this.nativeInboxRefreshTimer)
      this.nativeInboxRefreshTimer = null
    }
    this.nativeInboxRefreshTimerGeneration = null
    this.nativeInboxRefreshInFlightGeneration = null
    this.nativeInboxRefreshQueuedGeneration = null
  }

  private resetTenantInboxState() {
    // An already-started HTTP refresh cannot always be canceled by closing the
    // socket. Store-level generation guards make any late tenant response inert.
    try {
      useContactsStore().resetForIdentityChange()
    } catch {
      // The service can be torn down while Pinia itself is being disposed.
    }
    try {
      useNotesStore().clearNotes()
    } catch {
      // Keep socket teardown reliable even if a store is no longer available.
    }
    try {
      useOmnichannelUnreadStore().resetForIdentityChange()
    } catch {
      // Tenant-scoped navigation attention must not survive logout or org switch.
    }
    try {
      useTagsStore().resetForIdentityChange()
    } catch {
      // Keep socket teardown reliable even if a store is no longer available.
    }
    try {
      useTransfersStore().resetForIdentityChange()
    } catch {
      // Keep socket teardown reliable even if a store is no longer available.
    }
    try {
      useCallingStore().resetForIdentityChange()
    } catch {
      // Media/cache teardown is best effort only when Pinia is being disposed.
    }
    try {
      useUsersStore().resetForIdentityChange()
    } catch {
      // Assignment names and in-flight user requests are tenant scoped.
    }
    try {
      useRolesStore().resetForIdentityChange()
    } catch {
      // Settings caches must not survive an SPA identity transition.
    }
    try {
      useTeamsStore().resetForIdentityChange()
    } catch {
      // Routing and assignment team names are tenant scoped.
    }
  }

  setCurrentContact(contactId: string | null) {
    this.send({
      type: WS_TYPE_SET_CONTACT,
      payload: { contact_id: contactId || '' }
    })
  }

  private send(message: WSMessage) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(message))
    }
  }

  private startPing() {
    this.stopPing()
    this.lastPongAt = Date.now()
    this.pingInterval = window.setInterval(() => {
      this.send({ type: WS_TYPE_PING, payload: {} })
    }, 30000) // Ping every 30 seconds
    this.heartbeatWatchdogInterval = window.setInterval(() => {
      if (
        this.ws?.readyState === WebSocket.OPEN &&
        Date.now() - this.lastPongAt >= 75000
      ) {
        // OPEN does not guarantee a proxy path is still usable. Force the
        // reconnect/catch-up path when application-level pong frames stop.
        this.forceReconnect(this.ws)
      }
    }, 15000)
  }

  private stopPing() {
    if (this.pingInterval !== null) {
      window.clearInterval(this.pingInterval)
      this.pingInterval = null
    }
    if (this.heartbeatWatchdogInterval !== null) {
      window.clearInterval(this.heartbeatWatchdogInterval)
      this.heartbeatWatchdogInterval = null
    }
    this.lastPongAt = 0
  }

  private recordPong() {
    this.lastPongAt = Date.now()
  }

  private refreshStaleData() {
    // Refresh contacts list
    const contactsStore = useContactsStore()
    contactsStore.fetchContacts()
    void contactsStore.refreshCurrentMessages()

    // Refresh transfers
    const transfersStore = useTransfersStore()
    transfersStore.fetchTransfers()
    const payload = { reason: 'reconnected' }
    this.emitChannelSync(payload)
    this.emitInboxActivity(WS_TYPE_CHANNEL_SYNC, payload)

    // Show subtle notification
    toast.info('Connection restored', {
      description: 'Data has been refreshed',
      duration: 3000
    })
  }

  getIsConnected() {
    return this.isConnected
  }

  getConnectionState() {
    return this.connectionState
  }
}

// Export singleton instance
export const wsService = new WebSocketService()
