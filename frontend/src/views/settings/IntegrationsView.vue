<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Activity,
  AtSign,
  Check,
  CheckCircle2,
  AlertCircle,
  CloudCog,
  Clock3,
  Copy,
  ExternalLink,
  KeyRound,
  Mail,
  MessageCircleMore,
  MessagesSquare,
  Music2,
  RefreshCw,
  Save,
  Search,
  ShieldCheck,
  Sparkles,
  Trash2,
  Wifi,
  WifiOff,
} from "lucide-vue-next";
import { toast } from "vue-sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { ConfirmDialog, PageHeader } from "@/components/shared";
import IntegrationProviderCard, {
  type ProviderCardDefinition,
} from "@/components/integrations/IntegrationProviderCard.vue";
import GoogleSearchConsoleDialog from "@/components/integrations/GoogleSearchConsoleDialog.vue";
import {
  integrationReadiness as buildIntegrationReadiness,
  type IntegrationReadinessState,
} from "@/lib/integrationReadiness";
import {
  integrationsService,
  type IntegrationProvider,
  type IntegrationState,
  type IntegrationStatus,
} from "@/services/integrations";
import {
  organizationEntitlementSupportService,
  type ThreadsPublicEngagementSupportStatusResponse,
} from "@/services/productSuite";
import { useAuthStore } from "@/stores/auth";
import { useOrganizationsStore } from "@/stores/organizations";

const THREADS_PUBLIC_ENGAGEMENT_ENTITLEMENT =
  "threads.public_engagement.enabled";

interface FieldOption {
  value: string;
  label: string;
  disabled?: boolean;
}

interface IntegrationField {
  key: string;
  label: string;
  description: string;
  placeholder?: string;
  kind?: "text" | "url" | "number" | "select";
  options?: FieldOption[];
  readOnly?: boolean;
  step?: string;
  min?: string;
  max?: string;
}

interface IntegrationDefinition extends ProviderCardDefinition {
  provider: IntegrationProvider;
  fields: IntegrationField[];
  secrets: IntegrationField[];
  connectLabel?: string;
  unavailableCopy?: string;
  preparationOnly?: boolean;
}

const reviewOptions: FieldOption[] = [
  { value: "not_submitted", label: "Not submitted" },
  { value: "pending", label: "In review" },
  { value: "approved", label: "Approved" },
  { value: "rejected", label: "Rejected" },
];

const threadsReviewOptions: FieldOption[] = [
  { value: "not_submitted", label: "Not submitted" },
  { value: "pending", label: "In review" },
  { value: "approved", label: "Approved by platform owner", disabled: true },
  { value: "rejected", label: "Rejected" },
];

const definitions: Record<IntegrationProvider, IntegrationDefinition> = {
  meta: {
    provider: "meta",
    name: "WhatsApp & Meta",
    eyebrow: "Business messaging",
    description:
      "Connect WhatsApp Business accounts through a shared, verified Meta application.",
    icon: MessageCircleMore,
    accent: "#55c77b",
    glow: "linear-gradient(145deg, #164d35, #287953)",
    capabilities: [
      "WhatsApp Cloud API",
      "Embedded signup",
      "Meta webhooks",
      "Instagram / Messenger relay",
      "Business calling",
    ],
    connectLabel: "Connect with Meta",
    fields: [
      {
        key: "app_id",
        label: "Meta App ID",
        description: "The numeric ID from your Meta developer application.",
        placeholder: "123456789012345",
      },
      {
        key: "config_id",
        label: "Embedded signup configuration ID",
        description:
          "The Facebook Login for Business configuration used during signup.",
        placeholder: "987654321098765",
      },
      {
        key: "api_version",
        label: "Graph API version",
        description: "Server-managed Graph API version used by ReReply.",
        placeholder: "v21.0",
        readOnly: true,
      },
      {
        key: "webhook_callback_path",
        label: "Meta webhook callback URL",
        description:
          "Copy this exact URL into the WhatsApp webhook configuration in Meta for Developers. Workspace-managed apps include a public tenant selector in the URL.",
        kind: "url",
        readOnly: true,
      },
    ],
    secrets: [
      {
        key: "app_secret",
        label: "Meta App Secret",
        description:
          "Write-only. Used server-side for OAuth exchange and webhook verification.",
        placeholder: "Enter a new app secret",
      },
      {
        key: "webhook_verify_token",
        label: "Webhook Verify Token",
        description:
          "Write-only and encrypted. Use this same value in Meta for Developers; it never returns to the browser after saving.",
        placeholder: "Enter a strong webhook verify token",
      },
    ],
  },
  threads: {
    provider: "threads",
    name: "Threads",
    eyebrow: "Public engagement",
    description:
      "Bring public replies and mentions into the inbox with target-safe responses.",
    icon: AtSign,
    accent: "#f2f2f2",
    glow: "linear-gradient(145deg, #242424, #565656)",
    capabilities: ["Replies", "Mentions", "Public engagement"],
    connectLabel: "Connect Threads",
    unavailableCopy:
      "Public replies and mentions only. Threads direct messages and standalone posts are not supported.",
    fields: [
      {
        key: "app_id",
        label: "Threads App ID",
        description:
          "Use a dedicated Meta Threads application. One App ID binds to one ReReply workspace and one Threads profile.",
        placeholder: "123456789012345",
      },
      {
        key: "redirect_uri",
        label: "OAuth redirect URI",
        description:
          "Register this exact callback URL in the Threads application in Meta for Developers.",
        placeholder:
          "https://app.example.com/api/integrations/threads/callback",
        kind: "url",
      },
      {
        key: "app_review_status",
        label: "Meta app review",
        description:
          "Track submission state here. Production approval is recorded separately by the platform owner with evidence.",
        kind: "select",
        options: threadsReviewOptions,
      },
      {
        key: "deauthorization_callback_url",
        label: "Uninstall / deauthorization callback URL",
        description:
          "Copy this exact non-secret URL into the Threads app uninstall or deauthorization callback setting.",
        kind: "url",
        readOnly: true,
      },
      {
        key: "data_deletion_callback_url",
        label: "Data deletion request URL",
        description:
          "Copy this exact non-secret URL into the Threads app data deletion request setting.",
        kind: "url",
        readOnly: true,
      },
      {
        key: "account_webhook_callback_url",
        label: "Connected account webhook callback URL",
        description:
          "Available after OAuth creates the workspace account. Copy the exact URL into the Threads webhook subscription.",
        placeholder: "Available after Threads OAuth",
        kind: "url",
        readOnly: true,
      },
    ],
    secrets: [
      {
        key: "app_secret",
        label: "Threads App Secret",
        description:
          "Write-only and encrypted. Existing values can never be revealed.",
        placeholder: "Enter a new app secret",
      },
      {
        key: "webhook_verify_token",
        label: "Webhook verify token",
        description:
          "Enter the same strong secret (at least 16 characters) in the Meta Threads Webhooks callback setup.",
        placeholder: "Enter a new webhook verify token",
      },
    ],
  },
  tiktok: {
    provider: "tiktok",
    name: "TikTok Business",
    eyebrow: "Business messaging",
    description:
      "Prepare TikTok Business Messaging for direct-message webhooks and agent replies.",
    icon: Music2,
    accent: "#25f4ee",
    glow: "linear-gradient(145deg, #123e41, #3a263d)",
    capabilities: ["Business DMs", "Media", "Conversation webhooks"],
    connectLabel: "Connect TikTok Business",
    preparationOnly: true,
    unavailableCopy:
      "OAuth unlocks only after TikTok grants Business Messaging API access.",
    fields: [
      {
        key: "client_id",
        label: "TikTok client ID",
        description:
          "The client ID issued to your approved API for Business application.",
        placeholder: "Enter client ID",
      },
      {
        key: "redirect_uri",
        label: "Future OAuth redirect URI",
        description:
          "Preparation record only. ReReply does not expose a TikTok callback until an approved adapter is installed.",
        placeholder: "https://future-approved-callback.example.com/tiktok",
        kind: "url",
      },
      {
        key: "approval_status",
        label: "TikTok API approval",
        description: "Track the Business Messaging permission review.",
        kind: "select",
        options: reviewOptions,
      },
    ],
    secrets: [
      {
        key: "client_secret",
        label: "TikTok client secret",
        description:
          "Write-only and encrypted. Used only for server-side token exchange.",
        placeholder: "Enter a new client secret",
      },
    ],
  },
  qwen: {
    provider: "qwen",
    name: "Qwen Copilot",
    eyebrow: "AI intelligence",
    description:
      "Control the model connection that powers suggested replies and CRM assistance.",
    icon: Sparkles,
    accent: "#d8a7ff",
    glow: "linear-gradient(145deg, #47315f, #6b4988)",
    capabilities: ["Reply drafting", "CRM assistance", "Knowledge context"],
    fields: [
      {
        key: "model",
        label: "Model",
        description: "The Qwen-compatible model deployed for this workspace.",
        placeholder: "qwen-plus",
      },
      {
        key: "max_tokens",
        label: "Maximum output tokens",
        description: "Upper limit for a single generated response.",
        placeholder: "1024",
        kind: "number",
        min: "64",
        max: "4000",
        step: "1",
      },
      {
        key: "temperature",
        label: "Temperature",
        description: "Lower values produce more predictable responses.",
        placeholder: "0.3",
        kind: "number",
        min: "0",
        max: "2",
        step: "0.1",
      },
      {
        key: "endpoint_region",
        label: "Endpoint region",
        description:
          "Select an official DashScope region. ReReply computes the allowlisted endpoint; arbitrary URLs are not accepted.",
        kind: "select",
        options: [
          { value: "platform", label: "Platform default" },
          { value: "singapore", label: "Singapore / International" },
          { value: "us", label: "United States" },
          { value: "china_beijing", label: "China (Beijing)" },
        ],
      },
      {
        key: "base_url",
        label: "API endpoint",
        description:
          "Computed from the approved region above. It cannot be edited directly.",
        kind: "url",
        readOnly: true,
      },
    ],
    secrets: [
      {
        key: "api_key",
        label: "Qwen API key",
        description:
          "Write-only and encrypted. Leave blank to keep the stored key.",
        placeholder: "Enter a new API key",
      },
    ],
  },
  google_search_console: {
    provider: "google_search_console",
    name: "Google Search Console",
    eyebrow: "Search intelligence",
    description:
      "Measure how verified website properties perform across Google Search surfaces.",
    icon: Search,
    accent: "#4285f4",
    glow: "linear-gradient(145deg, #173f6d, #2c67a2)",
    capabilities: [
      "Google clicks",
      "Impressions & CTR",
      "Queries & pages",
      "Average position",
    ],
    resourceLabel: "Properties",
    credentialSummary: "Google OAuth",
    connectLabel: "Connect Google",
    fields: [],
    secrets: [],
  },
  email: {
    provider: "email",
    name: "Email",
    eyebrow: "Owned channel",
    description:
      "Monitor connected mailboxes and route customer email into the unified inbox.",
    icon: Mail,
    accent: "#7dd3fc",
    glow: "linear-gradient(145deg, #164e63, #25677c)",
    capabilities: ["Inbound email", "Agent replies", "Conversation history"],
    readOnly: true,
    managePath: "/inbox",
    manageLabel: "Open inbox",
    fields: [],
    secrets: [],
  },
  webchat: {
    provider: "webchat",
    name: "Webchat",
    eyebrow: "Owned channel",
    description:
      "Monitor website chat endpoints and their real-time inbox activity.",
    icon: MessagesSquare,
    accent: "#cbd49a",
    glow: "linear-gradient(145deg, #454b2c, #697046)",
    capabilities: ["Website chat", "Realtime messages", "Agent handoff"],
    readOnly: true,
    managePath: "/inbox",
    manageLabel: "Open inbox",
    fields: [],
    secrets: [],
  },
};

