import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { ReleaseComparison } from "./ReleaseComparison";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const releases = { releases: [{ id: "old11111-a", created_at: "2026-11-01T10:00:00Z" }, { id: "new22222-b", created_at: "2026-11-02T10:00:00Z" }] };

const diff = {
  from: "old11111-a",
  to: "new22222-b",
  configuration: [
    { field: "variables.DATABASE_PASSWORD", changed: true },
    { field: "source.credentials", changed: true },
    { field: "compose", changed: false },
    { field: "routes", changed: false },
  ],
  release: {
    from_revision: "1111111111111111111111111111111111111111",
    to_revision: "2222222222222222222222222222222222222222",
    images: [
      { service: "web", from: "ghcr.io/acme/web@sha256:aaa", to: "ghcr.io/acme/web@sha256:bbb", changed: true },
      { service: "worker", from: "ghcr.io/acme/worker@sha256:ccc", to: "ghcr.io/acme/worker@sha256:ccc", changed: false },
    ],
    commits: [{ sha: "2222222222222222222222222222222222222222", subject: "Fix checkout totals" }],
    commits_complete: false,
    commit_message: "Fix checkout totals",
  },
};

function mockApi(operations: unknown[] = [], comparison: () => Promise<unknown> = () => Promise.resolve(diff)) {
  vi.mocked(request).mockImplementation((_method, path) => {
    if (path.endsWith("/releases")) return Promise.resolve(releases);
    if (path.startsWith("/api/operations")) return Promise.resolve(operations);
    if (path.includes("/release-diff")) return comparison();
    return Promise.reject(new Error(`unexpected ${path}`));
  });
}

function renderComparison() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ReleaseComparison workloadId="w1" /></QueryClientProvider>);
}

async function choose(name: string, option: RegExp) {
  fireEvent.click(await screen.findByRole("combobox", { name }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

it("compares two retained releases and shows only change indicators for settings", async () => {
  mockApi();
  renderComparison();
  await choose("From release", /old11111/);
  await choose("To release", /new22222/);
  fireEvent.click(screen.getByRole("button", { name: "Compare" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/release-diff?from=old11111-a&to=new22222-b", undefined, expect.anything()));

  const changed = await screen.findByRole("list", { name: "Changed settings" });
  expect(within(changed).getByText("Variable DATABASE_PASSWORD")).toBeInTheDocument();
  expect(within(changed).getByText("Git access token")).toBeInTheDocument();
  expect(screen.getByText("2 unchanged settings")).toBeInTheDocument();

  expect(screen.getByText("ghcr.io/acme/web@sha256:aaa")).toBeInTheDocument();
  expect(screen.getByText("ghcr.io/acme/web@sha256:bbb")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Commits" })).toHaveTextContent("Fix checkout totals");
  expect(screen.getByText("Commit history is incomplete")).toBeInTheDocument();
  expect(screen.getByText("222222222222")).toBeInTheDocument();
});

it("does not compare while an operation is running on the resource", async () => {
  mockApi([{ id: "op", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: "2026-11-02T10:00:00Z" }]);
  renderComparison();
  await choose("From release", /old11111/);
  await choose("To release", /new22222/);
  expect(await screen.findByText("An operation is running on this resource. Compare when it finishes.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Compare" })).toBeDisabled();
  expect(request).not.toHaveBeenCalledWith("GET", expect.stringContaining("/release-diff"), undefined, expect.anything());
});

it("explains why releases cannot be compared", async () => {
  mockApi([], () => Promise.reject(new ApiError(409, "upgrade and verify the worker to compare retained releases")));
  renderComparison();
  await choose("From release", /old11111/);
  await choose("To release", /new22222/);
  fireEvent.click(screen.getByRole("button", { name: "Compare" }));
  expect(await screen.findByText("Could not compare releases")).toBeInTheDocument();
  expect(screen.getByText(/worker must be upgraded/)).toBeInTheDocument();
});

it("says when there are not enough retained releases", async () => {
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path.endsWith("/releases") ? { releases: [releases.releases[0]] } : []));
  renderComparison();
  expect(await screen.findByText("Only one successful release is retained. Deploy again to compare releases.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Compare" })).not.toBeInTheDocument();
});

it("offers only successful releases, labelled by their Git revision", async () => {
  const reported = [
    { id: "good1111-a", created_at: "2026-11-01T10:00:00Z", state: "succeeded", revision: "aaaaaaa1111111111111111111111111111111111" },
    { id: "fail2222-b", created_at: "2026-11-02T10:00:00Z", state: "failed", revision: "bbbbbbb2222222222222222222222222222222222" },
    { id: "stop3333-c", created_at: "2026-11-03T10:00:00Z", state: "cancelled" },
    { id: "busy4444-d", created_at: "2026-11-04T10:00:00Z", state: "running" },
    { id: "good5555-e", created_at: "2026-11-05T10:00:00Z", state: "succeeded", revision: "ccccccc5555555555555555555555555555555555" },
    { id: "good6666-f", created_at: "2026-11-06T10:00:00Z", state: "succeeded" },
  ];
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path.endsWith("/releases") ? { releases: reported } : []));
  renderComparison();
  fireEvent.click(await screen.findByRole("combobox", { name: "From release" }));
  const names = (await screen.findAllByRole("option")).map((option) => option.textContent ?? "");
  expect(names.filter((name) => name !== "Select a release")).toEqual([
    expect.stringMatching(/release good6666$/),
    expect.stringMatching(/commit ccccccc$/),
    expect.stringMatching(/commit aaaaaaa$/),
  ]);
  expect(screen.queryByText(/did not report whether some releases succeeded/)).not.toBeInTheDocument();
});

it("keeps releases from older workers selectable and explains the upgrade", async () => {
  mockApi();
  renderComparison();
  expect(await screen.findByText(/did not report whether some releases succeeded.*Upgrade the worker/)).toBeInTheDocument();
  await choose("From release", /release old11111/);
  await choose("To release", /release new22222/);
  expect(screen.getByRole("button", { name: "Compare" })).toBeEnabled();
});

it("says when fewer than two retained releases succeeded", async () => {
  const reported = [
    { id: "fail1111-a", created_at: "2026-11-01T10:00:00Z", state: "failed" },
    { id: "good2222-b", created_at: "2026-11-02T10:00:00Z", state: "succeeded" },
  ];
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path.endsWith("/releases") ? { releases: reported } : []));
  renderComparison();
  expect(await screen.findByText("Only one successful release is retained. Deploy again to compare releases.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Compare" })).not.toBeInTheDocument();
});
