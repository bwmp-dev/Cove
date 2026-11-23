import { expect, it } from "vitest";
import type { Project } from "../api/projects";
import type { ConfigurationDraft, ProjectConfiguration, WorkloadDrift } from "../api/project-configuration";
import type { WorkloadSummary } from "../api/types";
import { draftState, driftResult, exportFilename, formatDiffValue, gitOpsCandidates, manifestYaml } from "./project-configuration";

it("shows JSON-encoded diff values readably", () => {
  expect(formatDiffValue('"main"')).toBe("main");
  expect(formatDiffValue("null")).toBe("Not set");
  expect(formatDiffValue('""')).toBe("Not set");
  expect(formatDiffValue("[]")).toBe("None");
  expect(formatDiffValue('{"web":{"cpus":1}}')).toBe('{"web":{"cpus":1}}');
  expect(formatDiffValue("not json")).toBe("not json");
});

const draft = (change: Partial<ConfigurationDraft>): ConfigurationDraft => ({ id: "d", base_version: "v1", changes: [], created_at: "2026-11-20T00:00:00Z", applied_at: null, operation_ids: null, ...change });

it("marks drafts saved against an older configuration as out of date", () => {
  expect(draftState(draft({}), "v1")).toBe("ready");
  expect(draftState(draft({}), "v2")).toBe("stale");
  expect(draftState(draft({ applied_at: "2026-11-21T00:00:00Z" }), "v2")).toBe("applied");
  expect(draftState(draft({}), undefined)).toBe("ready");
});

const report = (change: Partial<WorkloadDrift>): WorkloadDrift => ({ baseline: true, busy: false, operation: "o", captured_at: null, checked_at: "2026-11-22T00:00:00Z", changes: [], ...change });

it("reports no drift only with a baseline, a completed inspection and no changes", () => {
  expect(driftResult(report({}))).toBe("clean");
  expect(driftResult(report({ changes: null }))).toBe("clean");
  expect(driftResult(report({ baseline: false }))).toBe("no-baseline");
  expect(driftResult(report({ busy: true }))).toBe("busy");
  expect(driftResult(report({ baseline: false, busy: true, deferred: "suspended" }))).toBe("suspended");
  // The worker skips loading a baseline while busy, so the real response is baseline:false with busy:true.
  expect(driftResult({ baseline: false, busy: true, operation: "", captured_at: null, checked_at: "2026-11-22T00:00:00Z", changes: null })).toBe("busy");
  expect(driftResult(report({ changes: [{ container: "c", service: "web", state: "missing", fields: null }] }))).toBe("drifted");
});

it("offers only editable persistent Git resources of the project as GitOps sources", () => {
  const project: Project = { id: "p", name: "Shop", permissions: ["read", "edit"], environments: [{ id: "e1", name: "Production", permissions: [] }, { id: "pr", name: "pr-4", preview_id: "x", permissions: [] }] };
  const summary = (id: string, extra: Partial<WorkloadSummary>): WorkloadSummary => ({ id, name: id, project_id: "p", node_id: "n", node: "One", kind: "service", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], ...extra });
  const git = { repository: "r" };
  const resources = [
    summary("w-web", { name: "web", environment_id: "e1", environment: "Production" }),
    summary("w-web-2", { name: "web", environment_id: "e1", environment: "Production" }),
    summary("w-db", { name: "db", environment_id: "e1", environment: "Production" }),
    summary("w-preview", { name: "preview", environment_id: "pr", environment: "pr-4" }),
    summary("w-readonly", { name: "readonly", permissions: ["read"] }),
    summary("w-elsewhere", { name: "elsewhere", project_id: "q" }),
    summary("w-shared", { name: "shared" }),
  ];
  const configuration: ProjectConfiguration = {
    version: "v1",
    manifest: {
      api_version: "cove/v1",
      project: "Shop",
      resources: [
        // Two resources share a name; only the one with the Git source may be offered.
        { key: "web-git", environment: "Production", configuration: { name: "web", source: git } },
        { key: "web-image", environment: "Production", configuration: { name: "web" } },
        { key: "db", environment: "Production", configuration: { name: "db" } },
        { key: "preview", configuration: { name: "preview", source: git } },
        { key: "readonly", configuration: { name: "readonly", source: git } },
        { key: "elsewhere", configuration: { name: "elsewhere", source: git } },
        { key: "shared-stable-key", configuration: { name: "shared", source: git } },
        { key: "unmapped", configuration: { name: "db", source: git } },
      ],
    },
    resource_ids: { "web-git": "w-web-2", "web-image": "w-web", db: "w-db", preview: "w-preview", readonly: "w-readonly", elsewhere: "w-elsewhere", "shared-stable-key": "w-shared" },
  };
  expect(gitOpsCandidates(project, resources, configuration).map((resource) => resource.id)).toEqual(["w-web-2", "w-shared"]);
  expect(gitOpsCandidates(project, resources, { ...configuration, resource_ids: null })).toEqual([]);
});

it("names exports after the server or the selected scope", () => {
  expect(exportFilename("shop.zip", "Shop", undefined)).toBe("shop.zip");
  expect(exportFilename(null, "My Shop", "Production")).toBe("my-shop-production-configuration.zip");
});

it("renders the manifest as YAML", () => {
  expect(manifestYaml({ api_version: "cove/v1", project: "Shop", variable_keys: ["TOKEN"] })).toBe("api_version: cove/v1\nproject: Shop\nvariable_keys:\n  - TOKEN\n");
});
