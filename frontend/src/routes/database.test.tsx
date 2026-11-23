import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { MaintenanceTask } from "../api/databases";
import type { DatabaseResource, Workload } from "../api/types";
import { ResourceDatabase } from "./database";

const resource = vi.hoisted((): { current: unknown } => ({ current: null }));
vi.mock("./workload-detail", () => ({ useResource: () => resource.current }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: unknown }) => children }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

const password = "fixture-password-value";
const database: DatabaseResource = { engine: "postgresql", version: "17.12-alpine", template_revision: "rev-1", service: "postgres", image: "postgres:17.12-alpine", port: 5432, host: "db-abc.internal", pooler: false, extensions: ["pg_trgm"] };
const baseDetails = {
  revision: "revision-1",
  maintenance_locked: false,
  maintenance_available: true,
  database,
  host: database.host,
  user: "app",
  name: "app",
  exports: ["DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_URL"].map((key) => ({ key, secret: key === "DB_PASSWORD" || key === "DB_URL" })),
  logical_backup: true,
  connection_required: true,
  admin_tools: ["adminer"],
  settings_state: "succeeded",
  dependents: [
    { id: "api", name: "orders-api", needs_redeploy: true },
    { id: "worker", name: "orders-worker", needs_redeploy: false },
  ],
  hidden_dependents: 1,
  copy_targets: [
    { version: "18.7-alpine", image: "postgres:18.7-alpine", variant: "" },
    { version: "18.6-postgis3.6", image: "postgis/postgis:18-3.6", variant: "postgis", warning: "Linux amd64 only." },
  ],
  extension_options: ["pg_trgm", "hstore", "uuid-ossp", "pg_stat_statements", "vector", "postgis"],
  tls_ca: "",
};
let details: Record<string, unknown> = baseDetails;
let tasks: MaintenanceTask[] = [];

function workload(permissions: Workload["permissions"]): Workload {
  return { id: "abc", name: "orders", node_id: "n1", project_id: "p1", kind: "database", database, compose: "", environment_keys: [], source: null, has_source_token: false, manually_stopped: false, deployed: true, permissions };
}

const preflight = { revision: "revision-1", runtime: { version: "17.12", extensions: ["plpgsql", "pg_trgm"], arch: "x86_64" }, copy_targets: baseDetails.copy_targets, backup_required: true, scope: "Only the app database is copied." };

function mockApi(overrides: (method: string, path: string, body: unknown) => unknown = () => undefined) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const override = overrides(method, path, body);
    if (override !== undefined) return override instanceof Error ? Promise.reject(override) : Promise.resolve(override);
    if (method === "GET" && path === "/api/workloads/abc/database") return Promise.resolve(details);
    if (method === "GET" && path === "/api/workloads/abc/database/maintenance") return Promise.resolve(tasks);
    if (method === "GET" && path.startsWith("/api/operations")) return Promise.resolve([]);
    if (method === "GET" && path === "/api/workloads/abc/database/credentials") return Promise.resolve({ host: database.host, port: 5432, user: "app", password, database: "app", url: `postgresql://app:${password}@${database.host}:5432/app` });
    if (method === "POST" && path === "/api/workloads/abc/database/preflight") return Promise.resolve(preflight);
    if (method === "POST" && path === "/api/workloads/abc/database/maintenance") return Promise.resolve({ id: "op-1", resource_id: "copy-1" });
    if (method === "POST" && path.startsWith("/api/workloads/abc/database/")) return Promise.resolve({ id: "op-2", ok: true });
    return Promise.reject(new Error(`Unexpected ${method} ${path}`));
  });
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceDatabase />
    </QueryClientProvider>,
  );
  return client;
}

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  details = baseDetails;
  tasks = [];
});
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const editor = () => workload(["read", "operate", "edit"]);

