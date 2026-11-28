import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { certificateRetriesKey } from "../api/certificates";
import { ApiError, request } from "../api/client";
import type { Node, Route, RouteCertificate, Workload } from "../api/types";
import { ResourceDomains } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
  this.dispatchEvent(new Event("close"));
};
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

// API-shaped fixtures for one resource; the PEM blocks are placeholders, not real key material.
const origin = "a".repeat(32);
const spare = "b".repeat(32);
const chainPem = "-----BEGIN CERTIFICATE-----\nTESTCERTIFICATE\n-----END CERTIFICATE-----\n";
const keyPem = "-----BEGIN PRIVATE KEY-----\nTESTPRIVATEKEYMATERIAL\n-----END PRIVATE KEY-----\n";

function certificate(id: string, name: string, patch: Partial<RouteCertificate> = {}): RouteCertificate {
  return { id, name, fingerprint: "c".repeat(64), common_name: "app.example.com", dns_names: ["app.example.com"], not_before: "2026-01-01T00:00:00Z", expires_at: "2099-01-01T00:00:00Z", created_at: "2026-01-02T00:00:00Z", ...patch };
}

const uploadedRoute: Route = { domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: origin } };
const plainRoute: Route = { domain: "app.example.com", service: "web", port: 8080 };

function workload(routes: Route[] | null, permissions = ["read", "operate", "edit"]): Workload {
  return { id: "w1", name: "web", node_id: "n1", kind: "service", compose: "services:\n  web:\n    image: nginx\n", source: null, environment_keys: [], has_source_token: false, permissions, manually_stopped: false, deployed: true, project_id: "p1", guarded_deploy: false, routes } as Workload;
}

function node(routeProtocol: number): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, route_policies: true, route_protocol: routeProtocol } };
}

type Handler = (method: string, path: string, body: unknown) => Promise<unknown> | undefined;

function renderPage({ routes = [uploadedRoute], protocol = 3, certificates = () => Promise.resolve([certificate(origin, "Origin CA 2026")]), permissions, handle }: { routes?: Route[] | null; protocol?: number; certificates?: () => Promise<unknown>; permissions?: string[]; handle?: Handler } = {}) {
  resource = workload(routes, permissions);
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const handled = handle?.(method, path, body);
    if (handled) return handled;
    if (path === "/api/nodes") return Promise.resolve([node(protocol)] as never);
    if (path.startsWith("/api/operations")) return Promise.resolve([] as never);
    if (method === "GET" && path === "/api/workloads/w1/certificates") return certificates();
    if (method === "GET" && (path === "/api/workloads/w1/certificate-issuers" || path === "/api/workloads/w1/certificate-zones")) return Promise.resolve([] as never);
    if (method === "PUT" && path === "/api/workloads/w1/routes") return Promise.resolve({ state: "saved" } as never);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceDomains />
    </QueryClientProvider>,
  );
  return client;
}

const calls = (method: string, path: string) => vi.mocked(request).mock.calls.filter(([m, p]) => m === method && p === path);
const routePuts = () => calls("PUT", "/api/workloads/w1/routes");

async function openAdvanced() {
  await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
}

async function chooseOption(combobox: HTMLElement, name: string | RegExp) {
  await userEvent.click(combobox);
  await userEvent.click(await screen.findByRole("option", { name }));
}

