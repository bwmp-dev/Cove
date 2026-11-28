import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { GitHubAppSetup, GitHubSource } from "./GitHubApp";

const session = vi.hoisted(() => ({ is_admin: true, team_id: "team", teams: [{ id: "team", name: "Team", role: "member", grants: [{ scope: "team", role: "editor" }] }] }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "GitHub unavailable" }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
vi.mock("@tanstack/react-router", () => ({ useRouterState: () => "", useBlocker: () => ({ status: "idle" }), Link: ({ children, title }: { children: ReactNode; title?: string }) => <a href="#operation" title={title}>{children}</a> }));
afterEach(() => { vi.clearAllMocks(); session.is_admin = true; });

it("prepares a manifest before submitting to GitHub and invalidates it after edits", async () => {
  vi.mocked(request).mockResolvedValueOnce({ configured: false }).mockResolvedValueOnce({ action: "https://github.com/settings/apps/new?state=fixture", manifest: '{"name":"Cove"}' });
  render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
  fireEvent.change(await screen.findByLabelText("App name"), { target: { value: "My Cove" } });
  fireEvent.click(screen.getByRole("button", { name: "Prepare GitHub App" }));
  const next = await screen.findByRole("button", { name: "Continue to GitHub" });
  expect(next.closest("form")).toHaveAttribute("action", "https://github.com/settings/apps/new?state=fixture");
  expect(next.closest("form")).toHaveAttribute("method", "post");
  expect(request).toHaveBeenCalledWith("POST", "/api/github/app/setup", expect.objectContaining({ name: "My Cove" }));
  fireEvent.change(screen.getByLabelText("App name"), { target: { value: "Other Cove" } });
  await waitFor(() => expect(screen.queryByRole("button", { name: "Continue to GitHub" })).not.toBeInTheDocument());
});

it("does not submit app setup to an unexpected host", async () => {
  vi.mocked(request).mockResolvedValueOnce({ configured: false }).mockResolvedValueOnce({ action: "https://github.com.attacker.invalid/settings/apps/new?state=fixture", manifest: "{}" });
  render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "Prepare GitHub App" }));
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
  expect(screen.queryByRole("button", { name: "Continue to GitHub" })).not.toBeInTheDocument();
});

it("lets a team editor authorize accounts without exposing instance controls", async () => {
  session.is_admin = false;
  vi.mocked(request).mockResolvedValueOnce({ configured: true, user_auth_configured: true, installations: [{ id: 41, account: "my-org", team_ids: ["team"], can_manage: false, shared_with_current_team: true }] })
    .mockResolvedValueOnce({ url: "https://github.com.attacker.invalid/login/oauth/authorize" });
  render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
  fireEvent.change(await screen.findByLabelText("GitHub account or organization"), { target: { value: "my-org" } });
  expect(screen.queryByRole("button", { name: "Refresh installations" })).not.toBeInTheDocument();
  expect(screen.queryByLabelText("App client secret")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save team access" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Remove team access" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Connect account on GitHub" }));
  expect(await screen.findByText("Could not connect GitHub")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/github/app/connect", { account: "my-org" });
});

it("explains one-time setup without offering app creation to team members", async () => {
  session.is_admin = false;
  vi.mocked(request).mockResolvedValueOnce({ configured: false });
  render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
  expect(await screen.findByText(/must create the shared GitHub App once/)).toBeInTheDocument();
  expect(screen.queryByLabelText("App name")).not.toBeInTheDocument();
});

it("saves edited assignments together and retains only failed changes for retry", async () => {
  const installations = [{ id: 41, account: "first-org", team_ids: ["team"], can_manage: true, shared_with_current_team: true }, { id: 42, account: "second-org", team_ids: ["team"], can_manage: true, shared_with_current_team: true }];
  let failSecond = true;
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (method === "GET") return Promise.resolve({ configured: true, installations: installations.map(item => ({ ...item })) });
    const item = installations.find(item => path.endsWith(`/${item.id}`))!;
    if (item.id === 42 && failSecond) return Promise.reject(new Error("In use"));
    item.team_ids = (body as { team_ids: string[] }).team_ids;
    return Promise.resolve({});
  });
  render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
  fireEvent.click(await screen.findByLabelText("first-org: Team"));
  fireEvent.click(screen.getByLabelText("second-org: Team"));
  expect(screen.getAllByRole("button", { name: "Save changes" })).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByText("Some assignments could not be saved");
  expect(screen.getAllByText("Unsaved assignment")).toHaveLength(1);
  expect(screen.getByLabelText("second-org: Team")).not.toBeChecked();
  failSecond = false;
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByText("Assignments saved");
  expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path.endsWith("/41"))).toHaveLength(1);
  expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path.endsWith("/42"))).toHaveLength(2);
});

