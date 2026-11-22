import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus, X } from "lucide-react";
import { nodesQuery } from "../api/queries";
import type { RolloutStrategy, Workload } from "../api/types";
import { workloadKind } from "../lib/operations";
import { rolloutLimits, rolloutsApply, type CheckRow, type ReplicaRow, type RolloutDraft, type RolloutErrors } from "../lib/rollouts";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Select } from "./Select";
import { Button, Field, Notice, QueryError } from "./ui";

const strategyHints: Record<RolloutStrategy, string> = {
  disabled: "Compose replaces the containers in place, through guarded deployment when it is enabled below.",
  rolling: "Starts the new containers beside the current ones, waits for their health checks, then moves all traffic and drains the old containers.",
  blue_green: "Starts the new containers and keeps all traffic on the current release until you promote or abort the rollout from the Overview.",
  canary: "Sends a share of traffic to the new containers until you promote or abort the rollout from the Overview.",
};

interface RolloutFieldsProps {
  id: string;
  draft: RolloutDraft;
  errors: RolloutErrors;
  disabled: boolean;
  onChange: (change: (draft: RolloutDraft) => RolloutDraft) => void;
}

function invalid(error: string | undefined) {
  return error ? true : undefined;
}

// Strategy and timing rows, rendered inside the page's SettingRows.
export function RolloutStrategyRows({ id, draft, errors, disabled, onChange }: RolloutFieldsProps) {
  const set = <K extends "drain" | "timeout" | "canary">(key: K, value: string) => onChange((current) => ({ ...current, [key]: value }));
  const enabled = draft.strategy !== "disabled";
  return (
    <>
      <Field layout="row" id={`${id}-strategy`} label="Replacement strategy" hint={strategyHints[draft.strategy]}>
        <Select
          id={`${id}-strategy`}
          className={inputClass}
          value={draft.strategy}
          disabled={disabled}
          aria-describedby={`${id}-strategy-hint`}
          onChange={(event) => {
            const strategy = event.target.value as RolloutStrategy;
            onChange((current) => ({ ...current, strategy }));
          }}
        >
          <option value="disabled">Off</option>
          <option value="rolling">Rolling</option>
          <option value="blue_green">Blue/green</option>
          <option value="canary">Canary</option>
        </Select>
      </Field>
      {draft.strategy === "canary" && (
        <Field layout="row" id={`${id}-canary`} label="Canary traffic" hint="Percentage of requests sent to the new release while it waits for promotion." error={errors.canary}>
          <NumberInput id={`${id}-canary`} value={draft.canary} min={1} max={99} unit="%" disabled={disabled} error={errors.canary} hint onChange={(value) => set("canary", value)} />
        </Field>
      )}
      {enabled && (
        <>
          <Field layout="row" id={`${id}-timeout`} label="Health timeout" hint="How long each new service may take to report healthy before the deploy fails and the current release keeps serving." error={errors.timeout}>
            <NumberInput id={`${id}-timeout`} value={draft.timeout} min={5} max={600} unit="seconds" disabled={disabled} error={errors.timeout} hint onChange={(value) => set("timeout", value)} />
          </Field>
          <Field layout="row" id={`${id}-drain`} label="Drain time" hint="How long old containers keep finishing in-flight requests after traffic moves away from them." error={errors.drain}>
            <NumberInput id={`${id}-drain`} value={draft.drain} min={0} max={120} unit="seconds" disabled={disabled} error={errors.drain} hint onChange={(value) => set("drain", value)} />
          </Field>
          <div className="flex items-start gap-3">
            <Checkbox
              id={`${id}-volumes`}
              checked={draft.parallelVolumes}
              disabled={disabled}
              aria-describedby={`${id}-volumes-description`}
              onChange={(event) => {
                const parallelVolumes = event.target.checked;
                onChange((current) => ({ ...current, parallelVolumes }));
              }}
            />
            <div className="min-w-0">
              <label htmlFor={`${id}-volumes`} className={`block text-[13px] font-medium ${disabled ? "text-fg-muted" : "text-fg"}`}>
                Old and new containers can write to the same volumes at once
              </label>
              <p id={`${id}-volumes-description`} className="mt-0.5 text-xs text-fg-muted">
                Needed when a service has writable mounts or image volumes. Do not select this for databases or other single-writer storage unless
                concurrent writers are safe. Without it, those services fall back to guarded replacement.
              </p>
            </div>
          </div>
        </>
      )}
    </>
  );
}

function NumberInput({ id, value, min, max, unit, disabled, error, hint, onChange }: { id: string; value: string; min: number; max: number; unit: string; disabled: boolean; error?: string; hint?: boolean; onChange: (value: string) => void }) {
  return (
    <div className="flex items-center gap-2">
      <input
        id={id}
        className={`${inputClass} max-w-28 tabular-nums`}
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        value={value}
        disabled={disabled}
        aria-invalid={invalid(error)}
        aria-describedby={describedBy(id, error, hint)}
        onChange={(event) => onChange(event.target.value)}
      />
      <span className="text-xs text-fg-muted">{unit}</span>
    </div>
  );
}

