/** @vitest-environment happy-dom */

import { flushPromises, mount, RouterLinkStub } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import AccountsView from "./AccountsView.vue";

const organizationId = "b1111111-1111-4111-8111-111111111111";

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  login: vi.fn(),
  toastError: vi.fn(),
  accounts: [] as Array<Record<string, unknown>>,
}));

vi.mock("@/services/api", () => ({
  api: { get: mocks.get, post: mocks.post },
}));
vi.mock("@/stores/auth", () => ({
  useAuthStore: () => ({
    hasPermission: () => true,
    organizationId,
  }),
}));
vi.mock("@/stores/organizations", () => ({
  useOrganizationsStore: () => ({
    selectedOrgId: organizationId,
    blockOrganizationSwitch: vi.fn(),
    unblockOrganizationSwitch: vi.fn(),
  }),
}));
vi.mock("vue-router", () => ({ onBeforeRouteLeave: vi.fn() }));
vi.mock("vue-i18n", async (importOriginal) => ({
  ...(await importOriginal<typeof import("vue-i18n")>()),
  useI18n: () => ({ t: (key: string) => key }),
}));
vi.mock("vue-sonner", () => ({
  toast: {
    success: vi.fn(),
    error: mocks.toastError,
    warning: vi.fn(),
    info: vi.fn(),
  },
}));
// The dialog renders inline while open so the steps can be driven directly.
vi.mock("@/components/ui/dialog", async () => {
  const { defineComponent } = await import("vue");
  const Passthrough = defineComponent({ template: "<div><slot /></div>" });
  return {
    Dialog: defineComponent({
      props: { open: Boolean },
      template: '<div v-if="open" data-testid="dialog"><slot /></div>',
    }),
    DialogContent: Passthrough,
    DialogDescription: Passthrough,
    DialogHeader: Passthrough,
    DialogTitle: Passthrough,
  };
});
vi.mock("@/components/ui/tooltip", async () => {
  const { defineComponent } = await import("vue");
  const Passthrough = defineComponent({ template: "<div><slot /></div>" });
  return {
    Tooltip: Passthrough,
    TooltipContent: Passthrough,
    TooltipTrigger: Passthrough,
  };
});
vi.mock("@/components/ui/scroll-area", async () => {
  const { defineComponent } = await import("vue");
  return {
    ScrollArea: defineComponent({ template: "<div><slot /></div>" }),
  };
});
vi.mock("@/components/shared", async () => {
  const { defineComponent } = await import("vue");
  const Empty = defineComponent({ template: "<div><slot /></div>" });
  return {
    PageHeader: defineComponent({
      template: '<header><slot name="actions" /></header>',
    }),
    DeleteConfirmDialog: Empty,
    ErrorState: Empty,
    DataTable: defineComponent({
      props: { items: { type: Array, default: () => [] } },
      template:
        '<div><slot v-if="!items.length" name="empty-action" /><span v-for="item in items" :key="item.id">{{ item.name }}</span></div>',
    }),
  };
});

type LoginCallback = (response: unknown) => void;

function finishMessage(data: Record<string, unknown>, event = "FINISH") {
  window.dispatchEvent(
    new MessageEvent("message", {
      origin: "https://www.facebook.com",
      data: { type: "WA_EMBEDDED_SIGNUP", event, data },
    }),
  );
}

let wrapper: ReturnType<typeof mount> | undefined;

async function openAccounts() {
  wrapper = mount(AccountsView, {
    attachTo: document.body,
    global: {
      mocks: { $t: (key: string) => key },
      stubs: { RouterLink: RouterLinkStub },
    },
  });
  await flushPromises();
  return wrapper;
}

async function openConnectionDialog(view: ReturnType<typeof mount>) {
  const connect = view
    .findAll("button")
    .find((button) => button.text().includes("accounts.connectFacebook"));
  expect(connect).toBeDefined();
  await connect!.trigger("click");
  await flushPromises();
}

async function chooseCoexistence(view: ReturnType<typeof mount>) {
  await openConnectionDialog(view);
  const card = view
    .findAll("button")
    .find((button) => button.text().includes("accounts.coexistenceTitle"));
  expect(card).toBeDefined();
  await card!.trigger("click");
  await flushPromises();
}

function lastLogin() {
  const call = mocks.login.mock.calls.at(-1);
  expect(call).toBeDefined();
  return {
    callback: call![0] as LoginCallback,
    options: call![1] as Record<string, any>,
  };
}

function exchangeBodies() {
  return mocks.post.mock.calls
    .filter(([url]) => url === "/accounts/exchange-token")
    .map(([, body]) => body as Record<string, unknown>);
}

