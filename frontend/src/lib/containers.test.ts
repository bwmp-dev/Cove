import { describe, expect, it } from "vitest";
import type { Container } from "../api/types";
import { containerStatus } from "./containers";

const container = (state: string, status: string, typed: Partial<Container> = {}): Container => ({ id: "1", name: "web", image: "nginx", state, status, ports: "", ...typed });

describe("containerStatus with typed worker fields", () => {
  it("labels Docker health without inventing it for containers that have no healthcheck", () => {
    expect(containerStatus(container("running", "Up 3 minutes (healthy)", { health: "healthy" }), "service")).toEqual({ tone: "ok", label: "Healthy" });
    expect(containerStatus(container("running", "Up 3 minutes (unhealthy)", { health: "unhealthy" }), "service")).toEqual({ tone: "bad", label: "Unhealthy" });
    expect(containerStatus(container("running", "Up 3 seconds (health: starting)", { health: "starting" }), "service")).toEqual({ tone: "warn", label: "Health check starting" });
    expect(containerStatus(container("running", "Up 3 minutes"), "service")).toEqual({ tone: "ok", label: "Running" });
  });

  it("prefers typed health over the display status", () => {
    expect(containerStatus(container("running", "Up 3 minutes (healthy)", { health: "unhealthy" }), "service").label).toBe("Unhealthy");
  });

  it("treats clean job exits and expected init completions as success", () => {
    expect(containerStatus(container("exited", "Exited (0) 2 minutes ago", { exit_code: 0 }), "job")).toEqual({ tone: "ok", label: "Completed" });
    expect(containerStatus(container("exited", "Exited (0) 2 minutes ago", { exit_code: 0, service: "migrate", expected_completed: true }), "service")).toEqual({ tone: "ok", label: "Completed" });
    expect(containerStatus(container("exited", "Exited (0) 2 minutes ago", { exit_code: 0 }), "service")).toEqual({ tone: "neutral", label: "Exited (0)" });
  });

  it("reports nonzero exits as failures with the exit code", () => {
    expect(containerStatus(container("exited", "Exited (137) 1 minute ago", { exit_code: 137 }), "job")).toEqual({ tone: "bad", label: "Failed (exit 137)" });
    expect(containerStatus(container("dead", "Dead", { exit_code: 1 }), "service")).toEqual({ tone: "bad", label: "Dead" });
  });
});

describe("containerStatus with older workers", () => {
  it("falls back to the raw Docker status", () => {
    expect(containerStatus(container("exited", "Exited (0) 2 minutes ago"), "job").tone).toBe("ok");
    expect(containerStatus(container("exited", "Exited (0) 2 minutes ago"), "service").tone).toBe("neutral");
    expect(containerStatus(container("exited", "Exited (1) 1 minute ago"), "job")).toEqual({ tone: "bad", label: "Failed (exit 1)" });
    expect(containerStatus(container("running", "Up 3 minutes (unhealthy)"), "service").tone).toBe("bad");
    expect(containerStatus(container("running", "Up 3 minutes (health: starting)"), "service").tone).toBe("warn");
    expect(containerStatus(container("restarting", "Restarting (1) 5 seconds ago"), "service")).toEqual({ tone: "warn", label: "Restarting" });
  });
});
