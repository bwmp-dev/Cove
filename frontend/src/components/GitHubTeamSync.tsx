import { queryOptions, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useRouterState } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { useGitHubStart } from "../api/auth";
import { errorMessage, isStatus, request } from "../api/client";
import {
  directoryKeys,
  directoryTeamsQuery,
  githubTeamSourcesQuery,
  useApplyGithubTeamSource,
  useDeleteGithubTeamSource,
  usePreviewGithubTeamSource,
  useSaveGithubTeamSource,
  type DirectoryChange,
  type DirectoryTeam,
  type GithubTeamReport,
  type GithubTeamSource,
} from "../api/directory";
import { accountSignInMethodsQuery } from "../api/security";
import {
  applyFailure,
  changeKind,
  githubSourceInput,
  githubSourceState,
  hasMappingErrors,
  installationMismatch,
  memberStateLabel,
  plural,
  previewIsCurrent,
  summarizeReport,
  teamName,
  toMappingRows,
  validateGithubSource,
  validateMappings,
  type ApplyFailure,
  type GithubSourceDraft,
  type GithubSourceErrors,
  type MappingRow,
  type ReportSummary,
} from "../lib/directory";
import { useOnline, useReviewed } from "../lib/hooks";
import { reauthErrorMessage } from "../lib/reauth";
import { useSession } from "../lib/session";
import { choiceClass, describedBy, inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { ReauthFields, useReauth } from "./AccountSecurity";
import { ConfirmDialog } from "./ConfirmDialog";
import { MappingEditor, MappingList } from "./DirectoryMappings";
import type { AppInfo } from "./GitHubApp";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, MetaList, Notice, QueryError, Section, SwitchField } from "./ui";

const maxSources = 8;

type Installation = NonNullable<AppInfo["installations"]>[number];

// Shares the GitHub App settings cache entry used by the Integrations page.
const githubAppQuery = queryOptions({ queryKey: ["github-app"], queryFn: ({ signal }) => request<AppInfo>("GET", "/api/github/app", undefined, signal) });

function summaryText(summary: ReportSummary) {
  const parts = [];
  if (summary.added) parts.push(`${summary.added} added`);
  if (summary.removed) parts.push(`${summary.removed} removed`);
  if (summary.changed) parts.push(`${summary.changed} changed`);
  return parts.length > 0 ? parts.join(" · ") : "No changes";
}

function InstallationField({ id, draft, installations, pending, error, onChange }: { id: string; draft: GithubSourceDraft; installations: Installation[]; pending: boolean; error?: string; onChange: (draft: GithubSourceDraft) => void }) {
  return (
    <Field id={id} label="Installation" hint="The Cove GitHub App must be installed on this organization with read access to Members." error={error}>
      <Select
        id={id}
        className={`${inputClass} sm:max-w-sm`}
        value={draft.installation_id}
        disabled={pending || installations.length === 0}
        onChange={(event) => {
          const installation = installations.find((item) => String(item.id) === event.target.value);
          onChange({ ...draft, installation_id: event.target.value, account: draft.account.trim() === "" && installation ? installation.account : draft.account });
        }}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
      >
        <option value="" disabled>
          {pending ? "Loading installations" : installations.length === 0 ? "No installations connected" : "Choose an installation"}
        </option>
        {installations.map((installation) => (
          <option key={installation.id} value={String(installation.id)}>
            {installation.account}
          </option>
        ))}
      </Select>
    </Field>
  );
}

function OAuthModeNotes({ linked, takesOwnership }: { linked: boolean; takesOwnership: boolean }) {
  return (
    <div className="space-y-2">
      <p className="text-[13px] text-fg-muted">
        Cove reads the organization's teams with your linked GitHub account. After saving, authorize organization read access from this list; GitHub may ask an organization owner to approve the request or SSO.
      </p>
      {!linked && (
        <Notice tone="warn" title="Your GitHub account is not linked">
          Link GitHub to your Cove account from the Account page first. Saving an OAuth source is refused until it is linked.
        </Notice>
      )}
      {takesOwnership && (
        <Notice tone="warn" title="You will become the connection owner">
          Saving replaces the current owner's authorization with yours. Sync stops until you authorize organization access with your GitHub account.
        </Notice>
      )}
    </div>
  );
}