const providerOrder: IntegrationProvider[] = [
  "meta",
  "threads",
  "tiktok",
  "qwen",
  "google_search_console",
  "email",
  "webchat",
];

const router = useRouter();
const route = useRoute();
const authStore = useAuthStore();
const organizationsStore = useOrganizationsStore();
const integrations = ref<IntegrationState[]>([]);
const isLoading = ref(true);
const loadError = ref("");
const isRefreshing = ref(false);
const selectedProvider = ref<IntegrationProvider | null>(null);
const isDialogOpen = ref(false);
const isSearchConsoleDialogOpen = ref(false);
const isRemoveOpen = ref(false);
const activeAction = ref<
  "save" | "test" | "connect" | "remove" | "entitlement" | null
>(null);
const configDraft = reactive<Record<string, string>>({});
const secretDraft = reactive<Record<string, string>>({});
const enabledDraft = ref(false);
const threadsSupportReason = ref("");
const threadsSupportRevokeReason = ref("");
const threadsSupportStatus = ref<ThreadsPublicEngagementSupportStatusResponse | null>(
  null,
);
const threadsSupportStatusState = ref<
  "idle" | "loading" | "ready" | "error"
>("idle");
const threadsSupportStatusTargetOrganizationId = ref("");
let threadsSupportStatusRequestID = 0;
const canWrite = computed(() =>
  authStore.hasPermission("settings.integrations", "write"),
);

const selectedIntegration = computed(() =>
  integrations.value.find((item) => item.provider === selectedProvider.value),
);
const selectedDefinition = computed(() =>
  selectedProvider.value ? definitions[selectedProvider.value] : undefined,
);
const selectedReadiness = computed(() =>
  selectedIntegration.value
    ? buildIntegrationReadiness(selectedIntegration.value)
    : [],
);
const threadsPublicEngagementEnabled = computed(() =>
  authStore.hasProductEntitlement(THREADS_PUBLIC_ENGAGEMENT_ENTITLEMENT),
);
const threadsEntitlementsAvailable = computed(
  () =>
    authStore.productEntitlementsLoaded &&
    authStore.productEntitlementMode !== "unavailable",
);
const threadsSupportTargetOrganizationId = computed(() =>
  (authStore.user?.is_super_admin
    ? organizationsStore.selectedOrgId || authStore.organizationId
    : authStore.organizationId
  ).trim(),
);
const isThreadsEntitlementLocked = computed(
  () =>
    selectedIntegration.value?.provider === "threads" &&
    (!authStore.productEntitlementsLoaded ||
      !threadsPublicEngagementEnabled.value),
);
const showThreadsEntitlementSupport = computed(
  () =>
    selectedIntegration.value?.provider === "threads" &&
    Boolean(authStore.user?.is_super_admin),
);
const isThreadsSupportStatusCurrent = computed(
  () =>
    threadsSupportStatusState.value === "ready" &&
    Boolean(threadsSupportStatus.value) &&
    Boolean(threadsSupportTargetOrganizationId.value) &&
    threadsSupportStatusTargetOrganizationId.value ===
      threadsSupportTargetOrganizationId.value &&
    threadsSupportStatus.value?.organization_id ===
      threadsSupportTargetOrganizationId.value,
);
const activeThreadsSupportOverride = computed(() => {
  const status = threadsSupportStatus.value;
  if (
    !isThreadsSupportStatusCurrent.value ||
    !status?.active ||
    status.source !== "support" ||
    !status.override_id?.trim()
  ) {
    return null;
  }
  return status;
});
const isThreadsSupportStatusInconsistent = computed(
  () =>
    isThreadsSupportStatusCurrent.value &&
    Boolean(threadsSupportStatus.value?.active) &&
    (!threadsEntitlementsAvailable.value ||
      !threadsPublicEngagementEnabled.value),
);
const isThreadsSupportStatusUnavailable = computed(
  () =>
    threadsSupportStatusState.value === "error" ||
    !threadsEntitlementsAvailable.value ||
    isThreadsSupportStatusInconsistent.value,
);
const showThreadsEntitlementEnableSupport = computed(
  () =>
    showThreadsEntitlementSupport.value &&
    isThreadsSupportStatusCurrent.value &&
    !isThreadsSupportStatusUnavailable.value &&
    !threadsSupportStatus.value?.active &&
    !threadsPublicEngagementEnabled.value,
);
const showThreadsEntitlementRevokeSupport = computed(
  () =>
    showThreadsEntitlementSupport.value &&
    isThreadsSupportStatusCurrent.value &&
    !isThreadsSupportStatusUnavailable.value &&
    threadsPublicEngagementEnabled.value &&
    Boolean(activeThreadsSupportOverride.value),
);
const showThreadsEntitlementWithoutSupportOverride = computed(
  () =>
    showThreadsEntitlementSupport.value &&
    isThreadsSupportStatusCurrent.value &&
    !isThreadsSupportStatusUnavailable.value &&
    threadsPublicEngagementEnabled.value &&
    !threadsSupportStatus.value?.active,
);
const canEnableThreadsEntitlement = computed(
  () =>
    showThreadsEntitlementEnableSupport.value &&
    Boolean(threadsSupportTargetOrganizationId.value) &&
    threadsSupportReason.value.trim().length > 0 &&
    activeAction.value === null,
);
const canRevokeThreadsEntitlementSupport = computed(
  () =>
    showThreadsEntitlementRevokeSupport.value &&
    Boolean(threadsSupportTargetOrganizationId.value) &&
    threadsSupportRevokeReason.value.trim().length > 0 &&
    activeAction.value === null,
);
function isApprovedThreadsReviewStatus(value: unknown) {
  return value === "approved";
}
function isThreadsReviewAccessAuthorized() {
  const mode = selectedIntegration.value?.config.app_review_access_mode;
  if (mode === "approved") {
    return isApprovedThreadsReviewStatus(configDraft.app_review_status);
  }
  return (
    mode === "development_testing" &&
    ["not_submitted", "pending"].includes(configDraft.app_review_status ?? "")
  );
}
const isThreadsReviewLocked = computed(
  () =>
    selectedIntegration.value?.provider === "threads" &&
    !isThreadsReviewAccessAuthorized(),
);
const isActivationLocked = computed(() => {
  const integration = selectedIntegration.value;
  if (!integration) return false;
  return (
    isThreadsEntitlementLocked.value ||
    isThreadsReviewLocked.value ||
    integration.status === "adapter_unavailable" ||
    (integration.provider !== "threads" &&
      integration.status === "approval_required") ||
    (integration.provider === "tiktok" && !integration.oauth.available)
  );
});
const orderedIntegrations = computed(() =>
  providerOrder
    .map((provider) =>
      integrations.value.find((item) => item.provider === provider),
    )
    .filter((item): item is IntegrationState => Boolean(item)),
);
const connectedCount = computed(
  () => integrations.value.filter((item) => item.status === "connected").length,
);
const attentionCount = computed(
  () =>
    integrations.value.filter((item) =>
      ["degraded", "approval_required", "adapter_unavailable"].includes(
        item.status,
      ),
    ).length,
);
const configuredCount = computed(
  () => integrations.value.filter((item) => item.configured).length,
);
const activeAccountCount = computed(() =>
  integrations.value.reduce(
    (total, item) => total + (item.connection?.active_count ?? 0),
    0,
  ),
);
const configuredSecrets = computed(() => {
  const states = integrations.value.flatMap((item) =>
    Object.values(item.credentials ?? {}),
  );
  return {
    configured: states.filter((item) => item.configured).length,
    total: states.length,
  };
});

