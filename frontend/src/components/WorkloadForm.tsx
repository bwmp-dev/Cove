import { Select } from "./Select";
import { ChoiceGroup, type Choice } from "./ChoiceGroup";
import { ResourcePlacement } from "./ResourcePlacement";
import type { ResourceDraft } from "../api/templates";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import type { Project } from "../api/projects";
import type { Node, WorkloadInput } from "../api/types";
import { formatBytes } from "../lib/format";
import { byteLength, composeLimit, formValues, isSSHRepository, routeRows, validateRoutes, validateWorkload, type EnvRow, type SourceKind, type WorkloadFormErrors, type WorkloadFormValues } from "../lib/workload-form";
import { ComposeEditor } from "./ComposeEditor";
import { EnvironmentEditor } from "./EnvironmentEditor";
import { RouteEditor } from "./RouteEditor";
import { nodeStates } from "../lib/nodes";
import { Button, Field, Notice, SwitchField } from "./ui";
import { GitProviderPicker } from "./GitProviderPicker";
import { automaticDescription, emptySelection, reportingDescription, type ProviderRepository, type ProviderSelection, type ProviderSourceInput } from "../api/git-providers";
import { buttonClass, describedBy, inputClass } from "../lib/styles";

function FormSection({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <section className="grid gap-x-8 gap-y-3 px-4 pt-5 sm:px-6 lg:grid-cols-[14rem_minmax(0,1fr)]">
      <div>
        <h2 className="text-sm font-semibold text-fg">{title}</h2>
        {description && <p className="mt-1 text-xs leading-relaxed text-fg-muted">{description}</p>}
      </div>
      <div className="max-w-3xl min-w-0 space-y-5 rounded-lg border border-line bg-surface p-4">{children}</div>
    </section>
  );
}

const kindChoices: Choice<"service" | "job">[] = [
  { value: "service", label: "Service", description: "Long-running containers that stay up after deploy." },
  { value: "job", label: "Job", description: "Runs to completion on each deploy; exit code 0 means success." },
];

const sourceChoices: Choice<SourceKind>[] = [
  { value: "compose", label: "Compose file", description: "Paste the Compose YAML directly." },
  { value: "git", label: "Git repository", description: "Clone a branch over HTTPS on each deploy." },
];

type SetValue = <K extends keyof WorkloadFormValues>(key: K, value: WorkloadFormValues[K]) => void;

export function TypeFields({ values, errors, set, kindHint }: { values: WorkloadFormValues; errors: WorkloadFormErrors; set: SetValue; kindHint?: ReactNode }) {
  const id = useId();
  return (
    <>
      <ChoiceGroup legend="Resource type" name={`${id}-kind`} value={values.kind} choices={kindChoices} onChange={(value) => set("kind", value)} />
      {kindHint && <p className="-mt-3 text-xs text-fg-muted">{kindHint}</p>}
      {values.kind === "job" && (
        <Field
          id={`${id}-job`}
          label="Job service"
          error={errors.jobService}
          hint="The Compose service whose exit code decides whether the run succeeded. Start and restart do not apply to jobs; deploy runs the job again."
        >
          <input
            id={`${id}-job`}
            className={`${inputClass} font-mono sm:max-w-xs`}
            value={values.jobService}
            onChange={(event) => set("jobService", event.target.value)}
            placeholder="migrate"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-invalid={errors.jobService ? true : undefined}
            aria-describedby={describedBy(`${id}-job`, errors.jobService, true)}
          />
        </Field>
      )}
    </>
  );
}

// The pin is supplied by the operator; Cove never discovers or trusts a host key on first use.
export function HostKeyField({ id, value, error, onChange }: { id: string; value: string; error?: string; onChange: (value: string) => void }) {
  return (
    <Field id={id} label="Host public key" error={error} hint="One line from the Git host, such as ssh-ed25519 AAAA…. Check it against the fingerprints your Git host publishes. Deployments refuse any other host key.">
      <textarea
        id={id}
        className={`${inputClass} font-mono break-all`}
        rows={2}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder="ssh-ed25519 AAAA…"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
      />
    </Field>
  );
}

