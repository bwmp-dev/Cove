import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Plus, RotateCw, Search } from "lucide-react";
import { useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { ApiError, errorMessage } from "../api/client";
import { nodeGPUsQuery, workloadGPUsQuery, type GPUDevice, type GPUInventory } from "../api/gpus";
import type { Node, Workload } from "../api/types";
import { formatAbsolute } from "../lib/format";
import {
  composeServiceNames,
  deviceOptions,
  deviceState,
  formatGPUMemory,
  gpuRows,
  maxGPUsPerService,
  mergeGPUAssignments,
  validateGPURows,
  type GPUMode,
  type GPURow,
  type GPURowErrors,
  type Limits,
} from "../lib/gpus";
import { useOnline } from "../lib/hooks";
import { nodeStates } from "../lib/nodes";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, Section } from "./ui";

const driverLabels: Record<GPUDevice["driver"], string> = { nvidia: "NVIDIA", cdi: "CDI" };

const requirements =
  "Native NVIDIA devices need the NVIDIA driver and NVIDIA Container Toolkit on the node. CDI devices need root-owned specs in /etc/cdi or /var/run/cdi. The node's operator installs these; Cove never installs or changes drivers.";
const emptyMeaning = "Cove reports only native NVIDIA devices and root-owned CDI GPU specs, so this does not mean the host has no GPU hardware.";

function inspectionBlocker(node: Node | undefined, online: boolean) {
  if (!online) return "You are offline. Reconnect to inspect GPUs.";
  if (!node) return undefined;
  if (!node.capabilities) return "Cove has no current probe result for this worker. Verify the node first.";
  if (!node.capabilities.gpu_assignments) return `${node.name} has not reported GPU support. Upgrade the worker to a Linux build that supports GPU assignments and verify the node.`;
  if (node.state !== "ready") return `The node is ${nodeStates[node.state]?.label.toLowerCase() ?? node.state}. GPU inspection needs a ready worker.`;
  return undefined;
}

function inspectionErrorTitle(error: unknown) {
  if (!(error instanceof ApiError)) return "Could not inspect GPUs";
  switch (error.status) {
    case 409:
      return "The worker cannot inspect GPUs yet";
    case 403:
      return "You do not have access to inspect GPUs here";
    case 404:
      return "The node no longer exists";
    case 502:
      return "GPU inspection failed on the node";
    case 503:
      return "Cove could not read the placement";
    default:
      return "Could not inspect GPUs";
  }
}

function InspectionError({ error }: { error: unknown }) {
  return (
    <Notice tone="error" title={inspectionErrorTitle(error)}>
      {errorMessage(error)}
    </Notice>
  );
}

function InspectButton({ query, disabled }: { query: UseQueryResult<GPUInventory>; disabled: boolean }) {
  const inspected = query.data !== undefined || query.isError;
  return (
    <Button onClick={() => void query.refetch()} pending={query.isFetching} disabled={disabled}>
      {!query.isFetching && (inspected ? <RotateCw aria-hidden className="size-3.5" /> : <Search aria-hidden className="size-3.5" />)}
      {inspected ? "Inspect again" : "Inspect GPUs"}
    </Button>
  );
}

function DeviceStatus({ device }: { device: GPUDevice }) {
  return device.assignable ? <StatusLabel tone="ok" label="Assignable" /> : <StatusLabel tone="warn" label="Not assignable" />;
}

