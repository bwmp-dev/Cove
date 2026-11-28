import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { useUpdateWorkloadOwner } from "../api/mutations";
import { workloadOwnerQuery, type WorkloadOwner } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const title = "Owner";
const description = "The team member responsible for this resource. Notification channels set to email the resource owner use this person's verified address. Ownership grants no additional access.";

function OwnerForm({ id, owner }: { id: string; owner: WorkloadOwner }) {
  const fieldID = useId();
  const online = useOnline();
  const update = useUpdateWorkloadOwner(id);
  const [selected, setSelected] = useState(owner.user_id);
  const dirty = selected !== owner.user_id;
  // The saved owner is always offered, so the field never shows a value that is not in the list.
  const candidates = owner.user_id && !owner.candidates.some((candidate) => candidate.user_id === owner.user_id) ? [{ user_id: owner.user_id, name: owner.name }, ...owner.candidates] : owner.candidates;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update.mutate(selected);
  }

  return (
    <form onSubmit={submit}>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title={title}
        description={description}
        footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={() => { update.reset(); setSelected(owner.user_id); }} status={update.isSuccess ? "Owner saved." : undefined} />}
      >
        <div className="space-y-4">
          <SettingRows>
            <Field layout="row" id={fieldID} label="Resource owner" hint="Active members of this team.">
              <Select id={fieldID} className={inputClass} value={selected} disabled={update.isPending} onChange={(event) => { update.reset(); setSelected(event.target.value); }} aria-describedby={`${fieldID}-hint`}>
                <option value="">No owner</option>
                {candidates.map((candidate) => <option key={candidate.user_id} value={candidate.user_id}>{candidate.name || candidate.user_id}</option>)}
              </Select>
            </Field>
          </SettingRows>
          {update.isError && <Notice tone="error" title="Could not save the owner">{errorMessage(update.error)}</Notice>}
        </div>
      </Section>
    </form>
  );
}

export function ResourceOwner({ id, editable }: { id: string; editable: boolean }) {
  const owner = useQuery(workloadOwnerQuery(id));
  if (owner.isPending || owner.isError) {
    return (
      <Section width="form" title={title} description={description} flush>
        {owner.isPending ? <LoadingRows label="Loading owner" rows={1} /> : <div className="p-4"><QueryError what="the resource owner" error={owner.error} onRetry={() => void owner.refetch()} retrying={owner.isFetching} /></div>}
      </Section>
    );
  }
  if (editable) return <OwnerForm id={id} owner={owner.data} />;
  return (
    <Section width="form" title={title} description={description}>
      <SettingRows>
        <SettingRow label="Resource owner">
          <span className={owner.data.user_id ? "text-fg" : "text-fg-muted"}>{owner.data.user_id ? owner.data.name || "Unnamed member" : "No owner"}</span>
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