it("lets a personal connection manager share with multiple editable teams", async () => {
  session.is_admin = false;
  session.teams.push({ id: "second", name: "Second", role: "member", grants: [{ scope: "team", role: "editor" }] });
  session.teams.push({ id: "viewer", name: "View only", role: "member", grants: [{ scope: "team", role: "viewer" }] });
  const app = { configured: true, installations: [{ id: 41, account: "my-org", team_ids: ["team"], can_manage: true, shared_with_current_team: true }] };
  vi.mocked(request).mockImplementation((method, _path, body) => {
    if (method === "GET") return Promise.resolve(app);
    app.installations[0]!.team_ids = (body as { team_ids: string[] }).team_ids;
    return Promise.resolve({});
  });
  try {
    render(<QueryClientProvider client={new QueryClient()}><GitHubAppSetup /></QueryClientProvider>);
    expect(await screen.findByLabelText("my-org: Team")).toBeChecked();
    expect(screen.queryByLabelText("my-org: View only")).not.toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("my-org: Second"));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByText("Assignments saved");
    expect(request).toHaveBeenCalledWith("PUT", "/api/github/app/installations/41", { team_ids: ["team", "second"] });
  } finally {
    session.teams.splice(1);
  }
});

const workload = { id: "web", name: "web" } as Workload;
const failedReport = { operation_id: "op-12345678-abcd", reported_state: "running", error: "GitHub reporting failed; check App permissions and provider availability", next_attempt_at: new Date(Date.now() + 120_000).toISOString() };

function mockSource(reporting: { enabled: boolean; reports: unknown[] }, put?: (body: unknown) => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/api/workloads/web/github") return Promise.resolve({ installation_id: 1, repository_id: 2, automatic: true });
    if (path === "/api/github/repositories") return Promise.resolve([]);
    if (path === "/api/workloads/web/github/reporting") return method === "PUT" && put ? put(body) : Promise.resolve(reporting);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

it("enables deployment reporting through the reporting endpoint and shows failures", async () => {
  mockSource({ enabled: false, reports: [failedReport] }, body => Promise.resolve({ enabled: (body as { enabled: boolean }).enabled, reports: [] }));
  render(<QueryClientProvider client={new QueryClient()}><GitHubSource workload={workload} /></QueryClientProvider>);
  expect(await screen.findByText(failedReport.error)).toBeInTheDocument();
  expect(screen.getByText("Running")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: /Operation op-12345/ })).toBeInTheDocument();
  const toggle = screen.getByRole("switch", { name: "Report deployments to GitHub" });
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByText("Reporting setting saved");
  expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/web/github/reporting", { enabled: true });
  expect(screen.getByRole("switch", { name: "Report deployments to GitHub" })).toBeChecked();
  expect(screen.getByText(/No deployments reported yet/)).toBeInTheDocument();
});

it("keeps the change unsaved when GitHub rejects the reporting permissions", async () => {
  mockSource({ enabled: false, reports: [] }, () => Promise.reject(new Error("approve permissions")));
  render(<QueryClientProvider client={new QueryClient()}><GitHubSource workload={workload} /></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("switch", { name: "Report deployments to GitHub" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Could not change GitHub reporting")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Report deployments to GitHub" })).toBeChecked();
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
});

it("shows previews the inherited reporting setting without a toggle", async () => {
  mockSource({ enabled: false, reports: [] });
  render(<QueryClientProvider client={new QueryClient()}><GitHubSource workload={{ ...workload, preview_source_id: "source" }} /></QueryClientProvider>);
  expect(await screen.findByText(/Previews follow the setting/)).toBeInTheDocument();
  expect(screen.queryByRole("switch", { name: "Report deployments to GitHub" })).not.toBeInTheDocument();
});
