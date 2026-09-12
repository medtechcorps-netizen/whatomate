<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  CalendarDays,
  ChevronLeft,
  ChevronRight,
  Clock3,
  Loader2,
  MapPin,
  Plus,
  UsersRound,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import ContactPicker from '@/components/shared/ContactPicker.vue'
import ResourceAvailabilityPanel from '@/components/booking/ResourceAvailabilityPanel.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'
import {
  bookingService,
  type Booking,
  type BookingEvent,
  type BookingResource,
  type BookingService,
} from '@/services/productSuite'

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const saving = ref(false)
const cursor = ref(startOfWeek(new Date()))
const services = ref<BookingService[]>([])
const resources = ref<BookingResource[]>([])
const events = ref<BookingEvent[]>([])
const selectedEvent = ref<BookingEvent | null>(null)
const bookings = ref<Booking[]>([])
const bookingsLoading = ref(false)
const showCreate = ref(false)
const showSetup = ref(false)
const setupError = ref('')
let loadGeneration = 0
let mounted = true
let draftOrganization = ''
let catalogueOrganization = ''
type CatalogueRecord = BookingService | BookingResource
type LifecycleTarget = {
  kind: 'service' | 'resource'
  action: 'deactivate' | 'reactivate' | 'delete'
  record: CatalogueRecord
  organization: string
}
const lifecycleTarget = ref<LifecycleTarget | null>(null)
const lifecycleOpen = ref(false)
const lifecycleError = ref('')
let lifecycleReturnFocus: HTMLElement | null = null
const newEvent = reactive({
  service_id: '',
  resource_id: '',
  date: '',
  start_time: '09:00',
  capacity: 1,
  location: '',
})
const bookingDraft = reactive({
  contact_id: '',
  quantity: 1,
  notes: '',
  allow_waitlist: true,
  idempotency_key: crypto.randomUUID(),
})
const resourceDraft = reactive({
  id: '',
  version: 0,
  user_id: undefined as string | undefined,
  is_active: true,
  metadata: {} as Record<string, unknown>,
  name: '',
  kind: 'practitioner' as BookingResource['kind'],
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Kuala_Lumpur',
  location: '',
})
const serviceDraft = reactive({
  id: '',
  name: '',
  description: '',
  kind: 'appointment' as BookingService['kind'],
  duration_minutes: 60,
  buffer_before_minutes: 0,
  buffer_after_minutes: 0,
  default_capacity: 1,
  price: '0',
  currency: 'MYR',
  resource_ids: [] as string[],
  version: 0,
  metadata: {} as Record<string, unknown>,
  reminder_policy: {} as Record<string, unknown>,
  is_active: true,
})
const canWriteBookings = computed(() => authStore.hasPermission('bookings', 'write'))
const canWriteBookingSettings = computed(() => authStore.hasPermission('booking.settings', 'write'))
const canDeleteBookingSettings = computed(() => authStore.hasPermission('booking.settings', 'delete'))
const canConfirmLifecycle = computed(() =>
  lifecycleTarget.value?.action === 'delete' ? canDeleteBookingSettings.value : canWriteBookingSettings.value,
)
const canReadContacts = computed(() => authStore.hasPermission('contacts', 'read'))
const canCreateAttendees = computed(() => canWriteBookings.value && canReadContacts.value)
const activeServices = computed(() => services.value.filter((service) => service.is_active))
const eligibleEventResources = computed(() => {
  const service = activeServices.value.find((item) => item.id === newEvent.service_id)
  const allowed = new Set(service?.resource_ids ?? [])
  return resources.value.filter(
    (resource) => resource.is_active && (allowed.size === 0 || allowed.has(resource.id)),
  )
})

const weekDays = computed(() =>
  Array.from({ length: 7 }, (_, index) => {
    const value = new Date(cursor.value)
    value.setDate(value.getDate() + index)
    return value
  }),
)

const weekLabel = computed(() => {
  const first = weekDays.value[0]
  const last = weekDays.value[6]
  return `${new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short' }).format(first)} – ${new Intl.DateTimeFormat(
    'en-MY',
    { day: 'numeric', month: 'short', year: 'numeric' },
  ).format(last)}`
})

const weekCapacity = computed(() => events.value.reduce((sum, event) => sum + event.capacity, 0))
const weekBooked = computed(() => events.value.reduce((sum, event) => sum + (event.booked_quantity ?? 0), 0))

function startOfWeek(value: Date) {
  const date = new Date(value)
  const day = date.getDay()
  const diff = day === 0 ? -6 : 1 - day
  date.setDate(date.getDate() + diff)
  date.setHours(0, 0, 0, 0)
  return date
}

function isoDate(value: Date) {
  const local = new Date(value.getTime() - value.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 10)
}

function eventsForDay(day: Date) {
  return events.value
    .filter((event) => eventDateKey(event) === isoDate(day))
    .sort((a, b) => new Date(a.starts_at).getTime() - new Date(b.starts_at).getTime())
}

function eventDateKey(event: BookingEvent) {
  const formatter = new Intl.DateTimeFormat('en-CA', {
    timeZone: event.resource?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  })
  const parts = Object.fromEntries(
    formatter
      .formatToParts(new Date(event.starts_at))
      .filter((part) => part.type !== 'literal')
      .map((part) => [part.type, part.value]),
  )
  return `${parts.year}-${parts.month}-${parts.day}`
}

function syncEventResource() {
  if (!eligibleEventResources.value.some((resource) => resource.id === newEvent.resource_id)) {
    newEvent.resource_id = eligibleEventResources.value[0]?.id ?? ''
  }
}

function isToday(day: Date) {
  return isoDate(day) === isoDate(new Date())
}

