import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { DownloadCloud, ScanSearch } from "lucide-react";
import { useId, useState } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useQueueOperation } from "../api/mutations";
import { useEnvironmentHold } from "./EnvironmentSuspension";
import { useIdleState } from "./IdleSleep";
import { idleReason } from "../lib/idle";
import { holdReason } from "../lib/suspension";
import { nodesQuery, workloadImagesQuery, workloadOperationsQuery } from "../api/queries";
import type { ImageStatus, Node, Operation, Workload, WorkloadKind } from "../api/types";
import { useOnline } from "../lib/hooks";
import { imageChecksSupported, imageRefreshKey, imageStatus, imageSummary, releaseLink, shortDigest } from "../lib/images";
import { actionLabels, activeOperation, stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { buttonClass, linkClass } from "../lib/styles";
import { useCapacityGate } from "./CapacityCheck";
import { DeployDialog } from "./DeployReview";
import { RelativeTime } from "./RelativeTime";
import { PendingRolloutMessage, usePendingRollout } from "./StagedRollout";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, Section } from "./ui";

type Queue = ReturnType<typeof useQueueOperation>;

function Digest({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex min-w-0 gap-1.5">
      <span className="text-fg-subtle">{label}</span>
      <span className="font-mono" title={value}>
        <span aria-hidden>{shortDigest(value)}</span>
        <span className="sr-only">{value}</span>
      </span>
    </span>
  );
}

function ImageRow({ image, stale, kind }: { image: ImageStatus; stale: boolean; kind: WorkloadKind }) {
  const status = imageStatus(image.status, kind);
  const release = releaseLink(image.release_notes);
  return (
    <li className="grid gap-x-4 gap-y-1.5 px-4 py-3 sm:px-6 md:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_11rem]">
      <p className="text-[13px] font-medium break-words text-fg">{image.service}</p>
      <div className="min-w-0 space-y-1">
        <p className="font-mono text-xs break-all text-fg">{image.reference}</p>
        <p className="flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-fg-muted">
          {image.image_id && <Digest label="Image" value={image.image_id} />}
          {image.registry_digest && <Digest label="Registry" value={image.registry_digest} />}
        </p>
        {image.message && <p className="text-xs break-words text-fg-muted">{image.message}</p>}
        {release && (
          <p className="text-xs text-fg-muted">
            <a href={release} target="_blank" rel="noreferrer" className={linkClass}>
              GitHub release
            </a>{" "}
            · derived from the image&apos;s source and version labels
          </p>
        )}
      </div>
      <div className="md:text-right" title={status.description}>
        <StatusLabel tone={stale ? "neutral" : status.tone} label={status.label} />
        <span className="sr-only">. {status.description}</span>
      </div>
    </li>
  );
}

const resultText = {
  service: {
    never: "No image check has completed yet. Running services are checked about once an hour, or you can check now.",
    stale: { title: "Result is from before the latest deployment", body: "A deploy or rollback finished after this check, so these statuses may not describe the running containers. Check images again for current results." },
    empty: "The check found no deployed containers.",
  },
  job: {
    never: "No image check has completed yet. Retained job containers are checked about once an hour, or check now. Checks never run the job. If the job has never run, run it first: checks read the containers its last run left behind.",
    stale: { title: "Result is from before the latest run", body: "The job was run or deployed after this check, including runs that failed, were cancelled or are still going, so these statuses may not describe the images of its last run. Check images again for current results." },
    empty: "The check found no retained containers from the job's last run. Run the job, then check again.",
  },
};

function ImageResults({ workload, operations }: { workload: Workload; operations: UseQueryResult<Operation[]> }) {
  const kind: WorkloadKind = workload.kind === "job" ? "job" : "service";
  const images = useQuery({ ...workloadImagesQuery(workload.id, imageRefreshKey(operations.data, workload.id, kind)), placeholderData: (previous) => previous });
  const text = resultText[kind];

  if (images.isPending) return <LoadingRows label="Loading image status" rows={2} />;
  if (images.isError && !images.data) {
    return (
      <div className="p-4 sm:px-6">
        <QueryError what="image status" error={images.error} onRetry={() => void images.refetch()} retrying={images.isFetching} />
      </div>
    );
  }
  const data = images.data;
  if (data.checked_at === null) return <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">{text.never}</p>;
  const summary = imageSummary(data.images);
  return (
    <>
      <div className="space-y-3 border-b border-line px-4 py-3 sm:px-6">
        <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
          <span>
            Checked <RelativeTime value={data.checked_at} />
          </span>
          {summary && <span>{summary}</span>}
          <Link to="/operations/$operationId" params={{ operationId: data.operation_id }} className={linkClass}>
            View check
          </Link>
        </p>
        {data.stale && (
          <Notice tone="warn" title={text.stale.title}>
            {text.stale.body}
          </Notice>
        )}
        {images.isError && <Notice tone="error">Could not refresh image status: {errorMessage(images.error)}</Notice>}
      </div>
      {data.images.length === 0 ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">{text.empty}</p>
      ) : (
        <ul aria-label={data.stale ? "Image status at the last check" : "Image status"} className="divide-y divide-line">
          {data.images.map((image) => (
            <ImageRow key={`${image.service}:${image.reference}:${image.image_id}`} image={image} stale={data.stale} kind={kind} />
          ))}
        </ul>
      )}
    </>
  );
}

