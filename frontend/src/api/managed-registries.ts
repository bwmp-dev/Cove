import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { registryPollInterval } from "../lib/managed-registries";
import { ApiError, request } from "./client";
import { connectionsQuery } from "./connections";
import { keys } from "./queries";

export interface ManagedRegistryRetention {
  keep_per_repository: number;
  minimum_age_days: number;
}

export interface ManagedRegistryStorage {
  hard_cap_verified: boolean;
  reason: string;
}

// The API never returns registry credentials. IDs that do not exist yet, such as the generated connection, are empty.
export interface ManagedRegistry {
  id: string;
  name: string;
  node_id: string;
  project_id: string;
  hostname: string;
  state: string;
  connection_id: string;
  operation_id: string;
  operation_state: string;
  last_error: string;
  retention: ManagedRegistryRetention;
  storage: ManagedRegistryStorage;
  image: string;
  // On a removed registry: its owned data volume was kept on the node. It is not a running registry.
  data_retained: boolean;
}

export interface ManagedRegistryList {
  registries: ManagedRegistry[];
  can_manage: boolean;
}

export interface ManagedRegistryInput {
  name: string;
  node_id: string;
  project_id: string;
  hostname: string;
  retention: ManagedRegistryRetention;
}

// acknowledgeUnavailableNodes is only sent with deleteData: the exact unavailable node IDs the person acknowledged.
export type ManagedRegistryOperation = { kind: "gc" } | { kind: "retry" } | { kind: "remove"; deleteData: boolean; acknowledgeUnavailableNodes?: string[] };

export interface ManagedRegistryNode {
  id: string;
  name: string;
}

export interface ManagedRegistryRemovalInventory {
  // Nodes that are not ready and may hold references to the registry, so their references could not be listed.
  unavailable_nodes: ManagedRegistryNode[];
}

export const managedRegistriesKey = ["managed-registries"] as const;

const basePath = "/api/managed-registries";
const path = (id: string) => `${basePath}/${encodeURIComponent(id)}`;

const text = (value: unknown) => (typeof value === "string" ? value : "");
const count = (value: unknown) => (typeof value === "number" && Number.isInteger(value) ? value : 0);

// Copies only the documented metadata, so an unexpected field such as a credential is never cached. Missing booleans
// read as false, so unknown data never claims a storage cap, retained data or management rights.
function registry(value: Partial<ManagedRegistry>): ManagedRegistry {
  return {
    id: text(value.id),
    name: text(value.name),
    node_id: text(value.node_id),
    project_id: text(value.project_id),
    hostname: text(value.hostname),
    state: text(value.state),
    connection_id: text(value.connection_id),
    operation_id: text(value.operation_id),
    operation_state: text(value.operation_state),
    last_error: text(value.last_error),
    retention: { keep_per_repository: count(value.retention?.keep_per_repository), minimum_age_days: count(value.retention?.minimum_age_days) },
    storage: { hard_cap_verified: value.storage?.hard_cap_verified === true, reason: text(value.storage?.reason) },
    image: text(value.image),
    data_retained: value.data_retained === true,
  };
}

// Matches the server's bounds on the node list and on each acknowledged node ID.
const maxUnavailableNodes = 1024;
const maxNodeIdLength = 128;

// Null unless every entry is a distinct node with an ID and a name, so a malformed list is never read as "no unavailable nodes".
function unavailableNodes(value: unknown): ManagedRegistryNode[] | null {
  if (!Array.isArray(value) || value.length > maxUnavailableNodes) return null;
  const nodes: ManagedRegistryNode[] = [];
  const seen = new Set<string>();
  for (const entry of value as unknown[]) {
    if (typeof entry !== "object" || entry === null) return null;
    const { id, name } = entry as Partial<Record<keyof ManagedRegistryNode, unknown>>;
    if (typeof id !== "string" || id === "" || id.length > maxNodeIdLength || seen.has(id)) return null;
    if (typeof name !== "string" || name.trim() === "") return null;
    seen.add(id);
    nodes.push({ id, name });
  }
  return nodes;
}

function registryList(value: Partial<ManagedRegistryList>): ManagedRegistryList {
  return { registries: (value.registries ?? []).map(registry), can_manage: value.can_manage === true };
}

function connectionSignature(list: ManagedRegistryList) {
  return list.registries.map((item) => `${item.id}:${item.state}:${item.connection_id}`).join(",");
}

function refreshRelated(client: QueryClient) {
  return Promise.all([
    client.invalidateQueries({ queryKey: connectionsQuery("registries").queryKey }),
    client.invalidateQueries({ queryKey: keys.operations }),
    client.invalidateQueries({ queryKey: keys.workloads }),
  ]);
}

// Activation and removal create or delete the generated registry connection outside this page, so a change of state
// or connection refreshes the connection list as well.
export function managedRegistriesQuery(client: QueryClient) {
  return queryOptions({
    queryKey: managedRegistriesKey,
    queryFn: async ({ signal }) => {
      const list = registryList(await request<ManagedRegistryList>("GET", basePath, undefined, signal));
      const previous = client.getQueryData<ManagedRegistryList>(managedRegistriesKey);
      if (previous && connectionSignature(previous) !== connectionSignature(list)) void refreshRelated(client);
      return list;
    },
    refetchInterval: (query) => (query.state.status === "error" ? false : registryPollInterval(query.state.data?.registries)),
    retry: false,
  });
}

export const removalInventoryKey = (id: string) => [...managedRegistriesKey, id, "removal-inventory"] as const;

