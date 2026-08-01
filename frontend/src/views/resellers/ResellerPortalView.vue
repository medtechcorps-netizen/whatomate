<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Building2,
  ChevronLeft,
  ChevronRight,
  CreditCard,
  Check,
  Gauge,
  Globe2,
  Mail,
  MessageSquareText,
  Palette,
  Plus,
  RefreshCw,
  ShieldCheck,
  Smartphone,
  Trash2,
  Users,
} from 'lucide-vue-next'
import { toast } from 'vue-sonner'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import {
  organizationsService,
  resellersService,
  type Organization,
  type Reseller,
  type ResellerMember,
  type ResellerUsage,
  type ResellerUsageOrganization,
} from '@/services/api'
import {
  organizationSubscriptionService,
  type PlanPriceSummary,
  type PlanSummary,
  type SetOrganizationSubscriptionRequest,
  type SubscriptionSummary,
} from '@/services/productSuite'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse, } from '@/lib/api-utils'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

const authStore = useAuthStore()
const organizationsStore = useOrganizationsStore()
const route = useRoute()
const router = useRouter()
const isPlatformOwner = computed(() => authStore.user?.is_super_admin === true)
const activeOrganizationId = computed(() =>
  isPlatformOwner.value
    ? organizationsStore.selectedOrgId || authStore.organizationId
    : authStore.organizationId,
)

const resellers = ref<Reseller[]>([])
const selectedId = ref('')
const usage = ref<ResellerUsage | null>(null)
const WORKSPACE_PAGE_LIMIT = 50
const MAX_WORKSPACE_DISCOVERY_PAGES = 100
const usagePage = ref(1)
const members = ref<ResellerMember[]>([])
const loading = ref(true)
const detailLoading = ref(false)
let detailRequestId = 0
const activeTab = ref('portfolio')

interface PlanPriceOption {
  key: string
  plan: PlanSummary
  price: PlanPriceSummary
}

const selected = computed(() => resellers.value.find((item) => item.id === selectedId.value) ?? null,
)
const plans = ref<PlanSummary[]>([])
const planCatalogLoading = ref(false)
const planCatalogError = ref('')
const licensePriceResolutionError = ref('')
let planCatalogRequestId = 0
const organizationLicenses = ref<Record<string, SubscriptionSummary>>({})
const organizationLicenseErrors = ref<Record<string, string>>({})
const organizationLicenseLoading = ref<Record<string, boolean>>({})
const selectedOrganizationId = ref('')
const licenseSubmitting = ref(false)

const selectedOrganization = computed(
  () =>
    usage.value?.organizations.find(
      (item) => item.id === selectedOrganizationId.value,
    ) ?? null,
)
const selectedOrganizationLicense = computed(() =>
  selectedOrganizationId.value
    ? (organizationLicenses.value[selectedOrganizationId.value] ?? null)
    : null,
)
const usageTotalPages = computed(() =>
  Math.max(1, Math.ceil((usage.value?.total ?? 0) / (usage.value?.limit || WORKSPACE_PAGE_LIMIT))),
)
const usageRange = computed(() => {
  const total = usage.value?.total ?? 0
  const count = usage.value?.organizations.length ?? 0
  if (!total || !count) return { start: 0, end: 0, total }
  const page = usage.value?.page || usagePage.value
  const limit = usage.value?.limit || WORKSPACE_PAGE_LIMIT
  const start = (page - 1) * limit + 1
  return { start, end: Math.min(total, start + count - 1), total }
})
const catalogPlanPriceOptions = computed<PlanPriceOption[]>(() =>
  plans.value.flatMap((plan) =>
    plan.prices
      .filter((price) => price.interval !== 'one_time')
      .map((price) => ({
        key: `${plan.id}::${price.id}`,
        plan,
        price,
      })),
  ),
)
const planPriceOptions = computed(() =>
  catalogPlanPriceOptions.value.filter(
    (option) => option.price.assignable !== false,
  ),
)
const licenseForm = reactive({
  planPriceKey: '',
  status: 'active' as SetOrganizationSubscriptionRequest['status'],
  trialDays: 14,
  manualReference: '',
})
const selectedPlanPrice = computed(
  () =>
    planPriceOptions.value.find(
      (option) => option.key === licenseForm.planPriceKey,
    ) ?? null,)
const organizationCapacity = computed(() => {
  if (!usage.value?.max_organizations) return 0
  return Math.min(100, Math.round((usage.value.organization_count / usage.value.max_organizations) * 100,),)
})

const createOpen = ref(false)
const createSubmitting = ref(false)
const createForm = reactive({
  name: '',
  brand_name: '',
  workspace_name: '',
  support_email: '',
  plan: 'starter' as Reseller['plan'],
})

const organizationOpen = ref(false)
const organizationSubmitting = ref(false)
const organizationName = ref('')
const organizationDeleteOpen = ref(false)
const organizationDeleteSubmitting = ref(false)
const organizationDeleteTarget = ref<Organization | null>(null)
const organizationDeleteConfirmation = ref('')

const memberOpen = ref(false)
const memberSubmitting = ref(false)
const memberForm = reactive({
  email: '',
  role: 'admin' as ResellerMember['role'],
})

const brandingSubmitting = ref(false)
const brandForm = reactive({
  name: '',
  brand_name: '',
  logo_url: '',
  primary_color: '#0f766e',
  accent_color: '#f59e0b',
  support_email: '',
  custom_domain: '',
  status: 'active' as Reseller['status'],
  plan: 'starter' as Reseller['plan'],
  max_organizations: 10,
})

const organizationDeleteBlockingReason = computed(() => {
  const organization =
    organizationDeleteOpen.value && organizationDeleteTarget.value
      ? organizationDeleteTarget.value
      : selectedOrganization.value
  if (!organization) return 'Select a workspace first'
  if (organization.id === activeOrganizationId.value) {
    return 'Switch to a different organization before deleting this workspace'
  }
  if ((usage.value?.organization_count ?? 0) <= 1) {
    return 'A partner portfolio must retain at least one organization'
  }
  const license = organizationLicenses.value[organization.id]
  if (
    license?.provider &&
    license.provider !== 'manual' &&
    !['canceled', 'expired', 'unlicensed'].includes(license.status)
  ) {
    return 'Cancel the provider-managed subscription before deleting this workspace'
  }
  return ''
})

const workspaceLicenseChangeBlockingReason = computed(() => {
  const license = selectedOrganizationLicense.value
  if (license?.provider && license.provider !== 'manual') {
    return 'This provider-managed subscription must be changed through its billing provider'
  }
  return ''
})

const portfolioPlanDescription = computed(() => {
  switch (brandForm.plan) {
    case 'growth':
      return 'Growth portfolio · recommended for partner teams managing up to 50 customer workspaces.'
    case 'enterprise':
      return 'Enterprise portfolio · expanded operations for up to 1,000 customer workspaces.'
    default:
      return 'Starter portfolio · launch and manage up to 10 customer workspaces.'
  }
})

const organizationDeleteConfirmed = computed(() =>
  Boolean(
    organizationDeleteTarget.value &&
      organizationDeleteConfirmation.value.trim() ===
        organizationDeleteTarget.value.name &&
      !organizationDeleteBlockingReason.value,
  ),
)

function requestedOrganizationId(): string {
  const value = route.query.organization_id
  return typeof value === 'string' ? value : ''
}

function hydrateBrandForm(reseller: Reseller | null) {
  if (!reseller) return
  brandForm.name = reseller.name
  brandForm.brand_name = reseller.brand_name || reseller.name
  brandForm.logo_url = reseller.logo_url || ''
  brandForm.primary_color = reseller.primary_color || '#0f766e'
  brandForm.accent_color = reseller.accent_color || '#f59e0b'
  brandForm.support_email = reseller.support_email || ''
  brandForm.custom_domain = reseller.custom_domain || ''
  brandForm.status = reseller.status
  brandForm.plan = reseller.plan
  brandForm.max_organizations = reseller.max_organizations
}

