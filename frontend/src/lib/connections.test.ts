import { expect, it } from "vitest";
import { edgePath, layoutGraph, resourceStatus, separateLabels, suggestedAlias, validAlias } from "./connections";

it("places connection targets to the right of their sources without duplicating nodes", () => {
  const layout = layoutGraph(["db", "a", "b", "c", "d", "idle"], [
    { source: "a", target: "db" },
    { source: "b", target: "db" },
    { source: "b", target: "c" },
    { source: "d", target: "c" },
  ]);
  const x = (id: string) => layout.nodes.get(id)!.x;
  expect(x("db")).toBeGreaterThan(x("a"));
  expect(x("c")).toBeGreaterThan(x("d"));
  expect(x("a")).toBe(x("b"));
  expect(layout.nodes.size).toBe(6);
  expect(x("idle")).toBe(x("a"));
});

it("terminates on cycles from connections in both directions", () => {
  const layout = layoutGraph(["a", "b"], [
    { source: "a", target: "b" },
    { source: "b", target: "a" },
  ]);
  expect(layout.nodes.size).toBe(2);
  const backwards = edgePath(layout.nodes.get("b")!, layout.nodes.get("a")!);
  expect(backwards.d.startsWith("M")).toBe(true);
});

it("suggests valid private hostnames", () => {
  expect(suggestedAlias("Postgres DB")).toBe("postgres-db.internal");
  expect(suggestedAlias("cove-api")).toBe("app-cove-api.internal");
  expect(suggestedAlias("!!!")).toBe("app.internal");
  expect(validAlias(suggestedAlias("Ünicode Service 2"))).toBe(true);
  expect(validAlias("cove-link")).toBe(false);
  expect(validAlias("Upper.internal")).toBe(false);
  expect(validAlias("localhost")).toBe(false);
});

it("describes resource status from intent and the latest operation", () => {
  expect(resourceStatus({ manually_stopped: true, last_action: "deploy", last_state: "succeeded" }).label).toBe("Stopped");
  expect(resourceStatus({ manually_stopped: false, last_action: "deploy", last_state: "failed" })).toEqual({ tone: "bad", label: "Last deploy failed" });
  expect(resourceStatus({ manually_stopped: false, last_action: "", last_state: "" }).label).toBe("Not deployed");
});

it("keeps overlapping connection labels apart", () => {
  const placed = separateLabels([{ x: 100, y: 100 }, { x: 110, y: 105 }, { x: 400, y: 100 }, { x: 100, y: 120 }]);
  expect(placed[2]).toEqual({ x: 400, y: 100 });
  const ys = [placed[0]!.y, placed[1]!.y, placed[3]!.y].sort((a, b) => a - b);
  expect(ys[1]! - ys[0]!).toBeGreaterThanOrEqual(26);
  expect(ys[2]! - ys[1]!).toBeGreaterThanOrEqual(26);
});
