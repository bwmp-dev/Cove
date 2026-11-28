import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { AuditPage } from "./audit";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: false }), useCurrentTeam: () => ({ id: "t1", name: "Team", role: "admin", grants: [] }) }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

it("shows the supplied reason as plain text and makes it searchable", async () => {
  vi.mocked(request).mockResolvedValue([
    { id: 2, actor: "user-1", action: "deployment reason", subject: "op-1", status: 200, created_at: "2026-11-22T09:00:00Z", details: { reason: "<b>Hotfix</b> for INC-42" } },
    { id: 1, actor: "user-1", action: "POST /api/workloads", subject: "w1", status: 201, created_at: "2026-11-22T08:00:00Z", details: {} },
  ]);
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AuditPage /></QueryClientProvider>);
  expect(await screen.findByText("Reason: <b>Hotfix</b> for INC-42")).toBeInTheDocument();
  fireEvent.change(screen.getByRole("searchbox"), { target: { value: "inc-42" } });
  expect(screen.getByText("1 of 2 events")).toBeInTheDocument();
  expect(screen.queryByText("POST /api/workloads")).not.toBeInTheDocument();
});
