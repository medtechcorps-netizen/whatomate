<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, type Component, type Ref } from 'vue'
import {
  AlertCircle,
  ArrowRight,
  CalendarClock,
  CalendarDays,
  Inbox,
  Loader2,
  Receipt,
  RefreshCw,
  Sun,
  Target,
  Trophy,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import InvoiceQuickDialog from '@/components/commerce/InvoiceQuickDialog.vue'
import { Button } from '@/components/ui/button'
import { contactDisplayName } from '@/lib/contactAddress'
import { invoiceForLead, isFollowUpDueToday, isFollowUpOverdue } from '@/lib/crmFlow'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import {
  bookingService,
  commerceService,
  crmService,
  type Booking,
  type BookingEvent,
  type CommerceInvoice,
  type CRMLead,
  type FollowUpTask,
} from '@/services/productSuite'
import { useAuthStore } from '@/stores/auth'
import { useOmnichannelUnreadStore } from '@/stores/omnichannelUnread'

type LoadStatus = 'idle' | 'loading' | 'ready' | 'error'
type CardStatus = 'loading' | 'ready' | 'error'
type CardId = 'unread' | 'follow-ups' | 'leads' | 'won' | 'unpaid' | 'appointments'

interface CardRow {
  id: string
  title: string
  subtitle?: string
  meta?: string
  metaTone?: 'danger' | 'muted'
  to?: string
  link?: { label: string; ariaLabel: string; to: string }
  action?: { label: string; ariaLabel: string; run: () => void }
}

interface CardGroup {
  key: string
  label?: string
  rows: CardRow[]
}

interface TodayCard {
  id: CardId
  title: string
  icon: Component
  iconClass: string
  status: CardStatus
  count: number | null
  errorText: string
  emptyText: string
  summary: string[]
  note?: string
  groups: CardGroup[]
  primary?: { label: string; to: string }
  retry: () => void
}

type TodayInvoice = CommerceInvoice & {
  contact?: { id: string; name?: string; profile_name?: string; phone_number?: string }
}

const MAX_ROWS = 5
const WON_WINDOW_DAYS = 30
// Invoices are matched to won leads by creation date. Look back further than
// the won window so an invoice made shortly before the lead was marked won
// still counts.
const INVOICE_LOOKBACK_DAYS = WON_WINDOW_DAYS + 30
const DAY_MS = 24 * 60 * 60 * 1000
const LOCALE = 'en-MY'

const authStore = useAuthStore()
const unreadStore = useOmnichannelUnreadStore()

let alive = true

function createResource<T>(fetcher: () => Promise<T[]>) {
  const status = ref<LoadStatus>('idle') as Ref<LoadStatus>
  const items = shallowRef<T[]>([])
  let generation = 0
  async function load() {
    const current = ++generation
    status.value = 'loading'
    try {
      const result = await fetcher()
      if (!alive || current !== generation) return
      items.value = Array.isArray(result) ? result : []
      status.value = 'ready'
    } catch {
      if (!alive || current !== generation) return
      status.value = 'error'
    }
  }
  return { status, items, load }
}

// ---- Access -----------------------------------------------------------------

const can = (resource: string, action = 'read') => authStore.hasPermission(resource, action)
const entitled = (key: string) => authStore.hasProductEntitlement(key)

const crmEnabled = computed(() => entitled('crm.enabled'))
const commerceEnabled = computed(() => entitled('commerce.enabled'))

// Same rule as the Inbox item in the main navigation.
const canSeeUnread = computed(() =>
  entitled('omnichannel.enabled') && can('conversations') && can('channel_accounts'),
)
const canSeeFollowUps = computed(() => crmEnabled.value && can('tasks'))
const canSeeLeads = computed(() => crmEnabled.value && can('crm.leads'))
const canSeeWon = computed(() => canSeeLeads.value && can('payments') && commerceEnabled.value)
const canSeeUnpaid = computed(() => can('payments') && commerceEnabled.value)
const canSeeAppointments = computed(() => can('bookings') && entitled('bookings.enabled'))
const canOpenChat = computed(() => can('chat'))
const canInvoice = computed(() => can('payments', 'write') && can('contacts') && commerceEnabled.value)
const canSellPackages = computed(() => canInvoice.value && can('packages', 'write'))
// Links only point at pages the user can open (same rules as the router).
const canOpenPipeline = computed(() => crmEnabled.value && can('crm.leads') && can('crm.pipelines'))
const canOpenCalendar = computed(() => entitled('bookings.enabled') && can('bookings') && can('booking.settings'))

// ---- Data (one request per resource, shared between cards) ------------------

// GET /api/tasks filters on a single status, so both active statuses are
// fetched and merged. A follow-up in progress is still due.
const tasks = createResource<FollowUpTask>(async () => {
  const [open, inProgress] = await Promise.all([
    crmService.allTasks({ status: 'open' }),
    crmService.allTasks({ status: 'in_progress' }),
  ])
  const merged = new Map<string, FollowUpTask>()
  for (const task of [...(open ?? []), ...(inProgress ?? [])]) {
    if (task?.id) merged.set(task.id, task)
  }
  return [...merged.values()]
})
const openLeads = createResource<CRMLead>(() => crmService.allLeads({ status: 'open' }))
const wonLeads = createResource<CRMLead>(() => crmService.allLeads({ status: 'won' }))
// Only open invoices can still be owed money (draft, void and paid ones are not).
const openInvoices = createResource<TodayInvoice>(() => commerceService.allInvoices({ status: 'open' }))
// Won leads are matched against recent invoices only, not the whole history.
const recentInvoices = createResource<TodayInvoice>(() =>
  commerceService.allInvoices({ from: new Date(Date.now() - INVOICE_LOOKBACK_DAYS * DAY_MS).toISOString() }),
)
const events = createResource<BookingEvent>(() => {
  const now = new Date()
  const from = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const to = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59, 999)
  return bookingService.allEvents({ from: from.toISOString(), to: to.toISOString() })
})

