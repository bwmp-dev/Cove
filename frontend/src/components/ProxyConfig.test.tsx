import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ProxyFile } from "../api/proxy";
import { ProxyFileEditor } from "./ProxyConfig";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
// The guard needs a router; leaving the page is covered by the shared component's own use.
vi.mock("./SaveBar", () => ({ UnsavedChangesGuard: () => null }));
afterEach(() => vi.clearAllMocks());

const revision = "a".repeat(64);
const changeId = expect.any(String) as string;
const content = "http:\n  routers:\n    legacy:\n      rule: Host(`legacy.example.com`)\n      service: legacy\n";

function file(status: ProxyFile["status"], extra: Partial<ProxyFile> = {}): ProxyFile {
  return { name: "legacy-import.yaml", revision, size: content.length, status, content, objects: [{ kind: "http.routers", name: "legacy", status: "disabled", errors: ['middleware "auth@file" does not exist'] }], ...extra };
}

function renderEditor(name?: string, onClose = vi.fn()) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ProxyFileEditor nodeId="node-1" name={name} existing={["legacy-import.yaml", "routes.yaml"]} onClose={onClose} />
    </QueryClientProvider>,
  );
  return onClose;
}

it("imports an existing file without editing it first", async () => {
  vi.mocked(request).mockResolvedValueOnce(file("unmanaged")).mockResolvedValueOnce({ action: "import", name: "legacy-import.yaml", state: "succeeded", started_at: "2026-11-20T10:00:00Z" }).mockResolvedValue(file("managed", { revision: revision }));
  renderEditor("legacy-import.yaml");
  const editor = await screen.findByLabelText("Configuration");
  expect(editor).toHaveAttribute("readonly");
  expect(screen.queryByRole("button", { name: "Validate and apply" })).not.toBeInTheDocument();
  expect(screen.getByText('middleware "auth@file" does not exist')).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Import file" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/node-1/ingress-config/files/legacy-import.yaml/import", { revision, change_id: changeId }));
});

it("applies edits against the loaded revision and shows Traefik's problems", async () => {
  vi.mocked(request).mockResolvedValueOnce(file("managed")).mockResolvedValueOnce({ action: "put", name: "legacy-import.yaml", state: "rejected", message: "Nothing was changed on the proxy.", problems: ['HTTP router "legacy": middleware "missing@file" does not exist'], started_at: "2026-11-20T10:00:00Z" });
  renderEditor("legacy-import.yaml");
  const editor = await screen.findByLabelText("Configuration");
  const apply = screen.getByRole("button", { name: "Validate and apply" });
  expect(apply).toBeDisabled();
  const edited = content.replace("service: legacy", "service: legacy\n      middlewares: [missing]");
  fireEvent.change(editor, { target: { value: edited } });
  fireEvent.click(apply);
  expect(await screen.findByText('HTTP router "legacy": middleware "missing@file" does not exist')).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/node-1/ingress-config/files/legacy-import.yaml", { content: edited, revision, change_id: changeId });
  expect(screen.getByLabelText("Configuration")).toHaveValue(edited);
});

it("offers a reload when the file changed after it was opened", async () => {
  vi.mocked(request).mockResolvedValueOnce(file("managed")).mockRejectedValueOnce(new ApiError(409, "The file changed after it was loaded. Reload it and reapply your edits."));
  renderEditor("legacy-import.yaml");
  fireEvent.change(await screen.findByLabelText("Configuration"), { target: { value: content + "# edit\n" } });
  fireEvent.click(screen.getByRole("button", { name: "Validate and apply" }));
  expect(await screen.findByRole("button", { name: "Reload file" })).toBeInTheDocument();
});

it("tells the administrator a lost reply may still complete", async () => {
  vi.mocked(request).mockResolvedValueOnce(file("managed")).mockRejectedValueOnce(new ApiError(0, "Cannot reach the control plane."));
  renderEditor("legacy-import.yaml");
  fireEvent.change(await screen.findByLabelText("Configuration"), { target: { value: content + "# edit\n" } });
  fireEvent.click(screen.getByRole("button", { name: "Validate and apply" }));
  expect(await screen.findByText("No reply from the worker")).toBeInTheDocument();
});

it("validates new file names before contacting the worker and opens the saved file", async () => {
  vi.mocked(request).mockResolvedValueOnce({ action: "put", name: "extra.yaml", state: "succeeded", started_at: "2026-11-20T10:00:00Z" });
  const onClose = renderEditor();
  fireEvent.change(screen.getByLabelText("Configuration"), { target: { value: "http: {}\n" } });
  fireEvent.change(screen.getByLabelText("File name"), { target: { value: "routes.yaml" } });
  fireEvent.click(screen.getByRole("button", { name: "Validate and apply" }));
  expect(await screen.findByText("routes.yaml, panel.yaml and cove- names are generated by Cove.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
  fireEvent.change(screen.getByLabelText("File name"), { target: { value: "extra.yaml" } });
  fireEvent.click(screen.getByRole("button", { name: "Validate and apply" }));
  await waitFor(() => expect(onClose).toHaveBeenCalledWith("extra.yaml"));
  expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/node-1/ingress-config/files/extra.yaml", { content: "http: {}\n", revision: "", change_id: changeId });
});
