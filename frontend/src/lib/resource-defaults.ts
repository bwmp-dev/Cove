import type { Node, ServiceLimits } from "../api/types";

// Blank means zero, which the API treats as "no default" (team) or "inherit the team value" (environment).
export function parseCpus(text: string): { value: number } | { error: string } {
  const trimmed = text.trim();
  if (trimmed === "") return { value: 0 };
  const value = Number(trimmed);
  if (!Number.isFinite(value) || value < 0 || value > 1024 || (value > 0 && value < 0.01)) return { error: "Enter CPU cores from 0.01 to 1024, or leave blank." };
  return { value };
}

export function parseMemoryMiB(text: string): { value: number } | { error: string } {
  const trimmed = text.trim();
  if (trimmed === "") return { value: 0 };
  const value = Number(trimmed);
  if (!Number.isSafeInteger(value) || value < 0 || value > 16777216 || (value > 0 && value < 6)) return { error: "Enter whole MiB from 6 to 16,777,216, or leave blank." };
  return { value };
}

export interface LimitErrors { cpus?: string; memory?: string }

// Invalid fields count as zero in the draft so previews stay usable while the error is shown.
export function parseLimits(cpus: string, memory: string): { draft: ServiceLimits; errors: LimitErrors } {
  const parsedCpus = parseCpus(cpus);
  const parsedMemory = parseMemoryMiB(memory);
  return {
    draft: { cpus: "value" in parsedCpus ? parsedCpus.value : 0, memory_mb: "value" in parsedMemory ? parsedMemory.value : 0 },
    errors: { cpus: "error" in parsedCpus ? parsedCpus.error : undefined, memory: "error" in parsedMemory ? parsedMemory.error : undefined },
  };
}

export function limitText(value: number) {
  return value > 0 ? String(value) : "";
}

export function formatCpus(cpus: number) {
  return `${Number(cpus.toFixed(2))} ${cpus === 1 ? "core" : "cores"}`;
}

export function formatMiB(mib: number) {
  return mib >= 1024 ? `${Number((mib / 1024).toFixed(2))} GiB` : `${mib} MiB`;
}

// Mirrors the API: an environment field of zero inherits that team field.
export function inheritDefaults(team: ServiceLimits, environment: ServiceLimits): ServiceLimits {
  return { cpus: environment.cpus || team.cpus, memory_mb: environment.memory_mb || team.memory_mb };
}

export function hasDefaults(limits: ServiceLimits | undefined) {
  return Boolean(limits && (limits.cpus > 0 || limits.memory_mb > 0));
}

export function describeDefaults(limits: ServiceLimits) {
  const parts = [];
  if (limits.cpus > 0) parts.push(`${formatCpus(limits.cpus)} CPU`);
  if (limits.memory_mb > 0) parts.push(`${formatMiB(limits.memory_mb)} memory`);
  return parts.length ? parts.join(", ") : "No defaults";
}

// Nodes whose worker reports no resource defaults support. Deploys that receive defaults are refused on these.
export function nodesWithoutDefaultsSupport(nodes: Node[]) {
  return nodes.filter((node) => !node.capabilities?.resource_defaults);
}
