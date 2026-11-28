import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCw } from "lucide-react";
import { useState, type FormEvent } from "react";
import { buildNodeQuery, choiceName, useSaveBuildNode, type BuildNodeSettings } from "../api/build-nodes";
import { errorMessage, isStatus } from "../api/client";
import { workloadOperationsQuery } from "../api/queries";
import type { Operation, Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { inputClass, linkClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const description =
  "The worker that builds and publishes images for this resource. The resource keeps running on its runtime node, and changes apply to future builds only.";

const hint =
  "Source, variables, build secrets and the selected registry credentials are sent to this worker for each build. A node that holds builds for this resource cannot be deleted until the resource is cleaned up.";

// Only the server knows whether an operation is queued, so unread or failed operation lists block saving like an active one.
function saveBlocker(settings: UseQueryResult<BuildNodeSettings>, operations: UseQueryResult<Operation[]>, active: Operation | undefined, online: boolean) {
  if (!online) return "You are offline.";
  if (settings.isError) return "The current settings could not be read.";
  if (operations.isPending) return "Checking for active operations.";
  if (operations.isError) return "Operations could not be read.";
  if (active) return "An operation is in progress.";
  return undefined;
}

export function BuildNodeSetting({ workload }: { workload: Workload }) {
  const settings = useQuery(buildNodeQuery(workload.id));
  if (!can(workload.permissions, "edit_configuration")) {
    return (
      <Section title="Build node" description={description} width="form">
        <div className="space-y-4">
          <div className="space-y-2 empty:hidden" aria-live="polite">
            {settings.isError && settings.data && <RefreshError settings={settings} canEdit={false} />}
            {settings.data && <SavedAssignment settings={settings.data} canEdit={false} />}
          </div>
          <BuildNodeControl settings={settings} canEdit={false} value="" disabled onChange={() => undefined} />
        </div>
      </Section>
    );
  }
  return <BuildNodeForm workload={workload} settings={settings} />;
}

function BuildNodeForm({ workload, settings }: { workload: Workload; settings: UseQueryResult<BuildNodeSettings> }) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const online = useOnline();
  const save = useSaveBuildNode(workload.id);
  const [selection, setSelection] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const data = settings.data;
  const savedValue = data && data.node_id !== data.runtime_node_id ? data.node_id : "";
  const dirty = selection !== null && selection !== savedValue;
  const active = activeOperation(operations.data ?? [], workload.id);
  const blocked = saveBlocker(settings, operations, active, online);

  function choose(next: string) {
    setSelection(next);
    setSaved(false);
    save.reset();
  }

  function discard() {
    setSelection(null);
    save.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (selection === null || !dirty || blocked) return;
    save.mutate(selection, {
      onSuccess: () => {
        setSelection(null);
        setSaved(true);
      },
    });
  }

  const status = blocked ? `${blocked} Saving is paused.` : saved ? "Build node saved." : "No unsaved changes";
  return (
    <form onSubmit={submit} aria-label="Build node setting" noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Build node"
        description={description}
        width="form"
        footer={<SaveBar dirty={dirty} pending={save.isPending} blocked={Boolean(blocked)} label="Save build node" onDiscard={discard} status={status} />}
      >
        <div className="space-y-4">
          <div className="space-y-2 empty:hidden" aria-live="polite">
            {settings.isError && data && <RefreshError settings={settings} canEdit />}
            <SaveNotices active={active} online={online} operationsFailed={operations.isError} />
            {save.isError && (
              <Notice tone={isStatus(save.error, 409) ? "warn" : "error"} title="The build node was not saved">
                <p className="break-words">{errorMessage(save.error)}</p>
                <p className="mt-1">Your choice is kept.</p>
              </Notice>
            )}
            {data && <SavedAssignment settings={data} canEdit />}
          </div>
          <BuildNodeControl settings={settings} canEdit value={selection ?? savedValue} disabled={save.isPending} onChange={choose} />
        </div>
      </Section>
    </form>
  );
}

function BuildNodeControl({ settings, canEdit, value, disabled, onChange }: { settings: UseQueryResult<BuildNodeSettings>; canEdit: boolean; value: string; disabled: boolean; onChange: (value: string) => void }) {
  const data = settings.data;
  if (settings.isPending) return <LoadingRows label="Loading build node" rows={1} />;
  if (!data) return <QueryError what="the build node" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  if (!canEdit) {
    return (
      <SettingRows>
        <SettingRow label="Build node" description={hint}>
          <EffectiveNode settings={data} />
        </SettingRow>
      </SettingRows>
    );
  }
  return (
    <SettingRows>
      <Field id="build-node" label="Build node" hint={hint} layout="row">
        <Select id="build-node" className={inputClass} value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)} aria-describedby="build-node-hint">
          {buildNodeOptions(data)}
        </Select>
        <Unavailable settings={data} />
      </Field>
    </SettingRows>
  );
}

