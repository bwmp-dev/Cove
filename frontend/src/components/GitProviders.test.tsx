import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { GitConnections } from "./GitConnections";
import { GitProviderSource } from "./GitProviderSource";

const session = vi.hoisted(() => ({ is_admin: false, team_id: "team", teams: [{ id: "team", name: "Team", role: "member", grants: [{ scope: "team", role: "editor" }] }], permissions: ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"] }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }), Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const connection = { id: 7, name: "Team GitLab", kind: "gitlab", url: "https://gitlab.com/api/v4", workspace: "", webhook_url: "https://cove.example/hooks/git/7" };
const repository = { id: 11, full_name: "acme/app", clone_url: "https://gitlab.com/acme/app.git", default_branch: "main", installation_id: 7 };

function wrap(children: ReactNode) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>);
}

function workload(overrides: Partial<Workload> = {}): Workload {
  return { id: "w1", name: "web", node_id: "n1", project_id: "p1", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], manually_stopped: false, deployed: false, compose: "", environment_keys: [], source: { repository: "https://gitlab.com/acme/app.git", branch: "main", compose_path: "compose.yaml" }, has_source_token: false, ...overrides };
}

it("creates a connection and shows its signing token only until dismissed", async () => {
  const user = userEvent.setup();
  let list: unknown[] = [];
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") return Promise.resolve(list);
    list = [connection];
    return Promise.resolve({ ...connection, webhook_secret: "whsec_c2lnbmluZw==" });
  });
  wrap(<GitConnections />);
  expect(await screen.findByText("No providers connected.")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Connect provider" }));
  expect(screen.getByText("Enter a name.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  await user.type(screen.getByLabelText("Name"), "Team GitLab");
  await user.type(screen.getByLabelText("Access token"), "glpat-secret-token");
  await user.click(screen.getByRole("button", { name: "Connect provider" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/git/connections", { name: "Team GitLab", kind: "gitlab", url: "https://gitlab.com", token: "glpat-secret-token" }));

  expect(await screen.findByText("whsec_c2lnbmluZw==")).toBeInTheDocument();
  expect(screen.getByText(/GitLab 19 or later signing tokens/)).toBeInTheDocument();
  expect(screen.getByLabelText("Access token")).toHaveValue("");
  expect(await screen.findByRole("list", { name: "Connected Git providers" })).toHaveTextContent("Team GitLab");

  await user.click(screen.getByRole("button", { name: "Done" }));
  expect(screen.queryByText("whsec_c2lnbmluZw==")).not.toBeInTheDocument();
});

it("requires a workspace for Bitbucket Cloud, keeps its API URL fixed and accepts an API token email", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue([]);
  wrap(<GitConnections />);
  await user.click(await screen.findByRole("combobox", { name: "Provider" }));
  await user.click(screen.getByRole("option", { name: "Bitbucket Cloud" }));
  expect(screen.getByLabelText("Server URL")).toHaveValue("https://api.bitbucket.org");
  expect(screen.getByLabelText("Server URL")).toHaveAttribute("readonly");
  await user.type(screen.getByLabelText("Name"), "Bitbucket");
  await user.type(screen.getByLabelText("OAuth access token"), "bitbucket-token");
  await user.click(screen.getByRole("button", { name: "Connect provider" }));
  expect(screen.getByText("Enter the workspace slug.")).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);

  await user.type(screen.getByLabelText("Workspace"), "acme");
  await user.type(screen.getByLabelText(/API token email/), "ops@example.com");
  expect(screen.getByLabelText("API token")).toHaveValue("bitbucket-token");
  await user.click(screen.getByRole("button", { name: "Connect provider" }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/git/connections", { name: "Bitbucket", kind: "bitbucket", url: "https://api.bitbucket.org", workspace: "acme", username: "ops@example.com", token: "bitbucket-token" }),
  );
});

it("connects a resource to a provider repository with automatic deployment and reporting", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.endsWith("/git-provider") && method === "GET") return Promise.resolve(null);
    if (path.endsWith("/github")) return Promise.resolve(null);
    if (path === "/api/git/connections") return Promise.resolve([connection]);
    if (path.endsWith("/repositories")) return Promise.resolve([repository]);
    if (path.endsWith("/branches")) return Promise.resolve([{ ref: "refs/heads/main" }, { ref: "refs/heads/release" }]);
    if (method === "PUT") return Promise.resolve({ ok: true });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  wrap(<GitProviderSource workload={workload()} editable temporary={false} blocked={false} blockers={null} />);

  await user.click(await screen.findByRole("combobox", { name: "Connection" }));
  await user.click(screen.getByRole("option", { name: "Team GitLab (GitLab)" }));
  await user.click(await screen.findByRole("combobox", { name: "Repository" }));
  await user.click(screen.getByRole("option", { name: "acme/app" }));
  const branch = await screen.findByRole("combobox", { name: "Branch" });
  await waitFor(() => expect(branch).toBeEnabled());
  expect(branch).toHaveTextContent("main");
  await user.click(branch);
  await user.click(screen.getByRole("option", { name: "release" }));
  await user.click(screen.getByRole("switch", { name: "Report deployments" }));
  await user.click(screen.getByRole("button", { name: "Connect repository" }));

  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/git-provider", { connection_id: 7, repository_id: 11, branch: "release", compose_path: "compose.yaml", automatic: true, reporting: true }),
  );
});

it("asks to disconnect the GitHub App before another provider", async () => {
  vi.mocked(request).mockImplementation((_method: string, path: string) => Promise.resolve(path.endsWith("/github") ? { installation_id: 1, repository_id: 2, automatic: true } : null));
  wrap(<GitProviderSource workload={workload()} editable temporary={false} blocked={false} blockers={null} />);
  expect(await screen.findByText("Connected through the GitHub App")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Connection" })).not.toBeInTheDocument();
});

it("shows previews a read-only provider summary with report history", async () => {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path.endsWith("/git-provider")) return Promise.resolve({ connection_id: 7, repository_id: 11, automatic: false, reporting: true, reports: [{ operation_id: "op-1", reported_state: "", error: "status rejected", next_attempt_at: new Date().toISOString() }] });
    if (path === "/api/git/connections") return Promise.resolve([connection]);
    return Promise.resolve(null);
  });
  wrap(<GitProviderSource workload={workload({ preview_source_id: "w0" })} editable temporary blocked={false} blockers={null} />);
  expect(await screen.findByText(/Previews use the repository/)).toBeInTheDocument();
  expect(await screen.findByText("Team GitLab (GitLab)")).toBeInTheDocument();
  expect(within(screen.getByRole("list", { name: "Recent GitLab reports" })).getByText("status rejected")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disconnect provider" })).not.toBeInTheDocument();
});
