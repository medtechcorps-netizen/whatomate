<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  AlertCircle,
  ArrowLeft,
  CalendarClock,
  Check,
  Clock3,
  Loader2,
  MapPin,
  RefreshCw,
  UserRound,
  UsersRound,
} from 'lucide-vue-next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import {
  bookingService,
  type Booking,
  type BookingAvailabilitySlot,
  type BookingResource,
} from '@/services/productSuite'

const props = withDefaults(defineProps<{
  open: boolean
  contactId: string
  contactName: string
  surface?: 'chat' | 'omnichannel'
}>(), {
  surface: 'chat',
})

const emit = defineEmits<{
  'update:open': [open: boolean]
  booked: [booking: Booking]
}>()

const loading = ref(false)
const submitting = ref(false)
const slots = ref<BookingAvailabilitySlot[]>([])
const serviceFilter = ref('')
const resourceFilter = ref('')
const selectedSlotId = ref('')
const reviewing = ref(false)
const loadError = ref('')
const bookingError = ref('')
const availabilityNotice = ref('')
const idempotencyKey = ref('')
let loadSequence = 0
let dialogGeneration = 0
let submissionSequence = 0

interface BookingOperation {
  id: number
  dialogGeneration: number
  contactId: string
}

let activeBookingOperation: BookingOperation | null = null

const now = () => new Date()

const validSlots = computed(() => {
  const currentTime = Date.now()
  return slots.value
    .filter((slot) => slot.status === 'scheduled')
    .filter((slot) => new Date(slot.starts_at).getTime() > currentTime)
    .filter((slot) => slot.remaining_capacity > 0)
    .filter((slot) => slot.service?.is_active !== false && slot.resource?.is_active !== false)
    .sort((left, right) => new Date(left.starts_at).getTime() - new Date(right.starts_at).getTime())
})

const serviceOptions = computed(() => {
  const unique = new Map<string, string>()
  for (const slot of validSlots.value) {
    unique.set(slot.service_id, slot.service?.name || 'Service')
  }
  return [...unique.entries()]
    .map(([id, name]) => ({ id, name }))
    .sort((left, right) => left.name.localeCompare(right.name))
})

const resourceOptions = computed(() => {
  const unique = new Map<string, { id: string; name: string; kind?: BookingResource['kind'] }>()
  for (const slot of validSlots.value) {
    if (serviceFilter.value && slot.service_id !== serviceFilter.value) continue
    unique.set(slot.resource_id, {
      id: slot.resource_id,
      name: slot.resource?.name || 'Resource',
      kind: slot.resource?.kind,
    })
  }
  return [...unique.values()].sort((left, right) => left.name.localeCompare(right.name))
})

const filteredSlots = computed(() =>
  validSlots.value.filter((slot) =>
    (!serviceFilter.value || slot.service_id === serviceFilter.value) &&
    (!resourceFilter.value || slot.resource_id === resourceFilter.value),
  ),
)

const selectedSlot = computed(() =>
  validSlots.value.find((slot) => slot.id === selectedSlotId.value) ?? null,
)

function availabilityRange() {
  const from = now()
  const to = new Date(from)
  to.setDate(to.getDate() + 30)
  return { from: from.toISOString(), to: to.toISOString() }
}

function resetDialog() {
  serviceFilter.value = ''
  resourceFilter.value = ''
  selectedSlotId.value = ''
  reviewing.value = false
  loadError.value = ''
  bookingError.value = ''
  availabilityNotice.value = ''
  idempotencyKey.value = crypto.randomUUID()
}

function ownsBookingOperation(operation: BookingOperation) {
  return activeBookingOperation?.id === operation.id &&
    operation.dialogGeneration === dialogGeneration &&
    operation.contactId === props.contactId &&
    props.open
}

async function loadAvailability(preserveNotice = false) {
  const sequence = ++loadSequence
  loading.value = true
  loadError.value = ''
  if (!preserveNotice) availabilityNotice.value = ''

  try {
    const result = await bookingService.allAvailability(availabilityRange())
    if (sequence !== loadSequence) return
    slots.value = result
    if (selectedSlotId.value && !validSlots.value.some((slot) => slot.id === selectedSlotId.value)) {
      selectedSlotId.value = ''
      reviewing.value = false
    }
  } catch (cause) {
    if (sequence !== loadSequence) return
    slots.value = []
    loadError.value = getErrorMessage(cause, 'Available appointments could not be loaded.')
  } finally {
    if (sequence === loadSequence) loading.value = false
  }
}

function closeDialog() {
  if (submitting.value) return
  emit('update:open', false)
}

