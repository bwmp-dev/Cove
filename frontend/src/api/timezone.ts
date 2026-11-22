import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export interface TimezonePreference {
  team_timezone: string;
  user_timezone: string;
  timezone: string;
  schedule_timezone: string;
}

// Keyed by team so a preference resolved for one team never shows while another team is selected.
export const timezonePreferenceQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["preferences", "timezone", teamId] as const,
    queryFn: ({ signal }) => request<TimezonePreference>("GET", "/api/preferences/timezone", undefined, signal),
    staleTime: 5 * 60_000,
  });

export const teamTimezoneQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["teams", teamId, "timezone"] as const,
    queryFn: ({ signal }) => request<TimezonePreference>("GET", `/api/teams/${encodeURIComponent(teamId)}/timezone`, undefined, signal),
  });

export function saveUserTimezone(timezone: string) {
  return request<TimezonePreference>("PUT", "/api/preferences/timezone", { timezone });
}

export function saveTeamTimezone(teamId: string, timezone: string) {
  return request<TimezonePreference>("PUT", `/api/teams/${encodeURIComponent(teamId)}/timezone`, { timezone });
}
