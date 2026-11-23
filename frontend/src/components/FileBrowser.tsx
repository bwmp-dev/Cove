import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUp, Download, File, FilePlus, FileQuestion, Folder, Link2, RotateCw, Trash2, Upload, X } from "lucide-react";
import { useEffect, useId, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import {
  fileDownloadUrl,
  fileKeys,
  fileLimitsQuery,
  fileListQuery,
  fileTargetsQuery,
  useDeleteFile,
  useFileMetadata,
  useReadFile,
  useUploadFile,
  useWriteFile,
  type FileEntry,
  type FileLocation,
  type FileTarget,
} from "../api/files";
import {
  baseName,
  containerTools,
  decodeBase64,
  decodeText,
  emptyRevision,
  encodeBase64,
  fileNameError,
  isBlockedPath,
  joinPath,
  maxEntries,
  maxFileBytes,
  parentPath,
  pathSegments,
  revisionOf,
  sortEntries,
  targetAvailable,
  targetKey,
  utf8Length,
} from "../lib/files";
import { buttonClass, describedBy, inputClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, EmptyState, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

interface OpenFile {
  location: FileLocation;
  size: number;
  revision: string;
  // Null when only the file's size and revision were read, because it is larger than the text editor.
  bytes: Uint8Array<ArrayBuffer> | null;
  // Null when the bytes cannot be edited as text without changing them; such files can be downloaded or replaced.
  text: string | null;
  carriageReturns: boolean;
}

interface Upload {
  file: File;
  path: string;
  replacing: boolean;
}

const encoder = new TextEncoder();
const editorLimit = formatBinaryBytes(maxFileBytes);

// Section titles do not wrap long unbroken names, so the full path is shown in the description instead.
function shortName(name: string) {
  return name.length > 48 ? `${name.slice(0, 22)}…${name.slice(-22)}` : name;
}

function targetLabel(target: FileTarget) {
  if (target.kind === "volume") return target.name;
  return targetAvailable(target) ? target.name : `${target.name} (${target.state || "unknown"}, not running)`;
}

function isAbort(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}

// A cancelled or failed upload is never retried: the worker may already have applied it.
function uploadErrorMessage(error: unknown) {
  if (isAbort(error)) return "The upload was cancelled. The change may still have completed; refresh before trying again.";
  return `${errorMessage(error)} The file may or may not have changed. Nothing was retried; refresh before trying again.`;
}

function filesDescription(transfers: Transfers) {
  const intro = `Browse one folder at a time, up to ${maxEntries} entries. Text files up to ${editorLimit} can be edited here`;
  const audit = "Every read and change is audited without its contents.";
  if (!transfers.supported) return `${intro}. ${audit}`;
  const limit = transfers.limit === undefined ? "the transfer limit" : formatBinaryBytes(transfers.limit);
  return `${intro}; files up to ${limit} can be downloaded, uploaded and replaced. ${audit}`;
}

function actionError(failures: [unknown, string][]) {
  const failure = failures.find(([error]) => error);
  if (!failure) return null;
  const [error, action] = failure;
  return `${action}: ${action === "Could not upload" ? uploadErrorMessage(error) : errorMessage(error)}`;
}

function TargetPicker({ targets, limited, value, disabled, onChange }: { targets: FileTarget[]; limited: boolean; value: string; disabled: boolean; onChange: (key: string) => void }) {
  const id = useId();
  const containers = targets.filter((item) => item.kind === "container");
  const volumes = targets.filter((item) => item.kind === "volume");
  return (
    <Field id={id} label="Container or volume" hint={limited ? "Some volumes are not listed: at most 128 owned volumes and 32 containers are shown." : "Stopped containers cannot be browsed. Volumes can be browsed while the resource runs."}>
      <Select id={id} className={`${inputClass} sm:max-w-md`} value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)} aria-describedby={`${id}-hint`}>
        <option value="" disabled>Choose a container or volume</option>
        {containers.length > 0 && <optgroup label="Containers">{containers.map((item) => <option key={targetKey(item)} value={targetKey(item)} disabled={!targetAvailable(item)}>{targetLabel(item)}</option>)}</optgroup>}
        {volumes.length > 0 && <optgroup label="Volumes">{volumes.map((item) => <option key={targetKey(item)} value={targetKey(item)}>{targetLabel(item)}</option>)}</optgroup>}
      </Select>
    </Field>
  );
}

function TargetNotice({ target, containersRunning }: { target: FileTarget; containersRunning: number }) {
  if (target.kind === "container") {
    return (
      <Notice tone="info" title="Changes apply to the running container immediately">
        The application can change these files at the same time, and changes outside a volume are lost when the container is recreated. The container must run Linux with /proc/self/fd and provide {containerTools}; minimal images without
        them cannot be browsed.
      </Notice>
    );
  }
  return (
    <Notice tone={containersRunning > 0 ? "warn" : "info"} title="Volume changes need the resource stopped">
      Browsing and reading use an isolated read-only helper. Saving, uploading and deleting fail while any container using this volume is running; Cove does not stop it for you.
      {containersRunning > 0 && ` ${containersRunning === 1 ? "1 container of this resource is" : `${containersRunning} containers of this resource are`} running now.`}
    </Notice>
  );
}

