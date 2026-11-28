import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ImageScanningSettings, ImageScanResults } from "../api/image-scans";
import type { ImageScan, ScannedImage, Workload } from "../api/types";
import { ImageScans, ScanOutcome, ScanSource } from "./ImageScans";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (original) => ({ ...(await original<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to, params }: { children: ReactNode; to: string; params?: Record<string, string> }) => (
    <a href={Object.entries(params ?? {}).reduce((href, [key, value]) => href.replace(`$${key}`, value), to)}>{children}</a>
  ),
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no worker, scanner, registry or notification connection is contacted.
const settingsPath = "/api/workloads/w1/image-scanning";
const resultsPath = "/api/workloads/w1/image-scans";
const operationsPath = "/api/workloads/w1/operations";
const pin = "docker.io/aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb";
const digest = (character: string) => `sha256:${character.repeat(64)}`;
const service = { id: "w1", name: "web", node_id: "n1", kind: "service", permissions: ["read", "operate", "edit"], source: null, compose: "" } as unknown as Workload;
const operator = { ...service, permissions: ["read", "operate"] } as unknown as Workload;
const viewer = { ...service, permissions: ["read"] } as unknown as Workload;

function settings(extra: Partial<ImageScanningSettings> = {}): ImageScanningSettings {
  return {
    enabled: false,
    notify: false,
    can_manage: true,
    node_id: "n1",
    worker_supported: true,
    node_ready: true,
    active_operation_id: null,
    scanner_image: pin,
    java_archives: "not_scanned",
    database_limit_bytes: 2 * 1024 ** 3,
    database_max_age_hours: 24,
    automatic: false,
    automatic_supported: true,
    automatic_running: false,
    next_automatic_check_at: null,
    automatic_message: "",
    ...extra,
  };
}

const hoursFromNow = (hours: number) => new Date(Date.now() + hours * 3_600_000).toISOString();

function image(extra: Partial<ScannedImage> = {}): ScannedImage {
  return {
    service: "app",
    image_id: digest("a"),
    config_digest: digest("a"),
    state: "complete",
    counts: { HIGH: 1, MEDIUM: 2 },
    findings: [
      { id: "CVE-2026-0001", package: "openssl", installed: "3.0.1-r0", fixed: "3.0.2-r0", severity: "HIGH" },
      { id: "CVE-2026-0002", package: "golang.org/x/net", installed: "v0.1.0", severity: "MEDIUM" },
      { id: "GHSA-aaaa-bbbb-cccc", package: "zlib", installed: "1.2.13-r0", fixed: "1.3-r0", severity: "MEDIUM" },
    ],
    truncated: false,
    end_of_life: false,
    coverage: { os: "scanned", other_languages: "scanned", java_archives: "not_detected" },
    ...extra,
  };
}

function report(extra: Partial<ImageScan> = {}): ImageScan {
  return {
    scanner_image: pin,
    scanner_version: "0.76.2",
    database: { digest: digest("d"), updated_at: hoursFromNow(-2), next_update: hoursFromNow(10) },
    checked_at: hoursFromNow(-1),
    state: "complete",
    images: [image()],
    ...extra,
  };
}

const none: ImageScanResults = { state: "unknown", origin: "", stale: true, operation_id: null, operation_state: null, result: null, message: "No vulnerability scan has completed." };

function results(extra: Partial<ImageScanResults> = {}): ImageScanResults {
  return { state: "complete", origin: "manual", operation_state: "succeeded", stale: false, operation_id: "op-scan", result: report(), ...extra };
}

interface Handlers {
  settings?: (method: string, body?: unknown) => unknown;
  results?: () => unknown;
  queue?: (body: unknown) => unknown;
}

function serve(handlers: Handlers) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    try {
      if (path === "/api/nodes") return Promise.resolve([{ id: "n1", name: "edge-1" }]);
      if (path === settingsPath) return Promise.resolve(handlers.settings ? handlers.settings(method, body) : settings());
      if (path === resultsPath) return Promise.resolve(handlers.results ? handlers.results() : none);
      if (path === operationsPath && method === "POST" && handlers.queue) return Promise.resolve(handlers.queue(body));
      if (path.startsWith("/api/operations")) return Promise.resolve([]);
      return Promise.reject(new Error(`unexpected request ${method} ${path}`));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function renderScans(workload: Workload = service) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <ImageScans workload={workload} />
    </QueryClientProvider>,
  );
  return { client, ...view };
}

const calls = (method: string, path?: string) => vi.mocked(request).mock.calls.filter(([callMethod, callPath]) => callMethod === method && (!path || callPath === path));
const scanButton = () => screen.findByRole("button", { name: "Scan for vulnerabilities" });
const enableSwitch = () => screen.findByRole("switch", { name: "Allow vulnerability scans" });

