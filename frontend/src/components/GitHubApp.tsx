import { Checkbox } from "./Checkbox";
import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { request, errorMessage } from "../api/client";
import { GitHubTeamConnection, GitHubUserAuthorization, TeamInstallation } from "./GitHubConnection";
import type { OperationState, Workload } from "../api/types";
import { useSession } from "../lib/session";
import { canManageIntegrations, managesTeam } from "../lib/permissions";
import { useOnline } from "../lib/hooks";
import { inputClass, buttonClass, choiceClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { OperationStateLabel, StatusLabel } from "./status";
import { RelativeTime } from "./RelativeTime";
import { shortId } from "../lib/format";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows, Subsection, SwitchField } from "./ui";

interface Installation { id: number; account: string; team_ids: string[]; can_manage: boolean; shared_with_current_team: boolean }
export interface AppInfo { configured: boolean; user_auth_configured?: boolean; callback_url?: string; slug?: string; install_url?: string; installations?: Installation[] }
interface Repository { id: number; full_name: string; clone_url: string; default_branch: string; installation_id: number }
interface Connection { installation_id: number; repository_id: number; automatic: boolean }
interface ReportingRecord { operation_id: string; reported_state: OperationState | ""; error: string; next_attempt_at: string }
interface Reporting { enabled: boolean; reports: ReportingRecord[] }

