import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Boxes, Tags, X } from "lucide-react";
import { useEffect, useId, useState, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { bulkActions, resourceBrowserQuery, useBulkAction, useSetTags, type BrowserFilters, type BrowserPage, type BrowserResource, type BulkAction, type BulkResult } from "../api/organization";
import { projectsQuery } from "../api/projects";
import { nodesQuery } from "../api/queries";
import { Checkbox } from "../components/Checkbox";
import { ConfirmDialog, Dialog } from "../components/ConfirmDialog";
import { FilterSelect, FilterToolbar, NoMatches } from "../components/FilterToolbar";
import { PaginationControls } from "../components/ListPagination";
import { PinButton } from "../components/PinButton";
import { Select } from "../components/Select";
import { Breadcrumbs, Button, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { countLabel } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { maxBulkSelection, parseTags, summarizeBulk } from "../lib/organization";
import { actionLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { inputClass, linkClass, listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";

const initialFilters: BrowserFilters = { q: "", tag: "", project_id: "", environment_id: "", node_id: "", pinned: false, page: 1, page_size: 25 };
const pageSizes = [25, 50];
const maxSearchBytes = 200;

interface BrowserState {
  filters: BrowserFilters;
  selected: Set<string>;
}

const sameFilters = (a: BrowserFilters, b: BrowserFilters) => (Object.keys(a) as (keyof BrowserFilters)[]).every((key) => a[key] === b[key]);

// Any change of filters or page drops the selection, so nothing stays selected out of sight.
function withFilters(patch: Partial<BrowserFilters>) {
  return (current: BrowserState): BrowserState => {
    const next = { ...current.filters, ...patch, page: patch.page ?? 1 };
    return sameFilters(next, current.filters) ? current : { filters: next, selected: new Set() };
  };
}

function TagEditor({ resource, onClose }: { resource: BrowserResource; onClose: () => void }) {
  const id = useId();
  const [text, setText] = useState(() => resource.tags.join(" "));
  const setTags = useSetTags();
  const parsed = parseTags(text);
  const error = parsed.error ?? (setTags.isError ? errorMessage(setTags.error) : undefined);
  return (
    <Dialog open title={`Tags for ${resource.name}`} onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (parsed.error) return;
          setTags.mutate({ id: resource.id, tags: parsed.tags }, { onSuccess: onClose });
        }}
      >
        <Field id={id} label="Tags" hint="Separate tags with spaces or commas. Up to 12 lowercase tags; everyone with access to the resource sees them." error={error}>
          <input id={id} className={inputClass} value={text} onChange={(event) => setText(event.target.value)} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : `${id}-hint`} autoComplete="off" spellCheck={false} />
        </Field>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={setTags.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" pending={setTags.isPending} disabled={Boolean(parsed.error)}>
            Save tags
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function ResourceRow({ resource, selected, locked, onSelect, onTag, onEditTags }: { resource: BrowserResource; selected: boolean; locked: boolean; onSelect: (checked: boolean) => void; onTag: (tag: string) => void; onEditTags: () => void }) {
  const operable = can(resource.permissions, "operate");
  const placement = resource.environment ? `${resource.project} / ${resource.environment}` : `${resource.project} / Project resources`;
  return (
    <tr className={`${tableRowClass} ${selected ? "bg-info-tint" : ""}`}>
      <td className="w-10 py-2 pl-3 align-top sm:align-middle">
        <Checkbox
          checked={selected}
          disabled={!operable || locked}
          onChange={(event) => onSelect(event.target.checked)}
          aria-label={operable ? `Select ${resource.name}` : `${resource.name}: you cannot run operations on this resource`}
          title={operable ? undefined : "You can view this resource but not run operations on it."}
        />
      </td>
      <td className="py-2 pr-4">
        <Link to="/resources/$resourceId" params={{ resourceId: resource.id }} className={`${listLinkClass} break-words`}>
          {resource.name}
        </Link>
        <p className="mt-0.5 text-xs text-fg-muted">
          {resource.kind === "job" ? "Job" : "Service"}
          <span className="lg:hidden"> · {placement}</span>
          <span className="md:hidden"> · {resource.node}</span>
        </p>
        {resource.tags.length > 0 && (
          <ul aria-label={`Tags for ${resource.name}`} className="mt-1 flex flex-wrap gap-1">
            {resource.tags.map((tag) => (
              <li key={tag}>
                <button type="button" onClick={() => onTag(tag)} className="rounded-md border border-line px-1.5 font-mono text-[11px] text-fg-muted hover:border-line-strong hover:text-fg pointer-coarse:min-h-8" title={`Show resources tagged ${tag}`}>
                  {tag}
                </button>
              </li>
            ))}
          </ul>
        )}
      </td>
      <td className="hidden py-2 pr-4 text-[13px] break-words text-fg-muted lg:table-cell">{placement}</td>
      <td className="hidden py-2 pr-4 text-[13px] break-words text-fg-muted md:table-cell">{resource.node}</td>
      <td className="py-2 pr-3 text-right">
        <div className="flex flex-wrap justify-end gap-1">
          {can(resource.permissions, "edit") && (
            <Button size="sm" variant="ghost" onClick={onEditTags} aria-label={`Edit tags for ${resource.name}`} title={`Edit tags for ${resource.name}`}>
              <Tags aria-hidden className="size-3.5" />
              <span className="hidden sm:inline">Tags</span>
            </Button>
          )}
          <PinButton kind="workload" id={resource.id} name={resource.name} size="sm" />
        </div>
      </td>
    </tr>
  );
}

function BulkResults({ results, names, action, onDismiss }: { results: BulkResult[]; names: Map<string, string>; action: BulkAction; onDismiss: () => void }) {
  const { queued, failed } = summarizeBulk(results);
  return (
    <Section
      title={`${actionLabels[action]} results`}
      description={`${countLabel(queued, "operation")} queued, ${failed} not queued. Queued operations run on their own; open each one to see whether it succeeded.`}
      actions={
        <Button size="sm" variant="ghost" onClick={onDismiss}>
          <X aria-hidden className="size-3.5" />
          Dismiss
        </Button>
      }
      flush
    >
      <ul aria-label="Bulk action results" className="divide-y divide-line">
        {results.map((result) => (
          <li key={result.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-2.5">
            <span className="min-w-0 flex-1 basis-40 text-[13px] break-words text-fg">{names.get(result.id) ?? result.id}</span>
            {result.status === 202 ? (
              <span className="text-xs text-fg-muted">
                Queued
                {result.operation_id && (
                  <>
                    {" · "}
                    <Link to="/operations/$operationId" params={{ operationId: result.operation_id }} className={linkClass}>
                      View operation
                    </Link>
                  </>
                )}
              </span>
            ) : (
              <span className="min-w-0 basis-full text-xs break-words text-bad sm:basis-auto">
                Not queued ({result.status}){result.error ? `: ${result.error}` : ""}
              </span>
            )}
          </li>
        ))}
      </ul>
    </Section>
  );
}

function BulkBar({ count, action, online, locked, onAction, onReview, onClear }: { count: number; action: BulkAction; online: boolean; locked: boolean; onAction: (action: BulkAction) => void; onReview: () => void; onClear: () => void }) {
  return (
    <div role="region" aria-label="Bulk actions" className="flex flex-wrap items-center gap-2 border-b border-line bg-info-tint px-3 py-2">
      <p className="text-[13px] font-medium text-fg tabular-nums">{countLabel(count, "resource")} selected</p>
      <div className="min-w-0 flex-1 basis-36 sm:w-40 sm:flex-none sm:basis-auto">
        <label htmlFor="bulk-action" className="sr-only">
          Action
        </label>
        <Select id="bulk-action" aria-label="Action" value={action} onChange={(event) => onAction(event.target.value as BulkAction)}>
          {bulkActions.map((value) => (
            <option key={value} value={value}>
              {actionLabels[value]}
            </option>
          ))}
        </Select>
      </div>
      <Button size="sm" variant="primary" onClick={onReview} disabled={!online || locked}>
        Review {actionLabels[action].toLowerCase()}
      </Button>
      <Button size="sm" variant="ghost" onClick={onClear}>
        Clear selection
      </Button>
    </div>
  );
}

interface BulkConfirmProps {
  open: boolean;
  action: BulkAction;
  items: BrowserResource[];
  pending: boolean;
  error: unknown;
  onConfirm: () => void;
  onCancel: () => void;
}

function BulkConfirm({ open, action, items, pending, error, onConfirm, onCancel }: BulkConfirmProps) {
  const jobs = action === "restart" ? items.filter((item) => item.kind === "job").length : 0;
  const subject = `${actionLabels[action]} ${countLabel(items.length, "resource")}`;
  return (
    <ConfirmDialog open={open} title={`${subject}?`} confirmLabel={subject} danger={action === "stop"} pending={pending} onConfirm={onConfirm} onCancel={onCancel}>
      <div className="space-y-3">
        {action === "deploy" && (
          <Notice tone="warn" title="Deploy without a per-resource review">
            Each resource deploys its saved configuration without the deploy review on its own page. Open a resource's Preflight first if you need to check its plan.
          </Notice>
        )}
        {jobs > 0 && <Notice tone="warn">{countLabel(jobs, "selected job")} cannot restart and will be refused.</Notice>}
        <p>Each resource is queued separately with its own permission check and audit entry. Some can be refused while others are queued; nothing is rolled back or retried.</p>
        <ul aria-label="Selected resources" className="max-h-48 divide-y divide-line overflow-y-auto rounded-md border border-line">
          {items.map((item) => (
            <li key={item.id} className="px-2.5 py-1.5 text-[13px] break-words text-fg">
              {item.name}
            </li>
          ))}
        </ul>
        {error !== null && error !== undefined && <Notice tone="error">{errorMessage(error)}</Notice>}
      </div>
    </ConfirmDialog>
  );
}

interface ToolbarProps {
  filters: BrowserFilters;
  text: string;
  tagText: string;
  filtering: boolean;
  summary: string;
  onText: (value: string) => void;
  onTagText: (value: string) => void;
  onUpdate: (patch: Partial<BrowserFilters>) => void;
  onReset: () => void;
}

function BrowserToolbar({ filters, text, tagText, filtering, summary, onText, onTagText, onUpdate, onReset }: ToolbarProps) {
  const projects = useQuery(projectsQuery);
  const nodes = useQuery(nodesQuery);
  const project = projects.data?.find((item) => item.id === filters.project_id);
  return (
    <FilterToolbar label="Filter resources" search={text} onSearch={onText} searchPlaceholder="Search by name" active={filtering} onReset={onReset} summary={summary}>
      <div className="min-w-0 flex-1 basis-36 sm:w-40 sm:flex-none sm:basis-auto">
        <label htmlFor="browser-tag" className="sr-only">
          Tag
        </label>
        <input id="browser-tag" className={`${inputClass} min-h-9 pointer-coarse:min-h-11`} placeholder="Tag" value={tagText} onChange={(event) => onTagText(event.target.value)} autoComplete="off" spellCheck={false} maxLength={32} />
      </div>
      <FilterSelect id="browser-project" label="Project" value={filters.project_id} onChange={(value) => onUpdate({ project_id: value, environment_id: "" })}>
        <option value="">All projects</option>
        {(projects.data ?? []).map((item) => (
          <option key={item.id} value={item.id}>
            {item.name}
          </option>
        ))}
      </FilterSelect>
      {project && project.environments.length > 0 && (
        <FilterSelect id="browser-environment" label="Environment" value={filters.environment_id} onChange={(value) => onUpdate({ environment_id: value })}>
          <option value="">All environments</option>
          {project.environments.map((environment) => (
            <option key={environment.id} value={environment.id}>
              {environment.name}
            </option>
          ))}
        </FilterSelect>
      )}
      {(nodes.data?.length ?? 0) > 1 && (
        <FilterSelect id="browser-node" label="Node" value={filters.node_id} onChange={(value) => onUpdate({ node_id: value })}>
          <option value="">All nodes</option>
          {(nodes.data ?? []).map((node) => (
            <option key={node.id} value={node.id}>
              {node.name}
            </option>
          ))}
        </FilterSelect>
      )}
      <label className="flex min-h-9 items-center gap-2 text-[13px] text-fg-muted pointer-coarse:min-h-11">
        <Checkbox checked={filters.pinned} onChange={(event) => onUpdate({ pinned: event.target.checked })} />
        Pinned only
      </label>
      <FilterSelect id="browser-page-size" label="Resources per page" value={String(filters.page_size)} onChange={(value) => onUpdate({ page_size: Number(value) })}>
        {pageSizes.map((size) => (
          <option key={size} value={size}>
            {size} per page
          </option>
        ))}
      </FilterSelect>
    </FilterToolbar>
  );
}

interface ResultsTableProps {
  data: BrowserPage;
  stale: boolean;
  locked: boolean;
  selected: Set<string>;
  onSelect: (ids: string[], checked: boolean) => void;
  onTag: (tag: string) => void;
  onEditTags: (resource: BrowserResource) => void;
  onPage: (page: number) => void;
}

function ResultsTable({ data, stale, locked, selected, onSelect, onTag, onEditTags, onPage }: ResultsTableProps) {
  const items = data.items;
  const eligible = items.filter((item) => can(item.permissions, "operate"));
  const allSelected = eligible.length > 0 && eligible.every((item) => selected.has(item.id));
  const pageCount = data.total > 0 ? Math.ceil(data.total / data.page_size) : undefined;
  const start = (data.page - 1) * data.page_size + 1;
  return (
    <>
      <table className="w-full text-left" aria-busy={stale || undefined}>
        <caption className="sr-only">Resources, page {data.page}</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="w-10 pl-3">
              <Checkbox
                checked={allSelected}
                disabled={eligible.length === 0 || locked}
                onChange={(event) => onSelect(eligible.slice(0, maxBulkSelection).map((item) => item.id), event.target.checked)}
                aria-label="Select every resource on this page that you can operate"
              />
            </th>
            <th scope="col" className="pr-4">
              Resource
            </th>
            <th scope="col" className="hidden pr-4 lg:table-cell">
              Project / environment
            </th>
            <th scope="col" className="hidden pr-4 md:table-cell">
              Node
            </th>
            <th scope="col" className="pr-3 text-right">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className={stale ? "text-fg-muted" : ""}>
          {items.map((item) => (
            <ResourceRow key={item.id} resource={item} selected={selected.has(item.id)} locked={locked} onSelect={(checked) => onSelect([item.id], checked)} onTag={onTag} onEditTags={() => onEditTags(item)} />
          ))}
        </tbody>
      </table>
      <PaginationControls
        label="Resource pages"
        summary={data.total > 0 ? `Showing ${start}-${start + items.length - 1} of ${countLabel(data.total, "resource")}` : `${countLabel(items.length, "resource")} on this page`}
        page={data.page}
        pageCount={pageCount}
        hasPrevious={data.page > 1}
        hasNext={data.has_more}
        onPage={onPage}
      />
    </>
  );
}

function EmptyPage({ onFirstPage }: { onFirstPage: () => void }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-6">
      <p className="text-[13px] text-fg-muted">There are no resources on this page.</p>
      <Button size="sm" onClick={onFirstPage}>
        Go to the first page
      </Button>
    </div>
  );
}

function NoResources() {
  return (
    <EmptyState
      title="No resources yet"
      action={
        <Link to="/resources" className={linkClass}>
          Open all resources
        </Link>
      }
    >
      Resources you can access appear here once they exist.
    </EmptyState>
  );
}

type Outcome = { action: BulkAction; names: Map<string, string>; results: BulkResult[] };

const isFiltering = (filters: BrowserFilters) => Boolean(filters.q || filters.tag || filters.project_id || filters.environment_id || filters.node_id || filters.pinned);

interface BrowserBodyProps {
  browser: UseQueryResult<BrowserPage>;
  page: number;
  filtering: boolean;
  onFirstPage: () => void;
  onReset: () => void;
  children: (data: BrowserPage) => ReactNode;
}

function BrowserBody({ browser, page, filtering, onFirstPage, onReset, children }: BrowserBodyProps) {
  if (browser.isPending) return <LoadingRows label="Loading resources" />;
  if (!browser.data) {
    return (
      <div className="p-3">
        <QueryError what="resources" error={browser.error} onRetry={() => void browser.refetch()} retrying={browser.isFetching} />
      </div>
    );
  }
  if (browser.data.items.length === 0) {
    if (page > 1) return <EmptyPage onFirstPage={onFirstPage} />;
    return filtering ? <NoMatches what="resources" onReset={onReset} /> : <NoResources />;
  }
  return (
    <>
      {browser.isRefetchError && (
        <div className="border-b border-line p-3">
          <QueryError what="the latest resources" error={browser.error} onRetry={() => void browser.refetch()} retrying={browser.isFetching} />
        </div>
      )}
      {children(browser.data)}
    </>
  );
}

export function ResourceBrowserPage() {
  const [state, setState] = useState<BrowserState>(() => ({ filters: initialFilters, selected: new Set() }));
  const { filters, selected } = state;
  const [text, setText] = useState("");
  const [tagText, setTagText] = useState("");
  const [action, setAction] = useState<BulkAction>("deploy");
  const [confirming, setConfirming] = useState(false);
  const [editing, setEditing] = useState<BrowserResource | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const online = useOnline();
  const browser = useQuery(resourceBrowserQuery(filters));
  const bulk = useBulkAction();

  const update = (patch: Partial<BrowserFilters>) => setState(withFilters(patch));
  const clearSelection = () => setState((current) => ({ ...current, selected: new Set() }));

  const searchTooLong = new TextEncoder().encode(text.trim()).length > maxSearchBytes;
  const tagFilter = tagText.trim().toLowerCase();
  useEffect(() => {
    if (searchTooLong) return;
    const timer = window.setTimeout(() => setState(withFilters({ q: text.trim(), tag: tagFilter })), 300);
    return () => window.clearTimeout(timer);
  }, [text, tagFilter, searchTooLong]);

  function select(ids: string[], checked: boolean) {
    const listed = new Set(items.map((item) => item.id));
    setState((current) => {
      const next = new Set([...current.selected].filter((id) => listed.has(id)));
      for (const id of ids) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return { ...current, selected: next };
    });
  }

  function reset() {
    setText("");
    setTagText("");
    setState({ filters: { ...initialFilters, page_size: filters.page_size }, selected: new Set() });
  }

  const items = browser.data?.items ?? [];
  const selectedItems = items.filter((item) => selected.has(item.id));
  const filtering = isFiltering(filters) || Boolean(text || tagText);
  const locked = browser.isPlaceholderData || text.trim() !== filters.q || tagFilter !== filters.tag;
  const total = browser.data?.total ?? 0;
  const summary = browser.data ? (filtering ? countLabel(total, "match", "matches") : countLabel(total, "resource")) : "";

  function run() {
    const names = new Map(selectedItems.map((item) => [item.id, item.name]));
    const chosen = action;
    bulk.mutate(
      { ids: selectedItems.map((item) => item.id), action: chosen },
      {
        onSuccess: (response) => {
          setOutcome({ action: chosen, names, results: response.items });
          clearSelection();
          setConfirming(false);
        },
      },
    );
  }

  const body = (
    <BrowserBody browser={browser} page={filters.page} filtering={filtering} onFirstPage={() => update({ page: 1 })} onReset={reset}>
      {(data) => (
        <>
          {selectedItems.length > 0 && <BulkBar count={selectedItems.length} action={action} online={online} locked={locked} onAction={setAction} onReview={() => setConfirming(true)} onClear={clearSelection} />}
          <ResultsTable
            data={data}
            stale={browser.isPlaceholderData}
            locked={locked}
            selected={selected}
            onSelect={select}
            onTag={(tag) => {
              setTagText(tag);
              update({ tag });
            }}
            onEditTags={setEditing}
            onPage={(page) => update({ page })}
          />
        </>
      )}
    </BrowserBody>
  );

  return (
    <>
      <PageHeader
        breadcrumbs={
          <Breadcrumbs>
            <Link to="/resources" className="rounded hover:text-fg">
              All resources
            </Link>
            Organize
          </Breadcrumbs>
        }
        icon={<Boxes aria-hidden />}
        title="Organize resources"
        description="Tag, pin and run an operation on several resources at once. Pins are personal to you."
      />
      {!online && (
        <div className="px-4 pt-5 sm:px-6">
          <Notice tone="warn" title="You are offline">
            The list may be out of date and bulk actions are unavailable until the connection returns.
          </Notice>
        </div>
      )}
      {outcome && <BulkResults results={outcome.results} names={outcome.names} action={outcome.action} onDismiss={() => setOutcome(null)} />}
      <Section title="Resources" description="Search and filters run on the server across every resource you can access. Pinned resources are listed first." flush>
        <BrowserToolbar filters={filters} text={text} tagText={tagText} filtering={filtering} summary={summary} onText={setText} onTagText={setTagText} onUpdate={update} onReset={reset} />
        {searchTooLong && (
          <p role="alert" className="border-b border-line px-3 py-2 text-xs text-bad">
            Search is limited to {maxSearchBytes} bytes.
          </p>
        )}
        {body}
      </Section>
      {editing && <TagEditor resource={editing} onClose={() => setEditing(null)} />}
      <BulkConfirm
        open={confirming}
        action={action}
        items={selectedItems}
        pending={bulk.isPending}
        error={bulk.error}
        onConfirm={run}
        onCancel={() => {
          setConfirming(false);
          bulk.reset();
        }}
      />
    </>
  );
}