it("keeps credentials concealed until an explicit reveal and show, and discards them on hide", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  const reveal = await screen.findByRole("button", { name: "Reveal credentials" });
  expect(request).not.toHaveBeenCalledWith("GET", "/api/workloads/abc/database/credentials");
  await user.click(reveal);
  await screen.findByLabelText("Password hidden");
  expect(screen.queryByText(password, { exact: false })).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Show password" }));
  expect(screen.getByText(password)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Hide credentials" }));
  expect(screen.queryByText(password, { exact: false })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Reveal credentials" })).toBeInTheDocument();
});

it("hides credential, settings and maintenance actions from viewers but shows bound resources", async () => {
  resource.current = workload(["read"]);
  mockApi();
  renderPage();
  await screen.findByText(/Editor access is needed to reveal credentials/);
  expect(screen.queryByRole("button", { name: "Reveal credentials" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Rotate password" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Enable public access" })).not.toBeInTheDocument();
  const bound = screen.getByRole("list", { name: "Bound resources" });
  expect(within(bound).getByText("orders-api").closest("li")).toHaveTextContent("Deploy needed");
  expect(within(bound).getByText("orders-worker").closest("li")).toHaveTextContent("Up to date");
  expect(within(bound).getByText(/1 more resource you cannot view/)).toBeInTheDocument();
});

it("requires a compatibility check and confirmation before creating an upgrade copy to a server-offered target", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Create upgrade copy" }));
  const dialog = screen.getByRole("dialog", { name: "Create an upgrade copy?" });
  expect(within(dialog).getByText("Linux amd64 only.")).toBeInTheDocument();
  const submit = within(dialog).getByRole("button", { name: "Create copy" });
  await user.click(within(dialog).getByRole("checkbox", { name: /one-time snapshot copy/ }));
  expect(submit).toBeDisabled();
  await user.click(within(dialog).getByRole("button", { name: "Check compatibility" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/preflight", { revision: "revision-1", version: "18.7-alpine" }));
  expect(await within(dialog).findByText("Only the app database is copied.")).toBeInTheDocument();
  // The writes-stopped acknowledgement is separate, unchecked by default and required.
  const writes = within(dialog).getByRole("checkbox", { name: /external writers to orders are stopped/ });
  expect(writes).not.toBeChecked();
  expect(submit).toBeDisabled();
  await user.click(writes);
  await user.click(submit);
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance", { kind: "upgrade", revision: "revision-1", confirm: true, confirm_writes_stopped: true, version: "18.7-alpine", name: "orders 18" }),
  );
  expect(await within(dialog).findByText("Copy queued")).toBeInTheDocument();
});

it("shows an incompatible copy target's reason and keeps the copy disabled", async () => {
  resource.current = editor();
  mockApi((method, path) => (method === "POST" && path.endsWith("/preflight") ? new ApiError(409, "the pinned PostGIS image supports Linux amd64 only") : undefined));
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Create upgrade copy" }));
  const dialog = screen.getByRole("dialog", { name: "Create an upgrade copy?" });
  await user.click(within(dialog).getByRole("radio", { name: /PostGIS/ }));
  await user.click(within(dialog).getByRole("checkbox", { name: /one-time snapshot copy/ }));
  await user.click(within(dialog).getByRole("checkbox", { name: /external writers/ }));
  await user.click(within(dialog).getByRole("button", { name: "Check compatibility" }));
  expect(await within(dialog).findByText(/supports Linux amd64 only/)).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Create copy" })).toBeDisabled();
});

it("disables an extension only after confirming its RESTRICT consequence", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Disable pg_trgm" }));
  const dialog = screen.getByRole("dialog", { name: "Disable pg_trgm?" });
  expect(dialog).toHaveTextContent(/RESTRICT/);
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Disable extension" }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance", { kind: "extension", revision: "revision-1", confirm: true, extension: "pg_trgm", enabled: false }),
  );
  expect(screen.queryByRole("button", { name: "Enable vector" })).not.toBeInTheDocument();
  expect(screen.getAllByText("Needs a copy to its image variant")).toHaveLength(2);
  expect(screen.getByRole("list", { name: "Extensions" })).toHaveTextContent("Not managed, not inspected");
});

it("enables the pooler with the reviewed revision and links the settings operation", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Enable pooler" }));
  const dialog = screen.getByRole("dialog", { name: "Enable the connection pooler?" });
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Enable pooler" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/pooler", { enabled: true, revision: "revision-1", confirm: true }));
  expect(await within(dialog).findByText("Settings change queued")).toBeInTheDocument();
});

