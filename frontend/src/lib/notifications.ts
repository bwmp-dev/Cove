import type { Notification } from "../api/connections";

interface DestinationField {
  label: string;
  hint: string;
}

export interface ProviderSpec {
  value: string;
  label: string;
  // Omitted for providers with a fixed endpoint (Telegram, Pushover, PagerDuty) and for email.
  url?: DestinationField & { secret: boolean; baseURL: boolean };
  token?: DestinationField & { required: boolean };
  target?: DestinationField;
  userKey?: DestinationField;
  // Incident providers open and resolve one incident per health condition, so the API only accepts
  // matching alert and recovery event pairs for them.
  incident?: boolean;
}

const webhookHint = "An HTTPS endpoint reachable at a public address. The URL is stored encrypted.";
const privateServers = "Only public HTTPS addresses are supported; private network servers and redirects are refused.";

export const notificationProviders: ProviderSpec[] = [
  { value: "webhook", label: "JSON webhook", url: { label: "Webhook URL", hint: webhookHint, secret: true, baseURL: false } },
  { value: "discord", label: "Discord", url: { label: "Webhook URL", hint: webhookHint, secret: true, baseURL: false } },
  { value: "slack", label: "Slack", url: { label: "Webhook URL", hint: webhookHint, secret: true, baseURL: false } },
  {
    value: "teams",
    label: "Microsoft Teams",
    url: { label: "Workflow webhook URL", hint: "The HTTPS URL of a Teams workflow that posts webhook requests to a channel. Messages are sent as Adaptive Cards. The URL is stored encrypted.", secret: true, baseURL: false },
  },
  { value: "email", label: "Email" },
  {
    value: "telegram",
    label: "Telegram",
    token: { label: "Bot token", required: true, hint: "The token BotFather gave your bot. Messages are sent through api.telegram.org." },
    target: { label: "Chat ID", hint: "The chat, group or channel ID, or @channelname for a public channel. The bot must be a member." },
  },
  {
    value: "ntfy",
    label: "ntfy",
    url: { label: "Server URL", hint: `The base URL of the ntfy server, for example https://ntfy.sh, without a query string. ${privateServers}`, secret: false, baseURL: true },
    target: { label: "Topic", hint: "The topic to publish to. On a public server, anyone who knows the topic can read it." },
    token: { label: "Access token", required: false, hint: "Sent as a bearer token, for servers that protect the topic." },
  },
  {
    value: "gotify",
    label: "Gotify",
    url: { label: "Server URL", hint: `The base URL of the Gotify server, without a query string. Messages are posted to /message. ${privateServers}`, secret: false, baseURL: true },
    token: { label: "Application token", required: true, hint: "The token of the Gotify application that sends the messages." },
  },
  {
    value: "pushover",
    label: "Pushover",
    token: { label: "Application token", required: true, hint: "The API token of your Pushover application. Messages are sent through api.pushover.net." },
    userKey: { label: "User key", hint: "The user or group key that receives the messages." },
  },
  {
    value: "matrix",
    label: "Matrix",
    url: { label: "Homeserver URL", hint: `The base URL of the homeserver, for example https://matrix.example.org, without a query string. ${privateServers}`, secret: false, baseURL: true },
    token: { label: "Access token", required: true, hint: "The access token of the account that posts the messages." },
    target: { label: "Room ID", hint: "For example !abc123:example.org. Messages are plain text, and encrypted rooms are not supported." },
  },
  {
    value: "pagerduty",
    label: "PagerDuty",
    token: { label: "Integration key", required: true, hint: "The Events API v2 integration (routing) key of the PagerDuty service. Events are sent to events.pagerduty.com." },
    incident: true,
  },
  {
    value: "opsgenie",
    label: "Opsgenie",
    url: { label: "API URL", hint: "https://api.opsgenie.com, or https://api.eu.opsgenie.com for accounts in the EU region.", secret: false, baseURL: true },
    token: { label: "API key", required: true, hint: "The key of an Opsgenie API integration that can create and close alerts." },
    incident: true,
  },
];

export function providerSpec(provider: string): ProviderSpec | undefined {
  return notificationProviders.find((option) => option.value === provider);
}