function FolderPath({ path, disabled, onNavigate }: { path: string; disabled: boolean; onNavigate: (path: string) => void }) {
  const segments = pathSegments(path);
  const crumbClass = "rounded px-1 font-mono text-xs text-fg-muted hover:bg-raised hover:text-fg disabled:cursor-not-allowed disabled:text-fg-disabled disabled:hover:bg-transparent pointer-coarse:min-h-10";
  return (
    <nav aria-label="Folder path" className="min-w-0 flex-1">
      <ol className="flex min-w-0 flex-wrap items-center gap-0.5">
        <li>
          {segments.length === 0 ? (
            <span aria-current="location" className="px-1 font-mono text-xs text-fg">/</span>
          ) : (
            <button type="button" className={crumbClass} disabled={disabled} onClick={() => onNavigate("/")}>/</button>
          )}
        </li>
        {segments.map((segment, index) => (
          <li key={segment.path} className="flex min-w-0 items-center gap-0.5">
            {index > 0 && <span aria-hidden className="font-mono text-xs text-fg-subtle">/</span>}
            {index === segments.length - 1 ? (
              <span aria-current="location" className="truncate px-1 font-mono text-xs text-fg" title={segment.name}>{segment.name}</span>
            ) : (
              <button type="button" className={`${crumbClass} max-w-48 truncate`} title={segment.name} disabled={disabled} onClick={() => onNavigate(segment.path)}>{segment.name}</button>
            )}
          </li>
        ))}
      </ol>
    </nav>
  );
}

// Downloads are plain links so the browser streams the file to disk itself; Cove never buffers it in the page.
function DownloadLink({ href, name, disabled, label }: { href: string; name: string; disabled: boolean; label?: string }) {
  const accessibleName = `Download ${name}`;
  if (disabled) {
    return (
      <Button variant={label ? "secondary" : "ghost"} size="sm" aria-label={label ? undefined : accessibleName} title={accessibleName} disabled>
        <Download aria-hidden className="size-3.5" />
        {label}
      </Button>
    );
  }
  return (
    <a href={href} target="_blank" rel="noopener" className={buttonClass(label ? "secondary" : "ghost", "gap-1.5", "sm")} aria-label={label ? undefined : accessibleName} title={accessibleName}>
      <Download aria-hidden className="size-3.5" />
      {label}
    </a>
  );
}

const entryIcons = { directory: Folder, file: File, link: Link2, other: FileQuestion } as const;

interface Transfers {
  // False on workers without streamed file support: only small text files can be read and edited there.
  supported: boolean;
  // The configured cap for downloads and uploads, once it has been read.
  limit: number | undefined;
}

function fileAccess(size: number, transfers: Transfers) {
  const editable = size <= maxFileBytes;
  const transferable = transfers.supported && transfers.limit !== undefined && size <= transfers.limit;
  return { editable, transferable, openable: editable || transferable };
}

function entryDetail(entry: FileEntry, blocked: boolean, transfers: Transfers) {
  if (entry.kind === "directory") return blocked ? "Folder · not browsable" : "Folder";
  if (entry.kind === "link") return "Symbolic link · not followed";
  if (entry.kind === "other") return "Special file · not readable";
  const size = formatBinaryBytes(entry.size);
  if (entry.size <= maxFileBytes) return size;
  if (!transfers.supported) return `${size} · larger than the ${editorLimit} editor; upgrade the worker to download`;
  if (transfers.limit !== undefined && entry.size > transfers.limit) return `${size} · over the ${formatBinaryBytes(transfers.limit)} transfer limit`;
  return `${size} · download or replace`;
}

interface EntryRowProps {
  entry: FileEntry;
  path: string;
  downloadUrl: string;
  blocked: boolean;
  transfers: Transfers;
  current: boolean;
  disabled: boolean;
  offline: boolean;
  reading: boolean;
  onOpen: () => void;
  onDelete: () => void;
}

