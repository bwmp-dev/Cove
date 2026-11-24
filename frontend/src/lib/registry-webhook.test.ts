import { describe, expect, it } from "vitest";
import { registryWebhookPollInterval, type RegistryWebhookStatus } from "../api/registry-webhook";
import { defaultProvider, dockerHubSecretURL, filterDraft, providerError, saveRotates, validateWebhookDraft, webhookDraftFrom, webhookProblem, type WebhookDraft } from "./registry-webhook";

// Fixture status; no control plane is involved.
function status(extra: Partial<RegistryWebhookStatus> = {}): RegistryWebhookStatus {
  return { configuration: null, endpoint: null, registry: { id: "r1", name: "Team", server: "registry.example.test" }, matches_saved_registry: false, worker_supported: true, last_received_at: null, last_matched_at: null, pending_at: null, last_operation: null, last_error: "", last_ignored_reason: "", ...extra };
}

const saved = (extra: Partial<RegistryWebhookStatus> = {}) => status({ configuration: { provider: "distribution", enabled: true, filters: [{ repository: "team/web", tag: "*" }], registry_id: "r1", registry_server: "registry.example.test" }, matches_saved_registry: true, ...extra });

const draft = (filters: [string, string][], provider: WebhookDraft["provider"] = "distribution"): WebhookDraft => ({ provider, enabled: false, filters: filters.map(([repository, tag]) => ({ ...filterDraft(), repository, tag })) });

describe("providers", () => {
  it("defaults by registry host and requires the provider's own registry", () => {
    expect(defaultProvider("ghcr.io")).toBe("ghcr");
    expect(defaultProvider("index.docker.io")).toBe("docker_hub");
    expect(defaultProvider("registry.example.test")).toBe("distribution");
    expect(providerError("distribution", "ghcr.io")).toBeUndefined();
    expect(providerError("docker_hub", "docker.io")).toBeUndefined();
    expect(providerError("ghcr", "registry.example.test")).toMatch(/needs the resource's registry to be ghcr.io/);
  });

  it("encodes the token in the Docker Hub secret URL", () => {
    expect(dockerHubSecretURL("https://cove.example.test/hooks/registry/h1", "a+b/c=")).toBe("https://cove.example.test/hooks/registry/h1?secret=a%2Bb%2Fc%3D");
  });
});

describe("validateWebhookDraft", () => {
  it("accepts exact tags and * and refuses hosts, bad tags and duplicates", () => {
    expect(validateWebhookDraft(draft([["team/web", "1.2.3"], ["team/web", "*"]]), "registry.example.test")).toEqual({});
    const errors = validateWebhookDraft(draft([["registry.example.test:5000/web", "stable"], ["team/api", "-bad"], ["team/db", "x"], ["team/db", "x"]]), "registry.example.test");
    expect(Object.values(errors)).toEqual(expect.arrayContaining([expect.stringMatching(/lowercase repository path/), expect.stringMatching(/tag of at most 128/), "This repository and tag are already listed."]));
    expect(validateWebhookDraft(draft([]), undefined).filters).toBe("Add at least one repository filter.");
    expect(validateWebhookDraft(draft(Array.from({ length: 33 }, (_, index) => [`team/app${index}`, "latest"] as [string, string])), undefined).filters).toBe("Use at most 32 filters.");
  });
});

describe("webhook state", () => {
  it("starts new webhooks disabled with the registry's provider", () => {
    expect(webhookDraftFrom(status({ registry: { id: "r", name: "GHCR", server: "ghcr.io" } }))).toMatchObject({ provider: "ghcr", enabled: false });
  });

  it("rotates on save after a provider or registry change only", () => {
    expect(saveRotates(status(), draft([["team/web", "*"]]))).toBe(false);
    expect(saveRotates(saved(), draft([["team/web", "*"]]))).toBe(false);
    expect(saveRotates(saved(), draft([["team/web", "*"]], "docker_hub"))).toBe(true);
    expect(saveRotates(saved({ matches_saved_registry: false }), draft([["team/web", "*"]]))).toBe(true);
  });

  it("names why pushes are not deployed", () => {
    expect(webhookProblem(status())).toBeNull();
    expect(webhookProblem(saved())).toBeNull();
    expect(webhookProblem(saved({ registry: null }))?.title).toBe("No connected registry");
    expect(webhookProblem(saved({ matches_saved_registry: false }))?.title).toBe("Registry changed");
    expect(webhookProblem(saved({ worker_supported: false }))?.title).toBe("Worker upgrade needed");
    expect(webhookProblem(saved({ configuration: { provider: "distribution", enabled: false, filters: [], registry_id: "r1", registry_server: "registry.example.test" } }))?.tone).toBe("info");
  });
});

describe("registryWebhookPollInterval", () => {
  it("rechecks an idle webhook and follows a pending push until its deploy settles", () => {
    expect(registryWebhookPollInterval(undefined)).toBe(30_000);
    expect(registryWebhookPollInterval(saved())).toBe(30_000);
    expect(registryWebhookPollInterval(saved({ pending_at: "2026-11-23T10:00:00Z" }))).toBe(3_000);
    for (const state of ["queued", "running"] as const) expect(registryWebhookPollInterval(saved({ last_operation: { id: "op1", state, created_at: "2026-11-23T10:00:00Z" } }))).toBe(3_000);
    for (const state of ["succeeded", "failed", "cancelled", "interrupted"] as const) expect(registryWebhookPollInterval(saved({ last_operation: { id: "op1", state, created_at: "2026-11-23T10:00:00Z" } }))).toBe(30_000);
  });
});
