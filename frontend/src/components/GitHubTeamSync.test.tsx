import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { directoryKeys, type GithubTeamReport, type GithubTeamSource } from "../api/directory";
import { GitHubTeamSync } from "./GitHubTeamSync";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
const router = vi.hoisted(() => ({ search: "", navigate: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useRouterState: ({ select }: { select: (state: { location: { searchStr: string } }) => string }) => select({ location: { searchStr: router.search } }), useNavigate: () => router.navigate }));
vi.mock("../lib/session", () => ({ useSession: () => ({ user_id: "admin", is_admin: true, github_linked: true, has_password: true }) }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const hash = "f".repeat(64);
const source = (overrides: Partial<GithubTeamSource> = {}): GithubTeamSource => ({
  id: "src",
  enabled: true,
  version: 3,
  checked_at: "2026-11-22T09:00:00Z",
  applied_at: null,
  next_check_at: "2026-11-22T09:15:00Z",
  last_error: "",
  credential_configured: true,
  configuration: { name: "Engineering", mode: "app", account: "my-org", installation_id: 7 },
  mappings: [{ group: "platform", team_id: "team", role: "admin" }],
  ...overrides,
});
const report: GithubTeamReport = {
  version: 3,
  roster_hash: hash,
  applied: false,
  unmatched_github_users: 2,
  changes: [
    { team_id: "team", user_id: "u1", user_name: "Ada", email: "ada@example.com", before: { role: "admin", grants: [] }, after: null },
    { team_id: "team", user_id: "u2", email: "lin@example.com", before: null, after: { role: "admin", grants: [] } },
    { team_id: "team", user_id: "u3", before: null, after: { role: "admin", grants: [] } },
  ],
};

function serve(sources: GithubTeamSource[], routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    const handler = routes[`${method} ${path}`];
    if (handler) return handler(body);
    if (path === "/api/settings/github-team-sync") return Promise.resolve({ sources });
    if (path === "/api/settings/directory/teams") return Promise.resolve({ teams: [{ id: "team", name: "Platform" }] });
    return Promise.reject(new ApiError(404, "Not found"));
  });
}

let client: QueryClient;
function renderSync() {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <GitHubTeamSync />
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  router.search = "";
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

it("previews exact changes and applies only the reviewed roster", async () => {
  serve([source()], {
    "POST /api/settings/github-team-sync/src/preview": () => Promise.resolve(report),
    "POST /api/settings/github-team-sync/src/reconcile": () => Promise.resolve({ ...report, applied: true }),
  });
  renderSync();
  fireEvent.click(await screen.findByRole("button", { name: /Preview changes\s*for Engineering/ }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/github-team-sync/src/preview", { version: 3 }));
  const table = await screen.findByRole("table", { name: "Membership changes" });
  expect(within(table).getByText("Ada")).toBeInTheDocument();
  expect(within(table).getByText("Loses access")).toBeInTheDocument();
  expect(within(table).getByText("ada@example.com")).toBeInTheDocument();
  expect(within(table).getByText("lin@example.com")).toBeInTheDocument();
  expect(within(table).getByText("u3")).toBeInTheDocument();
  expect(within(table).getAllByText("Platform")).toHaveLength(3);
  expect(screen.getByText(/2 GitHub team members have no linked Cove account/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/settings/github-team-sync/src/reconcile", expect.anything());

  fireEvent.click(screen.getByRole("button", { name: "Apply 3 changes" }));
  const dialog = await screen.findByRole("dialog", { name: "Apply changes from Engineering?" });
  expect(dialog).toHaveTextContent("1 person lose the access this source granted");
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply and remove 1 membership" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/github-team-sync/src/reconcile", { version: 3, roster_hash: hash, confirm: true }));
  expect(await screen.findByText("Changes applied")).toBeInTheDocument();
});

it("refuses to apply a preview once the source revision changes", async () => {
  serve([source()], { "POST /api/settings/github-team-sync/src/preview": () => Promise.resolve(report) });
  renderSync();
  fireEvent.click(await screen.findByRole("button", { name: /Preview changes\s*for Engineering/ }));
  expect(await screen.findByRole("button", { name: "Apply 3 changes" })).toBeEnabled();
  await act(async () => {
    client.setQueryData(directoryKeys.githubSources, [source({ version: 4 })]);
    await Promise.resolve();
  });
  expect(await screen.findByText("Preview out of date")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Apply 3 changes" })).toBeDisabled();
});

it("discards a preview the server reports as stale and says nothing changed", async () => {
  serve([source()], {
    "POST /api/settings/github-team-sync/src/preview": () => Promise.resolve(report),
    "POST /api/settings/github-team-sync/src/reconcile": () => Promise.reject(new ApiError(409, "Source or roster changed, or a refresh is active; preview again")),
  });
  renderSync();
  fireEvent.click(await screen.findByRole("button", { name: /Preview changes\s*for Engineering/ }));
  fireEvent.click(await screen.findByRole("button", { name: "Apply 3 changes" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Apply and remove 1 membership" }));
  expect(await screen.findByText(/Nothing was changed. Preview again/)).toBeInTheDocument();
  expect(screen.queryByRole("table", { name: "Membership changes" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^Apply/ })).not.toBeInTheDocument();
});

it("explains that a failed refresh keeps existing access", async () => {
  serve([source({ last_error: "GitHub rate limit reached." })]);
  renderSync();
  expect(await screen.findByText("Last refresh failed")).toBeInTheDocument();
  expect(screen.getByText(/Existing access was kept: Cove never removes members while GitHub cannot be read completely/)).toBeInTheDocument();
});

it("removes a source with the reviewed revision", async () => {
  serve([source()], { "DELETE /api/settings/github-team-sync/src": () => Promise.resolve({ id: "src" }) });
  renderSync();
  fireEvent.click(await screen.findByRole("button", { name: /^Remove\s*Engineering/ }));
  const dialog = await screen.findByRole("dialog", { name: "Remove Engineering?" });
  expect(dialog).toHaveTextContent("Access granted by hand or by other sources is kept");
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove source and its access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/settings/github-team-sync/src", { version: 3, confirm: true }));
});

it("shows the consent outcome once, refreshes the sources and removes the marker from the address", async () => {
  router.search = "?github_team_sync=permission_required";
  serve([source({ configuration: { name: "Engineering", mode: "oauth", account: "acme", oauth_user_id: "admin" }, credential_configured: false })]);
  renderSync();
  expect(await screen.findByText("Organization access was not granted")).toBeInTheDocument();
  expect(screen.getByTestId("github-team-sync-consent")).toHaveFocus();
  await waitFor(() => expect(router.navigate).toHaveBeenCalledWith({ to: "/admin", search: {}, hash: "github-team-sync", replace: true }));
  fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByText("Organization access was not granted")).not.toBeInTheDocument();
});

it("ignores an unknown consent marker", async () => {
  router.search = "?github_team_sync=anything";
  serve([source()]);
  renderSync();
  expect(await screen.findByRole("button", { name: /Preview changes\s*for Engineering/ })).toBeInTheDocument();
  expect(screen.queryByTestId("github-team-sync-consent")).not.toBeInTheDocument();
});
