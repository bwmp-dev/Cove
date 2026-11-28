import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { WorkloadSummary } from "../api/types";
import { DeploymentPlan } from "./DeploymentPlan";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const resources: WorkloadSummary[] = [
  { id: "migrate", name: "migrate", kind: "job", node_id: "n1", node: "node-a", project_id: "p1", permissions: ["read", "operate"] },
  { id: "web", name: "web", kind: "service", node_id: "n1", node: "node-a", project_id: "p1", permissions: ["read", "operate"] },
];

function mockApi() {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/projects/p1/deployment-sequence") return Promise.resolve({ workload_ids: ["migrate", "web"], automatic: false });
    if (method === "POST" && path === "/api/projects/p1/deployments") return Promise.resolve({ operation_ids: ["o1", "o2"] });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

async function openPlan() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><DeploymentPlan projectId="p1" resources={resources} /></QueryClientProvider>);
  const open = await screen.findByRole("button", { name: "Deploy in order" });
  await waitFor(() => expect(open).toBeEnabled());
  fireEvent.click(open);
  return screen.findByRole("dialog", { name: "Deploy resources in order" });
}

it("sends the reason with every deployment the sequence queues", async () => {
  mockApi();
  const dialog = await openPlan();
  fireEvent.change(within(dialog).getByLabelText(/Reason/), { target: { value: "Release 4.2" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy selected resources" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/projects/p1/deployments", { workload_ids: ["migrate", "web"], reason: "Release 4.2" }));
  expect(await within(dialog).findByText("Deployment plan queued.")).toBeInTheDocument();
});

it("deploys without a reason when none is given", async () => {
  mockApi();
  const dialog = await openPlan();
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy selected resources" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/projects/p1/deployments", { workload_ids: ["migrate", "web"] }));
});

it("blocks a reason longer than the API accepts", async () => {
  mockApi();
  const dialog = await openPlan();
  fireEvent.change(within(dialog).getByLabelText(/Reason/), { target: { value: "é".repeat(501) } });
  expect(within(dialog).getByText("Shorten the reason to 1000 bytes (currently 1002).")).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Deploy selected resources" })).toBeDisabled();
});