// `access` chooses how the repository is reached; `provider` replaces the repository URL, branch and
// credential fields when a Git provider connection supplies them.
export function SourceFields({ values, errors, set, tokenHint, tokenSaved = false, access, provider }: { values: WorkloadFormValues; errors: WorkloadFormErrors; set: SetValue; tokenHint: string; tokenSaved?: boolean; access?: ReactNode; provider?: ReactNode }) {
  const id = useId();
  const [revealToken, setRevealToken] = useState(false);
  const composeBytes = byteLength(values.compose);
  const ssh = !provider && isSSHRepository(values.repository);
  const composeEditor = (
    <Field id={`${id}-compose`} label="Compose file" error={errors.compose} hint={`${formatBytes(composeBytes)} of ${formatBytes(composeLimit())}`}>
      <ComposeEditor
        id={`${id}-compose`}
        labelId={`${id}-compose-label`}
        value={values.compose}
        onChange={(value) => set("compose", value)}
        placeholder={'services:\n  web:\n    image: nginx:1.27\n    ports:\n      - "80:80"'}
        invalid={Boolean(errors.compose)}
        describedBy={describedBy(`${id}-compose`, errors.compose, true)}
      />
    </Field>
  );
  return (
    <>
      <ChoiceGroup legend="Compose source" name={`${id}-source`} value={values.sourceKind} choices={sourceChoices} onChange={(value) => set("sourceKind", value)} />
      {values.sourceKind === "compose" ? composeEditor : (
        <>
          {access}
          {provider ?? <Field id={`${id}-repo`} label="Repository URL" error={errors.repository} hint="HTTPS, or ssh://user@host/path to deploy with an SSH deploy key. Do not include credentials in the URL.">
            <input
              id={`${id}-repo`}
              className={`${inputClass} font-mono`}
              value={values.repository}
              onChange={(event) => set("repository", event.target.value)}
              placeholder="https://github.com/acme/app.git"
              inputMode="url"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-invalid={errors.repository ? true : undefined}
              aria-describedby={describedBy(`${id}-repo`, errors.repository, true)}
            />
          </Field>}
          <ChoiceGroup legend="Deployment configuration" name={`${id}-git-compose`} value={values.inlineCompose ? "panel" : "repository"} choices={[
            { value: "repository", label: "File in repository", description: "Use the Compose file committed with your code." },
            { value: "panel", label: "Manage in Cove", description: "Keep Compose here and build from the checked-out repository. Previews require a repository file." },
          ]} onChange={(value) => set("inlineCompose", value === "panel")} />
          <div className="grid gap-5 sm:grid-cols-2">
            {!provider && <Field id={`${id}-branch`} label="Branch" error={errors.branch}>
              <input
                id={`${id}-branch`}
                className={`${inputClass} font-mono`}
                value={values.branch}
                onChange={(event) => set("branch", event.target.value)}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                aria-invalid={errors.branch ? true : undefined}
                aria-describedby={describedBy(`${id}-branch`, errors.branch)}
              />
            </Field>}
            {!values.inlineCompose && <Field id={`${id}-path`} label="Compose file path" error={errors.composePath} hint="Relative to the repository root.">
              <input
                id={`${id}-path`}
                className={`${inputClass} font-mono`}
                value={values.composePath}
                onChange={(event) => set("composePath", event.target.value)}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                aria-invalid={errors.composePath ? true : undefined}
                aria-describedby={describedBy(`${id}-path`, errors.composePath, true)}
              />
            </Field>}
          </div>
          {values.inlineCompose && composeEditor}
          <Field id={`${id}-watch`} label="Watch paths" optional error={errors.watchPaths} hint="One repository-relative pattern per line. Supports *, **, ?, and ! exclusions; last match wins. Empty deploys every push. Unknown changes still deploy. Manual deployments are unaffected. Ordered sequences run together when any member matches.">
            <textarea id={`${id}-watch`} className={`${inputClass} font-mono`} rows={3} aria-invalid={errors.watchPaths ? true : undefined} aria-describedby={describedBy(`${id}-watch`, errors.watchPaths, true)} value={values.watchPaths ?? ""} onChange={(event) => set("watchPaths", event.target.value)} spellCheck={false} />
          </Field>
          {ssh && (
            <>
              <HostKeyField id={`${id}-host-key`} value={values.sshHostKey} error={errors.sshHostKey} onChange={(value) => set("sshHostKey", value)} />
              <p className="text-xs text-fg-muted">Deployments authenticate with a read-only deploy key that Cove generates for this resource. Once the source is saved, create the key under SSH deploy key on the Source page and add its public key to the repository.</p>
            </>
          )}
          {!provider && (!ssh || values.token !== "") && <Field id={`${id}-token`} label="Access token" optional error={errors.token} hint={tokenHint}>
            <div className="flex gap-2">
              <input
                id={`${id}-token`}
                className={`${inputClass} font-mono`}
                type={revealToken ? "text" : "password"}
                value={values.token}
                onChange={(event) => set("token", event.target.value)}
                placeholder={tokenSaved ? "Saved token kept" : ""}
                autoComplete="off"
                spellCheck={false}
                data-1p-ignore
                data-lpignore="true"
                aria-invalid={errors.token ? true : undefined}
                aria-describedby={describedBy(`${id}-token`, errors.token, true)}
              />
              <Button variant="ghost" onClick={() => setRevealToken((value) => !value)} aria-pressed={revealToken}>
                {revealToken ? "Hide" : "Show"}
              </Button>
            </div>
          </Field>}
        </>
      )}
    </>
  );
}

