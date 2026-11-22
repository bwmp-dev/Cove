import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { DownloadCloud, ScanSearch } from "lucide-react";
import { useId, useState } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useQueueOperation } from "../api/mutations";
import { nodesQuery, workloadImagesQuery, workloadOperationsQuery } from "../api/queries";
import type { ImageStatus, Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { imageRefreshKey, imageStatus, imageSummary, releaseLink, shortDigest } from "../lib/images";
import { actionLabels, activeOperation, stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { buttonClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, Section } from "./ui";

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

function ImageRow({ image, stale }: { image: ImageStatus; stale: boolean }) {
  const status = imageStatus(image.status);
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

function ImageResults({ workload }: { workload: Workload }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const images = useQuery({ ...workloadImagesQuery(workload.id, imageRefreshKey(operations.data, workload.id)), placeholderData: (previous) => previous });

  if (images.isPending) return <LoadingRows label="Loading image status" rows={2} />;
  if (images.isError && !images.data) {
    return (
      <div className="p-4 sm:px-6">
        <QueryError what="image status" error={images.error} onRetry={() => void images.refetch()} retrying={images.isFetching} />
      </div>
    );
  }
  const data = images.data;
  if (data.checked_at === null) {
    return (
      <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">
        No image check has completed yet. Running services are checked about once an hour, or you can check now.
      </p>
    );
  }
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
          <Notice tone="warn" title="Result is from before the latest deployment">
            A deploy or rollback finished after this check, so these statuses may not describe the running containers. Check images again for current results.
          </Notice>
        )}
        {images.isError && <Notice tone="error">Could not refresh image status: {errorMessage(images.error)}</Notice>}
      </div>
      {data.images.length === 0 ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">The check found no deployed containers.</p>
      ) : (
        <ul aria-label={data.stale ? "Image status at the last check" : "Image status"} className="divide-y divide-line">
          {data.images.map((image) => (
            <ImageRow key={`${image.service}:${image.reference}:${image.image_id}`} image={image} stale={data.stale} />
          ))}
        </ul>
      )}
    </>
  );
}

export function ResourceImages({ workload }: { workload: Workload }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const statusId = useId();
  const [confirmPull, setConfirmPull] = useState(false);
  const canOperate = can(workload.permissions, "operate");
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const ready = node?.state === "ready";
  const supported = Boolean(node?.capabilities?.image_lifecycle);
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const blocked = !online || !ready || !supported || Boolean(active) || queue.isPending || operations.isPending;
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
            <Button
              size="sm"
              disabled={blocked}
              pending={(queue.isPending && queue.variables.action === "image_check") || active?.action === "image_check"}
              aria-describedby={statusId}
              onClick={() => queue.mutate({ action: "image_check" })}
            >
              <ScanSearch aria-hidden className="size-3.5" />
              Check images
            </Button>
            <Button
              size="sm"
              disabled={blocked}
              pending={queue.isPending && queue.variables.action === "deploy"}
              aria-describedby={statusId}
              onClick={() => setConfirmPull(true)}
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
            {node.name} does not report image lifecycle support, which needs Docker Buildx on the node. Upgrade the worker, then verify it from the{" "}
            <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
              node page
            </Link>
            .
          </Notice>
        )}
        {canOperate && reason && <p className="text-xs text-fg-muted">{reason}</p>}
        {canOperate && active && (
          <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted">
            <span>
              {actionLabels[active.action]} {stateLabels[active.state].toLowerCase()}. Image actions are available when it finishes.
            </span>
            <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
              View operation
            </Link>
          </p>
        )}
        {queue.isSuccess && (
          <Notice
            tone="success"
            title={queue.variables.action === "image_check" ? "Image check queued" : "Pull and redeploy queued"}
            action={
              <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost", "", "sm")}>
                View operation
              </Link>
            }
          />
        )}
        {queue.isError && (
          <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title={queue.variables.action === "image_check" ? "Could not queue image check" : "Could not queue pull and redeploy"}>
            {errorMessage(queue.error)}
          </Notice>
        )}
      </div>
      {nodes.isError && (
        <div className="border-b border-line p-4 sm:px-6">
          <QueryError what="node status" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />
        </div>
      )}
      <ImageResults workload={workload} />
      <ConfirmDialog
        open={confirmPull}
        title={`Pull images and redeploy ${workload.name}?`}
        confirmLabel="Pull and redeploy"
        onCancel={() => setConfirmPull(false)}
        onConfirm={() => {
          setConfirmPull(false);
          queue.mutate({ action: "deploy", pull_latest: true });
        }}
      >
        This deploys the saved configuration again after pulling the currently configured tags, so containers are replaced. Newer version tags are not selected, and
        services with a build section are rebuilt instead of pulled. Deployment approval and freeze rules still apply.
      </ConfirmDialog>
    </Section>
  );
}
