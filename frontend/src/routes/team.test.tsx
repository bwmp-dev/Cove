import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Member } from "../api/teams";
import type { TeamMembership } from "../api/types";
import { Members } from "./team";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
const session = vi.hoisted(() => ({ user_id: "me", is_admin: false }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const members: Member[] = [
  { user_id: "me", email: "ada@example.com", name: "Ada", role: "owner", grants: [], managed_by: [] },
  { user_id: "bob", email: "bob@example.com", name: "Bob", role: "member", grants: [{ scope: "team", scope_id: "t", role: "editor" }], managed_by: [{ source_kind: "github", source_id: "src", display_name: "Engineering (acme)" }] },
  { user_id: "cy", email: "cy@example.com", name: "Cy", role: "member", grants: [], managed_by: [{ source_kind: "ldap", source_id: "dir" }] },
];

function serve(routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    const handler = routes[`${method} ${path}`];
    if (handler) return handler(body);
    if (path === "/api/teams/t/members") return Promise.resolve(members);
    return Promise.reject(new ApiError(404, "Not found"));
  });
}

function renderMembers(team: TeamMembership) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Members team={team} projects={[]} management="full" />
    </QueryClientProvider>,
  );
}

const owned: TeamMembership = { id: "t", name: "Platform", role: "owner", grants: [] };

afterEach(() => {
  session.is_admin = false;
  vi.clearAllMocks();
});

it("shows where each membership comes from and offers only owner recovery for managed rows", async () => {
  serve({ "PUT /api/teams/t/members/bob": () => Promise.resolve({ ok: true }) });
  renderMembers(owned);
  const bob = (await screen.findByText("Managed by GitHub team sync — Engineering (acme)")).closest("li");
  expect(bob).not.toBeNull();
  expect(screen.getByText("Added manually")).toBeInTheDocument();
  // Without an administrator display name, only the kind of source is shown.
  expect(screen.getByText("Managed by LDAP")).toBeInTheDocument();
  const row = within(bob as HTMLElement);
  expect(row.queryByRole("button", { name: /^Edit/ })).not.toBeInTheDocument();
  expect(row.queryByRole("button", { name: /^Remove/ })).not.toBeInTheDocument();
  expect(row.getByText(/Change the directory's group mappings/)).toBeInTheDocument();

  fireEvent.click(row.getByRole("button", { name: /Make local owner/ }));
  const dialog = await screen.findByRole("dialog", { name: "Make bob@example.com a local owner?" });
  expect(dialog).toHaveTextContent("stay one even if GitHub team sync — Engineering (acme) later removes their access");
  fireEvent.click(within(dialog).getByRole("button", { name: "Make local owner" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/teams/t/members/bob", { role: "owner", grants: [] }));
});

it("does not offer owner recovery to team admins", async () => {
  serve();
  renderMembers({ ...owned, role: "admin" });
  expect(await screen.findByText("Managed by GitHub team sync — Engineering (acme)")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Make local owner/ })).not.toBeInTheDocument();
});

it("lets an instance administrator recover a team they do not own", async () => {
  session.is_admin = true;
  serve();
  renderMembers({ ...owned, role: "admin" });
  expect(await screen.findAllByRole("button", { name: /Make local owner/ })).toHaveLength(2);
});
