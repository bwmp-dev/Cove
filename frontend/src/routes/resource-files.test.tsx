import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { useSession } from "../lib/session";
import { ResourceFiles } from "./resource-files";

vi.mock("../api/client", () => ({ request: vi.fn(), requestBlob: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../lib/session", () => ({ useSession: vi.fn() }));
const resource = { id: "w1", node_id: "n1", name: "web", permissions: ["view", "read", "read_secrets", "edit_configuration", "edit"] };
vi.mock("./workload-detail", () => ({ useResource: () => resource }));

const readyNode = { id: "n1", name: "edge", state: "ready", capabilities: { file_browser: true } };

function serve(node: unknown, operations: unknown[] = []) {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([node]);
    if (path.includes("/operations")) return Promise.resolve(operations);
    return Promise.resolve({ targets: [], entries: [], data: null, revision: "", limited: false, max_bytes: 32768 });
  });
}

function renderPage(isAdmin: boolean) {
  vi.mocked(useSession).mockReturnValue({ is_admin: isAdmin } as ReturnType<typeof useSession>);
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ResourceFiles /></QueryClientProvider>);
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  resource.permissions = ["view", "read", "read_secrets", "edit_configuration", "edit"];
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

function fileCalls() {
  return vi.mocked(request).mock.calls.filter(([, path]) => path.includes("/files"));
}

it("explains the restriction to team members who are not instance administrators and calls no API", () => {
  serve(readyNode);
  renderPage(false);
  expect(screen.getByText("Instance administrators only")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it.each([
  ["Edit configuration without Read secrets", ["view", "read", "edit_configuration", "edit"]],
  ["Read secrets without Edit configuration", ["view", "read", "read_secrets"]],
])("keeps files closed to an instance administrator with %s and calls no API", (_label, permissions) => {
  resource.permissions = permissions;
  serve(readyNode);
  renderPage(true);
  expect(screen.getByText("Read secrets and Edit configuration required")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("asks for a worker upgrade before using file access on an old worker", async () => {
  serve({ ...readyNode, capabilities: { terminal: true } });
  renderPage(true);
  expect(await screen.findByText("This worker does not support file browsing")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("refuses an unready worker", async () => {
  serve({ ...readyNode, state: "unreachable" });
  renderPage(true);
  expect(await screen.findByText("The worker is not ready")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("pauses file access while a resource operation is active", async () => {
  serve(readyNode, [{ id: "op1", workload_id: "w1", state: "running", action: "deploy" }]);
  renderPage(true);
  expect(await screen.findByText("File access is paused while an operation runs")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("lists targets for administrators on a supported worker", async () => {
  serve(readyNode);
  renderPage(true);
  expect(await screen.findByText("Nothing to browse")).toBeInTheDocument();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/files/targets", undefined, expect.anything()));
});
