<script setup lang="ts">
import { ref, computed, onBeforeUnmount, onMounted, watch } from "vue";
import { onBeforeRouteLeave } from "vue-router";
import { useI18n } from "vue-i18n";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  PageHeader,
  DataTable,
  DeleteConfirmDialog,
  ErrorState,
  type Column,
} from "@/components/shared";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api } from "@/services/api";
import { useOrganizationsStore } from "@/stores/organizations";
import { useAuthStore } from "@/stores/auth";
import { toast } from "vue-sonner";
import { getErrorMessage } from "@/lib/api-utils";
import { formatDate } from "@/lib/utils";
import {
  createMetaEmbeddedSignupSession,
  type MetaEmbeddedSignupAbortReason,
  type MetaEmbeddedSignupSession,
} from "@/lib/metaEmbeddedSignup";
import {
  Plus,
  Pencil,
  Trash2,
  Phone,
  Check,
  Loader2,
  Facebook,
  Smartphone,
  Network,
  PlugZap,
  X,
} from "lucide-vue-next";

declare global {
  interface Window {
    FB: any;
  }
}

const { t } = useI18n();
const organizationsStore = useOrganizationsStore();
const authStore = useAuthStore();

interface WhatsAppAccount {
  id: string;
  name: string;
  phone_id: string;
  business_id: string;
  api_version: string;
  is_default_incoming: boolean;
  is_default_outgoing: boolean;
  status: string;
  has_access_token: boolean;
  created_at: string;
}

const accounts = ref<WhatsAppAccount[]>([]);
const isLoading = ref(true);
const fetchError = ref(false);
const deleteDialogOpen = ref(false);
const accountToDelete = ref<WhatsAppAccount | null>(null);
const isDeleting = ref(false);

// Facebook Embedded Signup State
const whatsappConfig = ref<{
  app_id: string;
  config_id: string;
  api_version: string;
  has_app_secret: boolean;
} | null>(null);
const whatsappConfigOrganizationId = ref<string | null>(null);
const isFBSDKLoaded = ref(false);
const initializedFacebookAppId = ref<string | null>(null);
const isConnectingFB = ref(false);
const canCancelEmbeddedSignup = ref(false);
const showOnboardingDialog = ref(false);
let isFacebookSDKLoading = false;
let accountsFetchSequence = 0;
let activeEmbeddedSignupSession: MetaEmbeddedSignupSession | null = null;
let activeEmbeddedSignupOrganizationId: string | null = null;
let activeEmbeddedSignupExchange: {
  organizationId: string;
  detached: boolean;
} | null = null;
const embeddedSignupSwitchLockOwner = "embedded-signup";
const embeddedSignupSwitchLockMessage =
  "Finish or cancel the WhatsApp connection before switching workspaces.";

const canWrite = computed(() => authStore.hasPermission("accounts", "write"));
const canDelete = computed(() => authStore.hasPermission("accounts", "delete"));
const canReadIntegrations = computed(() =>
  authStore.hasPermission("settings.integrations", "read"),
);
const activeOrganizationId = computed(
  () => organizationsStore.selectedOrgId || authStore.organizationId || null,
);
const isMetaIntegrationReady = computed(() =>
  Boolean(
    whatsappConfigOrganizationId.value === activeOrganizationId.value &&
    whatsappConfig.value?.app_id &&
    whatsappConfig.value?.config_id &&
    whatsappConfig.value?.has_app_secret,
  ),
);
const breadcrumbs = computed(() => [
  { label: t("nav.settings"), href: "/settings" },
  { label: t("settings.accounts") },
]);

const sortKey = ref("name");
const sortDirection = ref<"asc" | "desc">("asc");

const columns = computed<Column<WhatsAppAccount>[]>(() => [
  {
    key: "account",
    label: t("accounts.account"),
    width: "w-[250px]",
    sortable: true,
    sortKey: "name",
  },
  { key: "phone_id", label: t("accounts.phoneNumberId"), sortable: true },
  { key: "api_version", label: t("accounts.apiVersion") },
  { key: "defaults", label: t("accounts.defaults") },
  {
    key: "status",
    label: t("accounts.status"),
    sortable: true,
    sortKey: "status",
  },
  {
    key: "created",
    label: t("common.created"),
    sortable: true,
    sortKey: "created_at",
  },
  { key: "actions", label: t("common.actions"), align: "right" },
]);

