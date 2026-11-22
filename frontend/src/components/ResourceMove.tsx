import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { ArrowRightLeft, RotateCw } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { projectsQuery, type Project } from "../api/projects";
import { keys, workloadOperationsQuery } from "../api/queries";
import { applyMove, destinationProjectsQuery, previewMove, refusedReview, type MoveDestination, type MoveReview } from "../api/resource-move";
import type { Workload } from "../api/types";
import { findPlacement } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { managesTeam } from "../lib/permissions";
import { useCurrentTeam, useSession } from "../lib/session";
import { rememberTeam } from "../lib/team";
import { inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Dialog } from "./ConfirmDialog";
import { ResourcePlacement } from "./ResourcePlacement";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Spinner } from "./ui";

function placementLabel(projects: Project[] | undefined, projectId: string, environmentId: string) {
  const placement = findPlacement(projects, environmentId || undefined, projectId);
  if (!placement) return "the selected destination";
  return `${placement.project.name} / ${placement.environment?.name ?? "Project resources"}`;
}

export function ResourceMove({ workload }: { workload: Workload }) {
  const session = useSession();
  const current = useCurrentTeam();
  const currentProjects = useQuery(projectsQuery);
  const sourceEnvironment = findPlacement(currentProjects.data, workload.environment_id, workload.project_id)?.environment;
  const isPreview = Boolean(workload.preview_source_id || sourceEnvironment?.preview_id);
  return (
    <Section
      width="form"
      title="Move resource"
      description="Change the project or environment, or transfer ownership to another team. Containers keep running and no deployment is queued."
    >
      {isPreview ? (
        <Notice title="Preview resources cannot be moved">Previews are created and removed by their preview. Move the source resource instead.</Notice>
      ) : current && session ? (
        <MoveForm
          workload={workload}
          currentTeamId={current.id}
          teams={session.is_admin || managesTeam(current) ? session.teams.filter((team) => team.id === current.id || session.is_admin || managesTeam(team)) : [current]}
        />
      ) : (
        <LoadingRows label="Loading teams" rows={2} />
      )}
    </Section>
  );
}

interface MoveFormProps {
  workload: Workload;
  currentTeamId: string;
  teams: { id: string; name: string }[];
}

