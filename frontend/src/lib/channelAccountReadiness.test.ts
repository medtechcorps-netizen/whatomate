import { describe, expect, it, vi } from "vitest";

import type { ChannelAccount } from "@/services/productSuite";
import {
  confirmMetaAccountOutboundApproval,
  confirmMetaAccountTest,
  isManagedMessengerAccount,
  isMetaRelayAccount,
  META_RECERTIFICATION_SEQUENCE,
  messengerAwaitingRelayRegistry,
  messengerReviewRelayReady,
  messengerRelayRegistryRecognized,
  metaAccountSettingsRequireRetest,
  metaAccountTestActionLabel,
  metaAccountTestStopsDelivery,
  metaAccountReadiness,
} from "./channelAccountReadiness";

function account(overrides: Partial<ChannelAccount> = {}): ChannelAccount {
  return {
    id: "account-1",
    channel: "messenger",
    provider: "relay",
    name: "Klinik INSAN Avenue Ampang",
    external_account_id: "700000000000001",
    status: "pending",
    capabilities: {},
    config: {},
    has_credentials: true,
    meta_provider_proof_version: "v1",
    outbox_pending: 0,
    outbox_failed: 0,
    ...overrides,
  };
}

describe("metaAccountReadiness", () => {
  it("fails closed when per-organization proof is absent", () => {
    const readiness = metaAccountReadiness(account());

    expect(readiness.preflightReady).toBe(false);
    expect(readiness.completedCount).toBe(0);
    expect(readiness.items.find((item) => item.key === "identity")?.state).toBe(
      "required",
    );
    expect(
      readiness.items.find((item) => item.key === "relay_mapping")?.state,
    ).toBe("blocked");
    expect(
      readiness.items.find((item) => item.key === "outbound_approval")?.state,
    ).toBe("blocked");
  });

  it("does not accept an identity confirmation copied from another asset", () => {
    const readiness = metaAccountReadiness(
      account({
        config: { identity_confirmed_id: "700000000000099" },
      }),
    );

    expect(readiness.preflightReady).toBe(false);
    expect(readiness.items.find((item) => item.key === "identity")?.state).toBe(
      "required",
    );
  });

  it("requires both a provider-verified subscription and observed inbound delivery", () => {
    const readiness = metaAccountReadiness(
      account({
        status: "active",
        last_health_check_at: "2026-08-06T02:00:00Z",
      }),
    );

    expect(
      readiness.items.find((item) => item.key === "webhook_subscription")
        ?.state,
    ).toBe("blocked");
  });

  it("rejects timestamps created before provider-proof v1 was verified", () => {
    const readiness = metaAccountReadiness(
      account({
        status: "active",
        last_health_check_at: "2026-08-06T02:00:00Z",
        last_inbound_at: "2026-08-06T02:05:00Z",
        meta_provider_proof_version: undefined,
        config: { identity_confirmed_id: "700000000000001" },
      }),
    );
    const relay = readiness.items.find((item) => item.key === "relay_mapping");

    expect(readiness.preflightReady).toBe(false);
    expect(relay?.state).toBe("blocked");
    expect(relay?.detail).toContain("provider-proof v1");
  });

  it.each([
    ["predates", "2026-08-06T02:05:00Z"],
    ["equals", "2026-08-06T02:10:00Z"],
  ])(
    "rejects inbound delivery that %s the latest hardened Test",
    (_relationship, lastInboundAt) => {
      const readiness = metaAccountReadiness(
        account({
          status: "active",
          last_health_check_at: "2026-08-06T02:10:00Z",
          last_inbound_at: lastInboundAt,
          config: { identity_confirmed_id: "700000000000001" },
        }),
      );
      const webhook = readiness.items.find(
        (item) => item.key === "webhook_subscription",
      );

      expect(readiness.preflightReady).toBe(false);
      expect(webhook?.state).toBe("blocked");
      expect(webhook?.detail).toContain(
        "does not postdate the latest relay Test",
      );
      expect(webhook?.detail).toContain("new incoming text DM");
      expect(webhook?.detail).toContain("genuine external customer profile");
      expect(webhook?.detail).toContain(
        "not an app admin, developer or tester",
      );
    },
  );

  it("fails closed on malformed provider timestamps", () => {
    const invalidHealth = metaAccountReadiness(
      account({
        status: "active",
        last_health_check_at: "not-a-date",
        last_inbound_at: "2026-08-06T02:05:00Z",
        config: { identity_confirmed_id: "700000000000001" },
      }),
    );
    const invalidInbound = metaAccountReadiness(
      account({
        status: "active",
        last_health_check_at: "2026-08-06T02:00:00Z",
        last_inbound_at: "not-a-date",
        config: { identity_confirmed_id: "700000000000001" },
      }),
    );

    expect(invalidHealth.preflightReady).toBe(false);
    expect(
      invalidHealth.items.find((item) => item.key === "relay_mapping")?.state,
    ).toBe("blocked");
    expect(invalidInbound.preflightReady).toBe(false);
    expect(
      invalidInbound.items.find((item) => item.key === "webhook_subscription")
        ?.detail,
    ).toContain("timestamp is invalid");
  });

  it("opens outbound approval only after every prerequisite passes", () => {
    const readyAccount = account({
      status: "active",
      last_health_check_at: "2026-08-06T02:00:00Z",
      last_inbound_at: "2026-08-06T02:05:00Z",
      config: {
        identity_confirmed_id: "700000000000001",
        outbound_enabled: false,
      },
    });
    const readiness = metaAccountReadiness(readyAccount);

    expect(readiness.preflightReady).toBe(true);
    expect(readiness.completedCount).toBe(4);
    expect(
      readiness.items.find((item) => item.key === "permissions")?.detail,
    ).toContain("future-organization Advanced Access profile");
    expect(
      readiness.items.find((item) => item.key === "outbound_approval")?.state,
    ).toBe("required");
  });

  it("flags an older outbound approval that lacks the new proof", () => {
    const readiness = metaAccountReadiness(
      account({ config: { outbound_enabled: true } }),
    );

    expect(readiness.hasLegacyOutboundApproval).toBe(true);
    expect(
      readiness.items.find((item) => item.key === "outbound_approval")?.state,
    ).toBe("blocked");
  });

  it("identifies only Instagram and Messenger signed relays", () => {
    expect(isMetaRelayAccount(account())).toBe(true);
    expect(isMetaRelayAccount(account({ channel: "instagram" }))).toBe(true);
    expect(isMetaRelayAccount(account({ channel: "email" }))).toBe(false);
    expect(isMetaRelayAccount(account({ provider: "meta_legacy" }))).toBe(
      false,
    );
  });

  it("locks a managed Messenger account until the runtime registry recognizes it", () => {
    const awaitingRegistry = account({
      status: "active",
      last_health_check_at: "2026-08-06T02:00:00Z",
      last_inbound_at: "2026-08-06T02:05:00Z",
      config: {
        onboarding_state: "awaiting_relay_registry",
        registry_recognized: false,
        identity_confirmed_id: "700000000000001",
      },
    });

    expect(isManagedMessengerAccount(awaitingRegistry)).toBe(true);
    expect(messengerAwaitingRelayRegistry(awaitingRegistry)).toBe(true);
    expect(messengerRelayRegistryRecognized(awaitingRegistry)).toBe(false);

    const readiness = metaAccountReadiness(awaitingRegistry);
    expect(readiness.preflightReady).toBe(false);
    expect(
      readiness.items.find((item) => item.key === "permissions")?.detail,
    ).toContain("runtime relay registry recognizes");
    expect(
      readiness.items.find((item) => item.key === "relay_mapping")?.detail,
    ).toContain("Test and outbound approval remain unavailable");
  });

  it("recognizes review readiness without granting production registry or outbound readiness", () => {
    const reviewReady = account({
      config: {
        onboarding_state: "review_relay_ready",
        review_only: true,
        registry_recognized: false,
        outbound_enabled: false,
        ai_reply_enabled: false,
        identity_confirmed_id: "700000000000001",
      },
    });

    expect(messengerReviewRelayReady(reviewReady)).toBe(true);
    expect(messengerRelayRegistryRecognized(reviewReady)).toBe(false);
    expect(metaAccountReadiness(reviewReady).preflightReady).toBe(false);
    expect(
      messengerReviewRelayReady(
        account({
          config: {
            ...reviewReady.config,
            outbound_enabled: true,
          },
        }),
      ),
    ).toBe(false);
  });

  it("keeps legacy Messenger and Instagram readiness unchanged", () => {
    const legacyMessenger = account();
    const instagram = account({ channel: "instagram" });

    expect(isManagedMessengerAccount(legacyMessenger)).toBe(false);
    expect(messengerRelayRegistryRecognized(legacyMessenger)).toBe(true);
    expect(isManagedMessengerAccount(instagram)).toBe(false);
    expect(messengerRelayRegistryRecognized(instagram)).toBe(true);
  });

  it("allows managed Messenger readiness after registry recognition", () => {
    const recognized = account({
      status: "active",
      last_health_check_at: "2026-08-06T02:00:00Z",
      last_inbound_at: "2026-08-06T02:05:00Z",
      config: {
        onboarding_state: "relay_registry_recognized",
        registry_recognized: true,
        identity_confirmed_id: "700000000000001",
      },
    });

    expect(messengerAwaitingRelayRegistry(recognized)).toBe(false);
    expect(messengerRelayRegistryRecognized(recognized)).toBe(true);
    expect(metaAccountReadiness(recognized).preflightReady).toBe(true);
  });

  it("fails closed if the registry flag changes before onboarding advances", () => {
    const inconsistent = account({
      config: {
        onboarding_state: "awaiting_relay_registry",
        registry_recognized: true,
      },
    });

    expect(messengerAwaitingRelayRegistry(inconsistent)).toBe(true);
    expect(messengerRelayRegistryRecognized(inconsistent)).toBe(false);
  });
});

