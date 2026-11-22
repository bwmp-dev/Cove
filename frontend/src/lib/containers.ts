import type { Container } from "../api/types";

export type ContainerTone = "ok" | "warn" | "bad" | "neutral";

export interface ContainerStatus {
  tone: ContainerTone;
  label: string;
}

// Workers that predate the typed fields only report Docker's display status, such as "Exited (1) 2 minutes ago" or "Up 3 minutes (unhealthy)".
function legacyHealth(status: string): Container["health"] {
  if (status.includes("(unhealthy)")) return "unhealthy";
  if (status.includes("health: starting")) return "starting";
  if (status.includes("(healthy)")) return "healthy";
  return undefined;
}

function legacyExitCode(status: string): number | undefined {
  const code = /exited \((-?\d+)\)/.exec(status)?.[1];
  return code === undefined ? undefined : Number(code);
}

export function containerStatus(container: Container, kind: "service" | "job"): ContainerStatus {
  const status = container.status.toLowerCase();
  switch (container.state) {
    case "running":
      switch (container.health ?? legacyHealth(status)) {
        case "unhealthy":
          return { tone: "bad", label: "Unhealthy" };
        case "starting":
          return { tone: "warn", label: "Health check starting" };
        case "healthy":
          return { tone: "ok", label: "Healthy" };
        default:
          return { tone: "ok", label: "Running" };
      }
    case "restarting":
      return { tone: "warn", label: "Restarting" };
    case "dead":
      return { tone: "bad", label: "Dead" };
    case "exited": {
      const code = container.exit_code ?? legacyExitCode(status);
      if (code === 0) {
        return kind === "job" || container.expected_completed ? { tone: "ok", label: "Completed" } : { tone: "neutral", label: "Exited (0)" };
      }
      return { tone: "bad", label: code === undefined ? "Exited" : `Failed (exit ${code})` };
    }
    default:
      return { tone: "neutral", label: container.state ? container.state.charAt(0).toUpperCase() + container.state.slice(1) : "Unknown" };
  }
}
