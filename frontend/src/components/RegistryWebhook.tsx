import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { KeyRound, Plus, RotateCw, Trash2, X } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { imageUpdatePolicyQuery } from "../api/image-updates";
import { registryWebhookQuery, useRemoveRegistryWebhook, useRotateRegistryWebhook, useSaveRegistryWebhook, type RegistryWebhookProvider, type RegistryWebhookStatus } from "../api/registry-webhook";
import type { Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { isManagedPreview } from "../lib/previews";
import {
  dockerHubSecretURL,
  enabledError,
  filterDraft,
  maxWebhookFilters,
  providerError,
  providers,
  rotateBlockedReason,
  validateWebhookDraft,
  webhookDraftFrom,
  webhookInput,
  webhookProblem,
  webhookSaveState,
  type FilterDraft,
  type FilterErrors,
  type WebhookAccess,
  type WebhookDraft,
} from "../lib/registry-webhook";
import { inputClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { OperationStateLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Subsection, SwitchField } from "./ui";

type Save = ReturnType<typeof useSaveRegistryWebhook>;
type Rotate = ReturnType<typeof useRotateRegistryWebhook>;
type Remove = ReturnType<typeof useRemoveRegistryWebhook>;

// A one-time secret with the provider and endpoint it was issued for, kept only in component state.
interface Secret {
  token: string;
  provider: RegistryWebhookProvider;
  endpoint: string | null;
}

interface Actions {
  save: Save;
  rotate: Rotate;
  remove: Remove;
  onSecret: (secret: Secret | null) => void;
  onOutcome: (outcome: string | null) => void;
}

const description =
  "When the connected registry reports a push of a watched tag, Cove queues an ordinary deploy that pulls the resource’s configured image tags and redeploys. Compose and tag settings stay unchanged, and each deploy still passes approval, freeze windows, deploy windows and quotas.";

// An enabled image update policy takes over matching pushes: the control plane requests a fresh image check and the
// policy's mode, ranges and window decide what is deployed. Without one, pushes still pull and redeploy directly.
const policyDescription =
  "An image update policy is on for this resource, so a push of a watched tag requests a fresh image check instead of a deploy. The policy's mode, version ranges and maintenance window decide whether anything is deployed. Compose and tag settings stay unchanged.";

export function RegistryWebhook({ workload }: { workload: Workload }) {
  if (workload.kind === "job" || isManagedPreview(workload)) {
    return (
      <Section title="Registry webhook" description={description} width="form">
        <p className="text-[13px] text-fg-muted">
          {workload.kind === "job" ? "Jobs are never pulled and redeployed automatically. Registry webhooks are available for services." : "Managed previews do not receive registry webhooks."}
        </p>
      </Section>
    );
  }
  return <WebhookSection workload={workload} />;
}

function WebhookSection({ workload }: { workload: Workload }) {
  const status = useQuery(registryWebhookQuery(workload.id));
  const policy = useQuery(imageUpdatePolicyQuery(workload.id));
  const policyControls = policy.data?.configuration?.enabled === true;
  const save = useSaveRegistryWebhook(workload.id);
  const rotate = useRotateRegistryWebhook(workload.id);
  const remove = useRemoveRegistryWebhook(workload.id);
  const online = useOnline();
  const [secret, setSecret] = useState<Secret | null>(null);
  const [outcome, setOutcome] = useState<string | null>(null);
  const actions: Actions = { save, rotate, remove, onSecret: setSecret, onOutcome: setOutcome };
  return (
    <Section
      title="Registry webhook"
      description={policyControls ? policyDescription : description}
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
        {!online && <Notice tone="warn" title="You are offline">Saving, rotating and removing are paused until the connection returns.</Notice>}
        {secret && <OneTimeSecret secret={secret} onClear={() => setSecret(null)} />}
        {outcome && <Notice tone="success" title={outcome} />}
        {policy.isError && <p className="text-xs text-fg-muted">Could not read the image update policy. If one is on, it decides what a matching push deploys.</p>}
      </div>
      <WebhookBody workload={workload} status={status} actions={actions} online={online} policyControls={policyControls} />
    </Section>
  );
}

function WebhookBody({ workload, status, actions, online, policyControls }: { workload: Workload; status: UseQueryResult<RegistryWebhookStatus>; actions: Actions; online: boolean; policyControls: boolean }) {
  if (status.isPending) return <LoadingRows label="Loading registry webhook" rows={3} />;
  if (status.isError) {
    return (
      <div className="p-4">
        <QueryError what="the registry webhook" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
      </div>
    );
  }
  const data = status.data;
  const access: WebhookAccess = { canEdit: can(workload.permissions, "edit_configuration"), canOperate: can(workload.permissions, "operate") };
  const blocked = online ? undefined : "You are offline.";
  return (
    <div className="divide-y divide-line">
      {data.configuration && <SavedWebhook status={data} actions={actions} access={access} blocked={blocked} policyControls={policyControls} />}
      <WebhookSettings workload={workload} status={data} actions={actions} access={access} blocked={blocked} policyControls={policyControls} />
    </div>
  );
}

function SavedWebhook({ status, actions, access, blocked, policyControls }: { status: RegistryWebhookStatus; actions: Actions; access: WebhookAccess; blocked?: string; policyControls: boolean }) {
  const problem = webhookProblem(status, access);
  return (
    <Subsection title="Webhook">
      {problem && (
        <Notice tone={problem.tone} title={problem.title}>
          {problem.text}
        </Notice>
      )}
      <WebhookDetails status={status} policyControls={policyControls} />
      <WebhookActivity status={status} />
      {access.canEdit && <ManageWebhook status={status} actions={actions} canOperate={access.canOperate} blocked={blocked} />}
    </Subsection>
  );
}

function WebhookSettings({ workload, status, actions, access, blocked, policyControls }: { workload: Workload; status: RegistryWebhookStatus; actions: Actions; access: WebhookAccess; blocked?: string; policyControls: boolean }) {
  if (!status.registry) {
    if (status.configuration) return null;
    return (
      <Subsection>
        <p className="text-[13px] text-fg-muted">Select a team registry under Docker registry first. Pushes are accepted only from the registry this resource uses.</p>
      </Subsection>
    );
  }
  if (workload.source && !status.configuration) {
    return (
      <Subsection>
        <p className="text-[13px] text-fg-muted">This resource deploys from Git. Registry webhooks redeploy saved image-only Compose files, without Git sources or build sections.</p>
      </Subsection>
    );
  }
  if (!access.canEdit) {
    return (
      <Subsection>
        <p className="text-xs text-fg-muted">Changing the registry webhook needs Edit configuration access. Settings are read-only.</p>
        {status.configuration && <FilterList filters={status.configuration.filters} />}
      </Subsection>
    );
  }
  return <WebhookForm key={JSON.stringify(status.configuration)} status={status} actions={actions} canOperate={access.canOperate} blocked={blocked} policyControls={policyControls} />;
}

function FilterList({ filters }: { filters: { repository: string; tag: string }[] }) {
  return (
    <ul aria-label="Watched tags" className="divide-y divide-line rounded-md border border-line">
      {filters.map((filter) => (
        <li key={`${filter.repository}:${filter.tag}`} className="px-3 py-2 font-mono text-xs break-all text-fg">
          {filter.repository}:{filter.tag === "*" ? "* (every tag)" : filter.tag}
        </li>
      ))}
    </ul>
  );
}

const none = <span className="text-fg-muted">None yet</span>;

function WebhookDetails({ status, policyControls }: { status: RegistryWebhookStatus; policyControls: boolean }) {
  const saved = status.configuration;
  if (!saved) return null;
  return (
    <DefinitionList
      items={[
        { term: "Provider", value: providers[saved.provider]?.label ?? saved.provider },
        {
          term: "Endpoint",
          value: status.endpoint ? (
            <span className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="min-w-0 font-mono text-xs break-all">{status.endpoint}</span>
              <CopyButton text={status.endpoint} size="sm" subject="webhook endpoint" />
            </span>
          ) : (
            <span className="text-fg-muted">Not available</span>
          ),
        },
        { term: "Saved for", value: <span className="font-mono text-xs break-all">{saved.registry_server}</span> },
        { term: "Automatic deploys", value: saved.enabled ? (policyControls ? "On: pushes request an image check for the image update policy" : "On") : "Off" },
      ]}
    />
  );
}

function WebhookActivity({ status }: { status: RegistryWebhookStatus }) {
  const operation = status.last_operation;
  return (
    <div className="space-y-2">
      <DefinitionList
        items={[
          { term: "Last delivery", value: status.last_received_at ? <RelativeTime value={status.last_received_at} /> : none },
          { term: "Last matching push", value: status.last_matched_at ? <RelativeTime value={status.last_matched_at} /> : none },
          { term: "Pending deploy", value: status.pending_at ? <span>Waiting since <RelativeTime value={status.pending_at} /></span> : <span className="text-fg-muted">None</span> },
          {
            term: "Last deploy",
            value: operation ? (
              <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <OperationStateLabel state={operation.state} />
                <RelativeTime value={operation.created_at} className="text-xs text-fg-muted" />
                <Link to="/operations/$operationId" params={{ operationId: operation.id }} className={linkClass}>
                  View operation
                </Link>
              </span>
            ) : (
              none
            ),
          },
        ]}
      />
      {status.last_error && (
        <Notice tone="error" title="Last error">
          <p className="break-words">{status.last_error}</p>
        </Notice>
      )}
      {status.last_ignored_reason && (
        <Notice tone="warn" title="Last push ignored">
          <p className="break-words">{status.last_ignored_reason}</p>
        </Notice>
      )}
    </div>
  );
}

function ProviderSetup({ provider, endpoint }: { provider: RegistryWebhookProvider; endpoint: string | null }) {
  const url = endpoint ? <span className="font-mono text-xs break-all">{endpoint}</span> : "the endpoint";
  if (provider === "ghcr") {
    return (
      <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
        <li>Add a repository or organization webhook on GitHub with payload URL {url} and content type application/json.</li>
        <li>Paste the token as the webhook Secret and send package events. Requests must carry a valid X-Hub-Signature-256 signature; SHA-1 signatures are refused.</li>
        <li>Untagged architecture manifests are ignored. Package access and the webhook are set up on GitHub; Cove does not configure them.</li>
      </ul>
    );
  }
  if (provider === "docker_hub") {
    return (
      <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
        <li>Docker Hub webhooks are configured with a URL only, so the secret is part of the URL. Treat that URL as a credential, and make sure reverse proxies do not log it.</li>
        <li>Docker Hub does not sign requests. A relay you control can send the token as Authorization: Bearer &lt;token&gt; instead.</li>
        <li>Identical pushes within the same second are deployed once, and the saved tag is pulled.</li>
      </ul>
    );
  }
  return (
    <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
      <li>Add a notification endpoint for {url} that sends the header Authorization: Bearer &lt;token&gt;.</li>
      <li>Only tagged manifest and index pushes count; pulls, deletes, layers and untagged digests are ignored.</li>
    </ul>
  );
}

function OneTimeSecret({ secret, onClear }: { secret: Secret; onClear: () => void }) {
  const id = useId();
  const hub = secret.provider === "docker_hub" && secret.endpoint ? dockerHubSecretURL(secret.endpoint, secret.token) : null;
  return (
    <section aria-labelledby={`${id}-title`} className="space-y-2 rounded-md border border-warn-line bg-warn-tint px-3 py-2.5 text-[13px]">
      <h3 id={`${id}-title`} className="font-medium text-fg">
        Copy the webhook secret now
      </h3>
      <p className="text-fg-muted">It is shown once and is not stored in this browser. If it is lost, rotate it; the old one stops working. Setup steps are listed under Webhook below.</p>
      {hub && (
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <input aria-label="Secret webhook URL" readOnly value={hub} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} autoComplete="off" spellCheck={false} onFocus={(event) => event.target.select()} />
          <CopyButton text={hub} label="Copy secret URL" />
        </div>
      )}
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <input id={id} aria-label="Webhook token" readOnly value={secret.token} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} autoComplete="off" spellCheck={false} onFocus={(event) => event.target.select()} />
        <CopyButton text={secret.token} label="Copy token" />
        <Button variant="ghost" onClick={onClear}>
          <X aria-hidden className="size-3.5" />
          Clear
        </Button>
      </div>
      {hub && <p className="text-xs text-fg-muted">The secret URL is a credential. Make sure reverse proxies do not log it.</p>}
    </section>
  );
}

