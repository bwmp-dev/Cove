import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { BuildCacheStatus } from "../api/build-cache";
import { ApiError, request } from "../api/client";
import type { Node, NodeState } from "../api/types";
import { cachePathError, formatBytes } from "../lib/build-cache";
import { BuildCacheSection } from "./BuildCache";

const session = vi.hoisted(() => ({ is_admin: true }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));

// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

afterEach(() => {
  vi.clearAllMocks();
  session.is_admin = true;
  window.dispatchEvent(new Event("online"));
});

const image = `ghcr.io/example/buildkit@sha256:${"c".repeat(64)}`;
const gib = 1024 ** 3;

function node(capability = true, state: NodeState = "ready"): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state, mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, ...(capability ? { build_cache_limits: true } : {}) } };
}

function status(overrides: Partial<BuildCacheStatus> = {}): BuildCacheStatus {
  return { supported: true, can_configure: true, configured: false, verified: false, cleanup_pending: false, path: "", image, reason: "", active_operation_id: "", capacity_bytes: 0, available_bytes: 0, can_manage: true, ...overrides };
}

const verified = (overrides: Partial<BuildCacheStatus> = {}) => status({ configured: true, verified: true, path: "/mnt/cache", capacity_bytes: 20 * gib, available_bytes: 15 * gib, ...overrides });

function serve(read: () => Promise<unknown>, save: (body: unknown) => Promise<unknown> = () => Promise.resolve(verified())) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/nodes/n1/build-cache") return method === "PUT" ? save(body) : read();
    return Promise.reject(new Error(`Unexpected request ${method} ${path}`));
  });
}

function renderSection(target = node()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BuildCacheSection node={target} />
    </QueryClientProvider>,
  );
  return client;
}

const pathField = () => screen.getByRole("textbox", { name: "Filesystem mount path" });
const useButton = () => screen.getByRole("button", { name: "Verify and use filesystem" });
const dialog = () => screen.getByRole("dialog");

it("is honest that there is no hard cap by default", async () => {
  serve(() => Promise.resolve(status({ reason: "no dedicated bounded build-cache filesystem is configured" })));
  renderSection();
  expect(await screen.findByText("No hard cap")).toBeInTheDocument();
  expect(screen.getByText(/do not limit it, so this is not a hard cap/)).toBeInTheDocument();
  expect(screen.getByText(/registry storage are outside this cap/)).toBeInTheDocument();
  expect(screen.queryByText("Hard cap enforced")).not.toBeInTheDocument();
  expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disable hard cap" })).not.toBeInTheDocument();
});

it("verifies a path with the exact body after confirmation and refreshes status, nodes and operations", async () => {
  const user = userEvent.setup();
  let saved = false;
  serve(
    () => Promise.resolve(saved ? verified() : status()),
    () => {
      saved = true;
      return Promise.resolve(verified());
    },
  );
  const client = renderSection();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  await user.type(await screen.findByRole("textbox", { name: "Filesystem mount path" }), "/mnt/cache");
  await user.click(useButton());
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  expect(dialog()).toHaveTextContent(/Any previous capped builder is stopped and its cache data is kept/);
  expect(dialog()).toHaveTextContent(/existing legacy Cove builder cache is left as it is, outside the cap/);
  await user.click(within(dialog()).getByRole("button", { name: "Verify and use filesystem" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/build-cache", { path: "/mnt/cache" }));
  expect(await screen.findByText("Hard cap verified. Builds on this node now use /mnt/cache.")).toBeInTheDocument();
  expect(screen.getByText("Hard cap enforced")).toBeInTheDocument();
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["nodes", "n1", "build-cache"], exact: true });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["nodes"], exact: true });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["operations"] });
});

it("disables the cap with an empty path after confirming that data is kept", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(verified()), () => Promise.resolve(status()));
  renderSection();
  expect(await screen.findByRole("button", { name: "Disable hard cap" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Retry cleanup" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Disable hard cap" }));
  expect(dialog()).toHaveTextContent(/no hard cap/);
  expect(dialog()).toHaveTextContent(/Nothing is deleted/);
  await user.click(within(dialog()).getByRole("button", { name: "Disable hard cap" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/build-cache", { path: "" }));
  expect(await screen.findByText(/Hard cap disabled. The cache data was kept/)).toBeInTheDocument();
});

it("cancelling the confirmation sends nothing", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(status()));
  renderSection();
  await user.type(await screen.findByRole("textbox", { name: "Filesystem mount path" }), "/mnt/cache");
  await user.click(useButton());
  await user.click(within(dialog()).getByRole("button", { name: "Cancel" }));
  expect(request).toHaveBeenCalledTimes(1);
});

