import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { connectionsQuery, notificationMountsQuery, thresholdRulesQuery, type Connection, type ThresholdMetric, type ThresholdRule, type ThresholdRuleInput } from "../api/connections";
import { nodesQuery, workloadQuery, workloadsQuery } from "../api/queries";
import { teamNodesQuery } from "../api/teams";
import type { Node, WorkloadSummary } from "../api/types";
import { useOnline } from "../lib/hooks";
import { currentTeam, managesTeam } from "../lib/permissions";
import { useSession } from "../lib/session";
import { providerLabel } from "../lib/notifications";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { confirmationChoices, defaultThresholdMetric, formatDuration, isThresholdError, maxRuleChannels, metricScope, mountScopes, repeatChoices, thresholdCondition, thresholdDeliveryProblem, thresholdMetric, thresholdMetrics, thresholdRuleFromForm, thresholdUnits, type MountScope, type ThresholdFieldName, type ThresholdFormError, type ThresholdScope } from "../lib/threshold-rules";
import { formatAbsolute } from "../lib/format";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const maxRules = 100;
const description = "Alert when a node's CPU, memory, data disk or mount usage stays at or above a percentage, when a service's certificate nears expiry, or when its containers keep restarting, run out of memory or crash. Rules are checked about once a minute against fresh samples; samples older than two minutes and values that cannot be read neither trigger nor clear an alert. Rules cannot be edited: delete one and create a new rule to change it.";

function placement(workload: WorkloadSummary) {
  return `${workload.project ?? "Unknown project"} / ${workload.environment ?? "Project resources"}`;
}

function groupByPlacement(workloads: WorkloadSummary[]) {
  const groups = new Map<string, WorkloadSummary[]>();
  for (const workload of workloads) groups.set(placement(workload), [...(groups.get(placement(workload)) ?? []), workload]);
  return [...groups].sort(([a], [b]) => a.localeCompare(b));
}

function DomainField({ workloadID, value, onChange, error, fieldID }: { workloadID: string; value: string; onChange: (value: string) => void; error?: string; fieldID: string }) {
  const workload = useQuery({ ...workloadQuery(workloadID), enabled: workloadID !== "" });
  const domains = [...new Set((workload.data?.routes ?? []).map((route) => route.domain))].sort();
  let hint = "Domains configured on the service's routes.";
  if (workloadID === "") hint = "Select a service first.";
  else if (workload.isPending) hint = "Loading the service's domains…";
  else if (workload.isSuccess && domains.length === 0) hint = "This service has no configured domains. Add a route on its Domains page first.";
  const disabled = workloadID === "" || !workload.isSuccess || domains.length === 0;
  return (
    <Field id={fieldID} label="Domain" hint={workload.isError ? undefined : hint} error={error}>
      {workload.isError ? (
        <QueryError what="the service's domains" error={workload.error} onRetry={() => void workload.refetch()} retrying={workload.isFetching} />
      ) : (
        <Select id={fieldID} name="domain" value={value} onChange={(event) => onChange(event.target.value)} disabled={disabled} required aria-invalid={error ? true : undefined} aria-describedby={describedBy(fieldID, error, true)}>
          <option value="" disabled>Select a domain</option>
          {domains.map((domain) => <option key={domain} value={domain}>{domain}</option>)}
        </Select>
      )}
    </Field>
  );
}

function isMountScope(value: string): value is MountScope {
  return mountScopes.some((scope) => scope.value === value);
}