it("reads settings and results with GET and queues exactly one image_scan operation", async () => {
  let current = settings({ enabled: true });
  serve({
    settings: () => current,
    queue: (body) => {
      current = settings({ enabled: true, active_operation_id: "op-new" });
      expect(body).toEqual({ action: "image_scan" });
      return { id: "op-new" };
    },
  });
  renderScans();
  const button = await scanButton();
  await waitFor(() => expect(button).toBeEnabled());
  expect(request).toHaveBeenCalledWith("GET", settingsPath, undefined, expect.any(AbortSignal));
  expect(request).toHaveBeenCalledWith("GET", resultsPath, undefined, expect.any(AbortSignal));
  fireEvent.click(button);
  await waitFor(() => expect(calls("POST", operationsPath)).toHaveLength(1));
  expect(calls("POST", operationsPath)[0]).toEqual(["POST", operationsPath, { action: "image_scan" }]);
  expect(await screen.findByText("Vulnerability scan queued")).toBeInTheDocument();
  expect(screen.getByText("Results appear here when the operation finishes.")).toBeInTheDocument();
  expect(screen.getAllByRole("link", { name: "View operation" }).map((link) => link.getAttribute("href"))).toContain("/operations/op-new");
  await waitFor(() => expect(screen.getByRole("button", { name: "Scan for vulnerabilities" })).toBeDisabled());
});

it("shows a refused scan request with the server message", async () => {
  serve({
    settings: () => settings({ enabled: true }),
    queue: () => {
      throw new ApiError(409, "image scanning requires node-operator and team/node opt-in on a ready worker");
    },
  });
  renderScans();
  const button = await scanButton();
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.click(button);
  expect(await screen.findByText("Could not queue the scan")).toBeInTheDocument();
  expect(screen.getByText("image scanning requires node-operator and team/node opt-in on a ready worker")).toBeInTheDocument();
  expect(screen.queryByText("Vulnerability scan queued")).not.toBeInTheDocument();
});

