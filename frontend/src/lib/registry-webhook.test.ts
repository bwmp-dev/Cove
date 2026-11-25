import { describe, expect, it } from "vitest";
import { registryWebhookPollInterval, type RegistryWebhookStatus } from "../api/registry-webhook";
import { defaultProvider, dockerHubSecretURL, filterDraft, providerError, rotateBlockedReason, saveRotates, validateWebhookDraft, webhookDraftFrom, webhookProblem, webhookSaveState, type WebhookDraft } from "./registry-webhook";

// Fixture status; no control plane is involved.
function status(extra: Partial<RegistryWebhookStatus> = {}): RegistryWebhookStatus {
  return { configuration: null, endpoint: null, registry: { id: "r1", name: "Team", server: "registry.example.test" }, matches_saved_registry: false, worker_supported: true, last_received_at: null, last_matched_at: null, pending_at: null, last_operation: null, last_error: "", last_ignored_reason: "", ownership_required: false, ...extra };
}

const editor = { canEdit: true, canOperate: true };
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

  it("claims ownership with an unchanged save and confirms turning automatic deploys on", () => {
    const enabled = { ...draft([["team/web", "*"]]), enabled: true };
    expect(webhookSaveState(saved(), enabled, enabled, true)).toMatchObject({ changed: false, claims: false, dirty: false, confirmsEnable: false });
    expect(webhookSaveState(saved({ ownership_required: true }), enabled, enabled, true)).toMatchObject({ changed: false, claims: true, dirty: true, confirmsEnable: true });
    expect(webhookSaveState(saved({ ownership_required: true }), enabled, enabled, false)).toMatchObject({ claims: false, dirty: false });
    expect(webhookSaveState(status(), enabled, enabled, true)).toMatchObject({ dirty: true, confirmsEnable: true });
  });

  it("explains why the secret cannot be rotated, offline first", () => {
    expect(rotateBlockedReason(saved(), true, undefined)).toBeUndefined();
    expect(rotateBlockedReason(saved({ registry: null }), false, "You are offline.")).toBe("You are offline.");
    expect(rotateBlockedReason(saved({ registry: null }), false, undefined)).toBe("Rotating the secret needs Edit configuration and Operate access.");
    expect(rotateBlockedReason(saved({ matches_saved_registry: false }), true, undefined)).toBe("Save the current registry selection first");
  });

  it("names why pushes are not deployed", () => {
    expect(webhookProblem(status(), editor)).toBeNull();
    expect(webhookProblem(saved(), editor)).toBeNull();
    expect(webhookProblem(saved({ registry: null }), editor)?.title).toBe("No connected registry");
    expect(webhookProblem(saved({ matches_saved_registry: false }), editor)?.title).toBe("Registry changed");
    expect(webhookProblem(saved({ worker_supported: false }), editor)?.title).toBe("Worker upgrade needed");
    expect(webhookProblem(saved({ configuration: { provider: "distribution", enabled: false, filters: [], registry_id: "r1", registry_server: "registry.example.test" } }), editor)?.tone).toBe("info");
  });

  it("says who can take over a webhook whose owner lost access, and never that it happens automatically", () => {
    const orphaned = saved({ ownership_required: true });
    const claim = webhookProblem(orphaned, editor);
    expect(claim).toMatchObject({ tone: "warn", title: "Automatic deploys suspended" });
    expect(claim?.text).toMatch(/No one currently authorised owns this webhook/);
    expect(claim?.text).toMatch(/losing access to this resource, no longer meeting the multi-factor authentication requirement, or being locked, deactivated or deprovisioned/);
    expect(claim?.text).toMatch(/If that access is restored, pushes deploy again without a new owner, unless the account was deactivated or deprovisioned\./);
    expect(claim?.text).toMatch(/cancels the previous owner’s pending webhook deploys/);
    expect(claim?.text).toMatch(/never taken over automatically/);
    expect(claim?.text).toMatch(/Save the webhook or rotate its secret to take ownership/);
    const editOnly = webhookProblem(orphaned, { canEdit: true, canOperate: false })?.text;
    expect(editOnly).toMatch(/Taking ownership needs Edit configuration and Operate access\. You can turn automatic deploys off or remove the webhook\./);
    expect(editOnly).not.toMatch(/Save the webhook or rotate/);
    expect(webhookProblem(orphaned, { canEdit: false, canOperate: true })?.text).toMatch(/Someone with Edit configuration and Operate access can save it/);
    expect(webhookProblem(saved({ ownership_required: true, configuration: { provider: "distribution", enabled: false, filters: [], registry_id: "r1", registry_server: "registry.example.test" } }), editor)?.title).toBe("No authorised owner");
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