function selectSlot(slotId: string) {
  selectedSlotId.value = slotId
  reviewing.value = false
  bookingError.value = ''
  availabilityNotice.value = ''
  idempotencyKey.value = crypto.randomUUID()
}

function startReview() {
  if (!selectedSlot.value) return
  reviewing.value = true
  bookingError.value = ''
}

async function confirmBooking() {
  const slot = selectedSlot.value
  if (!slot || submitting.value) return

  const operation: BookingOperation = {
    id: ++submissionSequence,
    dialogGeneration,
    contactId: props.contactId,
  }
  const requestIdempotencyKey = idempotencyKey.value
  activeBookingOperation = operation
  submitting.value = true
  bookingError.value = ''
  try {
    const response = await bookingService.createBooking(slot.id, {
      contact_id: operation.contactId,
      quantity: 1,
      status: 'reserved',
      source: 'agent',
      allow_waitlist: false,
      idempotency_key: requestIdempotencyKey,
    })
    if (!ownsBookingOperation(operation)) return
    const booking = unwrapItemResponse<Booking>(response)
    emit('booked', booking)
    emit('update:open', false)
  } catch (cause) {
    if (!ownsBookingOperation(operation)) return
    const status = (cause as { response?: { status?: number } })?.response?.status
    if (status === 409) {
      selectedSlotId.value = ''
      reviewing.value = false
      idempotencyKey.value = crypto.randomUUID()
      availabilityNotice.value = getErrorMessage(
        cause,
        'That appointment is no longer available. The available times have been refreshed.',
      )
      await loadAvailability(true)
    } else {
      bookingError.value = getErrorMessage(cause, 'The appointment could not be booked.')
    }
  } finally {
    if (ownsBookingOperation(operation)) {
      activeBookingOperation = null
      submitting.value = false
    }
  }
}

function slotTimeZone(slot: BookingAvailabilitySlot) {
  return slot.timezone || slot.resource?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone
}

function slotDate(slot: BookingAvailabilitySlot) {
  return new Intl.DateTimeFormat('en-MY', {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    timeZone: slotTimeZone(slot),
  }).format(new Date(slot.starts_at))
}

function slotTime(slot: BookingAvailabilitySlot) {
  return new Intl.DateTimeFormat('en-MY', {
    hour: '2-digit',
    minute: '2-digit',
    timeZone: slotTimeZone(slot),
  }).format(new Date(slot.starts_at))
}

function resourceLabel(slot: BookingAvailabilitySlot) {
  return slot.resource?.name || 'Assigned resource'
}

function resourceKind(slot: BookingAvailabilitySlot) {
  return (slot.resource?.kind || 'resource').replace('_', ' ')
}

function remainingLabel(slot: BookingAvailabilitySlot) {
  return `${slot.remaining_capacity} ${slot.remaining_capacity === 1 ? 'place' : 'places'} left`
}

watch(serviceFilter, () => {
  if (resourceFilter.value && !resourceOptions.value.some((resource) => resource.id === resourceFilter.value)) {
    resourceFilter.value = ''
  }
  if (selectedSlot.value && !filteredSlots.value.some((slot) => slot.id === selectedSlot.value?.id)) {
    selectedSlotId.value = ''
    reviewing.value = false
  }
})

watch(resourceFilter, () => {
  if (selectedSlot.value && !filteredSlots.value.some((slot) => slot.id === selectedSlot.value?.id)) {
    selectedSlotId.value = ''
    reviewing.value = false
  }
})

watch(
  () => [props.open, props.contactId] as const,
  ([open]) => {
    dialogGeneration += 1
    activeBookingOperation = null
    submitting.value = false
    if (!open) {
      loadSequence += 1
      return
    }
    resetDialog()
    void loadAvailability()
  },
  { immediate: true },
)
</script>

