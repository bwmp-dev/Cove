import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Hammer } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { buildNodeQuery, choiceName, type BuildNodeSettings } from "../api/build-nodes";
import { buildRecipeQuery } from "../api/build-recipes";
import { errorMessage, isStatus } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { useQueueOperation } from "../api/mutations";
import { nodesQuery, workloadOperationsQuery, workloadRegistryQuery } from "../api/queries";
import type { Node, Operation, Workload } from "../api/types";
import { engineLabel, isRecipeEngine, recordedEngine } from "../lib/build-recipes";
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

// The node recorded on the operation, never today's assignment: older control planes omit it, and it is then shown as not recorded.
function ExecutionNode({ operation, settings, nodes }: { operation: Operation; settings?: BuildNodeSettings; nodes?: Node[] }) {
  const id = operation.execution_node_id;
  if (!id) return <span>Build node not recorded</span>;
  const name = nodes?.find((node) => node.id === id)?.name || choiceName(settings, id);
  return <span className="min-w-0 break-all">Built on {name === id ? <span className="font-mono">{id}</span> : name}</span>;
}

function LatestAttempt({ operation, hasSuccess, node }: { operation: Operation; hasSuccess: boolean; node: ReactNode }) {
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
        {node}
        <span>{recordedEngine(operation)}</span>
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

function SuccessfulBuild({ operation, node }: { operation: Operation; node: ReactNode }) {
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
          {node}
          <span>{recordedEngine(operation)}</span>
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
  const settings = useQuery(buildNodeQuery(workload.id));
  const recipe = useQuery(buildRecipeQuery(workload.id));
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const statusId = useId();
  const [repository, setRepository] = useState("");
  const [noCache, setNoCache] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canOperate = can(workload.permissions, "operate");
  // Eligibility comes from the authoritative build node read. Pending or failed reads block building instead of trusting cached data.
  const effectiveId = settings.data?.effective_node_id;
  const choice = settings.data?.nodes.find((item) => item.id === effectiveId);
  const node = nodes.data?.find((item) => item.id === effectiveId);
  const nodeName = node?.name || (effectiveId ? choiceName(settings.data, effectiveId) : "");
  const eligible = choice?.eligible === true;
  const ready = node?.state === "ready";
  const supported = node?.capabilities?.build_publish === true;
  // Ordinary Compose builds are unaffected; a saved explicit recipe the server reports as unusable refuses the build.
  const savedRecipe = recipe.isError ? undefined : recipe.data;
  const recipeEngine = savedRecipe?.recipe.engine ?? "compose";
  const recipeBlocked = savedRecipe !== undefined && recipeEngine !== "compose" && (!isRecipeEngine(recipeEngine) || !savedRecipe.supported || savedRecipe.reason !== "");
  const unreadable = recipe.isPending || recipe.isError || settings.isPending || settings.isError || nodes.isPending || nodes.isError || operations.isPending || operations.isError || registry.isPending || registry.isError;
  const registryId = registry.data?.registry_id ?? "";
  const server = registries.data?.find((item) => item.id === registryId)?.registry?.server;
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const history = operations.data ? buildHistory(operations.data, workload.id) : undefined;
  const latestAttempt = history?.attempt && history.attempt !== history.success ? history.attempt : undefined;
  const blocked = !online || unreadable || recipeBlocked || !eligible || !registryId || Boolean(active) || queue.isPending;
  const executionNode = (operation: Operation) => <ExecutionNode operation={operation} settings={settings.data} nodes={nodes.data} />;

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
      description="Builds every Compose service that has a build section, or the service chosen in the build recipe, on the build node and pushes the images to this resource's registry. Nothing is deployed, started or routed."
      flush
    >
      <div id={statusId} className="space-y-3 border-b border-line px-4 py-3 empty:hidden sm:px-6">
        {!canOperate && <p className="text-xs text-fg-muted">Building and publishing need operator access. Results are read-only.</p>}
        {canOperate && settings.data && !choice && (
          <Notice tone="warn" title="Build node unavailable">
            The build node <span className="font-mono break-all">{effectiveId}</span> is not available to this resource. Building is refused until it is available again or the build node is changed.
          </Notice>
        )}
        {canOperate && choice && !eligible && node && ready && !supported && (
          <Notice tone="warn" title="Worker upgrade needed">
            {node.name} does not report build and publish support, which needs Docker Buildx, the Compose builder and pushing for its platform. Upgrade the worker, then verify it from the{" "}
            <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
              node page
            </Link>
            .
          </Notice>
        )}
        {canOperate && savedRecipe && recipeBlocked && (
          <Notice tone="warn" title="The build recipe cannot build">
            <span className="break-words">
              {engineLabel(savedRecipe.recipe.engine)}: {savedRecipe.reason || (isRecipeEngine(savedRecipe.recipe.engine) ? "The build worker does not support this recipe." : "This panel does not recognise the saved engine.")} Change the build recipe above or resolve the problem to build.
            </span>
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
        {canOperate && choice && !eligible && (!node || (ready && supported)) && nodes.data && (
          <Notice tone="warn" title="Build node unavailable">
            <span className="break-words">
              {nodeName}: {choice.reason || "It cannot build for this resource."}
            </span>
          </Notice>
        )}
        {canOperate && online && choice && !eligible && node && !ready && !active && <p className="text-xs text-fg-muted">{nodeName} is not ready. Building is available once the node is ready.</p>}
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
      {(settings.isError || recipe.isError || nodes.isError || registry.isError || (operations.isError && operations.data)) && (
        <div className="border-b border-line p-4 sm:px-6">
          <QueryError
            what={settings.isError ? "the build node" : recipe.isError ? "the build recipe" : nodes.isError ? "node status" : registry.isError ? "registry" : "the latest builds"}
            error={settings.error ?? recipe.error ?? nodes.error ?? registry.error ?? operations.error}
            onRetry={() => {
              void settings.refetch();
              void recipe.refetch();
              void nodes.refetch();
              void registry.refetch();
              void operations.refetch();
            }}
            retrying={settings.isFetching || recipe.isFetching || nodes.isFetching || registry.isFetching || operations.isFetching}
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
              <p>
                Building never changes running containers, routes, releases or the saved Compose file. To deploy the result on the runtime node, put the published repository@sha256 references in
                your Compose file and deploy as usual. The build node&apos;s local image IDs do not exist there. Deployment approval and freeze rules apply then.
              </p>
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
          {latestAttempt && <LatestAttempt operation={latestAttempt} hasSuccess={Boolean(history.success)} node={executionNode(latestAttempt)} />}
          {history.success ? (
            <SuccessfulBuild operation={history.success} node={executionNode(history.success)} />
          ) : (
            <p className="px-4 py-4 text-[13px] text-fg-muted sm:px-6">No successful build in the recent operation history.</p>
          )}
        </>
      )}
    </Section>
  );
}
