<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  Banknote,
  CheckCircle2,
  CircleDollarSign,
  Loader2,
  Package,
  Plus,
  Receipt,
  RefreshCw,
  ShieldCheck,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import ContactPicker from '@/components/shared/ContactPicker.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import {
  bookingService,
  commerceService,
  type BookingService,
  type CommerceInvoice,
  type CommerceSummary,
  type ContactPackage,
  type PackageDefinition,
} from '@/services/productSuite'

interface PaymentRecord {
  id: string
  type: 'charge' | 'refund' | 'fee' | 'adjustment'
  status: 'pending' | 'succeeded' | 'failed' | 'reversed'
  amount_minor: number
  currency: string
  occurred_at: string
  created_at: string
  provider_account?: {
    name: string
    provider: string
  }
}

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const saving = ref(false)
const loadingMore = ref(false)
const tab = ref<'packages' | 'customer-plans' | 'invoices' | 'payments'>('packages')
const packages = ref<PackageDefinition[]>([])
const contactPackages = ref<ContactPackage[]>([])
const bookingServices = ref<BookingService[]>([])
const invoices = ref<CommerceInvoice[]>([])
const payments = ref<PaymentRecord[]>([])
const summary = ref<CommerceSummary | null>(null)
const contactPackagePage = ref(1)
const contactPackageTotal = ref(0)
const invoicePage = ref(1)
const invoiceTotal = ref(0)
const paymentPage = ref(1)
const paymentTotal = ref(0)
const draft = ref({
  name: '',
  description: '',
  price: '',
  validity_days: 30,
  currency: 'MYR',
  booking_service_id: '',
  credits: 5,
  is_unlimited: false,
})
const invoiceDraft = ref({
  contact_id: '',
  description: '',
  quantity: 1,
  unit_price: '',
  tax: '0',
  discount: '0',
  currency: 'MYR',
  due_date: '',
  idempotency_key: crypto.randomUUID(),
})
const manualPaymentInvoice = ref<CommerceInvoice | null>(null)
const manualPaymentDraft = ref({
  amount: '',
  reference: '',
  notes: '',
  confirm_manual: false,
  idempotency_key: '',
})
const packageSaleDraft = ref({
  contact_id: '',
  package_definition_id: '',
  mode: 'invoice' as 'invoice' | 'grant',
  due_date: '',
  idempotency_key: crypto.randomUUID(),
})
const canReadPayments = computed(() => authStore.hasPermission('payments', 'read'))
const canWritePayments = computed(() => authStore.hasPermission('payments', 'write'))
const canReadPackages = computed(() => authStore.hasPermission('packages', 'read'))
const canWritePackages = computed(() => authStore.hasPermission('packages', 'write'))
const canReadBookingSettings = computed(() => authStore.hasPermission('booking.settings', 'read'))
const canReadContacts = computed(() => authStore.hasPermission('contacts', 'read'))
const canCreatePackages = computed(() => canWritePackages.value && canReadBookingSettings.value)
const canIssueInvoices = computed(() => canWritePayments.value && canReadContacts.value)
const canAssignPackages = computed(() => canWritePackages.value && canReadContacts.value)
const canSellPackages = computed(() => canAssignPackages.value && canWritePayments.value)
const showCommerceAside = computed(
  () =>
    (tab.value === 'packages' && (canCreatePackages.value || canWritePackages.value)) ||
    (tab.value === 'customer-plans' && (canAssignPackages.value || canWritePackages.value)) ||
    (tab.value === 'invoices' && canWritePayments.value),
)
const commerceTabs = computed(() => [
  ...(canReadPackages.value
    ? [
        { key: 'packages' as const, label: 'Packages', icon: Package },
        { key: 'customer-plans' as const, label: 'Customer plans', icon: ShieldCheck },
      ]
    : []),
  ...(canReadPayments.value
    ? [
        { key: 'invoices' as const, label: 'Invoices', icon: Receipt },
        { key: 'payments' as const, label: 'Payments', icon: Banknote },
      ]
    : []),
])

const activePackages = computed(() =>
  summary.value?.packages_visible
    ? summary.value.active_packages
    : packages.value.filter((item) => item.is_active).length,
)
const outstandingValues = computed(() =>
  summary.value
    ? summary.value.outstanding.map(
        (item) => [item.currency, item.amount_minor] as [string, number],
      )
    : currencyTotals(
        invoices.value.map((invoice) => ({
          amount_minor: Math.max(0, invoice.due_minor || 0),
          currency: invoice.currency,
        })),
      ),
)
const collectedValues = computed(() =>
  summary.value
    ? summary.value.collected_charges.map(
        (item) => [item.currency, item.amount_minor] as [string, number],
      )
    : currencyTotals(
        payments.value.filter(
          (payment) => payment.status === 'succeeded' && payment.type === 'charge',
        ),
      ),
)

