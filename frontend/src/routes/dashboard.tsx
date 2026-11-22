import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { LayoutDashboard } from "lucide-react";
import { projectsQuery } from "../api/projects";
import { nodesQuery, operationsQuery, workloadsQuery } from "../api/queries";
import { OperationsTable } from "../components/OperationsTable";
import { EmptyState, LoadingRows, PageHeader, QueryError, Section } from "../components/ui";
import { linkClass } from "../lib/styles";

function Stat({ label, to, value, total }: { label: string; to: "/projects" | "/resources" | "/nodes"; value: number; total?: number }) {
  return (
    <div className="border-t border-line-strong pt-3">
      <dt className="text-[13px] text-fg-muted">
        <Link to={to} className="rounded hover:text-fg hover:underline hover:decoration-fg-subtle hover:underline-offset-4">
          {label}
        </Link>
      </dt>
      <dd className="mt-1.5 text-2xl font-semibold text-fg tabular-nums">
        {value}
        {total !== undefined && <span className="text-sm font-normal text-fg-muted"> of {total}</span>}
      </dd>
    </div>
  );
}

export function DashboardPage() {
  const projects = useQuery(projectsQuery);
  const resources = useQuery(workloadsQuery);
  const nodes = useQuery(nodesQuery);
  const operations = useQuery(operationsQuery);
  const queries = [projects, resources, nodes, operations];
  const failed = queries.find((query) => query.isError);
  return <>
    <PageHeader icon={<LayoutDashboard aria-hidden />} title="Dashboard" description="Projects, infrastructure, and recent activity for your current team." />
    {failed ? <Section title="Overview"><QueryError what="dashboard data" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} /></Section>
      : queries.some((query) => query.isPending) ? <LoadingRows label="Loading dashboard" />
      : <>
        <dl className="grid grid-cols-1 gap-x-8 gap-y-4 px-4 pt-6 min-[420px]:grid-cols-3 sm:px-6">
          <Stat label="Projects" to="/projects" value={projects.data?.length ?? 0} />
          <Stat label="Resources" to="/resources" value={resources.data?.length ?? 0} />
          <Stat label="Ready nodes" to="/nodes" value={nodes.data?.filter((node) => node.state === "ready").length ?? 0} total={nodes.data?.length ?? 0} />
        </dl>
        <Section title="Recent operations" actions={<Link to="/operations" className={linkClass}>View all</Link>} flush>
          {operations.data?.length ? <OperationsTable operations={operations.data.slice(0, 10)} workloadNames={new Map(resources.data?.map((resource) => [resource.id, resource.name]))} caption="Recent operations" /> : <EmptyState title="No operations yet">Deployment and other resource operations will appear here.</EmptyState>}
        </Section>
      </>}
  </>;
}
