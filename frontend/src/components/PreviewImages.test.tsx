import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { PreviewImages } from "./PreviewImages";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no CI system or registry is contacted, and the token is made up.
const token = "fixture-ci-token-0123456789abcdef";
const path = "/api/projects/p/preview-images";
const api = { id: "api", name: "api", services: ["web", "worker"], registry: { id: "r1", server: "registry.example.test" }, eligible: true, reason: "" };
const db = { id: "db", name: "db", services: ["db"], registry: null, eligible: false, reason: "select a team registry connection on the resource first" };
const saved = { resource_ids: ["api"], recipe_version: 3, matches_scope: true, expires_at: "2026-12-24T00:00:00Z", created_at: "2026-11-24T00:00:00Z", last_received_at: null };

function status(extra: Record<string, unknown> = {}) {
  return { configuration: null, resources: [api, db], endpoint: "/hooks/preview-images/p", can_manage: true, setup_reason: "", max_resources: 16, max_preview_keys: 1024, max_deliveries: 4096, used_preview_keys: 0, retained_deliveries: 0, ...extra };
}

function serve(handler: (method: string, body?: unknown) => unknown) {
  vi.mocked(request).mockImplementation((method: string, _path: string, body?: unknown) => {
    try {
      return Promise.resolve(handler(method, body));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function renderSection() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <PreviewImages projectId="p" />
    </QueryClientProvider>,
  );
  return client;
}

const calls = (method: string) => vi.mocked(request).mock.calls.filter(([callMethod]) => callMethod === method);

function expectTokenOnlyOnScreen(client: QueryClient) {
  const cached = JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data));
  const mutations = JSON.stringify(client.getMutationCache().getAll().map((mutation) => [mutation.state.data, mutation.state.variables]));
  expect(cached).not.toContain(token);
  expect(mutations).not.toContain(token);
  expect(JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href).not.toContain(token);
}

it("issues a credential after confirmation with the exact body and shows the token only in local state", async () => {
  let current = status();
  serve((method) => {
    if (method === "PUT") {
      current = status({ configuration: saved });
      return { token, expires_at: "2026-12-24T00:00:00Z" };
    }
    return current;
  });
  const client = renderSection();
  fireEvent.click(await screen.findByRole("checkbox", { name: /^api/ }));
  expect(screen.getByRole("checkbox", { name: /^db/ })).toBeDisabled();
  expect(screen.getByText("select a team registry connection on the resource first")).toBeInTheDocument();
  expect(screen.getByLabelText("Expires after")).toHaveValue(30);
  fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));

  const dialog = screen.getByRole("dialog", { name: "Issue a CI credential?" });
  expect(dialog).toHaveTextContent("run images of their choice in previews of the selected resources, with the runtime variables and secret files the recipe gives those previews");
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Issue credential" }));

  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { resource_ids: ["api"], days: 30 }));
  expect(await screen.findByLabelText("CI credential")).toHaveValue(token);
  expect(screen.getByText("Credential issued")).toBeInTheDocument();
  expect(screen.getByText(/Copy it to your CI secret store before clearing this message\. Cove cannot show it again\./)).toBeInTheDocument();
  expect(document.body.textContent).not.toContain("not stored in this browser");
  expectTokenOnlyOnScreen(client);
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
  expect(await screen.findByRole("button", { name: "Replace credential" })).toBeInTheDocument();
  expectTokenOnlyOnScreen(client);

  fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  expect(screen.queryByLabelText("CI credential")).not.toBeInTheDocument();
  expect(document.body.textContent).not.toContain(token);
});

