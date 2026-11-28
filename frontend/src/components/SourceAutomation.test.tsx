import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { DeployHook } from "./DeployHook";
import { SourceTrigger } from "./SourceTrigger";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
afterEach(() => vi.clearAllMocks());
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const resource: Workload = { id: "w1", name: "web", node_id: "n1", project_id: "p1", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], manually_stopped: false, deployed: false, compose: "", environment_keys: [], source: { repository: "https://bitbucket.org/acme/app.git", branch: "main", compose_path: "compose.yaml" }, has_source_token: false };
const future = new Date(Date.now() + 90 * 86_400_000).toISOString();

function wrap(children: ReactNode) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>);
}

it("saves a tag trigger and offers no release trigger for Bitbucket Cloud", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path.endsWith("/github")) return Promise.resolve(null);
    if (path.endsWith("/git-provider")) return Promise.resolve({ connection_id: 4, repository_id: 5, automatic: false, reporting: false, reports: [] });
    if (path === "/api/git/connections") return Promise.resolve([{ id: 4, name: "Bitbucket", kind: "bitbucket", url: "https://api.bitbucket.org/2.0", workspace: "acme", webhook_url: "https://cove.example/hooks/git/4" }]);
    if (path.endsWith("/git-trigger")) return Promise.resolve(method === "PUT" ? body : { kind: "branch", pattern: "" });
    return Promise.reject(new Error(path));
  });
  wrap(<SourceTrigger workload={resource} editable blocked={false} blockers={null} />);

  expect(await screen.findByText("Automatic deployments are off")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByRole("radio", { name: /Releases/ })).not.toBeInTheDocument());
  expect(screen.getByText(/Bitbucket Cloud has no release events/)).toBeInTheDocument();
  expect(screen.getByRole("radio", { name: /Pushes to main/ })).toBeChecked();

  await user.click(screen.getByRole("radio", { name: /Tags/ }));
  await user.click(screen.getByRole("button", { name: "Save changes" }));
  expect(screen.getByText("Enter a tag pattern, such as v*.")).toBeInTheDocument();
  await user.type(screen.getByLabelText("Tag pattern"), "v*");
  await user.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/git-trigger", { kind: "tags", pattern: "v*" }));
});

it("hides the trigger for resources without a managed connection", async () => {
  vi.mocked(request).mockResolvedValue(null);
  const { container } = wrap(<SourceTrigger workload={resource} editable blocked={false} blockers={null} />);
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
  expect(container).toBeEmptyDOMElement();
});

it("shows a new deploy hook token once and rotates only after confirmation", async () => {
  const user = userEvent.setup();
  let hook: unknown = null;
  let serial = 0;
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") return Promise.resolve(hook);
    serial += 1;
    const created = { id: `h${serial}`, url: `https://cove.example/hooks/deploy/h${serial}`, token: `token-${serial}`, expires_at: future };
    hook = { id: created.id, url: created.url, expires_at: created.expires_at };
    return Promise.resolve(created);
  });
  wrap(<DeployHook workload={{ ...resource, source: null }} editable />);

  await user.click(await screen.findByRole("button", { name: "Create deploy hook" }));
  expect(await screen.findByText("token-1")).toBeInTheDocument();
  expect(screen.getByText("https://cove.example/hooks/deploy/h1")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Hide token" }));
  expect(screen.queryByText("token-1")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Rotate token" }));
  expect(request).toHaveBeenCalledTimes(2);
  await user.click(screen.getAllByRole("button", { name: "Rotate token" }).at(-1)!);
  expect(await screen.findByText("token-2")).toBeInTheDocument();
  expect(screen.getByText("https://cove.example/hooks/deploy/h2")).toBeInTheDocument();
});

it("revokes a deploy hook after confirmation", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "GET" ? { id: "h1", url: "https://cove.example/hooks/deploy/h1", expires_at: future } : { ok: true }));
  wrap(<DeployHook workload={resource} editable />);
  await user.click(await screen.findByRole("button", { name: "Revoke hook" }));
  await user.click(screen.getAllByRole("button", { name: "Revoke hook" }).at(-1)!);
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/workloads/w1/deploy-hook"));
  expect(await screen.findByText("No deploy hook exists for this resource.")).toBeInTheDocument();
});
