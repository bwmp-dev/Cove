import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { CloudflareConnect } from "./CloudflareConnect";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Could not connect" }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: false }) }));
afterEach(() => vi.clearAllMocks());

function mount() { render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><CloudflareConnect /></QueryClientProvider>); }

it("discovers multiple domains with one token and imports only the selection", async () => {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/cloudflare/oauth") return Promise.resolve({ configured: false });
    if (path === "/api/cloudflare/connections") return Promise.resolve(method === "GET" ? [] : { id: "personal" });
    return Promise.resolve(method === "GET" ? [{ id: "one", name: "example.com" }, { id: "two", name: "example.net" }] : { connected: 1 });
  });
  mount();
  expect(screen.getByRole("button", { name: "Connect with Cloudflare" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Use API token" }));
  fireEvent.change(screen.getByLabelText("API token"), { target: { value: "test-secret-token" } });
  fireEvent.click(screen.getByRole("button", { name: "Discover domains" }));
  fireEvent.click(await screen.findByLabelText("example.net"));
  expect(screen.getByLabelText("example.com")).not.toBeChecked();
  fireEvent.click(screen.getByRole("button", { name: "Connect selected domains (1)" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/cloudflare/connections/personal/zones", { zones: ["two"] }));
  await screen.findByRole("button", { name: "Use API token" });
  expect(screen.queryByLabelText("API token")).not.toBeInTheDocument();
});

it("keeps provider errors visible and does not enable an empty import", async () => {
  vi.mocked(request).mockImplementation((_method, path) => {
    if (path === "/api/cloudflare/oauth") return Promise.resolve({ configured: true });
    if (path === "/api/cloudflare/connections") return Promise.resolve([{ id: "personal", name: "My account", kind: "oauth" }]);
    return Promise.reject(new Error("Provider failed"));
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Choose domains" }));
  await screen.findByRole("alert");
  expect(screen.getByRole("button", { name: "Connect selected domains" })).toBeDisabled();
  expect(screen.queryByLabelText("Client secret")).not.toBeInTheDocument();
});

it("replaces a token on its existing personal connection", async () => {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/cloudflare/oauth") return Promise.resolve({ configured: true });
    if (path === "/api/cloudflare/connections") return Promise.resolve([{ id: "personal", name: "My account", kind: "token" }]);
    return Promise.resolve(method === "PUT" ? { id: "personal" } : []);
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Replace token" }));
  fireEvent.change(screen.getByLabelText("API token"), { target: { value: "replacement-token" } });
  fireEvent.click(screen.getByRole("button", { name: "Discover domains" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/cloudflare/connections/personal/credentials", { name: "My account", token: "replacement-token" }));
});
