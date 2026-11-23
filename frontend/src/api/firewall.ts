import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export type FirewallProtocol = "tcp" | "udp";

export interface FirewallPort {
  protocol: FirewallProtocol;
  port: number;
}

// An empty source list denies new connections to the port from everywhere.
export interface FirewallRule extends FirewallPort {
  sources: string[] | null;
}

export interface FirewallResult {
  supported: boolean;
  // Cove's own table exists on the host; it says nothing about the health of the rest of the firewall.
  active: boolean;
  rules: FirewallRule[] | null;
  available: FirewallPort[] | null;
  reserved: number[] | null;
  // Set only by preview: binds the exact current table, ports and desired rules.
  revision: string;
  // An applied change that the panel has not confirmed yet; the host reverts it independently.
  pending?: { id: string; deadline: string };
  ufw: string;
  nftables: string;
  limited: boolean;
  notice: string;
  last_error: string;
}

export interface FirewallRuleInput {
  protocol: FirewallProtocol;
  port: number;
  sources: string[];
}

const firewallKey = (nodeId: string) => ["nodes", nodeId, "firewall"] as const;
export const firewallMutationKey = (nodeId: string) => ["nodes", nodeId, "firewall", "change"] as const;
const firewallPath = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/firewall`;

// Inspecting runs commands on the host, so it is only fetched when someone asks for it.
export const firewallQuery = (nodeId: string) =>
  queryOptions({
    queryKey: firewallKey(nodeId),
    queryFn: ({ signal }) => request<FirewallResult>("GET", firewallPath(nodeId), undefined, signal),
    enabled: false,
    retry: false,
  });

export function usePreviewFirewall(nodeId: string) {
  return useMutation({
    mutationKey: firewallMutationKey(nodeId),
    mutationFn: (rules: FirewallRuleInput[]) =>
      request<FirewallResult>("POST", `${firewallPath(nodeId)}/preview`, {
        rules,
      }),
    gcTime: 0,
  });
}

// A successful response is the host's read-back state. The caller still re-inspects after every outcome, because an error can follow an applied change.
export function useApplyFirewall(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationKey: firewallMutationKey(nodeId),
    mutationFn: ({ rules, revision }: { rules: FirewallRuleInput[]; revision: string }) => request<FirewallResult>("POST", firewallPath(nodeId), { rules, revision, confirm: true }),
    onSuccess: (result) => client.setQueryData(firewallKey(nodeId), result),
    gcTime: 0,
  });
}

export function useRevertFirewall(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationKey: firewallMutationKey(nodeId),
    mutationFn: () => request<FirewallResult>("POST", firewallPath(nodeId), { revert: true, confirm: true }),
    onSuccess: (result) => client.setQueryData(firewallKey(nodeId), result),
    gcTime: 0,
  });
}