const eventCustomers = ref<Record<string, string>>({})
let customerGeneration = 0

function loadTasks() {
  return canSeeFollowUps.value ? tasks.load() : Promise.resolve()
}

async function loadAppointments() {
  if (!canSeeAppointments.value) return
  await events.load()
  if (events.status.value !== 'ready') return
  const generation = ++customerGeneration
  const visible = upcomingEvents.value
    .slice(0, MAX_ROWS)
    .filter((event) => event.booked_quantity === undefined || event.booked_quantity > 0)
  const results = await Promise.allSettled(
    visible.map((event) => bookingService.allBookings({ event_id: event.id })),
  )
  if (!alive || generation !== customerGeneration) return
  const next: Record<string, string> = {}
  results.forEach((result, index) => {
    const event = visible[index]
    if (!event || result.status !== 'fulfilled') return
    const people = (result.value as Booking[]).filter(
      (booking) => booking && booking.status !== 'cancelled',
    )
    if (!people.length) return
    const first = contactDisplayName(people[0].contact)
    next[event.id] = people.length > 1 ? `${first} + ${people.length - 1} more` : first
  })
  eventCustomers.value = next
}

function loadAll() {
  void loadTasks()
  if (canSeeLeads.value) void openLeads.load()
  if (canSeeWon.value) {
    void wonLeads.load()
    void recentInvoices.load()
  }
  if (canSeeUnpaid.value) void openInvoices.load()
  void loadAppointments()
}

onMounted(loadAll)
onBeforeUnmount(() => {
  alive = false
})

const anyLoading = computed(() =>
  [tasks, openLeads, wonLeads, openInvoices, recentInvoices, events].some((resource) => resource.status.value === 'loading'),
)

// ---- Formatting -------------------------------------------------------------

const todayLabel = new Intl.DateTimeFormat(LOCALE, {
  weekday: 'long',
  day: 'numeric',
  month: 'long',
  year: 'numeric',
}).format(new Date())

function parseTime(value?: string | null): number {
  if (!value) return 0
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? 0 : time
}

function formatTime(value?: string | null): string {
  const time = parseTime(value)
  if (!time) return ''
  return new Intl.DateTimeFormat(LOCALE, { hour: 'numeric', minute: '2-digit' }).format(new Date(time))
}

function formatDay(value?: string | null): string {
  const time = parseTime(value)
  if (!time) return ''
  return new Intl.DateTimeFormat(LOCALE, { day: 'numeric', month: 'short' }).format(new Date(time))
}

