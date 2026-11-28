import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export type HostPackageManager = "apt" | "dnf" | "unsupported";

// Null and empty values are unknown; they never mean zero pending updates or a healthy host.
export interface HostPatchStatus {
  checked_at: string;
  os: string;
  version: string;
  manager: HostPackageManager;
  security_updates: number | null;
  packages: string[];
  limited: boolean;
  metadata_at: string | null;
  reboot_required: boolean | null;
  automatic_reboot: boolean | null;
  policy_supported: boolean;
  policy_enabled: boolean;
  last_result: string;
  notices: string[];
}

const hostMaintenanceKey = (nodeId: string) => ["nodes", nodeId, "host-maintenance"] as const;
const hostMaintenancePath = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/host-maintenance`;

// Inspecting runs commands on the host, so it is only fetched when someone asks for it.
export const hostMaintenanceQuery = (nodeId: string) =>
  queryOptions({
    queryKey: hostMaintenanceKey(nodeId),
    queryFn: ({ signal }) => request<HostPatchStatus>("GET", hostMaintenancePath(nodeId), undefined, signal),
    enabled: false,
    retry: false,
  });

export function useSetHostSecuritySchedule(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (enabled: boolean) => request<HostPatchStatus>("PUT", hostMaintenancePath(nodeId), { enabled, confirm: true }),
    onSuccess: (status) => client.setQueryData(hostMaintenanceKey(nodeId), status),
  });
}
