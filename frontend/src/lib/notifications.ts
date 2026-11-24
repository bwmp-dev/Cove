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

// Outcome events from audited actions. The control plane sends them only to channels that select
// them by name; the result groups never include them.
const outcomeGroups = {
  security: { title: "Account security", description: "Messages name the event only, without addresses, browsers or reasons. Review the audit log for details." },
  changes: { title: "Deployments and configuration", description: "Policy overrides, deploy hook deliveries and reviewed configuration changes. Applying configuration does not deploy resources." },
  migration: { title: "Imports and adoption", description: "Import reviews and Compose adoption. Importers do not change the source system." },
  rollouts: { title: "Rollouts and CPU scaling", description: "Staged rollouts waiting for promotion and their outcome, and replica changes queued by CPU autoscaling." },
  sleep: { title: "Idle sleep", description: "Request-driven services stopped after being idle and woken by the next request." },
  directory: { title: "Directory provisioning", description: "Accounts and team access changed by SCIM, LDAP or GitHub team sync, and directory syncs that did not finish." },
  certificates: { title: "Certificates", description: "Certificate requests and renewals by a resource's issuers that did not finish. Messages name the event only, without hostnames or keys. Open the resource's Domains page for details." },
} satisfies Record<string, { title: string; description: string }>;

const outcomeEvents: Record<string, { group: keyof typeof outcomeGroups; label: string }> = {
  "security.lockout": { group: "security", label: "Sign-in lockout" },
  "security.new_device": { group: "security", label: "Sign-in from a new browser" },
  "security.second_factor_reset": { group: "security", label: "Second factor reset by an administrator" },
  "audit_export.failed": { group: "security", label: "Audit export delivery failed" },
  "deployment.freeze_override": { group: "changes", label: "Deployment policy overridden" },
  "deploy_hook.accepted": { group: "changes", label: "Deploy hook delivery accepted" },
  "deploy_hook.failed": { group: "changes", label: "Deploy hook delivery refused" },
  "gitops.drift": { group: "changes", label: "GitOps difference staged for review" },
  "gitops.applied": { group: "changes", label: "GitOps configuration applied" },
  "configuration.applied": { group: "changes", label: "Reviewed configuration applied" },
  "import.reviewed": { group: "migration", label: "Import review prepared" },
  "import.failed": { group: "migration", label: "Import request refused" },
  "adoption.succeeded": { group: "migration", label: "Resource adopted" },
  "adoption.failed": { group: "migration", label: "Compose adoption did not finish" },
  "rollout.staged": { group: "rollouts", label: "Rollout staged for promotion" },
  "rollout.promoted": { group: "rollouts", label: "Rollout promoted" },
  "rollout.aborted": { group: "rollouts", label: "Rollout aborted" },
  "rollout.failed": { group: "rollouts", label: "Rollout failed" },
  "autoscale.queued": { group: "rollouts", label: "CPU scaling queued" },
  "autoscale.succeeded": { group: "rollouts", label: "CPU scaling applied" },
  "autoscale.failed": { group: "rollouts", label: "CPU scaling failed" },
  "resource.asleep": { group: "sleep", label: "Resource went to sleep" },
  "resource.awake": { group: "sleep", label: "Resource woke" },
  "identity.provisioned": { group: "directory", label: "Account provisioned" },
  "identity.deprovisioned": { group: "directory", label: "Account deprovisioned" },
  "identity.membership_changed": { group: "directory", label: "Team access changed by a directory" },
  "identity.sync_failed": { group: "directory", label: "Directory sync failed" },
  "certificate.failed": { group: "certificates", label: "Certificate issuance or renewal failed" },
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
    const known = outcomeEvents[event];
    if (known) {
      let group = groups.get(known.group);
      if (!group) {
        group = { key: known.group, ...outcomeGroups[known.group], health: false, events: [] };
        groups.set(known.group, group);
      }
      group.events.push({ value: event, label: known.label });
      continue;
    }
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
