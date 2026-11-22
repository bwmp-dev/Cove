import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, FileUp, Plus, Trash2 } from "lucide-react";
import { useId, useRef, useState, type ChangeEvent, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { composeBundleKey, composeBundleQuery, exportComposeBundle, saveComposeBundle, type ComposeBundleSummary } from "../api/compose-bundle";
import type { Workload } from "../api/types";
import { bundleLimits, bundleMode, contentBytes, draftFromBundle, draftFromSummary, fileRow, move, primaryFile, sameDraft, validateBundle, type BundleDraft, type BundleMode, type FileRow } from "../lib/compose-bundle";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { UnsavedChangesGuard } from "./SaveBar";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const title = "Compose files and profiles";
const description = "Merge override files after the primary Compose file and choose which profiles start. The selection belongs to this resource in its environment. Saving does not deploy.";
const savedStatus = "Saved. Changes apply on the next deploy.";

interface Props {
  workload: Workload;
  editable: boolean;
  // PR previews and environment previews deploy one allowlisted file; the server refuses bundles for them.
  temporary: boolean;
  blocked: boolean;
  blockers: ReactNode;
}

export function ComposeBundleSection({ workload, editable, temporary, blocked, blockers }: Props) {
  if (temporary) {
    return (
      <Section width="form" title={title}>
        <Notice>Preview resources deploy the single Compose file from their source. Select override files and profiles on the resource the preview was created from.</Notice>
      </Section>
    );
  }
  return <ComposeBundlePanel workload={workload} editable={editable} blocked={blocked} blockers={blockers} />;
}

type Editing = { draft: BundleDraft; replacing: boolean };

function ComposeBundlePanel({ workload, editable, blocked, blockers }: Omit<Props, "temporary">) {
  const client = useQueryClient();
  const summary = useQuery(composeBundleQuery(workload.id));
  const mode = bundleMode(workload);
  const primary = primaryFile(workload);
  // File contents only live here while editing; cancelling, saving or leaving the page drops them.
  const [editing, setEditing] = useState<Editing | null>(null);
  const [dialog, setDialog] = useState<"load" | "clear" | null>(null);
  const [status, setStatus] = useState<string | null>(null);
  const clear = useMutation({
    gcTime: 0,
    mutationFn: () => saveComposeBundle(workload.id, null),
    onSuccess: (result) => {
      client.setQueryData(composeBundleKey(workload.id), result);
      setStatus("Selection cleared. Only the primary Compose file is used from the next deploy.");
      setDialog(null);
    },
    networkMode: "always",
  });

  if (summary.isPending) {
    return (
      <Section width="form" title={title} description={description}>
        <LoadingRows label="Loading Compose files" rows={2} />
      </Section>
    );
  }
  if (summary.isError) {
    return (
      <Section width="form" title={title} description={description}>
        <QueryError error={summary.error} onRetry={() => void summary.refetch()} retrying={summary.isFetching} what="Compose files" />
      </Section>
    );
  }
  const saved = summary.data;

  if (editing) {
    return (
      <BundleEditor
        workload={workload}
        mode={mode}
        primary={primary}
        initial={editing.draft}
        replacing={editing.replacing}
        blocked={blocked}
        blockers={blockers}
        onCancel={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          setStatus(savedStatus);
        }}
      />
    );
  }

  // Saved contents are never in the metadata, so editing pasted files needs the audited export or an explicit replacement.
  const needsContents = mode === "inline" && saved.enabled && (saved.files.length > 0 || saved.assets.length > 0);
  function startEditing() {
    setStatus(null);
    if (needsContents) setDialog("load");
    else setEditing({ draft: draftFromSummary(saved), replacing: false });
  }

  return (
    <Section width="form" title={title} description={description}>
      <div className="space-y-4">
        {editable && blockers}
        {!saved.available && (
          <Notice tone="warn" title="Upgrade the worker to select files and profiles">
            This resource's node runs a worker without support for override files and profiles. Upgrade and verify the worker first.
            {editable && saved.enabled && " You can still clear the saved selection."}
          </Notice>
        )}
        {status && <Notice tone="success">{status}</Notice>}
        <BundleSummary saved={saved} mode={mode} primary={primary} />
        {editable && (
          <div className="flex flex-wrap items-center justify-end gap-2 border-t border-line pt-4">
            {saved.enabled && (
              <Button variant="ghost" disabled={blocked} onClick={() => { setStatus(null); clear.reset(); setDialog("clear"); }}>
                Clear selection
              </Button>
            )}
            <Button disabled={blocked || !saved.available} onClick={startEditing}>
              {saved.enabled ? "Edit files and profiles" : "Select files and profiles"}
            </Button>
          </div>
        )}
      </div>
      <Dialog open={dialog === "load"} title="Load saved Compose files?" onClose={() => setDialog(null)}>
        <LoadSaved
          workload={workload}
          onClose={() => setDialog(null)}
          onLoaded={(draft) => {
            setDialog(null);
            setEditing({ draft, replacing: false });
          }}
          onReplace={() => {
            setDialog(null);
            setEditing({ draft: { ...draftFromSummary(saved), files: [], assets: [] }, replacing: true });
          }}
        />
      </Dialog>
      <ConfirmDialog
        open={dialog === "clear"}
        title="Clear Compose files and profiles?"
        confirmLabel="Clear selection"
        danger
        pending={clear.isPending}
        onConfirm={() => clear.mutate()}
        onCancel={() => setDialog(null)}
      >
        <div className="space-y-3">
          <p>Saved override files, ancillary files and profiles are removed from this resource. From the next deploy only the primary Compose file is used. Nothing is deployed now.</p>
          {clear.isError && <Notice tone="error" title="Could not clear the selection">{errorMessage(clear.error)}</Notice>}
        </div>
      </ConfirmDialog>
    </Section>
  );
}

