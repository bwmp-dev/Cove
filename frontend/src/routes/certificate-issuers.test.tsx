import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { CertificateChallenge, CertificateIssuer, CertificateZone, Node, Route, RouteCertificate, Workload } from "../api/types";
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
});

// API-shaped fixtures; no real zone, provider or certificate is involved.
const zoneId = "f".repeat(32);
const issued = "1".repeat(32);
const uploadedId = "2".repeat(32);
const zone: CertificateZone = { zone_id: zoneId, domain: "example.com", domains: ["example.com", "*.example.com"] };

function certificate(id: string, name: string, patch: Partial<RouteCertificate> = {}): RouteCertificate {
  return { id, name, source: "uploaded", fingerprint: "c".repeat(64), dns_names: ["app.example.com"], not_before: "2026-01-01T00:00:00Z", expires_at: "2099-01-01T00:00:00Z", created_at: "2026-01-02T00:00:00Z", ...patch };
}

function issuer(patch: Partial<CertificateIssuer> = {}): CertificateIssuer {
  return { id: "i1", name: "Edge origin", provider: "cloudflare_origin", zone_id: zoneId, domains: ["app.example.com"], validity_days: 365, auto_renew: true, phase: "issued", certificate_id: issued, operation_id: "", last_error: "", next_check_at: null, created_at: "2026-11-01T00:00:00Z", ...patch };
}

const publicAuthority = { directory: "https://acme.example.test/directory", terms_url: "https://acme.example.test/terms-v1.pdf", terms_text: "" };

function acmeIssuer(patch: Partial<CertificateIssuer> = {}): CertificateIssuer {
  return issuer({ id: "a1", name: "Wildcard DNS", provider: "acme_dns", validity_days: 0, domains: ["*.example.com"], certificate_id: "", phase: "requesting", directory: publicAuthority.directory, terms_url: publicAuthority.terms_url, terms_accepted_at: "2026-11-20T10:00:00Z", ...patch });
}

function challenge(patch: Partial<CertificateChallenge> = {}): CertificateChallenge {
  return { id: "c1", issuer_id: "a1", workload_id: "w1", zone_id: zoneId, hostname: "_acme-challenge.example.com", phase: "present", cleanup_requested: false, last_error: "", next_check_at: null, created_at: "2026-11-20T10:00:00Z", ...patch };
}

const appRoute: Route = { domain: "app.example.com", service: "web", port: 8080 };
const issuedRoute: Route = { ...appRoute, policy: { certificate_source: "uploaded", certificate_id: issued } };

function node(routeProtocol: number): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, route_policies: true, route_protocol: routeProtocol } };
}

type Handler = (method: string, path: string, body: unknown) => Promise<unknown> | undefined;

interface Options {
  routes?: Route[];
  protocol?: number;
  permissions?: string[];
  issuers?: () => Promise<unknown>;
  zones?: () => Promise<unknown>;
  certificates?: RouteCertificate[];
  challenges?: () => Promise<unknown>;
  authority?: () => Promise<unknown>;
  handle?: Handler;
}

function renderPage({ routes = [issuedRoute], protocol = 4, permissions = ["read", "operate", "edit"], issuers = () => Promise.resolve([issuer()]), zones = () => Promise.resolve([zone]), certificates = [certificate(issued, "Edge origin 2026-11", { source: "cloudflare_origin" })], challenges = () => Promise.resolve({ items: [], more: false }), authority = () => Promise.resolve(publicAuthority), handle }: Options = {}) {
  resource = { id: "w1", name: "web", node_id: "n1", kind: "service", compose: "services:\n  web:\n    image: nginx\n", source: null, environment_keys: [], has_source_token: false, permissions, manually_stopped: false, deployed: true, project_id: "p1", guarded_deploy: false, routes } as Workload;
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const handled = handle?.(method, path, body);
    if (handled) return handled;
    if (path === "/api/nodes") return Promise.resolve([node(protocol)] as never);
    if (path.startsWith("/api/operations")) return Promise.resolve([] as never);
    if (method === "GET" && path === "/api/workloads/w1/certificates") return Promise.resolve(certificates as never);
    if (method === "GET" && path === "/api/workloads/w1/certificate-issuers") return issuers();
    if (method === "GET" && path === "/api/workloads/w1/certificate-zones") return zones();
    if (method === "GET" && path === "/api/workloads/w1/certificate-challenges") return challenges();
    if (method === "GET" && path === "/api/workloads/w1/certificate-authority") return authority();
    if (method === "PUT" && path === "/api/workloads/w1/routes") return Promise.resolve({ state: "saved" } as never);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceDomains />
    </QueryClientProvider>,
  );
}