it("asks before allowing scans, naming the team and node scope and the cost, and sends both fields", async () => {
  let current = settings();
  serve({
    settings: (method, body) => {
      if (method === "PUT") current = settings(body as Partial<ImageScanningSettings>);
      return current;
    },
  });
  renderScans();
  expect(await screen.findByText("One setting for every resource of this team on edge-1, not only web.")).toBeInTheDocument();
  fireEvent.click(await enableSwitch());
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  let dialog = screen.getByRole("dialog", { name: "Allow vulnerability scans on edge-1?" });
  expect(dialog).toHaveTextContent("This applies to every resource of this team on edge-1, not only web.");
  expect(dialog).toHaveTextContent("exports them to a read-only archive");
  expect(dialog).toHaveTextContent("(up to 2 GiB per refresh)");
  expect(dialog).toHaveTextContent("No credentials are used.");
  expect(dialog).toHaveTextContent("results do not block deploys");
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(calls("PUT")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  dialog = screen.getByRole("dialog", { name: "Allow vulnerability scans on edge-1?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Allow scans" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", settingsPath, { enabled: true, notify: false, automatic: false }]);
  expect(await screen.findByText("Scanning settings saved")).toBeInTheDocument();
});

// A real control plane can answer the refetch after a save later than the saved data re-keys the form.
it("keeps the saved notice while the refetch after a save is held open, and disables without asking", async () => {
  let current = settings({ enabled: true });
  let release: () => void = () => undefined;
  let saved = false;
  serve({
    settings: (method, body) => {
      if (method === "PUT") {
        current = settings(body as Partial<ImageScanningSettings>);
        saved = true;
        return current;
      }
      if (!saved) return current;
      const reply = current;
      return new Promise((resolve) => {
        release = () => resolve(reply);
      });
    },
  });
  renderScans();
  const toggle = await enableSwitch();
  expect(toggle).toBeChecked();
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole("switch", { name: "Notify about new findings" }));
  expect(screen.getByText(/Nothing is sent while scans are off\./)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]?.[2]).toEqual({ enabled: false, notify: true, automatic: false });
  expect(await screen.findByText("Scanning settings saved")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Allow vulnerability scans" })).not.toBeChecked();
  await act(() => {
    release();
    return Promise.resolve();
  });
  expect(screen.getByText("Scanning settings saved")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Notify about new findings" })).toBeChecked();

  fireEvent.click(screen.getByRole("switch", { name: "Notify about new findings" }));
  expect(screen.queryByText("Scanning settings saved")).not.toBeInTheDocument();
});

it("keeps edits when a save is refused", async () => {
  serve({
    settings: (method) => {
      if (method === "PUT") throw new ApiError(409, "the node operator must enable scanning and verify a ready worker first");
      return settings();
    },
  });
  renderScans();
  fireEvent.click(await screen.findByRole("switch", { name: "Notify about new findings" }));
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  expect(await screen.findByText("the node operator must enable scanning and verify a ready worker first")).toBeInTheDocument();
  expect(screen.getByText("Your changes are kept.")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Notify about new findings" })).toBeChecked();
  expect(screen.getByRole("button", { name: "Save scanning settings" })).toBeEnabled();
});

it("lets an operator queue scans but not change the team setting", async () => {
  serve({ settings: () => settings({ enabled: true, notify: true, can_manage: false }) });
  renderScans(operator);
  await waitFor(async () => expect(await scanButton()).toBeEnabled());
  expect(screen.getByText("Only team administrators can change this setting.")).toBeInTheDocument();
  expect(screen.getByText("Allowed")).toBeInTheDocument();
  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save scanning settings" })).not.toBeInTheDocument();
});

it("shows results read-only to viewers, without a scan control", async () => {
  serve({ settings: () => settings({ enabled: true, can_manage: false }), results: () => results() });
  renderScans(viewer);
  expect(await screen.findByText("Queuing a scan needs operator access. Results are read-only.")).toBeInTheDocument();
  expect(await screen.findByRole("list", { name: "Scanned images" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Scan for vulnerabilities" })).not.toBeInTheDocument();
  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
});

it("explains the node operator switch for an old worker and never offers to enable it", async () => {
  serve({ settings: () => settings({ worker_supported: false }) });
  renderScans();
  expect(await screen.findByText("Node operator setup needed")).toBeInTheDocument();
  expect(screen.getAllByText(/COVE_IMAGE_SCANNING=1/).length).toBeGreaterThan(0);
  expect(screen.getByText(/The team setting below does not change the node\./)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "node page" })).toHaveAttribute("href", "/nodes/n1");
  expect(await enableSwitch()).toBeDisabled();
  expect(await scanButton()).toBeDisabled();
  expect(screen.getByText("The worker on edge-1 does not report vulnerability scanning.")).toBeInTheDocument();
});

it("lets an administrator turn scans off on an old worker, even during another operation", async () => {
  let current = settings({ enabled: true, worker_supported: undefined as unknown as boolean, active_operation_id: "op-run" });
  serve({
    settings: (method, body) => {
      if (method === "PUT") current = settings({ ...(body as Partial<ImageScanningSettings>), worker_supported: false });
      return current;
    },
  });
  renderScans();
  const toggle = await enableSwitch();
  expect(screen.getByText(/does not report vulnerability scanning\..*You can still turn scans off\./)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(toggle);
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  await waitFor(() => expect(calls("PUT")[0]).toEqual(["PUT", settingsPath, { enabled: false, notify: false, automatic: false }]));
  expect(await screen.findByText("Scanning settings saved")).toBeInTheDocument();
});

it("blocks scanning on an unready node", async () => {
  serve({ settings: () => settings({ enabled: true, node_ready: false }) });
  renderScans();
  expect(await screen.findByText("Scanning is available once edge-1 is ready.")).toBeInTheDocument();
  expect(await scanButton()).toBeDisabled();
});

it("blocks scanning while the team setting is off", async () => {
  serve({});
  renderScans();
  expect(await screen.findByText("Vulnerability scans are off for this team on edge-1. A team administrator can allow them in the setting below.")).toBeInTheDocument();
  expect(await scanButton()).toBeDisabled();
});

it("keeps scanning blocked while the settings are loading", async () => {
  vi.mocked(request).mockImplementation((_method: string, path: string) => (path === resultsPath ? Promise.resolve(none) : new Promise(() => undefined)));
  renderScans();
  expect(await screen.findByText("Checking whether scanning is allowed.")).toBeInTheDocument();
  expect(await scanButton()).toBeDisabled();
});

it("does not trust cached settings after a failed refetch, and recovers with Retry", async () => {
  let fail = false;
  serve({
    settings: () => {
      if (fail) throw new ApiError(503, "image scanning settings unavailable");
      return settings({ enabled: true });
    },
  });
  const { client } = renderScans();
  await waitFor(async () => expect(await scanButton()).toBeEnabled());
  fail = true;
  await act(() => client.refetchQueries({ queryKey: ["workloads", "w1", "image-scanning"] }));
  expect(await screen.findByText("Could not refresh the scanning settings")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Scan for vulnerabilities" })).toBeDisabled();
  expect(screen.getByText("Cove cannot read the scanning settings. Scanning is paused until they load.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Notify about new findings" }));
  expect(screen.getByRole("button", { name: "Save scanning settings" })).toBeDisabled();

  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByText("Could not refresh the scanning settings")).not.toBeInTheDocument());
  expect(screen.getByRole("button", { name: "Scan for vulnerabilities" })).toBeEnabled();
});

it("shows failed first loads with Retry and keeps scanning blocked", async () => {
  serve({
    settings: () => {
      throw new ApiError(503, "image scanning settings unavailable");
    },
    results: () => {
      throw new ApiError(503, "vulnerability results unavailable");
    },
  });
  renderScans();
  expect(await screen.findByText("Could not load the scanning settings")).toBeInTheDocument();
  expect(await screen.findByText("Could not load vulnerability results")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Scan for vulnerabilities" })).toBeDisabled();
});

it("marks results unverified after a failed refetch and blocks scanning", async () => {
  let fail = false;
  serve({
    settings: () => settings({ enabled: true }),
    results: () => {
      if (fail) throw new ApiError(503, "vulnerability results unavailable");
      return results();
    },
  });
  const { client } = renderScans();
  expect((await screen.findAllByText("Scan completed")).length).toBeGreaterThan(0);
  fail = true;
  await act(() => client.refetchQueries({ queryKey: ["workloads", "w1", "image-scans"] }));
  expect(await screen.findByText("Could not refresh vulnerability results")).toBeInTheDocument();
  expect(screen.getByText("Scan completed (unverified)")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Scanned images (unverified)" })).toBeInTheDocument();
  expect(screen.queryByText(/^Scan completed\. /)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Scan for vulnerabilities" })).toBeDisabled();
});

it("pauses scanning and saving while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve({ settings: () => settings({ enabled: true }) });
  renderScans();
  expect(await screen.findByText("You are offline. Scanning is available when the connection returns.")).toBeInTheDocument();
  expect(await scanButton()).toBeDisabled();
  fireEvent.click(screen.getByRole("switch", { name: "Notify about new findings" }));
  expect(screen.getByRole("button", { name: "Save scanning settings" })).toBeDisabled();
});

it("links an active operation, blocks a second scan and does not suggest starting the resource", async () => {
  serve({ settings: () => settings({ enabled: true, active_operation_id: "op-run" }) });
  renderScans();
  expect(await screen.findByText(/a running scan can be cancelled from its operation page/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View operation" })).toHaveAttribute("href", "/operations/op-run");
  expect(await scanButton()).toBeDisabled();
  expect(screen.queryByText(/(start|wake) (it|the resource|this resource|the service|the app)/i)).not.toBeInTheDocument();
});

it("never calls a missing result clean", async () => {
  serve({ settings: () => settings({ enabled: true }) });
  renderScans();
  expect(await screen.findByText("No completed scan")).toBeInTheDocument();
  expect(screen.getByText("No vulnerability scan has completed.")).toBeInTheDocument();
  expect(screen.queryByText(/clean|no known vulnerabilities|Scan completed/i)).not.toBeInTheDocument();
});

it("shows a completed scan with grouped counts, scanner pin, database and wrapped findings", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results() });
  renderScans();
  expect(await screen.findByText("Scan completed. 3 known vulnerabilities in scanned packages.")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View scan" })).toHaveAttribute("href", "/operations/op-scan");
  expect(screen.getByText("Version 0.76.2")).toBeInTheDocument();
  expect(screen.getAllByText(pin).length).toBeGreaterThan(0);
  expect(screen.getByText(digest("d"))).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Findings by severity" })).toHaveTextContent("High1Medium2");
  const table = screen.getByRole("table", { name: "Findings for app" });
  expect(within(table).getAllByText("CVE-2026-0001")[0]).toHaveClass("break-all");
  expect(within(table).getAllByText("No fix reported").length).toBeGreaterThan(0);
  expect(within(table).queryByRole("link")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Coverage for app")).toHaveTextContent("Java archives (JAR, WAR, EAR)None detected");
});

it("says no known vulnerabilities only for a complete scan with zero findings", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: [image({ counts: {}, findings: [] })] }) }) });
  renderScans();
  expect(await screen.findByText("Scan completed. No known vulnerabilities in scanned packages.")).toBeInTheDocument();
  expect(screen.getByText("No known vulnerabilities in scanned packages.")).toBeInTheDocument();
  expect(screen.queryByText(/clean/i)).not.toBeInTheDocument();
});

