import type { ImageStatus, ImageStatusValue, Operation, Workload } from "../api/types";
import type { Tone } from "./nodes";

export const imageStatuses: Record<ImageStatusValue, { tone: Tone; label: string; description: string }> = {
  current: { tone: "ok", label: "Current", description: "The running image matches the registry digest for its tag." },
  update_available: { tone: "warn", label: "Update available", description: "The registry has a different image for the same tag. Pull images and redeploy to use it." },
  pinned: { tone: "neutral", label: "Pinned", description: "The reference is an image ID or digest, so it cannot change." },
  unpublished: { tone: "neutral", label: "Unpublished", description: "Built or loaded on the node, so there is no registry digest to compare." },
  unknown: { tone: "neutral", label: "Unknown", description: "The registry could not be checked for this image." },
};

export function imageStatus(status: string) {
  return imageStatuses[status as ImageStatusValue] ?? { tone: "neutral" as const, label: "Unknown", description: imageStatuses.unknown.description };
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

// Checks run on deployed services only; jobs and managed previews are not checked.
export function supportsImageLifecycle(workload: Workload) {
  return workload.kind !== "job" && !workload.preview_source_id;
}

// The newest finished operation that can change the cached image result.
export function imageRefreshKey(operations: Operation[] | undefined, workloadId: string) {
  const operation = operations?.find(
    (item) => item.workload_id === workloadId && item.state === "succeeded" && (item.action === "image_check" || item.action === "deploy" || item.action === "rollback"),
  );
  return operation?.id ?? null;
}