function applyPortfolioPlanCapacity() {
  const capacities: Record<Reseller['plan'], number> = {
    starter: 10,
    growth: 50,
    enterprise: 1000,
  }
  brandForm.max_organizations = capacities[brandForm.plan]
}

function hydrateLicenseForm() {
  const license = selectedOrganizationLicense.value
  let option: PlanPriceOption | null = null
  licensePriceResolutionError.value = ''
  if (license && license.status !== 'unlicensed') {
    if (license.plan_id && license.plan_price_id) {
      option =
        planPriceOptions.value.find(
          (item) =>
            item.plan.id === license.plan_id &&
            item.price.id === license.plan_price_id,
        ) ?? null
    }
    const retiredCurrent =
      license.plan_id && license.plan_price_id
        ? catalogPlanPriceOptions.value.find(
            (item) =>
              item.plan.id === license.plan_id &&
              item.price.id === license.plan_price_id &&
              item.price.assignable === false,
          )
        : null
    if (!option && retiredCurrent) {
      const replacements = planPriceOptions.value.filter(
        (item) => item.plan.id === retiredCurrent.plan.id,
      )
      option =
        replacements.find(
          (item) =>
            item.price.interval === 'month' &&
            (item.price.interval_count || 1) === 1,
        ) ??
        replacements[0] ??
        null
      if (option) {
        licensePriceResolutionError.value =
          'This workspace uses a retired pilot price. The published replacement is selected; no change occurs until you approve and save it.'
      }
    }
    if (!option && !planCatalogLoading.value) {
      licensePriceResolutionError.value =
        'The current license price is not in this workspace’s assignable catalog. Select a price explicitly before saving.'
    }
  } else {
    option = planPriceOptions.value[0] ?? null
  }
  licenseForm.planPriceKey = option?.key ?? ''
  licenseForm.status = license?.status === 'trialing' ? 'trialing' : 'active'
  licenseForm.trialDays = option?.plan.trial_days || 14
  licenseForm.manualReference = ''
}

async function loadPlanCatalog() {
  const organizationId = selectedOrganizationId.value
  const requestId = ++planCatalogRequestId
  plans.value = []
  planCatalogError.value = ''
  licensePriceResolutionError.value = ''
  if (!isPlatformOwner.value || !organizationId) {
    planCatalogLoading.value = false
    hydrateLicenseForm()
    return
  }
  planCatalogLoading.value = true
  try {
    const response = await organizationSubscriptionService.plans(organizationId)
    if (
      requestId !== planCatalogRequestId ||
      organizationId !== selectedOrganizationId.value
    )
      return
    plans.value = unwrapListResponse<PlanSummary>(response, 'plans')
  } catch (error) {
    if (
      requestId !== planCatalogRequestId ||
      organizationId !== selectedOrganizationId.value
    )
      return
    plans.value = []
    planCatalogError.value = getErrorMessage(
      error,
      'Unable to load assignable plans',
    )
  } finally {
    if (
      requestId === planCatalogRequestId &&
      organizationId === selectedOrganizationId.value
    ) {
      planCatalogLoading.value = false
      hydrateLicenseForm()
    }
  }
}

async function refreshOrganizationLicense(
  organizationId: string,
  quiet = false,
) {
  organizationLicenseLoading.value = {
    ...organizationLicenseLoading.value,
    [organizationId]: true,
  }
  try {
    const response = await organizationSubscriptionService.get(organizationId)
    const license = unwrapItemResponse<SubscriptionSummary>(response)
    organizationLicenses.value = {
      ...organizationLicenses.value,
      [organizationId]: license,
    }
    const nextErrors = { ...organizationLicenseErrors.value }
    delete nextErrors[organizationId]
    organizationLicenseErrors.value = nextErrors
    if (organizationId === selectedOrganizationId.value) hydrateLicenseForm()
    return license
  } catch (error) {
    const message = getErrorMessage(error, 'Unable to load workspace license')
    organizationLicenseErrors.value = {
      ...organizationLicenseErrors.value,
      [organizationId]: message,
    }
    if (!quiet) toast.error(message)
    return null
  } finally {
    organizationLicenseLoading.value = {
      ...organizationLicenseLoading.value,
      [organizationId]: false,
    }
  }
}

function hydrateOrganizationLicenses(organizations: ResellerUsageOrganization[]) {
  const organizationIds = new Set(organizations.map((item) => item.id))
  organizationLicenses.value = Object.fromEntries(
    organizations.map((item) => [item.id, item.subscription]),
  )
  organizationLicenseErrors.value = {}
  organizationLicenseLoading.value = {}
  if (!organizations.length) {
    selectedOrganizationId.value = ''
    return
  }
  if (!organizationIds.has(selectedOrganizationId.value)) {
    selectedOrganizationId.value = organizations[0].id
  }
  hydrateLicenseForm()
}

async function loadResellers(preferredId?: string, reloadSelectedIfUnchanged = true) {
  loading.value = true
  try {
    const response = await resellersService.list()
    resellers.value = unwrapListResponse<Reseller>(response, 'resellers')
    const requestedId = requestedOrganizationId()
    let requestedResellerId = ''
    if (requestedId) {
      try {
        const organizationsResponse = await organizationsService.list()
        const organizations = unwrapListResponse<Organization>(
          organizationsResponse,
          'organizations',
        )
        const requestedOrganization = organizations.find(
          (organization) => organization.id === requestedId,
        )
        requestedResellerId = requestedOrganization?.reseller_id || ''
        if (requestedOrganization) {
          selectedOrganizationId.value = requestedOrganization.id
        }
      } catch {
        // Partner Console still loads its default portfolio if the deep-link
        // target is no longer available to this administrator.
      }
    }
    const targetId =
      requestedResellerId &&
      resellers.value.some((item) => item.id === requestedResellerId)
        ? requestedResellerId
        : preferredId &&
            resellers.value.some((item) => item.id === preferredId)
          ? preferredId
          : selectedId.value &&
              resellers.value.some((item) => item.id === selectedId.value)
            ? selectedId.value
            : resellers.value[0]?.id || ''
    const unchanged = targetId === selectedId.value
    selectedId.value = targetId
    if (targetId && unchanged && reloadSelectedIfUnchanged) {
      await loadSelected({
        page: usagePage.value,
        requestedOrganizationId: requestedOrganizationId() || undefined,
      })
    }
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to load partner portfolios'))
  } finally {
    loading.value = false
  }
}

function normalizeUsagePage(nextUsage: ResellerUsage, requestedPage: number): ResellerUsage {
  return {
    ...nextUsage,
    page: nextUsage.page || requestedPage,
    limit: nextUsage.limit || WORKSPACE_PAGE_LIMIT,
    total: nextUsage.total ?? nextUsage.organization_count,
  }
}

async function fetchUsagePage(resellerId: string, page: number) {
  const response = await resellersService.usage(resellerId, {
    page,
    limit: WORKSPACE_PAGE_LIMIT,
  })
  return normalizeUsagePage(unwrapItemResponse<ResellerUsage>(response), page)
}

async function findOrganizationUsagePage(
  resellerId: string,
  organizationId: string,
  initialUsage: ResellerUsage,
  requestId: number,
) {
  if (initialUsage.organizations.some((item) => item.id === organizationId)) {
    return initialUsage
  }

  const totalPages = Math.min(
    MAX_WORKSPACE_DISCOVERY_PAGES,
    Math.max(1, Math.ceil(initialUsage.total / initialUsage.limit)),
  )
  for (let page = 1; page <= totalPages; page += 1) {
    if (page === initialUsage.page) continue
    const candidate = await fetchUsagePage(resellerId, page)
    if (requestId !== detailRequestId || resellerId !== selectedId.value) return null
    if (candidate.organizations.some((item) => item.id === organizationId)) {
      return candidate
    }
  }
  return initialUsage
}

