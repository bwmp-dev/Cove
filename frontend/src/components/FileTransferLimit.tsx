import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { fileTransferLimitsQuery, useUpdateFileTransferLimits, type FileLimits } from "../api/files";
import { bytesToMiBText, parseTransferLimit } from "../lib/backups";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRows } from "./ui";

const title = "File transfers";
const description = "Size limit for downloading, uploading and replacing files on a resource's Files page. Applies to every team. The text editor stays limited to 32 KiB.";

function LimitForm({ saved }: { saved: FileLimits }) {
  const update = useUpdateFileTransferLimits();
  const online = useOnline();
  const [draft, setDraft] = useState(() => bytesToMiBText(saved.file_bytes));
  const [error, setError] = useState<string>();
  const dirty = draft.trim() !== bytesToMiBText(saved.file_bytes);
  const id = "file-transfer-limit";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseTransferLimit(draft, saved.file_bytes, saved.min_file_bytes, saved.max_file_bytes);
    if ("error" in parsed) {
      setError(parsed.error);
      document.getElementById(id)?.focus();
      return;
    }
    setError(undefined);
    if (parsed.bytes === saved.file_bytes) return;
    update.mutate(parsed.bytes, { onSuccess: (limits) => setDraft(bytesToMiBText(limits.file_bytes)) });
  }

  const hint = `Currently ${formatBinaryBytes(saved.file_bytes)}. Allowed ${formatBinaryBytes(saved.min_file_bytes)} to ${formatBinaryBytes(saved.max_file_bytes)}.`;
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
              setDraft(bytesToMiBText(saved.file_bytes));
              setError(undefined);
              update.reset();
            }}
            status={update.isSuccess ? "Saved. New file transfers use this limit." : undefined}
          />
        }
      >
        <div className="space-y-4">
          <SettingRows>
            <Field id={id} label="Largest file (MiB)" hint={hint} error={error} layout="row">
              <input
                id={id}
                name="file-mib"
                type="number"
                inputMode="numeric"
                min={Math.ceil(saved.min_file_bytes / 1048576)}
                max={Math.floor(saved.max_file_bytes / 1048576)}
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
            <li>At most four file transfers run at once across this instance, and each must finish within 30 minutes.</li>
            <li>Uploads are staged beside the file they create or replace. Raising the limit does not add storage, so check the free space in containers and volumes first.</li>
          </ul>
          {!online && <Notice tone="warn">You are offline. Reconnect to save.</Notice>}
          {update.isError && <Notice tone="error" title="Could not save the file transfer limit">{errorMessage(update.error)}</Notice>}
        </div>
      </Section>
    </form>
  );
}

export function FileTransferLimit() {
  const limits = useQuery(fileTransferLimitsQuery);
  if (limits.isPending) return <Section title={title} description={description} width="form"><LoadingRows label="Loading file transfer limit" rows={1} /></Section>;
  if (limits.isError) return <Section title={title} description={description} width="form"><QueryError what="file transfer limit" error={limits.error} onRetry={() => void limits.refetch()} retrying={limits.isFetching} /></Section>;
  return <LimitForm saved={limits.data} />;
}
