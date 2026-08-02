<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { RouterLink } from "vue-router";
import {
  Activity,
  ArrowUpRight,
  CheckCircle2,
  ExternalLink,
  Globe2,
  Link2,
  RefreshCw,
  Search,
  ShieldCheck,
  Trash2,
  AlertTriangle,
} from "lucide-vue-next";
import { toast } from "vue-sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { ConfirmDialog } from "@/components/shared";
import {
  integrationsService,
  type IntegrationState,
} from "@/services/integrations";
import {
  searchConsoleService,
  type SearchConsoleProperty,
} from "@/services/searchConsole";

const open = defineModel<boolean>("open", { default: false });

const props = defineProps<{
  integration: IntegrationState;
  canWrite: boolean;
  canViewAnalytics: boolean;
}>();

const emit = defineEmits<{
  refresh: [];
}>();

const properties = ref<SearchConsoleProperty[]>([]);
const selectedPropertyIds = ref<string[]>([]);
const isLoading = ref(false);
const loadError = ref("");
const loadErrorAction = ref<"load" | "refresh" | null>(null);
const activeAction = ref<
  "connect" | "save" | "refresh" | "test" | "disconnect" | null
>(null);
const disconnectOpen = ref(false);

const isConnected = computed(() => props.integration.status === "connected");
const hasGoogleAuthorization = computed(
  () =>
    isConnected.value ||
    props.integration.status === "degraded" ||
    props.integration.configured ||
    Boolean(props.integration.credentials?.refresh_token?.configured),
);
const websiteProperties = computed(() =>
  properties.value.filter((property) => {
    const type = property.property_type.toLowerCase();
    if (["instagram", "tiktok", "x", "youtube"].includes(type)) return false;
    return (
      ["domain", "url_prefix", "website"].includes(type) ||
      /^(https?:\/\/|sc-domain:)/i.test(property.site_url)
    );
  }),
);
const selectedCount = computed(() => selectedPropertyIds.value.length);
const selectionChanged = computed(() => {
  const initial = websiteProperties.value
    .filter((property) => property.selected)
    .map((property) => property.id)
    .sort();
  return initial.join("|") !== [...selectedPropertyIds.value].sort().join("|");
});
const oauthStartAvailable = computed(() => props.integration.oauth.available);
const providerOperationsAvailable = computed(() =>
  Boolean(props.integration.config?.operations_available),
);

function errorMessage(error: unknown, fallback: string) {
  if (!error || typeof error !== "object") return fallback;
  const response = (error as { response?: { data?: unknown } }).response;
  const payload = response?.data as
    | { error?: string | { message?: string }; message?: string }
    | undefined;
  if (typeof payload?.error === "string") return payload.error;
  if (typeof payload?.error === "object" && payload.error?.message)
    return payload.error.message;
  return payload?.message || fallback;
}