function ActiveOperation({ active, job }: { active: Operation; job: boolean }) {
  const label = job && active.action === "deploy" ? "Run" : actionLabels[active.action];
  return (
    <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted">
      <span>
        {label} {stateLabels[active.state].toLowerCase()}. Image actions are available when it finishes.
      </span>
      <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
        View operation
      </Link>
    </p>
  );
}

function QueueOutcome({ queue }: { queue: Queue }) {
  if (queue.isSuccess) {
    return (
      <Notice
        tone="success"
        title={queue.variables.action === "image_check" ? "Image check queued" : "Pull and redeploy queued"}
        action={
          <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost", "", "sm")}>
            View operation
          </Link>
        }
      />
    );
  }
  if (queue.isError) {
    return (
      <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title={queue.variables.action === "image_check" ? "Could not queue image check" : "Could not queue pull and redeploy"}>
        {errorMessage(queue.error)}
      </Notice>
    );
  }
  return null;
}

function NodeError({ nodes }: { nodes: UseQueryResult<Node[]> }) {
  if (!nodes.isError) return null;
  return (
    <div className="border-b border-line p-4 sm:px-6">
      <QueryError what="node status" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />
    </div>
  );
}

function CheckButton({ queue, active, disabled, statusId }: { queue: Queue; active?: Operation; disabled: boolean; statusId: string }) {
  return (
    <Button
      size="sm"
      disabled={disabled}
      pending={(queue.isPending && queue.variables.action === "image_check") || active?.action === "image_check"}
      aria-describedby={statusId}
      onClick={() => queue.mutate({ action: "image_check" })}
    >
      <ScanSearch aria-hidden className="size-3.5" />
      Check images
    </Button>
  );
}

function NodeLink({ node }: { node: Node }) {
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
      node page
    </Link>
  );
}

function ServiceImages({ workload }: { workload: Workload }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const statusId = useId();
  const [confirmPull, setConfirmPull] = useState(false);
  // Pulling and redeploying is a manual deploy: the capacity check runs first, then the configuration review asks for a reason.
  const capacity = useCapacityGate({ workloadId: workload.id, actionLabel: "Pull and redeploy", onDeploy: () => setConfirmPull(true) });
  const canOperate = can(workload.permissions, "operate");
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const ready = node?.state === "ready";
  const supported = imageChecksSupported(node?.capabilities, "service");
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const pendingRollout = usePendingRollout(workload, Boolean(active));
  const { hold } = useEnvironmentHold(workload);
  const idle = useIdleState(workload);
  const held = holdReason(hold, "deploy", "Pulling images and redeploying") ?? idleReason(idle.hold, "deploy", "Pulling images and redeploying");
  const blocked = !online || !ready || !supported || Boolean(active) || queue.isPending || operations.isPending || capacity.checking;
  const reason = !online
    ? "You are offline. Image actions are available when the connection returns."
    : node && !ready && !active
      ? `${node.name} is not ready. Image actions are available once the node is ready.`
      : null;

  return (
    <Section
      title="Images"
      description="Compares deployed containers with their registry tags. Checks do not pull images or replace containers."
      flush
      actions={
        canOperate && (
          <div className="flex flex-wrap gap-2">
            <CheckButton queue={queue} active={active} disabled={blocked} statusId={statusId} />
            <Button
              size="sm"
              disabled={blocked || Boolean(pendingRollout) || Boolean(held)}
              pending={(queue.isPending && queue.variables.action === "deploy") || capacity.checking}
              aria-describedby={statusId}
              onClick={capacity.start}
            >
              <DownloadCloud aria-hidden className="size-3.5" />
              Pull images and redeploy
            </Button>
          </div>
        )
      }
    >
      <div id={statusId} className="space-y-3 border-b border-line px-4 py-3 empty:hidden sm:px-6">
        {!canOperate && <p className="text-xs text-fg-muted">Checking images and redeploying need operator access. Results are read-only.</p>}
        {canOperate && node && ready && !supported && (
          <Notice tone="warn" title="Worker upgrade needed">
            {node.name} does not report image lifecycle support, which needs Docker Buildx on the node. Upgrade the worker, then verify it from the <NodeLink node={node} />.
          </Notice>
        )}
        {canOperate && reason && <p className="text-xs text-fg-muted">{reason}</p>}
        {canOperate && held && <p className="text-xs text-fg-muted">{held}</p>}
        {canOperate && !active && pendingRollout && <PendingRolloutMessage workloadId={workload.id} action="pull images and redeploy" />}
        {canOperate && active && <ActiveOperation active={active} job={false} />}
        <QueueOutcome queue={queue} />
      </div>
      <NodeError nodes={nodes} />
      <ImageResults workload={workload} operations={operations} />
      {capacity.dialog}
      <DeployDialog
        open={confirmPull}
        title={`Pull images and redeploy ${workload.name}?`}
        confirmLabel="Pull and redeploy"
        workloadId={workload.id}
        pending={queue.isPending}
        onCancel={() => setConfirmPull(false)}
        onDeploy={(reason) => queue.mutate({ action: "deploy", pull_latest: true, reason }, { onSettled: () => setConfirmPull(false) })}
      >
        This deploys the saved configuration again, so containers are replaced. Services whose image an active automatic image update policy applied keep running
        that exact image; other services pull their configured tags first. Newer version tags are not selected, and services with a build section are rebuilt
        instead of pulled. Deployment approval and freeze rules still apply.
      </DeployDialog>
    </Section>
  );
}

