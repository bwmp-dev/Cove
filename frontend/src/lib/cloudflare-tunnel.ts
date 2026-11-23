import type { TunnelStatus, TunnelZone } from "../api/cloudflare-tunnel";
import type { Tone } from "./nodes";

export const maxTunnelHostnames = 128;

// Only the official connector image, pinned by a numeric release tag and digest, as the worker requires.
const officialImage = /^(?:docker\.io\/)?cloudflare\/cloudflared:([0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?)@sha256:[a-f0-9]{64}$/;
const officialRepository = /^(?:docker\.io\/)?cloudflare\/cloudflared(?::|@|$)/;

export function imageError(image: string) {
  const value = image.trim();
  if (!value) return "Enter the connector image.";
  if (officialImage.test(value)) return undefined;
  if (!officialRepository.test(value)) return "Use the official cloudflare/cloudflared image.";
  if (/:latest(?:@|$)/.test(value)) return "Pin a release version such as 2026.11.1, not latest.";
  return "Pin a release version and digest: cloudflare/cloudflared:<version>@sha256:<64 lowercase hex digits>.";
}

export function sameImage(a: string, b: string) {
  const strip = (value: string) => value.trim().replace(/^docker\.io\//, "");
  return strip(a) === strip(b);
}

export function imageVersion(image: string) {
  return officialImage.exec(image.trim())?.[1];
}

export function inZone(hostname: string, zone?: TunnelZone) {
  if (!zone) return false;
  const domain = zone.domain.toLowerCase();
  return hostname === domain || hostname.endsWith(`.${domain}`);
}

export interface PhaseInfo {
  tone: Tone;
  label: string;
  active: boolean;
}

const controlPhases: Record<string, PhaseInfo> = {
  disabled: { tone: "neutral", label: "Not published", active: false },
  pending: { tone: "info", label: "Queued", active: true },
  provisioning: { tone: "info", label: "Creating the tunnel", active: true },
  confirming: { tone: "info", label: "Confirming the tunnel", active: true },
  connecting: { tone: "info", label: "Starting the connector", active: true },
  publishing: { tone: "info", label: "Publishing DNS records", active: true },
  active: { tone: "ok", label: "Published", active: false },
  disabling: { tone: "info", label: "Stopping the connector", active: true },
  cleanup: { tone: "info", label: "Removing DNS records and the tunnel", active: true },
  blocked: { tone: "bad", label: "Blocked", active: false },
};

export function controlPhase(phase: string): PhaseInfo {
  return controlPhases[phase] ?? { tone: "warn", label: `Unknown state (${phase || "none"})`, active: false };
}

const workerPhases: Record<string, PhaseInfo> = {
  disabled: { tone: "neutral", label: "Disabled", active: false },
  preparing: { tone: "info", label: "Preparing", active: true },
  connecting: { tone: "info", label: "Connecting", active: true },
  active: { tone: "ok", label: "Active", active: false },
  disabling: { tone: "info", label: "Disabling", active: true },
};

export function workerPhase(phase: string): PhaseInfo {
  return workerPhases[phase] ?? { tone: "warn", label: `Unknown state (${phase || "none"})`, active: false };
}

const dnsPhases: Record<string, PhaseInfo> = {
  pending: { tone: "info", label: "Pending", active: true },
  // The create request was sent; its outcome stays unknown until Cove confirms it owns the record.
  presenting: { tone: "info", label: "Creating, confirming ownership", active: true },
  present: { tone: "ok", label: "Present", active: false },
  cleanup: { tone: "info", label: "Removing", active: true },
};

export function dnsPhase(phase: string, cleanupRequested: boolean, error: string): PhaseInfo {
  const known = dnsPhases[phase];
  if (!known) return { tone: "warn", label: `Unknown state (${phase || "none"})`, active: false };
  if (cleanupRequested && phase !== "cleanup") return { tone: error ? "bad" : "info", label: error ? "Removal failed" : "Removal pending", active: !error };
  if (error) return { ...known, tone: "warn" };
  return known;
}

// Only the worker's own report proves the edge connection; a control "active" phase or a failed read never does.
export function edgeState(status: TunnelStatus): { tone: Tone; label: string; detail: string } {
  const worker = status.worker;
  if (status.worker_error || !worker) {
    return { tone: "warn", label: "Unknown", detail: status.worker_error ? "The worker could not be read, so the connector state is unknown." : "The worker has not reported a connector state." };
  }
  if (worker.connected && worker.running) return { tone: "ok", label: "Connected to the Cloudflare edge", detail: "The connector is running and reports an authenticated connection." };
  if (worker.running) return { tone: "warn", label: "Running, not connected", detail: "The connector container runs but has not reported a connection to the Cloudflare edge." };
  return { tone: "neutral", label: "Not running", detail: "No connector container is running on this node." };
}

// A tunnel, or cleanup of one, still exists, so the zone cannot change until it is disabled and cleaned up.
export function tunnelRemains(status: TunnelStatus) {
  return status.configuration !== null || status.cleanup_requested || status.phase !== "disabled" || status.dns.length > 0;
}

export function needsRetry(status: TunnelStatus) {
  return Boolean(status.last_error || status.worker_error || status.worker?.last_error || status.phase === "blocked" || status.dns.some((record) => record.last_error));
}

export interface TunnelDraft {
  zoneId: string;
  hostnames: string[];
  image: string;
}

export function draftFrom(status: TunnelStatus): TunnelDraft {
  const tokenZones = status.zones.filter((zone) => zone.connection_kind === "token");
  return {
    zoneId: status.configuration?.zone_id ?? (tokenZones.length === 1 ? (tokenZones[0]?.id ?? "") : ""),
    hostnames: status.configuration?.hostnames ?? [],
    image: status.configuration?.image ?? status.default_image,
  };
}

export function draftKey(draft: TunnelDraft) {
  return JSON.stringify([draft.zoneId, [...draft.hostnames].sort(), draft.image.trim()]);
}

export type DraftErrors = Partial<Record<"zone" | "hostnames" | "image", string>>;

export function validateTunnelDraft(draft: TunnelDraft, status: TunnelStatus): DraftErrors {
  const errors: DraftErrors = {};
  const zone = status.zones.find((item) => item.id === draft.zoneId);
  if (!zone) errors.zone = "Choose a Cloudflare zone.";
  else if (zone.connection_kind !== "token") errors.zone = "This zone uses an OAuth DNS-only connection, which cannot manage tunnels. Import it with an API token.";
  else if (status.configuration && status.configuration.zone_id !== draft.zoneId) errors.zone = "Disable the tunnel and wait for cleanup before choosing another zone.";
  if (draft.hostnames.length === 0) errors.hostnames = "Choose at least one hostname.";
  else if (draft.hostnames.length > maxTunnelHostnames) errors.hostnames = `Choose at most ${maxTunnelHostnames} hostnames.`;
  else if (zone && draft.hostnames.some((hostname) => !inZone(hostname, zone))) errors.hostnames = `Every hostname must be in ${zone.domain}.`;
  const image = imageError(draft.image);
  if (image) errors.image = image;
  return errors;
}