// Per-service replica and HTTP check editors, shown below the settings rows while a strategy is selected.
export function RolloutServiceEditors({ id, draft, errors, disabled, onChange }: RolloutFieldsProps) {
  const updateReplica = (rowId: string, key: "service" | "count", value: string) =>
    onChange((current) => ({ ...current, replicas: current.replicas.map((row) => (row.id === rowId ? { ...row, [key]: value } : row)) }));
  const updateCheck = (rowId: string, key: "service" | "port" | "path", value: string) =>
    onChange((current) => ({ ...current, checks: current.checks.map((row) => (row.id === rowId ? { ...row, [key]: value } : row)) }));
  return (
    <div className="space-y-6 border-t border-line pt-5">
      <fieldset className="min-w-0 space-y-3" aria-describedby={`${id}-replicas-description`}>
        <legend className="text-[13px] font-semibold text-fg">Replicas</legend>
        <p id={`${id}-replicas-description`} className="-mt-2 text-xs text-fg-muted">
          Services without a row run one replica. Each service can run up to {rolloutLimits.replicasPerService}, and the resource up to {rolloutLimits.totalReplicas} in total,
          counting services without a row. Guarded replacement ignores these and uses Compose scaling.
        </p>
        {draft.replicas.map((row) => (
          <ReplicaFields key={row.id} id={id} row={row} errors={errors} disabled={disabled} onChange={updateReplica} onRemove={() => onChange((current) => ({ ...current, replicas: current.replicas.filter((item) => item.id !== row.id) }))} />
        ))}
        {errors.replicas && <p className="text-xs text-bad">{errors.replicas}</p>}
        {!disabled && (
          <Button
            disabled={draft.replicas.length >= rolloutLimits.serviceRows}
            onClick={() => onChange((current) => ({ ...current, replicas: [...current.replicas, { id: crypto.randomUUID(), service: "", count: "2" }] }))}
          >
            <Plus aria-hidden className="size-3.5" />
            Add replica setting
          </Button>
        )}
      </fieldset>
      <fieldset className="min-w-0 space-y-3" aria-describedby={`${id}-checks-description`}>
        <legend className="text-[13px] font-semibold text-fg">HTTP health checks</legend>
        <p id={`${id}-checks-description`} className="-mt-2 text-xs text-fg-muted">
          Used only for services whose image and Compose file define no Docker healthcheck. Cove requests the path on the container&apos;s own loopback
          interface, so it is not a public URL. Redirects and responses outside 2xx fail the check.
        </p>
        {draft.checks.map((row) => (
          <CheckFields key={row.id} id={id} row={row} errors={errors} disabled={disabled} onChange={updateCheck} onRemove={() => onChange((current) => ({ ...current, checks: current.checks.filter((item) => item.id !== row.id) }))} />
        ))}
        {errors.checks && <p className="text-xs text-bad">{errors.checks}</p>}
        {!disabled && (
          <Button
            disabled={draft.checks.length >= rolloutLimits.serviceRows}
            onClick={() => onChange((current) => ({ ...current, checks: [...current.checks, { id: crypto.randomUUID(), service: "", port: "", path: "/" }] }))}
          >
            <Plus aria-hidden className="size-3.5" />
            Add HTTP check
          </Button>
        )}
      </fieldset>
    </div>
  );
}

function RemoveRow({ label, disabled, onRemove }: { label: string; disabled: boolean; onRemove: () => void }) {
  if (disabled) return null;
  return (
    <Button variant="ghost" className="self-end" aria-label={label} title={label} onClick={onRemove}>
      <X aria-hidden className="size-3.5" />
    </Button>
  );
}

function ReplicaFields({ id, row, errors, disabled, onChange, onRemove }: { id: string; row: ReplicaRow; errors: RolloutErrors; disabled: boolean; onChange: (rowId: string, key: "service" | "count", value: string) => void; onRemove: () => void }) {
  const serviceId = `${id}-${row.id}-replica-service`;
  const countId = `${id}-${row.id}-replica-count`;
  const serviceError = errors[`${row.id}.service`];
  const countError = errors[`${row.id}.count`];
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_6rem_auto] items-start gap-x-3 gap-y-2 border-b border-line pb-3">
      <Field id={serviceId} label="Compose service" error={serviceError}>
        <input id={serviceId} className={`${inputClass} font-mono`} value={row.service} maxLength={64} autoComplete="off" spellCheck={false} disabled={disabled} aria-invalid={invalid(serviceError)} aria-describedby={describedBy(serviceId, serviceError)} onChange={(event) => onChange(row.id, "service", event.target.value)} />
      </Field>
      <Field id={countId} label="Replicas" error={countError}>
        <input id={countId} className={`${inputClass} tabular-nums`} type="number" inputMode="numeric" min={1} max={rolloutLimits.replicasPerService} step={1} value={row.count} disabled={disabled} aria-invalid={invalid(countError)} aria-describedby={describedBy(countId, countError)} onChange={(event) => onChange(row.id, "count", event.target.value)} />
      </Field>
      <div className="pt-6">
        <RemoveRow label={`Remove replica setting${row.service ? ` for ${row.service}` : ""}`} disabled={disabled} onRemove={onRemove} />
      </div>
    </div>
  );
}

