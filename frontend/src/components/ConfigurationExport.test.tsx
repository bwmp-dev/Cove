import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, requestFile } from "../api/client";
import type { Project } from "../api/projects";
import { ConfigurationExport } from "./ConfigurationExport";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), requestFile: vi.fn() }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const createObjectURL = vi.fn(() => "blob:archive");
const revokeObjectURL = vi.fn();
let clicked: HTMLAnchorElement[] = [];
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  URL.createObjectURL = createObjectURL;
  URL.revokeObjectURL = revokeObjectURL;
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this);
  });
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const permissions: Project["permissions"] = ["read", "operate", "edit"];
const project: Project = { id: "p", name: "Shop", permissions, environments: [{ id: "e1", name: "Production", permissions }, { id: "e2", name: "Staging", permissions: ["read"] }] };

function setup() {
  const client = new QueryClient();
  render(
    <QueryClientProvider client={client}>
      <ConfigurationExport project={project} environments={project.environments} online />
    </QueryClientProvider>,
  );
  return client;
}

it("exports only after the secret warning is acknowledged and keeps no archive in memory", async () => {
  const user = userEvent.setup();
  const archive = new Blob(["PK-secret-bytes"], { type: "application/zip" });
  vi.mocked(requestFile).mockResolvedValue({ blob: archive, filename: "shop-production.zip" });
  const client = setup();

  await user.click(screen.getByRole("combobox", { name: "What to export" }));
  expect(screen.queryByRole("option", { name: "Staging environment" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("option", { name: "Production environment" }));
  await user.click(screen.getByRole("button", { name: "Export ZIP" }));

  const dialog = screen.getByRole("dialog", { name: "Export Production?" });
  expect(within(dialog).getByText("The archive contains secrets")).toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Export" });
  expect(confirm).toBeDisabled();
  await user.click(within(dialog).getByRole("checkbox", { name: /contains decrypted secrets/ }));
  await user.click(confirm);

  expect(await screen.findByText("Export saved")).toBeInTheDocument();
  expect(requestFile).toHaveBeenCalledWith("POST", "/api/projects/p/configuration/export", { environment_id: "e1" });
  expect(clicked).toHaveLength(1);
  expect(clicked[0]!.download).toBe("shop-production.zip");
  expect(createObjectURL).toHaveBeenCalledWith(archive);
  await vi.waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith("blob:archive"));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(client.getMutationCache().getAll().some((mutation) => mutation.state.data instanceof Blob || mutation.state.variables !== undefined)).toBe(false);
});

it("shows export failures and resets the acknowledgement when reopened", async () => {
  const user = userEvent.setup();
  vi.mocked(requestFile).mockRejectedValue(new ApiError(409, "Shared variable DATABASE_URL could not be resolved."));
  setup();
  await user.click(screen.getByRole("button", { name: "Export ZIP" }));
  await user.click(screen.getByRole("checkbox", { name: /contains decrypted secrets/ }));
  await user.click(screen.getByRole("button", { name: "Export" }));
  expect(await screen.findByText("Shared variable DATABASE_URL could not be resolved.")).toBeInTheDocument();
  expect(requestFile).toHaveBeenCalledWith("POST", "/api/projects/p/configuration/export", {});
  expect(clicked).toHaveLength(0);

  await user.click(screen.getByRole("button", { name: "Cancel" }));
  await user.click(screen.getByRole("button", { name: "Export ZIP" }));
  expect(screen.getByRole("checkbox", { name: /contains decrypted secrets/ })).not.toBeChecked();
  expect(screen.queryByText("Shared variable DATABASE_URL could not be resolved.")).not.toBeInTheDocument();
});
