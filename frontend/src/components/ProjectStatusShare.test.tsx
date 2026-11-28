import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { ProjectStatusShare } from "./ProjectStatusShare";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};
beforeEach(() => {
  Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn().mockResolvedValue(undefined) }, configurable: true });
});
afterEach(() => vi.clearAllMocks());

const token = "a".repeat(43);
const created = { title: "Public API", expires_at: "2026-12-22T00:00:00Z", dashboard_url: `https://cove.example/shared/${token}`, badge_url: `https://cove.example/status/${token}/badge.svg` };

function renderShare(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  render(
    <QueryClientProvider client={client}>
      <ProjectStatusShare projectId="p1" />
    </QueryClientProvider>,
  );
  return client;
}

it("requires the disclosure before creating, then shows the links once without loading the badge", async () => {
  const user = userEvent.setup();
  let share: object = { enabled: false };
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") return Promise.resolve(share);
    share = { enabled: true, title: created.title, expires_at: created.expires_at, created_at: "2026-11-22T00:00:00Z" };
    return Promise.resolve(created);
  });
  const client = renderShare();
  await user.type(await screen.findByLabelText("Public title"), "Public API");
  const submit = screen.getByRole("button", { name: "Create public link" });
  expect(submit).toBeDisabled();
  await user.click(screen.getByRole("checkbox", { name: /anyone who has the link/ }));
  await user.click(submit);

  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p1/status-share", { title: "Public API", days: 30, acknowledge: true });
  expect(await screen.findByLabelText("Status page link")).toHaveValue(created.dashboard_url);
  expect(screen.getByRole("button", { name: "Copy badge URL" })).toBeInTheDocument();
  expect(document.querySelector("img")).toBeNull();
  // Neither the query nor the mutation cache keeps the one-time links.
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain(token);
  expect(client.getMutationCache().getAll()).toHaveLength(0);

  await user.click(screen.getByRole("button", { name: "I have copied the links" }));
  expect(screen.queryByDisplayValue(created.dashboard_url)).not.toBeInTheDocument();
  expect(await screen.findByText(/Sharing as/)).toBeInTheDocument();
});

it("confirms before rotating and revoking", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") return Promise.resolve({ enabled: true, title: "Public API", expires_at: "2026-12-22T00:00:00Z", created_at: "2026-11-22T00:00:00Z" });
    if (method === "DELETE") return Promise.resolve({ ok: true });
    return Promise.resolve(created);
  });
  renderShare();
  await user.click(await screen.findByRole("button", { name: "Turn off sharing" }));
  const revoke = screen.getByRole("dialog", { name: "Turn off the public status page?" });
  await user.click(within(revoke).getByRole("button", { name: "Turn off sharing" }));
  expect(request).toHaveBeenCalledWith("DELETE", "/api/projects/p1/status-share");

  await user.click(screen.getByRole("button", { name: "Rotate link" }));
  await user.click(screen.getByRole("checkbox", { name: /anyone who has the link/ }));
  await user.click(screen.getByRole("button", { name: "Rotate link" }));
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  const rotate = screen.getByRole("dialog", { name: "Rotate the public link?" });
  await user.click(within(rotate).getByRole("button", { name: "Rotate link" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p1/status-share", { title: "Public API", days: 30, acknowledge: true });
  expect(await screen.findByLabelText("Status page link")).toHaveValue(created.dashboard_url);
});

it("explains an expired link", async () => {
  vi.mocked(request).mockResolvedValue({ enabled: false, title: "Old", expires_at: "2026-01-01T00:00:00Z", created_at: "2025-12-01T00:00:00Z" });
  renderShare();
  expect(await screen.findByText("The previous link expired")).toBeInTheDocument();
});

it("explains a suspended link without an owner and claims it only through the confirmed rotation", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") return Promise.resolve({ enabled: true, title: "Public API", expires_at: "2026-12-22T00:00:00Z", created_at: "2026-11-22T00:00:00Z", ownership_required: true });
    return Promise.resolve(created);
  });
  renderShare();
  expect(await screen.findByText("Sharing is suspended")).toBeInTheDocument();
  expect(screen.getByText(/Rotate the link to make your account responsible for it/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);

  await user.click(screen.getByRole("button", { name: "Rotate link" }));
  await user.click(screen.getByRole("checkbox", { name: /anyone who has the link/ }));
  await user.click(screen.getByRole("button", { name: "Rotate link" }));
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  await user.click(within(screen.getByRole("dialog", { name: "Rotate the public link?" })).getByRole("button", { name: "Rotate link" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p1/status-share", { title: "Public API", days: 30, acknowledge: true });
});

it("explains a turned-off link whose owner lost access instead of calling it expired", async () => {
  vi.mocked(request).mockResolvedValue({ enabled: false, title: "Old", expires_at: "2026-12-22T00:00:00Z", created_at: "2026-11-01T00:00:00Z", ownership_required: true });
  renderShare();
  expect(await screen.findByText("Sharing is suspended")).toBeInTheDocument();
  expect(screen.queryByText("The previous link expired")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create public link" })).toBeInTheDocument();
});
