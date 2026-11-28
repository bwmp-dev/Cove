import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { TemplatePreview } from "../api/templates";
import { templateEntry } from "../test/templates";
import { SaveAsTemplate } from "./SaveAsTemplate";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => vi.clearAllMocks());

const preview: TemplatePreview = {
  template: templateEntry({
    id: "my-app",
    compose: "services:\n  app:\n    image: example/app:1.2.3\n    environment:\n      TOKEN: ${SETTING_1:?Enter a fresh template value}\n",
    variables: [{ key: "SETTING_1", label: "Service app environment TOKEN", type: "secret", default: "", required: true, generated: false }],
    web: { route: false, service: "app", port: 8080, url_variable: "" },
  }).template,
  configuration_revision: "config-1",
  warnings: ["Exported values were replaced with required inputs."],
};

function renderForm(respond: (body: { confirm: boolean }) => Promise<unknown>) {
  vi.mocked(request).mockImplementation((_method, _path, body) => respond(body as { confirm: boolean }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <SaveAsTemplate workloadId="w1" workloadName="My App" />
    </QueryClientProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Save as team template" }));
  return client;
}

function fill() {
  fireEvent.change(screen.getByLabelText("Description"), { target: { value: "Our app." } });
  fireEvent.change(screen.getByLabelText(/^Category/), { target: { value: "Applications" } });
  fireEvent.change(screen.getByLabelText(/^Source link/), { target: { value: "https://example.com/src" } });
  fireEvent.change(screen.getByLabelText(/^Licence link/), { target: { value: "https://example.com/license" } });
  fireEvent.change(screen.getByLabelText(/^Documentation link/), { target: { value: "https://example.com/docs" } });
  fireEvent.change(screen.getByLabelText("CPUs"), { target: { value: "0.5" } });
  fireEvent.change(screen.getByLabelText("Memory (MiB)"), { target: { value: "256" } });
  fireEvent.change(screen.getByLabelText("Disk (MiB)"), { target: { value: "0" } });
}

const metadata = {
  schema_version: 1,
  id: "my-app",
  name: "My App",
  description: "Our app.",
  category: "Applications",
  source: "https://example.com/src",
  license: "https://example.com/license",
  docs: "https://example.com/docs",
  minimum: { cpus: 0.5, memory_mb: 256, disk_mb: 0 },
};

it("validates metadata, previews the sanitized template and saves only after confirmation", async () => {
  const client = renderForm((body) => Promise.resolve(body.confirm ? { id: "my-app", revision: "r" } : preview));
  expect(screen.getByLabelText(/^Template ID/)).toHaveValue("my-app");
  fireEvent.click(screen.getByRole("button", { name: "Preview template" }));
  expect(await screen.findByText(/Enter a description/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Preview template" }));
  expect(await screen.findByText("SETTING_1")).toBeInTheDocument();
  expect(screen.getByText(/Exported values were replaced/)).toBeInTheDocument();
  expect(screen.getByText(/No public route is created/)).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls[0]).toEqual(["POST", "/api/workloads/w1/template", { template: metadata, confirm: false }]);
  fireEvent.click(screen.getByRole("button", { name: "Save to team" }));
  expect(vi.mocked(request)).toHaveBeenCalledTimes(1);
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Save template" }));
  expect(await screen.findByText("Template saved")).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls[1]).toEqual(["POST", "/api/workloads/w1/template", { template: metadata, confirm: true, configuration_revision: "config-1" }]);
  expect(client.getMutationCache().getAll().every((mutation) => mutation.options.gcTime === 0)).toBe(true);
});

it("discards the preview when metadata changes", async () => {
  renderForm(() => Promise.resolve(preview));
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Preview template" }));
  expect(await screen.findByRole("button", { name: "Save to team" })).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText(/^Category/), { target: { value: "Tools" } });
  expect(screen.queryByRole("button", { name: "Save to team" })).not.toBeInTheDocument();
  expect(screen.queryByText("SETTING_1")).not.toBeInTheDocument();
});

it("asks for a new preview when the source changed before saving", async () => {
  renderForm((body) => (body.confirm ? Promise.reject(new ApiError(409, "Source configuration changed; preview the template again")) : Promise.resolve(preview)));
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Preview template" }));
  fireEvent.click(await screen.findByRole("button", { name: "Save to team" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Save template" }));
  expect(await screen.findByText("Preview again before saving")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByRole("button", { name: "Save to team" })).not.toBeInTheDocument());
});

it("shows an explicit refusal for unsupported Compose", async () => {
  renderForm(() => Promise.reject(new ApiError(400, "Remove or convert source include before exporting")));
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Preview template" }));
  expect(await screen.findByText("Could not preview the template")).toBeInTheDocument();
  expect(screen.getByText("Remove or convert source include before exporting")).toBeInTheDocument();
});
