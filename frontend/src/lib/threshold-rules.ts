import type { Notification, ThresholdMetric, ThresholdRuleInput } from "../api/connections";

export type ThresholdScope = "node" | "resource";
export type ThresholdUnit = "percent" | "days" | "events" | "failures";

interface ThresholdMetricOption { value: ThresholdMetric; label: string; description: string; scope: ThresholdScope; unit: ThresholdUnit }

export const defaultThresholdMetric: ThresholdMetricOption = { value: "cpu", label: "CPU usage", description: "Processor utilisation of the node.", scope: "node", unit: "percent" };

export const thresholdMetrics: ThresholdMetricOption[] = [
  defaultThresholdMetric,
  { value: "memory", label: "Memory used", description: "Memory in use: total memory minus the memory available to new processes.", scope: "node", unit: "percent" },
  { value: "disk", label: "Disk used", description: "Used space on the filesystem that holds /var/lib/cove, on the fullest mount, or on one mount you choose.", scope: "node", unit: "percent" },
  { value: "certificate", label: "Certificate expiry", description: "Days until the managed ACME certificate stored by the node's proxy for one of the service's configured domains expires. The live certificate served to visitors is not probed.", scope: "resource", unit: "days" },
  { value: "uptime", label: "Uptime check failures", description: "Consecutive failed HTTPS checks from the Cove panel to one of the service's domains. Only domains with an enabled uptime check can be watched.", scope: "resource", unit: "failures" },
  { value: "restarts", label: "Container restarts", description: "Automatic restarts by Docker's restart policy in the last ten minutes.", scope: "resource", unit: "events" },
  { value: "oom", label: "Out-of-memory kills", description: "Out-of-memory events in the last ten minutes, or at least one container currently stopped by one.", scope: "resource", unit: "events" },
  { value: "crashes", label: "Container crashes", description: "Known unexpected container exits in the last ten minutes, or at least one container currently stopped by a known crash. Manual stops and completed one-shot containers are excluded, and exits with an unknown cause are skipped.", scope: "resource", unit: "events" },
];

export const thresholdUnits: Record<ThresholdUnit, { label: string; hint: string; min: number; max: number; step: string; message: string }> = {
  percent: { label: "Threshold (%)", hint: "Alert at or above this percentage, above 0 and at most 100.", min: 0.1, max: 100, step: "any", message: "Enter a percentage above 0 and at most 100." },
  days: { label: "Threshold (days)", hint: "Alert when the remaining days are at or below this value, above 0 and at most 3650.", min: 0.1, max: 3650, step: "any", message: "Enter a number of days above 0 and at most 3650." },
  events: { label: "Threshold (events)", hint: "Alert at or above this many in ten minutes, a whole number from 1 to 1000.", min: 1, max: 1000, step: "1", message: "Enter a whole number from 1 to 1000." },
  failures: { label: "Threshold (consecutive failures)", hint: "Alert after this many failed checks in a row, a whole number from 1 to 1000.", min: 1, max: 1000, step: "1", message: "Enter a whole number from 1 to 1000." },
};

export function thresholdMetric(metric: string) {
  return thresholdMetrics.find((option) => option.value === metric);
}

export function metricLabel(metric: string) {
  return thresholdMetric(metric)?.label ?? metric;
}

export function metricScope(metric: string): ThresholdScope {
  return thresholdMetric(metric)?.scope ?? "node";
}

export type MountScope = "data" | "any" | "path";

export const mountScopes: { value: MountScope; label: string; hint: string }[] = [
  { value: "data", label: "Cove data filesystem", hint: "Only the filesystem that holds /var/lib/cove." },
  { value: "any", label: "Fullest mount", hint: "The highest utilisation across every mount the worker measures. Unreadable or clipped mounts never count as recovery." },
  { value: "path", label: "One mount", hint: "A single mount point reported by the worker. While it is missing or unreadable the alert neither triggers nor clears." },
];

export function mountScope(path: string): MountScope {
  return path === "" ? "data" : path === "*" ? "any" : "path";
}

export function diskTargetLabel(path: string) {
  if (path === "") return "Data disk used";
  if (path === "*") return "Fullest mount used";
  return `Disk used on ${path}`;
}

export const maxMountPathBytes = 4096;

export const confirmationChoices = [60, 120, 300, 600, 900, 1800, 3600];
export const repeatChoices = [300, 900, 1800, 3600, 7200, 21600, 43200, 86400, 604800];
export const maxRuleChannels = 32;

export function formatDuration(seconds: number) {
  const units: [number, string][] = [[86400, "day"], [3600, "hour"], [60, "minute"]];
  for (const [size, unit] of units) {
    if (seconds >= size && seconds % size === 0) {
      const count = seconds / size;
      return `${count} ${unit}${count === 1 ? "" : "s"}`;
    }
  }
  return `${seconds} seconds`;
}

export function formatPercent(value: number) {
  return `${Number.isInteger(value) ? value : value.toFixed(1)}%`;
}

export function formatDays(value: number) {
  return `${Number.isInteger(value) ? value : value.toFixed(1)} ${value === 1 ? "day" : "days"}`;
}

