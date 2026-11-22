import catalog from "./templates.json";
import type { Route } from "../api/types";

export interface ResourceDraft {
  name: string;
  compose: string;
  environment: Record<string, string>;
  routes: Route[];
}

export type ResourceTemplate = (typeof catalog)[number];
export const templates = catalog;

export function templateValues(template: ResourceTemplate): Record<string, string> {
  return Object.fromEntries(template.variables.map((variable) => [variable.key, variable.secret
    ? Array.from(crypto.getRandomValues(new Uint8Array(32)), (byte) => byte.toString(16).padStart(2, "0")).join("")
    : variable.value]));
}

export function templateDraft(template: ResourceTemplate, values: Record<string, string>, hostname: string): ResourceDraft {
  const domain = hostname.trim().toLowerCase();
  if (template.web && (domain.length > 253 || !domain.includes(".") || !domain.split(".").every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)))) {
    throw new Error("Enter a hostname without a scheme, path, port or wildcard.");
  }
  const environment: Record<string, string> = {};
  for (const variable of template.variables) {
    const value = values[variable.key] ?? "";
    if (!value.trim()) throw new Error(`${variable.label} is required.`);
    if (variable.secret && !/^[a-f0-9]{64}$/.test(value)) throw new Error(`${variable.label} must contain 64 lowercase hexadecimal characters.`);
    environment[variable.key] = value;
  }
  if (template.web?.urlVariable) environment[template.web.urlVariable] = `https://${domain}`;
  return {
    name: template.id, compose: template.compose, environment,
    routes: template.web ? [{ domain, service: template.web.service, port: template.web.port }] : [],
  };
}
