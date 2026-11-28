import { expect, it } from "vitest";
import { initialMappings, mappingErrors, parseCoolifyEnvelope, sampleEnvelope, suggestKey } from "./migration-imports";

it("accepts the documented envelope and explains malformed ones", () => {
  expect(parseCoolifyEnvelope(sampleEnvelope).items).toHaveLength(2);
  expect(parseCoolifyEnvelope("").error).toMatch(/Paste or upload/);
  expect(parseCoolifyEnvelope("{").error).toMatch(/not valid JSON/);
  expect(parseCoolifyEnvelope("[]").error).toMatch(/"items" array/);
  expect(parseCoolifyEnvelope('{"items":[]}').error).toMatch(/1 to 32/);
  expect(parseCoolifyEnvelope('{"items":[{"kind":"database","resource":{}}]}').error).toMatch(/Item 1 needs "kind"/);
  expect(parseCoolifyEnvelope('{"items":[{"kind":"service"}]}').error).toMatch(/"resource" object/);
  expect(parseCoolifyEnvelope('{"items":[{"kind":"service","resource":{},"environment_variables":{}}]}').error).toMatch(/not an array/);
  const many = JSON.stringify({ items: Array.from({ length: 33 }, () => ({ kind: "service", resource: {} })) });
  expect(parseCoolifyEnvelope(many).error).toMatch(/1 to 32/);
});

it("suggests unused valid resource keys from source names", () => {
  expect(suggestKey("My Shop!", new Set())).toBe("my-shop");
  expect(suggestKey("--", new Set())).toBe("imported");
  expect(suggestKey("web", new Set(["web", "web-2"]))).toBe("web-3");
});

it("selects importable candidates and validates their mappings against existing keys", () => {
  const existing = new Set(["web"]);
  const mappings = initialMappings(
    [
      { id: "a", name: "web", kind: "service", blockers: [] },
      { id: "b", name: "worker", kind: "application", blockers: ["Only Docker Compose applications"] },
    ],
    existing,
  );
  expect(mappings.a).toMatchObject({ selected: true, key: "web-2", nodeId: "" });
  expect(mappings.b?.selected).toBe(false);
  expect(mappingErrors(mappings, existing)).toEqual({ a: { nodeId: "Choose a node." } });
  expect(mappingErrors({ a: { ...mappings.a!, key: "web", nodeId: "n" } }, existing).a?.key).toMatch(/already used/);
  expect(mappingErrors({ a: { ...mappings.a!, key: "-bad", nodeId: "n" } }, existing).a?.key).toMatch(/starting with a letter/);
  const duplicate = { ...mappings.a!, key: "same", nodeId: "n" };
  expect(mappingErrors({ a: duplicate, c: duplicate }, existing).c?.key).toMatch(/its own key/);
});