function customerName(contact?: Parameters<typeof contactDisplayName>[0]): string | undefined {
  return contact ? contactDisplayName(contact) : undefined
}

function pipelineLink(lead: CRMLead): string {
  return `/crm/pipeline?pipeline=${encodeURIComponent(lead.pipeline_id)}&lead=${encodeURIComponent(lead.id)}`
}

function plural(count: number, one: string, many: string) {
  return `${count} ${count === 1 ? one : many}`
}

// ---- Derived lists ----------------------------------------------------------

const byDueDate = (a: FollowUpTask, b: FollowUpTask) => parseTime(a.due_at) - parseTime(b.due_at)

const overdueTasks = computed(() =>
  tasks.items.value.filter((task) => isFollowUpOverdue(task.due_at)).sort(byDueDate),
)
const todayTasks = computed(() =>
  tasks.items.value.filter((task) => isFollowUpDueToday(task.due_at)).sort(byDueDate),
)

const leadsWithoutFollowUp = computed(() => {
  const followed = new Set(
    tasks.items.value
      .filter((task) => task?.lead_id && (task.status === 'open' || task.status === 'in_progress'))
      .map((task) => task.lead_id as string),
  )
  return openLeads.items.value
    .filter((lead) => lead.status === 'open' && !lead.next_action_at && !followed.has(lead.id))
    .sort(
      (a, b) =>
        parseTime(a.last_activity_at || a.updated_at || a.created_at) -
        parseTime(b.last_activity_at || b.updated_at || b.created_at),
    )
})

const wonNotInvoiced = computed(() => {
  const since = Date.now() - WON_WINDOW_DAYS * DAY_MS
  // A voided invoice bills nothing, so it does not count as invoiced.
  const billed = recentInvoices.items.value.filter((invoice) => invoice.status !== 'void')
  return wonLeads.items.value
    .filter((lead) => lead.status === 'won' && parseTime(lead.won_at) >= since)
    .filter((lead) => !invoiceForLead(billed, lead.id))
    .sort((a, b) => parseTime(b.won_at) - parseTime(a.won_at))
})

const unpaidInvoices = computed(() =>
  openInvoices.items.value
    .filter((invoice) => invoice.status === 'open' && (invoice.due_minor ?? 0) > 0)
    .sort(
      (a, b) =>
        (parseTime(a.due_at) || Number.MAX_SAFE_INTEGER) - (parseTime(b.due_at) || Number.MAX_SAFE_INTEGER) ||
        parseTime(a.issued_at) - parseTime(b.issued_at),
    ),
)

const unpaidTotals = computed(() => {
  const totals = new Map<string, number>()
  for (const invoice of unpaidInvoices.value) {
    const currency = (invoice.currency || 'MYR').trim().toUpperCase()
    totals.set(currency, (totals.get(currency) ?? 0) + invoice.due_minor)
  }
  return [...totals.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([currency, amount]) => formatCurrencyMinorUnits(currency, amount))
})

const upcomingEvents = computed(() =>
  events.items.value
    .filter((event) => event.status !== 'cancelled')
    .sort((a, b) => parseTime(a.starts_at) - parseTime(b.starts_at)),
)

// ---- Invoice dialog ---------------------------------------------------------

const invoiceDialogOpen = ref(false)
const invoiceLead = ref<CRMLead | null>(null)
const invoiceContact = computed(() =>
  invoiceLead.value
    ? { id: invoiceLead.value.contact_id, name: contactDisplayName(invoiceLead.value.contact) }
    : null,
)

function openInvoiceDialog(lead: CRMLead) {
  invoiceLead.value = lead
  invoiceDialogOpen.value = true
}

function invoiceCreated(invoice: CommerceInvoice) {
  const created = invoice as TodayInvoice
  recentInvoices.items.value = [created, ...recentInvoices.items.value.filter((item) => item.id !== created.id)]
  if (canSeeUnpaid.value && openInvoices.status.value === 'ready') {
    const rest = openInvoices.items.value.filter((item) => item.id !== created.id)
    openInvoices.items.value = created.status === 'open' ? [created, ...rest] : rest
  }
}

// ---- Cards ------------------------------------------------------------------

