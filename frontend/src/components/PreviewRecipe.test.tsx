import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { RecipeView } from "../api/previews";
import type { Project } from "../api/projects";
import { PreviewRecipeEditor } from "./PreviewRecipe";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Could not save" }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const project: Project = { id: "p", name: "Shop", permissions: ["read", "operate", "edit"], environments: [{ id: "prod", name: "Production", permissions: ["edit"] }, { id: "pv", name: "Preview demo", preview_id: "x", permissions: ["edit"] }] };

const view: RecipeView = {
  saved: true,
  recipe: { enabled: true, base_environment_id: "prod", branch_pattern: "staging/*", repositories: [7], domain_template: "", access: "private", ttl_hours: 72, max_previews: 3, require_approval: true, resources: { api: { variables: { SESSION_SECRET: { mode: "override", has_value: true } } } } },
  plan: {
    blocked: true,
    dns: "",
    example_hostnames: [],
    repositories: [{ id: 7, name: "acme/api", selected: true, assigned: true }],
    resources: [
      {
        id: "api",
        name: "api",
        kind: "service",
        included: true,
        repository: "acme/api",
        repository_id: 7,
        follows_branch: true,
        source_kind: "git",
        deployed_revision: "0123456789abcdef0123456789abcdef01234567",
        database: { engine: "postgresql", service: "db", name: "app", mode: "copy", supported: true, bytes: 4096 },
        services: [{ name: "api", image: "sha256:1", run: true, default: true, reason: "Serves a domain." }],
        volumes: [{ key: "volume:pgdata", type: "volume", source: "cove-api_pgdata", services: ["db"], mode: "database", default: "database", allowed: ["database", "empty"], reason: "Restored from a fresh logical snapshot of the production database." }],
        variables: [
          { key: "STRIPE_KEY", classification: "sensitive", mode: "", has_override: false, reason: "Looks like a credential or external endpoint." },
          { key: "SESSION_SECRET", classification: "sensitive", mode: "override", has_override: true, reason: "Looks like a credential or external endpoint." },
        ],
        dependencies: [{ key: "connection:9", kind: "connection", label: "billing.internal:443", inside: false }],
        routes: [],
        blockers: [
          { code: "variable_decision", message: "Decide how previews use STRIPE_KEY.", resolution: "Copy the production value, override it, or remove it.", key: "STRIPE_KEY" },
          { code: "external_connection", message: "Private connection billing.internal:443 reaches a resource outside this preview.", resolution: "Omit it.", key: "connection:9" },
        ],
        notes: [],
      },
    ],
  },
};

it("requires values for new overrides, keeps saved ones and never sends the saved marker", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method) => Promise.resolve(method === "GET" ? view : { ok: true }));
  render(
    <QueryClientProvider client={new QueryClient()}>
      <PreviewRecipeEditor project={project} />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("2 decisions needed")).toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "Preview demo" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "STRIPE_KEY in previews" }));
  await user.click(screen.getByRole("option", { name: "Use a preview value" }));
  expect(screen.getByRole("button", { name: "Save recipe" })).toBeDisabled();
  expect(screen.getByText(/needs a preview value/)).toBeInTheDocument();
  expect(screen.getByLabelText("Preview value for SESSION_SECRET")).toHaveAttribute("placeholder", "Saved value is kept");
  await user.type(screen.getByLabelText("Preview value for STRIPE_KEY"), "sk_test_value");
  await user.click(screen.getByRole("checkbox", { name: /Omit private connection/ }));
  await user.click(screen.getByRole("button", { name: "Save recipe" }));
  expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p/preview-recipe", expect.objectContaining({
    resources: { api: { variables: { SESSION_SECRET: { mode: "override" }, STRIPE_KEY: { mode: "override", value: "sk_test_value" } }, external: { "connection:9": "omit" } } },
  }));
});
