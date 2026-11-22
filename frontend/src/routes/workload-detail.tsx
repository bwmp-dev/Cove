import { ResourcePlacement } from "../components/ResourcePlacement";
import { Clone } from "../components/Clone";
import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, Outlet, useRouterState } from "@tanstack/react-router";
import {
  Archive,
  Boxes,
  ChevronDown,
  ClipboardCheck,
  Container,
  FileClock,
  FileCode,
  Gauge,
  GitPullRequest,
  Globe,
  History,
  KeyRound,
  LayoutDashboard,
  Network,
  Play,
  RotateCcw,
  Rocket,
  ScrollText,
  Settings,
  ShieldCheck,
  SlidersHorizontal,
  Square,
  Stethoscope,
  Timer,
  Webhook,
  type LucideIcon,
} from "lucide-react";
import { createContext, use, useEffect, useId, useMemo, useRef, useState } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useCancelOperation, useQueueOperation } from "../api/mutations";
import { projectsQuery, useAssignEnvironment, type Project } from "../api/projects";
import { nodesQuery, workloadOperationsQuery, workloadQuery } from "../api/queries";
import type { Operation, WorkerOperation, Workload } from "../api/types";
import { Backups } from "../components/Backups";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { DeleteResource } from "../components/DeleteResource";
import { JobOutcome } from "../components/JobOutcome";
import { LogViewer } from "../components/LogViewer";
import { OperationsTable } from "../components/OperationsTable";
import { RelativeTime } from "../components/RelativeTime";
import { ContainerLogs, RuntimeContainers } from "../components/Runtime";
import { PendingRolloutMessage, StagedRollout, usePendingRollout } from "../components/StagedRollout";
import { NodeStateLabel, OperationStateLabel } from "../components/status";
import { Breadcrumbs, Button, DefinitionList, EmptyState, LoadingRows, MetaList, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { countLabel, findPlacement } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { actionLabels, activeOperation, allowedActions, isActive, latestDeploymentOperation, stateLabels, workloadKind } from "../lib/operations";
import { resultSummary } from "../lib/rollouts";
import { buttonClass, groupLabelClass, navItemClass, linkClass } from "../lib/styles";

const route = getRouteApi("/app/resources/$resourceId");

const ResourceContext = createContext<Workload | null>(null);

export function useResource() {
  const workload = use(ResourceContext);
  if (!workload) throw new Error("Resource pages must render inside the resource layout.");
  return workload;
}

const actionIcons = { deploy: Rocket, start: Play, stop: Square, restart: RotateCcw } as const;

function outcome(operation: Operation, kind: "service" | "job") {
  // Workers with job results report the exit status themselves; it is shown with JobOutcome.
  if (kind === "job" && operation.action === "deploy" && !operation.result?.job) {
    if (operation.state === "succeeded") return "The last run completed with exit code 0.";
    if (operation.state === "failed") return "The last run failed. Check the logs for the job's exit status.";
  }
  if (operation.state === "interrupted") {
    return "The worker stopped tracking this operation before it finished. Check the logs, then run the action again if needed.";
  }
  return null;
}

function LatestOperation({ operation, kind }: { operation: Operation; kind: "service" | "job" }) {
  const message = outcome(operation, kind);
  const error = operation.result?.error;
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
        <Link to="/operations/$operationId" params={{ operationId: operation.id }} className={linkClass}>
          {kind === "job" && operation.action === "deploy" ? "Run" : actionLabels[operation.action]}
        </Link>
        <OperationStateLabel state={operation.state} />
        <RelativeTime value={operation.created_at} className="text-fg-muted" />
        {operation.result?.revision && (
          <span className="font-mono text-xs text-fg-muted" title={operation.result.revision}>
            {operation.result.revision.slice(0, 7)}
          </span>
        )}
      </div>
      {message && <p className="text-[13px] text-fg-muted">{message}</p>}
      {operation.result?.job && <JobOutcome job={operation.result.job} />}
      {operation.result?.rollout && <RolloutResult rollout={operation.result.rollout} />}
      {error && (
        <Notice tone="error" title="Worker error">
          <span className="font-mono text-xs break-words">{error}</span>
        </Notice>
      )}
      <LogViewer operationId={operation.id} active={isActive(operation.state)} />
    </div>
  );
}

