import type { CatalogOption, ProviderKind, ProvisionPhase } from "../api/nodeProviders";
import { currencyDigits, minorToInput, parseAmount } from "./costs";
import type { Tone } from "./nodes";

export const providerKinds: Record<ProviderKind, { label: string; tokenHint: string }> = {
  hetzner: { label: "Hetzner Cloud", tokenHint: "A Read & Write API token from the Hetzner Cloud project where the servers should be created. Hetzner tokens apply to one project only." },
  digitalocean: { label: "DigitalOcean", tokenHint: "Grant only what Cove needs: reading regions, sizes and images, and creating and reading droplets." },
  vultr: { label: "Vultr", tokenHint: "Vultr API keys apply to the whole account and cannot be limited to products. Limit the key's access control list to the panel's outbound IP address." },
};

export function providerLabel(kind: string) {
  return providerKinds[kind as ProviderKind]?.label ?? kind;
}

export interface ProviderErrors {
  name?: string;
  token?: string;
}

// The API validates authoritatively; this mirrors its limits so mistakes are caught before sending a token.
export function validateProvider(values: { name: string; token: string }, editing: boolean): { input?: { name: string; token: string }; errors: ProviderErrors } {
  const errors: ProviderErrors = {};
  const name = values.name.trim();
  const token = values.token.trim();
  if (name === "") errors.name = "Enter a name.";
  else if (name.length > 120) errors.name = "Use at most 120 characters.";
  if (token === "") {
    if (!editing) errors.token = "Paste the provider's API token.";
  } else if (/\s/.test(token)) errors.token = "Paste the token on a single line without spaces.";
  else if (token.length < 16 || token.length > 4096) errors.token = "API tokens are 16 to 4096 characters long.";
  if (errors.name || errors.token) return { errors };
  return { input: { name, token }, errors };
}

export const phases: Record<ProvisionPhase, { label: string; tone: Tone; description: string }> = {
  queued: { label: "Queued", tone: "info", description: "Waiting to ask the provider for the server." },
  creating: { label: "Creating server", tone: "info", description: "Cove asked the provider to create the server. The provider may already bill for it." },
  server_pending: { label: "Waiting for server", tone: "info", description: "The provider created the server and is starting it. It is billed from now on." },
  enrolling: { label: "Enrolling worker", tone: "info", description: "The server is installing the worker from its first-boot script and registering its SSH host key." },
  succeeded: { label: "Node added", tone: "ok", description: "The worker enrolled and the node passed verification." },
  failed: { label: "Failed", tone: "bad", description: "Cove stopped tracking this job. A server may still exist and be billed: check the provider console and delete it there if you do not need it." },
  uncertain: { label: "Outcome unknown", tone: "warn", description: "Cove could not confirm whether the provider created the server. It may exist and be billed. Reconcile to look it up; this never creates another server." },
  cancelled: { label: "Tracking abandoned", tone: "neutral", description: "Cove stopped tracking this job. Abandoning never deletes a server: check the provider console and delete it there if one was created." },
};

export function phaseInfo(phase: string) {
  return phases[phase as ProvisionPhase] ?? { label: phase, tone: "neutral" as const, description: "" };
}

export function catalogPrice(option: Pick<CatalogOption, "monthly" | "currency">) {
  if (!option.monthly || !option.currency) return undefined;
  const amount = Number(option.monthly);
  if (!Number.isFinite(amount)) return undefined;
  try {
    return `${new Intl.NumberFormat("en", { style: "currency", currency: option.currency, currencyDisplay: "code", maximumFractionDigits: 4 }).format(amount)} per month`;
  } catch {
    return `${option.monthly} ${option.currency} per month`;
  }
}

// Converts the provider's decimal price into the node price form's text, or undefined when it does not
// fit the currency's minor unit exactly (such as Hetzner's four-decimal net prices).
export function catalogPriceInput(option: Pick<CatalogOption, "monthly" | "currency">) {
  if (!option.monthly || !option.currency) return undefined;
  const currency = option.currency.toUpperCase();
  if (currencyDigits(currency) === undefined) return undefined;
  const text = option.monthly.trim().replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "");
  const parsed = parseAmount(text, currency);
  return "minor" in parsed ? { currency, amount: minorToInput(parsed.minor, currency) } : undefined;
}

function memory(mb: number) {
  return mb >= 1024 ? `${Number((mb / 1024).toFixed(1)).toLocaleString("en")} GB` : `${mb.toLocaleString("en")} MB`;
}

export function planSummary(plan: CatalogOption) {
  const parts: string[] = [];
  if (plan.cpus !== undefined) parts.push(`${plan.cpus} vCPU`);
  if (plan.memory_mb !== undefined) parts.push(memory(plan.memory_mb));
  if (plan.architecture) parts.push(plan.architecture);
  parts.push(catalogPrice(plan) ?? "Price unknown");
  return parts.join(" Â· ");
}

export function optionLabel(option: CatalogOption) {
  return option.architecture ? `${option.name} (${option.architecture})` : option.name;
}

// A plan without a region list is offered everywhere as far as the catalogue says; the API still decides.
export function plansForRegion(plans: CatalogOption[], region: string) {
  if (region === "") return plans;
  return plans.filter((plan) => !plan.regions || plan.regions.includes(region));
}

export function architectureMismatch(plan: CatalogOption | undefined, image: CatalogOption | undefined) {
  return Boolean(plan?.architecture && image?.architecture && plan.architecture !== image.architecture);
}

export function validateServerName(value: string) {
  const name = value.trim();
  if (name === "") return "Enter a server name.";
  if (name.length > 120) return "Use at most 120 characters.";
  return undefined;
}

// The optional price is recorded as the node's monthly cost. Both fields blank means no price.
export function parseCost(currency: string, amount: string): { cost?: { monthly_minor: number; currency: string }; errors: { currency?: string; amount?: string } } {
  const code = currency.trim().toUpperCase();
  if (code === "" && amount.trim() === "") return { errors: {} };
  const parsed = parseAmount(amount, code);
  if ("error" in parsed) return { errors: currencyDigits(code) === undefined ? { currency: parsed.error } : { amount: parsed.error } };
  return { cost: { monthly_minor: parsed.minor, currency: code }, errors: {} };
}