function EntryRow({ entry, path, downloadUrl, blocked, transfers, current, disabled, offline, reading, onOpen, onDelete }: EntryRowProps) {
  const Icon = entryIcons[entry.kind];
  const access = fileAccess(entry.size, transfers);
  const openable = (entry.kind === "directory" && !blocked) || (entry.kind === "file" && access.openable);
  const nameContent = (
    <>
      <Icon aria-hidden className={`size-4 shrink-0 ${entry.kind === "directory" ? "text-fg-muted" : "text-fg-subtle"}`} />
      <span className="min-w-0 truncate font-mono text-[13px]">{entry.name}</span>
    </>
  );
  return (
    <li className={`flex min-h-11 items-center gap-2 px-3 py-1.5 hover:bg-raised ${current ? "bg-raised" : ""}`}>
      <div className="min-w-0 flex-1">
        {openable ? (
          <button
            type="button"
            className="flex max-w-full items-center gap-2 rounded text-left text-fg hover:underline hover:decoration-fg-subtle hover:underline-offset-4 disabled:cursor-not-allowed disabled:text-fg-disabled disabled:no-underline pointer-coarse:min-h-10"
            title={path}
            aria-current={current ? "true" : undefined}
            disabled={disabled}
            onClick={onOpen}
          >
            {nameContent}
          </button>
        ) : (
          <span className="flex max-w-full items-center gap-2 text-fg-muted" title={path}>{nameContent}</span>
        )}
        <p className="mt-0.5 pl-6 text-xs text-fg-muted">
          {entryDetail(entry, blocked, transfers)}
          {reading && " · opening…"}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        {entry.kind === "file" && access.transferable && <DownloadLink href={downloadUrl} name={entry.name} disabled={offline} />}
        {(entry.kind === "file" || (entry.kind === "directory" && !blocked)) && (
          <Button variant="ghost" size="sm" aria-label={`Delete ${entry.name}`} title={`Delete ${entry.name}`} disabled={disabled} onClick={onDelete}>
            <Trash2 aria-hidden className="size-3.5" />
          </Button>
        )}
      </div>
    </li>
  );
}

interface EntryListProps {
  workloadId: string;
  location: FileLocation;
  entries: FileEntry[];
  transfers: Transfers;
  openPath: string | undefined;
  readingPath: string | undefined;
  disabled: boolean;
  offline: boolean;
  onOpen: (entry: FileEntry, location: FileLocation) => void;
  onDelete: (entry: FileEntry) => void;
}

function EntryList({ workloadId, location, entries, transfers, openPath, readingPath, disabled, offline, onOpen, onDelete }: EntryListProps) {
  return (
    <ul aria-label={`Contents of ${location.path}`} className="divide-y divide-line">
      {entries.map((entry) => {
        const entryLocation = { ...location, path: joinPath(location.path, entry.name) };
        return (
          <EntryRow
            key={entry.name}
            entry={entry}
            path={entryLocation.path}
            downloadUrl={fileDownloadUrl(workloadId, entryLocation)}
            blocked={isBlockedPath(location.kind, entryLocation.path)}
            transfers={transfers}
            current={openPath === entryLocation.path}
            disabled={disabled}
            offline={offline}
            reading={readingPath === entryLocation.path}
            onOpen={() => onOpen(entry, entryLocation)}
            onDelete={() => onDelete(entry)}
          />
        );
      })}
    </ul>
  );
}

function UploadStatus({ upload, onCancel }: { upload: Upload; onCancel: () => void }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <p role="status" className="min-w-0 text-xs break-all text-fg-muted">
        {upload.replacing ? "Replacing" : "Uploading"} <span className="font-mono">{upload.path}</span> with {upload.file.name} ({formatBinaryBytes(upload.file.size)})…
      </p>
      <Button variant="ghost" size="sm" onClick={onCancel}>Cancel upload</Button>
    </div>
  );
}

// A local error was found before any request; a server error may have left a change applied or not.
function FolderFeedback({ upload, error, localOnly, status, onCancelUpload }: { upload: Upload | null; error: string | null; localOnly: boolean; status: string | null; onCancelUpload: () => void }) {
  return (
    <>
      {upload && (
        <div className="px-3 py-2">
          <UploadStatus upload={upload} onCancel={onCancelUpload} />
        </div>
      )}
      {(error || status) && (
        <div className="px-3 py-2">
          {error ? <Notice tone="error" title={localOnly ? "Nothing was sent" : "Could not confirm the action"}>{error}</Notice> : <p role="status" className="text-xs text-fg-muted">{status}</p>}
        </div>
      )}
    </>
  );
}

function CreateFileDialog({ open, directory, kind, existing, pending, error, onCreate, onClose }: { open: boolean; directory: string; kind: FileTarget["kind"]; existing: Set<string>; pending: boolean; error: string | undefined; onCreate: (name: string) => void; onClose: () => void }) {
  const id = useId();
  const [name, setName] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const problem = existing.has(name) ? "An entry with this name already exists in this folder." : fileNameError(name, directory, kind);
  const shownError = submitted ? (problem ?? error) : error;
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitted(true);
    if (!problem) onCreate(name);
  }
  return (
    <Dialog open={open} title="New file" onClose={onClose}>
      <form onSubmit={submit} className="space-y-4 text-fg">
        <Field id={id} label="File name" hint={<>Created empty in <span className="font-mono break-all">{directory}</span>. Cove never overwrites an existing file here.</>} error={shownError}>
          <input id={id} className={inputClass} value={name} autoComplete="off" spellCheck={false} onChange={(event) => setName(event.target.value)} aria-invalid={Boolean(shownError) || undefined} aria-describedby={describedBy(id, shownError, true)} />
        </Field>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={pending}>Cancel</Button>
          <Button type="submit" variant="primary" pending={pending}>Create file</Button>
        </div>
      </form>
    </Dialog>
  );
}

