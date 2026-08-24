// @vitest-environment happy-dom

import { flushPromises, shallowMount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { MetaInstagramOffPilotError } from "@/services/metaInstagramOnboarding";
import ChannelsView from "@/views/channels/ChannelsView.vue";

const mocks = vi.hoisted(() => ({
  organizationId: "11111111-1111-4111-8111-111111111111",
  status: vi.fn(),
  begin: vi.fn(),
  accounts: vi.fn(),
  conversations: vi.fn(),
  createAccount: vi.fn(),
  toast: {
    success: vi.fn(),
    warning: vi.fn(),
    error: vi.fn(),
  },
  blockOrganizationSwitch: vi.fn(),
  unblockOrganizationSwitch: vi.fn(),
  connectWebSocket: vi.fn().mockResolvedValue(undefined),
  onInboxActivity: vi.fn(() => vi.fn()),
  onConnectionStateChange: vi.fn((callback) => {
    callback("connected");
    return vi.fn();
  }),
  refreshUnread: vi.fn().mockResolvedValue(true),
}));

const organizationId = mocks.organizationId;

vi.mock("@vueuse/core", () => ({
  useMediaQuery: () => ({ value: false }),
}));

vi.mock("vue-router", () => ({
  onBeforeRouteLeave: vi.fn(),
}));

vi.mock("@/composables/useAppToast", () => ({
  useAppToast: () => mocks.toast,
}));

vi.mock("@/stores/auth", () => ({
  useAuthStore: () => ({
    organizationId: mocks.organizationId,
    user: { id: 'agent-1' },
    hasPermission: () => true,
    hasProductEntitlement: () => true,
  }),
}));

vi.mock("@/stores/organizations", () => ({
  useOrganizationsStore: () => ({
    selectedOrgId: mocks.organizationId,
    blockOrganizationSwitch: mocks.blockOrganizationSwitch,
    unblockOrganizationSwitch: mocks.unblockOrganizationSwitch,
  }),
}));

vi.mock("@/stores/omnichannelUnread", () => ({
  useOmnichannelUnreadStore: () => ({ refresh: mocks.refreshUnread }),
}));

vi.mock("@/services/websocket", () => ({
  isInboxContentRefreshActivity: (event: { type: string; payload?: any }) =>
    event.type !== "status_update"
    && !(event.type === "realtime_sync" && event.payload?.kind === "message_status_changed"),
  wsService: {
    getConnectionState: () => "connected",
    connect: mocks.connectWebSocket,
    onInboxActivity: mocks.onInboxActivity,
    onConnectionStateChange: mocks.onConnectionStateChange,
  },
}));

vi.mock("@/services/metaMessengerOnboarding", () => {
  class MetaMessengerAuthorizationCancelledError extends Error {}
  class MetaMessengerOrganizationChangedError extends Error {}
  return {
    MetaMessengerAuthorizationCancelledError,
    MetaMessengerOrganizationChangedError,
    metaMessengerOnboarding: {
      status: vi.fn().mockResolvedValue(false),
    },
  };
});

vi.mock("@/services/metaInstagramOnboarding", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/services/metaInstagramOnboarding")>();
  return {
    ...actual,
    metaInstagramOnboarding: {
      ...actual.metaInstagramOnboarding,
      status: mocks.status,
      begin: mocks.begin,
    },
  };
});

vi.mock("@/services/productSuite", () => ({
  channelsService: {
    accounts: mocks.accounts,
    conversations: mocks.conversations,
    createAccount: mocks.createAccount,
  },
}));

const PageHeaderStub = {
  template: "<header><slot name='actions' /></header>",
};
const ButtonStub = {
  template: "<button><slot /></button>",
};
const InputStub = {
  props: ["modelValue"],
  emits: ["update:modelValue"],
  template:
    "<input :value='modelValue' @input='$emit(\"update:modelValue\", $event.target.value)' />",
};

function pending<T>() {
  return new Promise<T>(() => undefined);
}

function response<T>(data: T) {
  return Promise.resolve({ data: { data } });
}

