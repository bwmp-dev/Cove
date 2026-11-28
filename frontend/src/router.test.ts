import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { keys } from "./api/queries";

async function routerAt(address: string, base: string) {
  if (base) document.head.insertAdjacentHTML("afterbegin", `<meta name="cove-base-path" content="${base}">`);
  window.history.replaceState(null, "", address);
  vi.resetModules();
  const { createAppRouter } = await import("./router");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { router: createAppRouter(queryClient), queryClient };
}

describe("router base path", () => {
  afterEach(() => {
    document.head.querySelector('meta[name="cove-base-path"]')?.remove();
    window.history.replaceState(null, "", "/");
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it("keeps root addresses unchanged", async () => {
    const { router } = await routerAt("/nodes?x=1", "");
    expect(router.state.location.pathname).toBe("/nodes");
    expect(router.buildLocation({ to: "/dashboard" }).publicHref).toBe("/dashboard");
  });

  it("matches deep links under the prefix and builds prefixed links", async () => {
    const { router } = await routerAt("/control/cove/nodes?x=1", "/control/cove");
    expect(router.state.location.pathname).toBe("/nodes");
    expect(router.state.location.href).toBe("/nodes?x=1");
    expect(router.buildLocation({ to: "/dashboard" }).publicHref).toBe("/control/cove/dashboard");
    expect(router.buildLocation({ href: "/control/cove/projects" } as never).publicHref).toBe("/control/cove/projects");
  });

  it("sends signed-out deep links to the prefixed login with an unprefixed destination", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("unauthorized", { status: 401, headers: { "content-type": "text/plain" } }))));
    const { router } = await routerAt("/control/cove/projects?tab=a", "/control/cove");
    await router.load();
    expect(window.location.pathname).toBe("/control/cove/login");
    expect(router.state.location.pathname).toBe("/login");
    expect(router.state.location.search).toEqual({ redirect: "/projects?tab=a" });
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toBe("/control/cove/api/session");
  });

  it("returns a signed-in visitor from login to the requested page once", async () => {
    const { router, queryClient } = await routerAt("/control/cove/login?redirect=%2Fnodes", "/control/cove");
    queryClient.setQueryData(keys.session, { user: "u", team_id: "" });
    await router.load();
    expect(window.location.pathname).toBe("/control/cove/nodes");
    expect(router.state.location.pathname).toBe("/nodes");
  });
});

// A prefix that is also an application path must not swallow a logical destination that starts with it.
describe.each(["", "/api", "/projects", "/control/cove"])("router under prefix %j", (base) => {
  afterEach(() => {
    document.head.querySelector('meta[name="cove-base-path"]')?.remove();
    window.history.replaceState(null, "", "/");
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it("round-trips a signed-out /projects deep link through login", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("unauthorized", { status: 401, headers: { "content-type": "text/plain" } }))));
    const { router } = await routerAt(`${base}/projects?tab=a`, base);
    await router.load();
    expect(window.location.pathname).toBe(`${base}/login`);
    expect(router.state.location.search).toEqual({ redirect: "/projects?tab=a" });
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toBe(`${base}/api/session`);
  });

  it("returns a signed-in visitor from login to /projects", async () => {
    const { router, queryClient } = await routerAt(`${base}/login?redirect=%2Fprojects%3Ftab%3Da`, base);
    queryClient.setQueryData(keys.session, { user: "u", team_id: "" });
    await router.load();
    expect(window.location.pathname).toBe(`${base}/projects`);
    expect(window.location.search).toBe("?tab=a");
    expect(router.state.location.pathname).toBe("/projects");
  });

  it("navigates to a logical address given as a browser href", async () => {
    const { router, queryClient } = await routerAt(`${base}/dashboard`, base);
    const { withBasePath } = await import("./lib/base-path");
    queryClient.setQueryData(keys.session, { user: "u", team_id: "" });
    await router.load();
    await router.navigate({ href: withBasePath("/projects") });
    expect(window.location.pathname).toBe(`${base}/projects`);
    expect(router.state.location.pathname).toBe("/projects");
  });
});
