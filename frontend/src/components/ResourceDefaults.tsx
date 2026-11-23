import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { nodesQuery } from "../api/queries";
import { teamNodesQuery } from "../api/teams";
import { useSession } from "../lib/session";
import { resourceDefaultsQuery, useSaveResourceDefaults, type DefaultsScope, type ResourceDefaults } from "../api/resourceDefaults";
import { useOnline } from "../lib/hooks";
import { describeDefaults, formatCpus, formatMiB, hasDefaults, limitText, inheritDefaults, nodesWithoutDefaultsSupport, parseLimits, type LimitErrors } from "../lib/resource-defaults";
import { LimitInput } from "./LimitInput";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const title = "Resource defaults";
const precedence =
  "Defaults apply to each Compose service at its next deploy, including existing resources. Saving does not change running containers. Cove resource limits come first, then limits in the Compose file, then environment defaults, then team defaults.";

function sectionDescription(target: DefaultsScope) {
  return target.scope === "team"
    ? "CPU and memory caps for Compose services that set no limit of their own, across every project in this team."
    : "CPU and memory caps for Compose services in this environment that set no limit of their own. Blank fields inherit the team default.";
}

function WorkerSupport({ active }: { active: boolean }) {
  const session = useSession();
  const admin = Boolean(session?.is_admin);
  const teamId = session?.team_id ?? "";
  const nodes = useQuery(nodesQuery);
  // Instance administrators are sent every node, but only nodes assigned to the team run its deploys.
  const assigned = useQuery({ ...teamNodesQuery(teamId), enabled: admin && teamId !== "" });
  if (!active) return null;
  if (nodes.isError || assigned.isError) {
    return (
      <Notice tone="warn" title="Worker support could not be checked">
        Deploys with resource defaults are refused on workers that predate them. Upgrade and verify the team&apos;s nodes before relying on these defaults.
      </Notice>
    );
  }
  if (!nodes.data || (admin && !assigned.data)) return null;
  const assignedIds = new Set(assigned.data);
  const teamNodes = admin ? nodes.data.filter((node) => assignedIds.has(node.id)) : nodes.data;
  const outdated = nodesWithoutDefaultsSupport(teamNodes);
  if (outdated.length === 0) return null;
  const names = outdated.slice(0, 5).map((node) => node.name);
  return (
    <Notice tone="warn" title="Some workers need an upgrade">
      {names.join(", ")}
      {outdated.length > names.length ? ` and ${outdated.length - names.length} more` : ""} {outdated.length === 1 ? "does" : "do"} not report resource defaults support. Deploys
      that receive defaults are refused there until the worker is upgraded and verified.
    </Notice>
  );
}

function EditWarnings({ online, active }: { online: boolean; active: boolean }) {
  return (
    <>
      <WorkerSupport active={active} />
      {!online && <Notice tone="warn">You are offline. Reconnect to save.</Notice>}
    </>
  );
}

function inheritedHint(target: DefaultsScope, team: number, format: (value: number) => string, kind: string) {
  if (target.scope === "team") return `Blank means no team default for ${kind}.`;
  return team > 0 ? `Blank inherits the team default of ${format(team)}.` : `Blank inherits the team default, which is not set for ${kind}.`;
}

