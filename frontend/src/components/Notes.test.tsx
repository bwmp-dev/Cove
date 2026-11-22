import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { Notes } from "./Notes";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", () => ({ UnsavedChangesGuard: () => null }));
afterEach(() => vi.clearAllMocks());

const saved = { markdown: "# Runbook\n<script>alert(1)</script>", html: "<h1>Runbook</h1>\n<!-- raw HTML omitted -->\n", version: "v1", updated_at: "2026-11-20T10:00:00Z" };

function renderNotes(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Notes scope="resource" id="w1" canEdit={canEdit} />
    </QueryClientProvider>,
  );
  return client;
}

it("renders only the server's sanitized HTML, never the Markdown source", async () => {
  vi.mocked(request).mockResolvedValue(saved);
  renderNotes(false);
  expect(await screen.findByRole("heading", { name: "Runbook" })).toBeInTheDocument();
  expect(document.querySelector("script")).toBeNull();
  expect(screen.queryByText(/alert\(1\)/)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit notes" })).not.toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/notes", undefined, expect.anything());
});

it("saves the draft against the version it started from", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) =>
    Promise.resolve(method === "GET" ? saved : { markdown: "Fresh runbook", html: "<p>Fresh runbook</p>\n", version: "v2", updated_at: "2026-11-22T10:00:00Z" }),
  );
  renderNotes();
  await user.click(await screen.findByRole("button", { name: "Edit notes" }));
  const editor = screen.getByLabelText("Markdown");
  await user.clear(editor);
  await user.type(editor, "Fresh runbook");
  await user.click(screen.getByRole("button", { name: "Save notes" }));
  expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/notes", { markdown: "Fresh runbook", version: "v1" });
  expect(await screen.findByText("Fresh runbook")).toBeInTheDocument();
  expect(screen.queryByLabelText("Markdown")).not.toBeInTheDocument();
});

it("keeps the draft when the notes changed on the server and saves against the new version", async () => {
  const user = userEvent.setup();
  let current = saved;
  vi.mocked(request).mockImplementation((method: string, _path: string, body?: unknown) => {
    if (method === "GET") return Promise.resolve(current);
    if ((body as { version: string }).version === "v1") return Promise.reject(new ApiError(409, "Notes changed; reload before saving"));
    return Promise.resolve({ markdown: "Mine", html: "<p>Mine</p>\n", version: "v3", updated_at: null });
  });
  renderNotes();
  await user.click(await screen.findByRole("button", { name: "Edit notes" }));
  const editor = screen.getByLabelText("Markdown");
  await user.clear(editor);
  await user.type(editor, "Mine");
  current = { ...saved, markdown: "Theirs", html: "<p>Theirs</p>\n", version: "v2" };
  await user.click(screen.getByRole("button", { name: "Save notes" }));
  expect(await screen.findByText("These notes changed while you were editing")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save notes" })).toBeDisabled();

  await user.click(screen.getByRole("button", { name: "Keep my draft" }));
  expect(screen.getByLabelText("Markdown")).toHaveValue("Mine");
  await user.click(screen.getByRole("button", { name: "Save notes" }));
  expect(request).toHaveBeenLastCalledWith("PUT", "/api/workloads/w1/notes", { markdown: "Mine", version: "v2" });
});

it("refuses to save notes over the size limit", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue({ markdown: "", html: "", version: "v0", updated_at: null });
  renderNotes();
  await user.click(await screen.findByRole("button", { name: "Add notes" }));
  const editor = screen.getByLabelText("Markdown");
  await user.click(editor);
  await user.paste("é".repeat(32_769));
  expect(screen.getByRole("button", { name: "Save notes" })).toBeDisabled();
  expect(editor).toHaveAttribute("aria-invalid", "true");
});