watch(activeOrganizationId, (organizationId) => {
  if (
    activeEmbeddedSignupOrganizationId &&
    organizationId !== activeEmbeddedSignupOrganizationId
  ) {
    cancelPendingEmbeddedSignup(true);
  }
  if (
    activeEmbeddedSignupExchange &&
    organizationId !== activeEmbeddedSignupExchange.organizationId
  ) {
    activeEmbeddedSignupExchange.detached = true;
    activeEmbeddedSignupExchange = null;
    isConnectingFB.value = false;
  }
  whatsappConfig.value = null;
  whatsappConfigOrganizationId.value = null;
  fetchAccounts();
  fetchWhatsAppConfig();
});
watch(isConnectingFB, (isConnecting) => {
  if (isConnecting) {
    organizationsStore.blockOrganizationSwitch(
      embeddedSignupSwitchLockOwner,
      embeddedSignupSwitchLockMessage,
    );
  } else {
    organizationsStore.unblockOrganizationSwitch(
      embeddedSignupSwitchLockOwner,
    );
  }
});
onMounted(async () => {
  window.addEventListener("beforeunload", guardEmbeddedSignupNavigation);
  await Promise.all([fetchAccounts(), fetchWhatsAppConfig()]);
});
onBeforeUnmount(() => {
  window.removeEventListener("beforeunload", guardEmbeddedSignupNavigation);
  cancelPendingEmbeddedSignup(false);
  if (activeEmbeddedSignupExchange) {
    activeEmbeddedSignupExchange.detached = true;
    activeEmbeddedSignupExchange = null;
  }
  isConnectingFB.value = false;
  organizationsStore.unblockOrganizationSwitch(embeddedSignupSwitchLockOwner);
});

onBeforeRouteLeave(() => {
  if (!isConnectingFB.value) return true;
  toast.error(embeddedSignupSwitchLockMessage);
  return false;
});

function guardEmbeddedSignupNavigation(event: BeforeUnloadEvent) {
  if (!isConnectingFB.value) return;
  event.preventDefault();
  event.returnValue = "";
}

function isEmbeddedSignupOrganizationCurrent(organizationId: string) {
  return activeOrganizationId.value === organizationId;
}

function cancelPendingEmbeddedSignup(notifyOrganizationChange: boolean) {
  const shouldNotify = notifyOrganizationChange && isConnectingFB.value;
  activeEmbeddedSignupSession?.cancel();
  activeEmbeddedSignupSession = null;
  activeEmbeddedSignupOrganizationId = null;
  canCancelEmbeddedSignup.value = false;
  if (!activeEmbeddedSignupExchange) {
    isConnectingFB.value = false;
  }

  if (shouldNotify) {
    toast.error(
      "WhatsApp signup was cancelled because the active workspace changed.",
    );
  }
}

function cancelEmbeddedSignupFromUI() {
  if (!canCancelEmbeddedSignup.value) return;
  cancelPendingEmbeddedSignup(false);
  toast.info("WhatsApp connection cancelled. You can start again when ready.");
}

async function fetchAccounts() {
  const organizationId = activeOrganizationId.value;
  const requestSequence = ++accountsFetchSequence;
  isLoading.value = true;
  fetchError.value = false;
  try {
    const response = await api.get("/accounts", {
      headers: organizationId
        ? { "X-Organization-ID": organizationId }
        : undefined,
    });
    if (
      requestSequence !== accountsFetchSequence ||
      activeOrganizationId.value !== organizationId
    ) {
      return;
    }
    accounts.value = response.data.data?.accounts || [];
  } catch {
    if (
      requestSequence !== accountsFetchSequence ||
      activeOrganizationId.value !== organizationId
    ) {
      return;
    }
    fetchError.value = true;
    toast.error(t("common.failedLoad", { resource: t("resources.accounts") }));
  } finally {
    if (
      requestSequence === accountsFetchSequence &&
      activeOrganizationId.value === organizationId
    ) {
      isLoading.value = false;
    }
  }
}

