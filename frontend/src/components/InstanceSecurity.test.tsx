import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { AuditExport } from "./InstanceSecurity";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
afterEach(() => vi.resetAllMocks());

const saved = { configuration: { webhook_url: "", syslog_address: "", syslog_tls: true, retention_days: 365 }, webhook_secret_configured: false, pending_deliveries: 0 };

it("describes the timestamp-bound signature and keeps the shared audit period current", async () => {
  vi.mocked(request).mockImplementation((method: string, _path: string, body?: unknown) => Promise.resolve(method === "PUT" ? { ...saved, configuration: body } : saved));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(client, "invalidateQueries");
  render(<QueryClientProvider client={client}><AuditExport /></QueryClientProvider>);

  expect(await screen.findByText(/X-Cove-Timestamp \(Unix seconds\), X-Cove-Signature: sha256=<HMAC of the timestamp, a dot and the body>/)).toBeInTheDocument();
  expect(screen.getByText(/same setting as Audit log in Telemetry retention/)).toBeInTheDocument();

  const syslog = screen.getByLabelText("Syslog collector");
  fireEvent.change(syslog, { target: { value: "logs.example.com:6514" } });
  client.setQueryData(["audit-export"], { ...saved, configuration: { ...saved.configuration, retention_days: 30 } });
  await waitFor(() => expect(screen.getByLabelText("Keep audit events for")).toHaveValue(30));
  expect(syslog).toHaveValue("logs.example.com:6514");

  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/audit-export", { webhook_url: "", syslog_address: "logs.example.com:6514", syslog_tls: true, retention_days: 30 }));
  await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["settings", "retention"] }));
});
