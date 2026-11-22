import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Workload } from "../api/types";
import { rememberTeam, selectedTeam } from "../lib/team";
import { ResourceMove } from "./ResourceMove";

interface TestTeam { id: string; name: string; role: string; grants: never[] }
const session = vi.hoisted((): { value: { is_admin: boolean; team_id: string; teams: TestTeam[] } } => ({ value: { is_admin: false, team_id: "a", teams: [] } }));
const navigate = vi.hoisted(() => vi.fn());
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigate }));
vi.mock("../lib/session", () => ({
  useSession: () => session.value,
  useCurrentTeam: () => session.value.teams.find((team) => team.id === session.value.team_id),
}));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const workload = { id: "w1", name: "web", project_id: "p1", environment_id: "e1", permissions: ["read", "operate", "edit"] } as Workload;
const edit = ["read", "operate", "edit"];
const projectsByTeam: Record<string, unknown[]> = {
  a: [{ id: "p1", name: "Shop", permissions: edit, environments: [{ id: "e1", name: "production", permissions: edit }, { id: "e2", name: "staging", permissions: edit }] }],
  b: [{ id: "p9", name: "Billing", permissions: edit, environments: [] }],
};
const review = { version: "v1", blockers: [], effects: ["No deployment is queued."], cross_team: false };

type Handler = (path: string, init: RequestInit) => Response | undefined;
let handler: Handler = () => undefined;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const text = (status: number, body: string) => new Response(body, { status, headers: { "content-type": "text/plain" } });

function calls() {
  return (vi.mocked(fetch).mock.calls as [string, RequestInit][]).map(([path, init]) => ({
    path,
    method: init.method,
    team: new Headers(init.headers).get("x-cove-team"),
    body: typeof init.body === "string" ? (JSON.parse(init.body) as unknown) : undefined,
  }));
}

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  vi.stubGlobal(
    "fetch",
    vi.fn((path: string, init: RequestInit) => {
      const custom = handler(path, init);
      if (custom) return Promise.resolve(custom);
      if (path === "/api/projects") return Promise.resolve(json(200, projectsByTeam[new Headers(init.headers).get("x-cove-team") ?? "a"] ?? []));
      if (path.startsWith("/api/operations")) return Promise.resolve(json(200, []));
      return Promise.resolve(text(404, "not found"));
    }),
  );
  rememberTeam("a");
  session.value = { is_admin: false, team_id: "a", teams: [{ id: "a", name: "Platform", role: "admin", grants: [] }, { id: "b", name: "Finance", role: "owner", grants: [] }] };
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
  handler = () => undefined;
  rememberTeam(null);
});

function renderMove(subject: Workload = workload) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><ResourceMove workload={subject} /></QueryClientProvider>);
  return client;
}

async function choose(user: ReturnType<typeof userEvent.setup>, label: string, option: string) {
  await user.click(await screen.findByRole("combobox", { name: label }));
  await user.click(screen.getByRole("option", { name: option }));
}

it("reviews effects and moves only after explicit acknowledgement, never through the legacy route", async () => {
  const user = userEvent.setup();
  handler = (path) => {
    if (path === "/api/workloads/w1/move/preview") return json(200, review);
    if (path === "/api/workloads/w1/move") return json(200, { ok: true, id: "w1", team_id: "a", project_id: "p1", environment_id: "e2" });
    return undefined;
  };
  renderMove();
  await choose(user, "Project and environment", "Shop / staging");
  await user.click(screen.getByRole("button", { name: "Review move" }));
  const dialog = await screen.findByRole("dialog", { name: "Move web" });
  expect(within(dialog).getByText("No deployment is queued.")).toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Move resource" });
  expect(confirm).toBeDisabled();
  expect(calls().some((call) => call.path === "/api/workloads/w1/move")).toBe(false);
  await user.click(within(dialog).getByRole("checkbox", { name: "I understand these effects" }));
  await user.click(confirm);
  await screen.findByText("Resource moved");
  const move = calls().find((call) => call.path === "/api/workloads/w1/move");
  expect(move).toMatchObject({ method: "POST", team: "a", body: { team_id: "a", project_id: "p1", environment_id: "e2", version: "v1", acknowledge: true } });
  expect(calls().some((call) => call.path.endsWith("/environment"))).toBe(false);
});