function InstallationAssignments({ installations }: { installations: Installation[] }) {
  const session = useSession();
  const online = useOnline();
  const cache = useQueryClient();
  const [drafts, setDrafts] = useState<Record<number, string[]>>({});
  const [errors, setErrors] = useState<Record<number, string>>({});
  const teams = session?.teams.filter(team => session.is_admin || managesTeam(team) || team.grants.some(grant => grant.scope === "team" && grant.role === "editor")) ?? [];
  const isChanged = (item: Installation) => {
    const draft = drafts[item.id];
    return draft !== undefined && [...draft].sort().join(",") !== [...item.team_ids].sort().join(",");
  };
  const changed = installations.filter(isChanged);
  const save = useMutation({
    mutationFn: async () => {
      return Promise.all(changed.map(item => request("PUT", `/api/github/app/installations/${item.id}`, { team_ids: drafts[item.id] }).then(
        () => ({ id: item.id, error: null }),
        error => ({ id: item.id, error: errorMessage(error) }),
      )));
    },
    onSuccess: async results => {
      const failures: Record<number, string> = {};
      const succeeded = new Set<number>();
      for (const { id, error } of results) {
        if (error !== null) failures[id] = error;
        else succeeded.add(id);
      }
      setErrors(failures);
      setDrafts(current => Object.fromEntries(Object.entries(current).filter(([id]) => !succeeded.has(Number(id)))));
      await Promise.all([cache.invalidateQueries({ queryKey: ["github-app"] }), cache.invalidateQueries({ queryKey: ["github-repositories"] })]);
    },
    networkMode: "always",
  });
  return <form className="space-y-3" onSubmit={event => { event.preventDefault(); if (online && changed.length && !save.isPending) save.mutate(); }}>
    <UnsavedChangesGuard dirty={changed.length > 0} />
    {teams.length === 0 && <p className="text-xs text-fg-muted">Team-wide editing access is required to share these connections.</p>}
    <ul aria-label="Connected GitHub accounts" className="divide-y divide-line rounded-md border border-line">
      {installations.map(item => {
        const selectedTeams = new Set(drafts[item.id] ?? item.team_ids);
        return <li key={item.id} className="px-3 py-2.5">
        <fieldset className="grid min-w-0 gap-x-4 gap-y-2 sm:grid-cols-[minmax(0,10rem)_minmax(0,1fr)] sm:items-start" disabled={save.isPending} aria-describedby={errors[item.id] ? `github-sharing-${item.id}-error` : undefined}>
          <legend className="float-left min-w-0 py-1 text-[13px] font-medium break-words text-fg sm:py-2">{item.account}</legend>
          <div className="min-w-0 space-y-1.5">
            {teams.length > 0 && <div className="flex flex-wrap gap-1.5">
              {teams.map(team => <label key={team.id} className={choiceClass}>
                <Checkbox aria-label={`${item.account}: ${team.name}`} checked={selectedTeams.has(team.id)} onChange={event => {
                  const selected = drafts[item.id] ?? item.team_ids;
                  setDrafts(current => ({ ...current, [item.id]: event.target.checked ? [...selected, team.id] : selected.filter(id => id !== team.id) }));
                  setErrors(current => { const next = { ...current }; delete next[item.id]; return next; });
                  save.reset();
                }} /><span className="min-w-0 break-words">{team.name}</span>
              </label>)}
            </div>}
            {isChanged(item) && <p className="text-xs text-warn">Unsaved assignment</p>}
            {errors[item.id] && <p id={`github-sharing-${item.id}-error`} role="alert" className="text-xs text-bad">{errors[item.id]}</p>}
          </div>
        </fieldset>
      </li>; })}
    </ul>
    {Object.keys(errors).length > 0 && <Notice tone="error" title="Some assignments could not be saved">Successful assignments were saved. Correct or retry the remaining changes.</Notice>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="border-t border-line pt-4"><SaveBar dirty={changed.length > 0} pending={save.isPending} blocked={!online} onDiscard={() => { setDrafts({}); setErrors({}); save.reset(); }} status={save.isSuccess ? "Assignments saved" : undefined} /></div>
  </form>;
}

export function GitHubAppSetup() {
  const session = useSession();
  const app = useQuery({ queryKey: ["github-app"], queryFn: ({ signal }) => request<AppInfo>("GET", "/api/github/app", undefined, signal) });
  if (app.isPending) return <LoadingRows label="Loading GitHub App" rows={3} />;
  if (app.isError) return <div className="p-4"><QueryError what="the GitHub App" error={app.error} onRetry={() => void app.refetch()} retrying={app.isFetching} /></div>;
  if (app.data.configured) return <GitHubAppConnections app={app.data} />;
  return <Subsection title="Create the shared app" description="GitHub generates the credentials; Cove stores them encrypted on the dashboard.">
    {session?.is_admin ? <GitHubAppRegistration /> : <p className="text-[13px] text-fg-muted">An instance administrator must create the shared GitHub App once. You can then connect your own accounts independently.</p>}
  </Subsection>;
}

function GitHubAppConnections({ app }: { app: AppInfo }) {
  const session = useSession();
  const online = useOnline();
  const cache = useQueryClient();
  const sync = useMutation({ mutationFn: () => request<AppInfo>("POST", "/api/github/app/sync", {}), onSuccess: data => cache.setQueryData(["github-app"], data), networkMode: "always" });
  const managed = app.installations?.filter(i => i.can_manage) ?? [];
  const shared = app.installations?.filter(i => !i.can_manage && i.shared_with_current_team) ?? [];
  return <div className="divide-y divide-line">
    <Subsection
      title={<span className="font-mono">{app.slug}</span>}
      description="Install the app on your account or organization, then connect it below to verify ownership. Only selected repositories become available."
      actions={<>
        {app.install_url?.startsWith("https://github.com/apps/") && <a className={buttonClass()} href={app.install_url}>Install on another account</a>}
        {session?.is_admin && <Button disabled={!online} pending={sync.isPending} onClick={() => sync.mutate()}>Refresh installations</Button>}
      </>}
    >
      <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
        <span>User authorization</span>
        <StatusLabel tone={app.user_auth_configured ? "ok" : "warn"} label={app.user_auth_configured ? "Configured" : "Setup required"} />
      </p>
      {sync.isError && <Notice tone="error" title="Could not refresh installations">{errorMessage(sync.error)}</Notice>}
    </Subsection>
    <Subsection title="Connect an account">
      <GitHubTeamConnection configured={!!app.user_auth_configured} />
    </Subsection>
    {managed.length > 0 && <Subsection title="Team access" description="Choose which teams can deploy each connected account's repositories. Team access does not grant permission to share onward.">
      <InstallationAssignments installations={managed} />
    </Subsection>}
    {shared.length > 0 && <Subsection title="Shared with this team">
      <ul className="divide-y divide-line rounded-md border border-line">{shared.map(i => <TeamInstallation key={i.id} installation={i} />)}</ul>
    </Subsection>}
    {app.installations?.length === 0 && <Subsection><p className="text-[13px] text-fg-muted">No installations yet.</p></Subsection>}
    {session?.is_admin && <GitHubUserAuthorization app={app} />}
  </div>;
}

function GitHubAppRegistration() {
  const online = useOnline();
  const [name, setName] = useState("Cove Deployments");
  const [description, setDescription] = useState("Deploy selected GitHub repositories with Cove.");
  const [organization, setOrganization] = useState("");
  const setup = useMutation({ mutationFn: () => request<{ action: string; manifest: string }>("POST", "/api/github/app/setup", { name, description, organization }), networkMode: "always" });
  return <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2">
      <Field id="github-app-name" label="App name"><input id="github-app-name" className={inputClass} maxLength={34} value={name} onChange={e => { setName(e.target.value); setup.reset(); }} /></Field>
      <Field id="github-app-owner" label="Owning organization" optional hint="Leave empty to use your personal GitHub account."><input id="github-app-owner" className={inputClass} value={organization} onChange={e => { setOrganization(e.target.value); setup.reset(); }} aria-describedby="github-app-owner-hint" /></Field>
      </div>
      <Field id="github-app-description" label="Description"><input id="github-app-description" className={inputClass} maxLength={1000} value={description} onChange={e => { setDescription(e.target.value); setup.reset(); }} /></Field>
      <p className="text-xs text-fg-muted">Requests read access to repository contents, push and pull request events, and write access to commit statuses, deployments and pull requests so deployments can be reported back. The app can be installed on multiple accounts.</p>
      {setup.data && /^https:\/\/github\.com\/(settings\/apps\/new|organizations\/[a-zA-Z0-9-]+\/settings\/apps\/new)\?state=/.test(setup.data.action)
        ? <form method="post" action={setup.data.action}><input type="hidden" name="manifest" value={setup.data.manifest} /><Button type="submit" disabled={!online}>Continue to GitHub</Button></form>
        : <Button disabled={!online || !name.trim()} pending={setup.isPending} onClick={() => setup.mutate()}>Prepare GitHub App</Button>}
    {setup.isError && <Notice tone="error" title="Could not prepare GitHub App">{errorMessage(setup.error)}</Notice>}
  </div>;
}

function RepositoryForm({ workload, connection }: { workload: Workload; connection: Connection | null }) {
  const online = useOnline();
  const cache = useQueryClient();
  const repos = useQuery({ queryKey: ["github-repositories"], queryFn: ({ signal }) => request<Repository[]>("GET", "/api/github/repositories", undefined, signal) });
  const [repository, setRepository] = useState(connection ? `${connection.installation_id}:${connection.repository_id}` : "");
  const [branch, setBranch] = useState(workload.source?.branch ?? "main");
  const [composePath, setComposePath] = useState(workload.source?.compose_path ?? "compose.yaml");
  const [automatic, setAutomatic] = useState(connection?.automatic ?? true);
  const inlineCompose = workload.source?.inline_compose ?? false;
  const selected = repos.data?.find(r => `${r.installation_id}:${r.id}` === repository);
  const save = useMutation({
    mutationFn: () => request("PUT", `/api/workloads/${workload.id}/github`, { installation_id: selected?.installation_id, repository_id: selected?.id, branch, compose_path: inlineCompose ? "" : composePath, ...(inlineCompose ? { inline_compose: true } : {}), automatic }),
    onSuccess: () => cache.invalidateQueries(), networkMode: "always",
  });
  const disconnect = useMutation({ mutationFn: () => request("DELETE", `/api/workloads/${workload.id}/github`), onSuccess: () => cache.invalidateQueries(), networkMode: "always" });
  if (repos.isPending) return <p>Loading repositories</p>;
  if (repos.isError) return <Notice tone="error" title="Could not load repositories">{errorMessage(repos.error)} <Button onClick={() => void repos.refetch()}>Retry</Button></Notice>;
  return <div className="space-y-4">
    <p className="text-sm text-fg-muted">Connect a repository for Git-based deployments. Environment values, routes and private network connections stay in Cove. Saving changes the next deployment; it does not restart the resource.</p>
    <Field id="github-repository" label="Repository"><Select id="github-repository" className={inputClass} value={repository} onChange={e => {setRepository(e.target.value); const repo = repos.data.find(r => `${r.installation_id}:${r.id}` === e.target.value); if (repo) setBranch(repo.default_branch); }}><option value="">Select repository</option>{repos.data.map(r => <option key={`${r.installation_id}:${r.id}`} value={`${r.installation_id}:${r.id}`}>{r.full_name}</option>)}</Select></Field>
    {!repos.data.length && <p className="text-xs text-fg-muted">Connect your GitHub account or organization under Integrations to make repositories available to this team.</p>}
    <Field id="github-branch" label="Branch"><input id="github-branch" className={inputClass} value={branch} onChange={e => setBranch(e.target.value)} /></Field>
    {inlineCompose ? <p className="text-sm text-fg-muted">Uses the Compose configuration saved in Cove under Source.</p> : <Field id="github-compose" label="Compose path in repository"><input id="github-compose" className={inputClass} value={composePath} onChange={e => setComposePath(e.target.value)} /></Field>}
    <SwitchField id="github-automatic" checked={automatic} onChange={e => setAutomatic(e.target.checked)} label="Deploy pushes to this branch automatically" />
    <div className="flex gap-3"><Button disabled={!online || !selected || !branch || (!inlineCompose && !composePath)} pending={save.isPending} onClick={() => save.mutate()}>Save GitHub connection</Button>{connection && <Button disabled={!online} pending={disconnect.isPending} onClick={() => disconnect.mutate()}>Disconnect app</Button>}</div>
    {connection && <p className="text-xs text-fg-muted">Disconnecting stops app-triggered deployments and removes private repository authentication. Running containers remain online.</p>}
    {save.isSuccess && <p className="text-sm">GitHub connection saved.</p>}
    {(save.isError || disconnect.isError) && <Notice tone="error" title="Could not update GitHub connection">{errorMessage(save.error ?? disconnect.error)}</Notice>}
  </div>;
}

function ReportingHistory({ reports }: { reports: ReportingRecord[] }) {
  return <div className="space-y-2">
    <h3 className="text-[13px] font-semibold text-fg">Recent reports</h3>
    {reports.length === 0 ? <p className="text-xs text-fg-muted">No deployments reported yet. Reporting starts with the next deployment after it is enabled.</p> : <ReportingRows reports={reports} />}
  </div>;
}

function ReportingRows({ reports }: { reports: ReportingRecord[] }) {
  return <ul aria-label="Recent GitHub reports" className="divide-y divide-line rounded-md border border-line">
    {reports.map(report => <li key={report.operation_id} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1 px-3 py-2.5">
      <div className="min-w-0 space-y-0.5">
        <Link to="/operations/$operationId" params={{ operationId: report.operation_id }} title={report.operation_id} className="rounded text-[13px] text-fg hover:underline">Operation <span className="font-mono">{shortId(report.operation_id)}</span></Link>
        {report.error && <p className="text-xs break-words text-bad">{report.error}</p>}
        {report.error && <p className="text-xs text-fg-muted">Next attempt <RelativeTime value={report.next_attempt_at} /></p>}
      </div>
      {report.reported_state
        ? <span className="flex items-center gap-2"><span className="text-xs text-fg-muted">GitHub shows</span><OperationStateLabel state={report.reported_state} /></span>
        : <StatusLabel tone={report.error ? "bad" : "neutral"} label={report.error ? "Not reported" : "Not reported yet"} />}
    </li>)}
  </ul>;
}

const reportingDescription = "Report each deployment to GitHub as a commit status and a deployment record, and keep one comment updated on pull request previews.";

function ReportingStatus({ preview, enabled }: { preview: boolean; enabled: boolean }) {
  return <SettingRow label="Reporting" description={preview ? "Previews follow the setting of the resource they were created from." : "Team owners, admins and team-wide editors change this setting."}>
    {preview ? <span className="text-xs text-fg-muted">Set on the source resource</span> : <StatusLabel tone={enabled ? "ok" : "neutral"} label={enabled ? "Enabled" : "Disabled"} />}
  </SettingRow>;
}

function ReportingForm({ workload, reporting }: { workload: Workload; reporting: Reporting }) {
  const online = useOnline();
  const cache = useQueryClient();
  const [draft, setDraft] = useState<boolean>();
  const save = useMutation({
    mutationFn: (enabled: boolean) => request<Reporting>("PUT", `/api/workloads/${workload.id}/github/reporting`, { enabled }),
    onSuccess: data => { cache.setQueryData(["github-reporting", workload.id], data); setDraft(undefined); },
    networkMode: "always",
  });
  const enabled = draft ?? reporting.enabled;
  const dirty = enabled !== reporting.enabled;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (dirty && online && !save.isPending) save.mutate(enabled);
  };
  return <form onSubmit={submit}>
    <UnsavedChangesGuard dirty={dirty} />
    <Section width="form" title="Deployment reporting" description={reportingDescription} footer={<SaveBar dirty={dirty} pending={save.isPending} blocked={!online} onDiscard={() => { setDraft(undefined); save.reset(); }} status={save.isSuccess ? "Reporting setting saved" : undefined} />}>
      <div className="space-y-4">
        <SettingRows>
          <SwitchField id="github-reporting" layout="row" checked={enabled} disabled={!online || save.isPending} onChange={event => { setDraft(event.target.checked); save.reset(); }} label="Report deployments to GitHub" description="Applies to new deployments. GitHub App installations created before this option must approve write access to commit statuses, deployments and pull requests." />
        </SettingRows>
        {save.isError && <Notice tone="error" title="Could not change GitHub reporting">{errorMessage(save.error)}</Notice>}
        <ReportingHistory reports={reporting.reports} />
      </div>
    </Section>
  </form>;
}

