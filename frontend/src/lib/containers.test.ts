import { describe, expect, it } from "vitest";
import type { Container } from "../api/types";
import { containerTone } from "./containers";

const container = (state: string, status: string): Container => ({ id: "1", name: "web", image: "nginx", state, status, ports: "" });

describe("containerTone", () => {
  it("treats a clean job exit as success but not as a running service", () => {
    expect(containerTone(container("exited", "Exited (0) 2 minutes ago"), "job")).toBe("ok");
    expect(containerTone(container("exited", "Exited (0) 2 minutes ago"), "service")).toBe("neutral");
  });

  it("flags failures and unhealthy containers", () => {
    expect(containerTone(container("exited", "Exited (1) 1 minute ago"), "job")).toBe("bad");
    expect(containerTone(container("running", "Up 3 minutes (unhealthy)"), "service")).toBe("bad");
    expect(containerTone(container("running", "Up 3 minutes (health: starting)"), "service")).toBe("warn");
    expect(containerTone(container("running", "Up 3 minutes"), "service")).toBe("ok");
  });
});
