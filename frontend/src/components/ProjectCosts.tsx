import { useQuery } from "@tanstack/react-query";
import { projectCostsQuery, type ProjectCostItem } from "../api/advice";
import { isStatus } from "../api/client";
import { byEnvironment, formatMinor, rollup, type CostRollup, type EnvironmentCosts } from "../lib/costs";
import { countLabel } from "../lib/hierarchy";
import { tableHeadClass, tableRowClass } from "../lib/styles";
import { LoadingRows, Notice, QueryError, Section } from "./ui";

function Totals({ costs }: { costs: CostRollup }) {
  if (costs.totals.length === 0) return <span className="text-fg-muted">Unknown</span>;
  return (
    <span className="tabular-nums">
      {costs.totals.map((total, index) => (
        <span key={total.currency}>
          {index > 0 && <span className="text-fg-subtle"> + </span>}
          <span className="whitespace-nowrap">{formatMinor(total.minor, total.currency)}</span>
        </span>
      ))}
      {costs.unpriced > 0 && <span className="text-fg-muted"> and unknown</span>}
    </span>
  );
}

function Share({ item }: { item: ProjectCostItem }) {
  if (item.currency === null || item.monthly_minor === null) return <span className="text-fg-muted">No node price</span>;
  return (
    <span className="whitespace-nowrap">
      {formatMinor(item.monthly_minor, item.currency)}
      {item.monthly_minor === 0 && <span className="text-fg-muted"> (free)</span>}
    </span>
  );
}

function CostSummary({ total, environmentId }: { total: CostRollup; environmentId?: string }) {
  return (
    <div className="space-y-1 border-b border-line px-4 py-3">
      <p className="text-[13px] text-fg">
        <span className="text-fg-muted">{environmentId === undefined ? "Project total" : "Environment total"}: </span>
        <Totals costs={total} />
        <span className="text-fg-muted"> per month for {countLabel(total.resources, "resource")}</span>
      </p>
      {total.unpriced > 0 && (
        <p className="text-xs text-fg-muted">
          {countLabel(total.unpriced, "resource")} {total.unpriced === 1 ? "runs" : "run"} on nodes without a price, so {total.unpriced === 1 ? "its" : "their"} cost is unknown and not included.
        </p>
      )}
      {total.totals.length > 1 && <p className="text-xs text-fg-muted">Nodes are priced in different currencies, so totals are listed per currency.</p>}
    </div>
  );
}

// Node names are not unique; the API returns one row per environment and node ID.
function CostTable({ groups, environmentId }: { groups: EnvironmentCosts[]; environmentId?: string }) {
  return (
    <table className="w-full table-fixed text-left">
      <caption className="sr-only">Estimated monthly cost by environment and node</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="py-2 pr-4 pl-4">
            Node
          </th>
          <th scope="col" className="hidden w-28 py-2 pr-4 text-right sm:table-cell">
            Resources
          </th>
          <th scope="col" className="w-40 py-2 pr-4 text-right sm:w-48">
            Monthly share
          </th>
        </tr>
      </thead>
      {groups.map((group) => (
        <tbody key={group.id || "project"}>
          {environmentId === undefined && (
            <tr className="border-b border-line bg-panel">
              <th scope="rowgroup" className="py-2 pr-4 pl-4 text-[13px] font-semibold text-fg">
                {group.name}
              </th>
              <td className="hidden py-2 pr-4 text-right text-xs text-fg-muted tabular-nums sm:table-cell">{group.resources}</td>
              <td className="py-2 pr-4 text-right text-[13px] font-medium">
                <Totals costs={group} />
              </td>
            </tr>
          )}
          {group.items.map((item) => (
            <tr key={item.node_id} className={tableRowClass}>
              <td className="py-2 pr-4 pl-4 text-[13px] text-fg">
                <span className="break-words">{item.node_name}</span>
                <span className="block text-xs text-fg-muted sm:hidden">{countLabel(item.resources, "resource")}</span>
              </td>
              <td className="hidden py-2 pr-4 text-right text-[13px] text-fg-muted tabular-nums sm:table-cell">{item.resources}</td>
              <td className="py-2 pr-4 text-right text-[13px] tabular-nums">
                <Share item={item} />
              </td>
            </tr>
          ))}
        </tbody>
      ))}
    </table>
  );
}

const explanation =
  "Counts every configured resource placement, including resources that are not deployed. It is an estimate from node prices set by instance administrators, not provider usage or billing.";

// Allocated shares only; currencies are kept apart and never converted.
export function ProjectCosts({ projectId, environmentId }: { projectId: string; environmentId?: string }) {
  const costs = useQuery(projectCostsQuery(projectId));
  const all = costs.data?.items ?? [];
  const items = environmentId === undefined ? all : all.filter((item) => item.environment_id === environmentId);
  const total = rollup(items);

  return (
    <Section title="Estimated monthly cost" description={`${costs.data?.allocation ?? "Equal share per configured resource on a node; currencies are not converted."} ${explanation}`} flush>
      {costs.isPending ? (
        <LoadingRows label="Loading cost estimate" rows={2} />
      ) : costs.isError ? (
        <div className="p-3">
          {isStatus(costs.error, 403) ? (
            <Notice title="Cost estimates need project access">Ask a team owner or admin for viewer access to the whole project.</Notice>
          ) : (
            <QueryError what="the cost estimate" error={costs.error} onRetry={() => void costs.refetch()} retrying={costs.isFetching} />
          )}
        </div>
      ) : items.length === 0 ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted">No resources are configured here, so there is nothing to allocate.</p>
      ) : (
        <>
          <CostSummary total={total} environmentId={environmentId} />
          <CostTable groups={byEnvironment(items)} environmentId={environmentId} />
          <p className="border-t border-line px-4 py-2.5 text-xs text-fg-muted">Shares are rounded to the currency&apos;s minor unit for display; totals are added before rounding.</p>
        </>
      )}
    </Section>
  );
}
