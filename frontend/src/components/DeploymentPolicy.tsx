import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { useCurrentTeam } from "../lib/session";
import { managesTeam } from "../lib/permissions";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { formatAbsolute } from "../lib/format";
import { Button, Field, Notice, Section, SwitchField } from "./ui";

interface Policy { approval: boolean; freeze_from: string | null; freeze_until: string | null; window_start: string; window_end: string }

function policySummary(policy: Policy | undefined, failed: boolean) {
  if (failed) return "Could not load the policy";
  if (!policy) return "Loading policy";
  const parts = [policy.approval ? "Approval required" : "No approval required"];
  // The API stores both freeze dates or neither; a freeze applies while start <= now < end.
  if (policy.freeze_from && policy.freeze_until) {
    const now = Date.now();
    if (now < Date.parse(policy.freeze_from)) parts.push(`Freeze from ${formatAbsolute(policy.freeze_from)}`);
    else if (now < Date.parse(policy.freeze_until)) parts.push(`Frozen until ${formatAbsolute(policy.freeze_until)}`);
  }
  parts.push(policy.window_start && policy.window_end ? `Deploys ${policy.window_start}–${policy.window_end} UTC` : "Any time of day");
  return parts.join(" · ");
}

export function DeploymentPolicy({ scope, id }: { scope: "project" | "environment"; id: string }) {
  const client = useQueryClient();
  const admin = managesTeam(useCurrentTeam());
  const online = useOnline();
  const key = ["deployment-policy", scope, id];
  const path = `/api/${scope}s/${id}/deployment-policy`;
  const query = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Policy>("GET", path, undefined, signal) });
  const save = useMutation({ mutationFn: (policy: Policy) => request("PUT", path, policy), onSuccess: () => client.invalidateQueries({ queryKey: key }) });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const from = formText(form, "from");
    const until = formText(form, "until");
    save.mutate({ approval: form.has("approval"), freeze_from: from ? `${from}:00Z` : null, freeze_until: until ? `${until}:00Z` : null, window_start: formText(form,"window_start"), window_end: formText(form,"window_end") });
  }
  return <Section title="Deployment policy" collapsed={{ summary: policySummary(query.data, query.isError) }} description="Project and environment rules both apply. Team administrators approve queued deployments and manage freeze windows.">
    <div className="space-y-4">
      {query.isError && <Notice tone="error">{errorMessage(query.error)}</Notice>}
      {query.data && <form key={id + JSON.stringify(query.data)} onSubmit={submit} className="space-y-4">
        <SwitchField id={`approval-${id}`} name="approval" defaultChecked={query.data.approval} disabled={!admin} label="Require approval for deployments and rollbacks" />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`freeze-from-${id}`} label="Freeze starts (UTC)" optional><input id={`freeze-from-${id}`} className={inputClass} name="from" type="datetime-local" defaultValue={query.data.freeze_from?.slice(0, 16) ?? ""} disabled={!admin} /></Field>
          <Field id={`freeze-until-${id}`} label="Freeze ends (UTC)" optional><input id={`freeze-until-${id}`} className={inputClass} name="until" type="datetime-local" defaultValue={query.data.freeze_until?.slice(0, 16) ?? ""} disabled={!admin} /></Field>
          <Field id={`window-start-${id}`} label="Daily deployment window starts (UTC)" optional><input id={`window-start-${id}`} className={inputClass} name="window_start" type="time" defaultValue={query.data.window_start} disabled={!admin} /></Field>
          <Field id={`window-end-${id}`} label="Daily deployment window ends (UTC)" optional><input id={`window-end-${id}`} className={inputClass} name="window_end" type="time" defaultValue={query.data.window_end} disabled={!admin} /></Field>
        </div>
        <p className="text-xs text-fg-muted">Clear both dates to remove the freeze. Running deployments continue; stop and recovery operations remain available. Approval authorizes the queued deployment, not an immutable review of branch contents or shared variables. Already-required approvals remain required if the policy is disabled.</p>
        <p className="text-xs text-fg-muted">An optional daily window allows new deployments only between those UTC times, including across midnight. Clear both times for any time of day. Project and environment windows must overlap. A deployment already running may finish outside the window.</p>
        {admin && <Button type="submit" pending={save.isPending} disabled={!online}>Save deployment policy</Button>}
      </form>}
      {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
      {save.isSuccess && <Notice>Deployment policy saved.</Notice>}
    </div>
  </Section>;
}
