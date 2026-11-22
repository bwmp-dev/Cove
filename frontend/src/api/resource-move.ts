import { queryOptions } from "@tanstack/react-query";
import { ApiError, request, requestForTeam } from "./client";
import { keys } from "./queries";
import type { Project } from "./projects";

export interface MoveDestination {
  team_id: string;
  project_id: string;
  environment_id: string;
}

export interface MoveReview {
  version: string;
  blockers: string[];
  effects: string[];
  cross_team: boolean;
}

export interface MoveResult extends MoveDestination {
  ok: boolean;
  id: string;
}

// Another team's projects are read with that team named on the request, so browsing destinations never changes the selected team.
export function destinationProjectsQuery(teamId: string) {
  return queryOptions({
    queryKey: [...keys.projects, "team", teamId] as const,
    queryFn: ({ signal }) => requestForTeam<Project[]>(teamId, "GET", "/api/projects", undefined, signal),
  });
}

export function previewMove(workloadId: string, destination: MoveDestination) {
  return request<MoveReview>("POST", `/api/workloads/${encodeURIComponent(workloadId)}/move/preview`, destination);
}

export function applyMove(workloadId: string, destination: MoveDestination, version: string) {
  return request<MoveResult>("POST", `/api/workloads/${encodeURIComponent(workloadId)}/move`, { ...destination, version, acknowledge: true });
}

// A refused move answers with a fresh review when its blockers changed, and with plain text otherwise.
export function refusedReview(error: unknown): MoveReview | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null;
  const details = error.details;
  if (typeof details !== "object" || details === null) return null;
  const review = details as Partial<MoveReview>;
  if (typeof review.version !== "string" || !Array.isArray(review.blockers) || !Array.isArray(review.effects)) return null;
  return { version: review.version, blockers: review.blockers, effects: review.effects, cross_team: review.cross_team === true };
}
