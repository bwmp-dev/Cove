import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { previewKeys, type EnvironmentPreview, type PreviewCIInputs } from "../api/previews";
import { PreviewActions } from "./PreviewActions";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>,
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
// Like browsers, showing the dialog focuses its autofocus element.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
  this.querySelector<HTMLElement>("[autofocus]")?.focus();
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only; the identities, revisions and digests are made up.
const web = "a".repeat(32);
const jobs = "b".repeat(32);
const ref = (repo: string, fill: string) => `registry.example.test:5000/team/${repo}@sha256:${fill.repeat(64)}`;
const first: PreviewCIInputs = {
  identity: "1".repeat(64),
  revision: "0123456789abcdef0123456789abcdef01234567",
  recipe_version: 7,
  images: { [web]: { web: ref("web", "c"), worker: ref("worker", "d") }, [jobs]: { cron: ref("cron", "e") } },
};
const second: PreviewCIInputs = { identity: "2".repeat(64), revision: "f".repeat(64), recipe_version: 7, images: { [web]: { web: ref("web", "9"), worker: ref("worker", "8") }, [jobs]: { cron: ref("cron", "7") } } };

// The list row: metadata only, no CI inputs, and nothing created yet.
const listed: EnvironmentPreview = {
  id: "pv1",
  project_id: "p",
  environment_id: "",
  base_environment_id: "prod",
  suffix: "pr42",
  slug: "pr42",
  state: "awaiting_approval",
  stage: "approval",
  detail: "",
  pinned: false,
  access: "private",
  delete_reason: "",
  ci: true,
  last_activity_at: "2026-11-24T00:00:00Z",
  expires_at: null,
  created_at: "2026-11-24T00:00:00Z",
  branches: [],
  permissions: ["read", "operate", "edit"],
};
const detailPath = "/api/environment-previews/pv1";
const approvePath = "/api/environment-previews/pv1/approve";

type Reply = unknown;

// Each detail read takes the next queued reply and keeps returning the last one.
function serve(details: Reply[], approve: (body: unknown) => Reply = () => ({ ok: true })) {
  const queue = [...details];
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const settle = (reply: Reply) => (reply instanceof Error ? Promise.reject(reply) : Promise.resolve(reply));
    if (method === "GET" && path === detailPath) return settle(queue.length > 1 ? queue.shift() : queue[0]);
    if (method === "GET" && path === "/api/workloads") return Promise.resolve([{ id: web, name: "storefront", permissions: ["read"], project_id: "p", node_id: "n" }]);
    if (method === "POST" && path === approvePath) return settle(approve(body));
    if (method === "GET") return Promise.resolve([]);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderActions(preview: EnvironmentPreview = listed, client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  render(
    <QueryClientProvider client={client}>
      <PreviewActions preview={preview} />
    </QueryClientProvider>,
  );
  return client;
}

const pending = (inputs: PreviewCIInputs | undefined, extra: Partial<EnvironmentPreview> = {}): EnvironmentPreview => ({ ...listed, resources: [], ci_inputs: inputs, ...extra });
const approvals = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "POST" && path === approvePath);

function openReview() {
  fireEvent.click(screen.getByRole("button", { name: "Review and approve" }));
  return screen.getByRole("dialog", { name: "Review CI inputs for pr42" });
}

function expectInputs(dialog: HTMLElement, inputs: PreviewCIInputs) {
  expect(within(dialog).getByText(inputs.revision)).toBeInTheDocument();
  expect(dialog).toHaveTextContent(`Version ${inputs.recipe_version}, frozen for this preview`);
  const list = within(dialog).getByRole("list", { name: "Images to run" });
  for (const [resource, services] of Object.entries(inputs.images)) {
    expect(list).toHaveTextContent(resource);
    for (const [service, reference] of Object.entries(services)) expect(within(list).getByText(reference, { exact: false })).toHaveTextContent(`${service} ${reference}`);
  }
}

