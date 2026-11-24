import { useQuery } from "@tanstack/react-query";
import { KeyRound, RotateCw, Trash2, X } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { previewImagesQuery, useIssuePreviewImagesCredential, useRevokePreviewImagesCredential, type PreviewImagesResource, type PreviewImagesStatus } from "../api/preview-images";
import { useOnline } from "../lib/hooks";
import { credentialDraft, credentialInput, daysError, issueBlocker, maxCredentialDays, sameSelection, selectableResources, wasRefused, type CredentialDraft, type IssuedCredential } from "../lib/preview-images";
import { choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Meter, MeterGroup, Notice, QueryError, Section, Subsection } from "./ui";

type Issue = ReturnType<typeof useIssuePreviewImagesCredential>;
type Revoke = ReturnType<typeof useRevokePreviewImagesCredential>;

const description =
  "A CI workflow submits images it built and pushed to each selected resource's team registry. Cove creates or updates the preview with those exact digests and never builds them. The recipe still decides data, variables, secret files, routes and approval. The CI credential cannot read secrets, use the API or deploy production.";

export function PreviewImages({ projectId }: { projectId: string }) {
  const status = useQuery(previewImagesQuery(projectId));
  const online = useOnline();
  const [secret, setSecret] = useState<IssuedCredential | null>(null);
  const [outcome, setOutcome] = useState<string | null>(null);
  const issue = useIssuePreviewImagesCredential(projectId, setSecret);
  const revoke = useRevokePreviewImagesCredential(projectId);
  return (
    <Section
      title="CI-built image previews"
      description={description}
      width="form"
      flush
      actions={
        <Button variant="ghost" onClick={() => void status.refetch()} pending={status.isFetching} disabled={status.isPending}>
          {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Reload
        </Button>
      }
    >
      <div className="space-y-2 p-4 pb-0 empty:hidden" aria-live="polite">
        {!online && (
          <Notice tone="warn" title="You are offline">
            Issuing, replacing and revoking are paused, and these settings are not rechecked until the connection returns.
          </Notice>
        )}
        {secret && <OneTimeCredential credential={secret} onClear={() => setSecret(null)} />}
        {outcome && <Notice tone="success" title={outcome} />}
      </div>
      {status.isPending ? (
        <LoadingRows label="Loading CI image preview settings" rows={3} />
      ) : status.isError ? (
        <div className="p-4">
          <QueryError what="CI image preview settings" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
        </div>
      ) : (
        <PreviewImagesBody status={status.data} issue={issue} revoke={revoke} online={online} onSecret={setSecret} onOutcome={setOutcome} />
      )}
    </Section>
  );
}

interface BodyProps {
  status: PreviewImagesStatus;
  issue: Issue;
  revoke: Revoke;
  online: boolean;
  onSecret: (credential: IssuedCredential | null) => void;
  onOutcome: (outcome: string | null) => void;
}

function PreviewImagesBody({ status, issue, revoke, online, onSecret, onOutcome }: BodyProps) {
  const saved = status.configuration;
  return (
    <div className="divide-y divide-line">
      {status.setup_reason && (
        <div className="p-4">
          <Notice tone="info">{status.setup_reason}</Notice>
        </div>
      )}
      {saved && <SavedCredential status={status} revoke={revoke} online={online} busy={issue.isPending} onSecret={onSecret} onOutcome={onOutcome} />}
      <Submission status={status} />
      {status.can_manage ? (
        <CredentialForm key={JSON.stringify(saved)} status={status} issue={issue} online={online} busy={revoke.isPending} onOutcome={onOutcome} />
      ) : status.setup_reason ? null : (
        <Subsection title="Resources">
          <p className="text-xs text-fg-muted">Issuing, replacing or revoking the CI credential needs edit access to the project and its production environment. These settings are read-only.</p>
          <ResourceList status={status} selected={saved?.resource_ids ?? []} />
        </Subsection>
      )}
    </div>
  );
}

function SavedCredential({ status, revoke, online, busy, onSecret, onOutcome }: { status: PreviewImagesStatus; revoke: Revoke; online: boolean; busy: boolean; onSecret: (credential: IssuedCredential | null) => void; onOutcome: (outcome: string | null) => void }) {
  const [confirm, setConfirm] = useState(false);
  const saved = status.configuration;
  if (!saved) return null;
  const names = new Map(status.resources.map((resource) => [resource.id, resource.name]));
  return (
    <Subsection title="Credential">
      {!saved.matches_scope && (
        <Notice tone="warn" title="Submissions are refused">
          The credential no longer matches the scope it was issued for. It may have expired, the recipe or a selected resource&apos;s configuration, Compose services or registry may have changed, or the credential&apos;s owner or the resource&apos;s worker may no longer be available. Restore the scope or issue a replacement. Previews created with a different scope need a new preview key.
        </Notice>
      )}
      <DefinitionList
        items={[
          { term: "Status", value: saved.matches_scope ? <StatusLabel tone="ok" label="Accepting submissions" /> : <StatusLabel tone="warn" label="Scope changed or expired" /> },
          {
            term: "Resources",
            value: (
              <ul className="space-y-0.5">
                {saved.resource_ids.map((id) => (
                  <li key={id} className="break-words">
                    {names.get(id) ?? (
                      <>
                        <span className="font-mono text-xs break-all">{id}</span> <span className="text-xs text-warn">no longer in the production environment</span>
                      </>
                    )}
                  </li>
                ))}
              </ul>
            ),
          },
          { term: "Expires", value: <RelativeTime value={saved.expires_at} /> },
          { term: "Issued", value: <RelativeTime value={saved.created_at} /> },
          { term: "Last accepted", value: saved.last_received_at ? <RelativeTime value={saved.last_received_at} /> : <span className="text-fg-muted">None yet</span> },
        ]}
      />
      {status.can_manage && (
        <div className="space-y-2">
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="danger" onClick={() => setConfirm(true)} disabled={!online || busy || revoke.isPending} pending={revoke.isPending} title={online ? undefined : "Unavailable while offline."}>
              {!revoke.isPending && <Trash2 aria-hidden className="size-3.5" />}
              Revoke credential
            </Button>
          </div>
          {revoke.isError && (
            <Notice tone="error" title={wasRefused(revoke.error) ? "The credential was not revoked" : "It is not known whether the credential was revoked"}>
              <p className="break-words">{errorMessage(revoke.error)}</p>
              {!wasRefused(revoke.error) && <p className="mt-1">Reload to check before relying on it.</p>}
            </Notice>
          )}
        </div>
      )}
      <ConfirmDialog
        open={confirm}
        title="Revoke the CI credential?"
        confirmLabel="Revoke credential"
        danger
        pending={revoke.isPending}
        onCancel={() => setConfirm(false)}
        onConfirm={() =>
          revoke.mutate(undefined, {
            onSuccess: () => {
              onSecret(null);
              onOutcome("Credential revoked");
              revoke.reset();
            },
            onSettled: () => setConfirm(false),
          })
        }
      >
        CI submissions are refused immediately. Existing previews and submissions that were already accepted are not cancelled.
      </ConfirmDialog>
    </Subsection>
  );
}

function Submission({ status }: { status: PreviewImagesStatus }) {
  const endpoint = new URL(status.endpoint, window.location.origin).href;
  return (
    <Subsection title="CI submissions">
      <DefinitionList
        items={[
          {
            term: "Endpoint",
            value: (
              <span className="flex min-w-0 flex-wrap items-center gap-2">
                <span className="min-w-0 font-mono text-xs break-all">{endpoint}</span>
                <CopyButton text={endpoint} size="sm" subject="CI endpoint" />
              </span>
            ),
          },
        ]}
      />
      <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
        <li>POST JSON with the header Authorization: Bearer &lt;credential&gt;: a delivery_id, the preview key, a sequence that increases for each key, the full commit revision and the images.</li>
        <li>Images name every selected resource and every one of its services, each as an immutable digest reference on that resource&apos;s registry. Mutable tags are refused.</li>
        <li>An accepted submission is queued, not published. Approval and the usual deployment gates still apply.</li>
        <li>While a preview is being prepared or updated, or has an operation running, a new submission for it is refused with 409 and its delivery_id and sequence stay unused. Retry once it settles.</li>
        <li>Resending a delivery that was already accepted is safe and changes nothing. The same images and revision with a higher sequence neither redeploy the preview nor ask for approval again.</li>
        <li>Preview keys stay reserved after a preview is deleted or the credential is replaced. Use a new key after the scope changes.</li>
      </ul>
      <MeterGroup label="CI submission history" columns={3}>
        <Meter label="Preview keys used" value={status.used_preview_keys} limit={status.max_preview_keys} />
        <Meter label="Deliveries kept for replay checks" value={status.retained_deliveries} limit={status.max_deliveries} />
      </MeterGroup>
    </Subsection>
  );
}

function ResourceList({ status, selected }: { status: PreviewImagesStatus; selected: string[] }) {
  const rows = selectableResources(status, { resourceIds: selected, days: "" });
  if (rows.length === 0) return <p className="text-xs text-fg-muted">The production environment has no resources.</p>;
  const chosen = new Set(selected);
  return (
    <ul aria-label="Resources for CI images" className="divide-y divide-line rounded-md border border-line">
      {rows.map(({ id, resource }) => (
        <li key={id} className="px-3 py-2">
          <ResourceSummary id={id} resource={resource} selected={chosen.has(id)} />
        </li>
      ))}
    </ul>
  );
}

function ResourceSummary({ id, resource, selected }: { id: string; resource: PreviewImagesResource | null; selected: boolean }) {
  if (!resource) {
    return (
      <span className="block min-w-0 text-xs">
        <span className="block font-mono break-all text-fg">{id}</span>
        <span className="block text-warn">Selected for the current credential, but no longer in the production environment.</span>
      </span>
    );
  }
  return (
    <span className="block min-w-0 text-xs">
      <span className="block text-[13px] break-words text-fg">
        {resource.name}
      </span>
      {resource.services.length > 0 && (
        <span className="block text-fg-muted">
          Services <span className="font-mono break-all">{resource.services.join(", ")}</span>
        </span>
      )}
      {resource.registry && (
        <span className="block text-fg-muted">
          Registry <span className="font-mono break-all">{resource.registry.server}</span>
        </span>
      )}
      {!resource.eligible && <span className={`block ${selected ? "text-warn" : "text-fg-muted"}`}>{resource.reason || "Cannot receive CI images."}</span>}
    </span>
  );
}

const issueCopy = {
  title: "Issue a credential",
  action: "Issue credential",
  dialog: "Issue a CI credential?",
  outcome: "Credential issued",
};

const replaceCopy = {
  title: "Replace the credential",
  action: "Replace credential",
  dialog: "Replace the CI credential?",
  outcome: "Credential replaced",
};

function CredentialForm({ status, issue, online, busy, onOutcome }: { status: PreviewImagesStatus; issue: Issue; online: boolean; busy: boolean; onOutcome: (outcome: string | null) => void }) {
  const saved = status.configuration;
  const replacing = saved !== null;
  const copy = replacing ? replaceCopy : issueCopy;
  const initial = credentialDraft(status);
  const [draft, setDraft] = useState<CredentialDraft>(initial);
  const [confirm, setConfirm] = useState(false);
  const blocker = online ? issueBlocker(status, draft) : "You are offline.";
  const changed = !sameSelection(draft.resourceIds, initial.resourceIds);
  const pristine = !changed && draft.days === initial.days;

  function edit(next: CredentialDraft) {
    setDraft(next);
    issue.reset();
    onOutcome(null);
  }

  function toggle(resourceId: string, checked: boolean) {
    edit({ ...draft, resourceIds: checked ? [...draft.resourceIds, resourceId] : draft.resourceIds.filter((value) => value !== resourceId) });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!blocker) setConfirm(true);
  }

  function send() {
    issue.mutate(credentialInput(draft), {
      onSuccess: () => {
        onOutcome(copy.outcome);
        issue.reset();
      },
      onSettled: () => setConfirm(false),
    });
  }

  return (
    <form onSubmit={submit} aria-label="CI credential settings" noValidate>
      <Subsection
        title={copy.title}
        description={`Choose up to ${status.max_resources} resources whose images CI provides. Other recipe resources keep their pinned production revisions. Set up a resource's registry on its Registry page.`}
      >
        <ResourceChoices status={status} draft={draft} disabled={issue.isPending} onToggle={toggle} />
        <DaysField value={draft.days} disabled={issue.isPending} onChange={(days) => edit({ ...draft, days })} />
        {issue.isError && <IssueError error={issue.error} replacing={replacing} />}
        <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2">
          <p role="status" className="mr-auto text-xs text-fg-muted">
            {blocker ?? issueSummary(replacing, changed)}
          </p>
          <Button variant="ghost" onClick={() => edit(initial)} disabled={issue.isPending || pristine}>
            Discard changes
          </Button>
          <Button type="submit" variant="primary" disabled={!!blocker || busy} pending={issue.isPending}>
            {!issue.isPending && <KeyRound aria-hidden className="size-3.5" />}
            {copy.action}
          </Button>
        </div>
      </Subsection>
      <ConfirmDialog open={confirm} title={copy.dialog} confirmLabel={copy.action} pending={issue.isPending} onCancel={() => setConfirm(false)} onConfirm={send}>
        <p>
          Anyone holding this credential can run images of their choice in previews of the selected resources, with the runtime variables and secret files the recipe gives those previews. It cannot read secrets, use the API or deploy production.
        </p>
        {replacing && <p className="mt-2">The current credential stops working immediately. Existing previews and accepted submissions are kept.</p>}
      </ConfirmDialog>
    </form>
  );
}