it("shows truncation and expands stored findings", async () => {
  const findings = Array.from({ length: 12 }, (_, index) => ({ id: `CVE-2026-${String(1000 + index)}`, package: "busybox", installed: "1.36.1-r0", severity: "LOW" as const }));
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: [image({ counts: { LOW: 300 }, findings, truncated: true })] }) }) });
  renderScans();
  expect(await screen.findByText("12 of 300 findings are stored, most severe first. The counts include every finding.")).toBeInTheDocument();
  const table = screen.getByRole("table", { name: "Findings for app" });
  expect(within(table).getAllByRole("row")).toHaveLength(11);
  fireEvent.click(screen.getByRole("button", { name: "Show all 12 stored findings" }));
  expect(within(table).getAllByRole("row")).toHaveLength(13);
  expect(screen.getByRole("button", { name: "Show the first 10 findings" })).toHaveAttribute("aria-expanded", "true");
});

it("shows the Java archive gap beside retained OS findings and keeps the result unknown", async () => {
  const java = image({
    state: "unknown",
    message: "OS and other-language findings are available, but Java archive dependency coverage is unknown.",
    coverage: { os: "scanned", other_languages: "not_detected", java_archives: "not_scanned", java_message: "JAR, WAR or EAR artifacts were detected. Java archive dependencies are not scanned because the Java index is outside this slice's database budget." },
  });
  serve({ settings: () => settings({ enabled: true }), results: () => results({ state: "unknown", stale: false, result: report({ state: "unknown", images: [java] }) }) });
  renderScans();
  expect((await screen.findAllByText("Coverage unknown", { selector: "span" })).length).toBeGreaterThan(0);
  const coverage = screen.getByLabelText("Coverage for app");
  expect(coverage).toHaveTextContent("OS packagesScanned");
  expect(coverage).toHaveTextContent("Java archives (JAR, WAR, EAR)Not scanned (unknown)");
  expect(screen.getByText(/Java archive dependencies are not scanned because the Java index/)).toBeInTheDocument();
  expect(screen.getByText(/Java archive \(JAR, WAR or EAR\) dependencies were not scanned in 1 image\. Other findings are kept below\./)).toBeInTheDocument();
  expect(within(screen.getByRole("table", { name: "Findings for app" })).getAllByText("CVE-2026-0001").length).toBeGreaterThan(0);
  expect(screen.getByText("An unknown result is not a clean result.")).toBeInTheDocument();
  expect(screen.queryByText(/^Scan completed/)).not.toBeInTheDocument();
  expect(screen.queryByText(/no known vulnerabilities/i)).not.toBeInTheDocument();
});