function RolloutResult({ rollout }: { rollout: NonNullable<WorkerOperation["rollout"]> }) {
  return (
    <div className="space-y-1">
      <p className="text-[13px] text-fg-muted">{resultSummary(rollout)}</p>
      {rollout.reason && <p className="text-xs break-words text-fg-muted">{rollout.reason}</p>}
    </div>
  );
}

function PlacementLink({ workload, projects }: { workload: Workload; projects: Project[] | undefined }) {
  const placement = findPlacement(projects, workload.environment_id, workload.project_id);
  if (placement && !placement.environment) return <Link to="/projects/$projectId" params={{ projectId: placement.project.id }} className={linkClass}>{placement.project.name} / <span className="whitespace-nowrap">Project resources</span></Link>;
  if (placement?.environment) {
    return (
      <Link
        to="/projects/$projectId/environments/$environmentId"
        params={{ projectId: placement.project.id, environmentId: placement.environment.id }}
        className={linkClass}
      >
        {placement.project.name} / <span className="whitespace-nowrap">{placement.environment.name}</span>
      </Link>
    );
  }
  return (
    <span>
      <span className="text-fg-muted">Project unavailable</span>{" "}
      {can(workload.permissions, "edit") && (
        <Link to="/resources/$resourceId/settings" params={{ resourceId: workload.id }} className="rounded text-fg underline underline-offset-4">
          Assign
        </Link>
      )}
    </span>
  );
}

// Explains why the resource actions are disabled and offers the way out, next to the actions themselves.
function ActiveOperationStatus({ id, operation, kind }: { id: string; operation: Operation; kind: "service" | "job" }) {
  const cancel = useCancelOperation(operation.id);
  const online = useOnline();
  const [confirm, setConfirm] = useState(false);
  const label = kind === "job" && operation.action === "deploy" ? "Run" : actionLabels[operation.action];
  const requested = operation.cancel_requested || cancel.isSuccess;
  return (
    <div id={id} className="flex max-w-md flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted md:justify-end md:text-right">
      <span>
        {label} {stateLabels[operation.state].toLowerCase()}. Other actions are available when it finishes.
      </span>
      <span className="inline-flex shrink-0 items-center gap-3 whitespace-nowrap">
        <Link to="/operations/$operationId" params={{ operationId: operation.id }} className={linkClass}>
          View operation
        </Link>
        <button
          type="button"
          className={`${linkClass} disabled:cursor-not-allowed disabled:text-fg-disabled disabled:no-underline`}
          disabled={!online || cancel.isPending || requested}
          onClick={() => setConfirm(true)}
        >
          {requested ? "Cancellation requested" : "Cancel operation"}
        </button>
      </span>
      {cancel.isError && <span role="alert" className="text-bad">Could not request cancellation: {errorMessage(cancel.error)}</span>}
      <ConfirmDialog open={confirm} title="Cancel this operation?" confirmLabel="Cancel operation" danger onCancel={() => setConfirm(false)} onConfirm={() => { setConfirm(false); cancel.mutate(); }}>
        This stops the deployment command when the worker acknowledges it. Changes already made to containers are not undone; check the resource afterward.
      </ConfirmDialog>
    </div>
  );
}

