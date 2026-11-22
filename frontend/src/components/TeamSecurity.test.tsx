import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { CustomRoles } from "./TeamSecurity";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

const team = { id: "t1", name: "Core", role: "owner" as const, grants: [] };
const catalogue = ["view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"];
const builtIn = { viewer: ["view"], operator: ["view", "operate", "manage_backups"], editor: ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"] };

function renderRoles() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <CustomRoles team={team} />
    </QueryClientProvider>,
  );
}

afterEach(() => vi.clearAllMocks());
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

it("creates a role from the permission catalogue with view always included", async () => {
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve({ roles: [], permission_catalogue: catalogue, built_in: builtIn }) : Promise.resolve({ id: "r1" })));
  renderRoles();
  fireEvent.click(await screen.findByRole("button", { name: "New role" }));
  expect(screen.getByRole("checkbox", { name: /^View/ })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Create role" }));
  expect(await screen.findByText("Enter a role name.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Role name"), { target: { value: "Member managers" } });
  fireEvent.click(screen.getByRole("checkbox", { name: /^Manage members/ }));
  fireEvent.click(screen.getByRole("checkbox", { name: /^Edit configuration/ }));
  expect(screen.getByText(/Not a full editor/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create role" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/teams/t1/roles", { name: "Member managers", permissions: ["view", "edit_configuration", "manage_members"] }));
});

it("explains why a role in use cannot be deleted", async () => {
  vi.mocked(request).mockImplementation((method) =>
    method === "GET" ? Promise.resolve({ roles: [{ id: "r1", name: "Deployers", permissions: ["view", "operate"] }], permission_catalogue: catalogue, built_in: builtIn }) : Promise.reject(new ApiError(409, "Role is in use")),
  );
  renderRoles();
  fireEvent.click(await screen.findByRole("button", { name: /Delete\s*Deployers/ }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete role" }));
  expect(await screen.findByText(/still use this role/)).toBeInTheDocument();
});
