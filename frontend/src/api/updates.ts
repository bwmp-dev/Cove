import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export type UpdateChannelName = "beta" | "release";

// The host updater's latest observation of a channel, assessed by the control plane. A target that is missing,
// stale or not comparable with the installed build is reported with available or allowed false and a message.
export interface UpdateTarget {
  version: string;
  revision: string;
  schema_hash: string;
  revision_time: string;
  checked_at: string;
  source_root: string;
  source_generation: number;
  available: boolean;
  downgrade: boolean;
  allowed: boolean;
  message: string;
}

export interface UpdateSettings {
  channel: UpdateChannelName;
  version: string;
  revision: string;
  revision_time?: string;
  build_channel: string;
  schema_hash?: string;
  targets?: Partial<Record<UpdateChannelName, UpdateTarget>>;
  confirmed_downgrade_version?: string;
  confirmed_downgrade_revision?: string;
}

export interface UpdateChannelInput {
  channel: UpdateChannelName;
  // Bound to the target revision the administrator reviewed, so a newer observation needs a fresh review.
  target_revision?: string;
  confirm_downgrade?: boolean;
}

export const updatesQuery = queryOptions({
  queryKey: ["updates"],
  queryFn: ({ signal }) => request<UpdateSettings>("GET", "/api/updates", undefined, signal),
});

export function useUpdateChannel() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: UpdateChannelInput) => request<UpdateSettings>("PUT", "/api/updates", input),
    onSuccess: (settings) => client.setQueryData(updatesQuery.queryKey, settings),
    // A 409 means the observation changed or was blocked; reload it so the choices reflect the current state.
    onError: () => client.invalidateQueries({ queryKey: updatesQuery.queryKey }),
  });
}
