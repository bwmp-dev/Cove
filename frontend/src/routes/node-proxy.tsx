import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { getRouteApi, Link } from "@tanstack/react-router";
import { FilePlus2, Network, RotateCw } from "lucide-react";
import { useState } from "react";
import { nodesQuery } from "../api/queries";
import { proxyConfigQuery, unsettled, type ProxyConfig } from "../api/proxy";
import type { Node } from "../api/types";
import { ProxyTraffic } from "../components/ProxyTraffic";
import { CloudflareTunnel } from "../components/CloudflareTunnel";
import { ManagedListeners } from "../components/ManagedListeners";
import { ChangeOutcome, LastChange, ProxyFileEditor, ProxyFiles, StaticSettings } from "../components/ProxyConfig";
import { Breadcrumbs, Button, EmptyState, LoadingRows, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { useSession } from "../lib/session";

const route = getRouteApi("/app/nodes/$nodeId/proxy");

function Header({ node }: { node?: Node }) {
  return (
    <PageHeader
      breadcrumbs={
        <Breadcrumbs>
          <Link to="/nodes" className="rounded hover:text-fg">
            Nodes
          </Link>
          {node ? (
            <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className="rounded hover:text-fg">
              {node.name}
            </Link>
          ) : (
            "Not found"
          )}
          Proxy configuration
        </Breadcrumbs>
      }
      icon={<Network className="size-4" />}
      title="Proxy configuration"
      description={node ? `Custom Traefik dynamic configuration on ${node.name}.` : undefined}
    />
  );
}

function ProxyNotices({ config }: { config: ProxyConfig }) {
  const change = config.change;
  const attention = unsettled(change) || change?.state === "failed" || change?.state === "interrupted";
  return (
    <div className="space-y-2 px-4 pt-5 sm:px-6" aria-live="polite">
      <Notice tone="warn" title="This proxy serves every team on this node">
        Changes affect all resources routed through it. Cove checks each dynamic configuration file change in a separate Traefik instance, applies it without restarting the proxy, and restores the previous file if the live proxy does not load it. Listener changes below restart the proxy.
      </Notice>
      {config.unavailable && <Notice tone="error" title="The proxy cannot be managed">{config.unavailable}</Notice>}
      {config.blocked && <Notice tone="warn" title="Changes are paused">{config.blocked}</Notice>}
      {config.notice && <Notice>{config.notice}</Notice>}
      {attention && change && <ChangeOutcome change={change} />}
    </div>
  );
}

type ConfigQuery = UseQueryResult<ProxyConfig>;

function DynamicConfiguration({ node, config }: { node: Node; config: ConfigQuery }) {
  // undefined is a new file; null closes the editor.
  const [editing, setEditing] = useState<string | undefined | null>(null);

  if (config.isPending) return <LoadingRows label="Loading proxy configuration" />;
  if (config.isError) {
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what="the proxy configuration" error={config.error} onRetry={() => void config.refetch()} retrying={config.isFetching} />
      </div>
    );
  }
  const data: ProxyConfig = config.data;
  const change = data.change;
  const names = data.files.map((file) => file.name);
  return (
    <>
      <ProxyNotices config={data} />

      {data.available && (
        <Section
          title="Dynamic configuration files"
          description={<LastChange change={change} />}
          flush
          actions={
            <>
              <Button variant="ghost" onClick={() => void config.refetch()} pending={config.isFetching}>
                {!config.isFetching && <RotateCw aria-hidden className="size-3.5" />}
                Refresh
              </Button>
              <Button onClick={() => setEditing(undefined)} disabled={!data.running || !!data.blocked || editing === undefined}>
                <FilePlus2 aria-hidden className="size-3.5" />
                New file
              </Button>
            </>
          }
        >
          <ProxyFiles files={data.files} selected={editing ?? null} onSelect={setEditing} />
        </Section>
      )}

      {editing !== null && (
        <ProxyFileEditor
          key={editing ?? "new file"}
          nodeId={node.id}
          name={editing}
          existing={names}
          blocked={data.blocked || (data.running ? undefined : "The proxy is stopped.")}
          onClose={(opened) => setEditing(opened ?? null)}
        />
      )}
    </>
  );
}

// The listener section keeps one position whatever state the proxy configuration query is in, so loading,
// refreshing or failing that query never remounts it and discards an unsaved listener draft or a reviewed plan.
function ProxyConfiguration({ node }: { node: Node }) {
  const config = useQuery(proxyConfigQuery(node.id));
  return (
    <>
      <DynamicConfiguration node={node} config={config} />
      <ManagedListeners node={node} />
      <CloudflareTunnel node={node} />
      {config.isSuccess && config.data.available && (
        <Section
          title="Static configuration"
          description="Read from the running proxy. Published ports, HTTP/3 and extra entry points are changed under Listeners and published ports, which recreates the proxy. Certificate resolvers cannot be changed here."
        >
          <StaticSettings config={config.data} />
        </Section>
      )}
    </>
  );
}

export function NodeProxyPage() {
  const { nodeId } = route.useParams();
  const admin = useSession()?.is_admin ?? false;
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
        <Header />
        <EmptyState title="This node does not exist">It may have been removed, or the link is incorrect.</EmptyState>
      </>
    );
  }
  if (!admin) {
    return (
      <>
        <Header node={node} />
        <EmptyState title="Instance administrators only">A node's proxy routes every team's resources on it, so only instance administrators can view or change its configuration.</EmptyState>
      </>
    );
  }
  return (
    <>
      <Header node={node} />
      <ProxyConfiguration node={node} />
      <ProxyTraffic node={node} />
    </>
  );
}
