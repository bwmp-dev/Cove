import { ApiError } from "../api/client";
import type { FirewallPort, FirewallResult, FirewallRuleInput } from "../api/firewall";
import type { Node } from "../api/types";
import { nodeStates } from "./nodes";

export const maxFirewallRules = 32;
export const maxFirewallSources = 32;

export const portKey = (port: FirewallPort) => `${port.protocol}/${port.port}`;

export function portLabel(port: FirewallPort) {
  return `${port.protocol.toUpperCase()} ${port.port}`;
}

// The API sends null for empty lists from older or unsupported hosts.
export function firewallState(result: FirewallResult) {
  return {
    rules: (result.rules ?? []).map((rule): FirewallRuleInput => ({
      protocol: rule.protocol,
      port: rule.port,
      sources: rule.sources ?? [],
    })),
    available: result.available ?? [],
    reserved: result.reserved ?? [],
  };
}

// Published Cove ports a rule may target: management ports are protected regardless of protocol.
export function selectablePorts(available: FirewallPort[], reserved: number[]) {
  const protectedPorts = new Set(reserved);
  return available.filter((port) => !protectedPorts.has(port.port));
}

export function firewallBlocker(node: Node, online: boolean) {
  if (!node.capabilities) return "Cove has no current probe result for this worker. Verify the node first.";
  if (!node.capabilities.firewall) return `${node.name} has not reported support for firewall management. Upgrade the worker to a Linux build that supports it and verify the node.`;
  if (node.state !== "ready") return `The node is ${nodeStates[node.state]?.label.toLowerCase() ?? node.state}. Firewall management needs a ready worker.`;
  if (!online) return "You are offline. Reconnect to inspect or change the firewall.";
  return undefined;
}

// Removal is offered for any Cove-managed state, including an empty policy or only leftover boot and recovery files.
export function firewallManaged(result: FirewallResult) {
  return result.managed === true || result.active;
}

export function removalUnavailable(node: Node, result: FirewallResult) {
  if (result.remove_supported === undefined) return "Stopping Cove's firewall management needs an upgraded worker. Upgrade the worker, verify the node and inspect again.";
  if (!result.remove_supported) return "Stopping Cove's firewall management is unavailable on this host.";
  if (!node.capabilities?.firewall_remove) return `${node.name} has not reported support for stopping firewall management. Verify the node and inspect again.`;
  return undefined;
}

// 400, 403 and 404 are refused before the worker is contacted. Anything else, including 409, can follow a change the host already applied or reverted.
export function definiteRefusal(error: unknown) {
  return error instanceof ApiError && (error.status === 400 || error.status === 403 || error.status === 404);
}

export interface RuleDraft {
  id: number;
  port: string;
  sources: string;
}

export interface DraftErrors {
  port?: string;
  sources?: string;
}

const ipv4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}\/(3[0-2]|[12]?\d)$/;
const ipv6 = /^[0-9a-f:.]*:[0-9a-f:.]*\/(12[0-8]|1[01]\d|[1-9]?\d)$/i;

export function parseSources(text: string) {
  return text.split(/[\s,]+/).filter(Boolean);
}

// A light syntax check; the worker canonicalizes and is authoritative.
export function validPrefix(value: string) {
  return value.length <= 64 && (ipv4.test(value) || ipv6.test(value));
}

export function draftsFromRules(rules: FirewallRuleInput[]): RuleDraft[] {
  return rules.map((rule, index) => ({
    id: index,
    port: portKey(rule),
    sources: rule.sources.join("\n"),
  }));
}

export function validateDrafts(drafts: RuleDraft[], selectable: FirewallPort[]) {
  const ports = new Map(selectable.map((port) => [portKey(port), port]));
  const errors = new Map<number, DraftErrors>();
  const seen = new Set<string>();
  const rules: FirewallRuleInput[] = [];
  for (const draft of drafts) {
    const error: DraftErrors = {};
    const port = ports.get(draft.port);
    if (!port) error.port = draft.port ? "This port is no longer a published Cove application port." : "Choose a port.";
    else if (seen.has(draft.port)) error.port = "Each port can have only one rule.";
    seen.add(draft.port);
    const sources = parseSources(draft.sources);
    const invalid = sources.filter((source) => !validPrefix(source));
    if (invalid.length > 0) error.sources = `Not an IPv4 or IPv6 CIDR prefix: ${invalid.slice(0, 3).join(", ")}${invalid.length > 3 ? ` and ${invalid.length - 3} more` : ""}.`;
    else if (sources.length > maxFirewallSources) error.sources = `At most ${maxFirewallSources} sources per rule.`;
    if (error.port || error.sources) errors.set(draft.id, error);
    else if (port) rules.push({ protocol: port.protocol, port: port.port, sources });
  }
  const formError = drafts.length > maxFirewallRules ? `At most ${maxFirewallRules} rules.` : undefined;
  return {
    rules: errors.size === 0 && !formError ? rules : undefined,
    errors,
    formError,
  };
}

// Ports whose Cove rule disappears: after the change Cove no longer restricts them at all.
export function removedPorts(current: FirewallRuleInput[], desired: FirewallRuleInput[]) {
  const kept = new Set(desired.map(portKey));
  return current.filter((rule) => !kept.has(portKey(rule)));
}
