import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, request } from "../api/client";
import { automaticDescription, emptySelection, gitConnectionsQuery, providerLabels, reportingDescription, type ProviderSelection, providerSourceQuery, saveProviderSource, type ProviderSource, type ProviderSourceInput } from "../api/git-providers";
import { githubConnectionQuery, keys } from "../api/queries";
import type { Workload } from "../api/types";
import { canManageIntegrations } from "../lib/permissions";
import { useSession } from "../lib/session";
import { describedBy, inputClass } from "../lib/styles";
import { validateComposePath } from "../lib/workload-form";
import { ConfirmDialog } from "./ConfirmDialog";
import { ReportingHistory } from "./GitHubApp";
import { GitProviderPicker } from "./GitProviderPicker";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, SettingRows, SwitchField } from "./ui";

const title = "Git provider";
const description = "Build from a GitLab, Gitea, Forgejo, Codeberg or Bitbucket Cloud repository through a team connection. Pushes and pull or merge requests arrive through the connection's webhook.";

function useConnection(connectionId: number | undefined) {
  const connections = useQuery({ ...gitConnectionsQuery, enabled: connectionId !== undefined });
  return connections.data?.find((item) => item.id === connectionId);
}

function ProviderSummary({ workload, source, note }: { workload: Workload; source: ProviderSource; note: ReactNode }) {
  const connection = useConnection(source.connection_id);
  const kind = connection ? providerLabels[connection.kind] : "the provider";
  return (
    <div className="space-y-4">
      <p className="text-[13px] text-fg-muted">{note}</p>
      <DefinitionList
        items={[
          { term: "Connection", value: connection ? `${connection.name} (${kind})` : `Connection ${source.connection_id}` },
          { term: "Repository", value: <span className="font-mono text-xs break-all">{workload.source?.repository ?? "Not saved"}</span> },
          { term: "Branch", value: <span className="font-mono text-xs">{workload.source?.branch}</span> },
          { term: "Automatic deployments", value: <StatusLabel tone={source.automatic ? "ok" : "neutral"} label={source.automatic ? "On" : "Off"} /> },
          { term: "Reporting", value: <StatusLabel tone={source.reporting ? "ok" : "neutral"} label={source.reporting ? "On" : "Off"} /> },
        ]}
      />
      {source.reporting && <ReportingHistory reports={source.reports} provider={kind} />}
    </div>
  );
}

// Saving or disconnecting rewrites the source and clears the plain webhook secret.
function refreshSource(client: QueryClient, workloadId: string) {
  return Promise.all([client.invalidateQueries({ queryKey: keys.workload(workloadId) }), client.invalidateQueries({ queryKey: ["webhook", workloadId] })]);
}

function ConnectedActions({ workload, source, blocked }: { workload: Workload; source: ProviderSource; blocked: boolean }) {
  const client = useQueryClient();
  const connection = useConnection(source.connection_id);
  const [confirming, setConfirming] = useState(false);
  const disconnect = useMutation({ mutationFn: () => request<{ ok: boolean }>("DELETE", `/api/workloads/${encodeURIComponent(workload.id)}/git-provider`), onSuccess: () => setConfirming(false), onSettled: () => refreshSource(client, workload.id), networkMode: "always" });
  return (
    <div className="space-y-4 border-t border-line pt-4">
      {source.reporting && <ReportingHistory reports={source.reports} provider={connection ? providerLabels[connection.kind] : "the provider"} />}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <Button variant="danger" disabled={blocked} onClick={() => { disconnect.reset(); setConfirming(true); }}>Disconnect provider</Button>
        <p className="text-xs text-fg-muted">The source stays saved, but webhook deployments, reporting and provider authentication stop.</p>
      </div>
      <ConfirmDialog open={confirming} title="Disconnect Git provider?" confirmLabel="Disconnect" danger pending={disconnect.isPending} onCancel={() => setConfirming(false)} onConfirm={() => disconnect.mutate()}>
        <div className="space-y-3">
          <p>Webhook deployments and reporting stop, and queued webhook deliveries are discarded. The repository, branch and Compose path stay saved, but private repositories fail to clone until another credential is configured. Running containers are not affected.</p>
          {disconnect.isError && <Notice tone="error" title="Could not disconnect">{errorMessage(disconnect.error)}</Notice>}
        </div>
      </ConfirmDialog>
    </div>
  );
}

interface Draft {
  selection: ProviderSelection;
  composePath: string;
  automatic: boolean;
  reporting: boolean;
}

function savedDraft(workload: Workload, source: ProviderSource | null): Draft {
  return {
    selection: source ? { connectionId: source.connection_id, repositoryId: source.repository_id, branch: workload.source?.branch ?? "" } : emptySelection,
    composePath: workload.source?.compose_path || "compose.yaml",
    automatic: source?.automatic ?? true,
    reporting: source?.reporting ?? false,
  };
}

function changed(draft: Draft, saved: Draft, inlineCompose: boolean) {
  const { selection: a } = draft;
  const { selection: b } = saved;
  return a.connectionId !== b.connectionId || a.repositoryId !== b.repositoryId || a.branch !== b.branch || (!inlineCompose && draft.composePath !== saved.composePath) || draft.automatic !== saved.automatic || draft.reporting !== saved.reporting;
}