describe("Meta account Test guard", () => {
  it("keeps the destructive recovery sequence explicit and stable", () => {
    expect(META_RECERTIFICATION_SEQUENCE).toBe(
      "Test pass -> provider-proven new incoming text DM from a genuine external customer profile not listed as a Meta app admin/developer/tester, strictly after Test -> refresh -> admin approval.",
    );
  });

  it.each([
    ["outbound", { outbound_enabled: true }],
    ["AI", { ai_reply_enabled: true }],
  ])("requires confirmation before %s re-certification", (_mode, config) => {
    const liveAccount = account({ config });
    const confirm = vi.fn((_message: string) => false);

    expect(metaAccountTestStopsDelivery(liveAccount)).toBe(true);
    expect(metaAccountTestActionLabel(liveAccount)).toBe("Re-certify");
    expect(confirmMetaAccountTest(liveAccount, confirm)).toBe(false);
    expect(confirm).toHaveBeenCalledOnce();
    const warning = confirm.mock.calls[0]?.[0] ?? "";
    expect(warning).toContain(
      "stops new outbound delivery and automatic AI replies",
    );
    expect(warning).toContain(
      "A provider request already dispatching across the network may still complete",
    );
    expect(warning).toContain(META_RECERTIFICATION_SEQUENCE);
  });

  it("does not prompt when Test cannot stop active Meta delivery", () => {
    const inactiveAccount = account();
    const confirm = vi.fn((_message: string) => false);

    expect(metaAccountTestActionLabel(inactiveAccount)).toBe("Test");
    expect(confirmMetaAccountTest(inactiveAccount, confirm)).toBe(true);
    expect(confirm).not.toHaveBeenCalled();
  });
});

