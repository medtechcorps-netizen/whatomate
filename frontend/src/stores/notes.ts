import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import { notesService, type ConversationNote } from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'

export const useNotesStore = defineStore('notes', () => {
  const authStore = useAuthStore()
  const organizationsStore = useOrganizationsStore()
  const notes = ref<ConversationNote[]>([])
  const isLoading = ref(false)
  const isLoadingOlder = ref(false)
  const hasMore = ref(false)
  const currentContactId = ref<string | null>(null)
  let notesLoadGeneration = 0
  const activeOrganizationScope = computed(
    () => organizationsStore.selectedOrgId || authStore.organizationId || '',
  )

  // Helper: append a note only if it doesn't already exist
  function pushIfNew(note: ConversationNote) {
    if (!notes.value.some(n => n.id === note.id)) {
      notes.value.push(note)
    }
  }

  async function fetchNotes(contactId: string) {
    const loadGeneration = ++notesLoadGeneration
    isLoading.value = true
    currentContactId.value = contactId
    notes.value = []
    hasMore.value = false
    try {
      const response = await notesService.list(contactId, { limit: 30 })
      if (
        loadGeneration !== notesLoadGeneration ||
        currentContactId.value !== contactId
      ) return
      const data = (response.data as any).data || response.data
      notes.value = data.notes || []
      hasMore.value = data.has_more ?? false
    } catch {
      if (
        loadGeneration === notesLoadGeneration &&
        currentContactId.value === contactId
      ) {
        notes.value = []
        hasMore.value = false
      }
    } finally {
      if (loadGeneration === notesLoadGeneration) isLoading.value = false
    }
  }

  async function fetchOlderNotes(contactId: string) {
    if (isLoadingOlder.value || !hasMore.value || notes.value.length === 0) return
    const loadGeneration = notesLoadGeneration
    isLoadingOlder.value = true
    try {
      const oldestNote = notes.value[0]
      const response = await notesService.list(contactId, { limit: 30, before: oldestNote.id })
      if (
        loadGeneration !== notesLoadGeneration ||
        currentContactId.value !== contactId
      ) return
      const data = (response.data as any).data || response.data
      const olderNotes: ConversationNote[] = data.notes || []
      if (olderNotes.length > 0) {
        notes.value = [...olderNotes, ...notes.value]
      }
      hasMore.value = data.has_more ?? false
    } catch {
      // ignore
    } finally {
      if (loadGeneration === notesLoadGeneration) isLoadingOlder.value = false
    }
  }

  async function createNote(contactId: string, content: string) {
    const mutationGeneration = notesLoadGeneration
    const response = await notesService.create(contactId, { content })
    const note: ConversationNote = (response.data as any).data || response.data
    if (
      mutationGeneration === notesLoadGeneration &&
      currentContactId.value === contactId
    ) {
      pushIfNew(note)
    }
    return note
  }

  async function updateNote(contactId: string, noteId: string, content: string) {
    const mutationGeneration = notesLoadGeneration
    const response = await notesService.update(contactId, noteId, { content })
    const updated: ConversationNote = (response.data as any).data || response.data
    if (
      mutationGeneration === notesLoadGeneration &&
      currentContactId.value === contactId
    ) {
      const index = notes.value.findIndex(n => n.id === noteId)
      if (index !== -1) {
        notes.value[index] = updated
      }
    }
    return updated
  }

  async function deleteNote(contactId: string, noteId: string) {
    const mutationGeneration = notesLoadGeneration
    await notesService.delete(contactId, noteId)
    if (
      mutationGeneration === notesLoadGeneration &&
      currentContactId.value === contactId
    ) {
      notes.value = notes.value.filter(n => n.id !== noteId)
    }
  }

  // WebSocket event handlers
  function addNote(note: ConversationNote) {
    if (currentContactId.value !== note.contact_id) return
    pushIfNew(note)
  }

  function onNoteUpdated(note: ConversationNote) {
    if (currentContactId.value !== note.contact_id) return
    const index = notes.value.findIndex(n => n.id === note.id)
    if (index !== -1) {
      notes.value[index] = note
    }
  }

  function onNoteDeleted(noteId: string) {
    notes.value = notes.value.filter(n => n.id !== noteId)
  }

  function clearNotes() {
    notesLoadGeneration++
    notes.value = []
    isLoading.value = false
    isLoadingOlder.value = false
    hasMore.value = false
    currentContactId.value = null
  }

  watch(activeOrganizationScope, (organizationScope, previousScope) => {
    if (organizationScope !== previousScope) clearNotes()
  })

  return {
    notes,
    isLoading,
    isLoadingOlder,
    hasMore,
    currentContactId,
    fetchNotes,
    fetchOlderNotes,
    createNote,
    updateNote,
    deleteNote,
    addNote,
    onNoteUpdated,
    onNoteDeleted,
    clearNotes
  }
})
