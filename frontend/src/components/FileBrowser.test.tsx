import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request, requestBlob } from "../api/client";
import type { FileEntry, FileResponse } from "../api/files";
import { encodeBase64, encodeText, revisionOf } from "../lib/files";
import { FileBrowser } from "./FileBrowser";

vi.mock("../api/client", () => ({ request: vi.fn(), requestBlob: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const container = "a1b2c3d4e5f6";
const base = "/api/workloads/w1/files";
const targets = [
  { kind: "container", id: container, name: "web", state: "running" },
  { kind: "container", id: "ffffffffffff", name: "worker", state: "exited" },
  { kind: "volume", id: "cove-w1_data", name: "cove-w1_data", state: "available" },
];
const folders: Record<string, FileEntry[]> = {
  "/": [
    { name: "proc", kind: "directory", size: 0 },
    { name: "app", kind: "directory", size: 0 },
    { name: "current", kind: "link", size: 0 },
  ],
  "/app": [
    { name: "config.yml", kind: "file", size: 9 },
    { name: "logo.bin", kind: "file", size: 3 },
    { name: "huge.log", kind: "file", size: 40000 },
    { name: "empty", kind: "directory", size: 0 },
    { name: "windows.txt", kind: "file", size: 6 },
  ],
};
const contents: Record<string, Uint8Array<ArrayBuffer>> = {
  "/app/config.yml": new TextEncoder().encode("port: 80\n"),
  "/app/logo.bin": Uint8Array.from([0x89, 0x00, 0xff]),
  "/app/windows.txt": new TextEncoder().encode("a\r\nb\r\n"),
};

function response(extra: Partial<FileResponse>): FileResponse {
  return { targets: [], entries: [], data: null, revision: "", limited: false, max_bytes: 32768, ...extra };
}

let setItem: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  setItem = vi.spyOn(Storage.prototype, "setItem");
  vi.mocked(request).mockImplementation(async (method: string, path: string) => {
    if (method !== "GET") return response({});
    const url = new URL(path, "http://cove.test");
    if (url.pathname === `${base}/targets`) return response({ targets: targets as FileResponse["targets"] });
    const location = url.searchParams.get("path") ?? "/";
    if (url.pathname === `${base}/content`) {
      const bytes = contents[location] ?? new Uint8Array();
      return response({ data: encodeBase64(bytes), revision: await revisionOf(bytes) });
    }
    return response({ entries: folders[location] ?? [] });
  });
});
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function renderBrowser(blocked = false) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <FileBrowser workloadId="w1" blocked={blocked} />
    </QueryClientProvider>,
  );
}

async function chooseTarget(name: string) {
  fireEvent.click(await screen.findByRole("combobox", { name: /Container or volume/ }));
  fireEvent.click(screen.getByRole("option", { name }));
}

async function openAppFolder() {
  renderBrowser();
  await chooseTarget("web");
  fireEvent.click(await screen.findByRole("button", { name: "app" }));
  return screen.findByRole("list", { name: "Contents of /app" });
}

function calls(method: string) {
  return vi.mocked(request).mock.calls.filter(([verb]) => verb === method);
}

