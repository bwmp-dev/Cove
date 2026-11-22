import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { connectionsQuery, thresholdRulesQuery, type Connection, type ThresholdMetric, type ThresholdRule, type ThresholdRuleInput } from "../api/connections";
import { nodesQuery } from "../api/queries";
import { teamNodesQuery } from "../api/teams";
import type { Node } from "../api/types";
import { useOnline } from "../lib/hooks";
import { currentTeam, managesTeam } from "../lib/permissions";
import { useSession } from "../lib/session";
import { providerLabel } from "../lib/notifications";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { confirmationChoices, formatDuration, formatPercent, isThresholdError, maxRuleChannels, metricLabel, repeatChoices, thresholdDeliveryProblem, thresholdMetrics, thresholdRuleFromForm, type ThresholdFieldName, type ThresholdFormError } from "../lib/threshold-rules";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const maxRules = 100;
const description = "Alert when a node's CPU, memory or data disk usage stays at or above a percentage. Rules are checked against fresh node samples about once a minute; samples older than two minutes neither trigger nor clear an alert. Rules cannot be edited: delete one and create a new rule to change it.";

function ThresholdRuleForm({ nodes, channels, close }: { nodes: Node[]; channels: Connection[]; close: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [metric, setMetric] = useState<ThresholdMetric>("cpu");
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

  const channelsError = message("channel_ids");
  return (
    <form onSubmit={submit} noValidate className="space-y-4 border-b border-line p-4">
      <h3 className="text-[13px] font-semibold text-fg">New threshold rule</h3>
      <fieldset disabled={create.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={fieldID("name")} label="Name" error={message("name")}>
            <input id={fieldID("name")} name="name" className={inputClass} maxLength={120} autoComplete="off" required aria-invalid={message("name") ? true : undefined} aria-describedby={message("name") ? `${fieldID("name")}-error` : undefined} />
          </Field>
          <Field id={fieldID("node_id")} label="Node" error={message("node_id")}>
            <Select id={fieldID("node_id")} name="node_id" defaultValue="" required aria-invalid={message("node_id") ? true : undefined} aria-describedby={message("node_id") ? `${fieldID("node_id")}-error` : undefined}>
              <option value="" disabled>Select a node</option>
              {nodes.map((node) => <option key={node.id} value={node.id}>{node.name}</option>)}
            </Select>
          </Field>
          <Field id={fieldID("metric")} label="Metric" hint={thresholdMetrics.find((option) => option.value === metric)?.description} error={message("metric")}>
            <Select id={fieldID("metric")} name="metric" value={metric} onChange={(event) => setMetric(thresholdMetrics.find((option) => option.value === event.target.value)?.value ?? "cpu")} aria-describedby={describedBy(fieldID("metric"), message("metric"), true)}>
              {thresholdMetrics.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
            </Select>
          </Field>
          <Field id={fieldID("threshold")} label="Threshold (%)" hint="Alert at or above this percentage, above 0 and at most 100." error={message("threshold")}>
            <input id={fieldID("threshold")} name="threshold" type="number" inputMode="decimal" min={0.1} max={100} step="any" className={inputClass} required aria-invalid={message("threshold") ? true : undefined} aria-describedby={describedBy(fieldID("threshold"), message("threshold"), true)} />
          </Field>
          <Field id={fieldID("confirmation_seconds")} label="Lasting at least" hint="Consecutive fresh samples must stay at or above the threshold for this long." error={message("confirmation_seconds")}>
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
        <fieldset aria-describedby={`${id}-channels-hint${channelsError ? ` ${id}-channels-error` : ""}`}>
          <legend className="text-[13px] font-medium text-fg">Channels</legend>
          <p id={`${id}-channels-hint`} className="mt-0.5 text-xs text-fg-muted">
            Up to {maxRuleChannels}. A channel only sends threshold alerts when it is enabled and includes health alerts, or selects both threshold events. Incident channels need the Threshold condition.
          </p>
          <div className="mt-2 grid gap-1.5 sm:grid-cols-2">
            {channels.map((channel) => {
              const problem = thresholdDeliveryProblem(channel.notification);
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
          {channelsError && <p id={`${id}-channels-error`} className="mt-1.5 text-xs text-bad">{channelsError}</p>}
        </fieldset>
        {create.isError && <Notice tone="error" title="Could not create the rule">{errorMessage(create.error)}</Notice>}
      </fieldset>
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close} disabled={create.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={create.isPending} disabled={!online}>Create rule</Button>
      </div>
    </form>
  );
}

function ruleCondition(rule: ThresholdRule) {
  return `${metricLabel(rule.metric)} at or above ${formatPercent(rule.threshold)} for ${formatDuration(rule.confirmation_seconds)}`;
}

function RuleRow({ rule, node, channels, assigned, onDelete, disabled }: { rule: ThresholdRule; node: Node | undefined; channels: Map<string, Connection>; assigned: boolean; onDelete: () => void; disabled: boolean }) {
  const names = rule.channel_ids.map((channel) => channels.get(channel)?.name ?? "Deleted channel");
  return (
    <li className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-4 py-2.5 hover:bg-raised">
      <div className="min-w-0 flex-1">
        <p className="break-words text-[13px] font-medium text-fg">{rule.name}</p>
        <p className="break-words text-xs text-fg-muted">
          {ruleCondition(rule)} on {node?.name ?? "an unavailable node"} · repeats at most every {formatDuration(rule.repeat_seconds)}
        </p>
        <p className="break-words text-xs text-fg-muted">Channels: {names.join(", ")}</p>
        {!assigned && <p className="text-xs text-warn">The node is no longer assigned to this team, so this rule is not evaluated.</p>}
      </div>
      <Button size="sm" variant="ghost" onClick={onDelete} disabled={disabled} aria-label={`Delete ${rule.name}`}>Delete</Button>
    </li>
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

  const failed = [rules, channels, nodes, assigned].find((query) => query.isError);
  const pending = rules.isPending || channels.isPending || nodes.isPending || (admin && assigned.isPending);
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
  } else if (pending || !rules.data || !channels.data || !nodes.data) {
    body = <LoadingRows label="Loading threshold rules" rows={2} />;
  } else {
    const assignedIDs = admin ? new Set(assigned.data ?? []) : null;
    const teamNodes = nodes.data.filter((node) => !assignedIDs || assignedIDs.has(node.id));
    const notificationChannels = channels.data.filter((channel) => channel.notification);
    const channelMap = new Map(notificationChannels.map((channel) => [channel.id, channel]));
    const unavailable = teamNodes.length === 0 ? "No nodes are assigned to this team." : notificationChannels.length === 0 ? "Add a notification channel before creating a threshold rule." : undefined;
    body = (
      <>
        {creating && (unavailable ? (
          <div className="space-y-3 border-b border-line p-4">
            <Notice tone="warn" title="A rule needs a node and a channel">{unavailable}</Notice>
            <div className="flex justify-end"><Button variant="ghost" onClick={() => setCreating(false)}>Close</Button></div>
          </div>
        ) : (
          <ThresholdRuleForm nodes={teamNodes} channels={notificationChannels} close={() => setCreating(false)} />
        ))}
        {deletion.isError && <div className="border-b border-line p-4"><Notice tone="error" title="Could not delete the rule">{errorMessage(deletion.error)}</Notice></div>}
        {atCap && <div className="border-b border-line p-4"><Notice tone="warn">This team has the maximum of {maxRules} threshold rules. Delete a rule to add another.</Notice></div>}
        {rules.data.length === 0 ? (
          <p className="px-4 py-6 text-[13px] text-fg-muted">No threshold rules yet. Node unreachable and low disk alerts still apply without rules.</p>
        ) : (
          <ul className="divide-y divide-line" aria-label="Threshold rules">
            {rules.data.map((rule) => (
              <RuleRow
                key={rule.id}
                rule={rule}
                node={nodes.data.find((node) => node.id === rule.node_id)}
                channels={channelMap}
                assigned={teamNodes.some((node) => node.id === rule.node_id)}
                disabled={!online || deletion.isPending}
                onDelete={() => {
                  deletion.reset();
                  setRemove(rule);
                }}
              />
            ))}
          </ul>
        )}
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
