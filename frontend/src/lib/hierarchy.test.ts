import { describe, expect, it } from "vitest";
import type { Project } from "../api/projects";
import type { WorkloadSummary } from "../api/types";
import { findPlacement, nodeNames, resourcesIn } from "./hierarchy";

const projects: Project[] = [
  {
    id: "p1",
    name: "Shop",
    permissions: ["read"],
    environments: [
      { id: "e1", name: "production", permissions: ["read"] },
      { id: "e2", name: "staging", permissions: ["read"] },
    ],
  },
];

function resource(id: string, environment_id: string, node: string): WorkloadSummary {
  return { project_id: id === "orphan" ? "p2" : "p1", id, name: id, environment_id, node, node_id: node, kind: "service", permissions: ["read"] };
}

const resources = [resource("web", "e1", "alpha"), resource("api", "e2", "beta"), resource("legacy", "", "alpha"), resource("orphan", "gone", "beta")];

describe("hierarchy", () => {
  it("finds a resource's project and environment", () => {
    expect(findPlacement(projects, "e2")).toMatchObject({ project: { name: "Shop" }, environment: { name: "staging" } });
    expect(findPlacement(projects, "")).toBeUndefined();
  });

  it("lists resources across a project's environments", () => {
    expect(resourcesIn(resources, "p1").map((item) => item.id)).toEqual(["web", "api", "legacy"]);
  });

  it("supports project resources without an environment", () => {
    expect(findPlacement(projects, undefined, "p1")).toMatchObject({ project: { id: "p1" }, environment: undefined });
    expect(findPlacement(projects, undefined, "unknown")).toBeUndefined();
  });

  it("reports the distinct nodes an environment spans", () => {
    expect(nodeNames(resources)).toEqual(["alpha", "beta"]);
  });
});