export type ProviderLink = Pick<ProviderSourceInput, "connection_id" | "repository_id" | "automatic" | "reporting">;

const accessChoices: Choice<"url" | "provider">[] = [
  { value: "url", label: "Repository URL", description: "HTTPS with an optional access token, or SSH with a deploy key." },
  { value: "provider", label: "Git provider", description: "Choose a repository from a GitLab, Gitea, Forgejo, Codeberg or Bitbucket Cloud connection." },
];

function ProviderFields({ selection, errors, automatic, reporting, onSelection, onRepository, onAutomatic, onReporting }: {
  selection: ProviderSelection;
  errors: WorkloadFormErrors;
  automatic: boolean;
  reporting: boolean;
  onSelection: (selection: ProviderSelection) => void;
  onRepository: (repository: ProviderRepository | undefined) => void;
  onAutomatic: (value: boolean) => void;
  onReporting: (value: boolean) => void;
}) {
  const id = useId();
  const error = errors.repository ?? errors.branch;
  return (
    <div className="space-y-4">
      <GitProviderPicker value={selection} onChange={onSelection} onRepository={onRepository} />
      {error && <p role="alert" className="text-xs text-bad">{error}</p>}
      <SwitchField id={`${id}-automatic`} checked={automatic} onChange={(event) => onAutomatic(event.target.checked)} label="Deploy pushes automatically" description={automaticDescription} />
      <SwitchField id={`${id}-reporting`} checked={reporting} onChange={(event) => onReporting(event.target.checked)} label="Report deployments" description={reportingDescription} />
    </div>
  );
}

interface WorkloadFormProps {
  draft?: ResourceDraft;
  nodes: Node[];
  projects: Project[];
  initialEnvironmentId: string;
  initialProjectId: string;
  pending: boolean;
  serverError: string | null;
  submitLabel: string;
  cancel: ReactNode;
  // `provider` is connected after the resource is created, because the connection is saved per resource.
  onSubmit: (input: WorkloadInput, provider?: ProviderLink) => void;
}