interface JobCheckState {
  online: boolean;
  operations: UseQueryResult<Operation[]>;
  nodes: UseQueryResult<Node[]>;
  node?: Node;
  active?: Operation;
}

// Unknown node or operation state blocks the check, even when older cached data is still shown.
function jobCheckBlocker({ online, operations, nodes, node, active }: JobCheckState): string | null {
  if (!online) return "You are offline. Checking images is available when the connection returns.";
  if (operations.isError) return "Cove cannot tell whether another operation is running. Checking images is available once operations load.";
  if (operations.isPending) return "Checking for active operations.";
  if (active) return null;
  if (nodes.isError) return "Cove cannot read the node. Checking images is available once the node loads.";
  if (nodes.isPending) return "Checking the node.";
  if (!node) return "Cove cannot find this job's node, so it cannot confirm the node is ready.";
  if (node.state !== "ready") return `${node.name} is not ready. Checking images is available once the node is ready.`;
  if (!node.capabilities) return `${node.name} has not reported its capabilities, so job image checks cannot be confirmed. Verify the node first.`;
  return null;
}

function useJobCheck(workload: Workload) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const online = useOnline();
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const active = operations.data && !operations.isError ? activeOperation(operations.data, workload.id) : undefined;
  const reason = jobCheckBlocker({ online, operations, nodes, node, active });
  const needsUpgrade = !reason && !active && !imageChecksSupported(node?.capabilities, "job");
  return { operations, nodes, node, active, reason, needsUpgrade };
}

function JobCheckStatus({ node, active, reason, needsUpgrade }: { node?: Node; active?: Operation; reason: string | null; needsUpgrade: boolean }) {
  return (
    <>
      {needsUpgrade && node && (
        <Notice tone="warn" title="Worker upgrade needed">
          {node.name} does not report job image checks. Upgrade the worker, then verify it from the <NodeLink node={node} />. Earlier results stay readable.
        </Notice>
      )}
      {reason && <p className="text-xs text-fg-muted">{reason}</p>}
      {active && <ActiveOperation active={active} job />}
    </>
  );
}

function JobImages({ workload }: { workload: Workload }) {
  const { operations, nodes, node, active, reason, needsUpgrade } = useJobCheck(workload);
  const queue = useQueueOperation(workload.id);
  const statusId = useId();
  const canOperate = can(workload.permissions, "operate");
  const blocked = Boolean(reason) || Boolean(active) || needsUpgrade || queue.isPending;

  return (
    <Section
      title="Images"
      description="Compares the images of the job's last run with their registry tags. Checks read the containers the last run left behind and never run the job, pull images or start containers."
      flush
      actions={canOperate && <CheckButton queue={queue} active={active} disabled={blocked} statusId={statusId} />}
    >
      <div id={statusId} className="space-y-3 border-b border-line px-4 py-3 empty:hidden sm:px-6">
        {canOperate ? <JobCheckStatus node={node} active={active} reason={reason} needsUpgrade={needsUpgrade} /> : <p className="text-xs text-fg-muted">Checking images needs operator access. Results are read-only.</p>}
        <QueueOutcome queue={queue} />
      </div>
      <NodeError nodes={nodes} />
      <ImageResults workload={workload} operations={operations} />
    </Section>
  );
}

export function ResourceImages({ workload }: { workload: Workload }) {
  return workload.kind === "job" ? <JobImages workload={workload} /> : <ServiceImages workload={workload} />;
}
