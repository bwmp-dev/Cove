export const maxCachePathLength = 4096;

// Mirrors the worker's check so obvious mistakes are caught before a node-wide pause; the worker still decides.
export function cachePathError(value: string) {
  if (value === "") return "Enter the filesystem's mount path.";
  if (value.length > maxCachePathLength) return `Use at most ${maxCachePathLength} characters.`;
  if (!value.startsWith("/")) return "Use an absolute Linux path, such as /mnt/cove-buildkit.";
  if (value === "/") return "The root filesystem cannot hold the build cache.";
  if (/[,"\0\r\n]/.test(value)) return "Paths cannot contain commas, quotes or line breaks.";
  if (value.endsWith("/") || value.includes("//") || value.split("/").some((part) => part === "." || part === "..")) {
    return "Use the clean path, without trailing slashes, repeated slashes or . and .. parts.";
  }
  return undefined;
}

const byteUnits = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function formatBytes(bytes: number) {
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0 ? `${value} B` : `${value.toFixed(1)} ${byteUnits[unit]}`;
}
