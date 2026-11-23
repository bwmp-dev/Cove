import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Workload } from "../api/types";
import { PortBindings } from "./PortBindings";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "app", compose: "services: {}", source: null, permissions: ["edit"] } as unknown as Workload;

function renderBindings(target: Workload = workload) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PortBindings workload={target} busy={false} />
    </QueryClientProvider>,
  );
}

it("previews the rewrite and saves only after explicit confirmation with the preview revision", async () => {
  vi.mocked(request).mockImplementation((_method, _path, body) =>
    Promise.resolve({ compose: "services:\n  web:\n    ports:\n      - 10.0.0.2:8080:80\n", changed: 1, revision: "abc", saved: (body as { confirm: boolean }).confirm }),
  );
  renderBindings();
  fireEvent.change(screen.getByLabelText("Bind address"), { target: { value: "10.0.0.2" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview rewrite" }));
  expect(await screen.findByLabelText("Rewritten Compose file")).toHaveTextContent("10.0.0.2:8080:80");
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/port-bindings", { address: "10.0.0.2", confirm: false });
  const save = screen.getByRole("button", { name: "Save rewritten Compose" });
  expect(save).toBeDisabled();
  fireEvent.click(screen.getByRole("checkbox"));
  fireEvent.click(save);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/port-bindings", { address: "10.0.0.2", confirm: true, revision: "abc" }));
  expect(await screen.findByText("Compose file saved")).toBeInTheDocument();
});

it("offers a new preview when the saved Compose changed", async () => {
  vi.mocked(request).mockImplementation((_method, _path, body) =>
    (body as { confirm: boolean }).confirm
      ? Promise.reject(new ApiError(409, "Compose changed; preview the rewrite again"))
      : Promise.resolve({ compose: "x", changed: 2, revision: "r1", saved: false }),
  );
  renderBindings();
  fireEvent.click(screen.getByRole("button", { name: "Preview rewrite" }));
  fireEvent.click(await screen.findByRole("checkbox"));
  fireEvent.click(screen.getByRole("button", { name: "Save rewritten Compose" }));
  expect(await screen.findByText("Compose changed; preview the rewrite again")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Preview again" })).toBeInTheDocument();
});

it("points Git-sourced Compose files to the repository", () => {
  renderBindings({ ...workload, source: { repository: "r", branch: "main", compose_path: "compose.yaml", inline_compose: false } });
  expect(screen.getByText("Compose comes from the Git repository")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Preview rewrite" })).not.toBeInTheDocument();
});