// collapsed puts the device list behind a disclosure, for pages that repeat the devices as choices.
export function GPUInventoryList({ inventory, collapsed = false }: { inventory: GPUInventory; collapsed?: boolean }) {
  const list = (
    <ul aria-label="Observed GPUs" className={`divide-y divide-line rounded-md border border-line ${collapsed ? "mt-2" : ""}`}>
      {inventory.devices.map((device) => (
        <li key={device.id} className="grid min-w-0 gap-x-4 gap-y-1 px-3 py-2.5 sm:grid-cols-[minmax(0,1fr)_auto]">
          <div className="min-w-0">
            <p className="text-[13px] font-medium break-words text-fg">{device.name || "Unnamed device"}</p>
            <p className="font-mono text-xs break-all text-fg-muted">{device.id}</p>
            <p className="mt-0.5 text-xs text-fg-muted">
              {driverLabels[device.driver]} · Memory{" "}
              {device.memory_mb === null ? <span className="text-warn">unknown</span> : <span className="tabular-nums">{formatGPUMemory(device.memory_mb)}</span>}
            </p>
            {device.notice && <p className="mt-0.5 text-xs text-fg-muted">{device.notice}</p>}
          </div>
          <div className="sm:text-right">
            <DeviceStatus device={device} />
          </div>
        </li>
      ))}
    </ul>
  );
  return (
    <div className="space-y-3">
      <p className="text-xs text-fg-muted">
        Inspected <RelativeTime value={inventory.checked_at} /> <span>({formatAbsolute(inventory.checked_at)})</span>
      </p>
      {inventory.devices.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No GPUs were observed. {emptyMeaning}</p>
      ) : collapsed ? (
        <details className="text-xs">
          <summary className="w-fit cursor-pointer rounded text-fg-muted hover:text-fg">Observed devices ({inventory.devices.length})</summary>
          {list}
        </details>
      ) : (
        list
      )}
      {inventory.limited && <Notice tone="warn" title="Inventory clipped">The node reported more devices than Cove lists. Only the first {inventory.devices.length} are shown.</Notice>}
      {inventory.notices.length > 0 && (
        <Notice tone="warn" title="Inspection notes">
          <ul className="list-disc space-y-0.5 pl-4">
            {inventory.notices.map((notice) => (
              <li key={notice}>{notice}</li>
            ))}
          </ul>
        </Notice>
      )}
    </div>
  );
}

function InventoryBody({ query, blocker, idle, collapsed = false }: { query: UseQueryResult<GPUInventory>; blocker: string | undefined; idle: ReactNode | undefined; collapsed?: boolean }) {
  return (
    <div className="space-y-3">
      {blocker && <p className="text-[13px] text-fg-muted">{blocker}</p>}
      {query.isError && <InspectionError error={query.error} />}
      {query.isFetching && !query.data ? (
        <LoadingRows label="Inspecting GPUs" rows={2} />
      ) : query.data ? (
        <GPUInventoryList inventory={query.data} collapsed={collapsed} />
      ) : (
        !blocker && !query.isError && idle && <p className="text-[13px] text-fg-muted">{idle}</p>
      )}
    </div>
  );
}

export function NodeGPUs({ node }: { node: Node }) {
  const online = useOnline();
  const query = useQuery(nodeGPUsQuery(node.id));
  const blocker = inspectionBlocker(node, online);
  return (
    <Section
      title="GPUs"
      description="Devices resources on this node can be assigned. Inspecting reads the node's native NVIDIA tools and CDI specs without changing anything."
      actions={<InspectButton query={query} disabled={blocker !== undefined} />}
    >
      <div className="space-y-3">
        <InventoryBody query={query} blocker={blocker} idle="Not inspected yet." />
        <p className="text-xs text-fg-muted">{requirements}</p>
      </div>
    </Section>
  );
}

const modes: { value: GPUMode; label: string; description: string }[] = [
  { value: "inherit", label: "Inherit Compose", description: "Use the GPU requests in the Compose file, if any." },
  { value: "none", label: "No standard GPU requests", description: "Remove Compose GPU reservations and CDI GPU mappings." },
  { value: "select", label: "Select devices", description: "Replace them with the devices chosen below." },
];

const modeLabels: Record<GPUMode, string> = { inherit: "Inherit Compose", none: "No standard GPU requests", select: "Selected devices" };

function SavedGPUs({ rows }: { rows: GPURow[] }) {
  if (rows.length === 0) return <p className="text-[13px] text-fg-muted">Every service inherits GPU requests from Compose.</p>;
  return (
    <ul aria-label="Saved GPU settings" className="divide-y divide-line rounded-md border border-line">
      {rows.map((row) => (
        <li key={row.key} className="min-w-0 px-3 py-2.5">
          <p className="font-mono text-[13px] break-all text-fg">{row.service}</p>
          <p className="text-xs text-fg-muted">{modeLabels[row.mode]}</p>
          {row.selected.length > 0 && (
            <ul className="mt-1 font-mono text-xs break-all text-fg-muted">
              {row.selected.map((id) => (
                <li key={id}>{id}</li>
              ))}
            </ul>
          )}
        </li>
      ))}
    </ul>
  );
}

