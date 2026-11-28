import type { Capabilities, ImageStatus, ImageStatusValue, Operation, Workload, WorkloadKind } from "../api/types";
import type { Tone } from "./nodes";
import { isManagedPreview } from "./previews";

export const imageStatuses: Record<ImageStatusValue, { tone: Tone; label: string; description: string }> = {
  current: { tone: "ok", label: "Current", description: "The running image matches the registry digest for its tag." },
  update_available: { tone: "warn", label: "Update available", description: "The registry has a different image for the same tag. Pull images and redeploy to use it." },
  pinned: { tone: "neutral", label: "Pinned", description: "The reference is an image ID or digest, so it cannot change." },
  unpublished: { tone: "neutral", label: "Unpublished", description: "Built or loaded on the node, so there is no registry digest to compare." },
  unknown: { tone: "neutral", label: "Unknown", description: "The registry could not be checked for this image." },
};

// A job's containers are retained from its last run, and checking never pulls or runs it.
const jobDescriptions: Partial<Record<ImageStatusValue, string>> = {
  current: "The image of the job's last run matches the registry digest for its tag.",
  update_available: "The registry has a different image for the same tag than the job's last run used. Checking does not pull it or run the job.",
};

export function imageStatus(status: string, kind: WorkloadKind = "service") {
  const known = imageStatuses[status as ImageStatusValue];
  if (!known) return { tone: "neutral" as const, label: "Unknown", description: imageStatuses.unknown.description };
  const description = kind === "job" ? jobDescriptions[status as ImageStatusValue] : undefined;
  return description ? { ...known, description } : known;
}

// "3 current, 1 update available". Unknown results are counted separately so a partial check never reads as all current.
export function imageSummary(images: ImageStatus[]) {
  const counts = new Map<string, number>();
  for (const image of images) {
    const label = imageStatus(image.status).label;
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  return Object.values(imageStatuses)
    .map(({ label }) => (counts.get(label) ? `${counts.get(label)} ${label.toLowerCase()}` : null))
    .filter((part): part is string => part !== null)
    .join(", ");
}

export function shortDigest(digest: string) {
  const separator = digest.indexOf(":");
  return digest.slice(0, separator + 1) + digest.slice(separator + 1, separator + 13);
}

// Release links are derived by the worker from image labels and never fetched; only GitHub HTTPS release pages are shown.
export function releaseLink(url: string | undefined) {
  if (!url) return null;
  try {
    const parsed = new URL(url);
    return parsed.protocol === "https:" && parsed.hostname === "github.com" && parsed.pathname.includes("/releases/") ? parsed.href : null;
  } catch {
    return null;
  }
}

// Services and jobs are checked; managed previews are not.
export function supportsImageLifecycle(workload: Workload) {
  return !isManagedPreview(workload);
}

// Job checks also need the worker to read a job's retained containers.
export function imageChecksSupported(capabilities: Capabilities | null | undefined, kind: WorkloadKind) {
  return Boolean(capabilities?.image_lifecycle && (kind !== "job" || capabilities.job_image_lifecycle));
}

const deployments = new Set(["deploy", "rollback"]);

// The newest operation that can change the cached image result. A job's result turns stale after any newer run
// attempt, including failed, cancelled and unfinished ones, so its key also follows that run's state.
export function imageRefreshKey(operations: Operation[] | undefined, workloadId: string, kind: WorkloadKind = "service") {
  const operation = operations?.find((item) => {
    if (item.workload_id !== workloadId) return false;
    if (item.action === "image_check") return item.state === "succeeded";
    return deployments.has(item.action) && (kind === "job" || item.state === "succeeded");
  });
  if (!operation) return null;
  return kind === "job" ? `${operation.id}:${operation.state}` : operation.id;
}
