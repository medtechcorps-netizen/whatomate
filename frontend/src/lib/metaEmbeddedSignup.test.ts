import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createMetaEmbeddedSignupSession,
  isAllowedMetaEmbeddedSignupOrigin,
  META_COEXISTENCE_SELECTION_TIMEOUT_MS,
  META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS,
  normalizeCoexistencePhoneNumber,
  type MetaEmbeddedSignupAbortReason,
  type MetaEmbeddedSignupMode,
  type MetaEmbeddedSignupResult,
} from "./metaEmbeddedSignup";

const facebookOrigin = "https://www.facebook.com";

function finishMessage(
  origin = facebookOrigin,
  data: unknown = {
    type: "WA_EMBEDDED_SIGNUP",
    event: "FINISH",
    data: { phone_number_id: "phone-123", waba_id: "waba-456" },
  },
) {
  return { origin, data };
}

function createHarness(
  codeFallbackMs = 50,
  context?: { isCurrent: () => boolean },
  mode: MetaEmbeddedSignupMode = "classic",
  target: { phoneNumberHint?: string; reconnectPhoneNumberId?: string } = {},
) {
  const completed: Array<Omit<MetaEmbeddedSignupResult, "diagnostics">> = [];
  const aborted: Array<{
    reason: MetaEmbeddedSignupAbortReason;
    detail?: string;
  }> = [];
  // Every settled outcome's diagnostics, in order.
  const diagnostics: string[] = [];
  let settledCount = 0;
  let contextChangedCount = 0;
  const session = createMetaEmbeddedSignupSession({
    mode,
    codeFallbackMs,
    ...target,
    onComplete: ({ diagnostics: summary, ...result }) => {
      diagnostics.push(summary);
      completed.push(result);
    },
    onAbort: (reason, detail, summary) => {
      diagnostics.push(summary);
      aborted.push({ reason, detail });
    },
    onSettled: () => {
      settledCount += 1;
    },
    isContextCurrent: context?.isCurrent,
    onContextChanged: () => {
      contextChangedCount += 1;
    },
  });

  return {
    aborted,
    completed,
    diagnostics,
    session,
    contextChangedCount: () => contextChangedCount,
    settledCount: () => settledCount,
  };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("Meta Embedded Signup origin validation", () => {
  it("accepts only supported Meta Embedded Signup hosts on HTTPS's default port", () => {
    expect(isAllowedMetaEmbeddedSignupOrigin("https://www.facebook.com")).toBe(
      true,
    );
    expect(isAllowedMetaEmbeddedSignupOrigin("https://web.facebook.com")).toBe(
      true,
    );
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://business.facebook.com"),
    ).toBe(true);
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://signup.business.facebook.com"),
    ).toBe(true);
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://business.facebook.com:443"),
    ).toBe(true);
  });

  it("rejects non-HTTPS, non-default ports, and hostname lookalikes", () => {
    expect(isAllowedMetaEmbeddedSignupOrigin("https://facebook.com")).toBe(
      false,
    );
    expect(isAllowedMetaEmbeddedSignupOrigin("http://www.facebook.com")).toBe(
      false,
    );
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://business.facebook.com:444"),
    ).toBe(false);
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://www.facebook.com.example.org"),
    ).toBe(false);
    expect(isAllowedMetaEmbeddedSignupOrigin("https://evilfacebook.com")).toBe(
      false,
    );
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://arbitrary.facebook.com"),
    ).toBe(false);
  });
});

