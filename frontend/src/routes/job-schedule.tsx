import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { RelativeTime } from "../components/RelativeTime";
import { Button, Field, Notice, Section } from "../components/ui";
import { formatAbsolute, formatAbsoluteInZone } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { useResource } from "./workload-detail";

interface Schedule { cron: string; timezone: string; next_run_at: string | null }
interface ScheduleInput { cron: string; timezone: string }

function NextRun({ schedule }: { schedule: Schedule }) {
 if (!schedule.next_run_at) return <p className="text-[13px] text-fg-muted">Scheduling is disabled.</p>;
 const zoned = formatAbsoluteInZone(schedule.next_run_at, schedule.timezone);
 return <p className="text-[13px] text-fg-muted">
  Next run: <time dateTime={schedule.next_run_at} className="text-fg">{zoned ?? `${formatAbsolute(schedule.next_run_at)} your time`}</time> ({schedule.timezone}), <RelativeTime value={schedule.next_run_at} />
 </p>;
}

export function JobSchedule() {
 const resource = useResource();
 const client = useQueryClient();
 const online = useOnline();
 const key = ["job-schedule", resource.id];
 const editable = can(resource.permissions, "edit");
 const schedule = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Schedule>("GET", `/api/workloads/${resource.id}/schedule`, undefined, signal), refetchInterval: 30_000 });
 const save = useMutation({ mutationFn: (input: ScheduleInput) => request<Schedule>("PUT", `/api/workloads/${resource.id}/schedule`, input), onSuccess: data => { client.setQueryData(key, data); } });
 function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  save.mutate({ cron: formText(form, "cron").trim(), timezone: formText(form, "timezone").trim() });
 }
 const formKey = schedule.data ? `${resource.id}:${schedule.data.cron}:${schedule.data.timezone}` : resource.id;
 return <Section width="form" title="Scheduled job" description="Run this one-shot Compose job on a cron schedule in the time zone you choose. Each run appears in Operations. Overlapping runs and ticks missed during a panel outage are skipped.">
  <div className="space-y-4">
   {resource.kind !== "job" ? <Notice>Create a one-shot job resource to schedule a command. Existing services are not restarted by this scheduler.</Notice> : <>
    {resource.manually_stopped && <Notice tone="warn">This job was manually stopped. Scheduled runs are skipped until you explicitly deploy it again.</Notice>}
    {schedule.isError && <Notice tone="error">{errorMessage(schedule.error)}</Notice>}
    {schedule.data && <form key={formKey} onSubmit={submit} className="space-y-3">
     <div className="grid gap-4 sm:grid-cols-2">
      <Field id="job-cron" label="Cron expression" hint="Five numeric fields: minute hour day month weekday (Sunday = 0). Supports *, lists, ranges, and steps. Example: 0 3 * * * runs at 03:00 in the schedule's time zone. Leave empty to disable."><input id="job-cron" name="cron" defaultValue={schedule.data.cron} className={`${inputClass} font-mono`} maxLength={256} spellCheck={false} autoComplete="off" aria-describedby="job-cron-hint" disabled={!editable} /></Field>
      <Field id="job-timezone" label="Time zone" hint="IANA time zone name, such as America/Los_Angeles. Jobs use UTC unless you change it. A local time skipped by a daylight saving change does not run, and a repeated one can run twice."><input id="job-timezone" name="timezone" defaultValue={schedule.data.timezone} className={`${inputClass} font-mono`} required maxLength={128} spellCheck={false} autoComplete="off" aria-describedby="job-timezone-hint" disabled={!editable} /></Field>
     </div>
     <NextRun schedule={schedule.data} />
     {editable && <Button type="submit" pending={save.isPending} disabled={!online}>Save schedule</Button>}
    </form>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
   </>}
  </div>
 </Section>;
}
