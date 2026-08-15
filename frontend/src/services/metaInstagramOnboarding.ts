import { isAxiosError, type AxiosResponse } from "axios";

import { unwrapItemResponse } from "@/lib/api-utils";
import { api } from "@/services/api";

export interface MetaInstagramStatus {
  organization_id: string;
  configured: boolean;
  enabled: boolean;
  quarantine_only: boolean;
  app_review_status:
    | "not_configured"
    | "not_submitted"
    | "pending"
    | "approved"
    | "rejected";
  redirect_url?: string;
  deauthorize_url?: string;
  data_deletion_url?: string;
  managed_webhook_url?: string;
}

export type MetaInstagramAvailability =
  | "loading"
  | "managed"
  | "off_pilot"
  | "error";

export interface MetaInstagramAvailabilityState {
  availability: MetaInstagramAvailability;
  organizationId: string;
  status: MetaInstagramStatus | null;
}

export function metaInstagramStaticCreationAvailable(
  state: MetaInstagramAvailabilityState,
  organizationId: string | null,
) {
  const currentOrganizationId = organizationId?.trim() ?? "";
  return (
    currentOrganizationId.length > 0 &&
    state.organizationId === currentOrganizationId &&
    state.availability === "off_pilot" &&
    state.status === null
  );
}

export function metaInstagramOAuthAvailable(
  status: MetaInstagramStatus | null,
) {
  return status?.enabled === true;
}

export function metaInstagramTeardownAvailable(
  status: MetaInstagramStatus | null,
) {
  return status?.configured === true;
}

export function metaInstagramReconciliationAvailable(
  status: MetaInstagramStatus | null,
  desiredState: "subscribed" | "unsubscribed" | undefined,
) {
  if (metaInstagramOAuthAvailable(status)) return true;
  return (
    metaInstagramTeardownAvailable(status) && desiredState === "unsubscribed"
  );
}

export interface MetaInstagramStart {
  provider: "meta";
  channel: "instagram";
  mode: "instagram_login";
  authorization_url: string;
  expires_at: string;
}

export class MetaInstagramOrganizationChangedError extends Error {
  constructor() {
    super("The active workspace changed during Instagram onboarding");
    this.name = "MetaInstagramOrganizationChangedError";
  }
}

export class MetaInstagramOffPilotError extends Error {
  constructor() {
    super("Managed Instagram onboarding is not available for this workspace");
    this.name = "MetaInstagramOffPilotError";
  }
}

const offPilotStatusMessage =
  "Managed Instagram onboarding is not available for this workspace";

function isOffPilotStatusError(error: unknown) {
  if (!isAxiosError(error) || error.response?.status !== 404) return false;
  const data = error.response.data as { message?: unknown } | undefined;
  return data?.message === offPilotStatusMessage;
}

const providerRequestTimeout = 120_000;

async function unwrap<T>(request: Promise<AxiosResponse>): Promise<T> {
  return unwrapItemResponse<T>(await request);
}

function organizationHeaders(organizationId: string) {
  const value = organizationId.trim();
  if (!value) throw new MetaInstagramOrganizationChangedError();
  return { "X-Organization-ID": value };
}

function providerRequestConfig(organizationId: string) {
  return {
    headers: organizationHeaders(organizationId),
    timeout: providerRequestTimeout,
  };
}

function validateStart(start: MetaInstagramStart) {
  if (
    start.provider !== "meta" ||
    start.channel !== "instagram" ||
    start.mode !== "instagram_login"
  ) {
    throw new Error(
      "Instagram onboarding returned an invalid provider binding",
    );
  }
  const authorizationURL = new URL(start.authorization_url);
  if (
    authorizationURL.protocol !== "https:" ||
    authorizationURL.username ||
    authorizationURL.password
  ) {
    throw new Error("Instagram authorization URL is invalid");
  }
  return start;
}

export const metaInstagramOnboarding = {
  async status(organizationId: string): Promise<MetaInstagramStatus> {
    let status: MetaInstagramStatus;
    try {
      status = await unwrap<MetaInstagramStatus>(
        api.get("/integrations/meta/instagram/onboarding/status", {
          headers: organizationHeaders(organizationId),
        }),
      );
    } catch (error) {
      if (isOffPilotStatusError(error)) throw new MetaInstagramOffPilotError();
      throw error;
    }
    if (status.organization_id !== organizationId) {
      throw new MetaInstagramOrganizationChangedError();
    }
    return status;
  },

  async begin(organizationId: string): Promise<MetaInstagramStart> {
    return validateStart(
      await unwrap<MetaInstagramStart>(
        api.post(
          "/integrations/meta/instagram/onboarding/start",
          {},
          providerRequestConfig(organizationId),
        ),
      ),
    );
  },

  async reconnect(
    accountId: string,
    organizationId: string,
  ): Promise<MetaInstagramStart> {
    return validateStart(
      await unwrap<MetaInstagramStart>(
        api.post(
          `/channel-accounts/${encodeURIComponent(accountId)}/meta-instagram/reconnect`,
          {},
          providerRequestConfig(organizationId),
        ),
      ),
    );
  },

  approve(accountId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-instagram/approve`,
      { approve: true },
      providerRequestConfig(organizationId),
    );
  },

  reconcile(accountId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-instagram/reconcile`,
      {},
      providerRequestConfig(organizationId),
    );
  },

  disconnect(accountId: string, profileId: string, organizationId: string) {
    return api.post(
      `/channel-accounts/${encodeURIComponent(accountId)}/meta-instagram/disconnect`,
      { confirm_account_id: profileId },
      providerRequestConfig(organizationId),
    );
  },
};