function deviceNote(id: string, inventory: GPUInventory | undefined): { tone: "warn" | "muted"; text: string } | undefined {
  const state = deviceState(id, inventory);
  if (state.kind === "unchecked") return { tone: "muted", text: "Saved. Inspect GPUs to check that it is still available." };
  if (state.kind === "missing") return { tone: "warn", text: "Not observed in the latest inspection. Deploys fail until it is available again; clear it to remove it." };
  if (state.kind === "unassignable") return { tone: "warn", text: state.device.notice || "Observed but not assignable. Deploys fail until it is assignable again; clear it to remove it." };
  return undefined;
}

function DeviceChoices({ row, saved, inventory, errorId, onToggle }: { row: GPURow; saved: string[]; inventory: GPUInventory | undefined; errorId: string | undefined; onToggle: (id: string, checked: boolean) => void }) {
  const options = deviceOptions(row.selected, saved, inventory);
  const selected = new Set(row.selected);
  const keepable = new Set(saved);
  const full = row.selected.length >= maxGPUsPerService;
  if (options.length === 0) {
    return <p className="text-[13px] text-fg-muted">{inventory ? `No GPUs were observed on the node. ${emptyMeaning}` : "Inspect GPUs to choose devices observed on the resource's node."}</p>;
  }
  return (
    <fieldset aria-describedby={errorId}>
      <legend className="mb-1.5 text-[13px] font-medium text-fg">
        Devices for <span className="font-mono">{row.service || "this service"}</span>
        <span className="ml-1.5 font-normal text-fg-muted tabular-nums">
          {row.selected.length} of up to {maxGPUsPerService} selected
        </span>
      </legend>
      <div className="grid gap-2">
        {options.map((id) => {
          const checked = selected.has(id);
          const state = deviceState(id, inventory);
          const device = state.kind === "available" || state.kind === "unassignable" ? state.device : undefined;
          // Saved devices stay selectable so they can be kept; new selections must be observed and assignable.
          const disabled = !checked && (full || (state.kind !== "available" && !keepable.has(id)));
          let note = checked ? deviceNote(id, inventory) : undefined;
          if (!checked && keepable.has(id) && state.kind !== "available") note = { tone: "muted", text: "Saved device, cleared. Select it again to keep it." };
          else if (!checked && device && !device.assignable) note = { tone: "muted", text: device.notice || "Not assignable." };
          return (
            <label key={id} className={`${choiceClass} items-start`}>
              <Checkbox className="mt-0.5" checked={checked} disabled={disabled} onChange={(event) => onToggle(id, event.target.checked)} />
              <span className="min-w-0">
                <span className="block break-words text-fg">{device?.name || (checked || keepable.has(id) ? "Saved device" : "Unnamed device")}</span>
                <span className="block font-mono text-xs break-all">{id}</span>
                {device && (
                  <span className="block text-xs">
                    {driverLabels[device.driver]} · Memory {device.memory_mb === null ? "unknown" : formatGPUMemory(device.memory_mb)}
                  </span>
                )}
                {note && <span className={`block text-xs ${note.tone === "warn" ? "text-warn" : ""}`}>{note.text}</span>}
              </span>
            </label>
          );
        })}
      </div>
      {full && <p className="mt-1.5 text-xs text-fg-muted">The limit of {maxGPUsPerService} devices for one service is reached.</p>}
    </fieldset>
  );
}