function DaysField({ value, disabled, onChange }: { value: string; disabled: boolean; onChange: (days: string) => void }) {
  const id = useId();
  const error = daysError(value);
  return (
    <Field id={id} label="Expires after" hint={`Days from now, 1 to ${maxCredentialDays}. Each issue sets a new lifetime.`} error={error}>
      <input
        id={id}
        type="number"
        min={1}
        max={maxCredentialDays}
        className={`${inputClass} max-w-32 tabular-nums`}
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : `${id}-hint`}
      />
    </Field>
  );
}

function ResourceChoices({ status, draft, disabled, onToggle }: { status: PreviewImagesStatus; draft: CredentialDraft; disabled: boolean; onToggle: (resourceId: string, checked: boolean) => void }) {
  const rows = selectableResources(status, draft);
  const selected = new Set(draft.resourceIds);
  return (
    <fieldset className="min-w-0" disabled={disabled}>
      <legend className="sr-only">Resources that receive CI images</legend>
      {rows.length === 0 ? (
        <p className="text-xs text-fg-muted">The production environment has no resources.</p>
      ) : (
        <div className="grid gap-2">
          {rows.map(({ id: resourceId, resource }) => {
            const checked = selected.has(resourceId);
            return (
              <label key={resourceId} className={`${choiceClass} items-start`}>
                {/* A selected resource that is no longer eligible can only be unselected, never silently dropped. */}
                <Checkbox checked={checked} disabled={!checked && resource?.eligible !== true} onChange={(event) => onToggle(resourceId, event.target.checked)} />
                <ResourceSummary id={resourceId} resource={resource} selected={checked} />
              </label>
            );
          })}
        </div>
      )}
    </fieldset>
  );
}