function NotEditableNotice({ file, transfers }: { file: OpenFile; transfers: Transfers }) {
  const size = formatBinaryBytes(file.size);
  if (file.bytes === null) {
    return (
      <Notice title={`Larger than the ${editorLimit} editor`}>
        Cove read this file's size and revision without its contents.{" "}
        {fileAccess(file.size, transfers).transferable ? `Download it, or replace it with a local file of up to ${formatBinaryBytes(transfers.limit ?? 0)}.` : "Download and replace need an upgraded worker."}
      </Notice>
    );
  }
  if (file.carriageReturns) {
    return (
      <Notice title="Text with carriage returns">
        Browsers convert CR line endings while editing, which would rewrite every line of this {size} file. {transfers.supported ? "Download it, edit it locally and replace it instead." : "Upgrade the worker to download and replace it instead."}
      </Notice>
    );
  }
  return (
    <Notice title="Binary file">
      This {size} file is not UTF-8 text, so it cannot be edited here. {transfers.supported ? "Download it, or replace it with a local file." : "Upgrade the worker to download or replace it."}
    </Notice>
  );
}

function failureMessage(saveError: unknown, replaceError: unknown, dirty: boolean) {
  if (saveError) return `${errorMessage(saveError)} Your edits are kept here. If the file changed since you opened it, read it again and reapply them.`;
  if (replaceError) return `${uploadErrorMessage(replaceError)}${dirty ? " Your edits are kept here." : ""}`;
  return null;
}

interface EditorActionsProps {
  file: OpenFile;
  downloadUrl: string;
  transfers: Transfers;
  disabled: boolean;
  offline: boolean;
  replacing: boolean;
  onReplace: () => void;
  onClose: () => void;
}

function EditorActions({ file, downloadUrl, transfers, disabled, offline, replacing, onReplace, onClose }: EditorActionsProps) {
  const name = baseName(file.location.path);
  return (
    <>
      {fileAccess(file.size, transfers).transferable && <DownloadLink href={downloadUrl} name={name} disabled={offline} label="Download" />}
      {transfers.supported && (
        <Button size="sm" onClick={onReplace} disabled={disabled || transfers.limit === undefined} pending={replacing}>
          {!replacing && <Upload aria-hidden className="size-3.5" />}Replace
        </Button>
      )}
      <Button variant="ghost" size="sm" aria-label="Close file" title="Close file" disabled={replacing} onClick={onClose}><X aria-hidden className="size-3.5" /></Button>
    </>
  );
}

interface FolderToolbarProps {
  path: string;
  navigationDisabled: boolean;
  busy: boolean;
  transfers: Transfers;
  uploading: boolean;
  limitsLoading: boolean;
  onNavigate: (path: string) => void;
  onCreate: () => void;
  onUpload: (event: ChangeEvent<HTMLInputElement>) => void;
}

function FolderToolbar({ path, navigationDisabled, busy, transfers, uploading, limitsLoading, onNavigate, onCreate, onUpload }: FolderToolbarProps) {
  const input = useRef<HTMLInputElement>(null);
  const waiting = uploading || limitsLoading;
  return (
    <div className="flex flex-wrap items-center gap-2 px-3 py-2">
      <Button variant="ghost" size="sm" aria-label="Parent folder" title="Parent folder" disabled={path === "/" || navigationDisabled} onClick={() => onNavigate(parentPath(path))}>
        <ArrowUp aria-hidden className="size-3.5" />
      </Button>
      <FolderPath path={path} disabled={navigationDisabled} onNavigate={onNavigate} />
      <div className="flex items-center gap-2">
        <Button size="sm" disabled={busy} onClick={onCreate}><FilePlus aria-hidden className="size-3.5" />New file</Button>
        {transfers.supported && (
          <>
            <Button size="sm" disabled={busy || transfers.limit === undefined} pending={waiting} onClick={() => input.current?.click()}>
              {!waiting && <Upload aria-hidden className="size-3.5" />}Upload
            </Button>
            <input ref={input} type="file" className="hidden" tabIndex={-1} aria-hidden onChange={onUpload} />
          </>
        )}
      </div>
    </div>
  );
}

interface EditorProps {
  file: OpenFile;
  draft: string;
  downloadUrl: string;
  transfers: Transfers;
  disabled: boolean;
  offline: boolean;
  saving: boolean;
  saveError: unknown;
  replaceError: unknown;
  saved: boolean;
  upload: Upload | null;
  onDraft: (value: string) => void;
  onSave: () => void;
  onDiscard: () => void;
  onReread: () => void;
  onReplace: () => void;
  onCancelUpload: () => void;
  onClose: () => void;
}