it("validates public access locally, sends the masked key once and clears it after saving", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Enable public access" }));
  const dialog = screen.getByRole("dialog", { name: "Enable public TLS access?" });
  const key = within(dialog).getByLabelText("Private key (PEM)");
  expect(key).toHaveAttribute("type", "password");
  await user.type(within(dialog).getByLabelText("Hostname"), "203.0.113.4");
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Publish database" }));
  expect(within(dialog).getByText(/not an IP address/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/workloads/abc/database/public-access", expect.anything());

  await user.clear(within(dialog).getByLabelText("Hostname"));
  await user.type(within(dialog).getByLabelText("Hostname"), "db.example.com");
  await user.type(within(dialog).getByLabelText("Bind address"), "0.0.0.0");
  await user.type(within(dialog).getByLabelText("Server certificate (PEM)"), "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----");
  // Password inputs drop line breaks, so the key is pasted on one line and rebuilt before sending.
  await user.type(key, "-----BEGIN PRIVATE KEY-----MIIEkey-----END PRIVATE KEY-----");
  await user.click(within(dialog).getByRole("button", { name: "Publish database" }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/public-access", {
      enabled: true,
      revision: "revision-1",
      confirm: true,
      hostname: "db.example.com",
      bind_address: "0.0.0.0",
      port: 5432,
      certificate: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
      private_key: "-----BEGIN PRIVATE KEY-----\nMIIEkey\n-----END PRIVATE KEY-----\n",
    }),
  );
  expect(await within(dialog).findByText("Settings change queued")).toBeInTheDocument();
  expect(within(dialog).queryByLabelText("Private key (PEM)")).not.toBeInTheDocument();
});

it("shows saved public access metadata only and confirms before disabling it", async () => {
  details = {
    ...baseDetails,
    host: "db.example.com",
    database: { ...database, host: "db.example.com", private_host: "db-abc.internal", public_access: { hostname: "db.example.com", bind_address: "0.0.0.0", port: 5432, fingerprint: "ab12", expires_at: "2027-11-22T00:00:00Z" } },
    tls_ca: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
  };
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  expect(await screen.findByText("ab12")).toBeInTheDocument();
  expect(screen.getByText("0.0.0.0:5432")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Disable public access" }));
  const dialog = screen.getByRole("dialog", { name: "Disable public TLS access?" });
  const submit = within(dialog).getByRole("button", { name: "Disable public access" });
  expect(submit).toBeDisabled();
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(submit);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/public-access", { enabled: false, revision: "revision-1", confirm: true }));
});

it("reports a failed settings operation with a link to it", async () => {
  details = { ...baseDetails, settings_state: "failed", database: { ...database, settings_operation: "op-settings" } };
  resource.current = editor();
  mockApi();
  renderPage();
  expect(await screen.findByText("The last settings change was not applied")).toBeInTheDocument();
});

const copyTask = (state: MaintenanceTask["state"], operation: string, extra: Partial<MaintenanceTask> = {}): MaintenanceTask => ({
  id: "task-1",
  kind: "upgrade",
  resource_id: "copy-1",
  source_id: "abc",
  state,
  operation_state: operation,
  error: "",
  created_at: "2026-11-22T10:00:00Z",
  cutover_confirmed: false,
  ...extra,
});

it("cuts over a verified copy only after both confirmations, using the source revision", async () => {
  tasks = [copyTask("succeeded", "succeeded", { can_cutover: true })];
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Cut over" }));
  const dialog = screen.getByRole("dialog", { name: "Cut over to the copy?" });
  const submit = within(dialog).getByRole("button", { name: "Switch to copy" });
  await user.click(within(dialog).getByRole("checkbox", { name: "Application writes to the source have stopped." }));
  expect(submit).toBeDisabled();
  await user.click(within(dialog).getByRole("checkbox", { name: /I reviewed the copy/ }));
  await user.click(submit);
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/cutover", { confirm: true, confirm_writes_stopped: true, revision: "revision-1" }),
  );
  expect(await within(dialog).findByText("Cut-over recorded")).toBeInTheDocument();
});

it("disables cut-over and explains why when the row says the caller cannot cut over", async () => {
  tasks = [copyTask("succeeded", "succeeded", { can_cutover: false })];
  resource.current = editor();
  mockApi();
  renderPage();
  expect(await screen.findByRole("button", { name: "Cut over" })).toBeDisabled();
  expect(screen.getByText(/Cut-over needs a team owner or admin who can also edit both the source and the copy/)).toBeInTheDocument();
});

it("discards a failed copy only after its name is typed", async () => {
  tasks = [copyTask("failed", "failed", { resource_id: "abc", source_id: "src-1" })];
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Discard copy" }));
  const dialog = screen.getByRole("dialog", { name: "Discard the failed copy?" });
  const submit = within(dialog).getByRole("button", { name: "Discard copy" });
  await user.type(within(dialog).getByLabelText("Type orders to confirm"), "order");
  expect(submit).toBeDisabled();
  await user.type(within(dialog).getByLabelText("Type orders to confirm"), "s");
  await user.click(submit);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/discard", { confirm: true, name: "orders" }));
  expect(await within(dialog).findByText("Failed copy discarded")).toBeInTheDocument();
});

