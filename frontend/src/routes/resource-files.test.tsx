import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Permission } from "../api/types";
import { ResourceFiles } from "./resource-files";

vi.mock("../api/client", () => ({ request: vi.fn(), uploadFile: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>, useBlocker: () => ({ status: "idle" }) }));
const resource = vi.hoisted(() => ({ permissions: [] as Permission[] }));
vi.mock("./workload-detail", () => ({ useResource: () => ({ id: "w1", node_id: "n1", name: "web", permissions: resource.permissions }) }));

const readyNode = { id: "n1", name: "edge", state: "ready", capabilities: { file_browser: true, file_transfers: true } };
const editor: Permission[] = ["read", "operate", "edit"];

function serve(node: unknown, operations: unknown[] = []) {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([node]);
    if (path.includes("/operations")) return Promise.resolve(operations);
    if (path.endsWith("/files/limits")) return Promise.resolve({ file_bytes: 67108864, min_file_bytes: 1048576, max_file_bytes: 1073741824, editor_bytes: 32768 });
    return Promise.resolve({ targets: [], entries: [], data: null, revision: "", limited: false, max_bytes: 32768 });
  });
}

function renderPage(permissions: Permission[]) {
  resource.permissions = permissions;
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ResourceFiles /></QueryClientProvider>);
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

function fileCalls() {
  return vi.mocked(request).mock.calls.filter(([, path]) => path.includes("/files"));
}

it.each([
  ["viewers", ["read"] as Permission[]],
  ["operators", ["read", "operate"] as Permission[]],
])("explains that %s need editor access and calls no API", (_role, permissions) => {
  serve(readyNode);
  renderPage(permissions);
  expect(screen.getByText("Files need editor access")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("asks for a worker upgrade before using file access on an old worker", async () => {
  serve({ ...readyNode, capabilities: { terminal: true } });
  renderPage(editor);
  expect(await screen.findByText("This worker does not support file browsing")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("refuses an unready worker", async () => {
  serve({ ...readyNode, state: "unreachable" });
  renderPage(editor);
  expect(await screen.findByText("The worker is not ready")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("pauses file access while a resource operation is active", async () => {
  serve(readyNode, [{ id: "op1", workload_id: "w1", state: "running", action: "deploy" }]);
  renderPage(editor);
  expect(await screen.findByText("File access is paused while an operation runs")).toBeInTheDocument();
  expect(fileCalls()).toHaveLength(0);
});

it("lists targets and reads the transfer limit for scoped editors who are not instance administrators", async () => {
  serve(readyNode);
  renderPage(editor);
  expect(await screen.findByText("Nothing to browse")).toBeInTheDocument();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/files/targets", undefined, expect.anything()));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/files/limits", undefined, expect.anything()));
});

it("does not read the transfer limit on workers without streamed file support", async () => {
  serve({ ...readyNode, capabilities: { file_browser: true } });
  renderPage(editor);
  expect(await screen.findByText("Nothing to browse")).toBeInTheDocument();
  expect(fileCalls().filter(([, path]) => path.endsWith("/limits"))).toHaveLength(0);
});