it("shows verified capacity, availability and a long pinned image that wraps", async () => {
  serve(() => Promise.resolve(verified()));
  renderSection();
  expect(await screen.findByText("Hard cap enforced")).toBeInTheDocument();
  expect(screen.getByRole("meter", { name: "Cache filesystem used" })).toHaveAttribute("aria-valuetext", "5.0 GiB of 20.0 GiB, 25%");
  expect(screen.getByText("15.0 GiB available")).toBeInTheDocument();
  expect(screen.getByText(image)).toHaveClass("break-all");
  expect(useButton()).toBeDisabled();
  expect(screen.getByText("This filesystem is already verified and in use.")).toBeInTheDocument();
});

it("never shows a cached verification after a failed refresh", async () => {
  const user = userEvent.setup();
  let fail = false;
  serve(() => (fail ? Promise.reject(new ApiError(502, "worker unavailable; build-cache verification could not be read or completed")) : Promise.resolve(verified())));
  const client = renderSection();
  expect(await screen.findByText("Hard cap enforced")).toBeInTheDocument();
  fail = true;
  await client.refetchQueries({ queryKey: ["nodes", "n1", "build-cache"] });
  expect(await screen.findByText("Worker unavailable")).toBeInTheDocument();
  expect(screen.queryByText("Hard cap enforced")).not.toBeInTheDocument();
  expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Verify and use filesystem" })).not.toBeInTheDocument();
  fail = false;
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByText("Hard cap enforced")).toBeInTheDocument();
});

it("drops the verified save message when the refresh after saving fails", async () => {
  const user = userEvent.setup();
  let fail = false;
  serve(
    () => (fail ? Promise.reject(new ApiError(502, "worker unavailable; build-cache verification could not be read or completed")) : Promise.resolve(status())),
    () => {
      fail = true;
      return Promise.resolve(verified());
    },
  );
  renderSection();
  await user.type(await screen.findByRole("textbox", { name: "Filesystem mount path" }), "/mnt/cache");
  await user.click(useButton());
  await user.click(within(dialog()).getByRole("button", { name: "Verify and use filesystem" }));
  expect(await screen.findByText("Worker unavailable")).toBeInTheDocument();
  expect(screen.queryByText(/Hard cap verified/)).not.toBeInTheDocument();
  expect(screen.queryByText("Hard cap enforced")).not.toBeInTheDocument();
});

it("treats an incomplete reply as unknown, not as no cap or verified", async () => {
  serve(() => Promise.resolve({ configured: true, verified: true, path: "/mnt/cache" }));
  renderSection();
  expect(await screen.findByText("Build-cache state unknown")).toBeInTheDocument();
  expect(screen.getByText("The build-cache status was incomplete.")).toBeInTheDocument();
  expect(screen.queryByText("Hard cap enforced")).not.toBeInTheDocument();
  expect(screen.queryByText("No hard cap")).not.toBeInTheDocument();
});

it("reports the worker's missing status as unknown", async () => {
  serve(() => Promise.reject(new ApiError(409, "worker did not return build-cache verification; upgrade and verify it")));
  renderSection();
  expect(await screen.findByText("Build-cache state unknown")).toBeInTheDocument();
  expect(screen.getByText(/upgrade and verify it/)).toBeInTheDocument();
});