function formatDate(value?: string) {
  if (!value) return "Not synced yet";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "Not synced yet";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function propertyTypeLabel(value: string) {
  const labels: Record<string, string> = {
    domain: "Domain property",
    url_prefix: "URL-prefix property",
    website: "Website",
  };
  return labels[value] || value.replace(/_/g, " ");
}

async function loadProperties() {
  if (!hasGoogleAuthorization.value) {
    properties.value = [];
    selectedPropertyIds.value = [];
    return;
  }
  isLoading.value = true;
  loadError.value = "";
  loadErrorAction.value = null;
  try {
    const response = await searchConsoleService.listProperties();
    properties.value = response.data.data.properties ?? [];
    selectedPropertyIds.value = websiteProperties.value
      .filter((property) => property.selected)
      .map((property) => property.id);
  } catch (error) {
    loadErrorAction.value = "load";
    loadError.value = errorMessage(
      error,
      "Verified Search Console properties could not be loaded.",
    );
  } finally {
    isLoading.value = false;
  }
}

function setPropertySelected(propertyId: string, checked: boolean | string) {
  if (checked === true) {
    selectedPropertyIds.value = [
      ...new Set([...selectedPropertyIds.value, propertyId]),
    ];
    return;
  }
  selectedPropertyIds.value = selectedPropertyIds.value.filter(
    (id) => id !== propertyId,
  );
}

async function connectGoogle() {
  if (!props.canWrite || !oauthStartAvailable.value) return;
  activeAction.value = "connect";
  try {
    const response = await integrationsService.connect("google_search_console");
    const result = response.data.data;
    if (!result.ready || !result.authorization_url) {
      toast.error(result.message || "Google authorization is unavailable.");
      return;
    }
    window.location.assign(result.authorization_url);
  } catch (error) {
    toast.error(errorMessage(error, "Could not start Google authorization."));
  } finally {
    activeAction.value = null;
  }
}

async function refreshProperties() {
  if (!props.canWrite || !hasGoogleAuthorization.value) return;
  activeAction.value = "refresh";
  loadError.value = "";
  loadErrorAction.value = null;
  try {
    const response = await searchConsoleService.refreshProperties();
    properties.value = response.data.data.properties ?? [];
    selectedPropertyIds.value = websiteProperties.value
      .filter((property) => property.selected)
      .map((property) => property.id);
    toast.success("Verified Google properties refreshed");
    emit("refresh");
  } catch (error) {
    loadErrorAction.value = "refresh";
    loadError.value = errorMessage(
      error,
      "Verified Search Console properties could not be refreshed.",
    );
  } finally {
    activeAction.value = null;
  }
}

function retryProperties() {
  if (loadErrorAction.value === "refresh") {
    void refreshProperties();
    return;
  }
  void loadProperties();
}

async function saveProperties() {
  if (!props.canWrite || selectedCount.value === 0) return;
  activeAction.value = "save";
  try {
    const response = await searchConsoleService.updateProperties(
      selectedPropertyIds.value,
    );
    properties.value = response.data.data.properties ?? [];
    selectedPropertyIds.value = websiteProperties.value
      .filter((property) => property.selected)
      .map((property) => property.id);
    toast.success(
      `${response.data.data.selected_count} Search Console ${
        response.data.data.selected_count === 1 ? "property" : "properties"
      } selected`,
    );
    emit("refresh");
  } catch (error) {
    toast.error(errorMessage(error, "Could not save property selection."));
  } finally {
    activeAction.value = null;
  }
}

async function testConnection() {
  if (!props.canWrite || !hasGoogleAuthorization.value) return;
  activeAction.value = "test";
  try {
    const response = await integrationsService.test("google_search_console");
    const result = response.data.data;
    if (result.success)
      toast.success(result.message || "Google Search Console is healthy");
    else toast.error(result.message || "Google authorization needs attention");
    emit("refresh");
  } catch (error) {
    toast.error(errorMessage(error, "Could not test Google authorization."));
  } finally {
    activeAction.value = null;
  }
}

async function disconnect() {
  if (!props.canWrite) return;
  activeAction.value = "disconnect";
  try {
    await searchConsoleService.disconnect();
    properties.value = [];
    selectedPropertyIds.value = [];
    disconnectOpen.value = false;
    open.value = false;
    toast.success("Google Search Console disconnected");
    emit("refresh");
  } catch (error) {
    toast.error(errorMessage(error, "Could not disconnect Google."));
  } finally {
    activeAction.value = null;
  }
}

watch(
  () => [open.value, props.integration.status] as const,
  ([isOpen]) => {
    if (isOpen) void loadProperties();
  },
  { immediate: true },
);
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent
      data-testid="integration-dialog-google_search_console"
      class="max-h-[92vh] max-w-3xl gap-0 overflow-hidden border-white/[0.1] bg-[#0d0d0e] p-0 light:border-gray-200 light:bg-white"
    >
      <DialogHeader
        class="relative border-b border-white/[0.08] px-5 py-5 pr-12 text-left sm:px-6 light:border-gray-200"
      >
        <div
          class="pointer-events-none absolute right-0 top-0 h-32 w-44 bg-[radial-gradient(circle_at_top_right,rgba(66,133,244,0.2),transparent_68%)]"
        />
        <div class="relative flex items-start gap-3">
          <div
            class="grid h-11 w-11 shrink-0 place-items-center rounded-xl border border-sky-300/15 bg-gradient-to-br from-[#173f6d] to-[#24578c] shadow-lg shadow-blue-950/30"
          >
            <Search class="h-5 w-5 text-sky-100" aria-hidden="true" />
          </div>
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <DialogTitle class="text-lg text-white light:text-gray-950">
                Google Search Console
              </DialogTitle>
              <Badge
                :variant="
                  isConnected
                    ? 'success'
                    : integration.status === 'degraded'
                      ? 'destructive'
                      : 'secondary'
                "
              >
                {{
                  isConnected
                    ? "Connected"
                    : integration.status === "degraded"
                      ? "Needs attention"
                      : "Not connected"
                }}
              </Badge>
            </div>
            <DialogDescription class="mt-1 text-white/65 light:text-gray-600">
              Select verified properties and measure how they perform in Google
              Search.
            </DialogDescription>
          </div>
        </div>
      </DialogHeader>

      <div class="max-h-[calc(92vh-154px)] overflow-y-auto px-5 py-5 sm:px-6">
        <section
          class="rounded-2xl border border-sky-300/10 bg-[linear-gradient(135deg,rgba(21,69,112,0.28),rgba(10,10,11,0.15))] p-4 light:border-sky-200 light:bg-sky-50"
          aria-labelledby="gsc-oauth-title"
        >
          <div
            class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between"
          >
            <div class="flex items-start gap-3">
              <ShieldCheck
                class="mt-0.5 h-5 w-5 shrink-0 text-sky-300 light:text-sky-700"
                aria-hidden="true"
              />
              <div>
                <h3
                  id="gsc-oauth-title"
                  class="text-sm font-semibold text-white light:text-gray-950"
                >
                  Read-only Google authorization
                </h3>
                <p
                  class="mt-1 max-w-xl text-xs leading-5 text-white/65 light:text-gray-600"
                >
                  ReReply never asks for a Google password, client secret, or
                  token. Google handles authorization and only verified
                  properties available to that account can be selected.
                </p>
              </div>
            </div>
            <Button
              v-if="!hasGoogleAuthorization"
              data-testid="gsc-connect"
              class="shrink-0"
              :loading="activeAction === 'connect'"
              :disabled="
                !canWrite || activeAction !== null || !oauthStartAvailable
              "
              @click="connectGoogle"
            >
              <ExternalLink class="h-4 w-4" aria-hidden="true" />
              Connect Google
            </Button>
            <Button
              v-else
              data-testid="gsc-reconnect"
              variant="outline"
              class="shrink-0"
              :loading="activeAction === 'connect'"
              :disabled="
                !canWrite || activeAction !== null || !oauthStartAvailable
              "
              @click="connectGoogle"
            >
              <Link2 class="h-4 w-4" aria-hidden="true" />
              Reauthorize
            </Button>
          </div>
          <div
            v-if="!oauthStartAvailable"
            class="mt-3 flex items-start gap-2 rounded-lg border border-amber-300/15 bg-amber-300/[0.06] px-3 py-2 text-xs text-amber-200 light:border-amber-200 light:bg-amber-50 light:text-amber-800"
            role="status"
          >
            <AlertTriangle
              class="mt-0.5 h-3.5 w-3.5 shrink-0"
              aria-hidden="true"
            />
            Google authorization cannot start on this deployment right now.
            {{
              integration.message ||
              "Ask a platform administrator to check OAuth, encryption, and state storage."
            }}
          </div>
        </section>

        <section
          v-if="hasGoogleAuthorization"
          class="mt-6"
          aria-labelledby="gsc-properties-title"
        >
          <div
            class="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between"
          >
            <div>
              <p
                class="text-[10px] font-semibold uppercase tracking-[0.18em] text-sky-300/70 light:text-sky-700"
              >
                Reporting scope
              </p>
              <h3
                id="gsc-properties-title"
                class="mt-1 text-sm font-semibold text-white light:text-gray-950"
              >
                Verified properties
              </h3>
              <p class="mt-1 text-xs text-white/60 light:text-gray-500">
                Choose one or more properties to make available in Search
                Visibility.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              :loading="activeAction === 'refresh'"
              :disabled="
                !canWrite ||
                activeAction !== null ||
                !providerOperationsAvailable
              "
              @click="refreshProperties"
            >
              <RefreshCw class="h-3.5 w-3.5" aria-hidden="true" />
              Refresh from Google
            </Button>
          </div>

          <div
            v-if="isLoading"
            class="mt-4 space-y-2"
            aria-label="Loading verified properties"
          >
            <div
              v-for="index in 3"
              :key="index"
              class="flex items-center gap-3 rounded-xl border border-white/[0.07] p-3.5 light:border-gray-200"
            >
              <Skeleton class="h-4 w-4 rounded" />
              <div class="min-w-0 flex-1 space-y-2">
                <Skeleton class="h-3.5 w-48 max-w-full" />
                <Skeleton class="h-3 w-64 max-w-full" />
              </div>
            </div>
          </div>

          <div
            v-else-if="loadError"
            class="mt-4 rounded-xl border border-red-400/15 bg-red-400/[0.05] p-4 text-sm text-red-200 light:border-red-200 light:bg-red-50 light:text-red-800"
            role="alert"
          >
            <div class="flex items-start gap-2">
              <AlertTriangle
                class="mt-0.5 h-4 w-4 shrink-0"
                aria-hidden="true"
              />
              <div>
                <p class="font-medium">Properties unavailable</p>
                <p class="mt-1 text-xs opacity-75">{{ loadError }}</p>
                <Button
                  class="mt-3"
                  variant="outline"
                  size="sm"
                  :disabled="activeAction !== null"
                  @click="retryProperties"
                >
                  Try again
                </Button>
              </div>
            </div>
          </div>

          <div
            v-else-if="!websiteProperties.length"
            class="mt-4 rounded-xl border border-dashed border-white/[0.1] px-5 py-8 text-center light:border-gray-300"
          >
            <Globe2
              class="mx-auto h-6 w-6 text-white/25 light:text-gray-400"
              aria-hidden="true"
            />
            <p class="mt-3 text-sm font-medium text-white light:text-gray-900">
              No verified properties found
            </p>
            <p
              class="mx-auto mt-1 max-w-md text-xs leading-5 text-white/60 light:text-gray-500"
            >
              Verify a website in Google Search Console, then return here and
              refresh the property list.
            </p>
          </div>

          <div v-else class="mt-4 space-y-2" data-testid="gsc-property-list">
            <label
              v-for="property in websiteProperties"
              :key="property.id"
              :for="`gsc-property-${property.id}`"
              class="group flex min-h-[64px] cursor-pointer items-start gap-3 rounded-xl border border-white/[0.07] bg-white/[0.018] p-3.5 transition-colors hover:border-sky-300/20 hover:bg-sky-300/[0.035] light:border-gray-200 light:bg-white light:hover:border-sky-300 light:hover:bg-sky-50/40"
            >
              <Checkbox
                :id="`gsc-property-${property.id}`"
                :checked="selectedPropertyIds.includes(property.id)"
                :disabled="!canWrite || activeAction !== null"
                class="mt-1"
                @update:checked="setPropertySelected(property.id, $event)"
              />
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span
                    class="truncate text-sm font-medium text-white light:text-gray-900"
                  >
                    {{ property.display_name || property.site_url }}
                  </span>
                  <Badge variant="secondary" class="px-1.5 py-0 text-[9px]">
                    {{ propertyTypeLabel(property.property_type) }}
                  </Badge>
                </div>
                <p
                  class="mt-1 truncate text-xs text-white/60 light:text-gray-500"
                >
                  {{ property.site_url }}
                </p>
                <div
                  class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[10px] text-white/55 light:text-gray-500"
                >
                  <span>{{
                    property.permission_level.replace(/_/g, " ")
                  }}</span>
                  <span
                    >Last sync: {{ formatDate(property.last_synced_at) }}</span
                  >
                </div>
              </div>
            </label>
          </div>

          <p
            v-if="websiteProperties.length && selectedCount === 0"
            class="mt-3 text-xs text-amber-300 light:text-amber-700"
            role="status"
          >
            Select at least one verified property before saving.
          </p>
        </section>

        <section
          v-else
          class="mt-6 rounded-2xl border border-dashed border-white/[0.1] px-5 py-8 text-center light:border-gray-300"
        >
          <Globe2
            class="mx-auto h-7 w-7 text-white/25 light:text-gray-400"
            aria-hidden="true"
          />
          <h3 class="mt-3 text-sm font-semibold text-white light:text-gray-900">
            Connect Google to choose a property
          </h3>
          <p
            class="mx-auto mt-1 max-w-md text-xs leading-5 text-white/60 light:text-gray-500"
          >
            Authorize a Google account that can access at least one verified
            website property.
          </p>
        </section>

        <div
          class="mt-5 flex items-start gap-2.5 rounded-xl border border-sky-300/10 bg-sky-300/[0.035] px-3.5 py-3 text-xs leading-5 text-sky-100/65 light:border-sky-200 light:bg-sky-50 light:text-sky-900"
          role="note"
        >
          <AlertTriangle
            class="mt-0.5 h-3.5 w-3.5 shrink-0 text-sky-300 light:text-sky-700"
            aria-hidden="true"
          />
          <p>
            Search Console reports Google clicks, impressions, CTR and average
            position. It does not report website visits or sessions; those
            require Google Analytics.
          </p>
        </div>
      </div>

      <DialogFooter
        class="flex-col-reverse gap-2 border-t border-white/[0.08] bg-white/[0.015] px-5 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-6 light:border-gray-200 light:bg-gray-50/60"
      >
        <Button
          v-if="hasGoogleAuthorization && canWrite"
          variant="ghost"
          size="sm"
          class="text-red-300 hover:bg-red-500/10 hover:text-red-200 light:text-red-700"
          :disabled="activeAction !== null"
          @click="disconnectOpen = true"
        >
          <Trash2 class="h-3.5 w-3.5" aria-hidden="true" />
          Disconnect
        </Button>
        <p
          v-else-if="!canWrite"
          class="text-xs text-white/60 light:text-gray-500"
        >
          View-only access — ask a workspace administrator to make changes.
        </p>
        <span v-else />

        <div
          v-if="hasGoogleAuthorization"
          class="flex flex-wrap justify-end gap-2"
        >
          <Button
            v-if="canWrite"
            variant="outline"
            size="sm"
            :loading="activeAction === 'test'"
            :disabled="activeAction !== null || !providerOperationsAvailable"
            @click="testConnection"
          >
            <Activity class="h-3.5 w-3.5" aria-hidden="true" />
            Test
          </Button>
          <Button v-if="canViewAnalytics" as-child variant="outline" size="sm">
            <RouterLink to="/analytics/search-visibility">
              Open dashboard
              <ArrowUpRight class="h-3.5 w-3.5" aria-hidden="true" />
            </RouterLink>
          </Button>
          <Button
            v-if="canWrite"
            data-testid="gsc-save-properties"
            size="sm"
            :loading="activeAction === 'save'"
            :disabled="
              activeAction !== null || selectedCount === 0 || !selectionChanged
            "
            @click="saveProperties"
          >
            <CheckCircle2 class="h-3.5 w-3.5" aria-hidden="true" />
            Save properties
          </Button>
        </div>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <ConfirmDialog
    v-model:open="disconnectOpen"
    title="Disconnect Google Search Console?"
    description="This removes this workspace's encrypted Google token and stops Search Visibility. It does not revoke the Google account's app grant, which may also be used by another workspace."
    confirm-label="Disconnect Google"
    variant="destructive"
    :is-submitting="activeAction === 'disconnect'"
    @confirm="disconnect"
  />
</template>
