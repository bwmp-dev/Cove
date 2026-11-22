import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface UpdateSettings {
  channel: "beta" | "release";
  version: string;
  revision: string;
  build_channel: string;
}

export const updatesQuery = queryOptions({
  queryKey: ["updates"],
  queryFn: ({ signal }) => request<UpdateSettings>("GET", "/api/updates", undefined, signal),
});

export function useUpdateChannel() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (channel: UpdateSettings["channel"]) => request<UpdateSettings>("PUT", "/api/updates", { channel }),
    onSuccess: (settings) => client.setQueryData(updatesQuery.queryKey, settings),
  });
}
