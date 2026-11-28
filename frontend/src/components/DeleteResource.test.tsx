import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { DeleteResource } from "./DeleteResource";

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn() }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Delete failed" }));

// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

it("says which resource data deletion keeps and removes", () => {
  render(<QueryClientProvider client={new QueryClient()}><DeleteResource kind="workload" id="app" /></QueryClientProvider>);
  expect(screen.getByText("Stop the resource first. Volumes, backups and the metadata needed to restore them stay on the node. Other worker files and unused build images with Cove-generated names are removed; worker files are kept if their storage layout cannot be verified.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Delete resource" }));
  const dialog = screen.getByRole("dialog", { name: "Delete this resource?" });
  expect(within(dialog).getByText("Stop the resource first. Cove removes its routes, stopped containers, API tokens, dashboard history, other worker files and unused build images with Cove-generated names. Volumes, backups and the metadata needed to restore them stay on the node, as do images you named, including ones Cove built. If Cove cannot verify the worker's storage layout, it keeps all worker files.")).toBeInTheDocument();
  expect(within(dialog).getByText(/as do images you named, including ones Cove built/)).toBeInTheDocument();
  expect(within(dialog).getByText(/cannot verify the worker's storage layout, it keeps all worker files/)).toBeInTheDocument();
  expect(screen.queryByText(/worker files remain/)).not.toBeInTheDocument();
});