function MoveForm({ workload, currentTeamId, teams }: MoveFormProps) {
  const teamFieldId = useId();
  const online = useOnline();
  const client = useQueryClient();
  const navigate = useNavigate();
  const [teamId, setTeamId] = useState(currentTeamId);
  const [placement, setPlacement] = useState("");
  const [review, setReview] = useState<MoveReview | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [moved, setMoved] = useState<string | null>(null);
  const projects = useQuery(destinationProjectsQuery(teamId));
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const [projectId = "", environmentId = ""] = placement.split(":");
  const destination: MoveDestination = { team_id: teamId, project_id: projectId, environment_id: environmentId };
  const unchanged = teamId === currentTeamId && projectId === workload.project_id && environmentId === (workload.environment_id ?? "");
  const teamName = (id: string) => teams.find((team) => team.id === id)?.name ?? id;
  const destinationLabel = `${teamId === currentTeamId ? "" : `${teamName(teamId)}: `}${placementLabel(projects.data, projectId, environmentId)}`;

  const preview = useMutation({
    mutationFn: () => previewMove(workload.id, destination),
    onSuccess: (result) => {
      setAcknowledged(false);
      setReview(result);
    },
    // A failed review leaves nothing to confirm; the error is shown beside the destination.
    onError: () => setReview(null),
  });
  const apply = useMutation({
    mutationFn: (version: string) => applyMove(workload.id, destination, version),
    onSuccess: async (result) => {
      setReview(null);
      if (result.team_id !== currentTeamId) {
        // The resource now belongs to the destination team; every cached query belongs to the previous team.
        rememberTeam(result.team_id);
        await client.cancelQueries();
        client.clear();
        await navigate({ to: "/resources/$resourceId", params: { resourceId: result.id } });
        return;
      }
      setMoved(destinationLabel);
      setPlacement("");
      await Promise.all([
        client.invalidateQueries({ queryKey: keys.workload(workload.id) }),
        client.invalidateQueries({ queryKey: keys.workloads }),
        client.invalidateQueries({ queryKey: keys.projects }),
      ]);
    },
    onError: (error) => {
      setAcknowledged(false);
      const refreshed = refusedReview(error);
      if (refreshed) setReview(refreshed);
    },
  });

  function changeDestination(next: () => void) {
    next();
    setReview(null);
    setMoved(null);
    preview.reset();
    apply.reset();
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (online && projectId && !unchanged && !active && !preview.isPending) {
      apply.reset();
      preview.mutate();
    }
  }

  function closeReview() {
    if (apply.isPending) return;
    setReview(null);
    apply.reset();
  }

  const crossTeam = teamId !== currentTeamId;
  const blocked = (review?.blockers.length ?? 0) > 0;
  // A plain-text refusal (stale version, permissions, missing destination) invalidates the review, so it must run again.
  const needsNewReview = apply.isError && !refusedReview(apply.error);

  return (
    <>
      <form onSubmit={submit} className="space-y-4">
        {teams.length > 1 ? (
          <Field
            id={teamFieldId}
            label="Team"
            hint="Transferring to another team requires owner or admin access on both teams. The resource's node must be assigned to the destination team."
          >
            <Select
              id={teamFieldId}
              className={inputClass}
              value={teamId}
              onChange={(event) =>
                changeDestination(() => {
                  setTeamId(event.target.value);
                  setPlacement("");
                })
              }
              aria-describedby={`${teamFieldId}-hint`}
            >
              {teams.map((team) => (
                <option key={team.id} value={team.id}>
                  {team.id === currentTeamId ? `${team.name} (current team)` : team.name}
                </option>
              ))}
            </Select>
          </Field>
        ) : (
          <p className="text-xs text-fg-muted">Transferring to another team requires owner or admin access on both teams.</p>
        )}
        {projects.isPending ? (
          <p role="status" className="flex items-center gap-2 text-[13px] text-fg-muted">
            <Spinner />
            Loading destinations{crossTeam ? ` in ${teamName(teamId)}` : ""}
          </p>
        ) : projects.isError ? (
          <QueryError
            what={crossTeam ? `projects in ${teamName(teamId)}` : "projects"}
            error={projects.error}
            onRetry={() => void projects.refetch()}
            retrying={projects.isFetching}
          />
        ) : hasDestinations(projects.data) ? (
          <ResourcePlacement projects={projects.data} value={placement} onChange={(value) => changeDestination(() => setPlacement(value))} />
        ) : (
          <Notice title="No destinations available">
            You need editor access to a project or environment{crossTeam ? ` in ${teamName(teamId)}` : ""} to move this resource there.
          </Notice>
        )}
        {active && (
          <Notice tone="warn" title="An operation is in progress">
            Finish or cancel the current operation before moving this resource.
          </Notice>
        )}
        {unchanged && placement && <p className="text-xs text-fg-muted">This is the resource's current placement. Choose a different destination.</p>}
        {preview.isError && (
          <Notice tone="error" title="Could not review the move">
            {errorMessage(preview.error)}
          </Notice>
        )}
        {moved && (
          <Notice tone="success" title="Resource moved">
            Moved to {moved}. Access now follows the destination; no deployment was queued.
          </Notice>
        )}
        {!online && <p className="text-xs text-fg-muted">You are offline. Reconnect to review a move.</p>}
        <div className="flex justify-end">
          <Button type="submit" disabled={!online || !projectId || unchanged || Boolean(active)} pending={preview.isPending}>
            <ArrowRightLeft aria-hidden className="size-3.5" />
            Review move
          </Button>
        </div>
      </form>
      <Dialog
        open={review !== null}
        title={review?.cross_team ? `Transfer ${workload.name} to ${teamName(teamId)}` : `Move ${workload.name}`}
        onClose={closeReview}
        footer={
          <>
            <Button variant="ghost" onClick={closeReview} disabled={apply.isPending}>
              {blocked ? "Close" : "Cancel"}
            </Button>
            {needsNewReview ? (
              <Button
                onClick={() => {
                  apply.reset();
                  preview.mutate();
                }}
                pending={preview.isPending}
                disabled={!online}
              >
                {!preview.isPending && <RotateCw aria-hidden className="size-3.5" />}
                Review again
              </Button>
            ) : (
              !blocked &&
              review && (
                <Button
                  variant={review.cross_team ? "danger" : "primary"}
                  onClick={() => apply.mutate(review.version)}
                  disabled={!online || !acknowledged}
                  pending={apply.isPending}
                >
                  {review.cross_team ? "Transfer resource" : "Move resource"}
                </Button>
              )
            )}
          </>
        }
      >
        {review && (
          <div className="space-y-3">
            <p>
              Destination: <span className="break-words text-fg">{destinationLabel}</span>
            </p>
            {blocked ? (
              <Notice tone="warn" title="Resolve these before moving">
                <ul className="list-disc space-y-1 pl-4">
                  {review.blockers.map((blocker) => (
                    <li key={blocker}>{blocker}</li>
                  ))}
                </ul>
              </Notice>
            ) : (
              <>
                <div>
                  <h3 className="text-[13px] font-semibold text-fg">What changes</h3>
                  <ul className="mt-1 list-disc space-y-1 pl-4">
                    {review.effects.map((effect) => (
                      <li key={effect}>{effect}</li>
                    ))}
                  </ul>
                </div>
                <label className="flex items-start gap-2 text-[13px] text-fg">
                  <Checkbox checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} disabled={apply.isPending} />
                  <span>{review.cross_team ? "I understand these effects, including revoked tokens, hooks and access leases" : "I understand these effects"}</span>
                </label>
              </>
            )}
            {apply.isError && (
              <Notice tone="error" title={review.cross_team ? "Could not transfer the resource" : "Could not move the resource"}>
                {refusedReview(apply.error) ? (
                  "The blockers changed since the review. Resolve them, then review the move again."
                ) : (
                  <>
                    {errorMessage(apply.error)}
                    {/* A stale-review conflict already asks for a new review. */}
                    {!isStatus(apply.error, 409) && <span className="block">Review the move again before retrying.</span>}
                  </>
                )}
              </Notice>
            )}
          </div>
        )}
      </Dialog>
    </>
  );
}

function hasDestinations(projects: Project[]) {
  return projects.some(
    (project) => project.permissions.includes("edit") || project.environments.some((environment) => !environment.preview_id && environment.permissions.includes("edit")),
  );
}
