import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { InstanceIntegrations } from "./InstanceIntegrations";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Settings unavailable" }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }), Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
afterEach(() => vi.clearAllMocks());

const saved = {
  smtp: { host: "smtp.example.com", port: "465", username: "mailer", from: "cove@example.com", password_set: true },
  github: { client_id: "fixture", client_secret_set: true, callback_url: "https://cove.example.com/auth/github/callback" },
};

it("preserves saved secrets when editing settings and clears newly entered secrets after saving", async () => {
  vi.mocked(request).mockImplementation(() => Promise.resolve(saved));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><InstanceIntegrations /></QueryClientProvider>);
  const host = await screen.findByLabelText("SMTP host");
  expect(screen.getByLabelText("SMTP password", { exact: false })).toHaveValue("");
  fireEvent.change(host, { target: { value: "new.example.com" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Save changes" })[0]!);
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/integrations", { smtp: { host: "new.example.com", port: "465", username: "mailer", from: "cove@example.com" } }));
  const secret = screen.getByLabelText("Client secret");
  fireEvent.change(secret, { target: { value: "new-secret" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Save changes" })[1]!);
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/integrations", { github: { client_id: "fixture", client_secret: "new-secret" } }));
  await waitFor(() => expect(secret).toHaveValue(""));
  expect(screen.getByText(saved.github.callback_url)).toBeInTheDocument();
});

it("tests saved SMTP settings and reports delivery acceptance", async () => {
  vi.mocked(request).mockImplementation(() => Promise.resolve(saved));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><InstanceIntegrations /></QueryClientProvider>);
  fireEvent.change(await screen.findByLabelText("Test recipient"), { target: { value: "owner@example.com" } });
  fireEvent.click(screen.getByRole("button", { name: "Send test email" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/smtp/test", { email: "owner@example.com" }));
  expect(await screen.findByText(/Test email accepted/)).toBeInTheDocument();
});
