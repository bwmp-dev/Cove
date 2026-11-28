import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { GitDeployKey } from "./GitDeployKey";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => String(error) }));
afterEach(() => vi.clearAllMocks());
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl";
const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakePublicKeyForTestsOnly000000000000000000";
const sshRepository = "ssh://git@github.com/acme/app.git";

function workload(source: Workload["source"], hasToken = false): Workload {
  return { id: "w1", name: "web", node_id: "n1", project_id: "p1", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], manually_stopped: false, deployed: false, compose: "", environment_keys: [], source, has_source_token: hasToken };
}

function mockApi(initial: unknown, github: unknown = null, provider: unknown = null) {
  let key = initial;
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.endsWith("/github")) return Promise.resolve(github);
    if (path.endsWith("/git-provider")) return Promise.resolve(provider);
    if (path.endsWith("/git-deploy-key") && method === "GET") return Promise.resolve(key);
    if (method === "POST") {
      key = { public_key: publicKey, repository: sshRepository, host_key: hostKey };
      return Promise.resolve(key);
    }
    if (method === "DELETE") {
      key = null;
      return Promise.resolve({ ok: true });
    }
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderKey(resource: Workload) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <GitDeployKey workload={resource} blocked={false} blockers={null} />
    </QueryClientProvider>,
  );
}

it("creates a key from an SSH URL and pin and explains switching from HTTPS", async () => {
  const user = userEvent.setup();
  mockApi(null);
  renderKey(workload({ repository: "https://github.com/acme/app.git", branch: "main", compose_path: "compose.yaml" }, true));
  expect(await screen.findByText(/saved access token is deleted/)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Create deploy key" }));
  expect(screen.getByText(/Enter the repository URL/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  await user.type(screen.getByLabelText("SSH repository URL"), sshRepository);
  await user.type(screen.getByLabelText("Host public key"), hostKey);
  await user.click(screen.getByRole("button", { name: "Create deploy key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/git-deploy-key", { repository: sshRepository, host_key: hostKey }));
  expect(await screen.findByLabelText("Public key")).toHaveTextContent(publicKey);
  expect(screen.getByRole("button", { name: "Copy public key" })).toBeInTheDocument();
});

it("blocks key creation while a GitHub App connection manages the source", async () => {
  mockApi(null, { installation_id: 1, repository_id: 2, automatic: true });
  renderKey(workload({ repository: "https://github.com/acme/app.git", branch: "main", compose_path: "compose.yaml" }));
  expect(await screen.findByText(/Disconnect the GitHub App/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create deploy key" })).toBeDisabled();
});

it("blocks key changes while another Git provider manages the source", async () => {
  mockApi({ public_key: publicKey, repository: sshRepository, host_key: hostKey }, null, { connection_id: 1, repository_id: 2, automatic: true, reporting: false, reports: [] });
  renderKey(workload({ repository: sshRepository, branch: "main", compose_path: "compose.yaml", ssh_host_key: hostKey }));
  expect(await screen.findByText(/Disconnect a Git provider/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Rotate key" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Delete key" })).toBeDisabled();
});

it("shows the public key, warns about a source mismatch and deletes after confirmation", async () => {
  const user = userEvent.setup();
  mockApi({ public_key: publicKey, repository: sshRepository, host_key: hostKey });
  renderKey(workload({ repository: "ssh://git@github.com/acme/other.git", branch: "main", compose_path: "compose.yaml", ssh_host_key: hostKey }));
  expect(await screen.findByLabelText("Public key")).toHaveTextContent(publicKey);
  expect(screen.getByText("Deployments will fail")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Delete key" }));
  expect(screen.getByText(/Cove cannot remove the public key from the Git host/)).toBeInTheDocument();
  await user.click(screen.getAllByRole("button", { name: "Delete key" }).at(-1)!);
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/workloads/w1/git-deploy-key"));
  expect(await screen.findByText(/Deploy key deleted/)).toBeInTheDocument();
});

it("rotates with the saved source values", async () => {
  const user = userEvent.setup();
  mockApi({ public_key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOldKey000000000000000000000000000000000000", repository: sshRepository, host_key: hostKey });
  renderKey(workload({ repository: sshRepository, branch: "main", compose_path: "compose.yaml", ssh_host_key: hostKey }));
  await user.click(await screen.findByRole("button", { name: "Rotate key" }));
  const dialog = screen.getByRole("dialog");
  expect(dialog).toHaveTextContent(/stops working immediately/);
  expect(dialog).toHaveTextContent(/webhook keeps working/);
  await user.click(screen.getAllByRole("button", { name: "Rotate key" }).at(-1)!);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/git-deploy-key", { repository: sshRepository, host_key: hostKey }));
  expect(await screen.findByText(/Key rotated/)).toBeInTheDocument();
});