it("browses only running containers and owned volumes, without following links or special filesystems", async () => {
  renderBrowser();
  fireEvent.click(await screen.findByRole("combobox", { name: /Container or volume/ }));
  expect(screen.getByRole("option", { name: "worker (exited, not running)" })).toHaveAttribute("aria-disabled", "true");
  fireEvent.click(screen.getByRole("option", { name: "web" }));
  expect(screen.getByText("Changes apply to the running container immediately")).toBeInTheDocument();
  const root = await screen.findByRole("list", { name: "Contents of /" });
  expect(within(root).getAllByRole("listitem").map((item) => item.textContent)).toEqual([expect.stringContaining("app"), expect.stringContaining("proc"), expect.stringContaining("current")]);
  expect(within(root).queryByRole("button", { name: "proc" })).not.toBeInTheDocument();
  expect(within(root).getByText("Folder · not browsable")).toBeInTheDocument();
  expect(within(root).queryByRole("button", { name: "current" })).not.toBeInTheDocument();
  expect(within(root).getByText("Symbolic link · not followed")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", `${base}?kind=container&target=${container}&path=%2F`, undefined, expect.anything());

  fireEvent.click(within(root).getByRole("button", { name: "app" }));
  const app = await screen.findByRole("list", { name: "Contents of /app" });
  expect(within(app).queryByRole("button", { name: "huge.log" })).not.toBeInTheDocument();
  expect(within(app).getByText(/larger than 32\.0 KB/)).toBeInTheDocument();
  fireEvent.click(within(screen.getByRole("navigation", { name: "Folder path" })).getByRole("button", { name: "/" }));
  expect(await screen.findByRole("list", { name: "Contents of /" })).toBeInTheDocument();
});

it("explains volume write prerequisites", async () => {
  renderBrowser();
  await chooseTarget("cove-w1_data");
  expect(screen.getByText("Volume changes need the resource stopped")).toBeInTheDocument();
  expect(screen.getByText(/1 container of this resource is running now/)).toBeInTheDocument();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", `${base}?kind=volume&target=cove-w1_data&path=%2F`, undefined, expect.anything()));
});

it("edits text with exact revisions and keeps the new revision for the next save", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "config.yml" }));
  const editor = await screen.findByLabelText("Contents of /app/config.yml");
  expect(editor).toHaveValue("port: 80\n");
  expect(editor).toHaveFocus();

  fireEvent.change(editor, { target: { value: "port: 8080\n" } });
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save file" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", `${base}/content`, { kind: "container", target: container, path: "/app/config.yml", data: encodeText("port: 8080\n"), revision: await revisionOf(contents["/app/config.yml"]!) }]);
  expect(await screen.findByText("Saved.")).toBeInTheDocument();

  fireEvent.change(editor, { target: { value: "port: 9090\n" } });
  fireEvent.click(screen.getByRole("button", { name: "Save file" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(2));
  expect(calls("PUT")[1]?.[2]).toMatchObject({ revision: await revisionOf(new TextEncoder().encode("port: 8080\n")) });
  expect(setItem).not.toHaveBeenCalled();
  expect(window.location.href).not.toContain("config");
});

it("keeps edits and shows the API error when saving fails", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "config.yml" }));
  const editor = await screen.findByLabelText("Contents of /app/config.yml");
  fireEvent.change(editor, { target: { value: "port: 1\n" } });
  vi.mocked(request).mockRejectedValueOnce(new Error("File access failed; check target ownership, path, revision, permissions and required worker tools"));
  fireEvent.click(screen.getByRole("button", { name: "Save file" }));
  expect(await screen.findByText(/check target ownership, path, revision/)).toBeInTheDocument();
  expect(editor).toHaveValue("port: 1\n");
  expect(screen.getByRole("button", { name: "Read again" })).toBeInTheDocument();
});

it("blocks text edits past 32 KiB of UTF-8", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "config.yml" }));
  const editor = await screen.findByLabelText("Contents of /app/config.yml");
  fireEvent.change(editor, { target: { value: "é".repeat(16385) } });
  expect(screen.getByText(/32,770 of 32,768 bytes/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save file" })).toBeDisabled();
});

