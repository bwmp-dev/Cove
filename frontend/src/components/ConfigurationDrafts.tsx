import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FileUp } from "lucide-react";
import { useId, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { applyConfigurationDraft, configurationDraftsQuery, configurationKeys, deleteConfigurationDraft, saveConfigurationDraft, type AppliedDraft, type ConfigurationDraft } from "../api/project-configuration";
import { keys } from "../api/queries";
import { countLabel } from "../lib/hierarchy";
import { draftState, manifestLimitBytes, newestFirst, type DraftState } from "../lib/project-configuration";
import { inputClass, linkClass } from "../lib/styles";
import { ChangeList } from "./ConfigurationChanges";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const offline = "Unavailable while offline.";

const stateLabels: Record<DraftState, { tone: "ok" | "warn" | "info"; label: string }> = {
  applied: { tone: "ok", label: "Applied" },
  stale: { tone: "warn", label: "Out of date" },
  ready: { tone: "info", label: "Ready to apply" },
};

interface Props {
  projectId: string;
  currentVersion: string | undefined;
  online: boolean;
  // Set when GitOps staged a draft, so it can be pointed out in the list.
  gitOpsDraft?: string;
}

export function ConfigurationDrafts({ projectId, currentVersion, online, gitOpsDraft }: Props) {
  const client = useQueryClient();
  const drafts = useQuery(configurationDraftsQuery(projectId));
  const [confirm, setConfirm] = useState<{ kind: "apply" | "delete"; draft: ConfigurationDraft } | null>(null);
  const [applied, setApplied] = useState<AppliedDraft | null>(null);
  const apply = useMutation({
    mutationFn: (draft: string) => applyConfigurationDraft(projectId, draft),
    onSuccess: (result) => {
      setApplied(result);
      setConfirm(null);
    },
    onSettled: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: configurationKeys.all(projectId) }),
        client.invalidateQueries({ queryKey: keys.workloads }),
        client.invalidateQueries({ queryKey: keys.projects }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]),
  });
  const remove = useMutation({
    mutationFn: (draft: string) => deleteConfigurationDraft(projectId, draft),
    onSuccess: () => setConfirm(null),
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.drafts(projectId) }),
  });

  function open(kind: "apply" | "delete", draft: ConfigurationDraft) {
    apply.reset();
    remove.reset();
    setApplied(null);
    setConfirm({ kind, draft });
  }

  const list = newestFirst(drafts.data ?? []);
  return (
    <Section title="Drafts" description="Paste or upload cove.yaml to save a draft. Saving changes nothing; review the changes, then apply the draft explicitly." flush>
      <div className="divide-y divide-line">
        <DraftComposer projectId={projectId} online={online} />
        <div className="min-w-0 space-y-3 p-4">
          {applied && <AppliedNotice applied={applied} />}
          {drafts.isPending ? (
            <LoadingRows label="Loading drafts" rows={2} />
          ) : drafts.isError ? (
            <QueryError what="drafts" error={drafts.error} onRetry={() => void drafts.refetch()} retrying={drafts.isFetching} />
          ) : list.length === 0 ? (
            <p className="text-[13px] text-fg-muted">No drafts. Saved drafts and drafts staged by a GitOps sync appear here.</p>
          ) : (
            <ul aria-label="Configuration drafts" className="space-y-4">
              {list.map((draft) => (
                <DraftRow key={draft.id} draft={draft} state={draftState(draft, currentVersion)} fromGitOps={draft.id === gitOpsDraft} online={online} busy={apply.isPending || remove.isPending} onApply={() => open("apply", draft)} onDelete={() => open("delete", draft)} />
              ))}
            </ul>
          )}
        </div>
      </div>
      <ConfirmDialog
        open={confirm?.kind === "apply"}
        title="Apply this draft?"
        confirmLabel="Apply draft"
        pending={apply.isPending}
        onCancel={() => setConfirm(null)}
        onConfirm={() => confirm && apply.mutate(confirm.draft.id)}
      >
        <div className="space-y-3">
          <p>All declaration changes in the draft are saved together. Applying does not deploy anything: changes take effect on each resource's next deploy. Changed domains on running resources queue route operations.</p>
          <p>Resources, environments and variables the draft leaves out are kept. Deployment policy changes also need team administration.</p>
          {apply.isError && (
            <Notice tone="error" title="Could not apply the draft">
              {errorMessage(apply.error)}
              {isStatus(apply.error, 409) && " If the configuration changed since this draft was saved, save the draft again from the current cove.yaml."}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
      <ConfirmDialog
        open={confirm?.kind === "delete"}
        title="Delete this draft?"
        confirmLabel="Delete draft"
        danger
        pending={remove.isPending}
        onCancel={() => setConfirm(null)}
        onConfirm={() => confirm && remove.mutate(confirm.draft.id)}
      >
        <div className="space-y-3">
          <p>The draft is removed. Saved configuration and resources are not changed.</p>
          {remove.isError && (
            <Notice tone="error" title="Could not delete the draft">
              {errorMessage(remove.error)}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
    </Section>
  );
}

function AppliedNotice({ applied }: { applied: AppliedDraft }) {
  const operations = applied.operation_ids ?? [];
  return (
    <Notice tone="success" title="Draft applied">
      <p>The declarations are saved. Nothing was deployed; deploy resources when you are ready.</p>
      {operations.length > 0 && (
        <p className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
          <span>Route changes queued:</span>
          {operations.map((operation, index) => (
            <Link key={operation} to="/operations/$operationId" params={{ operationId: operation }} className={linkClass}>
              Route operation {index + 1}
            </Link>
          ))}
        </p>
      )}
    </Notice>
  );
}

interface RowProps {
  draft: ConfigurationDraft;
  state: DraftState;
  fromGitOps: boolean;
  online: boolean;
  busy: boolean;
  onApply: () => void;
  onDelete: () => void;
}

function DraftRow({ draft, state, fromGitOps, online, busy, onApply, onDelete }: RowProps) {
  const changes = draft.changes ?? [];
  const operations = draft.operation_ids ?? [];
  const status = stateLabels[state];
  return (
    <li className="min-w-0 space-y-2.5 border-b border-line pb-4 last:border-b-0 last:pb-0">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 space-y-0.5">
          <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px]">
            <span className="font-medium text-fg">
              Saved <RelativeTime value={draft.created_at} />
            </span>
            <StatusLabel tone={status.tone} label={status.label} />
            {fromGitOps && <span className="text-xs text-fg-muted">Staged by GitOps sync</span>}
          </p>
          <p className="text-xs text-fg-muted">
            {countLabel(changes.length, "resource change")}
            {draft.applied_at && (
              <>
                {" · Applied "}
                <RelativeTime value={draft.applied_at} />
              </>
            )}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="ghost" onClick={onDelete} disabled={!online || busy} title={online ? undefined : offline}>
            Delete
          </Button>
          {state !== "applied" && (
            <Button size="sm" variant="primary" onClick={onApply} disabled={!online || busy || state === "stale"} title={!online ? offline : state === "stale" ? "Saved against an older configuration" : undefined}>
              Review and apply
            </Button>
          )}
        </div>
      </div>
      {state === "stale" && <p className="text-xs text-warn">The configuration changed after this draft was saved. Save the cove.yaml again to review it against the current configuration.</p>}
      {operations.length > 0 && (
        <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs">
          <span className="text-fg-muted">Route operations:</span>
          {operations.map((operation, index) => (
            <Link key={operation} to="/operations/$operationId" params={{ operationId: operation }} className={linkClass}>
              Operation {index + 1}
            </Link>
          ))}
        </p>
      )}
      {state !== "applied" && <ChangeList changes={changes} label="Changes in this draft" />}
    </li>
  );
}

function DraftComposer({ projectId, online }: { projectId: string; online: boolean }) {
  const id = useId();
  const client = useQueryClient();
  const fileInput = useRef<HTMLInputElement>(null);
  // The pasted cove.yaml can hold variable values and source tokens. It lives only here and is never a mutation variable.
  const [content, setContent] = useState("");
  const [inputError, setInputError] = useState("");
  const [saved, setSaved] = useState(false);
  const save = useMutation({
    gcTime: 0,
    mutationFn: () => saveConfigurationDraft(projectId, content),
    onSuccess: () => {
      setContent("");
      setSaved(true);
    },
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.drafts(projectId) }),
    networkMode: "always",
  });

  function edit(value: string) {
    save.reset();
    setSaved(false);
    setInputError("");
    setContent(value);
  }

  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    if (file.size > manifestLimitBytes) {
      setInputError("The file is larger than 2 MiB.");
      return;
    }
    try {
      const text = await file.text();
      edit(text);
    } catch {
      setInputError("The file could not be read.");
    }
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (content.trim() === "") {
      setInputError("Paste or upload a cove.yaml first.");
      return;
    }
    if (new Blob([content]).size > manifestLimitBytes) {
      setInputError("The configuration is larger than 2 MiB.");
      return;
    }
    save.mutate();
  }

  const shownError = inputError || undefined;
  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={content !== ""} />
      <Subsection
        title="New draft"
        description="YAML or JSON in the cove/v1 schema. Resources and environments you leave out are kept, and so are omitted variables, Compose files and domains of existing resources."
      >
        <div className="space-y-1.5">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <label htmlFor={`${id}-content`} className="text-[13px] font-medium text-fg">
              cove.yaml
            </label>
            <Button size="sm" variant="ghost" onClick={() => fileInput.current?.click()} disabled={save.isPending}>
              <FileUp aria-hidden className="size-3.5" />
              Upload file
            </Button>
            <input ref={fileInput} type="file" accept=".yaml,.yml,.json,application/json,text/yaml" tabIndex={-1} aria-hidden className="hidden" onChange={(event) => void choose(event)} />
          </div>
          <textarea
            id={`${id}-content`}
            rows={10}
            value={content}
            onChange={(event) => edit(event.target.value)}
            disabled={save.isPending}
            className={`${inputClass} h-auto py-2 font-mono text-xs`}
            placeholder={"api_version: cove/v1\nproject: shop"}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            data-1p-ignore
            data-lpignore="true"
            aria-invalid={shownError ? true : undefined}
            aria-describedby={shownError ? `${id}-error` : `${id}-hint`}
          />
          {shownError ? (
            <p id={`${id}-error`} className="text-xs text-bad">
              {shownError}
            </p>
          ) : (
            <p id={`${id}-hint`} className="text-xs text-fg-muted">
              Up to 2 MiB, 32 environments and 128 resources. Values you paste are sealed when the draft is saved and never shown again. Source tokens, deploy keys and infrastructure credentials are not portable between instances.
            </p>
          )}
        </div>
        <ul className="list-disc space-y-0.5 pl-5 text-xs text-fg-muted">
          <li>Moving an existing resource to another node or environment uses its Settings page, not a draft.</li>
          <li>Disconnect a repository connected through a Git provider before a draft changes it.</li>
          <li>Changing an SSH repository or its pinned host key needs a deploy key rotation.</li>
        </ul>
        {saved && (
          <Notice tone="success" title="Draft saved">
            Nothing changed yet. Review the draft below and apply it when ready.
          </Notice>
        )}
        {save.isError && (
          <Notice tone="error" title="Could not save the draft">
            {errorMessage(save.error)}
          </Notice>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          {content !== "" && (
            <Button variant="ghost" onClick={() => edit("")} disabled={save.isPending}>
              Clear
            </Button>
          )}
          <Button type="submit" variant="primary" pending={save.isPending} disabled={!online || content === ""} title={online ? undefined : offline}>
            Save draft
          </Button>
        </div>
      </Subsection>
    </form>
  );
}
