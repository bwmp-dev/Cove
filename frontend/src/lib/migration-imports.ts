import type { CoolifyExportItem, ImportCandidate } from "../api/migration-imports";

export const importLimitBytes = 2 * 1024 * 1024;
export const importItemLimit = 32;

// Mirrors the server's cove/v1 resource key rule.
export const resourceKey = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;

// The sample uses placeholder values only; it documents the envelope shape.
export const sampleEnvelope = `{
  "items": [
    {
      "kind": "application",
      "resource": { "uuid": "<application uuid>", "name": "shop", "build_pack": "dockercompose", "docker_compose_raw": "services:\\n  web:\\n    image: nginx:1.27" },
      "environment_variables": [{ "key": "APP_ENV", "value": "production", "is_preview": false }]
    },
    {
      "kind": "service",
      "resource": { "uuid": "<service uuid>", "name": "cache", "docker_compose_raw": "services:\\n  redis:\\n    image: redis:7" },
      "environment_variables": []
    }
  ]
}`;

export type ParsedEnvelope = { items: CoolifyExportItem[]; error?: undefined } | { items?: undefined; error: string };

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

// Checks the envelope shape only. Field-level rules (build packs, masked values, Compose) are the server's.
export function parseCoolifyEnvelope(text: string): ParsedEnvelope {
  if (text.trim() === "") return { error: "Paste or upload an export envelope first." };
  if (new Blob([text]).size > importLimitBytes) return { error: "The envelope is larger than 2 MiB." };
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { error: "The envelope is not valid JSON." };
  }
  if (!isRecord(value) || !Array.isArray(value.items)) return { error: 'The envelope needs an "items" array.' };
  const items = value.items;
  if (items.length === 0 || items.length > importItemLimit) return { error: `Include 1 to ${importItemLimit} applications or services.` };
  for (const [index, item] of items.entries()) {
    const position = `Item ${index + 1}`;
    if (!isRecord(item)) return { error: `${position} is not an object.` };
    if (item.kind !== "application" && item.kind !== "service") return { error: `${position} needs "kind" set to "application" or "service".` };
    if (!isRecord(item.resource)) return { error: `${position} needs a "resource" object from the Coolify resource API.` };
    if (item.environment_variables !== undefined && !Array.isArray(item.environment_variables)) return { error: `${position} has "environment_variables" that is not an array.` };
  }
  return { items: items as CoolifyExportItem[] };
}

export function suggestKey(name: string, taken: ReadonlySet<string>) {
  const slug =
    name
      .toLowerCase()
      .replace(/[^a-z0-9_.-]+/g, "-")
      .replace(/^[^a-z0-9]+/, "")
      .replace(/-+$/, "")
      .slice(0, 120) || "imported";
  if (!taken.has(slug)) return slug;
  for (let suffix = 2; ; suffix++) {
    const candidate = `${slug}-${suffix}`;
    if (!taken.has(candidate)) return candidate;
  }
}

export interface MappingDraft {
  selected: boolean;
  key: string;
  name: string;
  environment: string;
  nodeId: string;
}

export function initialMappings(candidates: readonly ImportCandidate[], existingKeys: ReadonlySet<string>): Record<string, MappingDraft> {
  const taken = new Set(existingKeys);
  const result: Record<string, MappingDraft> = {};
  for (const candidate of candidates) {
    const key = suggestKey(candidate.name, taken);
    taken.add(key);
    result[candidate.id] = { selected: (candidate.blockers ?? []).length === 0, key, name: candidate.name, environment: "", nodeId: "" };
  }
  return result;
}

export type MappingErrors = Partial<Record<"key" | "name" | "nodeId", string>>;

export function mappingErrors(mappings: Record<string, MappingDraft>, existingKeys: ReadonlySet<string>): Record<string, MappingErrors> {
  const counts = new Map<string, number>();
  for (const mapping of Object.values(mappings)) {
    if (mapping.selected) counts.set(mapping.key, (counts.get(mapping.key) ?? 0) + 1);
  }
  const result: Record<string, MappingErrors> = {};
  for (const [id, mapping] of Object.entries(mappings)) {
    if (!mapping.selected) continue;
    const errors: MappingErrors = {};
    if (!resourceKey.test(mapping.key)) errors.key = "Use 1–128 letters, digits, dots, dashes or underscores, starting with a letter or digit.";
    else if (existingKeys.has(mapping.key)) errors.key = "This key is already used in the project configuration. Imports only create new resources.";
    else if ((counts.get(mapping.key) ?? 0) > 1) errors.key = "Each imported resource needs its own key.";
    if (mapping.name.length > 120) errors.name = "Use at most 120 characters.";
    if (mapping.nodeId === "") errors.nodeId = "Choose a node.";
    if (Object.keys(errors).length > 0) result[id] = errors;
  }
  return result;
}
