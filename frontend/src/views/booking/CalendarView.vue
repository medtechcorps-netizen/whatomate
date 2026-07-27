<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
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
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
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
  default_capacity: 1,
  price: '0',
  currency: 'MYR',
  resource_ids: [] as string[],
  version: 0,
  metadata: {} as Record<string, unknown>,
  reminder_policy: {} as Record<string, unknown>,
})
const canWriteBookings = computed(() => authStore.hasPermission('bookings', 'write'))
const canWriteBookingSettings = computed(() => authStore.hasPermission('booking.settings', 'write'))
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
    services.value = serviceResponse
    resources.value = resourceResponse
    events.value = eventResponse
    if (selectedEvent.value) {
      selectedEvent.value =
        events.value.find((event) => event.id === selectedEvent.value?.id) ?? null
    }

    if (!newEvent.service_id || !activeServices.value.some((service) => service.id === newEvent.service_id)) {
      newEvent.service_id = activeServices.value[0]?.id ?? ''
    }
    syncEventResource()
    if (!serviceDraft.resource_ids.length && resources.value[0]?.id) {
      serviceDraft.resource_ids = [resources.value[0].id]
    }
    if (!newEvent.date) newEvent.date = isoDate(new Date())
  } catch (error) {
    toast.error('Calendar could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function createResource() {
  if (!resourceDraft.name.trim() || !resourceDraft.timezone.trim()) {
    toast.warning('Resource name and timezone are required')
    return
  }
  saving.value = true
  try {
    const response = await bookingService.createResource({
      name: resourceDraft.name.trim(),
      kind: resourceDraft.kind,
      timezone: resourceDraft.timezone.trim(),
      location: resourceDraft.location.trim(),
      is_active: true,
    })
    const resource = unwrapItemResponse<BookingResource>(response)
    resourceDraft.name = ''
    resourceDraft.location = ''
    if (!serviceDraft.resource_ids.includes(resource.id)) {
      serviceDraft.resource_ids = [...serviceDraft.resource_ids, resource.id]
    }
    toast.success('Booking resource created')
    await load()
  } catch (error) {
    toast.error('Resource was not created', getErrorMessage(error))
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
  serviceDraft.default_capacity = 1
  serviceDraft.price = '0'
  serviceDraft.currency = 'MYR'
  serviceDraft.resource_ids = resources.value[0]?.id ? [resources.value[0].id] : []
  serviceDraft.version = 0
  serviceDraft.metadata = {}
  serviceDraft.reminder_policy = {}
}

function reviewService(service: BookingService) {
  showSetup.value = true
  serviceDraft.id = service.id
  serviceDraft.name = service.name
  serviceDraft.description = service.description ?? ''
  serviceDraft.kind = service.kind
  serviceDraft.duration_minutes = service.duration_minutes
  serviceDraft.default_capacity = service.default_capacity
  serviceDraft.price = (service.price_minor / 100).toFixed(2)
  serviceDraft.currency = service.currency
  serviceDraft.resource_ids = service.resource_ids?.length
    ? [...service.resource_ids]
    : resources.value[0]?.id
      ? [resources.value[0].id]
      : []
  serviceDraft.version = service.version
  serviceDraft.metadata = service.metadata ?? {}
  serviceDraft.reminder_policy = service.reminder_policy ?? {}
}

async function saveService() {
  const priceMinor = Math.round(Number(serviceDraft.price) * 100)
  if (
    !serviceDraft.name.trim() ||
    !serviceDraft.resource_ids.length ||
    serviceDraft.duration_minutes < 1 ||
    serviceDraft.default_capacity < 1 ||
    !Number.isFinite(priceMinor) ||
    priceMinor < 0
  ) {
    toast.warning('Name, resource, duration, capacity and a valid price are required')
    return
  }
  saving.value = true
  const payload: Partial<BookingService> = {
    name: serviceDraft.name.trim(),
    description: serviceDraft.description.trim(),
    kind: serviceDraft.kind,
    duration_minutes: serviceDraft.duration_minutes,
    buffer_before_minutes: 0,
    buffer_after_minutes: 0,
    default_capacity: serviceDraft.default_capacity,
    price_minor: priceMinor,
    currency: serviceDraft.currency,
    reminder_policy: serviceDraft.reminder_policy,
    metadata: serviceDraft.id
      ? {
          ...serviceDraft.metadata,
          requires_review: false,
          reviewed_at: new Date().toISOString(),
        }
      : serviceDraft.metadata,
    resource_ids: serviceDraft.resource_ids,
    is_active: true,
    version: serviceDraft.version || undefined,
  }
  try {
    if (serviceDraft.id) {
      await bookingService.updateService(serviceDraft.id, payload)
      toast.success('Service reviewed and activated')
    } else {
      await bookingService.createService(payload)
      toast.success('Booking service created')
    }
    resetServiceDraft()
    await load()
  } catch (error) {
    toast.error('Service was not saved', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function selectEvent(event: BookingEvent) {
  selectedEvent.value = event
  await loadBookings()
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
  <div class="flex h-full flex-col bg-[#08090a] light:bg-[#f6f4ef]">
    <PageHeader
      title="Bookings & classes"
      description="Appointments, resources and capacity across the week."
      :icon="CalendarDays"
      icon-gradient="bg-gradient-to-br from-fuchsia-500 to-violet-700 shadow-fuchsia-500/20"
    >
      <template #actions>
        <Button v-if="canWriteBookingSettings" variant="outline" @click="showSetup = !showSetup">
          Service setup
        </Button>
        <Button
          v-if="canWriteBookings"
          class="bg-fuchsia-400 text-black hover:bg-fuchsia-300"
          :disabled="!activeServices.length || !resources.length"
          @click="showCreate = !showCreate"
        >
          <Plus class="mr-2 h-4 w-4" />
          Add schedule
        </Button>
      </template>
    </PageHeader>

    <section
      v-if="showSetup && canWriteBookingSettings"
      class="grid gap-4 border-b border-cyan-300/15 bg-cyan-300/[0.035] px-5 py-4 xl:grid-cols-2"
    >
      <form
        class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 md:grid-cols-2 light:border-gray-200 light:bg-white"
        @submit.prevent="createResource"
      >
        <div class="md:col-span-2">
          <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-cyan-300">1 · Schedulable resource</p>
          <p class="mt-1 text-xs text-white/40 light:text-gray-500">Add a practitioner, instructor, room, or equipment.</p>
        </div>
        <Input v-model="resourceDraft.name" required maxlength="255" placeholder="Resource name" />
        <select v-model="resourceDraft.kind" class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
          <option value="practitioner">Practitioner</option>
          <option value="instructor">Instructor</option>
          <option value="room">Room</option>
          <option value="equipment">Equipment</option>
        </select>
        <Input v-model="resourceDraft.timezone" required maxlength="100" placeholder="Timezone, e.g. Asia/Kuala_Lumpur" />
        <Input v-model="resourceDraft.location" maxlength="255" placeholder="Location (optional)" />
        <Button type="submit" class="md:col-span-2" variant="outline" :disabled="saving">
          <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
          Add resource
        </Button>
      </form>

      <form
        class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 md:grid-cols-2 light:border-gray-200 light:bg-white"
        @submit.prevent="saveService"
      >
        <div class="flex items-start justify-between gap-3 md:col-span-2">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-cyan-300">2 · Service offering</p>
            <p class="mt-1 text-xs text-white/40 light:text-gray-500">
              {{ serviceDraft.id ? 'Review the starter values before activation.' : 'Tie an active service to a resource.' }}
            </p>
          </div>
          <Button v-if="serviceDraft.id" type="button" size="sm" variant="outline" @click="resetServiceDraft">New instead</Button>
        </div>
        <Input v-model="serviceDraft.name" required maxlength="255" placeholder="Service name" />
        <select v-model="serviceDraft.kind" class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
          <option value="appointment">Appointment</option>
          <option value="class">Class</option>
        </select>
        <select
          v-model="serviceDraft.resource_ids"
          required
          multiple
          class="min-h-24 rounded-md border border-white/10 bg-[#0d0f10] px-3 py-2 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
        >
          <option v-for="resource in resources" :key="resource.id" :value="resource.id">{{ resource.name }}</option>
        </select>
        <Input v-model.number="serviceDraft.duration_minutes" required type="number" min="1" max="10080" placeholder="Minutes" />
        <Input v-model.number="serviceDraft.default_capacity" required type="number" min="1" max="100000" placeholder="Capacity" />
        <div class="grid grid-cols-[1fr_90px] gap-2">
          <Input v-model="serviceDraft.price" required type="number" min="0" step="0.01" placeholder="Price" />
          <select v-model="serviceDraft.currency" class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-2 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
            <option value="MYR">MYR</option>
            <option value="SGD">SGD</option>
            <option value="USD">USD</option>
          </select>
        </div>
        <Input v-model="serviceDraft.description" maxlength="10000" placeholder="Description (optional)" class="md:col-span-2" />
        <Button type="submit" class="bg-cyan-400 text-black hover:bg-cyan-300 md:col-span-2" :disabled="saving || !resources.length">
          <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
          {{ serviceDraft.id ? 'Review & activate' : 'Create service' }}
        </Button>
      </form>
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
        <option v-for="service in activeServices" :key="service.id" :value="service.id">{{ service.name }}</option>
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

    <section
      v-if="selectedEvent"
      class="border-b border-violet-300/15 bg-violet-300/[0.035] px-5 py-4"
    >
      <div class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-violet-300">Attendee desk</p>
          <h2 class="mt-1 text-base font-semibold text-white light:text-gray-900">
            {{ selectedEvent.service?.name || 'Scheduled service' }}
          </h2>
          <p class="mt-1 text-xs text-white/40 light:text-gray-500">
            {{ eventDateTimeLabel(selectedEvent) }}
            · {{ selectedEvent.booked_quantity ?? 0 }}/{{ selectedEvent.capacity }} seats
          </p>
        </div>
        <Button variant="outline" size="sm" @click="selectedEvent = null; bookings = []">Close</Button>
      </div>

      <div class="mt-4 grid gap-4 xl:grid-cols-[390px_1fr]">
        <form
          v-if="canCreateAttendees"
          class="grid gap-3 rounded-2xl border border-white/[0.08] bg-black/15 p-4 light:border-gray-200 light:bg-white"
          @submit.prevent="createAttendeeBooking"
        >
          <ContactPicker v-model="bookingDraft.contact_id" placeholder="Search customer to book" />
          <div class="grid grid-cols-[100px_1fr] gap-3">
            <Input v-model.number="bookingDraft.quantity" type="number" min="1" :max="selectedEvent.capacity" />
            <Input v-model="bookingDraft.notes" maxlength="2000" placeholder="Internal booking note (optional)" />
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

        <div class="min-w-0 rounded-2xl border border-white/[0.08] bg-black/15 p-3 light:border-gray-200 light:bg-white">
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
                  <p class="truncate text-sm font-medium text-white light:text-gray-900">{{ customerLabel(booking) }}</p>
                  <p class="mt-1 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">
                    {{ booking.quantity }} seat{{ booking.quantity === 1 ? '' : 's' }}
                  </p>
                </div>
                <Badge variant="outline" class="shrink-0 capitalize">{{ booking.status.replace('_', ' ') }}</Badge>
              </div>
              <div v-if="canWriteBookings && bookingTransitions(booking.status).length" class="mt-3 flex flex-wrap gap-1.5">
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
            <p v-if="!bookings.length" class="col-span-full py-8 text-center text-xs text-white/35 light:text-gray-500">
              No attendees have been booked for this schedule.
            </p>
          </div>
        </div>
      </div>
    </section>

    <div class="flex items-center justify-between border-b border-white/[0.08] px-5 py-3 light:border-gray-200">
      <div class="flex items-center gap-2">
        <Button variant="outline" size="icon" @click="shiftWeek(-1)"><ChevronLeft class="h-4 w-4" /></Button>
        <Button variant="outline" size="sm" @click="goToday">Today</Button>
        <Button variant="outline" size="icon" @click="shiftWeek(1)"><ChevronRight class="h-4 w-4" /></Button>
        <p class="ml-2 text-sm font-semibold text-white light:text-gray-900">{{ weekLabel }}</p>
      </div>
      <div class="hidden items-center gap-5 text-xs text-white/45 light:text-gray-500 md:flex">
        <span>{{ events.length }} schedules</span>
        <span>{{ weekBooked }}/{{ weekCapacity }} seats booked</span>
      </div>
    </div>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-fuchsia-300" />
    </div>

    <div v-else class="grid min-h-0 flex-1 xl:grid-cols-[1fr_270px]">
      <div class="overflow-auto p-4 md:p-5">
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
              <p class="text-[10px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-gray-500">
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
                role="button"
                tabindex="0"
                class="cursor-pointer rounded-xl border p-3 transition hover:-translate-y-0.5"
                :class="serviceTone(event)"
                @click="selectEvent(event)"
                @keydown.enter="selectEvent(event)"
              >
                <div class="flex items-center justify-between gap-2">
                  <span class="text-[10px] font-semibold uppercase tracking-wider opacity-70">
                    {{ timeLabel(event.starts_at, event.resource?.timezone) }}
                  </span>
                  <Badge variant="outline" class="h-5 border-current/20 px-1.5 text-[9px]">
                    {{ event.booked_quantity ?? 0 }}/{{ event.capacity }}
                  </Badge>
                </div>
                <p class="mt-2 text-xs font-semibold leading-5">{{ event.service?.name || 'Scheduled service' }}</p>
                <div class="mt-2 space-y-1 text-[10px] opacity-65">
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

      <aside class="hidden overflow-y-auto border-l border-white/[0.08] bg-[#0b0c0d] p-4 light:border-gray-200 light:bg-white xl:block">
        <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">Service menu</p>
        <div class="mt-3 space-y-2">
          <article
            v-for="service in services"
            :key="service.id"
            class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
          >
            <div class="flex items-start justify-between gap-2">
              <div>
                <p class="text-xs font-medium text-white light:text-gray-900">{{ service.name }}</p>
                <p class="mt-1 flex items-center gap-1 text-[10px] text-white/35 light:text-gray-500">
                  <Clock3 class="h-3 w-3" />
                  {{ service.duration_minutes }} min · {{ service.kind }}
                </p>
              </div>
              <div class="text-right">
                <span class="text-[11px] font-medium text-emerald-300">
                  {{ new Intl.NumberFormat('en-MY', { style: 'currency', currency: service.currency }).format(service.price_minor / 100) }}
                </span>
                <button
                  v-if="!service.is_active && canWriteBookingSettings"
                  type="button"
                  class="mt-2 block text-[10px] font-semibold text-amber-300 hover:text-amber-200"
                  @click="reviewService(service)"
                >
                  Review & activate
                </button>
              </div>
            </div>
          </article>
          <div v-if="services.length === 0" class="rounded-xl bg-white/[0.025] p-5 text-center text-xs text-white/35 light:bg-gray-50 light:text-gray-500">
            Apply a vertical playbook or add your first service.
          </div>
        </div>

        <p class="mt-7 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">Resources</p>
        <div class="mt-3 space-y-2">
          <div
            v-for="resource in resources"
            :key="resource.id"
            class="flex items-center gap-3 rounded-xl border border-white/[0.07] px-3 py-2.5 light:border-gray-200"
          >
            <div class="flex h-8 w-8 items-center justify-center rounded-full bg-violet-300/10 text-xs font-semibold text-violet-200">
              {{ resource.name.slice(0, 2).toUpperCase() }}
            </div>
            <div class="min-w-0">
              <p class="truncate text-xs font-medium text-white light:text-gray-900">{{ resource.name }}</p>
              <p class="mt-0.5 text-[10px] capitalize text-white/35 light:text-gray-500">{{ resource.kind }}</p>
            </div>
          </div>
        </div>
      </aside>
    </div>
  </div>
</template>
