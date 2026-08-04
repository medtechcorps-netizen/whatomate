import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createMetaEmbeddedSignupSession,
  isAllowedMetaEmbeddedSignupOrigin,
  META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH,
  META_EMBEDDED_SIGNUP_FINISH,
  type MetaEmbeddedSignupAbortReason,
  type MetaEmbeddedSignupResult,
} from "./metaEmbeddedSignup";

const facebookOrigin = "https://www.facebook.com";

function finishMessage(data: Record<string, unknown>, event = "FINISH") {
  return {
    origin: facebookOrigin,
    data: { type: "WA_EMBEDDED_SIGNUP", event, data },
  };
}

function createHarness(
  context?: { isCurrent: () => boolean },
  expectedSignupEvent:
    | typeof META_EMBEDDED_SIGNUP_FINISH
    | typeof META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH = META_EMBEDDED_SIGNUP_FINISH,
) {
  const completed: MetaEmbeddedSignupResult[] = [];
  const aborted: Array<{
    reason: MetaEmbeddedSignupAbortReason;
    detail?: string;
  }> = [];
  let settledCount = 0;
  let contextChangedCount = 0;
  const session = createMetaEmbeddedSignupSession({
    expectedSignupEvent,
    channelWaitMs: 50,
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
  it("accepts secure facebook.com origins and rejects hostname lookalikes", () => {
    expect(isAllowedMetaEmbeddedSignupOrigin(facebookOrigin)).toBe(true);
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://signup.business.facebook.com"),
    ).toBe(true);
    expect(
      isAllowedMetaEmbeddedSignupOrigin("https://www.facebook.com.example.org"),
    ).toBe(false);
    expect(isAllowedMetaEmbeddedSignupOrigin("https://evilfacebook.com")).toBe(
      false,
    );
    expect(isAllowedMetaEmbeddedSignupOrigin("http://www.facebook.com")).toBe(
      false,
    );
  });
});

describe("createMetaEmbeddedSignupSession", () => {
  it("correlates a classic finish message that arrives before the code", () => {
    const harness = createHarness();
    harness.session.handleMessage(
      finishMessage({ phone_number_id: "phone-123", waba_id: "waba-456" }),
    );
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        phoneNumberId: "phone-123",
        wabaId: "waba-456",
        signupEvent: "FINISH",
      },
    ]);
  });

  it("correlates a JSON message that arrives after the code", () => {
    const harness = createHarness();
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleMessage({
      origin: facebookOrigin,
      data: JSON.stringify({
        type: "WA_EMBEDDED_SIGNUP",
        event: "FINISH",
        data: { phone_number_id: "phone-123", waba_id: "waba-456" },
      }),
    });

    expect(harness.completed).toHaveLength(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("accepts Meta's documented WABA-only coexistence result", () => {
    const harness = createHarness(
      undefined,
      META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH,
    );
    harness.session.handleMessage(
      finishMessage(
        { waba_id: "waba-789" },
        "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
      ),
    );
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toEqual([
      {
        code: "code-abc",
        phoneNumberId: undefined,
        wabaId: "waba-789",
        signupEvent: "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
      },
    ]);
  });

  it("rejects a completion event from a different launch mode", () => {
    const classic = createHarness();
    classic.session.handleMessage(
      finishMessage(
        { waba_id: "waba-789" },
        META_EMBEDDED_SIGNUP_COEXISTENCE_FINISH,
      ),
    );

    expect(classic.completed).toHaveLength(0);
    expect(classic.aborted[0]?.detail).toContain("different signup mode");
  });

  it("never completes from a code alone", () => {
    vi.useFakeTimers();
    const harness = createHarness();
    harness.session.handleLoginResponse({
      authResponse: {
        code: "code-abc",
        phone_number_id: "wrong-source",
        waba_id: "wrong-source",
      },
    });
    vi.advanceTimersByTime(50);

    expect(harness.completed).toHaveLength(0);
    expect(harness.aborted[0]?.reason).toBe("error");
  });

  it("rejects incomplete and unsupported success messages", () => {
    const classic = createHarness();
    classic.session.handleMessage(finishMessage({ waba_id: "waba-456" }));
    expect(classic.aborted[0]?.reason).toBe("error");

    const unsupported = createHarness();
    unsupported.session.handleMessage(
      finishMessage({ waba_id: "waba-456" }, "FINISH_ONLY_WABA"),
    );
    expect(unsupported.aborted[0]?.detail).toContain("FINISH_ONLY_WABA");
  });

  it("distinguishes ordinary cancellation from Meta-reported errors", () => {
    const cancelled = createHarness();
    cancelled.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "CANCEL",
        data: { current_step: "PHONE_NUMBER_SETUP" },
      },
    });
    expect(cancelled.aborted[0]).toMatchObject({ reason: "cancelled" });

    const failed = createHarness();
    failed.session.handleMessage({
      origin: facebookOrigin,
      data: {
        type: "WA_EMBEDDED_SIGNUP",
        event: "CANCEL",
        data: { error_code: "E1", error_message: "Meta rejected setup" },
      },
    });
    expect(failed.aborted[0]).toEqual({
      reason: "error",
      detail: "Meta rejected setup",
    });
  });

  it("ignores forged messages and settles only once", () => {
    const harness = createHarness();
    harness.session.handleMessage({
      ...finishMessage({ phone_number_id: "bad", waba_id: "bad" }),
      origin: "https://www.facebook.com.example.org",
    });
    harness.session.handleMessage(
      finishMessage({ phone_number_id: "phone-123", waba_id: "waba-456" }),
    );
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });
    harness.session.handleLoginResponse({ authResponse: { code: "code-def" } });

    expect(harness.completed).toHaveLength(1);
    expect(harness.settledCount()).toBe(1);
  });

  it("rejects a pending result after the pinned workspace changes", () => {
    let isCurrent = true;
    const harness = createHarness({ isCurrent: () => isCurrent });
    harness.session.handleMessage(
      finishMessage({ phone_number_id: "phone-123", waba_id: "waba-456" }),
    );
    isCurrent = false;
    harness.session.handleLoginResponse({ authResponse: { code: "code-abc" } });

    expect(harness.completed).toHaveLength(0);
    expect(harness.contextChangedCount()).toBe(1);
  });
});
