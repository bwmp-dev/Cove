import type { ComposeBundle, ComposeBundleSummary } from "../api/compose-bundle";
import type { Workload } from "../api/types";
import { byteLength } from "./workload-form";

// Mirrors worker.ValidateComposeBundle so mistakes are shown next to the field before saving.
export const bundleLimits = { files: 7, paths: 7, assets: 32, profiles: 16, fileBytes: 1024 * 1024, totalBytes: 512 * 1024 };

const profilePattern = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;

// inline: pasted Compose, so overrides and assets are contents. paths: repository overrides by path.
// profiles: Cove-managed Compose in a Git source, which accepts profiles only.
export type BundleMode = "inline" | "paths" | "profiles";

export function bundleMode(workload: Workload): BundleMode {
  if (!workload.source) return "inline";
  return workload.source.inline_compose ? "profiles" : "paths";
}

// The worker stages pasted Compose as compose.yaml, and a repository's primary file is its compose path.
export function primaryFile(workload: Workload) {
  return workload.source && !workload.source.inline_compose ? workload.source.compose_path : "compose.yaml";
}

export interface FileRow {
  key: string;
  path: string;
  content: string;
}

export interface BundleDraft {
  files: FileRow[];
  assets: FileRow[];
  paths: FileRow[];
  profiles: string;
}

let nextKey = 0;

export function fileRow(path = "", content = ""): FileRow {
  nextKey += 1;
  return { key: `bundle-${nextKey}`, path, content };
}

export function draftFromSummary(summary: ComposeBundleSummary): BundleDraft {
  return { files: [], assets: [], paths: summary.paths.map((path) => fileRow(path)), profiles: summary.profiles.join(" ") };
}

export function draftFromBundle(bundle: ComposeBundle | null): BundleDraft {
  return {
    files: (bundle?.files ?? []).map((file) => fileRow(file.path, file.content)),
    assets: Object.entries(bundle?.assets ?? {})
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([name, content]) => fileRow(name, content)),
    paths: (bundle?.paths ?? []).map((path) => fileRow(path)),
    profiles: (bundle?.profiles ?? []).join(" "),
  };
}

export function sameDraft(a: BundleDraft, b: BundleDraft) {
  const strip = (draft: BundleDraft) => JSON.stringify([draft.files, draft.assets, draft.paths].map((rows) => rows.map(({ path, content }) => [path, content])).concat([[parseProfiles(draft.profiles)]]));
  return strip(a) === strip(b);
}

export function parseProfiles(text: string) {
  return text.split(/[\s,]+/).filter(Boolean);
}

export function validateRelativePath(value: string): string | null {
  if (value === "") return "Enter a relative path.";
  if (byteLength(value) > 240) return "Use at most 240 characters.";
  if (/[\\:\0\r\n]/.test(value)) return "Use forward slashes, without colons or backslashes.";
  if (value.startsWith("/")) return "Use a path relative to the primary Compose file.";
  for (const part of value.split("/")) {
    if (part === "" || part === "." || part === "..") return "Remove empty, . and .. path segments.";
    const lower = part.toLowerCase();
    if (lower === ".git" || lower.startsWith(".cove-")) return "Paths cannot use .git or .cove- segments.";
  }
  return null;
}

function validateContent(content: string): string | null {
  if (content.includes("\0")) return "Use text without NUL characters.";
  if (byteLength(content) > bundleLimits.fileBytes) return "Each file can be at most 1 MiB.";
  return null;
}

export function contentBytes(draft: BundleDraft) {
  return [...draft.files, ...draft.assets].reduce((total, row) => total + byteLength(row.content), 0);
}

export function isEmptyDraft(draft: BundleDraft) {
  return draft.files.length === 0 && draft.assets.length === 0 && draft.paths.length === 0 && parseProfiles(draft.profiles).length === 0;
}

export function validateBundle(draft: BundleDraft, mode: BundleMode, primary: string): { bundle?: ComposeBundle; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  const seen = new Set([primary.toLowerCase()]);
  const rows = mode === "inline" ? [...draft.files, ...draft.assets] : mode === "paths" ? draft.paths : [];
  for (const row of rows) {
    const path = row.path.trim();
    const pathError = validateRelativePath(path) ?? (seen.has(path.toLowerCase()) ? (path.toLowerCase() === primary.toLowerCase() ? `${primary} is the primary Compose file. Choose another name.` : "Each path must be unique, ignoring case.") : null);
    if (pathError) errors[`${row.key}-path`] = pathError;
    seen.add(path.toLowerCase());
  }
  if (mode === "inline") {
    for (const row of [...draft.files, ...draft.assets]) {
      const contentError = validateContent(row.content);
      if (contentError) errors[`${row.key}-content`] = contentError;
    }
    if (draft.files.length > bundleLimits.files) errors.form = `Add at most ${bundleLimits.files} override files.`;
    if (draft.assets.length > bundleLimits.assets) errors.form = `Add at most ${bundleLimits.assets} ancillary files.`;
    if (contentBytes(draft) > bundleLimits.totalBytes) errors.form = "Override and ancillary files can total at most 512 KiB.";
  }
  if (mode === "paths" && draft.paths.length > bundleLimits.paths) errors.form = `Add at most ${bundleLimits.paths} override paths.`;
  const profiles = parseProfiles(draft.profiles);
  const invalid = profiles.find((profile) => !profilePattern.test(profile));
  if (invalid) errors.profiles = `${invalid} is not a valid profile name. Start with a letter or digit and use letters, digits, dots, dashes or underscores, up to 64 characters.`;
  else if (new Set(profiles).size !== profiles.length) errors.profiles = "Each profile can be listed once.";
  else if (profiles.length > bundleLimits.profiles) errors.profiles = `Select at most ${bundleLimits.profiles} profiles.`;
  if (!errors.form && isEmptyDraft(draft)) errors.form = "Add a file or profile, or cancel and clear the selection instead.";
  if (Object.keys(errors).length > 0) return { errors };
  return {
    errors,
    bundle: {
      files: mode === "inline" ? draft.files.map((row) => ({ path: row.path.trim(), content: row.content })) : [],
      assets: mode === "inline" ? Object.fromEntries(draft.assets.map((row) => [row.path.trim(), row.content])) : {},
      paths: mode === "paths" ? draft.paths.map((row) => row.path.trim()) : [],
      profiles,
    },
  };
}

export function move<T>(rows: T[], index: number, offset: -1 | 1) {
  const target = index + offset;
  if (target < 0 || target >= rows.length) return rows;
  const next = [...rows];
  const [row] = next.splice(index, 1);
  if (row === undefined) return rows;
  next.splice(target, 0, row);
  return next;
}
