import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { nodesQuery, workloadOperationsQuery } from "../api/queries";
import { FileBrowser } from "../components/FileBrowser";
import { EmptyState, LoadingRows, Notice } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { useSession } from "../lib/session";
import { linkClass } from "../lib/styles";
import { useResource } from "./workload-detail";

function AdminFiles() {
  const workload = useResource();
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;

  if (nodes.isPending) return <LoadingRows label="Checking the worker" rows={2} />;
  // When the node list cannot be read the control plane still refuses unsupported workers, and its message is shown.
  if (node && (node.state !== "ready" || !node.capabilities?.file_browser)) {
    return (
      <EmptyState
        title={node.state === "ready" ? "This worker does not support file browsing" : "The worker is not ready"}
        action={<Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>Open {node.name}</Link>}
      >
        {node.state === "ready" ? "Upgrade and verify the worker on this resource's node to browse container and volume files." : "File browsing needs a ready, verified worker. Check the node's connection, then verify it."}
      </EmptyState>
    );
  }
  return (
    <>
      {!online && (
        <div className="px-4 pt-5 sm:px-6">
          <Notice tone="warn" title="You are offline">Files cannot be read or changed until the connection returns. Unsaved edits stay in this page.</Notice>
        </div>
      )}
      {active && (
        <div className="px-4 pt-5 sm:px-6">
          <Notice
            tone="warn"
            title="File access is paused while an operation runs"
            action={<Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>View operation</Link>}
          >
            Reading and changing files resumes when the operation finishes. Unsaved edits stay in this page.
          </Notice>
        </div>
      )}
      <FileBrowser workloadId={workload.id} blocked={!online || Boolean(active) || operations.isPending} />
    </>
  );
}

export function ResourceFiles() {
  const session = useSession();
  const workload = useResource();
  if (!session?.is_admin) {
    return (
      <EmptyState title="Instance administrators only">
        File access reaches inside containers and volumes on a worker that other teams may share, so it is limited to instance administrators like the node terminal. Ask one for help with this resource's files.
      </EmptyState>
    );
  }
  // The browser both reads (secrets may sit in files) and writes, so it needs Read secrets and Edit configuration on
  // this resource as well as instance administration.
  if (!can(workload.permissions, "read_secrets") || !can(workload.permissions, "edit_configuration")) {
    return (
      <EmptyState title="Read secrets and Edit configuration required">
        Files can hold secrets and change the running configuration, so browsing them needs both rights on this resource. Ask a team administrator for access.
      </EmptyState>
    );
  }
  return <AdminFiles />;
}
