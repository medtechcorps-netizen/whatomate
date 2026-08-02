<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { RouterLink } from "vue-router";
import { useI18n } from "vue-i18n";
import { CalendarDate } from "@internationalized/date";
import {
  ArrowUpRight,
  BarChart3,
  AlertCircle,
  Clock3,
  ExternalLink,
  Eye,
  FilterX,
  Globe2,
  MousePointerClick,
  Percent,
  RefreshCw,
  Search,
  Settings2,
  Target,
} from "lucide-vue-next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { DateRangePicker, ErrorState, PageHeader } from "@/components/shared";
import { useDateRange } from "@/composables/useDateRange";
import { Line } from "@/lib/charts";
import {
  searchConsoleService,
  type SearchConsoleProperty,
  type SearchVisibilityResponse,
  type SearchVisibilitySetupResponse,
  type SearchVisibilityType,
} from "@/services/searchConsole";
import { useAuthStore } from "@/stores/auth";

type PageState =
  | "loading"
  | "ready"
  | "disconnected"
  | "degraded"
  | "no_properties"
  | "error";

const { t } = useI18n();
const authStore = useAuthStore();
const pageState = ref<PageState>("loading");
const integration = ref<SearchVisibilitySetupResponse | null>(null);
const properties = ref<SearchConsoleProperty[]>([]);
const selectedPropertyId = ref("");
const selectedSearchType = ref<SearchVisibilityType>("web");
const pageFilterDraft = ref("");
const appliedPageFilter = ref("");
const pageFilterError = ref("");
const analytics = ref<SearchVisibilityResponse | null>(null);
const isAnalyticsLoading = ref(false);
const analyticsError = ref("");
let requestSequence = 0;
let activeRequestKey = "";
let lastLoadedRequestKey = "";

const {
  selectedRange,
  customDateRange,
  isDatePickerOpen,
  dateRange,
  formatDateRangeDisplay,
  applyCustomRange: applyCustomRangeBase,
} = useDateRange({
  defaultPreset: "30days",
  storageKey: "search_visibility",
  endOffsetDays: 2,
  anchorToUTCDate: true,
});

const searchConsoleCutoffDate = (() => {
  const cutoff = new Date();
  cutoff.setUTCDate(cutoff.getUTCDate() - 2);
  return new CalendarDate(
    cutoff.getUTCFullYear(),
    cutoff.getUTCMonth() + 1,
    cutoff.getUTCDate(),
  );
})();

const selectedProperties = computed(() =>
  properties.value.filter((property) => property.selected),
);
const selectedProperty = computed(() =>
  selectedProperties.value.find(
    (property) => property.id === selectedPropertyId.value,
  ),
);
const canManageIntegration = computed(() =>
  authStore.hasPermission("settings.integrations", "read"),
);
const hasData = computed(() => {
  const data = analytics.value;
  if (!data) return false;
  return Boolean(
    data.summary.clicks ||
    data.summary.impressions ||
    data.trend.some((point) => point.clicks || point.impressions) ||
    data.top_queries.length ||
    data.top_pages.length,
  );
});

const searchTypeOptions: Array<{ value: SearchVisibilityType; label: string }> =
  [
    { value: "web", label: "Web" },
    { value: "image", label: "Image" },
    { value: "video", label: "Video" },
    { value: "news", label: "News" },
    { value: "discover", label: "Discover" },
    { value: "googleNews", label: "Google News" },
  ];

function extractError(error: unknown, fallback: string) {
  if (!error || typeof error !== "object") return fallback;
  const response = (error as { response?: { data?: unknown } }).response;
  const payload = response?.data as
    | {
        error?: string | { message?: string };
        message?: string;
        data?: { message?: string };
      }
    | undefined;
  if (typeof payload?.error === "string") return payload.error;
  if (typeof payload?.error === "object" && payload.error?.message)
    return payload.error.message;
  return payload?.data?.message || payload?.message || fallback;
}

function formatNumber(value: number) {
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(
    value || 0,
  );
}

function formatCTR(value: number) {
  const percent = Math.abs(value) <= 1 ? value * 100 : value;
  return `${new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(percent || 0)}%`;
}

function formatPosition(value: number) {
  return new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(value || 0);
}

function formatDate(value?: string) {
  if (!value) return t("searchVisibility.notSynced", "Not synced yet");
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()))
    return t("searchVisibility.notSynced", "Not synced yet");
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function compactURL(value: string) {
  try {
    const url = new URL(value);
    return `${url.hostname}${url.pathname === "/" ? "" : url.pathname}`;
  } catch {
    return value.replace(/^sc-domain:/, "");
  }
}

function safeHTTPURL(value: string) {
  try {
    const parsed = new URL(value);
    if (
      !["http:", "https:"].includes(parsed.protocol) ||
      parsed.username ||
      parsed.password
    ) {
      return "";
    }
    return parsed.toString();
  } catch {
    return "";
  }
}

