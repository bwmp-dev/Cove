import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { PanelAccessSettings } from "./PanelAccess";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));

// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

afterEach(() => vi.clearAllMocks());

function renderPanel() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PanelAccessSettings />
    </QueryClientProvider>,
  );
}

it("asks before saving a restriction and sends the parsed list", async () => {
  vi.mocked(request).mockImplementation((method, _path, body) =>
    Promise.resolve(
      method === "GET"
        ? { allowlist: null, environment_override: false, client_address: "203.0.113.7" }
        : { allowlist: (body as { allowlist: string[] }).allowlist, environment_override: false, client_address: "203.0.113.7" },
    ),
  );
  renderPanel();
  expect(await screen.findByText("Off, every address allowed")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Allowed addresses"), { target: { value: "198.51.100.0/24, 198.51.100.0/24" } });
  fireEvent.click(screen.getByRole("button", { name: "Add my address" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  const dialog = await screen.findByRole("dialog", { name: "Restrict panel access?" });
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Restrict access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/panel-access", { allowlist: ["198.51.100.0/24", "203.0.113.7"] }));
  expect(await screen.findByText("On, 2 ranges allowed")).toBeInTheDocument();
});

it("saves an empty list without confirmation and shows the server's refusal", async () => {
  vi.mocked(request).mockImplementation((method) =>
    method === "GET"
      ? Promise.resolve({ allowlist: ["192.0.2.0/24"], environment_override: true, client_address: "192.0.2.5" })
      : Promise.reject(new ApiError(503, "Panel access unavailable")),
  );
  renderPanel();
  expect(await screen.findByText("The host environment overrides this list")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Allowed addresses"), { target: { value: "" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/panel-access", { allowlist: [] }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(await screen.findByText("Panel access unavailable")).toBeInTheDocument();
});