it("does not treat a complete root state as complete when an image still has a Java gap", async () => {
  const java = image({ counts: {}, findings: [], coverage: { os: "scanned", other_languages: "not_detected", java_archives: "not_scanned" } });
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: [java] }) }) });
  renderScans();
  expect(await screen.findByText("No findings reported, but coverage is unknown.")).toBeInTheDocument();
  expect(screen.queryByText(/no known vulnerabilities/i)).not.toBeInTheDocument();
  expect(screen.getByText("Java archive dependency coverage is unknown for this image.")).toBeInTheDocument();
});

it("warns about an end-of-life operating system", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: [image({ end_of_life: true })] }) }) });
  renderScans();
  expect(await screen.findByText("End-of-life operating system")).toBeInTheDocument();
  expect(screen.getByText(/findings may be incomplete/)).toBeInTheDocument();
});

it("lists platform images that share an image ID separately by their identity", async () => {
  const errors = vi.spyOn(console, "error");
  const platforms = [image({ image_identity: "linux/amd64" }), image({ image_identity: "linux/arm64" })];
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: platforms }) }) });
  renderScans();
  const list = await screen.findByRole("list", { name: "Scanned images" });
  expect(within(list).getAllByRole("heading", { name: "app" })).toHaveLength(2);
  expect(errors.mock.calls.flat().join(" ")).not.toContain("same key");
});

it.each([
  ["failed", "Scan failed", "The last scan failed."],
  ["cancelled", "Scan cancelled", "The last scan was cancelled."],
  ["interrupted", "Scan interrupted", "The last scan was interrupted."],
])("shows a %s scan's partial report as incomplete", async (state, label, reason) => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ state: "unknown", operation_state: state, result: report({ state: "unknown" }) }) });
  renderScans();
  expect(await screen.findByText(label)).toBeInTheDocument();
  expect(screen.getByText("Scan did not finish")).toBeInTheDocument();
  expect(screen.getByText(new RegExp(`${reason.replace(/\./g, "\\.")} Only part of its report is available`))).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Scanned images" })).toBeInTheDocument();
  expect(screen.queryByText(/no known vulnerabilities/i)).not.toBeInTheDocument();
});

it("shows a failed scan without a report", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ state: "unknown", operation_state: "failed", result: null }) });
  renderScans();
  expect(await screen.findByText("Scan failed")).toBeInTheDocument();
  expect(screen.getByText("The last scan failed. It recorded no report.")).toBeInTheDocument();
  expect(screen.queryByRole("list", { name: "Scanned images" })).not.toBeInTheDocument();
});

it.each([
  ["stale", { digest: digest("d"), updated_at: hoursFromNow(-30), next_update: hoursFromNow(-6) }, /more than 24 hours old/],
  ["future", { digest: digest("d"), updated_at: hoursFromNow(3), next_update: hoursFromNow(20) }, /dated in the future/],
  ["unrecognised", { digest: "", updated_at: "", next_update: "" }, /did not record a recognised vulnerability database/],
])("treats a %s database as unknown coverage", async (_name, database, reason) => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ state: "unknown", stale: true, result: report({ database }) }) });
  renderScans();
  expect(await screen.findByText(reason)).toBeInTheDocument();
  expect(screen.getAllByText("Coverage unknown", { selector: "span" }).length).toBeGreaterThan(0);
  expect(screen.queryByText(/^Scan completed\./)).not.toBeInTheDocument();
});