export function providerLabel(provider: string) {
  return providerSpec(provider)?.label ?? provider;
}

const subjects: Record<string, string> = {
  operation: "Other operations",
  backup: "Backups",
  restore: "Restores",
  drill: "Restore drills",
  node_unreachable: "Node unreachable",
  disk_low: "Low disk space",
  resource_unhealthy: "Unhealthy resource",
  backup_overdue: "Backup overdue",
  threshold: "Threshold condition",
  host_key_changed: "SSH host key changed",
};

const outcomes: Record<string, string> = {
  succeeded: "Succeeded",
  failed: "Failed",
  interrupted: "Interrupted",
  cancelled: "Cancelled",
  alert: "Alert",
  recovered: "Recovered",
};

export interface EventGroup {
  key: string;
  title: string;
  description?: string;
  health: boolean;
  events: { value: string; label: string }[];
}

// The catalogue comes from the API; unknown subjects or outcomes keep their raw names so a newer
// backend never hides an event the dashboard does not have a label for.
export function groupNotificationEvents(catalogue: string[]): EventGroup[] {
  const groups = new Map<string, EventGroup>();
  for (const event of catalogue) {
    const dot = event.lastIndexOf(".");
    const subject = dot > 0 ? event.slice(0, dot) : event;
    const outcome = dot > 0 ? event.slice(dot + 1) : "";
    const health = outcome === "alert" || outcome === "recovered";
    const key = health ? "health" : subject;
    let group = groups.get(key);
    if (!group) {
      group = health
        ? { key, title: "Health alerts", description: "Alerts are sent after a failure is confirmed; recovery notices follow when it clears.", health, events: [] }
        : { key, title: subjects[subject] ?? subject, description: subject === "operation" ? "Deployments and other operations that are not backups, restores or drills." : undefined, health, events: [] };
      groups.set(key, group);
    }
    const outcomeLabel = outcomes[outcome] ?? (outcome || event);
    group.events.push({ value: event, label: health ? `${subjects[subject] ?? subject}: ${outcomeLabel.toLowerCase()}` : outcomeLabel });
  }
  return [...groups.values()].sort((a, b) => Number(a.health) - Number(b.health));
}

// Health conditions with both an alert and a recovery event in the catalogue. Incident providers
// select these as pairs.
export function incidentConditions(catalogue: string[]) {
  return catalogue.flatMap((event) => {
    if (!event.endsWith(".alert")) return [];
    const value = event.slice(0, -".alert".length);
    return catalogue.includes(`${value}.recovered`) ? [{ value, label: subjects[value] ?? value }] : [];
  });
}

export function selectedIncidentConditions(events: string[] | undefined) {
  return (events ?? []).filter((event) => event.endsWith(".alert")).map((event) => event.slice(0, -".alert".length));
}