function combinedStatus(...statuses: LoadStatus[]): CardStatus {
  if (statuses.some((status) => status === 'error')) return 'error'
  if (statuses.some((status) => status === 'loading' || status === 'idle')) return 'loading'
  return 'ready'
}

function taskRow(task: FollowUpTask, overdue: boolean): CardRow {
  const customer = customerName(task.contact)
  const row: CardRow = {
    id: task.id,
    title: task.title,
    subtitle: customer,
    meta: overdue ? `Due ${formatDay(task.due_at)}` : formatTime(task.due_at),
    metaTone: overdue ? 'danger' : 'muted',
  }
  if (canOpenChat.value && task.contact_id) {
    row.link = {
      label: 'Open chat',
      ariaLabel: `Open chat with ${customer ?? 'customer'}`,
      to: `/chat/${encodeURIComponent(task.contact_id)}`,
    }
  }
  return row
}

const unreadCard = computed<TodayCard>(() => {
  const count = unreadStore.unreadConversationCount
  const waiting = count === null && unreadStore.loading
  return {
    id: 'unread',
    title: 'Unread conversations',
    icon: Inbox,
    iconClass: 'bg-sky-400/10 text-sky-300 light:bg-sky-50 light:text-sky-700',
    status: waiting ? 'loading' : 'ready',
    count,
    errorText: '',
    emptyText:
      count === null
        ? 'The unread count is not available yet. Open the inbox to check.'
        : count > 0
          ? `${plural(count, 'conversation is', 'conversations are')} waiting for a reply.`
          : 'All caught up. No unread conversations.',
    summary: [],
    groups: [],
    primary: { label: 'Open inbox', to: '/inbox' },
    retry: () => undefined,
  }
})

const followUpsCard = computed<TodayCard>(() => {
  const overdue = overdueTasks.value
  const today = todayTasks.value
  const shownOverdue = overdue.slice(0, MAX_ROWS)
  const shownToday = today.slice(0, Math.max(0, MAX_ROWS - shownOverdue.length))
  const groups: CardGroup[] = []
  if (shownOverdue.length) {
    groups.push({ key: 'overdue', label: `Overdue (${overdue.length})`, rows: shownOverdue.map((task) => taskRow(task, true)) })
  }
  if (shownToday.length) {
    groups.push({ key: 'today', label: `Today (${today.length})`, rows: shownToday.map((task) => taskRow(task, false)) })
  }
  return {
    id: 'follow-ups',
    title: 'Follow-ups due',
    icon: CalendarClock,
    iconClass: 'bg-violet-400/10 text-violet-300 light:bg-violet-50 light:text-violet-700',
    status: combinedStatus(tasks.status.value),
    count: overdue.length + today.length,
    errorText: 'Follow-ups could not be loaded.',
    emptyText: 'Nothing due today and nothing overdue.',
    summary: [`${overdue.length} overdue · ${today.length} due today`],
    groups,
    primary: { label: 'Open follow-ups', to: '/crm/tasks' },
    retry: () => void tasks.load(),
  }
})

const leadsCard = computed<TodayCard>(() => {
  const leadStatus = openLeads.status.value
  const taskStatus = tasks.status.value
  let status: CardStatus = combinedStatus(leadStatus)
  // Wait for the shared follow-up list so a lead is not flagged too early.
  if (status === 'ready' && canSeeFollowUps.value && (taskStatus === 'loading' || taskStatus === 'idle')) {
    status = 'loading'
  }
  let note: string | undefined
  if (!canSeeFollowUps.value) {
    note = 'Only the next step date on each lead is checked, because you cannot see follow-ups.'
  } else if (taskStatus === 'error') {
    note = 'Follow-ups could not be checked, so some of these leads may already have one.'
  }
  const leads = leadsWithoutFollowUp.value
  return {
    id: 'leads',
    title: 'Leads without a follow-up',
    icon: Target,
    iconClass: 'bg-amber-400/10 text-amber-300 light:bg-amber-50 light:text-amber-700',
    status,
    count: leads.length,
    errorText: 'Leads could not be loaded.',
    emptyText: 'Every open lead has a next step.',
    summary: [],
    note,
    groups: [
      {
        key: 'leads',
        rows: leads.slice(0, MAX_ROWS).map((lead) => ({
          id: lead.id,
          title: lead.title || 'Untitled lead',
          subtitle: customerName(lead.contact),
          meta: lead.stage?.name,
          metaTone: 'muted',
          to: canOpenPipeline.value ? pipelineLink(lead) : undefined,
        })),
      },
    ],
    primary: canOpenPipeline.value ? { label: 'Open pipeline', to: '/crm/pipeline' } : undefined,
    retry: () => {
      void openLeads.load()
      if (canSeeFollowUps.value && tasks.status.value === 'error') void tasks.load()
    },
  }
})