async function loadSelected(options: { page?: number; requestedOrganizationId?: string } = {}) {
  if (!selectedId.value) {
    detailRequestId += 1
    usage.value = null
    usagePage.value = 1
    members.value = []
    selectedOrganizationId.value = ''
    organizationLicenses.value = {}
    organizationLicenseErrors.value = {}
    organizationLicenseLoading.value = {}
    return
  }
  const resellerId = selectedId.value
  const requestId = ++detailRequestId
  const requestedPage = Math.max(1, options.page ?? usagePage.value)
  detailLoading.value = true
  try {
    const [initialUsage, memberResponse] = await Promise.all([
      fetchUsagePage(resellerId, requestedPage),
      resellersService.members(resellerId),
    ])
    if (requestId !== detailRequestId || resellerId !== selectedId.value) return

    let nextUsage = initialUsage
    const lastPage = Math.max(1, Math.ceil(nextUsage.total / nextUsage.limit))
    if (nextUsage.total > 0 && !nextUsage.organizations.length && nextUsage.page > lastPage) {
      nextUsage = await fetchUsagePage(resellerId, lastPage)
    }
    if (options.requestedOrganizationId) {
      const locatedUsage = await findOrganizationUsagePage(
        resellerId,
        options.requestedOrganizationId,
        nextUsage,
        requestId,
      )
      if (!locatedUsage) return
      nextUsage = locatedUsage
    }
    if (requestId !== detailRequestId || resellerId !== selectedId.value) return

    usage.value = nextUsage
    usagePage.value = nextUsage.page
    members.value = unwrapListResponse<ResellerMember>(memberResponse, 'members',)
    hydrateBrandForm(selected.value)
    if (
      options.requestedOrganizationId &&
      nextUsage.organizations.some((item) => item.id === options.requestedOrganizationId)
    ) {
      selectedOrganizationId.value = options.requestedOrganizationId
    }
    hydrateOrganizationLicenses(nextUsage.organizations)
  } catch (error) {
    if (requestId === detailRequestId) {
      toast.error(getErrorMessage(error, 'Unable to load portfolio details'))
    }
  } finally {
    if (requestId === detailRequestId) detailLoading.value = false
  }
}

async function changeUsagePage(page: number) {
  if (detailLoading.value || page < 1 || page > usageTotalPages.value || page === usagePage.value) return
  await loadSelected({ page })
}

watch(selectedId, () => {
  usagePage.value = 1
  void loadSelected({
    page: 1,
    requestedOrganizationId: requestedOrganizationId() || undefined,
  })
})
watch(selectedOrganizationId, () => {
  organizationDeleteOpen.value = false
  hydrateLicenseForm()
  void loadPlanCatalog()
})
watch(organizationDeleteOpen, (open) => {
  if (open) return
  organizationDeleteTarget.value = null
  organizationDeleteConfirmation.value = ''
})
watch(
  () => route.query.organization_id,
  async () => {
    const requestedId = requestedOrganizationId()
    if (!requestedId) return
    if (
      usage.value?.organizations.some(
        (organization) => organization.id === requestedId,
      )
    ) {
      selectedOrganizationId.value = requestedId
      return
    }
    await loadResellers()
  },
)
watch(planPriceOptions, hydrateLicenseForm)
onMounted(loadResellers)

async function createReseller() {
  if (!createForm.name.trim()) {
    toast.error('Company name is required')
    return
  }
  createSubmitting.value = true
  try {
    const response = await resellersService.create({
      name: createForm.name.trim(),
      brand_name: createForm.brand_name.trim() || createForm.name.trim(),
      workspace_name: createForm.workspace_name.trim() || undefined,
      support_email: createForm.support_email.trim() || undefined,
      plan: createForm.plan,
    })
    const result = unwrapItemResponse<{ reseller: Reseller }>(response)
    createOpen.value = false
    Object.assign(createForm, {
      name: '',
      brand_name: '',
      workspace_name: '',
      support_email: '',
      plan: 'starter',
    })
    toast.success('Partner and private workspace created')
    await loadResellers(result.reseller.id)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to create partner'))
  } finally {
    createSubmitting.value = false
  }
}

async function createOrganization() {
  if (!selected.value || !organizationName.value.trim()) {
    toast.error('Business name is required')
    return
  }
  organizationSubmitting.value = true
  try {
    const response = await organizationsService.create({
      name: organizationName.value.trim(),
      reseller_id: selected.value.id,
    })
    const createdOrganization = unwrapItemResponse<Organization>(response)
    organizationOpen.value = false
    organizationName.value = ''
    toast.success('Customer workspace provisioned')
    await loadResellers(selected.value.id, false)
    await loadSelected({
      page: 1,
      requestedOrganizationId: createdOrganization.id,
    })
    await router.replace({
      path: '/resellers',
      query: { organization_id: createdOrganization.id },
    })
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to create customer workspace'))
  } finally {
    organizationSubmitting.value = false
  }
}

function openOrganizationDeleteDialog(target?: Organization) {
  const organization = target ?? selectedOrganization.value
  if (!organization) return
  organizationDeleteTarget.value = organization
  organizationDeleteConfirmation.value = ''
  organizationDeleteOpen.value = true
}

async function deleteOrganization() {
  const target = organizationDeleteTarget.value
  if (!target || !organizationDeleteConfirmed.value) return

  organizationDeleteSubmitting.value = true
  try {
    const resellerId = selectedId.value
    await organizationsService.delete(target.id)
    organizationDeleteOpen.value = false
    organizationDeleteTarget.value = null
    organizationDeleteConfirmation.value = ''
    selectedOrganizationId.value = ''
    await loadSelected()
    await Promise.all([
      loadResellers(resellerId, false),
      organizationsStore.fetchOrganizations(),
    ])
    await router.replace({
      path: '/resellers',
      query: selectedOrganizationId.value
        ? { organization_id: selectedOrganizationId.value }
        : undefined,
    })
    toast.success(`${target.name} deleted. Its data is retained for recovery.`)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to delete workspace'))
  } finally {
    organizationDeleteSubmitting.value = false
  }
}

async function addMember() {
  if (!selected.value || !memberForm.email.trim()) {
    toast.error('Administrator email is required')
    return
  }
  memberSubmitting.value = true
  try {
    await resellersService.addMember(selected.value.id, {
      email: memberForm.email.trim(),
      role: memberForm.role,
    })
    memberOpen.value = false
    memberForm.email = ''
    memberForm.role = 'admin'
    toast.success('Partner administrator access synchronized')
    await loadSelected()
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to add administrator'))
  } finally {
    memberSubmitting.value = false
  }
}

async function removeMember(member: ResellerMember) {
  if (!selected.value) return
  if (!window.confirm(`Revoke ${member.full_name || member.email} from this entire portfolio?`,)) return
  try {
    await resellersService.removeMember(selected.value.id, member.id)
    toast.success('Partner access revoked')
    await loadSelected()
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to revoke administrator'))
  }
}

async function saveBranding() {
  if (!selected.value) return
  brandingSubmitting.value = true
  try {
    const payload: Parameters<typeof resellersService.update>[1] = {
      name: brandForm.name.trim(),
      brand_name: brandForm.brand_name.trim(),
      logo_url: brandForm.logo_url.trim(),
      primary_color: brandForm.primary_color,
      accent_color: brandForm.accent_color,
      support_email: brandForm.support_email.trim(),
      custom_domain: brandForm.custom_domain.trim().toLowerCase(),
    }
    if (isPlatformOwner.value) {
      payload.status = brandForm.status
      payload.plan = brandForm.plan
      payload.max_organizations = Number(brandForm.max_organizations)
    }
    await resellersService.update(selected.value.id, payload)
    toast.success('Partner configuration saved')
    await loadResellers(selected.value.id)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to save partner configuration'))
  } finally {
    brandingSubmitting.value = false
  }
}

