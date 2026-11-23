import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";
import type { MountMetric } from "./types";

export type ConnectionKind = "registries" | "storage-locations" | "notifications";
export interface Registry { server: string; username: string; password?: string }
export interface Storage { ca_certificate?: string; protocol: string; endpoint: string; path: string; username?: string; password?: string; access_key?: string; secret_key?: string; region?: string; host_key?: string; tls_mode?: string }
export interface Notification { health?: boolean; provider: string; url?: string; enabled: boolean; success: boolean; failure: boolean; events?: string[]; email?: string; owner?: boolean; signing_secret?: string; signing_secret_set?: boolean; token?: string; token_set?: boolean; target?: string; user_key?: string; user_key_set?: boolean }
// warning is set on storage locations created by the Garage add-on, which live on the same node as the data they protect.
export interface Connection { id: string; name: string; warning?: string; registry?: Registry; storage?: Storage; notification?: Notification }
export function connectionsQuery(kind: ConnectionKind) {
  return queryOptions({ queryKey: ["connections", kind], queryFn: ({ signal }) => request<Connection[]>("GET", `/api/${kind}`, undefined, signal) });
}
export const notificationEventsQuery = queryOptions({ queryKey: ["notification-events"], queryFn: ({ signal }) => request<string[]>("GET", "/api/notifications/events", undefined, signal), staleTime: Infinity });

export type NodeThresholdMetric = "cpu" | "memory" | "disk";
export type ResourceThresholdMetric = "certificate" | "uptime" | "restarts" | "oom" | "crashes";
export type ThresholdMetric = NodeThresholdMetric | ResourceThresholdMetric;
// Resource rules leave node_id blank on create; the API derives it from the resource. Node rules send empty workload_id and domain.
// mount_path applies to disk rules only: empty is the Cove data filesystem, "*" the fullest mount, an absolute path one mount.
export interface ThresholdRuleInput { name: string; node_id: string; workload_id: string; domain: string; mount_path: string; metric: ThresholdMetric; threshold: number; confirmation_seconds: number; repeat_seconds: number; channel_ids: string[] }
export interface ThresholdRule extends ThresholdRuleInput { id: string }
export interface NotificationMounts { mounts: MountMetric[]; limited: boolean; sampled_at: string }
export const notificationMountsQuery = (nodeId: string) => queryOptions({ queryKey: ["notification-mounts", nodeId], queryFn: ({ signal }) => request<NotificationMounts>("GET", `/api/notifications/mounts?node_id=${encodeURIComponent(nodeId)}`, undefined, signal), staleTime: 60_000 });
export const thresholdRulesQuery = queryOptions({ queryKey: ["threshold-rules"], queryFn: ({ signal }) => request<ThresholdRule[]>("GET", "/api/notifications/rules", undefined, signal) });
