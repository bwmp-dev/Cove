import { parse } from "yaml";
import type { GPUDevice, GPUInventory } from "../api/gpus";
import type { WorkloadServiceLimits } from "../api/types";

export type GPUMode = "inherit" | "none" | "select";
export type Limits = Record<string, WorkloadServiceLimits>;

export const maxGPUsPerService = 16;
const serviceName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;

export interface GPURow {
  key: string;
  service: string;
  mode: GPUMode;
  selected: string[];
}

export function gpuMode(gpus: string[] | null | undefined): GPUMode {
  if (gpus === undefined || gpus === null) return "inherit";
  return gpus.length === 0 ? "none" : "select";
}

// A service has an explicit GPU setting only when gpus is an array; null and omitted both inherit Compose.
function explicitGPUs(limit: WorkloadServiceLimits | undefined): limit is WorkloadServiceLimits & { gpus: string[] } {
  return Array.isArray(limit?.gpus);
}

export function gpuRows(limits: Limits | undefined): GPURow[] {
  return Object.entries(limits ?? {})
    .filter(([, limit]) => explicitGPUs(limit))
    .map(([service, limit]) => ({ key: service, service, mode: gpuMode(limit.gpus), selected: [...(limit.gpus ?? [])] }));
}

// Entries that only carry a GPU setting belong to the GPU section, so the CPU and memory form hides them.
export function isGPUOnly(limit: WorkloadServiceLimits) {
  return explicitGPUs(limit) && !(limit.cpus > 0) && !(limit.memory_mb > 0);
}

function withoutGPUs(limit: WorkloadServiceLimits): WorkloadServiceLimits {
  return { cpus: limit.cpus, memory_mb: limit.memory_mb };
}

// Applies the CPU and memory form to the saved map: each service keeps its saved gpus value exactly (including null),
// and services removed from the form keep an explicit GPU setting instead of losing it.
export function mergeCPULimits(saved: Limits | undefined, edited: Record<string, { cpus: number; memory_mb: number }>): Limits {
  const result: Limits = {};
  for (const [service, limit] of Object.entries(edited)) {
    const previous = saved?.[service];
    result[service] = previous && Object.hasOwn(previous, "gpus") ? { ...limit, gpus: previous.gpus } : { ...limit };
  }
  for (const [service, limit] of Object.entries(saved ?? {})) {
    if (!Object.hasOwn(result, service) && explicitGPUs(limit)) result[service] = { cpus: 0, memory_mb: 0, gpus: [...limit.gpus] };
  }
  return result;
}

// Applies the GPU rows to the saved map without touching CPU or memory. A saved explicit setting that is no longer a
// row was removed or renamed, so it returns to inheriting Compose. Entries left with nothing to apply are dropped.
export function mergeGPUAssignments(saved: Limits | undefined, rows: GPURow[]): Limits {
  const result: Limits = {};
  const byService = new Map(rows.map((row) => [row.service, row]));
  for (const [service, limit] of Object.entries(saved ?? {})) {
    const row = byService.get(service);
    if (row) result[service] = applyRow(limit, row);
    else result[service] = explicitGPUs(limit) ? withoutGPUs(limit) : { ...limit };
  }
  for (const row of rows) {
    if (!Object.hasOwn(result, row.service)) result[row.service] = applyRow({ cpus: 0, memory_mb: 0 }, row);
  }
  for (const [service, limit] of Object.entries(result)) {
    const previous = saved?.[service];
    const empty = !(limit.cpus > 0) && !(limit.memory_mb > 0) && !explicitGPUs(limit);
    if (empty && (!previous || explicitGPUs(previous))) delete result[service];
  }
  return result;
}

function applyRow(limit: WorkloadServiceLimits, row: GPURow): WorkloadServiceLimits {
  if (row.mode === "inherit") return withoutGPUs(limit);
  return { ...limit, gpus: row.mode === "none" ? [] : [...row.selected] };
}

export interface GPURowErrors {
  service?: string;
  selection?: string;
}

export function validateGPURows(rows: GPURow[], composeServices: string[] | null): Record<string, GPURowErrors> {
  const errors: Record<string, GPURowErrors> = {};
  const seen = new Set<string>();
  const known = composeServices ? new Set(composeServices) : null;
  for (const row of rows) {
    const rowErrors: GPURowErrors = {};
    if (!serviceName.test(row.service)) rowErrors.service = "Enter the Compose service name.";
    else if (seen.has(row.service)) rowErrors.service = "Each service can appear only once.";
    else if (known && !known.has(row.service)) rowErrors.service = "This service is not in the saved Compose file.";
    seen.add(row.service);
    if (row.mode === "select" && row.selected.length === 0) rowErrors.selection = "Select at least one device, or choose No standard GPU requests.";
    else if (row.mode === "select" && row.selected.length > maxGPUsPerService) rowErrors.selection = `Select at most ${maxGPUsPerService} devices.`;
    else if (row.mode === "select" && new Set(row.selected).size !== row.selected.length) rowErrors.selection = "Each device can be selected once.";
    if (rowErrors.service || rowErrors.selection) errors[row.key] = rowErrors;
  }
  return errors;
}

export type DeviceState =
  | { kind: "available"; device: GPUDevice }
  | { kind: "unassignable"; device: GPUDevice }
  | { kind: "missing" }
  | { kind: "unchecked" };

export function deviceState(id: string, inventory: GPUInventory | undefined): DeviceState {
  if (!inventory) return { kind: "unchecked" };
  const device = inventory.devices.find((item) => item.id === id);
  if (!device) return { kind: "missing" };
  return device.assignable ? { kind: "available", device } : { kind: "unassignable", device };
}

// Devices a row can show: everything the latest inspection observed, then saved or selected IDs it did not observe.
// Saved IDs stay listed after they are cleared, so they can be kept again before saving.
export function deviceOptions(selected: string[], saved: string[], inventory: GPUInventory | undefined): string[] {
  const options = inventory?.devices.map((device) => device.id) ?? [];
  const seen = new Set(options);
  for (const id of [...saved, ...selected]) {
    if (!seen.has(id)) {
      seen.add(id);
      options.push(id);
    }
  }
  return options;
}

// Service names from inline Compose. Returns null when the source is in Git or the file cannot be read,
// so callers do not guess.
export function composeServiceNames(compose: string): string[] | null {
  if (!compose.trim()) return null;
  try {
    const document: unknown = parse(compose);
    if (typeof document !== "object" || document === null || !("services" in document)) return null;
    const services = document.services;
    if (typeof services !== "object" || services === null || Array.isArray(services)) return null;
    return Object.keys(services);
  } catch {
    return null;
  }
}

export function formatGPUMemory(memory: number) {
  return memory >= 1024 ? `${Number((memory / 1024).toFixed(1))} GiB` : `${memory} MiB`;
}