function RefreshError({ settings, canEdit }: { settings: UseQueryResult<BuildNodeSettings>; canEdit: boolean }) {
  return (
    <Notice
      tone="error"
      title="Could not refresh the build node"
      action={
        <Button variant="ghost" size="sm" onClick={() => void settings.refetch()} pending={settings.isFetching}>
          {!settings.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      }
    >
      <p className="break-words">{errorMessage(settings.error)}</p>
      <p className="mt-1">Showing the last setting that loaded. Building{canEdit ? " and saving are" : " is"} paused until the current setting loads.</p>
    </Notice>
  );
}

function SaveNotices({ active, online, operationsFailed }: { active?: Operation; online: boolean; operationsFailed: boolean }) {
  return (
    <>
      {!online && <Notice tone="warn" title="You are offline">Saving is paused until the connection returns.</Notice>}
      {active && (
        <Notice
          tone="warn"
          title="An operation is in progress"
          action={
            <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
              View operation
            </Link>
          }
        >
          The build node cannot change until it finishes.
        </Notice>
      )}
      {operationsFailed && <Notice tone="warn" title="Operations could not be read">Saving is paused until Cove can confirm that no operation is active.</Notice>}
    </>
  );
}

// Select reads its options from direct option children, so these are returned as elements rather than rendered by a component.
function buildNodeOptions(settings: BuildNodeSettings) {
  const runtime = settings.runtime_node_id;
  const others = settings.nodes.filter((node) => node.id !== runtime);
  const savedMissing = settings.node_id !== "" && settings.node_id !== runtime && !others.some((node) => node.id === settings.node_id);
  return (
    <>
      <option value="">Runtime node ({choiceName(settings, runtime)})</option>
      {others.map((node) => (
        <option key={node.id} value={node.id} disabled={!node.eligible}>
          {node.eligible ? node.name || node.id : `${node.name || node.id} (unavailable)`}
        </option>
      ))}
      {savedMissing && (
        <option value={settings.node_id} disabled>
          {settings.node_id} (no longer listed)
        </option>
      )}
    </>
  );
}

function Unavailable({ settings }: { settings: BuildNodeSettings }) {
  const unavailable = settings.nodes.filter((node) => !node.eligible);
  const designated = settings.nodes.some((node) => node.id !== settings.runtime_node_id && node.eligible);
  return (
    <div className="space-y-1 text-xs text-fg-muted">
      {!designated && <p>No other node available to this team can build for this resource.</p>}
      {unavailable.length > 0 && (
        <ul aria-label="Nodes that cannot build" className="space-y-1">
          {unavailable.map((node) => (
            <li key={node.id} className="break-words">
              <span className="font-medium text-fg">{node.name || node.id}</span>
              {node.id === settings.runtime_node_id && " (runtime node)"}: {node.reason || "Not available for builds."}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function EffectiveNode({ settings }: { settings: BuildNodeSettings }) {
  const id = settings.effective_node_id;
  return (
    <span className="min-w-0 break-words">
      {choiceName(settings, id)}
      <span className="text-fg-muted">{id === settings.runtime_node_id ? " (runtime node)" : " (designated)"}</span>
    </span>
  );
}

// A designated node that lost eligibility stays saved and shown; builds are refused until it recovers or someone changes it.
function SavedAssignment({ settings, canEdit }: { settings: BuildNodeSettings; canEdit: boolean }) {
  if (settings.node_id === "" || settings.node_id === settings.runtime_node_id) return null;
  const choice = settings.nodes.find((node) => node.id === settings.node_id);
  if (choice?.eligible) return null;
  return (
    <Notice tone="warn" title={`The saved build node ${choice?.name || settings.node_id} is unavailable`}>
      <p className="break-words">{choice ? choice.reason || "It cannot build for this resource." : "It is no longer available to this team."}</p>
      <p className="mt-1">Builds are refused until it is available again{canEdit ? " or you choose another node" : ""}. The saved choice is not changed automatically.</p>
    </Notice>
  );
}
