import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { OutboundProxySettings } from "../api/instance-settings";
import { OutboundProxy } from "./OutboundProxy";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));

afterEach(() => vi.clearAllMocks());

const saved: OutboundProxySettings = {
  http_proxy: "http://proxy.example.com:3128",
  https_proxy: "http://proxy.example.com:3128",
  no_proxy: "10.0.0.0/8",
  http_credentials_set: true,
  https_credentials_set: true,
};

function renderProxy(put: (body: unknown) => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method, _path, body) => (method === "GET" ? Promise.resolve(saved) : put(body)));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <OutboundProxy />
    </QueryClientProvider>,
  );
  return client;
}

const puts = () => vi.mocked(request).mock.calls.filter(([method]) => method === "PUT");

it("sends only the edited no_proxy list, so saved proxy URLs and their credentials are kept", async () => {
  renderProxy((body) => Promise.resolve({ ...saved, ...(body as object) }));
  expect(await screen.findAllByText("Credentials saved")).toHaveLength(2);
  fireEvent.change(screen.getByLabelText(/Bypass the proxy for/), { target: { value: " 10.0.0.0/8, .internal.example.com " } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]).toEqual(["PUT", "/api/settings/outbound-proxy", { no_proxy: "10.0.0.0/8, .internal.example.com" }]);
  expect(await screen.findByText("Outbound proxy saved")).toBeInTheDocument();
});

it("replaces one proxy with a masked credential-bearing URL and resets the input after saving", async () => {
  const client = renderProxy(() => Promise.resolve({ ...saved, https_proxy: "https://new.example.com:8443", https_credentials_set: true }));
  fireEvent.click(await screen.findByRole("button", { name: "Replace the HTTPS proxy" }));
  const input = screen.getByLabelText("HTTPS proxy");
  expect(input).toHaveAttribute("type", "password");
  fireEvent.change(input, { target: { value: "https://user:s3cret@new.example.com:8443" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]?.[2]).toEqual({ https_proxy: "https://user:s3cret@new.example.com:8443" });
  expect(await screen.findByText("https://new.example.com:8443")).toBeInTheDocument();
  expect(screen.queryByLabelText("HTTPS proxy")).not.toBeInTheDocument();
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain("s3cret");
  expect(client.getMutationCache().getAll().every((mutation) => mutation.options.gcTime === 0)).toBe(true);
});

it("clears a proxy with an explicit empty string and can undo before saving", async () => {
  renderProxy((body) => Promise.resolve({ ...saved, ...(body as object), http_credentials_set: false }));
  fireEvent.click(await screen.findByRole("button", { name: "Clear the HTTP proxy" }));
  expect(screen.getByText("Cleared when saved")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Undo clearing the HTTP proxy" }));
  expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Clear the HTTP proxy" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]?.[2]).toEqual({ http_proxy: "" });
  expect(await screen.findByText("Not set")).toBeInTheDocument();
});

it("refuses a URL with a path before sending and shows the server's refusal", async () => {
  renderProxy(() => Promise.reject(new ApiError(400, "valid HTTP(S) proxy origins and NO_PROXY list required")));
  fireEvent.click(await screen.findByRole("button", { name: "Replace the HTTP proxy" }));
  fireEvent.change(screen.getByLabelText("HTTP proxy"), { target: { value: "http://proxy.example.com:3128/path" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText(/without a path/)).toBeInTheDocument();
  expect(screen.getByLabelText("HTTP proxy")).toHaveAttribute("aria-invalid", "true");
  expect(puts()).toHaveLength(0);
  fireEvent.change(screen.getByLabelText("HTTP proxy"), { target: { value: "http://proxy.example.com:3128" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Could not save the outbound proxy")).toBeInTheDocument();
  expect(screen.getByText(/NO_PROXY list required/)).toBeInTheDocument();
  expect(screen.getByLabelText("HTTP proxy")).toHaveValue("http://proxy.example.com:3128");
});

it("shows a retryable error when the settings cannot be read", async () => {
  vi.mocked(request).mockRejectedValue(new ApiError(503, "outbound settings unavailable"));
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <OutboundProxy />
    </QueryClientProvider>,
  );
  expect(await screen.findByText(/outbound settings unavailable/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
});