const calls = (method: string, path: string) => vi.mocked(request).mock.calls.filter(([m, p]) => m === method && p === path);
const issuerForm = () => screen.findByRole("form", { name: "Request a certificate" });

async function choose(name: string, option: string | RegExp, scope: HTMLElement = document.body) {
  await userEvent.click(within(scope).getByRole("combobox", { name }));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

describe("Origin CA issuers", () => {
  it("shows viewers the issuer, its latest version and saved-route usage without actions", async () => {
    renderPage({ permissions: ["read"], issuers: () => Promise.resolve([issuer({ next_check_at: "2027-09-01T00:00:00Z" })]) });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    expect(within(list).getByText("Edge origin")).toBeInTheDocument();
    expect(within(list).getByText("Issued")).toBeInTheDocument();
    expect(await within(list).findByText("example.com")).toBeInTheDocument();
    expect(within(list).getByText(/Renews automatically/)).toBeInTheDocument();
    expect(within(list).getByText("Edge origin 2026-11")).toBeInTheDocument();
    expect(within(list).getByText(/Selected by saved routes for/)).toHaveTextContent("app.example.com");
    expect(within(list).getByText(/Next check/)).toBeInTheDocument();
    expect(screen.getByText("Trusted by Cloudflare, not by browsers")).toBeInTheDocument();
    expect(within(list).queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Request a certificate" })).not.toBeInTheDocument();
    expect(calls("GET", "/api/workloads/w1/certificate-authority")).toHaveLength(0);
  });

  it("never treats an unknown or failed state as complete and explains Retry", async () => {
    renderPage({ issuers: () => Promise.resolve([issuer({ id: "i1", name: "Odd", phase: "paused", certificate_id: "" }), issuer({ id: "i2", name: "Broken", phase: "failed", certificate_id: "", last_error: "Cloudflare refused the request: the token lacks SSL and Certificates Edit" })]) });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    expect(within(list).getByText("Unknown state (paused)")).toBeInTheDocument();
    expect(within(list).getAllByText("No certificate has been issued yet.")).toHaveLength(2);
    expect(within(list).getByRole("alert")).toHaveTextContent("lacks SSL and Certificates Edit");
    expect(within(list).getByText(/never requests a second certificate blindly/)).toBeInTheDocument();
    expect(within(list).getAllByRole("button", { name: /^Retry issuer/ })).toHaveLength(1);
  });

  it("does not claim a renewed version is live while routes are being updated", async () => {
    renderPage({ issuers: () => Promise.resolve([issuer({ phase: "applying", auto_renew: false, operation_id: "op1" })]), handle: (_method, path) => (path === "/api/operations/op1" ? Promise.resolve({ id: "op1", state: "running" }) : undefined) });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    expect(within(list).getByText("Updating routes")).toBeInTheDocument();
    expect(within(list).getByText(/keep serving the previous version until the route update succeeds/)).toBeInTheDocument();
    expect(within(list).getByText(/already in progress still completes/)).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "Route update" })).toBeInTheDocument();
    expect(await within(list).findByText("Running")).toBeInTheDocument();
  });

  it("requests a certificate with the exact payload and tells the editor to assign it", async () => {
    renderPage({ issuers: () => Promise.resolve([]), handle: (method, path) => (method === "POST" && path === "/api/workloads/w1/certificate-issuers" ? Promise.resolve({ id: "i9", phase: "requesting" }) : undefined) });
    const form = await issuerForm();
    await userEvent.type(within(form).getByLabelText("Name"), "  Edge origin  ");
    await waitFor(() => expect(within(form).getByRole("combobox", { name: "Cloudflare zone" })).toBeEnabled());
    await choose("Cloudflare zone", "example.com", form);
    expect(within(form).getByText("Shared hostnames: example.com, *.example.com")).toBeInTheDocument();
    await userEvent.click(within(form).getByRole("button", { name: "Use route hostnames" }));
    expect(within(form).getByLabelText("Hostnames")).toHaveValue("app.example.com");
    await userEvent.type(within(form).getByLabelText("Hostnames"), "{enter}*.Example.com");
    await choose("Validity", "90 days", form);
    await userEvent.click(within(form).getByRole("switch", { name: "Renew automatically" }));
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(calls("POST", "/api/workloads/w1/certificate-issuers")).toHaveLength(1));
    expect(calls("POST", "/api/workloads/w1/certificate-issuers")[0]?.[2]).toEqual({ name: "Edge origin", provider: "cloudflare_origin", zone_id: zoneId, domains: ["app.example.com", "*.example.com"], validity_days: 90, auto_renew: false });
    expect(await within(form).findByText("Certificate requested")).toBeInTheDocument();
    expect(within(form).getByText(/select its version on the routes/)).toBeInTheDocument();
    expect(within(form).getByLabelText("Name")).toHaveValue("");
  });

  it("validates before sending and focuses the first invalid field", async () => {
    renderPage({ issuers: () => Promise.resolve([]) });
    const form = await issuerForm();
    await waitFor(() => expect(within(form).getByRole("combobox", { name: "Cloudflare zone" })).toBeEnabled());
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("Enter a name.")).toBeInTheDocument();
    expect(within(form).getByText("Choose a Cloudflare zone.")).toBeInTheDocument();
    await waitFor(() => expect(within(form).getByLabelText("Name")).toHaveFocus());
    await userEvent.type(within(form).getByLabelText("Name"), "Apex");
    await choose("Cloudflare zone", "example.com", form);
    await userEvent.type(within(form).getByLabelText("Hostnames"), "api.example.net");
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("api.example.net is not in the example.com zone.")).toBeInTheDocument();
    expect(calls("POST", "/api/workloads/w1/certificate-issuers")).toHaveLength(0);
  });

  it("keeps the input and shows the server's refusal as a warning on conflict", async () => {
    renderPage({ issuers: () => Promise.resolve([]), handle: (method, path) => (method === "POST" && path === "/api/workloads/w1/certificate-issuers" ? Promise.reject(new ApiError(409, "a requested certificate hostname overlaps the panel or another resource")) : undefined) });
    const form = await issuerForm();
    await userEvent.type(within(form).getByLabelText("Name"), "Overlap");
    await waitFor(() => expect(within(form).getByRole("combobox", { name: "Cloudflare zone" })).toBeEnabled());
    await choose("Cloudflare zone", "example.com", form);
    await userEvent.type(within(form).getByLabelText("Hostnames"), "app.example.com");
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("a requested certificate hostname overlaps the panel or another resource")).toBeInTheDocument();
    expect(within(form).queryByRole("alert")).not.toBeInTheDocument();
    expect(within(form).getByLabelText("Name")).toHaveValue("Overlap");
    expect(within(form).getByLabelText("Hostnames")).toHaveValue("app.example.com");
  });

  it("explains an empty zone list and the issuer limit instead of offering the form", async () => {
    renderPage({ zones: () => Promise.resolve([]), issuers: () => Promise.resolve(Array.from({ length: 8 }, (_, index) => issuer({ id: `i${index}`, name: `Issuer ${index}`, certificate_id: "" }))) });
    const form = await issuerForm();
    expect(await within(form).findByText("No Cloudflare zones are shared with this team")).toBeInTheDocument();
    expect(within(form).getByText("Issuer limit reached")).toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Request certificate" })).toBeDisabled();
  });

  it("reports failed issuer and zone reads with Retry", async () => {
    renderPage({ issuers: () => Promise.reject(new ApiError(503, "certificate issuers unavailable")), zones: () => Promise.reject(new ApiError(503, "certificate zones unavailable")) });
    expect(await screen.findByText("Could not load certificate issuers")).toBeInTheDocument();
    expect(await screen.findByText("Could not load shared Cloudflare zones")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Request certificate" })).toBeDisabled();
  });

  it("retries, toggles automatic renewal and removes with the exact requests", async () => {
    renderPage({
      issuers: () => Promise.resolve([issuer({ phase: "reconciling" })]),
      handle: (method, path) => {
        if (method === "PUT" && path === "/api/workloads/w1/certificate-issuers/i1") return Promise.resolve({ ok: true });
        if (method === "DELETE" && path === "/api/workloads/w1/certificate-issuers/i1") return Promise.resolve({ ok: true });
        return undefined;
      },
    });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    await userEvent.click(within(list).getByRole("button", { name: "Retry issuer Edge origin" }));
    await waitFor(() => expect(calls("PUT", "/api/workloads/w1/certificate-issuers/i1")).toHaveLength(1));
    expect(calls("PUT", "/api/workloads/w1/certificate-issuers/i1")[0]?.[2]).toEqual({ retry: true });

    await userEvent.click(within(list).getByRole("button", { name: "Actions for issuer Edge origin" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Turn off automatic renewal" }));
    await waitFor(() => expect(calls("PUT", "/api/workloads/w1/certificate-issuers/i1")).toHaveLength(2));
    expect(calls("PUT", "/api/workloads/w1/certificate-issuers/i1")[1]?.[2]).toEqual({ auto_renew: false });

    await userEvent.click(within(list).getByRole("button", { name: "Actions for issuer Edge origin" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Remove issuer" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove issuer Edge origin?" });
    expect(dialog).toHaveTextContent("Nothing is revoked at Cloudflare");
    expect(dialog).toHaveTextContent("routes that select them stay as they are");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove issuer" }));
    await waitFor(() => expect(calls("DELETE", "/api/workloads/w1/certificate-issuers/i1")).toHaveLength(1));
  });

  it("shows a refused removal in the dialog", async () => {
    renderPage({ handle: (method, path) => (method === "DELETE" && path === "/api/workloads/w1/certificate-issuers/i1" ? Promise.reject(new ApiError(409, "wait for this issuer's route update before removing it")) : undefined) });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    await userEvent.click(within(list).getByRole("button", { name: "Actions for issuer Edge origin" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Remove issuer" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove issuer Edge origin?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove issuer" }));
    expect(await within(dialog).findByText("wait for this issuer's route update before removing it")).toBeInTheDocument();
  });

  it("marks issued versions in the certificate list and blocks deleting an issuer's latest version", async () => {
    renderPage({ routes: [appRoute], certificates: [certificate(issued, "Edge origin 2026-11", { source: "cloudflare_origin" }), certificate(uploadedId, "Own CA", { source: undefined })] });
    const list = await screen.findByRole("list", { name: "Stored certificates" });
    await waitFor(() => expect(within(list).getByText(/Latest version from issuer Edge origin/)).toBeInTheDocument());
    expect(within(list).getByText(/Cloudflare Origin CA/)).toBeInTheDocument();
    expect(within(list).getByText("Uploaded")).toBeInTheDocument();
    expect(within(list).getByRole("button", { name: "Delete certificate Edge origin 2026-11" })).toBeDisabled();
    expect(within(list).getByText("Latest version of an issuer. Remove the issuer first")).toBeInTheDocument();
    expect(within(list).getByRole("button", { name: "Delete certificate Own CA" })).toBeEnabled();
  });
});

describe("wildcard routes and the proxy default certificate", () => {
  const wildcardId = "3".repeat(32);
  const wildcardRoute: Route = { domain: "*.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: wildcardId } };

  it("saves a wildcard route with a matching stored certificate on a protocol 4 worker", async () => {
    renderPage({ routes: [appRoute], issuers: () => Promise.resolve([]), certificates: [certificate(wildcardId, "Wildcard", { dns_names: ["*.example.com"] })] });
    const hostname = await screen.findByLabelText("Hostname");
    await userEvent.clear(hostname);
    await userEvent.type(hostname, "*.example.com");
    expect(screen.getByText(/Matches exactly one label below/)).toHaveTextContent("not example.com itself or deeper names");
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/A wildcard hostname needs a stored certificate .*Advanced routing/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    await choose("Certificate", "Stored certificate");
    await choose("Certificate version", /^Wildcard/);
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(calls("PUT", "/api/workloads/w1/routes")).toHaveLength(1));
    expect(calls("PUT", "/api/workloads/w1/routes")[0]?.[2]).toEqual({ routes: [wildcardRoute] });
  });

  it("refuses a new wildcard on an older worker and keeps saved protocol 4 routes visible there", async () => {
    renderPage({ protocol: 3, routes: [appRoute], issuers: () => Promise.resolve([]) });
    const hostname = await screen.findByLabelText("Hostname");
    await userEvent.clear(hostname);
    await userEvent.type(hostname, "*.example.com");
    expect(await screen.findByText("Wildcard hostnames and the proxy default certificate need a newer worker")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/Wildcard hostnames need a newer worker on this node/)).toBeInTheDocument();
    expect(calls("PUT", "/api/workloads/w1/routes")).toHaveLength(0);
  });

  it("keeps a saved proxy default on an older worker and lets it be switched to Let's Encrypt explicitly", async () => {
    renderPage({ protocol: 3, routes: [{ ...appRoute, policy: { certificate_source: "default" } }], issuers: () => Promise.resolve([]) });
    expect(await screen.findByText("Wildcard hostnames and the proxy default certificate need a newer worker")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.getByRole("combobox", { name: "Certificate" })).toHaveTextContent("Proxy default certificate");
    await choose("Certificate", "Let's Encrypt");
    expect(screen.getByText("Saving may briefly restart the shared proxy")).toBeInTheDocument();
    expect(screen.queryByText("Wildcard hostnames and the proxy default certificate need a newer worker")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(calls("PUT", "/api/workloads/w1/routes")).toHaveLength(1));
    expect(calls("PUT", "/api/workloads/w1/routes")[0]?.[2]).toEqual({ routes: [{ ...appRoute, policy: { certificate_source: "acme" } }] });
  });

  it("warns that choosing the proxy default can restart the shared proxy on a protocol 4 worker", async () => {
    renderPage({ routes: [appRoute], issuers: () => Promise.resolve([]) });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    await choose("Certificate", "Proxy default certificate");
    expect(screen.getByText("Saving may briefly restart the shared proxy")).toBeInTheDocument();
    expect(screen.getByText(/interrupting connections to every resource on worker-1, not just this one/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await waitFor(() => expect(calls("PUT", "/api/workloads/w1/routes")).toHaveLength(1));
    expect(calls("PUT", "/api/workloads/w1/routes")[0]?.[2]).toEqual({ routes: [{ ...appRoute, policy: { certificate_source: "default" } }] });
  });
});

describe("ACME DNS-01 issuers", () => {
  const post = (path = "/api/workloads/w1/certificate-issuers") => calls("POST", path);
  const created: Handler = (method, path) => (method === "POST" && path === "/api/workloads/w1/certificate-issuers" ? Promise.resolve({ id: "a9", phase: "requesting" }) : undefined);

  async function fillDNS(form: HTMLElement) {
    await userEvent.type(within(form).getByLabelText("Name"), "Wildcard DNS");
    await waitFor(() => expect(within(form).getByRole("combobox", { name: "Cloudflare zone" })).toBeEnabled());
    await choose("Cloudflare zone", "example.com", form);
    await userEvent.type(within(form).getByLabelText("Hostnames"), "*.example.com");
  }

  it("contacts the authority only once DNS-01 is chosen, requires unchecked-by-default consent and sends the exact payload", async () => {
    renderPage({ issuers: () => Promise.resolve([]), handle: created });
    const form = await issuerForm();
    expect(within(form).getByRole("radio", { name: /Cloudflare Origin CA/ })).toBeChecked();
    expect(screen.getByText("Trusted by Cloudflare, not by browsers")).toBeInTheDocument();
    expect(within(form).getByText(/Zone SSL and Certificates Edit permission/)).toBeInTheDocument();
    expect(calls("GET", "/api/workloads/w1/certificate-authority")).toHaveLength(0);

    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    await waitFor(() => expect(calls("GET", "/api/workloads/w1/certificate-authority")).toHaveLength(1));
    expect(await within(form).findByText(publicAuthority.directory)).toBeInTheDocument();
    expect(within(form).getByText(/Zone Read and DNS Edit permission/)).toBeInTheDocument();
    expect(within(form).queryByRole("combobox", { name: "Validity" })).not.toBeInTheDocument();
    expect(screen.queryByText("Trusted by Cloudflare, not by browsers")).not.toBeInTheDocument();
    expect(screen.getByText("Trusted only where the panel's certificate authority is trusted")).toBeInTheDocument();
    const link = within(form).getByRole("link", { name: /Read the terms of service/ });
    expect(link).toHaveAttribute("href", publicAuthority.terms_url);
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    const consent = within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" });
    expect(consent).not.toBeChecked();

    await fillDNS(form);
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("Accept the certificate authority's terms to request a certificate.")).toBeInTheDocument();
    await waitFor(() => expect(consent).toHaveFocus());
    expect(post()).toHaveLength(0);

    await userEvent.click(consent);
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(post()).toHaveLength(1));
    expect(post()[0]?.[2]).toEqual({ name: "Wildcard DNS", provider: "acme_dns", zone_id: zoneId, domains: ["*.example.com"], validity_days: 0, auto_renew: true, accept_terms: true, terms_url: publicAuthority.terms_url });
    expect(await within(form).findByText("Certificate requested")).toBeInTheDocument();
    expect(within(form).getByText(/DNS challenge records/)).toBeInTheDocument();
    expect(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" })).not.toBeChecked();
  });

  it("shows plain-text terms as escaped text, never as HTML or a data: link, and posts the exact data URI identity", async () => {
    const markup = "Pebble terms\n<script>window.hacked = true</script><a href=\"javascript:alert(1)\">click</a>";
    const termsUrl = `data:text/plain,${encodeURIComponent(markup)}`;
    renderPage({ issuers: () => Promise.resolve([]), authority: () => Promise.resolve({ directory: "https://pebble:14000/dir", terms_url: termsUrl, terms_text: markup }), handle: created });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    const region = await within(form).findByRole("region", { name: "Terms of service" });
    expect(region.textContent).toBe(markup);
    expect(region.querySelector("script, a")).toBeNull();
    expect(document.querySelector('a[href^="data:"], a[href^="javascript:"]')).toBeNull();
    expect(within(form).queryByRole("link", { name: /terms/i })).not.toBeInTheDocument();
    expect((window as { hacked?: boolean }).hacked).toBeUndefined();
    await fillDNS(form);
    await userEvent.click(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" }));
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(post()).toHaveLength(1));
    expect(post()[0]?.[2]).toMatchObject({ provider: "acme_dns", terms_url: termsUrl, accept_terms: true });
  });

  it("still asks for explicit consent when the authority publishes no terms", async () => {
    renderPage({ issuers: () => Promise.resolve([]), authority: () => Promise.resolve({ directory: "https://ca.internal/dir", terms_url: "", terms_text: "" }), handle: created });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    expect(await within(form).findByText(/publishes no terms of service/)).toBeInTheDocument();
    await fillDNS(form);
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("Accept the certificate authority's terms to request a certificate.")).toBeInTheDocument();
    await userEvent.click(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" }));
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(post()).toHaveLength(1));
    expect(post()[0]?.[2]).toMatchObject({ accept_terms: true, terms_url: "" });
  });

  it("refuses to offer consent for terms it cannot display", async () => {
    renderPage({ issuers: () => Promise.resolve([]), authority: () => Promise.resolve({ directory: "https://ca.internal/dir", terms_url: "data:text/html,<b>x</b>", terms_text: "" }) });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    expect(await within(form).findByText("The terms cannot be shown")).toBeInTheDocument();
    expect(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" })).toBeDisabled();
    expect(document.querySelector('a[href^="data:"]')).toBeNull();
  });

  it("refreshes the terms after a changed-terms refusal and requires fresh acceptance of the new identity", async () => {
    let version = 1;
    renderPage({
      issuers: () => Promise.resolve([]),
      authority: () => Promise.resolve({ ...publicAuthority, terms_url: `https://acme.example.test/terms-v${version}.pdf` }),
      handle: (method, path) => {
        if (method !== "POST" || path !== "/api/workloads/w1/certificate-issuers") return undefined;
        if (version === 1) {
          version = 2;
          return Promise.reject(new ApiError(409, "certificate authority terms changed; refresh and accept the current terms before requesting"));
        }
        return Promise.resolve({ id: "a9", phase: "requesting" });
      },
    });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    await fillDNS(form);
    await userEvent.click(await within(form).findByRole("checkbox", { name: "I accept this certificate authority's terms of service" }));
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText("The certificate authority's terms changed")).toBeInTheDocument();
    await waitFor(() => expect(calls("GET", "/api/workloads/w1/certificate-authority")).toHaveLength(2));
    await waitFor(() => expect(within(form).getByRole("link", { name: /Read the terms of service/ })).toHaveAttribute("href", "https://acme.example.test/terms-v2.pdf"));
    expect(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" })).not.toBeChecked();
    expect(within(form).getByLabelText("Name")).toHaveValue("Wildcard DNS");
    await userEvent.click(within(form).getByRole("checkbox", { name: "I accept this certificate authority's terms of service" }));
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(post()).toHaveLength(2));
    expect(post()[1]?.[2]).toMatchObject({ terms_url: "https://acme.example.test/terms-v2.pdf" });
  });

  it("drops consent when switching providers and keeps Origin requests unchanged", async () => {
    renderPage({ issuers: () => Promise.resolve([]), handle: created });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    await userEvent.click(await within(form).findByRole("checkbox", { name: "I accept this certificate authority's terms of service" }));
    await userEvent.click(within(form).getByRole("radio", { name: /Cloudflare Origin CA/ }));
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    expect(await within(form).findByRole("checkbox", { name: "I accept this certificate authority's terms of service" })).not.toBeChecked();
    await userEvent.click(within(form).getByRole("radio", { name: /Cloudflare Origin CA/ }));
    await fillDNS(form);
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    await waitFor(() => expect(post()).toHaveLength(1));
    expect(post()[0]?.[2]).toEqual({ name: "Wildcard DNS", provider: "cloudflare_origin", zone_id: zoneId, domains: ["*.example.com"], validity_days: 365, auto_renew: true });
  });

  it("reports an unavailable authority with Retry and does not send a request", async () => {
    renderPage({ issuers: () => Promise.resolve([]), authority: () => Promise.reject(new ApiError(503, "configured certificate authority is unavailable; review the panel's ACME directory and trust settings")) });
    const form = await issuerForm();
    await userEvent.click(within(form).getByRole("radio", { name: /ACME DNS-01/ }));
    expect(await within(form).findByText("Could not load the certificate authority's terms")).toBeInTheDocument();
    expect(within(form).getByText(/review the panel's ACME directory/)).toBeInTheDocument();
    await fillDNS(form);
    await userEvent.click(within(form).getByRole("button", { name: "Request certificate" }));
    expect(await within(form).findByText(/could not be reached/)).toBeInTheDocument();
    expect(post()).toHaveLength(0);
  });

  it("shows viewers an ACME issuer's authority, accepted terms and challenges without contacting the authority", async () => {
    const terms = "Plain terms <b>not bold</b>";
    renderPage({
      permissions: ["read"],
      issuers: () => Promise.resolve([acmeIssuer({ terms_url: `data:text/plain,${encodeURIComponent(terms)}`, terms_text: terms })]),
      certificates: [certificate(issued, "Wildcard DNS 2026-11", { source: "acme_dns", dns_names: ["*.example.com"] })],
      challenges: () => Promise.resolve({ items: [challenge()], more: false }),
    });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    expect(within(list).getByText((_, element) => element?.tagName === "P" && (element.textContent ?? "").startsWith("ACME DNS-01 · Zone"))).toHaveTextContent("Validity chosen by the authority");
    expect(within(list).getByText(publicAuthority.directory)).toBeInTheDocument();
    expect(within(list).getByText(/Terms accepted/)).toBeInTheDocument();
    await userEvent.click(within(list).getByText("Show accepted terms"));
    expect(within(list).getByRole("region", { name: "Terms of service" }).textContent).toBe(terms);
    expect(document.querySelector('a[href^="data:"]')).toBeNull();
    expect(screen.queryByText("Trusted by Cloudflare, not by browsers")).not.toBeInTheDocument();
    const records = await screen.findByRole("list", { name: "DNS challenge records" });
    expect(within(records).getByText("_acme-challenge.example.com")).toBeInTheDocument();
    expect(within(records).getByText("Issuer Wildcard DNS", { exact: false })).toBeInTheDocument();
    expect(within(records).queryByRole("button")).not.toBeInTheDocument();
    expect(within(screen.getByRole("list", { name: "Stored certificates" })).getByText(/ACME DNS-01/)).toBeInTheDocument();
    expect(calls("GET", "/api/workloads/w1/certificate-authority")).toHaveLength(0);
  });

  it("states each challenge phase honestly, keeps cleanup visible after issuer removal and reports truncation", async () => {
    renderPage({
      issuers: () => Promise.resolve([]),
      challenges: () =>
        Promise.resolve({
          more: true,
          items: [
            challenge({ id: "c1", phase: "presenting", hostname: "_acme-challenge.one.example.com" }),
            challenge({ id: "c2", phase: "blocked", hostname: "_acme-challenge.two.example.com", last_error: `Cloudflare refused the record: ${"permission denied ".repeat(20)}` }),
            challenge({ id: "c3", phase: "cleanup", issuer_id: null, cleanup_requested: true, hostname: "_acme-challenge.three.example.com", next_check_at: "2026-11-22T10:00:00Z" }),
            challenge({ id: "c4", phase: "frozen", hostname: "_acme-challenge.four.example.com" }),
          ],
        }),
    });
    const records = await screen.findByRole("list", { name: "DNS challenge records" });
    expect(within(records).getByText("Publishing")).toBeInTheDocument();
    expect(within(records).getByText(/may or may not exist/)).toBeInTheDocument();
    expect(within(records).getByText("Refused by the DNS provider")).toBeInTheDocument();
    expect(within(records).getByRole("alert")).toHaveTextContent("permission denied");
    expect(within(records).getByText(/then retry the issuer/)).toBeInTheDocument();
    expect(within(records).getByText("Removing")).toBeInTheDocument();
    expect(within(records).getByText(/Issuer removed/)).toBeInTheDocument();
    expect(within(records).getByText(/Cleanup requested/)).toBeInTheDocument();
    expect(within(records).getByText("Unknown state (frozen)")).toBeInTheDocument();
    expect(within(records).getByText(/does not treat the record as resolved/)).toBeInTheDocument();
    expect(screen.getByText("Showing the oldest 4 unresolved records")).toBeInTheDocument();
  });

  it("says when no challenge records are outstanding for an ACME issuer", async () => {
    renderPage({ issuers: () => Promise.resolve([acmeIssuer()]) });
    expect(await screen.findByText("No outstanding DNS challenge records.")).toBeInTheDocument();
  });

  it("reports an unreadable challenge list even without ACME issuers", async () => {
    renderPage({ issuers: () => Promise.resolve([]), challenges: () => Promise.reject(new ApiError(503, "DNS challenge status unavailable")) });
    expect(await screen.findByText("Could not load DNS challenge records")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("hides the challenge section for Origin-only resources with nothing outstanding", async () => {
    renderPage();
    await screen.findByRole("list", { name: "Certificate issuers" });
    await waitFor(() => expect(calls("GET", "/api/workloads/w1/certificate-challenges")).toHaveLength(1));
    expect(screen.queryByText("DNS challenge records")).not.toBeInTheDocument();
  });

  it("explains that removing an ACME issuer leaves DNS cleanup to finish in the background", async () => {
    renderPage({ issuers: () => Promise.resolve([acmeIssuer({ phase: "failed", last_error: "authorization failed" })]), handle: (method, path) => (method === "DELETE" && path === "/api/workloads/w1/certificate-issuers/a1" ? Promise.resolve({ ok: true }) : undefined) });
    const list = await screen.findByRole("list", { name: "Certificate issuers" });
    expect(within(list).getByText(/resumes the saved order with the certificate authority/)).toBeInTheDocument();
    await userEvent.click(within(list).getByRole("button", { name: "Actions for issuer Wildcard DNS" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Remove issuer" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove issuer Wildcard DNS?" });
    expect(dialog).toHaveTextContent("Nothing is revoked at the certificate authority");
    expect(dialog).toHaveTextContent("does not clear them at once");
    expect(dialog).not.toHaveTextContent("Nothing is revoked at Cloudflare");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove issuer" }));
    await waitFor(() => expect(calls("DELETE", "/api/workloads/w1/certificate-issuers/a1")).toHaveLength(1));
    await waitFor(() => expect(calls("GET", "/api/workloads/w1/certificate-challenges").length).toBeGreaterThanOrEqual(2));
  });
});
