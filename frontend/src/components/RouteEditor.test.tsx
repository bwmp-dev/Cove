import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Route } from "../api/types";
import { routeRows, validateRoutes, type RouteRow } from "../lib/workload-form";
import { RouteEditor } from "./RouteEditor";

let latest: RouteRow[] = [];

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => vi.unstubAllGlobals());

function Harness({ policies, extensions, saved, initial }: { policies?: boolean; extensions?: boolean; saved?: Route[]; initial: RouteRow[] }) {
  const [rows, setRows] = useState(initial);
  return (
    <RouteEditor
      routes={rows}
      policies={policies}
      extensions={extensions}
      saved={saved}
      onChange={(next) => {
        latest = next;
        setRows(next);
      }}
    />
  );
}

const plain = () => routeRows([{ domain: "app.example.com", service: "web", port: 80 }]);

describe("RouteEditor", () => {
  it("explains the worker upgrade instead of offering policies the node cannot apply", () => {
    render(<Harness policies={false} initial={plain()} />);
    expect(screen.getByText("Advanced routing needs a newer worker")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Advanced routing/ })).not.toBeInTheDocument();
  });

  it("edits headers, upstreams and allowed ranges into the saved policy", async () => {
    render(<Harness policies initial={plain()} />);
    const toggle = screen.getByRole("button", { name: /Advanced routing/ });
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await userEvent.type(screen.getByLabelText(/Path prefix/), "/api");
    await userEvent.click(screen.getByRole("button", { name: "Add request header" }));
    await userEvent.type(screen.getByLabelText("Name"), "X-App");
    await userEvent.type(screen.getByLabelText("Value"), "api");
    await userEvent.type(screen.getByLabelText(/Allowed source IP ranges/), "10.0.0.0/8");
    await userEvent.click(screen.getByRole("button", { name: "Add upstream" }));
    const upstreams = screen.getByRole("list", { name: "Additional upstreams" });
    await userEvent.type(upstreams.querySelector("input") as HTMLInputElement, "api");
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(validateRoutes(latest)).toEqual({
      routes: [
        {
          domain: "app.example.com",
          service: "web",
          port: 80,
          policy: { path_prefix: "/api", request_headers: { "X-App": "api" }, ip_allow_list: ["10.0.0.0/8"], upstreams: [{ service: "api", port: 80 }], sticky: true },
        },
      ],
      errors: {},
    });
    await userEvent.click(screen.getByRole("button", { name: "Remove request header X-App" }));
    expect(validateRoutes(latest).routes?.[0]?.policy?.request_headers).toBeUndefined();
  });

  const protectedRoute: Route = {
    domain: "app.example.com",
    service: "web",
    port: 80,
    policy: { maintenance: { enabled: true, html: "<script>alert(1)</script><h1>Back soon</h1>" }, error_pages: { "502": "<h1>Unavailable</h1>" }, forward_auth: { url: "https://auth.example.com/verify", response_headers: ["Remote-User"] } },
  };

  it("edits maintenance, error pages and owned-service forward-auth with the keyboard-reachable controls", async () => {
    render(<Harness policies extensions initial={plain()} />);
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    await userEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    await userEvent.type(screen.getByLabelText(/Maintenance page/), "<h1>Updating</h1>");
    await userEvent.click(screen.getByRole("button", { name: "Add error page" }));
    await userEvent.type(screen.getByLabelText("Status"), "502");
    await userEvent.type(screen.getByLabelText("HTML for status 502"), "<h1>Down</h1>");
    await userEvent.click(screen.getByRole("combobox", { name: "Provider" }));
    await userEvent.click(screen.getByRole("option", { name: "Service in this resource" }));
    const auth = screen.getByRole("group", { name: "Forward-auth" });
    expect(within(auth).getByText(/Do not put tokens, passwords or other secrets/)).toBeInTheDocument();
    await userEvent.type(within(auth).getByLabelText("Compose service"), "auth");
    await userEvent.clear(within(auth).getByLabelText("Container port"));
    await userEvent.type(within(auth).getByLabelText("Container port"), "9091");
    await userEvent.type(within(auth).getByLabelText(/Path/), "/api/verify");
    await userEvent.type(screen.getByLabelText(/Identity headers/), "Remote-User");
    expect(validateRoutes(latest).routes?.[0]?.policy).toEqual({
      maintenance: { enabled: true, html: "<h1>Updating</h1>" },
      error_pages: { "502": "<h1>Down</h1>" },
      forward_auth: { service: "auth", port: 9091, path: "/api/verify", response_headers: ["Remote-User"] },
    });
  });

  it("shows supplied HTML as text only", async () => {
    const { container } = render(<Harness policies extensions saved={[protectedRoute]} initial={routeRows([protectedRoute])} />);
    expect(screen.getByRole("button", { name: /maintenance on, forward-auth, 1 error page/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.getByLabelText(/Maintenance page/)).toHaveValue("<script>alert(1)</script><h1>Back soon</h1>");
    expect(container.querySelector("script")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Back soon" })).not.toBeInTheDocument();
  });

  it("warns before saving removes forward-auth and sends an explicit null", async () => {
    render(<Harness policies extensions saved={[protectedRoute]} initial={routeRows([protectedRoute])} />);
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.queryByText("Saving removes forward-auth")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("combobox", { name: "Provider" }));
    await userEvent.click(screen.getByRole("option", { name: "Off" }));
    expect(screen.getByText("Saving removes forward-auth")).toBeInTheDocument();
    expect(validateRoutes(latest, [protectedRoute]).routes?.[0]?.policy?.forward_auth).toBeNull();
  });

  it("keeps saved settings read-only on an older worker and lets them be removed", async () => {
    render(<Harness policies extensions={false} saved={[protectedRoute]} initial={routeRows([protectedRoute])} />);
    expect(screen.getByText("Maintenance, error pages and forward-auth need a newer worker")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.getByRole("switch", { name: "Maintenance mode" })).toBeDisabled();
    expect(screen.getByLabelText(/Maintenance page/)).toHaveAttribute("readonly");
    expect(screen.queryByRole("button", { name: "Add error page" })).not.toBeInTheDocument();
    // Prior route controls stay editable.
    expect(screen.getByLabelText(/Path prefix/)).not.toHaveAttribute("readonly");
    await userEvent.click(screen.getByRole("button", { name: "Remove forward-auth" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove maintenance settings" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove error page 502" }));
    expect(screen.queryByRole("group", { name: "Maintenance" })).not.toBeInTheDocument();
    expect(validateRoutes(latest, [protectedRoute]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 80, policy: { maintenance: null, error_pages: null, forward_auth: null } }]);
  });

  it("hides the new settings on an older worker when none are saved", async () => {
    render(<Harness policies extensions={false} initial={plain()} />);
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.getByLabelText(/Path prefix/)).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Forward-auth" })).not.toBeInTheDocument();
  });
});
