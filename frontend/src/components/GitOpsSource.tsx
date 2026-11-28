import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RefreshCw } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import type { Project } from "../api/projects";
import { configurationKeys, disconnectGitOps, gitOpsQuery, saveGitOps, syncGitOps, type GitOpsSource as Source, type GitOpsStatus, type GitOpsSync, type ProjectConfiguration } from "../api/project-configuration";
import type { WorkloadSummary } from "../api/types";
import { gitOpsCandidates, shortRevision } from "../lib/project-configuration";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { ChangeList } from "./ConfigurationChanges";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

const title = "GitOps";
const description = "Keep this project's configuration in cove.yaml in one of its Git repositories. Syncing stages the repository's changes as a draft; it never deploys applications.";
const offline = "Unavailable while offline.";

interface Props {
  project: Project;
  resources: readonly WorkloadSummary[];
  configuration: UseQueryResult<ProjectConfiguration>;
  editable: boolean;
  online: boolean;
  // Automatic apply runs under an individual account with a verified email; shared administrator sessions have none.
  individualAccount: boolean;
}

export function GitOpsSource(props: Props) {
  const status = useQuery(gitOpsQuery(props.project.id));
  const [editing, setEditing] = useState(false);
  if (status.isPending) {
    return (
      <Section title={title} description={description} flush>
        <LoadingRows label="Loading GitOps settings" rows={2} />
      </Section>
    );
  }
  if (status.isError) {
    return (
      <Section title={title} description={description}>
        <QueryError what="GitOps settings" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
      </Section>
    );
  }
  const view = status.data;
  if (props.editable && (!view.connected || editing)) {
    return <GitOpsForm {...props} current={view.connected ? view.source : null} onDone={() => setEditing(false)} />;
  }
  if (!view.connected) {
    return (
      <Section title={title} description={description}>
        <p className="text-[13px] text-fg-muted">Not connected. An editor of this project can choose a repository to sync configuration from.</p>
      </Section>
    );
  }
  return <GitOpsStatusView {...props} view={view} onEdit={() => setEditing(true)} />;
}

