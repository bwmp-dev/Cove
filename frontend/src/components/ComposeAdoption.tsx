import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RefreshCw, ScanSearch } from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { ApiError, errorMessage, isStatus } from "../api/client";
import {
  adopt,
  adoptionKeys,
  adoptionQuery,
  adoptionSelectionLimit,
  adoptionsQuery,
  deleteAdoption,
  discoverAdoptions,
  planAdoption,
  refreshAdoption,
  type Adoption,
  type AdoptionDiscovery,
  type AdoptionSummary,
  type DiscoveredProject,
} from "../api/adoptions";
import { projectConfigurationQuery } from "../api/project-configuration";
import { projectsQuery, type Project } from "../api/projects";
import { keys } from "../api/queries";
import type { Node } from "../api/types";
import { adoptionDraftErrors, adoptionStatuses, isBlocked, plannedExpiry, selectedProjects, selectionError, selectProject, type AdoptionDraft, type AdoptionDraftErrors } from "../lib/adoption";
import { countLabel, productionEnvironments } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { describedBy, inputClass, listLinkClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ChangeList } from "./ConfigurationChanges";
import { ConfirmDialog } from "./ConfirmDialog";
import { ExpiryTime, RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const offlineReason = "Unavailable while offline.";

export const adoptionAnchor = "compose-adoption";

// The node page opens a pending review directly when linked as #adoption-<id>.
export function reviewFromHash(hash: string) {
  return hash.startsWith("adoption-") ? hash.slice("adoption-".length) || null : null;
}

interface Props {
  node: Node;
  initialReview: string | null;
}

export function ComposeAdoption({ node, initialReview }: Props) {
  const online = useOnline();
  const [selected, setSelected] = useState<string | null>(initialReview);
  const supported = node.capabilities?.adoption === true;
  const ready = node.state === "ready";

  return (
    <div id={adoptionAnchor} className="scroll-mt-16 lg:scroll-mt-4">
      <Section
        title="Adopt existing Compose projects"
        description="Bring Docker Compose projects that already run on this node under Cove management without restarting them. Adoption records ownership only: containers, source files, project names, volumes, networks, ports and proxy routes stay as they are."
        flush
      >
        {!supported ? (
          <div className="p-4">
            <Notice tone="warn" title="Upgrade the worker to adopt Compose projects">
              This node's worker does not report adoption support. Upgrade the worker on this node, then verify it here before scanning.
            </Notice>
          </div>
        ) : (
          <div className="divide-y divide-line">
            <ScanStep
              node={node}
              ready={ready}
              online={online}
              onPlanned={(plan) => setSelected(plan.id)}
            />
            <ReviewList nodeId={node.id} online={online} selected={selected} onSelect={setSelected} />
            {selected && <AdoptionReview key={selected} node={node} ready={ready} adoptionId={selected} online={online} scrollOnMount={selected === initialReview} onClose={() => setSelected(null)} />}
          </div>
        )}
      </Section>
    </div>
  );
}

function ScanStep({ node, ready, online, onPlanned }: { node: Node; ready: boolean; online: boolean; onPlanned: (plan: Adoption) => void }) {
  const discover = useMutation({ gcTime: 0, mutationFn: () => discoverAdoptions(node.id) });
  const disabledReason = !online ? offlineReason : !ready ? "The node must be ready to scan." : undefined;
  return (
    <Subsection
      title="1. Scan"
      description="A read-only inspection lists Compose projects that Cove does not manage, with their services and container counts. Cove does not read variable values, commands, mount paths or label values, and nothing on the node changes. Scans run only when you ask."
      actions={
        <Button size="sm" variant={discover.data ? "secondary" : "primary"} onClick={() => discover.mutate()} pending={discover.isPending} disabled={Boolean(disabledReason)} title={disabledReason}>
          {!discover.isPending && <ScanSearch aria-hidden className="size-3.5" />}
          {discover.data ? "Scan again" : "Scan node"}
        </Button>
      }
    >
      {!ready && (
        <Notice tone="warn" title="The node is not ready">
          Scanning, reviewing and adopting need a connected worker. Verify the node, then scan again.
        </Notice>
      )}
      {discover.isPending && <LoadingRows label="Scanning Compose projects" rows={2} />}
      {discover.isError && (
        <Notice tone="error" title="Could not scan the node">
          {errorMessage(discover.error)}
          {isStatus(discover.error, 409) && " Upgrade and verify the worker, then scan again."}
        </Notice>
      )}
      {discover.data && !discover.isPending && (
        <MappingForm
          key={discover.submittedAt}
          node={node}
          discovery={discover.data}
          online={online}
          onPlanned={(plan) => {
            discover.reset();
            onPlanned(plan);
          }}
        />
      )}
    </Subsection>
  );
}

interface MappingProps {
  node: Node;
  discovery: AdoptionDiscovery;
  online: boolean;
  onPlanned: (plan: Adoption) => void;
}

function MappingForm({ node, discovery, online, onPlanned }: MappingProps) {
  const id = useId();
  const client = useQueryClient();
  const projects = useQuery(projectsQuery);
  const [projectId, setProjectId] = useState("");
  const [environmentId, setEnvironmentId] = useState("");
  const [drafts, setDrafts] = useState<Record<string, AdoptionDraft>>({});
  const [attempted, setAttempted] = useState(false);
  const configuration = useQuery({ ...projectConfigurationQuery(projectId), enabled: projectId !== "" });
  const existingKeys = new Set((configuration.data?.manifest.resources ?? []).map((resource) => resource.key));
  const found = discovery.projects ?? [];
  const warnings = discovery.warnings ?? [];
  const adoptable = found.filter((project) => !isBlocked(project));
  const chosen = selectedProjects(drafts);
  const errors = adoptionDraftErrors(drafts, existingKeys);
  const countError = selectionError(chosen.length);
  const destinations = (projects.data ?? []).filter((project) => can(project.permissions, "edit"));
  const destination = destinations.find((project) => project.id === projectId);

  const plan = useMutation({
    gcTime: 0,
    mutationFn: () =>
      planAdoption(node.id, {
        project_id: projectId,
        environment_id: environmentId,
        projects: chosen.map((project) => ({ project, key: drafts[project]!.key, name: drafts[project]!.name.trim() })),
      }),
    onSuccess: (result) => {
      client.setQueryData(adoptionKeys.detail(node.id, result.id), result);
      onPlanned(result);
    },
    onSettled: () => client.invalidateQueries({ queryKey: adoptionKeys.all(node.id), exact: true }),
  });

  function edit(project: string, change: Partial<AdoptionDraft>) {
    plan.reset();
    setDrafts((current) => ({ ...current, [project]: { ...(current[project] ?? { selected: false, key: "", name: "" }), ...change } }));
  }

  function toggle(project: string, checked: boolean) {
    plan.reset();
    setDrafts((current) => selectProject(current, project, checked, existingKeys));
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttempted(true);
    if (projectId === "" || countError || Object.keys(errors).length > 0 || configuration.isPending) return;
    plan.mutate();
  }

  if (found.length === 0) {
    return <p className="text-[13px] text-fg-muted">No unmanaged Compose projects were found on this node.</p>;
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      {warnings.length > 0 && <Warnings title="What adoption does and does not do" warnings={warnings} />}
      <p className="text-xs text-fg-muted">
        Found {countLabel(found.length, "project")}; {countLabel(adoptable.length, "project")} can be adopted. Select up to {adoptionSelectionLimit} for one review.
      </p>
      <ul aria-label="Discovered Compose projects" className="divide-y divide-line rounded-md border border-line">
        {found.map((project) => (
          <DiscoveredRow
            key={project.project}
            project={project}
            draft={drafts[project.project]}
            errors={attempted ? errors[project.project] : undefined}
            locked={plan.isPending}
            onToggle={(checked) => toggle(project.project, checked)}
            onChange={(change) => edit(project.project, change)}
          />
        ))}
      </ul>
      {attempted && countError && <p className="text-xs text-bad">{countError}</p>}

      <fieldset className="space-y-3">
        <legend className="text-[13px] font-semibold text-fg">Destination</legend>
        <p className="text-xs text-fg-muted">New resources are created in an existing project of the current team. Each selected Compose project becomes one resource with a new key.</p>
        {projects.isError ? (
          <QueryError what="projects" error={projects.error} onRetry={() => void projects.refetch()} retrying={projects.isFetching} />
        ) : (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={`${id}-project`} label="Project" error={attempted && projectId === "" ? "Choose a destination project." : undefined}>
              <Select
                id={`${id}-project`}
                value={projectId}
                onChange={(event) => {
                  plan.reset();
                  setProjectId(event.target.value);
                  setEnvironmentId("");
                }}
                disabled={plan.isPending || projects.isPending}
                aria-invalid={attempted && projectId === "" ? true : undefined}
                aria-describedby={attempted && projectId === "" ? `${id}-project-error` : undefined}
              >
                <option value="" disabled>
                  {projects.isPending ? "Loading projects" : destinations.length === 0 ? "No projects you can edit" : "Choose a project"}
                </option>
                {destinations.map((project) => (
                  <option key={project.id} value={project.id}>
                    {project.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field id={`${id}-environment`} label="Environment" hint="Persistent environments only.">
              <Select id={`${id}-environment`} value={environmentId} onChange={(event) => { plan.reset(); setEnvironmentId(event.target.value); }} disabled={plan.isPending || !destination} aria-describedby={`${id}-environment-hint`}>
                <option value="">Project resources</option>
                {destination &&
                  productionEnvironments(destination).map((environment) => (
                    <option key={environment.id} value={environment.id}>
                      {environment.name}
                    </option>
                  ))}
              </Select>
            </Field>
          </div>
        )}
        {configuration.isError && <QueryError what="the destination configuration" error={configuration.error} onRetry={() => void configuration.refetch()} retrying={configuration.isFetching} />}
      </fieldset>

      {plan.isError && (
        <Notice tone="error" title="Dry-run review failed">
          {errorMessage(plan.error)}
          {isStatus(plan.error, 409) && " Nothing was reserved or changed. Correct the source project on the node, then scan and review again."}
        </Notice>
      )}
      <div className="flex flex-wrap items-center justify-end gap-x-4 gap-y-2">
        <p className="text-xs text-fg-muted" aria-live="polite">
          {countLabel(chosen.length, "project")} selected
        </p>
        <Button type="submit" variant="primary" pending={plan.isPending} disabled={!online || adoptable.length === 0} title={online ? undefined : offlineReason}>
          Review adoption
        </Button>
      </div>
    </form>
  );
}

interface DiscoveredRowProps {
  project: DiscoveredProject;
  draft: AdoptionDraft | undefined;
  errors: AdoptionDraftErrors | undefined;
  locked: boolean;
  onToggle: (checked: boolean) => void;
  onChange: (change: Partial<AdoptionDraft>) => void;
}

function DiscoveredRow({ project, draft, errors, locked, onToggle, onChange }: DiscoveredRowProps) {
  const id = useId();
  const blockers = project.blockers ?? [];
  const services = project.services ?? [];
  const blocked = blockers.length > 0;
  const selected = !blocked && draft?.selected === true;
  return (
    <li className="min-w-0 space-y-3 px-3 py-3">
      <div className="flex min-w-0 items-start gap-2.5">
        {!blocked && <Checkbox id={`${id}-include`} checked={selected} onChange={(event) => onToggle(event.target.checked)} disabled={locked} className="mt-0.5" />}
        <div className="min-w-0 flex-1">
          {blocked ? (
            <p className="font-mono text-[13px] break-all text-fg">{project.project}</p>
          ) : (
            <label htmlFor={`${id}-include`} className="cursor-pointer font-mono text-[13px] break-all text-fg">
              {project.project}
            </label>
          )}
          <p className="mt-0.5 text-xs break-words text-fg-muted">
            {countLabel(project.containers, "container")}
            {services.length > 0 && (
              <>
                {" · "}
                <span className="font-mono">{services.join(", ")}</span>
              </>
            )}
          </p>
        </div>
      </div>
      {blocked && (
        <div className="space-y-1 rounded-md border border-warn-line bg-warn-tint px-3 py-2">
          <p className="text-xs font-medium text-fg">Cannot be adopted</p>
          <ul aria-label={`Blockers for ${project.project}`} className="list-disc space-y-0.5 pl-4 text-xs break-words text-fg-muted">
            {blockers.map((blocker) => (
              <li key={blocker}>{blocker}</li>
            ))}
          </ul>
        </div>
      )}
      {selected && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field id={`${id}-key`} label="Resource key" error={errors?.key} hint="New, unused cove.yaml key.">
            <input id={`${id}-key`} value={draft.key} onChange={(event) => onChange({ key: event.target.value })} disabled={locked} className={`${inputClass} font-mono`} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={errors?.key ? true : undefined} aria-describedby={describedBy(`${id}-key`, errors?.key, true)} />
          </Field>
          <Field id={`${id}-name`} label="Name" error={errors?.name} optional>
            <input id={`${id}-name`} value={draft.name} onChange={(event) => onChange({ name: event.target.value })} disabled={locked} className={inputClass} placeholder={project.project} autoComplete="off" aria-invalid={errors?.name ? true : undefined} aria-describedby={errors?.name ? `${id}-name-error` : undefined} />
          </Field>
        </div>
      )}
    </li>
  );
}

function placementName(projects: Project[] | undefined, projectId: string, environmentId: string) {
  const project = projects?.find((item) => item.id === projectId);
  if (!project) return "Project unavailable";
  const environment = environmentId ? project.environments.find((item) => item.id === environmentId) : undefined;
  return `${project.name} / ${environmentId ? (environment?.name ?? "Environment unavailable") : "Project resources"}`;
}

function ReviewList({ nodeId, online, selected, onSelect }: { nodeId: string; online: boolean; selected: string | null; onSelect: (id: string | null) => void }) {
  const client = useQueryClient();
  const reviews = useQuery(adoptionsQuery(nodeId));
  const projects = useQuery(projectsQuery);
  const [confirm, setConfirm] = useState<AdoptionSummary | null>(null);
  const remove = useMutation({
    mutationFn: (id: string) => deleteAdoption(nodeId, id),
    onSuccess: (_, id) => {
      setConfirm(null);
      client.removeQueries({ queryKey: adoptionKeys.detail(nodeId, id) });
      if (selected === id) onSelect(null);
    },
    onSettled: () => client.invalidateQueries({ queryKey: adoptionKeys.all(nodeId), exact: true }),
  });
  const list = reviews.data ?? [];
  const pending = list.filter((review) => review.status === "applying").length;
  return (
    <Subsection title="Adoption reviews" description="Reviews for the current team on this node. Unstarted reviews expire after 24 hours; pending and completed adoptions are kept.">
      {pending > 0 && (
        <Notice tone="warn" title={`${countLabel(pending, "adoption")} pending`}>
          Reserved resources stay read-only until their adoption completes. Open the review and retry the same adoption; do not create a new review for these projects.
        </Notice>
      )}
      {reviews.isPending ? (
        <LoadingRows label="Loading adoption reviews" rows={1} />
      ) : reviews.isError && !reviews.data ? (
        <QueryError what="adoption reviews" error={reviews.error} onRetry={() => void reviews.refetch()} retrying={reviews.isFetching} />
      ) : list.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No adoption reviews. Scan the node to start one.</p>
      ) : (
        <ul aria-label="Adoption reviews" className="divide-y divide-line rounded-md border border-line">
          {list.map((review) => {
            const status = adoptionStatuses[review.status];
            const current = selected === review.id;
            return (
              <li key={review.id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-3 py-2">
                <div className="min-w-0 space-y-0.5">
                  <p className="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[13px]">
                    <span className="font-medium break-words text-fg">{placementName(projects.data, review.project_id, review.environment_id)}</span>
                    <StatusLabel tone={status?.tone ?? "neutral"} label={status?.label ?? review.status} />
                  </p>
                  <p className="text-xs text-fg-muted">
                    Reviewed <RelativeTime value={review.created_at} />
                    {review.status === "planned" && (
                      <>
                        <span className="mx-1.5">·</span>
                        <ExpiryTime value={plannedExpiry(review)} />
                      </>
                    )}
                  </p>
                </div>
                <div className="flex flex-wrap gap-2">
                  {review.status === "planned" && (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        remove.reset();
                        setConfirm(review);
                      }}
                      disabled={!online || remove.isPending}
                      title={online ? undefined : offlineReason}
                    >
                      Delete
                    </Button>
                  )}
                  <Button size="sm" onClick={() => onSelect(review.id)} disabled={current} aria-current={current ? "true" : undefined}>
                    {review.status === "applying" ? "Resume" : "View"}
                  </Button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <ConfirmDialog open={confirm !== null} title="Delete this adoption review?" confirmLabel="Delete review" danger pending={remove.isPending} onCancel={() => setConfirm(null)} onConfirm={() => confirm && remove.mutate(confirm.id)}>
        <div className="space-y-3">
          <p>The unstarted review is removed. Nothing was reserved or adopted, and the Compose projects on the node are not affected.</p>
          {remove.isError && (
            <Notice tone="error" title="Could not delete the review">
              {errorMessage(remove.error)}
              {isStatus(remove.error, 409) && " It has already started, so it can no longer be deleted. Reload the reviews."}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
    </Subsection>
  );
}

interface ReviewProps {
  node: Node;
  ready: boolean;
  adoptionId: string;
  online: boolean;
  scrollOnMount: boolean;
  onClose: () => void;
}

function AdoptionReview({ node, ready, adoptionId, online, scrollOnMount, onClose }: ReviewProps) {
  const review = useQuery(adoptionQuery(node.id, adoptionId));
  const projects = useQuery(projectsQuery);
  const container = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (scrollOnMount) container.current?.scrollIntoView({ block: "start" });
  }, [scrollOnMount]);

  let body;
  if (review.data) {
    body = <ReviewBody node={node} ready={ready} adoption={review.data} placement={placementName(projects.data, review.data.project_id, review.data.environment_id)} online={online} reloading={review.isFetching} onReload={() => void review.refetch()} />;
  } else if (review.isError) {
    body = isStatus(review.error, 404) ? (
      <Notice tone="warn" title="This review is no longer available">
        It expired or was deleted. Scan the node and create a new review.
      </Notice>
    ) : (
      <QueryError what="the adoption review" error={review.error} onRetry={() => void review.refetch()} retrying={review.isFetching} />
    );
  } else {
    body = <LoadingRows label="Loading adoption review" rows={3} />;
  }
  return (
    <div ref={container} id={`adoption-${adoptionId}`} className="scroll-mt-16 lg:scroll-mt-4">
      <Subsection
        title="2. Review and adopt"
        description="Check the new resources and warnings, confirm both statements, then adopt. Running containers are not restarted."
        actions={
          <Button size="sm" variant="ghost" onClick={onClose}>
            Close
          </Button>
        }
      >
        {body}
      </Subsection>
    </div>
  );
}

interface BodyProps {
  node: Node;
  ready: boolean;
  adoption: Adoption;
  placement: string;
  online: boolean;
  reloading: boolean;
  onReload: () => void;
}

function ReviewBody({ node, ready, adoption, placement, online, reloading, onReload }: BodyProps) {
  const client = useQueryClient();
  const [acknowledged, setAcknowledged] = useState(false);
  const [paused, setPaused] = useState(false);
  const resources = adoption.resources ?? [];
  const warnings = adoption.warnings ?? [];
  const status = adoptionStatuses[adoption.status];

  const settle = () =>
    Promise.all([
      client.invalidateQueries({ queryKey: adoptionKeys.all(node.id) }),
      client.invalidateQueries({ queryKey: keys.nodes }),
      client.invalidateQueries({ queryKey: keys.workloads }),
      client.invalidateQueries({ queryKey: keys.projects }),
      client.invalidateQueries({ queryKey: keys.operations }),
    ]);
  const run = useMutation({
    gcTime: 0,
    mutationFn: () => adopt(node.id, adoption.id),
    onSuccess: (result) => client.setQueryData(adoptionKeys.detail(node.id, adoption.id), result),
    onSettled: () => {
      setAcknowledged(false);
      setPaused(false);
      return settle();
    },
  });
  const refresh = useMutation({
    gcTime: 0,
    mutationFn: () => refreshAdoption(node.id, adoption.id),
    onMutate: () => run.reset(),
    onSuccess: (result) => client.setQueryData(adoptionKeys.detail(node.id, adoption.id), result),
    onSettled: () => {
      setAcknowledged(false);
      setPaused(false);
      return Promise.all([client.invalidateQueries({ queryKey: adoptionKeys.all(node.id), exact: true }), client.invalidateQueries({ queryKey: keys.workloads })]);
    },
  });

  const complete = adoption.status === "complete";
  const applying = adoption.status === "applying";
  const busy = run.isPending || refresh.isPending;
  const adoptReason = !online ? offlineReason : !acknowledged || !paused ? "Confirm both statements first." : undefined;
  const adoptedCount = resources.filter((resource) => resource.complete).length;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
        <StatusLabel tone={status?.tone ?? "neutral"} label={status?.label ?? adoption.status} />
        <span className="break-words text-fg-muted">Destination: {placement}</span>
      </div>

      {complete && (
        <Notice tone="success" title="Adopted without restarting">
          <p>
            Cove now owns {countLabel(resources.length, "resource")}. No containers were started, stopped, restarted or recreated, no deployment ran, and source files, Docker project names, volumes, networks, ports and proxy routes are unchanged.
          </p>
          <p className="mt-1">Configure routes, authentication, backups and Git sources on each resource before its first deploy from Cove, which can recreate containers.</p>
        </Notice>
      )}
      {applying && (
        <Notice tone="warn" title="Adoption is pending">
          <p>
            An earlier attempt was interrupted or is still finishing; {adoptedCount} of {countLabel(resources.length, "resource")} are adopted. Containers were not restarted. Reserved resources are read-only until this adoption completes.
          </p>
          <p className="mt-1">Retry this same adoption. If the source Compose files changed since the review, refresh the source review first. Never create a new review for these projects.</p>
        </Notice>
      )}
      {!ready && !complete && (
        <Notice tone="warn" title="The node is not ready">
          Adopting needs a connected worker. Verify the node before you continue.
        </Notice>
      )}

      <ul aria-label="Adopted resources" className="divide-y divide-line rounded-md border border-line">
        {resources.map((resource) => (
          <li key={resource.resource_id} className="flex min-w-0 flex-wrap items-center justify-between gap-x-4 gap-y-1 px-3 py-2.5">
            <div className="min-w-0 space-y-0.5">
              <p className="text-[13px] break-words">
                {adoption.status === "planned" ? (
                  <span className="font-medium text-fg">{resource.name}</span>
                ) : (
                  <Link to="/resources/$resourceId" params={{ resourceId: resource.resource_id }} className={listLinkClass}>
                    {resource.name}
                  </Link>
                )}
              </p>
              <p className="text-xs break-all text-fg-muted">
                <span className="font-mono">{resource.project}</span>
                <span className="sr-only"> becomes key </span>
                <span aria-hidden> → </span>
                <span className="font-mono">{resource.key}</span>
              </p>
            </div>
            <StatusLabel tone={resource.complete ? "ok" : applying ? "warn" : "neutral"} label={resource.complete ? "Adopted" : applying ? "Pending" : "Not started"} />
          </li>
        ))}
      </ul>

      <div className="space-y-1.5">
        <p className="text-xs font-medium text-fg">Canonical configuration changes</p>
        <ChangeList changes={adoption.changes} label="Adoption configuration changes" />
      </div>

      {warnings.length > 0 && <Warnings title={complete ? "What adoption did not change" : "Before you adopt"} warnings={warnings} />}

      {refresh.isSuccess && !refresh.isPending && (
        <Notice tone="info" title="Source review refreshed">
          Pending configuration was updated from the current source files. Source containers were not touched. Review the warnings and confirm both statements again before retrying.
        </Notice>
      )}
      {refresh.isError && (
        <Notice tone="error" title="Could not refresh the source review">
          {errorMessage(refresh.error)}
        </Notice>
      )}
      {run.isError && <AdoptError error={run.error} planned={adoption.status === "planned"} reloading={reloading} onReload={onReload} />}

      {!complete && (
        <div className="space-y-3 border-t border-line pt-4">
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} disabled={busy} className="mt-0.5" />
            <span>I reviewed the resources and warnings above. Adoption registers ownership of the running containers without restarting them, and their first deploy from Cove can recreate them.</span>
          </label>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={paused} onChange={(event) => setPaused(event.target.checked)} disabled={busy} className="mt-0.5" />
            <span>I paused the source manager's deployment, cleanup and patching automation for these projects. Cove cannot verify this.</span>
          </label>
          <div className="flex flex-wrap justify-end gap-2">
            {applying && (
              <Button onClick={() => refresh.mutate()} pending={refresh.isPending} disabled={!online || run.isPending} title={online ? "Re-inspect changed source files without touching containers" : offlineReason}>
                {!refresh.isPending && <RefreshCw aria-hidden className="size-3.5" />}
                Refresh source review
              </Button>
            )}
            <Button variant="primary" onClick={() => run.mutate()} pending={run.isPending} disabled={Boolean(adoptReason) || refresh.isPending} title={adoptReason}>
              {applying ? "Retry adoption" : "Adopt without restarting"}
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function AdoptError({ error, planned, reloading, onReload }: { error: unknown; planned: boolean; reloading: boolean; onReload: () => void }) {
  const pending = isStatus(error, 502) || isStatus(error, 503) || (error instanceof ApiError && error.isNetwork);
  let hint = "";
  if (pending) hint = " The adoption may now be pending with its resources reserved. Reload the review to see its status, then retry this same adoption.";
  else if (isStatus(error, 409) && planned) hint = " If the destination project changed, delete this review and create a new one.";
  return (
    <Notice
      tone={pending ? "warn" : "error"}
      title={pending ? "Adoption did not finish" : "Could not adopt"}
      action={
        <Button size="sm" variant="ghost" onClick={onReload} pending={reloading}>
          Reload review
        </Button>
      }
    >
      {errorMessage(error)}
      {hint}
    </Notice>
  );
}

function Warnings({ title, warnings }: { title: string; warnings: string[] }) {
  return (
    <Notice tone="warn" title={title}>
      <ul aria-label={title} className="list-disc space-y-1 pl-4">
        {warnings.map((warning) => (
          <li key={warning}>{warning}</li>
        ))}
      </ul>
    </Notice>
  );
}