describe("createMetaEmbeddedSignupSession", () => {
  it("completes when the asset message arrives before the login code", () => {
    const harness = createHarness();

    harness.session.handleMessage(finishMessage());
    expect(harness.completed).toHaveLength(0);
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: "phone-123",
        wabaId: "waba-456",
      },
    ]);
    expect(harness.settledCount()).toBe(1);
  });

  it("completes when the login code arrives before the asset message", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage(
      finishMessage(
        facebookOrigin,
        JSON.stringify({
          type: "WA_EMBEDDED_SIGNUP",
          event: "FINISH",
          data: { phone_number_id: "phone-123", waba_id: "waba-456" },
        }),
      ),
    );
    vi.advanceTimersByTime(100);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: "phone-123",
        wabaId: "waba-456",
      },
    ]);
    expect(harness.settledCount()).toBe(1);
  });

  it("supports WABA-only completion events", () => {
    const harness = createHarness();

    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_ONLY_WABA",
        data: { waba_id: "waba-456" },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: undefined,
        wabaId: "waba-456",
      },
    ]);
  });

  it("completes WhatsApp Business App onboarding with a WABA-only message", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");

    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: "waba-789" },
      },
    });
    vi.advanceTimersByTime(100);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: "waba-789",
      },
    ]);
    expect(harness.settledCount()).toBe(1);
  });

  it("waits for the bounded fallback when a finish event has no WABA", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: { phone_number_id: "phone-123" },
      },
    });
    expect(harness.completed).toHaveLength(0);

    vi.advanceTimersByTime(50);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: "phone-123",
        wabaId: undefined,
      },
    ]);
  });

  it("uses a bounded code-only fallback and never trusts IDs in authResponse", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.handleLoginResponse({
      authResponse: {
        code: "code-abc",
        phone_number_id: "wrong-phone-source",
        waba_id: "wrong-waba-source",
      },
    });
    vi.advanceTimersByTime(49);
    expect(harness.completed).toHaveLength(0);
    vi.advanceTimersByTime(1);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: undefined,
        wabaId: undefined,
      },
    ]);
    expect(harness.settledCount()).toBe(1);
  });

  it("ignores forged and unrelated window messages", () => {
    vi.useFakeTimers();
    const harness = createHarness();
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    harness.session.handleMessage(
      finishMessage("https://www.facebook.com.example.org"),
    );
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: { type: "SOMETHING_ELSE", event: "FINISH" },
    });
    vi.advanceTimersByTime(50);

    expect(harness.completed[0]).toEqual({
      code: "code-abc",
      mode: "classic",
      phoneNumberId: undefined,
      wabaId: undefined,
    });
  });

  it.each([
    ["FINISH", undefined],
    ["FINISH", 3],
    ["FINISH_ONLY_WABA", undefined],
    ["FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING", undefined],
    ["FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING", "3"],
    ["FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING", 4],
    ["FINISH", "4"],
  ])(
    "retains selected assets and Coexistence mode for %s version %s",
    (event, version) => {
      const harness = createHarness(50, undefined, "coexistence");
      harness.session.handleMessage({
        origin: facebookOrigin,
        data: {
          type: "WA_EMBEDDED_SIGNUP",
          event,
          version,
          data: { waba_id: "selected-waba", phone_number_id: "selected-phone" },
        },
      });
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc" },
      });

      expect(harness.completed).toEqual([
        {
          code: "code-abc",
          mode: "coexistence",
          wabaId: "selected-waba",
          phoneNumberId: "selected-phone",
        },
      ]);
      expect(harness.aborted).toHaveLength(0);
    },
  );

  it("retains a Coexistence selection received after the classic fallback deadline", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    vi.advanceTimersByTime(5_001);
    expect(harness.completed).toHaveLength(0);
    expect(harness.settledCount()).toBe(0);

    harness.session.handleMessage(finishMessage());
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        wabaId: "waba-456",
        phoneNumberId: "phone-123",
      },
    ]);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.settledCount()).toBe(1);
  });

  it.each([1, 2, "2", "", " 3", "3.0", 3.5, 1000, null, true, {}, []])(
    "never uses IDs from unsupported Coexistence version %j and falls back to a code-only exchange",
    (version) => {
      vi.useFakeTimers();
      const harness = createHarness(50, undefined, "coexistence");
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc" },
      });
      for (const event of [
        "FINISH",
        "FINISH_ONLY_WABA",
        "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
      ]) {
        harness.session.handleMessage({
          origin: facebookOrigin,
          data: {
            type: "WA_EMBEDDED_SIGNUP",
            event,
            version,
            data: { waba_id: "wrong-version-waba" },
          },
        });
      }
      vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS - 1);
      expect(harness.completed).toHaveLength(0);
      vi.advanceTimersByTime(1);

      // The server accepts a code-only exchange only when the token grants
      // exactly one WABA, so the unsupported message's ID is never sent.
      expect(harness.completed).toEqual([
        {
          code: "code-abc",
          mode: "coexistence",
          phoneNumberId: undefined,
          wabaId: undefined,
        },
      ]);
      expect(harness.aborted).toHaveLength(0);
      expect(harness.settledCount()).toBe(1);
      expect(harness.diagnostics).toHaveLength(1);
      expect(harness.diagnostics[0]).toContain(
        "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=",
      );
      expect(harness.diagnostics[0]).toMatch(
        /; coexistence sent code after a 15s wait$/,
      );
      expect(harness.diagnostics[0]).not.toMatch(/wrong-version-waba|code-abc/);
    },
  );

  it.each([undefined, { phone_number_id: "phone-only" }])(
    "falls back to a bounded code-only exchange when no Coexistence WABA arrives",
    (data) => {
      vi.useFakeTimers();
      const harness = createHarness(50, undefined, "coexistence");
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc", waba_id: "untrusted-auth-waba" },
      });
      if (data) {
        harness.session.handleMessage({
          origin: facebookOrigin,
          data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data },
        });
      }
      vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS - 1);
      expect(harness.completed).toHaveLength(0);
      expect(harness.aborted).toHaveLength(0);
      vi.advanceTimersByTime(1);
      // A selection after the deadline cannot change the settled exchange.
      harness.session.handleMessage(finishMessage());

      expect(harness.completed).toEqual([
        {
          code: "code-abc",
          mode: "coexistence",
          phoneNumberId: undefined,
          wabaId: undefined,
        },
      ]);
      expect(harness.aborted).toHaveLength(0);
      expect(harness.settledCount()).toBe(1);
      expect(harness.diagnostics).toEqual([
        data
          ? "Meta signup diagnostics: FINISH v=none waba_id=absent waba_ids=absent phone_number_id=present (0s after code); coexistence sent code after a 15s wait"
          : "Meta signup diagnostics: no WA_EMBEDDED_SIGNUP message received; coexistence sent code after a 15s wait",
      ]);
    },
  );

  it("keeps the Coexistence wait within half of Meta's 30 second code lifetime", () => {
    expect(META_COEXISTENCE_SELECTION_TIMEOUT_MS).toBeGreaterThan(
      META_EMBEDDED_SIGNUP_CODE_FALLBACK_MS,
    );
    expect(META_COEXISTENCE_SELECTION_TIMEOUT_MS).toBeLessThanOrEqual(15_000);
  });

  it.each([
    { waba_ids: [" selected-waba ", "selected-waba", ""] },
    { waba_id: "selected-waba", waba_ids: ["selected-waba"] },
    // Meta's documented multi-WABA shape lists every shared WABA in waba_ids
    // and names the flow's WABA in waba_id.
    { waba_id: "selected-waba", waba_ids: ["other-waba", "selected-waba"] },
    { waba_id: null, waba_ids: ["selected-waba"] },
  ])(
    "accepts one distinct selected WABA from singular/plural fields %j",
    (data) => {
      const harness = createHarness(50, undefined, "coexistence");
      harness.session.handleMessage({
        origin: facebookOrigin,
        data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH_ONLY_WABA", data },
      });
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc" },
      });
      expect(harness.completed).toEqual([
        {
          code: "code-abc",
          mode: "coexistence",
          wabaId: "selected-waba",
          phoneNumberId: undefined,
        },
      ]);
    },
  );

  it("prefers the singular waba_id in Meta's documented multi-WABA Coexistence completion", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: "waba-a", waba_ids: ["waba-a", "waba-b"] },
      },
    });
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: "waba-a",
      },
    ]);
    expect(harness.aborted).toHaveLength(0);
  });

  it.each([
    [
      { waba_ids: ["waba-a", "waba-b"] },
      "more than one selected WhatsApp account",
    ],
    [
      { waba_id: "waba-a", waba_ids: ["waba-b"] },
      "conflicting WhatsApp account selections",
    ],
    [
      { waba_id: "waba-a", waba_ids: ["waba-b", "waba-c"] },
      "conflicting WhatsApp account selections",
    ],
  ])("rejects ambiguous or conflicting selected WABAs %j", (data, detail) => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data },
    });
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toEqual([
      {
        reason: "error",
        detail: expect.stringContaining(detail),
      },
    ]);
  });

  it("treats a null Coexistence waba_id as absent and waits for the fallback", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: null },
      },
    });
    expect(harness.aborted).toHaveLength(0);
    expect(harness.settledCount()).toBe(0);
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: "late-waba" },
      },
    });
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: "late-waba",
      },
    ]);
  });

  it("preserves a phone selection when Meta repeats a WABA-only completion", () => {
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleMessage(finishMessage());
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_ONLY_WABA",
        data: { waba_id: "waba-456" },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    expect(harness.completed[0]?.phoneNumberId).toBe("phone-123");
  });

  it.each([
    { waba_ids: ["waba-a", 123] },
    { waba_id: "waba-a", waba_ids: "waba-b" },
    { waba_id: 123, waba_ids: ["waba-a"] },
    { waba_ids: ["waba-a", null] },
  ])("rejects malformed explicit WABA selections %j", (data) => {
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toEqual([
      {
        reason: "error",
        detail: expect.stringContaining("invalid WhatsApp account selection"),
      },
    ]);
  });

  it("rejects conflicting WABAs across completion messages before the login code", () => {
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleMessage(finishMessage());
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: { waba_id: "another-waba" },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toHaveLength(1);
  });

  it("rejects conflicting phone selections within the same WABA before the login code", () => {
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleMessage(finishMessage());
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: { waba_id: "waba-456", phone_number_id: "another-phone" },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toEqual([
      {
        reason: "error",
        detail: expect.stringContaining(
          "more than one selected WhatsApp phone number",
        ),
      },
    ]);
  });

  it("never uses a forged Coexistence selection, even at the deadline", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage(
      finishMessage("https://www.facebook.com.example.org"),
    );
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: undefined,
      },
    ]);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: no WA_EMBEDDED_SIGNUP message received; 1 message(s) ignored from unlisted non-Meta origin; coexistence sent code after a 15s wait",
    ]);
  });

  it("reports what Meta sent and what was exchanged, never IDs, codes or raw values", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    const syntheticIds = {
      phone_number_id: "1000000000000003",
      waba_id: "1000000000000004",
      business_id: "1000000000000005",
    };
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: JSON.stringify({
        type: "WA_EMBEDDED_SIGNUP",
        event: "finish<img src=x>",
        version: "1000000000000004",
        data: syntheticIds,
      }),
    });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_1000000000000004",
        version: 1000000000000004,
        data: { waba_ids: ["1000000000000004", "1000000000000006"] },
      },
    });
    // Same payload type from origins outside the allowlist: counted, not used.
    harness.session.handleMessage({
      origin: "https://m.facebook.com",
      data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data: syntheticIds },
    });
    harness.session.handleMessage({
      origin: "https://1000000000000004.example.org",
      data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data: syntheticIds },
    });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: { type: "WA_EMBEDDED_SIGNUP", event: 1000000000000004 },
    });
    harness.session.handleLoginResponse({
      authResponse: { code: "code-1000000000000007" },
    });
    vi.advanceTimersByTime(3_000);
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: "1000000000000004" },
      },
    });

    expect(harness.completed[0]?.wabaId).toBe("1000000000000004");
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: " +
        "UNRECOGNIZED v=invalid waba_id=present waba_ids=absent phone_number_id=present (before code); " +
        "UNRECOGNIZED v=invalid waba_id=absent waba_ids=2 listed phone_number_id=absent (before code); " +
        "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=3 waba_id=present waba_ids=absent phone_number_id=absent (3s after code); " +
        "1 malformed message(s) ignored; " +
        "1 message(s) ignored from unlisted m.facebook.com; " +
        "1 message(s) ignored from unlisted non-Meta origin; " +
        "coexistence sent code+waba_id",
    ]);
    expect(harness.diagnostics[0]).not.toMatch(/10000000000|<img|code-/);
  });

  it("adds diagnostics to a refused Coexistence selection", () => {
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        version: "3",
        data: { waba_ids: ["waba-a", "waba-b"], phone_number_id: "phone-a" },
      },
    });
    expect(harness.aborted).toEqual([
      {
        reason: "error",
        detail: expect.stringContaining(
          "more than one selected WhatsApp account",
        ),
      },
    ]);
    expect(harness.diagnostics).toEqual([
      'Meta signup diagnostics: FINISH v="3" waba_id=absent waba_ids=2 listed phone_number_id=present (before code); coexistence sent nothing',
    ]);
  });

  it("lists at most six messages in diagnostics", () => {
    const harness = createHarness(50, undefined, "coexistence");
    for (let index = 0; index < 8; index += 1) {
      harness.session.handleMessage({
        origin: facebookOrigin,
        data: { type: "WA_EMBEDDED_SIGNUP", event: "FINISH", data: {} },
      });
    }
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: { type: "WA_EMBEDDED_SIGNUP", event: "CANCEL", data: {} },
    });
    expect(harness.diagnostics[0]?.split("; ")).toEqual([
      ...Array.from(
        { length: 6 },
        (_, index) =>
          `${index === 0 ? "Meta signup diagnostics: " : ""}FINISH v=none waba_id=absent waba_ids=absent phone_number_id=absent (before code)`,
      ),
      "3 more not listed",
      "coexistence sent nothing",
    ]);
  });

  it("reports what a Classic exchange sent", () => {
    const harness = createHarness();
    harness.session.handleMessage(finishMessage());
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: FINISH v=none waba_id=present waba_ids=absent phone_number_id=present (before code); classic sent code+waba_id+phone_number_id",
    ]);
  });

  it("cleans up the Coexistence deadline if the workspace changes", () => {
    vi.useFakeTimers();
    let isCurrent = true;
    const harness = createHarness(
      50,
      { isCurrent: () => isCurrent },
      "coexistence",
    );
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    isCurrent = false;
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.contextChangedCount()).toBe(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("cleans up the Coexistence deadline after cancellation", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence");
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.cancel();
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);
    harness.session.handleMessage(finishMessage());
    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.settledCount()).toBe(1);
  });

  it("ignores a Coexistence finish event in classic mode", () => {
    vi.useFakeTimers();
    const harness = createHarness();
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        version: 3,
        data: { waba_id: "coexistence-waba" },
      },
    });

    vi.advanceTimersByTime(50);
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: undefined,
        wabaId: undefined,
      },
    ]);
  });

  it.each([
    ["CANCEL", "cancelled", { current_step: "business_profile" }],
    ["ERROR", "error", { error_message: "Meta rejected the selection" }],
  ] as const)(
    "cleans up a pending fallback after a %s message",
    (event, expectedReason, data) => {
      vi.useFakeTimers();
      const harness = createHarness();
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc" },
      });

      harness.session.handleMessage({
        origin: facebookOrigin,
        data: { type: "WA_EMBEDDED_SIGNUP", event, data },
      });
      vi.advanceTimersByTime(100);

      expect(harness.completed).toHaveLength(0);
      expect(harness.aborted[0]?.reason).toBe(expectedReason);
      expect(harness.settledCount()).toBe(1);
    },
  );

  it.each(["classic", "coexistence"] as const)(
    "shows a Meta-reported %s error sent as CANCEL without its session ID",
    (mode) => {
      const harness = createHarness(50, undefined, mode);
      harness.session.handleMessage({
        origin: facebookOrigin,
        data: {
          type: "WA_EMBEDDED_SIGNUP",
          event: "CANCEL",
          data: {
            error_message: "This phone number is already registered",
            error_code: 524126,
            session_id: "synthetic-session-id",
            timestamp: "1700000000",
          },
        },
      });
      harness.session.handleLoginResponse({
        authResponse: { code: "code-abc" },
      });

      expect(harness.completed).toHaveLength(0);
      expect(harness.aborted).toEqual([
        {
          reason: "error",
          detail:
            "This phone number is already registered (Meta error code 524126)",
        },
      ]);
      expect(JSON.stringify(harness.aborted)).not.toContain(
        "synthetic-session-id",
      );
      expect(harness.settledCount()).toBe(1);
    },
  );

  it.each([
    [
      { error_message: "Meta rejected it", error_code: "524126" },
      " (Meta error code 524126)",
    ],
    [{ error_message: "Meta rejected it", error_code: "52x" }, ""],
    [{ error_message: "Meta rejected it", error_code: -1 }, ""],
    [{ error_message: "Meta rejected it" }, ""],
  ])(
    "formats Meta's error code only when it is numeric: %j",
    (data, suffix) => {
      const harness = createHarness();
      harness.session.handleMessage({
        origin: facebookOrigin,
        data: { type: "WA_EMBEDDED_SIGNUP", event: "ERROR", data },
      });
      expect(harness.aborted).toEqual([
        { reason: "error", detail: `Meta rejected it${suffix}` },
      ]);
    },
  );

  it("cleans up when the Facebook login callback returns an error", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.handleLoginResponse({
      error: { message: "Authorization was denied" },
    });
    vi.advanceTimersByTime(100);

    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toEqual([
      { reason: "error", detail: "Authorization was denied" },
    ]);
    expect(harness.settledCount()).toBe(1);
  });

  it("rejects a login callback after its pinned context changes", () => {
    let isCurrent = true;
    const harness = createHarness(50, { isCurrent: () => isCurrent });
    isCurrent = false;

    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.contextChangedCount()).toBe(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("rejects a pending result after its pinned context changes", () => {
    vi.useFakeTimers();
    let isCurrent = true;
    const harness = createHarness(50, { isCurrent: () => isCurrent });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    isCurrent = false;
    vi.advanceTimersByTime(50);

    expect(harness.completed).toHaveLength(0);
    expect(harness.contextChangedCount()).toBe(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("settles only once when Meta repeats completion signals", () => {
    const harness = createHarness();
    harness.session.handleMessage(finishMessage());
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage(finishMessage());
    harness.session.handleLoginResponse({ authResponse: { code: "code-def" } });

    expect(harness.completed).toHaveLength(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("cancels a pending session and ignores every late Meta signal", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.cancel();
    harness.session.handleLoginResponse({
      authResponse: { code: "late-code" },
    });
    harness.session.handleMessage(finishMessage());
    vi.advanceTimersByTime(100);

    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toHaveLength(0);
    expect(harness.settledCount()).toBe(1);
  });
});

describe("normalizeCoexistencePhoneNumber", () => {
  // The same rules as the server's normalizeEmbeddedSignupPhoneNumberHint.
  it.each([
    ["60123456789", "60123456789"],
    ["+60123456789", "60123456789"],
    [" +60 12-345 6789 ", "60123456789"],
    ["+1 (631) 555.0100", "16315550100"],
    ["(60) 12 345 6789", "60123456789"],
    ["+6831234", "6831234"],
    ["+123456789012345", "123456789012345"],
  ])("accepts %j as %s", (value, digits) => {
    expect(normalizeCoexistencePhoneNumber(value)).toEqual({ digits });
  });

  it.each([
    ["", "empty"],
    ["   ", "empty"],
    ["012-345 6789", "missing_country_code"],
    ["0060123456789", "missing_country_code"],
    ["+1234567890123456", "invalid"],
    ["+60 1234", "invalid"],
    ["++60123456789", "invalid"],
    ["60+123456789", "invalid"],
    ["+6O123456789", "invalid"],
    ["+60123456789 ext 2", "invalid"],
    ["+60/123456789", "invalid"],
    ["\uff16\uff10123456789", "invalid"],
    ["1 ".repeat(17), "invalid"],
    ["tel:+60123456789", "invalid"],
    ["+ - ( )", "invalid"],
  ])("refuses %j as %s", (value, problem) => {
    expect(normalizeCoexistencePhoneNumber(value)).toEqual({ problem });
  });
});

describe("Coexistence phone selection", () => {
  const wabaOnlyFinish = {
    origin: facebookOrigin,
    data: {
      type: "WA_EMBEDDED_SIGNUP",
      event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
      version: 3,
      data: { waba_id: "1000000000000004" },
    },
  };

  it("sends the operator's number when Meta names only the WABA", () => {
    const harness = createHarness(50, undefined, "coexistence", {
      phoneNumberHint: "60123456789",
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage(wabaOnlyFinish);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: "1000000000000004",
        phoneNumberHint: "60123456789",
      },
    ]);
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING v=3 waba_id=present waba_ids=absent phone_number_id=absent (0s after code); coexistence sent code+waba_id+phone_number_hint",
    ]);
    expect(harness.diagnostics[0]).not.toMatch(/6789|10000000000|code-/);
  });

  it("keeps the operator's number for a code-only fallback", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence", {
      phoneNumberHint: "60123456789",
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);

    // The server can still discover a single granted WABA and then needs the
    // number to choose among its phones.
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: undefined,
        wabaId: undefined,
        phoneNumberHint: "60123456789",
      },
    ]);
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: no WA_EMBEDDED_SIGNUP message received; coexistence sent code+phone_number_hint after a 15s wait",
    ]);
  });

  it("sends Meta's phone ID instead of the operator's number when Meta supplies one", () => {
    const harness = createHarness(50, undefined, "coexistence", {
      phoneNumberHint: "60123456789",
    });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: {
          waba_id: "1000000000000004",
          phone_number_id: "1000000000000003",
        },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: "1000000000000003",
        wabaId: "1000000000000004",
      },
    ]);
    expect(harness.completed[0]).not.toHaveProperty("phoneNumberHint");
    expect(harness.diagnostics[0]).toMatch(
      /coexistence sent code\+waba_id\+phone_number_id$/,
    );
  });

  it("never sends the operator's number or a reconnect phone in classic mode", () => {
    const harness = createHarness(50, undefined, "classic", {
      phoneNumberHint: "60123456789",
      reconnectPhoneNumberId: "1000000000000009",
    });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_ONLY_WABA",
        data: { waba_id: "1000000000000004" },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "classic",
        phoneNumberId: undefined,
        wabaId: "1000000000000004",
      },
    ]);
    expect(harness.completed[0]).not.toHaveProperty("phoneNumberHint");
    expect(harness.diagnostics[0]).toMatch(/classic sent code\+waba_id$/);
  });

  it("sends a chosen workspace account's phone ID for a reconnect", () => {
    const harness = createHarness(50, undefined, "coexistence", {
      // A reconnect makes the typed number unnecessary; it is never sent.
      phoneNumberHint: "60123456789",
      reconnectPhoneNumberId: " 1000000000000003 ",
    });
    harness.session.handleMessage(wabaOnlyFinish);
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: "1000000000000003",
        wabaId: "1000000000000004",
      },
    ]);
    expect(harness.completed[0]).not.toHaveProperty("phoneNumberHint");
    expect(harness.diagnostics[0]).toMatch(
      /coexistence sent code\+waba_id\+reconnect_account_phone_id$/,
    );
    expect(harness.diagnostics[0]).not.toMatch(/10000000000|6789/);
  });

  it("accepts Meta's phone ID when it is the reconnected account's phone", () => {
    const harness = createHarness(50, undefined, "coexistence", {
      reconnectPhoneNumberId: "1000000000000003",
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: {
          waba_id: "1000000000000004",
          phone_number_id: "1000000000000003",
        },
      },
    });
    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        mode: "coexistence",
        phoneNumberId: "1000000000000003",
        wabaId: "1000000000000004",
      },
    ]);
    expect(harness.diagnostics[0]).toMatch(
      /coexistence sent code\+waba_id\+phone_number_id$/,
    );
  });

  it("refuses a Meta phone ID that differs from the reconnected account's phone", () => {
    vi.useFakeTimers();
    const harness = createHarness(50, undefined, "coexistence", {
      reconnectPhoneNumberId: "1000000000000009",
    });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: {
          waba_id: "1000000000000004",
          phone_number_id: "1000000000000003",
        },
      },
    });
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    vi.advanceTimersByTime(META_COEXISTENCE_SELECTION_TIMEOUT_MS);

    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted).toEqual([
      {
        reason: "error",
        detail: expect.stringContaining(
          "different phone number from the account you chose to reconnect",
        ),
      },
    ]);
    expect(harness.settledCount()).toBe(1);
    expect(harness.diagnostics).toEqual([
      "Meta signup diagnostics: FINISH v=none waba_id=present waba_ids=absent phone_number_id=present (before code); coexistence sent nothing",
    ]);
  });
});
