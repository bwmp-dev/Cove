import type { Container } from "../api/types";

export type ContainerTone = "ok" | "warn" | "bad" | "neutral";

export function containerTone(container: Container, kind: "service" | "job"): ContainerTone {
  const status = container.status.toLowerCase();
  switch (container.state) {
    case "running":
      if (status.includes("(unhealthy)")) return "bad";
      if (status.includes("health: starting")) return "warn";
      return "ok";
    case "restarting":
      return "warn";
    case "dead":
      return "bad";
    case "exited": {
      const code = /exited \((\d+)\)/.exec(status)?.[1];
      if (code === "0") return kind === "job" ? "ok" : "neutral";
      return "bad";
    }
    default:
      return "neutral";
  }
}