function ProviderForm({ workload, source, blocked, blockers }: { workload: Workload; source: ProviderSource | null; blocked: boolean; blockers: ReactNode }) {
  const id = useId();
  const client = useQueryClient();
  const inlineCompose = Boolean(workload.source?.inline_compose);
  const saved = savedDraft(workload, source);
  const [draft, setDraft] = useState(saved);
  const [pathError, setPathError] = useState<string>();
  const { selection } = draft;
  const save = useMutation({
    mutationFn: (input: ProviderSourceInput) => saveProviderSource(workload.id, input),
    onSettled: () => refreshSource(client, workload.id),
    networkMode: "always",
  });
  const complete = selection.connectionId !== null && selection.repositoryId !== null && selection.branch !== "";
  const dirty = changed(draft, saved, inlineCompose);
  const update = (patch: Partial<Draft>) => {
    save.reset();
    setDraft((current) => ({ ...current, ...patch }));
  };

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const composePath = inlineCompose ? "" : draft.composePath.trim();
    const error = inlineCompose ? null : validateComposePath(composePath);
    setPathError(error ?? undefined);
    if (error || selection.connectionId === null || selection.repositoryId === null || !selection.branch) return;
    save.mutate({ connection_id: selection.connectionId, repository_id: selection.repositoryId, branch: selection.branch, compose_path: composePath, ...(inlineCompose ? { inline_compose: true } : {}), automatic: draft.automatic, reporting: draft.reporting });
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title={title}
        description={description}
        footer={<SaveBar dirty={dirty} pending={save.isPending} blocked={blocked || !complete} label={source ? "Save changes" : "Connect repository"} onDiscard={() => { setDraft(saved); setPathError(undefined); save.reset(); }} status={save.isSuccess ? "Saved. Changes apply on the next deploy." : undefined} />}
      >
        <fieldset disabled={save.isPending} className="min-w-0 space-y-5">
          {blockers}
          <GitProviderPicker value={selection} onChange={(next) => update({ selection: next })} />
          {inlineCompose ? (
            <p className="text-[13px] text-fg-muted">Uses the Compose configuration saved in Cove above.</p>
          ) : (
            <Field id={`${id}-compose`} label="Compose path in repository" error={pathError}>
              <input id={`${id}-compose`} className={`${inputClass} font-mono`} value={draft.composePath} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => update({ composePath: event.target.value })} aria-invalid={pathError ? true : undefined} aria-describedby={describedBy(`${id}-compose`, pathError)} />
            </Field>
          )}
          <SettingRows>
            <SwitchField layout="row" id={`${id}-automatic`} checked={draft.automatic} onChange={(event) => update({ automatic: event.target.checked })} label="Deploy pushes automatically" description={automaticDescription} />
            <SwitchField layout="row" id={`${id}-reporting`} checked={draft.reporting} onChange={(event) => update({ reporting: event.target.checked })} label="Report deployments" description={reportingDescription} />
          </SettingRows>
          <p className="text-xs text-fg-muted">Saving replaces the source repository, branch and Compose path, deletes a saved access token and turns off the plain repository webhook. Environment values, routes and private network connections stay in Cove. Running containers are not restarted.</p>
          {save.isError && <Notice tone="error" title="Could not save the Git provider source">{errorMessage(save.error)}</Notice>}
          {source && <ConnectedActions workload={workload} source={source} blocked={blocked} />}
        </fieldset>
      </Section>
    </form>
  );
}

export function GitProviderSource({ workload, editable, temporary, blocked, blockers }: { workload: Workload; editable: boolean; temporary: boolean; blocked: boolean; blockers: ReactNode }) {
  const session = useSession();
  const source = useQuery(providerSourceQuery(workload.id));
  const github = useQuery(githubConnectionQuery(workload.id));

  if (source.isPending || github.isPending) {
    return <Section width="form" title={title} description={description}><LoadingRows label="Loading Git provider source" rows={2} /></Section>;
  }
  if (source.isError) {
    return <Section width="form" title={title} description={description}><QueryError what="the Git provider source" error={source.error} onRetry={() => void source.refetch()} retrying={source.isFetching} /></Section>;
  }
  if (temporary || !editable || !canManageIntegrations(session)) {
    if (!source.data) return null;
    const note = temporary ? "Previews use the repository of the resource they were created from and cannot change it." : editable ? "Team owners, admins and team-wide editors manage repository connections." : "Changing the connection needs editor access to this resource.";
    return (
      <Section width="form" title={title} description={description}>
        <ProviderSummary workload={workload} source={source.data} note={note} />
      </Section>
    );
  }
  if (github.data) {
    return (
      <Section width="form" title={title} description={description}>
        <Notice tone="warn" title="Connected through the GitHub App">Disconnect the GitHub App below before connecting another Git provider.</Notice>
      </Section>
    );
  }
  return <ProviderForm key={`${source.data?.connection_id}:${source.data?.repository_id}:${source.data?.automatic}:${source.data?.reporting}:${workload.source?.branch}`} workload={workload} source={source.data} blocked={blocked} blockers={blockers} />;
}
