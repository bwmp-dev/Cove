import { useQuery } from "@tanstack/react-query";
import { DeleteResource } from "../components/DeleteResource";
import { CleanupPreview } from "../components/CleanupPreview";
import { BuildCacheSection } from "../components/BuildCache";
import { getRouteApi, Link } from "@tanstack/react-router";
import { KeyRound, Network, Pencil, RefreshCw, Server } from "lucide-react";
import { useState } from "react";
import { errorMessage } from "../api/client";
import { useRenewEnrollment, useUpdateNode, useVerifyNode } from "../api/mutations";
import { nodesQuery, workloadsQuery } from "../api/queries";
import type { Node, NodeState } from "../api/types";
import { EnrollmentPanel } from "../components/EnrollmentPanel";
import { NodeLinks } from "../components/NodeLinks";
import { NodeForm } from "../components/NodeForm";
import { ExpiryTime, RelativeTime } from "../components/RelativeTime";
import { NodeStateLabel } from "../components/status";
import { nodeStates } from "../lib/nodes";
import { Breadcrumbs, Button, DefinitionList, EmptyState, LoadingRows, Meter, MeterGroup, Notice, PageHeader, QueryError, Section, SectionGrid } from "../components/ui";
import { projectsQuery } from "../api/projects";
import { findPlacement } from "../lib/hierarchy";
import { buttonClass, listLinkClass } from "../lib/styles";
import { useSession } from "../lib/session";
import { SharedNodeWarning } from "../components/Access";
import { useNow } from "../lib/hooks";
import { canVerify, nodeAddress } from "../lib/nodes";

const route = getRouteApi("/app/nodes/$nodeId");

function Capabilities({ node }: { node: Node }) {
  const capabilities = node.capabilities;
  if (!capabilities) {
    return <p className="text-[13px] text-fg-muted">No probe result yet. Verify the node after it registers.</p>;
  }
  const value = (text: string) => (text ? <span className="font-mono text-xs">{text}</span> : <span className="text-fg-subtle">Not detected</span>);
  return (
    <DefinitionList
      items={[
        { term: "Operating system", value: value(capabilities.os) },
        { term: "Architecture", value: value(capabilities.arch) },
        { term: "Docker", value: value(capabilities.docker) },
        { term: "Docker Compose", value: value(capabilities.compose) },
        { term: "Worker protocol", value: <span className="font-mono text-xs">{capabilities.protocol}</span> },
        { term: "Ready for resources", value: capabilities.ready ? "Yes" : "No" },
      ]}
    />
  );
}

