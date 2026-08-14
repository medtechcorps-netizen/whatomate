import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createMetaEmbeddedSignupSession,
  isAllowedMetaEmbeddedSignupOrigin,
  type MetaEmbeddedSignupAbortReason,
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
) {
  const completed: MetaEmbeddedSignupResult[] = [];
  const aborted: Array<{
    reason: MetaEmbeddedSignupAbortReason;
    detail?: string;
  }> = [];
  let settledCount = 0;
  let contextChangedCount = 0;
  const session = createMetaEmbeddedSignupSession({
    codeFallbackMs,
    onComplete: (result) => completed.push(result),
    onAbort: (reason, detail) => aborted.push({ reason, detail }),
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
        phoneNumberId: undefined,
        wabaId: "waba-456",
      },
    ]);
  });

  it("completes WhatsApp Business App onboarding with a WABA-only message", () => {
    vi.useFakeTimers();
    const harness = createHarness();

    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
        data: { waba_id: "waba-789" },
      },
    });
    vi.advanceTimersByTime(100);

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
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
      phoneNumberId: undefined,
      wabaId: undefined,
    });
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
