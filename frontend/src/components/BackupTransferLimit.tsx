import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { transferLimitsQuery, useUpdateTransferLimits, type TransferLimits } from "../api/backup-transfers";
import { bytesToMiBText, parseTransferLimit } from "../lib/backups";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRows } from "./ui";

const title = "Backup transfers";
const description = "Size limit for downloading a backup, uploading a database dump, and importing a dump from an S3 storage location. Applies to every team.";

function LimitForm({ saved }: { saved: TransferLimits }) {
  const update = useUpdateTransferLimits();
  const online = useOnline();
  const [draft, setDraft] = useState(() => bytesToMiBText(saved.backup_bytes));
  const [error, setError] = useState<string>();
  const dirty = draft.trim() !== bytesToMiBText(saved.backup_bytes);
  const id = "transfer-limit";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseTransferLimit(draft, saved.backup_bytes, saved.min_backup_bytes, saved.max_backup_bytes);
    if ("error" in parsed) {
      setError(parsed.error);
      document.getElementById(id)?.focus();
      return;
    }
    setError(undefined);
    if (parsed.bytes === saved.backup_bytes) return;
    update.mutate(parsed.bytes, { onSuccess: (limits) => setDraft(bytesToMiBText(limits.backup_bytes)) });
  }

  const hint = `Currently ${formatBinaryBytes(saved.backup_bytes)}. Allowed ${formatBinaryBytes(saved.min_backup_bytes)} to ${formatBinaryBytes(saved.max_backup_bytes)}.`;
  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty && !update.isPending} />
      <Section
        title={title}
        description={description}
        width="form"
        footer={
          <SaveBar
            dirty={dirty}
            pending={update.isPending}
            blocked={!online}
            onDiscard={() => {
              setDraft(bytesToMiBText(saved.backup_bytes));
              setError(undefined);
              update.reset();
            }}
            status={update.isSuccess ? "Saved. New transfers use this limit." : undefined}
          />
        }
      >
        <div className="space-y-4">
          <SettingRows>
            <Field id={id} label="Largest backup (MiB)" hint={hint} error={error} layout="row">
              <input
                id={id}
                name="backup-mib"
                type="number"
                inputMode="numeric"
                min={Math.ceil(saved.min_backup_bytes / 1048576)}
                max={Math.floor(saved.max_backup_bytes / 1048576)}
                step={1}
                className={inputClass}
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? `${id}-hint ${id}-error` : `${id}-hint`}
              />
            </Field>
          </SettingRows>
          <ul className="list-disc space-y-1 pl-5 text-xs text-fg-muted">
            <li>At most four transfers run at once across this instance, and each must finish within 30 minutes.</li>
            <li>The worker stages each transfer on its own disk and keeps 256 MiB free beyond the temporary data. Raising the limit does not add storage, so check the worker's free space first.</li>
          </ul>
          {!online && <Notice tone="warn">You are offline. Reconnect to save.</Notice>}
          {update.isError && <Notice tone="error" title="Could not save the transfer limit">{errorMessage(update.error)}</Notice>}
        </div>
      </Section>
    </form>
  );
}

export function BackupTransferLimit() {
  const limits = useQuery(transferLimitsQuery);
  if (limits.isPending) return <Section title={title} description={description} width="form"><LoadingRows label="Loading transfer limit" rows={1} /></Section>;
  if (limits.isError) return <Section title={title} description={description} width="form"><QueryError what="transfer limit" error={limits.error} onRetry={() => void limits.refetch()} retrying={limits.isFetching} /></Section>;
  return <LimitForm saved={limits.data} />;
}
