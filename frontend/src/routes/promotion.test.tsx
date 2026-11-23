import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Operation, Workload, WorkloadSummary } from "../api/types";
import { Promotion } from "./promotion";

const target: Partial<Workload> = { id: "prod", name: "web", project_id: "p1", kind: "service", permissions: ["read", "edit"] };
const source: Partial<WorkloadSummary> = { id: "stage", name: "web", project_id: "p1", kind: "service", environment: "staging", permissions: ["read", "edit"] };

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", async importOriginal => ({ ...await importOriginal<typeof import("../api/client")>(), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => target }));
afterEach(() => vi.clearAllMocks());

function operation(workloadId: string, action: Operation["action"], state: Operation["state"]) {
  return { id: `${workloadId}-${action}`, workload_id: workloadId, action, state, created_at: "2026-11-22T10:00:00Z" } as Operation;
}

function serve(operations: Record<string, () => Promise<Operation[]>>, promote: () => Promise<unknown> = () => Promise.resolve({ id: "op-promote" })) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return promote();
    if (path === "/api/workloads") return Promise.resolve([target, source]);
    if (path.endsWith("/releases")) return Promise.resolve({ releases: [{ id: "rel-1", created_at: "2026-11-22T09:00:00Z" }] });
    const id = new URLSearchParams(path.split("?")[1]).get("workload_id") ?? "";
    return (operations[id] ?? (() => Promise.resolve([])))();
  });
}

async function chooseRelease() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Promotion /></QueryClientProvider>);
  // Select keeps a hidden native select that holds the form value.
  const sourceSelect = () => document.getElementById("promotion-source-native") as HTMLSelectElement;
  const releaseSelect = () => document.getElementById("promotion-release-native") as HTMLSelectElement;
  await waitFor(() => expect(sourceSelect().querySelector('option[value="stage"]')).not.toBeNull());
  fireEvent.change(sourceSelect(), { target: { value: "stage" } });
  await waitFor(() => expect(releaseSelect().querySelector('option[value="rel-1"]')).not.toBeNull());
  fireEvent.change(releaseSelect(), { target: { value: "rel-1" } });
}

it("waits for a running image check on the source before allowing promotion", async () => {
  serve({ stage: () => Promise.resolve([operation("stage", "image_check", "running")]) });
  await chooseRelease();
  const button = screen.getByRole("button", { name: "Promote release" });
  expect(await screen.findByText(/source resource has a check images operation running/)).toBeInTheDocument();
  expect(button).toBeDisabled();
  expect(screen.getByRole("link", { name: "View operation" })).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
});

it("waits for an active operation on the target", async () => {
  serve({ prod: () => Promise.resolve([operation("prod", "deploy", "queued")]) });
  await chooseRelease();
  expect(await screen.findByText(/target resource has a deploy operation queued/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Promote release" })).toBeDisabled();
});

it("does not treat unknown operation status as idle", async () => {
  serve({ stage: () => Promise.reject(new ApiError(500, "operations unavailable")), prod: () => new Promise(() => {}) });
  await chooseRelease();
  expect(await screen.findByText("Could not load source resource operations")).toBeInTheDocument();
  expect(screen.getByText(/Checking for active operations on the target resource/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Promote release" })).toBeDisabled();
});

it("promotes once both resources are idle and keeps the conflict message for a race", async () => {
  serve({}, () => Promise.reject(new ApiError(409, "Wait for active operations on both resources")));
  await chooseRelease();
  const button = screen.getByRole("button", { name: "Promote release" });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/prod/promote", { source_id: "stage", release: "rel-1", registry_repository: "" }));
  expect(await screen.findByText("Wait for active operations on both resources")).toBeInTheDocument();
});
