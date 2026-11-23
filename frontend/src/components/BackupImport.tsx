import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useRef, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { importBackup, resourceTransferLimitsQuery, type ImportedBackup, type ImportSource } from "../api/backup-transfers";
import { connectionsQuery } from "../api/connections";
import { backupScopeLabel, importFormats, objectKeyError, type DatabaseBackup } from "../lib/backups";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { ChoiceGroup } from "./ChoiceGroup";
import { Dialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { Button, Field, Notice, QueryError, Spinner } from "./ui";

type SourceKind = ImportSource["kind"];

const sourceChoices = [
  { value: "upload" as const, label: "Upload a file", description: "From this computer." },
  { value: "s3" as const, label: "S3 storage location", description: "An object in a configured location." },
];

function fileProblem(file: File, limit: number | undefined) {
  if (file.size === 0) return "This file is empty.";
  if (limit !== undefined && file.size > limit) return `This file is ${formatBinaryBytes(file.size)}. The limit is ${formatBinaryBytes(limit)}.`;
  return undefined;
}

function focus(id: string) {
  document.getElementById(id)?.focus();
}

function ImportScope({ database }: { database: DatabaseBackup }) {
  const format = importFormats[database.engine];
  return <>
    <p>
      Imports into <span className="text-fg">{backupScopeLabel(database)}</span> (<span className="font-mono">{database.service}</span>), the scope of this resource's backup policy.
    </p>
    <Notice title="Importing does not change this resource">
      Cove restores the dump into an isolated container to verify it, then stores it as an encrypted backup in this resource's repository. The database keeps running and its data is not touched. To use the data afterwards, restore the backup: that saves a safety backup first and asks for confirmation.
    </Notice>
    {format && (
      <div>
        <p className="text-[13px] font-medium text-fg">Accepted file: {format.file}</p>
        <p className="mt-0.5 text-xs">{format.description} ZIP, tar and volume archives are not accepted.</p>
      </div>
    )}
    {format?.warning && <Notice tone="warn" title="Use a trusted dump">{format.warning}</Notice>}
  </>;
}

function ImportResult({ result, restorable, onRestore, onClose }: { result: ImportedBackup; restorable: boolean | undefined; onRestore: () => void; onClose: () => void }) {
  return <>
    <Notice tone="success" title="Verified backup stored">
      <p>{formatBinaryBytes(result.size)} imported and verified as backup <span className="font-mono break-all">{result.backup_id}</span>.</p>
      <p className="mt-1">
        SHA-256 <span className="font-mono break-all">{result.sha256}</span>
      </p>
    </Notice>
    {restorable === undefined && (
      <p role="status" className="flex items-center gap-2 text-xs">
        <Spinner className="size-3" /> Refreshing the backup list…
      </p>
    )}
    <div className="flex flex-wrap justify-end gap-2">
      <Button variant="ghost" onClick={onClose}>Close</Button>
      {restorable && <Button variant="primary" onClick={onRestore}>Restore this backup…</Button>}
    </div>
  </>;
}

function UploadField({ limit, file, error, disabled, onChoose }: { limit: number | undefined; file: File | null; error: string | undefined; disabled: boolean; onChoose: (file: File | null) => void }) {
  const hint = limit === undefined ? "Loading the size limit…" : `Up to ${formatBinaryBytes(limit)}. The file is sent directly to the worker; keep this page open until verification finishes.`;
  return (
    <Field id="import-file" label="Dump file" error={error} hint={file && !error ? `${file.name}, ${formatBinaryBytes(file.size)}. ${hint}` : hint}>
      <input
        id="import-file"
        type="file"
        className={`${inputClass} file:mr-3 file:rounded-md file:border-0 file:bg-raised file:px-2 file:py-1 file:text-xs file:text-fg`}
        disabled={disabled || limit === undefined}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy("import-file", error, true)}
        onChange={(event) => onChoose(event.target.files?.[0] ?? null)}
      />
    </Field>
  );
}

interface S3FieldsProps { limit: number | undefined; storage: string; objectKey: string; error: string | undefined; disabled: boolean; onStorage: (id: string) => void; onObjectKey: (key: string) => void }

function S3Fields({ limit, storage, objectKey, error, disabled, onStorage, onObjectKey }: S3FieldsProps) {
  const locations = useQuery(connectionsQuery("storage-locations"));
  if (locations.isPending) return <p className="text-xs">Loading storage locations…</p>;
  if (locations.isError) return <QueryError what="storage locations" error={locations.error} onRetry={() => void locations.refetch()} retrying={locations.isFetching} />;
  const s3Locations = locations.data.filter((item) => item.storage?.protocol === "s3");
  if (s3Locations.length === 0) {
    return <Notice tone="warn" title="No S3 storage locations">
      Add an S3 location in <Link to="/storage" className={linkClass}>Storage locations</Link> to import from it.
    </Notice>;
  }
  return <>
    <Field id="import-storage" label="Storage location">
      <Select id="import-storage" className={inputClass} value={storage} disabled={disabled} onChange={(event) => onStorage(event.target.value)} required>
        <option value="">Choose a location</option>
        {s3Locations.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
      </Select>
    </Field>
    <Field id="import-key" label="Object key" error={error} hint={`Relative to the location's bucket and path, for example dumps/app.dump.${limit === undefined ? "" : ` Up to ${formatBinaryBytes(limit)}.`}`}>
      <input
        id="import-key"
        className={`${inputClass} font-mono`}
        value={objectKey}
        maxLength={1024}
        autoComplete="off"
        spellCheck={false}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy("import-key", error, true)}
        onChange={(event) => onObjectKey(event.target.value)}
      />
    </Field>
  </>;
}

