import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Environment } from "../api/projects";
import type { EnvironmentSuspension, Node, SuspendedResource } from "../api/types";
import { EnvironmentHoldNotice, EnvironmentSuspensionSection, useEnvironmentHold } from "./EnvironmentSuspension";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children, className }: { children: ReactNode; className?: string }) => <a href="/" className={className}>{children}</a> }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.open = true; };
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.open = false; };
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const operator: Environment = { id: "e1", name: "staging", permissions: ["read", "operate"] };
const node: Node = { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: null };
const resource = (overrides: Partial<SuspendedResource> = {}): SuspendedResource => ({ id: "w1", name: "web", kind: "service", node_id: "n1", supported: true, phase: "active", resume_wanted: false, ...overrides });

function serve(get: () => Promise<EnvironmentSuspension>, put?: (body: { paused: boolean }) => Promise<EnvironmentSuspension>) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/nodes") return Promise.resolve([node]);
    if (path === "/api/environments/e1/suspension") return method === "PUT" ? (put?.(body as { paused: boolean }) ?? Promise.reject(new Error("unexpected PUT"))) : get();
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderSection(environment = operator, preview = false) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <EnvironmentSuspensionSection environment={environment} preview={preview} />
    </QueryClientProvider>,
  );
}

it("asks for confirmation before requesting a pause and shows queued progress, not completion", async () => {
  let current: EnvironmentSuspension = { paused: false, phase: "active", resources: [resource()] };
  serve(
    () => Promise.resolve(current),
    (body) => {
      current = { paused: body.paused, phase: "pausing", resources: [resource({ phase: "pausing" })] };
      return Promise.resolve(current);
    },
  );
  renderSection();
  fireEvent.click(await screen.findByRole("button", { name: "Pause environment" }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText(/Active operations in this environment are asked to cancel/)).toBeInTheDocument();
  expect(within(dialog).getByText(/return 503 while paused/)).toBeInTheDocument();
  expect(within(dialog).getByText(/not a backup/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Pause environment" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/environments/e1/suspension", { paused: true }));
  expect((await screen.findAllByText("Pausing")).length).toBeGreaterThan(0);
  expect(screen.getByText("Requested: pause")).toBeInTheDocument();
  expect(screen.getByText("Waiting to be queued")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Resume environment" })).toBeInTheDocument();
});

it("lists what each resource will do on resume and links its operation", async () => {
  serve(() =>
    Promise.resolve({
      paused: true,
      phase: "paused",
      resources: [
        resource({ phase: "paused", resume_wanted: true, operation_id: "op-1" }),
        resource({ id: "w2", name: "worker", phase: "paused", resume_wanted: false }),
        resource({ id: "w3", name: "nightly", kind: "job", phase: "paused" }),
      ],
    }),
  );
  renderSection();
  const list = await screen.findByRole("list", { name: "Pause state of resources in staging" });
  expect(within(list).getByText("Starts on resume")).toBeInTheDocument();
  expect(within(list).getByText(/Stays stopped on resume/)).toBeInTheDocument();
  expect(within(list).getByText(/Jobs do not run on resume/)).toBeInTheDocument();
  expect(within(list).getByRole("link", { name: "View operation" })).toBeInTheDocument();
  expect(within(list).getAllByText("worker-1")).toHaveLength(3);
});

it("retries a failed resume with the same requested state and shows the error", async () => {
  serve(
    () => Promise.resolve({ paused: false, phase: "error", resources: [resource({ phase: "error", operation_id: "op-2", error: "web did not become healthy within 2 minutes" })] }),
    () => Promise.resolve({ paused: false, phase: "resuming", resources: [resource({ phase: "resuming" })] }),
  );
  renderSection();
  expect(await screen.findByText("web did not become healthy within 2 minutes")).toBeInTheDocument();
  expect(screen.getByText(/repair a resource with Deploy, Start, Restart or a backup Restore/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry resume" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/environments/e1/suspension", { paused: false }));
});

it("explains a rejected request without hiding the state", async () => {
  serve(
    () => Promise.resolve({ paused: false, phase: "active", resources: [resource()] }),
    () => Promise.reject(new ApiError(409, "upgrade and verify every worker in the environment before changing its suspension")),
  );
  renderSection();
  fireEvent.click(await screen.findByRole("button", { name: "Pause environment" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Pause environment" }));
  expect(await screen.findByText("Could not request the pause")).toBeInTheDocument();
  expect(screen.getByText(/upgrade and verify every worker/)).toBeInTheDocument();
  expect(screen.getByText("Active")).toBeInTheDocument();
});

it("blocks changes until every worker supports pausing and links the worker", async () => {
  serve(() => Promise.resolve({ paused: false, phase: "active", resources: [resource({ supported: false })] }));
  renderSection();
  expect(await screen.findByText("Some workers cannot pause resources")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Pause environment" })).toBeDisabled();
});

it("is read-only for viewers and preview environments", async () => {
  serve(() => Promise.resolve({ paused: false, phase: "active", resources: [] }));
  renderSection({ ...operator, permissions: ["read"] });
  expect(await screen.findByText(/needs operator access/)).toBeInTheDocument();
  expect(screen.getByText(/Pausing it has nothing to stop/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pause environment" })).not.toBeInTheDocument();
});

it("hides changes for managed preview environments", async () => {
  serve(() => Promise.resolve({ paused: false, phase: "active", resources: [] }));
  renderSection({ ...operator, preview_id: "pv1" }, true);
  expect(await screen.findByText(/through their preview/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pause environment" })).not.toBeInTheDocument();
});

// The query retries once before reporting the failure.
it("shows an unreadable state as unknown with Retry", async () => {
  serve(() => Promise.reject(new ApiError(503, "environment state unavailable")));
  renderSection();
  expect(await screen.findByText("Could not load the environment's pause state", {}, { timeout: 4_000 })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pause environment" })).not.toBeInTheDocument();
});

it("disables changes while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => Promise.resolve({ paused: true, phase: "paused", resources: [] }));
  renderSection();
  expect(await screen.findByText("Pausing and resuming are unavailable while offline.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Resume environment" })).toBeDisabled();
});

function HoldProbe({ paused }: { paused?: boolean }) {
  const { hold, state } = useEnvironmentHold({ id: "w1", environment_id: "e1", environment_paused: paused });
  return <EnvironmentHoldNotice hold={hold} environment={<a href="/">Open staging</a>} onRetry={() => void state.refetch()} retrying={state.isFetching} />;
}

function renderProbe(paused?: boolean) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <HoldProbe paused={paused} />
    </QueryClientProvider>,
  );
}

it("tells a resource that its environment is paused and links the environment", async () => {
  serve(() => Promise.resolve({ paused: true, phase: "pausing", resources: [resource({ phase: "pausing" })] }));
  renderProbe(true);
  expect(await screen.findByText("Environment pause in progress")).toBeInTheDocument();
  expect(screen.getByText(/Stop and Abort stay available/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Open staging" })).toBeInTheDocument();
});

it("offers repair after a failed resume", async () => {
  serve(() => Promise.resolve({ paused: false, phase: "error", resources: [resource({ phase: "error", error: "start failed" })] }));
  renderProbe(false);
  expect(await screen.findByText("Resuming this resource failed")).toBeInTheDocument();
  expect(screen.getByText(/use Deploy, Start, Restart or a backup Restore to repair/)).toBeInTheDocument();
});

it("never reports an unreadable environment as active", async () => {
  serve(() => Promise.reject(new ApiError(503, "environment state unavailable")));
  renderProbe(false);
  expect(await screen.findByText("Environment pause state unknown", {}, { timeout: 4_000 })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});