export function routingSummary(notification: Notification) {
  if (notification.events && notification.events.length > 0) {
    if (providerSpec(notification.provider)?.incident) {
      const count = selectedIncidentConditions(notification.events).length;
      return `${count} health ${count === 1 ? "condition" : "conditions"}`;
    }
    return `${notification.events.length} selected ${notification.events.length === 1 ? "event" : "events"}`;
  }
  const parts = [notification.success && "successful operations", notification.failure && "failed operations", notification.health && "health alerts"].filter((part) => part !== false);
  if (parts.length === 0) return "No events";
  const text = parts.join(", ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export type RoutingMode = "groups" | "events";
export type EmailRecipient = "fixed" | "owner";

export type NotificationFieldName = "url" | "token" | "target" | "user_key" | "events";
export interface NotificationFormError {
  field: NotificationFieldName;
  message: string;
}

export function isFormError(value: Notification | NotificationFormError): value is NotificationFormError {
  return "message" in value;
}

const lineBreak = /[\r\n]/;

function urlProblem(value: string, baseURL: boolean) {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return "Enter a full HTTPS URL.";
  }
  if (url.protocol !== "https:") return "The URL must use HTTPS.";
  if (url.username || url.password) return "Remove the user name and password from the URL.";
  if (url.hash) return "Remove the # fragment from the URL.";
  if (baseURL && url.search) return "Enter the base URL without a query string.";
  return undefined;
}

// `saved` is the channel being edited. A blank URL, token or user key keeps the saved value only
// when the provider is unchanged, as the API does.
export function notificationFromForm(data: FormData, saved?: Notification): Notification | NotificationFormError {
  const text = (key: string) => {
    const value = data.get(key);
    return typeof value === "string" ? value : "";
  };
  const provider = text("provider");
  const spec = providerSpec(provider);
  const keeps = saved !== undefined && saved.provider === provider;
  const exact = Boolean(spec?.incident) || text("routing") === "events";
  let events: string[] = [];
  if (spec?.incident) events = data.getAll("incident_events").flatMap((value) => (typeof value === "string" ? [`${value}.alert`, `${value}.recovered`] : []));
  else if (exact) events = data.getAll("events").filter((value): value is string => typeof value === "string");
  const notification: Notification = {
    provider,
    enabled: data.has("enabled"),
    success: !exact && data.has("success"),
    failure: !exact && data.has("failure"),
    health: !exact && data.has("health"),
    events,
  };
  if (events.length === 0 && !notification.success && !notification.failure && !notification.health) {
    return { field: "events", message: spec?.incident ? "Select at least one health condition." : "Select at least one event." };
  }
  if (provider === "email") {
    if (text("recipient") === "owner") return { ...notification, url: "", email: "", owner: true };
    return { ...notification, url: "", email: text("email").trim() };
  }

  notification.url = spec?.url ? text("url").trim() : "";
  if (spec?.url) {
    if (notification.url === "" && !keeps) return { field: "url", message: `${spec.url.label} is required.` };
    const problem = notification.url === "" ? undefined : urlProblem(notification.url, spec.url.baseURL);
    if (problem) return { field: "url", message: problem };
  }
  if (spec?.token) {
    const token = text("token").trim();
    if (token === "" && spec.token.required && !(keeps && saved.token_set)) return { field: "token", message: `${spec.token.label} is required.` };
    if (token.length > 4096 || lineBreak.test(token)) return { field: "token", message: "The token must be a single line of at most 4096 characters." };
    if (provider === "telegram" && /[/?#%\s]/.test(token)) return { field: "token", message: "A Telegram bot token cannot contain spaces or the characters / ? # %." };
    if (token !== "") notification.token = token;
  }
  if (spec?.target) {
    const target = text("target").trim();
    const label = spec.target.label;
    if (target === "") return { field: "target", message: `${label} is required.` };
    if (target.length > 256 || lineBreak.test(target)) return { field: "target", message: `${label} must be a single line of at most 256 characters.` };
    notification.target = target;
  }
  if (spec?.userKey) {
    const userKey = text("user_key").trim();
    if (userKey === "" && !(keeps && saved.user_key_set)) return { field: "user_key", message: "User key is required." };
    if (userKey.length > 256 || lineBreak.test(userKey)) return { field: "user_key", message: "The user key must be a single line of at most 256 characters." };
    if (userKey !== "") notification.user_key = userKey;
  }
  const secret = text("signing_secret");
  if (provider === "webhook" && secret !== "") notification.signing_secret = secret;
  return notification;
}

export function notificationTestDescription(notification: Notification) {
  const always = "even if notifications are disabled or no events are selected";
  const spec = providerSpec(notification.provider);
  if (notification.provider === "email" && notification.owner) return `This sends a test email to the verified email address of your own account, not to a resource owner, through the instance SMTP settings, ${always}. The test fails if your account has no verified email address.`;
  if (notification.provider === "email") return `This sends a test email to ${notification.email || "the saved recipient"} through the instance SMTP settings, ${always}.`;
  if (spec?.incident) return `This opens a new incident named "Cove notification connection test" with a random incident key, ${always}. It can page whoever is on call for the service. Cove does not resolve test incidents, so resolve or close it in ${spec.label} afterwards.`;
  const destination = notification.target ? `${spec?.label ?? notification.provider} (${notification.target})` : spec ? `the saved ${spec.label} destination` : "the saved destination";
  return `This sends a test message to ${destination}, ${always}.`;
}