function money(amountMinor: number, currency = 'MYR') {
  return new Intl.NumberFormat('en-MY', {
    style: 'currency',
    currency,
  }).format((amountMinor || 0) / 100)
}

function currencyTotals(items: Array<{ amount_minor: number; currency: string }>) {
  const totals = new Map<string, number>()
  for (const item of items) {
    totals.set(item.currency, (totals.get(item.currency) ?? 0) + item.amount_minor)
  }
  return [...totals.entries()].sort(([left], [right]) => left.localeCompare(right))
}

function moneyTotals(items: Array<[string, number]>) {
  if (!items.length) return '—'
  return items.map(([currency, amount]) => money(amount, currency)).join(' · ')
}

function date(value?: string) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short', year: 'numeric' }).format(new Date(value))
}

function totalFromResponse(response: any) {
  const payload = response?.data?.data ?? response?.data
  return typeof payload?.total === 'number' ? payload.total : 0
}

function paymentProvider(payment: PaymentRecord) {
  return payment.provider_account?.name || payment.provider_account?.provider || 'Payment ledger'
}

function paymentTone(payment: PaymentRecord) {
  if (payment.status === 'succeeded') {
    return {
      icon: 'bg-emerald-400/10 text-emerald-300',
      status: 'text-emerald-300',
    }
  }
  if (payment.status === 'pending') {
    return {
      icon: 'bg-amber-300/10 text-amber-200',
      status: 'text-amber-200',
    }
  }
  return {
    icon: 'bg-red-400/10 text-red-300',
    status: 'text-red-300',
  }
}

function paymentAmount(payment: PaymentRecord) {
  const formatted = money(payment.amount_minor, payment.currency)
  if (payment.type === 'refund' || payment.type === 'fee') return `−${formatted}`
  if (payment.type === 'adjustment') return `±${formatted}`
  return formatted
}

