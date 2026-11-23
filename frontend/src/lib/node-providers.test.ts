import { expect, it } from "vitest";
import { architectureMismatch, catalogPrice, catalogPriceInput, parseCost, planSummary, plansForRegion, validateProvider } from "./node-providers";

it("validates provider tokens without requiring a replacement on edit", () => {
  expect(validateProvider({ name: " ", token: "" }, false).errors).toEqual({ name: "Enter a name.", token: "Paste the provider's API token." });
  expect(validateProvider({ name: "Hetzner", token: "short" }, false).errors.token).toMatch(/16 to 4096/);
  expect(validateProvider({ name: "Hetzner", token: "abcdefghijklmnop\nqrs" }, false).errors.token).toMatch(/single line/);
  expect(validateProvider({ name: "Hetzner", token: "abcdefgh ijklmnopq" }, false).errors.token).toMatch(/without spaces/);
  expect(validateProvider({ name: " Hetzner ", token: "" }, true)).toEqual({ input: { name: "Hetzner", token: "" }, errors: {} });
  expect(validateProvider({ name: "Hetzner", token: " abcdefghijklmnop " }, false).input).toEqual({ name: "Hetzner", token: "abcdefghijklmnop" });
});

it("shows absent catalogue prices as unknown, never free", () => {
  expect(catalogPrice({})).toBeUndefined();
  expect(catalogPrice({ monthly: "4.5100", currency: "EUR" })).toMatch(/^EUR\s4\.51 per month$/);
  expect(planSummary({ id: "cx22", name: "CX22", cpus: 2, memory_mb: 4096, architecture: "x86" })).toBe("2 vCPU · 4 GB · x86 · Price unknown");
});

it("only suggests a node price that fits the currency's minor unit exactly", () => {
  expect(catalogPriceInput({ monthly: "6.0000", currency: "usd" })).toEqual({ currency: "USD", amount: "6.00" });
  expect(catalogPriceInput({ monthly: "4.5149", currency: "EUR" })).toBeUndefined();
  expect(catalogPriceInput({ monthly: "5" })).toBeUndefined();
});

it("filters plans by listed regions and keeps plans without a region list", () => {
  const plans = [{ id: "a", name: "A", regions: ["fsn1"] }, { id: "b", name: "B", regions: ["nbg1"] }, { id: "c", name: "C" }];
  expect(plansForRegion(plans, "fsn1").map((plan) => plan.id)).toEqual(["a", "c"]);
  expect(plansForRegion(plans, "").map((plan) => plan.id)).toEqual(["a", "b", "c"]);
});

it("flags differing plan and image architectures only when both are known", () => {
  expect(architectureMismatch({ id: "p", name: "P", architecture: "arm" }, { id: "i", name: "I", architecture: "x86" })).toBe(true);
  expect(architectureMismatch({ id: "p", name: "P" }, { id: "i", name: "I", architecture: "x86" })).toBe(false);
});

it("parses the optional node price as exact integer minor units", () => {
  expect(parseCost("", " ")).toEqual({ errors: {} });
  expect(parseCost("usd", "19.99")).toEqual({ cost: { monthly_minor: 1999, currency: "USD" }, errors: {} });
  expect(parseCost("JPY", "1.5").errors.amount).toMatch(/no minor unit/);
  expect(parseCost("", "5").errors.currency).toMatch(/ISO 4217/);
});
