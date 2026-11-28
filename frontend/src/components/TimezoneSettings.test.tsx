import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { keys } from "../api/queries";
import { timezonePreferenceQuery, type TimezonePreference } from "../api/timezone";
import { AccountTimezone, TeamTimezone } from "./TimezoneSettings";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (importActual) => ({ ...(await importActual<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => vi.resetAllMocks());

const utc: TimezonePreference = { team_timezone: "UTC", user_timezone: "", timezone: "UTC", schedule_timezone: "UTC" };
const session = { user: "u", user_id: "u1", email: "", name: "", is_admin: false, has_password: true, github_linked: false, team_id: "t1", teams: [] };

async function renderWithHeldRead(saved: TimezonePreference) {
  let releaseHeld: (value: TimezonePreference) => void = () => {};
  let holdNextRead = false;
  vi.mocked(request).mockImplementation((method, path) => {
    if (method === "PUT") return Promise.resolve(saved);
    if (path === "/api/session") return Promise.resolve(session);
    if (path === "/api/preferences/timezone" && holdNextRead) {
      holdNextRead = false;
      return new Promise((resolve) => (releaseHeld = resolve));
    }
    return Promise.resolve(utc);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(keys.session, session);
  render(
    <QueryClientProvider client={client}>
      <AccountTimezone />
      <TeamTimezone teamId="t1" />
    </QueryClientProvider>,
  );
  expect(await screen.findByText(/Times currently show in UTC\./)).toBeInTheDocument();
  holdNextRead = true;
  void client.invalidateQueries({ queryKey: timezonePreferenceQuery("t1").queryKey });
  return async function releaseStaleRead() {
    act(() => releaseHeld(utc));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    // Query observers are notified on a zero-delay timer, so let it run before reading the page.
    await act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  };
}

async function saveZone(label: string) {
  const zone = screen.getByRole("combobox", { name: label });
  fireEvent.click(zone);
  fireEvent.click(screen.getByRole("option", { name: "America/New_York" }));
  fireEvent.submit(zone.closest("form") as HTMLFormElement);
  expect(await screen.findByText("Time zone saved")).toBeInTheDocument();
}

it("keeps the saved team zone when a read started before the save resolves afterwards", async () => {
  const releaseStaleRead = await renderWithHeldRead({ team_timezone: "America/New_York", user_timezone: "", timezone: "America/New_York", schedule_timezone: "UTC" });
  await saveZone("Default time zone");
  await releaseStaleRead();
  expect(screen.getByText(/Times currently show in America\/New_York\./)).toBeInTheDocument();
});

it("keeps the saved personal zone when a read started before the save resolves afterwards", async () => {
  const releaseStaleRead = await renderWithHeldRead({ team_timezone: "UTC", user_timezone: "America/New_York", timezone: "America/New_York", schedule_timezone: "UTC" });
  await saveZone("Display time zone");
  await releaseStaleRead();
  expect(screen.getByText(/Times currently show in America\/New_York\./)).toBeInTheDocument();
});