function MountScopeField({ node, value, onChange, fieldID }: { node: Node | undefined; value: MountScope; onChange: (value: MountScope) => void; fieldID: string }) {
  const supported = node?.capabilities?.mount_metrics === true;
  const selected = mountScopes.find((scope) => scope.value === value) ?? mountScopes[0]!;
  let hint = selected.hint;
  if (node === undefined) hint = `${hint} Select a node to watch other mounts.`;
  else if (!supported) hint = `${hint} ${node.name} does not report mounts; upgrade and verify its worker to watch other mounts.`;
  return (
    <Field id={fieldID} label="Filesystem" hint={hint}>
      <Select id={fieldID} name="mount_scope" value={value} onChange={(event) => isMountScope(event.target.value) && onChange(event.target.value)} aria-describedby={`${fieldID}-hint`}>
        {mountScopes.map((scope) => (
          <option key={scope.value} value={scope.value} disabled={scope.value !== "data" && !supported}>
            {scope.label}
          </option>
        ))}
      </Select>
    </Field>
  );
}

function MountPathField({ nodeID, value, onChange, error, fieldID }: { nodeID: string; value: string; onChange: (value: string) => void; error?: string; fieldID: string }) {
  const mounts = useQuery({ ...notificationMountsQuery(nodeID), enabled: nodeID !== "" });
  if (mounts.isError) {
    return (
      <Field id={fieldID} label="Mount" error={error}>
        {isStatus(mounts.error, 409) || isStatus(mounts.error, 404) ? (
          <Notice tone="warn" title="Mounts unavailable">{errorMessage(mounts.error)}</Notice>
        ) : (
          <QueryError what="the node's mounts" error={mounts.error} onRetry={() => void mounts.refetch()} retrying={mounts.isFetching} />
        )}
      </Field>
    );
  }
  const choices = (mounts.data?.mounts ?? []).filter((mount) => mount.known || mount.error).sort((a, b) => a.path.localeCompare(b.path));
  let hint = "Mount points the worker reported in its last sample.";
  if (mounts.isPending) hint = "Loading the node's mounts…";
  else if (choices.length === 0) hint = "The worker reported no measurable mounts.";
  else if (mounts.data) hint = `Mount points the worker reported at ${formatAbsolute(mounts.data.sampled_at)}.${mounts.data.limited ? " The list was clipped at the worker's limit, so some mounts are missing." : ""}`;
  return (
    <Field id={fieldID} label="Mount" hint={hint} error={error}>
      <Select id={fieldID} name="mount_path" value={value} onChange={(event) => onChange(event.target.value)} disabled={choices.length === 0} required aria-invalid={error ? true : undefined} aria-describedby={describedBy(fieldID, error, true)}>
        <option value="" disabled>Select a mount</option>
        {choices.map((mount) => (
          <option key={mount.path} value={mount.path}>
            {mount.error ? `${mount.path} (unreadable)` : mount.path}
          </option>
        ))}
      </Select>
    </Field>
  );
}

// Remounted per node (keyed by node id), so a scope the new node cannot measure never carries over.
function DiskScopeFields({ node, error, fieldID }: { node: Node | undefined; error?: string; fieldID: string }) {
  const [target, setTarget] = useState<MountScope>("data");
  const [path, setPath] = useState("");
  return (
    <>
      <MountScopeField
        node={node}
        value={target}
        onChange={(value) => {
          setTarget(value);
          setPath("");
        }}
        fieldID={`${fieldID}-scope`}
      />
      {target === "path" && node && <MountPathField nodeID={node.id} value={path} onChange={setPath} error={error} fieldID={fieldID} />}
      {target !== "data" && (
        <p className="text-xs text-fg-muted sm:col-span-2">
          Mounts are read in the worker's namespace; in container installs that may not include every host disk. Unknown, unreadable or clipped mount readings never count as recovery.
        </p>
      )}
    </>
  );
}

function ServiceField({ services, value, onChange, error, fieldID }: { services: WorkloadSummary[]; value: string; onChange: (value: string) => void; error?: string; fieldID: string }) {
  return (
    <Field id={fieldID} label="Service" hint={services.length === 0 ? "This team has no services. Jobs and one-shot containers are not watched." : "Long-running services in this team. Jobs are not watched."} error={error}>
      <Select id={fieldID} name="workload_id" value={value} onChange={(event) => onChange(event.target.value)} disabled={services.length === 0} required aria-invalid={error ? true : undefined} aria-describedby={describedBy(fieldID, error, true)}>
        <option value="" disabled>Select a service</option>
        {groupByPlacement(services).map(([label, group]) => (
          <optgroup key={label} label={label}>
            {group.map((workload) => <option key={workload.id} value={workload.id}>{workload.name}</option>)}
          </optgroup>
        ))}
      </Select>
    </Field>
  );
}