const wonCard = computed<TodayCard>(() => {
  const leads = wonNotInvoiced.value
  return {
    id: 'won',
    title: 'Won, not invoiced',
    icon: Trophy,
    iconClass: 'bg-emerald-400/10 text-emerald-300 light:bg-emerald-50 light:text-emerald-700',
    status: combinedStatus(wonLeads.status.value, recentInvoices.status.value),
    count: leads.length,
    errorText: 'Won leads could not be checked.',
    emptyText: `Every lead won in the last ${WON_WINDOW_DAYS} days has an invoice.`,
    summary: [],
    groups: [
      {
        key: 'won',
        rows: leads.slice(0, MAX_ROWS).map((lead) => {
          const customer = customerName(lead.contact)
          const row: CardRow = {
            id: lead.id,
            title: lead.title || 'Untitled lead',
            subtitle: [customer, lead.won_at ? `Won ${formatDay(lead.won_at)}` : '']
              .filter(Boolean)
              .join(' · '),
            meta: lead.value_minor > 0 ? formatCurrencyMinorUnits(lead.currency || 'MYR', lead.value_minor) : undefined,
            metaTone: 'muted',
          }
          if (canInvoice.value) {
            row.action = {
              label: 'Create invoice',
              ariaLabel: `Create invoice for ${lead.title || customer || 'this lead'}`,
              run: () => openInvoiceDialog(lead),
            }
          } else if (canOpenPipeline.value) {
            row.link = {
              label: 'Open in pipeline',
              ariaLabel: `Open ${lead.title || 'lead'} in pipeline`,
              to: pipelineLink(lead),
            }
          }
          return row
        }),
      },
    ],
    primary: canOpenPipeline.value ? { label: 'Open pipeline', to: '/crm/pipeline' } : undefined,
    retry: () => {
      if (wonLeads.status.value === 'error') void wonLeads.load()
      if (recentInvoices.status.value === 'error') void recentInvoices.load()
    },
  }
})

const unpaidCard = computed<TodayCard>(() => {
  const list = unpaidInvoices.value
  const now = Date.now()
  return {
    id: 'unpaid',
    title: 'Unpaid invoices',
    icon: Receipt,
    iconClass: 'bg-rose-400/10 text-rose-300 light:bg-rose-50 light:text-rose-700',
    status: combinedStatus(openInvoices.status.value),
    count: list.length,
    errorText: 'Invoices could not be loaded.',
    emptyText: 'No unpaid invoices.',
    summary: unpaidTotals.value.length ? [`Total unpaid: ${unpaidTotals.value.join(' · ')}`] : [],
    groups: [
      {
        key: 'unpaid',
        rows: list.slice(0, MAX_ROWS).map((invoice) => {
          const overdue = parseTime(invoice.due_at) > 0 && parseTime(invoice.due_at) < now
          const subtitle = [
            customerName(invoice.contact),
            invoice.due_at ? `${overdue ? 'Was due' : 'Due'} ${formatDay(invoice.due_at)}` : '',
          ]
            .filter(Boolean)
            .join(' · ')
          return {
            id: invoice.id,
            title: invoice.invoice_number || 'Invoice',
            subtitle: subtitle || undefined,
            meta: formatCurrencyMinorUnits(invoice.currency || 'MYR', invoice.due_minor),
            metaTone: overdue ? 'danger' : 'muted',
          }
        }),
      },
    ],
    primary: { label: 'Open invoices', to: '/commerce?tab=invoices' },
    retry: () => void openInvoices.load(),
  }
})