async function load() {
  const generation = ++loadGeneration
  const organization = currentOrganization()
  loading.value = true
  try {
    const from = weekDays.value[0].toISOString()
    const toDate = new Date(weekDays.value[6])
    toDate.setHours(23, 59, 59, 999)
    const [serviceResponse, resourceResponse, eventResponse] = await Promise.all([
      bookingService.allServices(),
      bookingService.allResources(),
      bookingService.allEvents({ from, to: toDate.toISOString() }),
    ])
    if (!isCurrent(organization) || generation !== loadGeneration) return
    catalogueOrganization = organization
    services.value = serviceResponse
    resources.value = resourceResponse
    events.value = eventResponse
    if (selectedEvent.value) {
      selectedEvent.value = events.value.find((event) => event.id === selectedEvent.value?.id) ?? null
    }

    if (!newEvent.service_id || !activeServices.value.some((service) => service.id === newEvent.service_id)) {
      newEvent.service_id = activeServices.value[0]?.id ?? ''
    }
    syncEventResource()
    if (!serviceDraft.id && !serviceDraft.resource_ids.length && resources.value[0]?.id) {
      serviceDraft.resource_ids = [resources.value[0].id]
    }
    if (!newEvent.date) newEvent.date = isoDate(new Date())
  } catch (error) {
    if (isCurrent(organization) && generation === loadGeneration) {
      toast.error('Calendar could not be loaded', getErrorMessage(error))
    }
  } finally {
    if (isCurrent(organization) && generation === loadGeneration) loading.value = false
  }
}

function currentOrganization() {
  return readSelectedOrganizationId() || authStore.organizationId || ''
}

function isCurrent(organization: string) {
  return mounted && organization === currentOrganization()
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value))
}

function resetResourceDraft() {
  Object.assign(resourceDraft, {
    id: '',
    version: 0,
    user_id: undefined,
    is_active: true,
    metadata: {},
    name: '',
    kind: 'practitioner',
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Kuala_Lumpur',
    location: '',
  })
}

function toggleSetup() {
  if (saving.value || !canWriteBookingSettings.value) return
  showSetup.value = !showSetup.value
  if (showSetup.value) {
    draftOrganization = currentOrganization()
    setupError.value = ''
    resetResourceDraft()
    resetServiceDraft()
  }
}

async function editResource(resource: BookingResource) {
  if (saving.value || !canWriteBookingSettings.value || !isCurrent(catalogueOrganization)) return
  showSetup.value = true
  draftOrganization = currentOrganization()
  setupError.value = ''
  Object.assign(resourceDraft, {
    ...clone(resource),
    user_id: resource.user_id,
    location: resource.location ?? '',
    metadata: clone(resource.metadata ?? {}),
  })
  await nextTick()
  document.getElementById('booking-resource-name')?.focus()
}

function validDraftContext() {
  if (!canWriteBookingSettings.value || !draftOrganization || !isCurrent(draftOrganization)) {
    setupError.value = 'Your organization or permission changed. Refresh before editing.'
    return false
  }
  return true
}

function putCatalogue(kind: 'service' | 'resource', item: CatalogueRecord) {
  // Invalidate an older read before applying an acknowledged mutation locally.
  ++loadGeneration
  if (kind === 'service') {
    services.value = [...services.value.filter((value) => value.id !== item.id), item as BookingService]
  } else {
    resources.value = [...resources.value.filter((value) => value.id !== item.id), item as BookingResource]
  }
  syncEventResource()
}

async function saveResource() {
  if (saving.value || !validDraftContext()) return
  if (!resourceDraft.name.trim() || !resourceDraft.timezone.trim()) {
    toast.warning('Resource name and timezone are required')
    return
  }
  const organization = draftOrganization
  const id = resourceDraft.id
  const payload: Partial<BookingResource> = {
    name: resourceDraft.name.trim(),
    kind: resourceDraft.kind,
    timezone: resourceDraft.timezone.trim(),
    location: resourceDraft.location.trim(),
    is_active: resourceDraft.is_active,
    user_id: resourceDraft.user_id,
    metadata: clone(resourceDraft.metadata),
    version: resourceDraft.version || undefined,
  }
  saving.value = true
  setupError.value = ''
  try {
    const response = id
      ? await bookingService.updateResource(id, payload, organization)
      : await bookingService.createResource(payload, organization)
    if (!isCurrent(organization)) return
    const resource = unwrapItemResponse<BookingResource>(response)
    putCatalogue('resource', resource)
    resetResourceDraft()
    if (!id && !serviceDraft.id && !serviceDraft.resource_ids.includes(resource.id)) {
      serviceDraft.resource_ids = [...serviceDraft.resource_ids, resource.id]
    }
    toast.success(id ? 'Booking resource saved' : 'Booking resource created')
    await load()
  } catch (error) {
    if (isCurrent(organization)) {
      setupError.value = getErrorMessage(error)
      toast.error('Resource was not saved', setupError.value)
    }
  } finally {
    saving.value = false
  }
}

function resetServiceDraft() {
  serviceDraft.id = ''
  serviceDraft.name = ''
  serviceDraft.description = ''
  serviceDraft.kind = 'appointment'
  serviceDraft.duration_minutes = 60
  serviceDraft.buffer_before_minutes = 0
  serviceDraft.buffer_after_minutes = 0
  serviceDraft.default_capacity = 1
  serviceDraft.price = '0'
  serviceDraft.currency = 'MYR'
  serviceDraft.resource_ids = resources.value[0]?.id ? [resources.value[0].id] : []
  serviceDraft.version = 0
  serviceDraft.metadata = {}
  serviceDraft.reminder_policy = {}
  serviceDraft.is_active = true
}