async function fetchWhatsAppConfig() {
  const organizationId = activeOrganizationId.value;
  whatsappConfig.value = null;
  whatsappConfigOrganizationId.value = null;
  try {
    const response = await api.get("/embedded-signup/config", {
      headers: organizationId
        ? { "X-Organization-ID": organizationId }
        : undefined,
    });
    if (activeOrganizationId.value !== organizationId) return;
    if (response.data.data.organization_id !== organizationId) {
      console.error("Embedded Signup configuration workspace mismatch");
      return;
    }

    whatsappConfig.value = {
      app_id: response.data.data.whatsapp_app_id,
      config_id: response.data.data.whatsapp_config_id,
      api_version: response.data.data.whatsapp_api_version || "v21.0",
      has_app_secret: response.data.data.has_app_secret === true,
    };
    whatsappConfigOrganizationId.value = organizationId;
    if (isMetaIntegrationReady.value) {
      loadFacebookSDK();
    }
  } catch {
    console.error("Failed to fetch WhatsApp configuration");
  }
}

function loadFacebookSDK() {
  const appId = whatsappConfig.value?.app_id;
  if (!appId) return;

  const initializeForCurrentApp = () => {
    const currentConfig = whatsappConfig.value;
    if (!currentConfig?.app_id || !window.FB?.init) return;

    window.FB.init({
      appId: currentConfig.app_id,
      cookie: true,
      xfbml: true,
      version: currentConfig.api_version,
    });
    initializedFacebookAppId.value = currentConfig.app_id;
    isFBSDKLoaded.value = true;
  };

  if (window.FB?.init) {
    isFacebookSDKLoading = false;
    if (initializedFacebookAppId.value !== appId) {
      initializeForCurrentApp();
    }
    return;
  }

  const existingScript = document.querySelector<HTMLScriptElement>(
    'script[src="https://connect.facebook.net/en_US/sdk.js"]',
  );
  if (existingScript) {
    if (isFacebookSDKLoading) return;
    isFacebookSDKLoading = true;
    existingScript.addEventListener(
      "load",
      () => {
        isFacebookSDKLoading = false;
        initializeForCurrentApp();
      },
      {
        once: true,
      },
    );
    return;
  }

  isFacebookSDKLoading = true;
  const script = document.createElement("script");
  script.src = "https://connect.facebook.net/en_US/sdk.js";
  script.async = true;
  script.defer = true;
  script.onload = () => {
    isFacebookSDKLoading = false;
    initializeForCurrentApp();
  };
  script.onerror = () => {
    isFacebookSDKLoading = false;
    isFBSDKLoaded.value = false;
    script.remove();
  };
  document.body.appendChild(script);
}