it("explains a rejected copy request without claiming a snapshot was taken", async () => {
  resource.current = editor();
  mockApi((method, path) => (method === "POST" && path === "/api/workloads/abc/database/maintenance" ? new ApiError(400, "confirm application writes have stopped") : undefined));
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Create upgrade copy" }));
  const dialog = screen.getByRole("dialog", { name: "Create an upgrade copy?" });
  await user.click(within(dialog).getByRole("button", { name: "Check compatibility" }));
  await within(dialog).findByText("Only the app database is copied.");
  for (const box of within(dialog).getAllByRole("checkbox")) await user.click(box);
  await user.click(within(dialog).getByRole("button", { name: "Create copy" }));
  expect(await within(dialog).findByText(/Nothing was copied and no snapshot was taken/)).toBeInTheDocument();
});

it("retries a failed copy only after the writes-stopped acknowledgement is given again", async () => {
  tasks = [copyTask("failed", "failed")];
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Retry" }));
  const dialog = screen.getByRole("dialog", { name: "Retry the upgrade copy?" });
  const writes = within(dialog).getByRole("checkbox", { name: /external writers to orders are stopped/ });
  const submit = within(dialog).getByRole("button", { name: "Retry copy" });
  expect(writes).not.toBeChecked();
  expect(submit).toBeDisabled();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/retry", expect.anything());
  await user.click(writes);
  await user.click(submit);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/retry", { confirm_writes_stopped: true }));
  expect(await within(dialog).findByText("Retry queued")).toBeInTheDocument();
});

it("explains a copy retry refused while the source or copy is busy", async () => {
  tasks = [copyTask("failed", "failed")];
  resource.current = editor();
  mockApi((method, path) => (method === "POST" && path.endsWith("/retry") ? new ApiError(409, "finish source and target operations before retrying maintenance") : undefined));
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Retry" }));
  const dialog = screen.getByRole("dialog", { name: "Retry the upgrade copy?" });
  await user.click(within(dialog).getByRole("checkbox"));
  await user.click(within(dialog).getByRole("button", { name: "Retry copy" }));
  expect(await within(dialog).findByText(/Wait for active operations on the source and the copy to finish/)).toBeInTheDocument();
});

it("retries a failed rotation directly with an empty body", async () => {
  tasks = [copyTask("failed", "failed", { kind: "rotate", source_id: "", resource_id: "abc" })];
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "Retry" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/retry", {}));
  expect(screen.queryByRole("dialog", { name: "Retry the upgrade copy?" })).not.toBeInTheDocument();
});