function ChannelChoices({ id, channels, scope, error }: { id: string; channels: Connection[]; scope: ThresholdScope; error?: string }) {
  return (
    <fieldset aria-describedby={`${id}-channels-hint${error ? ` ${id}-channels-error` : ""}`}>
      <legend className="text-[13px] font-medium text-fg">Channels</legend>
      <p id={`${id}-channels-hint`} className="mt-0.5 text-xs text-fg-muted">
        Up to {maxRuleChannels}. A channel only sends threshold alerts when it is enabled and includes health alerts, or selects both threshold events. Incident channels need the Threshold condition.{scope === "resource" && " Resource owner emails go to the owner of the selected service."}
      </p>
      <div className="mt-2 grid gap-1.5 sm:grid-cols-2">
        {channels.map((channel) => {
          const problem = thresholdDeliveryProblem(channel.notification, scope);
          return (
            <label key={channel.id} className={`${choiceClass} items-start`}>
              <Checkbox name="channel_ids" value={channel.id} className="mt-0.5" />
              <span className="min-w-0 break-words">
                {channel.name}
                <span className="block text-xs text-fg-muted">{providerLabel(channel.notification?.provider ?? "")}</span>
                {problem && <span className="block text-xs text-warn">{problem}</span>}
              </span>
            </label>
          );
        })}
      </div>
      {error && <p id={`${id}-channels-error`} className="mt-1.5 text-xs text-bad">{error}</p>}
    </fieldset>
  );
}

