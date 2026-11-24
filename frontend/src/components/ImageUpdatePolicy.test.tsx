import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ImageUpdatePolicyStatus } from "../api/image-updates";
import type { Workload } from "../api/types";
import { ImageUpdatePolicy } from "./ImageUpdatePolicy";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (original) => ({ ...(await original<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to, params }: { children: ReactNode; to: string; params?: { operationId?: string } }) => <a href={params?.operationId ? `/operations/${params.operationId}` : to}>{children}</a>,
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no worker, registry or notification connection is contacted.
const path = "/api/workloads/w1/image-update-policy";
const service = { id: "w1", name: "web", node_id: "n1", kind: "service", permissions: ["read", "operate", "edit"], source: null, compose: "" } as unknown as Workload;
const services = [
  { service: "app", image: "registry.example.test/team/app:1.2.0", mutable: true },
  { service: "db", image: "postgres@sha256:0123456789abcdef", mutable: false },
];
const rule = { service: "app", range: "^1.2.0" };

function status(extra: Partial<ImageUpdatePolicyStatus> = {}): ImageUpdatePolicyStatus {
  return {
    configuration: null,
    services,
    eligible: true,
    unavailable_reason: "",
    worker_supported: true,
    node_ready: true,
    active_operation_id: null,
    last_check_id: null,
    last_operation_id: null,
    last_error: "",
    checked_at: null,
    stale: false,
    updates: null,
    ...extra,
  };
}

function saved(configuration: Partial<NonNullable<ImageUpdatePolicyStatus["configuration"]>> = {}, extra: Partial<ImageUpdatePolicyStatus> = {}) {
  return status({ configuration: { enabled: true, mode: "notify", rules: [rule], window_start: "", window_end: "", ...configuration }, ...extra });
}

type Handler = (method: string, body?: unknown) => unknown;

function serve(handler: Handler) {
  vi.mocked(request).mockImplementation((method: string, requested: string, body?: unknown) => {
    if (requested !== path) return Promise.reject(new Error(`unexpected request ${method} ${requested}`));
    try {
      return Promise.resolve(handler(method, body));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function renderPolicy(workload: Workload = service) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ImageUpdatePolicy workload={workload} />
    </QueryClientProvider>,
  );
  return client;
}

const calls = (method: string) => vi.mocked(request).mock.calls.filter(([callMethod]) => callMethod === method);

async function chooseService(index: number, name: string) {
  fireEvent.click(await screen.findByRole("combobox", { name: `Service ${index}` }));
  fireEvent.click(screen.getByRole("option", { name }));
}

it("reads the policy with GET and creates an off, notify-only policy with exactly five fields", async () => {
  let current = status();
  serve((method, body) => {
    if (method === "PUT") {
      current = status({ configuration: body as ImageUpdatePolicyStatus["configuration"] });
      return current;
    }
    return current;
  });
  renderPolicy();
  expect(await screen.findByRole("switch", { name: "Enable image update policy" })).not.toBeChecked();
  expect(screen.getByRole("combobox", { name: "When an update is found" })).toHaveTextContent("Notify only");
  expect(screen.getByLabelText("Starts")).toHaveValue("");
  expect(screen.getByLabelText("Ends")).toHaveValue("");
  expect(screen.getByLabelText("Version range 1")).toHaveAttribute("maxlength", "200");
  expect(request).toHaveBeenCalledWith("GET", path, undefined, expect.any(AbortSignal));

  await chooseService(1, "app");
  fireEvent.click(screen.getByRole("button", { name: "Create policy" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", path, { enabled: false, mode: "notify", rules: [{ service: "app", range: "" }], window_start: "", window_end: "" }]);
  expect(await screen.findByText("Policy created")).toBeInTheDocument();
});

// The real control plane answers the refetch after a save later than the saved data re-renders the form, so the
// confirmation must survive the form being replaced by its saved state.
it("keeps the created and saved notices while the refetch after a save is still pending", async () => {
  let current = status();
  let release: () => void = () => undefined;
  serve((method, body) => {
    if (method === "PUT") {
      current = saved(body as Partial<NonNullable<ImageUpdatePolicyStatus["configuration"]>>, { active_operation_id: null });
      return current;
    }
    if (current.configuration) {
      const reply = current;
      return new Promise((resolve) => {
        release = () => resolve(reply);
      });
    }
    return current;
  });
  renderPolicy();
  await chooseService(1, "app");
  fireEvent.click(screen.getByRole("button", { name: "Create policy" }));
  expect(await screen.findByRole("button", { name: "Save policy" })).toBeDisabled();
  release();
  expect(await screen.findByText("Policy created")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Service 1" })).toHaveTextContent("app");

  fireEvent.change(screen.getByLabelText("Version range 1"), { target: { value: "^1.2.0" } });
  expect(screen.queryByText("Policy created")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(2));
  await waitFor(() => expect(screen.getByLabelText("Version range 1")).toHaveValue("^1.2.0"));
  release();
  expect(await screen.findByText("Policy saved")).toBeInTheDocument();

  for (const [index, enabled] of [[2, true], [3, false]] as const) {
    fireEvent.click(screen.getByRole("switch", { name: "Enable image update policy" }));
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(calls("PUT")).toHaveLength(index + 1));
    expect(calls("PUT")[index]?.[2]).toEqual({ enabled, mode: "notify", rules: [{ service: "app", range: "^1.2.0" }], window_start: "", window_end: "" });
    await waitFor(() => expect(screen.getByRole("switch", { name: "Enable image update policy", checked: enabled })).toBeInTheDocument());
    release();
    expect(await screen.findByText("Policy saved")).toBeInTheDocument();
  }
});

it("rejects blank and duplicate services and half-set windows before sending", async () => {
  serve(() => status());
  renderPolicy();
  fireEvent.click(await screen.findByRole("button", { name: "Create policy" }));
  expect(await screen.findByText("Choose a service.")).toBeInTheDocument();

  await chooseService(1, "app");
  fireEvent.click(screen.getByRole("button", { name: "Add service" }));
  await chooseService(2, "app");
  fireEvent.change(screen.getByLabelText("Starts"), { target: { value: "23:00" } });
  fireEvent.click(screen.getByRole("button", { name: "Create policy" }));
  expect(await screen.findByText("This service is already listed.")).toBeInTheDocument();
  expect(screen.getByText("Set both times, or clear both for any time of day.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Ends"), { target: { value: "23:00" } });
  expect(await screen.findByText("Use two different times, or clear both for any time of day.")).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);
});

it("asks before turning on automatic deploys and sends nothing when cancelled", async () => {
  serve((method, body) => (method === "PUT" ? status({ configuration: body as ImageUpdatePolicyStatus["configuration"] }) : saved({ enabled: false })));
  renderPolicy();
  fireEvent.click(await screen.findByRole("switch", { name: "Enable image update policy" }));
  fireEvent.click(screen.getByRole("combobox", { name: "When an update is found" }));
  fireEvent.click(screen.getByRole("option", { name: "Deploy automatically" }));
  expect(screen.getByText(/pins every service to an exact image/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Starts"), { target: { value: "23:00" } });
  fireEvent.change(screen.getByLabelText("Ends"), { target: { value: "02:00" } });
  expect(screen.getByText(/Currently: 23:00–02:00 UTC, continuing past midnight\./)).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  let dialog = screen.getByRole("dialog", { name: "Turn on automatic image updates?" });
  expect(dialog).toHaveTextContent("Saved Compose and image tags are not changed");
  expect(dialog).toHaveTextContent("23:00–02:00 UTC, continuing past midnight");
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(calls("PUT")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  dialog = screen.getByRole("dialog", { name: "Turn on automatic image updates?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Turn on automatic updates" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { enabled: true, mode: "auto", rules: [rule], window_start: "23:00", window_end: "02:00" }));
  expect(await screen.findByText("Policy saved")).toBeInTheDocument();
});

it("does not ask again when an automatic policy stays automatic, and saves a window ending at midnight", async () => {
  serve((method, body) => (method === "PUT" ? status({ configuration: body as ImageUpdatePolicyStatus["configuration"] }) : saved({ mode: "auto" })));
  renderPolicy();
  fireEvent.change(await screen.findByLabelText("Starts"), { target: { value: "22:00" } });
  fireEvent.change(screen.getByLabelText("Ends"), { target: { value: "00:00" } });
  expect(screen.getByText(/Currently: 22:00–00:00 UTC\./)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { enabled: true, mode: "auto", rules: [rule], window_start: "22:00", window_end: "00:00" }));
});

it.each([
  [400, "invalid semantic version range"],
  [409, "wait for the active operation before changing image updates"],
])("keeps the draft when a save is refused with %i", async (code, message) => {
  serve((method) => {
    if (method === "PUT") throw new ApiError(code, message);
    return saved();
  });
  renderPolicy();
  fireEvent.change(await screen.findByLabelText("Version range 1"), { target: { value: ">=2 <" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText(message)).toBeInTheDocument();
  expect(screen.getByText("Your changes are kept.")).toBeInTheDocument();
  expect(screen.queryByText("Policy saved")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Version range 1")).toHaveValue(">=2 <");
  expect(screen.getByRole("combobox", { name: "Service 1" })).toHaveTextContent("app");
  expect(screen.getByRole("button", { name: "Save policy" })).toBeEnabled();
});

it("keeps a saved rule whose service left the Compose file and sends it unchanged", async () => {
  serve((method, body) => (method === "PUT" ? status({ configuration: body as ImageUpdatePolicyStatus["configuration"] }) : saved({ enabled: false, rules: [{ service: "worker", range: "~2.1" }] })));
  renderPolicy();
  expect(await screen.findByRole("combobox", { name: "Service 1" })).toHaveTextContent("worker (not in Compose)");
  expect(screen.getByText("worker is not in the saved Compose file. Remove this rule or add the service back.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Add service" }));
  await chooseService(2, "db (no plain tag)");
  expect(screen.getByText(/db has no plain image tag/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("PUT", path, { enabled: false, mode: "notify", rules: [{ service: "worker", range: "~2.1" }, { service: "db", range: "" }], window_start: "", window_end: "" }),
  );
});

it("shows candidates from current to target with check and deploy links, without claiming the deploy finished", async () => {
  serve(() =>
    saved(
      { mode: "auto" },
      {
        last_check_id: "op-check",
        last_operation_id: "op-deploy",
        checked_at: "2026-11-23T09:00:00Z",
        updates: { state: "available", candidates: [{ service: "app", current: "registry.example.test/team/app:1.2.0", target: "registry.example.test/team/app:1.3.0@sha256:abc" }] },
      },
    ),
  );
  renderPolicy();
  const list = await screen.findByRole("list", { name: "Update candidates" });
  expect(list).toHaveTextContent("Currentregistry.example.test/team/app:1.2.0");
  expect(list).toHaveTextContent("Targetregistry.example.test/team/app:1.3.0@sha256:abc");
  expect(screen.getByText("Update available")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View check" })).toHaveAttribute("href", "/operations/op-check");
  expect(screen.getByRole("link", { name: "View operation" })).toHaveAttribute("href", "/operations/op-deploy");
  expect(screen.getByText("Queued by this policy")).toBeInTheDocument();
  expect(screen.getByText(/A queued deploy request has not deployed anything until its operation succeeds/)).toBeInTheDocument();
  expect(screen.queryByText(/deployed successfully|update complete/i)).not.toBeInTheDocument();
});

it("shows a stale result as a previous result, not as confirmed availability", async () => {
  serve(() =>
    saved({}, { stale: true, last_check_id: "op-check", checked_at: "2026-11-23T09:00:00Z", updates: { state: "available", candidates: [{ service: "app", current: "registry.example.test/team/app:1.2.0", target: "registry.example.test/team/app:1.3.0@sha256:abc" }] } }),
  );
  renderPolicy();
  expect(await screen.findByText("Result is from before the latest change")).toBeInTheDocument();
  expect(screen.getByText("Update available (previous result)")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Previous update candidates" })).toHaveTextContent("app:1.3.0@sha256:abc");
  expect(screen.queryByRole("list", { name: "Update candidates" })).not.toBeInTheDocument();
  expect(screen.queryByText(/Each candidate is/)).not.toBeInTheDocument();
});

it("treats a missing stale flag as stale", async () => {
  serve(() => ({ ...saved({}, { updates: { state: "current", candidates: [] } }), stale: undefined }));
  renderPolicy();
  expect(await screen.findByText("Current (previous result)")).toBeInTheDocument();
});

it("shows a recorded hold such as the maintenance window as a policy message", async () => {
  serve(() => saved({ mode: "auto", window_start: "10:00", window_end: "11:00" }, { last_error: "Waiting for the image update UTC maintenance window." }));
  renderPolicy();
  expect(await screen.findByText("Latest policy message")).toBeInTheDocument();
  expect(screen.getByText("Waiting for the image update UTC maintenance window.")).toBeInTheDocument();
  expect(screen.getByText("10:00–11:00 UTC")).toBeInTheDocument();
});

it("keeps unknown and unrecognised results unknown and marks incomplete candidates", async () => {
  serve(() =>
    saved({}, { last_check_id: "op-check", checked_at: "2026-11-23T09:00:00Z", updates: { state: "partially-known", message: "A candidate image could not be verified for the deployed platform.", candidates: [{ service: "app", current: "registry.example.test/team/app:1.2.0", target: "" }] } }),
  );
  renderPolicy();
  expect(await screen.findByText("Update status unknown")).toBeInTheDocument();
  expect(screen.getByText("Unknown")).toBeInTheDocument();
  expect(screen.getByText("A candidate image could not be verified for the deployed platform.")).toBeInTheDocument();
  expect(screen.getByText("Nothing is notified or deployed from an unknown result.")).toBeInTheDocument();
  expect(within(screen.getByRole("list", { name: "Update candidates" })).getByText("Not reported")).toBeInTheDocument();
  expect(screen.queryByText("Update available")).not.toBeInTheDocument();
});

it("blocks turning the policy on for an unknown or unsupported worker, but lets a saved policy be turned off", async () => {
  serve((method, body) => (method === "PUT" ? status({ configuration: body as ImageUpdatePolicyStatus["configuration"], worker_supported: false }) : saved({}, { worker_supported: undefined as unknown as boolean })));
  renderPolicy();
  const toggle = await screen.findByRole("switch", { name: "Enable image update policy" });
  expect(screen.getByText(/The worker does not report image update support\. Upgrade and verify the worker first\. You can still turn the policy off\./)).toBeInTheDocument();
  expect(toggle).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(toggle);
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { enabled: false, mode: "notify", rules: [rule], window_start: "", window_end: "" }));
});

it("explains why a Git-source resource cannot use a policy and offers no form", async () => {
  serve(() => status({ eligible: false, unavailable_reason: "Registry webhooks redeploy saved image-only Compose files, without Git sources or build sections." }));
  renderPolicy();
  expect(await screen.findByText(/without Git sources or build sections/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create policy" })).not.toBeInTheDocument();
});

it("never loads a policy for jobs or managed previews", () => {
  serve(() => status());
  renderPolicy({ ...service, kind: "job" });
  expect(screen.getByText(/Jobs are never updated automatically/)).toBeInTheDocument();
  renderPolicy({ ...service, preview_source_id: "w0" });
  expect(screen.getByText("Managed previews do not use image update policies.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("is read-only without edit access", async () => {
  serve(() => saved({ rules: [rule, { service: "worker", range: "" }] }));
  renderPolicy({ ...service, permissions: ["read", "operate"] });
  expect(await screen.findByText("Changing the image update policy needs edit access. Settings are read-only.")).toBeInTheDocument();
  const list = screen.getByRole("list", { name: "Policy services" });
  expect(list).toHaveTextContent("app^1.2.0");
  expect(list).toHaveTextContent("workerConfigured tag");
  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save policy" })).not.toBeInTheDocument();
});

it("pauses saving while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => saved());
  renderPolicy();
  fireEvent.change(await screen.findByLabelText("Version range 1"), { target: { value: "^1.3.0" } });
  expect(screen.getByText("You are offline")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save policy" })).toBeDisabled();
});

it("refuses changes during an active operation and links to it", async () => {
  serve(() => saved({}, { active_operation_id: "op-run" }));
  renderPolicy();
  expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
  expect(screen.getByText(/including turning it off, are refused until it finishes/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View operation" })).toHaveAttribute("href", "/operations/op-run");
  fireEvent.click(screen.getByRole("switch", { name: "Enable image update policy" }));
  expect(screen.getByRole("button", { name: "Save policy" })).toBeDisabled();
});

it("shows a failed first load with Retry", async () => {
  serve(() => {
    throw new ApiError(503, "image update policy unavailable");
  });
  renderPolicy();
  expect(await screen.findByText("Could not load the image update policy")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
});

it("does not trust cached readiness after a failed refetch", async () => {
  let fail = false;
  serve(() => {
    if (fail) throw new ApiError(503, "image update policy unavailable");
    return saved({ enabled: false });
  });
  const client = renderPolicy();
  const toggle = await screen.findByRole("switch", { name: "Enable image update policy" });
  fail = true;
  await client.refetchQueries({ queryKey: ["image-update-policy", "w1"] });
  expect(await screen.findByText("Could not refresh the image update policy")).toBeInTheDocument();
  expect(screen.getByText(/Showing the last status that loaded\. Saving is paused/)).toBeInTheDocument();
  fireEvent.click(toggle);
  expect(screen.getByRole("button", { name: "Save policy" })).toBeDisabled();

  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByText("Could not refresh the image update policy")).not.toBeInTheDocument());
  expect(screen.getByRole("button", { name: "Save policy" })).toBeEnabled();
});

it("follows an active operation closely and slows down once it finishes", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let current = saved({}, { active_operation_id: "op-check" });
    serve(() => current);
    renderPolicy();
    expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
    current = saved({}, { last_check_id: "op-check", checked_at: "2026-11-23T09:00:00Z", updates: { state: "current", candidates: [] } });
    await vi.advanceTimersByTimeAsync(3_100);
    expect(await screen.findByText("Current")).toBeInTheDocument();
    const reads = calls("GET").length;
    await vi.advanceTimersByTimeAsync(3_100);
    expect(calls("GET")).toHaveLength(reads);
  } finally {
    vi.useRealTimers();
  }
});
