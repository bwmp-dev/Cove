import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ConfigurationDraft } from "../api/project-configuration";
import { ConfigurationDrafts } from "./ConfigurationDrafts";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({
  useBlocker: () => ({ status: "idle" }),
  Link: ({ children, params }: { children: ReactNode; params: { operationId: string } }) => <a href={`/operations/${params.operationId}`}>{children}</a>,
}));
afterEach(() => vi.clearAllMocks());
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const secret = "api_version: cove/v1\nproject: Shop\nvariables:\n  TOKEN: hunter2\n";
const change = { resource: "web", action: "update" as const, fields: ["routes", "environment"], diffs: [{ field: "routes", before: "[]", after: '[{"domain":"shop.example.com"}]' }] };
const ready: ConfigurationDraft = { id: "d1", base_version: "v1", changes: [change], created_at: "2026-11-21T10:00:00Z", applied_at: null, operation_ids: null };
const stale: ConfigurationDraft = { ...ready, id: "d0", base_version: "v0", created_at: "2026-11-20T10:00:00Z" };

function setup(drafts: ConfigurationDraft[], handlers: Record<string, (body?: unknown) => Promise<unknown>> = {}) {
  let list = drafts;
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const key = `${method} ${path}`;
    if (handlers[key]) return handlers[key](body);
    if (key === "GET /api/projects/p/configuration/drafts") return Promise.resolve(list);
    if (key === "POST /api/projects/p/configuration/drafts") {
      list = [{ ...ready, id: "d2", created_at: "2026-11-22T10:00:00Z" }, ...list];
      return Promise.resolve({ id: "d2", base_version: "v1", changes: [change] });
    }
    return Promise.reject(new Error(key));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ConfigurationDrafts projectId="p" currentVersion="v1" online />
    </QueryClientProvider>,
  );
  return client;
}

function cacheText(client: QueryClient) {
  return JSON.stringify([client.getQueryCache().getAll().map((query) => query.state.data), client.getMutationCache().getAll().map((mutation) => [mutation.state.variables, mutation.state.data])]);
}

it("saves a pasted cove.yaml as a draft without keeping its contents anywhere", async () => {
  const user = userEvent.setup();
  const client = setup([]);
  expect(await screen.findByText(/No drafts/)).toBeInTheDocument();
  const editor = screen.getByLabelText("cove.yaml");
  await user.click(editor);
  await user.paste(secret);
  await user.click(screen.getByRole("button", { name: "Save draft" }));

  expect(await screen.findByText("Draft saved")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p/configuration/drafts", { content: secret });
  expect(editor).toHaveValue("");
  expect(await screen.findByRole("list", { name: "Configuration drafts" })).toBeInTheDocument();
  expect(cacheText(client)).not.toContain("hunter2");
});

it("lists safe diffs, blocks out-of-date drafts and applies after confirmation", async () => {
  const user = userEvent.setup();
  setup([stale, ready], {
    "POST /api/projects/p/configuration/drafts/d1/apply": () => Promise.resolve({ id: "d1", operation_ids: ["op-9"], state: "saved" }),
  });
  const list = await screen.findByRole("list", { name: "Configuration drafts" });
  const [first, second] = within(list).getAllByRole("listitem").filter((item) => item.parentElement === list);
  expect(within(first!).getByText("Ready to apply")).toBeInTheDocument();
  expect(within(first!).getByText("shop.example.com", { exact: false })).toBeInTheDocument();
  expect(within(first!).getByText("environment")).toBeInTheDocument();
  expect(within(second!).getByText("Out of date")).toBeInTheDocument();
  expect(within(second!).getByRole("button", { name: "Review and apply" })).toBeDisabled();

  await user.click(within(first!).getByRole("button", { name: "Review and apply" }));
  const dialog = screen.getByRole("dialog", { name: "Apply this draft?" });
  expect(within(dialog).getByText(/does not deploy anything/)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Apply draft" }));

  expect(await screen.findByText("Draft applied")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Route operation 1" })).toHaveAttribute("href", "/operations/op-9");
});

it("explains a stale apply conflict without closing the review", async () => {
  const user = userEvent.setup();
  setup([ready], {
    "POST /api/projects/p/configuration/drafts/d1/apply": () => Promise.reject(new ApiError(409, "The configuration changed after this draft was saved.")),
  });
  await user.click(await screen.findByRole("button", { name: "Review and apply" }));
  await user.click(screen.getByRole("button", { name: "Apply draft" }));
  const dialog = screen.getByRole("dialog", { name: "Apply this draft?" });
  expect(await within(dialog).findByText(/save the draft again from the current cove.yaml/)).toBeInTheDocument();
});

it("deletes a draft after confirmation", async () => {
  const user = userEvent.setup();
  const remove = vi.fn(() => Promise.resolve({ ok: true }));
  setup([ready], { "DELETE /api/projects/p/configuration/drafts/d1": remove });
  await user.click(await screen.findByRole("button", { name: "Delete" }));
  await user.click(screen.getByRole("button", { name: "Delete draft" }));
  await waitFor(() => expect(remove).toHaveBeenCalled());
});

it("shows the server's validation error when saving fails", async () => {
  const user = userEvent.setup();
  setup([], { "POST /api/projects/p/configuration/drafts": () => Promise.reject(new ApiError(400, "Unknown field spec at line 3.")) });
  await user.click(await screen.findByLabelText("cove.yaml"));
  await user.paste("spec: {}");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("Unknown field spec at line 3.")).toBeInTheDocument();
  expect(screen.getByLabelText("cove.yaml")).toHaveValue("spec: {}");
});
