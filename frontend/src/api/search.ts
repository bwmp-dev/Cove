import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export const searchKinds = ["workload", "project", "environment", "node", "operation", "page"] as const;
export type SearchKind = (typeof searchKinds)[number];

export interface SearchItem {
  kind: SearchKind;
  name: string;
  detail: string;
  path: string;
}

interface SearchResponse {
  items: unknown[];
  limit: number;
}

// Results are scoped to the selected team, so the team is part of the key; gcTime 0 keeps nothing once the palette closes.
export function searchQuery(team: string, text: string) {
  return queryOptions({
    queryKey: ["search", team, text] as const,
    queryFn: async ({ signal }) => {
      const response = await request<SearchResponse>("GET", `/api/search?q=${encodeURIComponent(text)}`, undefined, signal);
      return Array.isArray(response.items) ? response.items.filter(isSearchItem) : [];
    },
    gcTime: 0,
    staleTime: 0,
    retry: false,
  });
}

function isSearchItem(value: unknown): value is SearchItem {
  if (typeof value !== "object" || value === null) return false;
  const item = value as Record<string, unknown>;
  return (
    typeof item.kind === "string" &&
    (searchKinds as readonly string[]).includes(item.kind) &&
    typeof item.name === "string" &&
    typeof item.detail === "string" &&
    typeof item.path === "string"
  );
}
