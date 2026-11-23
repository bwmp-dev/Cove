import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useId, useState, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { usePreviewMove, useQueueMove, workloadMoveQuery, type MovePreview, type WorkloadMove as Move } from "../api/moves";
import { keys, nodesQuery, workloadOperationsQuery } from "../api/queries";
import { teamNodesQuery } from "../api/teams";
import type { Node, Operation, Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { ambiguousSubmission, isUnresolvedPhase, moveDestinations, phaseInfo, supportsMoves, type Destinations } from "../lib/moves";
import { actionLabels, activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { useSession } from "../lib/session";
import { inputClass, linkClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, Notice, QueryError, Section, Spinner } from "./ui";

function nodeName(nodes: Node[] | undefined, id: string) {
  return nodes?.find((node) => node.id === id)?.name ?? "a node you cannot view";
}

function OperationLink({ id, children }: { id: string; children: ReactNode }) {
  return (
    <Link to="/operations/$operationId" params={{ operationId: id }} className={linkClass}>
      {children}
    </Link>
  );
}

function MoveStatus({ move, nodes, refreshError }: { move: Move; nodes: Node[] | undefined; refreshError: unknown }) {
  const phase = phaseInfo(move.phase);
  const unresolved = isUnresolvedPhase(move.phase);
  return (
    <div className="space-y-3">
      {refreshError !== null && (
        <Notice tone="warn" title="Could not refresh the move status">
          {errorMessage(refreshError)} The status below is from the last successful reading and may no longer be current.
        </Notice>
      )}
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <StatusLabel tone={phase.tone} label={phase.label} pulse={unresolved && phase.tone === "info"} />
        <span className="text-xs text-fg-muted">
          Updated <RelativeTime value={move.updated_at} />
        </span>
      </div>
      <p className="text-[13px] text-fg">
        {unresolved ? "Moving" : "Last move"} from <span className="break-words font-medium">{nodeName(nodes, move.source_node_id)}</span> to{" "}
        <span className="break-words font-medium">{nodeName(nodes, move.destination_node_id)}</span>.
      </p>
      <p className="text-xs text-fg-muted">{phase.detail}</p>
      {move.last_error && (
        <Notice tone={unresolved ? "error" : "warn"} title={unresolved ? "The move reported an error" : "Last reported error"}>
          <span className="break-words">{move.last_error}</span>
        </Notice>
      )}
      {unresolved && (
        <p className="text-xs text-fg-muted">
          Progress is saved and resumes automatically when an unreachable node reconnects. To cancel, use Cancel on the operation page:
          cancellation waits until the move reaches a safe point, which can take a while, and does not undo a placement that was already committed.
        </p>
      )}
      <p className="text-[13px]">
        <OperationLink id={move.id}>View the move operation</OperationLink>
      </p>
    </div>
  );
}

function Requirements() {
  return (
    <details className="group text-xs text-fg-muted">
      <summary className="cursor-pointer rounded text-[13px] font-medium text-fg">Requirements and limits</summary>
      <ul className="mt-2 list-disc space-y-1 pl-4">
        <li>Both nodes use the same CPU architecture and standard worker path, with independent Docker daemons. There is no cross-architecture emulation.</li>
        <li>Services only. At most 128 containers, 16 application volumes plus the local backup repository, 256 retained images, a 64 GiB archive and 64 MiB of worker metadata.</li>
        <li>Each node needs free space for twice the actual archive size plus 256 MiB. The preview and the move check actual free space.</li>
        <li>
          Only Cove-owned named volumes move. Remove or handle host bind mounts, external or shared volumes, active point-in-time recovery history, private connections, shared
          networks, host namespaces, privileged options and GPU assignments first.
        </li>
        <li>Retained releases and images are copied by immutable image ID, so rollback keeps working on the destination.</li>
        <li>DNS records that Cove does not manage may need updating to point at the destination, and new certificates can take a few minutes.</li>
      </ul>
    </details>
  );
}

function PreviewSummary({ preview, nodes }: { preview: MovePreview; nodes: Node[] | undefined }) {
  const { source, target } = preview;
  const volumes = source.volumes.map((volume) => volume.name);
  return (
    <DefinitionList
      compact
      items={[
        { term: "From", value: nodeName(nodes, preview.source_node_id) },
        { term: "To", value: nodeName(nodes, preview.destination_node_id) },
        { term: "Architecture", value: <span className="font-mono text-xs">{source.architecture}</span> },
        {
          term: "Containers",
          value: `${source.containers} ${source.containers === 1 ? "container" : "containers"}, ${source.running} running`,
        },
        {
          term: "Volumes",
          value:
            volumes.length === 0 ? (
              "No named volumes"
            ) : (
              <>
                {volumes.length} {volumes.length === 1 ? "volume" : "volumes"}
                <ul className="mt-1 space-y-0.5">
                  {volumes.map((name) => (
                    <li key={name} className="break-all font-mono text-xs text-fg-muted">
                      {name}
                    </li>
                  ))}
                </ul>
              </>
            ),
        },
        { term: "Destination", value: target.containers === 0 && target.volumes.length === 0 ? "No existing copy of this resource" : `${target.containers} existing containers, ${target.volumes.length} volumes` },
        { term: "Archive limit", value: formatBinaryBytes(source.max_bytes) },
      ]}
    />
  );
}

function excludedText(excluded: Destinations["excluded"]) {
  const notReady = excluded.filter((item) => item.reason === "not_ready").map((item) => item.node.name);
  const upgrade = excluded.filter((item) => item.reason === "needs_upgrade").map((item) => item.node.name);
  const parts = [];
  if (notReady.length > 0) parts.push(`Not ready: ${notReady.join(", ")}.`);
  if (upgrade.length > 0) parts.push(`Needs a worker upgrade and verify before it can receive moves: ${upgrade.join(", ")}.`);
  return parts.length > 0 ? parts.join(" ") : undefined;
}

function ConfirmBody({ workload, source, destination }: { workload: Workload; source: string; destination: string }) {
  return (
    <div className="space-y-2">
      <p>
        <span className="font-medium text-fg">{workload.name}</span> is offline from the moment the move starts until it is healthy on <span className="font-medium text-fg">{destination}</span>.
      </p>
      <ul className="list-disc space-y-1 pl-4">
        <li>Running containers on {source} are stopped. Containers that are already stopped stay stopped.</li>
        <li>Volumes, the current release, retained images and the local encrypted backup repository are copied to {destination}.</li>
        <li>The destination is health-checked, and placement switches only after that.</li>
        <li>Keep both nodes reachable until the move succeeds or rolls back. If {destination} becomes unreachable, the source may stay stopped deliberately until it returns.</li>
      </ul>
    </div>
  );
}

function NoDestination({ hint }: { hint: string | undefined }) {
  return (
    <div className="space-y-2">
      <Notice tone="info" title="No destination available">
        A move needs another node that is assigned to this team, ready, and running a worker with move support.
      </Notice>
      {hint && <p className="text-xs text-fg-muted">{hint}</p>}
    </div>
  );
}

function PreviewError({ error, online, onRetry }: { error: unknown; online: boolean; onRetry: () => void }) {
  return (
    <Notice
      tone="error"
      title="This resource cannot be moved to that node yet"
      action={
        <Button size="sm" variant="ghost" onClick={onRetry} disabled={!online}>
          Inspect again
        </Button>
      }
    >
      <span className="break-words">{errorMessage(error)}</span>
    </Notice>
  );
}

function QueueResult({ queue }: { queue: ReturnType<typeof useQueueMove> }) {
  if (queue.isSuccess) {
    return (
      <Notice tone="success" title="Move queued">
        <OperationLink id={queue.data.id}>Follow the move operation</OperationLink>
      </Notice>
    );
  }
  if (!queue.isError) return null;
  if (ambiguousSubmission(queue.error)) {
    return (
      <Notice tone="warn" title="The move may have been queued">
        The response was lost before Cove confirmed the request. The move status and operations were refreshed; check them before trying again.
      </Notice>
    );
  }
  return (
    <Notice tone="error" title="The move was not queued">
      <span className="break-words">{errorMessage(queue.error)}</span> Preview the move again to review the current state.
    </Notice>
  );
}

interface ReviewProps {
  preview: MovePreview;
  nodes: Node[];
  online: boolean;
  inspecting: boolean;
  queueing: boolean;
  onInspect: () => void;
  onMove: () => void;
}

function PreviewReview({ preview, nodes, online, inspecting, queueing, onInspect, onMove }: ReviewProps) {
  return (
    <div className="space-y-4 border-t border-line pt-4">
      <h3 className="text-[13px] font-semibold text-fg">Move preview</h3>
      <PreviewSummary preview={preview} nodes={nodes} />
      <p className="text-xs text-fg-muted">This preview was read without changing either node. The move checks it again and refuses to start if anything changed.</p>
      <div className="flex flex-wrap items-center justify-end gap-2">
        <Button variant="ghost" onClick={onInspect} disabled={!online || queueing} pending={inspecting}>
          Inspect again
        </Button>
        <Button variant="danger" onClick={onMove} disabled={!online || inspecting}>
          Move with downtime
        </Button>
      </div>
    </div>
  );
}

function MovePlanner({ workload, nodes, destinations }: { workload: Workload; nodes: Node[]; destinations: Destinations }) {
  const id = useId();
  const online = useOnline();
  const preview = usePreviewMove(workload.id);
  const queue = useQueueMove(workload.id);
  const [destination, setDestination] = useState("");
  const [confirming, setConfirming] = useState(false);
  const current = preview.data?.destination_node_id === destination ? preview.data : undefined;
  const destinationName = nodeName(nodes, destination);
  const hint = excludedText(destinations.excluded);

  function choose(value: string) {
    setDestination(value);
    preview.reset();
    queue.reset();
  }

  function inspect() {
    queue.reset();
    preview.mutate(destination);
  }

  function submit() {
    if (!current || queue.isPending) return;
    queue.mutate(current, {
      onSuccess: () => {
        preview.reset();
        setDestination("");
      },
      // A refused or unconfirmed request needs a fresh preview before another attempt.
      onError: () => preview.reset(),
      onSettled: () => setConfirming(false),
    });
  }

  if (destinations.available.length === 0) return <NoDestination hint={hint} />;

  return (
    <div className="space-y-4">
      <Field id={`${id}-destination`} label="Destination node" hint={hint}>
        <Select id={`${id}-destination`} className={inputClass} value={destination} onChange={(event) => choose(event.target.value)} aria-describedby={hint ? `${id}-destination-hint` : undefined}>
          <option value="">Choose a node</option>
          {destinations.available.map((node) => (
            <option key={node.id} value={node.id}>
              {node.name}
            </option>
          ))}
        </Select>
      </Field>
      {current ? (
        <PreviewReview
          preview={current}
          nodes={nodes}
          online={online}
          inspecting={preview.isPending}
          queueing={queue.isPending}
          onInspect={inspect}
          onMove={() => {
            queue.reset();
            setConfirming(true);
          }}
        />
      ) : (
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button onClick={inspect} disabled={destination === "" || !online} pending={preview.isPending}>
            Preview move
          </Button>
        </div>
      )}
      {preview.isPending && (
        <p className="flex items-center gap-2 text-xs text-fg-muted" role="status">
          <Spinner /> Inspecting both nodes. Nothing is stopped or changed.
        </p>
      )}
      {preview.isError && <PreviewError error={preview.error} online={online} onRetry={inspect} />}
      <QueueResult queue={queue} />
      <ConfirmDialog
        open={confirming && current !== undefined}
        danger
        title={`Move ${workload.name} to ${destinationName}?`}
        confirmLabel="Stop and move"
        pending={queue.isPending}
        onCancel={() => {
          if (!queue.isPending) setConfirming(false);
        }}
        onConfirm={submit}
      >
        <ConfirmBody workload={workload} source={nodeName(nodes, workload.node_id)} destination={destinationName} />
      </ConfirmDialog>
    </div>
  );
}

function sourceBlocker(sourceNode: Node | undefined, active: Operation | undefined): ReactNode {
  if (!sourceNode || sourceNode.state !== "ready") {
    return <Notice tone="warn" title="The current node is not ready">A move inspects the source node first. Wait until it is ready again.</Notice>;
  }
  if (!supportsMoves(sourceNode)) {
    return (
      <Notice tone="warn" title="The current node needs a worker upgrade">
        Upgrade and verify the worker on {sourceNode.name} before moving resources off it.
      </Notice>
    );
  }
  if (active) {
    return (
      <Notice tone="info" title="An operation is in progress">
        A move can be previewed once the active {actionLabels[active.action].toLowerCase()} finishes.
      </Notice>
    );
  }
  return null;
}

// Instance administrators are sent every node, but only the team's assigned nodes can receive its resources.
function useAssignedNodes() {
  const session = useSession();
  const admin = Boolean(session?.is_admin);
  const teamId = session?.team_id ?? "";
  const assigned = useQuery({ ...teamNodesQuery(teamId), enabled: admin && teamId !== "" });
  return { admin, assigned };
}

function PlannerGate({ workload, nodes }: { workload: Workload; nodes: Node[] }) {
  const online = useOnline();
  const { admin, assigned } = useAssignedNodes();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;

  if (admin && assigned.isError) return <QueryError error={assigned.error} onRetry={() => void assigned.refetch()} retrying={assigned.isFetching} what="the team's nodes" />;
  if (admin && !assigned.data) return <p className="text-xs text-fg-muted">Loading the team&apos;s nodes…</p>;
  const blocker = sourceBlocker(
    nodes.find((node) => node.id === workload.node_id),
    active,
  );
  if (blocker) return blocker;
  return (
    <>
      {!online && <Notice tone="warn" title="You are offline">Reconnect to preview or start a move.</Notice>}
      <MovePlanner workload={workload} nodes={nodes} destinations={moveDestinations(nodes, workload.node_id, admin ? (assigned.data ?? []) : null)} />
    </>
  );
}

export function WorkloadMove({ workload }: { workload: Workload }) {
  const client = useQueryClient();
  const move = useQuery(workloadMoveQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const editable = can(workload.permissions, "edit");
  const committed = move.data?.phase === "succeeded" ? move.data.destination_node_id : null;

  // A finished move changes the resource's node, which the resource header and General page read.
  useEffect(() => {
    if (committed && committed !== workload.node_id) void client.invalidateQueries({ queryKey: keys.workload(workload.id), exact: true });
  }, [client, committed, workload.id, workload.node_id]);

  let body: ReactNode;
  if (move.isPending || nodes.isPending) {
    body = (
      <p className="flex items-center gap-2 text-xs text-fg-muted" role="status">
        <Spinner /> Loading move status…
      </p>
    );
  } else if (move.isError && move.data === undefined) {
    body = <QueryError error={move.error} onRetry={() => void move.refetch()} retrying={move.isFetching} what="the move status" />;
  } else if (nodes.isError) {
    body = <QueryError error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} what="nodes" />;
  } else {
    body = (
      <>
        {move.data && <MoveStatus move={move.data} nodes={nodes.data} refreshError={move.isError ? move.error : null} />}
        {!move.data && !editable && <p className="text-[13px] text-fg-muted">This resource has not been moved.</p>}
        {!editable && <p className="text-xs text-fg-muted">Moving this resource needs editor access.</p>}
        {editable && !(move.data && isUnresolvedPhase(move.data.phase)) && <PlannerGate workload={workload} nodes={nodes.data} />}
      </>
    );
  }

  return (
    <Section
      width="form"
      title="Move to another node"
      description="Copy this service, its data and its releases to another node assigned to the team. The service is offline while it moves."
    >
      <div className="space-y-4">
        {body}
        <Requirements />
      </div>
    </Section>
  );
}
