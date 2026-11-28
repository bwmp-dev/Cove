import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { firewallQuery, type FirewallResult } from "../api/firewall";
import type { Node } from "../api/types";
import { NodeFirewall } from "./NodeFirewall";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
    this.removeAttribute("open");
  };
});
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, firewall: true };
const node: Node = { id: "n1", name: "edge-worker", state: "ready", mode: "ssh", address: "10.0.0.12", port: 22, last_checked_at: null, capabilities };
const path = "/api/nodes/n1/firewall";
const revision = "a".repeat(64);

const inspected: FirewallResult = {
  supported: true,
  active: true,
  rules: [{ protocol: "tcp", port: 8080, sources: ["192.0.2.0/24"] }],
  available: [
    { protocol: "tcp", port: 8080 },
    { protocol: "udp", port: 5353 },
    { protocol: "tcp", port: 2222 },
  ],
  reserved: [22, 80, 443, 2222],
  revision: "",
  ufw: "Status: inactive",
  nftables: "table inet filter {}",
  limited: false,
  notice: "",
  last_error: "",
};

type Handlers = { get?: () => Promise<unknown>; preview?: (body: unknown) => Promise<unknown>; apply?: (body: unknown) => Promise<unknown> };

function mockApi(handlers: Handlers) {
  vi.mocked(request).mockImplementation((method, url, body) => {
    if (method === "GET" && url === path && handlers.get) return handlers.get();
    if (method === "POST" && url === `${path}/preview` && handlers.preview) return handlers.preview(body);
    if (method === "POST" && url === path && handlers.apply) return handlers.apply(body);
    return Promise.reject(new Error(`unexpected ${method} ${url}`));
  });
}

function renderSection(target: Node = node) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <NodeFirewall node={target} />
    </QueryClientProvider>,
  );
}

const calls = (method: string, url: string) => vi.mocked(request).mock.calls.filter(([m, u]) => m === method && u === url);

async function inspect() {
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  return screen.findByRole("button", { name: "Edit rules" });
}

async function previewSources(text: string) {
  fireEvent.click(await inspect());
  fireEvent.change(screen.getByLabelText("Allowed sources"), { target: { value: text } });
  fireEvent.click(screen.getByRole("button", { name: "Preview change" }));
  return screen.findByRole("dialog", { name: "Apply this firewall policy?" });
}