function isWorkspaceMetaDraft() {
  const integration = selectedIntegration.value;
  if (!integration || integration.provider !== "meta") return false;
  if (String(integration.config?.management_mode ?? "platform") === "workspace")
    return true;
  const appIDChanged =
    (configDraft.app_id ?? "").trim() !==
    String(integration.config?.app_id ?? "").trim();
  const configIDChanged =
    (configDraft.config_id ?? "").trim() !==
    String(integration.config?.config_id ?? "").trim();
  return (
    appIDChanged || configIDChanged || Boolean(secretDraft.app_secret?.trim())
  );
}

function isSecretReadOnly(field: IntegrationField) {
  return (
    selectedIntegration.value?.provider === "meta" &&
    field.key === "webhook_verify_token" &&
    !isWorkspaceMetaDraft()
  );
}

function defaultIntegration(provider: IntegrationProvider): IntegrationState {
  return {
    provider,
    display_name: definitions[provider].name,
    status: "not_configured",
    enabled: false,
    configured: false,
    read_only: definitions[provider].readOnly,
    config: {},
    credentials: Object.fromEntries(
      definitions[provider].secrets.map((field) => [
        field.key,
        { configured: false },
      ]),
    ),
    connection: { account_count: 0, active_count: 0 },
    oauth: { supported: false, available: false },
    test_supported: false,
  };
}

function normalizeIntegration(value: IntegrationState): IntegrationState {
  const fallback = defaultIntegration(value.provider);
  return {
    ...fallback,
    ...value,
    config: value.config ?? {},
    credentials: { ...fallback.credentials, ...(value.credentials ?? {}) },
    connection: { ...fallback.connection, ...(value.connection ?? {}) },
    oauth: { ...fallback.oauth, ...(value.oauth ?? {}) },
  };
}

function getErrorMessage(error: unknown, fallback: string) {
  if (typeof error !== "object" || error === null) return fallback;
  const response = (error as { response?: { data?: unknown } }).response;
  const payload = response?.data as
    | { error?: string | { message?: string }; message?: string }
    | undefined;
  if (typeof payload?.error === "string") return payload.error;
  if (typeof payload?.error === "object" && payload.error?.message)
    return payload.error.message;
  if (payload?.message) return payload.message;
  return fallback;
}

function resetThreadsSupportStatus() {
  threadsSupportStatusRequestID += 1;
  threadsSupportStatus.value = null;
  threadsSupportStatusState.value = "idle";
  threadsSupportStatusTargetOrganizationId.value = "";
}

function invalidateThreadsSupportStatus(organizationID: string) {
  threadsSupportStatusRequestID += 1;
  threadsSupportStatus.value = null;
  threadsSupportStatusState.value = "error";
  threadsSupportStatusTargetOrganizationId.value = organizationID;
}

function isValidThreadsSupportStatus(
  value: unknown,
  organizationID: string,
): value is ThreadsPublicEngagementSupportStatusResponse {
  if (!value || typeof value !== "object") return false;
  const status = value as Record<string, unknown>;
  if (
    status.organization_id !== organizationID ||
    status.entitlement_key !== THREADS_PUBLIC_ENGAGEMENT_ENTITLEMENT ||
    typeof status.active !== "boolean"
  ) {
    return false;
  }
  if (status.source !== undefined && status.source !== "support") return false;
  if (
    status.active &&
    (status.source !== "support" ||
      typeof status.override_id !== "string" ||
      !status.override_id.trim())
  ) {
    return false;
  }
  return true;
}

async function refreshThreadsSupportStatus(
  organizationID = threadsSupportTargetOrganizationId.value,
): Promise<boolean> {
  if (
    !organizationID ||
    !authStore.user?.is_super_admin ||
    selectedProvider.value !== "threads" ||
    !isDialogOpen.value ||
    threadsSupportTargetOrganizationId.value !== organizationID
  ) {
    resetThreadsSupportStatus();
    return false;
  }

  const requestID = ++threadsSupportStatusRequestID;
  threadsSupportStatus.value = null;
  threadsSupportStatusState.value = "loading";
  threadsSupportStatusTargetOrganizationId.value = organizationID;

  try {
    const response =
      await organizationEntitlementSupportService.getThreadsPublicEngagementSupportStatus(
        organizationID,
      );
    if (
      requestID !== threadsSupportStatusRequestID ||
      threadsSupportTargetOrganizationId.value !== organizationID ||
      selectedProvider.value !== "threads" ||
      !isDialogOpen.value
    ) {
      return false;
    }

    const status = response.data?.data;
    if (!isValidThreadsSupportStatus(status, organizationID)) {
      invalidateThreadsSupportStatus(organizationID);
      return false;
    }

    threadsSupportStatus.value = status;
    threadsSupportStatusState.value = "ready";
    return true;
  } catch {
    if (
      requestID === threadsSupportStatusRequestID &&
      threadsSupportTargetOrganizationId.value === organizationID
    ) {
      invalidateThreadsSupportStatus(organizationID);
    }
    return false;
  }
}

async function loadIntegrations(options: { quiet?: boolean } = {}) {
  if (options.quiet) isRefreshing.value = true;
  else isLoading.value = true;
  loadError.value = "";
  try {
    const response = await integrationsService.list();
    const returned = response.data?.data?.integrations ?? [];
    const returnedByProvider = new Map(
      returned.map((item) => [item.provider, normalizeIntegration(item)]),
    );
    integrations.value = providerOrder.map(
      (provider) =>
        returnedByProvider.get(provider) ?? defaultIntegration(provider),
    );
  } catch (error) {
    loadError.value = getErrorMessage(
      error,
      "We could not load integration status.",
    );
  } finally {
    isLoading.value = false;
    isRefreshing.value = false;
  }
}

function openConfiguration(provider: IntegrationProvider) {
  const integration = integrations.value.find(
    (item) => item.provider === provider,
  );
  if (!integration || integration.read_only || definitions[provider].readOnly)
    return;

  selectedProvider.value = provider;
  if (provider === "google_search_console") {
    isSearchConsoleDialogOpen.value = true;
    return;
  }
  enabledDraft.value = integration.enabled;
  Object.keys(configDraft).forEach((key) => delete configDraft[key]);
  Object.keys(secretDraft).forEach((key) => delete secretDraft[key]);

  definitions[provider].fields.forEach((field) => {
    const value = integration.config?.[field.key];
    const displayValue =
      provider === "meta" && field.key === "webhook_callback_path"
        ? publicWebhookCallbackURL(String(value ?? ""))
        : value;
    configDraft[field.key] =
      displayValue === undefined || displayValue === null || displayValue === ""
        ? field.kind === "select"
          ? field.options?.[0]?.value || ""
          : ""
        : String(displayValue);
  });
  definitions[provider].secrets.forEach((field) => {
    secretDraft[field.key] = "";
  });
  isDialogOpen.value = true;
  if (provider === "threads") {
    void refreshThreadsSupportStatus();
  } else {
    resetThreadsSupportStatus();
  }
}

