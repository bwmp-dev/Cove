import { expect, it } from "vitest";
import type { ProjectCostItem } from "../api/advice";
import { byEnvironment, formatMinor, minorToInput, parseAmount, rollup } from "./costs";

it("parses amounts into exact minor units for each currency", () => {
  expect(parseAmount("19.99", "USD")).toEqual({ minor: 1999 });
  expect(parseAmount("0", "EUR")).toEqual({ minor: 0 });
  expect(parseAmount("1500", "JPY")).toEqual({ minor: 1500 });
  expect(parseAmount("1.5", "JPY")).toHaveProperty("error");
  expect(parseAmount("1.234", "USD")).toHaveProperty("error");
  expect(parseAmount("-1", "USD")).toHaveProperty("error");
  expect(parseAmount("1", "us")).toHaveProperty("error");
  expect(parseAmount("1000000001", "USD")).toHaveProperty("error");
});

it("formats minor units and round-trips them into the input", () => {
  const plain = (text: string) => text.replace(/\s/g, " ");
  expect(plain(formatMinor(1999, "USD"))).toBe("USD 19.99");
  expect(plain(formatMinor(1500, "JPY"))).toBe("JPY 1,500");
  expect(minorToInput(5, "USD")).toBe("0.05");
  expect(minorToInput(1500, "JPY")).toBe("1500");
});

const item = (overrides: Partial<ProjectCostItem>): ProjectCostItem => ({ environment_id: "", environment_name: "", node_id: "n1", node_name: "node", currency: "USD", monthly_minor: 100, resources: 1, ...overrides });

it("totals per currency, keeps unpriced resources unknown and free nodes at zero", () => {
  const items = [
    item({ environment_id: "e1", environment_name: "production", monthly_minor: 333.33 }),
    item({ environment_id: "e1", environment_name: "production", currency: "EUR", monthly_minor: 0 }),
    item({ environment_id: "e1", environment_name: "production", currency: null, monthly_minor: null, resources: 2 }),
    item({ monthly_minor: 66.67 }),
  ];
  expect(rollup(items)).toEqual({ totals: [{ currency: "EUR", minor: 0 }, { currency: "USD", minor: 400 }], unpriced: 2, resources: 5 });
  const groups = byEnvironment(items);
  expect(groups.map((group) => [group.name, group.unpriced, group.resources])).toEqual([["production", 2, 4], ["Project resources", 0, 1]]);
});