async function openConnect(wrapper: VueWrapper) {
  await flushPromises();
  const button = wrapper
    .findAll("button")
    .find((candidate) => candidate.text().trim() === "Connect");
  expect(button).toBeDefined();
  await button!.trigger("click");
}

describe("Channels managed Instagram creation gate", () => {
  let wrapper: VueWrapper | null = null;

  beforeEach(() => {
    vi.clearAllMocks();
    mocks.accounts.mockReturnValue(response({ accounts: [] }));
    mocks.conversations.mockReturnValue(
      response({ conversations: [], total: 0 }),
    );
    mocks.createAccount.mockReturnValue(
      response({
        account: { id: "synthetic-account" },
        inbound_secret: "synthetic-secret",
        webhook_path: "/api/webhooks/channels/synthetic-account",
      }),
    );
  });

  afterEach(() => {
    wrapper?.unmount();
    wrapper = null;
  });

  async function mountChannels() {
    wrapper = shallowMount(ChannelsView, {
      global: {
        stubs: {
          PageHeader: PageHeaderStub,
          Button: ButtonStub,
          Input: InputStub,
          Badge: { template: "<span><slot /></span>" },
          Sheet: { template: "<div><slot /></div>" },
          SheetContent: { template: "<div><slot /></div>" },
          SheetTitle: true,
          SheetDescription: true,
          CustomerRevenueWorkspace: true,
        },
      },
    });
    await openConnect(wrapper);
    return wrapper;
  }

  it("shows a fail-closed loading state without a static create action", async () => {
    mocks.status.mockReturnValue(pending());
    const view = await mountChannels();

    expect(
      view.find('[data-testid="instagram-onboarding-loading"]').exists(),
    ).toBe(true);
    expect(view.find('[data-testid="channel-connect-submit"]').exists()).toBe(
      false,
    );
    expect(mocks.createAccount).not.toHaveBeenCalled();
  });

  it("shows a fail-closed error state without a static create action", async () => {
    mocks.status.mockRejectedValue(new Error("synthetic status failure"));
    const view = await mountChannels();
    await flushPromises();

    expect(
      view.find('[data-testid="instagram-onboarding-error"]').exists(),
    ).toBe(true);
    expect(view.find('[data-testid="channel-connect-submit"]').exists()).toBe(
      false,
    );
    expect(mocks.createAccount).not.toHaveBeenCalled();
  });

  it("keeps quarantine-only workspaces on the managed unavailable surface", async () => {
    mocks.status.mockReturnValue(
      response({
        organization_id: organizationId,
        configured: true,
        enabled: false,
        quarantine_only: true,
        app_review_status: "approved",
      }).then((result) => result.data.data),
    );
    const view = await mountChannels();
    await flushPromises();

    expect(
      view.find('[data-testid="instagram-onboarding-unavailable"]').text(),
    ).toContain("quarantine-only mode");
    expect(view.find('[data-testid="channel-connect-submit"]').exists()).toBe(
      false,
    );
    expect(mocks.createAccount).not.toHaveBeenCalled();
  });

  it("preserves static Instagram creation for the exact off-pilot state", async () => {
    mocks.status.mockRejectedValue(new MetaInstagramOffPilotError());
    const view = await mountChannels();
    await flushPromises();

    expect(view.find('[data-testid="channel-connect-submit"]').exists()).toBe(
      true,
    );
    await view
      .find('input[placeholder="Connection name"]')
      .setValue("Synthetic Instagram");
    await view
      .find('input[placeholder="External account ID"]')
      .setValue("700000000000101");
    await view
      .find('input[placeholder="HTTPS signed-relay URL"]')
      .setValue("https://relay.example.test/instagram");
    await view.find('[data-testid="channel-connect-submit"]').trigger("click");
    await flushPromises();

    expect(mocks.createAccount).toHaveBeenCalledWith({
      channel: "instagram",
      provider: "relay",
      name: "Synthetic Instagram",
      external_account_id: "700000000000101",
      config: { relay_url: "https://relay.example.test/instagram" },
    });
  });
});
