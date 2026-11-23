import { useQuery } from "@tanstack/react-query";
import { Plus, RotateCw, Trash2 } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import {
  MAX_REPOSITORIES,
  templateRepositoriesQuery,
  useAddRepository,
  useRefreshRepository,
  useRemoveRepository,
  type RepositoryKind,
  type RepositoryScope,
  type TemplateRepository,
} from "../api/templates";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { shortRevision, sourceURLError } from "../lib/templates";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, MetaList, Notice, QueryError, Section } from "./ui";

const description =
  "Add public HTTPS Git repositories or template indexes. Cove reads cove-templates.json without running scripts or builds, validates every template, and refreshes each source every 6 hours.";

export function TemplateRepositories({ scope }: { scope: RepositoryScope }) {
  const repositories = useQuery(templateRepositoriesQuery(scope));
  const [adding, setAdding] = useState(false);
  const list = repositories.data?.repositories ?? [];
  const full = list.length >= MAX_REPOSITORIES;
  const scopeNote = scope.kind === "team" ? "Templates from these sources are offered only to this team." : "Templates from these sources are offered to every team.";
  const addButton = (
    <Button onClick={() => setAdding(true)} disabled={full} title={full ? `At most ${MAX_REPOSITORIES} sources` : undefined}>
      <Plus aria-hidden className="size-3.5" />
      Add source
    </Button>
  );
  return (
    <Section title="Template sources" description={`${description} ${scopeNote}`} actions={repositories.data && !adding ? addButton : undefined}
      flush
    >
      {repositories.isPending ? (
        <LoadingRows label="Loading template sources" rows={2} />
      ) : repositories.isError ? (
        <div className="p-4">
          <QueryError what="template sources" error={repositories.error} onRetry={() => void repositories.refetch()} retrying={repositories.isFetching} />
        </div>
      ) : (
        <RepositoryList scope={scope} list={list} adding={adding} onAdded={() => setAdding(false)} />
      )}
    </Section>
  );
}

function RepositoryList({ scope, list, adding, onAdded }: { scope: RepositoryScope; list: TemplateRepository[]; adding: boolean; onAdded: () => void }) {
  return (
    <div className="divide-y divide-line">
      {adding && <AddRepository scope={scope} onDone={onAdded} />}
      {list.length === 0 && !adding ? (
        <div className="px-4 py-6">
          <p className="text-[13px] font-medium text-fg">No template sources</p>
          <p className="mt-0.5 text-xs text-fg-muted">Only the built-in catalogue{scope.kind === "team" ? ", instance sources and this team's saved templates are" : " is"} offered.</p>
        </div>
      ) : (
        <ul className="divide-y divide-line">
          {list.map((repository) => (
            <RepositoryRow key={repository.id} scope={scope} repository={repository} />
          ))}
        </ul>
      )}
      {list.length >= MAX_REPOSITORIES && <p className="px-4 py-2.5 text-xs text-fg-muted">This scope has the maximum of {MAX_REPOSITORIES} sources. Remove one to add another.</p>}
    </div>
  );
}

function repositoryStatus(repository: TemplateRepository) {
  if (repository.refreshing) return <StatusLabel tone="info" label="Refreshing" pulse />;
  if (repository.error) return <StatusLabel tone="bad" label={repository.revision ? "Refresh failed, last valid revision kept" : "Refresh failed, no valid revision"} />;
  if (!repository.checked_at) return <StatusLabel tone="neutral" label="Waiting for first check" />;
  return <StatusLabel tone="ok" label="Valid" />;
}

function RepositoryRow({ scope, repository }: { scope: RepositoryScope; repository: TemplateRepository }) {
  const online = useOnline();
  const refresh = useRefreshRepository(scope);
  const remove = useRemoveRepository(scope);
  const [confirming, setConfirming] = useState(false);
  return (
    <li className="space-y-2 px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-64 space-y-1">
          <p className="text-[13px] font-medium break-words text-fg">{repository.name}</p>
          <p className="font-mono text-xs break-all text-fg-muted">{repository.url}</p>
          <div className="text-xs text-fg-muted">
            <MetaList>
              <span key="kind">{repository.kind === "git" ? "Git repository" : "HTTPS index"}</span>
              {repository.kind === "git" && (
                <span key="ref">
                  Reference <span className="font-mono text-fg">{repository.ref}</span>
                </span>
              )}
              <span key="revision">
                {repository.revision ? (
                  <>
                    Resolved <span className="font-mono text-fg" title={repository.revision}>{shortRevision(repository.revision)}</span>
                  </>
                ) : (
                  "Not resolved yet"
                )}
              </span>
              <span key="count">
                {repository.template_count} {repository.template_count === 1 ? "template" : "templates"}
              </span>
            </MetaList>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            onClick={() => {
              remove.reset();
              refresh.mutate(repository.id);
            }}
            pending={refresh.isPending}
            disabled={!online || repository.refreshing}
            aria-label={`Refresh ${repository.name}`}
          >
            {!refresh.isPending && <RotateCw aria-hidden className="size-3.5" />}
            Refresh
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(true)} disabled={!online} aria-label={`Remove ${repository.name}`}>
            <Trash2 aria-hidden className="size-3.5" />
            Remove
          </Button>
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-fg-muted">
        {repositoryStatus(repository)}
        {repository.checked_at && (
          <span>
            Last checked <RelativeTime value={repository.checked_at} />
          </span>
        )}
        <span>
          Next check <RelativeTime value={repository.next_refresh_at} />
        </span>
      </div>
      {repository.error && <p className="text-xs text-bad">{repository.error}</p>}
      {refresh.isError && (
        <Notice tone="error" title={isStatus(refresh.error, 422) ? "Refresh failed" : "Could not refresh"}>
          {isStatus(refresh.error, 422)
            ? `The source could not be fetched or one of its templates failed validation. ${repository.revision ? "The last validated revision is still offered." : "It offers no templates until a refresh succeeds."}`
            : errorMessage(refresh.error)}
        </Notice>
      )}
      {refresh.isSuccess && <p className="text-xs text-ok">Refreshed.</p>}
      <ConfirmDialog
        open={confirming}
        title={`Remove ${repository.name}?`}
        confirmLabel="Remove source"
        danger
        pending={remove.isPending}
        onCancel={() => {
          remove.reset();
          setConfirming(false);
        }}
        onConfirm={() => remove.mutate(repository.id, { onSuccess: () => setConfirming(false) })}
      >
        <p>Its templates leave the catalogue. Resources already created from them keep their configuration and are not changed.</p>
        {remove.isError && (
          <div className="mt-3">
            <Notice tone="error" title="Could not remove the source">
              {errorMessage(remove.error)}
            </Notice>
          </div>
        )}
      </ConfirmDialog>
    </li>
  );
}