function useResourceActions(workload: Workload) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const [confirmStop, setConfirmStop] = useState(false);
  const kind = workloadKind(workload.kind);
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const pendingRollout = usePendingRollout(workload, Boolean(active));
  const blocked = !online || Boolean(active) || queue.isPending || operations.isPending;
  const statusId = useId();
  const rolloutId = useId();

  return {
    buttons: (
      <div className="flex flex-col gap-2 md:items-end">
        <div className="flex flex-wrap items-center gap-2">
          {allowedActions(kind).filter(action => !workload.preview_source_id || action === "stop").map((action) => {
            const Icon = actionIcons[action];
            const pending = queue.isPending && queue.variables.action === action;
            const label = kind === "job" && action === "deploy" ? "Deploy and run" : actionLabels[action];
            const waitsForRollout = action === "deploy" && Boolean(pendingRollout);
            return (
              <Button
                key={action}
                variant={action === "deploy" ? "primary" : "secondary"}
                disabled={blocked || waitsForRollout}
                aria-describedby={active ? statusId : waitsForRollout ? rolloutId : undefined}
                pending={pending}
                onClick={() => (action === "stop" ? setConfirmStop(true) : queue.mutate({ action }))}
              >
                {!pending && <Icon aria-hidden className="size-3.5" />}
                {label}
              </Button>
            );
          })}
        </div>
        {active && <ActiveOperationStatus id={statusId} operation={active} kind={kind} />}
        {!active && pendingRollout && <PendingRolloutMessage id={rolloutId} workloadId={workload.id} action="deploy again" className="max-w-md md:text-right" />}
      </div>
    ),
    feedback: (
      <>
        <ConfirmDialog
          open={confirmStop}
          title={`Stop ${workload.name}?`}
          confirmLabel="Stop"
          danger
          onCancel={() => setConfirmStop(false)}
          onConfirm={() => {
            setConfirmStop(false);
            queue.mutate({ action: "stop" });
          }}
        >
          {kind === "job"
            ? "The job's containers on the node will be stopped. Deploy runs the job again."
            : "The resource's containers on the node will be stopped and stay down until you start or deploy it again."}
        </ConfirmDialog>
        {queue.isSuccess && (
          <Notice
            tone="success"
            title={`${actionLabels[queue.variables.action]} queued`}
            action={
              <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost")}>
                View operation
              </Link>
            }
          />
        )}
        {queue.isError && (
          <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title={`Could not queue ${actionLabels[queue.variables.action].toLowerCase()}`}>
            {isStatus(queue.error, 409) ? "Another operation is already active for this resource. Wait for it to finish." : errorMessage(queue.error)}
          </Notice>
        )}
      </>
    ),
  };
}

function ResourceHeader({ workload }: { workload: Workload }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const projects = useQuery(projectsQuery);
  const actions = useResourceActions(workload);
  const kind = workloadKind(workload.kind);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const placement = findPlacement(projects.data, workload.environment_id, workload.project_id);
  const latest = operations.data ? latestDeploymentOperation(operations.data, workload.id) : undefined;

  return (
    <>
      <PageHeader
        breadcrumbs={
          <Breadcrumbs>
            {placement ? (
              <Link key="projects" to="/projects" className="rounded hover:text-fg">
                Projects
              </Link>
            ) : (
              <Link key="resources" to="/resources" className="rounded hover:text-fg">
                All resources
              </Link>
            )}
            {placement && (
              <Link key="project" to="/projects/$projectId" params={{ projectId: placement.project.id }} className="rounded hover:text-fg">
                {placement.project.name}
              </Link>
            )}
            {placement?.environment && (
              <Link
                key="environment"
                to="/projects/$projectId/environments/$environmentId"
                params={{ projectId: placement.project.id, environmentId: placement.environment.id }}
                className="rounded hover:text-fg"
              >
                {placement.environment.name}
              </Link>
            )}
            {workload.name}
          </Breadcrumbs>
        }
        icon={kind === "job" ? <Timer className="size-4" /> : <Boxes className="size-4" />}
        title={workload.name}
        description={
          <MetaList>
            <span key="kind">{kind === "job" ? "Job" : "Service"}</span>
            <span key="node" className="inline-flex min-w-0 items-center gap-2 align-middle">
              <Link to="/nodes/$nodeId" params={{ nodeId: workload.node_id }} className="truncate rounded hover:text-fg">
                {node?.name ?? "Node"}
              </Link>
              {node && <NodeStateLabel state={node.state} />}
            </span>
            {latest && (
              <span key="operation" className="inline-flex flex-wrap items-center gap-x-2 gap-y-1 align-middle">
                <Link to="/operations/$operationId" params={{ operationId: latest.id }} className="rounded hover:text-fg">
                  {kind === "job" && latest.action === "deploy" ? "Run" : actionLabels[latest.action]}
                </Link>
                <OperationStateLabel state={latest.state} />
                <RelativeTime value={latest.created_at} className="text-fg-subtle" />
              </span>
            )}
            {workload.routes?.[0] && (
              <span key="domain">
                <a href={`https://${workload.routes[0].domain}`} target="_blank" rel="noreferrer" className="rounded break-all hover:text-fg">
                  {workload.routes[0].domain}
                </a>
                {workload.routes.length > 1 && <span className="text-fg-subtle"> +{workload.routes.length - 1}</span>}
              </span>
            )}
            {workload.manually_stopped && (
              <span key="stopped" className="text-warn">
                Manually stopped, automatic deployments paused
              </span>
            )}
          </MetaList>
        }
        actions={can(workload.permissions, "operate") ? actions.buttons : undefined}
      />
      <div className="space-y-2 px-4 pt-4 empty:hidden sm:px-6" aria-live="polite">
        {actions.feedback}
        {workload.preview_source_id && <Notice>This is a temporary preview. Its configuration is managed by <Link to="/resources/$resourceId/previews" params={{ resourceId: workload.preview_source_id }} className="underline">the source resource</Link>. Closing it deletes its preview data.</Notice>}
        {node && node.state !== "ready" && (
          <Notice
            tone="warn"
            title="The node is not ready"
            action={
              <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={buttonClass("secondary")}>
                Open node
              </Link>
            }
          >
            Operations are queued but cannot reach the worker until the node is reachable again.
          </Notice>
        )}
      </div>
    </>
  );
}