async function assignWorkspaceLicense() {
  if (!isPlatformOwner.value || !selectedOrganization.value) return
  if (!selectedPlanPrice.value) {
    toast.error('Choose an active recurring plan price')
    return
  }
  const reference = licenseForm.manualReference.trim()
  if (!reference) {
    toast.error('An approval or contract reference is required')
    return
  }
  const payload: SetOrganizationSubscriptionRequest = {
    plan_id: selectedPlanPrice.value.plan.id,
    plan_price_id: selectedPlanPrice.value.price.id,
    status: licenseForm.status,
    manual_reference: reference,
  }
  if (licenseForm.status === 'trialing') {
    const trialDays = Number(licenseForm.trialDays)
    if (!Number.isInteger(trialDays) || trialDays < 1 || trialDays > 365) {
      toast.error('Trial days must be between 1 and 365')
      return
    }
    payload.trial_days = trialDays
  }

  licenseSubmitting.value = true
  try {
    const organizationId = selectedOrganization.value.id
    const response = await organizationSubscriptionService.set(
      organizationId,
      payload,
    )
    organizationLicenses.value = {
      ...organizationLicenses.value,
      [organizationId]: unwrapItemResponse<SubscriptionSummary>(response),
    }
    if (organizationId === activeOrganizationId.value) {
      await authStore.fetchProductEntitlements()
    }
    toast.success(`${selectedOrganization.value.name} license assigned`)
    await refreshOrganizationLicense(organizationId, true)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to assign workspace license'))
  } finally {
    licenseSubmitting.value = false
  }
}

function licenseStatusVariant(
  status?: string,
): 'success' | 'info' | 'warning' | 'destructive' {
  if (status === 'active') return 'success'
  if (status === 'trialing') return 'info'
  if (status === 'unlicensed') return 'warning'
  return 'destructive'
}

function licenseStatusLabel(status?: string) {
  if (!status) return 'Unavailable'
  return status
    .replace(/_/g, ' ')
    .replace(/\b\w/g, (character: string) => character.toUpperCase())
}

function formatMoney(price: PlanPriceSummary) {
  return formatMoneyAmount(price.currency, price.unit_amount_minor)
}

function formatMoneyAmount(currency: string, amountMinor: number) {
  return formatCurrencyMinorUnits(currency, amountMinor)
}

function formatPriceInterval(price: PlanPriceSummary) {
  const count = price.interval_count || 1
  if (count === 1) return price.interval
  return `${count} ${price.interval}s`
}

function formatLicenseDate(value?: string) {
  if (!value) return 'Not scheduled'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'Not scheduled'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
  }).format(date)
}

function formatNumber(value?: number) {
  return new Intl.NumberFormat().format(value ?? 0)
}
</script>

