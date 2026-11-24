import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Route, Workload } from "../api/types";
import { ResourceObjectStorage } from "./object-storage";

const consumer: Workload = { id: "app-1", name: "uploads-api", kind: "service", node_id: "n1", manually_stopped: false, deployed: true, permissions: ["read", "edit"], project_id: "p1", compose: "", environment_keys: [], source: null, has_source_token: false };
const garage = (routes: Route[]): Workload => ({ ...consumer, id: "garage-1", name: "garage", compose: "services:\n  garage:\n    image: dxflrs/garage:v2.1.0\n", permissions: ["read", "edit", "read_secrets"], routes });

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: true, team_id: "t1", teams: [] }) }));
vi.mock("./workload-detail", () => ({ useResource: () => consumer }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

function serve(storage: Workload) {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/workloads/app-1/object-buckets") return Promise.resolve([]);
    if (path === "/api/workloads") return Promise.resolve([{ id: storage.id, name: storage.name, kind: "service", node_id: "n1", node: "worker", project_id: "p1", permissions: storage.permissions }]);
    if (path === "/api/workloads/garage-1") return Promise.resolve(storage);
    return Promise.reject(new Error(`unexpected ${path}`));
  });
}

async function chooseGarage() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceObjectStorage />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("combobox", { name: "Garage resource" }));
  fireEvent.click(await screen.findByRole("option", { name: "garage" }));
}

it("never offers a wildcard hostname as the S3 endpoint", async () => {
  serve(garage([
    { domain: "*.s3.example.com", service: "garage", port: 3900 },
    { domain: "s3.example.com", service: "garage", port: 3900 },
  ]));
  await chooseGarage();
  const endpoint = await screen.findByRole("combobox", { name: "S3 endpoint" });
  expect(endpoint).toHaveTextContent("https://s3.example.com");
  fireEvent.click(endpoint);
  const options = within(screen.getByRole("listbox")).getAllByRole("option");
  expect(options.map((option) => option.textContent)).toEqual(["https://s3.example.com"]);
});

it("asks for an exact domain when the Garage resource only has a wildcard S3 route", async () => {
  serve(garage([{ domain: "*.s3.example.com", service: "garage", port: 3900 }]));
  await chooseGarage();
  expect(await screen.findByText("Add the S3 route first")).toBeInTheDocument();
  expect(screen.getByText(/A wildcard domain or a LAN or Tailscale only domain cannot be the S3 endpoint/)).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "S3 endpoint" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Provision bucket" })).toBeDisabled();
});

it("never offers a private or raw route as the S3 endpoint", async () => {
  serve(garage([
    { domain: "s3.lan.example.com", service: "garage", port: 3900, policy: { visibility: "lan", listener: "office", certificate_source: "default" } },
    { domain: "s3.ts.example.com", service: "garage", port: 3900, policy: { visibility: "tailscale", listener: "tail", certificate_source: "default" } },
    { protocol: "tcp", domain: "", service: "garage", port: 3900, policy: { visibility: "public", listener: "s3raw" } },
    { domain: "s3.example.com", service: "garage", port: 3900, policy: { visibility: "public" } },
  ]));
  await chooseGarage();
  const endpoint = await screen.findByRole("combobox", { name: "S3 endpoint" });
  fireEvent.click(endpoint);
  const options = within(screen.getByRole("listbox")).getAllByRole("option");
  expect(options.map((option) => option.textContent)).toEqual(["https://s3.example.com"]);
});

it("refuses a Garage resource whose S3 route is private", async () => {
  serve(garage([{ domain: "s3.lan.example.com", service: "garage", port: 3900, policy: { visibility: "lan", listener: "office", certificate_source: "default" } }]));
  await chooseGarage();
  expect(await screen.findByText("Add the S3 route first")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "S3 endpoint" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Provision bucket" })).toBeDisabled();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
});