export function WorkloadPage() {
  const { resourceId } = route.useParams();
  const workload = useQuery(workloadQuery(resourceId));

  if (workload.isPending) return <LoadingRows label="Loading resource" />;
  if (workload.isError && !workload.data) {
    if (isStatus(workload.error, 404)) {
      return (
        <>
          <PageHeader
            icon={<Boxes aria-hidden />}
            breadcrumbs={
              <Breadcrumbs>
                <Link to="/resources" className="rounded hover:text-fg">
                  All resources
                </Link>
                Not found
              </Breadcrumbs>
            }
            title="Resource not found"
          />
          <EmptyState title="This resource does not exist">It may have been removed, or the link is incorrect.</EmptyState>
        </>
      );
    }
    return (
      <div className="px-4 py-5 sm:px-6">
        <QueryError what="the resource" error={workload.error} onRetry={() => void workload.refetch()} retrying={workload.isFetching} />
      </div>
    );
  }
  return (
    <ResourceContext key={workload.data.id} value={workload.data}>
      <ResourceHeader workload={workload.data} />
      <div className="lg:grid lg:grid-cols-[12.5rem_minmax(0,1fr)]">
        <ResourceNav workload={workload.data} />
        <div className="min-w-0 pb-2">
          <Outlet />
        </div>
      </div>
    </ResourceContext>
  );
}

type ResourcePath =
  | "/resources/$resourceId"
  | "/resources/$resourceId/logs"
  | "/resources/$resourceId/operations"
  | "/resources/$resourceId/limits"
  | "/resources/$resourceId/general"
  | "/resources/$resourceId/source"
  | "/resources/$resourceId/domains"
  | "/resources/$resourceId/environment"
  | "/resources/$resourceId/networking"
  | "/resources/$resourceId/registry"
  | "/resources/$resourceId/backups"
  | "/resources/$resourceId/preflight"
  | "/resources/$resourceId/schedule"
  | "/resources/$resourceId/hooks"
  | "/resources/$resourceId/deployment-safety"
  | "/resources/$resourceId/previews"
  | "/resources/$resourceId/promotion"
  | "/resources/$resourceId/troubleshooting"
  | "/resources/$resourceId/configuration-history"
  | "/resources/$resourceId/settings";

interface ResourceSection {
  to: ResourcePath;
  label: string;
  icon: LucideIcon;
  exact?: boolean;
}

