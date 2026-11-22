import { useMutation, useQuery, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, request } from "../api/client";
import { providerSourceQuery } from "../api/git-providers";
import { gitDeployKeyQuery, githubConnectionQuery, keys } from "../api/queries";
import type { GitDeployKey as DeployKey, Workload } from "../api/types";
import { describedBy, inputClass } from "../lib/styles";
import { isSSHRepository, validateHostKey, validateRepository } from "../lib/workload-form";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { HostKeyField } from "./WorkloadForm";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const title = "SSH deploy key";
const description = "A key pair Cove generates for this resource to clone an ssh:// repository. Only the public key is shown; the private key is stored encrypted.";
// The server clears the plain webhook secret only when the key moves the source to another repository.
function webhookEffect(savedRepository: string | undefined, repository: string) {
  return savedRepository === repository.trim()
    ? "The repository is unchanged, so the automatic-deployment webhook keeps working."
    : "The source repository changes, so the automatic-deployment webhook is turned off and needs a new secret if you use it.";
}

interface KeyInput {
  repository: string;
  host_key: string;
}

type KeyErrors = Partial<Record<keyof KeyInput, string>>;

function validateKeyInput(repository: string, hostKey: string): { input?: KeyInput; errors: KeyErrors } {
  const errors: KeyErrors = {};
  const url = repository.trim();
  const pin = hostKey.trim();
  const repositoryError = validateRepository(url) ?? (isSSHRepository(url) ? null : "Use an ssh:// URL, as in ssh://git@github.com/acme/app.git.");
  const hostKeyError = validateHostKey(pin);
  if (repositoryError) errors.repository = repositoryError;
  if (hostKeyError) errors.host_key = hostKeyError;
  return Object.keys(errors).length > 0 ? { errors } : { input: { repository: url, host_key: pin }, errors };
}

// Starts from the saved SSH source so the key matches what deployments will use.
function initialInput(workload: Workload, key?: DeployKey): KeyInput {
  const source = workload.source;
  if (source && isSSHRepository(source.repository)) return { repository: source.repository, host_key: source.ssh_host_key ?? "" };
  return { repository: key?.repository ?? "", host_key: key?.host_key ?? "" };
}

export function GitDeployKey({ workload, blocked, blockers }: { workload: Workload; blocked: boolean; blockers: ReactNode }) {
  const client = useQueryClient();
  const path = `/api/workloads/${encodeURIComponent(workload.id)}/git-deploy-key`;
  const key = useQuery(gitDeployKeyQuery(workload.id));
  const github = useQuery(githubConnectionQuery(workload.id));
  const provider = useQuery(providerSourceQuery(workload.id));
  const [status, setStatus] = useState<string | null>(null);
  // Creating a key rewrites the saved source and can clear the webhook secret, so both are reloaded.
  const refresh = () => Promise.all([client.invalidateQueries({ queryKey: keys.workload(workload.id) }), client.invalidateQueries({ queryKey: ["webhook", workload.id] })]);
  const generate = useMutation({
    mutationFn: (input: KeyInput) => request<DeployKey>("POST", path, input),
    onSuccess: (data) => client.setQueryData(keys.gitDeployKey(workload.id), data),
    onSettled: refresh,
  });
  const remove = useMutation({
    mutationFn: () => request<{ ok: boolean }>("DELETE", path),
    onSuccess: () => client.setQueryData(keys.gitDeployKey(workload.id), null),
    onSettled: refresh,
  });

  if (!workload.source && !key.data) return null;
  if (key.isPending) {
    return (
      <Section width="form" title={title} description={description}>
        <LoadingRows label="Loading deploy key" rows={2} />
      </Section>
    );
  }
  if (key.isError) {
    return (
      <Section width="form" title={title} description={description}>
        <QueryError what="the deploy key" error={key.error} onRetry={() => void key.refetch()} retrying={key.isFetching} />
      </Section>
    );
  }

  // The server refuses deploy-key changes while a managed connection authenticates the source.
  const managed = github.data ? "the GitHub App" : provider.data ? "a Git provider" : null;
  const managedNotice = managed && (
    <Notice tone="warn" title={`Connected through ${managed}`}>
      Disconnect {managed} below before changing the SSH deploy key.
    </Notice>
  );
  const props = { workload, blocked: blocked || managed !== null, notices: <>{blockers}{managedNotice}</>, status, setStatus, generate };
  return key.data ? <ExistingKey {...props} deployKey={key.data} remove={remove} /> : <CreateKey {...props} />;
}

