import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Hammer } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { useQueueOperation } from "../api/mutations";
import { nodesQuery, workloadOperationsQuery, workloadRegistryQuery } from "../api/queries";
import type { Operation, Workload } from "../api/types";
import { buildHistory, repositoryError, repositoryMaxLength } from "../lib/builds";
import { useOnline } from "../lib/hooks";
import { actionLabels, activeOperation, isActive, stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { buttonClass, inputClass, linkClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { CopyButton } from "./CopyButton";
import { RelativeTime } from "./RelativeTime";
import { OperationStateLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

function OperationLink({ operation, children }: { operation: Operation; children: string }) {
  return (
    <Link to="/operations/$operationId" params={{ operationId: operation.id }} className={linkClass}>
      {children}
    </Link>
  );
}

function LatestAttempt({ operation, hasSuccess }: { operation: Operation; hasSuccess: boolean }) {
  const active = isActive(operation.state);
  const error = operation.result?.error;
  return (
    <div className="space-y-2 border-b border-line px-4 py-3 sm:px-6">
      <h3 className="text-[13px] font-semibold text-fg">Latest attempt</h3>
      <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
        <OperationStateLabel state={operation.state} />
        <span>
          Started <RelativeTime value={operation.created_at} />
        </span>
        <OperationLink operation={operation}>{active ? "View build progress" : "View build"}</OperationLink>
      </p>
      {error && (
        <Notice tone="error" title="Build error">
          <span className="font-mono text-xs break-words">{error}</span>
        </Notice>
      )}
      {!active && operation.state !== "succeeded" && (
        <p className="text-xs text-fg-muted">
          Images this attempt pushed before it stopped may remain in the registry, but only a successful build reports verified references.
          {hasSuccess && " The latest successful build is still shown below."}
        </p>
      )}
      {operation.state === "succeeded" && <p className="text-xs text-fg-muted">This build did not report published images.</p>}
    </div>
  );
}

function SuccessfulBuild({ operation }: { operation: Operation }) {
  const images = operation.result?.build?.images ?? [];
  const finished = operation.result?.finished_at ?? operation.created_at;
  return (
    <div>
      <div className="space-y-1 px-4 pt-3 sm:px-6">
        <h3 className="text-[13px] font-semibold text-fg">Latest successful build</h3>
        <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
          <span>
            Published <RelativeTime value={finished} />
          </span>
          {operation.result?.revision && (
            <span className="font-mono" title={operation.result.revision}>
              {operation.result.revision.slice(0, 7)}
            </span>
          )}
          <OperationLink operation={operation}>View build</OperationLink>
        </p>
      </div>
      {images.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted sm:px-6">The build reported no published images.</p>
      ) : (
        <ul aria-label="Published images" className="mt-2 divide-y divide-line border-t border-line">
          {images.map((image) => (
            <li key={`${image.service}:${image.reference}`} className="grid gap-x-4 gap-y-1.5 px-4 py-3 sm:px-6 md:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] md:items-start">
              <p className="text-[13px] font-medium break-words text-fg">{image.service}</p>
              <p className="min-w-0 font-mono text-xs break-all text-fg">{image.reference}</p>
              <CopyButton text={image.reference} size="sm" variant="ghost" subject={`reference for ${image.service}`} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function ResourceBuild({ workload }: { workload: Workload }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const registry = useQuery(workloadRegistryQuery(workload.id));
  const registries = useQuery(connectionsQuery("registries"));
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const statusId = useId();
  const [repository, setRepository] = useState("");
  const [noCache, setNoCache] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canOperate = can(workload.permissions, "operate");
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const ready = node?.state === "ready";
  const supported = node?.capabilities?.build_publish === true;
  const registryId = registry.data?.registry_id ?? "";
  const server = registries.data?.find((item) => item.id === registryId)?.registry?.server;
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const history = operations.data ? buildHistory(operations.data, workload.id) : undefined;
  const latestAttempt = history?.attempt && history.attempt !== history.success ? history.attempt : undefined;
  const blocked = !online || !ready || !supported || !registryId || Boolean(active) || queue.isPending || operations.isPending || registry.isPending;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = repository.trim();
    const problem = repositoryError(value);
    setError(problem);
    if (problem) return;
    queue.mutate({ action: "build", build: { repository: value, no_cache: noCache } });
  }

  return (
    <Section
      title="Build and publish"
      description="Builds every Compose service that has a build section and pushes the images to this resource's registry. Nothing is deployed, started or routed."
      flush
    >
      <div id={statusId} className="space-y-3 border-b border-line px-4 py-3 empty:hidden sm:px-6">
        {!canOperate && <p className="text-xs text-fg-muted">Building and publishing need operator access. Results are read-only.</p>}
        {canOperate && node && ready && !supported && (
          <Notice tone="warn" title="Worker upgrade needed">
            {node.name} does not report build and publish support, which needs Docker Buildx, the Compose builder and pushing for its platform. Upgrade the worker, then verify it from the{" "}
            <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
              node page
            </Link>
            .
          </Notice>
        )}
        {canOperate && registry.data && !registryId && (
          <Notice tone="warn" title="No registry selected">
            Select and save registry credentials above to publish images. Team registries are added on the{" "}
            <Link to="/registries" className={linkClass}>
              Registries page
            </Link>
            .
          </Notice>
        )}
        {canOperate && !online && <p className="text-xs text-fg-muted">You are offline. Building is available when the connection returns.</p>}
        {canOperate && online && node && !ready && !active && <p className="text-xs text-fg-muted">{node.name} is not ready. Building is available once the node is ready.</p>}
        {canOperate && active && active.action !== "build" && (
          <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted">
            <span>
              {actionLabels[active.action]} {stateLabels[active.state].toLowerCase()}. Building is available when it finishes.
            </span>
            <OperationLink operation={active}>View operation</OperationLink>
          </p>
        )}
        {queue.isSuccess && (
          <Notice
            tone="success"
            title="Build queued"
            action={
              <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost", "", "sm")}>
                View build progress
              </Link>
            }
          />
        )}
        {queue.isError && (
          <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title="Could not queue build">
            {errorMessage(queue.error)}
          </Notice>
        )}
      </div>
      {(nodes.isError || registry.isError) && (
        <div className="border-b border-line p-4 sm:px-6">
          <QueryError
            what={nodes.isError ? "node status" : "registry"}
            error={nodes.error ?? registry.error}
            onRetry={() => {
              void nodes.refetch();
              void registry.refetch();
            }}
            retrying={nodes.isFetching || registry.isFetching}
          />
        </div>
      )}
      {canOperate && (
        <div className="border-b border-line">
          <form onSubmit={submit} noValidate className="max-w-3xl space-y-3 px-4 py-4 sm:px-6">
            <Field
              id="build-repository"
              label="Repository"
              error={error ?? undefined}
              hint={
                <>
                  A lowercase path within {server ? <span className="font-mono break-all">{server}</span> : "the selected registry"}, such as{" "}
                  <span className="font-mono">team/app</span>. Leave out the host, tag and digest. Each service is pushed with its own tags.
                </>
              }
            >
              <input
                id="build-repository"
                className={inputClass}
                value={repository}
                maxLength={repositoryMaxLength}
                autoCapitalize="none"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? "build-repository-error" : "build-repository-hint"}
                onChange={(event) => {
                  setRepository(event.target.value);
                  setError(null);
                }}
              />
            </Field>
            <label className="flex items-center gap-2 text-[13px] text-fg">
              <Checkbox checked={noCache} onChange={(event) => setNoCache(event.target.checked)} />
              Bypass the build cache
            </label>
            <div className="space-y-1.5 text-xs text-fg-muted">
              <p>
                Uses the saved Compose file, Git source, variables and registry. File secrets configured for build services are mounted with BuildKit, which keeps them out of image layers
                unless the Dockerfile copies them. A build script can still read them.
              </p>
              <p>To deploy the result, put the published digest references in your Compose file. Deployment approval and freeze rules apply then.</p>
            </div>
            <div className="flex justify-end">
              <Button type="submit" variant="primary" disabled={blocked} pending={queue.isPending || active?.action === "build"} aria-describedby={statusId}>
                <Hammer aria-hidden className="size-3.5" />
                Build and publish
              </Button>
            </div>
          </form>
        </div>
      )}
      {operations.isPending ? (
        <LoadingRows label="Loading builds" rows={2} />
      ) : operations.isError && !operations.data ? (
        <div className="p-4 sm:px-6">
          <QueryError what="builds" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        </div>
      ) : !history?.attempt ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">No builds in the recent operation history.</p>
      ) : (
        <>
          {latestAttempt && <LatestAttempt operation={latestAttempt} hasSuccess={Boolean(history.success)} />}
          {history.success ? (
            <SuccessfulBuild operation={history.success} />
          ) : (
            <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">No successful build in the recent operation history.</p>
          )}
        </>
      )}
    </Section>
  );
}