// Grouped by what the visitor is doing: watching the resource, configuring what runs, controlling releases, or managing its data and lifecycle.
function resourceSections(workload: Workload): { label: string; links: ResourceSection[] }[] {
  const service = workloadKind(workload.kind) === "service";
  return [
    {
      label: "Monitor",
      links: [
        { to: "/resources/$resourceId", label: "Overview", icon: LayoutDashboard, exact: true },
        { to: "/resources/$resourceId/logs", label: "Logs", icon: ScrollText },
        { to: "/resources/$resourceId/operations", label: "Operations", icon: History },
        { to: "/resources/$resourceId/troubleshooting", label: "Troubleshooting", icon: Stethoscope },
      ],
    },
    {
      label: "Configure",
      links: [
        { to: "/resources/$resourceId/general", label: "General", icon: SlidersHorizontal },
        { to: "/resources/$resourceId/source", label: "Source", icon: FileCode },
        { to: "/resources/$resourceId/environment", label: "Environment variables", icon: KeyRound },
        ...(service ? [{ to: "/resources/$resourceId/domains" as const, label: "Domains", icon: Globe }] : []),
        { to: "/resources/$resourceId/networking", label: "Networking", icon: Network },
        { to: "/resources/$resourceId/registry", label: "Registry", icon: Container },
        { to: "/resources/$resourceId/limits", label: "Resource limits", icon: Gauge },
      ],
    },
    {
      label: "Deploy",
      links: [
        { to: "/resources/$resourceId/preflight", label: "Preflight", icon: ClipboardCheck },
        ...(service ? [{ to: "/resources/$resourceId/deployment-safety" as const, label: "Deployment safety", icon: ShieldCheck }] : []),
        ...(service ? [{ to: "/resources/$resourceId/hooks" as const, label: "Deployment hooks", icon: Webhook }] : [{ to: "/resources/$resourceId/schedule" as const, label: "Schedule", icon: Timer }]),
        ...(service && workload.source && !workload.preview_source_id ? [{ to: "/resources/$resourceId/previews" as const, label: "Previews", icon: GitPullRequest }] : []),
        ...(service ? [{ to: "/resources/$resourceId/promotion" as const, label: "Promote release", icon: Rocket }] : []),
        { to: "/resources/$resourceId/configuration-history", label: "Change history", icon: FileClock },
      ],
    },
    {
      label: "Manage",
      links: [
        ...(service ? [{ to: "/resources/$resourceId/backups" as const, label: "Backups", icon: Archive }] : []),
        { to: "/resources/$resourceId/settings", label: "Settings", icon: Settings },
      ],
    },
  ];
}

function SectionLinks({ workload, links }: { workload: Workload; links: ResourceSection[] }) {
  return links.map(({ to, label, icon: Icon, exact }) => (
    <Link key={to} to={to} params={{ resourceId: workload.id }} activeOptions={{ exact: Boolean(exact) }} className={navItemClass}>
      <Icon aria-hidden className="size-4 shrink-0" />
      <span className="truncate">{label}</span>
    </Link>
  ));
}

// Phones get a disclosure naming the current section instead of a long horizontal strip.
function MobileResourceNav({ workload, groups }: { workload: Workload; groups: { label: string; links: ResourceSection[] }[] }) {
  const pathname = useRouterState({ select: (state) => state.location.pathname }).replace(/\/$/, "");
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === pathname;
  const trigger = useRef<HTMLButtonElement>(null);
  const panelId = useId();
  const current = groups
    .flatMap((group) => group.links.map((link) => ({ group: group.label, ...link, path: link.to.replace("$resourceId", workload.id) })))
    .find((link) => (link.exact ? pathname === link.path : pathname === link.path || pathname.startsWith(`${link.path}/`)));

  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setOpenOn(null);
      trigger.current?.focus();
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [open]);

  const CurrentIcon = current?.icon;
  return (
    <nav aria-label="Resource sections" className="border-b border-line px-4 py-2.5 sm:px-6 lg:hidden">
      <button
        ref={trigger}
        type="button"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpenOn(open ? null : pathname)}
        className="flex h-9 w-full items-center gap-2 rounded-md border border-line-strong bg-raised px-3 text-left text-[13px] text-fg hover:bg-hover"
      >
        {CurrentIcon && <CurrentIcon aria-hidden className="size-4 shrink-0 text-fg-muted" />}
        <span className="min-w-0 flex-1 truncate">
          {current && <span className="text-fg-muted">{current.group} · </span>}
          <span className="font-medium">{current?.label ?? "Sections"}</span>
        </span>
        <ChevronDown aria-hidden className={`size-4 shrink-0 text-fg-muted transition-transform ${open ? "rotate-180" : ""}`} />
        <span className="sr-only">, show all sections</span>
      </button>
      <div id={panelId} hidden={!open} className="mt-2 space-y-3 pb-1">
        {groups.map((group) => (
          <div key={group.label}>
            <p className={groupLabelClass}>{group.label}</p>
            <div className="grid grid-cols-1 gap-0.5 min-[360px]:grid-cols-2">
              <SectionLinks workload={workload} links={group.links} />
            </div>
          </div>
        ))}
      </div>
    </nav>
  );
}

