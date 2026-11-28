import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { WorkloadInput } from "../api/types";
import type { ProviderLink } from "../components/WorkloadForm";
import { NewWorkloadPage } from "./workload-new";

const navigate = vi.fn();

// The form is replaced so the test controls the exact input submitted; it covers the page's create and provider-link wiring.
const fixture = vi.hoisted(() => ({
  input: {
    name: "web",
    node_id: "n1",
    kind: "service",
    compose: "services:\n  web:\n    image: nginx\n",
    source: { repository: "https://git.example.com/team/web.git", branch: "main", compose_path: "compose.yaml", token: "source-token-secret" },
    routes: [
      {
        domain: "app.example.com",
        service: "web",
        port: 8080,
        policy: { request_headers: { "X-Token": "typed-header-secret" }, forward_auth: { service: "auth", port: 9091, url: "https://forward-auth-secret.example.com/verify" } },
      },
    ],
  } satisfies WorkloadInput,
  provider: { connection_id: 4, repository_id: 7, automatic: true, reporting: false },
}));

vi.mock("@tanstack/react-router", () => ({
  getRouteApi: () => ({ useSearch: () => ({}) }),
  Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>,
  useNavigate: () => navigate,
}));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../components/TemplateBrowser", () => ({
  TemplateBrowser: ({ onCustom }: { onCustom: () => void }) => (
    <button type="button" onClick={onCustom}>
      Custom resource
    </button>
  ),
}));
vi.mock("../components/WorkloadForm", () => ({
  WorkloadForm: ({ pending, serverError, onSubmit }: { pending: boolean; serverError: string | null; onSubmit: (input: WorkloadInput, provider?: ProviderLink) => void }) => (
    <div>
      {serverError && <p>{serverError}</p>}
      <button type="button" disabled={pending} onClick={() => onSubmit(structuredClone(fixture.input), fixture.provider)}>
        Create resource
      </button>
    </div>
  ),
}));

afterEach(() => vi.clearAllMocks());

const secrets = ["source-token-secret", "typed-header-secret", "forward-auth-secret"];
const creates = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "POST" && path === "/api/workloads");
const links = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w9/git-provider");

function expectNoSecretsInMutationCache(client: QueryClient) {
  const mutations = client.getMutationCache().getAll();
  expect(mutations.length).toBeGreaterThan(0);
  const state = JSON.stringify(mutations.map((mutation) => ({ variables: mutation.state.variables, data: mutation.state.data })));
  for (const secret of secrets) expect(state).not.toContain(secret);
}

it("keeps routes and the source token out of the mutation cache while creating, after a rejection and on retry", async () => {
  let rejectFirst: (error: unknown) => void = () => {};
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/nodes" || path === "/api/projects") return Promise.resolve([]);
    if (method === "POST" && path === "/api/workloads") {
      if (creates().length === 1) return new Promise((_, reject) => (rejectFirst = reject));
      return Promise.resolve({ id: "w9" });
    }
    if (method === "PUT" && path === "/api/workloads/w9/git-provider") return Promise.reject(new ApiError(502, "Provider unavailable"));
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const user = userEvent.setup();
  render(
    <QueryClientProvider client={client}>
      <NewWorkloadPage />
    </QueryClientProvider>,
  );

  await user.click(await screen.findByRole("button", { name: "Custom resource" }));
  await user.click(screen.getByRole("button", { name: "Create resource" }));
  await waitFor(() => expect(creates()).toHaveLength(1));
  expect(creates()[0]?.[2]).toEqual(fixture.input);
  expect(screen.getByRole("button", { name: "Create resource" })).toBeDisabled();
  expectNoSecretsInMutationCache(client);

  rejectFirst(new ApiError(400, "Compose rejected"));
  expect(await screen.findByText("Compose rejected")).toBeInTheDocument();
  expectNoSecretsInMutationCache(client);

  // The resource exists after the retry, so a failed provider link is reported instead of navigating away.
  await user.click(screen.getByRole("button", { name: "Create resource" }));
  expect(await screen.findByText("Resource created, but the Git provider was not connected")).toBeInTheDocument();
  expect(creates()).toHaveLength(2);
  expect(creates()[1]?.[2]).toEqual(fixture.input);
  expect(links()).toHaveLength(1);
  expect(links()[0]?.[2]).toEqual({ ...fixture.provider, branch: "main", compose_path: "compose.yaml" });
  expect(navigate).not.toHaveBeenCalled();
  expectNoSecretsInMutationCache(client);
});