<template>
  <Dialog :open="open" @update:open="(value) => value ? undefined : closeDialog()">
    <DialogContent class="max-h-[92vh] w-[calc(100vw-1rem)] max-w-3xl gap-0 overflow-hidden p-0 [&>button]:flex [&>button]:h-11 [&>button]:w-11 [&>button]:items-center [&>button]:justify-center sm:w-full">
      <DialogHeader class="border-b border-white/[0.08] px-5 py-5 pr-12 text-left light:border-gray-200 sm:px-6">
        <div class="flex items-center gap-2 text-fuchsia-300">
          <CalendarClock class="h-4 w-4" />
          <span class="text-[10px] font-semibold uppercase tracking-[0.2em]">Care and bookings</span>
        </div>
        <DialogTitle class="mt-2 text-lg">Book an appointment</DialogTitle>
        <DialogDescription class="mt-1">
          Choose an available time for <span class="font-medium text-white light:text-gray-900">{{ contactName }}</span>.
          Availability covers the next 30 days.
        </DialogDescription>
      </DialogHeader>

      <div class="min-h-0 overflow-y-auto px-5 py-5 sm:px-6">
        <div v-if="loading" class="flex min-h-64 flex-col items-center justify-center text-center" aria-live="polite">
          <Loader2 class="h-6 w-6 animate-spin text-fuchsia-300" />
          <p class="mt-3 text-sm font-medium">Checking available appointments</p>
          <p class="mt-1 text-xs text-white/40 light:text-gray-500">Only active, future times with capacity are included.</p>
        </div>

        <div v-else-if="loadError" class="flex min-h-64 flex-col items-center justify-center text-center" role="alert">
          <AlertCircle class="h-7 w-7 text-rose-300" />
          <h3 class="mt-3 text-sm font-semibold">Availability is unavailable</h3>
          <p class="mt-1 max-w-sm text-xs leading-5 text-white/45 light:text-gray-600">{{ loadError }}</p>
          <Button type="button" variant="outline" class="mt-4 h-11" @click="loadAvailability()">
            <RefreshCw class="mr-2 h-4 w-4" />
            Try again
          </Button>
        </div>

        <template v-else-if="!reviewing">
          <div
            v-if="availabilityNotice"
            class="mb-4 flex items-start gap-2 rounded-xl border border-amber-300/20 bg-amber-300/[0.06] p-3 text-xs leading-5 text-amber-100 light:text-amber-800"
            role="status"
          >
            <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
            <span>{{ availabilityNotice }}</span>
          </div>

          <div v-if="validSlots.length" class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label for="contact-booking-service">Service</Label>
              <select
                id="contact-booking-service"
                v-model="serviceFilter"
                class="h-11 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fuchsia-300 light:border-gray-200 light:bg-white light:text-gray-900"
              >
                <option value="">All services</option>
                <option v-for="service in serviceOptions" :key="service.id" :value="service.id">{{ service.name }}</option>
              </select>
            </div>
            <div class="space-y-1.5">
              <Label for="contact-booking-resource">Practitioner or resource</Label>
              <select
                id="contact-booking-resource"
                v-model="resourceFilter"
                class="h-11 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fuchsia-300 light:border-gray-200 light:bg-white light:text-gray-900"
              >
                <option value="">All practitioners and resources</option>
                <option v-for="resource in resourceOptions" :key="resource.id" :value="resource.id">
                  {{ resource.name }}{{ resource.kind ? ` · ${resource.kind}` : '' }}
                </option>
              </select>
            </div>
          </div>

          <fieldset v-if="filteredSlots.length" class="mt-5 space-y-2">
            <legend class="mb-2 text-xs font-semibold text-white/65 light:text-gray-700">Available times</legend>
            <label
              v-for="slot in filteredSlots"
              :key="slot.id"
              data-testid="booking-slot-option"
              class="block min-h-24 cursor-pointer rounded-xl border p-3 transition focus-within:ring-2 focus-within:ring-fuchsia-300"
              :class="selectedSlotId === slot.id
                ? 'border-fuchsia-300/55 bg-fuchsia-300/[0.08]'
                : 'border-white/[0.08] bg-white/[0.025] hover:border-fuchsia-300/25 hover:bg-fuchsia-300/[0.035] light:border-gray-200 light:bg-gray-50'"
            >
              <input
                v-model="selectedSlotId"
                type="radio"
                name="contact-booking-slot"
                :value="slot.id"
                class="sr-only"
                :aria-label="`${slot.service?.name || 'Appointment'}, ${slotDate(slot)} at ${slotTime(slot)} with ${resourceLabel(slot)}`"
                @change="selectSlot(slot.id)"
              />
              <span class="flex items-start justify-between gap-3">
                <span class="min-w-0">
                  <span class="block truncate text-sm font-semibold">{{ slot.service?.name || 'Appointment' }}</span>
                  <span class="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-white/50 light:text-gray-600">
                    <span class="flex items-center gap-1.5"><Clock3 class="h-3.5 w-3.5" />{{ slotDate(slot) }} · {{ slotTime(slot) }}</span>
                    <span class="flex items-center gap-1.5 capitalize"><UserRound class="h-3.5 w-3.5" />{{ resourceLabel(slot) }} · {{ resourceKind(slot) }}</span>
                    <span class="flex items-center gap-1.5"><MapPin class="h-3.5 w-3.5" />{{ slot.location || slot.resource?.location || 'Location not specified' }}</span>
                  </span>
                  <span class="mt-2 block text-[10px] text-white/35 light:text-gray-500">{{ slotTimeZone(slot) }}</span>
                </span>
                <span class="flex shrink-0 items-center gap-2">
                  <Badge variant="outline" class="gap-1 border-emerald-300/25 text-[9px] text-emerald-200 light:text-emerald-800">
                    <UsersRound class="h-3 w-3" />
                    {{ remainingLabel(slot) }}
                  </Badge>
                  <span
                    v-if="selectedSlotId === slot.id"
                    class="flex h-6 w-6 items-center justify-center rounded-full bg-fuchsia-300 text-black"
                    aria-hidden="true"
                  >
                    <Check class="h-3.5 w-3.5" />
                  </span>
                </span>
              </span>
            </label>
          </fieldset>

          <div v-else class="flex min-h-56 flex-col items-center justify-center rounded-xl border border-dashed border-white/[0.08] px-5 text-center light:border-gray-200">
            <CalendarClock class="h-7 w-7 text-white/25 light:text-gray-400" />
            <h3 class="mt-3 text-sm font-semibold">No available appointments</h3>
            <p class="mt-1 max-w-sm text-xs leading-5 text-white/40 light:text-gray-500">
              {{ validSlots.length ? 'No times match these filters. Try another service or practitioner.' : 'There are no active future schedules with capacity in the next 30 days.' }}
            </p>
            <Button v-if="validSlots.length" type="button" variant="outline" class="mt-4 h-11" @click="serviceFilter = ''; resourceFilter = ''">
              Clear filters
            </Button>
          </div>
        </template>

        <div v-else-if="selectedSlot" class="space-y-4">
          <Button type="button" variant="ghost" class="h-11 px-2" :disabled="submitting" @click="reviewing = false">
            <ArrowLeft class="mr-2 h-4 w-4" />
            Choose another time
          </Button>

          <div class="rounded-2xl border border-fuchsia-300/20 bg-fuchsia-300/[0.05] p-4 sm:p-5">
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-fuchsia-300">Confirm booking</p>
            <h3 class="mt-2 text-base font-semibold">{{ selectedSlot.service?.name || 'Appointment' }}</h3>
            <dl class="mt-4 grid gap-3 text-sm sm:grid-cols-2">
              <div>
                <dt class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Customer</dt>
                <dd class="mt-1 font-medium">{{ contactName }}</dd>
              </div>
              <div>
                <dt class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Practitioner / resource</dt>
                <dd class="mt-1 font-medium">{{ resourceLabel(selectedSlot) }}</dd>
              </div>
              <div>
                <dt class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Local date and time</dt>
                <dd class="mt-1 font-medium">{{ slotDate(selectedSlot) }} · {{ slotTime(selectedSlot) }}</dd>
                <dd class="mt-0.5 text-[10px] text-white/35 light:text-gray-500">{{ slotTimeZone(selectedSlot) }}</dd>
              </div>
              <div>
                <dt class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Location</dt>
                <dd class="mt-1 font-medium">{{ selectedSlot.location || selectedSlot.resource?.location || 'Location not specified' }}</dd>
              </div>
            </dl>
            <div class="mt-4 flex items-center justify-between gap-3 border-t border-white/[0.08] pt-4 text-xs light:border-gray-200">
              <span class="text-white/45 light:text-gray-600">Status after booking</span>
              <Badge variant="outline" class="capitalize">Reserved · 1 place</Badge>
            </div>
          </div>

          <div v-if="bookingError" class="flex items-start gap-2 rounded-xl border border-rose-300/20 bg-rose-300/[0.06] p-3 text-xs leading-5 text-rose-100 light:text-rose-800" role="alert">
            <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
            <span>{{ bookingError }}</span>
          </div>

          <p class="text-xs leading-5 text-white/40 light:text-gray-500">
            Capacity will be checked once more when you confirm. If the final place has gone, this list will refresh automatically.
          </p>
        </div>
      </div>

      <DialogFooter class="border-t border-white/[0.08] px-5 py-4 light:border-gray-200 sm:px-6">
        <Button type="button" variant="outline" class="h-11" :disabled="submitting" @click="closeDialog">Cancel</Button>
        <Button
          v-if="!reviewing"
          type="button"
          class="h-11 bg-fuchsia-400 text-black hover:bg-fuchsia-300"
          :disabled="loading || !selectedSlot"
          @click="startReview"
        >
          Review appointment
        </Button>
        <Button
          v-else
          type="button"
          class="h-11 bg-fuchsia-400 text-black hover:bg-fuchsia-300"
          :disabled="!selectedSlot || submitting"
          @click="confirmBooking"
        >
          <Loader2 v-if="submitting" class="mr-2 h-4 w-4 animate-spin" />
          {{ submitting ? 'Booking…' : 'Confirm booking' }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
