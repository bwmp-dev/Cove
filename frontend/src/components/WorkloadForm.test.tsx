import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Project } from "../api/projects";
import type { ResourceDraft } from "../api/templates";
import type { Node } from "../api/types";
import { WorkloadForm } from "./WorkloadForm";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: unknown }) => children }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => vi.unstubAllGlobals());

const node: Node = { id: "n1", name: "worker", address: "10.0.0.2", port: 22, state: "ready", capabilities: null, mode: "ssh", last_checked_at: null };
const project: Project = { id: "p1", name: "Apps", permissions: ["read", "operate", "edit"], environments: [] };
const draft: ResourceDraft = {
  name: "garage",
  compose: "services:\n  garage:\n    image: dxflrs/garage:v2.5.0\n",
  environment: { RPC_SECRET: "a".repeat(64) },
  routes: [],
  file_secrets: [{ name: "rpc", variable: "RPC_SECRET", services: ["garage"], build_services: [] }],
};

it("keeps template file secrets in the create request", async () => {
  const user = userEvent.setup();
  const onSubmit = vi.fn();
  render(<WorkloadForm draft={draft} nodes={[node]} projects={[project]} initialProjectId="p1" initialEnvironmentId="" pending={false} serverError={null} submitLabel="Create resource" cancel={null} onSubmit={onSubmit} />);
  expect(screen.getByText(/mounts these variables as files/)).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "worker" }));
  await user.click(screen.getByRole("button", { name: "Create resource" }));
  await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
  expect(onSubmit.mock.calls[0]?.[0]).toMatchObject({ name: "garage", node_id: "n1", project_id: "p1", file_secrets: draft.file_secrets, environment: draft.environment });
});