// Creates a resource. Existing resources are edited section by section on the resource pages.
export function WorkloadForm({ draft, nodes, projects, initialEnvironmentId, initialProjectId, pending, serverError, submitLabel, cancel, onSubmit }: WorkloadFormProps) {
  const [values, setValues] = useState(() => formValues(undefined, draft));
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const [routeErrors, setRouteErrors] = useState<Record<string, string>>({});
  const [resourcePlacement, setResourcePlacement] = useState(initialProjectId ? `${initialProjectId}:${initialEnvironmentId}` : "");
  const [routes, setRoutes] = useState(() => routeRows(draft?.routes));
  const [access, setAccess] = useState<"url" | "provider">("url");
  const [selection, setSelection] = useState<ProviderSelection>(emptySelection);
  const [automatic, setAutomatic] = useState(true);
  const [reporting, setReporting] = useState(false);
  const id = useId();

  const set: SetValue = (key, value) => setValues((current) => ({ ...current, [key]: value }));

  function chooseAccess(next: "url" | "provider") {
    setAccess(next);
    setSelection(emptySelection);
    setValues((current) => ({ ...current, repository: "", branch: next === "url" ? "main" : "", token: "", sshHostKey: "" }));
  }

  const readyNodes = nodes.filter((node) => node.state === "ready");
  const fileSecrets = draft?.file_secrets ?? [];
  const selectedNode = nodes.find((node) => node.id === values.nodeId);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const linked = values.sourceKind === "git" && access === "provider";
    const result = validateWorkload(linked ? { ...values, token: "", sshHostKey: "" } : values, "create");
    if (linked && (selection.connectionId === null || selection.repositoryId === null || !selection.branch)) {
      result.errors.repository = "Select a connection, repository and branch.";
      delete result.input;
    }
    const checkedRoutes = values.kind === "job" ? { routes: [], errors: {} } : validateRoutes(routes);
    setErrors(result.errors);
    setRouteErrors(checkedRoutes.errors);
    if (result.input && checkedRoutes.routes && resourcePlacement) {
      const [projectId, environmentId] = resourcePlacement.split(":");
      const provider = linked && selection.connectionId !== null && selection.repositoryId !== null ? { connection_id: selection.connectionId, repository_id: selection.repositoryId, automatic, reporting } : undefined;
      const input: WorkloadInput = { ...result.input, routes: checkedRoutes.routes, project_id: projectId, environment_id: environmentId };
      if (fileSecrets.length > 0) input.file_secrets = fileSecrets;
      onSubmit(input, provider);
    } else {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
    }
  }

  const environment = values.environment ?? [];

  return (
    <form onSubmit={submit} noValidate>
      <fieldset disabled={pending} className="min-w-0">
        <FormSection title="General" description="Where the resource is listed and which node runs it.">
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            <input
              id={`${id}-name`}
              className={inputClass}
              value={values.name}
              onChange={(event) => set("name", event.target.value)}
              maxLength={120}
              autoComplete="off"
              aria-invalid={errors.name ? true : undefined}
              aria-describedby={describedBy(`${id}-name`, errors.name)}
            />
          </Field>
          <ResourcePlacement projects={projects} value={resourcePlacement} onChange={setResourcePlacement} />
          {readyNodes.length === 0 ? (
            <Notice
              tone="warn"
              title="No ready nodes"
              action={
                <Link to="/nodes" className={buttonClass("secondary")}>
                  Open nodes
                </Link>
              }
            >
              A resource needs a node that has passed SSH verification with Docker Compose available.
            </Notice>
          ) : (
            <Field id={`${id}-node`} label="Node" error={errors.nodeId} hint="The Compose stack runs entirely on this node.">
              <Select
                id={`${id}-node`}
                className={inputClass}
                value={values.nodeId}
                onChange={(event) => set("nodeId", event.target.value)}
                aria-invalid={errors.nodeId ? true : undefined}
                aria-describedby={describedBy(`${id}-node`, errors.nodeId, true)}
              >
                <option value="">Select a node</option>
                {nodes.map((node) => (
                  <option key={node.id} value={node.id} disabled={node.state !== "ready"}>
                    {node.state === "ready" ? node.name : `${node.name} (${nodeStates[node.state]?.label ?? node.state})`}
                  </option>
                ))}
              </Select>
            </Field>
          )}
        </FormSection>

        <FormSection title="Type" description="Decides how deploys are run and how their results are reported.">
          <TypeFields values={values} errors={errors} set={set} />
        </FormSection>

        <FormSection title="Source">
          <SourceFields
            values={values}
            errors={errors}
            set={set}
            tokenHint="Needed for private repositories. Stored encrypted and never shown again."
            access={<ChoiceGroup legend="Repository access" name={`${id}-access`} value={access} choices={accessChoices} onChange={chooseAccess} />}
            provider={
              access === "provider" ? (
                <ProviderFields
                  selection={selection}
                  errors={errors}
                  automatic={automatic}
                  reporting={reporting}
                  onSelection={(next) => {
                    setSelection(next);
                    set("branch", next.branch);
                  }}
                  onRepository={(repository) => set("repository", repository?.clone_url ?? "")}
                  onAutomatic={setAutomatic}
                  onReporting={setReporting}
                />
              ) : undefined
            }
          />
        </FormSection>

        {values.kind === "service" && (
          <FormSection
            title="Routes"
            description="The worker serves HTTPS directly. Point DNS at this node and make ports 80 and 443 available; deployment provisions a managed Traefik proxy. Existing services occupying those ports are not replaced."
          >
            <RouteEditor
              routes={routes}
              errors={routeErrors}
              policies={selectedNode ? Boolean(selectedNode.capabilities?.route_policies) : undefined}
              extensions={selectedNode ? (selectedNode.capabilities?.route_protocol ?? 0) >= 2 : undefined}
              onChange={setRoutes}
            />
          </FormSection>
        )}

        <FormSection title="Environment" description="Delivered to Compose on the node at deploy time. Values are encrypted at rest and never shown again after saving.">
          <EnvironmentEditor rows={environment} errors={errors.rows} onChange={(rows: EnvRow[]) => set("environment", rows)} />
          {errors.environment && <p className="text-xs text-bad">{errors.environment}</p>}
          {fileSecrets.length > 0 && (
            <div className="space-y-1 text-xs text-fg-muted">
              <p>The template mounts these variables as files instead of exposing them as environment values. Keep the variables listed here.</p>
              <ul className="space-y-0.5">
                {fileSecrets.map((file) => (
                  <li key={file.name} className="break-all">
                    <span className="font-mono text-fg">{file.variable}</span> → <span className="font-mono">{file.name}</span>
                    {file.services.length > 0 && ` in ${file.services.join(", ")}`}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </FormSection>
      </fieldset>

      <div className="space-y-4 px-4 py-5 sm:px-6 lg:pl-[calc(14rem+3.5rem)]">
        {(errors.form || serverError) && (
          <div className="max-w-3xl">
            <Notice tone="error" title="Could not create the resource">
              {errors.form ?? serverError}
            </Notice>
          </div>
        )}
        <div className="flex items-center gap-2">
          <Button type="submit" variant="primary" pending={pending} disabled={readyNodes.length === 0 || !resourcePlacement}>
            {submitLabel}
          </Button>
          {cancel}
        </div>
      </div>
    </form>
  );
}