function CheckFields({ id, row, errors, disabled, onChange, onRemove }: { id: string; row: CheckRow; errors: RolloutErrors; disabled: boolean; onChange: (rowId: string, key: "service" | "port" | "path", value: string) => void; onRemove: () => void }) {
  const serviceId = `${id}-${row.id}-check-service`;
  const portId = `${id}-${row.id}-check-port`;
  const pathId = `${id}-${row.id}-check-path`;
  const serviceError = errors[`${row.id}.service`];
  const portError = errors[`${row.id}.port`];
  const pathError = errors[`${row.id}.path`];
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-3 border-b border-line pb-3">
      <div className="grid min-w-0 gap-x-3 gap-y-2 sm:grid-cols-[minmax(0,1fr)_6rem_minmax(0,1.4fr)]">
        <Field id={serviceId} label="Compose service" error={serviceError}>
          <input id={serviceId} className={`${inputClass} font-mono`} value={row.service} maxLength={64} autoComplete="off" spellCheck={false} disabled={disabled} aria-invalid={invalid(serviceError)} aria-describedby={describedBy(serviceId, serviceError)} onChange={(event) => onChange(row.id, "service", event.target.value)} />
        </Field>
        <Field id={portId} label="Container port" error={portError}>
          <input id={portId} className={`${inputClass} tabular-nums`} type="number" inputMode="numeric" min={1} max={65535} step={1} value={row.port} disabled={disabled} aria-invalid={invalid(portError)} aria-describedby={describedBy(portId, portError)} onChange={(event) => onChange(row.id, "port", event.target.value)} />
        </Field>
        <Field id={pathId} label="Path" error={pathError}>
          <input id={pathId} className={`${inputClass} font-mono`} value={row.path} maxLength={1024} autoComplete="off" spellCheck={false} placeholder="/healthz" disabled={disabled} aria-invalid={invalid(pathError)} aria-describedby={describedBy(pathId, pathError)} onChange={(event) => onChange(row.id, "path", event.target.value)} />
        </Field>
      </div>
      <div className="pt-6">
        <RemoveRow label={`Remove HTTP check${row.service ? ` for ${row.service}` : ""}`} disabled={disabled} onRemove={onRemove} />
      </div>
    </div>
  );
}

// Conditions the worker checks against the resolved Compose file and images at deploy time.
export function RolloutEligibility({ resourceId }: { resourceId: string }) {
  return (
    <Notice title="Parallel replacement is conditional">
      <p>Each deploy checks the resolved Compose file and images. A parallel replacement needs:</p>
      <ul className="mt-1 list-disc space-y-0.5 pl-4">
        <li>a managed HTTP route (domain) on the resource and at most 32 Compose services;</li>
        <li>a Docker healthcheck or an HTTP check above for every service;</li>
        <li>no fixed host ports, container names, host namespaces, devices, privileged mode, added capabilities, links, profiles or lifecycle hooks;</li>
        <li>no deployment hooks, private connections, shared or external networks, custom address pools or fixed container IPs;</li>
        <li>writable volumes only when the option above is selected.</li>
      </ul>
      <p className="mt-2">
        Otherwise the deploy falls back to guarded Compose replacement and records why in the operation. That fallback can interrupt traffic, ignores replica settings and never
        stages a canary. A first deploy has no current release, so every strategy sends all traffic to it at once.{" "}
        <Link to="/resources/$resourceId/preflight" params={{ resourceId }} className={linkClass}>
          Run preflight
        </Link>{" "}
        to see whether the saved configuration qualifies.
      </p>
    </Notice>
  );
}

export function useRolloutSupport(workload: Pick<Workload, "node_id">) {
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  return { nodes, node, supported: Boolean(node?.capabilities?.rollouts) };
}

// Explains why replacement strategies are unavailable: the resource type, or a worker that cannot run them.
export function RolloutAvailability({ workload, editable }: { workload: Workload; editable: boolean }) {
  const { nodes, node, supported } = useRolloutSupport(workload);
  if (!rolloutsApply(workload)) {
    return <Notice>{workloadKind(workload.kind) === "job" ? "Jobs" : "Managed previews"} always use Compose replacement. Replacement strategies apply to services outside previews.</Notice>;
  }
  if (!editable) return null;
  if (nodes.isError) return <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />;
  if (!node || supported) return null;
  return (
    <Notice tone="warn" title="Worker upgrade needed">
      {node.name} does not report rollout support, so replacement strategies cannot be changed. Upgrade the worker, then verify it from the{" "}
      <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
        node page
      </Link>
      .{" "}
      {workload.rollout && workload.rollout.strategy !== "disabled"
        ? "This resource has a saved replacement strategy, so deploys and rollbacks are rejected until the worker is upgraded."
        : "Guarded deployment and other operations still work."}
    </Notice>
  );
}