it("asks for a worker upgrade without reading status when the capability is absent", () => {
  renderSection(node(false));
  expect(screen.getByText("Worker upgrade required")).toBeInTheDocument();
  expect(screen.getByText("Whether a hard cap is enforced on this node is unknown until the worker is upgraded and verified.")).toBeInTheDocument();
  expect(screen.queryByText(/ordinary garbage collection/)).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("does not read status from a node that is not ready", () => {
  renderSection(node(true, "unreachable"));
  expect(screen.getByText("Build-cache state unknown")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("pauses changes while the browser is offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => Promise.resolve(verified({ verified: false, reason: "mount changed" })));
  renderSection();
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(useButton()).toBeDisabled();
  expect(screen.getByRole("button", { name: "Disable hard cap" })).toBeDisabled();
  vi.restoreAllMocks();
});

it("keeps a saved invalid path and allows verifying the same path again", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(status({ configured: true, path: "/mnt/cache", reason: "filesystem identity changed" })));
  renderSection();
  expect(await screen.findByText("Saved filesystem not verified")).toBeInTheDocument();
  expect(screen.getByText("filesystem identity changed")).toBeInTheDocument();
  expect(pathField()).toHaveValue("/mnt/cache");
  expect(useButton()).toBeEnabled();
  await user.click(useButton());
  await user.click(within(dialog()).getByRole("button", { name: "Verify and use filesystem" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/build-cache", { path: "/mnt/cache" }));
});

it("keeps the draft when the worker rejects the path", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(status()), () => Promise.reject(new ApiError(409, "the build-cache path must be mode 0700")));
  renderSection();
  await user.type(await screen.findByRole("textbox", { name: "Filesystem mount path" }), "/mnt/other");
  await user.click(useButton());
  await user.click(within(dialog()).getByRole("button", { name: "Verify and use filesystem" }));
  expect(await screen.findByText("Verifying the filesystem did not complete")).toBeInTheDocument();
  expect(screen.getByText("the build-cache path must be mode 0700")).toBeInTheDocument();
  expect(screen.getByText("Your path is kept. Check the refreshed status: the worker may have saved the setting or its ownership marker before the reply failed.")).toBeInTheDocument();
  expect(screen.queryByText(/Nothing on the filesystem was changed/)).not.toBeInTheDocument();
  expect(pathField()).toHaveValue("/mnt/other");
});

it("does not claim the setting is unchanged when disabling fails", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(verified()), () => Promise.reject(new ApiError(502, "worker unavailable")));
  renderSection();
  await user.click(await screen.findByRole("button", { name: "Disable hard cap" }));
  await user.click(within(dialog()).getByRole("button", { name: "Disable hard cap" }));
  expect(await screen.findByText("Disabling did not complete")).toBeInTheDocument();
  expect(screen.getByText("Check the refreshed status: the worker may have saved the setting or its ownership marker before the reply failed.")).toBeInTheDocument();
  expect(screen.queryByText(/unchanged/)).not.toBeInTheDocument();
});

it("rejects an unclean path before asking for confirmation", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(status()));
  renderSection();
  await user.type(await screen.findByRole("textbox", { name: "Filesystem mount path" }), "mnt/cache");
  await user.click(useButton());
  expect(screen.getByText("Use an absolute Linux path, such as /mnt/cove-buildkit.")).toBeInTheDocument();
  expect(pathField()).toHaveAttribute("aria-invalid", "true");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("blocks changes while an operation runs", async () => {
  serve(() => Promise.resolve(verified({ active_operation_id: "op1", path: "/mnt/cache" })));
  renderSection();
  expect(await screen.findByText("An operation is running on this node")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Disable hard cap" })).toBeDisabled();
  expect(useButton()).toBeDisabled();
});

it("lets an administrator retry pending cleanup by verifying the saved path again", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(verified({ cleanup_pending: true, reason: "owned build-cache cleanup is pending; new builds remain paused" })));
  renderSection();
  expect(await screen.findByText("Unfinished build-cache cleanup")).toBeInTheDocument();
  expect(screen.getByText(/Verify the saved path again or disable the cap to retry it/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Disable hard cap" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Retry cleanup" })).not.toBeInTheDocument();
  expect(pathField()).toHaveValue("/mnt/cache");
  await user.click(useButton());
  await user.click(within(dialog()).getByRole("button", { name: "Verify and use filesystem" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/build-cache", { path: "/mnt/cache" }));
});

it("offers to retry cleanup of a retired builder when no cap is configured", async () => {
  const user = userEvent.setup();
  serve(() => Promise.resolve(status({ cleanup_pending: true, reason: "owned build-cache cleanup is pending; new builds remain paused" })), () => Promise.resolve(status()));
  renderSection();
  expect(await screen.findByText("Unfinished build-cache cleanup")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disable hard cap" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Retry cleanup" }));
  expect(dialog()).toHaveTextContent(/No hard cap is configured, and none is added/);
  await user.click(within(dialog()).getByRole("button", { name: "Retry cleanup" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/build-cache", { path: "" }));
  expect(vi.mocked(request).mock.calls.filter(([method]) => method === "PUT")).toEqual([["PUT", "/api/nodes/n1/build-cache", { path: "" }]]);  expect(await screen.findByText("Cleanup finished. No hard cap is configured.")).toBeInTheDocument();
});

it("still allows disabling a saved cap when the worker cannot configure one", async () => {
  serve(() => Promise.resolve(status({ configured: true, can_configure: false, path: "/mnt/cache", reason: "the local Docker endpoint cannot be verified" })));
  renderSection();
  expect(await screen.findByText("This worker cannot verify a dedicated filesystem.")).toBeInTheDocument();
  expect(useButton()).toBeDisabled();
  expect(screen.getByRole("button", { name: "Disable hard cap" })).toBeEnabled();
});

it("renders nothing and requests nothing for people who are not instance administrators", () => {
  session.is_admin = false;
  const { container } = render(
    <QueryClientProvider client={new QueryClient()}>
      <BuildCacheSection node={node()} />
    </QueryClientProvider>,
  );
  expect(container).toBeEmptyDOMElement();
  expect(request).not.toHaveBeenCalled();
});

it("validates paths like the worker and formats bytes", () => {
  expect(cachePathError("/mnt/cache")).toBeUndefined();
  for (const bad of ["", "relative", "/", "/mnt/cache/", "/mnt//cache", "/mnt/../cache", "/mnt/a,b", `/${"a".repeat(4096)}`]) expect(cachePathError(bad)).toBeDefined();
  expect(formatBytes(512)).toBe("512 B");
  expect(formatBytes(1.5 * gib)).toBe("1.5 GiB");
});
