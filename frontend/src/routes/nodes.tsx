import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus, RefreshCw, Server } from "lucide-react";
import { errorMessage } from "../api/client";
import { useEnableLocalNode, useVerifyNode } from "../api/mutations";
import { nodesQuery } from "../api/queries";
import type { Node } from "../api/types";
import { LocalNodeButton, LocalNodeResult } from "../components/LocalNode";
import { ExpiryTime, RelativeTime } from "../components/RelativeTime";
import { NodeStateLabel } from "../components/status";
import { nodeStates } from "../lib/nodes";
import { ClientPagination } from "../components/ListPagination";
import { Button, EmptyState, LoadingRows, PageHeader, QueryError, Section } from "../components/ui";
import { SharedNodeWarning } from "../components/Access";
import { Bastions } from "../components/Bastions";
import { CloudServers, ProviderTokens } from "../components/NodeProviders";
import { countLabel } from "../lib/hierarchy";
import { useSession } from "../lib/session";
import { buttonClass, listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { canVerify, nodeAddress, platform } from "../lib/nodes";

type VerifyMutation = ReturnType<typeof useVerifyNode>;

function Missing() {
  return <span className="font-sans text-fg-subtle">Unknown</span>;
}

function VerifyCell({ node, verify }: { node: Node; verify: VerifyMutation }) {
  const current = verify.variables === node.id;
  if (!canVerify(node)) {
    return <span className="text-xs text-fg-subtle">Awaiting setup</span>;
  }
  const pending = current && verify.isPending;
  return (
    <div className="flex flex-col items-end gap-1">
      <Button variant="ghost" size="sm" onClick={() => verify.mutate(node.id)} pending={pending} aria-label={`Verify ${node.name}`}>
        {!pending && <RefreshCw aria-hidden className="size-3.5" />}
        Verify
      </Button>
      {current && verify.isError && (
        <span role="alert" className="max-w-48 text-right text-xs text-bad">
          {errorMessage(verify.error)}
        </span>
      )}
    </div>
  );
}

function NodeStatus({ node }: { node: Node }) {
  return (
    <>
      <NodeStateLabel state={node.state} />
      {node.enrollment_expires_at && <p className="mt-1 text-xs text-fg-muted"><ExpiryTime subject="Setup command" value={node.enrollment_expires_at} /></p>}
      <div className="mt-0.5 text-xs text-fg-subtle">
        {node.last_checked_at ? (
          <>
            Checked <RelativeTime value={node.last_checked_at} />
          </>
        ) : (
          "Not checked yet"
        )}
      </div>
    </>
  );
}

function NodeRow({ node, verify, admin }: { node: Node; verify: VerifyMutation; admin: boolean }) {
  const capabilities = node.capabilities;
  return (
    <tr className={tableRowClass}>
      <td className="py-3 pr-4 pl-4">
        <Link
          to="/nodes/$nodeId"
          params={{ nodeId: node.id }}
          className={listLinkClass}
        >
          {node.name}
        </Link>
        <div className="mt-0.5 font-mono text-xs text-fg-muted md:hidden">{nodeAddress(node)}</div>
        <div className="mt-0.5 text-xs text-fg-subtle lg:hidden">{platform(node) ?? "Not probed"}</div>
        {/* Phones show status as full-width lines here instead of a narrow column. */}
        <div className="mt-2 sm:hidden" title={nodeStates[node.state]?.description}>
          <NodeStatus node={node} />
        </div>
      </td>
      <td className="hidden py-3 pr-4 font-mono text-xs text-fg-muted md:table-cell">{nodeAddress(node)}</td>
      <td className="hidden py-3 pr-4 text-[13px] text-fg-muted lg:table-cell">
        {platform(node) ?? <span className="text-fg-subtle">Not probed</span>}
      </td>
      <td className="hidden py-3 pr-4 font-mono text-xs text-fg-muted xl:table-cell">{capabilities?.docker || <Missing />}</td>
      <td className="hidden py-3 pr-4 font-mono text-xs text-fg-muted xl:table-cell">{capabilities?.compose || <Missing />}</td>
      <td className="hidden py-3 pr-4 sm:table-cell" title={nodeStates[node.state]?.description}>
        <NodeStatus node={node} />
      </td>
      {admin && (
        <td className="py-3 pr-4 text-right align-top sm:align-middle">
          <VerifyCell node={node} verify={verify} />
        </td>
      )}
    </tr>
  );
}

function AddNodeLink() {
  return (
    <Link to="/nodes/new" className={buttonClass("primary")}>
      <Plus aria-hidden className="size-3.5" />
      Add node
    </Link>
  );
}

function NoNodes({ admin }: { admin: boolean }) {
  if (!admin) return <EmptyState title="No nodes are assigned to this team">Ask an instance administrator to assign a node before creating resources.</EmptyState>;
  return (
    <EmptyState title="No nodes yet" action={<AddNodeLink />}>
      Add a Linux worker with systemd and OpenSSH. Cove generates a one-time setup command that installs the worker and registers its SSH host key.
    </EmptyState>
  );
}

function nodesDescription(admin: boolean) {
  return admin
    ? "Shared infrastructure for every team. The main node reaches workers over SSH with a pinned host key."
    : "Nodes assigned to this team. Instance administrators add, verify and assign nodes.";
}

export function NodesPage() {
  const nodes = useQuery(nodesQuery);
  const verify = useVerifyNode();
  const local = useEnableLocalNode();
  const hasLocal = nodes.data?.some((node) => node.mode === "local") ?? true;
  const admin = useSession()?.is_admin ?? false;

  return (
    <>
      <PageHeader
        icon={<Server aria-hidden />}
        title="Nodes"
        description={nodesDescription(admin)}
        actions={
          admin && (
            <>
              {!hasLocal && <LocalNodeButton mutation={local} />}
              <AddNodeLink />
            </>
          )
        }
      />
      {(local.isError || local.isSuccess) && (
        <div className="px-4 pt-4 sm:px-6">
          <LocalNodeResult mutation={local} />
        </div>
      )}
      {nodes.isPending ? (
        <LoadingRows label="Loading nodes" />
      ) : nodes.isError && !nodes.data ? (
        <div className="px-4 py-5 sm:px-6">
          <QueryError what="nodes" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />
        </div>
      ) : nodes.data.length === 0 ? (
        <NoNodes admin={admin} />
      ) : (
        <>
          {nodes.isRefetchError && (
            <div className="px-4 pt-4 sm:px-6">
              <QueryError what="the latest node state" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />
            </div>
          )}
          {admin && (
            <div className="px-4 pt-5 sm:px-6">
              <SharedNodeWarning />
            </div>
          )}
          <Section title={countLabel(nodes.data.length, "node")} flush>
            <ClientPagination rows={nodes.data} label="Node pages" noun="nodes">
              {(rows) => (
            <table className="w-full text-left">
              <caption className="sr-only">Nodes</caption>
              <thead>
                <tr className={tableHeadClass}>
                  <th scope="col" className="py-2 pr-4 pl-4 font-medium">
                    Name
                  </th>
                  <th scope="col" className="hidden py-2 pr-4 font-medium md:table-cell">
                    Address
                  </th>
                  <th scope="col" className="hidden py-2 pr-4 font-medium lg:table-cell">
                    Platform
                  </th>
                  <th scope="col" className="hidden py-2 pr-4 font-medium xl:table-cell">
                    Docker
                  </th>
                  <th scope="col" className="hidden py-2 pr-4 font-medium xl:table-cell">
                    Compose
                  </th>
                  <th scope="col" className="hidden py-2 pr-4 font-medium sm:table-cell">
                    Status
                  </th>
                  {admin && (
                    <th scope="col" className="py-2 pr-4 text-right font-medium">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {rows.map((node) => (
                  <NodeRow key={node.id} node={node} verify={verify} admin={admin} />
                ))}
              </tbody>
            </table>
              )}
            </ClientPagination>
          </Section>
        </>
      )}
      {admin && <CloudServers />}
      {admin && <ProviderTokens />}
      {admin && <Bastions />}
    </>
  );
}