const metricCards = computed(() => {
  const summary = analytics.value?.summary ?? {
    clicks: 0,
    impressions: 0,
    ctr: 0,
    position: 0,
  };
  return [
    {
      key: "clicks",
      label: t("searchVisibility.googleClicks", "Google clicks"),
      value: formatNumber(summary.clicks),
      hint: t(
        "searchVisibility.googleClicksHint",
        "Clicks from a Google result to this property",
      ),
      icon: MousePointerClick,
      accent: "text-sky-300 light:text-sky-700",
      glow: "bg-sky-400/10 light:bg-sky-100",
    },
    {
      key: "impressions",
      label: t("searchVisibility.impressions", "Impressions"),
      value: formatNumber(summary.impressions),
      hint: t(
        "searchVisibility.impressionsHint",
        "Times content appeared on Google",
      ),
      icon: Eye,
      accent: "text-emerald-300 light:text-emerald-700",
      glow: "bg-emerald-400/10 light:bg-emerald-100",
    },
    {
      key: "ctr",
      label: t("searchVisibility.ctr", "CTR"),
      value: formatCTR(summary.ctr),
      hint: t(
        "searchVisibility.ctrHint",
        "Google clicks divided by impressions",
      ),
      icon: Percent,
      accent: "text-amber-300 light:text-amber-700",
      glow: "bg-amber-400/10 light:bg-amber-100",
    },
    {
      key: "position",
      label: t("searchVisibility.averagePosition", "Average position"),
      value: summary.impressions > 0 ? formatPosition(summary.position) : "—",
      hint: t(
        "searchVisibility.positionHint",
        "Average topmost result position",
      ),
      icon: Target,
      accent: "text-violet-300 light:text-violet-700",
      glow: "bg-violet-400/10 light:bg-violet-100",
    },
  ];
});

const trendChartData = computed(() => ({
  labels: (analytics.value?.trend ?? []).map((point) =>
    new Date(`${point.date}T00:00:00`).toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    }),
  ),
  datasets: [
    {
      label: t("searchVisibility.googleClicks", "Google clicks"),
      data: (analytics.value?.trend ?? []).map((point) => point.clicks),
      borderColor: "#60a5fa",
      backgroundColor: "rgba(96, 165, 250, 0.09)",
      pointBackgroundColor: "#93c5fd",
      pointRadius: (analytics.value?.trend.length ?? 0) > 45 ? 0 : 2,
      pointHoverRadius: 4,
      borderWidth: 2,
      fill: true,
      tension: 0.35,
      yAxisID: "yClicks",
    },
    {
      label: t("searchVisibility.impressions", "Impressions"),
      data: (analytics.value?.trend ?? []).map((point) => point.impressions),
      borderColor: "#6ee7b7",
      backgroundColor: "transparent",
      pointBackgroundColor: "#a7f3d0",
      pointRadius: (analytics.value?.trend.length ?? 0) > 45 ? 0 : 2,
      pointHoverRadius: 4,
      borderWidth: 1.5,
      tension: 0.35,
      yAxisID: "yImpressions",
    },
  ],
}));

const trendChartOptions = {
  responsive: true,
  maintainAspectRatio: false,
  interaction: { mode: "index" as const, intersect: false },
  plugins: {
    legend: {
      position: "bottom" as const,
      labels: { color: "#9ca3af", usePointStyle: true, boxWidth: 7 },
    },
  },
  scales: {
    x: {
      grid: { display: false },
      ticks: { color: "#6b7280", maxRotation: 0, autoSkipPadding: 24 },
      border: { color: "rgba(148,163,184,0.12)" },
    },
    yClicks: {
      beginAtZero: true,
      position: "left" as const,
      grid: { color: "rgba(148,163,184,0.08)" },
      ticks: { color: "#6b7280", precision: 0 },
      border: { display: false },
    },
    yImpressions: {
      beginAtZero: true,
      position: "right" as const,
      grid: { drawOnChartArea: false },
      ticks: { color: "#6b7280", precision: 0 },
      border: { display: false },
    },
  },
};

async function bootstrap() {
  pageState.value = "loading";
  analyticsError.value = "";
  try {
    const setupResponse = await searchConsoleService.getVisibilitySetup();
    integration.value = setupResponse.data.data;

    if (!integration.value || integration.value.status === "not_configured") {
      pageState.value = "disconnected";
      return;
    }
    if (integration.value.status === "degraded") {
      pageState.value = "degraded";
      return;
    }
    if (integration.value.status !== "connected") {
      pageState.value = "disconnected";
      return;
    }

    properties.value = integration.value.properties ?? [];
    if (!selectedProperties.value.length) {
      pageState.value = "no_properties";
      return;
    }

    const savedPropertyId = localStorage.getItem(
      "search_visibility_property_id",
    );
    selectedPropertyId.value = selectedProperties.value.some(
      (property) => property.id === savedPropertyId,
    )
      ? savedPropertyId!
      : selectedProperties.value[0].id;
    pageState.value = "ready";
    await fetchAnalytics();
  } catch (error) {
    analyticsError.value = extractError(
      error,
      t("searchVisibility.loadError", "Search visibility could not be loaded."),
    );
    pageState.value = "error";
  }
}