const appointmentsCard = computed<TodayCard>(() => {
  const list = upcomingEvents.value
  return {
    id: 'appointments',
    title: 'Appointments today',
    icon: CalendarDays,
    iconClass: 'bg-cyan-400/10 text-cyan-300 light:bg-cyan-50 light:text-cyan-700',
    status: combinedStatus(events.status.value),
    count: list.length,
    errorText: 'Appointments could not be loaded.',
    emptyText: 'No appointments today.',
    summary: [],
    groups: [
      {
        key: 'appointments',
        rows: list.slice(0, MAX_ROWS).map((event) => ({
          id: event.id,
          title: event.service?.name || 'Appointment',
          subtitle: eventCustomers.value[event.id] || event.resource?.name || undefined,
          meta: [formatTime(event.starts_at), formatTime(event.ends_at)].filter(Boolean).join(' – '),
          metaTone: 'muted',
        })),
      },
    ],
    primary: canOpenCalendar.value ? { label: 'Open calendar', to: '/calendar' } : undefined,
    retry: () => void loadAppointments(),
  }
})

const cards = computed<TodayCard[]>(() => {
  const list: TodayCard[] = []
  if (canSeeUnread.value) list.push(unreadCard.value)
  if (canSeeFollowUps.value) list.push(followUpsCard.value)
  if (canSeeLeads.value) list.push(leadsCard.value)
  if (canSeeWon.value) list.push(wonCard.value)
  if (canSeeUnpaid.value) list.push(unpaidCard.value)
  if (canSeeAppointments.value) list.push(appointmentsCard.value)
  return list
})