function FileEditor({ file, draft, downloadUrl, transfers, disabled, offline, saving, saveError, replaceError, saved, upload, onDraft, onSave, onDiscard, onReread, onReplace, onCancelUpload, onClose }: EditorProps) {
  const id = useId();
  const editor = useRef<HTMLTextAreaElement>(null);
  const notice = useRef<HTMLDivElement>(null);
  const dirty = file.text !== null && draft !== file.text;
  const bytes = file.text === null ? file.size : utf8Length(draft);
  const tooLarge = bytes > maxFileBytes;
  const path = file.location.path;
  const name = baseName(path);
  const failure = failureMessage(saveError, replaceError, dirty);

  useEffect(() => {
    if (file.text === null) notice.current?.focus();
    else editor.current?.focus();
  }, [path, file.text]);

  return (
    <Section
      title={shortName(name)}
      description={
        <>
          <span className="font-mono break-all">{path}</span> · {formatBinaryBytes(file.size)} · revision <span className="font-mono">{file.revision.slice(0, 12)}</span>
        </>
      }
      actions={<EditorActions file={file} downloadUrl={downloadUrl} transfers={transfers} disabled={disabled} offline={offline} replacing={Boolean(upload?.replacing)} onReplace={onReplace} onClose={onClose} />}
      footer={
        file.text === null ? undefined : (
          <form className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2" onSubmit={(event) => { event.preventDefault(); onSave(); }}>
            <p role="status" className="mr-auto text-xs text-fg-muted">{dirty ? "Unsaved changes" : saved ? "Saved." : ""}</p>
            <Button variant="ghost" onClick={onDiscard} disabled={!dirty || saving}>Discard changes</Button>
            <Button type="submit" variant="primary" disabled={!dirty || tooLarge || disabled} pending={saving}>Save file</Button>
          </form>
        )
      }
    >
      <div className="space-y-3">
        {upload?.replacing && <UploadStatus upload={upload} onCancel={onCancelUpload} />}
        {failure && (
          <Notice
            tone="error"
            title="Could not confirm the file change"
            action={<Button variant="ghost" size="sm" onClick={onReread} disabled={disabled}><RotateCw aria-hidden className="size-3.5" />Read again</Button>}
          >
            {failure}
          </Notice>
        )}
        {file.text === null ? (
          <div ref={notice} tabIndex={-1} className="rounded-md">
            <NotEditableNotice file={file} transfers={transfers} />
          </div>
        ) : (
          <div className="space-y-1.5">
            <label htmlFor={id} className="sr-only">Contents of {path}</label>
            <textarea
              ref={editor}
              id={id}
              value={draft}
              spellCheck={false}
              autoCapitalize="off"
              autoComplete="off"
              wrap="off"
              rows={18}
              readOnly={saving}
              aria-invalid={tooLarge || undefined}
              aria-describedby={`${id}-size`}
              onChange={(event) => onDraft(event.target.value)}
              className={`${inputClass} h-auto min-h-64 resize-y py-2 font-mono text-xs leading-relaxed`}
            />
            <p id={`${id}-size`} className={`text-xs tabular-nums ${tooLarge ? "text-bad" : "text-fg-muted"}`}>
              {bytes.toLocaleString()} of {maxFileBytes.toLocaleString()} bytes{tooLarge && " — remove content before saving, or replace the file with a local copy"}
            </p>
          </div>
        )}
      </div>
    </Section>
  );
}

interface FileBrowserProps {
  workloadId: string;
  blocked: boolean;
  offline: boolean;
  // Whether the worker streams downloads, uploads and metadata; older workers only edit small text files.
  transfersSupported: boolean;
}

