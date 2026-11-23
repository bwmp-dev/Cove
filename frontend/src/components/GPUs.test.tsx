import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { GPUInventory } from "../api/gpus";
import type { Node, Workload } from "../api/types";
import { NodeGPUs, ResourceGPUs, type GPUSaveState } from "./GPUs";

const guard = vi.hoisted(() => ({ beforeUnload: false }));
vi.mock("@tanstack/react-router", () => ({
  useBlocker: (options: { enableBeforeUnload: boolean }) => {
    guard.beforeUnload = options.enableBeforeUnload;
    return { status: "idle" };
  },
}));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

afterEach(() => vi.clearAllMocks());

const uuid = "GPU-11111111-2222-3333-4444-555555555555";
const second = "GPU-aaaaaaaa-2222-3333-4444-555555555555";
const stale = "vendor.com/gpu=retired";
const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, gpu_assignments: true };
const node: Node = { id: "n1", name: "gpu-host", state: "ready", mode: "ssh", address: "10.0.0.12", port: 22, last_checked_at: null, capabilities };

const inventory: GPUInventory = {
  checked_at: "2026-11-22T10:00:00Z",
  devices: [
    { id: uuid, name: "NVIDIA L4", driver: "nvidia", memory_mb: 23034, assignable: true, notice: "" },
    { id: second, name: "NVIDIA L4", driver: "nvidia", memory_mb: 23034, assignable: true, notice: "" },
    { id: "vendor.com/gpu=1", name: "vendor.com/gpu=1", driver: "cdi", memory_mb: null, assignable: false, notice: "The CDI spec is writable by non-root users." },
  ],
  limited: false,
  notices: [],
};

function client() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

