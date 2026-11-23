import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { TemplateRepository } from "../api/templates";
import { TemplateRepositories } from "./TemplateRepositories";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const repository: TemplateRepository = {
  id: "repo-1",
  name: "Community templates",
  kind: "git",
  url: "https://github.com/example/templates.git",
  ref: "main",
  revision: "0123456789abcdef0123456789abcdef01234567",
  error: "",
  checked_at: "2026-11-22T10:00:00Z",
  next_refresh_at: "2026-11-22T16:00:00Z",
  refreshing: false,
  template_count: 3,
};

function renderManager(list: () => TemplateRepository[], mutate: (method: string, path: string, body: unknown) => Promise<unknown> = () => Promise.resolve({})) {
  vi.mocked(request).mockImplementation((method, path, body) => (method === "GET" ? Promise.resolve(path === "/api/templates" ? { templates: [] } : { repositories: list() }) : mutate(method, path, body)));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TemplateRepositories scope={{ kind: "team", teamId: "team-1" }} />
    </QueryClientProvider>,
  );
}

const calls = (method: string) => vi.mocked(request).mock.calls.filter(([m]) => m === method);

it("shows the reference separately from the resolved revision, with status and counts", async () => {
  renderManager(() => [repository]);
  expect(await screen.findByText("Community templates")).toBeInTheDocument();
  expect(screen.getByText("main")).toBeInTheDocument();
  expect(screen.getByTitle(repository.revision)).toHaveTextContent("0123456789ab");
  expect(screen.getByText("3 templates")).toBeInTheDocument();
  expect(screen.getByText("Valid")).toBeInTheDocument();
  expect(calls("GET")[0]?.[1]).toBe("/api/teams/team-1/template-repositories");
});

it("validates a new source before sending and requires a Git reference", async () => {
  renderManager(() => [], () => Promise.resolve({ id: "repo-2" }));
  fireEvent.click(await screen.findByRole("button", { name: "Add source" }));
  const form = screen.getByRole("heading", { name: "Add template source" }).closest("form");
  if (!form) throw new Error("form");
  fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "Private" } });
  fireEvent.change(within(form).getByLabelText("URL"), { target: { value: "https://user:token@git.example.com/repo.git" } });
  fireEvent.click(within(form).getByRole("button", { name: "Add source" }));
  expect(await screen.findByText(/Template sources must be public/)).toBeInTheDocument();
  expect(screen.getByText(/Enter a branch, tag or commit/)).toBeInTheDocument();
  expect(calls("POST")).toHaveLength(0);
  fireEvent.change(within(form).getByLabelText("URL"), { target: { value: "https://git.example.com/repo.git" } });
  fireEvent.change(within(form).getByLabelText("Reference"), { target: { value: "v1.0" } });
  fireEvent.click(within(form).getByRole("button", { name: "Add source" }));
  await waitFor(() => expect(calls("POST")).toHaveLength(1));
  expect(calls("POST")[0]).toEqual(["POST", "/api/teams/team-1/template-repositories", { name: "Private", kind: "git", url: "https://git.example.com/repo.git", ref: "v1.0" }]);
});

it("reports a failed refresh, reloads the list and keeps the last valid revision", async () => {
  let failed = false;
  renderManager(
    () => [failed ? { ...repository, error: "Fetch or template validation failed; previous validated snapshot retained." } : repository],
    () => {
      failed = true;
      return Promise.reject(new ApiError(422, "Catalogue refresh failed; last validated revision was retained"));
    },
  );
  fireEvent.click(await screen.findByRole("button", { name: "Refresh Community templates" }));
  expect(await screen.findByText("Refresh failed")).toBeInTheDocument();
  expect(await screen.findByText("Refresh failed, last valid revision kept")).toBeInTheDocument();
  expect(screen.getByTitle(repository.revision)).toBeInTheDocument();
  expect(calls("POST")[0]).toEqual(["POST", "/api/teams/team-1/template-repositories/repo-1/refresh", {}]);
});

it("asks for confirmation before removing a source", async () => {
  let list = [repository];
  renderManager(
    () => list,
    () => {
      list = [];
      return Promise.resolve({ id: "repo-1" });
    },
  );
  fireEvent.click(await screen.findByRole("button", { name: "Remove Community templates" }));
  expect(calls("DELETE")).toHaveLength(0);
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText(/keep their configuration/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove source" }));
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]?.[1]).toBe("/api/teams/team-1/template-repositories/repo-1");
  expect(await screen.findByText("No template sources")).toBeInTheDocument();
});

it("stops offering Add at the scope limit", async () => {
  renderManager(() => Array.from({ length: 8 }, (_, index) => ({ ...repository, id: `repo-${index}`, name: `Source ${index}` })));
  expect(await screen.findByRole("button", { name: "Add source" })).toBeDisabled();
  expect(screen.getByText(/maximum of 8 sources/)).toBeInTheDocument();
});

it("polls a newly added source until its first check, then stops", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let checked = false;
    renderManager(() => [checked ? repository : { ...repository, revision: "", checked_at: null, template_count: 0 }]);
    expect(await screen.findByText("Waiting for first check")).toBeInTheDocument();
    expect(screen.getByText("Not resolved yet")).toBeInTheDocument();
    expect(screen.queryByText("Valid")).not.toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(5_000);
    await waitFor(() => expect(calls("GET")).toHaveLength(2));
    checked = true;
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await screen.findByText("Valid")).toBeInTheDocument();
    const settled = calls("GET").length;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(calls("GET")).toHaveLength(settled);
  } finally {
    vi.useRealTimers();
  }
});

it("does not poll a source that has been checked and is not refreshing", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    renderManager(() => [repository]);
    expect(await screen.findByText("Valid")).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(15_000);
    expect(calls("GET")).toHaveLength(1);
  } finally {
    vi.useRealTimers();
  }
});