function SourceEditor({ source, onSaved, onClose }: { source?: GithubTeamSource; onSaved: () => void; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const session = useSession();
  const save = useSaveGithubTeamSource();
  const app = useQuery(githubAppQuery);
  const installations = app.data?.installations ?? [];
  const [draft, setDraft] = useState<GithubSourceDraft>(() => ({
    name: source?.configuration.name ?? "",
    mode: source?.configuration.mode ?? "app",
    account: source?.configuration.account ?? "",
    installation_id: source?.configuration.installation_id ? String(source.configuration.installation_id) : "",
    enabled: source?.enabled ?? true,
  }));
  const [rows, setRows] = useState<MappingRow[]>(() => toMappingRows(source?.mappings));
  // The revision being edited; a save after someone else's change is refused instead of overwriting it.
  const baseVersion = useReviewed(source?.version);
  const [errors, setErrors] = useState<GithubSourceErrors>({});
  const [mappingErrors, setMappingErrors] = useState<ReturnType<typeof validateMappings>>({ rows: {} });
  const takesOwnership = source?.configuration.mode === "oauth" && draft.mode === "oauth" && source.configuration.oauth_user_id !== session?.user_id;
  const selectedInstallation = installations.find((item) => String(item.id) === draft.installation_id);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problems = validateGithubSource(draft);
    const mismatch = installationMismatch(draft, selectedInstallation);
    if (mismatch) problems.installation_id = mismatch;
    const mappingProblems = validateMappings(rows, "github");
    setErrors(problems);
    setMappingErrors(mappingProblems);
    if (Object.keys(problems).length > 0 || hasMappingErrors(mappingProblems)) return;
    save.mutate({ id: source?.id, input: githubSourceInput(draft, rows, baseVersion) }, { onSuccess: onSaved });
  }

  const stale = isStatus(save.error, 409);
  return (
    <form className="space-y-4 border-t border-line bg-panel p-4" noValidate onSubmit={submit} aria-label={source ? `Edit ${source.configuration.name}` : "New GitHub team sync source"}>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-name`} label="Name" hint="Shown in this list and in the audit log." error={errors.name}>
          <input
            id={`${id}-name`}
            className={inputClass}
            autoComplete="off"
            value={draft.name}
            onChange={(event) => setDraft({ ...draft, name: event.target.value })}
            aria-invalid={errors.name ? true : undefined}
            aria-describedby={describedBy(`${id}-name`, errors.name, true)}
          />
        </Field>
        <Field id={`${id}-account`} label="GitHub organization" hint="The organization's login, as in github.com/<organization>." error={errors.account}>
          <input
            id={`${id}-account`}
            className={`${inputClass} font-mono`}
            autoComplete="off"
            spellCheck={false}
            placeholder="my-org"
            value={draft.account}
            onChange={(event) => setDraft({ ...draft, account: event.target.value })}
            aria-invalid={errors.account ? true : undefined}
            aria-describedby={describedBy(`${id}-account`, errors.account, true)}
          />
        </Field>
      </div>
      <fieldset className="space-y-2">
        <legend className="mb-1.5 text-[13px] font-medium text-fg">Read team membership with</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <label className={choiceClass}>
            <input type="radio" name={`${id}-mode`} value="app" checked={draft.mode === "app"} onChange={() => setDraft({ ...draft, mode: "app" })} className="accent-info" />
            The GitHub App installation
          </label>
          <label className={choiceClass}>
            <input type="radio" name={`${id}-mode`} value="oauth" checked={draft.mode === "oauth"} onChange={() => setDraft({ ...draft, mode: "oauth" })} className="accent-info" />
            Your GitHub account (OAuth)
          </label>
        </div>
      </fieldset>
      {draft.mode === "app" ? (
        <InstallationField
          id={`${id}-installation`}
          draft={draft}
          installations={installations}
          pending={app.isPending}
          error={errors.installation_id ?? (app.isError ? `Installations could not be loaded: ${errorMessage(app.error)}` : undefined)}
          onChange={setDraft}
        />
      ) : (
        <OAuthModeNotes linked={!session || session.github_linked} takesOwnership={takesOwnership} />
      )}
      <SwitchField
        id={`${id}-enabled`}
        label="Refresh automatically"
        description="Every 15 minutes and when a linked GitHub user signs in. Turned off, membership changes only when you preview and apply them here."
        checked={draft.enabled}
        onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })}
      />
      <div className="space-y-2">
        <h4 className="text-[13px] font-semibold text-fg">Team mappings</h4>
        <p className="text-xs text-fg-muted">Members of each GitHub team join the Cove team with this role. People are matched by their linked GitHub account, never by email, and no accounts are created.</p>
        <MappingEditor id={`${id}-mappings`} kind="github" rows={rows} onChange={setRows} errors={mappingErrors.rows} listError={mappingErrors.list} />
      </div>
      {save.isError && (
        <Notice tone="error" title={stale ? "This source changed or the request was refused" : "Could not save the source"}>
          {errorMessage(save.error)}
          {stale && source && " Cancel, review the current settings and edit again."}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>
          {source ? "Save source" : "Add source"}
        </Button>
      </div>
    </form>
  );
}

function OAuthAuthorization({ source }: { source: GithubTeamSource }) {
  const id = useId();
  const online = useOnline();
  const session = useSession();
  const factors = useQuery(accountSignInMethodsQuery);
  const start = useGitHubStart();
  const reauth = useReauth(factors.data ?? { totp_enrolled: false, recovery_codes_remaining: 0 });
  const [open, setOpen] = useState(false);
  const owner = source.configuration.oauth_user_id === session?.user_id;
  if (!owner) {
    return (
      <p className="text-xs text-fg-muted">
        Only the administrator who configured this OAuth source can authorize it. Editing the source as yourself makes you the owner, after which you authorize it.
      </p>
    );
  }
  if (!open) {
    return (
      <Button size="sm" disabled={!online} onClick={() => setOpen(true)}>
        {source.credential_configured ? "Reauthorize organization access" : "Authorize organization access"}
        <span className="sr-only"> for {source.configuration.name}</span>
      </Button>
    );
  }
  return (
    <form
      className="space-y-3 border-t border-line pt-3"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const proof = reauth.proof();
        if (proof) start.mutate({ link: true, ...proof }, { onSettled: reauth.clear });
      }}
    >
      <p className="text-[13px] text-fg-muted">
        GitHub asks you to grant Cove read access to organization membership (read:org). Approve access for <span className="font-mono text-fg">{source.configuration.account}</span>, including SSO if the organization requires it. GitHub then sends you back to this section.
      </p>
      <ReauthFields id={id} reauth={reauth} />
      {start.isError && <Notice tone="error" title="Could not start GitHub authorization">{reauthErrorMessage(start.error, reauth.method)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={() => { setOpen(false); start.reset(); reauth.clear(); }} disabled={start.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={start.isPending || start.isSuccess}>
          Continue to GitHub
        </Button>
      </div>
    </form>
  );
}

// Older responses carry only the user ID, which is then the last-resort label.
function Person({ change }: { change: DirectoryChange }) {
  const name = change.user_name?.trim();
  const email = change.email?.trim();
  if (!name && !email) return <span className="font-mono text-xs break-all">{change.user_id}</span>;
  return (
    <>
      <span className="block">{name || email}</span>
      {name && email && <span className="block text-xs break-all text-fg-muted">{email}</span>}
    </>
  );
}

const kindLabels = { added: "Joins", removed: "Loses access", changed: "Role changes" } as const;

function ChangeTable({ changes, teams }: { changes: DirectoryChange[]; teams: readonly DirectoryTeam[] | undefined }) {
  return (
    <table className="w-full min-w-[36rem] text-left text-[13px]">
      <caption className="sr-only">Membership changes</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="px-4">Person</th>
          <th scope="col" className="px-4">Team</th>
          <th scope="col" className="px-4">Change</th>
          <th scope="col" className="px-4">Before</th>
          <th scope="col" className="px-4">After</th>
        </tr>
      </thead>
      <tbody>
        {changes.map((change) => {
          const kind = changeKind(change);
          return (
            <tr key={`${change.team_id}:${change.user_id}`} className={tableRowClass}>
              <td className="px-4 py-2 align-top break-words text-fg">
                <Person change={change} />
                {change.warning && <p className="mt-0.5 text-xs text-warn">{change.warning}</p>}
              </td>
              <td className="px-4 py-2 align-top break-words text-fg-muted">{teamName(teams, change.team_id) ?? <span className="font-mono text-xs">{change.team_id}</span>}</td>
              <td className="px-4 py-2 align-top">
                <StatusLabel tone={kind === "removed" ? "bad" : kind === "added" ? "ok" : "info"} label={kindLabels[kind]} />
              </td>
              <td className="px-4 py-2 align-top text-fg-muted">{change.before ? memberStateLabel(change.before) : "Not a member"}</td>
              <td className="px-4 py-2 align-top text-fg-muted">{change.after ? memberStateLabel(change.after) : "Not a member"}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function ReportDetails({ report }: { report: GithubTeamReport }) {
  const teams = useQuery(directoryTeamsQuery);
  const changes = report.changes ?? [];
  return (
    <div className="space-y-3">
      {changes.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No membership changes. Cove already matches the GitHub teams this source maps.</p>
      ) : (
        <div className="overflow-x-auto rounded-md border border-line">
          <ChangeTable changes={changes} teams={teams.data} />
        </div>
      )}
      {report.unmatched_github_users > 0 && (
        <p className="text-xs text-fg-muted">
          {plural(report.unmatched_github_users, "GitHub team member")} {report.unmatched_github_users === 1 ? "has" : "have"} no linked Cove account and {report.unmatched_github_users === 1 ? "is" : "are"} skipped. They are matched after they link GitHub to their own Cove account.
        </p>
      )}
    </div>
  );
}

function ApplyDialog({ source, report, pending, onConfirm, onCancel }: { source: GithubTeamSource; report: GithubTeamReport; pending: boolean; onConfirm: (reviewed: GithubTeamReport) => void; onCancel: () => void }) {
  // Pinned when the dialog opens, so a background refresh cannot change what is confirmed or sent.
  const reviewed = useReviewed({ report, name: source.configuration.name });
  const summary = summarizeReport(reviewed.report);
  return (
    <ConfirmDialog
      open
      title={`Apply changes from ${reviewed.name}?`}
      confirmLabel={summary.removed > 0 ? `Apply and remove ${plural(summary.removed, "membership")}` : "Apply changes"}
      danger={summary.removed > 0}
      pending={pending}
      onCancel={onCancel}
      onConfirm={() => onConfirm(reviewed.report)}
    >
      <p>{summaryText(summary)}.</p>
      {summary.removed > 0 && (
        <p className="mt-2">
          {plural(summary.removed, "person", "people")} lose the access this source granted. Access granted by hand or by other sources is kept, and a team's last owner stays as a local owner.
        </p>
      )}
      <p className="mt-2">Cove fetches the roster again and applies only if it still matches this preview exactly. If anything changed, nothing is applied and you preview again.</p>
    </ConfirmDialog>
  );
}

const failureMessages: Record<ApplyFailure, { title: string; body: string }> = {
  stale: { title: "Preview out of date", body: "The source settings or the GitHub roster changed, or a refresh was running. Nothing was changed. Preview again to review the current changes." },
  github: { title: "GitHub unavailable", body: "GitHub could not be reached or the connection lacks Members permission. Nothing was changed and existing access was kept." },
  unavailable: { title: "Cove could not apply the changes", body: "The control plane is temporarily unavailable. Nothing was changed. Try again shortly." },
  other: { title: "Could not apply the changes", body: "" },
};

function PreviewFailureNotice({ error }: { error: unknown }) {
  const failure = applyFailure(error);
  return (
    <Notice tone="error" title={failure === "github" ? "GitHub unavailable" : failure === "stale" ? "Source changed" : "Could not preview"}>
      {failure === "github" ? failureMessages.github.body : errorMessage(error)}
    </Notice>
  );
}

function ApplyFailureNotice({ error }: { error: unknown }) {
  const failure = applyFailure(error);
  return (
    <Notice tone={failure === "stale" ? "warn" : "error"} title={failureMessages[failure].title}>
      {failure === "other" ? errorMessage(error) : failureMessages[failure].body}
    </Notice>
  );
}

function ReviewedReport({ report, current, blocked, pending, onApply }: { report: GithubTeamReport; current: boolean; blocked: boolean; pending: boolean; onApply: () => void }) {
  const summary = summarizeReport(report);
  const changes = (report.changes ?? []).length;
  return (
    <div className="space-y-3">
      <p className="text-[13px] text-fg" role="status">
        {summaryText(summary)}
        {summary.warnings > 0 && <span className="text-warn"> · {plural(summary.warnings, "warning")}</span>}
      </p>
      {!current && (
        <Notice tone="warn" title="Preview out of date">
          This source was saved or refreshed after the preview was taken. Preview again before applying.
        </Notice>
      )}
      <ReportDetails report={report} />
      {changes > 0 && (
        <div className="flex justify-end">
          <Button variant={summary.removed > 0 ? "danger" : "primary"} disabled={blocked || !current} pending={pending} onClick={onApply}>
            Apply {plural(changes, "change")}
          </Button>
        </div>
      )}
    </div>
  );
}

function SourcePreview({ source, onClose }: { source: GithubTeamSource; onClose: () => void }) {
  const online = useOnline();
  const preview = usePreviewGithubTeamSource();
  const apply = useApplyGithubTeamSource();
  const [confirming, setConfirming] = useState(false);
  const report = preview.data;
  const current = report ? previewIsCurrent(report, source) : false;
  const failure = apply.isError ? applyFailure(apply.error) : null;

  function runPreview() {
    apply.reset();
    preview.mutate({ id: source.id, version: source.version });
  }

  // Opening the panel is the request to preview, so the first preview starts once, after the first render.
  const started = useRef(false);
  const { mutate } = preview;
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    mutate({ id: source.id, version: source.version });
  }, [mutate, source.id, source.version]);

  return (
    <div className="space-y-3 border-t border-line bg-panel p-4">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-64">
          <h4 className="text-[13px] font-semibold text-fg">Preview changes</h4>
          <p className="mt-0.5 text-xs text-fg-muted">Reads every mapped team from GitHub now and shows exactly what applying would change. Previewing never changes membership.</p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="ghost" onClick={onClose} disabled={preview.isPending || apply.isPending}>
            Close preview
          </Button>
          <Button onClick={runPreview} disabled={!online || apply.isPending} pending={preview.isPending}>
            {report ? "Preview again" : "Preview"}
          </Button>
        </div>
      </div>
      {preview.isError && <PreviewFailureNotice error={preview.error} />}
      {apply.isSuccess && (
        <Notice tone="success" title="Changes applied">
          {summaryText(summarizeReport(apply.data))}. Team member lists now show the updated access.
        </Notice>
      )}
      {apply.isError && <ApplyFailureNotice error={apply.error} />}
      {report && !apply.isSuccess && failure !== "stale" && (
        <ReviewedReport report={report} current={current} blocked={!online} pending={apply.isPending} onApply={() => setConfirming(true)} />
      )}
      {confirming && report && (
        <ApplyDialog
          source={source}
          report={report}
          pending={apply.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={(reviewed) =>
            apply.mutate(
              { id: source.id, version: reviewed.version, roster_hash: reviewed.roster_hash },
              {
                onSettled: () => setConfirming(false),
                onError: (error) => {
                  if (applyFailure(error) === "stale") preview.reset();
                },
              },
            )
          }
        />
      )}
    </div>
  );
}

function DeleteDialog({ source, pending, onConfirm, onCancel }: { source: GithubTeamSource; pending: boolean; onConfirm: (version: number) => void; onCancel: () => void }) {
  const reviewed = useReviewed({ version: source.version, name: source.configuration.name, mappings: (source.mappings ?? []).length });
  return (
    <ConfirmDialog open title={`Remove ${reviewed.name}?`} confirmLabel="Remove source and its access" danger pending={pending} onCancel={onCancel} onConfirm={() => onConfirm(reviewed.version)}>
      <p>
        Cove removes the team access this source granted through its {plural(reviewed.mappings, "mapping")} and stops reading these GitHub teams.
      </p>
      <p className="mt-2">Access granted by hand or by other sources is kept, and a team's last owner stays as a local owner. Nothing changes in GitHub.</p>
    </ConfirmDialog>
  );
}

function SourceSummary({ source }: { source: GithubTeamSource }) {
  const state = githubSourceState(source);
  const { configuration } = source;
  return (
    <div className="min-w-0 flex-1 basis-72 space-y-1">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <p className="text-[13px] font-medium break-words text-fg">{configuration.name}</p>
        <StatusLabel tone={state.tone} label={state.label} />
      </div>
      <MetaList className="text-xs text-fg-muted">
        <span className="font-mono break-all">{configuration.account}</span>
        <span>{configuration.mode === "app" ? "GitHub App installation" : "OAuth with an administrator's GitHub account"}</span>
        <span>{source.enabled ? "Refreshes automatically" : "Automatic refresh paused"}</span>
      </MetaList>
      <MetaList className="text-xs text-fg-muted">
        <span>{source.checked_at ? <>Checked <RelativeTime value={source.checked_at} /></> : "Not checked yet"}</span>
        <span>{source.applied_at ? <>Last applied <RelativeTime value={source.applied_at} /></> : "No changes applied yet"}</span>
        {source.enabled && source.credential_configured && <span>Next check <RelativeTime value={source.next_check_at} /></span>}
      </MetaList>
      <div className="pt-1">
        <MappingList mappings={source.mappings ?? []} />
      </div>
    </div>
  );
}

function SourceNotices({ source, removeError }: { source: GithubTeamSource; removeError: unknown }) {
  return (
    <div className="space-y-2 px-4 pb-3">
      {source.last_error !== "" && (
        <Notice tone="error" title="The last refresh failed">
          {source.last_error} Existing access was kept: Cove never removes members while GitHub cannot be read completely.
        </Notice>
      )}
      {source.configuration.mode === "oauth" && <OAuthAuthorization source={source} />}
      {removeError !== null && (
        <Notice tone="error" title="Could not remove the source">
          {errorMessage(removeError)}
          {isStatus(removeError, 409) && " Review the source again before removing it."}
        </Notice>
      )}
    </div>
  );
}

function SourceRow({ source, editing, previewing, onEdit, onPreview, onClose, onSaved }: { source: GithubTeamSource; editing: boolean; previewing: boolean; onEdit: () => void; onPreview: () => void; onClose: () => void; onSaved: () => void }) {
  const online = useOnline();
  const remove = useDeleteGithubTeamSource();
  const [deleting, setDeleting] = useState(false);
  const { configuration } = source;
  return (
    <li>
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-3">
        <SourceSummary source={source} />
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="ghost" size="sm" disabled={!online || previewing} onClick={onPreview}>
            Preview changes<span className="sr-only"> for {configuration.name}</span>
          </Button>
          <Button variant="ghost" size="sm" disabled={!online || editing} onClick={onEdit}>
            Edit<span className="sr-only"> {configuration.name}</span>
          </Button>
          <Button variant="ghost" size="sm" disabled={!online} pending={remove.isPending} onClick={() => { remove.reset(); setDeleting(true); }}>
            Remove<span className="sr-only"> {configuration.name}</span>
          </Button>
        </div>
      </div>
      {(source.last_error !== "" || configuration.mode === "oauth" || remove.isError) && <SourceNotices source={source} removeError={remove.isError ? remove.error : null} />}
      {previewing && <SourcePreview key={source.id} source={source} onClose={onClose} />}
      {editing && <SourceEditor source={source} onSaved={onSaved} onClose={onClose} />}
      {deleting && <DeleteDialog source={source} pending={remove.isPending} onCancel={() => setDeleting(false)} onConfirm={(version) => remove.mutate({ id: source.id, version }, { onSettled: () => setDeleting(false) })} />}
    </li>
  );
}

const githubTeamSyncAnchor = "github-team-sync";

const consentOutcomes = {
  connected: { tone: "success", title: "GitHub organization access authorized", body: "Cove can now read this organization's teams. Preview a source to review the changes before applying them." },
  permission_required: {
    tone: "warn",
    title: "Organization access was not granted",
    body: "GitHub did not grant read access to organization membership. Approve Cove for the organization, including SSO if it is required, and authorize again.",
  },
  error: { tone: "error", title: "GitHub authorization did not finish", body: "Nothing was saved and existing access is unchanged. Authorize organization access again." },
} as const;

type ConsentOutcome = keyof typeof consentOutcomes;

function consentOutcome(search: string): ConsentOutcome | null {
  const value = new URLSearchParams(search).get("github_team_sync");
  return value !== null && Object.hasOwn(consentOutcomes, value) ? (value as ConsentOutcome) : null;
}

// GitHub's consent callback returns to /admin?github_team_sync=<outcome>#github-team-sync. The outcome is read
// once, the marker is removed from the address so a reload does not repeat it, and the notice takes focus.
function ConsentResult() {
  const search = useRouterState({ select: (state) => state.location.searchStr });
  const navigate = useNavigate();
  const client = useQueryClient();
  const [outcome, setOutcome] = useState(() => consentOutcome(search));
  const notice = useRef<HTMLDivElement>(null);
  const marked = new URLSearchParams(search).has("github_team_sync");
  useEffect(() => {
    if (!marked) return;
    void client.invalidateQueries({ queryKey: directoryKeys.githubSources });
    void navigate({ to: "/admin", search: {}, hash: githubTeamSyncAnchor, replace: true });
  }, [marked, client, navigate]);
  useEffect(() => {
    if (!outcome) return;
    notice.current?.scrollIntoView?.({ block: "center" });
    notice.current?.focus();
  }, [outcome]);
  if (!outcome) return null;
  const message = consentOutcomes[outcome];
  return (
    <div ref={notice} tabIndex={-1} className="rounded-md p-3 focus-visible:outline-none" data-testid="github-team-sync-consent">
      <Notice
        tone={message.tone}
        title={message.title}
        action={
          <Button variant="ghost" size="sm" onClick={() => setOutcome(null)}>
            Dismiss
          </Button>
        }
      >
        {message.body}
      </Notice>
    </div>
  );
}

export function GitHubTeamSync() {
  const online = useOnline();
  const sources = useQuery(githubTeamSourcesQuery);
  const [open, setOpen] = useState<{ id: string; mode: "edit" | "preview" } | "new" | null>(null);
  const [saved, setSaved] = useState(false);
  const count = sources.data?.length ?? 0;
  return (
    // The consent callback links to this anchor.
    <div id={githubTeamSyncAnchor} className="min-w-0 scroll-mt-4">
      <Section
        title="GitHub team sync"
        description="Keep Cove team membership in step with GitHub organization teams. Members are matched by their linked GitHub account; nobody is matched by email or created automatically."
        flush
        actions={
          open !== "new" && (
            <Button disabled={!online || !sources.isSuccess || count >= maxSources} title={count >= maxSources ? `At most ${maxSources} sources` : undefined} onClick={() => { setSaved(false); setOpen("new"); }}>
              <Plus aria-hidden className="size-3.5" />
              Add source
            </Button>
          )
        }
      >
        <ConsentResult />
        {sources.isPending ? (
          <LoadingRows label="Loading GitHub team sync sources" rows={2} />
        ) : sources.isError ? (
          <div className="p-3">
            <QueryError what="GitHub team sync" error={sources.error} onRetry={() => void sources.refetch()} retrying={sources.isFetching} />
          </div>
        ) : (
          <>
            {saved && (
              <div className="p-3">
                <Notice tone="success" title="Source saved">
                  Enabled sources apply changes automatically within a few minutes. Preview a source to see exactly who would be added, changed or removed.
                </Notice>
              </div>
            )}
            {open === "new" && (
              <SourceEditor
                onSaved={() => { setSaved(true); setOpen(null); }}
                onClose={() => setOpen(null)}
              />
            )}
            {sources.data.length === 0 && open !== "new" && <p className="px-4 py-3 text-[13px] text-fg-muted">No GitHub team sync sources. Add one to map GitHub teams to Cove teams.</p>}
            <ul className="divide-y divide-line">
              {sources.data.map((source) => (
                <SourceRow
                  key={source.id}
                  source={source}
                  editing={open !== "new" && open?.id === source.id && open.mode === "edit"}
                  previewing={open !== "new" && open?.id === source.id && open.mode === "preview"}
                  onEdit={() => { setSaved(false); setOpen({ id: source.id, mode: "edit" }); }}
                  onPreview={() => { setSaved(false); setOpen({ id: source.id, mode: "preview" }); }}
                  onClose={() => setOpen(null)}
                  onSaved={() => { setSaved(true); setOpen(null); }}
                />
              ))}
            </ul>
          </>
        )}
      </Section>
    </div>
  );
}