function launchWhatsAppSignup(isCoexistence: boolean = true) {
  const signupOrganizationId = activeOrganizationId.value;
  if (!signupOrganizationId) {
    toast.error("Select a workspace before connecting WhatsApp.");
    return;
  }

  if (whatsappConfigOrganizationId.value !== signupOrganizationId) {
    toast.error(
      "The Meta configuration for this workspace is still loading. Please wait.",
    );
    return;
  }

  if (!isMetaIntegrationReady.value) {
    toast.error(
      "Complete the Meta App ID, Config ID and App Secret in Integrations first.",
    );
    return;
  }

  if (
    !isFBSDKLoaded.value ||
    initializedFacebookAppId.value !== whatsappConfig.value?.app_id
  ) {
    toast.error("Facebook SDK not loaded yet. Please wait...");
    return;
  }

  if (!whatsappConfig.value) {
    toast.error("WhatsApp configuration not loaded");
    return;
  }

  cancelPendingEmbeddedSignup(false);
  showOnboardingDialog.value = false;
  isConnectingFB.value = true;

  const loginOptions: any = {
    config_id: whatsappConfig.value.config_id,
    response_type: "code",
    override_default_response_type: true,
  };

  if (isCoexistence) {
    loginOptions.extras = {
      setup: {},
      featureType: "whatsapp_business_app_onboarding",
      sessionInfoVersion: "3",
    };
  } else {
    loginOptions.extras = {
      setup: {},
    };
  }

  let session: MetaEmbeddedSignupSession;
  const handleAbort = (
    reason: MetaEmbeddedSignupAbortReason,
    detail?: string,
  ) => {
    if (reason === "cancelled") {
      toast.error(
        detail
          ? `WhatsApp signup was cancelled at ${detail}.`
          : "Facebook login was cancelled",
      );
    } else {
      toast.error(detail ? `Facebook error: ${detail}` : "Facebook error");
    }
    isConnectingFB.value = false;
  };

  session = createMetaEmbeddedSignupSession({
    onComplete: ({ code, phoneNumberId, wabaId }) => {
      if (!isEmbeddedSignupOrganizationCurrent(signupOrganizationId)) {
        cancelPendingEmbeddedSignup(true);
        return;
      }
      exchangeCodeForToken(code, signupOrganizationId, phoneNumberId, wabaId);
    },
    onAbort: handleAbort,
    isContextCurrent: () =>
      isEmbeddedSignupOrganizationCurrent(signupOrganizationId),
    onContextChanged: () => {
      cancelPendingEmbeddedSignup(true);
    },
    onSettled: () => {
      canCancelEmbeddedSignup.value = false;
      window.removeEventListener("message", session.handleMessage);
      if (activeEmbeddedSignupSession === session) {
        activeEmbeddedSignupSession = null;
        activeEmbeddedSignupOrganizationId = null;
      }
    },
  });
  activeEmbeddedSignupSession = session;
  activeEmbeddedSignupOrganizationId = signupOrganizationId;
  canCancelEmbeddedSignup.value = true;
  window.addEventListener("message", session.handleMessage);

  try {
    window.FB.login(session.handleLoginResponse, loginOptions);
  } catch {
    session.cancel();
    toast.error("Unable to start Facebook login. Please try again.");
    isConnectingFB.value = false;
  }
}

async function exchangeCodeForToken(
  code: string,
  organizationId: string,
  phoneNumberId?: string,
  wabaId?: string,
) {
  if (!isEmbeddedSignupOrganizationCurrent(organizationId)) {
    cancelPendingEmbeddedSignup(true);
    return;
  }

  const exchange = {
    organizationId,
    detached: false,
  };
  activeEmbeddedSignupExchange = exchange;

  try {
    const response = await api.post(
      "/accounts/exchange-token",
      {
        code,
        phone_id: phoneNumberId,
        waba_id: wabaId,
      },
      {
        headers: { "X-Organization-ID": organizationId },
        // The server performs bounded Meta validation, registration and
        // subscription phases under a 60-second deadline. Keep the browser
        // alive long enough to receive the durable final state.
        timeout: 90_000,
      },
    );

    if (
      exchange.detached ||
      !isEmbeddedSignupOrganizationCurrent(organizationId)
    ) {
      return;
    }

    const exchangeResult = response.data.data;
    const account = exchangeResult.account;
    if (account.status === "pending_registration") {
      toast.warning("Account created. Phone registration required.");
    } else if (account.status === "subscription_failed") {
      toast.warning(
        "Account created, but webhook subscription needs attention. Open the account and use Subscribe.",
      );
    } else if (account.status === "active") {
      toast.success("WhatsApp account connected successfully!");
    }

    if (
      typeof exchangeResult.warning === "string" &&
      exchangeResult.warning.trim()
    ) {
      toast.warning(exchangeResult.warning.trim());
    }

    await fetchAccounts();
  } catch (error: any) {
    if (
      exchange.detached ||
      !isEmbeddedSignupOrganizationCurrent(organizationId)
    ) {
      return;
    }
    toast.error(getErrorMessage(error, "Failed to connect WhatsApp account"));
  } finally {
    if (activeEmbeddedSignupExchange === exchange) {
      activeEmbeddedSignupExchange = null;
      isConnectingFB.value = false;
    }
  }
}

function openDeleteDialog(account: WhatsAppAccount) {
  accountToDelete.value = account;
  deleteDialogOpen.value = true;
}

