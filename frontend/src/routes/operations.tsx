import { Activity } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { operationsQuery, workloadsQuery } from "../api/queries";
import { FilterSelect, FilterToolbar, NoMatches } from "../components/FilterToolbar";
import { ClientPagination } from "../components/ListPagination";
import { OperationsTable } from "../components/OperationsTable";
import { EmptyState, LoadingRows, PageHeader, QueryError, Section } from "../components/ui";
import { actionLabels, isActive, matchesSearch, parseOperationsSearch, stateFilters, type OperationsSearch } from "../lib/operations";

const route = getRouteApi("/app/operations");

export function OperationsPage() {
  const search = route.useSearch();
  const navigate = useNavigate({ from: "/operations" });
  const operations = useQuery(operationsQuery);
  const workloads = useQuery(workloadsQuery);
  const [text, setText] = useState("");

  const names = useMemo(() => new Map((workloads.data ?? []).map((workload) => [workload.id, workload.name])), [workloads.data]);
  const query = text.trim().toLowerCase();
  const filtered = useMemo(
    () =>
      (operations.data ?? []).filter((operation) => {
        if (!matchesSearch(operation, search)) return false;
        if (!query) return true;
        const action = actionLabels[operation.action] ?? operation.action;
        return [action, names.get(operation.workload_id) ?? "", operation.id].some((value) => value.toLowerCase().includes(query));
      }),
    [operations.data, search, query, names],
  );
  const filtering = Boolean(search.state || search.workload || query);
  const activeCount = operations.data?.filter((operation) => isActive(operation.state)).length ?? 0;

  function setFilter(patch: Partial<OperationsSearch>) {
    void navigate({ search: (current) => parseOperationsSearch({ ...current, ...patch }), replace: true });
  }

  function reset() {
    setText("");
    void navigate({ search: {}, replace: true });
  }

  let body;
  if (operations.isPending) {
    body = <LoadingRows label="Loading operations" />;
  } else if (operations.isError && !operations.data) {
    body = (
      <div className="px-4 py-5 sm:px-6">
        <QueryError what="operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
      </div>
    );
  } else if (operations.data.length === 0) {
    body = <EmptyState title="No operations yet">Operations appear here when a resource is deployed, started, stopped, restarted, backed up or restored.</EmptyState>;
  } else {
    body = (
      <>
        {operations.isRefetchError && (
          <div className="px-4 pt-4 sm:px-6">
            <QueryError what="the latest operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
          </div>
        )}
        <Section title="Recent operations" description="Search and filters apply to the 100 most recent operations." flush>
          <FilterToolbar
            label="Filter operations"
            search={text}
            onSearch={setText}
            searchPlaceholder="Search action, resource or ID"
            active={filtering}
            onReset={reset}
            summary={filtering ? `${filtered.length} of ${operations.data.length} operations` : `${operations.data.length} operations`}
          >
            <FilterSelect id="filter-state" label="Status" value={search.state ?? "all"} onChange={(value) => setFilter({ state: value as OperationsSearch["state"] })}>
              {Object.entries(stateFilters).map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </FilterSelect>
            <FilterSelect id="filter-workload" label="Resource" value={search.workload ?? ""} onChange={(value) => setFilter({ workload: value })}>
              <option value="">All resources</option>
              {(workloads.data ?? []).map((workload) => (
                <option key={workload.id} value={workload.id}>
                  {workload.name}
                </option>
              ))}
            </FilterSelect>
          </FilterToolbar>
          {filtered.length === 0 ? (
            <NoMatches what="operations" scope="None of the 100 most recent operations match the search and filters." onReset={reset} />
          ) : (
            <ClientPagination rows={filtered} resetKey={`${query}|${search.state ?? ""}|${search.workload ?? ""}`} label="Operation pages" noun="operations">
              {(rows) => <OperationsTable operations={rows} workloadNames={names} caption="Operations" />}
            </ClientPagination>
          )}
        </Section>
      </>
    );
  }

  return (
    <>
      <PageHeader
        icon={<Activity aria-hidden />}
        title="Operations"
        description={
          activeCount > 0
            ? `${activeCount} active. The 100 most recent operations, refreshed automatically.`
            : "The 100 most recent operations on resources you can access."
        }
      />
      {body}
    </>
  );
}
