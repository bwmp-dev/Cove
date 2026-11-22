import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export type ProxyFileStatus = "managed" | "unmanaged" | "protected" | "unsupported" | "missing";
export type ProxyObjectStatus = "enabled" | "disabled" | "warning" | "not_loaded" | "not_reported";
export type ProxyChangeAction = "put" | "delete" | "import" | "validate";
export type ProxyChangeState = "applying" | "restoring" | "succeeded" | "failed" | "rejected" | "interrupted" | "valid" | "invalid";

export interface ProxyObject {
  kind: string;
  name: string;
  status: ProxyObjectStatus;
  errors?: string[];
}

export interface ProxyFile {
  name: string;
  revision?: string;
  size: number;
  status: ProxyFileStatus;
  modified?: boolean;
  problem?: string;
  content?: string;
  objects?: ProxyObject[];
  warnings?: string[];
}

export interface ProxyChange {
  id?: string;
  action: ProxyChangeAction;
  name: string;
  state: ProxyChangeState;
  message?: string;
  problems?: string[];
  warnings?: string[];
  revision?: string;
  started_at: string;
  finished_at?: string;
}

export interface ProxyConfig {
  available: boolean;
  unavailable?: string;
  running: boolean;
  image?: string;
  ports: string[];
  entrypoints: { name: string; address: string }[];
  resolvers: string[];
  notice?: string;
  blocked?: string;
  files: ProxyFile[];
  change?: ProxyChange;
}

const base = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/ingress-config`;

export const proxyKeys = {
  config: (nodeId: string) => ["proxy-config", nodeId] as const,
  file: (nodeId: string, name: string) => ["proxy-config", nodeId, "file", name] as const,
};

export function unsettled(change: ProxyChange | undefined) {
  return change?.state === "applying" || change?.state === "restoring";
}

export const proxyConfigQuery = (nodeId: string) =>
  queryOptions({
    queryKey: proxyKeys.config(nodeId),
    queryFn: ({ signal }) => request<ProxyConfig>("GET", base(nodeId), undefined, signal),
    // The worker restores an unsettled change on its own; follow it until it settles.
    refetchInterval: (query) => (unsettled(query.state.data?.change) ? 3_000 : false),
    retry: false,
  });

// File contents may hold credentials such as basic authentication hashes, so they leave the
// cache as soon as the editor unmounts and are never refetched underneath an open editor.
export const proxyFileQuery = (nodeId: string, name: string) =>
  queryOptions({
    queryKey: proxyKeys.file(nodeId, name),
    queryFn: ({ signal }) => request<ProxyFile>("GET", `${base(nodeId)}/files/${encodeURIComponent(name)}`, undefined, signal),
    gcTime: 0,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    retry: false,
  });

export type ProxyChangeInput =
  | { action: "put"; name: string; content: string; revision: string }
  | { action: "validate"; name: string; content: string }
  | { action: "import" | "delete"; name: string; revision: string };

function send(nodeId: string, input: ProxyChangeInput) {
  const file = `${base(nodeId)}/files/${encodeURIComponent(input.name)}`;
  const change_id = crypto.randomUUID();
  switch (input.action) {
    case "validate":
      return request<ProxyChange>("POST", `${base(nodeId)}/validate`, { name: input.name, content: input.content });
    case "put":
      return request<ProxyChange>("PUT", file, { content: input.content, revision: input.revision, change_id });
    case "import":
      return request<ProxyChange>("POST", `${file}/import`, { revision: input.revision, change_id });
    case "delete":
      return request<ProxyChange>("DELETE", file, { revision: input.revision, change_id });
  }
}

export function useProxyChange(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: ProxyChangeInput) => send(nodeId, input),
    // A lost reply can hide a change the worker still completes, so status is reloaded either way.
    onSettled: async (_data, _error, input) => {
      if (input.action === "validate") return;
      await client.invalidateQueries({ queryKey: proxyKeys.config(nodeId), exact: true });
    },
  });
}