function ThresholdRuleForm({ nodes, services, channels, close }: { nodes: Node[]; services: WorkloadSummary[]; channels: Connection[]; close: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [metric, setMetric] = useState<ThresholdMetric>("cpu");
  const [workloadID, setWorkloadID] = useState("");
  const [domain, setDomain] = useState("");
  const [nodeID, setNodeID] = useState("");
  const [error, setError] = useState<ThresholdFormError>();
  const create = useMutation({
    mutationFn: (rule: ThresholdRuleInput) => request<{ id: string }>("POST", "/api/notifications/rules", rule),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: thresholdRulesQuery.queryKey });
      close();
    },
  });
  const fieldID = (field: ThresholdFieldName) => `${id}-${field}`;
  const message = (field: ThresholdFieldName) => (error?.field === field ? error.message : undefined);
  const selected = thresholdMetric(metric) ?? defaultThresholdMetric;
  const scope = selected.scope;
  const unit = thresholdUnits[selected.unit];
  const service = services.find((workload) => workload.id === workloadID);
  const serviceNode = service ? nodes.find((node) => node.id === service.node_id) : undefined;
  const needsUpgrade = scope === "resource" && serviceNode !== undefined && !serviceNode.capabilities?.alert_observations;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const rule = thresholdRuleFromForm(new FormData(event.currentTarget));
    if (isThresholdError(rule)) {
      setError(rule);
      const target = rule.field === "channel_ids" ? event.currentTarget.querySelector<HTMLElement>('input[name="channel_ids"]') : document.getElementById(fieldID(rule.field));
      target?.focus();
      return;
    }
    setError(undefined);
    create.mutate(rule);
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4 border-b border-line p-4">
      <h3 className="text-[13px] font-semibold text-fg">New threshold rule</h3>
      <fieldset disabled={create.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={fieldID("name")} label="Name" error={message("name")}>
            <input id={fieldID("name")} name="name" className={inputClass} maxLength={120} autoComplete="off" required aria-invalid={message("name") ? true : undefined} aria-describedby={message("name") ? `${fieldID("name")}-error` : undefined} />
          </Field>
          <Field id={fieldID("metric")} label="Metric" hint={selected.description} error={message("metric")}>
            <Select id={fieldID("metric")} name="metric" value={metric} onChange={(event) => setMetric(thresholdMetric(event.target.value)?.value ?? "cpu")} aria-describedby={describedBy(fieldID("metric"), message("metric"), true)}>
              <optgroup label="Node">
                {thresholdMetrics.filter((option) => option.scope === "node").map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
              </optgroup>
              <optgroup label="Service">
                {thresholdMetrics.filter((option) => option.scope === "resource").map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
              </optgroup>
            </Select>
          </Field>
          {scope === "node" ? (
            <Field id={fieldID("node_id")} label="Node" error={message("node_id")}>
              <Select id={fieldID("node_id")} name="node_id" value={nodeID} onChange={(event) => setNodeID(event.target.value)} required aria-invalid={message("node_id") ? true : undefined} aria-describedby={message("node_id") ? `${fieldID("node_id")}-error` : undefined}>
                <option value="" disabled>Select a node</option>
                {nodes.map((node) => <option key={node.id} value={node.id}>{node.name}</option>)}
              </Select>
            </Field>
          ) : (
            <ServiceField
              services={services}
              value={workloadID}
              onChange={(value) => {
                setWorkloadID(value);
                setDomain("");
              }}
              error={message("workload_id")}
              fieldID={fieldID("workload_id")}
            />
          )}
          {metric === "disk" && <DiskScopeFields key={nodeID} node={nodes.find((item) => item.id === nodeID)} error={message("mount_path")} fieldID={fieldID("mount_path")} />}
          {selected.unit === "days" && <DomainField workloadID={workloadID} value={domain} onChange={setDomain} error={message("domain")} fieldID={fieldID("domain")} />}
          <Field id={fieldID("threshold")} label={unit.label} hint={unit.hint} error={message("threshold")}>
            <input key={selected.unit} id={fieldID("threshold")} name="threshold" type="number" inputMode={selected.unit === "events" ? "numeric" : "decimal"} min={unit.min} max={unit.max} step={unit.step} className={inputClass} required aria-invalid={message("threshold") ? true : undefined} aria-describedby={describedBy(fieldID("threshold"), message("threshold"), true)} />
          </Field>
          <Field id={fieldID("confirmation_seconds")} label="Lasting at least" hint="Consecutive fresh samples must stay past the threshold for this long." error={message("confirmation_seconds")}>
            <Select id={fieldID("confirmation_seconds")} name="confirmation_seconds" defaultValue={300} aria-describedby={describedBy(fieldID("confirmation_seconds"), message("confirmation_seconds"), true)}>
              {confirmationChoices.map((seconds) => <option key={seconds} value={seconds}>{formatDuration(seconds)}</option>)}
            </Select>
          </Field>
          <Field id={fieldID("repeat_seconds")} label="Repeat at most every" hint="While the alert stays active. One recovery notice follows when it clears." error={message("repeat_seconds")}>
            <Select id={fieldID("repeat_seconds")} name="repeat_seconds" defaultValue={3600} aria-describedby={describedBy(fieldID("repeat_seconds"), message("repeat_seconds"), true)}>
              {repeatChoices.map((seconds) => <option key={seconds} value={seconds}>{formatDuration(seconds)}</option>)}
            </Select>
          </Field>
        </div>
        {scope === "resource" && (
          <p className="text-xs text-fg-muted">
            Unknown values, such as a certificate that cannot be read or Docker events that exceed the bounded backlog, are skipped. Events during Cove operations are not counted, container rules stay quiet while the service is manually stopped, and completed one-shot containers are excluded.
          </p>
        )}
        {needsUpgrade && (
          <Notice tone="warn" title="Upgrade this service's worker first">
            {serviceNode.name} has not reported support for certificate and container observations. Upgrade the worker on that node and verify it again before creating this rule.
          </Notice>
        )}
        <ChannelChoices id={id} channels={channels} scope={scope} error={message("channel_ids")} />
        {create.isError && <Notice tone="error" title="Could not create the rule">{errorMessage(create.error)}</Notice>}
      </fieldset>
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close} disabled={create.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={create.isPending} disabled={!online || needsUpgrade}>Create rule</Button>
      </div>
    </form>
  );
}

function RuleRow({ rule, target, channels, warning, onDelete, disabled }: { rule: ThresholdRule; target: string; channels: Map<string, Connection>; warning?: string; onDelete: () => void; disabled: boolean }) {
  const names = rule.channel_ids.map((channel) => channels.get(channel)?.name ?? "Deleted channel");
  return (
    <li className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-4 py-2.5 hover:bg-raised">
      <div className="min-w-0 flex-1">
        <p className="break-words text-[13px] font-medium text-fg">{rule.name}</p>
        <p className="break-words text-xs text-fg-muted">
          {thresholdCondition(rule)} on {target} · repeats at most every {formatDuration(rule.repeat_seconds)}
        </p>
        <p className="break-words text-xs text-fg-muted">Channels: {names.join(", ")}</p>
        {warning && <p className="text-xs text-warn">{warning}</p>}
      </div>
      <Button size="sm" variant="ghost" onClick={onDelete} disabled={disabled} aria-label={`Delete ${rule.name}`}>Delete</Button>
    </li>
  );
}

function ruleTarget(rule: ThresholdRule, nodes: Node[], workloads: WorkloadSummary[], teamNodes: Node[]) {
  if (metricScope(rule.metric) === "node") {
    const node = nodes.find((item) => item.id === rule.node_id);
    const assigned = teamNodes.some((item) => item.id === rule.node_id);
    return { target: node?.name ?? "an unavailable node", warning: assigned ? undefined : "The node is no longer assigned to this team, so this rule is not evaluated." };
  }
  const workload = workloads.find((item) => item.id === rule.workload_id);
  if (!workload) return { target: "an unavailable service", warning: "The service is no longer available to this team, so this rule is not evaluated." };
  const assigned = teamNodes.some((item) => item.id === workload.node_id);
  return {
    target: `${workload.name} (${placement(workload)})`,
    warning: assigned ? undefined : "The service's node is no longer assigned to this team, so this rule is not evaluated.",
  };
}

function RulesBody({ rules, nodes, teamNodes, workloads, channels, creating, closeForm, deleteDisabled, onDelete }: { rules: ThresholdRule[]; nodes: Node[]; teamNodes: Node[]; workloads: WorkloadSummary[]; channels: Connection[]; creating: boolean; closeForm: () => void; deleteDisabled: boolean; onDelete: (rule: ThresholdRule) => void }) {
  const services = workloads.filter((workload) => workload.kind === "service").sort((a, b) => a.name.localeCompare(b.name));
  const channelMap = new Map(channels.map((channel) => [channel.id, channel]));
  const unavailable = teamNodes.length === 0 ? "No nodes are assigned to this team." : channels.length === 0 ? "Add a notification channel before creating a threshold rule." : undefined;
  return (
    <>
      {creating && (unavailable ? (
        <div className="space-y-3 border-b border-line p-4">
          <Notice tone="warn" title="A rule needs a node and a channel">{unavailable}</Notice>
          <div className="flex justify-end"><Button variant="ghost" onClick={closeForm}>Close</Button></div>
        </div>
      ) : (
        <ThresholdRuleForm nodes={teamNodes} services={services} channels={channels} close={closeForm} />
      ))}
      {rules.length >= maxRules && <div className="border-b border-line p-4"><Notice tone="warn">This team has the maximum of {maxRules} threshold rules. Delete a rule to add another.</Notice></div>}
      {rules.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-fg-muted">No threshold rules yet. Node unreachable and low disk alerts still apply without rules.</p>
      ) : (
        <ul className="divide-y divide-line" aria-label="Threshold rules">
          {rules.map((rule) => {
            const { target, warning } = ruleTarget(rule, nodes, workloads, teamNodes);
            return <RuleRow key={rule.id} rule={rule} target={target} warning={warning} channels={channelMap} disabled={deleteDisabled} onDelete={() => onDelete(rule)} />;
          })}
        </ul>
      )}
    </>
  );
}

