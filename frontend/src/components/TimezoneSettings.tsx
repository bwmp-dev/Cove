import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { saveTeamTimezone, saveUserTimezone, teamTimezoneQuery, timezonePreferenceQuery, type TimezonePreference } from "../api/timezone";
import { useOnline } from "../lib/hooks";
import { useSession } from "../lib/session";
import { inputClass } from "../lib/styles";
import { browserTimeZone, scheduleTimeZone, timeZoneOptions } from "../lib/timezone";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

interface ZoneFormProps {
  title: string;
  description: string;
  label: string;
  hint: ReactNode;
  saved: string;
  inheritLabel?: string;
  pending: boolean;
  error: unknown;
  succeeded: boolean;
  onSave: (zone: string) => void;
  onEdit: () => void;
  children?: ReactNode;
}

function ZoneForm({ title, description, label, hint, saved, inheritLabel, pending, error, succeeded, onSave, onEdit, children }: ZoneFormProps) {
  const id = useId();
  const online = useOnline();
  const [draft, setDraft] = useState(saved);
  const dirty = draft !== saved;
  const options = timeZoneOptions([saved]);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (dirty) onSave(draft);
  }

  return (
    <form className="min-w-0" onSubmit={submit}>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title={title}
        description={description}
        width="form"
        footer={<SaveBar dirty={dirty} pending={pending} blocked={!online} label="Save time zone" onDiscard={() => setDraft(saved)} status={succeeded ? "Time zone saved" : undefined} />}
      >
        <div className="space-y-4">
          <SettingRows>
            <Field layout="row" id={`${id}-zone`} label={label} hint={hint}>
              <Select
                id={`${id}-zone`}
                className={inputClass}
                value={draft}
                onChange={(event) => {
                  setDraft(event.target.value);
                  onEdit();
                }}
                aria-describedby={`${id}-zone-hint`}
              >
                {inheritLabel !== undefined && <option value="">{inheritLabel}</option>}
                {options.map((zone) => (
                  <option key={zone} value={zone}>
                    {zone}
                  </option>
                ))}
              </Select>
            </Field>
            {children}
          </SettingRows>
          {Boolean(error) && (
            <Notice tone="error" title="Could not save the time zone">
              {errorMessage(error)}
            </Notice>
          )}
        </div>
      </Section>
    </form>
  );
}

function ScheduleRow() {
  return (
    <SettingRow label="Schedules" description="Cron expressions and scheduled backups always run in UTC. Only how times are displayed changes.">
      <span className="font-mono text-xs">{scheduleTimeZone}</span>
    </SettingRow>
  );
}

function Frame({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Section title={title} width="form">
      {children}
    </Section>
  );
}

export function AccountTimezone() {
  const session = useSession();
  const client = useQueryClient();
  const teamId = session?.team_id ?? "";
  const preference = useQuery({ ...timezonePreferenceQuery(teamId), enabled: Boolean(teamId) });
  const save = useMutation({
    mutationFn: saveUserTimezone,
    gcTime: 0,
    onSuccess: (data) => {
      client.setQueryData(timezonePreferenceQuery(teamId).queryKey, data);
      void client.invalidateQueries({ queryKey: teamTimezoneQuery(teamId).queryKey });
    },
  });
  const title = "Time zone";

  if (!teamId) {
    return (
      <Frame title={title}>
        <p className="text-[13px] text-fg-muted">Join or create a team to choose how times are displayed. Times use this browser's zone ({browserTimeZone()}) until then.</p>
      </Frame>
    );
  }
  if (preference.isPending) {
    return (
      <Frame title={title}>
        <LoadingRows label="Loading time zone" rows={1} />
      </Frame>
    );
  }
  if (preference.isError) {
    return (
      <Frame title={title}>
        <QueryError what="your time zone" error={preference.error} onRetry={() => void preference.refetch()} retrying={preference.isFetching} />
      </Frame>
    );
  }
  const data = preference.data;
  return (
    <ZoneForm
      key={data.user_timezone}
      title={title}
      description="Applies to you in every team. Leave it on the team default to follow each team's setting."
      label="Display time zone"
      hint={`Times currently show in ${data.timezone}. This browser uses ${browserTimeZone()}.`}
      saved={data.user_timezone}
      inheritLabel={`Team default (${data.team_timezone})`}
      pending={save.isPending}
      error={save.error}
      succeeded={save.isSuccess}
      onSave={(zone) => save.mutate(zone)}
      onEdit={() => save.reset()}
    >
      <ScheduleRow />
    </ZoneForm>
  );
}

export function TeamTimezone({ teamId }: { teamId: string }) {
  const session = useSession();
  const client = useQueryClient();
  const setting = useQuery(teamTimezoneQuery(teamId));
  const save = useMutation({
    mutationFn: (zone: string) => saveTeamTimezone(teamId, zone),
    gcTime: 0,
    onSuccess: (data: TimezonePreference) => {
      client.setQueryData(teamTimezoneQuery(teamId).queryKey, data);
      // The response resolves the effective zone for the saved team, which is only the active one when it is selected.
      if (session?.team_id === teamId) client.setQueryData(timezonePreferenceQuery(teamId).queryKey, data);
    },
  });
  const title = "Team time zone";

  if (setting.isPending) {
    return (
      <Frame title={title}>
        <LoadingRows label="Loading team time zone" rows={1} />
      </Frame>
    );
  }
  if (setting.isError) {
    return (
      <Frame title={title}>
        <QueryError what="the team time zone" error={setting.error} onRetry={() => void setting.refetch()} retrying={setting.isFetching} />
      </Frame>
    );
  }
  return (
    <ZoneForm
      key={setting.data.team_timezone}
      title={title}
      description="The default for everyone in this team who has not chosen their own time zone."
      label="Default time zone"
      hint={setting.data.user_timezone ? `Your personal setting (${setting.data.user_timezone}) overrides this for you.` : "Members can override it in their account settings."}
      saved={setting.data.team_timezone}
      pending={save.isPending}
      error={save.error}
      succeeded={save.isSuccess}
      onSave={(zone) => save.mutate(zone)}
      onEdit={() => save.reset()}
    >
      <ScheduleRow />
    </ZoneForm>
  );
}