function ModeChoices({ name, legend, value, invalid, errorId, onChange }: { name: string; legend: string; value: GPUMode; invalid: boolean; errorId: string | undefined; onChange: (mode: GPUMode) => void }) {
  return (
    <fieldset>
      <legend className="mb-1.5 text-[13px] font-medium text-fg">{legend}</legend>
      <div className="divide-y divide-line overflow-hidden rounded-md border border-line-strong md:flex md:divide-x md:divide-y-0">
        {modes.map((mode) => (
          <label
            key={mode.value}
            className="flex flex-1 cursor-pointer items-start gap-2.5 px-3 py-2.5 hover:bg-panel has-[:checked]:bg-raised has-[:focus-visible]:outline-2 has-[:focus-visible]:-outline-offset-2 has-[:focus-visible]:outline-info pointer-coarse:min-h-11"
          >
            <input
              type="radio"
              name={name}
              value={mode.value}
              checked={value === mode.value}
              onChange={() => onChange(mode.value)}
              aria-invalid={mode.value === "select" && invalid ? true : undefined}
              aria-describedby={mode.value === "select" ? errorId : undefined}
              className="mt-0.5 accent-fg focus-visible:outline-none"
            />
            <span className="text-[13px] font-medium text-fg">
              {mode.label}
              <span className="mt-0.5 block text-xs font-normal text-fg-muted">{mode.description}</span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

export interface GPUSaveState {
  pending: boolean;
  blocked: boolean;
  saved: boolean;
  error: unknown;
  reset: () => void;
}

interface AssignmentProps {
  workload: Workload;
  node: Node | undefined;
  editable: boolean;
  save: GPUSaveState;
  blockers: ReactNode;
  onSave: (limits: Limits) => void;
}

const sectionTitle = "GPU assignments";
const sectionDescription = "Choose which GPUs each Compose service requests on the next deploy. Running containers keep their devices until then. CPU and memory limits are not changed here.";
const sharing = "Cove does not reserve devices: the same GPU can be assigned to other services and resources, and containers sharing it are not isolated from each other.";

function UnsupportedWorker({ node }: { node: Node | undefined }) {
  if (!node?.capabilities || node.capabilities.gpu_assignments) return null;
  return (
    <Notice tone="warn" title="This worker cannot deploy GPU settings">
      {node.name} has not reported GPU assignment support. Deploys with an explicit setting, including No standard GPU requests, are refused until the worker is upgraded and verified. Inherit Compose keeps working.
    </Notice>
  );
}

function SavedGPUSettings({ workload, node }: { workload: Workload; node: Node | undefined }) {
  return (
    <Section width="form" title={sectionTitle} description={sectionDescription}>
      <div className="space-y-4">
        <UnsupportedWorker node={node} />
        <SavedGPUs rows={gpuRows(workload.limits)} />
        <p className="text-xs text-fg-muted">
          Selections apply on the next deploy and are checked against the node's live inventory first. {sharing} {requirements}
        </p>
      </div>
    </Section>
  );
}

function comparable(rows: GPURow[]) {
  return JSON.stringify(rows.map(({ service, mode, selected }) => ({ service, mode, selected: mode === "select" ? selected : [] })));
}

interface Inspection {
  query: UseQueryResult<GPUInventory>;
  blocker: string | undefined;
}

interface ServiceRowProps {
  idPrefix: string;
  row: GPURow;
  errors: GPURowErrors;
  composeServices: string[] | null;
  saved: string[];
  inventory: GPUInventory | undefined;
  onChange: (update: (row: GPURow) => GPURow) => void;
  onRemove: () => void;
}

function ServiceRow({ idPrefix, row, errors, composeServices, saved, inventory, onChange, onRemove }: ServiceRowProps) {
  const serviceId = `${idPrefix}-service`;
  const selectionErrorId = errors.selection ? `${idPrefix}-selection-error` : undefined;
  return (
    <div className="min-w-0 space-y-3 border-b border-line pb-4">
      <Field id={serviceId} label="Compose service" error={errors.service} hint={composeServices ? `Services in the saved Compose file: ${composeServices.join(", ")}` : undefined}>
        <input
          id={serviceId}
          className={`${inputClass} font-mono`}
          value={row.service}
          onChange={(event) => onChange((current) => ({ ...current, service: event.target.value }))}
          aria-invalid={errors.service ? true : undefined}
          aria-describedby={describedBy(serviceId, errors.service, composeServices)}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
      <ModeChoices
        name={`${idPrefix}-mode`}
        legend={`GPU requests for ${row.service || "this service"}`}
        value={row.mode}
        invalid={Boolean(errors.selection)}
        errorId={selectionErrorId}
        onChange={(mode) => onChange((current) => ({ ...current, mode }))}
      />
      {row.mode === "select" && (
        <DeviceChoices
          row={row}
          saved={saved}
          inventory={inventory}
          errorId={selectionErrorId}
          onToggle={(device, checked) => onChange((current) => ({ ...current, selected: checked ? [...current.selected, device] : current.selected.filter((item) => item !== device) }))}
        />
      )}
      {errors.selection && (
        <p id={selectionErrorId} className="text-xs text-bad">
          {errors.selection}
        </p>
      )}
      <Button variant="ghost" size="sm" onClick={onRemove}>
        Remove {row.service || "service"}
      </Button>
    </div>
  );
}

function AssignmentForm({ workload, node, save, blockers, onSave, inspection }: AssignmentProps & { inspection: Inspection }) {
  const inventory = inspection.query.data;
  const id = useId();
  const [initial] = useState(() => gpuRows(workload.limits));
  const [rows, setRows] = useState(initial);
  const [errors, setErrors] = useState<Record<string, GPURowErrors>>({});
  const created = useRef(0);
  const composeServices = composeServiceNames(workload.compose);
  const savedSelections = new Map(initial.map((row) => [row.key, row.selected]));
  const dirty = comparable(rows) !== comparable(initial);
  const explicit = rows.some((row) => row.mode !== "inherit");
  const unsupported = Boolean(node?.capabilities && !node.capabilities.gpu_assignments);

  function change(key: string, update: (row: GPURow) => GPURow) {
    save.reset();
    setRows((current) => current.map((row) => (row.key === key ? update(row) : row)));
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const next = validateGPURows(rows, composeServices);
    setErrors(next);
    if (Object.keys(next).length > 0) {
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    onSave(mergeGPUAssignments(workload.limits, rows));
  }
  function discard() {
    setRows(initial);
    setErrors({});
    save.reset();
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title={sectionTitle}
        description={sectionDescription}
        actions={<InspectButton query={inspection.query} disabled={inspection.blocker !== undefined} />}
        footer={<SaveBar dirty={dirty} pending={save.pending} blocked={save.blocked} label="Save GPU assignments" onDiscard={discard} status={save.saved ? "Saved. Changes apply on the next deploy." : undefined} />}
      >
        <div className="space-y-4">
          <UnsupportedWorker node={node} />
          {blockers}
          <InventoryBody query={inspection.query} blocker={unsupported ? undefined : inspection.blocker} idle={unsupported ? undefined : `Not inspected yet. Inspect GPUs to list the devices observed on ${node?.name ?? "the resource's node"}.`} collapsed />
          <fieldset disabled={save.pending} className="min-w-0 space-y-4">
            {rows.length === 0 && <p className="text-[13px] text-fg-muted">Every service inherits GPU requests from Compose.</p>}
            {rows.map((row) => (
              <ServiceRow
                key={row.key}
                idPrefix={`${id}-${row.key}`}
                row={row}
                errors={errors[row.key] ?? {}}
                composeServices={composeServices}
                saved={savedSelections.get(row.key) ?? []}
                inventory={inventory}
                onChange={(update) => change(row.key, update)}
                onRemove={() => {
                  save.reset();
                  setRows((current) => current.filter((item) => item.key !== row.key));
                }}
              />
            ))}
            <Button
              disabled={rows.length >= 64}
              onClick={() => {
                save.reset();
                created.current += 1;
                const key = `new-${created.current}`;
                setRows((current) => [...current, { key, service: "", mode: "select", selected: [] }]);
              }}
            >
              <Plus aria-hidden className="size-3.5" />
              Add service GPU setting
            </Button>
            {Object.keys(errors).length > 0 && <Notice tone="error" title="Check GPU assignments">Fix the highlighted services before saving.</Notice>}
            {save.error !== null && save.error !== undefined && <Notice tone="error" title="Could not save GPU assignments">{errorMessage(save.error)}</Notice>}
          </fieldset>
          <p className="text-xs text-fg-muted">
            {explicit ? "Selections apply on the next deploy and are checked against the node's live inventory first. " : ""}
            {sharing} {requirements}
          </p>
        </div>
      </Section>
    </form>
  );
}

export function ResourceGPUs(props: AssignmentProps) {
  const { workload, editable } = props;
  // Viewers and operators cannot inspect, so their page never creates the inventory query.
  return editable ? <EditableGPUs {...props} /> : <SavedGPUSettings workload={workload} node={props.node} />;
}

function EditableGPUs(props: AssignmentProps) {
  const online = useOnline();
  const query = useQuery(workloadGPUsQuery(props.workload.id));
  const blocker = inspectionBlocker(props.node, online);
  return <AssignmentForm key={comparable(gpuRows(props.workload.limits))} {...props} inspection={{ query, blocker }} />;
}
