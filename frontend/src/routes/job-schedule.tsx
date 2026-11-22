import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { Button, Field, Notice, Section } from "../components/ui";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { displayTimeZoneLabel, scheduleTimeZone, useDisplayTimeZone } from "../lib/timezone";
import { useResource } from "./workload-detail";

interface Schedule { cron: string; next_run_at: string | null }

export function JobSchedule() {
 const resource = useResource();
 const client = useQueryClient();
 const online = useOnline();
 const displayZone = useDisplayTimeZone();
 const key = ["job-schedule", resource.id];
 const schedule = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Schedule>("GET", `/api/workloads/${resource.id}/schedule`, undefined, signal), refetchInterval: 30_000 });
 const save = useMutation({ mutationFn: (cron: string) => request<Schedule>("PUT", `/api/workloads/${resource.id}/schedule`, { cron }), onSuccess: data => { client.setQueryData(key, data); } });
 function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); save.mutate(formText(new FormData(event.currentTarget), "cron").trim()); }
 return <Section width="form" title="Scheduled job" description="Run this one-shot Compose job on a UTC schedule. Each run appears in Operations. Overlapping runs and ticks missed during a panel outage are skipped.">
  <div className="space-y-4">
   {resource.kind !== "job" ? <Notice>Create a one-shot job resource to schedule a command. Existing services are not restarted by this scheduler.</Notice> : <>
    {resource.manually_stopped && <Notice tone="warn">This job was manually stopped. Scheduled runs are skipped until you explicitly deploy it again.</Notice>}
    {schedule.isError && <Notice tone="error">{errorMessage(schedule.error)}</Notice>}
    {schedule.data && <form onSubmit={submit} className="space-y-3">
     <Field id="job-cron" label="Cron expression (UTC)" hint="Five numeric fields: minute hour day month weekday (Sunday = 0). Supports *, lists, ranges, and steps. Example: 0 3 * * * runs at 03:00 UTC. Leave empty to disable."><input key={resource.id + schedule.data.cron} id="job-cron" name="cron" defaultValue={schedule.data.cron} className={inputClass} maxLength={256} disabled={!can(resource.permissions, "edit")} /></Field>
     <p className="text-sm text-fg-muted">{schedule.data.next_run_at ? `Next scheduled tick: ${formatAbsolute(schedule.data.next_run_at, displayZone)} in the display time zone, ${new Date(schedule.data.next_run_at).toUTCString()}` : "Scheduling is disabled."}</p>
     <p className="text-xs text-fg-muted">Schedule time zone: {scheduleTimeZone}. Display time zone: {displayTimeZoneLabel(displayZone)}.</p>
     {can(resource.permissions, "edit") && <Button type="submit" pending={save.isPending} disabled={!online}>Save schedule</Button>}
    </form>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
   </>}
  </div>
 </Section>;
}
