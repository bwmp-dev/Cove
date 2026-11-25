import { useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import type { ManagedRegistryRetention, useSaveManagedRegistry } from "../api/managed-registries";
import { retentionDraft, retentionInput, retentionLimits, validateRetention, type RetentionDraft, type RetentionErrors } from "../lib/managed-registries";
import { describedBy, inputClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Field, Notice, Subsection } from "./ui";

interface FieldsProps {
  id: string;
  draft: RetentionDraft;
  errors: RetentionErrors;
  onChange: (patch: Partial<RetentionDraft>) => void;
}

export function RetentionFields({ id, draft, errors, onChange }: FieldsProps) {
  const keepId = `${id}-keep`;
  const ageId = `${id}-age`;
  return (
    <fieldset className="min-w-0 space-y-2">
      <legend className="text-[13px] font-medium text-fg">Retention for Cove build images</legend>
      <p className="text-xs text-fg-muted">Garbage collection deletes a Cove build manifest only when it is older than the minimum age and is not among the newest manifests of its repository. Images referenced by retained releases, previews and latest builds, and tags that Cove builds did not create, are always kept.</p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={keepId} label="Keep per repository" error={errors.keep} hint={`Newest manifests kept, ${retentionLimits.keep.min} to ${retentionLimits.keep.max}.`}>
          <input id={keepId} type="number" min={retentionLimits.keep.min} max={retentionLimits.keep.max} step={1} inputMode="numeric" className={inputClass} value={draft.keep} aria-invalid={errors.keep ? true : undefined} aria-describedby={describedBy(keepId, errors.keep, true)} onChange={(event) => onChange({ keep: event.target.value })} />
        </Field>
        <Field id={ageId} label="Minimum age in days" error={errors.age} hint={`Younger manifests are kept, ${retentionLimits.age.min} to ${retentionLimits.age.max} days.`}>
          <input id={ageId} type="number" min={retentionLimits.age.min} max={retentionLimits.age.max} step={1} inputMode="numeric" className={inputClass} value={draft.age} aria-invalid={errors.age ? true : undefined} aria-describedby={describedBy(ageId, errors.age, true)} onChange={(event) => onChange({ age: event.target.value })} />
        </Field>
      </div>
    </fieldset>
  );
}

interface FormProps {
  registryId: string;
  retention: ManagedRegistryRetention;
  save: ReturnType<typeof useSaveManagedRegistry>;
  blocked?: string;
  onOutcome: (outcome: string | null) => void;
}

// Keyed by the saved retention in the caller, so a save that lands replaces the draft with the saved values.
export function RetentionForm({ registryId, retention, save, blocked, onOutcome }: FormProps) {
  const formRef = useRef<HTMLFormElement>(null);
  const saved = retentionDraft(retention);
  const [draft, setDraft] = useState<RetentionDraft>(saved);
  const [showErrors, setShowErrors] = useState(false);
  const errors = showErrors ? validateRetention(draft) : {};
  const dirty = draft.keep !== saved.keep || draft.age !== saved.age;

  function edit(patch: Partial<RetentionDraft>) {
    setDraft((current) => ({ ...current, ...patch }));
    save.reset();
    onOutcome(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (Object.keys(validateRetention(draft)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    save.mutate({ retention: retentionInput(draft) }, { onSuccess: () => onOutcome("Retention saved") });
  }

  return (
    <form ref={formRef} onSubmit={submit} aria-label="Retention settings" noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Subsection>
        <fieldset disabled={save.isPending} className="min-w-0">
          <RetentionFields id={`retention-${registryId}`} draft={draft} errors={errors} onChange={edit} />
        </fieldset>
        {save.isError && (
          <Notice tone="error" title="Retention was not saved">
            <p className="break-words">{errorMessage(save.error)}</p>
            <p className="mt-1">Your changes are kept.</p>
          </Notice>
        )}
        <div className="border-t border-line pt-4">
          <SaveBar dirty={dirty} pending={save.isPending} blocked={Boolean(blocked)} label="Save retention" onDiscard={() => edit(saved)} status={blocked ? `${blocked} Saving is paused.` : "No unsaved changes"} />
        </div>
      </Subsection>
    </form>
  );
}
