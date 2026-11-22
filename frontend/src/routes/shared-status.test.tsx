import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { keys } from "../api/queries";
import { getDisplayTimeZone } from "../lib/timezone";
import { createAppRouter } from "../router";

afterEach(() => {
  window.history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

const publicStatus = { title: "Storefront", state: "succeeded", counts: { total: 1, succeeded: 1, failed: 0, pending: 0, stopped: 0, unknown: 0 }, as_of: "2026-07-01T12:00:00Z", description: "Latest results." };

// A session cached from earlier use of the app may have expired; any private request here would answer 401 and send a
// visitor holding a valid public link to sign-in.
it("opens a public status link without private requests while a signed-in session is cached", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => Promise.resolve(url.startsWith("/api/public/") ? Response.json(publicStatus) : new Response("unauthorized", { status: 401, headers: { "content-type": "text/plain" } }))),
  );
  window.history.replaceState(null, "", "/shared/share-token");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(keys.session, { user: "u", team_id: "t1" });
  const router = createAppRouter(queryClient);
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  expect(await screen.findByRole("heading", { name: "Storefront" })).toBeInTheDocument();
  expect(vi.mocked(fetch).mock.calls.map(([url]) => url)).toEqual(["/api/public/projects/share-token"]);
  expect(router.state.location.pathname).toBe("/shared/share-token");
  expect(getDisplayTimeZone()).toBeUndefined();
});