function issueSummary(replacing: boolean, changed: boolean) {
  if (!replacing) return "Ready to issue.";
  return changed ? "Replacing changes the selected resources." : "Replacing keeps the selection with a new lifetime.";
}

function IssueError({ error, replacing }: { error: Error; replacing: boolean }) {
  const refused = wasRefused(error);
  return (
    <Notice tone="error" title={refused ? "No credential was issued" : "It is not known whether a credential was issued"}>
      <p className="break-words">{errorMessage(error)}</p>
      <p className="mt-1">{refused ? "Nothing changed and your selection is kept." : `${replacing ? "The previous credential may already be revoked. " : ""}Reload to check, then issue a new credential to get a token you can see.`}</p>
    </Notice>
  );
}

function OneTimeCredential({ credential, onClear }: { credential: IssuedCredential; onClear: () => void }) {
  const id = useId();
  return (
    <section aria-labelledby={`${id}-title`} className="space-y-2 rounded-md border border-warn-line bg-warn-tint px-3 py-2.5 text-[13px]">
      <h3 id={`${id}-title`} className="font-medium text-fg">
        Copy the CI credential now
      </h3>
      <p className="text-fg-muted">
        Copy it to your CI secret store before clearing this message. Cove cannot show it again. It expires <RelativeTime value={credential.expires_at} />. If it is lost, issue a replacement.
      </p>
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <input aria-label="CI credential" readOnly value={credential.token} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} autoComplete="off" spellCheck={false} onFocus={(event) => event.target.select()} />
        <CopyButton text={credential.token} label="Copy credential" />
        <Button variant="ghost" onClick={onClear}>
          <X aria-hidden className="size-3.5" />
          Clear
        </Button>
      </div>
    </section>
  );
}