export function FileBrowser({ workloadId, blocked, offline, transfersSupported }: FileBrowserProps) {
  const client = useQueryClient();
  const replaceInput = useRef<HTMLInputElement>(null);
  const uploadAbort = useRef<AbortController | null>(null);
  const [selectedKey, setSelectedKey] = useState("");
  const [path, setPath] = useState("/");
  const [file, setFile] = useState<OpenFile | null>(null);
  const [draft, setDraft] = useState("");
  const [pendingLeave, setPendingLeave] = useState<(() => void) | null>(null);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<FileEntry | null>(null);
  const [replacing, setReplacing] = useState<File | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);
  const [status, setStatus] = useState<string | null>(null);

  const targets = useQuery({ ...fileTargetsQuery(workloadId), enabled: !blocked, refetchOnWindowFocus: false });
  const limits = useQuery({ ...fileLimitsQuery(workloadId), enabled: !blocked && transfersSupported, refetchOnWindowFocus: false });
  const target = targets.data?.targets.find((item) => targetKey(item) === selectedKey);
  const location = target && targetAvailable(target) ? { kind: target.kind, target: target.id, path } : null;
  const listing = useQuery({
    ...fileListQuery(workloadId, location ?? { kind: "container", target: "", path }),
    enabled: !blocked && location !== null,
    refetchOnWindowFocus: false,
  });
  const transfers: Transfers = { supported: transfersSupported, limit: limits.data?.file_bytes };

  const read = useReadFile(workloadId);
  const metadata = useFileMetadata(workloadId);
  const write = useWriteFile(workloadId);
  const createFile = useWriteFile(workloadId);
  const upload = useUploadFile(workloadId);
  const remove = useDeleteFile(workloadId);
  const busy = blocked || read.isPending || metadata.isPending || write.isPending || createFile.isPending || upload.isPending || remove.isPending;
  const dirty = file?.text != null && draft !== file.text;
  const activeUpload: Upload | null = upload.isPending ? { file: upload.variables.file, path: upload.variables.path, replacing: upload.variables.revision !== "" } : null;
  const replaceFailed = upload.isError && upload.variables.revision !== "";

  const entries = listing.data ? sortEntries(listing.data.entries) : [];
  const names = new Set(entries.map((entry) => entry.name));
  const containersRunning = targets.data?.targets.filter((item) => item.kind === "container" && item.state === "running").length ?? 0;

  function guard(action: () => void) {
    if (dirty) setPendingLeave(() => action);
    else action();
  }

  function resetFeedback() {
    setLocalError(null);
    setStatus(null);
    remove.reset();
    // A running upload keeps its state so it can still be cancelled and its result reported.
    if (!upload.isPending) upload.reset();
    write.reset();
    metadata.reset();
  }

  function closeFile() {
    setFile(null);
    setDraft("");
    read.reset();
    metadata.reset();
    write.reset();
    if (upload.isError) upload.reset();
  }

  function refreshListing() {
    if (location) void client.invalidateQueries({ queryKey: fileKeys.list(workloadId, location) });
  }

  function chooseTarget(key: string) {
    guard(() => {
      resetFeedback();
      closeFile();
      setSelectedKey(key);
      setPath("/");
    });
  }

  function navigate(next: string) {
    guard(() => {
      resetFeedback();
      closeFile();
      setPath(next);
    });
  }

  function show(location: FileLocation, bytes: Uint8Array<ArrayBuffer>, revision: string) {
    const decoded = decodeText(bytes);
    const carriageReturns = decoded?.includes("\r") ?? false;
    const text = carriageReturns ? null : decoded;
    setFile({ location, size: bytes.length, bytes, text, revision, carriageReturns });
    setDraft(text ?? "");
  }

  function showMetadata(location: FileLocation, size: number, revision: string) {
    setFile({ location, size, revision, bytes: null, text: null, carriageReturns: false });
    setDraft("");
  }

  // Files the editor can hold are read in full; larger ones only have their size and revision read, so nothing is buffered.
  function load(location: FileLocation, size: number) {
    if (size <= maxFileBytes) {
      read.mutate(location, { onSuccess: (response) => show(location, decodeBase64(response.data), response.revision) });
    } else {
      metadata.mutate(location, { onSuccess: (response) => showMetadata(location, response.size ?? size, response.revision) });
    }
  }

  function openFile(location: FileLocation, size: number) {
    guard(() => {
      resetFeedback();
      load(location, size);
    });
  }

  // The next save must name the revision just written, which is the SHA-256 of the bytes sent.
  async function nextRevision(bytes: Uint8Array<ArrayBuffer>) {
    try {
      return await revisionOf(bytes);
    } catch {
      setLocalError("This browser cannot compute file revisions. Open Cove over HTTPS to change files.");
      return null;
    }
  }

  async function save() {
    if (!file || file.text === null) return;
    const bytes = encoder.encode(draft);
    const saved = draft;
    setStatus(null);
    const revision = await nextRevision(bytes);
    if (revision === null) return;
    write.mutate(
      { ...file.location, data: encodeBase64(bytes), revision: file.revision },
      {
        onSuccess: () => {
          setFile((current) => (current && current.location.path === file.location.path ? { ...current, size: bytes.length, bytes, text: saved, revision } : current));
          refreshListing();
        },
      },
    );
  }

  function create(name: string) {
    if (!location) return;
    const created = { ...location, path: joinPath(path, name) };
    createFile.mutate(
      { ...created, data: "", revision: "" },
      {
        onSuccess: () => {
          setCreating(false);
          write.reset();
          show(created, new Uint8Array(), emptyRevision);
          setStatus(`Created ${name}.`);
          refreshListing();
        },
      },
    );
  }

  function sizeProblem(chosen: File) {
    if (transfers.limit === undefined || chosen.size <= transfers.limit) return undefined;
    return `${chosen.name} is ${formatBinaryBytes(chosen.size)}; files are limited to ${formatBinaryBytes(transfers.limit)}.`;
  }

  function startUpload(location: FileLocation, chosen: File, revision: string, onSuccess: (size: number, revision: string) => void) {
    const controller = new AbortController();
    uploadAbort.current = controller;
    upload.mutate(
      { ...location, file: chosen, revision, signal: controller.signal },
      {
        onSuccess: (receipt) => onSuccess(receipt.size, receipt.revision),
        onSettled: () => {
          if (uploadAbort.current === controller) uploadAbort.current = null;
        },
      },
    );
  }

  function chooseUpload(event: ChangeEvent<HTMLInputElement>) {
    const chosen = event.target.files?.[0];
    event.target.value = "";
    if (!chosen || !location) return;
    resetFeedback();
    const problem = sizeProblem(chosen) ?? (names.has(chosen.name) ? `${chosen.name} already exists in this folder. Open it and use Replace to overwrite it.` : fileNameError(chosen.name, path, location.kind));
    if (problem) {
      setLocalError(problem);
      return;
    }
    startUpload({ ...location, path: joinPath(path, chosen.name) }, chosen, "", (size) => {
      setStatus(`Uploaded ${chosen.name} (${formatBinaryBytes(size)}).`);
    });
  }

  function chooseReplacement(event: ChangeEvent<HTMLInputElement>) {
    const chosen = event.target.files?.[0];
    event.target.value = "";
    if (!chosen || !file) return;
    resetFeedback();
    const problem = sizeProblem(chosen);
    if (problem) setLocalError(problem);
    else setReplacing(chosen);
  }

  function confirmReplace() {
    if (!file || !replacing) return;
    const chosen = replacing;
    const location = file.location;
    setReplacing(null);
    startUpload(location, chosen, file.revision, (size, revision) => {
      // The receipt's revision is what the next change must name, so it is kept even if showing the new contents fails.
      showMetadata(location, size, revision);
      if (size <= maxFileBytes) load(location, size);
      setStatus(`Replaced ${baseName(location.path)} with ${chosen.name}.`);
    });
  }

  function confirmDelete() {
    if (!deleting || !location) return;
    const removed = joinPath(path, deleting.name);
    remove.mutate(
      { ...location, path: removed },
      {
        onSuccess: () => {
          if (file?.location.path === removed) closeFile();
          setStatus(`Deleted ${deleting.name}.`);
          setDeleting(null);
          refreshListing();
        },
        onError: () => setDeleting(null),
      },
    );
  }

  // The page explains why file access is paused; there is nothing earlier to show.
  if (blocked && !targets.data) return null;
  if (targets.isPending) return <Section title="Files" flush><LoadingRows label="Loading containers and volumes" rows={3} /></Section>;
  if (targets.isError) {
    return (
      <Section title="Files">
        <QueryError what="containers and volumes" error={targets.error} onRetry={() => void targets.refetch()} retrying={targets.isFetching} />
      </Section>
    );
  }
  const available = targets.data?.targets ?? [];
  if (!blocked && available.length === 0) {
    return <EmptyState title="Nothing to browse">This resource has no containers or Cove-owned named volumes on its node. Deploy it first; host paths and external volumes are never shown.</EmptyState>;
  }
  const mutationError =
    localError ??
    actionError([
      [remove.error, "Could not delete"],
      [replaceFailed ? null : upload.error, "Could not upload"],
      [read.error, "Could not open the file"],
      [metadata.error, "Could not read the file's size and revision"],
    ]);

  return (
    <>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Files"
        description={filesDescription(transfers)}
        actions={location && (
          <Button size="sm" onClick={() => { resetFeedback(); void targets.refetch(); refreshListing(); }} disabled={blocked || upload.isPending} pending={listing.isFetching}>
            {!listing.isFetching && <RotateCw aria-hidden className="size-3.5" />}Refresh
          </Button>
        )}
        flush
      >
        <div className="divide-y divide-line">
          <div className="space-y-3 p-4">
            <TargetPicker targets={available} limited={Boolean(targets.data?.limited)} value={selectedKey} disabled={blocked || upload.isPending} onChange={chooseTarget} />
            {!transfersSupported && (
              <Notice title="Downloads and uploads need an upgraded worker">
                This worker can open and edit text files up to {editorLimit}. Upgrade and verify it to download files, upload them and replace large or binary files.
              </Notice>
            )}
            {limits.isError && (
              <QueryError what="the file transfer limit" error={limits.error} onRetry={() => void limits.refetch()} retrying={limits.isFetching} />
            )}
            {target && <TargetNotice target={target} containersRunning={containersRunning} />}
            {target && !targetAvailable(target) && <Notice tone="warn" title={`${target.name} is not running`}>Start the resource to browse this container. Cove does not start or stop containers for file access.</Notice>}
          </div>
          {location && (
            <>
              <FolderToolbar
                path={path}
                navigationDisabled={blocked || upload.isPending}
                busy={busy}
                transfers={transfers}
                uploading={activeUpload?.replacing === false}
                limitsLoading={limits.isLoading}
                onNavigate={navigate}
                onCreate={() => { resetFeedback(); setCreating(true); }}
                onUpload={chooseUpload}
              />
              <FolderFeedback upload={activeUpload?.replacing ? null : activeUpload} error={mutationError} localOnly={localError !== null} status={status} onCancelUpload={() => uploadAbort.current?.abort()} />
              {listing.isPending ? (
                <LoadingRows label="Loading folder" rows={4} />
              ) : listing.isError ? (
                <div className="p-4">
                  <QueryError what="this folder" error={listing.error} onRetry={() => void listing.refetch()} retrying={listing.isFetching} />
                </div>
              ) : (
                <>
                  {listing.data.limited && (
                    <div className="px-3 py-2">
                      <Notice tone="warn" title="Some entries are not shown">This folder has more than {maxEntries} entries, or names Cove cannot display safely (control characters or invalid UTF-8).</Notice>
                    </div>
                  )}
                  {entries.length === 0 ? (
                    <p className="px-3 py-6 text-center text-[13px] text-fg-muted">This folder is empty.</p>
                  ) : (
                    <EntryList
                      workloadId={workloadId}
                      location={location}
                      entries={entries}
                      transfers={transfers}
                      openPath={file?.location.path}
                      readingPath={[read, metadata].find((mutation) => mutation.isPending)?.variables?.path}
                      disabled={busy}
                      offline={offline || blocked}
                      onOpen={(entry, entryLocation) => (entry.kind === "directory" ? navigate(entryLocation.path) : openFile(entryLocation, entry.size))}
                      onDelete={(entry) => { resetFeedback(); setDeleting(entry); }}
                    />
                  )}
                </>
              )}
            </>
          )}
        </div>
      </Section>
      {file && (
        <FileEditor
          file={file}
          draft={draft}
          downloadUrl={fileDownloadUrl(workloadId, file.location)}
          transfers={transfers}
          disabled={busy}
          offline={offline || blocked}
          saving={write.isPending}
          saveError={write.error}
          replaceError={replaceFailed ? upload.error : null}
          saved={write.isSuccess && !dirty}
          upload={activeUpload}
          onDraft={(value) => { write.reset(); setDraft(value); }}
          onSave={() => void save()}
          onDiscard={() => { write.reset(); setDraft(file.text ?? ""); }}
          onReread={() => guard(() => { resetFeedback(); load(file.location, file.size); })}
          onReplace={() => replaceInput.current?.click()}
          onCancelUpload={() => uploadAbort.current?.abort()}
          onClose={() => guard(closeFile)}
        />
      )}
      {transfersSupported && <input ref={replaceInput} type="file" className="hidden" tabIndex={-1} aria-hidden onChange={chooseReplacement} />}
      {location && creating && (
        <CreateFileDialog
          open
          directory={path}
          kind={location.kind}
          existing={names}
          pending={createFile.isPending}
          error={createFile.isError ? errorMessage(createFile.error) : undefined}
          onCreate={create}
          onClose={() => { createFile.reset(); setCreating(false); }}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        title={`Delete ${deleting?.name ?? ""}?`}
        confirmLabel={deleting?.kind === "directory" ? "Delete folder" : "Delete file"}
        danger
        pending={remove.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={confirmDelete}
      >
        <span className="font-mono break-all text-fg">{deleting ? joinPath(path, deleting.name) : ""}</span> in {target?.kind === "volume" ? "volume" : "container"} {target?.name} will be permanently removed. Cove keeps no copy and cannot undo this.
        {deleting?.kind === "directory" && " Only empty folders can be deleted; Cove never deletes recursively."}
        {target?.kind === "volume" && " Every container using this volume must be stopped."}
      </ConfirmDialog>
      <ConfirmDialog
        open={replacing !== null}
        title={`Replace ${file ? baseName(file.location.path) : "file"}?`}
        confirmLabel="Replace file"
        danger
        onCancel={() => setReplacing(null)}
        onConfirm={confirmReplace}
      >
        The contents of <span className="font-mono break-all text-fg">{file?.location.path}</span> ({formatBinaryBytes(file?.size ?? 0)}, revision <span className="font-mono">{file?.revision.slice(0, 12)}</span>) will be overwritten with{" "}
        <span className="break-all">{replacing?.name}</span> ({formatBinaryBytes(replacing?.size ?? 0)}), byte for byte.
        {dirty && " Your unsaved edits will be discarded."} The replacement fails if the file changed since Cove read this revision, and leaves it unchanged. Its owner and permissions are kept. Cove keeps no copy of the old contents.
      </ConfirmDialog>
      <ConfirmDialog
        open={pendingLeave !== null}
        title="Discard unsaved changes?"
        confirmLabel="Discard changes"
        danger
        onCancel={() => setPendingLeave(null)}
        onConfirm={() => {
          const action = pendingLeave;
          setPendingLeave(null);
          action?.();
        }}
      >
        Your edits to {file?.location.path} have not been saved.
      </ConfirmDialog>
    </>
  );
}