// Read fresh each time a destructive confirmation needs it, never from an earlier dialog.
export function removalInventoryQuery(id: string) {
  return queryOptions({
    queryKey: removalInventoryKey(id),
    queryFn: async ({ signal }): Promise<ManagedRegistryRemovalInventory> => {
      const body = await request<{ unavailable_nodes?: unknown }>("GET", `${path(id)}/removal-inventory`, undefined, signal);
      const nodes = unavailableNodes(body.unavailable_nodes);
      if (nodes === null) throw new ApiError(200, "The server returned an unreadable response.");
      return { unavailable_nodes: nodes };
    },
    gcTime: 0,
    retry: false,
  });
}

// The refusal of a data deletion whose acknowledged unavailable nodes no longer match, with the current nodes.
export function inventoryRefusal(error: unknown): ManagedRegistryNode[] | null {
  if (!(error instanceof ApiError) || error.status !== 409 || typeof error.details !== "object" || error.details === null) return null;
  const details = error.details as { error?: unknown; unavailable_nodes?: unknown };
  if (details.error !== "registry_inventory_changed") return null;
  return unavailableNodes(details.unavailable_nodes);
}

export interface ManagedRegistryInventoryFailure {
  resource: { id: string; name: string };
  node: { id: string; name: string };
  reason: string;
}

const maxNameLength = 256;
const maxReasonLength = 300;

function namedEntity(value: unknown): { id: string; name: string } | null {
  if (typeof value !== "object" || value === null) return null;
  const { id, name } = value as { id?: unknown; name?: unknown };
  if (typeof id !== "string" || id === "" || id.length > maxNodeIdLength) return null;
  if (typeof name !== "string" || name.trim() === "" || name.length > maxNameLength) return null;
  return { id, name };
}

// The refusal of an operation because a ready node could not list its references to the registry. Malformed details
// return null, so they are shown as a generic refusal and never read as a complete inventory.
export function inventoryFailure(error: unknown): ManagedRegistryInventoryFailure | null {
  if (!(error instanceof ApiError) || error.status !== 409 || typeof error.details !== "object" || error.details === null) return null;
  const details = error.details as { error?: unknown; resource?: unknown; node?: unknown; reason?: unknown };
  if (details.error !== "registry_inventory_incomplete") return null;
  const resource = namedEntity(details.resource);
  const node = namedEntity(details.node);
  const { reason } = details;
  if (!resource || !node || typeof reason !== "string" || reason.trim() === "" || reason.length > maxReasonLength) return null;
  return { resource, node, reason };
}

function removalBody(operation: { deleteData: boolean; acknowledgeUnavailableNodes?: string[] }) {
  const acknowledged = operation.deleteData ? (operation.acknowledgeUnavailableNodes ?? []) : [];
  return acknowledged.length > 0 ? { delete_data: true, acknowledge_unavailable_nodes: acknowledged } : { delete_data: operation.deleteData };
}

function updateRegistry(client: QueryClient, id: string, patch: Partial<ManagedRegistry>) {
  client.setQueryData<ManagedRegistryList>(managedRegistriesKey, (list) => list && { ...list, registries: list.registries.map((item) => (item.id === id ? { ...item, ...patch } : item)) });
}

export function useCreateManagedRegistry() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: ManagedRegistryInput) => request<{ id: string; operation_id: string }>("POST", basePath, input),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: managedRegistriesKey, exact: true });
      void refreshRelated(client);
    },
  });
}

export interface ManagedRegistryUpdate {
  retention: ManagedRegistryRetention;
  // Only while provisioning failed: a stored certificate of the registry resource, or "" for Let's Encrypt.
  certificate_id?: string;
}

export function useSaveManagedRegistry(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: ManagedRegistryUpdate) => request<{ ok: true }>("PUT", path(id), input),
    // A read that started before the save could otherwise land afterwards and show the old retention.
    onSuccess: async (_, input) => {
      await client.cancelQueries({ queryKey: managedRegistriesKey, exact: true });
      updateRegistry(client, id, { retention: input.retention });
    },
    onSettled: (_, __, input) => {
      void client.invalidateQueries({ queryKey: managedRegistriesKey, exact: true });
      if (input.certificate_id !== undefined) void client.invalidateQueries({ queryKey: keys.workload(id), exact: true });
    },
  });
}

// The API deletes the empty removed registry with its generated resource and operation history, so the resource and
// operation lists change as well.
export function useDeleteManagedRegistryRecord(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ ok: true }>("DELETE", `${path(id)}/record`),
    onSuccess: () => Promise.all([client.invalidateQueries({ queryKey: managedRegistriesKey, exact: true }), refreshRelated(client)]),
  });
}

const operationStates: Record<ManagedRegistryOperation["kind"], string> = { gc: "maintenance", retry: "provisioning", remove: "removing" };

// The API commits the new state and operation before it answers 202, so the cache shows them until the refetch lands.
export function useManagedRegistryOperation(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (operation: ManagedRegistryOperation) =>
      operation.kind === "remove" ? request<{ operation_id: string }>("DELETE", path(id), removalBody(operation)) : request<{ operation_id: string }>("POST", `${path(id)}/${operation.kind}`),
    onSuccess: async ({ operation_id }, operation) => {
      await client.cancelQueries({ queryKey: managedRegistriesKey, exact: true });
      updateRegistry(client, id, { state: operationStates[operation.kind], operation_id: text(operation_id), operation_state: "queued", last_error: "" });
    },
    // The open confirmation shows the nodes from the refusal and asks for a new acknowledgement.
    onError: (error) => {
      const nodes = inventoryRefusal(error);
      if (nodes) client.setQueryData<ManagedRegistryRemovalInventory>(removalInventoryKey(id), { unavailable_nodes: nodes });
    },
    onSettled: () => {
      void client.invalidateQueries({ queryKey: managedRegistriesKey, exact: true });
      void refreshRelated(client);
    },
  });
}