function DefaultsForm({ target, saved, canEdit }: { target: DefaultsScope; saved: ResourceDefaults; canEdit: boolean }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveResourceDefaults(target);
  const [cpus, setCpus] = useState(() => limitText(saved.configured.cpus));
  const [memory, setMemory] = useState(() => limitText(saved.configured.memory_mb));
  const [errors, setErrors] = useState<LimitErrors>({});
  const dirty = cpus.trim() !== limitText(saved.configured.cpus) || memory.trim() !== limitText(saved.configured.memory_mb);

  const parsed = parseLimits(cpus, memory);
  const draft = parsed.draft;
  const draftEffective = target.scope === "team" ? draft : inheritDefaults(saved.team, draft);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setErrors(parsed.errors);
    if (parsed.errors.cpus || parsed.errors.memory) {
      document.getElementById(parsed.errors.cpus ? `${id}-cpus` : `${id}-memory`)?.focus();
      return;
    }
    save.mutate(draft, {
      onSuccess: (result) => {
        setCpus(limitText(result.configured.cpus));
        setMemory(limitText(result.configured.memory_mb));
      },
    });
  }

  function discard() {
    setCpus(limitText(saved.configured.cpus));
    setMemory(limitText(saved.configured.memory_mb));
    setErrors({});
    save.reset();
  }

  const fields = (
    <SettingRows>
      <Field layout="row" id={`${id}-cpus`} label="CPU cores" optional hint={inheritedHint(target, saved.team.cpus, formatCpus, "CPU")} error={errors.cpus}>
        <LimitInput id={`${id}-cpus`} kind="cpus" value={cpus} error={errors.cpus} onChange={(value) => { setCpus(value); setErrors((current) => ({ ...current, cpus: undefined })); }} />
      </Field>
      <Field layout="row" id={`${id}-memory`} label="Memory (MiB)" optional hint={inheritedHint(target, saved.team.memory_mb, formatMiB, "memory")} error={errors.memory}>
        <LimitInput id={`${id}-memory`} kind="memory" value={memory} error={errors.memory} onChange={(value) => { setMemory(value); setErrors((current) => ({ ...current, memory: undefined })); }} />
      </Field>
      <SettingRow label={target.scope === "team" ? "Applied defaults" : "Applied in this environment"} description="What services without their own limits receive at their next deploy.">
        <p className="text-[13px] text-fg">{describeDefaults(dirty ? draftEffective : saved.effective)}</p>
      </SettingRow>
    </SettingRows>
  );

  return (
    <form onSubmit={submit} noValidate>
      {canEdit && <UnsavedChangesGuard dirty={dirty && !save.isPending} />}
      <Section
        title={title}
        description={sectionDescription(target)}
        width="form"
        footer={canEdit && <SaveBar dirty={dirty} pending={save.isPending} blocked={!online} onDiscard={discard} status={save.isSuccess ? "Saved. Applies at each resource's next deploy." : undefined} />}
      >
        <div className="space-y-4">
          {!canEdit && <p className="text-xs text-fg-muted">Team owners and admins can change these defaults.</p>}
          <fieldset disabled={!canEdit || save.isPending} className="min-w-0">
            {fields}
          </fieldset>
          <p className="text-xs text-fg-muted">{precedence} Rollback keeps the release it restores. Limits are caps, not reservations.</p>
          {canEdit && <EditWarnings online={online} active={hasDefaults(draftEffective)} />}
          {save.isError && <Notice tone="error" title="Could not save resource defaults">{errorMessage(save.error)}</Notice>}
        </div>
      </Section>
    </form>
  );
}

export function ResourceDefaultsSection({ target, canEdit }: { target: DefaultsScope; canEdit: boolean }) {
  const defaults = useQuery(resourceDefaultsQuery(target));
  if (defaults.isPending) {
    return (
      <Section title={title} description={sectionDescription(target)} width="form">
        <LoadingRows label="Loading resource defaults" rows={1} />
      </Section>
    );
  }
  if (defaults.isError) {
    return (
      <Section title={title} description={sectionDescription(target)} width="form">
        <QueryError what="resource defaults" error={defaults.error} onRetry={() => void defaults.refetch()} retrying={defaults.isFetching} />
      </Section>
    );
  }
  return <DefaultsForm target={target} saved={defaults.data} canEdit={canEdit} />;
}

// Read-only explanation for a resource's limits page. Viewers without access to the defaults still get the precedence rules.
export function ResourceDefaultsNote({ environmentId }: { environmentId?: string }) {
  const target: DefaultsScope = environmentId ? { scope: "environment", id: environmentId } : { scope: "team" };
  const defaults = useQuery(resourceDefaultsQuery(target));
  const origin = environmentId ? "environment and team" : "team";
  let current = "";
  if (defaults.data) {
    current = hasDefaults(defaults.data.effective)
      ? ` Current ${origin} defaults: ${describeDefaults(defaults.data.effective)}.`
      : ` No ${origin} defaults are set.`;
  }
  return (
    <p className="text-xs text-fg-muted">
      Services with no Cove or Compose limit use the {origin} resource defaults at their next deploy.{current}
    </p>
  );
}
