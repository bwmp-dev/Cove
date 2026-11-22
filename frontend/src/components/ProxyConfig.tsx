import { useQuery } from "@tanstack/react-query";
import { FileCheck2, Import, Trash2, X } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { ApiError, errorMessage, isStatus } from "../api/client";
import { proxyFileQuery, useProxyChange, type ProxyChange, type ProxyConfig, type ProxyFile } from "../api/proxy";
import { formatBytes } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { changeStates, changeTitle, fileStatuses, objectStatuses, proxyFileLimit, validateProxyFileName } from "../lib/proxy-config";
import { describedBy, inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { byteLength } from "../lib/workload-form";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, Notice, QueryError, Section } from "./ui";

function MessageList({ items }: { items?: string[] }) {
  if (!items?.length) return null;
  return (
    <ul className="mt-1 list-disc space-y-1 pl-4">
      {items.map((item) => (
        <li key={item} className="break-words">
          {item}
        </li>
      ))}
    </ul>
  );
}

export function ChangeOutcome({ change }: { change: ProxyChange }) {
  const info = changeStates[change.state];
  return (
    <Notice tone={info?.tone ?? "info"} title={changeTitle(change.action, change.name, change.state)}>
      {change.message && <p>{change.message}</p>}
      {change.problems && change.problems.length > 0 && (
        <div className="mt-2">
          <p className="font-medium text-fg">Problems</p>
          <MessageList items={change.problems} />
        </div>
      )}
      {change.warnings && change.warnings.length > 0 && (
        <div className="mt-2">
          <p className="font-medium text-fg">Warnings</p>
          <MessageList items={change.warnings} />
        </div>
      )}
    </Notice>
  );
}

export function LastChange({ change }: { change?: ProxyChange }) {
  if (!change) return null;
  return (
    <p className="text-xs text-fg-muted">
      Last change: {changeTitle(change.action, change.name, change.state)}
      {change.finished_at && (
        <>
          {" · "}
          <RelativeTime value={change.finished_at} />
        </>
      )}
    </p>
  );
}

