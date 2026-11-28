import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { ResourceExec } from "./ResourceExec";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
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

const workload = { id: "w".repeat(32), name: "shop", node_id: "n1", compose: "services:\n  web:\n    image: nginx\n  worker:\n    image: app\n", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"] } as Workload;
const container = { id: "abcdef123456", name: "cove-w-web-1", image: "nginx", state: "running", status: "Up", ports: "" };

function mockApi(exec: boolean | undefined, run: () => Promise<unknown>, containers: unknown[] = [container]) {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([{ id: "n1", name: "edge", capabilities: exec === undefined ? null : { exec } }]);
    if (path.endsWith("/runtime")) return Promise.resolve({ containers });
    return run();
  });
}

function renderExec(operationActive = false, target: Workload = workload) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ResourceExec workload={target} operationActive={operationActive} />
    </QueryClientProvider>,
  );
}

const execCalls = () => vi.mocked(request).mock.calls.filter(([method]) => method === "POST");

it("runs the exact argument list only after confirmation and shows the bounded result", async () => {
  const user = userEvent.setup();
  mockApi(true, () => Promise.resolve({ output: "migrated\n", exit_code: 0, truncated: true, timed_out: false }));
  renderExec();
  await screen.findByRole("option", { name: /cove-w-web-1/, hidden: true });
  expect(execCalls()).toHaveLength(0);

  await user.click(screen.getByRole("combobox", { name: "Service" }));
  await user.click(screen.getByRole("option", { name: "web" }));
  await user.type(screen.getByLabelText("Program"), "sh");
  await user.click(screen.getByRole("button", { name: "Add argument" }));
  await user.type(screen.getByLabelText("Argument 1"), "-c");
  await user.click(screen.getByRole("button", { name: "Add argument" }));
  await user.type(screen.getByLabelText("Argument 2"), "echo $HOME | wc");
  expect(screen.getAllByText('["sh","-c","echo $HOME | wc"]').length).toBeGreaterThan(0);

  await user.click(screen.getByRole("button", { name: "Run command" }));
  const dialog = screen.getByRole("dialog");
  expect(execCalls()).toHaveLength(0);
  await user.click(within(dialog).getByRole("button", { name: "Run command" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", `/api/workloads/${workload.id}/exec`, { service: "web", command: ["sh", "-c", "echo $HOME | wc"], timeout_seconds: 10 }));
  expect(await screen.findByRole("region", { name: "Command output" })).toHaveTextContent("migrated");
  expect(screen.getByText("Exit code 0")).toBeInTheDocument();
  expect(screen.getByText("Output truncated at 64 KiB")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Clear output" }));
  expect(screen.queryByRole("region", { name: "Command output" })).not.toBeInTheDocument();
});

it("validates the service and program before asking to run", async () => {
  const user = userEvent.setup();
  mockApi(true, () => Promise.reject(new Error("unexpected")));
  renderExec();
  await user.click(screen.getByRole("button", { name: "Run command" }));
  expect(screen.getByText("Choose the Compose service to run the command in.")).toBeInTheDocument();
  expect(screen.getByText("Enter the program to run as the first argument.")).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(execCalls()).toHaveLength(0);
});

it("blocks workers that do not report exec support", async () => {
  mockApi(false, () => Promise.reject(new Error("unexpected")));
  renderExec();
  expect(await screen.findByText("This node's worker cannot run commands")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Run command" })).toBeDisabled();
});

it("shows the control plane's refusal", async () => {
  const user = userEvent.setup();
  mockApi(undefined, () => Promise.reject(new Error("Upgrade and verify the worker, and wait for active operations before executing a command")));
  renderExec(true);
  expect(screen.getByText("An operation is in progress")).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Service" }));
  await user.click(screen.getByRole("option", { name: "worker" }));
  await user.type(screen.getByLabelText("Program"), "date");
  await user.click(screen.getByRole("button", { name: "Run command" }));
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Run command" }));
  expect(await screen.findByText(/Upgrade and verify the worker/)).toBeInTheDocument();
});

it("warns that a command may still be running after the wait timeout", async () => {
  const user = userEvent.setup();
  mockApi(true, () => Promise.resolve({ output: "", exit_code: -1, truncated: false, timed_out: true }));
  renderExec();
  await user.click(screen.getByRole("combobox", { name: "Service" }));
  await user.click(screen.getByRole("option", { name: "web" }));
  await user.type(screen.getByLabelText("Program"), "sleep");
  await user.click(screen.getByRole("button", { name: "Run command" }));
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Run command" }));
  expect(await screen.findByText("Wait timeout after 10 seconds")).toBeInTheDocument();
  expect(screen.getByText("The command may still be running in the container. Inspect it before retrying.")).toBeInTheDocument();
  expect(screen.queryByText(/Exit code/)).not.toBeInTheDocument();
  expect(execCalls()).toHaveLength(1);
});

it("offers the services running containers report for a Git workload and lists only that service's replicas", async () => {
  const user = userEvent.setup();
  const git: Workload = { ...workload, compose: "" };
  const replicas = [
    { ...container, id: "aaaaaaaaaaaa", name: "cove-w-api-1", service: "api" },
    { ...container, id: "bbbbbbbbbbbb", name: "cove-w-api-2", service: "api" },
    { ...container, id: "cccccccccccc", name: "cove-w-queue-1", service: "queue" },
  ];
  mockApi(true, () => Promise.resolve({ output: "ok", exit_code: 0, truncated: false, timed_out: false }), replicas);
  renderExec(false, git);

  await user.click(await screen.findByRole("combobox", { name: "Service" }));
  expect(screen.getByRole("option", { name: "queue" })).toBeInTheDocument();
  await user.click(screen.getByRole("option", { name: "api" }));
  await user.click(screen.getByRole("combobox", { name: /Container/ }));
  expect(screen.getByRole("option", { name: /cove-w-api-1/ })).toBeInTheDocument();
  expect(screen.queryByRole("option", { name: /cove-w-queue-1/ })).not.toBeInTheDocument();
  await user.click(screen.getByRole("option", { name: /cove-w-api-2/ }));
  await user.type(screen.getByLabelText("Program"), "date");
  await user.click(screen.getByRole("button", { name: "Run command" }));
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Run command" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", `/api/workloads/${git.id}/exec`, { service: "api", container: "bbbbbbbbbbbb", command: ["date"], timeout_seconds: 10 }));
});