it("inspects only when asked and shows rules, ports and read-only host output", async () => {
  mockApi({ get: () => Promise.resolve(inspected) });
  renderSection();
  expect(screen.getByText(/Not inspected yet/)).toBeInTheDocument();
  expect(screen.getByText(/Cove's own allow-list/)).toHaveTextContent("running Cove resources, the shared proxy's extra listeners and the private-link gateway. Ports bound only to loopback are not listed.");
  expect(request).not.toHaveBeenCalled();
  await inspect();
  const rules = screen.getByRole("list", { name: "Current Cove rules" });
  expect(within(rules).getByText("TCP 8080")).toBeInTheDocument();
  expect(within(rules).getByText("192.0.2.0/24")).toBeInTheDocument();
  expect(screen.getByText("(protected)")).toBeInTheDocument();
  const scope = screen.getByText(/Host ports published by running Cove resources/);
  expect(scope).toHaveTextContent("the shared proxy's extra listeners and the private-link gateway, excluding loopback-only bindings");
  expect(scope).toHaveTextContent("Restricting the gateway's port can interrupt private links to this worker.");
  expect(screen.getByText(/never changed/)).toHaveTextContent("SSH, the panel port and ports 80 and 443 are never changed");
  const output = screen.getByRole("region", { name: "UFW status" });
  expect(output).toHaveAttribute("tabindex", "0");
  expect(output).toHaveTextContent("Status: inactive");
  expect(calls("GET", path)).toHaveLength(1);
});

it("explains why inspection is unavailable for old, unprobed, unready or offline workers", () => {
  const old = renderSection({ ...node, capabilities: { ...capabilities, firewall: false } });
  expect(screen.getByText(/has not reported support for firewall management/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  old.unmount();

  const unprobed = renderSection({ ...node, capabilities: null });
  expect(screen.getByText(/no current probe result/)).toBeInTheDocument();
  unprobed.unmount();

  const unready = renderSection({ ...node, state: "unreachable" });
  expect(screen.getByText(/needs a ready worker/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  unready.unmount();

  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  renderSection();
  expect(screen.getByText(/You are offline/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  expect(request).not.toHaveBeenCalled();
});

it("keeps unsupported hosts read-only and tolerates null lists", async () => {
  mockApi({
    get: () =>
      Promise.resolve({
        ...inspected,
        supported: false,
        active: false,
        rules: null,
        available: null,
        reserved: null,
        limited: true,
        notice: "Install nftables and verify host namespace privileges before changing the firewall.",
      }),
  });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Firewall changes are unavailable on this host")).toBeInTheDocument();
  expect(screen.getByText(/Install nftables/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Edit rules" })).toBeDisabled();
  expect(screen.getByText(/clipped at the inspection limit/)).toBeInTheDocument();
  expect(screen.getByText(/Firewall changes stay unavailable until the host reports/)).toBeInTheDocument();
});

it("shows inspection refusals such as a busy node with a retry", async () => {
  mockApi({ get: () => Promise.reject(new ApiError(409, "verify an upgraded Linux worker and wait for operations and SSH identity changes")) });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Could not load the firewall status")).toBeInTheDocument();
  expect(screen.getByText(/wait for operations/)).toBeInTheDocument();
  expect(calls("GET", path)).toHaveLength(1);
});

it("only offers published ports that are not protected", async () => {
  mockApi({ get: () => Promise.resolve(inspected) });
  renderSection();
  fireEvent.click(await inspect());
  fireEvent.click(screen.getByRole("button", { name: "Add rule" }));
  // The first free selectable port is UDP 5353; TCP 2222 is protected and can never be chosen.
  expect(screen.getAllByLabelText("Port")).toHaveLength(2);
  expect(screen.getByRole("button", { name: "Add rule" })).toBeDisabled();
  expect(screen.queryByRole("option", { name: "TCP 2222", hidden: true })).not.toBeInTheDocument();
});

it("validates sources locally before previewing", async () => {
  mockApi({ get: () => Promise.resolve(inspected) });
  renderSection();
  fireEvent.click(await inspect());
  expect(screen.getByRole("button", { name: "Preview change" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Allowed sources"), { target: { value: "192.0.2.0/24\nnot-a-prefix" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview change" }));
  expect(await screen.findByText(/Not an IPv4 or IPv6 CIDR prefix: not-a-prefix/)).toBeInTheDocument();
  expect(calls("POST", `${path}/preview`)).toHaveLength(0);
});

it("applies exactly the previewed canonical rules and revision after the node name is typed", async () => {
  const preview = vi.fn().mockResolvedValue({ ...inspected, rules: [{ protocol: "tcp", port: 8080, sources: ["198.51.100.0/24"] }], revision, notice: "" });
  const apply = vi.fn().mockResolvedValue({ ...inspected, rules: [{ protocol: "tcp", port: 8080, sources: ["198.51.100.0/24"] }] });
  mockApi({ get: () => Promise.resolve(inspected), preview, apply });
  renderSection();
  const dialog = await previewSources("198.51.100.7/24");
  expect(preview).toHaveBeenCalledWith({ rules: [{ protocol: "tcp", port: 8080, sources: ["198.51.100.7/24"] }] });
  expect(within(dialog).getByText("198.51.100.0/24")).toBeInTheDocument();

  const submit = within(dialog).getByRole("button", { name: "Apply firewall change" });
  expect(submit).toBeDisabled();
  const confirm = within(dialog).getByLabelText("Type edge-worker to confirm");
  fireEvent.change(confirm, { target: { value: "edge-work" } });
  // Submitting with Enter must not bypass the disabled button.
  fireEvent.submit(confirm.closest("form") as HTMLFormElement);
  expect(apply).not.toHaveBeenCalled();

  fireEvent.change(confirm, { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply firewall change" }));
  await waitFor(() => expect(apply).toHaveBeenCalledWith({ rules: [{ protocol: "tcp", port: 8080, sources: ["198.51.100.0/24"] }], revision, confirm: true }));
  expect(await screen.findByText("Firewall change confirmed")).toBeInTheDocument();
  expect(screen.getByText(/protected boot service restores this policy/)).toBeInTheDocument();
  expect(screen.getByText(/new management port is discovered later/)).toBeInTheDocument();
  await waitFor(() => expect(calls("GET", path)).toHaveLength(2));
  expect(apply).toHaveBeenCalledTimes(1);
});

it("lists removal effects and discards the preview when the dialog is cancelled", async () => {
  const preview = vi.fn().mockResolvedValue({ ...inspected, rules: [], revision, notice: "" });
  mockApi({ get: () => Promise.resolve(inspected), preview });
  renderSection();
  fireEvent.click(await inspect());
  fireEvent.click(screen.getByRole("button", { name: /Remove rule for TCP 8080/ }));
  fireEvent.click(screen.getByRole("button", { name: "Preview change" }));
  const dialog = await screen.findByRole("dialog", { name: "Apply this firewall policy?" });
  expect(dialog).toHaveTextContent("Cove will stop restricting TCP 8080");
  expect(dialog).toHaveTextContent(/Cove will not restrict any published application port/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  // Editing again requires a fresh preview.
  fireEvent.click(screen.getByRole("button", { name: "Add rule" }));
  fireEvent.click(screen.getByRole("button", { name: "Preview change" }));
  await waitFor(() => expect(preview).toHaveBeenCalledTimes(2));
});

it("reports an ambiguous apply without retrying and re-inspects once", async () => {
  let gets = 0;
  const pendingResult = { ...inspected, pending: { id: "c".repeat(32), deadline: "2099-01-01T00:02:00Z" } };
  const apply = vi.fn().mockRejectedValue(new ApiError(502, "firewall confirmation could not be verified; refresh before retrying and inspect the recovery deadline"));
  mockApi({ get: () => Promise.resolve(gets++ === 0 ? inspected : pendingResult), preview: () => Promise.resolve({ ...inspected, revision }), apply });
  renderSection();
  const dialog = await previewSources("192.0.2.0/24\n203.0.113.0/24");
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply firewall change" }));
  expect(await screen.findByText("The firewall change could not be confirmed")).toBeInTheDocument();
  expect(screen.getByText(/may or may not have been applied/)).toBeInTheDocument();
  expect(await screen.findByText("Unconfirmed firewall change")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Edit rules" })).toBeDisabled();
  expect(calls("GET", path)).toHaveLength(2);
  expect(apply).toHaveBeenCalledTimes(1);
});

it("treats a 409 after apply as ambiguous but a 400 as a refusal", async () => {
  const apply = vi
    .fn()
    .mockRejectedValueOnce(new ApiError(409, "host firewall prerequisites or a confirmed pending change are unavailable; inspect its status"))
    .mockRejectedValueOnce(new ApiError(400, "preview the current firewall policy first"));
  mockApi({ get: () => Promise.resolve(inspected), preview: () => Promise.resolve({ ...inspected, revision }), apply });
  renderSection();
  let dialog = await previewSources("192.0.2.0/24\n203.0.113.0/24");
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply firewall change" }));
  expect(await screen.findByText(/may or may not have been applied/)).toBeInTheDocument();

  fireEvent.click(await screen.findByRole("button", { name: "Edit rules" }));
  fireEvent.change(screen.getByLabelText("Allowed sources"), { target: { value: "198.51.100.0/24" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview change" }));
  dialog = await screen.findByRole("dialog", { name: "Apply this firewall policy?" });
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply firewall change" }));
  expect(await screen.findByText("The firewall change was refused")).toBeInTheDocument();
  expect(screen.queryByText(/may or may not have been applied/)).not.toBeInTheDocument();
});

it("reverts a pending change only after confirmation", async () => {
  let gets = 0;
  const pendingResult = { ...inspected, pending: { id: "c".repeat(32), deadline: "2099-01-01T00:02:00Z" } };
  const apply = vi.fn().mockResolvedValue({ ...inspected, last_error: "The unconfirmed firewall change was reverted." });
  mockApi({ get: () => Promise.resolve(gets++ === 0 ? pendingResult : { ...inspected, last_error: "The unconfirmed firewall change was reverted." }), apply });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Unconfirmed firewall change")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Unconfirmed Cove rules" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Edit rules" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Revert change" }));
  const dialog = screen.getByRole("dialog", { name: "Revert the unconfirmed firewall change?" });
  expect(apply).not.toHaveBeenCalled();
  fireEvent.click(within(dialog).getByRole("button", { name: "Revert change" }));
  await waitFor(() => expect(apply).toHaveBeenCalledWith({ revert: true, confirm: true }));
  expect(await screen.findByText("Unconfirmed change reverted")).toBeInTheDocument();
  expect(await screen.findByText("Last recovery note")).toBeInTheDocument();
  expect(apply).toHaveBeenCalledTimes(1);
  expect(calls("GET", path)).toHaveLength(2);
});

it("clears an earlier outcome when the host is inspected again manually", async () => {
  const apply = vi.fn().mockResolvedValue(inspected);
  mockApi({ get: () => Promise.resolve(inspected), preview: () => Promise.resolve({ ...inspected, revision }), apply });
  renderSection();
  const dialog = await previewSources("198.51.100.0/24");
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply firewall change" }));
  expect(await screen.findByText("Firewall change confirmed")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Inspect again" })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: "Inspect again" }));
  await waitFor(() => expect(screen.queryByText("Firewall change confirmed")).not.toBeInTheDocument());
  await waitFor(() => expect(calls("GET", path)).toHaveLength(3));
});

const removalNode: Node = { ...node, capabilities: { ...capabilities, firewall_remove: true } };
// An empty managed policy whose table is already gone: only boot restoration and recovery files remain.
const residue: FirewallResult = { ...inspected, active: false, managed: true, remove_supported: true, rules: [] };
const removed: FirewallResult = { ...inspected, active: false, managed: false, remove_supported: true, rules: [] };

async function previewRemoval() {
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  fireEvent.click(await screen.findByRole("button", { name: "Stop managing firewall" }));
  return screen.findByRole("dialog", { name: "Stop managing this host's firewall?" });
}

function confirmRemoval(dialog: HTMLElement) {
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Stop managing firewall" }));
}

it("stops managing an empty residual policy with a fresh removal preview and its exact revision", async () => {
  let gets = 0;
  const preview = vi.fn().mockResolvedValue({ ...residue, revision });
  const apply = vi.fn().mockResolvedValue(removed);
  mockApi({ get: () => Promise.resolve(gets++ === 0 ? residue : removed), preview, apply });
  renderSection(removalNode);
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText(/Cove has no rules on this host, but its table, boot restoration or recovery files remain/)).toBeInTheDocument();
  expect(screen.getByText(/An empty policy keeps them/)).toBeInTheDocument();
  expect(preview).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Stop managing firewall" }));
  const dialog = await screen.findByRole("dialog", { name: "Stop managing this host's firewall?" });
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preview).toHaveBeenCalledWith({ remove: true });
  expect(dialog).toHaveTextContent("Cove's empty table, boot restoration and recovery files are still removed");
  expect(dialog).toHaveTextContent("UFW, other nftables tables and default policies are not changed and still apply");
  expect(dialog).toHaveTextContent("independent two-minute revert");
  const submit = within(dialog).getByRole("button", { name: "Stop managing firewall" });
  expect(submit).toBeDisabled();
  fireEvent.submit(submit.closest("form") as HTMLFormElement);
  expect(apply).not.toHaveBeenCalled();

  confirmRemoval(dialog);
  await waitFor(() => expect(apply).toHaveBeenCalledWith({ remove: true, revision, confirm: true }));
  expect(await screen.findByText("Cove stopped managing this host's firewall")).toBeInTheDocument();
  await waitFor(() => expect(calls("GET", path)).toHaveLength(2));
  expect(screen.queryByRole("button", { name: "Stop managing firewall" })).not.toBeInTheDocument();
  expect(apply).toHaveBeenCalledTimes(1);
});

it("lists the active rules removal drops and offers it even with no selectable ports", async () => {
  const active = { ...inspected, managed: true, remove_supported: true, available: [{ protocol: "tcp" as const, port: 2222 }] };
  mockApi({ get: () => Promise.resolve(active), preview: () => Promise.resolve({ ...active, revision }) });
  renderSection(removalNode);
  const dialog = await previewRemoval();
  expect(within(dialog).getByRole("list", { name: "Cove rules to remove" })).toHaveTextContent("192.0.2.0/24");
});

it("does not offer removal to old workers or unprobed capabilities and hides it when nothing is managed", async () => {
  mockApi({ get: () => Promise.resolve({ ...inspected, managed: true }) });
  const old = renderSection(removalNode);
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText(/needs an upgraded worker/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Stop managing firewall" })).not.toBeInTheDocument();
  old.unmount();

  mockApi({ get: () => Promise.resolve(residue) });
  const unprobed = renderSection(node);
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText(/has not reported support for stopping firewall management/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Stop managing firewall" })).not.toBeInTheDocument();
  unprobed.unmount();

  mockApi({ get: () => Promise.resolve(removed) });
  renderSection(removalNode);
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText(/No Cove rules\. Cove does not restrict/)).toBeInTheDocument();
  expect(screen.queryByText("Stop managing the firewall")).not.toBeInTheDocument();
  expect(calls("POST", `${path}/preview`)).toHaveLength(0);
});

it("sends nothing when the removal dialog is cancelled and previews again for the next attempt", async () => {
  const preview = vi
    .fn()
    .mockResolvedValueOnce({ ...residue, revision })
    .mockResolvedValueOnce({ ...residue, revision: "d".repeat(64) });
  const apply = vi.fn().mockResolvedValue(removed);
  mockApi({ get: () => Promise.resolve(residue), preview, apply });
  renderSection(removalNode);
  let dialog = await previewRemoval();
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(apply).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Stop managing firewall" }));
  dialog = await screen.findByRole("dialog", { name: "Stop managing this host's firewall?" });
  expect(preview).toHaveBeenCalledTimes(2);
  confirmRemoval(dialog);
  await waitFor(() => expect(apply).toHaveBeenCalledWith({ remove: true, revision: "d".repeat(64), confirm: true }));
  expect(apply).toHaveBeenCalledTimes(1);
});

it("reports an ambiguous removal without retrying and describes the pending removal truthfully", async () => {
  let gets = 0;
  const pendingRemoval: FirewallResult = { ...residue, pending: { id: "c".repeat(32), deadline: "2099-01-01T00:02:00Z", remove: true } };
  const apply = vi.fn().mockRejectedValue(new ApiError(502, "firewall confirmation could not be verified; refresh before retrying and inspect the recovery deadline"));
  mockApi({ get: () => Promise.resolve(gets++ === 0 ? residue : pendingRemoval), preview: () => Promise.resolve({ ...residue, revision }), apply });
  renderSection(removalNode);
  confirmRemoval(await previewRemoval());
  expect(await screen.findByText("The request to stop managing the firewall could not be confirmed")).toBeInTheDocument();
  expect(screen.getByText(/may or may not have been applied/)).toBeInTheDocument();
  expect(await screen.findByText("Unconfirmed stop of firewall management")).toBeInTheDocument();
  expect(screen.getByText(/restores the previous policy by/)).toBeInTheDocument();
  expect(screen.queryByText("Cove stopped managing this host's firewall")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Stop managing firewall" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Revert change" })).toBeEnabled();
  expect(calls("GET", path)).toHaveLength(2);
  expect(apply).toHaveBeenCalledTimes(1);
});

it("treats a refused removal as definite and keeps an unreadable status from offering removal", async () => {
  let fail = false;
  const apply = vi.fn().mockRejectedValue(new ApiError(400, "preview stopping firewall management first"));
  mockApi({ get: () => (fail ? Promise.reject(new ApiError(503, "worker unavailable")) : Promise.resolve(residue)), preview: () => Promise.resolve({ ...residue, revision }), apply });
  renderSection(removalNode);
  confirmRemoval(await previewRemoval());
  expect(await screen.findByText("The request to stop managing the firewall was refused")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Stop managing firewall" })).toBeEnabled());

  fail = true;
  fireEvent.click(screen.getByRole("button", { name: "Inspect again" }));
  expect(await screen.findByText("Could not load the latest firewall status")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Stop managing firewall" })).toBeDisabled();
});

it("blocks an open removal confirmation when the node loses eligibility or the status is read again or fails", async () => {
  let held = false;
  let failRead: ((error: unknown) => void) | undefined;
  const apply = vi.fn().mockResolvedValue(removed);
  mockApi({
    get: () =>
      held
        ? new Promise((_, reject) => {
            failRead = reject;
          })
        : Promise.resolve(residue),
    preview: () => Promise.resolve({ ...residue, revision }),
    apply,
  });
  const client = new QueryClient();
  const view = (target: Node) => (
    <QueryClientProvider client={client}>
      <NodeFirewall node={target} />
    </QueryClientProvider>
  );
  const { rerender } = render(view(removalNode));
  const dialog = await previewRemoval();
  fireEvent.change(within(dialog).getByLabelText("Type edge-worker to confirm"), { target: { value: "edge-worker" } });
  const submit = within(dialog).getByRole("button", { name: "Stop managing firewall" });
  expect(submit).toBeEnabled();

  rerender(view({ ...removalNode, state: "unreachable" }));
  expect(submit).toBeDisabled();
  expect(dialog).toHaveTextContent("this preview cannot be confirmed");
  fireEvent.submit(submit.closest("form") as HTMLFormElement);
  rerender(view(removalNode));
  expect(submit).toBeEnabled();

  held = true;
  let reading: Promise<FirewallResult> | undefined;
  act(() => {
    reading = client.fetchQuery(firewallQuery(removalNode.id));
  });
  await waitFor(() => expect(submit).toBeDisabled());
  fireEvent.submit(submit.closest("form") as HTMLFormElement);
  await act(async () => {
    failRead?.(new ApiError(503, "worker unavailable"));
    await expect(reading).rejects.toThrow("worker unavailable");
  });
  expect(await screen.findByText("Could not load the latest firewall status")).toBeInTheDocument();
  expect(submit).toBeDisabled();
  fireEvent.click(submit);
  fireEvent.submit(submit.closest("form") as HTMLFormElement);
  expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeEnabled();
  expect(apply).not.toHaveBeenCalled();
  expect(calls("POST", path)).toHaveLength(0);
});

it("shows a removal preview refusal without opening the confirmation", async () => {
  mockApi({ get: () => Promise.resolve(residue), preview: () => Promise.reject(new ApiError(409, "upgrade this worker before stopping firewall management")) });
  renderSection(removalNode);
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  fireEvent.click(await screen.findByRole("button", { name: "Stop managing firewall" }));
  expect(await screen.findByText("Could not preview stopping firewall management")).toBeInTheDocument();
  expect(screen.getByText(/upgrade this worker/)).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(calls("POST", path)).toHaveLength(0);
});
