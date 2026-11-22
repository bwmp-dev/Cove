import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Clone } from "./Clone";
import { request } from "../api/client";

const navigate = vi.hoisted(() => vi.fn());
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigate }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Copy failed" }));
afterEach(() => vi.clearAllMocks());

it("excludes secrets by default and opens the cloned resource", async () => {
  vi.mocked(request).mockResolvedValue({ id: "copy" });
  render(<QueryClientProvider client={new QueryClient()}><Clone kind="resource" id="source" name="App" /></QueryClientProvider>);
  expect(screen.getByRole("checkbox")).not.toBeChecked();
  fireEvent.change(screen.getByLabelText("Copy name"), { target: { value: "Staging" } });
  fireEvent.click(screen.getByRole("button", { name: "Clone resource" }));
  await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/resources/$resourceId", params: { resourceId: "copy" } }));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/source/clone", { name: "Staging", include_secrets: false });
});

it("supports explicitly copying credentials and reports failures without navigating", async () => {
  vi.mocked(request).mockRejectedValue(new Error("conflict"));
  render(<QueryClientProvider client={new QueryClient()}><Clone kind="project" id="source" name="Project" /></QueryClientProvider>);
  fireEvent.click(screen.getByRole("checkbox"));
  fireEvent.click(screen.getByRole("button", { name: "Clone project" }));
  await screen.findByText("Copy failed");
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/source/clone", { name: "Project copy", include_secrets: true });
  expect(navigate).not.toHaveBeenCalled();
});