export function ProxyFiles({ files, selected, onSelect }: { files: ProxyFile[]; selected: string | null; onSelect: (name: string) => void }) {
  if (files.length === 0) {
    return <p className="px-4 py-3 text-[13px] text-fg-muted">The dynamic directory has no files yet.</p>;
  }
  return (
    <table className="w-full text-left">
      <caption className="sr-only">Dynamic configuration files</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="py-2 pr-4 pl-4 font-medium">
            File
          </th>
          <th scope="col" className="py-2 pr-4 font-medium">
            Status
          </th>
          <th scope="col" className="hidden py-2 pr-4 font-medium sm:table-cell">
            Size
          </th>
          <th scope="col" className="py-2 pr-4 text-right font-medium">
            <span className="sr-only">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {files.map((file) => {
          const info = fileStatuses[file.status];
          const action = file.status === "managed" ? "Edit" : file.status === "unmanaged" ? "Review" : "View";
          return (
            <tr key={file.name} className={tableRowClass} aria-current={selected === file.name ? "true" : undefined}>
              <td className="py-2.5 pr-4 pl-4 align-top">
                <span className="font-mono text-xs break-all text-fg">{file.name}</span>
                {file.problem && <p className="mt-0.5 text-xs text-fg-muted">{file.problem}</p>}
              </td>
              <td className="py-2.5 pr-4 align-top" title={info?.description}>
                <StatusLabel tone={info?.tone ?? "neutral"} label={info?.label ?? file.status} />
                {file.modified && <p className="mt-0.5 text-xs text-warn">Changed outside Cove</p>}
              </td>
              <td className="hidden py-2.5 pr-4 align-top text-xs text-fg-muted sm:table-cell">{file.status === "missing" ? "—" : formatBytes(file.size)}</td>
              <td className="py-2 pr-4 text-right align-top">
                {file.status !== "missing" && (
                  <Button size="sm" variant="ghost" onClick={() => onSelect(file.name)} aria-label={`${action} ${file.name}`}>
                    {action}
                  </Button>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export function StaticSettings({ config }: { config: ProxyConfig }) {
  return (
    <DefinitionList
      items={[
        { term: "Proxy image", value: <span className="font-mono text-xs">{config.image}</span> },
        { term: "State", value: config.running ? "Running" : "Stopped" },
        {
          term: "Published ports",
          value: config.ports.length ? (
            <span className="font-mono text-xs">{config.ports.join(", ")}</span>
          ) : (
            <span className="text-fg-muted">None. A staged proxy receives traffic through the migration bridge.</span>
          ),
        },
        {
          term: "Entry points",
          value: config.entrypoints.length ? (
            <ul className="space-y-0.5">
              {config.entrypoints.map((entry) => (
                <li key={entry.name}>
                  <span className="font-mono text-xs">{entry.name}</span> <span className="font-mono text-xs text-fg-muted">{entry.address}</span>
                  {entry.name === "traefik" && <span className="text-xs text-fg-muted"> · management API, local to the proxy</span>}
                </li>
              ))}
            </ul>
          ) : (
            <span className="text-fg-muted">Not available</span>
          ),
        },
        {
          term: "Certificate resolvers",
          value: config.resolvers.length ? <span className="font-mono text-xs">{config.resolvers.join(", ")}</span> : <span className="text-fg-muted">Not available</span>,
        },
      ]}
    />
  );
}

function FileObjects({ file }: { file: ProxyFile }) {
  if (!file.objects?.length) return null;
  return (
    <div className="mt-4">
      <h3 className="text-[13px] font-medium text-fg">Objects in the live proxy</h3>
      <p className="mt-0.5 text-xs text-fg-muted">TLS settings and servers transports are not reported by Traefik's API.</p>
      <ul className="mt-2 divide-y divide-line rounded-md border border-line">
        {file.objects.map((object) => {
          const info = objectStatuses[object.status];
          return (
            <li key={`${object.kind}/${object.name}`} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1 px-3 py-2 text-[13px]">
              <div className="min-w-0">
                <span className="font-mono text-xs break-all text-fg">{object.name}</span>
                <span className="ml-2 text-xs text-fg-muted">{object.kind}</span>
                {object.errors?.map((error) => (
                  <p key={error} className="mt-0.5 text-xs break-words text-bad">
                    {error}
                  </p>
                ))}
              </div>
              <StatusLabel tone={info?.tone ?? "neutral"} label={info?.label ?? object.status} />
            </li>
          );
        })}
      </ul>
    </div>
  );
}

interface EditorProps {
  nodeId: string;
  // Omitted for a new file.
  name?: string;
  existing: string[];
  blocked?: string;
  onClose: (opened?: string) => void;
}

// The mutation lives here so its outcome stays visible when the form remounts for a new revision.
export function ProxyFileEditor({ nodeId, name, existing, blocked, onClose }: EditorProps) {
  const change = useProxyChange(nodeId);
  if (name === undefined) return <NewFileForm existing={existing} blocked={blocked} change={change} onClose={onClose} />;
  return <ExistingFile nodeId={nodeId} name={name} blocked={blocked} change={change} onClose={onClose} />;
}

function ExistingFile({ nodeId, name, blocked, change, onClose }: { nodeId: string; name: string; blocked?: string; change: Change; onClose: () => void }) {
  const file = useQuery(proxyFileQuery(nodeId, name));
  if (file.isPending) {
    return (
      <Section title={name}>
        <p role="status" className="text-[13px] text-fg-muted">
          Loading file
        </p>
      </Section>
    );
  }
  if (file.isError) {
    return (
      <Section title={name} actions={<CloseButton onClose={onClose} />}>
        <QueryError what="the file" error={file.error} onRetry={() => void file.refetch()} retrying={file.isFetching} />
      </Section>
    );
  }
  return <ExistingFileForm key={file.data.revision ?? "unreadable"} file={file.data} blocked={blocked} change={change} onClose={onClose} onReload={() => void file.refetch()} />;
}

function CloseButton({ onClose }: { onClose: () => void }) {
  return (
    <Button variant="ghost" onClick={onClose}>
      <X aria-hidden className="size-3.5" />
      Close
    </Button>
  );
}

type Change = ReturnType<typeof useProxyChange>;

function useDraft(original: string, change: Change, blocked?: string) {
  const online = useOnline();
  const [content, setContent] = useState(original);
  const tooLarge = byteLength(content) > proxyFileLimit;
  return {
    content,
    setContent,
    tooLarge,
    unavailable: !online || change.isPending || !!blocked,
    canCheck: online && !tooLarge && content !== "",
  };
}

function ExistingFileForm({ file, blocked, change, onClose, onReload }: { file: ProxyFile; blocked?: string; change: Change; onClose: () => void; onReload: () => void }) {
  const original = file.content ?? "";
  const draft = useDraft(original, change, blocked);
  const editable = file.status === "managed";
  const dirty = editable && draft.content !== original;

  function apply(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    change.mutate({ action: "put", name: file.name, content: draft.content, revision: file.revision ?? "" }, { onSuccess: (result) => result.state === "succeeded" && onReload() });
  }

  return (
    <Section title={file.name} description={fileStatuses[file.status]?.description} actions={<CloseButton onClose={onClose} />}>
      <UnsavedChangesGuard dirty={dirty} />
      <form onSubmit={apply} className="space-y-4" aria-label={`Edit ${file.name}`}>
        <FileNotices file={file} change={change} disabled={draft.unavailable} onReload={onReload} />
        {file.content !== undefined && <ContentField value={draft.content} editable={editable} onChange={draft.setContent} />}
        <ChangeFeedback change={change} onReload={onReload} />
        {editable && (
          <EditorActions
            change={change}
            dirty={dirty}
            canApply={dirty && !draft.tooLarge && !draft.unavailable}
            canCheck={draft.canCheck}
            onCheck={() => change.mutate({ action: "validate", name: file.name, content: draft.content })}
            onDiscard={() => draft.setContent(original)}
          >
            <RemoveFile file={file} change={change} disabled={draft.unavailable || dirty} onRemoved={onClose} />
          </EditorActions>
        )}
      </form>
      <FileObjects file={file} />
    </Section>
  );
}

function NewFileForm({ existing, blocked, change, onClose }: { existing: string[]; blocked?: string; change: Change; onClose: (opened?: string) => void }) {
  const draft = useDraft("", change, blocked);
  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<string | null>(null);
  const dirty = draft.content !== "" || name !== "";

  function target() {
    const trimmed = name.trim();
    const error = validateProxyFileName(trimmed, existing);
    setNameError(error);
    return error ? null : trimmed;
  }

  function apply(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const targetName = target();
    if (!targetName) return;
    change.mutate({ action: "put", name: targetName, content: draft.content, revision: "" }, { onSuccess: (result) => result.state === "succeeded" && onClose(targetName) });
  }

  function check() {
    const targetName = target();
    if (targetName) change.mutate({ action: "validate", name: targetName, content: draft.content });
  }

  return (
    <Section title="New file" description="Traefik dynamic configuration in YAML: http, tcp, udp and tls sections." actions={<CloseButton onClose={() => onClose()} />}>
      <UnsavedChangesGuard dirty={dirty} />
      <form onSubmit={apply} className="space-y-4" aria-label="Edit New file">
        <NameField
          value={name}
          error={nameError}
          onChange={(value) => {
            setName(value);
            setNameError(null);
          }}
        />
        <ContentField value={draft.content} editable onChange={draft.setContent} />
        <ChangeFeedback change={change} onReload={() => undefined} />
        <EditorActions change={change} dirty={dirty} canApply={dirty && !draft.tooLarge && !draft.unavailable} canCheck={draft.canCheck} onCheck={check} onDiscard={() => onClose()} />
      </form>
    </Section>
  );
}

function FileNotices({ file, change, disabled, onReload }: { file: ProxyFile; change: Change; disabled: boolean; onReload: () => void }) {
  const revision = file.revision;
  return (
    <>
      {file.modified && (
        <Notice tone="warn" title="Changed outside Cove">
          The file no longer matches the version Cove last applied. Saving replaces it with the contents below.
        </Notice>
      )}
      {file.status === "unmanaged" && (
        <Notice
          tone="warn"
          title="Not imported"
          action={
            <Button variant="primary" pending={change.isPending} disabled={disabled || !revision} onClick={() => revision && change.mutate({ action: "import", name: file.name, revision }, { onSuccess: (result) => result.state === "succeeded" && onReload() })}>
              {!change.isPending && <Import aria-hidden className="size-3.5" />}
              Import file
            </Button>
          }
        >
          The proxy already loads this file. Importing records it as managed without changing it, so Cove can then validate, apply and roll back edits.
        </Notice>
      )}
      {file.problem && <Notice tone="warn">{file.problem}</Notice>}
      {file.warnings && file.warnings.length > 0 && (
        <Notice tone="warn" title="Review before editing">
          <MessageList items={file.warnings} />
        </Notice>
      )}
    </>
  );
}

function errorFeedback(error: unknown) {
  if (error instanceof ApiError && (error.isNetwork || error.status === 502)) {
    return { conflict: false, title: "No reply from the worker", text: "The worker may still finish this change. The status above reloads automatically; check it before retrying." };
  }
  if (isStatus(error, 409) && errorMessage(error).startsWith("The file changed")) {
    return { conflict: true, title: "The file changed", text: "Someone changed this file after you opened it. Reloading replaces your edits with the current version; copy them first if you need them." };
  }
  return { conflict: false, title: "The change was not applied", text: errorMessage(error) };
}

function ChangeFeedback({ change, onReload }: { change: Change; onReload: () => void }) {
  const failure = change.isError ? errorFeedback(change.error) : null;
  return (
    <div aria-live="polite" className="space-y-2">
      {change.data && <ChangeOutcome change={change.data} />}
      {failure && (
        <Notice
          tone="error"
          title={failure.title}
          action={
            failure.conflict && (
              <Button variant="secondary" onClick={onReload}>
                Reload file
              </Button>
            )
          }
        >
          {failure.text}
        </Notice>
      )}
    </div>
  );
}

function RemoveFile({ file, change, disabled, onRemoved }: { file: ProxyFile; change: Change; disabled: boolean; onRemoved: () => void }) {
  const [confirming, setConfirming] = useState(false);
  const revision = file.revision;
  return (
    <>
      <Button variant="danger" className="sm:ml-auto" onClick={() => setConfirming(true)} disabled={disabled || !revision}>
        <Trash2 aria-hidden className="size-3.5" />
        Remove file
      </Button>
      <ConfirmDialog
        open={confirming}
        title={`Remove ${file.name}?`}
        confirmLabel="Remove file"
        danger
        pending={change.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() =>
          revision &&
          change.mutate(
            { action: "delete", name: file.name, revision },
            {
              onSettled: () => setConfirming(false),
              onSuccess: (result) => result.state === "succeeded" && onRemoved(),
            },
          )
        }
      >
        Traefik unloads this file without restarting, and routes defined only here stop working for every team on this node. A copy is kept in the proxy volume. Cove first checks that no other file depends on it.
      </ConfirmDialog>
    </>
  );
}

interface EditorActionsProps {
  change: Change;
  dirty: boolean;
  canApply: boolean;
  canCheck: boolean;
  onCheck: () => void;
  onDiscard: () => void;
  children?: ReactNode;
}

function EditorActions({ change, dirty, canApply, canCheck, onCheck, onDiscard, children }: EditorActionsProps) {
  const pendingAction = change.isPending ? change.variables?.action : undefined;
  return (
    <div className="flex flex-wrap items-center gap-2 border-t border-line pt-4">
      <Button type="submit" variant="primary" pending={pendingAction === "put"} disabled={!canApply}>
        Validate and apply
      </Button>
      <Button onClick={onCheck} pending={pendingAction === "validate"} disabled={!canCheck || change.isPending}>
        {pendingAction !== "validate" && <FileCheck2 aria-hidden className="size-3.5" />}
        Check only
      </Button>
      <Button variant="ghost" onClick={onDiscard} disabled={!dirty || change.isPending}>
        Discard changes
      </Button>
      {children}
      <p role="status" className="basis-full text-xs text-fg-muted">
        {change.isPending ? "Validating with Traefik and waiting for the live proxy. This can take up to a minute." : dirty ? "Unsaved changes" : ""}
      </p>
    </div>
  );
}

function NameField({ value, error, onChange }: { value: string; error: string | null; onChange: (value: string) => void }) {
  const id = useId();
  return (
    <Field id={id} label="File name" error={error ?? undefined} hint="Stored in the proxy's dynamic directory, for example legacy-routes.yaml.">
      <input
        id={id}
        className={`${inputClass} max-w-sm font-mono`}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        autoComplete="off"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error ?? undefined, true)}
      />
    </Field>
  );
}

const contentExample = "http:\n  routers:\n    example:\n      rule: Host(`app.example.com`)\n      entryPoints: [websecure]\n      service: example\n      tls:\n        certResolver: cove\n  services:\n    example:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.10:8080";

function ContentField({ value, editable, onChange }: { value: string; editable: boolean; onChange: (value: string) => void }) {
  const id = useId();
  const bytes = byteLength(value);
  const error = bytes > proxyFileLimit ? `The file must not exceed ${formatBytes(proxyFileLimit)}.` : undefined;
  const hint = editable ? `${formatBytes(bytes)} of ${formatBytes(proxyFileLimit)}. Templates are not supported.` : "Read-only.";
  return (
    <Field id={id} label="Configuration" error={error} hint={hint}>
      <textarea
        id={id}
        className={`${inputClass} min-h-96 resize-y font-mono text-xs leading-relaxed`}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        readOnly={!editable}
        spellCheck={false}
        autoCapitalize="none"
        autoComplete="off"
        wrap="off"
        placeholder={contentExample}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
      />
    </Field>
  );
}
