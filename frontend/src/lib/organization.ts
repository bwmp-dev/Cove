import type { BulkResult } from "../api/organization";

export const maxTags = 12;
export const maxBulkSelection = 50;
const tagPattern = /^[a-z0-9][a-z0-9._-]{0,31}$/;

// Mirrors the server's rules so people see the problem before saving; the server stays authoritative.
export function parseTags(text: string): { tags: string[]; error?: string } {
  const tags = [...new Set(text.split(/[\s,]+/).map((tag) => tag.trim().toLowerCase()).filter(Boolean))].sort();
  const invalid = tags.find((tag) => !tagPattern.test(tag));
  if (invalid) return { tags, error: `"${invalid}" is not a valid tag. Use 1-32 lowercase letters, numbers, dots, underscores or hyphens, starting with a letter or number.` };
  if (tags.length > maxTags) return { tags, error: `Use at most ${maxTags} tags.` };
  return { tags };
}

// Activity paths come from the server; only the three link shapes it produces are rendered as links.
export type ActivityLink =
  | { to: "/operations/$operationId"; params: { operationId: string } }
  | { to: "/resources/$resourceId"; params: { resourceId: string } }
  | { to: "/projects/$projectId"; params: { projectId: string } };

export function activityLink(path: string): ActivityLink | null {
  const match = /^\/(operations|resources|projects)\/([A-Za-z0-9_-]{1,128})$/.exec(path);
  const section = match?.[1];
  const id = match?.[2];
  if (!section || !id) return null;
  if (section === "operations") return { to: "/operations/$operationId", params: { operationId: id } };
  if (section === "resources") return { to: "/resources/$resourceId", params: { resourceId: id } };
  return { to: "/projects/$projectId", params: { projectId: id } };
}

export function pageWindow(total: number, pageSize: number, page: number) {
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(Math.max(1, page), pageCount);
  const start = (current - 1) * pageSize;
  return { page: current, pageCount, start, end: Math.min(start + pageSize, total) };
}

// 202 means the operation was queued, not that it finished.
export function summarizeBulk(results: BulkResult[]) {
  const queued = results.filter((result) => result.status === 202).length;
  return { queued, failed: results.length - queued };
}
