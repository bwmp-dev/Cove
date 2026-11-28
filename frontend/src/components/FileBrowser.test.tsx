import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request, uploadFile } from "../api/client";
import type { FileEntry, FileResponse } from "../api/files";
import { encodeBase64, encodeText, revisionOf } from "../lib/files";
import { FileBrowser } from "./FileBrowser";

vi.mock("../api/client", () => ({ request: vi.fn(), uploadFile: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
vi.mock("../lib/team", () => ({ selectedTeam: () => "team-1" }));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const container = "a1b2c3d4e5f6";
const base = "/api/workloads/w1/files";
const transferLimit = 1048576;
const largeRevision = "a".repeat(64);
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
    { name: "dump.sql", kind: "file", size: transferLimit + 1 },
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
    if (url.pathname === `${base}/limits`) return { file_bytes: transferLimit, min_file_bytes: 1048576, max_file_bytes: 1073741824, editor_bytes: 32768 };
    const location = url.searchParams.get("path") ?? "/";
    if (url.pathname === `${base}/metadata`) return response({ revision: largeRevision, size: 40000 });
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

function renderBrowser(blocked = false, transfersSupported = true) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <FileBrowser workloadId="w1" blocked={blocked} offline={false} transfersSupported={transfersSupported} />
    </QueryClientProvider>,
  );
}

async function chooseTarget(name: string) {
  fireEvent.click(await screen.findByRole("combobox", { name: /Container or volume/ }));
  fireEvent.click(screen.getByRole("option", { name }));
}

async function openAppFolder(transfersSupported = true) {
  renderBrowser(false, transfersSupported);
  await chooseTarget("web");
  fireEvent.click(await screen.findByRole("button", { name: "app" }));
  const list = await screen.findByRole("list", { name: "Contents of /app" });
  if (transfersSupported) await waitFor(() => expect(screen.getByRole("button", { name: "Upload" })).toBeEnabled());
  return list;
}

function calls(method: string) {
  return vi.mocked(request).mock.calls.filter(([verb]) => verb === method);
}

function fileInputs() {
  return document.body.querySelectorAll<HTMLInputElement>('input[type="file"]');
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
  expect(await within(app).findByText("39.1 KiB · download or replace")).toBeInTheDocument();
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
  expect(screen.getByText("Could not confirm the file change")).toBeInTheDocument();
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

it("downloads through a native same-origin link with the team context instead of buffering in the page", async () => {
  const app = await openAppFolder();
  const href = `${base}/download?kind=container&target=${container}&path=%2Fapp%2Flogo.bin&team_id=team-1`;
  expect(within(app).getByRole("link", { name: "Download logo.bin" })).toHaveAttribute("href", href);
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  expect(await screen.findByText("Binary file")).toBeInTheDocument();
  expect(screen.queryByLabelText("Contents of /app/logo.bin")).not.toBeInTheDocument();
  const link = screen.getByRole("link", { name: "Download" });
  expect(link).toHaveAttribute("href", href);
  expect(link).toHaveAttribute("target", "_blank");
  expect(link).toHaveAttribute("rel", "noopener");
  expect(vi.mocked(request).mock.calls.filter(([, path]) => path.includes("/download"))).toHaveLength(0);
});

it("opens files larger than the editor by size and revision only, and offers download and replace", async () => {
  const app = await openAppFolder();
  expect(within(app).getByRole("link", { name: "Download huge.log" })).toBeInTheDocument();
  fireEvent.click(within(app).getByRole("button", { name: "huge.log" }));
  expect(await screen.findByText("Larger than the 32.0 KiB editor")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", `${base}/metadata?kind=container&target=${container}&path=%2Fapp%2Fhuge.log`);
  expect(vi.mocked(request).mock.calls.filter(([, path]) => path.includes("/content"))).toHaveLength(0);
  expect(screen.getByText(largeRevision.slice(0, 12))).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Replace" })).toBeEnabled();
});

it("does not offer files over the configured transfer limit for download", async () => {
  const app = await openAppFolder();
  expect(within(app).getByText("1.00 MiB · over the 1.00 MiB transfer limit")).toBeInTheDocument();
  expect(within(app).queryByRole("button", { name: "dump.sql" })).not.toBeInTheDocument();
  expect(within(app).queryByRole("link", { name: "Download dump.sql" })).not.toBeInTheDocument();
});

it("keeps old workers to small text editing and makes no streamed or metadata requests", async () => {
  const app = await openAppFolder(false);
  expect(screen.getByText("Downloads and uploads need an upgraded worker")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Upload" })).not.toBeInTheDocument();
  expect(within(app).queryByRole("button", { name: "huge.log" })).not.toBeInTheDocument();
  expect(within(app).getByText(/39\.1 KiB · larger than the 32\.0 KiB editor; upgrade the worker/)).toBeInTheDocument();
  expect(within(app).queryByRole("link")).not.toBeInTheDocument();
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  expect(await screen.findByText("Binary file")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Replace" })).not.toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([, path]) => /\/(limits|metadata|download|upload)/.test(path))).toHaveLength(0);
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

it("streams new uploads create-only as the raw File and refuses oversize or existing names locally", async () => {
  const arrayBuffer = vi.spyOn(Blob.prototype, "arrayBuffer");
  const text = vi.spyOn(Blob.prototype, "text");
  vi.mocked(uploadFile).mockResolvedValue({ revision: largeRevision, size: 40000 });
  await openAppFolder();
  const input = fileInputs()[0]!;

  fireEvent.change(input, { target: { files: [new File([new Uint8Array(transferLimit + 1)], "big.bin")] } });
  expect(await screen.findByText(/big\.bin is 1\.00 MiB; files are limited to 1\.00 MiB/)).toBeInTheDocument();
  fireEvent.change(input, { target: { files: [new File(["x"], "config.yml")] } });
  expect(await screen.findByText(/config\.yml already exists in this folder/)).toBeInTheDocument();
  expect(screen.getByText("Nothing was sent")).toBeInTheDocument();
  expect(uploadFile).not.toHaveBeenCalled();

  const seed = new File([new Uint8Array(40000)], "seed.bin");
  fireEvent.change(input, { target: { files: [seed] } });
  await waitFor(() => expect(uploadFile).toHaveBeenCalledTimes(1));
  const [path, body, , headers] = vi.mocked(uploadFile).mock.calls[0]!;
  expect(path).toBe(`${base}/upload?kind=container&target=${container}&path=%2Fapp%2Fseed.bin`);
  expect(body).toBe(seed);
  expect(headers).toEqual({});
  expect(await screen.findByText("Uploaded seed.bin (39.1 KiB).")).toBeInTheDocument();
  const folderReads = () => calls("GET").filter(([, url]) => url === `${base}?kind=container&target=${container}&path=%2Fapp`).length;
  await waitFor(() => expect(folderReads()).toBe(2));
  expect(arrayBuffer).not.toHaveBeenCalled();
  expect(text).not.toHaveBeenCalled();
  expect(calls("POST")).toHaveLength(0);
});

it("cancels a running upload and says the change may still have completed", async () => {
  vi.mocked(uploadFile).mockImplementation((_path, _file, signal) => new Promise((_resolve, reject) => signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")))));
  await openAppFolder();
  fireEvent.change(fileInputs()[0]!, { target: { files: [new File(["abc"], "seed.bin")] } });
  expect(await screen.findByText(/Uploading/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Parent folder" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Cancel upload" }));
  expect(await screen.findByText(/The upload was cancelled\. The change may still have completed; refresh before trying again\./)).toBeInTheDocument();
  expect(uploadFile).toHaveBeenCalledTimes(1);
});

it("replaces a small binary file only after confirmation, naming its revision", async () => {
  vi.mocked(uploadFile).mockResolvedValue({ revision: await revisionOf(new TextEncoder().encode("new")), size: 3 });
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  await screen.findByText("Binary file");
  const local = new File(["new"], "local.bin");
  const inputs = fileInputs();
  fireEvent.change(inputs[inputs.length - 1]!, { target: { files: [local] } });
  const dialog = await screen.findByRole("dialog", { name: "Replace logo.bin?" });
  expect(uploadFile).not.toHaveBeenCalled();
  const revision = await revisionOf(contents["/app/logo.bin"]!);
  fireEvent.click(within(dialog).getByRole("button", { name: "Replace file" }));
  await waitFor(() => expect(uploadFile).toHaveBeenCalledWith(`${base}/upload?kind=container&target=${container}&path=%2Fapp%2Flogo.bin`, local, expect.any(AbortSignal), { "X-Cove-Revision": revision }));
  expect(await screen.findByText("Replaced logo.bin with local.bin.")).toBeInTheDocument();
});

it("replaces a large file with the metadata revision and keeps it unchanged when the revision is stale", async () => {
  vi.mocked(uploadFile).mockRejectedValueOnce(new Error("File transfer failed; check ownership, revision, size, free space and required tools; refresh before retrying changes."));
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "huge.log" }));
  await screen.findByText("Larger than the 32.0 KiB editor");
  const local = new File([new Uint8Array(50000)], "huge.log");
  const inputs = fileInputs();
  fireEvent.change(inputs[inputs.length - 1]!, { target: { files: [local] } });
  fireEvent.click(within(await screen.findByRole("dialog", { name: "Replace huge.log?" })).getByRole("button", { name: "Replace file" }));
  await waitFor(() => expect(uploadFile).toHaveBeenCalledWith(expect.stringContaining("path=%2Fapp%2Fhuge.log"), local, expect.any(AbortSignal), { "X-Cove-Revision": largeRevision }));
  // A generic worker failure can happen mid-write, so the page must not claim the file is unchanged.
  expect(await screen.findByText("Could not confirm the file change")).toBeInTheDocument();
  expect(screen.queryByText(/was not changed/)).not.toBeInTheDocument();
  expect(screen.getByText(/The file may or may not have changed\. Nothing was retried; refresh before trying again\./)).toBeInTheDocument();
  expect(screen.getByText(largeRevision.slice(0, 12))).toBeInTheDocument();
  expect(uploadFile).toHaveBeenCalledTimes(1);
});

it("refuses a replacement larger than the transfer limit before sending it", async () => {
  const app = await openAppFolder();
  fireEvent.click(within(app).getByRole("button", { name: "logo.bin" }));
  await screen.findByText("Binary file");
  const inputs = fileInputs();
  fireEvent.change(inputs[inputs.length - 1]!, { target: { files: [new File([new Uint8Array(transferLimit + 1)], "big.bin")] } });
  expect(await screen.findByText(/big\.bin is 1\.00 MiB; files are limited to 1\.00 MiB/)).toBeInTheDocument();
  expect(screen.queryByRole("dialog", { name: "Replace logo.bin?" })).not.toBeInTheDocument();
  expect(uploadFile).not.toHaveBeenCalled();
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
  expect(uploadFile).not.toHaveBeenCalled();
});