function BundleSummary({ saved, mode, primary }: { saved: ComposeBundleSummary; mode: BundleMode; primary: string }) {
  const overrides = mode === "inline" ? saved.files : saved.paths;
  return (
    <DefinitionList
      items={[
        {
          term: "Merge order",
          value: (
            <ol className="space-y-1">
              <li className="flex min-w-0 items-baseline gap-2">
                <span className="text-xs text-fg-subtle tabular-nums">1.</span>
                {mode === "profiles" ? <span>Compose managed in Cove</span> : <span className="font-mono text-xs break-all">{primary}</span>}
                <span className="text-xs text-fg-muted">Primary</span>
              </li>
              {overrides.map((name, index) => (
                <li key={name} className="flex min-w-0 items-baseline gap-2">
                  <span className="text-xs text-fg-subtle tabular-nums">{index + 2}.</span>
                  <span className="font-mono text-xs break-all">{name}</span>
                </li>
              ))}
            </ol>
          ),
        },
        ...(mode === "inline"
          ? [{ term: "Ancillary files", value: saved.assets.length > 0 ? <span className="font-mono text-xs break-all">{saved.assets.join(", ")}</span> : "None" }]
          : []),
        { term: "Profiles", value: saved.profiles.length > 0 ? <span className="font-mono text-xs break-all">{saved.profiles.join(", ")}</span> : "None. Only services without a profile start." },
      ]}
    />
  );
}