interface KeyProps {
  workload: Workload;
  blocked: boolean;
  notices: ReactNode;
  status: string | null;
  setStatus: (status: string | null) => void;
  generate: UseMutationResult<DeployKey, Error, KeyInput>;
}

function KeyFields({ input, errors, onChange }: { input: KeyInput; errors: KeyErrors; onChange: (input: KeyInput) => void }) {
  const id = useId();
  return (
    <>
      <Field id={`${id}-repository`} label="SSH repository URL" error={errors.repository} hint="Saved as the source repository. Branch, Compose file and watch paths are kept.">
        <input
          id={`${id}-repository`}
          className={`${inputClass} font-mono`}
          value={input.repository}
          onChange={(event) => onChange({ ...input, repository: event.target.value })}
          placeholder="ssh://git@github.com/acme/app.git"
          inputMode="url"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          aria-invalid={errors.repository ? true : undefined}
          aria-describedby={describedBy(`${id}-repository`, errors.repository, true)}
        />
      </Field>
      <HostKeyField id={`${id}-host-key`} value={input.host_key} error={errors.host_key} onChange={(host_key) => onChange({ ...input, host_key })} />
    </>
  );
}

function CreateKey({ workload, blocked, notices, status, setStatus, generate }: KeyProps) {
  const [input, setInput] = useState(() => initialInput(workload));
  const [errors, setErrors] = useState<KeyErrors>({});
  const source = workload.source;
  const switching = source && !isSSHRepository(source.repository);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateKeyInput(input.repository, input.host_key);
    setErrors(result.errors);
    if (!result.input) return;
    setStatus(null);
    generate.mutate(result.input, { onSuccess: () => setStatus("Deploy key created. Add the public key to the repository's deploy keys before deploying.") });
  }

  return (
    <form onSubmit={submit} noValidate>
      <Section
        width="form"
        title={title}
        description={description}
        footer={
          <div className="flex justify-end">
            <Button type="submit" pending={generate.isPending} disabled={blocked}>
              Create deploy key
            </Button>
          </div>
        }
      >
        <fieldset disabled={generate.isPending} className="min-w-0 space-y-5">
          {notices}
          {status && <Notice tone="success" title={status} />}
          <p className="text-[13px] text-fg-muted">No deploy key exists for this resource. {source ? "" : "Save a Git source above first. "}Cove does not look up host keys; paste the one you have verified.</p>
          <KeyFields
            input={input}
            errors={errors}
            onChange={(next) => {
              generate.reset();
              setInput(next);
            }}
          />
          <ul className="list-disc space-y-1 pl-5 text-xs text-fg-muted">
            {switching && (
              <li>
                The source changes from <span className="font-mono break-all">{source.repository}</span> to the SSH URL{workload.has_source_token ? " and its saved access token is deleted" : ""}.
              </li>
            )}
            {source && <li>{webhookEffect(source.repository, input.repository)}</li>}
          </ul>
          {generate.isError && (
            <Notice tone="error" title="Could not create the deploy key">
              {errorMessage(generate.error)}
            </Notice>
          )}
        </fieldset>
      </Section>
    </form>
  );
}

