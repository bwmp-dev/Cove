import { useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { rememberTeam } from "../lib/team";
import { keys } from "./queries";
import type { Enrollment, NodeInput, OperationAction, QueuedOperation, Route, RoutesUpdate, VerifyResult, WorkloadInput } from "./types";

// Mutations carrying credentials (passwords, setup commands, secrets) use gcTime 0 so
// their variables and results leave the in-memory mutation cache once the form unmounts.
const transient = { gcTime: 0 } as const;

export function useLogin() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    // A second_factor_required response carries a session that only proves the password; the dashboard stays closed until the challenge passes.
    mutationFn: (input: { email?: string; password: string; break_glass?: boolean }) => request<{ user: string; second_factor_required: boolean }>("POST", "/auth/login", input),
    // The session (teams, permissions) is reloaded in full rather than taken from the login response, starting from the account's default team.
    onSuccess: () => {
      rememberTeam(null);
      client.clear();
    },
  });
}

export function useLogout() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ ok: boolean }>("POST", "/auth/logout"),
    onSuccess: () => {
      rememberTeam(null);
      void client.cancelQueries();
      client.clear();
      client.setQueryData(keys.session, null);
    },
  });
}

export function useCreateNode() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (input: NodeInput) => request<Enrollment>("POST", "/api/nodes", input),
    onSettled: () => client.invalidateQueries({ queryKey: keys.nodes }),
  });
}

export function useRenewEnrollment(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: () => request<Enrollment>("POST", `/api/nodes/${encodeURIComponent(nodeId)}/enrollment`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.nodes }),
  });
}

export function useUpdateNode(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: NodeInput) => request<{ id: string }>("PUT", `/api/nodes/${encodeURIComponent(nodeId)}`, input),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.nodes }), client.invalidateQueries({ queryKey: keys.workloads })]),
  });
}

export function useEnableLocalNode() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ id: string; state: string }>("POST", "/api/nodes/local"),
    onSettled: () => client.invalidateQueries({ queryKey: keys.nodes }),
  });
}

export function useVerifyNode() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (nodeId: string) => request<VerifyResult>("POST", `/api/nodes/${encodeURIComponent(nodeId)}/verify`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.nodes }),
  });
}

export function useCreateWorkload() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (input: WorkloadInput) => request<{ id: string }>("POST", "/api/workloads", input),
    onSettled: () => client.invalidateQueries({ queryKey: keys.workloads }),
  });
}

export function useUpdateWorkload(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (input: WorkloadInput) => request<{ id: string }>("PUT", `/api/workloads/${encodeURIComponent(workloadId)}`, input),
    onSettled: () => client.invalidateQueries({ queryKey: keys.workloads }),
  });
}

export function useUpdateRoutes(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (routes: Route[]) => request<RoutesUpdate>("PUT", `/api/workloads/${encodeURIComponent(workloadId)}/routes`, { routes }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.operations }), client.invalidateQueries({ queryKey: keys.workload(workloadId) })]),
  });
}

export function useQueueOperation(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (action: Exclude<OperationAction, "routes">) =>
      request<QueuedOperation>("POST", `/api/workloads/${encodeURIComponent(workloadId)}/operations`, { action }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.operations }), client.invalidateQueries({ queryKey: keys.workload(workloadId) })]),
  });
}

export function useCancelOperation(operationId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ status: string }>("POST", `/api/operations/${encodeURIComponent(operationId)}/cancel`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.operations }),
  });
}

export function useRollback(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (release: string) => request<QueuedOperation>("POST", `/api/workloads/${encodeURIComponent(workloadId)}/operations`, { action: "rollback", release }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.operations }), client.invalidateQueries({ queryKey: keys.workload(workloadId) })]),
  });
}

export interface PortBindingRewrite {
  compose: string;
  changed: number;
  revision: string;
  saved: boolean;
}

// Previews with confirm false; saving needs confirm true and the revision the preview returned.
export function usePortBindings(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (input: { address: string; confirm: boolean; revision?: string }) => request<PortBindingRewrite>("POST", `/api/workloads/${encodeURIComponent(workloadId)}/port-bindings`, input),
    onSuccess: (result) => {
      if (result.saved) void client.invalidateQueries({ queryKey: keys.workload(workloadId) });
    },
  });
}