beforeEach(() => {
  mocks.get.mockReset();
  mocks.post.mockReset();
  mocks.login.mockReset();
  mocks.toastError.mockReset();
  mocks.accounts = [];
  mocks.get.mockImplementation(async (url: string) => {
    if (url === "/embedded-signup/config") {
      return {
        data: {
          data: {
            organization_id: organizationId,
            whatsapp_app_id: "1000000000000001",
            whatsapp_config_id: "1000000000000002",
            whatsapp_api_version: "v24.0",
            has_app_secret: true,
          },
        },
      };
    }
    if (url === "/accounts") {
      return { data: { data: { accounts: mocks.accounts } } };
    }
    throw new Error(`unexpected GET ${url}`);
  });
  mocks.post.mockResolvedValue({
    data: {
      data: {
        account: { id: "account-1", name: "Synthetic", status: "active" },
      },
    },
  });
  (window as unknown as { FB: unknown }).FB = {
    init: vi.fn(),
    login: mocks.login,
  };
});

afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
  delete (window as unknown as { FB?: unknown }).FB;
});

describe("AccountsView Coexistence number step", () => {
  it("asks for the number before opening Meta's login", async () => {
    const view = await openAccounts();
    await chooseCoexistence(view);

    expect(mocks.login).not.toHaveBeenCalled();
    expect(view.text()).toContain("accounts.coexistenceNumberTitle");
    const input = view.get("#coexistence-phone-number");
    expect(input.attributes("type")).toBe("tel");
    expect(input.attributes("autocomplete")).toBe("tel");
    // No workspace account exists, so there is nothing to reconnect.
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(false);

    const back = view
      .findAll("button")
      .find((button) => button.text().includes("common.back"));
    await back!.trigger("click");
    expect(view.text()).toContain("accounts.coexistenceTitle");
    expect(view.find("#coexistence-phone-number").exists()).toBe(false);
  });

  it.each([
    ["", "accounts.coexistenceNumberRequired"],
    ["012-345 6789", "accounts.coexistenceNumberMissingCountryCode"],
    ["+60 12 34", "accounts.coexistenceNumberInvalid"],
    ["+60 12-345-6789 x2", "accounts.coexistenceNumberInvalid"],
  ])(
    "refuses %j without opening Meta's login",
    async (value, expectedError) => {
      const view = await openAccounts();
      await chooseCoexistence(view);
      await view.get("#coexistence-phone-number").setValue(value);
      await view.get("form").trigger("submit");
      await flushPromises();

      expect(mocks.login).not.toHaveBeenCalled();
      expect(view.get("#coexistence-phone-number-error").text()).toBe(
        expectedError,
      );
      expect(
        view.get("#coexistence-phone-number").attributes("aria-invalid"),
      ).toBe("true");
      expect(exchangeBodies()).toEqual([]);
    },
  );

  it("sends the entered number's digits with a WABA-only Coexistence completion", async () => {
    const view = await openAccounts();
    await chooseCoexistence(view);
    await view.get("#coexistence-phone-number").setValue(" +60 12-345 6789 ");
    await view.get("form").trigger("submit");

    const { callback, options } = lastLogin();
    expect(options.extras).toEqual({
      setup: {},
      featureType: "whatsapp_business_app_onboarding",
      sessionInfoVersion: "3",
    });
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();

    expect(exchangeBodies()).toEqual([
      {
        code: "review-safe-code",
        signup_mode: "coexistence",
        phone_id: undefined,
        waba_id: "1000000000000004",
        phone_number_hint: "60123456789",
      },
    ]);
  });

  it("never sends the number in classic mode", async () => {
    const view = await openAccounts();
    await chooseCoexistence(view);
    // A number typed in the Coexistence step stays in that step.
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    const back = view
      .findAll("button")
      .find((button) => button.text().includes("common.back"));
    await back!.trigger("click");
    const classic = view
      .findAll("button")
      .find((button) => button.text().includes("accounts.classicTitle"));
    await classic!.trigger("click");

    const { callback, options } = lastLogin();
    expect(options.extras).toEqual({ setup: {} });
    finishMessage({ waba_id: "1000000000000004" }, "FINISH_ONLY_WABA");
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();

    const bodies = exchangeBodies();
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toMatchObject({
      code: "review-safe-code",
      signup_mode: "classic",
      waba_id: "1000000000000004",
    });
    expect(bodies[0]?.phone_number_hint).toBeUndefined();
  });

  it("makes the number optional when reconnecting a workspace account", async () => {
    mocks.accounts = [
      {
        id: "b4444444-4444-4444-8444-444444444444",
        name: "Existing clinic number",
        phone_id: "1000000000000003",
        status: "active",
        created_at: "2026-09-04T01:00:00Z",
      },
    ];
    const view = await openAccounts();
    await chooseCoexistence(view);

    const reconnect = view.get("#coexistence-reconnect-account");
    expect(reconnect.text()).toContain("accounts.coexistenceReconnectOption");
    await reconnect.setValue("b4444444-4444-4444-8444-444444444444");
    expect(
      view.get("#coexistence-phone-number").attributes("disabled"),
    ).toBeDefined();
    await view.get("form").trigger("submit");

    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();

    expect(exchangeBodies()).toEqual([
      {
        code: "review-safe-code",
        signup_mode: "coexistence",
        phone_id: "1000000000000003",
        waba_id: "1000000000000004",
        phone_number_hint: undefined,
      },
    ]);
  });
});