type AddErrors = Partial<Record<"name" | "url" | "ref", string>>;

function AddRepository({ scope, onDone }: { scope: RepositoryScope; onDone: () => void }) {
  const id = useId();
  const online = useOnline();
  const add = useAddRepository(scope);
  const [name, setName] = useState("");
  const [kind, setKind] = useState<RepositoryKind>("git");
  const [url, setURL] = useState("");
  const [ref, setRef] = useState("");
  const [errors, setErrors] = useState<AddErrors>({});

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problems: AddErrors = {};
    if (name.trim() === "") problems.name = "Enter a name.";
    else if (name.trim().length > 120) problems.name = "Use at most 120 characters.";
    const urlProblem = sourceURLError(url);
    if (urlProblem) problems.url = urlProblem;
    if (kind === "git") {
      if (ref.trim() === "") problems.ref = "Enter a branch, tag or commit, such as main or v1.0.";
      else if (ref.trim().length > 200 || /\s/.test(ref.trim())) problems.ref = "Enter a single Git reference of at most 200 characters.";
    }
    setErrors(problems);
    if (Object.keys(problems).length > 0) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    add.mutate({ name: name.trim(), kind, url: url.trim(), ref: kind === "git" ? ref.trim() : "" }, { onSuccess: onDone });
  }

  const urlHint =
    kind === "git"
      ? "Public HTTPS clone URL. Cove reads cove-templates.json at the repository root. Credentials, query strings, other ports and redirects are refused."
      : "Public HTTPS URL of a cove-templates.json index. Credentials, query strings, other ports and redirects are refused.";

  return (
    <form onSubmit={submit} noValidate className="space-y-4 p-4">
      <h3 className="text-[13px] font-semibold text-fg">Add template source</h3>
      <fieldset disabled={add.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            <input
              id={`${id}-name`}
              className={inputClass}
              value={name}
              maxLength={120}
              onChange={(event) => setName(event.target.value)}
              autoComplete="off"
              aria-invalid={errors.name ? true : undefined}
              aria-describedby={describedBy(`${id}-name`, errors.name)}
            />
          </Field>
          <Field id={`${id}-kind`} label="Type">
            <Select id={`${id}-kind`} className={inputClass} value={kind} onChange={(event) => setKind(event.target.value === "index" ? "index" : "git")}>
              <option value="git">Git repository</option>
              <option value="index">HTTPS index file</option>
            </Select>
          </Field>
        </div>
        <Field id={`${id}-url`} label="URL" hint={urlHint} error={errors.url}>
          <input
            id={`${id}-url`}
            className={`${inputClass} font-mono`}
            value={url}
            onChange={(event) => setURL(event.target.value)}
            placeholder={kind === "git" ? "https://github.com/example/templates.git" : "https://example.com/cove-templates.json"}
            inputMode="url"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-invalid={errors.url ? true : undefined}
            aria-describedby={describedBy(`${id}-url`, errors.url, true)}
          />
        </Field>
        {kind === "git" && (
          <Field id={`${id}-ref`} label="Reference" hint="A branch, tag or commit, such as main or v1.0. Cove resolves it to an exact commit on each refresh." error={errors.ref}>
            <input
              id={`${id}-ref`}
              className={`${inputClass} font-mono sm:max-w-xs`}
              value={ref}
              onChange={(event) => setRef(event.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-invalid={errors.ref ? true : undefined}
              aria-describedby={describedBy(`${id}-ref`, errors.ref, true)}
            />
          </Field>
        )}
        <p className="text-xs text-fg-muted">Private repositories and authenticated hosts are not supported. Sources are fetched through the instance's outbound proxy, and private network destinations are refused.</p>
      </fieldset>
      {add.isError && (
        <Notice tone="error" title="Could not add the source">
          {errorMessage(add.error)}
        </Notice>
      )}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={onDone} disabled={add.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" pending={add.isPending} disabled={!online}>
          Add source
        </Button>
      </div>
    </form>
  );
}