describe("route certificate selection", () => {
  it("shows the saved certificate with its metadata and saves other changes with the selection unchanged", async () => {
    renderPage();
    await openAdvanced();
    expect(await screen.findByRole("combobox", { name: "Certificate version" })).toHaveTextContent(/^Origin CA 2026/);
    expect(screen.getAllByText("app.example.com", { selector: "span" }).length).toBeGreaterThan(0);
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(screen.queryByText("Saving may briefly restart the shared proxy")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [{ ...uploadedRoute, policy: { ...uploadedRoute.policy, sticky: true } }] });
  });

  it("keeps a saved ID when the certificate list cannot be loaded, and offers Retry", async () => {
    let fail = true;
    renderPage({ certificates: () => (fail ? Promise.reject(new ApiError(503, "Certificates unavailable")) : Promise.resolve([certificate(origin, "Origin CA 2026")])) });
    await openAdvanced();
    const picker = await screen.findByRole("combobox", { name: "Certificate version" });
    expect(picker).toHaveTextContent(`Saved certificate ${origin.slice(0, 8)}`);
    expect(picker).toBeDisabled();
    expect((await screen.findAllByText("Could not load stored certificates")).length).toBeGreaterThan(0);
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [{ ...uploadedRoute, policy: { ...uploadedRoute.policy, sticky: true } }] });
    fail = false;
    await userEvent.click(within(screen.getByText("Could not load stored certificates").closest("div[class*=border]") as HTMLElement).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Certificate version" })).toHaveTextContent("Origin CA 2026"));
  });

  it("names a saved ID that is no longer listed instead of dropping it", async () => {
    renderPage({ certificates: () => Promise.resolve([certificate(spare, "Spare")]) });
    await openAdvanced();
    expect(await screen.findByRole("combobox", { name: "Certificate version" })).toHaveTextContent(`Unknown certificate ${origin.slice(0, 8)}`);
    expect(screen.getByText(/is not among this resource's stored certificates/)).toBeInTheDocument();
  });

  it("opens a resource with no saved routes, sent as null by the API, and saves its first route", async () => {
    renderPage({ routes: null, certificates: () => Promise.resolve([]) });
    expect(await screen.findByText("No certificates are stored for this resource.")).toBeInTheDocument();
    expect(screen.queryByText("Saving may briefly restart the shared proxy")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add route" }));
    await userEvent.type(screen.getByLabelText("Hostname"), "app.example.com");
    await userEvent.type(screen.getByLabelText("Compose service"), "web");
    await userEvent.clear(screen.getByLabelText("Container port"));
    await userEvent.type(screen.getByLabelText("Container port"), "8080");
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [plainRoute] });
  });

  it("selects an uploaded certificate for a route and switches away by naming the new source", async () => {
    renderPage({ routes: [plainRoute], certificates: () => Promise.resolve([certificate(origin, "Origin CA 2026")]) });
    await openAdvanced();
    await chooseOption(screen.getByRole("combobox", { name: "Certificate" }), "Stored certificate");
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText("Choose a certificate version.")).toBeInTheDocument();
    expect(routePuts()).toHaveLength(0);
    await chooseOption(screen.getByRole("combobox", { name: "Certificate version" }), /^Origin CA 2026/);
    expect(screen.getByText("Saving may briefly restart the shared proxy")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [uploadedRoute] });
  });

  it("sends Let's Encrypt explicitly and no certificate ID when switching a saved selection away", async () => {
    renderPage();
    await openAdvanced();
    await chooseOption(await screen.findByRole("combobox", { name: "Certificate" }), "Let's Encrypt");
    expect(screen.queryByRole("combobox", { name: "Certificate version" })).not.toBeInTheDocument();
    expect(screen.getByText("Saving may briefly restart the shared proxy")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "acme" } }] });
  });

  it("reports a certificate naming hostnames outside the resource's routes before saving", async () => {
    renderPage({ certificates: () => Promise.resolve([certificate(origin, "Wide", { dns_names: ["app.example.com", "api.example.com"] })]) });
    await openAdvanced();
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Certificate version" })).toHaveTextContent("Wide"));
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/also names api\.example\.com/)).toBeInTheDocument();
    expect(routePuts()).toHaveLength(0);
  });

  it("re-checks a reported certificate error against the current choice and still refuses an invalid one", async () => {
    renderPage({ certificates: () => Promise.resolve([certificate(origin, "Wide", { dns_names: ["app.example.com", "api.example.com"] }), certificate(spare, "Narrow")]) });
    await openAdvanced();
    const picker = await screen.findByRole("combobox", { name: "Certificate version" });
    await waitFor(() => expect(picker).toHaveTextContent("Wide"));
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/also names api\.example\.com/)).toBeInTheDocument();
    expect(picker).toHaveAttribute("aria-invalid", "true");

    await chooseOption(picker, /^Narrow/);
    expect(screen.queryByText(/also names api\.example\.com/)).not.toBeInTheDocument();
    expect(picker).not.toHaveAttribute("aria-invalid");

    await chooseOption(picker, /^Wide/);
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/also names api\.example\.com/)).toBeInTheDocument();
    expect(picker).toHaveAttribute("aria-invalid", "true");
    expect(routePuts()).toHaveLength(0);
  });

  it("keeps a saved selection visible on an older worker and lets it be switched away explicitly", async () => {
    renderPage({ protocol: 2 });
    await openAdvanced();
    expect(await screen.findByText("Stored certificates need a newer worker")).toBeInTheDocument();
    expect(await screen.findByRole("combobox", { name: "Certificate version" })).toBeDisabled();
    await userEvent.click(screen.getByRole("combobox", { name: "Certificate" }));
    expect(await screen.findByRole("option", { name: "Proxy default certificate" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.click(screen.getByRole("option", { name: "Let's Encrypt" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(routePuts()).toHaveLength(1));
    expect(routePuts()[0]?.[2]).toEqual({ routes: [{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "acme" } }] });
  });

  it("does not offer a new uploaded selection on an older worker", async () => {
    renderPage({ routes: [plainRoute], protocol: 2 });
    await openAdvanced();
    await userEvent.click(screen.getByRole("combobox", { name: "Certificate" }));
    expect(await screen.findByRole("option", { name: "Stored certificate" })).toHaveAttribute("aria-disabled", "true");
  });
});

describe("certificate upload and deletion", () => {
  it("uploads from component state only, clears the PEM fields and leaves no key material in caches or storage", async () => {
    const stored: RouteCertificate[] = [];
    const client = renderPage({
      routes: [plainRoute],
      certificates: () => Promise.resolve([...stored]),
      handle: (method, path, body) => {
        if (method !== "POST" || path !== "/api/workloads/w1/certificates") return undefined;
        stored.push(certificate(origin, (body as { name: string }).name));
        return Promise.resolve(stored[0]);
      },
    });
    expect(await screen.findByText("No certificates are stored for this resource.")).toBeInTheDocument();
    const form = screen.getByRole("form", { name: "Upload a certificate" });
    await userEvent.type(within(form).getByLabelText("Name"), "Origin CA 2026");
    await userEvent.click(within(form).getByLabelText("Certificate chain"));
    await userEvent.paste(chainPem);
    await userEvent.click(within(form).getByLabelText("Private key"));
    await userEvent.paste(keyPem);
    await userEvent.click(within(form).getByRole("button", { name: "Upload certificate" }));
    await waitFor(() => expect(calls("POST", "/api/workloads/w1/certificates")).toHaveLength(1));
    expect(calls("POST", "/api/workloads/w1/certificates")[0]?.[2]).toEqual({ name: "Origin CA 2026", certificate_pem: chainPem, private_key_pem: keyPem });
    expect(await screen.findByText("Certificate uploaded")).toBeInTheDocument();
    expect(within(form).getByLabelText("Certificate chain")).toHaveValue("");
    expect(within(form).getByLabelText("Private key")).toHaveValue("");
    expect(within(screen.getByRole("list", { name: "Stored certificates" })).getByText("Origin CA 2026")).toBeInTheDocument();
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    const cached = JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data));
    expect(cached).not.toContain("PRIVATE KEY");
    expect(cached).not.toContain("TESTCERTIFICATE");
    expect(JSON.stringify({ ...localStorage })).not.toContain("PRIVATE KEY");
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("PRIVATE KEY");
    expect(window.location.href).not.toContain("PRIVATE");
  });

  it("keeps the typed input and shows the API's reason when an upload is rejected", async () => {
    const client = renderPage({
      routes: [plainRoute],
      certificates: () => Promise.resolve([]),
      handle: (method, path) => (method === "POST" && path === "/api/workloads/w1/certificates" ? Promise.reject(new ApiError(400, "Certificate and private key do not form a valid TLS key pair")) : undefined),
    });
    const form = await screen.findByRole("form", { name: "Upload a certificate" });
    await userEvent.type(within(form).getByLabelText("Name"), "Mismatch");
    await userEvent.click(within(form).getByLabelText("Certificate chain"));
    await userEvent.paste(chainPem);
    await userEvent.click(within(form).getByLabelText("Private key"));
    await userEvent.paste(keyPem);
    await userEvent.click(within(form).getByRole("button", { name: "Upload certificate" }));
    expect(await screen.findByText("Certificate and private key do not form a valid TLS key pair")).toBeInTheDocument();
    expect(within(form).getByLabelText("Private key")).toHaveValue(keyPem);
    expect(within(form).getByLabelText("Name")).toHaveValue("Mismatch");
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    await userEvent.click(within(form).getByRole("button", { name: "Clear" }));
    expect(within(form).getByLabelText("Private key")).toHaveValue("");
    expect(screen.queryByText("Certificate and private key do not form a valid TLS key pair")).not.toBeInTheDocument();
  });

  it("blocks deleting a certificate saved routes use, and deletes an unused one after confirmation", async () => {
    renderPage({
      certificates: () => Promise.resolve([certificate(origin, "Origin CA 2026"), certificate(spare, "Spare")]),
      handle: (method, path) => (method === "DELETE" && path === `/api/workloads/w1/certificates/${spare}` ? Promise.resolve({ state: "deleted" }) : undefined),
    });
    const list = await screen.findByRole("list", { name: "Stored certificates" });
    expect(within(list).getByRole("button", { name: "Delete certificate Origin CA 2026" })).toBeDisabled();
    expect(within(list).getByText("Used by saved routes")).toBeInTheDocument();
    await userEvent.click(within(list).getByRole("button", { name: "Delete certificate Spare" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete certificate Spare?" });
    expect(dialog).toHaveTextContent("does not revoke the certificate");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete certificate" }));
    await waitFor(() => expect(calls("DELETE", `/api/workloads/w1/certificates/${spare}`)).toHaveLength(1));
  });

  it("never puts the private key in mutation variables, even while a rejected upload is in flight", async () => {
    let reject: (error: Error) => void = () => undefined;
    const client = renderPage({
      routes: [plainRoute],
      certificates: () => Promise.resolve([]),
      handle: (method, path) => (method === "POST" && path === "/api/workloads/w1/certificates" ? new Promise((_, fail) => (reject = fail)) : undefined),
    });
    const form = await screen.findByRole("form", { name: "Upload a certificate" });
    await userEvent.type(within(form).getByLabelText("Name"), "Mismatch");
    await userEvent.click(within(form).getByLabelText("Certificate chain"));
    await userEvent.paste(chainPem);
    await userEvent.click(within(form).getByLabelText("Private key"));
    await userEvent.paste(keyPem);
    await userEvent.click(within(form).getByRole("button", { name: "Upload certificate" }));
    await waitFor(() => expect(calls("POST", "/api/workloads/w1/certificates")).toHaveLength(1));
    expect(calls("POST", "/api/workloads/w1/certificates")[0]?.[2]).toEqual({ name: "Mismatch", certificate_pem: chainPem, private_key_pem: keyPem });
    const mutations = () => JSON.stringify(client.getMutationCache().getAll().map((mutation) => [mutation.state.variables, mutation.state.data, mutation.state.context]));
    expect(client.getMutationCache().getAll()).toHaveLength(1);
    expect(mutations()).not.toContain("PRIVATE KEY");
    reject(new ApiError(400, "Certificate and private key do not form a valid TLS key pair"));
    expect(await screen.findByText("Certificate and private key do not form a valid TLS key pair")).toBeInTheDocument();
    expect(mutations()).not.toContain("PRIVATE KEY");
    expect(mutations()).not.toContain("TESTCERTIFICATE");
  });

  it("shows the API's conflict when a certificate became used after the list loaded", async () => {
    renderPage({
      routes: [plainRoute],
      handle: (method, path) => (method === "DELETE" && path === `/api/workloads/w1/certificates/${origin}` ? Promise.reject(new ApiError(409, "Remove this certificate from all routes before deleting it")) : undefined),
    });
    const list = await screen.findByRole("list", { name: "Stored certificates" });
    await userEvent.click(within(list).getByRole("button", { name: "Delete certificate Origin CA 2026" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete certificate Origin CA 2026?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete certificate" }));
    expect(await within(dialog).findByText(/Remove this certificate from all routes before deleting it/)).toBeInTheDocument();
  });

  it("shows viewers metadata and the selection without upload or delete controls", async () => {
    renderPage({ permissions: ["read"], certificates: () => Promise.resolve([certificate(origin, "Origin CA 2026", { expires_at: "2026-01-01T00:00:00Z", not_before: "2025-01-01T00:00:00Z" })]) });
    const list = await screen.findByRole("list", { name: "Stored certificates" });
    expect(within(list).getAllByText("Expired").length).toBeGreaterThan(0);
    expect(await screen.findByText("stored certificate Origin CA 2026")).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Upload a certificate" })).not.toBeInTheDocument();
    expect(within(list).queryByRole("button")).not.toBeInTheDocument();
  });

  it("reports a failed list with Retry and no upload limit guesswork", async () => {
    renderPage({ certificates: () => Promise.reject(new ApiError(403, "Access denied.")) });
    expect(await screen.findByText("Could not load certificates")).toBeInTheDocument();
  });
});

describe("pending public certificate retries", () => {
  const retriesPath = "/api/workloads/w1/certificate-retries";
  const message = "Certificate issuance is still pending. Cove retries automatically; check this hostname's DNS and public reachability on ports 80 and 443. Repeated failures slow retries to once every six hours.";
  const retry = (hostname: string, attempts = 1) => ({ hostname, attempts, next_attempt_at: "2099-01-01T00:00:00Z", message });
  const storedRoute: Route = { domain: "secure.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: origin } };
  const defaultRoute: Route = { domain: "default.example.com", service: "web", port: 8080, policy: { certificate_source: "default" } };
  const wildcardRoute: Route = { domain: "*.example.com", service: "web", port: 8080 };

  it("lists retries only beside current Let's Encrypt hostnames, without a manual retry", async () => {
    renderPage({
      routes: [plainRoute, storedRoute, defaultRoute, wildcardRoute],
      handle: (method, path) => (method === "GET" && path === retriesPath ? Promise.resolve([retry("app.example.com"), retry("secure.example.com"), retry("default.example.com"), retry("gone.example.com", 3)]) : undefined),
    });
    const list = await screen.findByRole("list", { name: "Pending certificate retries" });
    expect(screen.getByText("Certificate issuance pending")).toBeInTheDocument();
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText("app.example.com")).toBeInTheDocument();
    expect(within(list).getByText(/^Next automatic retry due/)).toBeInTheDocument();
    expect(within(list).queryByText(/configuration retr/)).not.toBeInTheDocument();
    expect(within(list).getByText(message)).toBeInTheDocument();
    expect(within(list).getByRole("time")).toHaveAttribute("dateTime", "2099-01-01T00:00:00Z");
    expect(within(list).queryByText(/secure\.example\.com|default\.example\.com|gone\.example\.com/)).not.toBeInTheDocument();
    expect(within(list).queryByRole("button")).not.toBeInTheDocument();
  });

  it("does not read retries when no route uses Let's Encrypt", async () => {
    renderPage({ routes: [storedRoute, defaultRoute, wildcardRoute] });
    expect(await screen.findByRole("list", { name: "Stored certificates" })).toBeInTheDocument();
    expect(calls("GET", retriesPath)).toHaveLength(0);
  });

  it("shows no status for an empty list rather than claiming a certificate was issued", async () => {
    const client = renderPage({ routes: [plainRoute], handle: (method, path) => (method === "GET" && path === retriesPath ? Promise.resolve([]) : undefined) });
    await waitFor(() => expect(client.getQueryState(certificateRetriesKey("w1"))?.status).toBe("success"));
    expect(screen.queryByText("Certificate issuance pending")).not.toBeInTheDocument();
    expect(screen.queryByText("Could not load certificate retry status")).not.toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Pending certificate retries" })).not.toBeInTheDocument();
  });

  it("shows the worker upgrade message when the worker cannot report retries", async () => {
    renderPage({ routes: [plainRoute], handle: (method, path) => (method === "GET" && path === retriesPath ? Promise.reject(new ApiError(409, "Upgrade this worker to enable automatic public certificate retries.")) : undefined) });
    const error = (await screen.findByText("Could not load certificate retry status")).closest("[role=alert]") as HTMLElement;
    expect(within(error).getByText(/Upgrade this worker to enable automatic public certificate retries\./)).toBeInTheDocument();
    expect(within(error).getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText("Certificate issuance pending")).not.toBeInTheDocument();
  });

  it("replaces cached retries with an error and Retry when a read fails", async () => {
    let reply: () => Promise<unknown> = () => Promise.resolve([retry("app.example.com", 0)]);
    const client = renderPage({ routes: [plainRoute], handle: (method, path) => (method === "GET" && path === retriesPath ? reply() : undefined) });
    expect(await screen.findByRole("list", { name: "Pending certificate retries" })).toBeInTheDocument();
    reply = () => Promise.reject(new ApiError(503, "certificate retry status unavailable"));
    await client.refetchQueries({ queryKey: certificateRetriesKey("w1") });
    const error = (await screen.findByText("Could not load certificate retry status")).closest("[role=alert]") as HTMLElement;
    expect(screen.queryByRole("list", { name: "Pending certificate retries" })).not.toBeInTheDocument();
    reply = () => Promise.resolve([retry("app.example.com", 2)]);
    await userEvent.click(within(error).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("list", { name: "Pending certificate retries" })).toHaveTextContent("app.example.com");
    expect(screen.queryByText("Could not load certificate retry status")).not.toBeInTheDocument();
  });
});