describe("NodeGPUs", () => {
  function renderNode(target: Node = node) {
    return render(<QueryClientProvider client={client()}><NodeGPUs node={target} /></QueryClientProvider>);
  }

  it("inspects only when asked and keeps unknown memory unknown", async () => {
    vi.mocked(request).mockResolvedValue(inventory);
    renderNode();
    expect(screen.getByText("Not inspected yet.")).toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    const list = await screen.findByRole("list", { name: "Observed GPUs" });
    expect(request).toHaveBeenCalledWith("GET", "/api/nodes/n1/gpus", undefined, expect.anything());
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    expect(within(list).getAllByText("Assignable")).toHaveLength(2);
    expect(within(list).getByText("Not assignable")).toBeInTheDocument();
    expect(within(list).getByText("unknown")).toBeInTheDocument();
    expect(within(list).getByText("The CDI spec is writable by non-root users.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Inspect again" })).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("does not claim there is no hardware when nothing is observed, and shows clipping and notices", async () => {
    vi.mocked(request).mockResolvedValue({ ...inventory, devices: [], limited: true, notices: ["nvidia-smi is not installed."] });
    renderNode();
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    expect(await screen.findByText(/does not mean the host has no GPU hardware/)).toBeInTheDocument();
    expect(screen.getByText("Inventory clipped")).toBeInTheDocument();
    expect(screen.getByText("nvidia-smi is not installed.")).toBeInTheDocument();
  });

  it.each([
    [409, "The worker cannot inspect GPUs yet"],
    [502, "GPU inspection failed on the node"],
    [503, "Cove could not read the placement"],
  ])("explains a %i response", async (status, title) => {
    vi.mocked(request).mockRejectedValue(new ApiError(status, "server detail"));
    renderNode();
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.getByText("server detail")).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("blocks inspection on workers without GPU support or that are not ready", () => {
    const { unmount } = renderNode({ ...node, capabilities: { ...capabilities, gpu_assignments: undefined } });
    expect(screen.getByText(/has not reported GPU support/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Inspect GPUs" })).toBeDisabled();
    unmount();
    renderNode({ ...node, state: "unreachable" });
    expect(screen.getByText(/GPU inspection needs a ready worker/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Inspect GPUs" })).toBeDisabled();
  });
});

describe("ResourceGPUs", () => {
  const workload: Workload = {
    id: "w1",
    name: "trainer",
    node_id: "n1",
    project_id: "p1",
    compose: "services:\n  web:\n    image: nginx\n  trainer:\n    image: cuda\n",
    environment_keys: [],
    source: null,
    has_source_token: false,
    manually_stopped: false,
    deployed: true,
    permissions: ["read", "operate", "edit"],
    limits: { web: { cpus: 1.5, memory_mb: 512 }, trainer: { cpus: 4, memory_mb: 8192, gpus: [uuid, stale] } },
  };

  function saveState(overrides: Partial<GPUSaveState> = {}): GPUSaveState {
    return { pending: false, blocked: false, saved: false, error: null, reset: vi.fn(), ...overrides };
  }

  function renderResource(target: Workload = workload, onSave = vi.fn(), targetNode: Node | undefined = node) {
    render(
      <QueryClientProvider client={client()}>
        <ResourceGPUs workload={target} node={targetNode} editable={target.permissions.includes("edit")} save={saveState()} blockers={null} onSave={onSave} />
      </QueryClientProvider>,
    );
    return onSave;
  }

  it("shows readers the saved settings without inspecting", () => {
    renderResource({ ...workload, permissions: ["read"] });
    const saved = screen.getByRole("list", { name: "Saved GPU settings" });
    expect(within(saved).getByText("trainer")).toBeInTheDocument();
    expect(within(saved).getByText(stale)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Inspect/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save GPU assignments" })).not.toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
  });

  it("keeps saved devices before inspection and flags stale ones after it", async () => {
    vi.mocked(request).mockResolvedValue(inventory);
    renderResource();
    expect(request).not.toHaveBeenCalled();
    expect(screen.getAllByText(/Inspect GPUs to check that it is still available/)).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    expect(await screen.findByText(/Not observed in the latest inspection/)).toBeInTheDocument();
    expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/gpus", undefined, expect.anything());
    const devices = screen.getByRole("group", { name: /Devices for trainer/ });
    expect(within(devices).getByRole("checkbox", { name: new RegExp(stale) })).toBeChecked();
    expect(within(devices).getByRole("checkbox", { name: /vendor.com\/gpu=1/ })).toBeDisabled();
    expect(within(devices).getByRole("checkbox", { name: new RegExp(second) })).toBeEnabled();
  });

  it("saves a selection while preserving CPU and memory", async () => {
    vi.mocked(request).mockResolvedValue(inventory);
    const onSave = renderResource();
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    const devices = await screen.findByRole("group", { name: /Devices for trainer/ });
    const staleBox = within(devices).getByRole("checkbox", { name: new RegExp(stale) });
    fireEvent.click(staleBox);
    expect(staleBox).not.toBeChecked();
    expect(staleBox).toBeEnabled();
    expect(within(devices).getByText(/Select it again to keep it/)).toBeInTheDocument();
    fireEvent.click(within(devices).getByRole("checkbox", { name: new RegExp(second) }));
    expect(guard.beforeUnload).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Save GPU assignments" }));
    expect(onSave).toHaveBeenCalledWith({ web: { cpus: 1.5, memory_mb: 512 }, trainer: { cpus: 4, memory_mb: 8192, gpus: [uuid, second] } });
  });

  it("serializes No standard GPU requests as [] and Inherit Compose as omitted", () => {
    const onSave = renderResource();
    fireEvent.click(screen.getByRole("radio", { name: /No standard GPU requests/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save GPU assignments" }));
    expect(onSave).toHaveBeenLastCalledWith({ web: { cpus: 1.5, memory_mb: 512 }, trainer: { cpus: 4, memory_mb: 8192, gpus: [] } });
    fireEvent.click(screen.getByRole("radio", { name: /Inherit Compose/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save GPU assignments" }));
    expect(onSave).toHaveBeenLastCalledWith({ web: { cpus: 1.5, memory_mb: 512 }, trainer: { cpus: 4, memory_mb: 8192 } });
  });

  it("focuses the first invalid field instead of saving", async () => {
    const onSave = renderResource({ ...workload, limits: {} });
    expect(screen.getByText("Every service inherits GPU requests from Compose.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add service GPU setting" }));
    const service = screen.getByLabelText("Compose service");
    fireEvent.change(service, { target: { value: "missing" } });
    fireEvent.click(screen.getByRole("button", { name: "Save GPU assignments" }));
    expect(onSave).not.toHaveBeenCalled();
    expect(screen.getByText("This service is not in the saved Compose file.")).toBeInTheDocument();
    expect(screen.getByText(/Select at least one device/)).toBeInTheDocument();
    await waitFor(() => expect(service).toHaveFocus());
  });

  it("limits a service to 16 devices", async () => {
    const devices = Array.from({ length: 17 }, (_, index) => ({ id: `vendor.com/gpu=${index}`, name: `GPU ${index}`, driver: "cdi" as const, memory_mb: null, assignable: true, notice: "" }));
    vi.mocked(request).mockResolvedValue({ ...inventory, devices });
    renderResource({ ...workload, limits: { trainer: { cpus: 0, memory_mb: 0, gpus: devices.slice(0, 16).map((device) => device.id) } } });
    fireEvent.click(screen.getByRole("button", { name: "Inspect GPUs" }));
    const group = await screen.findByRole("group", { name: /Devices for trainer/ });
    expect(within(group).getByRole("checkbox", { name: /vendor.com\/gpu=16/ })).toBeDisabled();
    expect(screen.getByText(/limit of 16 devices/)).toBeInTheDocument();
  });

  it("warns that old workers cannot deploy explicit settings and does not inspect them", () => {
    renderResource(workload, vi.fn(), { ...node, capabilities: { ...capabilities, gpu_assignments: false } });
    expect(screen.getByText("This worker cannot deploy GPU settings")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Inspect GPUs" })).toBeDisabled();
    expect(screen.queryByText(/Not inspected yet/)).not.toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
  });
});