export function thresholdCondition(rule: Pick<ThresholdRuleInput, "metric" | "threshold" | "domain" | "confirmation_seconds"> & { mount_path?: string }) {
  const lasting = `for ${formatDuration(rule.confirmation_seconds)}`;
  const label = rule.metric === "disk" ? diskTargetLabel(rule.mount_path ?? "") : metricLabel(rule.metric);
  switch (thresholdMetric(rule.metric)?.unit) {
    case "days":
      return `${label} for ${rule.domain} at or below ${formatDays(rule.threshold)} ${lasting}`;
    case "events":
      return `${label} at or above ${rule.threshold} in ten minutes ${lasting}`;
    case "failures":
      return `${rule.domain} failing ${rule.threshold} uptime ${rule.threshold === 1 ? "check" : "checks"} in a row ${lasting}`;
    default:
      return `${label} at or above ${formatPercent(rule.threshold)} ${lasting}`;
  }
}

export type ThresholdFieldName = "name" | "node_id" | "workload_id" | "domain" | "mount_path" | "metric" | "threshold" | "confirmation_seconds" | "repeat_seconds" | "channel_ids";
export interface ThresholdFormError {
  field: ThresholdFieldName;
  message: string;
}

export function isThresholdError(value: ThresholdRuleInput | ThresholdFormError): value is ThresholdFormError {
  return "message" in value;
}

export function thresholdRuleFromForm(data: FormData): ThresholdRuleInput | ThresholdFormError {
  const text = (key: string) => {
    const value = data.get(key);
    return typeof value === "string" ? value.trim() : "";
  };
  const name = text("name");
  if (name === "" || name.length > 120) return { field: "name", message: "Enter a name of at most 120 characters." };
  const metric = thresholdMetric(text("metric"));
  if (!metric) return { field: "metric", message: "Select a metric." };
  let nodeID = "";
  let workloadID = "";
  let domain = "";
  let mountPath = "";
  if (metric.scope === "node") {
    nodeID = text("node_id");
    if (nodeID === "") return { field: "node_id", message: "Select a node." };
    if (metric.value === "disk") {
      const scope = text("mount_scope");
      if (scope === "any") mountPath = "*";
      if (scope === "path") {
        mountPath = text("mount_path");
        if (!mountPath.startsWith("/") || new TextEncoder().encode(mountPath).length > maxMountPathBytes) return { field: "mount_path", message: "Select a mount." };
      }
    }
  } else {
    workloadID = text("workload_id");
    if (workloadID === "") return { field: "workload_id", message: "Select a service." };
    if (metric.unit === "days" || metric.unit === "failures") {
      domain = text("domain");
      if (domain === "") return { field: "domain", message: "Select a domain." };
    }
  }
  const unit = thresholdUnits[metric.unit];
  const threshold = text("threshold") === "" ? Number.NaN : Number(text("threshold"));
  if (!Number.isFinite(threshold) || threshold <= 0 || threshold > unit.max || (unit.step === "1" && !Number.isInteger(threshold))) return { field: "threshold", message: unit.message };
  const confirmation = Number(text("confirmation_seconds"));
  if (!Number.isInteger(confirmation) || confirmation < 60 || confirmation > 3600) return { field: "confirmation_seconds", message: "Select how long the condition must last." };
  const repeat = Number(text("repeat_seconds"));
  if (!Number.isInteger(repeat) || repeat < 300 || repeat > 604800) return { field: "repeat_seconds", message: "Select how often the alert repeats." };
  const channels = [...new Set(data.getAll("channel_ids").filter((value): value is string => typeof value === "string" && value !== ""))];
  if (channels.length === 0) return { field: "channel_ids", message: "Select at least one channel." };
  if (channels.length > maxRuleChannels) return { field: "channel_ids", message: `Select at most ${maxRuleChannels} channels.` };
  return { name, node_id: nodeID, workload_id: workloadID, domain, mount_path: mountPath, metric: metric.value, threshold, confirmation_seconds: confirmation, repeat_seconds: repeat, channel_ids: channels };
}

// A rule only restricts which channels may receive its events; each channel's own switches and
// event selection still decide whether it sends them.
// Resource owner emails reach the owner of the resource a rule watches, so they only suit resource rules.
export function thresholdDeliveryProblem(notification: Notification | undefined, scope: ThresholdScope) {
  if (!notification) return "Not a notification channel.";
  if (!notification.enabled) return "Notifications are disabled on this channel.";
  if (scope === "node" && notification.provider === "email" && notification.owner) return "Resource owner emails skip node events, so this channel never sends threshold alerts.";
  const events = notification.events ?? [];
  if (events.length > 0) {
    const alert = events.includes("threshold.alert");
    const recovered = events.includes("threshold.recovered");
    if (!alert && !recovered) return "This channel does not select threshold events.";
    if (!alert) return "This channel sends threshold recovery notices but not alerts.";
    if (!recovered) return "This channel sends threshold alerts but not recovery notices.";
    return undefined;
  }
  return notification.health ? undefined : "This channel does not include health alerts.";
}
