import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export type NotesScope = "project" | "resource";

export interface Notes {
  markdown: string;
  // Rendered by the server with raw HTML, images and unsafe links removed. It is the only HTML the browser renders.
  html: string;
  version: string;
  updated_at: string | null;
}

export const notesByteLimit = 65_536;

function notesPath(scope: NotesScope, id: string) {
  return `/api/${scope === "project" ? "projects" : "workloads"}/${encodeURIComponent(id)}/notes`;
}

// Notes may hold sensitive operational detail, so nothing is kept in the cache once the notes are off screen.
export const notesQuery = (scope: NotesScope, id: string) =>
  queryOptions({
    queryKey: ["notes", scope, id] as const,
    queryFn: ({ signal }) => request<Notes>("GET", notesPath(scope, id), undefined, signal),
    gcTime: 0,
  });

export function saveNotes(scope: NotesScope, id: string, markdown: string, version: string) {
  return request<Notes>("PUT", notesPath(scope, id), { markdown, version });
}