function ManageWebhook({ status, actions, canOperate, blocked }: { status: RegistryWebhookStatus; actions: Actions; canOperate: boolean; blocked?: string }) {
  const [confirm, setConfirm] = useState<"rotate" | "remove" | null>(null);
  const { rotate, remove } = actions;
  const saved = status.configuration;
  const busy = rotate.isPending || remove.isPending || actions.save.isPending;
  const rotateBlocked = rotateBlockedReason(status, canOperate, blocked);
  if (!saved) return null;
  const close = () => setConfirm(null);
  return (
    <div className="space-y-2">
      <ProviderSetup provider={saved.provider} endpoint={status.endpoint} />
      <div className="flex flex-wrap justify-end gap-2">
        <Button onClick={() => setConfirm("rotate")} disabled={!!rotateBlocked || busy} pending={rotate.isPending} title={rotateBlocked}>
          {!rotate.isPending && <KeyRound aria-hidden className="size-3.5" />}
          Rotate secret
        </Button>
        <Button variant="danger" onClick={() => setConfirm("remove")} disabled={!!blocked || busy} pending={remove.isPending} title={blocked}>
          {!remove.isPending && <Trash2 aria-hidden className="size-3.5" />}
          Remove webhook
        </Button>
      </div>
      {rotate.isError && <Notice tone="error" title="The secret was not rotated">{errorMessage(rotate.error)}</Notice>}
      {remove.isError && <Notice tone="error" title="The webhook was not removed">{errorMessage(remove.error)}</Notice>}
      <RotateDialog open={confirm === "rotate"} provider={saved.provider} actions={actions} onClose={close} />
      <RemoveDialog open={confirm === "remove"} actions={actions} onClose={close} />
    </div>
  );
}