<template>
  <div class="min-h-full bg-[#08090a] light:bg-gray-50 text-white light:text-gray-900">
    <header class="border-b border-white/[0.08] light:border-gray-200 bg-[#0b0c0e]/95 light:bg-white/95">
      <div class="flex min-h-20 items-center gap-4 px-6 py-4">
        <div class="flex h-11 w-11 items-center justify-center rounded-xl border border-emerald-400/20 bg-emerald-400/10">
          <Building2 class="h-5 w-5 text-emerald-300 light:text-emerald-700" />
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <h1 class="text-xl font-semibold tracking-tight">Partner Console</h1>
            <Badge variant="outline">Control plane</Badge>
          </div>
          <p class="mt-0.5 text-sm text-white/45 light:text-gray-500">
            Isolated customer workspaces, partner access and white-label operations.
          </p>
        </div>
        <Button variant="outline" size="sm" :disabled="loading" @click="loadResellers(selectedId)">
          <RefreshCw class="mr-2 h-4 w-4" :class="{ 'animate-spin': loading }" />
          Refresh
        </Button>
        <Button v-if="isPlatformOwner" size="sm" @click="createOpen = true">
          <Plus class="mr-2 h-4 w-4" />
          New partner
        </Button>
      </div>
    </header>

    <main class="grid min-h-[calc(100vh-5rem)] grid-cols-1 lg:grid-cols-[300px_minmax(0,1fr)]">
      <aside class="border-b border-white/[0.08] light:border-gray-200 lg:border-b-0 lg:border-r">
        <div class="p-4">
          <p class="px-2 text-[11px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-gray-500">
            Portfolios · {{ resellers.length }}
          </p>
          <div v-if="loading" class="mt-4 space-y-2">
            <div v-for="index in 3" :key="index" class="h-20 animate-pulse rounded-xl bg-white/[0.04] light:bg-gray-100" />
          </div>
          <div v-else-if="resellers.length" class="mt-3 space-y-2">
            <button
              v-for="reseller in resellers"
              :key="reseller.id"
              class="group w-full rounded-xl border p-3 text-left transition"
              :class="selectedId === reseller.id
                ? 'border-emerald-400/35 bg-emerald-400/[0.08]'
                : 'border-transparent bg-white/[0.025] hover:border-white/10 hover:bg-white/[0.045] light:bg-white light:hover:border-gray-200'"
              @click="selectedId = reseller.id"
            >
              <div class="flex items-start gap-3">
                <div
                  class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-sm font-bold text-white"
                  :style="{ backgroundColor: reseller.primary_color || '#0f766e', }"
                >
                  {{ (reseller.brand_name || reseller.name).slice(0, 1).toUpperCase() }}
                </div>
                <div class="min-w-0 flex-1">
                  <p class="truncate text-sm font-medium">{{ reseller.brand_name || reseller.name }}</p>
                  <div class="mt-1 flex items-center gap-2 text-xs text-white/40 light:text-gray-500">
                    <span>{{ reseller.organization_count }} workspace{{ reseller.organization_count === 1 ? '' : 's' }}</span>
                    <span>·</span>
                    <span class="capitalize">{{ reseller.plan }}</span>
                  </div>
                </div>
                <span
                  class="mt-1 h-2 w-2 rounded-full"
                  :class="reseller.status === 'active' ? 'bg-emerald-400' : 'bg-amber-400'"
                />
              </div>
            </button>
          </div>
          <div v-else class="mt-10 text-center text-sm text-white/40 light:text-gray-500">
            No partner portfolios yet.
          </div>
        </div>
      </aside>

      <section v-if="selected" class="min-w-0 p-5 md:p-7">
        <div
          class="relative overflow-hidden rounded-2xl border border-white/10 bg-[#101214] p-6 light:border-gray-200 light:bg-white"
          :style="{ '--brand-primary': selected.primary_color, '--brand-accent': selected.accent_color, }"
        >
          <div class="absolute inset-y-0 left-0 w-1 bg-[var(--brand-primary)]" />
          <div class="absolute -right-12 -top-20 h-52 w-52 rounded-full bg-[var(--brand-primary)] opacity-[0.09] blur-3xl" />
          <div class="relative flex flex-col gap-4 md:flex-row md:items-center">
            <div
              class="flex h-14 w-14 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-white/10 text-xl font-bold text-white"
              :style="{ backgroundColor: selected.primary_color || '#0f766e' }"
            >
              <img v-if="selected.logo_url" :src="selected.logo_url" :alt="selected.brand_name" class="h-full w-full object-contain p-1.5" />
              <span v-else>{{ (selected.brand_name || selected.name).slice(0, 1).toUpperCase() }}</span>
            </div>
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <h2 class="truncate text-2xl font-semibold tracking-tight">{{ selected.brand_name || selected.name }}</h2>
                <Badge :variant="selected.status === 'active' ? 'success' : 'warning'" class="capitalize">
                  {{ selected.status }}
                </Badge>
                <Badge variant="secondary" class="capitalize">{{ selected.plan }}</Badge>
              </div>
              <p class="mt-1 text-sm text-white/45 light:text-gray-500">
                {{ selected.custom_domain || `${selected.slug}.partner` }}
              </p>
            </div>
            <Button variant="outline" size="sm" @click="activeTab = 'brand'">
              <Palette class="mr-2 h-4 w-4" />
              Configure brand
            </Button>
          </div>
        </div>

        <div class="mt-5 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-blue-400/10 p-2.5"><Building2 class="h-5 w-5 text-blue-300 light:text-blue-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.organization_count) }}</p><p class="text-xs text-white/40 light:text-gray-500">Customer workspaces</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-violet-400/10 p-2.5"><Users class="h-5 w-5 text-violet-300 light:text-violet-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.user_count) }}</p><p class="text-xs text-white/40 light:text-gray-500">Unique CRM users</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-emerald-400/10 p-2.5"><Users class="h-5 w-5 text-emerald-300 light:text-emerald-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.contacts) }}</p><p class="text-xs text-white/40 light:text-gray-500">Contacts managed</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-amber-400/10 p-2.5"><MessageSquareText class="h-5 w-5 text-amber-300 light:text-amber-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.messages) }}</p><p class="text-xs text-white/40 light:text-gray-500">Messages processed</p></div>
            </CardContent>
          </Card>
        </div>

        <Tabs v-model="activeTab" class="mt-6">
          <TabsList class="bg-white/[0.04] light:bg-gray-100">
            <TabsTrigger value="portfolio">Portfolio</TabsTrigger>
            <TabsTrigger value="team">Administrators</TabsTrigger>
            <TabsTrigger value="brand">Brand & portfolio capacity</TabsTrigger>
          </TabsList>

          <TabsContent value="portfolio" class="mt-4">
            <div class="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader class="flex-row items-center justify-between space-y-0">
                  <div>
                    <CardTitle class="text-base">Customer workspaces</CardTitle>
                    <p class="mt-1 text-xs text-white/40 light:text-gray-500">Each business has its own CRM boundary and PostgreSQL RLS context.</p>
                  </div>
                  <Button size="sm" @click="organizationOpen = true"><Plus class="mr-2 h-4 w-4" />New workspace</Button>
                </CardHeader>
                <CardContent class="px-0 pb-0">
                  <div class="overflow-x-auto">
                    <table class="w-full text-sm">
                      <thead class="border-y border-white/[0.07] bg-white/[0.02] text-left text-[11px] uppercase tracking-wider text-white/35 light:border-gray-200 light:text-gray-500">
                        <tr><th class="px-6 py-3 font-medium">Business</th><th class="px-4 py-3 font-medium">License</th>
                          <th class="px-4 py-3 font-medium">Workspace ID</th><th class="px-4 py-3 font-medium">Created</th>
                          <th class="px-6 py-3 text-right font-medium">
                            <span class="sr-only">Licensedetails</span></th></tr>
                      </thead>
                      <tbody>
                        <tr v-for="organization in usage?.organizations" :key="organization.id"
                          data-testid="workspace-license-row" class="border-b border-white/[0.06] transition last:border-0 light:border-gray-100"
                          :class="{
                            'bg-emerald-400/[0.045] light:bg-emerald-50':
                              selectedOrganizationId === organization.id,
                          }">
                          <td class="px-6 py-4">
                            <p class="font-medium">{{ organization.name }}</p>
                            <p class="mt-0.5 text-xs text-white/35 light:text-gray-500">{{ organization.slug }}</p>
                          </td>
                          <td class="px-4 py-4">
                            <div
                              v-if="organizationLicenseLoading[organization.id]"
                              class="space-y-2"
                            >
                              <div
                                class="h-4 w-24 animate-pulse rounded bg-white/[0.07] light:bg-gray-100"
                              />
                              <div
                                class="h-3 w-14 animate-pulse rounded bg-white/[0.04] light:bg-gray-100"
                              />
                            </div>
                            <Badge
                              v-else-if="
                                organizationLicenseErrors[organization.id]
                              "
                              variant="destructive"
                            >
                              Unavailable
                            </Badge>
                            <div v-else>
                              <p class="max-w-48 truncate text-xs font-medium">
                                {{
                                  organizationLicenses[organization.id]
                                    ?.plan_name || 'No active plan'
                                }}
                              </p>
                              <Badge
                                :variant="
                                  licenseStatusVariant(
                                    organizationLicenses[organization.id]
                                      ?.status,
                                  )
                                "
                                class="mt-1"
                              >
                                {{
                                  licenseStatusLabel(
                                    organizationLicenses[organization.id]
                                      ?.status,
                                  )
                                }}
                              </Badge>
                            </div>
                          </td>
                          <td
                            class="px-4 py-4 font-mono text-xs text-white/40 light:text-gray-500">{{ organization.id.slice(0, 8) }}…</td>
                          <td class="px-4 py-4 text-xs text-white/40 light:text-gray-500">{{ new Date(organization.created_at,).toLocaleDateString() }}</td>
                        <td class="px-6 py-4 text-right">
                            <div class="flex items-center justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="sm"
                              data-testid="workspace-license-select"
                              @click="selectedOrganizationId = organization.id"
                            >
                              {{ isPlatformOwner ? 'Manage' : 'View' }}
                            </Button>
                              <Button
                                v-if="isPlatformOwner"
                                variant="ghost"
                                size="icon"
                                data-testid="workspace-delete-row"
                                :aria-label="`Delete ${organization.name}`"
                                :title="`Delete ${organization.name}`"
                                @click="openOrganizationDeleteDialog(organization)"
                              >
                                <Trash2 class="h-4 w-4 text-red-300 light:text-red-700" />
                              </Button>
                            </div>
                          </td>
                        </tr>
                        <tr v-if="!usage?.organizations.length"><td colspan="5" class="px-6 py-12 text-center text-white/35 light:text-gray-500">No customer workspaces yet.</td></tr>
                      </tbody>
                    </table>
                  </div>
                  <nav
                    v-if="usage"
                    data-testid="workspace-pagination"
                    aria-label="Customer workspace pages"
                    class="flex flex-col gap-3 border-t border-white/[0.07] px-6 py-3 light:border-gray-200 sm:flex-row sm:items-center sm:justify-between"
                  >
                    <p class="text-xs text-white/40 light:text-gray-600" aria-live="polite">
                      Showing
                      <span class="font-medium text-white/70 light:text-gray-800">
                        {{ usageRange.start }}&ndash;{{ usageRange.end }}
                      </span>
                      of {{ usageRange.total }} workspaces
                    </p>
                    <div class="flex items-center gap-2">
                      <span class="text-[11px] text-white/35 light:text-gray-500">
                        Page {{ usagePage }} of {{ usageTotalPages }}
                      </span>
                      <Button
                        type="button"
                        variant="outline"
                        size="icon"
                        class="h-8 w-8"
                        aria-label="Previous workspace page"
                        :disabled="detailLoading || usagePage <= 1"
                        @click="changeUsagePage(usagePage - 1)"
                      >
                        <ChevronLeft class="h-4 w-4" />
                      </Button>
                      <Button
                        type="button"
                        variant="outline"
                        size="icon"
                        class="h-8 w-8"
                        aria-label="Next workspace page"
                        :disabled="detailLoading || usagePage >= usageTotalPages"
                        @click="changeUsagePage(usagePage + 1)"
                      >
                        <ChevronRight class="h-4 w-4" />
                      </Button>
                    </div>
                  </nav>
                </CardContent>
              </Card>

              <div class="space-y-4">
                <Card
                  data-testid="workspace-license-panel"
                  class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white"
                >
                  <CardHeader
                    class="flex-row items-center justify-between space-y-0"
                  >
                    <CardTitle class="flex items-center gap-2 text-sm">
                      <CreditCard
                        class="h-4 w-4 text-amber-300 light:text-amber-700"
                      />
                      Workspace license
                    </CardTitle>
                    <Button
                      v-if="selectedOrganization"
                      variant="ghost"
                      size="icon"
                      title="Refresh selected workspace license"
                      aria-label="Refresh selected workspace license"
                      :disabled="
                        organizationLicenseLoading[selectedOrganization.id]
                      "
                      @click="
                        refreshOrganizationLicense(selectedOrganization.id)
                      "
                    >
                      <RefreshCw
                        class="h-4 w-4"
                        :class="{
                          'animate-spin':
                            organizationLicenseLoading[selectedOrganization.id],
                        }"
                      />
                    </Button>
                  </CardHeader>
                  <CardContent>
                    <div v-if="!selectedOrganization" class="py-6 text-center">
                      <p class="text-sm text-white/45 light:text-gray-500">
                        Select a workspace to inspect its license.
                      </p>
                    </div>
                    <div
                      v-else-if="
                        organizationLicenseLoading[selectedOrganization.id]
                      "
                      class="space-y-3"
                    >
                      <div
                        class="h-5 w-36 animate-pulse rounded bg-white/[0.07] light:bg-gray-100"
                      />
                      <div
                        class="h-4 w-24 animate-pulse rounded bg-white/[0.04] light:bg-gray-100"
                      />
                      <div
                        class="h-24 animate-pulse rounded-xl bg-white/[0.025] light:bg-gray-50"
                      />
                    </div>
                    <div
                      v-else-if="
                        organizationLicenseErrors[selectedOrganization.id]
                      "
                      class="rounded-xl border border-red-400/15 bg-red-400/[0.06] p-4"
                    >
                      <p
                        class="text-sm font-medium text-red-200 light:text-red-700"
                      >
                        License unavailable
                      </p>
                      <p
                        class="mt-1 text-xs leading-5 text-red-100/55 light:text-red-600"
                      >
                        {{ organizationLicenseErrors[selectedOrganization.id] }}
                      </p>
                      <Button
                        class="mt-3"
                        variant="outline"
                        size="sm"
                        @click="
                          refreshOrganizationLicense(selectedOrganization.id)
                        "
                      >
                        Try again
                      </Button>
                    </div>
                    <div v-else class="space-y-4">
                      <div>
                        <p class="truncate text-sm font-semibold">
                          {{ selectedOrganization.name }}
                        </p>
                        <p
                          class="mt-0.5 truncate text-xs text-white/35 light:text-gray-500"
                        >
                          {{ selectedOrganization.slug }}
                        </p>
                      </div>
                      <div
                        class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-4 light:border-gray-200 light:bg-gray-50"
                      >
                        <div
                          class="flex flex-wrap items-center justify-between gap-2"
                        >
                          <p class="text-sm font-medium">
                            {{
                              selectedOrganizationLicense?.plan_name ||
                              'No active plan'
                            }}
                          </p>
                          <Badge
                            :variant="
                              licenseStatusVariant(
                                selectedOrganizationLicense?.status,
                              )
                            "
                          >
                            {{
                              licenseStatusLabel(
                                selectedOrganizationLicense?.status,
                              )
                            }}
                          </Badge>
                        </div>
                        <div class="mt-3 grid grid-cols-2 gap-3 text-xs">
                          <div>
                            <p class="text-white/35 light:text-gray-500">
                              {{
                                selectedOrganizationLicense?.status ===
                                'trialing'
                                  ? 'Trial ends'
                                  : 'Period ends'
                              }}
                            </p>
                            <p class="mt-1 font-medium">
                              {{
                                formatLicenseDate(
                                  selectedOrganizationLicense?.status ===
                                    'trialing'
                                    ? selectedOrganizationLicense?.trial_ends_at
                                    : selectedOrganizationLicense?.current_period_end,
                                )
                              }}
                            </p>
                          </div>
                          <div>
                            <p class="text-white/35 light:text-gray-500">
                              Plan code
                            </p>
                            <p class="mt-1 truncate font-mono text-[11px]">
                              {{
                                selectedOrganizationLicense?.plan_code ||
                                'unlicensed'
                              }}
                            </p>
                          </div>
                        </div>
                      </div>

                      <template v-if="isPlatformOwner">
                        <div
                          class="border-t border-white/[0.07] pt-4 light:border-gray-200"
                        >
                          <div
                            class="mb-3 flex items-center justify-between gap-2"
                          >
                            <div>
                              <p class="text-sm font-medium">
                                Assign manual license
                              </p>
                              <p
                                class="mt-0.5 text-xs text-white/35 light:text-gray-500"
                              >
                                Stores the approval reference with the license
                                record.
                              </p>
                            </div>
                            <Badge variant="outline">Platform owner</Badge>
                          </div>

                          <div v-if="planCatalogLoading" class="space-y-2">
                            <div
                              class="h-10 animate-pulse rounded-md bg-white/[0.05] light:bg-gray-100"
                            />
                            <div
                              class="h-10 animate-pulse rounded-md bg-white/[0.035] light:bg-gray-50"
                            />
                          </div>
                          <div
                            v-else-if="planCatalogError"
                            class="rounded-lg border border-red-400/15 bg-red-400/[0.05] p-3"
                          >
                            <p
                              class="text-xs leading-5 text-red-100/65 light:text-red-700"
                            >
                              {{ planCatalogError }}
                            </p>
                            <Button
                              class="mt-2"
                              variant="outline"
                              size="sm"
                              @click="loadPlanCatalog"
                              >Retry catalog</Button
                            >
                          </div>
                          <div
                            v-else-if="
                              !plans.length || !planPriceOptions.length
                            "
                            data-testid="workspace-license-no-plans"
                            class="rounded-lg border border-amber-400/15 bg-amber-400/[0.05] p-3"
                          >
                            <p
                              class="text-xs font-medium text-amber-100 light:text-amber-800"
                            >
                              {{
                                plans.length
                                  ? 'No active recurring plan prices'
                                  : 'No assignable product plans'
                              }}
                            </p>
                            <p
                              class="mt-1 text-xs leading-5 text-white/40 light:text-gray-600"
                            >
                              Create or activate a manual recurring plan for
                              this workspace’s portfolio before assigning it.
                            </p>
                          </div>
                          <div
                            v-else-if="workspaceLicenseChangeBlockingReason"
                            class="rounded-lg border border-amber-400/15 bg-amber-400/[0.05] p-3"
                          >
                            <p
                              class="text-xs font-medium text-amber-100 light:text-amber-800"
                            >
                              Manual plan change unavailable
                            </p>
                            <p
                              class="mt-1 text-xs leading-5 text-white/40 light:text-gray-600"
                            >
                              {{ workspaceLicenseChangeBlockingReason }}
                            </p>
                          </div>
                          <form
                            v-else
                            class="space-y-3"
                            @submit.prevent="assignWorkspaceLicense"
                          >
                            <div class="space-y-1.5">
                              <Label for="workspace-license-plan"
                                >Active plan price</Label
                              >
                              <select
                                id="workspace-license-plan"
                                v-model="licenseForm.planPriceKey"
                                data-testid="workspace-license-plan"
                                class="control-select"
                                required
                              >
                                <option disabled value="">
                                  Select a plan price
                                </option>
                                <option
                                  v-for="option in planPriceOptions"
                                  :key="option.key"
                                  :value="option.key"
                                >
                                  {{ option.plan.name }} ·
                                  {{ formatMoney(option.price) }} /
                                  {{ formatPriceInterval(option.price) }}
                                </option>
                              </select>
                              <p
                                v-if="licensePriceResolutionError"
                                data-testid="workspace-license-price-unresolved"
                                class="text-[11px] leading-5 text-amber-200/75 light:text-amber-700"
                              >
                                {{ licensePriceResolutionError }}
                              </p>
                              <p
                                v-if="
                                  selectedPlanPrice?.price.setup_amount_minor
                                "
                                class="text-[11px] text-white/35 light:text-gray-500"
                              >
                                Setup fee:
                                {{
                                  formatMoneyAmount(
                                    selectedPlanPrice.price.currency,
                                    selectedPlanPrice.price.setup_amount_minor,
                                  )
                                }}
                              </p>
                            </div>
                            <div class="grid grid-cols-2 gap-3">
                              <div class="space-y-1.5">
                                <Label for="workspace-license-status"
                                  >Status</Label
                                >
                                <select
                                  id="workspace-license-status"
                                  v-model="licenseForm.status"
                                  data-testid="workspace-license-status"
                                  class="control-select"
                                >
                                  <option value="active">Active</option>
                                  <option value="trialing">Trialing</option>
                                </select>
                              </div>
                              <div
                                v-if="licenseForm.status === 'trialing'"
                                class="space-y-1.5"
                              >
                                <Label for="workspace-license-trial"
                                  >Trial days</Label
                                >
                                <Input
                                  id="workspace-license-trial"
                                  v-model.number="licenseForm.trialDays"
                                  data-testid="workspace-license-trial"
                                  type="number"
                                  min="1"
                                  max="365"
                                  required
                                />
                              </div>
                            </div>
                            <div class="space-y-1.5">
                              <Label for="workspace-license-reference"
                                >Approval / contract reference</Label
                              >
                              <Input
                                id="workspace-license-reference"
                                v-model="licenseForm.manualReference"
                                data-testid="workspace-license-reference"
                                maxlength="255"
                                placeholder="REALIGN-2026-001"
                                required
                              />
                            </div>
                            <Button
                              type="submit"
                              class="w-full"
                              data-testid="workspace-license-submit"
                              :loading="licenseSubmitting"
                              :disabled="
                                licenseSubmitting ||
                                !selectedPlanPrice ||
                                !licenseForm.manualReference.trim()
                              "
                            >
                              <Check class="mr-2 h-4 w-4" />
                              Assign license
                            </Button>
                          </form>
                        </div>
                      </template>
                      <p
                        v-else
                        class="border-t border-white/[0.07] pt-3 text-xs leading-5 text-white/35 light:border-gray-200 light:text-gray-500"
                      >
                        License changes are reserved for the platform owner.
                        Partner administrators retain read-only portfolio
                        visibility.
                      </p>
                    </div>
                  </CardContent>
                </Card>
                <Card
                  v-if="isPlatformOwner"
                  data-testid="workspace-danger-zone"
                  class="border-red-400/15 bg-[#0d0f11] light:border-red-200 light:bg-white"
                >
                  <CardHeader>
                    <CardTitle class="flex items-center gap-2 text-sm text-red-200 light:text-red-700">
                      <Trash2 class="h-4 w-4" />
                      Workspace actions
                    </CardTitle>
                    <p class="text-xs leading-5 text-white/40 light:text-gray-600">
                      This control remains available while license information refreshes. Tenant records are retained
                      for recovery.
                    </p>
                  </CardHeader>
                  <CardContent>
                    <Button
                      class="w-full"
                      variant="destructive"
                      size="sm"
                      data-testid="workspace-delete-open"
                      :disabled="!selectedOrganization"
                      @click="openOrganizationDeleteDialog()"
                    >
                      <Trash2 class="mr-2 h-4 w-4" />
                      Delete
                      {{ selectedOrganization?.name || 'selected organization' }}
                    </Button>
                    <p
                      v-if="organizationDeleteBlockingReason"
                      class="mt-2 text-[11px] leading-5 text-amber-200/75 light:text-amber-700"
                    >
                      {{ organizationDeleteBlockingReason }}
                    </p>
                  </CardContent>
                </Card>
                <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader><CardTitle class="flex items-center gap-2 text-sm"><Gauge class="h-4 w-4 text-emerald-300" />Plan capacity</CardTitle></CardHeader>
                  <CardContent>
                    <div class="flex items-end justify-between">
                      <p class="text-2xl font-semibold">{{ usage?.organization_count || 0 }}<span class="text-sm font-normal text-white/35"> / {{ usage?.max_organizations || 0 }}</span></p>
                      <p class="text-xs text-white/40 light:text-gray-500">{{ organizationCapacity }}%</p>
                    </div>
                    <Progress :model-value="organizationCapacity" class="mt-3" />
                    <p class="mt-3 text-xs leading-5 text-white/35 light:text-gray-500">Organization limits are enforced before provisioning. Platform owners can adjust the plan.</p>
                  </CardContent>
                </Card>
                <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                  <CardHeader><CardTitle class="flex items-center gap-2 text-sm"><Smartphone class="h-4 w-4 text-blue-300" />WhatsApp footprint</CardTitle></CardHeader>
                  <CardContent>
                    <p class="text-3xl font-semibold">{{ formatNumber(usage?.whatsapp_accounts) }}</p>
                    <p class="mt-1 text-xs text-white/40 light:text-gray-500">Connected business accounts</p>
                  </CardContent>
                </Card>
              </div>
            </div>
          </TabsContent>

          <TabsContent value="team" class="mt-4">
            <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
              <CardHeader class="flex-row items-center justify-between space-y-0">
                <div>
                  <CardTitle class="text-base">Partner administrators</CardTitle>
                  <p class="mt-1 text-xs text-white/40 light:text-gray-500">Access is inherited across this portfolio and revoked as one traceable unit.</p>
                </div>
                <Button size="sm" @click="memberOpen = true"><Plus class="mr-2 h-4 w-4" />Add administrator</Button>
              </CardHeader>
              <CardContent class="space-y-2">
                <div v-for="member in members" :key="member.id" class="flex items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.02] p-4 light:border-gray-200 light:bg-gray-50">
                  <div class="flex h-10 w-10 items-center justify-center rounded-full bg-white/[0.07] text-sm font-semibold light:bg-white">
                    {{ (member.full_name || member.email).slice(0, 1).toUpperCase() }}
                  </div>
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm font-medium">{{ member.full_name || member.email }}</p>
                    <p class="truncate text-xs text-white/40 light:text-gray-500">{{ member.email }}</p>
                  </div>
                  <Badge :variant="member.role === 'owner' ? 'info' : 'secondary'" class="capitalize">{{ member.role }}</Badge>
                  <Button variant="ghost" size="icon" title="Revoke portfolio access" @click="removeMember(member)">
                    <Trash2 class="h-4 w-4 text-red-300" />
                  </Button>
                </div>
                <div v-if="!members.length" class="py-12 text-center">
                  <ShieldCheck class="mx-auto h-8 w-8 text-white/20 light:text-gray-300" />
                  <p class="mt-3 text-sm text-white/45 light:text-gray-500">No delegated administrators yet.</p>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="brand" class="mt-4">
            <form class="grid gap-4 xl:grid-cols-[minmax(0,1fr)_320px]" @submit.prevent="saveBranding">
              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader>
                  <CardTitle class="text-base">Partner identity</CardTitle>
                  <p class="text-xs text-white/40 light:text-gray-500">Configure the customer-facing brand and support identity.</p>
                </CardHeader>
                <CardContent class="grid gap-5 sm:grid-cols-2">
                  <div class="space-y-2"><Label for="partner-name">Legal / partner name</Label><Input id="partner-name" v-model="brandForm.name" /></div>
                  <div class="space-y-2"><Label for="brand-name">Display brand</Label><Input id="brand-name" v-model="brandForm.brand_name" /></div>
                  <div class="space-y-2 sm:col-span-2"><Label for="logo-url">Logo URL</Label><Input id="logo-url" v-model="brandForm.logo_url" placeholder="https://…" /></div>
                  <div class="space-y-2"><Label for="support-email">Support email</Label><Input id="support-email" v-model="brandForm.support_email" type="email" placeholder="support@example.com" /></div>
                  <div class="space-y-2"><Label for="custom-domain">Custom hostname</Label><Input id="custom-domain" v-model="brandForm.custom_domain" placeholder="crm.partner.com" /></div>
                  <div class="space-y-2"><Label for="primary-color">Primary color</Label><div class="flex gap-2"><Input id="primary-color" v-model="brandForm.primary_color" /><input v-model="brandForm.primary_color" type="color" class="h-10 w-12 rounded-md border border-white/10 bg-transparent p-1" /></div></div>
                  <div class="space-y-2"><Label for="accent-color">Accent color</Label><div class="flex gap-2"><Input id="accent-color" v-model="brandForm.accent_color" /><input v-model="brandForm.accent_color" type="color" class="h-10 w-12 rounded-md border border-white/10 bg-transparent p-1" /></div></div>
                  <template v-if="isPlatformOwner">
                    <div class="space-y-2"><Label for="partner-status">Status</Label><select id="partner-status" v-model="brandForm.status" class="control-select"><option value="active">Active</option><option value="suspended">Suspended</option></select></div>
                    <div class="space-y-2">
                      <Label for="partner-plan">Partner portfolio tier</Label>
                      <select id="partner-plan" v-model="brandForm.plan" class="control-select" @change="applyPortfolioPlanCapacity">
                        <option value="starter">Starter · portfolio capacity</option>
                        <option value="growth">Growth · portfolio capacity</option>
                        <option value="enterprise">Enterprise · portfolio capacity</option>
                      </select>
                      <p class="text-[11px] leading-5 text-white/40 light:text-gray-500">
                        {{ portfolioPlanDescription }} Selecting a tier updates the organization-limit field below; you can adjust it before saving. This does not change feature access for an individual workspace.
                      </p>
                    </div>
                    <div class="space-y-2"><Label for="org-limit">Organization limit</Label><Input id="org-limit" v-model.number="brandForm.max_organizations" type="number" min="1" /></div>
                  </template>
                  <div class="flex justify-end sm:col-span-2"><Button type="submit" :loading="brandingSubmitting"><Check class="mr-2 h-4 w-4" />Save configuration</Button></div>
                </CardContent>
              </Card>

              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader><CardTitle class="text-base">Live preview</CardTitle></CardHeader>
                <CardContent>
                  <div class="overflow-hidden rounded-xl border border-white/10 bg-[#090a0b] light:border-gray-200 light:bg-gray-50">
                    <div class="h-1" :style="{ backgroundColor: brandForm.primary_color }" />
                    <div class="p-5">
                      <div class="flex items-center gap-3">
                        <div class="flex h-11 w-11 items-center justify-center rounded-lg text-lg font-semibold text-white" :style="{ backgroundColor: brandForm.primary_color }">
                          <img v-if="brandForm.logo_url" :src="brandForm.logo_url" class="h-full w-full object-contain p-1" />
                          <span v-else>{{ (brandForm.brand_name || brandForm.name || 'P').slice(0, 1).toUpperCase() }}</span>
                        </div>
                        <div><p class="font-semibold">{{ brandForm.brand_name || 'Partner brand' }}</p><p class="text-xs text-white/35 light:text-gray-500">WhatsApp CRM</p></div>
                      </div>
                      <div class="mt-6 space-y-2">
                        <div class="h-2 w-3/4 rounded bg-white/10 light:bg-gray-200" />
                        <div class="h-2 w-1/2 rounded bg-white/[0.06] light:bg-gray-100" />
                      </div>
                      <button type="button" class="mt-6 w-full rounded-lg px-3 py-2 text-sm font-medium text-white" :style="{ backgroundColor: brandForm.accent_color }">Open inbox</button>
                    </div>
                  </div>
                  <div class="mt-4 space-y-3 text-xs text-white/40 light:text-gray-500">
                    <p class="flex items-center gap-2"><Globe2 class="h-4 w-4" />{{ brandForm.custom_domain || 'Canonical domain not assigned' }}</p>
                    <p class="flex items-center gap-2"><Mail class="h-4 w-4" />{{ brandForm.support_email || 'Support identity not assigned' }}</p>
                  </div>
                </CardContent>
              </Card>
            </form>
          </TabsContent>
        </Tabs>
        <div v-if="detailLoading" class="pointer-events-none fixed inset-0 z-20 bg-black/5" />
      </section>

      <section v-else class="flex min-h-[60vh] items-center justify-center p-8 text-center">
        <div><Building2 class="mx-auto h-10 w-10 text-white/15 light:text-gray-300" /><h2 class="mt-4 font-medium">Select a partner portfolio</h2><p class="mt-1 text-sm text-white/40 light:text-gray-500">Portfolio controls will appear here.</p></div>
      </section>
    </main>

    <Dialog v-model:open="createOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Create partner</DialogTitle><DialogDescription>Creates an isolated reseller portfolio and a private administration workspace.</DialogDescription></DialogHeader>
        <div class="grid gap-4 py-2">
          <div class="space-y-2"><Label for="create-name">Company name</Label><Input id="create-name" v-model="createForm.name" placeholder="Example Health Technologies" /></div>
          <div class="space-y-2"><Label for="create-brand">Display brand</Label><Input id="create-brand" v-model="createForm.brand_name" placeholder="Example Health" /></div>
          <div class="space-y-2"><Label for="create-workspace">Private workspace name</Label><Input id="create-workspace" v-model="createForm.workspace_name" placeholder="Example Health HQ" /></div>
          <div class="space-y-2"><Label for="create-email">Support email</Label><Input id="create-email" v-model="createForm.support_email" type="email" /></div>
          <div class="space-y-2"><Label for="create-plan">Initial plan</Label><select id="create-plan" v-model="createForm.plan" class="control-select"><option value="starter">Starter · 10 organizations</option><option value="growth">Growth · 50 organizations</option><option value="enterprise">Enterprise · 1,000 organizations</option></select></div>
        </div>
        <DialogFooter><Button variant="outline" @click="createOpen = false">Cancel</Button><Button :loading="createSubmitting" @click="createReseller">Create partner</Button></DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="organizationOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Provision customer workspace</DialogTitle><DialogDescription>The new business receives its own CRM tenant, roles, settings and RLS boundary.</DialogDescription></DialogHeader>
        <div class="space-y-2 py-3"><Label for="organization-name">Business name</Label><Input id="organization-name" v-model="organizationName" placeholder="Serenity Wellness Clinic" @keyup.enter="createOrganization" /></div>
        <DialogFooter><Button variant="outline" @click="organizationOpen = false">Cancel</Button><Button :loading="organizationSubmitting" @click="createOrganization">Provision workspace</Button></DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="organizationDeleteOpen">
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            Delete {{ organizationDeleteTarget?.name || 'organization' }}?
          </DialogTitle>
          <DialogDescription>
            Access is removed immediately and any manual license is canceled.
            Tenant records are retained so the workspace can be recovered.
          </DialogDescription>
        </DialogHeader>
        <div class="space-y-4 py-3">
          <div
            class="rounded-lg border border-red-400/20 bg-red-400/[0.06] p-3 text-xs leading-5 text-red-100/70 light:text-red-700"
          >
            Users will no longer be able to switch into this organization.
            Provider-managed subscriptions must be canceled before deletion.
          </div>
          <div class="space-y-2">
            <Label for="workspace-delete-confirmation">
              Type
              <strong>{{ organizationDeleteTarget?.name }}</strong>
              to confirm
            </Label>
            <Input
              id="workspace-delete-confirmation"
              v-model="organizationDeleteConfirmation"
              data-testid="workspace-delete-confirmation"
              autocomplete="off"
              @keyup.enter="
                organizationDeleteConfirmed && deleteOrganization()
              "
            />
          </div>
          <p
            v-if="organizationDeleteBlockingReason"
            class="text-xs leading-5 text-amber-200/75 light:text-amber-700"
          >
            {{ organizationDeleteBlockingReason }}
          </p>
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            :disabled="organizationDeleteSubmitting"
            @click="organizationDeleteOpen = false"
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            data-testid="workspace-delete-submit"
            :loading="organizationDeleteSubmitting"
            :disabled="
              organizationDeleteSubmitting || !organizationDeleteConfirmed
            "
            @click="deleteOrganization"
          >
            Delete organization
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="memberOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Add partner administrator</DialogTitle><DialogDescription>The user must already exist in a workspace. Their inherited access will cover every current and future organization in this portfolio.</DialogDescription></DialogHeader>
        <div class="grid gap-4 py-3">
          <div class="space-y-2"><Label for="member-email">Existing user email</Label><Input id="member-email" v-model="memberForm.email" type="email" placeholder="admin@partner.com" /></div>
          <div class="space-y-2"><Label for="member-role">Portfolio role</Label><select id="member-role" v-model="memberForm.role" class="control-select"><option value="admin">Administrator</option><option value="owner">Owner</option></select></div>
        </div>
        <DialogFooter><Button variant="outline" @click="memberOpen = false">Cancel</Button><Button :loading="memberSubmitting" @click="addMember">Grant portfolio access</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

<style scoped>
.control-select {
  @apply flex h-10 w-full rounded-md border border-white/10 bg-[#111315] px-3 py-2 text-sm text-white outline-none ring-offset-background transition focus:border-emerald-400/50 focus:ring-2 focus:ring-emerald-400/15;
}

:global(.light) .control-select {
  @apply border-gray-200 bg-white text-gray-900;
}
</style>
