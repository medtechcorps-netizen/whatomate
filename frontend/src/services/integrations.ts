import { api } from "@/services/api";

export type IntegrationProvider =
  | "meta"
  | "threads"
  | "tiktok"
  | "qwen"
  | "google_search_console"
  | "email"
  | "webchat";

export type IntegrationStatus =
  | "not_configured"
  | "configured"
  | "pending"
  | "connected"
  | "degraded"
  | "disabled"
  | "approval_required"
  | "adapter_unavailable";

export interface IntegrationCredentialState {
  configured: boolean;
  updated_at?: string;
  source?:
    | "platform"
    | "workspace"
    | "copilot"
    | "legacy_chatbot"
    | "legacy_account";
}

export interface IntegrationConnectionState {
  account_count: number;
  active_count: number;
  pending_count?: number;
  last_health_check_at?: string;
  last_inbound_at?: string;
  last_outbound_at?: string;
  last_error?: string;
}

export type IntegrationChannel =
  | "whatsapp"
  | "instagram"
  | "messenger"
  | "threads"
  | "email"
  | "webchat";

export interface IntegrationOAuthState {
  supported: boolean;
  available: boolean;
  mode?: string;
  authorization_url?: string;
}

export interface IntegrationState {
  provider: IntegrationProvider;
  display_name: string;
  status: IntegrationStatus;
  enabled: boolean;
  configured: boolean;
  read_only?: boolean;
  config: Record<string, unknown>;
  credentials: Record<string, IntegrationCredentialState>;
  connection: IntegrationConnectionState;
  channel_connections?: Partial<
    Record<IntegrationChannel, IntegrationConnectionState>
  >;
  intended_channels?: IntegrationChannel[];
  oauth: IntegrationOAuthState;
  test_supported: boolean;
  message?: string;
  required_scopes?: string[];
  last_tested_at?: string;
}

export interface IntegrationUpdateInput {
  enabled?: boolean;
  config?: Record<string, unknown>;
  credentials?: Record<string, string>;
  clear_credentials?: string[];
}

export interface IntegrationConnectResult {
  provider: IntegrationProvider;
  ready: boolean;
  mode: string;
  authorization_url?: string;
  reconnect?: boolean;
  expires_at?: string;
  message?: string;
  public_config?: Record<string, unknown>;
}

export interface IntegrationTestResult {
  provider: IntegrationProvider;
  success: boolean;
  status: IntegrationStatus;
  message?: string;
  tested_at?: string;
}

interface APIEnvelope<T> {
  data: T;
}

export const integrationsService = {
  list: () =>
    api.get<APIEnvelope<{ integrations: IntegrationState[] }>>("/integrations"),
  update: (provider: IntegrationProvider, data: IntegrationUpdateInput) =>
    api.put<APIEnvelope<IntegrationState>>(
      `/integrations/${encodeURIComponent(provider)}`,
      data,
    ),
  clearCredentials: (provider: IntegrationProvider) =>
    api.delete<APIEnvelope<IntegrationState>>(
      `/integrations/${encodeURIComponent(provider)}/credentials`,
    ),
  test: (provider: IntegrationProvider) =>
    api.post<APIEnvelope<IntegrationTestResult>>(
      `/integrations/${encodeURIComponent(provider)}/test`,
    ),
  connect: (provider: IntegrationProvider) =>
    api.post<APIEnvelope<IntegrationConnectResult>>(
      `/integrations/${encodeURIComponent(provider)}/connect`,
    ),
  connectManagedThreads: () =>
    api.post<APIEnvelope<IntegrationConnectResult>>(
      "/integrations/threads/managed/onboarding/start",
    ),
};
