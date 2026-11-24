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

it("lets the zone owner follow DNS challenge cleanup, including records of deleted resources", async () => {
  const challenges = { more: false, items: [
    { id: "c1", issuer_id: null, workload_id: null, zone_id: zone.zone_id, hostname: "_acme-challenge.gone.example.com", phase: "cleanup", cleanup_requested: true, last_error: "", next_check_at: null, created_at: "2026-11-20T10:00:00Z" },
    { id: "c2", issuer_id: "i1", workload_id: "b".repeat(32), zone_id: zone.zone_id, hostname: "_acme-challenge.app.example.com", phase: "present", cleanup_requested: false, last_error: "", next_check_at: null, created_at: "2026-11-21T10:00:00Z" },
  ] };
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : path === `/api/cloudflare/${zone.zone_id}/certificate-challenges` ? challenges : [zone]));
  render(<QueryClientProvider client={new QueryClient()}><CloudflareSettings /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "DNS challenges for example.com" }));
  expect(screen.getByText("DNS challenge records in example.com")).toBeInTheDocument();
  expect(await screen.findByText("_acme-challenge.gone.example.com")).toBeInTheDocument();
  expect(screen.getByText(/Resource deleted · issuer removed/)).toBeInTheDocument();
  expect(screen.getByText(/Cleanup requested/)).toBeInTheDocument();
  expect(screen.getByText(/Resource bbbbbbbb/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", `/api/cloudflare/${zone.zone_id}/certificate-challenges`, undefined, expect.anything());
  fireEvent.click(screen.getByRole("button", { name: "Close" }));
  expect(await screen.findByRole("button", { name: "Share domains" })).toBeInTheDocument();
});

it("does not offer zone challenge records to a team that only receives a share", async () => {
  vi.mocked(request).mockImplementation((_method, path) => Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : [{ ...zone, can_manage: false }]));
  render(<QueryClientProvider client={new QueryClient()}><CloudflareSettings /></QueryClientProvider>);
  await screen.findByText("Shared with this team: app.example.com");
  expect(screen.queryByRole("button", { name: /DNS challenges/ })).not.toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.some(([, path]) => String(path).endsWith("/certificate-challenges"))).toBe(false);
});

it("reports a refused zone challenge read with Retry", async () => {
  vi.mocked(request).mockImplementation((_method, path) => path === `/api/cloudflare/${zone.zone_id}/certificate-challenges` ? Promise.reject(new Error("connection owner access required")) : Promise.resolve(path === "/api/cloudflare/oauth" ? { configured: false } : path === "/api/cloudflare/connections" ? [] : [zone]));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><CloudflareSettings /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "DNS challenges for example.com" }));
  expect(await screen.findByText("Could not load DNS challenge records")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});