function GitOpsStatusView({ project, resources, editable, online, view, onEdit }: Props & { view: Extract<GitOpsStatus, { connected: true }>; onEdit: () => void }) {
  const client = useQueryClient();
  const [confirm, setConfirm] = useState(false);
  const [result, setResult] = useState<GitOpsSync | null>(null);
  const sync = useMutation({
    mutationFn: () => syncGitOps(project.id),
    onSuccess: setResult,
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.all(project.id) }),
  });
  const disconnect = useMutation({
    mutationFn: () => disconnectGitOps(project.id),
    onSuccess: () => {
      setConfirm(false);
      setResult(null);
    },
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.gitops(project.id) }),
  });
  const source = resources.find((resource) => resource.id === view.source.workload_id);
  const drift = view.drift ?? [];

  return (
    <Section
      title={title}
      description={description}
      actions={
        editable && (
          <Button
            onClick={() => {
              setResult(null);
              sync.mutate();
            }}
            pending={sync.isPending}
            disabled={!online}
            title={online ? undefined : offline}
          >
            {!sync.isPending && <RefreshCw aria-hidden className="size-3.5" />}
            Sync cove.yaml
          </Button>
        )
      }
    >
      <div className="space-y-4">
        <DefinitionList
          compact
          items={[
            {
              term: "Source",
              value: source ? (
                <Link to="/resources/$resourceId" params={{ resourceId: source.id }} className={linkClass}>
                  {source.name}
                </Link>
              ) : (
                <span className="text-fg-muted">Resource you cannot view</span>
              ),
            },
            { term: "Path", value: <span className="font-mono text-xs break-all">{view.source.path}</span> },
            {
              term: "Apply",
              value: view.source.automatic ? <StatusLabel tone="info" label="Automatic, checked every minute" /> : <StatusLabel tone="neutral" label="Manual review" />,
            },
            {
              term: "Last revision",
              value: view.last_revision ? (
                <span className="font-mono text-xs" title={view.last_revision}>
                  {shortRevision(view.last_revision)}
                </span>
              ) : (
                "Not synced yet"
              ),
            },
            { term: "Last sync", value: view.synced_at ? <RelativeTime value={view.synced_at} /> : "Never" },
            { term: "Pending draft", value: view.draft_id ? "A staged draft is waiting in Drafts" : "None" },
          ]}
        />
        {view.last_error && (
          <Notice tone="error" title="The last sync failed">
            {view.last_error}
          </Notice>
        )}
        {view.source.automatic && (
          <p className="text-xs text-fg-muted">Automatic apply uses the current access of the account that saved this setting and stops if that account loses access or is disabled. It can overwrite configuration edited manually since the last sync.</p>
        )}
        {drift.length > 0 && (
          <div className="space-y-2">
            <h3 className="text-[13px] font-semibold text-fg">Differences from cove.yaml</h3>
            <p className="text-xs text-fg-muted">Saved configuration that no longer matches the repository at the last revision.</p>
            <ChangeList changes={drift} label="Differences from cove.yaml" />
          </div>
        )}
        {result && <SyncResult result={result} automatic={view.source.automatic} />}
        {sync.isError && (
          <Notice tone="error" title="Could not sync cove.yaml">
            {errorMessage(sync.error)}
          </Notice>
        )}
        {editable && (
          <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-4">
            <Button
              variant="ghost"
              onClick={() => {
                disconnect.reset();
                setConfirm(true);
              }}
              disabled={!online}
            >
              Disconnect
            </Button>
            <Button onClick={onEdit} disabled={!online}>
              Edit settings
            </Button>
          </div>
        )}
      </div>
      <ConfirmDialog open={confirm} title="Disconnect GitOps?" confirmLabel="Disconnect" danger pending={disconnect.isPending} onCancel={() => setConfirm(false)} onConfirm={() => disconnect.mutate()}>
        <div className="space-y-3">
          <p>Cove stops reading cove.yaml from the repository. Saved configuration, drafts and resources stay as they are.</p>
          {disconnect.isError && (
            <Notice tone="error" title="Could not disconnect">
              {errorMessage(disconnect.error)}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
    </Section>
  );
}

function SyncResult({ result, automatic }: { result: GitOpsSync; automatic: boolean }) {
  const revision = <span className="font-mono text-xs">{shortRevision(result.revision)}</span>;
  if (result.state === "unchanged") {
    return (
      <Notice tone="success" title="Already up to date">
        cove.yaml at {revision} matches the saved configuration.
      </Notice>
    );
  }
  return (
    <div className="space-y-2">
      {result.state === "saved" ? (
        <Notice tone="success" title="Configuration applied">
          Automatic apply saved the changes from {revision}. Nothing was deployed.
        </Notice>
      ) : (
        <Notice tone="info" title="Draft staged">
          The changes from {revision} wait in Drafts. {automatic ? "They were not applied automatically." : "Review and apply them there."}
        </Notice>
      )}
      <ChangeList changes={result.changes} label="Changes from cove.yaml" />
    </div>
  );
}

function GitOpsForm({ project, resources, configuration, online, individualAccount, current, onDone }: Props & { current: Source | null; onDone: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const saved = current ? resources.find((resource) => resource.id === current.workload_id) : undefined;
  // Without the manifest's resource IDs there is no reliable way to tell which resources have a Git source.
  const offered = configuration.data ? gitOpsCandidates(project, resources, configuration.data) : [];
  const candidates = configuration.data && saved && !offered.includes(saved) ? [saved, ...offered] : offered;
  const [workload, setWorkload] = useState(current?.workload_id ?? "");
  const [path, setPath] = useState(current?.path ?? "cove.yaml");
  const [automatic, setAutomatic] = useState(current?.automatic ?? false);
  const [error, setError] = useState("");
  const save = useMutation({
    mutationFn: (source: Source) => saveGitOps(project.id, source),
    onSuccess: (view) => {
      client.setQueryData(configurationKeys.gitops(project.id), view);
      onDone();
    },
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workload) {
      setError("Choose the resource whose repository holds cove.yaml.");
      return;
    }
    setError("");
    save.mutate({ workload_id: workload, path: path.trim() || "cove.yaml", automatic });
  }

  const unchanged = current !== null && current.workload_id === workload && current.path === (path.trim() || "cove.yaml") && current.automatic === automatic;
  return (
    <form onSubmit={submit} noValidate>
      <Section
        title={title}
        description={description}
        footer={
          <div className="flex flex-wrap justify-end gap-2">
            {current && (
              <Button variant="ghost" onClick={onDone} disabled={save.isPending}>
                Cancel
              </Button>
            )}
            <Button type="submit" variant="primary" pending={save.isPending} disabled={!online || unchanged || candidates.length === 0} title={online ? undefined : offline}>
              {current ? "Save settings" : "Connect"}
            </Button>
          </div>
        }
      >
        <fieldset disabled={save.isPending} className="min-w-0 space-y-4">
          {configuration.isPending ? (
            <p role="status" className="text-[13px] text-fg-muted">
              Loading the resources with a Git source…
            </p>
          ) : configuration.isError ? (
            <QueryError what="the resources with a Git source" error={configuration.error} onRetry={() => void configuration.refetch()} retrying={configuration.isFetching} />
          ) : candidates.length === 0 ? (
            <Notice tone="warn" title="No Git resource to sync from">
              Connect a Git repository to a resource in this project first. Preview resources cannot be used.
            </Notice>
          ) : (
            <div className="grid gap-4 sm:grid-cols-2">
              <Field id={`${id}-workload`} label="Repository of" error={error || undefined} hint="A resource in this project with a Git source. Cove reads cove.yaml from its branch.">
                <Select id={`${id}-workload`} value={workload} onChange={(event) => setWorkload(event.target.value)} aria-invalid={error ? true : undefined} aria-describedby={describedBy(`${id}-workload`, error, true)}>
                  <option value="">Choose a resource</option>
                  {candidates.map((resource) => (
                    <option key={resource.id} value={resource.id}>
                      {resource.environment ? `${resource.name} (${resource.environment})` : resource.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field id={`${id}-path`} label="Path in repository" hint="Relative to the repository root.">
                <input id={`${id}-path`} className={`${inputClass} font-mono`} value={path} onChange={(event) => setPath(event.target.value)} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-describedby={`${id}-path-hint`} />
              </Field>
            </div>
          )}
          <SwitchField
            id={`${id}-automatic`}
            label="Apply changes automatically"
            checked={automatic}
            disabled={!individualAccount && !automatic}
            onChange={(event) => setAutomatic(event.target.checked)}
            description={
              individualAccount
                ? "Off: each sync stages a draft that someone applies. On: Cove checks every minute and applies configuration changes with your current access. Applications are never deployed, but changed domains queue route operations. Automatic apply can overwrite later manual edits."
                : "Needs an individual account with a verified email. Shared administrator sessions can only stage drafts."
            }
          />
          {save.isError && (
            <Notice tone="error" title="Could not save GitOps settings">
              {errorMessage(save.error)}
            </Notice>
          )}
        </fieldset>
      </Section>
    </form>
  );
}
