import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { keys } from "../api/queries";
import { getDisplayTimeZone, setDisplayTimeZone } from "../lib/timezone";
import { RelativeTime } from "./RelativeTime";
import { TimezoneProvider } from "./TimezoneProvider";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
afterEach(() => {
  vi.clearAllMocks();
  setDisplayTimeZone(undefined);
});

const zones: Record<string, string> = { alpha: "America/New_York", beta: "Asia/Tokyo" };
const session = (team: string) => ({ user: "u", user_id: "u1", email: "", name: "", is_admin: false, has_password: true, github_linked: false, team_id: team, teams: [] });

it("shows timestamps in the selected team's effective zone and follows team switches", async () => {
  let team = "alpha";
  vi.mocked(request).mockImplementation(() => Promise.resolve({ team_timezone: zones[team], user_timezone: "", timezone: zones[team], schedule_timezone: "UTC" }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(keys.session, session("alpha"));
  render(
    <QueryClientProvider client={client}>
      <TimezoneProvider>
        <RelativeTime value="2026-07-01T12:00:00Z" />
      </TimezoneProvider>
    </QueryClientProvider>,
  );
  const time = screen.getByText(/ago|in /);
  await waitFor(() => expect(time).toHaveAttribute("title", "Jul 1, 2026, 8:00:00 AM"));

  team = "beta";
  act(() => {
    client.clear();
  });
  act(() => {
    client.setQueryData(keys.session, session("beta"));
  });
  await waitFor(() => expect(time).toHaveAttribute("title", "Jul 1, 2026, 9:00:00 PM"));
  expect(request).toHaveBeenCalledTimes(2);
});

it("does not request a preference without a session", () => {
  const client = new QueryClient();
  render(
    <QueryClientProvider client={client}>
      <TimezoneProvider>
        <span>public</span>
      </TimezoneProvider>
    </QueryClientProvider>,
  );
  expect(request).not.toHaveBeenCalled();
});

it("returns to the browser's zone when the signed-in app unmounts", async () => {
  vi.mocked(request).mockResolvedValue({ team_timezone: zones.beta, user_timezone: "", timezone: zones.beta, schedule_timezone: "UTC" });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(keys.session, session("beta"));
  const { unmount } = render(
    <QueryClientProvider client={client}>
      <TimezoneProvider>
        <span>app</span>
      </TimezoneProvider>
    </QueryClientProvider>,
  );
  await waitFor(() => expect(getDisplayTimeZone()).toBe(zones.beta));
  unmount();
  expect(getDisplayTimeZone()).toBeUndefined();
});