it.each([
  ["an unrecognised root state", { state: "clean" }],
  ["a missing stale flag", { stale: undefined as unknown as boolean }],
  ["a stale result after a lifecycle change", { stale: true }],
])("keeps %s unknown", async (_name, extra) => {
  serve({ settings: () => settings({ enabled: true }), results: () => results(extra) });
  renderScans();
  expect((await screen.findAllByText("Coverage unknown", { selector: "span" })).length).toBeGreaterThan(0);
  expect(screen.queryByText(/^Scan completed\./)).not.toBeInTheDocument();
  expect(screen.queryByText("Clean")).not.toBeInTheDocument();
});

it("shows unrecognised coverage values as unknown, naming the value", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results({ result: report({ images: [image({ coverage: { os: "partial", other_languages: "", java_archives: "indexed" } })] }) }) });
  renderScans();
  const coverage = await screen.findByLabelText("Coverage for app");
  expect(coverage).toHaveTextContent("OS packagesUnknown (partial)");
  expect(coverage).toHaveTextContent("Other language packagesUnknown");
  expect(coverage).toHaveTextContent("Java archives (JAR, WAR, EAR)Unknown (indexed)");
  expect(screen.queryByText(/^Scan completed\./)).not.toBeInTheDocument();
});

it("has no credential, configuration or free-text inputs", async () => {
  serve({ settings: () => settings({ enabled: true }), results: () => results() });
  const { container } = renderScans();
  await screen.findAllByText("Scan completed");
  expect(container.querySelectorAll("textarea, select, input:not([role=switch])")).toHaveLength(0);
  expect(screen.getAllByRole("switch").map((item) => item.getAttribute("aria-label") ?? item.id)).toHaveLength(3);
});

it("never loads scans for managed previews", () => {
  serve({});
  renderScans({ ...service, preview_source_id: "w0" });
  expect(screen.getByText("Managed previews are not scanned.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("follows an active scan every 3 seconds, refreshes results when it ends, then slows down", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let current = settings({ enabled: true, active_operation_id: "op-scan" });
    let latest: ImageScanResults = none;
    serve({ settings: () => current, results: () => latest });
    renderScans();
    expect(await screen.findByText(/a running scan can be cancelled/)).toBeInTheDocument();
    current = settings({ enabled: true });
    latest = results();
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect((await screen.findAllByText("Scan completed")).length).toBeGreaterThan(0);
    const reads = calls("GET", settingsPath).length;
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect(calls("GET", settingsPath)).toHaveLength(reads);
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(calls("GET", settingsPath).length).toBeGreaterThan(reads);
  } finally {
    vi.useRealTimers();
  }
});

it("stops polling after a failed read until Retry", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let fail = false;
    serve({
      settings: () => {
        if (fail) throw new ApiError(503, "image scanning settings unavailable");
        return settings({ enabled: true, active_operation_id: "op-scan" });
      },
    });
    renderScans();
    expect(await screen.findByText(/a running scan can be cancelled/)).toBeInTheDocument();
    fail = true;
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect(await screen.findByText("Could not refresh the scanning settings")).toBeInTheDocument();
    const reads = calls("GET", settingsPath).length;
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(calls("GET", settingsPath)).toHaveLength(reads);
  } finally {
    vi.useRealTimers();
  }
});

