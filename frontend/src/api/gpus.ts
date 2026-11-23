import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export interface GPUDevice {
  id: string;
  name: string;
  driver: "nvidia" | "cdi";
  // Total memory in MiB for native NVIDIA devices; CDI specs do not report it.
  memory_mb: number | null;
  assignable: boolean;
  notice: string;
}

// Only native NVIDIA devices and root-owned CDI GPU specs are reported, so an empty list never means the host has no GPU.
export interface GPUInventory {
  checked_at: string;
  devices: GPUDevice[];
  limited: boolean;
  notices: string[];
}

// Inspection runs native tools on the worker, so it is only fetched when someone asks. The keys sit outside
// the node and workload prefixes so invalidating those never triggers an inspection.
export const nodeGPUsQuery = (nodeId: string) =>
  queryOptions({
    queryKey: ["gpu-inventory", "node", nodeId] as const,
    queryFn: ({ signal }) => request<GPUInventory>("GET", `/api/nodes/${encodeURIComponent(nodeId)}/gpus`, undefined, signal),
    enabled: false,
    retry: false,
  });

export const workloadGPUsQuery = (workloadId: string) =>
  queryOptions({
    queryKey: ["gpu-inventory", "workload", workloadId] as const,
    queryFn: ({ signal }) => request<GPUInventory>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/gpus`, undefined, signal),
    enabled: false,
    retry: false,
  });