function ResourceNav({ workload }: { workload: Workload }) {
  const groups = resourceSections(workload);
  return (
    <>
      <MobileResourceNav workload={workload} groups={groups} />
      <nav aria-label="Resource sections" className="hidden border-r border-line lg:block">
        <div className="sticky top-0 flex flex-col gap-4 px-2 py-4">
          {groups.map((group) => (
            <div key={group.label} className="flex flex-col gap-0.5">
              <p className={groupLabelClass}>{group.label}</p>
              <SectionLinks workload={workload} links={group.links} />
            </div>
          ))}
        </div>
      </nav>
    </>
  );
}

export function ResourceOverview() {
  const workload = useResource();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const projects = useQuery(projectsQuery);
  const kind = workloadKind(workload.kind);
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  // An active image check shows its progress here; otherwise housekeeping checks do not replace the latest deployment.
  const latest = active ?? (operations.data ? latestDeploymentOperation(operations.data, workload.id) : undefined);

  return (
    <div className="grid grid-cols-1 xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
      <div className="min-w-0">
        <StagedRollout workload={workload} operationActive={Boolean(active)} />
        <Section title={active ? "Active operation" : kind === "job" ? "Last run" : "Latest operation"}>
          {operations.isPending ? (
            <p className="text-[13px] text-fg-muted">Loading operations</p>
          ) : operations.isError && !operations.data ? (
            <QueryError what="operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
          ) : latest ? (
            <LatestOperation key={latest.id} operation={latest} kind={kind} />
          ) : (
            <p className="text-[13px] text-fg-muted">
              {kind === "job" ? "This job has not run recently. Deploy runs it once." : "No recent operations. Deploy to start the resource on its node."}
            </p>
          )}
        </Section>
        <Section
          title="Containers"
          description={kind === "job" ? "Containers from the latest run. An exited job container is expected after a run completes." : "Live Docker state on the node."}
          flush
        >
          <RuntimeContainers workloadId={workload.id} kind={kind} operationActive={Boolean(active)} manuallyStopped={workload.manually_stopped} />
        </Section>
      </div>
      <Section title="Details">
        <DefinitionList
          compact
          items={[
            { term: "Placement", value: <PlacementLink workload={workload} projects={projects.data} /> },
            { term: "Type", value: kind === "job" ? "Job" : "Service" },
            { term: "Source", value: workload.source ? <span className="font-mono text-xs break-all">{workload.source.repository}</span> : "Compose file" },
            ...(workload.source ? [{ term: "Branch", value: <span className="font-mono text-xs">{workload.source.branch}</span> }] : []),
            ...(kind === "service"
              ? [
                  {
                    term: "Domains",
                    value: workload.routes?.length ? (
                      <div className="space-y-1">
                        {workload.routes.map((item) => (
                          <a key={item.domain} className="block break-all underline underline-offset-4" href={`https://${item.domain}`} target="_blank" rel="noreferrer">
                            {item.domain}
                          </a>
                        ))}
                      </div>
                    ) : (
                      <Link to="/resources/$resourceId/domains" params={{ resourceId: workload.id }} className="rounded text-fg-muted underline underline-offset-4 hover:text-fg">
                        None
                      </Link>
                    ),
                  },
                ]
              : []),
            {
              term: "Variables",
              value: (
                <Link to="/resources/$resourceId/environment" params={{ resourceId: workload.id }} className="rounded underline underline-offset-4">
                  {workload.environment_keys.length === 0 ? "None" : countLabel(workload.environment_keys.length, "variable")}
                </Link>
              ),
            },
          ]}
        />
      </Section>
    </div>
  );
}