function RotateDialog({ open, provider, actions, onClose }: { open: boolean; provider: RegistryWebhookProvider; actions: Actions; onClose: () => void }) {
  const { rotate } = actions;
  return (
    <ConfirmDialog
      open={open}
      title="Rotate the webhook secret?"
      confirmLabel="Rotate secret"
      pending={rotate.isPending}
      onCancel={onClose}
      onConfirm={() =>
        rotate.mutate(undefined, {
          onSuccess: ({ status: next, takeToken }) => {
            const token = takeToken();
            if (token && next.configuration) actions.onSecret({ token, provider: next.configuration.provider, endpoint: next.endpoint });
            actions.onOutcome("Secret rotated");
            rotate.reset();
          },
          onSettled: onClose,
        })
      }
    >
      The current secret stops working immediately{provider === "docker_hub" ? ", including the secret URL" : ""}. Update the registry with the new one. You become the webhook’s owner, so pushes deploy only while you keep Edit configuration and Operate access. Accepted pushes waiting to deploy are kept if you already own the webhook; otherwise the previous owner’s pending webhook deploys are cancelled.
    </ConfirmDialog>
  );
}

function RemoveDialog({ open, actions, onClose }: { open: boolean; actions: Actions; onClose: () => void }) {
  const { remove } = actions;
  return (
    <ConfirmDialog
      open={open}
      title="Remove the registry webhook?"
      confirmLabel="Remove webhook"
      danger
      pending={remove.isPending}
      onCancel={onClose}
      onConfirm={() =>
        remove.mutate(undefined, {
          onSuccess: () => {
            actions.onSecret(null);
            actions.onOutcome("Webhook removed");
            remove.reset();
          },
          onSettled: onClose,
        })
      }
    >
      Pushes are no longer deployed. A push waiting to be deployed is dropped and queued webhook deploys are cancelled; one already running may finish.
    </ConfirmDialog>
  );
}

