import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { Backups, type RetentionSupport } from "./Backups";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

const policy = { repository: "local", keep_last: 5, keep_weekly: 8, interval_hours: 24, drill_interval_hours: 0 };

function renderBackups(saved: typeof policy | null, retention: RetentionSupport) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "PUT") return Promise.resolve({ ok: true });
    if (path.endsWith("/backup-policy")) return Promise.resolve({ backup: saved });
    if (path.endsWith("/backups")) return Promise.resolve({ backups: [] });
    return Promise.resolve([]);
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Backups workloadId="w1" busy={false} canOperate canEdit retention={retention} /></QueryClientProvider>);
}

async function savedPolicy() {
  return waitFor(() => {
    const call = vi.mocked(request).mock.calls.find(([method, path]) => method === "PUT" && path === "/api/workloads/w1/backup-policy");
    if (!call) throw new Error("Policy was not saved");
    return (call[2] as { backup: unknown }).backup;
  });
}

it("summarises and preserves calendar tiers, sending explicit zeros for disabled tiers", async () => {
  renderBackups(policy, { supported: true, nodeName: "edge" });
  expect(await screen.findByText(/Keep 5 latest snapshots plus 8 weekly/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
  expect(screen.getByLabelText("Weekly snapshots")).toHaveValue(8);
  expect(screen.getByLabelText("Daily snapshots")).toHaveValue(0);
  expect(screen.getByLabelText("Monthly snapshots")).toHaveAttribute("max", "120");
  fireEvent.change(screen.getByLabelText("Daily snapshots"), { target: { value: "14" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ keep_last: 5, keep_daily: 14, keep_weekly: 8, keep_monthly: 0, password: "" });
});

it("explains the upgrade and refuses calendar rules on a worker without retention support", async () => {
  renderBackups(null, { supported: false, nodeName: "old-worker" });
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  expect(screen.getByText("Daily, weekly and monthly retention needs an upgraded worker")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  fireEvent.change(screen.getByLabelText("Monthly snapshots"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText("Set these to 0, or upgrade and verify old-worker first.")).toBeInTheDocument();
  expect(screen.getByLabelText("Monthly snapshots")).toHaveFocus();
  expect(screen.getByLabelText("Monthly snapshots")).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.change(screen.getByLabelText("Monthly snapshots"), { target: { value: "0" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ keep_last: 7, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 });
});
