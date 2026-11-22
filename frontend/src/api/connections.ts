import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export type ConnectionKind = "registries" | "storage-locations" | "notifications";
export interface Registry { server: string; username: string; password?: string }
export interface Storage { ca_certificate?: string; protocol: string; endpoint: string; path: string; username?: string; password?: string; access_key?: string; secret_key?: string; region?: string; host_key?: string; tls_mode?: string }
export interface Notification { health?: boolean; provider: string; url?: string; enabled: boolean; success: boolean; failure: boolean }
export interface Connection { id: string; name: string; registry?: Registry; storage?: Storage; notification?: Notification }
export function connectionsQuery(kind: ConnectionKind) {
  return queryOptions({ queryKey: ["connections", kind], queryFn: ({ signal }) => request<Connection[]>("GET", `/api/${kind}`, undefined, signal) });
}
