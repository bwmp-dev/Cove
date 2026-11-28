import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { editorAccess, editWithoutSecrets, secretsWithoutEdit, viewerAccess } from "../test/permissions";
import { BuiltinVariables, EnvironmentFileActions, SecretReferenceBuilder } from "./EnvironmentFile";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const workload: Workload = { id: "w1", name: "web app", node_id: "n1", project_id: "p1", permissions: editorAccess, manually_stopped: false, deployed: false, compose: "", environment_keys: ["API_KEY"], source: null, has_source_token: false };

function mutationState(client: QueryClient) {
  return JSON.stringify(client.getMutationCache().getAll().map((mutation) => ({ variables: mutation.state.variables, data: mutation.state.data })));
}

function wrap(children: ReactNode, client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return { client, ...render(<QueryClientProvider client={client}>{children}</QueryClientProvider>) };
}

it("exports only after confirmation and clears the file when dismissed", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue({ dotenv: 'API_KEY="s3cret"\n' });
  const { client } = wrap(<EnvironmentFileActions workload={workload} temporary={false} blocked={false} />);
  await user.click(screen.getByRole("button", { name: "Export .env" }));
  expect(request).not.toHaveBeenCalled();
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("The file contains secret values in plain text")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Export variables" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/variables/export");
  expect(await within(dialog).findByText(/1 variable is ready/)).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Download web-app.env" })).toBeInTheDocument();
  expect(within(dialog).queryByText(/s3cret/)).not.toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(client.getMutationCache().getAll().some((mutation) => mutation.state.data !== undefined)).toBe(false));
  expect(client.getQueryCache().getAll()).toHaveLength(0);
});

it("imports a pasted .env file and reports the saved names", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue({ keys: ["A", "B"] });
  const { client } = wrap(<EnvironmentFileActions workload={workload} temporary={false} blocked={false} />);
  await user.click(screen.getByRole("button", { name: "Import .env" }));
  const dialog = screen.getByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: "Import variables" }));
  expect(within(dialog).getByText("Paste a .env file or choose one.")).toBeInTheDocument();
  await user.click(within(dialog).getByLabelText(".env contents"));
  await user.paste("A=1\nB='$literal'\n");
  await user.click(within(dialog).getByRole("button", { name: "Import variables" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/variables/import", { dotenv: "A=1\nB='$literal'\n" });
  expect(await screen.findByText("Imported 2 variables")).toBeInTheDocument();
  expect(mutationState(client)).not.toContain("literal");
  await user.click(screen.getByRole("button", { name: "Import .env" }));
  expect(within(screen.getByRole("dialog")).getByLabelText(".env contents")).toHaveValue("");
});

it("clears pasted text and a failed import when the dialog is dismissed with Escape", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockRejectedValue(new Error("TOKEN: invalid quoting"));
  const { client } = wrap(<EnvironmentFileActions workload={workload} temporary={false} blocked={false} />);
  await user.click(screen.getByRole("button", { name: "Import .env" }));
  const dialog = screen.getByRole("dialog");
  await user.click(within(dialog).getByLabelText(".env contents"));
  await user.paste("TOKEN='plain-secret\n");
  await user.click(within(dialog).getByRole("button", { name: "Import variables" }));
  expect(await within(dialog).findByText("TOKEN: invalid quoting")).toBeInTheDocument();
  expect(mutationState(client)).not.toContain("plain-secret");
  // jsdom does not close a modal dialog on Escape; browsers dispatch this close event.
  fireEvent(dialog, new Event("close"));
  await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  await user.click(screen.getByRole("button", { name: "Import .env" }));
  const reopened = screen.getByRole("dialog");
  expect(within(reopened).getByLabelText(".env contents")).toHaveValue("");
  expect(within(reopened).queryByText("TOKEN: invalid quoting")).not.toBeInTheDocument();
});

it("disables import and export for preview resources", () => {
  wrap(<EnvironmentFileActions workload={workload} temporary blocked={false} />);
  expect(screen.getByText(/Preview resources cannot import or export saved variables/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Import .env" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Export .env" })).toBeDisabled();
});

it("disables export when nothing is saved", () => {
  wrap(<EnvironmentFileActions workload={{ ...workload, environment_keys: [] }} temporary={false} blocked={false} />);
  expect(screen.getByRole("button", { name: "Import .env" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Export .env" })).toBeDisabled();
  expect(screen.getByText("No variables to export.")).toBeInTheDocument();
});

it("offers import but never export to a custom role that can edit without reading secrets", () => {
  wrap(<EnvironmentFileActions workload={{ ...workload, permissions: editWithoutSecrets }} temporary={false} blocked={false} />);
  expect(screen.getByRole("heading", { name: "Import" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Import .env" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Export .env" })).not.toBeInTheDocument();
  expect(screen.getByText("Exporting needs permission to read secrets.")).toBeInTheDocument();
});

it("offers an audited export but no import to a custom role that reads secrets without editing", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue({ dotenv: 'API_KEY="s3cret"\n' });
  const { client } = wrap(<EnvironmentFileActions workload={{ ...workload, permissions: secretsWithoutEdit }} temporary={false} blocked={false} />);
  expect(screen.queryByRole("button", { name: "Import .env" })).not.toBeInTheDocument();
  expect(screen.getByText("Importing needs edit access.")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Export .env" }));
  const dialog = screen.getByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: "Export variables" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/variables/export");
  expect(await within(dialog).findByText(/1 variable is ready/)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(mutationState(client)).not.toContain("s3cret"));
  expect(client.getQueryCache().getAll()).toHaveLength(0);
});

it("renders nothing for a viewer", () => {
  const { container } = wrap(<EnvironmentFileActions workload={{ ...workload, permissions: viewerAccess }} temporary={false} blocked={false} />);
  expect(container).toBeEmptyDOMElement();
});

it("lists built-in variables and explains when the node does not support them", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockResolvedValue({ available: false, variables: [{ name: "COVE_FQDN", description: "The first public route's hostname." }] });
  wrap(<BuiltinVariables workloadId="w1" />);
  expect(await screen.findByText("Not supported by this resource's node yet")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Built-in variables" }));
  expect(screen.getByText("COVE_FQDN")).toBeInTheDocument();
  expect(screen.getByText("Not added yet")).toBeInTheDocument();
});

it("builds a secret reference from a saved connection", async () => {
  const user = userEvent.setup();
  const onAdd = vi.fn();
  const id = "c".repeat(32);
  vi.mocked(request).mockResolvedValue([{ id, name: "Vault", kind: "vault", url: "https://vault.example.com", project: "", environment: "", namespace: "", available: true }]);
  wrap(<SecretReferenceBuilder onAdd={onAdd} />);
  await user.click(await screen.findByRole("combobox", { name: "Provider" }));
  await user.click(screen.getByRole("option", { name: "Vault · HashiCorp Vault" }));
  expect(screen.getByText(/mount\/path#field\. A KV version 2/)).toBeInTheDocument();
  await user.type(screen.getByLabelText("Variable name"), "DB_PASSWORD");
  await user.type(screen.getByLabelText("Reference"), "secret/app#password");
  expect(screen.getByText(`{{secret.${id}:secret/app#password}}`)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Add to variables" }));
  expect(onAdd).toHaveBeenCalledWith("DB_PASSWORD", `{{secret.${id}:secret/app#password}}`);
});
