import { api } from "@/services/api";

interface APIEnvelope<T> {
  data: T;
}

export type SearchConsolePropertyType =
  | "domain"
  | "url_prefix"
  | "website"
  | string;

export type SearchVisibilityType =
  | "web"
  | "image"
  | "video"
  | "news"
  | "discover"
  | "googleNews";

export interface SearchConsoleProperty {
  id: string;
  site_url: string;
  display_name: string;
  property_type: SearchConsolePropertyType;
  permission_level: string;
  selected: boolean;
  last_synced_at?: string;
}

export interface SearchConsolePropertiesResponse {
  properties: SearchConsoleProperty[];
  selected_count: number;
}

export interface SearchVisibilitySetupResponse {
  status:
    | "not_configured"
    | "configured"
    | "connected"
    | "degraded"
    | "disabled"
    | string;
  message?: string;
  last_error?: string;
  properties: SearchConsoleProperty[];
}

export interface SearchVisibilityProperty {
  id: string;
  site_url: string;
  display_name: string;
  property_type: SearchConsolePropertyType;
}

export interface SearchVisibilitySummary {
  clicks: number;
  impressions: number;
  ctr: number;
  position: number;
}

export interface SearchVisibilityTrendPoint extends SearchVisibilitySummary {
  date: string;
}

export interface SearchVisibilityQueryRow extends SearchVisibilitySummary {
  query: string;
}

export interface SearchVisibilityPageRow extends SearchVisibilitySummary {
  page: string;
}

export interface SearchVisibilityResponse {
  property: SearchVisibilityProperty;
  start_date: string;
  end_date: string;
  search_type: SearchVisibilityType;
  data_state: "final" | "all" | string;
  page?: string;
  summary: SearchVisibilitySummary;
  trend: SearchVisibilityTrendPoint[];
  top_queries: SearchVisibilityQueryRow[];
  top_pages: SearchVisibilityPageRow[];
}

export interface SearchVisibilityParams {
  property_id?: string;
  start_date: string;
  end_date: string;
  search_type: SearchVisibilityType;
  page?: string;
  limit?: number;
}

export const searchConsoleService = {
  listProperties: () =>
    api.get<APIEnvelope<SearchConsolePropertiesResponse>>(
      "/integrations/google_search_console/properties",
    ),
  updateProperties: (propertyIds: string[]) =>
    api.put<APIEnvelope<SearchConsolePropertiesResponse>>(
      "/integrations/google_search_console/properties",
      { property_ids: propertyIds },
    ),
  refreshProperties: () =>
    api.post<APIEnvelope<SearchConsolePropertiesResponse>>(
      "/integrations/google_search_console/properties/refresh",
    ),
  disconnect: () =>
    api.delete("/integrations/google_search_console/connection"),
  getVisibilitySetup: () =>
    api.get<APIEnvelope<SearchVisibilitySetupResponse>>(
      "/analytics/search-visibility/setup",
    ),
  getVisibility: (params: SearchVisibilityParams) =>
    api.get<APIEnvelope<SearchVisibilityResponse>>(
      "/analytics/search-visibility",
      { params },
    ),
};
