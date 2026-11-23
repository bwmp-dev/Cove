import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Bastion } from "../api/bastions";
import { ApiError, request } from "../api/client";
import { Bastions } from "./Bastions";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute("open", ""); };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute("open"); };
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl";
const jump: Bastion = { id: "b1", name: "Jump", address: "jump.example", port: 22, user: "cove-jump", host_key: hostKey, public_key: "ssh-ed25519 AAAAgenerated cove", fingerprint: "SHA256:abc", targets: ["10.0.0.12:22"], limited: false };

function renderBastions() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Bastions /></QueryClientProvider>);
}

it("creates a bastion with a verified host pin and shows only public values", async () => {
  const created: Bastion = { ...jump, id: "b2", name: "Edge", targets: [] };
  let list: Bastion[] = [];
  vi.mocked(request).mockImplementation((method) => {
    if (method === "POST") { list = [created]; return Promise.resolve(created); }
    return Promise.resolve(list);
  });
  renderBastions();
  expect(await screen.findByText(/No bastions/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Add bastion" }));
  const dialog = screen.getByRole("dialog", { name: "Add bastion" });
  expect(within(dialog).getByText(/Do not copy it from ssh-keyscan/)).toBeInTheDocument();
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Edge" } });
  fireEvent.change(within(dialog).getByLabelText("SSH address"), { target: { value: "jump.example" } });
  fireEvent.change(within(dialog).getByLabelText("Forwarding user"), { target: { value: "cove-jump" } });
  fireEvent.change(within(dialog).getByLabelText("Verified bastion host public key"), { target: { value: "-----BEGIN OPENSSH PRIVATE KEY-----" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add bastion" }));
  expect(within(dialog).getByText(/This is a private key/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.change(within(dialog).getByLabelText("Verified bastion host public key"), { target: { value: hostKey } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add bastion" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/bastions", { name: "Edge", address: "jump.example", port: 22, user: "cove-jump", host_key: hostKey }));
  expect(await screen.findByText("Generated public key")).toBeInTheDocument();
  expect(screen.getByText("ssh-ed25519 AAAAgenerated cove")).toBeInTheDocument();
  expect(screen.getByText(/permits no forwarding/)).toBeInTheDocument();
  expect(screen.getByText(/PermitOpen none/)).toBeInTheDocument();
  expect(screen.queryByText(/PRIVATE KEY/)).not.toBeInTheDocument();
});

it("blocks deleting a bastion that nodes still use", async () => {
  vi.mocked(request).mockResolvedValue([jump]);
  renderBastions();
  fireEvent.click(await screen.findByRole("button", { name: "Delete Jump" }));
  const dialog = screen.getByRole("dialog", { name: "Delete Jump?" });
  expect(within(dialog).getByText("Nodes still use this bastion")).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Delete bastion" })).toBeDisabled();
});

it("shows the API refusal when nodes were assigned meanwhile", async () => {
  vi.mocked(request).mockImplementation((method) => (method === "DELETE" ? Promise.reject(new ApiError(409, "detach this bastion's nodes before removing it")) : Promise.resolve([{ ...jump, targets: [] }])));
  renderBastions();
  fireEvent.click(await screen.findByRole("button", { name: "Delete Jump" }));
  const dialog = screen.getByRole("dialog", { name: "Delete Jump?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete bastion" }));
  expect(await within(dialog).findByText("detach this bastion's nodes before removing it")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("DELETE", "/api/bastions/b1");
});