it("asks before discarding unsaved edits when moving to another folder", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "config.yml" }));
  fireEvent.change(await screen.findByLabelText("Contents of /app/config.yml"), { target: { value: "changed" } });
  fireEvent.click(within(app).getByRole("button", { name: "empty" }));
  const dialog = screen.getByRole("dialog", { name: "Discard unsaved changes?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(screen.getByLabelText("Contents of /app/config.yml")).toHaveValue("changed");
  fireEvent.click(within(app).getByRole("button", { name: "empty" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Discard unsaved changes?" })).getByRole("button", { name: "Discard changes" }));
  expect(await screen.findByText("This folder is empty.")).toBeInTheDocument();
  expect(screen.queryByLabelText("Contents of /app/config.yml")).not.toBeInTheDocument();
});

it("downloads binary files with their exact bytes instead of editing them", async () => {
  const createObjectURL = vi.fn(() => "blob:file");
  URL.createObjectURL = createObjectURL;
  URL.revokeObjectURL = vi.fn();
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
  const blob = new Blob([contents["/app/logo.bin"]!]);
  vi.mocked(requestBlob).mockResolvedValue(blob);
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  expect(await screen.findByText("Binary file")).toBeInTheDocument();
  expect(screen.queryByLabelText("Contents of /app/logo.bin")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Download" }));
  await waitFor(() => expect(createObjectURL).toHaveBeenCalledWith(blob));
  expect(requestBlob).toHaveBeenCalledWith(`${base}/download?kind=container&target=${container}&path=%2Fapp%2Flogo.bin`);
  expect(click).toHaveBeenCalled();
});

it("does not offer text editing that would rewrite CRLF line endings", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "windows.txt" }));
  expect(await screen.findByText("Text with carriage returns")).toBeInTheDocument();
  expect(screen.queryByLabelText("Contents of /app/windows.txt")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Replace" })).toBeEnabled();
});

it("creates files without overwriting existing names", async () => {
  await openAppFolder();
  fireEvent.click(screen.getByRole("button", { name: "New file" }));
  const dialog = screen.getByRole("dialog", { name: "New file" });
  fireEvent.change(within(dialog).getByLabelText("File name"), { target: { value: "config.yml" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create file" }));
  expect(within(dialog).getByText("An entry with this name already exists in this folder.")).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);

  fireEvent.change(within(dialog).getByLabelText("File name"), { target: { value: ".env" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create file" }));
  await waitFor(() => expect(calls("PUT")[0]?.[2]).toEqual({ kind: "container", target: container, path: "/app/.env", data: "", revision: "" }));
  expect(await screen.findByLabelText("Contents of /app/.env")).toHaveValue("");
});

it("uploads new files create-only and refuses oversize or existing names locally", async () => {
  await openAppFolder();
  const input = document.body.querySelector<HTMLInputElement>('input[type="file"]')!;

  fireEvent.change(input, { target: { files: [new File([new Uint8Array(32769)], "big.bin")] } });
  expect(await screen.findByText(/big\.bin is 32\.0 KB; uploads are limited to 32\.0 KB/)).toBeInTheDocument();
  fireEvent.change(input, { target: { files: [new File(["x"], "config.yml")] } });
  expect(await screen.findByText(/config\.yml already exists in this folder/)).toBeInTheDocument();
  expect(calls("POST")).toHaveLength(0);

  fireEvent.change(input, { target: { files: [new File([Uint8Array.from([1, 0, 2])], "seed.bin")] } });
  await waitFor(() => expect(calls("POST")[0]).toEqual(["POST", `${base}/upload`, { kind: "container", target: container, path: "/app/seed.bin", data: encodeBase64(Uint8Array.from([1, 0, 2])), revision: "" }]));
  expect(await screen.findByText("Uploaded seed.bin.")).toBeInTheDocument();
});

it("replaces an open file only after confirmation, using its revision", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  await screen.findByText("Binary file");
  const inputs = document.body.querySelectorAll<HTMLInputElement>('input[type="file"]');
  fireEvent.change(inputs[inputs.length - 1]!, { target: { files: [new File(["new"], "local.bin")] } });
  const dialog = await screen.findByRole("dialog", { name: "Replace logo.bin?" });
  expect(calls("POST")).toHaveLength(0);
  const revision = await revisionOf(contents["/app/logo.bin"]!);
  fireEvent.click(within(dialog).getByRole("button", { name: "Replace file" }));
  await waitFor(() => expect(calls("POST")[0]?.[2]).toEqual({ kind: "container", target: container, path: "/app/logo.bin", data: encodeText("new"), revision }));
});

it("deletes one file or empty folder after naming it in the confirmation", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "Delete empty" }));
  const dialog = screen.getByRole("dialog", { name: "Delete empty?" });
  expect(dialog).toHaveTextContent("/app/empty");
  expect(dialog).toHaveTextContent("Cove never deletes recursively");
  expect(calls("DELETE")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete folder" }));
  await waitFor(() => expect(calls("DELETE")[0]).toEqual(["DELETE", `${base}/content`, { kind: "container", target: container, path: "/app/empty" }]));
  expect(await screen.findByText("Deleted empty.")).toBeInTheDocument();
});

it("makes no file requests while access is blocked", () => {
  renderBrowser(true);
  expect(request).not.toHaveBeenCalled();
});
