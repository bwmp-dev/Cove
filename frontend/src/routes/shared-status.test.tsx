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

const uptimeStatus = { title: "Shop status", source: "Periodic HTTPS checks from the panel.", checks: [] };

// The opt-in uptime status page is a separate public route with the same isolation as share links.
it.each(["", "/control/cove"])("opens a public uptime status page under prefix %j without the session, team or private requests", async (base) => {
  if (base) document.head.insertAdjacentHTML("afterbegin", `<meta name="cove-base-path" content="${base}">`);
  vi.resetModules();
  const { createAppRouter: createRouter } = await import("../router");
  const { rememberTeam } = await import("../lib/team");
  const { keys: queryKeys } = await import("../api/queries");
  const timezone = await import("../lib/timezone");
  rememberTeam("t1");
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => Promise.resolve(url === `${base}/api/status/shop-slug` ? Response.json(uptimeStatus) : new Response("unauthorized", { status: 401, headers: { "content-type": "text/plain" } }))),
  );
  window.history.replaceState(null, "", `${base}/status/shop-slug`);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.session, { user: "u", team_id: "t1" });
  const router = createRouter(queryClient);
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("heading", { name: "Shop status" })).toBeInTheDocument();
    const calls = vi.mocked(fetch).mock.calls as [string, RequestInit][];
    expect(calls.map(([url]) => url)).toEqual([`${base}/api/status/shop-slug`]);
    expect(calls[0]?.[1].credentials).toBe("omit");
    expect(new Headers(calls[0]?.[1].headers).get("x-cove-team")).toBeNull();
    expect(router.state.location.pathname).toBe("/status/shop-slug");
    expect(timezone.getDisplayTimeZone()).toBeUndefined();
  } finally {
    rememberTeam(null);
    document.head.querySelector('meta[name="cove-base-path"]')?.remove();
    vi.resetModules();
  }
});
