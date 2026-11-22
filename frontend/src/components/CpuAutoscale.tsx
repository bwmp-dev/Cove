import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useUpdateAutoscalePolicy } from "../api/mutations";
import { autoscalePolicyQuery, nodesQuery, workloadOperationsQuery } from "../api/queries";
import type { AutoscaleState, CPUSample, Node, Operation, ScaleResult, ServiceLimits, Workload } from "../api/types";
import {
  autoscaleDraft,
  autoscaleLimits,
  autoscaleStatus,
  candidateServices,
  cooldownEnds,
  formatCPU,
  hasLimits,
  latestSample,
  sameAutoscaleDraft,
  sampleIsStale,
  scaleChanges,
  validateAutoscale,
  type AutoscaleDraft,
  type AutoscaleErrors,
  type ServiceDraft,
} from "../lib/autoscale";
import { useNow, useOnline } from "../lib/hooks";
import { actionLabels, activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { describedBy, inputClass, linkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { OperationStateLabel, StatusLabel } from "./status";
import { Notice, QueryError, Section, SettingRow, SettingRows, Spinner, SwitchField } from "./ui";

const cpuMeaning = "CPU is each replica's use as a percentage of its own CPU limit, averaged over all replicas of the service. 100% means the replicas use their whole limit; Docker can report more.";

function ResourceLink({ workloadId, to, children }: { workloadId: string; to: "/resources/$resourceId/limits" | "/resources/$resourceId/preflight"; children: ReactNode }) {
  return (
    <Link to={to} params={{ resourceId: workloadId }} className={linkClass}>
      {children}
    </Link>
  );
}

function saveBlocker({ online, operations, active, node, nodesRead, enabling }: { online: boolean; operations: { isSuccess: boolean }; active?: Operation; node?: Node; nodesRead: boolean; enabling: boolean }): ReactNode {
  if (!online) return "You are offline. Changes can be saved when the connection returns.";
  if (!operations.isSuccess) return "Changes can be saved once active operations can be read.";
  if (active) {
    return (
      <>
        Changes can be saved after the active {actionLabels[active.action].toLowerCase()} finishes.{" "}
        <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
          View operation
        </Link>
      </>
    );
  }
  if (enabling) {
    if (!nodesRead || !node?.capabilities) return "Cove cannot confirm the worker supports CPU scaling, so it cannot be turned on. You can still turn it off.";
    if (!node.capabilities.cpu_autoscale) return "The worker does not report CPU scaling support, so it cannot be turned on. You can still turn it off.";
    if (node.state !== "ready") return "The worker is not ready, so CPU scaling cannot be turned on. You can still turn it off.";
  }
  return null;
}

function NodeCapabilityNotice({ nodes, node, savedEnabled }: { nodes: { isError: boolean; isSuccess: boolean; error: unknown; refetch: () => unknown; isFetching: boolean }; node?: Node; savedEnabled: boolean }) {
  const nodeLink = node && (
    <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
      node page
    </Link>
  );
  if (nodes.isError) return <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />;
  if (nodes.isSuccess && !node) {
    return (
      <Notice tone="warn" title="Worker unknown">
        The worker for this resource is not in the node list, so CPU scaling support cannot be confirmed and scaling cannot be turned on.
      </Notice>
    );
  }
  if (node && !node.capabilities) {
    return (
      <Notice tone="warn" title="Worker capabilities unknown">
        {node.name} has not reported its capabilities, so CPU scaling support cannot be confirmed and scaling cannot be turned on. Verify it from the {nodeLink}.
      </Notice>
    );
  }
  if (node?.capabilities && !node.capabilities.cpu_autoscale) {
    return (
      <Notice tone="warn" title="Worker upgrade needed for CPU scaling">
        {node.name} does not report CPU scaling support. Upgrade the worker, then verify it from the {nodeLink}.
        {savedEnabled ? " The saved policy stays on but does not scale until then. You can turn it off." : ""}
      </Notice>
    );
  }
  if (node && node.state !== "ready") {
    return (
      <Notice tone="warn" title="The node is not ready">
        Scaling pauses while the worker is unavailable. Running replicas keep serving.
      </Notice>
    );
  }
  return null;
}

function useAutoscaleDraft(workload: Workload, state?: AutoscaleState) {
  const [draft, setDraft] = useState<AutoscaleDraft | null>(null);
  const services = state ? candidateServices(workload.limits, state.policy) : [];
  const saved = state && autoscaleDraft(state.policy, services);
  const values = draft ?? saved;
  const dirty = Boolean(draft && saved && !sameAutoscaleDraft(draft, saved));
  return { draft, setDraft, services, saved, values, dirty };
}

// The worker, operation and connection gates for saving the policy.
function useSaveGates(workload: Workload, values?: AutoscaleDraft) {
  const online = useOnline();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const nodes = useQuery(nodesQuery);
  const node = nodes.isError ? undefined : nodes.data?.find((item) => item.id === workload.node_id);
  const blocker = values ? saveBlocker({ online, operations, active, node, nodesRead: nodes.isSuccess, enabling: values.enabled }) : null;
  return { operations, nodes, node, blocker };
}

// Keeps the last loaded state when a refresh fails, and says so.
function PolicyRead({ state, children }: { state: UseQueryResult<AutoscaleState>; children: (data: AutoscaleState) => ReactNode }) {
  if (state.isPending) {
    return (
      <p className="flex items-center gap-2 text-[13px] text-fg-muted">
        <Spinner />
        Loading the CPU scaling policy
      </p>
    );
  }
  if (!state.data) return <QueryError what="the CPU scaling policy" error={state.error} onRetry={() => void state.refetch()} retrying={state.isFetching} />;
  return (
    <>
      {state.isError && (
        <Notice tone="warn" title="Showing the last loaded state">
          <span className="break-words">Could not refresh CPU scaling: {errorMessage(state.error)}</span>
        </Notice>
      )}
      {children(state.data)}
    </>
  );
}

function SaveError({ error }: { error: unknown }) {
  return (
    <Notice tone={isStatus(error, 409) ? "warn" : "error"} title="Could not save CPU scaling">
      <span className="block break-words">{errorMessage(error)}</span>
      Your changes are kept.
    </Notice>
  );
}

export function CpuAutoscaleSettings({ workload }: { workload: Workload }) {
  const id = useId();
  const editable = can(workload.permissions, "edit");
  const state = useQuery(autoscalePolicyQuery(workload.id));
  const update = useUpdateAutoscalePolicy(workload.id);
  const { draft, setDraft, services, saved, values, dirty } = useAutoscaleDraft(workload, state.data);
  const { operations, nodes, node, blocker } = useSaveGates(workload, values);
  const [errors, setErrors] = useState<AutoscaleErrors>({});
  const savedEnabled = Boolean(state.data?.policy.enabled);
  const locked = !editable || update.isPending;

  function edit(next: AutoscaleDraft) {
    update.reset();
    setErrors({});
    setDraft(next);
  }

  function discard() {
    update.reset();
    setErrors({});
    setDraft(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!draft || !dirty || blocker) return;
    const result = validateAutoscale(draft, { limits: workload.limits, manualReplicas: workload.rollout?.replicas });
    setErrors(result.errors);
    if (!result.policy) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    update.mutate(result.policy, { onSuccess: () => setDraft(null) });
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title="CPU scaling"
        description="Adds or removes replicas of selected services based on their CPU use, within the bounds you set. Saved separately; scaling uses a rolling deploy of the current release."
        footer={editable && state.data && <SaveBar dirty={dirty} pending={update.isPending} blocked={Boolean(blocker)} onDiscard={discard} status={update.isSuccess ? "Saved. The panel uses it on its next check." : undefined} />}
      >
        <div className="space-y-5">
          {!editable && <p className="text-xs text-fg-muted">Changing this needs editor access. Ask a team owner or admin.</p>}
          {(editable || savedEnabled) && <NodeCapabilityNotice nodes={nodes} node={node} savedEnabled={savedEnabled} />}
          {editable && operations.isError && <QueryError what="active operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />}
          <PolicyRead state={state}>
            {(data) => (
              <>
                {values && saved && <PolicyFields id={id} workload={workload} services={services} values={values} saved={saved} errors={errors} locked={locked} onEdit={edit} />}
                {editable && blocker && dirty && <p className="text-xs break-words text-fg-muted">{blocker}</p>}
                {update.isError && <SaveError error={update.error} />}
                <CurrentState state={data} operations={operations.data} />
              </>
            )}
          </PolicyRead>
          <HowItWorks workloadId={workload.id} />
        </div>
      </Section>
    </form>
  );
}

function PolicyFields({ id, workload, services, values, saved, errors, locked, onEdit }: { id: string; workload: Workload; services: string[]; values: AutoscaleDraft; saved: AutoscaleDraft; errors: AutoscaleErrors; locked: boolean; onEdit: (next: AutoscaleDraft) => void }) {
  const strategy = workload.rollout?.strategy || (workload.rollout ? "rolling" : "disabled");
  function editService(name: string, next: Partial<ServiceDraft>) {
    const current = values.services[name] ?? saved.services[name];
    if (!current) return;
    onEdit({ ...values, services: { ...values.services, [name]: { ...current, ...next } } });
  }
  return (
    <>
      <SettingRows>
        <SwitchField
          layout="row"
          id={`${id}-enabled`}
          checked={values.enabled}
          disabled={locked}
          onChange={(event) => onEdit({ ...values, enabled: event.target.checked })}
          label="Scale on CPU use"
          description="Turning it on needs a deployed, healthy rolling release and a current CPU reading for every selected service."
        />
      </SettingRows>
      {values.enabled && strategy !== "rolling" && (
        <Notice tone="warn" title="Rolling replacement needed">
          CPU scaling needs the Rolling replacement strategy, saved and deployed. Choose it under Deployment safety above, save, then deploy.
        </Notice>
      )}
      <fieldset className="min-w-0 space-y-3" aria-describedby={`${id}-services-hint`}>
        <legend className="text-[13px] font-semibold text-fg">Services</legend>
        <p id={`${id}-services-hint`} className="text-xs text-fg-muted">
          Only services with explicit CPU and memory limits can scale. Their maximum replicas reserve team quota for those limits. {cpuMeaning}
        </p>
        {services.length === 0 ? (
          <p className="text-[13px] text-fg-muted">
            No service has CPU and memory limits yet. Set them in <ResourceLink workloadId={workload.id} to="/resources/$resourceId/limits">Resource limits</ResourceLink>, then return here.
          </p>
        ) : (
          <div className="divide-y divide-line border-y border-line">
            {services.map((name) => (
              <ServiceRow
                key={name}
                id={`${id}-${name}`}
                name={name}
                workloadId={workload.id}
                limit={workload.limits?.[name]}
                manual={workload.rollout?.replicas?.[name]}
                value={values.services[name] ?? saved.services[name]}
                errors={errors}
                locked={locked}
                onChange={(next) => editService(name, next)}
              />
            ))}
          </div>
        )}
        {errors.form && (
          <p role="alert" className="text-xs break-words text-bad">
            {errors.form}
          </p>
        )}
      </fieldset>
    </>
  );
}

function limitSummary(limit: ServiceLimits | undefined) {
  if (!hasLimits(limit) || !limit) return null;
  return `${limit.cpus.toLocaleString("en-US", { maximumFractionDigits: 2 })} CPU, ${limit.memory_mb.toLocaleString("en-US")} MiB per replica.`;
}

function ServiceRow({ id, name, workloadId, limit, manual, value, errors, locked, onChange }: { id: string; name: string; workloadId: string; limit?: ServiceLimits; manual?: number; value?: ServiceDraft; errors: AutoscaleErrors; locked: boolean; onChange: (next: Partial<ServiceDraft>) => void }) {
  if (!value) return null;
  const summary = limitSummary(limit);
  // A service without limits cannot be newly selected, but a saved selection can still be cleared.
  const selectable = Boolean(summary) || value.selected;
  const limitsError = errors[`${name}.limits`];
  return (
    <div className="space-y-3 py-3">
      <div className="flex items-start gap-3">
        <Checkbox
          id={`${id}-selected`}
          checked={value.selected}
          disabled={locked || !selectable}
          aria-invalid={limitsError ? true : undefined}
          aria-describedby={limitsError ? `${id}-limits-error` : `${id}-detail`}
          onChange={(event) => onChange({ selected: event.target.checked })}
        />
        <div className="min-w-0">
          <label htmlFor={`${id}-selected`} className={`block text-[13px] font-medium break-all ${locked || !selectable ? "text-fg-muted" : "text-fg"}`}>
            Scale <span className="font-mono">{name}</span>
          </label>
          <p id={`${id}-detail`} className="mt-0.5 text-xs text-fg-muted">
            {summary ?? (
              <>
                No explicit CPU and memory limits. Set them in <ResourceLink workloadId={workloadId} to="/resources/$resourceId/limits">Resource limits</ResourceLink>.
              </>
            )}
            {manual !== undefined && ` Fixed at ${manual} ${manual === 1 ? "replica" : "replicas"} under Deployment safety.`}
          </p>
          {limitsError && (
            <p id={`${id}-limits-error`} className="mt-1 text-xs text-bad">
              {limitsError}
            </p>
          )}
        </div>
      </div>
      {value.selected && (
        <div className="grid gap-4 sm:grid-cols-3 sm:pl-8">
          <NumberField id={`${id}-min`} label="Minimum replicas" hint={`${autoscaleLimits.minReplicas} to ${autoscaleLimits.maxReplicas}`} value={value.min} error={errors[`${name}.min`]} disabled={locked} onChange={(min) => onChange({ min })} />
          <NumberField id={`${id}-max`} label="Maximum replicas" hint={`${autoscaleLimits.minReplicas} to ${autoscaleLimits.maxReplicas}`} value={value.max} error={errors[`${name}.max`]} disabled={locked} onChange={(max) => onChange({ max })} />
          <NumberField id={`${id}-target`} label="Target CPU (%)" hint={`${autoscaleLimits.minTarget} to ${autoscaleLimits.maxTarget}% of the limit`} value={value.target} error={errors[`${name}.target`]} disabled={locked} onChange={(target) => onChange({ target })} />
        </div>
      )}
    </div>
  );
}

function NumberField({ id, label, hint, value, error, disabled, onChange }: { id: string; label: string; hint: string; value: string; error?: string; disabled: boolean; onChange: (value: string) => void }) {
  return (
    <div className="min-w-0 space-y-1.5">
      <label htmlFor={id} className="block text-[13px] font-medium text-fg">
        {label}
      </label>
      <input
        id={id}
        className={inputClass}
        inputMode="numeric"
        value={value}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
        onChange={(event) => onChange(event.target.value)}
      />
      {error ? (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      ) : (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          {hint}
        </p>
      )}
    </div>
  );
}

function CurrentState({ state, operations }: { state: AutoscaleState; operations?: Operation[] }) {
  const now = useNow(15_000);
  const status = autoscaleStatus(state.status);
  const sample = latestSample(state);
  const stale = sample ? sampleIsStale(sample, now) : false;
  const recorded = state.samples?.length ?? 0;
  const operation = state.operation_id ? operations?.find((item) => item.id === state.operation_id) : undefined;
  const cooldown = cooldownEnds(state.last_queued_at);
  const coolingDown = cooldown !== null && Date.parse(cooldown) > now;
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <h3 className="text-[13px] font-semibold text-fg">Current state</h3>
      <SettingRows>
        <SettingRow label="Status" description={status.detail}>
          <StatusLabel tone={status.tone} label={status.label} pulse={state.status === "queued"} />
        </SettingRow>
        <SettingRow label="Latest sample" description={`Read at most once a minute while the panel is online; busy installations may take longer. A sample older than ${autoscaleLimits.freshSeconds} seconds cannot start scaling.`}>
          {sample ? (
            <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
              <RelativeTime value={sample.sampled_at} className="text-fg-muted" />
              {stale && <StatusLabel tone="warn" label="Stale" />}
            </span>
          ) : (
            <span className="text-fg-muted">None recorded</span>
          )}
        </SettingRow>
        <SettingRow label="Samples recorded" description="Scaling decides only from the latest 3 consistent samples.">
          <span className="text-fg tabular-nums">
            {Math.min(recorded, autoscaleLimits.samplesNeeded)} of {autoscaleLimits.samplesNeeded}
          </span>
        </SettingRow>
        <SettingRow label="Last scaling deploy">
          {state.operation_id ? (
            <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
              {operation && <OperationStateLabel state={operation.state} />}
              {state.last_queued_at && <RelativeTime value={state.last_queued_at} className="text-fg-muted" />}
              <Link to="/operations/$operationId" params={{ operationId: state.operation_id }} className={linkClass}>
                View operation
              </Link>
            </span>
          ) : state.last_queued_at ? (
            <RelativeTime value={state.last_queued_at} className="text-fg-muted" />
          ) : (
            <span className="text-fg-muted">None</span>
          )}
        </SettingRow>
        {coolingDown && cooldown && (
          <SettingRow label="Cooldown ends" description="Each queued attempt starts a 5-minute cooldown, even if it fails or is cancelled.">
            <RelativeTime value={cooldown} className="text-fg-muted" />
          </SettingRow>
        )}
      </SettingRows>
      {state.last_error && (
        <Notice tone="warn" title="The last check reported a problem">
          <span className="break-words">{state.last_error}</span>
        </Notice>
      )}
      {sample && <SampleTable sample={sample} stale={stale} />}
    </div>
  );
}

function SampleTable({ sample, stale }: { sample: CPUSample; stale: boolean }) {
  const services = [...new Set([...Object.keys(sample.cpu_percent ?? {}), ...Object.keys(sample.replicas ?? {})])].sort();
  if (services.length === 0) return <p className="text-[13px] text-fg-muted">The latest sample has no service readings.</p>;
  return (
    <div className="min-w-0 overflow-x-auto">
      <table className="w-full text-[13px]">
        <caption className="pb-2 text-left text-xs text-fg-muted">
          {stale ? "Latest sample, now stale. " : "Latest sample. "}
          {cpuMeaning} Replicas are the deployed count when the sample was read.
        </caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="text-left">
              Service
            </th>
            <th scope="col" className="text-right">
              CPU of limit
            </th>
            <th scope="col" className="pl-3 text-right">
              Replicas
            </th>
          </tr>
        </thead>
        <tbody>
          {services.map((service) => {
            const cpu = sample.cpu_percent?.[service];
            const replicas = sample.replicas?.[service];
            return (
              <tr key={service} className={tableRowClass}>
                <td className="font-mono text-xs break-all text-fg">{service}</td>
                <td className="text-right tabular-nums text-fg">{cpu === undefined ? <span className="text-fg-muted">Not reported</span> : formatCPU(cpu)}</td>
                <td className="pl-3 text-right tabular-nums text-fg">{replicas === undefined ? <span className="text-fg-muted">Not reported</span> : replicas}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function HowItWorks({ workloadId }: { workloadId: string }) {
  return (
    <Notice title="How CPU scaling works">
      <ul className="list-disc space-y-0.5 pl-4">
        <li>The panel reads CPU at most once a minute per resource while it is online, in small batches, so busy installations may take longer. Each decision needs 3 consistent samples from the same deployment, no more than 90 seconds apart, with the latest under 90 seconds old.</li>
        <li>If a service is at or above its target in all 3 samples, it gains 1 replica. At or below half its target in all 3, it loses 1. Bounds are enforced after 3 samples.</li>
        <li>Each queued attempt starts a 5-minute cooldown; failed or cancelled attempts count, and turning scaling off and on does not reset it.</li>
        <li>
          Scaling deploys the current release again with new replica counts: a health-gated rolling replacement with no Git checkout, build or image pull, then the old generation drains.
          Approval, deployment freezes, windows and deployment quotas still apply, so a scaling deploy can wait for approval.
        </li>
        <li>
          Selected services must qualify for rolling replacement: no fixed host ports, shared or private networking, deployment hooks, or writable volumes without the parallel volume
          assertion. Check them with <ResourceLink workloadId={workloadId} to="/resources/$resourceId/preflight">Preflight</ResourceLink>.
        </li>
        <li>Maximum replicas reserve team quota for their limits; the rolling replacement also needs room for old and new replicas at once.</li>
        <li>Nothing scales while samples are unknown, stale or partial, or while the app is unhealthy, asleep, stopped, paused, staged or has an active operation. Apps keep serving while the panel is offline, but do not scale.</li>
        <li>Scaling uses CPU only; there are no request-based thresholds.</li>
      </ul>
    </Notice>
  );
}

export function ScaleOutcome({ scale }: { scale: ScaleResult }) {
  const changes = scaleChanges(scale);
  return (
    <div className="space-y-1">
      <p>CPU scaling deployed the current release with new replica counts.</p>
      {changes.length === 0 ? (
        <p className="text-xs text-fg-muted">No replica changes were reported.</p>
      ) : (
        <ul className="space-y-0.5 text-xs text-fg-muted">
          {changes.map((change) => (
            <li key={change.service} className="break-words">
              <span className="font-mono text-fg">{change.service}</span>: {change.before ?? "unknown"} to {change.after} {change.after === 1 ? "replica" : "replicas"}
              {change.cpu !== undefined && ` at ${formatCPU(change.cpu)} CPU of limit`}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
