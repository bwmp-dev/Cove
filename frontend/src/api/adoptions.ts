import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";
import type { ConfigurationChange } from "./project-configuration";

// Metadata only: the scan never returns environment values, commands, mount paths or label values.
export interface DiscoveredProject {
  project: string;
  services: string[] | null;
  containers: number;
  blockers: string[] | null;
}

export interface AdoptionDiscovery {
  projects: DiscoveredProject[] | null;
  warnings: string[] | null;
}

export type AdoptionStatus = "planned" | "applying" | "complete";

export interface AdoptionSummary {
  id: string;
  project_id: string;
  environment_id: string;
  status: AdoptionStatus;
  created_at: string;
}

export interface AdoptionResource {
  project: string;
  key: string;
  name: string;
  resource_id: string;
  complete: boolean;
}

export interface Adoption {
  id: string;
  status: AdoptionStatus;
  project_id: string;
  environment_id: string;
  resources: AdoptionResource[] | null;
  warnings: string[] | null;
  changes: ConfigurationChange[] | null;
}

export interface AdoptionSelection {
  project: string;
  key: string;
  name: string;
}

export interface AdoptionPlanInput {
  project_id: string;
  environment_id: string;
  projects: AdoptionSelection[];
}

// Planned reviews expire on the server 24 hours after they are created.
export const plannedLifetimeMs = 24 * 60 * 60 * 1000;
export const adoptionSelectionLimit = 16;

const base = (node: string) => `/api/nodes/${encodeURIComponent(node)}/adoptions`;
const item = (node: string, id: string) => `${base(node)}/${encodeURIComponent(id)}`;

export const adoptionKeys = {
  all: (node: string) => ["nodes", node, "adoptions"] as const,
  detail: (node: string, id: string) => ["nodes", node, "adoptions", id] as const,
};

export const adoptionsQuery = (node: string) =>
  queryOptions({
    queryKey: adoptionKeys.all(node),
    queryFn: ({ signal }) => request<AdoptionSummary[] | null>("GET", base(node), undefined, signal),
  });

export const adoptionQuery = (node: string, id: string) =>
  queryOptions({
    queryKey: adoptionKeys.detail(node, id),
    queryFn: ({ signal }) => request<Adoption>("GET", item(node, id), undefined, signal),
    retry: false,
  });

export function discoverAdoptions(node: string) {
  return request<AdoptionDiscovery>("POST", `${base(node)}/discover`, {});
}

export function planAdoption(node: string, input: AdoptionPlanInput) {
  return request<Adoption>("POST", base(node), input);
}

export function deleteAdoption(node: string, id: string) {
  return request<{ ok: boolean }>("DELETE", item(node, id));
}

export function adopt(node: string, id: string) {
  return request<Adoption>("POST", `${item(node, id)}/adopt`, { acknowledge: true, manager_paused: true });
}

export function refreshAdoption(node: string, id: string) {
  return request<Adoption>("POST", `${item(node, id)}/refresh`, {});
}
