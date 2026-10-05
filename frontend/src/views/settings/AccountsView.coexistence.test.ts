/** @vitest-environment happy-dom */

import { flushPromises, mount, RouterLinkStub } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import AccountsView from "./AccountsView.vue";

const organizationId = "b1111111-1111-4111-8111-111111111111";
const otherOrganizationId = "b9999999-9999-4999-8999-999999999999";

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  login: vi.fn(),
  toastError: vi.fn(),
  accounts: [] as Array<Record<string, unknown>>,
  // Overrides GET /accounts for one workspace, for example to keep it
  // loading or to fail it.
  accountsFor: undefined as
    | ((organizationId: string | undefined) => Promise<unknown> | undefined)
    | undefined,
  organizations: { selectedOrgId: "" } as { selectedOrgId: string },
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
vi.mock("@/stores/organizations", async () => {
  const { reactive } = await import("vue");
  const store = reactive({
    selectedOrgId: "",
    blockOrganizationSwitch: vi.fn(),
    unblockOrganizationSwitch: vi.fn(),
  });
  mocks.organizations = store;
  return { useOrganizationsStore: () => store };
});
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
  mocks.accountsFor = undefined;
  mocks.organizations.selectedOrgId = organizationId;
  mocks.get.mockImplementation(
    async (url: string, config?: { headers?: Record<string, string> }) => {
      if (url === "/embedded-signup/config") {
        return {
          data: {
            data: {
              organization_id: config?.headers?.["X-Organization-ID"],
              whatsapp_app_id: "1000000000000001",
              whatsapp_config_id: "1000000000000002",
              whatsapp_api_version: "v24.0",
              has_app_secret: true,
            },
          },
        };
      }
      if (url === "/accounts") {
        const override = mocks.accountsFor?.(
          config?.headers?.["X-Organization-ID"],
        );
        if (override) return override;
        return { data: { data: { accounts: mocks.accounts } } };
      }
      throw new Error(`unexpected GET ${url}`);
    },
  );
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
    ["+60 012-345 6789", "accounts.coexistenceNumberZeroAfterCountryCode"],
    ["+60 (0)12-345 6789", "accounts.coexistenceNumberZeroAfterCountryCode"],
    ["12-345 6789", "accounts.coexistenceNumberUsCanadaLength"],
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

  it("forgets the number and reconnect choice when the workspace changes", async () => {
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
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    await view
      .get("#coexistence-reconnect-account")
      .setValue("b4444444-4444-4444-8444-444444444444");

    mocks.organizations.selectedOrgId = otherOrganizationId;
    await flushPromises();
    mocks.organizations.selectedOrgId = organizationId;
    await flushPromises();

    expect(view.text()).toContain("accounts.coexistenceTitle");
    await chooseCoexistence(view);
    expect(
      (view.get("#coexistence-phone-number").element as HTMLInputElement).value,
    ).toBe("");
    expect(
      (view.get("#coexistence-reconnect-account").element as HTMLSelectElement)
        .value,
    ).toBe("");
    expect(mocks.login).not.toHaveBeenCalled();
  });

  it("shows the digits it will match and sends a pasted number as ASCII digits", async () => {
    const view = await openAccounts();
    await chooseCoexistence(view);
    const input = view.get("#coexistence-phone-number");
    expect(view.find("#coexistence-phone-number-preview").exists()).toBe(false);

    // A kept 0 after +60 shows the fix, not a preview.
    await input.setValue("+60 012-345 6789");
    expect(view.find("#coexistence-phone-number-preview").exists()).toBe(false);

    // Copied from a contacts app: bidi marks, no-break spaces and a
    // non-breaking hyphen around an otherwise valid number.
    await input.setValue("\u202a+60\u00a012\u2011345\u00a06789\u202c");
    const preview = view.get("#coexistence-phone-number-preview");
    expect(preview.text()).toContain("accounts.coexistenceNumberPreview");
    expect(preview.text()).toContain("+60123456789");
    expect(preview.get("span").attributes("dir")).toBe("ltr");
    expect(input.attributes("aria-describedby")).toContain(
      "coexistence-phone-number-preview",
    );
    expect(view.get("#coexistence-phone-number-error").text()).toBe("");

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
        phone_id: undefined,
        waba_id: "1000000000000004",
        phone_number_hint: "60123456789",
      },
    ]);
  });

  it("forgets the number after a connection, so the next signup names its own", async () => {
    const view = await openAccounts();
    await chooseCoexistence(view);
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();
    expect(exchangeBodies()).toHaveLength(1);

    // Adding another number starts empty: pressing Continue without typing
    // never reuses the number that is now connected.
    await chooseCoexistence(view);
    expect(
      (view.get("#coexistence-phone-number").element as HTMLInputElement).value,
    ).toBe("");
    expect(view.find("#coexistence-phone-number-preview").exists()).toBe(false);
    await view.get("form").trigger("submit");
    await flushPromises();
    expect(view.get("#coexistence-phone-number-error").text()).toBe(
      "accounts.coexistenceNumberRequired",
    );
    expect(mocks.login).toHaveBeenCalledTimes(1);
    expect(exchangeBodies()).toHaveLength(1);
  });

  it("forgets the reconnect choice after a connection", async () => {
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
    await view
      .get("#coexistence-reconnect-account")
      .setValue("b4444444-4444-4444-8444-444444444444");
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();
    expect(exchangeBodies()).toHaveLength(1);

    await chooseCoexistence(view);
    expect(
      (view.get("#coexistence-reconnect-account").element as HTMLSelectElement)
        .value,
    ).toBe("");
    expect(
      view.get("#coexistence-phone-number").attributes("disabled"),
    ).toBeUndefined();
  });

  it("keeps the number after a refusal so it can be corrected", async () => {
    mocks.post.mockRejectedValueOnce({
      isAxiosError: true,
      response: {
        status: 400,
        data: {
          message:
            "the number ending in 6789 is not listed in the selected WhatsApp Business Account",
        },
      },
    });
    const view = await openAccounts();
    await chooseCoexistence(view);
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();
    expect(exchangeBodies()).toHaveLength(1);
    expect(mocks.toastError).toHaveBeenCalled();

    await chooseCoexistence(view);
    expect(
      (view.get("#coexistence-phone-number").element as HTMLInputElement).value,
    ).toBe("+60 12-345 6789");
  });

  it("forgets the number when the connection result could not be confirmed", async () => {
    // No response: Meta may have accepted the number.
    mocks.post.mockRejectedValueOnce(new Error("Network Error"));
    const view = await openAccounts();
    await chooseCoexistence(view);
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();
    expect(exchangeBodies()).toHaveLength(1);

    await chooseCoexistence(view);
    expect(
      (view.get("#coexistence-phone-number").element as HTMLInputElement).value,
    ).toBe("");
  });

  it("never offers another workspace's accounts while this workspace's list loads", async () => {
    mocks.accounts = [
      {
        id: "b4444444-4444-4444-8444-444444444444",
        name: "Workspace A number",
        phone_id: "1000000000000003",
        status: "active",
        created_at: "2026-09-04T01:00:00Z",
      },
    ];
    let resolveOtherAccounts: (value: unknown) => void = () => {};
    mocks.accountsFor = (requestedOrganizationId) =>
      requestedOrganizationId === otherOrganizationId
        ? new Promise((resolve) => {
            resolveOtherAccounts = resolve;
          })
        : undefined;
    const view = await openAccounts();
    await chooseCoexistence(view);
    expect(
      view
        .findAll("#coexistence-reconnect-account option")
        .map((option) => option.attributes("value")),
    ).toEqual(["", "b4444444-4444-4444-8444-444444444444"]);

    mocks.organizations.selectedOrgId = otherOrganizationId;
    await flushPromises();
    await chooseCoexistence(view);
    // Workspace B's list is still loading: nothing to reconnect, and
    // workspace A's account is gone from the page.
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(false);
    expect(view.text()).not.toContain("Workspace A number");

    resolveOtherAccounts({
      data: {
        data: {
          accounts: [
            {
              id: "b5555555-5555-4555-8555-555555555555",
              name: "Workspace B number",
              phone_id: "1000000000000005",
              status: "active",
              created_at: "2026-09-04T01:00:00Z",
            },
          ],
        },
      },
    });
    await flushPromises();
    expect(
      view
        .findAll("#coexistence-reconnect-account option")
        .map((option) => option.attributes("value")),
    ).toEqual(["", "b5555555-5555-4555-8555-555555555555"]);
  });

  it("never offers another workspace's accounts when this workspace's list fails", async () => {
    mocks.accounts = [
      {
        id: "b4444444-4444-4444-8444-444444444444",
        name: "Workspace A number",
        phone_id: "1000000000000003",
        status: "active",
        created_at: "2026-09-04T01:00:00Z",
      },
    ];
    mocks.accountsFor = (requestedOrganizationId) =>
      requestedOrganizationId === otherOrganizationId
        ? Promise.reject(new Error("accounts unavailable"))
        : undefined;
    const view = await openAccounts();
    await chooseCoexistence(view);
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(true);

    mocks.organizations.selectedOrgId = otherOrganizationId;
    await flushPromises();
    await chooseCoexistence(view);
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(false);
    expect(view.text()).not.toContain("Workspace A number");

    // Workspace A's phone ID can no longer be sent with workspace B.
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();

    const exchanges = mocks.post.mock.calls.filter(
      ([url]) => url === "/accounts/exchange-token",
    );
    expect(exchanges).toHaveLength(1);
    expect(exchanges[0]?.[1]).toEqual({
      code: "review-safe-code",
      signup_mode: "coexistence",
      phone_id: undefined,
      waba_id: "1000000000000004",
      phone_number_hint: "60123456789",
    });
    expect(exchanges[0]?.[2]).toMatchObject({
      headers: { "X-Organization-ID": otherOrganizationId },
    });
  });

  it("offers no reconnect choice after this workspace's list fails to reload", async () => {
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
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(true);
    await view.get("#coexistence-phone-number").setValue("+60 12-345 6789");
    // The list reload that follows the connection fails.
    mocks.accountsFor = () => Promise.reject(new Error("accounts unavailable"));
    await view.get("form").trigger("submit");
    const { callback } = lastLogin();
    finishMessage(
      { waba_id: "1000000000000004" },
      "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
    );
    callback({ authResponse: { code: "review-safe-code" } });
    await flushPromises();
    expect(exchangeBodies()).toHaveLength(1);

    // The earlier list of this workspace is still in memory, but it is no
    // longer known to be current, so nothing is offered.
    await chooseCoexistence(view);
    expect(view.find("#coexistence-reconnect-account").exists()).toBe(false);
  });
});