export function ResourceLogs() {
  const workload = useResource();
  return (
    <Section title="Container logs" description="Recent output from the resource's containers on the node.">
      <ContainerLogs workloadId={workload.id} />
    </Section>
  );
}

export function ResourceOperations() {
  const workload = useResource();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const history = useMemo(() => (operations.data ?? []).filter((operation) => operation.workload_id === workload.id), [operations.data, workload.id]);
  return (
    <Section title="Operation history" description="The 100 most recent operations for this resource." flush>
      {operations.isPending ? (
        <LoadingRows label="Loading operations" rows={3} />
      ) : operations.isError && !operations.data ? (
        <div className="p-3">
          <QueryError what="operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        </div>
      ) : history.length > 0 ? (
        <OperationsTable operations={history} caption={`Operations for ${workload.name}`} />
      ) : (
        <p className="px-4 py-4 text-[13px] text-fg-muted">No operations in the recent history.</p>
      )}
    </Section>
  );
}

export function ResourceBackups() {
  const workload = useResource();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  if (workloadKind(workload.kind) === "job") {
    return <EmptyState title="Backups are not available for jobs">Backup policies apply to long-running services with persistent data.</EmptyState>;
  }
  return (
    <Section width="form" title="Backups" description="Encrypted snapshots of writable volumes and bind mounts. Containers stop briefly for consistency.">
      <Backups workloadId={workload.id} busy={Boolean(active)} canOperate={can(workload.permissions, "operate")} canEdit={can(workload.permissions, "edit")} />
    </Section>
  );
}

function PlacementForm({ workload }: { workload: Workload }) {
  const projects = useQuery(projectsQuery);
  const assign = useAssignEnvironment(workload.id);
  const online = useOnline();
  const [selected, setSelected] = useState(`${workload.project_id}:${workload.environment_id ?? ""}`);
  const current = `${workload.project_id}:${workload.environment_id ?? ""}`;

  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        const [project_id, environment_id] = selected.split(":");
        if (project_id) assign.mutate({ project_id, environment_id: environment_id ?? "" });
      }}
    >
      <ResourcePlacement projects={projects.data ?? []} value={selected} onChange={(value) => { assign.reset(); setSelected(value); }} />
      {projects.isError && <QueryError what="projects" error={projects.error} onRetry={() => void projects.refetch()} retrying={projects.isFetching} />}
      {assign.isError && (
        <Notice tone="error" title="Could not change placement">
          {errorMessage(assign.error)}
        </Notice>
      )}
      {assign.isSuccess && selected === current && <Notice tone="success" title="Placement saved" />}
      <Button type="submit" variant="primary" disabled={!online || !selected || selected === current} pending={assign.isPending}>
        Save placement
      </Button>
    </form>
  );
}

export function ResourceSettings() {
  const workload = useResource();
  if (!can(workload.permissions, "edit")) {
    return (
      <EmptyState title="Settings need editor access">
        You can view and {can(workload.permissions, "operate") ? "operate" : "read"} this resource. Ask a team owner or admin for editor access to change its placement, clone or
        delete it.
      </EmptyState>
    );
  }
  return (
    <>
      <Section width="form" title="Placement" description="Projects and environments organise resources. Nodes are chosen when a resource is created.">
        <PlacementForm workload={workload} />
      </Section>
      <Clone kind="resource" id={workload.id} name={workload.name} />
      <Section width="form" title="Danger zone" tone="danger">
        <DeleteResource kind="workload" id={workload.id} />
      </Section>
    </>
  );
}