describe("Meta outbound human attestation", () => {
  it("requires an explicit external-customer text-DM confirmation", () => {
    const confirm = vi.fn((_message: string) => true);

    expect(confirmMetaAccountOutboundApproval(account(), confirm)).toBe(true);
    const warning = confirm.mock.calls[0]?.[0] ?? "";
    expect(warning).toContain("new incoming text DM strictly after");
    expect(warning).toContain("genuine external customer profile");
    expect(warning).toContain("admin, developer or tester");
    expect(warning).toContain("photo, sticker or reaction alone");
  });

  it("does not add a Meta attestation prompt to another relay channel", () => {
    const confirm = vi.fn((_message: string) => true);

    expect(
      confirmMetaAccountOutboundApproval(
        account({ channel: "email" }),
        confirm,
      ),
    ).toBe(true);
    expect(confirm).not.toHaveBeenCalled();
  });
});

describe("Meta settings retest guard", () => {
  const configured = account({
    config: {
      relay_url:
        "https://app.rereply.app/meta-relay/v1/accounts/messenger/700000000000001",
      identity_confirmed_id: "700000000000001",
    },
  });

  it("keeps unchanged settings eligible without a retest", () => {
    expect(
      metaAccountSettingsRequireRetest(
        configured,
        "https://app.rereply.app/meta-relay/v1/accounts/messenger/700000000000001",
        "700000000000001",
        false,
      ),
    ).toBe(false);
  });

  it.each([
    ["relay URL", "https://relay.example.com/meta", "700000000000001", false],
    [
      "identity",
      "https://app.rereply.app/meta-relay/v1/accounts/messenger/700000000000001",
      "700000000000002",
      false,
    ],
    [
      "credential",
      "https://app.rereply.app/meta-relay/v1/accounts/messenger/700000000000001",
      "700000000000001",
      true,
    ],
  ])(
    "requires a retest after changing the %s",
    (_change, relayURL, identity, rotates) => {
      expect(
        metaAccountSettingsRequireRetest(
          configured,
          relayURL,
          identity,
          rotates,
        ),
      ).toBe(true);
    },
  );
});