it("replaces with the same selection and a newly chosen lifetime after warning that the old one stops working", async () => {
  serve((method) => (method === "PUT" ? { token, expires_at: "2027-01-01T00:00:00Z" } : status({ configuration: saved })));
  renderSection();
  expect(await screen.findByRole("checkbox", { name: /^api/ })).toBeChecked();
  expect(screen.getByText("Accepting submissions")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Expires after"), { target: { value: "90" } });
  fireEvent.click(screen.getByRole("button", { name: "Replace credential" }));
  const dialog = screen.getByRole("dialog", { name: "Replace the CI credential?" });
  expect(dialog).toHaveTextContent("The current credential stops working immediately. Existing previews and accepted submissions are kept.");
  fireEvent.click(within(dialog).getByRole("button", { name: "Replace credential" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { resource_ids: ["api"], days: 90 }));
  expect(await screen.findByText("Credential replaced")).toBeInTheDocument();
});

it("revokes after confirmation with a DELETE that has no body", async () => {
  let current = status({ configuration: saved });
  serve((method) => {
    if (method === "DELETE") {
      current = status();
      return { ok: true };
    }
    return current;
  });
  renderSection();
  fireEvent.click(await screen.findByRole("button", { name: "Revoke credential" }));
  const dialog = screen.getByRole("dialog", { name: "Revoke the CI credential?" });
  expect(dialog).toHaveTextContent("Existing previews and submissions that were already accepted are not cancelled.");
  expect(calls("DELETE")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Revoke credential" }));
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]).toEqual(["DELETE", path]);
  expect(await screen.findByText("Credential revoked")).toBeInTheDocument();
  expect(await screen.findByRole("button", { name: "Issue credential" })).toBeInTheDocument();
});

it("keeps the draft when the server refuses, and does not claim nothing changed when the reply is uncertain", async () => {
  let reply: Error = new ApiError(409, "CI resource scope is unavailable: the worker is not ready");
  serve((method) => {
    if (method === "PUT") throw reply;
    return status({ configuration: saved });
  });
  renderSection();
  fireEvent.change(await screen.findByLabelText("Expires after"), { target: { value: "12" } });
  fireEvent.click(screen.getByRole("button", { name: "Replace credential" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Replace the CI credential?" })).getByRole("button", { name: "Replace credential" }));
  expect(await screen.findByText("No credential was issued")).toBeInTheDocument();
  expect(screen.getByText("CI resource scope is unavailable: the worker is not ready")).toBeInTheDocument();
  expect(screen.getByText("Nothing changed and your selection is kept.")).toBeInTheDocument();
  expect(screen.getByLabelText("Expires after")).toHaveValue(12);
  expect(screen.getByRole("checkbox", { name: /^api/ })).toBeChecked();

  reply = new ApiError(503, "Could not issue CI credential");
  fireEvent.click(screen.getByRole("button", { name: "Replace credential" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Replace the CI credential?" })).getByRole("button", { name: "Replace credential" }));
  expect(await screen.findByText("It is not known whether a credential was issued")).toBeInTheDocument();
  expect(screen.getByText(/The previous credential may already be revoked/)).toBeInTheDocument();
  expect(screen.queryByText("Nothing changed and your selection is kept.")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Expires after")).toHaveValue(12);
});

it("treats a 201 reply without a readable token as uncertain", async () => {
  serve((method) => (method === "PUT" ? { ok: true } : status()));
  renderSection();
  fireEvent.click(await screen.findByRole("checkbox", { name: /^api/ }));
  fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Issue a CI credential?" })).getByRole("button", { name: "Issue credential" }));
  expect(await screen.findByText("It is not known whether a credential was issued")).toBeInTheDocument();
  expect(screen.queryByLabelText("CI credential")).not.toBeInTheDocument();
});

it("shows a stale scope and keeps saved ineligible and missing resources selected until they are unselected", async () => {
  serve((method) => (method === "PUT" ? { token, expires_at: "2027-01-01T00:00:00Z" } : status({ configuration: { ...saved, matches_scope: false, resource_ids: ["api", "db", "gone"] } })));
  renderSection();
  expect(await screen.findByText("Submissions are refused")).toBeInTheDocument();
  expect(screen.getByText("Scope changed or expired")).toBeInTheDocument();
  expect(screen.getByText(/configuration, Compose services or registry may have changed, or the credential's owner or the resource's worker may no longer be available/)).toBeInTheDocument();
  const gone = screen.getByRole("checkbox", { name: /^gone/ });
  const database = screen.getByRole("checkbox", { name: /^db/ });
  expect(gone).toBeChecked();
  expect(database).toBeChecked();
  expect(database).toBeEnabled();
  expect(screen.getAllByText(/no longer in the production environment/).length).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: "Replace credential" })).toBeDisabled();
  expect(screen.getByText(/^Unselect db, gone:/)).toBeInTheDocument();

  fireEvent.click(database);
  fireEvent.click(gone);
  expect(screen.getByRole("checkbox", { name: /^db/ })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Replace credential" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Replace the CI credential?" })).getByRole("button", { name: "Replace credential" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { resource_ids: ["api"], days: 30 }));
});

it("is read-only for viewers but still shows the credential metadata", async () => {
  serve(() => status({ configuration: saved, can_manage: false }));
  renderSection();
  expect(await screen.findByText(/needs edit access to the project and its production environment/)).toBeInTheDocument();
  expect(screen.getByText("Accepting submissions")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Resources for CI images" })).toHaveTextContent("registry.example.test");
  expect(screen.queryByRole("button", { name: /Issue credential|Replace credential|Revoke credential/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

it("explains what is missing before a recipe is saved or enabled", async () => {
  serve(() => status({ resources: [], can_manage: false, setup_reason: "Save a preview recipe first." }));
  renderSection();
  expect(await screen.findByText("Save a preview recipe first.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /credential/ })).not.toBeInTheDocument();
});

it("blocks issuing while the recipe is disabled", async () => {
  serve(() => status({ setup_reason: "Enable the preview recipe first.", resources: [{ ...api, eligible: false, reason: "enable the preview recipe first" }] }));
  renderSection();
  expect(await screen.findAllByText("Enable the preview recipe first.")).not.toHaveLength(0);
  expect(screen.getByRole("button", { name: "Issue credential" })).toBeDisabled();
});

it("pauses changes while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => status({ configuration: saved }));
  renderSection();
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(await screen.findByRole("button", { name: "Replace credential" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Revoke credential" })).toBeDisabled();
});

it("explains refused submissions during operations, safe retries and same-identity resubmissions", async () => {
  serve(() => status({ configuration: saved }));
  renderSection();
  expect(await screen.findByText(/a new submission for it is refused with 409 and its delivery_id and sequence stay unused\. Retry once it settles\./)).toBeInTheDocument();
  expect(screen.getByText(/Resending a delivery that was already accepted is safe and changes nothing\. The same images and revision with a higher sequence neither redeploy the preview nor ask for approval again\./)).toBeInTheDocument();
});

it.each([
  ["a missing configuration", () => Object.fromEntries(Object.entries(status()).filter(([key]) => key !== "configuration"))],
  ["a malformed registry on an eligible resource", () => status({ resources: [{ ...api, registry: { id: "r1" } }] })],
])("reports %s as an error instead of offering to issue", async (_name, reply) => {
  serve(reply);
  renderSection();
  expect(await screen.findByText("Could not load CI image preview settings")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /credential/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

it("reports unrecognised metadata as an error without offering actions", async () => {
  serve(() => ({ ...status(), resources: "api" }));
  renderSection();
  expect(await screen.findByText("Could not load CI image preview settings")).toBeInTheDocument();
  expect(screen.getByText(/does not recognise/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /credential/ })).not.toBeInTheDocument();
});

it("stops polling after a failed read until Retry", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    serve(() => {
      throw new ApiError(403, "Access denied. Your account does not have permission for this action.");
    });
    renderSection();
    expect(await screen.findByText("Could not load CI image preview settings")).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(95_000);
    expect(calls("GET")).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(calls("GET")).toHaveLength(2));
  } finally {
    vi.useRealTimers();
  }
});

it("rechecks every 30 seconds while reads succeed", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    serve(() => status());
    renderSection();
    expect(await screen.findByRole("button", { name: "Issue credential" })).toBeInTheDocument();
    expect(calls("GET")).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(30_500);
    await waitFor(() => expect(calls("GET")).toHaveLength(2));
  } finally {
    vi.useRealTimers();
  }
});
