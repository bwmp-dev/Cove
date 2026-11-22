import { Select } from "./Select";
import { ResourcePlacement } from "./ResourcePlacement";
import type { ResourceDraft } from "../api/templates";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import type { Project } from "../api/projects";
import type { Node, WorkloadInput } from "../api/types";
import { formatBytes } from "../lib/format";
import { byteLength, composeLimit, formValues, routeRows, validateRoutes, validateWorkload, type EnvRow, type SourceKind, type WorkloadFormErrors, type WorkloadFormValues } from "../lib/workload-form";
import { ComposeEditor } from "./ComposeEditor";
import { EnvironmentEditor } from "./EnvironmentEditor";
import { RouteEditor } from "./RouteEditor";
import { nodeStates } from "../lib/nodes";
import { Button, Field, Notice } from "./ui";
import { buttonClass, describedBy, inputClass } from "../lib/styles";

interface Choice<T extends string> {
  value: T;
  label: string;
  description: string;
}

function ChoiceGroup<T extends string>({ legend, name, value, choices, onChange }: { legend: string; name: string; value: T; choices: Choice<T>[]; onChange: (value: T) => void }) {
  return (
    <fieldset>
      <legend className="mb-1.5 text-[13px] font-medium text-fg">{legend}</legend>
      <div className="divide-y divide-line overflow-hidden rounded-md border border-line-strong sm:flex sm:divide-x sm:divide-y-0">
        {choices.map((choice) => (
          <label
            key={choice.value}
            className="flex flex-1 cursor-pointer items-start gap-2.5 px-3 py-2.5 hover:bg-panel has-[:checked]:bg-raised has-[:focus-visible]:outline-2 has-[:focus-visible]:-outline-offset-2 has-[:focus-visible]:outline-info"
          >
            <input
              type="radio"
              name={name}
              value={choice.value}
              checked={value === choice.value}
              onChange={() => onChange(choice.value)}
              className="mt-0.5 accent-fg focus-visible:outline-none"
            />
            <span className="text-[13px] font-medium text-fg">
              {choice.label}
              <span className="mt-0.5 block text-xs font-normal text-fg-muted">{choice.description}</span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

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

export function SourceFields({ values, errors, set, tokenHint, tokenSaved = false }: { values: WorkloadFormValues; errors: WorkloadFormErrors; set: SetValue; tokenHint: string; tokenSaved?: boolean }) {
  const id = useId();
  const [revealToken, setRevealToken] = useState(false);
  const composeBytes = byteLength(values.compose);
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
          <Field id={`${id}-repo`} label="Repository URL" error={errors.repository} hint="HTTPS only. Do not include credentials in the URL.">
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
          </Field>
          <ChoiceGroup legend="Deployment configuration" name={`${id}-git-compose`} value={values.inlineCompose ? "panel" : "repository"} choices={[
            { value: "repository", label: "File in repository", description: "Use the Compose file committed with your code." },
            { value: "panel", label: "Manage in Cove", description: "Keep Compose here and build from the checked-out repository. Previews require a repository file." },
          ]} onChange={(value) => set("inlineCompose", value === "panel")} />
          <div className="grid gap-5 sm:grid-cols-2">
            <Field id={`${id}-branch`} label="Branch" error={errors.branch}>
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
            </Field>
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
          <Field id={`${id}-token`} label="Access token" optional error={errors.token} hint={tokenHint}>
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
          </Field>
        </>
      )}
    </>
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
  onSubmit: (input: WorkloadInput) => void;
}

// Creates a resource. Existing resources are edited section by section on the resource pages.
export function WorkloadForm({ draft, nodes, projects, initialEnvironmentId, initialProjectId, pending, serverError, submitLabel, cancel, onSubmit }: WorkloadFormProps) {
  const [values, setValues] = useState(() => formValues(undefined, draft));
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const [routeErrors, setRouteErrors] = useState<Record<string, string>>({});
  const [resourcePlacement, setResourcePlacement] = useState(initialProjectId ? `${initialProjectId}:${initialEnvironmentId}` : "");
  const [routes, setRoutes] = useState(() => routeRows(draft?.routes));
  const id = useId();

  const set: SetValue = (key, value) => setValues((current) => ({ ...current, [key]: value }));

  const readyNodes = nodes.filter((node) => node.state === "ready");
  const fileSecrets = draft?.file_secrets ?? [];

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateWorkload(values, "create");
    const checkedRoutes = values.kind === "job" ? { routes: [], errors: {} } : validateRoutes(routes);
    setErrors(result.errors);
    setRouteErrors(checkedRoutes.errors);
    if (result.input && checkedRoutes.routes && resourcePlacement) {
      const [projectId, environmentId] = resourcePlacement.split(":");
      const input: WorkloadInput = { ...result.input, routes: checkedRoutes.routes, project_id: projectId, environment_id: environmentId };
      if (fileSecrets.length > 0) input.file_secrets = fileSecrets;
      onSubmit(input);
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
          <SourceFields values={values} errors={errors} set={set} tokenHint="Needed for private repositories. Stored encrypted and never shown again." />
        </FormSection>

        {values.kind === "service" && (
          <FormSection
            title="Routes"
            description="The worker serves HTTPS directly. Point DNS at this node and make ports 80 and 443 available; deployment provisions a managed Traefik proxy. Existing services occupying those ports are not replaced."
          >
            <RouteEditor routes={routes} errors={routeErrors} onChange={setRoutes} />
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