it("shows the complete input map of a new CI preview before anything exists and approves exactly that identity", async () => {
  serve([pending(first)]);
  renderActions();
  const dialog = openReview();
  expect(within(dialog).getByRole("button", { name: "Cancel" })).toHaveFocus();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  expect(confirm).toBeDisabled();
  await waitFor(() => expect(confirm).toBeEnabled());
  expectInputs(dialog, first);
  expect(dialog).toHaveTextContent("Approving creates the preview pr42 from these images.");
  expect(dialog).not.toHaveTextContent("Image updates keep the preview's data");
  // A known resource shows its name and still its ID; an unnamed one shows only its ID.
  expect(await within(dialog).findByRole("link", { name: "storefront" })).toBeInTheDocument();
  expect(within(dialog).getByRole("link", { name: jobs })).toBeInTheDocument();
  expect(within(dialog).getByText(web)).toBeInTheDocument();

  fireEvent.click(confirm);
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(approvals()).toEqual([["POST", approvePath, { ci_identity: first.identity }]]);
});

it("reads the current inputs on every opening and never unlocks a cached older identity", async () => {
  let release: (value: EnvironmentPreview) => void = () => undefined;
  serve([new Promise((resolve) => (release = resolve))]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(previewKeys.detail("pv1"), pending(first));
  renderActions(listed, client);
  const dialog = openReview();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  expect(within(dialog).getByRole("status", { name: "Loading the current CI inputs" })).toBeInTheDocument();
  expect(dialog).not.toHaveTextContent(first.revision);
  fireEvent.click(confirm);
  expect(approvals()).toEqual([]);

  release(pending(second));
  await waitFor(() => expect(confirm).toBeEnabled());
  expectInputs(dialog, second);
  expect(dialog).not.toHaveTextContent(ref("web", "c"));
  fireEvent.click(confirm);
  await waitFor(() => expect(approvals()).toEqual([["POST", approvePath, { ci_identity: second.identity }]]));
});

it("says an existing preview is being updated", async () => {
  serve([pending(first, { environment_id: "env1", state: "awaiting_approval" })]);
  renderActions({ ...listed, environment_id: "env1" });
  const dialog = openReview();
  await waitFor(() => expect(within(dialog).getByRole("button", { name: "Approve CI inputs" })).toBeEnabled());
  expect(dialog).toHaveTextContent("Approving updates the existing preview pr42 to these images.");
  expect(dialog).toHaveTextContent("preparation that was interrupted earlier may resume");
  expect(dialog).not.toHaveTextContent(/always|production data is copied/i);
});

it("keeps the confirmation disabled after a failed read until Retry succeeds", async () => {
  serve([new ApiError(503, "The control plane is temporarily unavailable."), pending(first)]);
  renderActions();
  const dialog = openReview();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  expect(await within(dialog).findByText("Could not load the current CI inputs")).toBeInTheDocument();
  expect(confirm).toBeDisabled();
  fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(confirm).toBeEnabled());
  expectInputs(dialog, first);
});

it.each([
  ["missing inputs", pending(undefined)],
  ["a mutable tag", pending({ ...first, images: { [web]: { web: "registry.example.test/team/web:latest" } } })],
  ["a tag beside the digest", pending({ ...first, images: { [web]: { web: `registry.example.test/team/web:v1@sha256:${"c".repeat(64)}` } } })],
  ["a resource without services", pending({ ...first, images: { ...first.images, [jobs]: {} } })],
  ["no resources", pending({ ...first, images: {} })],
  ["a short revision", pending({ ...first, revision: "0123456" })],
  ["a malformed identity", pending({ ...first, identity: "abc" })],
  ["no recipe version", pending({ ...first, recipe_version: 0 })],
])("refuses to approve %s", async (_label, detail) => {
  serve([detail]);
  renderActions();
  const dialog = openReview();
  expect(await within(dialog).findByText("These CI inputs cannot be reviewed")).toBeInTheDocument();
  expect(within(dialog).queryByRole("list", { name: "Images to run" })).not.toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Approve CI inputs" })).toBeDisabled();
});