function publicWebhookCallbackURL(path: string) {
  const normalizedPath = path.trim();
  if (!normalizedPath) return "";
  if (/^https:\/\//i.test(normalizedPath)) return normalizedPath;
  const basePath = String((window as any).__BASE_PATH__ ?? "").replace(
    /\/$/,
    "",
  );
  return `${window.location.origin}${basePath}${normalizedPath.startsWith("/") ? normalizedPath : `/${normalizedPath}`}`;
}

async function copyWebhookCallback() {
  const callback = configDraft.webhook_callback_path?.trim();
  if (!callback) return;
  try {
    await navigator.clipboard.writeText(callback);
    toast.success("Meta webhook callback copied");
  } catch {
    toast.error("Could not copy the webhook callback");
  }
}

async function copyThreadsSetupURL(key: string, label: string) {
  const callback = configDraft[key] ?? "";
  if (!callback.trim()) return;
  try {
    await navigator.clipboard.writeText(callback);
    toast.success(`${label} copied`);
  } catch {
    toast.error(`Could not copy ${label.toLowerCase()}`);
  }
}

function replaceIntegration(next: IntegrationState) {
  const normalized = normalizeIntegration(next);
  const index = integrations.value.findIndex(
    (item) => item.provider === normalized.provider,
  );
  if (index === -1) integrations.value.push(normalized);
  else integrations.value.splice(index, 1, normalized);
}

function typedConfigValue(field: IntegrationField, value: string): unknown {
  if (field.kind !== "number") return value.trim();
  if (value.trim() === "") return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : value;
}

async function saveConfiguration() {
  if (!canWrite.value) return;
  const integration = selectedIntegration.value;
  const definition = selectedDefinition.value;
  if (!integration || !definition) return;

  activeAction.value = "save";
  try {
    const workspaceMetaDraft = isWorkspaceMetaDraft();
    const config = Object.fromEntries(
      definition.fields
        .filter((field) => {
          if (field.readOnly) return false;
          if (
            integration.provider === "meta" &&
            ["app_id", "config_id"].includes(field.key)
          ) {
            return workspaceMetaDraft;
          }
          return true;
        })
        .map((field) => [
          field.key,
          typedConfigValue(field, configDraft[field.key] ?? ""),
        ]),
    );
    const credentials = Object.fromEntries(
      definition.secrets
        .map((field) => [field.key, (secretDraft[field.key] ?? "").trim()])
        .filter(([, value]) => value !== ""),
    );
    const preserveThreadsEnabledState =
      integration.provider === "threads" &&
      isThreadsEntitlementLocked.value &&
      !isThreadsReviewLocked.value;
    const response = await integrationsService.update(integration.provider, {
      ...(preserveThreadsEnabledState
        ? {}
        : {
            enabled: isActivationLocked.value ? false : enabledDraft.value,
          }),
      config,
      ...(Object.keys(credentials).length ? { credentials } : {}),
    });
    const nextIntegration = response.data.data;
    replaceIntegration(nextIntegration);
    definition.secrets.forEach((field) => {
      secretDraft[field.key] = "";
    });
    toast.success(`${definition.name} configuration saved`);
  } catch (error) {
    toast.error(getErrorMessage(error, `Could not save ${definition.name}.`));
  } finally {
    activeAction.value = null;
  }
}

async function enableThreadsPublicEngagement() {
  const organizationID = threadsSupportTargetOrganizationId.value;
  const reason = threadsSupportReason.value.trim();
  if (!canEnableThreadsEntitlement.value || !organizationID || !reason) return;

  activeAction.value = "entitlement";
  try {
    const response =
      await organizationEntitlementSupportService.enableThreadsPublicEngagement(
        organizationID,
        { reason },
      );
    if (!response.data.data.effective_enabled) {
      throw new Error("Threads public engagement did not become effective");
    }

    const [entitlementsRefreshed, , supportStatusRefreshed] = await Promise.all([
      authStore.fetchProductEntitlements(),
      loadIntegrations({ quiet: true }),
      refreshThreadsSupportStatus(organizationID),
    ]);
    if (
      !entitlementsRefreshed ||
      !authStore.hasProductEntitlement(THREADS_PUBLIC_ENGAGEMENT_ENTITLEMENT)
    ) {
      toast.error(
        "The entitlement was enabled, but its status could not be refreshed. The Enabled switch remains locked.",
      );
      return;
    }

    const activeSupportOverride = activeThreadsSupportOverride.value;
    if (
      !supportStatusRefreshed ||
      !activeSupportOverride ||
      activeSupportOverride.override_id !== response.data.data.override_id
    ) {
      invalidateThreadsSupportStatus(organizationID);
      toast.error(
        "The entitlement was enabled, but the active support override could not be verified. Support actions remain locked.",
      );
      return;
    }

    threadsSupportReason.value = "";
    toast.success(
      response.data.data.created
        ? "Threads public engagement entitlement enabled"
        : "Threads public engagement entitlement is already enabled",
    );
  } catch (error) {
    toast.error(
      getErrorMessage(
        error,
        "Could not enable Threads public engagement entitlement.",
      ),
    );
  } finally {
    activeAction.value = null;
  }
}

async function revokeThreadsPublicEngagementSupport() {
  const organizationID = threadsSupportTargetOrganizationId.value;
  const reason = threadsSupportRevokeReason.value.trim();
  const supportOverride = activeThreadsSupportOverride.value;
  const overrideID = supportOverride?.override_id?.trim() ?? "";
  if (
    !canRevokeThreadsEntitlementSupport.value ||
    !organizationID ||
    !reason ||
    !overrideID ||
    supportOverride?.organization_id !== organizationID ||
    threadsSupportStatusTargetOrganizationId.value !== organizationID
  )
    return;

  activeAction.value = "entitlement";
  try {
    const response =
      await organizationEntitlementSupportService.revokeThreadsPublicEngagementSupport(
        organizationID,
        { override_id: overrideID, reason },
      );

    const [entitlementsRefreshed, , supportStatusRefreshed] = await Promise.all([
      authStore.fetchProductEntitlements(),
      loadIntegrations({ quiet: true }),
      refreshThreadsSupportStatus(organizationID),
    ]);
    if (
      !supportStatusRefreshed ||
      !isThreadsSupportStatusCurrent.value ||
      threadsSupportStatus.value?.active
    ) {
      invalidateThreadsSupportStatus(organizationID);
      toast.error(
        "The support entitlement was revoked, but the updated support override status could not be verified. Support actions remain locked.",
      );
      return;
    }

    threadsSupportRevokeReason.value = "";
    if (!entitlementsRefreshed) {
      toast.error(
        "The support entitlement was revoked, but its status could not be refreshed. Threads activation remains locked.",
      );
      return;
    }

    if (
      authStore.hasProductEntitlement(THREADS_PUBLIC_ENGAGEMENT_ENTITLEMENT)
    ) {
      toast.info(
        "Threads support entitlement revoked; public engagement remains enabled by another entitlement.",
      );
      return;
    }
    toast.success(
      response.data.data.revoked
        ? "Threads support entitlement revoked"
        : "Threads support entitlement was already revoked",
    );
  } catch (error) {
    const responseStatus =
      typeof error === "object" && error !== null
        ? (error as { response?: { status?: number } }).response?.status
        : undefined;
    if (responseStatus === 409) {
      threadsSupportRevokeReason.value = "";
      const supportStatusRefreshed =
        await refreshThreadsSupportStatus(organizationID);
      if (!supportStatusRefreshed) {
        invalidateThreadsSupportStatus(organizationID);
        toast.error(
          "The active support grant changed and its replacement could not be verified. Support actions remain locked.",
        );
        return;
      }
      toast.error(
        "The active support grant changed. Its status was refreshed; review it before entering a new revocation reason.",
      );
      return;
    }
    toast.error(
      getErrorMessage(
        error,
        "Could not revoke the Threads support entitlement.",
      ),
    );
  } finally {
    activeAction.value = null;
  }
}

async function testConnection() {
  if (!canWrite.value) return;
  const integration = selectedIntegration.value;
  const definition = selectedDefinition.value;
  if (!integration || !definition || !integration.test_supported) return;
  activeAction.value = "test";
  try {
    const response = await integrationsService.test(integration.provider);
    const result = response.data.data;
    if (result.success)
      toast.success(result.message || `${definition.name} is healthy`);
    else toast.error(result.message || `${definition.name} needs attention`);
    await loadIntegrations({ quiet: true });
  } catch (error) {
    toast.error(getErrorMessage(error, `Could not test ${definition.name}.`));
  } finally {
    activeAction.value = null;
  }
}

async function connectProvider() {
  if (!canWrite.value) return;
  const integration = selectedIntegration.value;
  const definition = selectedDefinition.value;
  if (
    !integration ||
    !definition ||
    !integration.enabled ||
    !integration.oauth.available
  )
    return;
  activeAction.value = "connect";
  try {
    const response = await integrationsService.connect(integration.provider);
    const result = response.data.data;
    if (result.ready && result.mode === "embedded_signup") {
      isDialogOpen.value = false;
      await router.push("/settings/accounts");
      return;
    }
    if (!result.ready || !result.authorization_url) {
      toast.info(result.message || "This connection is not available yet.");
      await loadIntegrations({ quiet: true });
      return;
    }
    window.location.assign(result.authorization_url);
  } catch (error) {
    toast.error(
      getErrorMessage(
        error,
        `Could not start ${definition.name} authorization.`,
      ),
    );
  } finally {
    activeAction.value = null;
  }
}

async function removeCredentials() {
  if (!canWrite.value) return;
  const integration = selectedIntegration.value;
  const definition = selectedDefinition.value;
  if (!integration || !definition) return;
  activeAction.value = "remove";
  try {
    const response = await integrationsService.clearCredentials(
      integration.provider,
    );
    const nextIntegration = response.data.data;
    replaceIntegration(nextIntegration);
    definition.secrets.forEach((field) => {
      secretDraft[field.key] = "";
    });
    isRemoveOpen.value = false;
    const platformCredentialRemains = Object.values(
      nextIntegration.credentials ?? {},
    ).some(
      (credential) => credential.configured && credential.source === "platform",
    );
    toast.success(
      platformCredentialRemains
        ? "Workspace override removed; platform credential remains"
        : `${definition.name} credentials removed`,
    );
  } catch (error) {
    toast.error(
      getErrorMessage(
        error,
        `Could not remove ${definition.name} credentials.`,
      ),
    );
  } finally {
    activeAction.value = null;
  }
}

function hasConfiguredCredentials(integration?: IntegrationState) {
  return Boolean(
    integration &&
    Object.values(integration.credentials ?? {}).some(
      (item) => item.configured,
    ),
  );
}

function hasRemovableCredentials(integration?: IntegrationState) {
  return Boolean(
    integration &&
    Object.values(integration.credentials ?? {}).some(
      (item) => item.configured && item.source !== "platform",
    ),
  );
}

function credentialSourceLabel(source?: string) {
  const labels: Record<string, string> = {
    platform: "Platform managed",
    workspace: "Workspace override",
    copilot: "Copilot settings",
    legacy_chatbot: "Legacy chatbot",
    legacy_account: "Legacy account fallback",
  };
  return source
    ? labels[source] || "Managed credential"
    : "Workspace credential";
}

function statusLabel(status: IntegrationStatus) {
  const labels: Record<IntegrationStatus, string> = {
    connected: "Connected",
    configured: "Ready to connect",
    degraded: "Needs attention",
    disabled: "Disabled",
    not_configured: "Setup required",
    approval_required: "Provider approval required",
    adapter_unavailable: "Adapter unavailable",
  };
  return labels[status] ?? "Setup required";
}

function statusBadgeVariant(status: IntegrationStatus) {
  if (status === "connected") return "success";
  if (status === "degraded") return "destructive";
  if (status === "configured") return "info";
  if (status === "approval_required" || status === "adapter_unavailable")
    return "warning";
  return "secondary";
}

function readinessIcon(state: IntegrationReadinessState) {
  if (state === "ready") return CheckCircle2;
  if (state === "prepared") return Clock3;
  if (state === "blocked") return AlertCircle;
  if (state === "managed") return CloudCog;
  return WifiOff;
}

function readinessLabel(state: IntegrationReadinessState) {
  const labels: Record<IntegrationReadinessState, string> = {
    ready: "Ready",
    prepared: "Preparation only",
    missing: "Missing",
    blocked: "Blocked",
    managed: "Managed elsewhere",
  };
  return labels[state];
}

function readinessClasses(state: IntegrationReadinessState) {
  const classes: Record<IntegrationReadinessState, string> = {
    ready:
      "border-emerald-400/15 bg-emerald-400/[0.045] text-emerald-300 light:border-emerald-200 light:bg-emerald-50 light:text-emerald-800",
    prepared:
      "border-violet-400/15 bg-violet-400/[0.045] text-violet-200 light:border-violet-200 light:bg-violet-50 light:text-violet-800",
    missing:
      "border-white/[0.08] bg-white/[0.025] text-white/45 light:border-gray-200 light:bg-gray-50 light:text-gray-600",
    blocked:
      "border-amber-400/20 bg-amber-400/[0.06] text-amber-200 light:border-amber-200 light:bg-amber-50 light:text-amber-800",
    managed:
      "border-sky-400/15 bg-sky-400/[0.05] text-sky-200 light:border-sky-200 light:bg-sky-50 light:text-sky-800",
  };
  return classes[state];
}

function formatDate(value?: string) {
  if (!value) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

watch(isDialogOpen, (open) => {
  if (!open) {
    Object.keys(secretDraft).forEach((key) => {
      secretDraft[key] = "";
    });
    threadsSupportReason.value = "";
    threadsSupportRevokeReason.value = "";
    resetThreadsSupportStatus();
  }
});

watch(
  () => configDraft.app_review_status,
  () => {
    if (selectedProvider.value === "threads" && isThreadsReviewLocked.value) {
      enabledDraft.value = false;
    }
  },
);

watch(threadsSupportTargetOrganizationId, (organizationID, previousID) => {
  if (
    organizationID === previousID ||
    !isDialogOpen.value ||
    selectedProvider.value !== "threads"
  ) {
    return;
  }
  resetThreadsSupportStatus();
  void refreshThreadsSupportStatus(organizationID);
});

async function handleSearchConsoleOAuthReturn() {
  const rawOutcome = route.query.google_search_console;
  if (rawOutcome == null) return;
  const outcome = Array.isArray(rawOutcome) ? rawOutcome[0] : rawOutcome;

  const nextQuery = { ...route.query };
  delete nextQuery.google_search_console;
  await router.replace({ query: nextQuery });

  if (outcome === "connected") {
    toast.success("Google Search Console connected");
    selectedProvider.value = "google_search_console";
    isSearchConsoleDialogOpen.value = true;
  } else if (outcome === "cancelled") {
    toast.info("Google authorization was cancelled. No changes were made.");
  } else if (outcome === "error") {
    toast.error(
      "Google authorization was not completed. Try connecting again.",
    );
  }
}

async function handleThreadsOAuthReturn() {
  const rawOutcome = route.query.threads;
  if (rawOutcome == null) return;
  const outcome = Array.isArray(rawOutcome) ? rawOutcome[0] : rawOutcome;

  const nextQuery = { ...route.query };
  delete nextQuery.threads;
  await router.replace({ query: nextQuery });

  if (outcome === "connected") {
    await loadIntegrations({ quiet: true });
    toast.success("Threads connected");
    openConfiguration("threads");
  } else if (outcome === "cancelled") {
    toast.info("Threads authorization was cancelled. No changes were made.");
  } else if (outcome === "approval_required") {
    await loadIntegrations({ quiet: true });
    toast.error(
      "Threads authorization stopped because Meta App Review is not approved.",
    );
    openConfiguration("threads");
  } else if (outcome === "error") {
    toast.error(
      "Threads authorization was not completed. Try connecting again.",
    );
  }
}

onMounted(async () => {
  await Promise.all([
    loadIntegrations(),
    authStore.ensureProductEntitlements(),
  ]);
  await handleSearchConsoleOAuthReturn();
  await handleThreadsOAuthReturn();
});
</script>

<template>
  <div
    data-testid="integrations-page"
    class="flex h-full flex-col bg-[#0a0a0b] light:bg-gray-50"
  >
    <PageHeader
      title="Integrations"
      description="Securely connect every customer channel and intelligence provider from one place."
      :icon="CloudCog"
      icon-gradient="bg-gradient-to-br from-[#59613b] to-[#89925c] shadow-[#697046]/20"
    >
      <template #actions>
        <Button
          variant="outline"
          size="sm"
          :disabled="isLoading || isRefreshing"
          @click="loadIntegrations({ quiet: true })"
        >
          <RefreshCw
            :class="['h-4 w-4', { 'animate-spin': isRefreshing }]"
            aria-hidden="true"
          />
          Refresh status
        </Button>
      </template>
    </PageHeader>

    <ScrollArea class="flex-1">
      <main class="mx-auto w-full max-w-7xl space-y-6 p-4 sm:p-6 lg:p-8">
        <section
          class="connection-fabric relative overflow-hidden rounded-2xl border border-[#cbd49a]/15 bg-[#10110e] p-5 sm:p-6 light:border-[#697046]/20 light:bg-[#f7f8f2]"
          aria-labelledby="connection-fabric-title"
        >
          <div
            class="fabric-grid pointer-events-none absolute inset-0 opacity-40 light:opacity-25"
          />
          <div
            class="pointer-events-none absolute -right-24 -top-36 h-80 w-80 rounded-full bg-[#84905b]/15 blur-3xl"
          />
          <div
            class="relative grid gap-6 lg:grid-cols-[1.25fr_1fr] lg:items-end"
          >
            <div>
              <div
                class="mb-3 inline-flex items-center gap-2 rounded-full border border-[#cbd49a]/15 bg-[#cbd49a]/[0.06] px-3 py-1 text-[10px] font-semibold uppercase tracking-[0.2em] text-[#dce5ac] light:text-[#59613b]"
              >
                <ShieldCheck class="h-3.5 w-3.5" aria-hidden="true" />
                Secure connection fabric
              </div>
              <h1
                id="connection-fabric-title"
                class="max-w-2xl text-2xl font-semibold tracking-tight text-white sm:text-3xl light:text-gray-950"
              >
                One control room for every conversation.
              </h1>
              <p
                class="mt-2 max-w-2xl text-sm leading-6 text-white/45 light:text-gray-600"
              >
                Provider secrets stay encrypted and write-only. Readiness, OAuth
                availability, active accounts, and connection health are
                reported directly by the server.
              </p>
            </div>

            <div
              class="grid grid-cols-2 gap-2 sm:grid-cols-4 lg:grid-cols-2 xl:grid-cols-4"
              aria-label="Integration summary"
            >
              <div class="fabric-stat">
                <div
                  class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500"
                >
                  <Wifi class="h-3 w-3" aria-hidden="true" /> Connected
                </div>
                <p
                  class="mt-1.5 text-xl font-semibold text-emerald-300 light:text-emerald-700"
                >
                  {{ connectedCount }}
                </p>
              </div>
              <div class="fabric-stat">
                <div
                  class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500"
                >
                  <Check class="h-3 w-3" aria-hidden="true" /> Configured
                </div>
                <p
                  class="mt-1.5 text-xl font-semibold text-white light:text-gray-900"
                >
                  {{ configuredCount }}
                </p>
              </div>
              <div class="fabric-stat">
                <div
                  class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500"
                >
                  <Activity class="h-3 w-3" aria-hidden="true" /> Active
                </div>
                <p
                  class="mt-1.5 text-xl font-semibold text-white light:text-gray-900"
                >
                  {{ activeAccountCount }}
                </p>
              </div>
              <div class="fabric-stat">
                <div
                  class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500"
                >
                  <AlertCircle class="h-3 w-3" aria-hidden="true" /> Attention
                </div>
                <p
                  :class="[
                    'mt-1.5 text-xl font-semibold',
                    attentionCount
                      ? 'text-amber-300 light:text-amber-700'
                      : 'text-white light:text-gray-900',
                  ]"
                >
                  {{ attentionCount }}
                </p>
              </div>
            </div>
          </div>
        </section>

        <div
          v-if="isLoading"
          class="grid gap-4 md:grid-cols-2 xl:grid-cols-3"
          aria-label="Loading integrations"
        >
          <div
            v-for="index in 7"
            :key="index"
            class="rounded-2xl border border-white/[0.08] p-5 light:border-gray-200 light:bg-white"
          >
            <div class="flex items-center gap-3">
              <Skeleton class="h-11 w-11 rounded-xl" />
              <div class="space-y-2">
                <Skeleton class="h-3 w-20" />
                <Skeleton class="h-5 w-32" />
              </div>
            </div>
            <Skeleton class="mt-5 h-14 w-full" />
            <Skeleton class="mt-5 h-16 w-full rounded-xl" />
            <Skeleton class="mt-5 h-8 w-full" />
          </div>
        </div>

        <section
          v-else-if="loadError"
          class="flex min-h-[300px] flex-col items-center justify-center rounded-2xl border border-dashed border-red-500/20 bg-red-500/[0.03] px-6 text-center"
          role="alert"
        >
          <div
            class="flex h-12 w-12 items-center justify-center rounded-full bg-red-500/10 text-red-300 light:text-red-700"
          >
            <WifiOff class="h-5 w-5" aria-hidden="true" />
          </div>
          <h2 class="mt-4 text-lg font-semibold text-white light:text-gray-900">
            Connection status is unavailable
          </h2>
          <p class="mt-1 max-w-md text-sm text-white/45 light:text-gray-600">
            {{ loadError }}
          </p>
          <Button class="mt-5" variant="outline" @click="loadIntegrations()">
            <RefreshCw class="h-4 w-4" aria-hidden="true" /> Try again
          </Button>
        </section>

        <section v-else aria-labelledby="providers-title">
          <div
            class="mb-4 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between"
          >
            <div>
              <p
                class="text-[10px] font-semibold uppercase tracking-[0.2em] text-[#cbd49a]/65 light:text-[#697046]"
              >
                Provider registry
              </p>
              <h2
                id="providers-title"
                class="mt-1 text-lg font-semibold text-white light:text-gray-950"
              >
                Channels and intelligence
              </h2>
              <p class="mt-1 text-sm text-white/40 light:text-gray-500">
                Configure provider applications here; manage connected customer
                accounts in the Omnichannel Inbox.
              </p>
            </div>
            <div
              class="flex items-center gap-2 text-xs text-white/35 light:text-gray-500"
            >
              <KeyRound class="h-3.5 w-3.5" aria-hidden="true" />
              {{ configuredSecrets.configured }}/{{ configuredSecrets.total }}
              write-only credentials configured
            </div>
          </div>

          <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            <IntegrationProviderCard
              v-for="integration in orderedIntegrations"
              :key="integration.provider"
              :integration="integration"
              :definition="definitions[integration.provider]"
              :readiness="buildIntegrationReadiness(integration)"
              :can-write="canWrite"
              :data-testid="`integration-card-${integration.provider}`"
              @configure="openConfiguration(integration.provider)"
            />
          </div>
        </section>

        <section
          class="flex flex-col gap-3 rounded-2xl border border-white/[0.07] bg-white/[0.02] p-5 sm:flex-row sm:items-center sm:justify-between light:border-gray-200 light:bg-white"
        >
          <div class="flex items-start gap-3">
            <div
              class="mt-0.5 rounded-lg bg-[#cbd49a]/10 p-2 text-[#cbd49a] light:text-[#697046]"
            >
              <ShieldCheck class="h-4 w-4" aria-hidden="true" />
            </div>
            <div>
              <h2 class="text-sm font-semibold text-white light:text-gray-900">
                Credential safety by design
              </h2>
              <p
                class="mt-1 max-w-3xl text-xs leading-5 text-white/40 light:text-gray-500"
              >
                Saved secrets never return to the browser. Replacing a secret
                requires entering a new value; removing credentials is explicit
                and audited.
              </p>
            </div>
          </div>
          <Badge variant="secondary" class="w-fit shrink-0 gap-1.5">
            <CheckCircle2 class="h-3 w-3" aria-hidden="true" /> Server-encrypted
          </Badge>
        </section>
      </main>
    </ScrollArea>

    <Dialog v-model:open="isDialogOpen">
      <DialogContent
        v-if="selectedIntegration && selectedDefinition"
        :data-testid="`integration-dialog-${selectedIntegration.provider}`"
        class="max-h-[90vh] max-w-2xl gap-0 overflow-hidden border-white/[0.1] bg-[#0d0d0e] p-0 light:border-gray-200 light:bg-white"
      >
        <DialogHeader
          class="relative border-b border-white/[0.08] px-6 py-5 text-left light:border-gray-200"
        >
          <div
            class="pointer-events-none absolute right-4 top-0 h-28 w-28 rounded-full opacity-20 blur-3xl"
            :style="{ background: selectedDefinition.glow }"
          />
          <div class="relative flex items-center gap-3 pr-8">
            <div
              class="flex h-11 w-11 items-center justify-center rounded-xl border border-white/10"
              :style="{ background: selectedDefinition.glow }"
            >
              <component
                :is="selectedDefinition.icon"
                class="h-5 w-5 text-white"
                aria-hidden="true"
              />
            </div>
            <div>
              <div class="flex flex-wrap items-center gap-2">
                <DialogTitle class="text-lg text-white light:text-gray-950">{{
                  selectedDefinition.name
                }}</DialogTitle>
                <Badge
                  :variant="statusBadgeVariant(selectedIntegration.status)"
                  >{{ statusLabel(selectedIntegration.status) }}</Badge
                >
              </div>
              <DialogDescription class="mt-1 text-white/40 light:text-gray-500">
                {{ selectedDefinition.description }}
              </DialogDescription>
            </div>
          </div>
        </DialogHeader>

        <ScrollArea class="max-h-[calc(90vh-150px)]">
          <form
            id="integration-config-form"
            class="space-y-6 px-6 py-5"
            @submit.prevent="saveConfiguration"
          >
            <div
              v-if="
                selectedIntegration.message ||
                selectedDefinition.unavailableCopy
              "
              :class="[
                'rounded-xl border px-4 py-3 text-sm leading-5',
                [
                  'approval_required',
                  'adapter_unavailable',
                  'degraded',
                ].includes(selectedIntegration.status)
                  ? 'border-amber-500/15 bg-amber-500/[0.06] text-amber-200 light:text-amber-800'
                  : 'border-white/[0.08] bg-white/[0.03] text-white/55 light:border-gray-200 light:bg-gray-50 light:text-gray-600',
              ]"
            >
              <div class="flex items-start gap-2.5">
                <AlertCircle
                  class="mt-0.5 h-4 w-4 shrink-0"
                  aria-hidden="true"
                />
                <span>{{
                  selectedIntegration.message ||
                  selectedDefinition.unavailableCopy
                }}</span>
              </div>
            </div>

            <section
              v-if="showThreadsEntitlementSupport"
              data-testid="threads-entitlement-support"
              class="rounded-xl border border-[#cbd49a]/20 bg-[#cbd49a]/[0.055] px-4 py-4 light:border-[#697046]/25 light:bg-[#f5f6ed]"
              aria-labelledby="threads-entitlement-support-title"
            >
              <div class="flex items-start gap-3">
                <div
                  class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-[#cbd49a]/20 bg-black/15 text-[#dce5aa] light:bg-white light:text-[#59613b]"
                >
                  <ShieldCheck class="h-4 w-4" aria-hidden="true" />
                </div>
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <h3
                      id="threads-entitlement-support-title"
                      class="text-sm font-semibold text-white light:text-gray-900"
                    >
                      Platform support entitlement
                    </h3>
                    <Badge variant="secondary" class="text-[9px]">
                      Platform owner
                    </Badge>
                  </div>
                  <p
                    v-if="
                      threadsSupportStatusState === 'idle' ||
                      threadsSupportStatusState === 'loading'
                    "
                    data-testid="threads-entitlement-support-loading"
                    class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600"
                  >
                    Checking this workspace for an active support override…
                  </p>
                  <p
                    v-else-if="isThreadsSupportStatusUnavailable"
                    data-testid="threads-entitlement-support-unavailable"
                    class="mt-1 text-xs leading-5 text-amber-200/80 light:text-amber-800"
                  >
                    Support override status could not be verified for this
                    workspace. Grant and revoke actions are locked.
                  </p>
                  <p
                    v-else-if="showThreadsEntitlementEnableSupport"
                    class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600"
                  >
                    Public engagement is not enabled for this workspace. Add an
                    audited reason to grant the reviewed Threads capability.
                  </p>
                  <p
                    v-else-if="showThreadsEntitlementRevokeSupport"
                    class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600"
                  >
                    An active platform support override exists for this
                    workspace. Revoking removes only this override; plan and
                    other entitlements remain intact.
                  </p>
                  <p
                    v-else-if="showThreadsEntitlementWithoutSupportOverride"
                    data-testid="threads-entitlement-no-support-override"
                    class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600"
                  >
                    Public engagement is enabled by the workspace plan or
                    another entitlement. No active platform support override
                    exists for this workspace.
                  </p>

                  <div
                    v-if="showThreadsEntitlementEnableSupport"
                    class="mt-3 space-y-1.5"
                  >
                    <Label
                      for="threads-entitlement-support-reason"
                      class="text-xs text-white/70 light:text-gray-700"
                    >
                      Support reason
                    </Label>
                    <Textarea
                      id="threads-entitlement-support-reason"
                      v-model="threadsSupportReason"
                      :rows="2"
                      maxlength="2000"
                      aria-required="true"
                      placeholder="Ticket, approval, or operational reason"
                      :disabled="activeAction !== null"
                      class="min-h-[72px] resize-none bg-black/15 light:bg-white"
                    />
                    <div
                      class="flex flex-col gap-3 pt-1 sm:flex-row sm:items-center sm:justify-between"
                    >
                      <p
                        class="text-[11px] leading-4 text-white/35 light:text-gray-500"
                      >
                        Required and recorded in the entitlement audit log.
                      </p>
                      <Button
                        type="button"
                        size="sm"
                        data-testid="threads-entitlement-enable"
                        :loading="activeAction === 'entitlement'"
                        :disabled="!canEnableThreadsEntitlement"
                        @click="enableThreadsPublicEngagement"
                      >
                        <ShieldCheck class="h-3.5 w-3.5" aria-hidden="true" />
                        Enable public engagement
                      </Button>
                    </div>
                  </div>

                  <div
                    v-if="showThreadsEntitlementRevokeSupport"
                    class="mt-3 space-y-1.5"
                  >
                    <Label
                      for="threads-entitlement-support-revoke-reason"
                      class="text-xs text-white/70 light:text-gray-700"
                    >
                      Revocation reason
                    </Label>
                    <Textarea
                      id="threads-entitlement-support-revoke-reason"
                      v-model="threadsSupportRevokeReason"
                      :rows="2"
                      maxlength="2000"
                      aria-required="true"
                      placeholder="Ticket, approval, or operational reason"
                      :disabled="activeAction !== null"
                      class="min-h-[72px] resize-none bg-black/15 light:bg-white"
                    />
                    <div
                      class="flex flex-col gap-3 pt-1 sm:flex-row sm:items-center sm:justify-between"
                    >
                      <p
                        class="text-[11px] leading-4 text-white/35 light:text-gray-500"
                      >
                        Required and recorded in the entitlement audit log.
                      </p>
                      <Button
                        type="button"
                        size="sm"
                        variant="destructive"
                        data-testid="threads-entitlement-revoke"
                        :loading="activeAction === 'entitlement'"
                        :disabled="!canRevokeThreadsEntitlementSupport"
                        @click="revokeThreadsPublicEngagementSupport"
                      >
                        <Trash2 class="h-3.5 w-3.5" aria-hidden="true" />
                        Revoke support entitlement
                      </Button>
                    </div>
                  </div>
                </div>
              </div>
            </section>

            <section aria-labelledby="application-config-title">
              <div class="mb-4 flex items-center justify-between gap-3">
                <div>
                  <div class="flex flex-wrap items-center gap-2">
                    <h3
                      id="application-config-title"
                      class="text-sm font-semibold text-white light:text-gray-900"
                    >
                      Application configuration
                    </h3>
                    <Badge
                      v-if="selectedDefinition.preparationOnly"
                      variant="warning"
                      data-testid="integration-preparation-only"
                    >
                      Preparation only
                    </Badge>
                  </div>
                  <p class="mt-0.5 text-xs text-white/35 light:text-gray-500">
                    {{
                      selectedDefinition.preparationOnly
                        ? "Store pre-approval details only. No live callback, account creation or authorization is available."
                        : "Public identifiers and provider setup details."
                    }}
                  </p>
                </div>
                <div class="flex items-center gap-2">
                  <Label
                    for="integration-enabled"
                    class="text-xs text-white/50 light:text-gray-600"
                  >
                    {{ isActivationLocked ? "Activation locked" : "Enabled" }}
                  </Label>
                  <Switch
                    id="integration-enabled"
                    v-model:checked="enabledDraft"
                    :disabled="
                      !canWrite || activeAction !== null || isActivationLocked
                    "
                  />
                </div>
              </div>

              <div class="grid gap-4 sm:grid-cols-2">
                <div
                  v-for="field in selectedDefinition.fields"
                  :key="field.key"
                  :class="[
                    'space-y-1.5',
                    field.kind === 'url' || field.readOnly
                      ? 'sm:col-span-2'
                      : '',
                  ]"
                >
                  <Label
                    :for="`${selectedIntegration.provider}-${field.key}`"
                    class="text-xs text-white/70 light:text-gray-700"
                  >
                    {{ field.label }}
                  </Label>
                  <Select
                    v-if="field.kind === 'select'"
                    v-model="configDraft[field.key]"
                    :disabled="
                      activeAction !== null || field.readOnly || !canWrite
                    "
                  >
                    <SelectTrigger
                      :id="`${selectedIntegration.provider}-${field.key}`"
                    >
                      <SelectValue placeholder="Select status" />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem
                        v-for="option in field.options"
                        :key="option.value"
                        :value="option.value"
                        :disabled="option.disabled"
                      >
                        {{ option.label }}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                  <div v-else class="flex items-center gap-2">
                    <Input
                      :id="`${selectedIntegration.provider}-${field.key}`"
                      v-model="configDraft[field.key]"
                      :type="
                        field.kind === 'number'
                          ? 'number'
                          : field.kind === 'url'
                            ? 'url'
                            : 'text'
                      "
                      :placeholder="field.placeholder"
                      :readonly="field.readOnly || !canWrite"
                      :disabled="activeAction !== null"
                      :min="field.min"
                      :max="field.max"
                      :step="field.step"
                      autocomplete="off"
                      spellcheck="false"
                      :class="
                        field.readOnly || !canWrite
                          ? 'cursor-not-allowed bg-white/[0.025] text-white/35 light:bg-gray-50 light:text-gray-500'
                          : ''
                      "
                    />
                    <Button
                      v-if="field.key === 'webhook_callback_path'"
                      type="button"
                      variant="outline"
                      size="icon"
                      data-testid="meta-webhook-callback-copy"
                      aria-label="Copy Meta webhook callback"
                      @click="copyWebhookCallback"
                    >
                      <Copy class="h-4 w-4" aria-hidden="true" />
                    </Button>
                    <Button
                      v-if="
                        selectedIntegration.provider === 'threads' &&
                        [
                          'redirect_uri',
                          'deauthorization_callback_url',
                          'data_deletion_callback_url',
                          'account_webhook_callback_url',
                        ].includes(field.key) &&
                        Boolean(configDraft[field.key]?.trim())
                      "
                      type="button"
                      variant="outline"
                      size="icon"
                      :data-testid="
                        field.key === 'redirect_uri'
                          ? 'threads-oauth-callback-copy'
                          : `threads-${field.key}-copy`
                      "
                      :aria-label="`Copy ${field.label}`"
                      @click="copyThreadsSetupURL(field.key, field.label)"
                    >
                      <Copy class="h-4 w-4" aria-hidden="true" />
                    </Button>
                  </div>
                  <p
                    class="text-[11px] leading-4 text-white/30 light:text-gray-500"
                  >
                    {{ field.description }}
                  </p>
                </div>
              </div>
            </section>

            <section
              v-if="selectedDefinition.secrets.length"
              class="border-t border-white/[0.08] pt-5 light:border-gray-200"
              aria-labelledby="credentials-title"
            >
              <div class="mb-4 flex items-center justify-between gap-3">
                <div>
                  <h3
                    id="credentials-title"
                    class="flex items-center gap-2 text-sm font-semibold text-white light:text-gray-900"
                  >
                    <KeyRound
                      class="h-4 w-4 text-[#cbd49a] light:text-[#697046]"
                      aria-hidden="true"
                    />
                    Write-only credentials
                  </h3>
                  <p class="mt-0.5 text-xs text-white/35 light:text-gray-500">
                    {{
                      selectedDefinition.preparationOnly
                        ? "Existing values are never revealed. Storing them prepares a future adapter; it does not connect this provider."
                        : "Existing values are never returned or revealed."
                    }}
                  </p>
                </div>
                <Badge
                  v-if="hasConfiguredCredentials(selectedIntegration)"
                  :variant="
                    selectedDefinition.preparationOnly ? 'secondary' : 'success'
                  "
                  class="gap-1"
                >
                  <Check class="h-3 w-3" aria-hidden="true" />
                  {{
                    selectedDefinition.preparationOnly
                      ? "Stored for preparation"
                      : "Configured"
                  }}
                </Badge>
              </div>

              <div class="space-y-4">
                <div
                  v-for="field in selectedDefinition.secrets"
                  :key="field.key"
                  class="space-y-1.5"
                >
                  <div class="flex items-center justify-between gap-3">
                    <Label
                      :for="`${selectedIntegration.provider}-${field.key}`"
                      class="text-xs text-white/70 light:text-gray-700"
                    >
                      {{ field.label }}
                    </Label>
                    <div
                      v-if="
                        selectedIntegration.credentials[field.key]?.configured
                      "
                      class="flex flex-wrap items-center justify-end gap-1.5"
                    >
                      <Badge variant="secondary" class="px-1.5 py-0 text-[9px]">
                        {{
                          credentialSourceLabel(
                            selectedIntegration.credentials[field.key]?.source,
                          )
                        }}
                      </Badge>
                      <span
                        class="text-[10px] text-emerald-400 light:text-emerald-700"
                      >
                        Stored{{
                          selectedIntegration.credentials[field.key]?.updated_at
                            ? ` · ${formatDate(selectedIntegration.credentials[field.key]?.updated_at)}`
                            : ""
                        }}
                      </span>
                    </div>
                  </div>
                  <Input
                    :id="`${selectedIntegration.provider}-${field.key}`"
                    v-model="secretDraft[field.key]"
                    type="password"
                    :placeholder="
                      isSecretReadOnly(field)
                        ? 'Managed by the shared platform Meta app'
                        : selectedIntegration.credentials[field.key]?.configured
                          ? 'Stored securely — enter a new value to replace'
                          : field.placeholder
                    "
                    :disabled="
                      activeAction !== null ||
                      !canWrite ||
                      isSecretReadOnly(field)
                    "
                    autocomplete="new-password"
                    spellcheck="false"
                  />
                  <p
                    class="text-[11px] leading-4 text-white/30 light:text-gray-500"
                  >
                    {{ field.description }}
                  </p>
                </div>
              </div>
            </section>

            <section
              v-if="selectedIntegration.required_scopes?.length"
              class="border-t border-white/[0.08] pt-5 light:border-gray-200"
              aria-labelledby="scopes-title"
            >
              <h3
                id="scopes-title"
                class="text-sm font-semibold text-white light:text-gray-900"
              >
                Required provider permissions
              </h3>
              <div class="mt-3 flex flex-wrap gap-1.5">
                <code
                  v-for="scope in selectedIntegration.required_scopes"
                  :key="scope"
                  class="rounded-md border border-white/[0.08] bg-white/[0.035] px-2 py-1 text-[11px] text-white/50 light:border-gray-200 light:bg-gray-50 light:text-gray-600"
                >
                  {{ scope }}
                </code>
              </div>
            </section>

            <section
              class="border-t border-white/[0.08] pt-5 light:border-gray-200"
              aria-labelledby="readiness-title"
            >
              <div class="flex items-end justify-between gap-4">
                <div>
                  <h3
                    id="readiness-title"
                    class="text-sm font-semibold text-white light:text-gray-900"
                  >
                    Setup preflight
                  </h3>
                  <p class="mt-1 text-xs text-white/35 light:text-gray-500">
                    Required components and the settings intentionally managed
                    elsewhere. Stored credential values are never displayed.
                  </p>
                </div>
                <Badge variant="secondary" class="shrink-0">
                  {{
                    selectedReadiness.filter((item) =>
                      ["missing", "blocked"].includes(item.state),
                    ).length
                  }}
                  blocked or missing
                </Badge>
              </div>
              <div
                class="mt-4 space-y-2"
                :data-testid="`integration-dialog-readiness-${selectedIntegration.provider}`"
              >
                <div
                  v-for="item in selectedReadiness"
                  :key="item.key"
                  :data-testid="`integration-dialog-readiness-${selectedIntegration.provider}-${item.key}`"
                  class="flex items-start gap-3 rounded-xl border px-3.5 py-3"
                  :class="readinessClasses(item.state)"
                >
                  <component
                    :is="readinessIcon(item.state)"
                    class="mt-0.5 h-4 w-4 shrink-0"
                    aria-hidden="true"
                  />
                  <div class="min-w-0 flex-1">
                    <div
                      class="flex flex-wrap items-center justify-between gap-2"
                    >
                      <p class="text-xs font-semibold">{{ item.label }}</p>
                      <span
                        class="text-[9px] font-semibold uppercase tracking-[0.12em] opacity-75"
                      >
                        {{ readinessLabel(item.state) }}
                      </span>
                    </div>
                    <p class="mt-1 text-[11px] leading-4 opacity-70">
                      {{ item.detail }}
                    </p>
                  </div>
                </div>
              </div>
            </section>
          </form>
        </ScrollArea>

        <DialogFooter
          class="flex-col-reverse gap-2 border-t border-white/[0.08] bg-white/[0.015] px-6 py-4 sm:flex-row sm:items-center sm:justify-between light:border-gray-200 light:bg-gray-50/60"
        >
          <Button
            v-if="
              canWrite &&
              selectedDefinition.secrets.length &&
              hasRemovableCredentials(selectedIntegration)
            "
            variant="ghost"
            size="sm"
            class="text-red-300 hover:bg-red-500/10 hover:text-red-200 light:text-red-700"
            :disabled="activeAction !== null"
            @click="isRemoveOpen = true"
          >
            <Trash2 class="h-3.5 w-3.5" aria-hidden="true" /> Remove credentials
          </Button>
          <span v-else-if="canWrite" />
          <p v-else class="text-xs text-white/35 light:text-gray-500">
            View-only access — ask a workspace administrator to make changes.
          </p>
          <div v-if="canWrite" class="flex flex-wrap justify-end gap-2">
            <Button
              v-if="selectedIntegration.test_supported"
              :data-testid="`integration-test-${selectedIntegration.provider}`"
              variant="outline"
              size="sm"
              :loading="activeAction === 'test'"
              :disabled="
                activeAction !== null || !selectedIntegration.configured
              "
              @click="testConnection"
            >
              <Activity class="h-3.5 w-3.5" aria-hidden="true" /> Test
            </Button>
            <Button
              v-if="
                selectedIntegration.oauth.supported &&
                (selectedIntegration.provider === 'threads' ||
                  selectedIntegration.enabled)
              "
              :data-testid="`integration-connect-${selectedIntegration.provider}`"
              variant="outline"
              size="sm"
              :loading="activeAction === 'connect'"
              :disabled="
                activeAction !== null ||
                !selectedIntegration.enabled ||
                !selectedIntegration.oauth.available ||
                isThreadsReviewLocked
              "
              @click="connectProvider"
            >
              <ExternalLink class="h-3.5 w-3.5" aria-hidden="true" />
              {{ selectedDefinition.connectLabel }}
            </Button>
            <Button
              :data-testid="`integration-save-${selectedIntegration.provider}`"
              type="submit"
              form="integration-config-form"
              size="sm"
              :loading="activeAction === 'save'"
              :disabled="activeAction !== null"
            >
              <Save class="h-3.5 w-3.5" aria-hidden="true" /> Save configuration
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <GoogleSearchConsoleDialog
      v-if="selectedIntegration?.provider === 'google_search_console'"
      v-model:open="isSearchConsoleDialogOpen"
      :integration="selectedIntegration"
      :can-write="canWrite"
      :can-view-analytics="authStore.hasPermission('analytics', 'read')"
      @refresh="loadIntegrations({ quiet: true })"
    />

    <ConfirmDialog
      v-model:open="isRemoveOpen"
      title="Remove stored credentials?"
      :description="`This removes every stored ${selectedDefinition?.name || ''} secret from this workspace and can interrupt active connections. Public configuration is kept.`"
      confirm-label="Remove credentials"
      variant="destructive"
      :is-submitting="activeAction === 'remove'"
      @confirm="removeCredentials"
    />
  </div>
</template>

<style scoped>
.fabric-grid {
  background-image:
    linear-gradient(rgba(203, 212, 154, 0.035) 1px, transparent 1px),
    linear-gradient(90deg, rgba(203, 212, 154, 0.035) 1px, transparent 1px);
  background-size: 32px 32px;
  mask-image: linear-gradient(to right, black, transparent 90%);
}

.fabric-stat {
  border: 1px solid rgba(255, 255, 255, 0.07);
  border-radius: 0.75rem;
  background: rgba(255, 255, 255, 0.025);
  padding: 0.65rem 0.75rem;
  backdrop-filter: blur(8px);
}

.light .fabric-stat {
  border-color: rgba(78, 82, 51, 0.1);
  background: rgba(255, 255, 255, 0.72);
}

@media (prefers-reduced-motion: reduce) {
  .connection-fabric * {
    animation: none !important;
  }
}
</style>
