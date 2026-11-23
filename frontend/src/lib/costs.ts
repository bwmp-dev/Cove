import type { ProjectCostItem } from "../api/advice";

export const maxMonthlyMinor = 100_000_000_000;

// Returns undefined for codes the browser does not recognise as an ISO 4217 currency.
export function currencyDigits(currency: string): number | undefined {
  if (!/^[A-Z]{3}$/.test(currency)) return undefined;
  try {
    return new Intl.NumberFormat("en", { style: "currency", currency }).resolvedOptions().maximumFractionDigits;
  } catch {
    return undefined;
  }
}

// Allocated shares can be fractional minor units; they are rounded only for display.
export function formatMinor(minor: number, currency: string) {
  const digits = currencyDigits(currency) ?? 2;
  return new Intl.NumberFormat("en", { style: "currency", currency, currencyDisplay: "code", minimumFractionDigits: digits, maximumFractionDigits: digits }).format(minor / 10 ** digits);
}

export function minorToInput(minor: number, currency: string) {
  const digits = currencyDigits(currency) ?? 2;
  if (digits === 0) return String(minor);
  const text = String(minor).padStart(digits + 1, "0");
  return `${text.slice(0, -digits)}.${text.slice(-digits)}`;
}

// Parses a monthly amount in major units without floating point, so 19.99 is exactly 1999 minor units.
export function parseAmount(text: string, currency: string): { minor: number } | { error: string } {
  const digits = currencyDigits(currency);
  if (digits === undefined) return { error: "Enter a three-letter ISO 4217 currency code, such as USD or EUR." };
  const match = /^(\d+)(?:\.(\d*))?$/.exec(text.trim());
  if (!match) return { error: "Enter the monthly amount as a number, such as 12.50. Use 0 for a node that costs nothing." };
  const fraction = match[2] ?? "";
  if (fraction.length > digits) return { error: digits === 0 ? `${currency} has no minor unit; enter a whole amount.` : `${currency} uses at most ${digits} decimal places.` };
  const minor = Number(`${match[1]}${fraction.padEnd(digits, "0")}`);
  if (!Number.isSafeInteger(minor) || minor > maxMonthlyMinor) return { error: "The amount is too large." };
  return { minor };
}

export interface CurrencyTotal {
  currency: string;
  minor: number;
}

export interface CostRollup {
  totals: CurrencyTotal[];
  // Resources on nodes without a price. Their cost is unknown, not zero.
  unpriced: number;
  resources: number;
}

export function rollup(items: ProjectCostItem[]): CostRollup {
  const totals = new Map<string, number>();
  let unpriced = 0;
  let resources = 0;
  for (const item of items) {
    resources += item.resources;
    if (item.currency === null || item.monthly_minor === null) {
      unpriced += item.resources;
      continue;
    }
    totals.set(item.currency, (totals.get(item.currency) ?? 0) + item.monthly_minor);
  }
  return {
    totals: [...totals.entries()].map(([currency, minor]) => ({ currency, minor })).sort((a, b) => a.currency.localeCompare(b.currency)),
    unpriced,
    resources,
  };
}

export interface EnvironmentCosts extends CostRollup {
  id: string;
  name: string;
  items: ProjectCostItem[];
}

// Project resources outside any environment have an empty environment ID and come last.
export function byEnvironment(items: ProjectCostItem[]): EnvironmentCosts[] {
  const groups = new Map<string, { name: string; items: ProjectCostItem[] }>();
  for (const item of items) {
    const group = groups.get(item.environment_id) ?? { name: item.environment_id ? item.environment_name : "Project resources", items: [] };
    group.items.push(item);
    groups.set(item.environment_id, group);
  }
  return [...groups.entries()]
    .map(([id, group]) => ({ id, name: group.name, items: group.items, ...rollup(group.items) }))
    .sort((a, b) => Number(a.id === "") - Number(b.id === "") || a.name.localeCompare(b.name));
}
