import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { workloadMoveQuery } from "../api/moves";
import type { Node } from "../api/types";
import { ambiguousSubmission, isUnresolvedPhase, moveDestinations, movePhases, phaseInfo, type MovePhase } from "./moves";

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true };
function node(id: string, overrides: Partial<Node> = {}): Node {
  return { id, name: id, address: `${id}.example`, port: 22, mode: "ssh", state: "ready", last_checked_at: null, capabilities: { ...capabilities, workload_moves: true }, ...overrides };
}

describe("moveDestinations", () => {
  const nodes = [node("source"), node("ready"), node("offline", { state: "unreachable" }), node("old", { capabilities }), node("other-team")];

  it("offers only the team's other ready nodes with move support", () => {
    const result = moveDestinations(nodes, "source", ["source", "ready", "offline", "old"]);
    expect(result.available.map((item) => item.id)).toEqual(["ready"]);
    expect(result.excluded.map((item) => [item.node.id, item.reason])).toEqual([
      ["offline", "not_ready"],
      ["old", "needs_upgrade"],
    ]);
  });

  it("uses the already scoped list for team members", () => {
    expect(moveDestinations(nodes, "source", null).available.map((item) => item.id)).toEqual(["ready", "other-team"]);
  });
});

describe("move phases", () => {
  it("polls only unresolved phases", () => {
    const interval = workloadMoveQuery("w1").refetchInterval;
    if (typeof interval !== "function") throw new Error("Expected a polling function");
    const at = (phase: MovePhase | null) => interval({ state: { data: phase ? { id: "op", source_node_id: "a", destination_node_id: "b", phase, last_error: "", updated_at: "" } : null } } as never);
    expect(at("transferring")).toBe(5_000);
    expect(at("cleaning")).toBe(5_000);
    expect(at("succeeded")).toBe(false);
    expect(at("rolled_back")).toBe(false);
    expect(at(null)).toBe(false);
    expect((Object.keys(movePhases) as MovePhase[]).filter(isUnresolvedPhase)).toHaveLength(9);
  });

  it("does not promise a running source or an undoable commit", () => {
    expect(movePhases.committing.detail).toMatch(/does not undo a committed placement/);
    expect(movePhases.cleaning.detail).toMatch(/keep both nodes/);
    expect(movePhases.rolling_back.detail).toMatch(/source stays stopped on purpose/);
    expect(movePhases.rolled_back.detail).toMatch(/check the resource status for whether the source is running/);
    expect(phaseInfo("teleporting")).toMatchObject({ label: "Unknown", tone: "warn" });
  });
});

it("treats lost responses and server errors as possibly queued", () => {
  expect(ambiguousSubmission(new ApiError(0, "offline"))).toBe(true);
  expect(ambiguousSubmission(new ApiError(503, "unconfirmed"))).toBe(true);
  expect(ambiguousSubmission(new Error("boom"))).toBe(true);
  expect(ambiguousSubmission(new ApiError(409, "changed"))).toBe(false);
});