async function confirmDelete() {
  if (!accountToDelete.value) return;
  isDeleting.value = true;
  try {
    await api.delete(`/accounts/${accountToDelete.value.id}`);
    toast.success(
      t("common.deletedSuccess", { resource: t("resources.Account") }),
    );
    deleteDialogOpen.value = false;
    accountToDelete.value = null;
    await fetchAccounts();
  } catch (e) {
    toast.error(
      getErrorMessage(
        e,
        t("common.failedDelete", { resource: t("resources.account") }),
      ),
    );
  } finally {
    isDeleting.value = false;
  }
}
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="$t('accounts.title')"
      :icon="Phone"
      icon-gradient="bg-gradient-to-br from-emerald-500 to-green-600 shadow-emerald-500/20"
      back-link="/settings"
      :breadcrumbs="breadcrumbs"
    >
      <template #actions>
        <div v-if="canWrite" class="flex items-center gap-2">
          <Button
            v-if="isMetaIntegrationReady"
            size="sm"
            @click="showOnboardingDialog = true"
            :disabled="isConnectingFB || !isFBSDKLoaded"
            class="bg-gradient-to-br from-facebook to-facebook-dark hover:from-facebook-hover hover:to-facebook-hoverDark text-white border-none shadow-none"
          >
            <Loader2 v-if="isConnectingFB" class="h-4 w-4 mr-2 animate-spin" />
            <Facebook v-else class="h-4 w-4 mr-2" />
            {{ $t("accounts.connectFacebook") }}
          </Button>
          <Button
            v-if="canCancelEmbeddedSignup"
            variant="outline"
            size="sm"
            @click="cancelEmbeddedSignupFromUI"
          >
            <X class="h-4 w-4 mr-2" />
            Cancel connection
          </Button>
          <RouterLink
            v-else-if="canReadIntegrations"
            to="/settings/integrations"
          >
            <Button variant="outline" size="sm">
              <PlugZap class="h-4 w-4 mr-2" />
              Configure Meta integration
            </Button>
          </RouterLink>
          <RouterLink to="/settings/accounts/new">
            <Button variant="outline" size="sm">
              <Plus class="h-4 w-4 mr-2" />
              {{ $t("accounts.addAccount") }}
            </Button>
          </RouterLink>
        </div>
      </template>
    </PageHeader>

    <ErrorState
      v-if="fetchError && !isLoading"
      :title="$t('common.loadErrorTitle')"
      :description="$t('common.loadErrorDescription')"
      class="flex-1"
    >
      <template #action
        ><Button size="sm" @click="fetchAccounts">{{
          $t("common.retry")
        }}</Button></template
      >
    </ErrorState>

    <ScrollArea v-else class="flex-1">
      <div class="p-6">
        <div>
          <Card>
            <CardHeader>
              <div>
                <CardTitle>{{ $t("accounts.yourAccounts") }}</CardTitle>
                <CardDescription>{{
                  $t("accounts.yourAccountsDesc")
                }}</CardDescription>
              </div>
            </CardHeader>
            <CardContent>
              <DataTable
                :items="accounts"
                :columns="columns"
                :is-loading="isLoading"
                :empty-icon="Phone"
                :empty-title="$t('accounts.noAccounts')"
                :empty-description="$t('accounts.noAccountsDesc')"
                v-model:sort-key="sortKey"
                v-model:sort-direction="sortDirection"
                item-name="accounts"
              >
                <template #empty-action>
                  <div v-if="canWrite" class="flex gap-3 justify-center">
                    <Button
                      v-if="isMetaIntegrationReady"
                      size="lg"
                      @click="showOnboardingDialog = true"
                      :disabled="isConnectingFB || !isFBSDKLoaded"
                      class="bg-gradient-to-br from-facebook to-facebook-dark hover:from-facebook-hover hover:to-facebook-hoverDark text-white border-none shadow-none"
                    >
                      <Facebook v-if="!isConnectingFB" class="mr-2 h-5 w-5" />
                      <Loader2 v-else class="mr-2 h-5 w-5 animate-spin" />
                      {{ $t("accounts.connectFacebook") }}
                    </Button>
                    <RouterLink
                      v-else-if="canReadIntegrations"
                      to="/settings/integrations"
                    >
                      <Button variant="outline" size="lg">
                        <PlugZap class="mr-2 h-5 w-5" />
                        Configure Meta integration
                      </Button>
                    </RouterLink>
                    <RouterLink to="/settings/accounts/new">
                      <Button variant="outline" size="lg">
                        <Plus class="mr-2 h-5 w-5" />
                        {{ $t("accounts.addAccount") }}
                      </Button>
                    </RouterLink>
                  </div>
                </template>
                <template #cell-account="{ item: account }">
                  <RouterLink
                    :to="`/settings/accounts/${account.id}`"
                    class="flex items-center gap-3 text-inherit no-underline hover:opacity-80"
                  >
                    <div
                      class="h-9 w-9 rounded-full bg-emerald-500/10 flex items-center justify-center flex-shrink-0"
                    >
                      <Phone class="h-4 w-4 text-emerald-500" />
                    </div>
                    <p class="font-medium truncate">{{ account.name }}</p>
                  </RouterLink>
                </template>
                <template #cell-phone_id="{ item: account }">
                  <code class="text-xs bg-muted px-1.5 py-0.5 rounded">{{
                    account.phone_id
                  }}</code>
                </template>
                <template #cell-api_version="{ item: account }">
                  <span class="text-sm">{{ account.api_version }}</span>
                </template>
                <template #cell-defaults="{ item: account }">
                  <div class="flex items-center gap-1.5 flex-wrap">
                    <Badge
                      v-if="account.is_default_incoming"
                      variant="outline"
                      class="text-[10px]"
                    >
                      <Check class="h-2.5 w-2.5 mr-0.5" />
                      {{ $t("accounts.incoming") }}
                    </Badge>
                    <Badge
                      v-if="account.is_default_outgoing"
                      variant="outline"
                      class="text-[10px]"
                    >
                      <Check class="h-2.5 w-2.5 mr-0.5" />
                      {{ $t("accounts.outgoing") }}
                    </Badge>
                  </div>
                </template>
                <template #cell-status="{ item: account }">
                  <Badge
                    variant="outline"
                    :class="
                      account.status === 'active'
                        ? 'border-green-600 text-green-600'
                        : ''
                    "
                  >
                    {{ account.status }}
                  </Badge>
                </template>
                <template #cell-created="{ item: account }">
                  <span class="text-muted-foreground">{{
                    formatDate(account.created_at)
                  }}</span>
                </template>
                <template #cell-actions="{ item: account }">
                  <div class="flex items-center justify-end gap-1">
                    <Tooltip>
                      <TooltipTrigger as-child>
                        <RouterLink :to="`/settings/accounts/${account.id}`">
                          <Button variant="ghost" size="icon" class="h-8 w-8"
                            ><Pencil class="h-4 w-4"
                          /></Button>
                        </RouterLink>
                      </TooltipTrigger>
                      <TooltipContent>{{ $t("common.edit") }}</TooltipContent>
                    </Tooltip>
                    <Tooltip v-if="canDelete">
                      <TooltipTrigger as-child>
                        <Button
                          variant="ghost"
                          size="icon"
                          class="h-8 w-8"
                          @click="openDeleteDialog(account)"
                        >
                          <Trash2 class="h-4 w-4 text-destructive" />
                        </Button>
                      </TooltipTrigger>
                      <TooltipContent>{{ $t("common.delete") }}</TooltipContent>
                    </Tooltip>
                  </div>
                </template>
              </DataTable>
            </CardContent>
          </Card>
        </div>
      </div>
    </ScrollArea>

    <DeleteConfirmDialog
      v-model:open="deleteDialogOpen"
      :title="$t('accounts.deleteAccount')"
      :item-name="accountToDelete?.name"
      :is-submitting="isDeleting"
      @confirm="confirmDelete"
    />

    <!-- Onboarding Method Selection Dialog -->
    <Dialog v-model:open="showOnboardingDialog">
      <DialogContent
        class="sm:max-w-2xl bg-[#0e0e11] border-[#222227] text-white light:bg-white light:border-gray-200 light:text-gray-900 p-6 shadow-2xl rounded-xl"
      >
        <DialogHeader class="mb-4">
          <DialogTitle
            class="text-xl font-bold bg-gradient-to-r from-emerald-400 to-green-400 light:from-emerald-600 light:to-green-600 bg-clip-text text-transparent flex items-center gap-2"
          >
            {{ $t("accounts.connectTitle") }}
          </DialogTitle>
          <DialogDescription class="text-gray-400 light:text-gray-500 mt-1">
            {{ $t("accounts.connectDesc") }}
          </DialogDescription>
        </DialogHeader>

        <div class="grid grid-cols-1 md:grid-cols-2 gap-4 my-4">
          <!-- Coexistence Option Card -->
          <div
            @click="launchWhatsAppSignup(true)"
            class="relative group cursor-pointer flex flex-col p-5 rounded-xl border border-emerald-500/20 bg-[#141419] hover:bg-[#181822] hover:border-emerald-500/50 hover:shadow-[0_0_20px_rgba(16,185,129,0.1)] light:bg-gray-50/50 light:border-emerald-200 light:hover:bg-gray-100/70 light:hover:border-emerald-400 light:hover:shadow-[0_0_20px_rgba(16,185,129,0.05)] transition-all duration-300 overflow-hidden"
          >
            <!-- Badge -->
            <div class="absolute top-3 right-3">
              <span
                class="text-[10px] bg-emerald-500/10 text-emerald-400 border border-emerald-500/20 px-2 py-0.5 rounded-full font-medium light:bg-emerald-50 light:text-emerald-600 light:border-emerald-200"
              >
                {{ $t("accounts.coexistenceRecommend") }}
              </span>
            </div>

            <div
              class="h-10 w-10 rounded-lg bg-emerald-500/10 light:bg-emerald-100/60 flex items-center justify-center mb-4 group-hover:scale-110 transition-transform duration-300"
            >
              <Smartphone
                class="h-5 w-5 text-emerald-400 light:text-emerald-600"
              />
            </div>

            <h3
              class="text-base font-semibold text-white light:text-gray-900 group-hover:text-emerald-400 light:group-hover:text-emerald-600 transition-colors duration-200"
            >
              {{ $t("accounts.coexistenceTitle") }}
            </h3>
            <p
              class="text-xs text-gray-400 light:text-gray-600 mt-2 flex-grow leading-relaxed"
            >
              {{ $t("accounts.coexistenceDesc") }}
            </p>

            <div
              class="mt-5 flex items-center justify-between text-xs font-medium text-emerald-400 light:text-emerald-600"
            >
              <span>{{ $t("accounts.selectMode") }}</span>
              <span
                class="group-hover:translate-x-1 transition-transform duration-200"
                >→</span
              >
            </div>
          </div>

          <!-- Classic Option Card -->
          <div
            @click="launchWhatsAppSignup(false)"
            class="relative group cursor-pointer flex flex-col p-5 rounded-xl border border-[#222227] bg-[#141419] hover:bg-[#181822] hover:border-blue-500/50 hover:shadow-[0_0_20px_rgba(59,130,246,0.1)] light:bg-gray-50/50 light:border-gray-200 light:hover:bg-gray-100/70 light:hover:border-blue-400 light:hover:shadow-[0_0_20px_rgba(59,130,246,0.05)] transition-all duration-300 overflow-hidden"
          >
            <!-- Badge -->
            <div class="absolute top-3 right-3">
              <span
                class="text-[10px] bg-blue-500/10 text-blue-400 border border-blue-500/20 px-2 py-0.5 rounded-full font-medium light:bg-blue-50 light:text-blue-600 light:border-blue-200"
              >
                {{ $t("accounts.classicRecommend") }}
              </span>
            </div>

            <div
              class="h-10 w-10 rounded-lg bg-blue-500/10 light:bg-blue-100/60 flex items-center justify-center mb-4 group-hover:scale-110 transition-transform duration-300"
            >
              <Network class="h-5 w-5 text-blue-400 light:text-blue-600" />
            </div>

            <h3
              class="text-base font-semibold text-white light:text-gray-900 group-hover:text-blue-400 light:group-hover:text-blue-600 transition-colors duration-200"
            >
              {{ $t("accounts.classicTitle") }}
            </h3>
            <p
              class="text-xs text-gray-400 light:text-gray-600 mt-2 flex-grow leading-relaxed"
            >
              {{ $t("accounts.classicDesc") }}
            </p>

            <div
              class="mt-5 flex items-center justify-between text-xs font-medium text-blue-400 light:text-blue-600"
            >
              <span>{{ $t("accounts.selectMode") }}</span>
              <span
                class="group-hover:translate-x-1 transition-transform duration-200"
                >→</span
              >
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  </div>
</template>