function ManagedThresholdRules() {
  const session = useSession();
  const client = useQueryClient();
  const online = useOnline();
  const admin = Boolean(session?.is_admin);
  const teamID = session?.team_id ?? "";
  const rules = useQuery(thresholdRulesQuery);
  const channels = useQuery(connectionsQuery("notifications"));
  const nodes = useQuery(nodesQuery);
  const workloads = useQuery(workloadsQuery);
  // Instance administrators are sent every node, but rules only accept nodes assigned to the team.
  const assigned = useQuery({ ...teamNodesQuery(teamID), enabled: admin && teamID !== "" });
  const [creating, setCreating] = useState(false);
  const [remove, setRemove] = useState<ThresholdRule | null>(null);
  const deletion = useMutation({
    mutationFn: (rule: ThresholdRule) => request("DELETE", `/api/notifications/rules/${encodeURIComponent(rule.id)}`),
    onSuccess: async () => {
      setRemove(null);
      await client.invalidateQueries({ queryKey: thresholdRulesQuery.queryKey });
    },
  });

  const failed = [rules, channels, nodes, workloads, assigned].find((query) => query.isError);
  const pending = rules.isPending || channels.isPending || nodes.isPending || workloads.isPending || (admin && assigned.isPending);
  const count = rules.data?.length ?? 0;
  const atCap = count >= maxRules;
  const action = !failed && !pending && !creating && (
    <Button variant="primary" onClick={() => setCreating(true)} disabled={!online || atCap}>
      <Plus aria-hidden className="size-3.5" />
      New rule
    </Button>
  );

  let body;
  if (failed) {
    body = <div className="p-4"><QueryError what="threshold rules" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} /></div>;
  } else if (pending || !rules.data || !channels.data || !nodes.data || !workloads.data) {
    body = <LoadingRows label="Loading threshold rules" rows={2} />;
  } else {
    const assignedIDs = admin ? new Set(assigned.data ?? []) : null;
    body = (
      <>
        {deletion.isError && <div className="border-b border-line p-4"><Notice tone="error" title="Could not delete the rule">{errorMessage(deletion.error)}</Notice></div>}
        <RulesBody
          rules={rules.data}
          nodes={nodes.data}
          teamNodes={nodes.data.filter((node) => !assignedIDs || assignedIDs.has(node.id))}
          workloads={workloads.data}
          channels={channels.data.filter((channel) => channel.notification)}
          creating={creating}
          closeForm={() => setCreating(false)}
          deleteDisabled={!online || deletion.isPending}
          onDelete={(rule) => {
            deletion.reset();
            setRemove(rule);
          }}
        />
      </>
    );
  }

  return (
    <Section title="Threshold rules" description={description} actions={action} flush>
      {body}
      <ConfirmDialog
        open={remove !== null}
        title={`Delete ${remove?.name ?? "rule"}?`}
        confirmLabel="Delete rule"
        danger
        pending={deletion.isPending}
        onCancel={() => setRemove(null)}
        onConfirm={() => {
          if (remove) deletion.mutate(remove);
        }}
      >
        The rule stops being evaluated and its alert is cleared in Cove. No recovery notice is sent, and an open PagerDuty or Opsgenie incident for it stays open until you resolve it there.
      </ConfirmDialog>
    </Section>
  );
}

export function ThresholdRules() {
  const session = useSession();
  const manager = Boolean(session?.is_admin) || managesTeam(currentTeam(session));
  if (!manager) {
    return (
      <Section title="Threshold rules" description={description}>
        <Notice title="Team owners and admins manage threshold rules">Ask a team owner or admin to add or remove rules.</Notice>
      </Section>
    );
  }
  return <ManagedThresholdRules />;
}