it("browses another team's destinations with an explicit header and shows blockers without a confirm action", async () => {
  const user = userEvent.setup();
  handler = (path) => (path === "/api/workloads/w1/move/preview" ? json(200, { ...review, cross_team: true, blockers: ["Detach private connections and shared networks before moving."] }) : undefined);
  renderMove();
  await choose(user, "Team", "Finance");
  await choose(user, "Project and environment", "Billing / Project resources");
  expect(calls().find((call) => call.path === "/api/projects" && call.team === "b")).toBeDefined();
  expect(selectedTeam()).toBe("a");
  await user.click(screen.getByRole("button", { name: "Review move" }));
  const dialog = await screen.findByRole("dialog", { name: "Transfer web to Finance" });
  expect(within(dialog).getByText("Detach private connections and shared networks before moving.")).toBeInTheDocument();
  expect(within(dialog).queryByRole("button", { name: "Transfer resource" })).not.toBeInTheDocument();
  expect(calls().find((call) => call.path === "/api/workloads/w1/move/preview")).toMatchObject({ team: "a", body: { team_id: "b", project_id: "p9", environment_id: "" } });
});

it("switches to the destination team after a confirmed transfer", async () => {
  const user = userEvent.setup();
  handler = (path) => {
    if (path === "/api/workloads/w1/move/preview") return json(200, { ...review, cross_team: true });
    if (path === "/api/workloads/w1/move") return json(200, { ok: true, id: "w1", team_id: "b", project_id: "p9", environment_id: "" });
    return undefined;
  };
  renderMove();
  await choose(user, "Team", "Finance");
  await choose(user, "Project and environment", "Billing / Project resources");
  await user.click(screen.getByRole("button", { name: "Review move" }));
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Transfer resource" }));
  await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/resources/$resourceId", params: { resourceId: "w1" } }));
  expect(selectedTeam()).toBe("b");
});

it("requires a new review after a stale refusal", async () => {
  const user = userEvent.setup();
  handler = (path) => {
    if (path === "/api/workloads/w1/move/preview") return json(200, review);
    if (path === "/api/workloads/w1/move") return text(409, "the resource or destination changed; review the move again");
    return undefined;
  };
  renderMove();
  await choose(user, "Project and environment", "Shop / staging");
  await user.click(screen.getByRole("button", { name: "Review move" }));
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Move resource" }));
  expect(await within(dialog).findByText("The resource or destination changed; review the move again")).toBeInTheDocument();
  expect(within(dialog).queryByRole("button", { name: "Move resource" })).not.toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Review again" }));
  await waitFor(() => expect(calls().filter((call) => call.path === "/api/workloads/w1/move/preview")).toHaveLength(2));
  expect(await within(dialog).findByRole("button", { name: "Move resource" })).toBeDisabled();
});

it("only offers the current team to members who do not manage it", async () => {
  session.value = { ...session.value, teams: [{ id: "a", name: "Platform", role: "member", grants: [] }, { id: "b", name: "Finance", role: "owner", grants: [] }] };
  renderMove();
  await screen.findByRole("combobox", { name: "Project and environment" });
  expect(screen.queryByRole("combobox", { name: "Team" })).not.toBeInTheDocument();
  expect(screen.getByText(/requires owner or admin access on both teams/)).toBeInTheDocument();
});

it("hides the move form for preview resources", () => {
  renderMove({ ...workload, preview_source_id: "w0" });
  expect(screen.getByText("Preview resources cannot be moved")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Review move" })).not.toBeInTheDocument();
});