// Rendered only while its dialog is open, so closing it any way unmounts the gcTime 0 export and its result.
function LoadSaved({ workload, onClose, onLoaded, onReplace }: { workload: Workload; onClose: () => void; onLoaded: (draft: BundleDraft) => void; onReplace: () => void }) {
  const exported = useMutation({
    gcTime: 0,
    mutationFn: () => exportComposeBundle(workload.id),
    onSuccess: (result) => onLoaded(draftFromBundle(result.bundle)),
    networkMode: "always",
  });
  return (
    <div className="space-y-4">
      <Notice tone="warn" title="Saved files can contain secrets">
        Ancillary files such as env files often hold passwords and tokens. Loading shows every saved file on this page and is recorded in the audit log.
      </Notice>
      <p>To edit without reading them, start over instead. Saving then replaces every saved override and ancillary file with what you enter. Profiles are kept.</p>
      {exported.isError && <Notice tone="error" title="Could not load saved files">{errorMessage(exported.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onClose} disabled={exported.isPending}>Cancel</Button>
        <Button onClick={onReplace} disabled={exported.isPending}>Start over</Button>
        <Button variant="primary" pending={exported.isPending} onClick={() => exported.mutate()}>Load saved files</Button>
      </div>
    </div>
  );
}

interface EditorProps {
  workload: Workload;
  mode: BundleMode;
  primary: string;
  initial: BundleDraft;
  replacing: boolean;
  blocked: boolean;
  blockers: ReactNode;
  onCancel: () => void;
  onSaved: () => void;
}

function BundleEditor({ workload, mode, primary, initial, replacing, blocked, blockers, onCancel, onSaved }: EditorProps) {
  const id = useId();
  const client = useQueryClient();
  const [draft, setDraft] = useState(initial);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [announcement, setAnnouncement] = useState("");
  // Takes no variables: they would stay in the mutation cache with the file contents after it settles.
  const save = useMutation({
    gcTime: 0,
    mutationFn: () => {
      const { bundle } = validateBundle(draft, mode, primary);
      if (!bundle) throw new Error("Fix the highlighted fields before saving.");
      return saveComposeBundle(workload.id, bundle);
    },
    onSuccess: (result) => {
      client.setQueryData(composeBundleKey(workload.id), result);
      onSaved();
    },
    networkMode: "always",
  });
  const dirty = replacing || !sameDraft(draft, initial);
  const used = contentBytes(draft);

  function edit(change: (current: BundleDraft) => BundleDraft) {
    save.reset();
    setDraft(change);
  }

  function updateRow(list: "files" | "assets" | "paths", key: string, change: Partial<FileRow>) {
    edit((current) => ({ ...current, [list]: current[list].map((row) => (row.key === key ? { ...row, ...change } : row)) }));
  }

  function moveRow(list: "files" | "paths", row: FileRow, index: number, offset: -1 | 1) {
    const position = index + offset + 2;
    edit((current) => ({ ...current, [list]: move(current[list], index, offset) }));
    setAnnouncement(`Moved ${row.path.trim() || "file"} to position ${position}.`);
    // Reordering moves the focused button in the DOM; keep focus on it, or on its pair at the ends of the list.
    requestAnimationFrame(() => {
      const same = document.getElementById(`${id}-${row.key}-${offset < 0 ? "up" : "down"}`) as HTMLButtonElement | null;
      const other = document.getElementById(`${id}-${row.key}-${offset < 0 ? "down" : "up"}`) as HTMLButtonElement | null;
      (same && !same.disabled ? same : other)?.focus();
    });
  }

  function removeRow(list: "files" | "assets" | "paths", key: string) {
    edit((current) => ({ ...current, [list]: current[list].filter((row) => row.key !== key) }));
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateBundle(draft, mode, primary);
    setErrors(result.errors);
    if (result.bundle) {
      save.mutate();
    } else {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
    }
  }

  const fileList = mode === "inline" ? draft.files : draft.paths;
  const fileListName = mode === "inline" ? "files" : "paths";
  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        flush
        title={title}
        description={description}
        footer={
          <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2">
            <p role="status" className="mr-auto text-xs text-fg-muted">{dirty ? "Unsaved changes" : "No changes"}</p>
            <Button variant="ghost" onClick={onCancel} disabled={save.isPending}>Cancel</Button>
            <Button type="submit" variant="primary" disabled={!dirty || blocked} pending={save.isPending}>Save files and profiles</Button>
          </div>
        }
      >
        <p className="sr-only" aria-live="polite">{announcement}</p>
        <fieldset disabled={save.isPending} className="min-w-0 divide-y divide-line">
          {(blockers || replacing) && (
            <div className="space-y-3 p-4">
              {blockers}
              {replacing && (
                <Notice tone="warn" title="Saved files were not loaded">
                  Saving replaces every saved override and ancillary file with the files below. Cancel to keep the saved files.
                </Notice>
              )}
            </div>
          )}
          {mode !== "profiles" && (
            <Subsection
              title={mode === "inline" ? "Override files" : "Override paths"}
              description={
                mode === "inline"
                  ? `Merged after ${primary} in this order. Later files override earlier ones.`
                  : `Repository paths relative to ${primary}, merged after it in this order. Later files override earlier ones.`
              }
            >
              <ol className="space-y-3">
                <li className="flex items-center gap-2 text-[13px]">
                  <span className="w-5 shrink-0 text-right text-xs text-fg-subtle tabular-nums">1.</span>
                  <span className="min-w-0 font-mono text-xs break-all">{primary}</span>
                  <span className="text-xs text-fg-muted">Primary, edited above</span>
                </li>
                {fileList.map((row, index) => {
                  const name = row.path.trim() || `file ${index + 2}`;
                  return (
                    <li key={row.key} className="space-y-2">
                      <div className="flex items-start gap-2">
                        <span className="mt-2 w-5 shrink-0 text-right text-xs text-fg-subtle tabular-nums">{index + 2}.</span>
                        <div className="min-w-0 flex-1">
                          <PathInput id={`${id}-${row.key}-path`} label={`Path of override ${index + 2}`} value={row.path} error={errors[`${row.key}-path`]} onChange={(path) => updateRow(fileListName, row.key, { path })} placeholder={mode === "inline" ? "production.yaml" : "deploy/production.yaml"} />
                        </div>
                        <div className="flex shrink-0 gap-1">
                          <Button id={`${id}-${row.key}-up`} size="sm" variant="ghost" aria-label={`Move ${name} up`} title="Move up" disabled={index === 0} onClick={() => moveRow(fileListName, row, index, -1)}>
                            <ArrowUp aria-hidden className="size-3.5" />
                          </Button>
                          <Button id={`${id}-${row.key}-down`} size="sm" variant="ghost" aria-label={`Move ${name} down`} title="Move down" disabled={index === fileList.length - 1} onClick={() => moveRow(fileListName, row, index, 1)}>
                            <ArrowDown aria-hidden className="size-3.5" />
                          </Button>
                          <Button size="sm" variant="ghost" aria-label={`Remove ${name}`} title="Remove" onClick={() => removeRow(fileListName, row.key)}>
                            <Trash2 aria-hidden className="size-3.5" />
                          </Button>
                        </div>
                      </div>
                      {mode === "inline" && (
                        <div className="pl-7">
                          <ContentInput id={`${id}-${row.key}-content`} label={`Contents of override ${index + 2}`} value={row.content} error={errors[`${row.key}-content`]} onChange={(content) => updateRow("files", row.key, { content })} onFile={(content, fileName) => updateRow("files", row.key, { content, ...(row.path.trim() === "" ? { path: fileName } : {}) })} />
                        </div>
                      )}
                    </li>
                  );
                })}
              </ol>
              <Button size="sm" disabled={fileList.length >= bundleLimits.files} onClick={() => edit((current) => ({ ...current, [fileListName]: [...current[fileListName], fileRow()] }))}>
                <Plus aria-hidden className="size-3.5" />
                {mode === "inline" ? "Add override file" : "Add override path"}
              </Button>
              {fileList.length >= bundleLimits.files && <p className="text-xs text-fg-muted">Up to {bundleLimits.files} overrides can follow the primary file.</p>}
            </Subsection>
          )}
          {mode === "inline" && (
            <Subsection title="Ancillary files" description="Files that Compose reads through include, extends, env_file or build, staged next to compose.yaml under these relative paths. They can hold secrets and are stored encrypted.">
              {draft.assets.length > 0 && (
                <ul className="space-y-3">
                  {draft.assets.map((row, index) => {
                    const name = row.path.trim() || `ancillary file ${index + 1}`;
                    return (
                      <li key={row.key} className="space-y-2">
                        <div className="flex items-start gap-2">
                          <div className="min-w-0 flex-1">
                            <PathInput id={`${id}-${row.key}-path`} label={`Path of ancillary file ${index + 1}`} value={row.path} error={errors[`${row.key}-path`]} onChange={(path) => updateRow("assets", row.key, { path })} placeholder="base.env" />
                          </div>
                          <Button size="sm" variant="ghost" aria-label={`Remove ${name}`} title="Remove" onClick={() => removeRow("assets", row.key)}>
                            <Trash2 aria-hidden className="size-3.5" />
                          </Button>
                        </div>
                        <ContentInput id={`${id}-${row.key}-content`} label={`Contents of ancillary file ${index + 1}`} value={row.content} error={errors[`${row.key}-content`]} onChange={(content) => updateRow("assets", row.key, { content })} onFile={(content, fileName) => updateRow("assets", row.key, { content, ...(row.path.trim() === "" ? { path: fileName } : {}) })} />
                      </li>
                    );
                  })}
                </ul>
              )}
              <Button size="sm" disabled={draft.assets.length >= bundleLimits.assets} onClick={() => edit((current) => ({ ...current, assets: [...current.assets, fileRow()] }))}>
                <Plus aria-hidden className="size-3.5" />
                Add ancillary file
              </Button>
            </Subsection>
          )}
          <div className="space-y-4 p-4">
            <Field id={`${id}-profiles`} label="Profiles" optional error={errors.profiles} hint={`Separate names with spaces or commas, up to ${bundleLimits.profiles}. Services without a profile always start.`}>
              <input
                id={`${id}-profiles`}
                className={`${inputClass} font-mono`}
                value={draft.profiles}
                onChange={(event) => edit((current) => ({ ...current, profiles: event.target.value }))}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                placeholder="worker metrics"
                aria-invalid={errors.profiles ? true : undefined}
                aria-describedby={describedBy(`${id}-profiles`, errors.profiles, true)}
              />
            </Field>
            {mode === "inline" && (
              <p className={`text-xs tabular-nums ${used > bundleLimits.totalBytes ? "text-bad" : "text-fg-muted"}`}>
                {formatKiB(used)} of 512 KiB used by override and ancillary files.
              </p>
            )}
            {errors.form && <Notice tone="error">{errors.form}</Notice>}
            {save.isError && <Notice tone="error" title="Could not save Compose files">{errorMessage(save.error)}</Notice>}
          </div>
        </fieldset>
      </Section>
    </form>
  );
}

function formatKiB(bytes: number) {
  return `${(bytes / 1024).toFixed(bytes === 0 ? 0 : 1)} KiB`;
}

function PathInput({ id, label, value, error, onChange, placeholder }: { id: string; label: string; value: string; error?: string; onChange: (value: string) => void; placeholder: string }) {
  return (
    <Field id={id} label={label} error={error}>
      <input
        id={id}
        className={`${inputClass} font-mono`}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        placeholder={placeholder}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error)}
      />
    </Field>
  );
}