it("refuses when the preview is no longer awaiting approval", async () => {
  serve([pending(first, { state: "creating", stage: "deploy" })]);
  renderActions();
  const dialog = openReview();
  expect(await within(dialog).findByText("Nothing to approve")).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Approve CI inputs" })).toBeDisabled();
});

it.each([
  ["an unknown state", { state: "paused" }],
  ["no state", { state: undefined }],
])("refuses safely when the preview reports %s", async (_label, extra) => {
  serve([pending(first, extra as unknown as Partial<EnvironmentPreview>)]);
  renderActions();
  const dialog = openReview();
  expect(await within(dialog).findByText("Nothing to approve")).toBeInTheDocument();
  expect(dialog).toHaveTextContent("This preview is in an unknown state, not awaiting approval.");
  expect(within(dialog).queryByRole("list", { name: "Images to run" })).not.toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  expect(confirm).toBeDisabled();
  fireEvent.click(confirm);
  expect(approvals()).toEqual([]);
});

it("shows a stale-identity refusal and requires a new confirmation for the replacement inputs", async () => {
  serve([pending(first), pending(second)], () => new ApiError(409, "The CI inputs changed; review them again."));
  renderActions();
  const dialog = openReview();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);

  expect(await within(dialog).findByText("The CI inputs changed; review them again.")).toBeInTheDocument();
  await waitFor(() => expectInputs(dialog, second));
  expect(dialog).toHaveTextContent("Newer CI inputs replaced the ones you reviewed.");
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  expect(approvals()).toEqual([["POST", approvePath, { ci_identity: first.identity }]]);
  expect(dialog).not.toHaveTextContent(/approved|deployed/i);

  vi.mocked(request).mockClear();
  serve([pending(second)]);
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);
  await waitFor(() => expect(approvals()).toEqual([["POST", approvePath, { ci_identity: second.identity }]]));
});

it("reports other approval failures without closing", async () => {
  serve([pending(first)], () => new ApiError(500, "The server could not complete the request."));
  renderActions();
  const dialog = openReview();
  const confirm = within(dialog).getByRole("button", { name: "Approve CI inputs" });
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);
  expect(await within(dialog).findByText("Could not approve the CI inputs")).toBeInTheDocument();
  expect(dialog).toHaveTextContent("The server could not complete the request.");
});

it("closes with Cancel or Escape without approving and returns focus to the row action", async () => {
  serve([pending(first)]);
  renderActions();
  let dialog = openReview();
  await waitFor(() => expect(within(dialog).getByRole("button", { name: "Approve CI inputs" })).toBeEnabled());
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Review and approve" })).toHaveFocus();

  dialog = openReview();
  fireEvent(dialog, new Event("close"));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Review and approve" })).toHaveFocus();
  expect(approvals()).toEqual([]);
});

it.each([
  ["viewers", ["read"]],
  ["operators", ["read", "operate"]],
])("offers no approval to %s", (_label, permissions) => {
  serve([pending(first)]);
  renderActions({ ...listed, permissions: permissions as EnvironmentPreview["permissions"] });
  expect(screen.queryByRole("button", { name: /approve/i })).not.toBeInTheDocument();
});

it("disables the review while offline", () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve([pending(first)]);
  renderActions();
  expect(screen.getByRole("button", { name: "Review and approve" })).toBeDisabled();
});

it("keeps the confirmation disabled when the current read no longer grants edit", async () => {
  serve([pending(first, { permissions: ["read"] })]);
  renderActions();
  const dialog = openReview();
  await waitFor(() => expectInputs(dialog, first));
  expect(within(dialog).getByRole("button", { name: "Approve CI inputs" })).toBeDisabled();
});

it("keeps Git preview approval a single bodyless request", async () => {
  serve([]);
  renderActions({ ...listed, ci: false });
  expect(screen.queryByRole("button", { name: "Review and approve" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
  await waitFor(() => expect(approvals()).toHaveLength(1));
  expect(approvals()[0]).toEqual(["POST", approvePath]);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});
