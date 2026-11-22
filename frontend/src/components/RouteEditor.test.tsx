import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { routeRows, validateRoutes, type RouteRow } from "../lib/workload-form";
import { RouteEditor } from "./RouteEditor";

let latest: RouteRow[] = [];

function Harness({ policies, initial }: { policies?: boolean; initial: RouteRow[] }) {
  const [rows, setRows] = useState(initial);
  return (
    <RouteEditor
      routes={rows}
      policies={policies}
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
});
