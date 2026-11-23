import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { FileTransferLimit } from "./FileTransferLimit";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

const MiB = 1048576;
const limits = { min_file_bytes: MiB, max_file_bytes: 1024 * MiB, editor_bytes: 32768 };

function renderLimit(fileBytes: number) {
  vi.mocked(request).mockImplementation((method: string, _path: string, body?: unknown) => Promise.resolve({ ...limits, file_bytes: fileBytes, ...(method === "PUT" ? (body as object) : {}) }));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><FileTransferLimit /></QueryClientProvider>);
  return screen.findByLabelText("Largest file (MiB)");
}

function puts() {
  return vi.mocked(request).mock.calls.filter(([method]) => method === "PUT");
}

it("saves the file transfer limit as an exact byte count within 1 MiB to 1 GiB", async () => {
  const field = await renderLimit(64 * MiB);
  expect(field).toHaveValue(64);
  expect(screen.getByText(/At most four file transfers run at once/)).toBeInTheDocument();

  fireEvent.change(field, { target: { value: "1025" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter a whole number from 1 to 1,024 MiB.")).toBeInTheDocument();
  expect(field).toHaveFocus();
  expect(field).toHaveAttribute("aria-invalid", "true");
  expect(puts()).toHaveLength(0);

  fireEvent.change(field, { target: { value: "256" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/file-transfer-limits", { file_bytes: 256 * MiB }));
  expect(await screen.findByText("Saved. New file transfers use this limit.")).toBeInTheDocument();
});

it("keeps an untouched saved value that is not a whole MiB exactly", async () => {
  const odd = 64 * MiB + 1;
  const field = await renderLimit(odd);
  expect(field).toHaveValue(64);
  expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  fireEvent.submit(field.closest("form")!);
  expect(puts()).toHaveLength(0);
  expect(screen.queryByText(/Enter a whole number/)).not.toBeInTheDocument();
});

it("shows the server's refusal and keeps the draft", async () => {
  const field = await renderLimit(64 * MiB);
  vi.mocked(request).mockRejectedValueOnce(new Error("File transfers must be limited to 1 MiB–1 GiB"));
  fireEvent.change(field, { target: { value: "128" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("File transfers must be limited to 1 MiB–1 GiB")).toBeInTheDocument();
  expect(field).toHaveValue(128);
});