it("labels extensions from the runtime only after an explicit inspection", async () => {
  resource.current = editor();
  mockApi();
  const user = userEvent.setup();
  renderPage();
  const list = await screen.findByRole("list", { name: "Extensions" });
  expect(within(list).getByText("pg_trgm").closest("li")).toHaveTextContent("Managed by Cove, not inspected");
  expect(within(list).getByText("hstore").closest("li")).toHaveTextContent("Not managed, not inspected");
  expect(request).not.toHaveBeenCalledWith("POST", "/api/workloads/abc/database/preflight", expect.anything());
  await user.click(screen.getByRole("button", { name: "Inspect runtime" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/preflight", { revision: "revision-1" }));
  expect(await within(list).findByText("Installed, managed by Cove")).toBeInTheDocument();
  expect(within(list).getByText("hstore").closest("li")).toHaveTextContent("Not installed");
});

it("shows an honest notice when the settings operation is no longer recorded", async () => {
  details = { ...baseDetails, settings_state: "unknown", database: { ...database, settings_operation: "op-gone" } };
  resource.current = editor();
  mockApi();
  renderPage();
  expect(await screen.findByText("Settings state unknown")).toBeInTheDocument();
  expect(screen.getByText("Disabled, apply state unknown")).toBeInTheDocument();
});

it("shows the pooler hostname returned by the API", async () => {
  details = { ...baseDetails, database: { ...database, pooler: true, pooler_host: "pool-abc.internal" } };
  resource.current = editor();
  mockApi();
  renderPage();
  expect(await screen.findByText("pool-abc.internal")).toBeInTheDocument();
});

// Open confirmation dialogs keep what was reviewed when the details refresh underneath them.
describe("dialogs opened before a background refresh", () => {
  it("keeps the pooler dialog on the enable action after its own success refreshes the details", async () => {
    resource.current = editor();
    mockApi((method, path) => {
      if (method === "POST" && path.endsWith("/pooler")) {
        details = { ...baseDetails, revision: "revision-2", database: { ...database, pooler: true, pooler_host: "pool-abc.internal" } };
        return { id: "op-pool" };
      }
      return undefined;
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole("button", { name: "Enable pooler" }));
    const dialog = screen.getByRole("dialog", { name: "Enable the connection pooler?" });
    await user.click(within(dialog).getByRole("checkbox"));
    await user.click(within(dialog).getByRole("button", { name: "Enable pooler" }));
    await within(dialog).findByText("Settings change queued");
    // The page behind the dialog now reflects the enabled pooler.
    expect(await screen.findByText("pool-abc.internal")).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Enable the connection pooler?" })).toBe(dialog);
    expect(screen.queryByRole("dialog", { name: "Disable the connection pooler?" })).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Close" })).toBeInTheDocument();
  });

  it("keeps an extension change and its reviewed revision", async () => {
    resource.current = editor();
    mockApi();
    const user = userEvent.setup();
    const client = renderPage();
    await user.click(await screen.findByRole("button", { name: "Disable pg_trgm" }));
    const dialog = screen.getByRole("dialog", { name: "Disable pg_trgm?" });
    details = { ...baseDetails, revision: "revision-2", database: { ...database, extensions: [] } };
    await client.invalidateQueries();
    await screen.findByRole("button", { name: "Enable pg_trgm" });
    expect(within(dialog).getByRole("button", { name: "Disable extension" })).toBeInTheDocument();
    await user.click(within(dialog).getByRole("checkbox"));
    await user.click(within(dialog).getByRole("button", { name: "Disable extension" }));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance", { kind: "extension", revision: "revision-1", confirm: true, extension: "pg_trgm", enabled: false }),
    );
  });

  it("keeps the public access enable dialog and its revision when access appears in a refresh", async () => {
    resource.current = editor();
    mockApi();
    const user = userEvent.setup();
    const client = renderPage();
    await user.click(await screen.findByRole("button", { name: "Enable public access" }));
    const dialog = screen.getByRole("dialog", { name: "Enable public TLS access?" });
    details = {
      ...baseDetails,
      revision: "revision-2",
      database: { ...database, host: "db.example.com", private_host: "db-abc.internal", public_access: { hostname: "db.example.com", bind_address: "0.0.0.0", port: 5432, fingerprint: "ab12", expires_at: "2027-11-22T00:00:00Z" } },
    };
    await client.invalidateQueries();
    await screen.findByText("ab12");
    expect(screen.getByRole("dialog", { name: "Enable public TLS access?" })).toBe(dialog);
    expect(within(dialog).getByRole("button", { name: "Publish database" })).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText("Hostname"), "db.example.com");
    await user.type(within(dialog).getByLabelText("Bind address"), "0.0.0.0");
    await user.type(within(dialog).getByLabelText("Server certificate (PEM)"), "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----");
    await user.type(within(dialog).getByLabelText("Private key (PEM)"), "-----BEGIN PRIVATE KEY-----MIIEkey-----END PRIVATE KEY-----");
    await user.click(within(dialog).getByRole("checkbox"));
    await user.click(within(dialog).getByRole("button", { name: "Publish database" }));
    await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/public-access", expect.objectContaining({ enabled: true, revision: "revision-1", hostname: "db.example.com" })));
  });

  it("keeps the cut-over revision reviewed when the dialog opened", async () => {
    tasks = [copyTask("succeeded", "succeeded", { can_cutover: true })];
    resource.current = editor();
    mockApi();
    const user = userEvent.setup();
    const client = renderPage();
    await user.click(await screen.findByRole("button", { name: "Cut over" }));
    const dialog = screen.getByRole("dialog", { name: "Cut over to the copy?" });
    details = { ...baseDetails, revision: "revision-2" };
    await client.invalidateQueries();
    for (const box of within(dialog).getAllByRole("checkbox")) await user.click(box);
    await user.click(within(dialog).getByRole("button", { name: "Switch to copy" }));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database/maintenance/task-1/cutover", { confirm: true, confirm_writes_stopped: true, revision: "revision-1" }),
    );
  });
});
