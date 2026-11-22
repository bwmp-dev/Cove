import type { Node, NodeInput, NodeState } from "../api/types";

export type Tone = "ok" | "warn" | "bad" | "info" | "neutral";

export const nodeStates: Record<NodeState, { tone: Tone; label: string; description: string }> = {
  pending: { tone: "neutral", label: "Awaiting enrollment", description: "The setup command has not registered this worker yet." },
  enrolled: { tone: "info", label: "Enrolled", description: "Registered; SSH verification has not succeeded yet." },
  unreachable: { tone: "bad", label: "Unreachable", description: "The main node could not complete an SSH connection with the pinned host key." },
  ssh_verified: { tone: "warn", label: "Not ready", description: "SSH works, but Docker or Compose is not available to the worker." },
  ready: { tone: "ok", label: "Ready", description: "SSH verified and Docker Compose is available." },
};

// SSH verification needs the host key captured during registration.
export function canVerify(node: Node) {
  return node.mode === "local" || node.state !== "pending";
}

export function nodeAddress(node: Node) {
  if (node.mode === "local") return "Local socket";
  // Connection details are only sent to instance administrators.
  if (node.address === "") return "SSH worker";
  const host = node.address.includes(":") ? `[${node.address}]` : node.address;
  return `${host}:${node.port}`;
}

export function platform(node: Node) {
  const capabilities = node.capabilities;
  if (!capabilities?.os) return null;
  return capabilities.arch ? `${capabilities.os}/${capabilities.arch}` : capabilities.os;
}

export type NodeErrors = Partial<Record<keyof NodeInput, string>>;

export function validateNode(values: { name: string; address: string; port: string }): { input?: NodeInput; errors: NodeErrors } {
  const errors: NodeErrors = {};
  const name = values.name.trim();
  const address = values.address.trim();
  const port = Number(values.port);
  if (name === "") errors.name = "Enter a name.";
  else if (name.length > 120) errors.name = "Use at most 120 characters.";
  if (address === "") errors.address = "Enter the hostname or IP address the main node uses for SSH.";
  else if (address.length > 253 || /[\s/\\"']/.test(address)) errors.address = "Enter a hostname or IP address without spaces, slashes, or quotes.";
  if (!Number.isInteger(port) || port < 1 || port > 65535) errors.port = "Enter a port from 1 to 65535.";
  if (Object.keys(errors).length > 0) return { errors };
  return { input: { name, address, port }, errors };
}
