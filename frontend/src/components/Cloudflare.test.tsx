import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { CloudflareSettings } from "./Cloudflare";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Could not save" }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: false, team_id: "one", teams: [
  { id: "one", name: "First team", role: "owner", grants: [] },
  { id: "two", name: "Second team", role: "member", grants: [{ scope: "team", role: "editor" }] },
  { id: "viewer", name: "View only", role: "member", grants: [] },
] }) }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
afterEach(() => vi.clearAllMocks());

const zone = { zone_id: "a".repeat(32), domain: "example.com", can_manage: true, enabled: true, proxied: false, nodes: {}, shares: [{ team_id: "one", domains: ["app.example.com"] }] };

it("assigns the root domain and every subdomain to multiple selected teams", async () => {
  vi.mocked(request).mockImplementation((method, path) => Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : method === "GET" ? [zone] : { ok: true }));
  render(<QueryClientProvider client={new QueryClient()}><CloudflareSettings /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "Share domains" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Full domain access for First team" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Full domain access for Second team" }));
  expect(screen.queryByRole("checkbox", { name: "Full domain access for View only" })).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox", { name: "First team" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByRole("button", { name: "Share domains" });
  expect(request).toHaveBeenCalledWith("PUT", `/api/cloudflare/${zone.zone_id}/shares`, { shares: [
    { team_id: "one", domains: ["example.com", "*.example.com"] },
    { team_id: "two", domains: ["example.com", "*.example.com"] },
  ] });
});

it("saves explicit domain grants for multiple teams and supports removing them", async () => {
  vi.mocked(request).mockImplementation((method, path) => Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : method === "GET" ? [zone] : { ok: true }));
  render(<QueryClientProvider client={new QueryClient()}><CloudflareSettings /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "Share domains" }));
  expect(screen.getByLabelText("First team")).toHaveValue("app.example.com");
  expect(screen.queryByLabelText("View only")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("First team"), { target: { value: "" } });
  fireEvent.change(screen.getByLabelText("Second team"), { target: { value: "api.example.com\n*.staging.example.com" } });
  expect(screen.getByRole("button", { name: "Close" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByRole("button", { name: "Share domains" });
  expect(request).toHaveBeenCalledWith("PUT", `/api/cloudflare/${zone.zone_id}/shares`, { shares: [{ team_id: "two", domains: ["api.example.com", "*.staging.example.com"] }] });
});

it("shows a recipient its granted domains without owner controls", async () => {
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : [{ ...zone, can_manage: false }]));
  render(<QueryClientProvider client={new QueryClient()}><CloudflareSettings /></QueryClientProvider>);
  expect(await screen.findByText("Shared with this team: app.example.com")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Share domains" })).not.toBeInTheDocument();
  expect(screen.queryByLabelText("API token")).not.toBeInTheDocument();
});