function NodeResources({ nodeId }: { nodeId: string }) {
  const workloads = useQuery(workloadsQuery);
  const projects = useQuery(projectsQuery);
  if (workloads.isPending) return <p className="px-4 py-3 text-[13px] text-fg-muted">Loading resources</p>;
  if (workloads.isError) {
    return (
      <div className="p-3">
        <QueryError what="resources" error={workloads.error} onRetry={() => void workloads.refetch()} retrying={workloads.isFetching} />
      </div>
    );
  }
  const placed = workloads.data.filter((workload) => workload.node_id === nodeId);
  if (placed.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No resources run on this node.</p>;
  return (
    <ul className="divide-y divide-line">
      {placed.map((workload) => {
        const placement = findPlacement(projects.data, workload.environment_id, workload.project_id);
        return (
          <li key={workload.id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-0.5 px-4 py-2.5 text-[13px]">
            <Link to="/resources/$resourceId" params={{ resourceId: workload.id }} className={listLinkClass}>
              {workload.name}
            </Link>
            <span className="text-xs text-fg-muted">{placement ? `${placement.project.name} / ${placement.environment?.name ?? "Project resources"}` : "Project unavailable"}</span>
          </li>
        );
      })}
    </ul>
  );
}

function NodeDetail({ node }: { node: Node }) {
  const verify = useVerifyNode();
  const update = useUpdateNode(node.id);
  const renew = useRenewEnrollment(node.id);
  const [editing, setEditing] = useState(false);
  const [renewedFrom, setRenewedFrom] = useState<NodeState>("pending");
  const ssh = node.mode === "ssh";
  const admin = useSession()?.is_admin ?? false;
  const info = nodeStates[node.state];

  return (
    <>
      <PageHeader
        breadcrumbs={<Breadcrumbs><Link to="/nodes" className="rounded hover:text-fg">Nodes</Link>{node.name}</Breadcrumbs>}
        icon={<Server className="size-4" />}
        title={node.name}
        description={
          <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
            <NodeStateLabel state={node.state} />
            <span className="font-mono text-xs">{nodeAddress(node)}</span>
          </span>
        }
        actions={
          admin && <>
            {ssh && !editing && (
              <Button variant="ghost" onClick={() => setEditing(true)}>
                <Pencil aria-hidden className="size-3.5" />
                Edit SSH details
              </Button>
            )}
            {ssh && !renew.data && (
              <Button
                variant="secondary"
                pending={renew.isPending}
                onClick={() => {
                  setRenewedFrom(node.state);
                  renew.mutate();
                }}
              >
                {!renew.isPending && <KeyRound aria-hidden className="size-3.5" />}
                New setup command
              </Button>
            )}
            {canVerify(node) && (
              <Button variant="primary" pending={verify.isPending} onClick={() => verify.mutate(node.id)}>
                {!verify.isPending && <RefreshCw aria-hidden className="size-3.5" />}
                Verify
              </Button>
            )}
          </>
        }
      />

      {(verify.isError || verify.isSuccess || renew.isError || update.isSuccess) && (
        <div className="space-y-2 px-4 pt-4 sm:px-6" aria-live="polite">
          {verify.isError && (
            <Notice tone="error" title="Verification failed">
              {errorMessage(verify.error)}
            </Notice>
          )}
          {verify.isSuccess && (
            <Notice tone={verify.data.state === "ready" ? "success" : "warn"} title={`Verification finished: ${nodeStates[verify.data.state]?.label ?? verify.data.state}`}>
              {nodeStates[verify.data.state]?.description}
            </Notice>
          )}
          {renew.isError && (
            <Notice tone="error" title="Could not generate a setup command">
              {errorMessage(renew.error)}
            </Notice>
          )}
          {update.isSuccess && !editing && (
            <Notice tone="info" title="SSH details saved">
              Readiness was reset. Verify the node to confirm the main node can connect with the pinned host key.
            </Notice>
          )}
        </div>
      )}

      {renew.data && (
        <EnrollmentPanel enrollment={renew.data} nodeId={node.id} initialState={renewedFrom} onClose={() => renew.reset()} />
      )}

      <SectionGrid>
        <div className="space-y-5">
        {editing && (
          <Section title="SSH details" description="Changing the address keeps the pinned host key. The node must be verified again.">
            <NodeForm
              initial={{ name: node.name, address: node.address, port: node.port }}
              disabled={update.isPending}
              onSubmit={(input) => update.mutate(input, { onSuccess: () => setEditing(false) })}
            >
              {update.isError && (
                <Notice tone="error" title="Could not save SSH details">
                  {errorMessage(update.error)}
                </Notice>
              )}
              <div className="flex items-center gap-2">
                <Button type="submit" variant="primary" pending={update.isPending}>
                  Save
                </Button>
                <Button
                  variant="ghost"
                  onClick={() => {
                    update.reset();
                    setEditing(false);
                  }}
                >
                  Cancel
                </Button>
              </div>
            </NodeForm>
          </Section>
        )}

        <Section title="Status">
          <p className="text-[13px] text-fg-muted">{info?.description}</p>
          <p className="mt-2 text-xs text-fg-subtle">
            {node.last_checked_at ? (
              <>
                Connectivity last checked <RelativeTime value={node.last_checked_at} />. Cove re-checks nodes periodically.
              </>
            ) : (
              "Connectivity has not been checked yet."
            )}
          </p>
          {admin && node.state === "pending" && ssh && !renew.data && (
            <p className="mt-2 text-[13px] text-fg-muted">
              If the original setup command expired or failed, generate a new one. Earlier commands stop working.
            </p>
          )}
        </Section>

        {admin && <NodeUsage node={node} />}
        <Section title="Capabilities" description="Reported by the worker during the last verification.">
          <Capabilities node={node} />
        </Section>
        <BuildCacheSection node={node} />

        </div>
        <div className="space-y-5">
        {admin && <SharedNodeWarning />}

        {admin && <Section title="Connection">
          {node.enrollment_expires_at && <p className="mb-3 text-xs text-fg-muted"><ExpiryTime subject="Setup command" value={node.enrollment_expires_at} />. Generate a new command if it has expired.</p>}
          <DefinitionList
            items={[
              { term: "Transport", value: ssh ? "SSH from the main node" : "Local socket on the main node" },
              ...(ssh
                ? [
                    { term: "Address", value: <span className="font-mono text-xs">{node.address}</span> },
                    { term: "Port", value: <span className="font-mono text-xs">{node.port}</span> },
                  ]
                : []),
              { term: "Node ID", value: <span className="font-mono text-xs">{node.id}</span> },
            ]}
          />
        </Section>}

        {admin && <NodeLinks nodeId={node.id} />}

        {admin && (
          <Section title="Proxy configuration" description="Custom Traefik routers, middlewares, services and TLS settings alongside Cove's generated routes. Changes are validated before activation and apply to every team on this node.">
            <Link to="/nodes/$nodeId/proxy" params={{ nodeId: node.id }} className={buttonClass("secondary")}>
              <Network aria-hidden className="size-3.5" />
              Manage proxy configuration
            </Link>
          </Section>
        )}

        <Section title="Resources on this node" flush>
          <NodeResources nodeId={node.id} />
        </Section>
        {admin && <CleanupPreview nodeId={node.id} />}
        {admin && (
          <Section title="Danger zone" tone="danger">
            <DeleteResource kind="node" id={node.id} />
          </Section>
        )}
        </div>
      </SectionGrid>
    </>
  );
}

export function NodePage() {
  const { nodeId } = route.useParams();
  const nodes = useQuery(nodesQuery);

  if (nodes.isPending) return <LoadingRows label="Loading node" />;
  if (nodes.isError && !nodes.data) {
    return (
      <div className="px-4 py-5 sm:px-6">
        <QueryError what="the node" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />
      </div>
    );
  }
  const node = nodes.data.find((item) => item.id === nodeId);
  if (!node) {
    return (
      <>
        <PageHeader icon={<Server aria-hidden />} breadcrumbs={<Breadcrumbs><Link to="/nodes" className="rounded hover:text-fg">Nodes</Link>Not found</Breadcrumbs>} title="Node not found" />
        <EmptyState title="This node does not exist">It may have been removed, or the link is incorrect.</EmptyState>
      </>
    );
  }
  return <NodeDetail key={node.id} node={node} />;
}

function NodeUsage({ node }: { node: Node }) {
  const now = useNow(10_000);
  const metrics = node.capabilities?.metrics;
  const fresh = metrics && node.state === "ready" && now - new Date(metrics.sampled_at).getTime() < 180_000;
  const gib = (bytes: number) => `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
  return <Section title="Node usage" description="Host usage at the last probe. Disk covers the filesystem containing /var/lib/cove; separately mounted Docker or application disks are not included.">
    <div className="space-y-4">
      {fresh ? <>
        <MeterGroup label="Node usage" columns={3}>
          <Meter label="CPU" value={metrics.cpu_percent} limit={100} percentValue format={(value) => `${value.toFixed(1)}%`} />
          <Meter label="Memory used" value={metrics.memory_total - metrics.memory_available} limit={metrics.memory_total} format={gib} />
          <Meter label="Disk used" value={metrics.disk_total - metrics.disk_available} limit={metrics.disk_total} format={gib} detail={`${gib(metrics.disk_available)} available`} />
        </MeterGroup>
        <p className="text-xs text-fg-subtle">Sampled <RelativeTime value={metrics.sampled_at} />.</p>
      </> : <Notice>Current usage unavailable. Verify the node or upgrade its worker.</Notice>}
      {node.capabilities?.shared_host?.coolify_detected && <Notice tone="warn" title="Coolify also manages this host">Cove cannot verify Coolify cleanup settings from Docker. Before migration, pause legacy cleanup, auto-deploy, schedules and host patching. Coolify cleanup can remove unused Cove rollback images and unattached storage.</Notice>}
    </div>
  </Section>;
}
