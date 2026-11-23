import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Workload } from "../api/types";
import { ResourceDeploymentSafety } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const base = {
  id: "w1",
  name: "web",
  node_id: "n1",
  kind: "service",
  compose: "services:\n  web:\n    image: nginx\n",
  source: null,
  environment_keys: [],
  has_source_token: false,
  permissions: ["read", "operate", "edit"],
  manually_stopped: false,
  deployed: true,
  project_id: "p1",
  guarded_deploy: false,
} as Workload;

function node(rollouts: boolean): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, rollouts } };
}

function serve(nodes: Node[]) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "PUT") return Promise.resolve({ id: "w1" });
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderPage(workload: Workload) {
  resource = workload;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceDeploymentSafety />
    </QueryClientProvider>,
  );
}

function choose(strategy: HTMLElement, option: string) {
  fireEvent.click(strategy);
  fireEvent.click(screen.getByRole("option", { name: option }));
}

function savedInput() {
  const call = vi.mocked(request).mock.calls.find(([method]) => method === "PUT");
  return call?.[2] as Record<string, unknown> | undefined;
}

it("saves a canary rollout with replica and HTTP check rows", async () => {
  serve([node(true)]);
  renderPage(base);
  const strategy = await screen.findByLabelText("Replacement strategy");
  await waitFor(() => expect(strategy).toBeEnabled());
  choose(strategy, "Canary");
  fireEvent.change(screen.getByLabelText("Canary traffic"), { target: { value: "20" } });
  fireEvent.click(screen.getByRole("button", { name: "Add replica setting" }));
  fireEvent.change(screen.getByLabelText("Compose service", { selector: "[id$=replica-service]" }), { target: { value: "web" } });
  fireEvent.change(screen.getByLabelText("Replicas"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Add HTTP check" }));
  fireEvent.change(screen.getByLabelText("Compose service", { selector: "[id$=check-service]" }), { target: { value: "web" } });
  fireEvent.change(screen.getByLabelText("Container port"), { target: { value: "8080" } });
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/healthz" } });
  expect(screen.getByText("Parallel replacement is conditional")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(savedInput()).toBeDefined());
  expect(savedInput()).toMatchObject({
    guarded_deploy: false,
    rollout: { strategy: "canary", canary_percent: 20, drain_seconds: 0, timeout_seconds: 120, parallel_volumes: false, replicas: { web: 3 }, http_checks: { web: { port: 8080, path: "/healthz" } } },
  });
});

it("keeps invalid input and does not save until fields are fixed", async () => {
  serve([node(true)]);
  renderPage(base);
  const strategy = await screen.findByLabelText("Replacement strategy");
  await waitFor(() => expect(strategy).toBeEnabled());
  choose(strategy, "Rolling");
  fireEvent.change(screen.getByLabelText("Health timeout"), { target: { value: "2" } });
  fireEvent.click(screen.getByRole("button", { name: "Add replica setting" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Use a whole number of seconds from 5 to 600.")).toBeInTheDocument();
  expect(screen.getByText("Enter the Compose service name.")).toBeInTheDocument();
  expect(screen.getByLabelText("Health timeout")).toHaveAttribute("aria-invalid", "true");
  expect(screen.getByLabelText("Health timeout")).toHaveValue(2);
  expect(savedInput()).toBeUndefined();
});

it("omits unchanged rollout options when only guarded deployment changes", async () => {
  serve([node(true)]);
  renderPage({ ...base, rollout: { strategy: "blue_green", parallel_volumes: true, drain_seconds: 5, timeout_seconds: 60, canary_percent: 10 } });
  fireEvent.click(await screen.findByRole("switch", { name: "Enable guarded deployment" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(savedInput()).toBeDefined());
  expect(savedInput()?.guarded_deploy).toBe(true);
  expect(savedInput()).not.toHaveProperty("rollout");
});

it("turns rollouts off explicitly and keeps the saved tuning", async () => {
  serve([node(true)]);
  const rollout = { strategy: "rolling" as const, parallel_volumes: false, drain_seconds: 5, timeout_seconds: 60, canary_percent: 10 };
  renderPage({ ...base, rollout });
  const strategy = await screen.findByLabelText("Replacement strategy");
  await waitFor(() => expect(strategy).toBeEnabled());
  choose(strategy, "Off");
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(savedInput()).toBeDefined());
  expect(savedInput()?.rollout).toEqual({ ...rollout, strategy: "disabled" });
});

it("disables strategies on workers without rollout support", async () => {
  serve([node(false)]);
  renderPage(base);
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByLabelText("Replacement strategy")).toBeDisabled();
  expect(screen.getByRole("switch", { name: "Enable guarded deployment" })).toBeEnabled();
});

it("keeps disabled rollout tuning without warning that deploys are rejected on an unsupported worker", async () => {
  serve([node(false)]);
  const tuning = { parallel_volumes: true, drain_seconds: 15, timeout_seconds: 90, canary_percent: 20 };
  renderPage({ ...base, rollout: { strategy: "disabled", ...tuning } });
  expect(await screen.findByText(/Guarded deployment and other operations still work\./)).toBeInTheDocument();
  expect(screen.queryByText(/deploys and rollbacks are rejected/)).not.toBeInTheDocument();
  expect(screen.getByLabelText("Replacement strategy")).toBeDisabled();
  fireEvent.click(screen.getByRole("switch", { name: "Enable guarded deployment" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(savedInput()).toBeDefined());
  expect(savedInput()).not.toHaveProperty("rollout");
});

it("warns that deploys are rejected when an enabled strategy is saved on an unsupported worker", async () => {
  serve([node(false)]);
  renderPage({ ...base, rollout: { strategy: "rolling", parallel_volumes: false, drain_seconds: 5, timeout_seconds: 60, canary_percent: 10 } });
  expect(await screen.findByText(/saved replacement strategy, so deploys and rollbacks are rejected/)).toBeInTheDocument();
  expect(screen.getByLabelText("Replacement strategy")).toBeDisabled();
  expect(screen.getByLabelText("Health timeout")).toBeDisabled();
});

it("hides strategies for managed previews and shows read-only settings without edit access", async () => {
  serve([node(true)]);
  renderPage({ ...base, preview_source_id: "w0" });
  expect(await screen.findByText(/Managed previews always use Compose replacement/)).toBeInTheDocument();
  expect(screen.queryByLabelText("Replacement strategy")).not.toBeInTheDocument();
});

it("shows saved settings read-only without edit access", async () => {
  serve([node(true)]);
  renderPage({ ...base, permissions: ["read"], rollout: { strategy: "canary", replicas: { web: 2 }, parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 30 } });
  expect(await screen.findByText("View only")).toBeInTheDocument();
  expect(screen.getByLabelText("Canary traffic")).toHaveValue(30);
  expect(screen.getByLabelText("Canary traffic")).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Add replica setting" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
});
