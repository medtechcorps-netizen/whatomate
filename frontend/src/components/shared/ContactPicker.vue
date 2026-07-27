<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Check, Loader2, Search, X } from 'lucide-vue-next'
import { contactsService } from '@/services/api'
import type { Contact } from '@/stores/contacts'
import { getErrorMessage, unwrapListResponse } from '@/lib/api-utils'
import { useAppToast } from '@/composables/useAppToast'

const props = withDefaults(
  defineProps<{
    modelValue?: string
    placeholder?: string
    disabled?: boolean
  }>(),
  {
    modelValue: '',
    placeholder: 'Search customer by name or phone',
    disabled: false,
  },
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
  selected: [contact: Contact]
}>()

const toast = useAppToast()
const query = ref('')
const contacts = ref<Contact[]>([])
const loading = ref(false)
const open = ref(false)
const selectedContact = ref<Contact | null>(null)
let searchTimer: ReturnType<typeof setTimeout> | undefined
let requestSequence = 0

const selectedLabel = computed(() => {
  if (!selectedContact.value) return ''
  return (
    selectedContact.value.profile_name ||
    selectedContact.value.name ||
    selectedContact.value.phone_number
  )
})

function contactLabel(contact: Contact) {
  return contact.profile_name || contact.name || contact.phone_number || 'Unnamed customer'
}

async function loadContacts(search = '') {
  const sequence = ++requestSequence
  loading.value = true
  try {
    const response = await contactsService.list({
      search: search.trim() || undefined,
      page: 1,
      limit: 20,
    })
    if (sequence !== requestSequence) return
    contacts.value = unwrapListResponse<Contact>(response, 'contacts')
  } catch (error) {
    if (sequence === requestSequence) {
      toast.error('Customers could not be searched', getErrorMessage(error))
    }
  } finally {
    if (sequence === requestSequence) loading.value = false
  }
}

async function hydrateSelectedContact(id: string) {
  if (!id || selectedContact.value?.id === id) return
  try {
    const response = await contactsService.get(id)
    const contact = (response.data?.data ?? response.data) as Contact
    if (contact?.id === id) {
      selectedContact.value = contact
      query.value = contactLabel(contact)
    }
  } catch {
    // A stale URL or deleted contact stays unresolved; the next search can
    // replace it without exposing a backend error in this input.
  }
}

function selectContact(contact: Contact) {
  selectedContact.value = contact
  query.value = contactLabel(contact)
  open.value = false
  emit('update:modelValue', contact.id)
  emit('selected', contact)
}

function clearSelection() {
  selectedContact.value = null
  query.value = ''
  contacts.value = []
  emit('update:modelValue', '')
  open.value = true
  void loadContacts()
}

function handleFocus() {
  if (props.disabled) return
  open.value = true
  if (!contacts.value.length) void loadContacts(selectedContact.value ? '' : query.value)
}

watch(query, (value) => {
  if (value === selectedLabel.value) return
  if (selectedContact.value) {
    selectedContact.value = null
    emit('update:modelValue', '')
  }
  open.value = true
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => void loadContacts(value), 250)
})

watch(
  () => props.modelValue,
  (value) => {
    if (!value) {
      if (selectedContact.value) {
        selectedContact.value = null
        query.value = ''
      }
      return
    }
    void hydrateSelectedContact(value)
  },
)

onMounted(() => {
  if (props.modelValue) void hydrateSelectedContact(props.modelValue)
  else void loadContacts()
})

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
})
</script>

<template>
  <div class="relative">
    <div class="relative">
      <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-white/30 light:text-gray-400" />
      <input
        v-model="query"
        type="search"
        autocomplete="off"
        :disabled="disabled"
        :placeholder="placeholder"
        class="h-11 w-full rounded-xl border border-white/10 bg-black/20 pl-9 pr-10 text-sm text-white outline-none placeholder:text-white/25 focus:border-emerald-300/45 disabled:cursor-not-allowed disabled:opacity-50 light:border-gray-200 light:bg-white light:text-gray-900 light:placeholder:text-gray-400"
        @focus="handleFocus"
        @keydown.escape="open = false"
      />
      <Loader2
        v-if="loading"
        class="pointer-events-none absolute right-3 top-1/2 h-4 w-4 -translate-y-1/2 animate-spin text-emerald-300"
      />
      <button
        v-else-if="modelValue"
        type="button"
        class="absolute right-2 top-1/2 flex h-7 w-7 -translate-y-1/2 items-center justify-center rounded-md text-white/35 hover:bg-white/[0.06] hover:text-white light:text-gray-400 light:hover:bg-gray-100 light:hover:text-gray-700"
        aria-label="Clear selected customer"
        @click="clearSelection"
      >
        <X class="h-3.5 w-3.5" />
      </button>
    </div>

    <div
      v-if="open && !disabled"
      class="absolute z-50 mt-2 max-h-72 w-full overflow-y-auto rounded-xl border border-white/10 bg-[#111416] p-1.5 shadow-2xl shadow-black/45 light:border-gray-200 light:bg-white"
    >
      <button
        v-for="contact in contacts"
        :key="contact.id"
        type="button"
        class="flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition hover:bg-white/[0.06] light:hover:bg-gray-50"
        @mousedown.prevent="selectContact(contact)"
      >
        <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-emerald-300/10 text-[11px] font-semibold uppercase text-emerald-200 light:bg-emerald-100 light:text-emerald-700">
          {{ contactLabel(contact).slice(0, 2) }}
        </div>
        <div class="min-w-0 flex-1">
          <p class="truncate text-sm font-medium text-white light:text-gray-900">
            {{ contactLabel(contact) }}
          </p>
          <p class="mt-0.5 truncate text-[11px] text-white/35 light:text-gray-500">
            {{ contact.phone_number }}
          </p>
        </div>
        <Check v-if="contact.id === modelValue" class="h-4 w-4 shrink-0 text-emerald-300" />
      </button>
      <p
        v-if="!loading && contacts.length === 0"
        class="px-3 py-8 text-center text-xs text-white/35 light:text-gray-500"
      >
        No customer matched this search.
      </p>
    </div>
  </div>
</template>