function hasRows(card: TodayCard) {
  return card.groups.some((group) => group.rows.length > 0)
}
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#090b0e] light:bg-[#f5f6f3]">
    <PageHeader
      title="Today"
      description="What needs your attention right now."
      :icon="Sun"
      icon-gradient="bg-gradient-to-br from-amber-400 to-orange-500 shadow-amber-500/20"
    >
      <template #actions>
        <div class="flex items-center gap-3">
          <p data-testid="today-date" class="text-sm text-white/55 light:text-gray-600">{{ todayLabel }}</p>
          <Button
            variant="outline"
            size="sm"
            class="gap-2"
            :disabled="anyLoading"
            aria-label="Refresh today"
            @click="loadAll"
          >
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': anyLoading }" />
            Refresh
          </Button>
        </div>
      </template>
    </PageHeader>

    <main class="mx-auto max-w-[1480px] p-4 sm:p-5 md:p-7">
      <p class="mb-5 text-sm text-white/50 light:text-gray-600 sm:hidden">What needs your attention right now.</p>

      <div
        v-if="!cards.length"
        data-testid="today-no-cards"
        class="rounded-2xl border border-white/[0.08] bg-white/[0.03] p-8 text-center text-sm text-white/50 light:border-black/10 light:bg-white light:text-gray-600"
      >
        There is nothing here for your role yet. Ask an admin if you think you should see more.
      </div>

      <div v-else class="grid gap-5 md:grid-cols-2 2xl:grid-cols-3">
        <section
          v-for="card in cards"
          :key="card.id"
          :data-testid="`today-card-${card.id}`"
          :aria-labelledby="`today-card-${card.id}-title`"
          :aria-busy="card.status === 'loading'"
          class="flex min-w-0 flex-col overflow-hidden rounded-2xl border border-white/[0.08] bg-[#111419] light:border-black/10 light:bg-white"
        >
          <header class="flex items-start justify-between gap-3 px-5 pt-5">
            <div class="flex min-w-0 items-center gap-3">
              <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl" :class="card.iconClass">
                <component :is="card.icon" class="h-4 w-4" aria-hidden="true" />
              </span>
              <h2 :id="`today-card-${card.id}-title`" class="truncate font-semibold text-white light:text-gray-900">
                {{ card.title }}
              </h2>
            </div>
            <p
              v-if="card.status === 'ready'"
              data-testid="today-card-count"
              class="text-3xl font-semibold leading-none tracking-tight text-white light:text-gray-900"
            >
              {{ card.count ?? '–' }}
            </p>
          </header>

          <div class="flex-1 px-5 py-4">
            <div
              v-if="card.status === 'loading'"
              role="status"
              class="flex items-center gap-2 py-6 text-sm text-white/45 light:text-gray-500"
            >
              <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />
              Loading…
            </div>

            <div
              v-else-if="card.status === 'error'"
              data-testid="today-card-error"
              class="flex flex-col items-start gap-3 py-4"
            >
              <p role="alert" class="flex items-center gap-2 text-sm text-rose-300 light:text-rose-700">
                <AlertCircle class="h-4 w-4 shrink-0" aria-hidden="true" />
                {{ card.errorText }}
              </p>
              <Button variant="outline" size="sm" class="gap-2" @click="card.retry()">
                <RefreshCw class="h-3.5 w-3.5" aria-hidden="true" />
                Try again
              </Button>
            </div>

            <template v-else>
              <p
                v-for="line in card.summary"
                :key="line"
                data-testid="today-card-summary"
                class="mb-3 text-sm font-medium text-white/70 light:text-gray-700"
              >
                {{ line }}
              </p>
              <p v-if="card.note" data-testid="today-card-note" class="mb-3 text-xs text-amber-200/80 light:text-amber-800">
                {{ card.note }}
              </p>
              <p
                v-if="!hasRows(card)"
                data-testid="today-card-empty"
                class="py-2 text-sm text-white/45 light:text-gray-500"
              >
                {{ card.emptyText }}
              </p>
              <template v-else>
                <div v-for="group in card.groups" :key="group.key" class="mb-3 last:mb-0">
                  <p
                    v-if="group.label && group.rows.length"
                    :data-testid="`today-group-${group.key}`"
                    class="mb-1 text-[10px] font-semibold uppercase tracking-[0.18em]"
                    :class="group.key === 'overdue' ? 'text-rose-300 light:text-rose-700' : 'text-white/40 light:text-gray-500'"
                  >
                    {{ group.label }}
                  </p>
                  <ul v-if="group.rows.length" class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
                    <li
                      v-for="row in group.rows"
                      :key="row.id"
                      data-testid="today-row"
                      class="flex items-center justify-between gap-3 py-2.5"
                    >
                      <div class="min-w-0">
                        <RouterLink
                          v-if="row.to"
                          :to="row.to"
                          class="block truncate text-sm font-medium text-white hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-gray-900"
                        >
                          {{ row.title }}
                        </RouterLink>
                        <p v-else class="truncate text-sm font-medium text-white light:text-gray-900">{{ row.title }}</p>
                        <p v-if="row.subtitle" class="truncate text-xs text-white/45 light:text-gray-500">{{ row.subtitle }}</p>
                      </div>
                      <div class="flex shrink-0 items-center gap-2">
                        <span
                          v-if="row.meta"
                          class="text-xs tabular-nums"
                          :class="row.metaTone === 'danger' ? 'text-rose-300 light:text-rose-700' : 'text-white/55 light:text-gray-600'"
                        >
                          {{ row.meta }}
                        </span>
                        <RouterLink
                          v-if="row.link"
                          :to="row.link.to"
                          :aria-label="row.link.ariaLabel"
                          class="rounded-md px-2 py-1 text-xs font-medium text-cyan-300 hover:bg-white/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-700 light:hover:bg-cyan-50"
                        >
                          {{ row.link.label }}
                        </RouterLink>
                        <Button
                          v-if="row.action"
                          size="sm"
                          variant="outline"
                          :aria-label="row.action.ariaLabel"
                          @click="row.action.run()"
                        >
                          {{ row.action.label }}
                        </Button>
                      </div>
                    </li>
                  </ul>
                </div>
              </template>
            </template>
          </div>

          <footer v-if="card.primary" class="border-t border-white/[0.07] px-5 py-3 light:border-black/10">
            <RouterLink
              :to="card.primary.to"
              :data-testid="`today-card-${card.id}-link`"
              class="inline-flex items-center gap-1.5 rounded-md text-sm font-medium text-cyan-300 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-700"
            >
              {{ card.primary.label }}
              <ArrowRight class="h-3.5 w-3.5" aria-hidden="true" />
            </RouterLink>
          </footer>
        </section>
      </div>
    </main>

    <InvoiceQuickDialog
      v-if="canInvoice"
      v-model:open="invoiceDialogOpen"
      :contact="invoiceContact"
      :lead="invoiceLead"
      :can-sell-packages="canSellPackages"
      source="today"
      @created="invoiceCreated"
    />
  </div>
</template>