function GitHubReporting({ workload }: { workload: Workload }) {
  const session = useSession();
  const reporting = useQuery({ queryKey: ["github-reporting", workload.id], queryFn: ({ signal }) => request<Reporting>("GET", `/api/workloads/${workload.id}/github/reporting`, undefined, signal) });
  const preview = !!workload.preview_source_id;
  if (reporting.isSuccess && canManageIntegrations(session) && !preview) return <ReportingForm workload={workload} reporting={reporting.data} />;
  return <Section width="form" title="Deployment reporting" description={reportingDescription}>
    {reporting.isPending ? <LoadingRows label="Loading GitHub reporting" rows={2} />
      : reporting.isError ? <QueryError what="GitHub reporting" error={reporting.error} onRetry={() => void reporting.refetch()} retrying={reporting.isFetching} />
      : <div className="space-y-4"><SettingRows><ReportingStatus preview={preview} enabled={reporting.data.enabled} /></SettingRows><ReportingHistory reports={reporting.data.reports} /></div>}
  </Section>;
}

export function GitHubSource({ workload }: { workload: Workload }) {
  const session = useSession();
  const connection = useQuery({ queryKey: ["github-source", workload.id], queryFn: ({ signal }) => request<Connection | null>("GET", `/api/workloads/${workload.id}/github`, undefined, signal) });
  return <>
    <Section width="form" title="GitHub App" description="Build from a private repository without personal access tokens.">
      {connection.isPending ? <p>Loading connection</p> : connection.isError ? <Notice tone="error" title="Could not load GitHub connection">{errorMessage(connection.error)}</Notice> : canManageIntegrations(session) ? <RepositoryForm key={`${connection.data?.repository_id}:${connection.data?.automatic}`} workload={workload} connection={connection.data} /> : <p className="text-sm text-fg-muted">{connection.data ? "Connected through the team's GitHub App." : "Not connected."} Team owners, admins and team-wide editors manage repository connections.</p>}
    </Section>
    {connection.data && <GitHubReporting workload={workload} />}
  </>;
}