async function fetchAnalytics(options: { force?: boolean } = {}) {
  if (pageState.value !== "ready" || !selectedPropertyId.value) return;
  const requestParams = {
    property_id: selectedPropertyId.value,
    start_date: dateRange.value.from,
    end_date: dateRange.value.to,
    search_type: selectedSearchType.value,
    ...(appliedPageFilter.value ? { page: appliedPageFilter.value } : {}),
    limit: 10,
  };
  const requestKey = JSON.stringify(requestParams);
  if (
    !options.force &&
    (activeRequestKey === requestKey || lastLoadedRequestKey === requestKey)
  ) {
    return;
  }
  const currentRequest = ++requestSequence;
  activeRequestKey = requestKey;
  isAnalyticsLoading.value = true;
  analyticsError.value = "";
  try {
    const response = await searchConsoleService.getVisibility(requestParams);
    if (currentRequest !== requestSequence) return;
    const data = response.data.data;
    analytics.value = {
      ...data,
      trend: data.trend ?? [],
      top_queries: data.top_queries ?? [],
      top_pages: data.top_pages ?? [],
    };
    lastLoadedRequestKey = requestKey;
  } catch (error) {
    if (currentRequest !== requestSequence) return;
    analytics.value = null;
    analyticsError.value = extractError(
      error,
      t(
        "searchVisibility.analyticsError",
        "Google Search performance data could not be loaded.",
      ),
    );
  } finally {
    if (currentRequest === requestSequence) {
      activeRequestKey = "";
      isAnalyticsLoading.value = false;
    }
  }
}

function refreshAnalytics() {
  void fetchAnalytics({ force: true });
}

function applyPageFilter() {
  pageFilterError.value = "";
  const value = pageFilterDraft.value.trim();
  if (!value) {
    appliedPageFilter.value = "";
    void fetchAnalytics();
    return;
  }
  try {
    const url = new URL(value);
    if (
      !["http:", "https:"].includes(url.protocol) ||
      url.username ||
      url.password ||
      url.hash
    ) {
      throw new Error();
    }
    appliedPageFilter.value = url.toString();
    pageFilterDraft.value = appliedPageFilter.value;
    void fetchAnalytics();
  } catch {
    pageFilterError.value = t(
      "searchVisibility.pageUrlError",
      "Enter a complete http:// or https:// page URL without credentials or a fragment.",
    );
  }
}

function clearPageFilter() {
  pageFilterDraft.value = "";
  pageFilterError.value = "";
  if (!appliedPageFilter.value) return;
  appliedPageFilter.value = "";
  void fetchAnalytics();
}

function applyCustomRange() {
  applyCustomRangeBase();
  void fetchAnalytics();
}

watch(selectedPropertyId, (value, previous) => {
  if (!value || value === previous || pageState.value !== "ready") return;
  localStorage.setItem("search_visibility_property_id", value);
  appliedPageFilter.value = "";
  pageFilterDraft.value = "";
  void fetchAnalytics();
});

watch(selectedSearchType, () => {
  if (pageState.value === "ready") void fetchAnalytics();
});

watch(selectedRange, (value) => {
  if (pageState.value === "ready" && value !== "custom") void fetchAnalytics();
});

onMounted(bootstrap);
</script>

