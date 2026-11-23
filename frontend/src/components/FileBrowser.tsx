import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUp, Download, File, FilePlus, FileQuestion, Folder, Link2, RotateCw, Trash2, Upload, X } from "lucide-react";
import { useEffect, useId, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import {
  fileKeys,
  fileListQuery,
  fileTargetsQuery,
  useDeleteFile,
  useDownloadFile,
  useReadFile,
  useUploadFile,
  useWriteFile,
  type FileEntry,
  type FileLocation,
  type FileTarget,
} from "../api/files";
import { formatBytes } from "../lib/format";
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
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, EmptyState, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

interface OpenFile {
  location: FileLocation;
  bytes: Uint8Array<ArrayBuffer>;
  // Null when the bytes cannot be edited as text without changing them; such files can be downloaded or replaced.
  text: string | null;
  carriageReturns: boolean;
  revision: string;
}

const encoder = new TextEncoder();
const sizeLimit = formatBytes(maxFileBytes);

function saveBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}

// Section titles do not wrap long unbroken names, so the full path is shown in the description instead.
function shortName(name: string) {
  return name.length > 48 ? `${name.slice(0, 22)}…${name.slice(-22)}` : name;
}

function targetLabel(target: FileTarget) {
  if (target.kind === "volume") return target.name;
  return targetAvailable(target) ? target.name : `${target.name} (${target.state || "unknown"}, not running)`;
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

function FolderPath({ path, onNavigate }: { path: string; onNavigate: (path: string) => void }) {
  const segments = pathSegments(path);
  const crumbClass = "rounded px-1 font-mono text-xs text-fg-muted hover:bg-raised hover:text-fg pointer-coarse:min-h-10";
  return (
    <nav aria-label="Folder path" className="min-w-0 flex-1">
      <ol className="flex min-w-0 flex-wrap items-center gap-0.5">
        <li>
          {segments.length === 0 ? (
            <span aria-current="location" className="px-1 font-mono text-xs text-fg">/</span>
          ) : (
            <button type="button" className={crumbClass} onClick={() => onNavigate("/")}>/</button>
          )}
        </li>
        {segments.map((segment, index) => (
          <li key={segment.path} className="flex min-w-0 items-center gap-0.5">
            {index > 0 && <span aria-hidden className="font-mono text-xs text-fg-subtle">/</span>}
            {index === segments.length - 1 ? (
              <span aria-current="location" className="truncate px-1 font-mono text-xs text-fg" title={segment.name}>{segment.name}</span>
            ) : (
              <button type="button" className={`${crumbClass} max-w-48 truncate`} title={segment.name} onClick={() => onNavigate(segment.path)}>{segment.name}</button>
            )}
          </li>
        ))}
      </ol>
    </nav>
  );
}

const entryIcons = { directory: Folder, file: File, link: Link2, other: FileQuestion } as const;

function entryDetail(entry: FileEntry, blocked: boolean) {
  if (entry.kind === "directory") return blocked ? "Folder · not browsable" : "Folder";
  if (entry.kind === "link") return "Symbolic link · not followed";
  if (entry.kind === "other") return "Special file · not readable";
  return entry.size > maxFileBytes ? `${formatBytes(entry.size)} · larger than ${sizeLimit}` : formatBytes(entry.size);
}

interface EntryRowProps {
  entry: FileEntry;
  path: string;
  blocked: boolean;
  current: boolean;
  disabled: boolean;
  reading: boolean;
  downloading: boolean;
  onOpen: () => void;
  onDownload: () => void;
  onDelete: () => void;
}

function EntryRow({ entry, path, blocked, current, disabled, reading, downloading, onOpen, onDownload, onDelete }: EntryRowProps) {
  const Icon = entryIcons[entry.kind];
  const openable = (entry.kind === "directory" && !blocked) || (entry.kind === "file" && entry.size <= maxFileBytes);
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
          {entryDetail(entry, blocked)}
          {reading && " · opening…"}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        {entry.kind === "file" && entry.size <= maxFileBytes && (
          <Button variant="ghost" size="sm" aria-label={`Download ${entry.name}`} title={`Download ${entry.name}`} disabled={disabled} pending={downloading} onClick={onDownload}>
            {!downloading && <Download aria-hidden className="size-3.5" />}
          </Button>
        )}
        {(entry.kind === "file" || (entry.kind === "directory" && !blocked)) && (
          <Button variant="ghost" size="sm" aria-label={`Delete ${entry.name}`} title={`Delete ${entry.name}`} disabled={disabled} onClick={onDelete}>
            <Trash2 aria-hidden className="size-3.5" />
          </Button>
        )}
      </div>
    </li>
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

interface EditorProps {
  file: OpenFile;
  draft: string;
  disabled: boolean;
  saving: boolean;
  saveError: unknown;
  saved: boolean;
  downloading: boolean;
  onDraft: (value: string) => void;
  onSave: () => void;
  onDiscard: () => void;
  onReread: () => void;
  onReplace: () => void;
  onDownload: () => void;
  onClose: () => void;
}

function FileEditor({ file, draft, disabled, saving, saveError, saved, downloading, onDraft, onSave, onDiscard, onReread, onReplace, onDownload, onClose }: EditorProps) {
  const id = useId();
  const editor = useRef<HTMLTextAreaElement>(null);
  const binaryNotice = useRef<HTMLDivElement>(null);
  const dirty = file.text !== null && draft !== file.text;
  const bytes = file.text === null ? file.bytes.length : utf8Length(draft);
  const tooLarge = bytes > maxFileBytes;
  const path = file.location.path;

  useEffect(() => {
    if (file.text === null) binaryNotice.current?.focus();
    else editor.current?.focus();
  }, [path, file.text]);

  return (
    <Section
      title={shortName(baseName(path))}
      description={
        <>
          <span className="font-mono break-all">{path}</span> · revision <span className="font-mono">{file.revision.slice(0, 12)}</span>
        </>
      }
      actions={
        <>
          <Button size="sm" onClick={onDownload} disabled={disabled} pending={downloading}>{!downloading && <Download aria-hidden className="size-3.5" />}Download</Button>
          <Button size="sm" onClick={onReplace} disabled={disabled}><Upload aria-hidden className="size-3.5" />Replace</Button>
          <Button variant="ghost" size="sm" aria-label="Close file" title="Close file" onClick={onClose}><X aria-hidden className="size-3.5" /></Button>
        </>
      }
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
        {Boolean(saveError) && (
          <Notice
            tone="error"
            title="The file was not changed"
            action={<Button variant="ghost" size="sm" onClick={onReread} disabled={disabled}><RotateCw aria-hidden className="size-3.5" />Read again</Button>}
          >
            {errorMessage(saveError)} Your edits are kept here. If the file changed since you opened it, read it again and reapply them.
          </Notice>
        )}
        {file.text === null ? (
          <div ref={binaryNotice} tabIndex={-1} className="rounded-md">
            {file.carriageReturns ? (
              <Notice title="Text with carriage returns">
                Browsers convert CR line endings while editing, which would rewrite every line of this {formatBytes(file.bytes.length)} file. Download it, edit it locally and replace it instead.
              </Notice>
            ) : (
              <Notice title="Binary file">
                This {formatBytes(file.bytes.length)} file is not UTF-8 text, so it cannot be edited here. Download it, or replace it with a local file.
              </Notice>
            )}
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
              {bytes.toLocaleString()} of {maxFileBytes.toLocaleString()} bytes{tooLarge && " — remove content before saving"}
            </p>
          </div>
        )}
      </div>
    </Section>
  );
}

export function FileBrowser({ workloadId, blocked }: { workloadId: string; blocked: boolean }) {
  const client = useQueryClient();
  const pickerId = useId();
  const uploadInput = useRef<HTMLInputElement>(null);
  const replaceInput = useRef<HTMLInputElement>(null);
  const [selectedKey, setSelectedKey] = useState("");
  const [path, setPath] = useState("/");
  const [file, setFile] = useState<OpenFile | null>(null);
  const [draft, setDraft] = useState("");
  const [pendingLeave, setPendingLeave] = useState<(() => void) | null>(null);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<FileEntry | null>(null);
  const [replacing, setReplacing] = useState<{ name: string; bytes: Uint8Array<ArrayBuffer>; revision: string } | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);
  const [status, setStatus] = useState<string | null>(null);

  const targets = useQuery({ ...fileTargetsQuery(workloadId), enabled: !blocked, refetchOnWindowFocus: false });
  const target = targets.data?.targets.find((item) => targetKey(item) === selectedKey);
  const location = target && targetAvailable(target) ? { kind: target.kind, target: target.id, path } : null;
  const listing = useQuery({
    ...fileListQuery(workloadId, location ?? { kind: "container", target: "", path }),
    enabled: !blocked && location !== null,
    refetchOnWindowFocus: false,
  });

  const read = useReadFile(workloadId);
  const download = useDownloadFile(workloadId);
  const write = useWriteFile(workloadId);
  const createFile = useWriteFile(workloadId);
  const upload = useUploadFile(workloadId);
  const remove = useDeleteFile(workloadId);
  const busy = blocked || read.isPending || write.isPending || createFile.isPending || upload.isPending || remove.isPending;
  const dirty = file?.text != null && draft !== file.text;

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
    download.reset();
    remove.reset();
    upload.reset();
    write.reset();
  }

  function closeFile() {
    setFile(null);
    setDraft("");
    read.reset();
    write.reset();
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
    setFile({ location, bytes, text, revision, carriageReturns });
    setDraft(text ?? "");
  }

  function openFile(location: FileLocation) {
    guard(() => {
      resetFeedback();
      read.mutate(location, { onSuccess: (response) => show(location, decodeBase64(response.data), response.revision) });
    });
  }

  function downloadFile(location: FileLocation) {
    download.mutate(location, { onSuccess: (blob) => saveBlob(blob, baseName(location.path)) });
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
          setFile((current) => (current && current.location.path === file.location.path ? { ...current, bytes, text: saved, revision } : current));
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

  async function chooseUpload(event: ChangeEvent<HTMLInputElement>) {
    const chosen = event.target.files?.[0];
    event.target.value = "";
    if (!chosen || !location) return;
    resetFeedback();
    const problem = chosen.size > maxFileBytes ? `${chosen.name} is ${formatBytes(chosen.size)}; uploads are limited to ${sizeLimit}.` : names.has(chosen.name) ? `${chosen.name} already exists in this folder. Open it and use Replace to overwrite it.` : fileNameError(chosen.name, path, location.kind);
    if (problem) {
      setLocalError(problem);
      return;
    }
    const bytes = new Uint8Array(await chosen.arrayBuffer());
    upload.mutate(
      { ...location, path: joinPath(path, chosen.name), data: encodeBase64(bytes), revision: "" },
      { onSuccess: () => { setStatus(`Uploaded ${chosen.name}.`); refreshListing(); } },
    );
  }

  async function chooseReplacement(event: ChangeEvent<HTMLInputElement>) {
    const chosen = event.target.files?.[0];
    event.target.value = "";
    if (!chosen || !file) return;
    resetFeedback();
    if (chosen.size > maxFileBytes) {
      setLocalError(`${chosen.name} is ${formatBytes(chosen.size)}; uploads are limited to ${sizeLimit}.`);
      return;
    }
    const bytes = new Uint8Array(await chosen.arrayBuffer());
    const revision = await nextRevision(bytes);
    if (revision !== null) setReplacing({ name: chosen.name, bytes, revision });
  }

  function confirmReplace() {
    if (!file || !replacing) return;
    const { bytes, name, revision } = replacing;
    const location = file.location;
    upload.mutate(
      { ...location, data: encodeBase64(bytes), revision: file.revision },
      {
        onSuccess: () => {
          setReplacing(null);
          show(location, bytes, revision);
          setStatus(`Replaced ${baseName(location.path)} with ${name}.`);
          refreshListing();
        },
        onError: () => setReplacing(null),
      },
    );
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
  const containers = available.filter((item) => item.kind === "container");
  const volumes = available.filter((item) => item.kind === "volume");
  const mutationError = localError ?? (remove.isError ? `Could not delete: ${errorMessage(remove.error)}` : upload.isError && !file ? `Could not upload: ${errorMessage(upload.error)}` : download.isError ? `Could not download: ${errorMessage(download.error)}` : read.isError ? `Could not open the file: ${errorMessage(read.error)}` : null);

  return (
    <>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Files"
        description={`Browse one folder at a time, up to ${maxEntries} entries. Files up to ${sizeLimit} can be opened, edited, uploaded and downloaded. Every read and change is audited without its contents.`}
        actions={location && (
          <Button size="sm" onClick={() => { resetFeedback(); void targets.refetch(); refreshListing(); }} disabled={blocked} pending={listing.isFetching}>
            {!listing.isFetching && <RotateCw aria-hidden className="size-3.5" />}Refresh
          </Button>
        )}
        flush
      >
        <div className="divide-y divide-line">
          <div className="space-y-3 p-4">
            <Field id={pickerId} label="Container or volume" hint={targets.data?.limited ? "Some volumes are not listed: at most 128 owned volumes and 32 containers are shown." : "Stopped containers cannot be browsed. Volumes can be browsed while the resource runs."}>
              <Select id={pickerId} className={`${inputClass} sm:max-w-md`} value={selectedKey} disabled={blocked} onChange={(event) => chooseTarget(event.target.value)} aria-describedby={`${pickerId}-hint`}>
                <option value="" disabled>Choose a container or volume</option>
                {containers.length > 0 && <optgroup label="Containers">{containers.map((item) => <option key={targetKey(item)} value={targetKey(item)} disabled={!targetAvailable(item)}>{targetLabel(item)}</option>)}</optgroup>}
                {volumes.length > 0 && <optgroup label="Volumes">{volumes.map((item) => <option key={targetKey(item)} value={targetKey(item)}>{targetLabel(item)}</option>)}</optgroup>}
              </Select>
            </Field>
            {target && <TargetNotice target={target} containersRunning={containersRunning} />}
            {target && !targetAvailable(target) && <Notice tone="warn" title={`${target.name} is not running`}>Start the resource to browse this container. Cove does not start or stop containers for file access.</Notice>}
          </div>
          {location && (
            <>
              <div className="flex flex-wrap items-center gap-2 px-3 py-2">
                <Button variant="ghost" size="sm" aria-label="Parent folder" title="Parent folder" disabled={path === "/" || blocked} onClick={() => navigate(parentPath(path))}>
                  <ArrowUp aria-hidden className="size-3.5" />
                </Button>
                <FolderPath path={path} onNavigate={navigate} />
                <div className="flex items-center gap-2">
                  <Button size="sm" disabled={busy} onClick={() => { resetFeedback(); setCreating(true); }}><FilePlus aria-hidden className="size-3.5" />New file</Button>
                  <Button size="sm" disabled={busy} pending={upload.isPending && !replacing} onClick={() => uploadInput.current?.click()}>{!(upload.isPending && !replacing) && <Upload aria-hidden className="size-3.5" />}Upload</Button>
                  <input ref={uploadInput} type="file" className="hidden" tabIndex={-1} aria-hidden onChange={(event) => void chooseUpload(event)} />
                </div>
              </div>
              {(mutationError || status) && (
                <div className="px-3 py-2">
                  {mutationError ? <Notice tone="error" title="The action did not complete">{mutationError}</Notice> : <p role="status" className="text-xs text-fg-muted">{status}</p>}
                </div>
              )}
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
                    <ul aria-label={`Contents of ${path}`} className="divide-y divide-line">
                      {entries.map((entry) => {
                        const entryPath = joinPath(path, entry.name);
                        const entryLocation = { ...location, path: entryPath };
                        return (
                          <EntryRow
                            key={entry.name}
                            entry={entry}
                            path={entryPath}
                            blocked={isBlockedPath(location.kind, entryPath)}
                            current={file?.location.path === entryPath}
                            disabled={busy}
                            reading={read.isPending && read.variables.path === entryPath}
                            downloading={download.isPending && download.variables.path === entryPath}
                            onOpen={() => (entry.kind === "directory" ? navigate(entryPath) : openFile(entryLocation))}
                            onDownload={() => { resetFeedback(); downloadFile(entryLocation); }}
                            onDelete={() => { resetFeedback(); setDeleting(entry); }}
                          />
                        );
                      })}
                    </ul>
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
          disabled={busy}
          saving={write.isPending}
          saveError={write.error ?? (upload.isError ? upload.error : null)}
          saved={write.isSuccess && !dirty}
          downloading={download.isPending && download.variables.path === file.location.path}
          onDraft={(value) => { write.reset(); setDraft(value); }}
          onSave={() => void save()}
          onDiscard={() => { write.reset(); setDraft(file.text ?? ""); }}
          onReread={() => openFile(file.location)}
          onReplace={() => replaceInput.current?.click()}
          onDownload={() => { resetFeedback(); downloadFile(file.location); }}
          onClose={() => guard(closeFile)}
        />
      )}
      <input ref={replaceInput} type="file" className="hidden" tabIndex={-1} aria-hidden onChange={(event) => void chooseReplacement(event)} />
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
        pending={upload.isPending}
        onCancel={() => setReplacing(null)}
        onConfirm={confirmReplace}
      >
        The contents of <span className="font-mono break-all text-fg">{file?.location.path}</span> will be overwritten with {replacing?.name} ({formatBytes(replacing?.bytes.length ?? 0)}).
        {dirty && " Your unsaved edits will be discarded."} The replacement fails if the file changed since you opened it. Cove keeps no copy of the old contents.
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