async function load() {
  loading.value = true
  try {
    const [packageResponse, contactPackageResponse, bookingServiceResponse, invoiceResponse, paymentResponse, summaryResponse] = await Promise.all([
      canReadPackages.value
        ? commerceService.allPackages()
        : Promise.resolve(null),
      canReadPackages.value
        ? commerceService.contactPackages({ limit: 100 })
        : Promise.resolve(null),
      canCreatePackages.value
        ? bookingService.allServices()
        : Promise.resolve(null),
      canReadPayments.value
        ? commerceService.invoices({ limit: 100 })
        : Promise.resolve(null),
      canReadPayments.value
        ? commerceService.payments({ limit: 100 })
        : Promise.resolve(null),
      canReadPayments.value
        ? commerceService.summary()
        : Promise.resolve(null),
    ])
    packages.value = packageResponse ?? []
    contactPackages.value = contactPackageResponse
      ? unwrapListResponse<ContactPackage>(contactPackageResponse, 'contact_packages')
      : []
    contactPackageTotal.value = contactPackageResponse ? totalFromResponse(contactPackageResponse) : 0
    contactPackagePage.value = 1
    bookingServices.value = bookingServiceResponse?.filter((item) => item.is_active) ?? []
    if (!draft.value.booking_service_id) {
      draft.value.booking_service_id = bookingServices.value[0]?.id ?? ''
    }
    if (
      !packageSaleDraft.value.package_definition_id ||
      !packages.value.some((item) => item.id === packageSaleDraft.value.package_definition_id && item.is_active)
    ) {
      packageSaleDraft.value.package_definition_id = packages.value.find((item) => item.is_active)?.id ?? ''
    }
    invoices.value = invoiceResponse
      ? unwrapListResponse<CommerceInvoice>(invoiceResponse, 'invoices')
      : []
    invoiceTotal.value = invoiceResponse ? totalFromResponse(invoiceResponse) : 0
    invoicePage.value = 1
    payments.value = paymentResponse
      ? unwrapListResponse<PaymentRecord>(paymentResponse, 'payments')
      : []
    paymentTotal.value = paymentResponse ? totalFromResponse(paymentResponse) : 0
    paymentPage.value = 1
    summary.value = summaryResponse ? unwrapItemResponse<CommerceSummary>(summaryResponse) : null
  } catch (error) {
    toast.error('Commerce desk could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function loadMoreCommerce(kind: 'contact-packages' | 'invoices' | 'payments') {
  if (loadingMore.value) return
  loadingMore.value = true
  try {
    if (kind === 'contact-packages') {
      const nextPage = contactPackagePage.value + 1
      const response = await commerceService.contactPackages({ page: nextPage, limit: 100 })
      const incoming = unwrapListResponse<ContactPackage>(response, 'contact_packages')
      const existing = new Set(contactPackages.value.map((item) => item.id))
      contactPackages.value.push(...incoming.filter((item) => !existing.has(item.id)))
      contactPackagePage.value = nextPage
      contactPackageTotal.value = totalFromResponse(response)
      return
    }
    if (kind === 'invoices') {
      const nextPage = invoicePage.value + 1
      const response = await commerceService.invoices({ page: nextPage, limit: 100 })
      const incoming = unwrapListResponse<CommerceInvoice>(response, 'invoices')
      const existing = new Set(invoices.value.map((item) => item.id))
      invoices.value.push(...incoming.filter((item) => !existing.has(item.id)))
      invoicePage.value = nextPage
      invoiceTotal.value = totalFromResponse(response)
      return
    }
    const nextPage = paymentPage.value + 1
    const response = await commerceService.payments({ page: nextPage, limit: 100 })
    const incoming = unwrapListResponse<PaymentRecord>(response, 'payments')
    const existing = new Set(payments.value.map((item) => item.id))
    payments.value.push(...incoming.filter((item) => !existing.has(item.id)))
    paymentPage.value = nextPage
    paymentTotal.value = totalFromResponse(response)
  } catch (error) {
    toast.error('Older commerce records could not be loaded', getErrorMessage(error))
  } finally {
    loadingMore.value = false
  }
}

async function sellOrGrantPackage() {
  const definition = packages.value.find((item) => item.id === packageSaleDraft.value.package_definition_id)
  if (!definition || !packageSaleDraft.value.contact_id || !canAssignPackages.value) {
    toast.warning('Choose an active package and customer')
    return
  }
  if (packageSaleDraft.value.mode === 'invoice' && !canSellPackages.value) {
    toast.warning('Payment write access is required to sell a package by invoice')
    return
  }

  saving.value = true
  try {
    const saleMode = packageSaleDraft.value.mode
    if (saleMode === 'invoice') {
      await commerceService.sellPackage({
        contact_id: packageSaleDraft.value.contact_id,
        package_definition_id: definition.id,
        due_at: packageSaleDraft.value.due_date
          ? new Date(`${packageSaleDraft.value.due_date}T23:59:59`).toISOString()
          : undefined,
        idempotency_key: packageSaleDraft.value.idempotency_key,
      })
    } else {
      await commerceService.createContactPackage({
        contact_id: packageSaleDraft.value.contact_id,
        package_definition_id: definition.id,
        source: 'complimentary_grant',
        idempotency_key: packageSaleDraft.value.idempotency_key,
      })
    }
    packageSaleDraft.value = {
      contact_id: '',
      package_definition_id: definition.id,
      mode: 'invoice',
      due_date: '',
      idempotency_key: crypto.randomUUID(),
    }
    toast.success(
      saleMode === 'invoice' ? 'Package sale created' : 'Package granted',
      saleMode === 'invoice'
        ? 'Invoice and customer plan were committed together. Credits activate automatically after payment.'
        : 'Credits are active now.',
    )
    await load()
  } catch (error) {
    toast.error('Customer package was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function createPackage() {
  if (
    !draft.value.name.trim() ||
    !draft.value.price ||
    !draft.value.booking_service_id ||
    (!draft.value.is_unlimited && draft.value.credits < 1)
  ) {
    toast.warning('Name, price, service and a valid credit allowance are required')
    return
  }
  saving.value = true
  try {
    await commerceService.createPackage({
      name: draft.value.name.trim(),
      description: draft.value.description.trim(),
      price_minor: Math.round(Number(draft.value.price) * 100),
      currency: draft.value.currency,
      validity_days: draft.value.validity_days,
      is_active: true,
      entitlements: [
        {
          booking_service_id: draft.value.booking_service_id,
          credits: draft.value.is_unlimited ? 0 : draft.value.credits,
          is_unlimited: draft.value.is_unlimited,
        },
      ],
    })
    draft.value = {
      name: '',
      description: '',
      price: '',
      validity_days: 30,
      currency: 'MYR',
      booking_service_id: bookingServices.value[0]?.id ?? '',
      credits: 5,
      is_unlimited: false,
    }
    toast.success('Package created')
    await load()
  } catch (error) {
    toast.error('Package was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function createInvoice() {
  if (!canIssueInvoices.value) {
    toast.warning('Contact read access is required to issue a customer invoice')
    return
  }
  const quantity = Number(invoiceDraft.value.quantity)
  const unitAmountMinor = Math.round(Number(invoiceDraft.value.unit_price) * 100)
  const taxMinor = Math.round(Number(invoiceDraft.value.tax || 0) * 100)
  const discountMinor = Math.round(Number(invoiceDraft.value.discount || 0) * 100)
  if (
    !invoiceDraft.value.contact_id ||
    !invoiceDraft.value.description.trim() ||
    !Number.isInteger(quantity) ||
    quantity < 1 ||
    !Number.isFinite(unitAmountMinor) ||
    unitAmountMinor < 0 ||
    taxMinor < 0 ||
    discountMinor < 0
  ) {
    toast.warning('Customer, line description, quantity and valid amounts are required')
    return
  }

  saving.value = true
  try {
    await commerceService.createInvoice({
      contact_id: invoiceDraft.value.contact_id,
      currency: invoiceDraft.value.currency,
      discount_minor: discountMinor,
      due_at: invoiceDraft.value.due_date
        ? new Date(`${invoiceDraft.value.due_date}T23:59:59`).toISOString()
        : undefined,
      idempotency_key: invoiceDraft.value.idempotency_key,
      lines: [
        {
          description: invoiceDraft.value.description.trim(),
          quantity,
          unit_amount_minor: unitAmountMinor,
          tax_minor: taxMinor,
        },
      ],
    })
    invoiceDraft.value = {
      contact_id: '',
      description: '',
      quantity: 1,
      unit_price: '',
      tax: '0',
      discount: '0',
      currency: 'MYR',
      due_date: '',
      idempotency_key: crypto.randomUUID(),
    }
    toast.success('Invoice created')
    await load()
  } catch (error) {
    toast.error('Invoice was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

function startManualPayment(invoice: CommerceInvoice) {
  manualPaymentInvoice.value = invoice
  manualPaymentDraft.value = {
    amount: (invoice.due_minor / 100).toFixed(2),
    reference: '',
    notes: '',
    confirm_manual: false,
    idempotency_key: crypto.randomUUID(),
  }
}

async function recordManualPayment() {
  const invoice = manualPaymentInvoice.value
  const amountMinor = Math.round(Number(manualPaymentDraft.value.amount) * 100)
  if (
    !invoice ||
    !Number.isFinite(amountMinor) ||
    amountMinor < 1 ||
    amountMinor > invoice.due_minor ||
    !manualPaymentDraft.value.reference.trim() ||
    !manualPaymentDraft.value.confirm_manual
  ) {
    toast.warning('Confirm a positive amount and enter a unique bank or receipt reference')
    return
  }

  saving.value = true
  try {
    await commerceService.recordManualPayment(invoice.id, {
      version: invoice.version,
      amount_minor: amountMinor,
      currency: invoice.currency,
      reference: manualPaymentDraft.value.reference.trim(),
      notes: manualPaymentDraft.value.notes.trim(),
      idempotency_key: manualPaymentDraft.value.idempotency_key,
      confirm_manual: true,
    })
    manualPaymentInvoice.value = null
    toast.success('Manual payment recorded', 'The invoice ledger and balance were updated together.')
    await load()
  } catch (error) {
    toast.error('Payment was not recorded', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  if (!canReadPackages.value && canReadPayments.value) tab.value = 'invoices'
  void load()
})
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#0a0c0b] light:bg-[#f4f5f1]">
    <PageHeader
      title="Revenue desk"
      description="Packages, invoices and provider-neutral payment records without spreadsheet drift."
      :icon="CircleDollarSign"
      icon-gradient="bg-gradient-to-br from-emerald-400 to-teal-700 shadow-emerald-500/20"
    >
      <template #actions>
        <Button variant="outline" size="sm" class="gap-2" :disabled="loading" @click="load">
          <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
          Refresh
        </Button>
      </template>
    </PageHeader>

    <main class="mx-auto max-w-[1500px] space-y-6 p-5 md:p-7">
      <section class="relative overflow-hidden rounded-[28px] border border-emerald-300/15 bg-[#10201a] px-6 py-6 light:border-emerald-200 light:bg-[#123e32]">
        <div class="absolute -right-16 -top-24 h-64 w-64 rounded-full bg-emerald-300/15 blur-3xl" />
        <div class="relative grid gap-5 md:grid-cols-[1.2fr_repeat(3,.6fr)] md:items-end">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.25em] text-emerald-200/70">Commercial pulse</p>
            <h2 class="mt-2 max-w-xl text-2xl font-semibold tracking-tight text-white md:text-3xl">
              Sell care plans with a ledger your team can trust.
            </h2>
            <p class="mt-2 max-w-lg text-sm leading-6 text-emerald-50/50">
              Money is stored in minor units, payment retries are idempotent, and every organization keeps its own ledger.
            </p>
          </div>
          <div v-for="metric in [
            { label: 'Active packages', value: activePackages, text: '' },
            { label: 'Tenant outstanding', value: moneyTotals(outstandingValues), text: '' },
            { label: 'Tenant collected charges', value: moneyTotals(collectedValues), text: '' },
          ]" :key="metric.label" class="rounded-2xl border border-white/10 bg-black/15 p-4">
            <p class="text-[10px] uppercase tracking-[0.17em] text-white/40">{{ metric.label }}</p>
            <p class="mt-2 text-xl font-semibold text-white">{{ metric.value }}</p>
          </div>
        </div>
      </section>

      <div
        class="grid gap-6"
        :class="showCommerceAside ? 'xl:grid-cols-[minmax(0,1fr)_390px]' : 'xl:grid-cols-1'"
      >
        <section class="min-w-0 overflow-hidden rounded-2xl border border-white/[0.08] bg-white/[0.025] light:border-black/10 light:bg-white">
          <div class="flex flex-wrap items-center justify-between gap-3 border-b border-white/[0.07] px-5 py-4 light:border-black/10">
            <div class="flex rounded-lg bg-black/20 p-1 light:bg-gray-100">
              <button
                v-for="item in commerceTabs"
                :key="item.key"
                class="flex items-center gap-2 rounded-md px-3 py-2 text-xs font-medium transition"
                :class="tab === item.key ? 'bg-white text-gray-950 shadow-sm' : 'text-white/45 hover:text-white light:text-gray-500'"
                @click="tab = item.key as typeof tab"
              >
                <component :is="item.icon" class="h-3.5 w-3.5" />
                {{ item.label }}
              </button>
            </div>
            <div class="flex items-center gap-2 text-[11px] text-white/35 light:text-gray-500">
              <ShieldCheck class="h-3.5 w-3.5 text-emerald-300" />
              Tenant-isolated ledger
            </div>
          </div>

          <div v-if="loading" class="flex h-80 items-center justify-center">
            <Loader2 class="h-6 w-6 animate-spin text-emerald-300" />
          </div>

          <div v-else-if="tab === 'packages'" class="grid gap-4 p-5 md:grid-cols-2">
            <article
              v-for="item in packages"
              :key="item.id"
              class="rounded-2xl border border-white/[0.08] bg-gradient-to-br from-white/[0.04] to-transparent p-5 light:border-black/10 light:from-gray-50"
            >
              <div class="flex items-start justify-between gap-3">
                <div>
                  <p class="font-semibold text-white light:text-gray-900">{{ item.name }}</p>
                  <p class="mt-1 line-clamp-2 text-xs leading-5 text-white/40 light:text-gray-500">{{ item.description || 'No description' }}</p>
                </div>
                <Badge variant="outline" :class="item.is_active ? 'border-emerald-400/25 text-emerald-300' : ''">
                  {{ item.is_active ? 'Active' : 'Inactive' }}
                </Badge>
              </div>
              <p class="mt-6 text-2xl font-semibold tracking-tight text-white light:text-gray-950">
                {{ money(item.price_minor, item.currency) }}
              </p>
              <div class="mt-3 flex items-center justify-between text-[11px] text-white/35 light:text-gray-500">
                <span>{{ item.validity_days }} days validity</span>
                <span>{{ item.entitlements?.length || 0 }} credit rules</span>
              </div>
            </article>
            <div v-if="!packages.length" class="col-span-full flex h-64 flex-col items-center justify-center text-center">
              <Package class="h-8 w-8 text-white/20 light:text-gray-300" />
              <p class="mt-3 text-sm font-medium text-white light:text-gray-900">No packages yet</p>
              <p class="mt-1 text-xs text-white/40 light:text-gray-500">Create your first sellable care plan from the panel.</p>
            </div>
          </div>

          <div v-else-if="tab === 'customer-plans'" class="grid gap-4 p-5 md:grid-cols-2">
            <article
              v-for="item in contactPackages"
              :key="item.id"
              class="rounded-2xl border border-white/[0.08] bg-gradient-to-br from-white/[0.04] to-transparent p-5 light:border-black/10 light:from-gray-50"
            >
              <div class="flex items-start justify-between gap-3">
                <div>
                  <p class="font-semibold text-white light:text-gray-900">
                    {{ item.contact?.profile_name || item.contact?.phone_number || 'Customer' }}
                  </p>
                  <p class="mt-1 text-xs text-white/40 light:text-gray-500">
                    {{ item.package_definition?.name || 'Package' }}
                  </p>
                </div>
                <Badge variant="outline" class="capitalize">{{ item.status }}</Badge>
              </div>
              <div class="mt-4 grid grid-cols-2 gap-3 text-xs">
                <div class="rounded-xl bg-black/10 p-3 light:bg-white">
                  <p class="text-white/35 light:text-gray-500">Available credits</p>
                  <p class="mt-1 text-lg font-semibold text-white light:text-gray-900">
                    {{ item.balances?.reduce((sum, balance) => sum + Math.max(0, balance.available), 0) ?? 0 }}
                  </p>
                </div>
                <div class="rounded-xl bg-black/10 p-3 light:bg-white">
                  <p class="text-white/35 light:text-gray-500">Expires</p>
                  <p class="mt-1 font-medium text-white light:text-gray-900">{{ date(item.expires_at) }}</p>
                </div>
              </div>
              <p class="mt-3 text-[10px] text-white/30 light:text-gray-400">
                {{ item.invoice_id ? 'Invoice-backed sale' : 'Complimentary / manual grant' }}
              </p>
            </article>
            <div v-if="!contactPackages.length" class="col-span-full flex h-64 flex-col items-center justify-center text-center">
              <ShieldCheck class="h-8 w-8 text-white/20 light:text-gray-300" />
              <p class="mt-3 text-sm font-medium text-white light:text-gray-900">No customer plans yet</p>
              <p class="mt-1 text-xs text-white/40 light:text-gray-500">Sell or grant an active package from the panel.</p>
            </div>
            <div v-if="contactPackages.length < contactPackageTotal" class="col-span-full text-center">
              <Button variant="outline" :disabled="loadingMore" @click="loadMoreCommerce('contact-packages')">
                <Loader2 v-if="loadingMore" class="mr-2 h-4 w-4 animate-spin" />
                Load older customer plans
              </Button>
            </div>
          </div>

          <div v-else-if="tab === 'invoices'" class="overflow-x-auto">
            <table class="w-full min-w-[720px] text-left">
              <thead class="border-b border-white/[0.07] text-[10px] uppercase tracking-[0.18em] text-white/35 light:border-black/10 light:text-gray-500">
                <tr>
                  <th class="px-5 py-3 font-medium">Invoice</th>
                  <th class="px-5 py-3 font-medium">Issued</th>
                  <th class="px-5 py-3 font-medium">Total</th>
                  <th class="px-5 py-3 font-medium">Due</th>
                  <th class="px-5 py-3 font-medium">Status</th>
                  <th v-if="canWritePayments" class="px-5 py-3 font-medium">Action</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
                <tr v-for="invoice in invoices" :key="invoice.id" class="text-sm">
                  <td class="px-5 py-4 font-medium text-white light:text-gray-900">{{ invoice.invoice_number }}</td>
                  <td class="px-5 py-4 text-white/45 light:text-gray-500">{{ date(invoice.issued_at) }}</td>
                  <td class="px-5 py-4 text-white light:text-gray-900">{{ money(invoice.total_minor, invoice.currency) }}</td>
                  <td class="px-5 py-4 text-white/55 light:text-gray-600">{{ money(invoice.due_minor, invoice.currency) }}</td>
                  <td class="px-5 py-4"><Badge variant="outline" class="capitalize">{{ invoice.status }}</Badge></td>
                  <td v-if="canWritePayments" class="px-5 py-4">
                    <Button
                      v-if="invoice.status === 'open' && invoice.due_minor > 0"
                      variant="outline"
                      size="sm"
                      @click="startManualPayment(invoice)"
                    >
                      Record payment
                    </Button>
                    <span v-else class="text-xs text-white/25 light:text-gray-400">—</span>
                  </td>
                </tr>
              </tbody>
            </table>
            <p v-if="!invoices.length" class="py-24 text-center text-sm text-white/35 light:text-gray-500">No invoices recorded.</p>
            <div v-if="invoices.length < invoiceTotal" class="p-4 text-center">
              <Button variant="outline" :disabled="loadingMore" @click="loadMoreCommerce('invoices')">
                <Loader2 v-if="loadingMore" class="mr-2 h-4 w-4 animate-spin" />
                Load older invoices
              </Button>
            </div>
          </div>

          <div v-else class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
            <article v-for="payment in payments" :key="payment.id" class="flex items-center gap-4 px-5 py-4">
              <div class="rounded-xl p-2.5" :class="paymentTone(payment).icon">
                <CheckCircle2 v-if="payment.status === 'succeeded'" class="h-5 w-5" />
                <RefreshCw v-else class="h-5 w-5" />
              </div>
              <div class="min-w-0 flex-1">
                <p class="text-sm font-medium text-white light:text-gray-900">{{ paymentProvider(payment) }}</p>
                <p class="mt-0.5 text-xs capitalize text-white/35 light:text-gray-500">
                  {{ payment.type }} · {{ date(payment.occurred_at) }}
                </p>
              </div>
              <div class="text-right">
                <p class="text-sm font-semibold text-white light:text-gray-900">{{ paymentAmount(payment) }}</p>
                <p class="mt-0.5 text-[10px] uppercase tracking-wider" :class="paymentTone(payment).status">
                  {{ payment.status }}
                </p>
              </div>
            </article>
            <p v-if="!payments.length" class="py-24 text-center text-sm text-white/35 light:text-gray-500">No payments recorded.</p>
            <div v-if="payments.length < paymentTotal" class="p-4 text-center">
              <Button variant="outline" :disabled="loadingMore" @click="loadMoreCommerce('payments')">
                <Loader2 v-if="loadingMore" class="mr-2 h-4 w-4 animate-spin" />
                Load older ledger entries
              </Button>
            </div>
          </div>
        </section>

        <aside v-if="tab === 'packages' && canCreatePackages" class="h-fit rounded-[24px] border border-emerald-300/15 bg-emerald-300/[0.045] p-5 light:border-emerald-200 light:bg-emerald-50">
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300 light:text-emerald-700">New offering</p>
          <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Create a package</h3>
          <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">
            Every package starts with an explicit service-credit rule so it cannot be sold without a deliverable.
          </p>
          <form class="mt-6 space-y-4" @submit.prevent="createPackage">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Package name</span>
              <input v-model="draft.name" required maxlength="255" placeholder="Pilates starter · 5 sessions" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none placeholder:text-white/25 focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Description</span>
              <textarea v-model="draft.description" rows="3" maxlength="2000" placeholder="Who this is for and what it includes…" class="w-full resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none placeholder:text-white/25 focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <div class="grid grid-cols-[1fr_90px] gap-3">
              <label>
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Price</span>
                <input v-model="draft.price" required min="0" step="0.01" type="number" placeholder="399.00" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <label>
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Currency</span>
                <select v-model="draft.currency" class="h-11 w-full rounded-xl border border-white/10 bg-[#15201c] px-2 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900">
                  <option value="MYR">MYR</option>
                  <option value="SGD">SGD</option>
                  <option value="USD">USD</option>
                </select>
              </label>
            </div>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Validity (days)</span>
              <input v-model.number="draft.validity_days" required min="1" max="3650" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Included service</span>
              <select v-model="draft.booking_service_id" required class="h-11 w-full rounded-xl border border-white/10 bg-[#15201c] px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="" disabled>Select a service</option>
                <option v-for="service in bookingServices" :key="service.id" :value="service.id">
                  {{ service.name }}
                </option>
              </select>
            </label>
            <div class="grid grid-cols-[1fr_auto] items-end gap-3">
              <label>
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Credits</span>
                <input v-model.number="draft.credits" :disabled="draft.is_unlimited" required min="1" max="100000" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none disabled:opacity-45 focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <label class="flex h-11 items-center gap-2 rounded-xl border border-white/10 bg-black/15 px-3 text-xs text-white/65 light:border-gray-200 light:bg-white light:text-gray-700">
                <input v-model="draft.is_unlimited" type="checkbox" class="accent-emerald-400" />
                Unlimited
              </label>
            </div>
            <p v-if="!bookingServices.length" class="rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 text-xs leading-5 text-amber-100/65">
              Create and activate a booking service before creating a package.
            </p>
            <Button type="submit" class="h-11 w-full gap-2 bg-emerald-500 text-gray-950 hover:bg-emerald-400" :disabled="saving || !draft.name.trim() || !draft.price || !draft.booking_service_id">
              <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
              <Plus v-else class="h-4 w-4" />
              Create package
            </Button>
          </form>
        </aside>
        <aside
          v-else-if="tab === 'packages' && canWritePackages"
          class="rounded-2xl border border-amber-300/15 bg-amber-300/[0.035] p-4 text-xs leading-5 text-amber-100/65"
        >
          Package creation also requires read access to booking settings so every package can be tied to a valid service.
        </aside>
        <aside
          v-else-if="tab === 'customer-plans' && canAssignPackages"
          class="h-fit rounded-[24px] border border-emerald-300/15 bg-emerald-300/[0.045] p-5 light:border-emerald-200 light:bg-emerald-50"
        >
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300 light:text-emerald-700">
            Customer package
          </p>
          <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Sell or grant a plan</h3>
          <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">
            Invoice-backed credits remain pending until payment. Complimentary grants activate immediately.
          </p>
          <form class="mt-6 space-y-4" @submit.prevent="sellOrGrantPackage">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Customer</span>
              <ContactPicker v-model="packageSaleDraft.contact_id" />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Package</span>
              <select v-model="packageSaleDraft.package_definition_id" required class="h-11 w-full rounded-xl border border-white/10 bg-[#15201c] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="" disabled>Select a package</option>
                <option v-for="item in packages.filter((entry) => entry.is_active)" :key="item.id" :value="item.id">
                  {{ item.name }} · {{ money(item.price_minor, item.currency) }}
                </option>
              </select>
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Fulfillment mode</span>
              <select v-model="packageSaleDraft.mode" class="h-11 w-full rounded-xl border border-white/10 bg-[#15201c] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="invoice" :disabled="!canSellPackages">Create invoice (recommended)</option>
                <option value="grant">Complimentary grant</option>
              </select>
            </label>
            <label v-if="packageSaleDraft.mode === 'invoice'" class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Due date (optional)</span>
              <input v-model="packageSaleDraft.due_date" type="date" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <label v-else class="flex items-start gap-3 rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 text-xs leading-5 text-amber-100/70">
              Complimentary grants create live credits without collecting payment. Use only with documented approval.
            </label>
            <Button
              type="submit"
              class="h-11 w-full bg-emerald-500 text-gray-950 hover:bg-emerald-400"
              :disabled="saving || !packageSaleDraft.contact_id || !packageSaleDraft.package_definition_id"
            >
              <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
              {{ packageSaleDraft.mode === 'invoice' ? 'Create sale' : 'Grant package' }}
            </Button>
          </form>
        </aside>
        <aside
          v-else-if="tab === 'customer-plans' && canWritePackages"
          class="rounded-2xl border border-amber-300/15 bg-amber-300/[0.035] p-4 text-xs leading-5 text-amber-100/65"
        >
          Assigning a customer package requires contact read access.
        </aside>
        <aside
          v-else-if="tab === 'invoices' && canWritePayments"
          class="h-fit rounded-[24px] border border-emerald-300/15 bg-emerald-300/[0.045] p-5 light:border-emerald-200 light:bg-emerald-50"
        >
          <template v-if="manualPaymentInvoice">
            <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300 light:text-emerald-700">
              External receipt
            </p>
            <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Record a manual payment</h3>
            <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">
              {{ manualPaymentInvoice.invoice_number }} has
              {{ money(manualPaymentInvoice.due_minor, manualPaymentInvoice.currency) }} outstanding.
              Use this only after independently confirming the funds were received.
            </p>
            <form class="mt-6 space-y-4" @submit.prevent="recordManualPayment">
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Amount received</span>
                <input v-model="manualPaymentDraft.amount" required min="0.01" step="0.01" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Bank / receipt reference</span>
                <input v-model="manualPaymentDraft.reference" required maxlength="255" placeholder="Unique transaction reference" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none placeholder:text-white/25 focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Notes</span>
                <textarea v-model="manualPaymentDraft.notes" rows="3" maxlength="2000" class="w-full resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <label class="flex items-start gap-3 rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 text-xs leading-5 text-amber-100/70 light:text-amber-900">
                <input v-model="manualPaymentDraft.confirm_manual" required type="checkbox" class="mt-1 accent-emerald-400" />
                I independently confirmed that these funds were received. This action updates the financial ledger and
                cannot impersonate a provider callback.
              </label>
              <div class="grid grid-cols-2 gap-3">
                <Button type="button" variant="outline" @click="manualPaymentInvoice = null">Cancel</Button>
                <Button type="submit" class="bg-emerald-500 text-gray-950 hover:bg-emerald-400" :disabled="saving || !manualPaymentDraft.confirm_manual">
                  <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
                  Record
                </Button>
              </div>
            </form>
          </template>
          <template v-else-if="canIssueInvoices">
            <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300 light:text-emerald-700">
              New invoice
            </p>
            <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Issue a customer invoice</h3>
            <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">
              Enter amounts in the selected currency. The server recalculates all totals in minor units.
            </p>
            <form class="mt-6 space-y-4" @submit.prevent="createInvoice">
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Customer</span>
                <ContactPicker v-model="invoiceDraft.contact_id" />
              </label>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Line description</span>
                <input v-model="invoiceDraft.description" required maxlength="2000" placeholder="Assessment, service or product" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none placeholder:text-white/25 focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <div class="grid grid-cols-[90px_1fr_90px] gap-3">
                <label>
                  <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Qty</span>
                  <input v-model.number="invoiceDraft.quantity" required min="1" max="100000" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
                </label>
                <label>
                  <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Unit price</span>
                  <input v-model="invoiceDraft.unit_price" required min="0" step="0.01" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
                </label>
                <label>
                  <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Currency</span>
                  <select v-model="invoiceDraft.currency" class="h-11 w-full rounded-xl border border-white/10 bg-[#15201c] px-2 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900">
                    <option value="MYR">MYR</option>
                    <option value="SGD">SGD</option>
                    <option value="USD">USD</option>
                  </select>
                </label>
              </div>
              <div class="grid grid-cols-2 gap-3">
                <label>
                  <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Tax</span>
                  <input v-model="invoiceDraft.tax" min="0" step="0.01" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
                </label>
                <label>
                  <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Discount</span>
                  <input v-model="invoiceDraft.discount" min="0" step="0.01" type="number" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
                </label>
              </div>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Due date (optional)</span>
                <input v-model="invoiceDraft.due_date" type="date" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none focus:border-emerald-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
              </label>
              <Button type="submit" class="h-11 w-full gap-2 bg-emerald-500 text-gray-950 hover:bg-emerald-400" :disabled="saving || !invoiceDraft.contact_id || !invoiceDraft.description.trim()">
                <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
                <Receipt v-else class="h-4 w-4" />
                Issue invoice
              </Button>
            </form>
          </template>
          <div v-else class="rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-4 text-xs leading-5 text-amber-100/70">
            You can review invoices and record verified payments, but issuing a customer invoice requires contact read
            access.
          </div>
        </aside>
      </div>
    </main>
  </div>
</template>
