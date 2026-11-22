import type { Notification, ThresholdMetric, ThresholdRuleInput } from "../api/connections";

export const thresholdMetrics: { value: ThresholdMetric; label: string; description: string }[] = [
  { value: "cpu", label: "CPU usage", description: "Processor utilisation of the node." },
  { value: "memory", label: "Memory used", description: "Memory in use: total memory minus the memory available to new processes." },
  { value: "disk", label: "Data disk used", description: "Only the filesystem that holds /var/lib/cove, not every mounted disk." },
];

export function metricLabel(metric: string) {
  return thresholdMetrics.find((option) => option.value === metric)?.label ?? metric;
}

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

export type ThresholdFieldName = "name" | "node_id" | "metric" | "threshold" | "confirmation_seconds" | "repeat_seconds" | "channel_ids";
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
  const nodeID = text("node_id");
  if (nodeID === "") return { field: "node_id", message: "Select a node." };
  const metric = thresholdMetrics.find((option) => option.value === text("metric"))?.value;
  if (!metric) return { field: "metric", message: "Select a metric." };
  const threshold = text("threshold") === "" ? Number.NaN : Number(text("threshold"));
  if (!Number.isFinite(threshold) || threshold <= 0 || threshold > 100) return { field: "threshold", message: "Enter a percentage above 0 and at most 100." };
  const confirmation = Number(text("confirmation_seconds"));
  if (!Number.isInteger(confirmation) || confirmation < 60 || confirmation > 3600) return { field: "confirmation_seconds", message: "Select how long the condition must last." };
  const repeat = Number(text("repeat_seconds"));
  if (!Number.isInteger(repeat) || repeat < 300 || repeat > 604800) return { field: "repeat_seconds", message: "Select how often the alert repeats." };
  const channels = [...new Set(data.getAll("channel_ids").filter((value): value is string => typeof value === "string" && value !== ""))];
  if (channels.length === 0) return { field: "channel_ids", message: "Select at least one channel." };
  if (channels.length > maxRuleChannels) return { field: "channel_ids", message: `Select at most ${maxRuleChannels} channels.` };
  return { name, node_id: nodeID, metric, threshold, confirmation_seconds: confirmation, repeat_seconds: repeat, channel_ids: channels };
}

// A rule only restricts which channels may receive its events; each channel's own switches and
// event selection still decide whether it sends them.
export function thresholdDeliveryProblem(notification: Notification | undefined) {
  if (!notification) return "Not a notification channel.";
  if (!notification.enabled) return "Notifications are disabled on this channel.";
  if (notification.provider === "email" && notification.owner) return "Resource owner emails skip node events, so this channel never sends threshold alerts.";
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