function ContentInput({ id, label, value, error, onChange, onFile }: { id: string; label: string; value: string; error?: string; onChange: (value: string) => void; onFile: (content: string, name: string) => void }) {
  const fileInput = useRef<HTMLInputElement>(null);
  const [fileError, setFileError] = useState("");
  const shown = fileError || error;

  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    if (file.size > bundleLimits.fileBytes) {
      setFileError("The file is larger than 1 MiB.");
      return;
    }
    try {
      const text = await file.text();
      if (text.includes("\0")) {
        setFileError("Choose a text file. This file contains NUL characters.");
        return;
      }
      setFileError("");
      onFile(text, file.name);
    } catch {
      setFileError("The file could not be read.");
    }
  }

  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <label htmlFor={id} className="text-[13px] font-medium text-fg">{label}</label>
        <Button size="sm" variant="ghost" onClick={() => fileInput.current?.click()}>
          <FileUp aria-hidden className="size-3.5" />
          Choose local file
        </Button>
        <input ref={fileInput} type="file" tabIndex={-1} aria-hidden className="hidden" onChange={(event) => void choose(event)} />
      </div>
      <textarea
        id={id}
        rows={8}
        value={value}
        onChange={(event) => {
          setFileError("");
          onChange(event.target.value);
        }}
        className={`${inputClass} h-auto py-2 font-mono text-xs`}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        data-1p-ignore
        data-lpignore="true"
        aria-invalid={shown ? true : undefined}
        aria-describedby={shown ? `${id}-error` : undefined}
      />
      {shown && <p id={`${id}-error`} className="text-xs text-bad">{shown}</p>}
    </div>
  );
}