function formStatus(save: Save, dirty: boolean, blocked: string | undefined, rotates: boolean, claims: boolean) {
  if (save.isPending) return "Saving";
  if (blocked) return blocked;
  if (dirty && rotates) return "Saving issues a new secret; set up the registry again afterwards.";
  if (claims) return "Saving makes you the webhook’s owner; later pushes deploy with your access.";
  if (dirty) return "Unsaved changes";
  return "No unsaved changes";
}

function WebhookForm({ status, actions, canOperate, blocked, policyControls }: { status: RegistryWebhookStatus; actions: Actions; canOperate: boolean; blocked?: string; policyControls: boolean }) {
  const formRef = useRef<HTMLFormElement>(null);
  const saved = webhookDraftFrom(status);
  const [draft, setDraft] = useState<WebhookDraft>(saved);
  const [showErrors, setShowErrors] = useState(false);
  const [confirmEnable, setConfirmEnable] = useState(false);
  const { save } = actions;
  const existing = status.configuration;
  const server = status.registry?.server;
  const { rotates, changed, claims, dirty, confirmsEnable } = webhookSaveState(status, saved, draft, canOperate);
  const enableError = enabledError(draft, status, canOperate);
  const errors = showErrors ? validateWebhookDraft(draft, server) : {};

  function edit(next: WebhookDraft) {
    setDraft(next);
    save.reset();
    actions.onOutcome(null);
  }

  function send() {
    save.mutate(webhookInput(draft), {
      onSuccess: ({ status: next, takeToken }) => {
        const token = takeToken();
        if (token && next.configuration) actions.onSecret({ token, provider: next.configuration.provider, endpoint: next.endpoint });
        actions.onOutcome(existing ? "Webhook saved" : "Webhook created");
        save.reset();
      },
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (enableError || Object.keys(validateWebhookDraft(draft, server)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    if (confirmsEnable) setConfirmEnable(true);
    else send();
  }

  return (
    <form ref={formRef} onSubmit={submit} aria-label="Registry webhook settings" noValidate>
      <UnsavedChangesGuard dirty={changed} />
      <fieldset disabled={!!blocked} className="min-w-0">
        <legend className="sr-only">Registry webhook settings</legend>
        <WebhookFields status={status} draft={draft} errors={errors} enableError={enableError} canOperate={canOperate} policyControls={policyControls} onChange={edit} />
      </fieldset>
      {save.isError && <SaveError error={save.error} existing={!!existing} />}
      <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2 border-t border-line px-4 py-3">
        <p role="status" className="mr-auto text-xs text-fg-muted">
          {formStatus(save, dirty, blocked, rotates, claims)}
        </p>
        <Button variant="ghost" onClick={() => edit(saved)} disabled={!changed || save.isPending}>
          Discard changes
        </Button>
        <Button type="submit" variant="primary" disabled={!dirty || !!blocked} pending={save.isPending}>
          {existing ? "Save webhook" : "Create webhook"}
        </Button>
      </div>
      <EnableDialog
        open={confirmEnable}
        takesOwnership={!!existing?.enabled}
        pending={save.isPending}
        onCancel={() => setConfirmEnable(false)}
        onConfirm={() => {
          setConfirmEnable(false);
          send();
        }}
        policyControls={policyControls}
      />
    </form>
  );
}

function SaveError({ error, existing }: { error: unknown; existing: boolean }) {
  return (
    <div className="px-4 pb-4">
      <Notice tone="error" title={existing ? "The webhook was not saved" : "The webhook was not created"}>
        <p className="break-words">{errorMessage(error)}</p>
        <p className="mt-1">Your changes are kept.</p>
      </Notice>
    </div>
  );
}

function EnableDialog({ open, takesOwnership, pending, policyControls, onCancel, onConfirm }: { open: boolean; takesOwnership: boolean; pending: boolean; policyControls: boolean; onCancel: () => void; onConfirm: () => void }) {
  return (
    <ConfirmDialog
      open={open}
      title={takesOwnership ? "Take ownership of automatic deploys?" : "Turn on automatic pull and redeploy?"}
      confirmLabel={takesOwnership ? "Take ownership" : "Turn on automatic deploys"}
      pending={pending}
      onCancel={onCancel}
      onConfirm={onConfirm}
    >
      {policyControls
        ? "While the image update policy is on, every authenticated push of a watched tag requests a fresh image check, and the policy decides whether to notify or deploy. If the policy is turned off, pushes queue a deploy that pulls the resource’s configured image tags and redeploys, replacing its containers. You become the webhook’s owner, so pushes are acted on only while you keep Edit configuration and Operate access. If someone else owned it, their pending webhook deploys are cancelled."
        : "Every authenticated push of a watched tag queues a deploy that pulls the resource’s configured image tags and redeploys, replacing its containers. Approval, freeze windows, deploy windows and quotas still apply, and a failed deploy is not retried automatically. You become the webhook’s owner, so pushes deploy only while you keep Edit configuration and Operate access. If someone else owned it, their pending webhook deploys are cancelled."}
    </ConfirmDialog>
  );
}

function WebhookFields({ status, draft, errors, enableError, canOperate, policyControls, onChange }: { status: RegistryWebhookStatus; draft: WebhookDraft; errors: FilterErrors; enableError?: string; canOperate: boolean; policyControls: boolean; onChange: (draft: WebhookDraft) => void }) {
  const id = useId();
  const server = status.registry?.server;
  const providerHint = providerError(draft.provider, server) ?? "Changing the provider issues a new secret and needs the registry set up again.";
  return (
    <Subsection
      title={status.configuration ? "Settings" : "Create a webhook"}
      description={
        <>
          For <span className="font-mono">{server}</span>. Only image-only Compose services can be redeployed: no Git source or build sections, and at least one image that uses a tag.
        </>
      }
    >
      <Field id={`${id}-provider`} label="Provider" error={errors.provider} hint={providerHint}>
        <Select
          id={`${id}-provider`}
          value={draft.provider}
          aria-labelledby={`${id}-provider-label`}
          aria-invalid={errors.provider ? true : undefined}
          aria-describedby={errors.provider ? `${id}-provider-error` : `${id}-provider-hint`}
          onChange={(event) => onChange({ ...draft, provider: event.target.value as RegistryWebhookProvider })}
        >
          {Object.entries(providers).map(([value, provider]) => (
            <option key={value} value={value}>
              {provider.label}
            </option>
          ))}
        </Select>
      </Field>
      <SwitchField
        id={`${id}-enabled`}
        label="Pull and redeploy on push"
        checked={draft.enabled}
        disabled={(!status.worker_supported || !canOperate) && !draft.enabled}
        onChange={(event) => onChange({ ...draft, enabled: event.target.checked })}
        aria-invalid={enableError ? true : undefined}
        description={enableError ? <span className="text-bad">{enableError}</span> : [canOperate ? "" : "Turning on automatic deploys needs Operate access as well as Edit configuration.", "Off keeps the webhook and its secret, ignores pushes and cancels queued webhook deploys.", policyControls ? "While the image update policy is on, a push requests an image check instead of a deploy." : ""].filter(Boolean).join(" ")}
      />
      <FilterEditor filters={draft.filters} errors={errors} onChange={(filters) => onChange({ ...draft, filters })} />
    </Subsection>
  );
}

function FilterEditor({ filters, errors, onChange }: { filters: FilterDraft[]; errors: FilterErrors; onChange: (filters: FilterDraft[]) => void }) {
  const id = useId();
  const full = filters.length >= maxWebhookFilters;
  const update = (key: string, patch: Partial<FilterDraft>) => onChange(filters.map((item) => (item.key === key ? { ...item, ...patch } : item)));
  return (
    <fieldset className="min-w-0 space-y-2" aria-describedby={`${id}-hint`}>
      <legend className="text-[13px] font-medium text-fg">Watched tags</legend>
      <p id={`${id}-hint`} className="text-xs text-fg-muted">
        Exact repository paths on this registry, without the host. Use * as the tag to watch every tag. Up to {maxWebhookFilters}.
      </p>
      <ul className="space-y-3 @sm:space-y-2">
        {filters.map((filter, index) => (
          <FilterRow key={filter.key} filter={filter} index={index} errors={errors} removable={filters.length > 1} onChange={(patch) => update(filter.key, patch)} onRemove={() => onChange(filters.filter((item) => item.key !== filter.key))} />
        ))}
      </ul>
      {errors.filters && <p className="text-xs text-bad">{errors.filters}</p>}
      <Button size="sm" onClick={() => onChange([...filters, filterDraft()])} disabled={full} title={full ? `At most ${maxWebhookFilters} filters` : undefined}>
        <Plus aria-hidden className="size-3.5" />
        Add tag
      </Button>
    </fieldset>
  );
}

function FilterRow({ filter, index, errors, removable, onChange, onRemove }: { filter: FilterDraft; index: number; errors: FilterErrors; removable: boolean; onChange: (patch: Partial<FilterDraft>) => void; onRemove: () => void }) {
  const errorId = useId();
  const repositoryError = errors[`${filter.key}.repository`];
  const tagError = errors[`${filter.key}.tag`];
  const describedBy = repositoryError || tagError ? errorId : undefined;
  return (
    <li className="min-w-0">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 @sm:grid-cols-[minmax(0,1fr)_9rem_auto]">
        <input
          className={`${inputClass} col-span-2 font-mono @sm:col-span-1`}
          value={filter.repository}
          placeholder="team/app"
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="none"
          aria-label={`Repository ${index + 1}`}
          aria-invalid={repositoryError ? true : undefined}
          aria-describedby={describedBy}
          onChange={(event) => onChange({ repository: event.target.value })}
        />
        <input
          className={`${inputClass} font-mono`}
          value={filter.tag}
          placeholder="latest or *"
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="none"
          aria-label={`Tag ${index + 1}`}
          aria-invalid={tagError ? true : undefined}
          aria-describedby={describedBy}
          onChange={(event) => onChange({ tag: event.target.value })}
        />
        <Button variant="ghost" className="px-2" aria-label={`Remove tag ${index + 1}`} title="Remove" disabled={!removable} onClick={onRemove}>
          <X aria-hidden className="size-3.5" />
        </Button>
      </div>
      {describedBy && (
        <p id={errorId} className="mt-1 text-xs break-words text-bad">
          {[repositoryError, tagError].filter(Boolean).join(" ")}
        </p>
      )}
    </li>
  );
}
