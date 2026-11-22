import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { AdminPage } from "./admin";

const session = vi.hoisted(() => ({ is_admin: true }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Update unavailable" }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
vi.mock("@tanstack/react-router", () => ({ useRouterState: () => "", useBlocker: () => ({ status: "idle" }), Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); session.is_admin = true; });

it("saves the chosen update channel and shows the installed version", async () => {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/settings/integrations") return Promise.resolve({ smtp: { host: "", port: "", username: "", from: "", password_set: false }, github: { client_id: "", client_secret_set: false, callback_url: "https://cove.example.com/auth/github/callback" } });
    if (path === "/api/settings") return Promise.resolve({ registration_enabled: false, email_enabled: false, github_enabled: false });
    return Promise.resolve({ channel: method === "PUT" ? "beta" : "release", version: "v0.1.0", build_channel: "release", revision: "abc" });
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AdminPage /></QueryClientProvider>);
  expect(await screen.findByText("v0.1.0")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Update channel" }));
  fireEvent.click(await screen.findByRole("option", { name: "Beta — every successful main build" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/updates", { channel: "beta" }));
  expect(await screen.findByText("Update channel saved")).toBeInTheDocument();
});

it("keeps channel controls private to instance administrators", () => {
  session.is_admin = false;
  render(<QueryClientProvider client={new QueryClient()}><AdminPage /></QueryClientProvider>);
  expect(screen.queryByRole("combobox", { name: "Update channel" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});