function ExistingKey({ workload, deployKey, blocked, notices, status, setStatus, generate, remove }: KeyProps & { deployKey: DeployKey; remove: UseMutationResult<{ ok: boolean }, Error, void> }) {
  const [rotating, setRotating] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const source = workload.source;
  const repositoryMismatch = source && source.repository !== deployKey.repository;
  const hostKeyMismatch = source && !repositoryMismatch && (source.ssh_host_key ?? "") !== deployKey.host_key;

  return (
    <Section width="form" title={title} description={description}>
      <div className="min-w-0 space-y-5">
        {notices}
        {status && <Notice tone="success" title={status} />}
        {(repositoryMismatch || hostKeyMismatch) && (
          <Notice tone="warn" title="Deployments will fail">
            {repositoryMismatch ? "The saved source repository is not the repository this key was created for." : "The saved source host key differs from the key's pinned host key."} Rotate the key with the intended repository and host key, or change the source back.
          </Notice>
        )}
        {!source && (
          <Notice tone="warn" title="No Git source">
            This resource no longer builds from a Git repository, so the key is unused. Delete it here and remove it from the Git host.
          </Notice>
        )}
        <div className="space-y-2">
          <p className="text-[13px] font-medium text-fg">Public key</p>
          <pre aria-label="Public key" className="overflow-x-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-fg">
            {deployKey.public_key}
          </pre>
          <div className="flex flex-wrap items-center gap-2">
            <CopyButton text={deployKey.public_key} label="Copy public key" />
          </div>
          <p className="text-xs text-fg-muted">Add it to the repository's deploy keys with read-only access. Cove never needs write access.</p>
        </div>
        <DefinitionList
          items={[
            { term: "Repository", value: <span className="font-mono text-xs break-all">{deployKey.repository}</span> },
            { term: "Pinned host key", value: <span className="font-mono text-xs break-all">{deployKey.host_key}</span> },
          ]}
        />
        <div className="space-y-2 border-t border-line pt-4">
          <div className="flex flex-wrap gap-2">
            <Button
              disabled={blocked}
              onClick={() => {
                generate.reset();
                setRotating(true);
              }}
            >
              Rotate key
            </Button>
            <Button
              variant="danger"
              disabled={blocked}
              onClick={() => {
                remove.reset();
                setDeleting(true);
              }}
            >
              Delete key
            </Button>
          </div>
          <p className="text-xs text-fg-muted">Rotate replaces the key pair and can update the repository URL or host key. Rotating for the same repository keeps the automatic-deployment webhook. Delete removes the key and leaves the source configured, so deployments fail until a new key exists.</p>
        </div>
      </div>
      <RotateDialog
        open={rotating}
        initial={initialInput(workload, deployKey)}
        savedRepository={source?.repository}
        generate={generate}
        onClose={() => setRotating(false)}
        onRotated={() => {
          setRotating(false);
          setStatus("Key rotated. Add the new public key to the Git host and remove the old one.");
        }}
      />
      <ConfirmDialog
        open={deleting}
        title="Delete deploy key?"
        confirmLabel="Delete key"
        danger
        pending={remove.isPending}
        onCancel={() => setDeleting(false)}
        onConfirm={() =>
          remove.mutate(undefined, {
            onSuccess: () => {
              setDeleting(false);
              setStatus("Deploy key deleted. Remove its public key from the Git host.");
            },
          })
        }
      >
        <div className="space-y-3">
          <p>Deployments of this resource fail until a new key is created. The source repository and host key stay configured. Running containers are not affected.</p>
          <p>Cove cannot remove the public key from the Git host; delete it there separately.</p>
          {remove.isError && (
            <Notice tone="error" title="Could not delete the deploy key">
              {errorMessage(remove.error)}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
    </Section>
  );
}

function RotateDialog({ open, initial, savedRepository, generate, onClose, onRotated }: { open: boolean; initial: KeyInput; savedRepository: string | undefined; generate: KeyProps["generate"]; onClose: () => void; onRotated: () => void }) {
  return (
    <Dialog open={open} title="Rotate deploy key" onClose={onClose}>
      {open && <RotateForm initial={initial} savedRepository={savedRepository} generate={generate} onClose={onClose} onRotated={onRotated} />}
    </Dialog>
  );
}

function RotateForm({ initial, savedRepository, generate, onClose, onRotated }: { initial: KeyInput; savedRepository: string | undefined; generate: KeyProps["generate"]; onClose: () => void; onRotated: () => void }) {
  const [input, setInput] = useState(initial);
  const [errors, setErrors] = useState<KeyErrors>({});

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateKeyInput(input.repository, input.host_key);
    setErrors(result.errors);
    if (result.input) generate.mutate(result.input, { onSuccess: onRotated });
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <p>A new key pair replaces the current one when you confirm. The current public key stops working immediately: add the new one to the Git host, then remove the old one.</p>
      <fieldset disabled={generate.isPending} className="min-w-0 space-y-4">
        <KeyFields
          input={input}
          errors={errors}
          onChange={(next) => {
            generate.reset();
            setInput(next);
          }}
        />
      </fieldset>
      {savedRepository && <p className="text-xs">{webhookEffect(savedRepository, input.repository)}</p>}
      {generate.isError && (
        <Notice tone="error" title="Could not rotate the deploy key">
          {errorMessage(generate.error)}
        </Notice>
      )}
      <div className="-mx-5 -mb-5 flex justify-end gap-2 border-t border-line px-5 py-3">
        <Button variant="ghost" onClick={onClose} disabled={generate.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" pending={generate.isPending}>
          Rotate key
        </Button>
      </div>
    </form>
  );
}