async function editService(service: BookingService) {
  if (saving.value || !canWriteBookingSettings.value || !isCurrent(catalogueOrganization)) return
  showSetup.value = true
  draftOrganization = currentOrganization()
  setupError.value = ''
  serviceDraft.id = service.id
  serviceDraft.name = service.name
  serviceDraft.description = service.description ?? ''
  serviceDraft.kind = service.kind
  serviceDraft.duration_minutes = service.duration_minutes
  serviceDraft.buffer_before_minutes = service.buffer_before_minutes ?? 0
  serviceDraft.buffer_after_minutes = service.buffer_after_minutes ?? 0
  serviceDraft.default_capacity = service.default_capacity
  serviceDraft.price = (service.price_minor / 100).toFixed(2)
  serviceDraft.currency = service.currency
  serviceDraft.resource_ids = [...(service.resource_ids ?? [])]
  serviceDraft.version = service.version
  serviceDraft.metadata = clone(service.metadata ?? {})
  serviceDraft.reminder_policy = clone(service.reminder_policy ?? {})
  serviceDraft.is_active = service.is_active
  await nextTick()
  document.getElementById('booking-service-name')?.focus()
}

async function saveService() {
  if (saving.value || !validDraftContext()) return
  const priceMinor = Math.round(Number(serviceDraft.price) * 100)
  if (
    !serviceDraft.name.trim() ||
    serviceDraft.duration_minutes < 1 ||
    serviceDraft.default_capacity < 1 ||
    !Number.isFinite(priceMinor) ||
    priceMinor < 0
  ) {
    toast.warning('Name, duration, capacity and a valid price are required')
    return
  }
  saving.value = true
  setupError.value = ''
  const organization = draftOrganization
  const id = serviceDraft.id
  const payload: Partial<BookingService> = {
    name: serviceDraft.name.trim(),
    description: serviceDraft.description.trim(),
    kind: serviceDraft.kind,
    duration_minutes: serviceDraft.duration_minutes,
    buffer_before_minutes: serviceDraft.buffer_before_minutes,
    buffer_after_minutes: serviceDraft.buffer_after_minutes,
    default_capacity: serviceDraft.default_capacity,
    price_minor: priceMinor,
    currency: serviceDraft.currency,
    reminder_policy: clone(serviceDraft.reminder_policy),
    metadata: clone(serviceDraft.metadata),
    resource_ids: [...serviceDraft.resource_ids],
    is_active: serviceDraft.is_active,
    version: serviceDraft.version || undefined,
  }
  try {
    const response = id
      ? await bookingService.updateService(id, payload, organization)
      : await bookingService.createService(payload, organization)
    if (!isCurrent(organization)) return
    putCatalogue('service', unwrapItemResponse<BookingService>(response))
    toast.success(id ? 'Booking service saved' : 'Booking service created')
    resetServiceDraft()
    await load()
  } catch (error) {
    if (isCurrent(organization)) {
      setupError.value = getErrorMessage(error)
      toast.error('Service was not saved', setupError.value)
    }
  } finally {
    saving.value = false
  }
}

function requestLifecycle(
  kind: 'service' | 'resource',
  record: CatalogueRecord,
  action: LifecycleTarget['action'],
  event: Event,
) {
  if (saving.value || !isCurrent(catalogueOrganization)) return
  if (
    action === 'delete' ? !canDeleteBookingSettings.value || record.is_active : !canWriteBookingSettings.value
  )
    return
  lifecycleReturnFocus = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  lifecycleTarget.value = {
    kind,
    record: clone(record),
    action,
    organization: currentOrganization(),
  }
  lifecycleError.value = ''
  lifecycleOpen.value = true
}

function setLifecycleOpen(open: boolean) {
  if (!saving.value) lifecycleOpen.value = open
}

function restoreLifecycleFocus(event: Event) {
  event.preventDefault()
  const target = lifecycleReturnFocus?.isConnected && !lifecycleReturnFocus.matches(':disabled')
    ? lifecycleReturnFocus
    : document.getElementById('booking-refresh')
  target?.focus()
  lifecycleReturnFocus = null
}

async function confirmLifecycle() {
  const target = lifecycleTarget.value
  if (saving.value || !target || !canConfirmLifecycle.value) return
  if (!target.organization || !isCurrent(target.organization)) {
    lifecycleError.value = 'Your organization changed. Close and refresh before making changes.'
    return
  }
  saving.value = true
  lifecycleError.value = ''
  try {
    if (target.action === 'delete') {
      if (target.record.is_active) return
      const remove = target.kind === 'service' ? bookingService.deleteService : bookingService.deleteResource
      await remove(target.record.id, target.record.version, target.organization)
      if (!isCurrent(target.organization)) return
      ++loadGeneration
      if (target.kind === 'service') {
        services.value = services.value.filter((item) => item.id !== target.record.id)
        if (serviceDraft.id === target.record.id) resetServiceDraft()
      } else {
        resources.value = resources.value.filter((item) => item.id !== target.record.id)
        if (resourceDraft.id === target.record.id) resetResourceDraft()
      }
    } else {
      const payload = {
        ...clone(target.record),
        is_active: target.action === 'reactivate',
      }
      const response =
        target.kind === 'service'
          ? await bookingService.updateService(
              target.record.id,
              payload as BookingService,
              target.organization,
            )
          : await bookingService.updateResource(
              target.record.id,
              payload as BookingResource,
              target.organization,
            )
      if (!isCurrent(target.organization)) return
      putCatalogue(target.kind, unwrapItemResponse<CatalogueRecord>(response))
      // Don't let an older edit draft silently reverse the acknowledged state.
      if (target.kind === 'service' && serviceDraft.id === target.record.id) resetServiceDraft()
      if (target.kind === 'resource' && resourceDraft.id === target.record.id) resetResourceDraft()
    }
    syncEventResource()
    lifecycleOpen.value = false
    toast.success(
      target.action === 'delete' ? 'Unused booking record deleted' : 'Booking availability updated',
    )
    await load()
  } catch (error) {
    if (isCurrent(target.organization)) {
      lifecycleError.value = getErrorMessage(error)
      toast.error('Booking record was not changed', lifecycleError.value)
    }
  } finally {
    saving.value = false
  }
}

