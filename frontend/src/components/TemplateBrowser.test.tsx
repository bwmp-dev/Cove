import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ResourceDraft, TemplateCatalogue } from "../api/templates";
import { templateEntry } from "../test/templates";
import { TemplateBrowser } from "./TemplateBrowser";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const secret = "f".repeat(64);
const draft: ResourceDraft = { name: "garage", compose: "services: {}", environment: { RPC_SECRET: secret }, routes: [], file_secrets: [{ name: "rpc", variable: "RPC_SECRET", services: ["garage"], build_services: [] }] };

function catalogue(revision = "rev-1"): TemplateCatalogue {
  return {
    schema_version: 1,
    can_manage_team: false,
    templates: [
      templateEntry({ id: "postgres", name: "PostgreSQL", category: "Databases", description: "Relational database." }),
      templateEntry(
        {
          id: "garage",
          name: "Garage",
          category: "Storage",
          variables: [{ key: "RPC_SECRET", label: "RPC secret", type: "secret", default: "", required: true, generated: true }],
        },
        { revision },
      ),
      templateEntry(
        { id: "dozzle", name: "Dozzle", category: "Observability", capabilities: ["docker_socket"], web: { route: false, service: "dozzle", port: 8080, url_variable: "" } },
        { warnings: ["Requires docker_socket access to the worker host; review before using this template."], validation: { compose_sha256: "", schema_validated: true, compose_config: false, runtime: "config_only", checked_at: "", message: "" } },
      ),
    ],
  };
}

function renderBrowser(handlers: { get?: () => Promise<unknown>; draft?: (body: unknown) => Promise<unknown> } = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (method === "GET" && path === "/api/templates") return (handlers.get ?? (() => Promise.resolve(catalogue())))();
    if (method === "POST" && path === "/api/templates/draft") return (handlers.draft ?? (() => Promise.resolve(draft)))(body);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onUse = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TemplateBrowser onUse={onUse} onCustom={vi.fn()} onDatabase={vi.fn()} />
    </QueryClientProvider>,
  );
  return { client, onUse };
}

const drafts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "POST" && path === "/api/templates/draft");

it("lists the server catalogue with categories taken from it and filters it", async () => {
  const user = userEvent.setup();
  renderBrowser();
  expect(await screen.findByRole("button", { name: /PostgreSQL/ })).toBeInTheDocument();
  expect(screen.getByText("3 templates")).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Category" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["All categories", "Databases", "Observability", "Storage"]);
  await user.click(screen.getByRole("option", { name: "Storage" }));
  expect(screen.getByText("1 of 3 templates")).toBeInTheDocument();
  fireEvent.change(screen.getByPlaceholderText("Search templates"), { target: { value: "nothing" } });
  expect(screen.getByText("No matching templates")).toBeInTheDocument();
});

it("shows a retryable error when the catalogue is unavailable", async () => {
  renderBrowser({ get: () => Promise.reject(new ApiError(503, "Template catalogue unavailable")) });
  expect(await screen.findByText("Could not load the template catalogue")).toBeInTheDocument();
  expect(screen.getByText("Template catalogue unavailable")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("omits an unedited generated secret, keeps the draft out of the caches and passes file secrets on", async () => {
  const { client, onUse } = renderBrowser();
  fireEvent.click(await screen.findByRole("button", { name: /Garage/ }));
  expect(screen.getByLabelText(/RPC secret/)).toHaveAttribute("type", "password");
  expect(screen.getByLabelText(/RPC secret/)).toHaveValue("");
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  await waitFor(() => expect(onUse).toHaveBeenCalledWith(draft));
  expect(drafts()[0]?.[2]).toEqual({ origin: "builtin", repository_id: "", id: "garage", revision: "rev-1", values: {}, hostname: "", acknowledge_host_access: false });
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain(secret);
  expect(client.getMutationCache().getAll().every((mutation) => mutation.options.gcTime === 0)).toBe(true);
});

it("rejects a replaced generated secret that is not 64 hexadecimal characters", async () => {
  renderBrowser();
  fireEvent.click(await screen.findByRole("button", { name: /Garage/ }));
  fireEvent.change(screen.getByLabelText(/RPC secret/), { target: { value: "short" } });
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  expect(await screen.findByText(/exactly 64 lowercase hexadecimal/)).toBeInTheDocument();
  expect(drafts()).toHaveLength(0);
});

it("requires an explicit host access acknowledgement and explains internal web tools", async () => {
  renderBrowser();
  fireEvent.click(await screen.findByRole("button", { name: /Dozzle/ }));
  expect(screen.getByText(/No public route: dozzle listens on port 8080/)).toBeInTheDocument();
  expect(screen.getAllByText(/Docker socket/).length).toBeGreaterThan(0);
  expect(screen.getByText(/Requires docker_socket access/)).toBeInTheDocument();
  expect(screen.getByText(/no runtime check for this revision/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  expect(await screen.findByText(/Confirm that you reviewed the host access/)).toBeInTheDocument();
  expect(drafts()).toHaveLength(0);
  fireEvent.click(screen.getByRole("checkbox", { name: /I reviewed this host access/ }));
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  await waitFor(() => expect(drafts()).toHaveLength(1));
  expect(drafts()[0]?.[2]).toMatchObject({ id: "dozzle", acknowledge_host_access: true });
});

it("reloads the catalogue when the revision changed and submits the new revision next time", async () => {
  let revision = "rev-1";
  let attempts = 0;
  const { onUse } = renderBrowser({
    get: () => Promise.resolve(catalogue(revision)),
    draft: () => {
      attempts += 1;
      if (attempts === 1) {
        revision = "rev-2";
        return Promise.reject(new ApiError(409, "Template revision changed; review it again"));
      }
      return Promise.resolve(draft);
    },
  });
  fireEvent.click(await screen.findByRole("button", { name: /Garage/ }));
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  expect(await screen.findByText("This template changed")).toBeInTheDocument();
  await waitFor(() => expect(vi.mocked(request).mock.calls.filter(([method]) => method === "GET")).toHaveLength(2));
  fireEvent.click(screen.getByRole("button", { name: /Review configuration/ }));
  await waitFor(() => expect(onUse).toHaveBeenCalled());
  expect(drafts()[1]?.[2]).toMatchObject({ revision: "rev-2" });
});

it("names the template source separately from the upstream project link", async () => {
  renderBrowser({
    get: () =>
      Promise.resolve({
        ...catalogue(),
        templates: [
          templateEntry(
            { id: "wiki", name: "Wiki", source: "https://github.com/example/wiki" },
            { origin: "repository", repository_id: "repo-1", repository_name: "Community templates", repository_url: "https://git.example.com/templates.git" },
          ),
        ],
      }),
  });
  fireEvent.click(await screen.findByRole("button", { name: /Wiki/ }));
  expect(screen.getByText("Community templates")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "https://git.example.com/templates.git" })).toHaveAttribute("href", "https://git.example.com/templates.git");
  expect(screen.getByRole("link", { name: "https://github.com/example/wiki" })).toHaveAttribute("href", "https://github.com/example/wiki");
  expect(screen.getByText("Provided by")).toBeInTheDocument();
  expect(screen.getByText("Upstream")).toBeInTheDocument();
});

it("shows no template source for built-in templates", async () => {
  renderBrowser();
  fireEvent.click(await screen.findByRole("button", { name: /PostgreSQL/ }));
  expect(screen.getByText("Upstream")).toBeInTheDocument();
  expect(screen.queryByText("Provided by")).not.toBeInTheDocument();
});