it("summarises a scan on the operation page without claiming current coverage", () => {
  const java = image({ state: "unknown", coverage: { os: "scanned", other_languages: "not_detected", java_archives: "not_scanned" } });
  render(<ScanOutcome scan={{ ...report({ images: [java] }), images: [{ ...java, counts: null as unknown as Record<string, number>, findings: null as unknown as ScannedImage["findings"] }] }} workloadId="w1" />);
  expect(screen.getByText("Coverage unknown")).toBeInTheDocument();
  expect(screen.getByText(/1 image scanned\. No findings reported, but coverage is unknown\./)).toBeInTheDocument();
  expect(screen.getByText(/Java archive \(JAR, WAR or EAR\) dependencies were not scanned in 1 image/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Registry page" })).toHaveAttribute("href", "/resources/w1/registry");
});

it("shows the operation page's scan source without inferring an older worker's missing origin", () => {
  const { rerender } = render(<ScanSource origin="scheduled" />);
  expect(screen.getByText("Automatic, after a vulnerability database update")).toBeInTheDocument();
  rerender(<ScanSource origin={undefined} />);
  expect(screen.getByText("Not recorded")).toHaveClass("text-fg-muted");
});

const automaticSwitch = () => screen.findByRole("switch", { name: "Automatic scans" });

it("keeps automatic scans off by default and saves exactly enabled, notify and automatic after confirming", async () => {
  let current = settings({ enabled: true });
  serve({
    settings: (method, body) => {
      if (method === "PUT") current = settings(body as Partial<ImageScanningSettings>);
      return current;
    },
  });
  renderScans();
  const toggle = await automaticSwitch();
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeEnabled();
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  const dialog = screen.getByRole("dialog", { name: "Turn on automatic scans on edge-1?" });
  expect(dialog).toHaveTextContent("This applies to every resource of this team on edge-1, not only web.");
  expect(dialog).toHaveTextContent("They never deploy, restart or wake apps");
  fireEvent.click(within(dialog).getByRole("button", { name: "Turn on automatic scans" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", settingsPath, { enabled: true, notify: false, automatic: true }]);
  expect(await screen.findByText("Scanning settings saved")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Automatic scans" })).toBeChecked();
});

it("turns automatic scans off with the main switch and saves without asking", async () => {
  serve({ settings: (method, body) => (method === "PUT" ? settings(body as Partial<ImageScanningSettings>) : settings({ enabled: true, automatic: true })) });
  renderScans();
  expect(await automaticSwitch()).toBeChecked();
  fireEvent.click(await enableSwitch());
  expect(screen.getByRole("switch", { name: "Automatic scans" })).not.toBeChecked();
  expect(screen.getByRole("switch", { name: "Automatic scans" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await waitFor(() => expect(calls("PUT")[0]).toEqual(["PUT", settingsPath, { enabled: false, notify: false, automatic: false }]));
});

it("describes how saving each switch off cancels automatic scans and where to cancel a manual scan", async () => {
  serve({ settings: () => settings({ enabled: true, automatic: true, next_automatic_check_at: hoursFromNow(2) }) });
  renderScans();
  await automaticSwitch();
  expect(screen.getByText(/Saving this off stops new scans and requests cancellation of waiting or running automatic scans\. Cancel a running manual scan from its operation page\./)).toBeInTheDocument();
  expect(screen.getByText(/Saving this off requests cancellation of waiting or running automatic scans\. The Allow vulnerability scans setting still controls manual scans\./)).toBeInTheDocument();
  expect(screen.getByText(/it may wait for a deploy, job run or vulnerability database refresh\./)).toBeInTheDocument();
  expect(screen.queryByText(/must be cancelled from its operation\./)).not.toBeInTheDocument();
  expect(screen.queryByText(/for the resource to be running/)).not.toBeInTheDocument();
});

it.each([
  ["a worker without the automatic capability", { automatic_supported: false }, true],
  ["an older control plane that omits the automatic fields", { automatic_supported: undefined as unknown as boolean, automatic: undefined as unknown as boolean }, true],
  ["an unready node", { node_ready: false }, false],
])("cannot turn automatic scans on with %s, and keeps manual results", async (_name, extra, manual) => {
  serve({ settings: () => settings({ enabled: true, ...extra }), results: () => results() });
  renderScans();
  const toggle = await automaticSwitch();
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeDisabled();
  expect((await screen.findAllByText("Scan completed")).length).toBeGreaterThan(0);
  if (manual) {
    expect(screen.getByText(/does not report automatic scans.*Manual scans are not affected\./)).toBeInTheDocument();
    await waitFor(async () => expect(await scanButton()).toBeEnabled());
  } else {
    expect(screen.getByText("Automatic scans can be turned on once edge-1 is ready.")).toBeInTheDocument();
  }
});

it("lets an administrator turn automatic scans off after the worker stops reporting them", async () => {
  serve({ settings: (method, body) => (method === "PUT" ? settings({ ...(body as Partial<ImageScanningSettings>), automatic_supported: false }) : settings({ enabled: true, automatic: true, automatic_supported: false })) });
  renderScans();
  const toggle = await automaticSwitch();
  expect(screen.getByText(/does not report automatic scans\..*You can still turn automatic scans off\./)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Notify about new findings" }));
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  await waitFor(() => expect(calls("PUT")[0]).toEqual(["PUT", settingsPath, { enabled: true, notify: true, automatic: false }]));
  expect(await screen.findByText("Scanning settings saved")).toBeInTheDocument();
});

it("keeps the automatic draft and shows the server message when turning it on is refused", async () => {
  serve({
    settings: (method) => {
      if (method === "PUT") throw new ApiError(409, "enable scanning on an upgraded ready worker before enabling automatic scans");
      return settings({ enabled: true });
    },
  });
  renderScans();
  fireEvent.click(await automaticSwitch());
  fireEvent.click(screen.getByRole("button", { name: "Save scanning settings" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Turn on automatic scans" }));
  expect(await screen.findByText("enable scanning on an upgraded ready worker before enabling automatic scans")).toBeInTheDocument();
  expect(screen.getByText("Your changes are kept.")).toBeInTheDocument();
  expect(screen.queryByText("Scanning settings saved")).not.toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Automatic scans" })).toBeChecked();
});

it("pauses saving an automatic scan change while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve({ settings: () => settings({ enabled: true }) });
  renderScans();
  fireEvent.click(await automaticSwitch());
  expect(screen.getByRole("button", { name: "Save scanning settings" })).toBeDisabled();
});

it("shows automatic scans read-only to operators, who can still queue a manual scan", async () => {
  serve({ settings: () => settings({ enabled: true, automatic: true, can_manage: false }) });
  renderScans(operator);
  await waitFor(async () => expect(await scanButton()).toBeEnabled());
  const [worker, setting] = screen.getAllByText("Automatic scans", { selector: "dt" });
  expect(worker?.nextElementSibling).toHaveTextContent("Reported");
  expect(setting?.nextElementSibling).toHaveTextContent("On");
  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
});

it("follows a background scan every 3 seconds without blocking manual scans or linking an operation", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let current = settings({ enabled: true, automatic: true, automatic_running: true });
    let latest: ImageScanResults = none;
    serve({ settings: () => current, results: () => latest, queue: () => ({ id: "op-manual" }) });
    renderScans();
    expect(await screen.findByText(/An automatic scan is running in the background\./)).toBeInTheDocument();
    await waitFor(async () => expect(await scanButton()).toBeEnabled());
    expect(screen.queryByRole("link", { name: "View operation" })).not.toBeInTheDocument();
    const reads = calls("GET", resultsPath).length;
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect(calls("GET", resultsPath).length).toBeGreaterThan(reads);

    current = settings({ enabled: true, automatic: true, next_automatic_check_at: hoursFromNow(1) });
    latest = results({ origin: "post_deploy" });
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect(await screen.findByText("Automatic, after a deploy or job run")).toBeInTheDocument();
    expect(screen.queryByText(/running in the background/)).not.toBeInTheDocument();
    expect(screen.getByText(/Next automatic check/)).toBeInTheDocument();
    const idle = calls("GET", settingsPath).length;
    await act(() => vi.advanceTimersByTimeAsync(3_100));
    expect(calls("GET", settingsPath)).toHaveLength(idle);

    fireEvent.click(screen.getByRole("button", { name: "Scan for vulnerabilities" }));
    await waitFor(() => expect(calls("POST", operationsPath)[0]).toEqual(["POST", operationsPath, { action: "image_scan" }]));
  } finally {
    vi.useRealTimers();
  }
});

it("shows a held automatic check as unknown, never as a completed scan", async () => {
  serve({
    settings: () => settings({ enabled: true, automatic: true, automatic_message: "Vulnerability database coverage is unknown. A bounded refresh may be pending or cooling down; no scan was queued." }),
  });
  renderScans();
  expect(await screen.findByText(/A bounded refresh may be pending or cooling down; no scan was queued\./)).toBeInTheDocument();
  expect(screen.getByText("No completed scan")).toBeInTheDocument();
  expect(screen.queryByText(/clean|Scan completed/i)).not.toBeInTheDocument();
});

it.each([
  ["manual", "Manual"],
  ["post_deploy", "Automatic, after a deploy or job run"],
  ["scheduled", "Automatic, after a vulnerability database update"],
  ["", "Not recorded"],
  [undefined, "Not recorded"],
  ["webhook", "Not recorded"],
])("labels the latest scan's origin %s", async (origin, label) => {
  serve({ settings: () => settings({ enabled: true }), results: () => ({ ...results(), origin }) });
  renderScans();
  const term = await screen.findByText("Source", { selector: "dt" });
  expect(term.nextElementSibling).toHaveTextContent(label);
});

it("explains which manual and automatic scan results notify", async () => {
  serve({ settings: () => settings({ enabled: true }) });
  renderScans();
  await screen.findByRole("switch", { name: "Notify about new findings" });
  expect(screen.getByText(/Manual scans notify every time they finish, whatever the result\./)).toBeInTheDocument();
  expect(screen.getByText(/Automatic scans notify about vulnerabilities only when a scan stores findings that were not in the previous scan of the same image, even if coverage is unknown\./)).toBeInTheDocument();
  expect(screen.getByText(/also notify on an image's first automatic result, and when its result changes to having no findings or to unknown coverage\./)).toBeInTheDocument();
  expect(screen.getByText(/Repeating the previous automatic result does not notify, including a rescan after a vulnerability database update\./)).toBeInTheDocument();
  expect(screen.queryByText(/unknown result do not notify/)).not.toBeInTheDocument();
  expect(screen.getByText(/may not be able to tell that a finding is new/)).toBeInTheDocument();
  expect(screen.queryByText(/image\.scan_completed|image\.scan_unknown/)).not.toBeInTheDocument();
});