watch(
  () => authStore.organizationId,
  () => {
    ++loadGeneration
    services.value = []
    resources.value = []
    events.value = []
    bookings.value = []
    selectedEvent.value = null
    showSetup.value = false
    showCreate.value = false
    lifecycleOpen.value = false
    resetResourceDraft()
    resetServiceDraft()
    void load()
  },
)
onBeforeUnmount(() => {
  mounted = false
  ++loadGeneration
})

async function selectEvent(event: BookingEvent) {
  selectedEvent.value = event
  await loadBookings()
}

function closeSelectedEvent() {
  selectedEvent.value = null
  bookings.value = []
}

async function loadBookings() {
  if (!selectedEvent.value) {
    bookings.value = []
    return
  }
  bookingsLoading.value = true
  try {
    bookings.value = await bookingService.allBookings({
      event_id: selectedEvent.value.id,
    })
  } catch (error) {
    toast.error('Attendees could not be loaded', getErrorMessage(error))
  } finally {
    bookingsLoading.value = false
  }
}

async function createAttendeeBooking() {
  if (!selectedEvent.value || !bookingDraft.contact_id || bookingDraft.quantity < 1) {
    toast.warning('Choose a customer and a valid quantity')
    return
  }
  saving.value = true
  try {
    await bookingService.createBooking(selectedEvent.value.id, {
      contact_id: bookingDraft.contact_id,
      quantity: bookingDraft.quantity,
      status: 'reserved',
      source: 'agent',
      notes: bookingDraft.notes.trim(),
      allow_waitlist: bookingDraft.allow_waitlist,
      idempotency_key: bookingDraft.idempotency_key,
    })
    bookingDraft.contact_id = ''
    bookingDraft.quantity = 1
    bookingDraft.notes = ''
    bookingDraft.idempotency_key = crypto.randomUUID()
    toast.success('Customer added to the schedule')
    await Promise.all([loadBookings(), load()])
  } catch (error) {
    toast.error('Booking was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

function bookingTransitions(status: Booking['status']) {
  const transitions: Partial<Record<Booking['status'], Array<{ key: string; label: string }>>> = {
    reserved: [
      { key: 'confirm', label: 'Confirm' },
      { key: 'cancel', label: 'Cancel' },
    ],
    confirmed: [
      { key: 'check-in', label: 'Check in' },
      { key: 'no-show', label: 'No show' },
      { key: 'cancel', label: 'Cancel' },
    ],
    waitlisted: [
      { key: 'reserve', label: 'Reserve' },
      { key: 'confirm', label: 'Confirm' },
      { key: 'cancel', label: 'Cancel' },
    ],
    checked_in: [
      { key: 'complete', label: 'Complete' },
      { key: 'no-show', label: 'No show' },
    ],
  }
  return transitions[status] ?? []
}

async function transitionAttendee(booking: Booking, transition: string) {
  try {
    await bookingService.transitionBooking(booking.id, transition, {
      version: booking.version,
    })
    toast.success('Booking status updated')
    await Promise.all([loadBookings(), load()])
  } catch (error) {
    toast.error('Booking status was not updated', getErrorMessage(error))
  }
}

function customerLabel(booking: Booking) {
  return booking.contact?.profile_name || booking.contact?.phone_number || 'Customer'
}

async function createEvent() {
  const service = services.value.find((item) => item.id === newEvent.service_id)
  const resource = eligibleEventResources.value.find((item) => item.id === newEvent.resource_id)
  if (!service || !resource || !newEvent.date || !newEvent.start_time) {
    toast.warning('Service, resource, date and start time are required')
    return
  }
  const localStartsAt = `${newEvent.date}T${newEvent.start_time}`
  const wallClock = new Date(`${localStartsAt}:00Z`)
  const localEndsAt = new Date(wallClock.getTime() + service.duration_minutes * 60_000)
    .toISOString()
    .slice(0, 16)

  saving.value = true
  try {
    await bookingService.createEvent({
      service_id: service.id,
      resource_id: resource.id,
      local_starts_at: localStartsAt,
      local_ends_at: localEndsAt,
      timezone: resource.timezone,
      capacity: Number(newEvent.capacity) || service.default_capacity,
      status: 'scheduled',
      location: newEvent.location,
    })
    showCreate.value = false
    toast.success('Schedule added')
    await load()
  } catch (error) {
    toast.error('Schedule was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function shiftWeek(amount: number) {
  const next = new Date(cursor.value)
  next.setDate(next.getDate() + amount * 7)
  cursor.value = next
  await load()
}

async function goToday() {
  cursor.value = startOfWeek(new Date())
  await load()
}

function timeLabel(value: string, timezone?: string) {
  return new Intl.DateTimeFormat('en-MY', {
    hour: '2-digit',
    minute: '2-digit',
    timeZone: timezone,
  }).format(new Date(value))
}

function eventDateTimeLabel(event: BookingEvent) {
  return new Intl.DateTimeFormat('en-MY', {
    dateStyle: 'medium',
    timeStyle: 'short',
    timeZone: event.resource?.timezone,
  }).format(new Date(event.starts_at))
}

function serviceTone(event: BookingEvent) {
  const index = services.value.findIndex((item) => item.id === event.service_id)
  return [
    'border-cyan-300/20 bg-cyan-300/[0.08] text-cyan-100',
    'border-violet-300/20 bg-violet-300/[0.08] text-violet-100',
    'border-amber-300/20 bg-amber-300/[0.08] text-amber-100',
    'border-emerald-300/20 bg-emerald-300/[0.08] text-emerald-100',
  ][Math.max(0, index) % 4]
}

onMounted(load)
</script>

<template>
  <div class="flex h-full min-w-0 flex-col overflow-y-auto bg-[#08090a] light:bg-[#f6f4ef]">
    <PageHeader
      title="Bookings & classes"
      description="Appointments, resources and capacity across the week."
      :icon="CalendarDays"
      icon-gradient="bg-gradient-to-br from-fuchsia-500 to-violet-700 shadow-fuchsia-500/20"
    >
      <template #actions>
        <Button
          id="booking-refresh"
          class="min-h-[44px]"
          variant="outline"
          :aria-disabled="saving || loading"
          @click="!saving && !loading && load()"
        >
          Refresh calendar
        </Button>
        <Button
          v-if="canWriteBookingSettings"
          class="min-h-[44px]"
          variant="outline"
          :disabled="saving"
          @click="toggleSetup"
        >
          Service setup
        </Button>
        <Button
          v-if="canWriteBookings"
          class="bg-fuchsia-400 text-black hover:bg-fuchsia-300"
          :disabled="saving || !activeServices.length || !eligibleEventResources.length"
          @click="showCreate = !showCreate"
        >
          <Plus class="mr-2 h-4 w-4" />
          Add schedule
        </Button>
      </template>
    </PageHeader>

    <section
      v-if="showSetup && canWriteBookingSettings"
      class="grid shrink-0 gap-4 border-b border-cyan-300/15 bg-cyan-300/[0.035] px-5 py-4 xl:grid-cols-2"
    >
      <p
        v-if="setupError"
        role="alert"
        class="rounded-md border border-destructive/30 p-3 text-sm text-destructive xl:col-span-2"
      >
        {{ setupError }} Your draft is preserved. For a version conflict, cancel the edit, refresh, and reopen
        the record.
      </p>
      <form
        aria-label="Resource details"
        class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 md:grid-cols-2 light:border-gray-200 light:bg-white"
        @submit.prevent="saveResource"
      >
        <fieldset :disabled="saving" class="contents">
          <div class="flex items-start justify-between gap-3 md:col-span-2">
            <div>
              <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-cyan-300">
                1 · Schedulable resource
              </p>
              <p class="mt-1 text-xs text-white/60 light:text-gray-600">
                {{
                  resourceDraft.id
                    ? 'Edit details without changing active status.'
                    : 'Add a practitioner, instructor, room, or equipment.'
                }}
              </p>
            </div>
            <Button
              v-if="resourceDraft.id"
              type="button"
              class="min-h-[44px]"
              variant="outline"
              :disabled="saving"
              @click="resetResourceDraft"
            >
              Cancel resource edit
            </Button>
          </div>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Resource name
            <Input
              id="booking-resource-name"
              v-model="resourceDraft.name"
              required
              maxlength="255"
              placeholder="Resource name"
            />
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Resource type
            <select
              v-model="resourceDraft.kind"
              class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
            >
              <option value="practitioner">Practitioner</option>
              <option value="instructor">Instructor</option>
              <option value="room">Room</option>
              <option value="equipment">Equipment</option>
            </select>
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Timezone
            <Input
              v-model="resourceDraft.timezone"
              required
              maxlength="100"
              placeholder="Timezone, e.g. Asia/Kuala_Lumpur"
            />
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Resource location
            <Input v-model="resourceDraft.location" maxlength="255" placeholder="Location (optional)" />
          </label>
          <Button type="submit" class="min-h-[44px] md:col-span-2" variant="outline" :disabled="saving">
            <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
            {{ resourceDraft.id ? 'Save resource' : 'Add resource' }}
          </Button>
        </fieldset>
      </form>

      <form
        aria-label="Service details"
        class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 md:grid-cols-2 light:border-gray-200 light:bg-white"
        @submit.prevent="saveService"
      >
        <fieldset :disabled="saving" class="contents">
          <div class="flex items-start justify-between gap-3 md:col-span-2">
            <div>
              <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-cyan-300">
                2 · Service offering
              </p>
              <p class="mt-1 text-xs text-white/40 light:text-gray-500">
                {{
                  serviceDraft.id
                    ? 'Edit details without changing active status.'
                    : 'Tie an active service to a resource.'
                }}
              </p>
            </div>
            <Button
              v-if="serviceDraft.id"
              type="button"
              class="min-h-[44px]"
              variant="outline"
              :disabled="saving"
              @click="resetServiceDraft"
              >Cancel service edit</Button
            >
          </div>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Service name
            <Input
              id="booking-service-name"
              v-model="serviceDraft.name"
              required
              maxlength="255"
              placeholder="Service name"
            />
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Service type
            <select
              v-model="serviceDraft.kind"
              class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
            >
              <option value="appointment">Appointment</option>
              <option value="class">Class</option>
            </select>
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Allowed resources (none means all active resources)
            <select
              v-model="serviceDraft.resource_ids"
              multiple
              class="min-h-24 rounded-md border border-white/10 bg-[#0d0f10] px-3 py-2 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
            >
              <option v-for="resource in resources" :key="resource.id" :value="resource.id">
                {{ resource.name }}{{ resource.is_active ? '' : ' (inactive)' }}
              </option>
            </select>
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Duration in minutes
            <Input
              v-model.number="serviceDraft.duration_minutes"
              required
              type="number"
              min="1"
              max="10080"
              placeholder="Minutes"
            />
          </label>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
            >Capacity
            <Input
              v-model.number="serviceDraft.default_capacity"
              required
              type="number"
              min="1"
              max="100000"
              placeholder="Capacity"
            />
          </label>
          <div class="grid grid-cols-[1fr_90px] gap-2">
            <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
              >Price
              <Input
                v-model="serviceDraft.price"
                required
                type="number"
                min="0"
                step="0.01"
                placeholder="Price"
              />
            </label>
            <label class="grid gap-1 text-xs text-white/70 light:text-gray-700"
              >Currency
              <select
                v-model="serviceDraft.currency"
                class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-2 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
              >
                <option value="MYR">MYR</option>
                <option value="SGD">SGD</option>
                <option value="USD">USD</option>
              </select>
            </label>
          </div>
          <label class="grid gap-1 text-xs text-white/70 light:text-gray-700 md:col-span-2"
            >Description
            <Input
              v-model="serviceDraft.description"
              maxlength="10000"
              placeholder="Description (optional)"
              class="md:col-span-2"
            />
          </label>
          <Button
            type="submit"
            class="min-h-[44px] bg-cyan-400 text-black hover:bg-cyan-300 md:col-span-2"
            :disabled="saving"
          >
            <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
            {{ serviceDraft.id ? 'Save service' : 'Create service' }}
          </Button>
        </fieldset>
      </form>

      <ResourceAvailabilityPanel :resources="resources" />
    </section>

    <div
      v-if="showCreate"
      class="grid gap-3 border-b border-fuchsia-400/20 bg-fuchsia-400/[0.04] px-5 py-4 md:grid-cols-2 xl:grid-cols-[1fr_1fr_.7fr_.55fr_.55fr_1fr_auto]"
    >
      <select
        v-model="newEvent.service_id"
        class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
        @change="syncEventResource"
      >
        <option value="" disabled>Service</option>
        <option v-for="service in activeServices" :key="service.id" :value="service.id">
          {{ service.name }}
        </option>
      </select>
      <select
        v-model="newEvent.resource_id"
        class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
      >
        <option value="" disabled>Resource</option>
        <option v-for="resource in eligibleEventResources" :key="resource.id" :value="resource.id">
          {{ resource.name }} · {{ resource.timezone }}
        </option>
      </select>
      <Input v-model="newEvent.date" type="date" />
      <Input v-model="newEvent.start_time" type="time" />
      <Input v-model="newEvent.capacity" type="number" min="1" />
      <Input v-model="newEvent.location" placeholder="Location" />
      <Button :disabled="saving" class="bg-fuchsia-400 text-black hover:bg-fuchsia-300" @click="createEvent">
        <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
        Save
      </Button>
    </div>

    <section v-if="selectedEvent" class="border-b border-violet-300/15 bg-violet-300/[0.035] px-5 py-4">
      <div class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-violet-300">Attendee desk</p>
          <h2 class="mt-1 text-base font-semibold text-white light:text-gray-900">
            {{ selectedEvent.service?.name || 'Scheduled service' }}
          </h2>
          <p class="mt-1 text-xs text-white/40 light:text-gray-500">
            {{ eventDateTimeLabel(selectedEvent) }}
            · {{ selectedEvent.booked_quantity ?? 0 }}/{{ selectedEvent.capacity }}
            seats
          </p>
        </div>
        <Button variant="outline" size="sm" @click="closeSelectedEvent">Close</Button>
      </div>

      <div class="mt-4 grid gap-4 xl:grid-cols-[390px_1fr]">
        <form
          v-if="canCreateAttendees"
          class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 light:border-gray-200 light:bg-white"
          @submit.prevent="createAttendeeBooking"
        >
          <ContactPicker v-model="bookingDraft.contact_id" placeholder="Search customer to book" />
          <div class="grid grid-cols-[100px_1fr] gap-3">
            <Input
              v-model.number="bookingDraft.quantity"
              type="number"
              min="1"
              :max="selectedEvent.capacity"
            />
            <Input
              v-model="bookingDraft.notes"
              maxlength="2000"
              placeholder="Internal booking note (optional)"
            />
          </div>
          <label class="flex items-start gap-2 text-xs leading-5 text-white/50 light:text-gray-600">
            <input v-model="bookingDraft.allow_waitlist" type="checkbox" class="mt-1 accent-violet-400" />
            Add to the waitlist if this schedule reaches capacity.
          </label>
          <Button
            type="submit"
            class="bg-violet-400 text-black hover:bg-violet-300"
            :disabled="saving || !bookingDraft.contact_id"
          >
            <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
            Add attendee
          </Button>
        </form>
        <div
          v-else-if="canWriteBookings && !canReadContacts"
          class="rounded-2xl border border-amber-300/15 bg-amber-300/[0.04] p-4 text-xs leading-5 text-amber-100/70"
        >
          You can manage booking statuses, but adding an attendee requires contact read access.
        </div>

        <div
          class="min-w-0 rounded-2xl border border-white/[0.08] bg-black/15 p-3 light:border-gray-200 light:bg-white"
        >
          <div v-if="bookingsLoading" class="flex min-h-24 items-center justify-center">
            <Loader2 class="h-5 w-5 animate-spin text-violet-300" />
          </div>
          <div v-else class="grid gap-2 md:grid-cols-2 2xl:grid-cols-3">
            <article
              v-for="booking in bookings"
              :key="booking.id"
              class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-white light:text-gray-900">
                    {{ customerLabel(booking) }}
                  </p>
                  <p class="mt-1 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">
                    {{ booking.quantity }} seat{{ booking.quantity === 1 ? '' : 's' }}
                  </p>
                </div>
                <Badge variant="outline" class="shrink-0 capitalize">{{
                  booking.status.replace('_', ' ')
                }}</Badge>
              </div>
              <div
                v-if="canWriteBookings && bookingTransitions(booking.status).length"
                class="mt-3 flex flex-wrap gap-1.5"
              >
                <Button
                  v-for="transition in bookingTransitions(booking.status)"
                  :key="transition.key"
                  type="button"
                  variant="outline"
                  size="sm"
                  class="h-7 px-2 text-[10px]"
                  @click="transitionAttendee(booking, transition.key)"
                >
                  {{ transition.label }}
                </Button>
              </div>
            </article>
            <p
              v-if="!bookings.length"
              class="col-span-full py-8 text-center text-xs text-white/35 light:text-gray-500"
            >
              No attendees have been booked for this schedule.
            </p>
          </div>
        </div>
      </div>
    </section>

    <div
      class="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b border-white/[0.08] px-5 py-3 light:border-gray-200"
    >
      <div class="flex flex-wrap items-center gap-2">
        <Button aria-label="Previous week" variant="outline" size="icon" @click="shiftWeek(-1)"
          ><ChevronLeft class="h-4 w-4"
        /></Button>
        <Button variant="outline" size="sm" @click="goToday">Today</Button>
        <Button aria-label="Next week" variant="outline" size="icon" @click="shiftWeek(1)"
          ><ChevronRight class="h-4 w-4"
        /></Button>
        <p class="ml-2 text-sm font-semibold text-white light:text-gray-900">
          {{ weekLabel }}
        </p>
      </div>
      <div class="hidden items-center gap-5 text-xs text-white/45 light:text-gray-500 md:flex">
        <span>{{ events.length }} schedules</span>
        <span>{{ weekBooked }}/{{ weekCapacity }} seats booked</span>
      </div>
    </div>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-fuchsia-300" />
    </div>

    <div v-else class="grid min-w-0 flex-1 xl:grid-cols-[minmax(0,1fr)_300px]">
      <div class="order-2 min-w-0 overflow-auto p-4 md:p-5 xl:order-1">
        <div class="grid min-w-[980px] grid-cols-7 gap-2">
          <section
            v-for="day in weekDays"
            :key="day.toISOString()"
            class="min-h-[620px] overflow-hidden rounded-2xl border bg-white/[0.018] light:bg-white"
            :class="isToday(day) ? 'border-fuchsia-300/35' : 'border-white/[0.08] light:border-gray-200'"
          >
            <header
              class="border-b px-3 py-3 text-center"
              :class="
                isToday(day)
                  ? 'border-fuchsia-300/20 bg-fuchsia-300/[0.07]'
                  : 'border-white/[0.07] light:border-gray-100'
              "
            >
              <p
                class="text-[10px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-gray-500"
              >
                {{ new Intl.DateTimeFormat('en-MY', { weekday: 'short' }).format(day) }}
              </p>
              <p
                class="mt-1 text-lg font-semibold"
                :class="isToday(day) ? 'text-fuchsia-200' : 'text-white light:text-gray-900'"
              >
                {{ day.getDate() }}
              </p>
            </header>

            <div class="space-y-2 p-2">
              <article
                v-for="event in eventsForDay(day)"
                :key="event.id"
                data-testid="calendar-event-card"
                role="button"
                tabindex="0"
                class="cursor-pointer rounded-xl border p-3 transition hover:-translate-y-0.5 light:text-gray-900"
                :class="serviceTone(event)"
                @click="selectEvent(event)"
                @keydown.enter="selectEvent(event)"
              >
                <div class="flex items-center justify-between gap-2">
                  <span
                    data-testid="calendar-event-time"
                    class="text-[10px] font-semibold uppercase tracking-wider opacity-70 light:opacity-100"
                  >
                    {{ timeLabel(event.starts_at, event.resource?.timezone) }}
                  </span>
                  <Badge variant="outline" class="h-5 border-current/20 px-1.5 text-[9px]">
                    {{ event.booked_quantity ?? 0 }}/{{ event.capacity }}
                  </Badge>
                </div>
                <p class="mt-2 text-xs font-semibold leading-5">
                  {{ event.service?.name || 'Scheduled service' }}
                </p>
                <div
                  data-testid="calendar-event-meta"
                  class="mt-2 space-y-1 text-[10px] opacity-65 light:opacity-100"
                >
                  <p class="flex items-center gap-1.5">
                    <UsersRound class="h-3 w-3" />
                    {{ event.resource?.name || 'Resource' }}
                  </p>
                  <p v-if="event.location" class="flex items-center gap-1.5">
                    <MapPin class="h-3 w-3" />
                    {{ event.location }}
                  </p>
                </div>
              </article>
              <div
                v-if="eventsForDay(day).length === 0"
                class="flex min-h-28 items-center justify-center rounded-xl border border-dashed border-white/[0.07] text-[10px] uppercase tracking-wider text-white/20 light:border-gray-200 light:text-gray-400"
              >
                Open day
              </div>
            </div>
          </section>
        </div>
      </div>

      <aside
        aria-label="Services and resources"
        class="order-1 min-w-0 border-l border-white/[0.08] bg-[#0b0c0d] p-4 light:border-gray-200 light:bg-white xl:order-2"
      >
        <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">
          Service menu
        </p>
        <div class="mt-3 space-y-2">
          <article
            v-for="service in services"
            :key="service.id"
            :data-testid="'booking-service-' + service.id"
            class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
          >
            <div class="flex items-start justify-between gap-2">
              <div>
                <p class="text-xs font-medium text-white light:text-gray-900">
                  {{ service.name }}
                </p>
                <p class="mt-1 flex items-center gap-1 text-[10px] text-white/35 light:text-gray-500">
                  <Clock3 class="h-3 w-3" />
                  {{ service.duration_minutes }} min · {{ service.kind }}
                </p>
              </div>
              <div class="text-right">
                <span class="text-[11px] font-medium text-emerald-300">
                  {{
                    new Intl.NumberFormat('en-MY', {
                      style: 'currency',
                      currency: service.currency,
                    }).format(service.price_minor / 100)
                  }}
                </span>
                <Badge variant="outline" class="mt-2">{{ service.is_active ? 'Active' : 'Inactive' }}</Badge>
              </div>
            </div>
            <div class="mt-3 flex flex-wrap gap-2">
              <Button
                v-if="canWriteBookingSettings"
                type="button"
                variant="outline"
                class="min-h-[44px]"
                :disabled="saving"
                :aria-label="'Edit service ' + service.name"
                @click="editService(service)"
                >Edit</Button
              >
              <Button
                v-if="canWriteBookingSettings"
                type="button"
                variant="outline"
                class="min-h-[44px]"
                :disabled="saving"
                :aria-label="(service.is_active ? 'Deactivate' : 'Reactivate') + ' service ' + service.name"
                @click="
                  requestLifecycle(
                    'service',
                    service,
                    service.is_active ? 'deactivate' : 'reactivate',
                    $event,
                  )
                "
              >
                {{ service.is_active ? 'Deactivate' : 'Reactivate' }}
              </Button>
              <Button
                v-if="canDeleteBookingSettings && !service.is_active"
                type="button"
                variant="outline"
                class="min-h-[44px] text-destructive"
                :disabled="saving"
                :aria-label="'Delete service ' + service.name"
                @click="requestLifecycle('service', service, 'delete', $event)"
                >Delete</Button
              >
            </div>
          </article>
          <div
            v-if="services.length === 0"
            class="rounded-xl bg-white/[0.025] p-5 text-center text-xs text-white/35 light:bg-gray-50 light:text-gray-500"
          >
            Apply a vertical playbook or add your first service.
          </div>
        </div>

        <p
          class="mt-7 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500"
        >
          Resources
        </p>
        <div class="mt-3 space-y-2">
          <div
            v-for="resource in resources"
            :key="resource.id"
            :data-testid="'booking-resource-' + resource.id"
            class="rounded-xl border border-white/[0.07] px-3 py-2.5 light:border-gray-200"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-8 w-8 items-center justify-center rounded-full bg-violet-300/10 text-xs font-semibold text-violet-200"
              >
                {{ resource.name.slice(0, 2).toUpperCase() }}
              </div>
              <div class="min-w-0">
                <p class="truncate text-xs font-medium text-white light:text-gray-900">
                  {{ resource.name }}
                </p>
                <p class="mt-0.5 text-[10px] capitalize text-white/35 light:text-gray-500">
                  {{ resource.kind }} ·
                  {{ resource.is_active ? 'Active' : 'Inactive' }}
                </p>
              </div>
            </div>
            <div class="mt-3 flex flex-wrap gap-2">
              <Button
                v-if="canWriteBookingSettings"
                type="button"
                variant="outline"
                class="min-h-[44px]"
                :disabled="saving"
                :aria-label="'Edit resource ' + resource.name"
                @click="editResource(resource)"
                >Edit</Button
              >
              <Button
                v-if="canWriteBookingSettings"
                type="button"
                variant="outline"
                class="min-h-[44px]"
                :disabled="saving"
                :aria-label="
                  (resource.is_active ? 'Deactivate' : 'Reactivate') + ' resource ' + resource.name
                "
                @click="
                  requestLifecycle(
                    'resource',
                    resource,
                    resource.is_active ? 'deactivate' : 'reactivate',
                    $event,
                  )
                "
              >
                {{ resource.is_active ? 'Deactivate' : 'Reactivate' }}
              </Button>
              <Button
                v-if="canDeleteBookingSettings && !resource.is_active"
                type="button"
                variant="outline"
                class="min-h-[44px] text-destructive"
                :disabled="saving"
                :aria-label="'Delete resource ' + resource.name"
                @click="requestLifecycle('resource', resource, 'delete', $event)"
                >Delete</Button
              >
            </div>
          </div>
        </div>
      </aside>
    </div>
    <AlertDialog :open="lifecycleOpen" @update:open="setLifecycleOpen">
      <AlertDialogContent @close-auto-focus="restoreLifecycleFocus">
        <AlertDialogHeader>
          <AlertDialogTitle>
            {{
              lifecycleTarget?.action === 'delete'
                ? 'Delete unused record?'
                : lifecycleTarget?.action === 'deactivate'
                  ? 'Deactivate record?'
                  : 'Reactivate record?'
            }}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {{ lifecycleTarget?.record.name }}.
            <template v-if="lifecycleTarget?.action === 'delete'">
              Only inactive records with no booking, schedule or purchase history and no remaining
              dependencies can be deleted. Dependent records will not be removed. Used records must stay
              deactivated to preserve history.
            </template>
            <template v-else-if="lifecycleTarget?.action === 'deactivate'">
              This stops new use. Existing schedules, bookings and history remain intact.
            </template>
            <template v-else>
              This allows new use again. Review the saved details before continuing.
            </template>
          </AlertDialogDescription>
        </AlertDialogHeader>
        <p
          v-if="lifecycleError"
          role="alert"
          class="rounded-md border border-destructive/30 p-3 text-sm text-destructive"
        >
          {{ lifecycleError }} The change was not confirmed. Close, refresh and review the current record
          before trying again.
        </p>
        <AlertDialogFooter>
          <AlertDialogCancel :disabled="saving">Keep as is</AlertDialogCancel>
          <Button :disabled="saving || !canConfirmLifecycle" @click.prevent="confirmLifecycle">
            <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
            {{
              lifecycleTarget?.action === 'delete'
                ? 'Delete unused record'
                : lifecycleTarget?.action === 'deactivate'
                  ? 'Deactivate record'
                  : 'Reactivate record'
            }}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  </div>
</template>