<template>
  <div
    data-testid="search-visibility-page"
    class="flex h-full flex-col bg-[#0a0a0b] light:bg-gray-50"
  >
    <PageHeader
      :title="t('searchVisibility.title', 'Search Visibility')"
      :description="
        t(
          'searchVisibility.subtitle',
          'Google Search performance for your verified website properties',
        )
      "
      :icon="Search"
      icon-gradient="bg-gradient-to-br from-[#173f6d] to-[#2c67a2] shadow-blue-950/30"
    >
      <template #actions>
        <Button
          v-if="pageState === 'ready'"
          variant="outline"
          size="sm"
          :loading="isAnalyticsLoading"
          :disabled="isAnalyticsLoading"
          @click="refreshAnalytics"
        >
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t("searchVisibility.refresh", "Refresh") }}
        </Button>
      </template>
    </PageHeader>

    <main class="flex-1 overflow-y-auto">
      <div class="mx-auto w-full max-w-7xl space-y-5 p-4 sm:p-6 lg:p-8">
        <div
          v-if="pageState === 'loading'"
          class="space-y-5"
          aria-label="Loading Search Visibility"
        >
          <Skeleton class="h-28 w-full rounded-2xl" />
          <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <Skeleton
              v-for="index in 4"
              :key="index"
              class="h-32 rounded-2xl"
            />
          </div>
          <Skeleton class="h-[360px] w-full rounded-2xl" />
        </div>

        <section
          v-else-if="pageState === 'disconnected'"
          class="relative min-h-[440px] overflow-hidden rounded-2xl border border-dashed border-sky-300/15 bg-[#0d1115] px-6 py-16 text-center light:border-sky-200 light:bg-white"
          aria-labelledby="search-disconnected-title"
        >
          <div
            class="pointer-events-none absolute inset-0 bg-[radial-gradient(circle_at_50%_0%,rgba(66,133,244,0.12),transparent_42%)]"
          />
          <div
            class="relative mx-auto grid h-14 w-14 place-items-center rounded-2xl border border-sky-300/15 bg-sky-400/10 text-sky-300 light:bg-sky-100 light:text-sky-700"
          >
            <Globe2 class="h-7 w-7" aria-hidden="true" />
          </div>
          <h2
            id="search-disconnected-title"
            class="relative mt-5 text-xl font-semibold text-white light:text-gray-950"
          >
            {{
              t(
                "searchVisibility.connectTitle",
                "Connect Google Search Console",
              )
            }}
          </h2>
          <p
            class="relative mx-auto mt-2 max-w-lg text-sm leading-6 text-white/65 light:text-gray-600"
          >
            {{
              t(
                "searchVisibility.connectDescription",
                "Authorize read-only access, then choose a verified website property to see Google clicks, impressions, CTR and average position.",
              )
            }}
          </p>
          <Button v-if="canManageIntegration" as-child class="relative mt-6">
            <RouterLink to="/settings/integrations">
              <ExternalLink class="h-4 w-4" aria-hidden="true" />
              {{ t("searchVisibility.openIntegrations", "Open Integrations") }}
            </RouterLink>
          </Button>
          <p
            v-else
            class="relative mx-auto mt-5 max-w-md text-xs text-white/60 light:text-gray-500"
          >
            Ask a workspace administrator to connect Google Search Console.
          </p>
        </section>

        <section
          v-else-if="pageState === 'degraded'"
          class="min-h-[380px] rounded-2xl border border-amber-300/15 bg-amber-300/[0.035] px-6 py-14 text-center light:border-amber-200 light:bg-amber-50"
          role="alert"
        >
          <AlertCircle
            class="mx-auto h-10 w-10 text-amber-300 light:text-amber-700"
            aria-hidden="true"
          />
          <h2 class="mt-4 text-lg font-semibold text-white light:text-gray-950">
            {{
              t(
                "searchVisibility.authorizationIssue",
                "Google authorization needs attention",
              )
            }}
          </h2>
          <p
            class="mx-auto mt-2 max-w-lg text-sm leading-6 text-white/65 light:text-gray-600"
          >
            {{
              integration?.last_error ||
              integration?.message ||
              t(
                "searchVisibility.reauthorizeDescription",
                "The connection may have expired or been revoked. Reauthorize it before loading current Search performance.",
              )
            }}
          </p>
          <Button
            v-if="canManageIntegration"
            as-child
            class="mt-6"
            variant="outline"
          >
            <RouterLink to="/settings/integrations">
              <Settings2 class="h-4 w-4" aria-hidden="true" />
              {{ t("searchVisibility.manageConnection", "Manage connection") }}
            </RouterLink>
          </Button>
        </section>

        <section
          v-else-if="pageState === 'no_properties'"
          class="min-h-[380px] rounded-2xl border border-dashed border-white/[0.1] px-6 py-14 text-center light:border-gray-300 light:bg-white"
        >
          <FilterX
            class="mx-auto h-9 w-9 text-white/25 light:text-gray-400"
            aria-hidden="true"
          />
          <h2 class="mt-4 text-lg font-semibold text-white light:text-gray-950">
            {{
              t(
                "searchVisibility.noPropertiesTitle",
                "No reporting property selected",
              )
            }}
          </h2>
          <p
            class="mx-auto mt-2 max-w-lg text-sm leading-6 text-white/65 light:text-gray-600"
          >
            {{
              t(
                "searchVisibility.noPropertiesDescription",
                "Choose at least one verified website property in the Google Search Console integration.",
              )
            }}
          </p>
          <Button
            v-if="canManageIntegration"
            as-child
            class="mt-6"
            variant="outline"
          >
            <RouterLink to="/settings/integrations">
              <Settings2 class="h-4 w-4" aria-hidden="true" />
              {{ t("searchVisibility.chooseProperties", "Choose properties") }}
            </RouterLink>
          </Button>
        </section>

        <section
          v-else-if="pageState === 'error'"
          class="min-h-[380px] rounded-2xl border border-white/[0.08] light:border-gray-200 light:bg-white"
        >
          <ErrorState
            :title="
              t(
                'searchVisibility.unavailableTitle',
                'Search Visibility is unavailable',
              )
            "
            :description="analyticsError"
            :retry-label="t('searchVisibility.tryAgain', 'Try again')"
            @retry="bootstrap"
          />
        </section>

        <template v-else>
          <section
            class="visibility-control relative overflow-hidden rounded-2xl border border-[#cbd49a]/15 bg-[#10110e] p-4 sm:p-5 light:border-[#697046]/20 light:bg-[#f7f8f2]"
            aria-labelledby="visibility-controls-title"
          >
            <div
              class="control-grid pointer-events-none absolute inset-0 opacity-35 light:opacity-20"
            />
            <div class="relative flex flex-col gap-4">
              <div
                class="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between"
              >
                <div>
                  <p
                    class="text-[10px] font-semibold uppercase tracking-[0.19em] text-[#dce5ac]/65 light:text-[#59613b]"
                  >
                    {{
                      t("searchVisibility.reportingScope", "Reporting scope")
                    }}
                  </p>
                  <h2
                    id="visibility-controls-title"
                    class="mt-1 text-base font-semibold text-white light:text-gray-950"
                  >
                    {{
                      selectedProperty?.display_name ||
                      selectedProperty?.site_url
                    }}
                  </h2>
                  <p class="mt-1 text-xs text-white/60 light:text-gray-500">
                    {{ selectedProperty?.site_url }}
                  </p>
                </div>
                <div
                  class="flex flex-wrap items-center gap-2 text-[11px] text-white/60 light:text-gray-500"
                >
                  <span
                    class="inline-flex items-center gap-1.5 rounded-full border border-white/[0.07] px-2.5 py-1 light:border-gray-200"
                  >
                    <Clock3 class="h-3 w-3" aria-hidden="true" />
                    {{ t("searchVisibility.lastSync", "Last sync") }}:
                    {{ formatDate(selectedProperty?.last_synced_at) }}
                  </span>
                  <RouterLink
                    v-if="canManageIntegration"
                    to="/settings/integrations"
                    class="inline-flex min-h-8 items-center gap-1 rounded-md px-2 text-[#dce5ac] transition-colors hover:bg-[#cbd49a]/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[#cbd49a]/60 light:text-[#59613b]"
                  >
                    {{ t("searchVisibility.manage", "Manage") }}
                    <ArrowUpRight class="h-3 w-3" aria-hidden="true" />
                  </RouterLink>
                </div>
              </div>

              <div
                class="grid gap-3 sm:grid-cols-2 xl:grid-cols-[minmax(210px,1.25fr)_150px_minmax(150px,auto)]"
              >
                <div class="space-y-1.5">
                  <Label
                    for="search-property"
                    class="text-[11px] text-white/50 light:text-gray-600"
                  >
                    {{ t("searchVisibility.property", "Property") }}
                  </Label>
                  <Select v-model="selectedPropertyId">
                    <SelectTrigger
                      id="search-property"
                      data-testid="search-visibility-property"
                    >
                      <SelectValue
                        :placeholder="
                          t(
                            'searchVisibility.selectProperty',
                            'Select property',
                          )
                        "
                      />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem
                        v-for="property in selectedProperties"
                        :key="property.id"
                        :value="property.id"
                      >
                        {{
                          property.display_name || compactURL(property.site_url)
                        }}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                </div>

                <div class="space-y-1.5">
                  <Label
                    for="search-type"
                    class="text-[11px] text-white/50 light:text-gray-600"
                  >
                    {{ t("searchVisibility.searchType", "Search type") }}
                  </Label>
                  <Select v-model="selectedSearchType">
                    <SelectTrigger
                      id="search-type"
                      data-testid="search-visibility-type"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem
                        v-for="option in searchTypeOptions"
                        :key="option.value"
                        :value="option.value"
                      >
                        {{ option.label }}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                </div>

                <div class="space-y-1.5">
                  <Label class="text-[11px] text-white/50 light:text-gray-600">
                    {{ t("searchVisibility.dateRange", "Date range") }}
                  </Label>
                  <div class="flex min-w-0 flex-wrap gap-2">
                    <DateRangePicker
                      v-model:selected-range="selectedRange"
                      v-model:custom-date-range="customDateRange"
                      v-model:is-date-picker-open="isDatePickerOpen"
                      :format-date-range-display="formatDateRangeDisplay"
                      :max-value="searchConsoleCutoffDate"
                      :today-label="
                        t('searchVisibility.latestFinalDay', 'Latest final day')
                      "
                      @apply-custom="applyCustomRange"
                    />
                  </div>
                </div>
              </div>

              <form
                class="flex flex-col gap-2 sm:flex-row sm:items-start"
                @submit.prevent="applyPageFilter"
              >
                <div class="min-w-0 flex-1">
                  <Label for="search-page-filter" class="sr-only">
                    {{ t("searchVisibility.pageFilter", "Exact page URL") }}
                  </Label>
                  <Input
                    id="search-page-filter"
                    v-model="pageFilterDraft"
                    type="url"
                    inputmode="url"
                    autocomplete="off"
                    spellcheck="false"
                    :aria-invalid="Boolean(pageFilterError)"
                    :aria-describedby="
                      pageFilterError ? 'search-page-filter-error' : undefined
                    "
                    :placeholder="
                      t(
                        'searchVisibility.pageFilterPlaceholder',
                        'Optional exact page URL, e.g. https://example.com/services',
                      )
                    "
                  />
                  <p
                    v-if="pageFilterError"
                    id="search-page-filter-error"
                    class="mt-1.5 text-xs text-red-300 light:text-red-700"
                    role="alert"
                  >
                    {{ pageFilterError }}
                  </p>
                </div>
                <div class="flex gap-2">
                  <Button
                    type="submit"
                    variant="outline"
                    :disabled="isAnalyticsLoading"
                  >
                    <Search class="h-4 w-4" aria-hidden="true" />
                    {{ t("searchVisibility.applyPage", "Apply page") }}
                  </Button>
                  <Button
                    v-if="pageFilterDraft || appliedPageFilter"
                    type="button"
                    variant="ghost"
                    :aria-label="
                      t('searchVisibility.clearPageFilter', 'Clear page filter')
                    "
                    @click="clearPageFilter"
                  >
                    <FilterX class="h-4 w-4" aria-hidden="true" />
                    <span class="sm:hidden">{{
                      t("searchVisibility.clear", "Clear")
                    }}</span>
                  </Button>
                </div>
              </form>
            </div>
          </section>

          <div
            class="flex items-start gap-2.5 rounded-xl border border-sky-300/10 bg-sky-300/[0.035] px-4 py-3 text-xs leading-5 text-sky-100/65 light:border-sky-200 light:bg-sky-50 light:text-sky-900"
            role="note"
          >
            <AlertCircle
              class="mt-0.5 h-4 w-4 shrink-0 text-sky-300 light:text-sky-700"
              aria-hidden="true"
            />
            <p>
              <strong class="font-semibold text-sky-100 light:text-sky-950"
                >Google clicks are not website visits.</strong
              >
              Search Console measures interactions from Google results. Website
              sessions and users require a separate Google Analytics
              integration. Data is normally delayed by roughly 2–3 days.
            </p>
          </div>

          <div
            v-if="isAnalyticsLoading"
            class="space-y-5"
            aria-label="Loading Google Search performance"
          >
            <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <Skeleton
                v-for="index in 4"
                :key="index"
                class="h-32 rounded-2xl"
              />
            </div>
            <Skeleton class="h-[360px] rounded-2xl" />
          </div>

          <section
            v-else-if="analyticsError"
            class="rounded-2xl border border-white/[0.08] light:border-gray-200 light:bg-white"
          >
            <ErrorState
              :title="
                t(
                  'searchVisibility.analyticsUnavailableTitle',
                  'Search performance is unavailable',
                )
              "
              :description="analyticsError"
              :retry-label="t('searchVisibility.tryAgain', 'Try again')"
              @retry="refreshAnalytics"
            />
          </section>

          <template v-else-if="analytics">
            <section
              class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4"
              aria-label="Search performance summary"
            >
              <article
                v-for="metric in metricCards"
                :key="metric.key"
                class="metric-card relative overflow-hidden rounded-2xl border border-white/[0.075] bg-[#101011] p-4 light:border-gray-200 light:bg-white"
              >
                <div class="flex items-start justify-between gap-3">
                  <div>
                    <p
                      class="text-[10px] font-semibold uppercase tracking-[0.15em] text-white/60 light:text-gray-500"
                    >
                      {{ metric.label }}
                    </p>
                    <p
                      class="mt-2 text-2xl font-semibold tracking-tight text-white light:text-gray-950"
                    >
                      {{ metric.value }}
                    </p>
                  </div>
                  <div
                    :class="[
                      'grid h-9 w-9 place-items-center rounded-xl',
                      metric.glow,
                      metric.accent,
                    ]"
                  >
                    <component
                      :is="metric.icon"
                      class="h-4 w-4"
                      aria-hidden="true"
                    />
                  </div>
                </div>
                <p
                  class="mt-3 text-[11px] leading-4 text-white/55 light:text-gray-500"
                >
                  {{ metric.hint }}
                </p>
              </article>
            </section>

            <section
              v-if="!hasData"
              class="rounded-2xl border border-dashed border-white/[0.1] px-6 py-12 text-center light:border-gray-300 light:bg-white"
            >
              <BarChart3
                class="mx-auto h-8 w-8 text-white/20 light:text-gray-400"
                aria-hidden="true"
              />
              <h2
                class="mt-4 text-base font-semibold text-white light:text-gray-950"
              >
                {{
                  t(
                    "searchVisibility.noDataTitle",
                    "No Google Search data for this view",
                  )
                }}
              </h2>
              <p
                class="mx-auto mt-2 max-w-lg text-sm leading-6 text-white/60 light:text-gray-600"
              >
                {{
                  t(
                    "searchVisibility.noDataDescription",
                    "Try a wider date range, another search type, or remove the exact page filter. Newly verified properties may take a few days to appear.",
                  )
                }}
              </p>
            </section>

            <template v-else>
              <section
                class="rounded-2xl border border-white/[0.075] bg-[#101011] p-4 sm:p-5 light:border-gray-200 light:bg-white"
                aria-labelledby="search-trend-title"
              >
                <div
                  class="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between"
                >
                  <div>
                    <p
                      class="text-[10px] font-semibold uppercase tracking-[0.16em] text-sky-300/65 light:text-sky-700"
                    >
                      {{ t("searchVisibility.overTime", "Over time") }}
                    </p>
                    <h2
                      id="search-trend-title"
                      class="mt-1 text-base font-semibold text-white light:text-gray-950"
                    >
                      {{
                        t(
                          "searchVisibility.performanceTrend",
                          "Google Search performance",
                        )
                      }}
                    </h2>
                    <p class="mt-1 text-xs text-white/60 light:text-gray-500">
                      {{ analytics.start_date }} — {{ analytics.end_date }}
                    </p>
                  </div>
                  <Badge
                    v-if="appliedPageFilter"
                    variant="info"
                    class="max-w-full gap-1.5"
                  >
                    <Globe2 class="h-3 w-3 shrink-0" aria-hidden="true" />
                    <span class="max-w-[260px] truncate">{{
                      compactURL(appliedPageFilter)
                    }}</span>
                  </Badge>
                </div>
                <div
                  class="mt-5 h-[280px] sm:h-[320px]"
                  role="img"
                  :aria-label="
                    t(
                      'searchVisibility.chartAria',
                      'Line chart of Google clicks and impressions over time',
                    )
                  "
                >
                  <Line :data="trendChartData" :options="trendChartOptions" />
                </div>
                <table class="sr-only">
                  <caption>
                    {{
                      t(
                        "searchVisibility.trendTable",
                        "Google clicks and impressions by date",
                      )
                    }}
                  </caption>
                  <thead>
                    <tr>
                      <th>Date</th>
                      <th>Google clicks</th>
                      <th>Impressions</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="point in analytics.trend" :key="point.date">
                      <td>{{ point.date }}</td>
                      <td>{{ point.clicks }}</td>
                      <td>{{ point.impressions }}</td>
                    </tr>
                  </tbody>
                </table>
              </section>

              <div class="grid gap-5 xl:grid-cols-2">
                <section
                  class="min-w-0 overflow-hidden rounded-2xl border border-white/[0.075] bg-[#101011] light:border-gray-200 light:bg-white"
                  aria-labelledby="top-queries-title"
                >
                  <div
                    class="border-b border-white/[0.07] px-4 py-4 sm:px-5 light:border-gray-200"
                  >
                    <h2
                      id="top-queries-title"
                      class="text-sm font-semibold text-white light:text-gray-950"
                    >
                      {{ t("searchVisibility.topQueries", "Top queries") }}
                    </h2>
                    <p class="mt-1 text-xs text-white/60 light:text-gray-500">
                      {{
                        t(
                          "searchVisibility.topQueriesHint",
                          "Search terms that led people to this property",
                        )
                      }}
                    </p>
                  </div>
                  <div
                    v-if="analytics.top_queries.length"
                    class="overflow-x-auto"
                  >
                    <table class="w-full min-w-[540px] text-left text-xs">
                      <thead
                        class="text-[10px] uppercase tracking-wider text-white/55 light:text-gray-500"
                      >
                        <tr>
                          <th class="px-5 py-3 font-medium">Query</th>
                          <th class="px-3 py-3 text-right font-medium">
                            Clicks
                          </th>
                          <th class="px-3 py-3 text-right font-medium">
                            Impressions
                          </th>
                          <th class="px-5 py-3 text-right font-medium">CTR</th>
                        </tr>
                      </thead>
                      <tbody
                        class="divide-y divide-white/[0.055] light:divide-gray-100"
                      >
                        <tr
                          v-for="row in analytics.top_queries"
                          :key="row.query"
                          class="transition-colors hover:bg-white/[0.025] light:hover:bg-gray-50"
                        >
                          <td
                            class="max-w-[280px] px-5 py-3.5 font-medium text-white/75 light:text-gray-800"
                          >
                            <span class="block truncate">{{
                              row.query || "—"
                            }}</span>
                          </td>
                          <td
                            class="px-3 py-3.5 text-right tabular-nums text-sky-300 light:text-sky-700"
                          >
                            {{ formatNumber(row.clicks) }}
                          </td>
                          <td
                            class="px-3 py-3.5 text-right tabular-nums text-white/65 light:text-gray-600"
                          >
                            {{ formatNumber(row.impressions) }}
                          </td>
                          <td
                            class="px-5 py-3.5 text-right tabular-nums text-white/65 light:text-gray-600"
                          >
                            {{ formatCTR(row.ctr) }}
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <p
                    v-else
                    class="px-5 py-10 text-center text-sm text-white/60 light:text-gray-500"
                  >
                    {{
                      t(
                        "searchVisibility.noQueries",
                        "No query data is available for this view.",
                      )
                    }}
                  </p>
                </section>

                <section
                  class="min-w-0 overflow-hidden rounded-2xl border border-white/[0.075] bg-[#101011] light:border-gray-200 light:bg-white"
                  aria-labelledby="top-pages-title"
                >
                  <div
                    class="border-b border-white/[0.07] px-4 py-4 sm:px-5 light:border-gray-200"
                  >
                    <h2
                      id="top-pages-title"
                      class="text-sm font-semibold text-white light:text-gray-950"
                    >
                      {{ t("searchVisibility.topPages", "Top pages") }}
                    </h2>
                    <p class="mt-1 text-xs text-white/60 light:text-gray-500">
                      {{
                        t(
                          "searchVisibility.topPagesHint",
                          "Pages receiving clicks from Google results",
                        )
                      }}
                    </p>
                  </div>
                  <div
                    v-if="analytics.top_pages.length"
                    class="overflow-x-auto"
                  >
                    <table class="w-full min-w-[540px] text-left text-xs">
                      <thead
                        class="text-[10px] uppercase tracking-wider text-white/55 light:text-gray-500"
                      >
                        <tr>
                          <th class="px-5 py-3 font-medium">Page</th>
                          <th class="px-3 py-3 text-right font-medium">
                            Clicks
                          </th>
                          <th class="px-3 py-3 text-right font-medium">
                            Impressions
                          </th>
                          <th class="px-5 py-3 text-right font-medium">
                            Position
                          </th>
                        </tr>
                      </thead>
                      <tbody
                        class="divide-y divide-white/[0.055] light:divide-gray-100"
                      >
                        <tr
                          v-for="row in analytics.top_pages"
                          :key="row.page"
                          class="transition-colors hover:bg-white/[0.025] light:hover:bg-gray-50"
                        >
                          <td class="max-w-[300px] px-5 py-3.5">
                            <a
                              v-if="safeHTTPURL(row.page)"
                              :href="safeHTTPURL(row.page)"
                              target="_blank"
                              rel="noopener noreferrer"
                              class="group/page flex min-h-6 items-center gap-1.5 font-medium text-white/70 outline-none hover:text-sky-300 focus-visible:ring-2 focus-visible:ring-sky-400 light:text-gray-800 light:hover:text-sky-700"
                              ><span class="block truncate">{{
                                compactURL(row.page)
                              }}</span
                              ><ExternalLink
                                class="h-3 w-3 shrink-0 opacity-0 transition-opacity group-hover/page:opacity-60"
                                aria-hidden="true"
                            /></a>
                            <span
                              v-else
                              class="block truncate text-white/55 light:text-gray-600"
                              >{{ row.page }}</span
                            >
                          </td>
                          <td
                            class="px-3 py-3.5 text-right tabular-nums text-sky-300 light:text-sky-700"
                          >
                            {{ formatNumber(row.clicks) }}
                          </td>
                          <td
                            class="px-3 py-3.5 text-right tabular-nums text-white/65 light:text-gray-600"
                          >
                            {{ formatNumber(row.impressions) }}
                          </td>
                          <td
                            class="px-5 py-3.5 text-right tabular-nums text-white/65 light:text-gray-600"
                          >
                            {{ formatPosition(row.position) }}
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <p
                    v-else
                    class="px-5 py-10 text-center text-sm text-white/60 light:text-gray-500"
                  >
                    {{
                      t(
                        "searchVisibility.noPages",
                        "No page data is available for this view.",
                      )
                    }}
                  </p>
                </section>
              </div>
            </template>
          </template>
        </template>
      </div>
    </main>
  </div>
</template>

<style scoped>
.control-grid {
  background-image:
    linear-gradient(rgba(203, 212, 154, 0.03) 1px, transparent 1px),
    linear-gradient(90deg, rgba(203, 212, 154, 0.03) 1px, transparent 1px);
  background-size: 28px 28px;
  mask-image: linear-gradient(to right, black, transparent 88%);
}

.visibility-control {
  box-shadow:
    0 18px 48px -36px rgba(0, 0, 0, 0.95),
    inset 0 1px 0 rgba(255, 255, 255, 0.025);
}

.metric-card {
  box-shadow:
    0 16px 40px -32px rgba(0, 0, 0, 0.95),
    inset 0 1px 0 rgba(255, 255, 255, 0.025);
  transition:
    border-color 180ms ease,
    transform 180ms ease;
}

.metric-card:hover {
  transform: translateY(-1px);
  border-color: rgba(96, 165, 250, 0.16);
}

@media (prefers-reduced-motion: reduce) {
  .metric-card {
    transition: none;
  }

  .metric-card:hover {
    transform: none;
  }
}
</style>