function ImportProgress({ kind }: { kind: SourceKind }) {
  return (
    <Notice title={kind === "upload" ? "Uploading and verifying" : "Reading and verifying"}>
      <span className="flex items-start gap-2">
        <Spinner className="mt-0.5 size-3.5 shrink-0" />
        <span>{kind === "upload" ? "The file is streaming to the worker" : "The worker is reading the object"}, then the dump is restored into an isolated container to check it. This can take several minutes; keep this page open.</span>
      </span>
    </Notice>
  );
}

interface ImportFormProps {
  limit: number | undefined;
  limitError: ReactNode;
  pending: boolean;
  failure: ReactNode;
  onImport: (source: ImportSource) => void;
  onEdit: () => void;
  onCancel: () => void;
}

function ImportForm({ limit, limitError, pending, failure, onImport, onEdit, onCancel }: ImportFormProps) {
  const online = useOnline();
  const [kind, setKind] = useState<SourceKind>("upload");
  const [file, setFile] = useState<File | null>(null);
  const [fileError, setFileError] = useState<string>();
  const [storage, setStorage] = useState("");
  const [objectKey, setObjectKey] = useState("");
  const [keyError, setKeyError] = useState<string>();

  function chooseFile(next: File | null) {
    setFile(next);
    onEdit();
    setFileError(next ? fileProblem(next, limit) : undefined);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (kind === "upload") {
      const error = file ? fileError : "Choose a dump file.";
      setFileError(error);
      if (error || !file) return focus("import-file");
      return onImport({ kind: "upload", file });
    }
    const error = objectKeyError(objectKey);
    setKeyError(error);
    if (error) return focus("import-key");
    onImport({ kind: "s3", storage_id: storage, object_key: objectKey });
  }

  const sourceReady = kind === "upload" ? Boolean(file) && !fileError && limit !== undefined : Boolean(storage);
  return (
    <form className="space-y-4" onSubmit={submit} noValidate>
      <ChoiceGroup legend="Source" name="import-source" value={kind} choices={sourceChoices} onChange={(value) => { setKind(value); onEdit(); }} />
      {limitError}
      {kind === "upload"
        ? <UploadField limit={limit} file={file} error={fileError} disabled={pending} onChoose={chooseFile} />
        : <S3Fields limit={limit} storage={storage} objectKey={objectKey} error={keyError} disabled={pending} onStorage={setStorage} onObjectKey={(key) => { setObjectKey(key); setKeyError(undefined); }} />}
      {pending && <ImportProgress kind={kind} />}
      {failure}
      {!online && <Notice tone="warn">You are offline. Reconnect to import.</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onCancel}>{pending ? "Cancel import" : "Close"}</Button>
        <Button type="submit" variant="primary" pending={pending} disabled={!online || !sourceReady}>Import and verify</Button>
      </div>
    </form>
  );
}

function ImportFailure({ error, aborted }: { error: unknown; aborted: boolean }) {
  if (aborted) {
    return <Notice tone="warn" title="Import cancelled in this browser">
      If the file had already been received, the worker may still finish and store the verified backup. Check the backup list before trying again.
    </Notice>;
  }
  return error ? <Notice tone="error" title="Import failed">{errorMessage(error)}</Notice> : null;
}

interface BackupImportProps {
  workloadId: string;
  database: DatabaseBackup;
  // The imported backup's row once the refreshed backup list includes it; restoring stays a separate, confirmed action.
  importedRow: (backupId: string) => { id: string } | undefined;
  canRestore: boolean;
  onRestore: (backupId: string) => void;
  // interrupted is true when the dialog was dismissed while an import was still running.
  onClose: (interrupted: boolean) => void;
}

export function BackupImport({ workloadId, database, importedRow, canRestore, onRestore, onClose }: BackupImportProps) {
  const client = useQueryClient();
  const limits = useQuery({ ...resourceTransferLimitsQuery(workloadId), gcTime: 0 });
  const [cancelled, setCancelled] = useState(false);
  const abort = useRef<AbortController | null>(null);

  const importDump = useMutation({
    gcTime: 0,
    mutationFn: (source: ImportSource) => {
      const controller = new AbortController();
      abort.current = controller;
      return importBackup(workloadId, source, controller.signal);
    },
    onSettled: () => {
      abort.current = null;
      void client.invalidateQueries({ queryKey: ["backups", workloadId] });
    },
  });

  function cancel() {
    if (!abort.current) return onClose(false);
    abort.current.abort();
    setCancelled(true);
  }

  function dismiss() {
    const interrupted = abort.current !== null;
    abort.current?.abort();
    onClose(interrupted);
  }

  const result = importDump.data;
  const restorable = result && importedRow(result.backup_id) ? canRestore : undefined;
  return (
    <Dialog open title="Import database dump" onClose={dismiss}>
      <div className="space-y-4">
        <ImportScope database={database} />
        {result ? (
          <ImportResult result={result} restorable={restorable} onRestore={() => onRestore(result.backup_id)} onClose={() => onClose(false)} />
        ) : (
          <ImportForm
            limit={limits.data?.backup_bytes}
            limitError={limits.isError && <QueryError what="transfer limit" error={limits.error} onRetry={() => void limits.refetch()} retrying={limits.isFetching} />}
            pending={importDump.isPending}
            failure={<ImportFailure error={importDump.error} aborted={cancelled && importDump.error instanceof DOMException} />}
            onImport={(source) => { setCancelled(false); importDump.mutate(source); }}
            onEdit={() => { importDump.reset(); setCancelled(false); }}
            onCancel={cancel}
          />
        )}
      </div>
    </Dialog>
  );
}
