import { Select } from "../components/Select";
import { formText } from "../lib/form-data";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { useUpdateRoutes, useUpdateWorkload } from "../api/mutations";
import { keys, nodesQuery, operationQuery, workloadOperationsQuery } from "../api/queries";
import type { Workload, WorkloadInput, DeploymentHooks, FileSecret, Route, RouteCertificate } from "../api/types";
import { certificateChallengesQuery, certificateIssuersQuery, certificatesQuery, certificateZonesQuery } from "../api/certificates";
import { CertificateChallenges } from "../components/CertificateChallenges";
import { CertificateIssuers } from "../components/CertificateIssuers";
import { EnvironmentEditor } from "../components/EnvironmentEditor";
import { ResourceVariables } from "../components/ResourceVariables";
import { BuiltinVariables, EnvironmentFileActions, SecretReferenceBuilder } from "../components/EnvironmentFile";
import { DatabaseAccess } from "../components/DatabaseAccess";
import { RightSizing } from "../components/RightSizing";
import { ResourceDefaultsNote } from "../components/ResourceDefaults";
import { ResourceGPUs } from "../components/GPUs";
import { isGPUOnly, mergeCPULimits } from "../lib/gpus";
import { GitHubSource } from "../components/GitHubApp";
import { GitDeployKey } from "../components/GitDeployKey";
import { GitProviderSource } from "../components/GitProviderSource";
import { SourceTrigger } from "../components/SourceTrigger";
import { DeployHook } from "../components/DeployHook";
import { ComposeBundleSection } from "../components/ComposeBundle";
import { projectsQuery } from "../api/projects";
import { findPlacement } from "../lib/hierarchy";
import { providerSourceQuery } from "../api/git-providers";
import { Networks } from "../components/Networks";
import { PortBindings } from "../components/PortBindings";
import { PrivateNetwork } from "../components/PrivateNetwork";
import { ResourceOwner } from "../components/ResourceOwner";
import { ResourceRegistry } from "../components/ResourceRegistry";
import { RegistryWebhook } from "../components/RegistryWebhook";
import { ImageScans } from "../components/ImageScans";
import { ImageUpdatePolicy } from "../components/ImageUpdatePolicy";
import { ResourceImages } from "../components/ResourceImages";
import { ResourceBuild } from "../components/ResourceBuild";
import { supportsBuildPublish } from "../lib/builds";
import { supportsImageLifecycle } from "../lib/images";
import { RouteEditor } from "../components/RouteEditor";
import { RouteCertificates } from "../components/RouteCertificates";
import type { CertificateChoices } from "../components/RouteCertificatePicker";
import type { ListenerChoices } from "../components/RouteVisibility";
import { TransportRouteEditor } from "../components/TransportRoutes";
import { isLinkableRoute, refreshTransportErrors, sameTransports, splitRoutes, transportRows, transportSummary, transportTargets, validateTransports, type TransportContext } from "../lib/route-transports";
import { routeListenersQuery, type Listener } from "../api/ingress-listeners";
import { isManagedPreview } from "../lib/previews";
import { routeProtocolOf } from "../lib/nodes";
import { shortId } from "../lib/format";
import { SaveBar, UnsavedChangesGuard } from "../components/SaveBar";
import { Webhook } from "../components/Webhook";
import { SourceFields, TypeFields } from "../components/WorkloadForm";
import { RolloutAvailability, RolloutEligibility, RolloutServiceEditors, RolloutStrategyRows, useRolloutSupport } from "../components/RolloutSettings";
import { UnhealthyRestartSettings } from "../components/UnhealthyRestart";
import { IdleSleepSettings } from "../components/IdleSleep";
import { CpuAutoscaleSettings } from "../components/CpuAutoscale";
import { Button, DefinitionList, EmptyState, Field, Notice, QueryError, Section, SettingRow, SettingRows, Spinner, SwitchField } from "../components/ui";
import { rolloutChanged, rolloutDraft, rolloutsApply, validateRollout, type RolloutDraft, type RolloutErrors } from "../lib/rollouts";
import { useOnline } from "../lib/hooks";
import { actionLabels, activeOperation, isActive, resourceTypeLabel, workloadKind } from "../lib/operations";
import { engineName, exportLabels, isDatabaseExport } from "../lib/databases";
import { can } from "../lib/permissions";
import { buttonClass, describedBy, inputClass } from "../lib/styles";
import { certificateSwitches, createRow, extensionSummary, formValues, isSSHRepository, refreshCertificateErrors, routeLimits, routeRows, sameRoutes, validateRoutes, validateWorkload, visibilitySummary, withoutRouteSecrets, type EnvRow, type RouteRow, type WorkloadFormErrors, type WorkloadFormValues } from "../lib/workload-form";
import { UptimeChecks } from "../components/UptimeChecks";
import { WorkloadMove } from "../components/WorkloadMove";
import { useResource } from "./workload-detail";

const nextDeploy = "Changes apply on the next deploy.";

export function ResourceDeploymentSafety() {
  const resource = useResource();
  return (
    <>
      <DeploymentSafetyForm workload={resource} />
      {rolloutsApply(resource) && <UnhealthyRestartSettings workload={resource} />}
      {rolloutsApply(resource) && <IdleSleepSettings workload={resource} />}
      {rolloutsApply(resource) && <CpuAutoscaleSettings workload={resource} />}
    </>
  );
}

function GuardedDeploymentSwitch({ id, checked, disabled, strategySelected, onChange }: { id: string; checked: boolean; disabled: boolean; strategySelected: boolean; onChange: (checked: boolean) => void }) {
  return (
    <SwitchField
      layout="row"
      id={id}
      checked={checked}
      onChange={(event) => onChange(event.target.checked)}
      disabled={disabled}
      label="Enable guarded deployment"
      description={strategySelected ? guardedWithStrategy : guardedDescription}
    />
  );
}

const guardedDescription =
  "Health-tests a temporary candidate before replacing the application, then recovers the previous release if replacement fails. The candidate receives the application's credentials and can reach its databases. Startup tasks must tolerate overlap. Hooks and database migrations are not undone by recovery.";
const guardedWithStrategy = "Applies only while the replacement strategy is off. With a strategy selected, ineligible deploys always use guarded replacement.";

function DeploymentSafetyForm({ workload }: { workload: Workload }) {
  const id = useId();
  const { save, update } = useSectionSave(workload);
  const blocked = useSaveBlocked(workload.id);
  const editable = can(workload.permissions, "edit");
  const supported = useRolloutSupport(workload).supported;
  const [guarded, setGuarded] = useState(Boolean(workload.guarded_deploy));
  const [draft, setDraft] = useState(() => rolloutDraft(workload.rollout));
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const [rolloutErrors, setRolloutErrors] = useState<RolloutErrors>({});
  const applies = rolloutsApply(workload);
  const strategySelected = applies && draft.strategy !== "disabled";
  const rolloutDirty = applies && rolloutChanged(draft, workload.rollout);
  const dirty = guarded !== Boolean(workload.guarded_deploy) || rolloutDirty;
  const rolloutLocked = !editable || !supported || update.isPending;

  function change(next: (current: RolloutDraft) => RolloutDraft) {
    update.reset();
    setDraft(next);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const rollout = rolloutDirty ? validateRollout(draft, workload.rollout) : { errors: {} };
    setRolloutErrors(rollout.errors);
    if (rolloutDirty && !rollout.options) {
      focusFirstInvalid(form);
      return;
    }
    setErrors(save({}, form, undefined, (input) => {
      input.guarded_deploy = guarded;
      // Sent only when changed: omitting it keeps the saved options, and an unchanged "off" stays unsaved.
      if (rollout.options) input.rollout = rollout.options;
    }));
  }

  function discard() {
    setGuarded(Boolean(workload.guarded_deploy));
    setDraft(rolloutDraft(workload.rollout));
    setErrors({});
    setRolloutErrors({});
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title="Deployment safety"
        description={`How a deploy replaces running containers. ${nextDeploy}`}
        footer={editable && <SaveBar dirty={dirty} pending={update.isPending} blocked={blocked} onDiscard={discard} status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined} />}
      >
        <div className="space-y-5">
          {editable ? <SaveBlockers workload={workload} /> : <ViewOnly />}
          <RolloutAvailability workload={workload} editable={editable} />
          <SettingRows>
            {applies && <RolloutStrategyRows id={id} draft={draft} errors={rolloutErrors} disabled={rolloutLocked} onChange={change} />}
            <GuardedDeploymentSwitch
              id={`${id}-guarded`}
              checked={guarded}
              disabled={!editable || update.isPending}
              strategySelected={strategySelected}
              onChange={(checked) => {
                update.reset();
                setGuarded(checked);
              }}
            />
          </SettingRows>
          {strategySelected ? (
            <>
              <RolloutServiceEditors id={id} draft={draft} errors={rolloutErrors} disabled={rolloutLocked} onChange={change} />
              <RolloutEligibility resourceId={workload.id} />
            </>
          ) : (
            <Notice>
              Guarded deployment is for one stateless service with an enabled Docker healthcheck. Persistent volumes, host ports, fixed container names and privileged options are
              rejected. A brief interruption is possible: it is a health check before replacement, not a traffic handoff.
            </Notice>
          )}
          {Object.keys(rolloutErrors).length > 0 && <Notice tone="error" title="Check the deployment settings">Fix the highlighted fields.</Notice>}
          <FormError errors={errors} error={update.error} />
        </div>
      </Section>
    </form>
  );
}

function ResourceFileSecrets() {
 const workload = useResource();
 const { save, update } = useSectionSave(workload);
 const blocked = useSaveBlocked(workload.id);
 const [rows, setRows] = useState<FileSecret[]>(workload.file_secrets ?? []);
 const [errors, setErrors] = useState<WorkloadFormErrors>({});
 const dirty = JSON.stringify(rows) !== JSON.stringify(workload.file_secrets ?? []);
 function change(index: number, field: keyof FileSecret, value: string) {
  setRows(current => current.map((row, i) => i === index ? { ...row, [field]: field === "services" || field === "build_services" ? value.split(",").map(item => item.trim()) : value } : row));
 }
 function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault();
  const secrets = rows.map(row => ({ ...row, services: row.services.filter(Boolean), build_services: row.build_services.filter(Boolean) }));
  setErrors(save({}, event.currentTarget, undefined, input => { input.file_secrets = secrets; }));
 }
 return <form onSubmit={submit}><UnsavedChangesGuard dirty={dirty} /><Section width="form" title="File and build secrets" description="Mount saved variables into selected services at /run/secrets/NAME. Build secrets use RUN --mount=type=secret,id=NAME in the Dockerfile. Values never become build arguments." footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={blocked} onDiscard={() => setRows(workload.file_secrets ?? [])} status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined} />}>
  <div className="space-y-4">
   <Notice>Values come from the variable store above, including shared references. Do not also map them into container environment variables if you want file-only delivery. Runtime files are readable by the selected container's processes. Workers retain files with releases for rollback; this is not isolation from node administrators.</Notice>
   {rows.map((row, index) => <div key={index} className="grid gap-3 border-b border-line pb-4 sm:grid-cols-2">
    <Field id={`secret-name-${index}`} label="Secret name"><input id={`secret-name-${index}`} className={inputClass} value={row.name} onChange={event => change(index, "name", event.target.value)} required pattern="[A-Za-z_][A-Za-z0-9_]*" /></Field>
    <Field id={`secret-variable-${index}`} label="Saved variable"><Select id={`secret-variable-${index}`} className={inputClass} value={row.variable} onChange={event => change(index, "variable", event.target.value)} required><option value="">Select variable</option>{workload.environment_keys.map(key => <option key={key}>{key}</option>)}</Select></Field>
    <Field id={`secret-runtime-${index}`} label="Runtime services" optional hint="Comma-separated Compose service names"><input id={`secret-runtime-${index}`} className={inputClass} value={row.services.join(",")} onChange={event => change(index, "services", event.target.value)} /></Field>
    <Field id={`secret-build-${index}`} label="Build services" optional hint="Comma-separated services with a build configuration"><input id={`secret-build-${index}`} className={inputClass} value={row.build_services.join(",")} onChange={event => change(index, "build_services", event.target.value)} /></Field>
    <Button size="sm" variant="ghost" className="justify-self-start" onClick={() => setRows(current => current.filter((_, i) => i !== index))}>Remove secret</Button>
   </div>)}
   <Button disabled={rows.length >= 64} onClick={() => setRows(current => [...current, { name: "", variable: "", services: [], build_services: [] }])}>Add file secret</Button>
   <FormError errors={errors} error={update.error} />
  </div>
 </Section></form>;
}

function useActiveOperation(workloadId: string) {
  const operations = useQuery(workloadOperationsQuery(workloadId));
  return operations.data ? activeOperation(operations.data, workloadId) : undefined;
}

function SaveBlockers({ workload }: { workload: Workload }) {
  const online = useOnline();
  const active = useActiveOperation(workload.id);
  if (active) {
    return (
      <Notice tone="warn" title="An operation is in progress">
        Changes cannot be saved until the active {actionLabels[active.action].toLowerCase()} finishes.
      </Notice>
    );
  }
  if (!online) return <Notice tone="warn" title="You are offline">Changes can be saved when the connection returns.</Notice>;
  return null;
}

function useSaveBlocked(workloadId: string) {
  const online = useOnline();
  const active = useActiveOperation(workloadId);
  return !online || Boolean(active);
}

function ViewOnly() {
  return <Notice title="View only">Editing this resource needs editor access. Ask a team owner or admin.</Notice>;
}

// Runs after React renders the new error state, which marks the invalid fields.
function focusFirstInvalid(form: HTMLFormElement) {
  requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
}

// Section forms send the full saved configuration with only their own fields changed. Omitted
// routes and variables are kept by the server, as is a saved Git token for the same repository.
function useSectionSave(workload: Workload) {
  const update = useUpdateWorkload(workload.id);
  const saved = formValues(workload);
  function save(changes: Partial<WorkloadFormValues>, form: HTMLFormElement, onSaved?: () => void, adjust?: (input: WorkloadInput) => void) {
    const result = validateWorkload({ ...saved, ...changes }, "edit");
    if (result.input) {
      adjust?.(result.input);
      update.mutate(result.input, { onSuccess: onSaved });
    } else {
      focusFirstInvalid(form);
    }
    return result.errors;
  }
  return { update, saved, save };
}

function FormError({ errors, error }: { errors: WorkloadFormErrors; error: unknown }) {
  const message = errors.form ?? (error ? errorMessage(error) : null);
  if (!message) return null;
  return (
    <Notice tone="error" title="Could not save the resource">
      {message}
    </Notice>
  );
}

export function ResourceGeneral() {
  const workload = useResource();
  const nodes = useQuery(nodesQuery);
  const nodeName = nodes.data?.find((node) => node.id === workload.node_id)?.name ?? "Node";
  const kind = workloadKind(workload.kind);
  const editable = can(workload.permissions, "edit");
  // Jobs, preview copies and managed databases cannot be moved between nodes.
  const movable = kind === "service" && !workload.preview_source_id && !workload.database;
  if (!editable) {
    return (
      <>
        <Section width="form" title="General">
          <div className="space-y-4">
            <ViewOnly />
            <DefinitionList
              items={[
                { term: "Name", value: workload.name },
                { term: "Type", value: workload.database ? `Database: managed ${engineName(workload.database.engine)} ${workload.database.version}` : kind === "job" ? "Job: runs to completion on each deploy" : "Service: long-running" },
                ...(kind === "job" ? [{ term: "Job service", value: <span className="font-mono text-xs">{workload.job_service}</span> }] : []),
                { term: "Node", value: nodeName },
              ]}
            />
          </div>
        </Section>
        <ResourceOwner id={workload.id} editable={false} />
        {movable && <WorkloadMove workload={workload} />}
      </>
    );
  }
  return (
    <>
      <GeneralForm workload={workload} nodeName={nodeName} />
      <ResourceOwner id={workload.id} editable />
      {movable && <WorkloadMove workload={workload} />}
    </>
  );
}

function GeneralForm({ workload, nodeName }: { workload: Workload; nodeName: string }) {
  const id = useId();
  const { update, saved, save } = useSectionSave(workload);
  const blocked = useSaveBlocked(workload.id);
  const [values, setValues] = useState(saved);
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const set = <K extends keyof WorkloadFormValues>(key: K, value: WorkloadFormValues[K]) => {
    update.reset();
    setValues((current) => ({ ...current, [key]: value }));
  };
  const dirty = values.name !== saved.name || values.kind !== saved.kind || (values.kind === "job" && values.jobService !== saved.jobService);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setErrors(save({ name: values.name, kind: values.kind, jobService: values.jobService }, event.currentTarget));
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title="General"
        description={nextDeploy}
        footer={
          <SaveBar
            dirty={dirty}
            pending={update.isPending}
            blocked={blocked}
            onDiscard={() => {
              setValues(saved);
              setErrors({});
            }}
            status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined}
          />
        }
      >
        <fieldset disabled={update.isPending} className="min-w-0 space-y-5">
          <SaveBlockers workload={workload} />
          <SettingRows>
          <Field layout="row" id={`${id}-name`} label="Name" error={errors.name}>
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
          <SettingRow label="Node" description={workloadKind(workload.kind) === "service" && !workload.preview_source_id ? "Fixed here. Use Move to another node below to transfer the service and its data." : "Fixed after creation."}>
            <span className="text-fg-muted">{nodeName}</span>
          </SettingRow>
          {workload.database ? (
            <SettingRow label="Type" description="The engine and version are fixed. Create another database, or a PostgreSQL copy, for a different version.">
              <span className="text-fg-muted">
                {resourceTypeLabel(workload.kind)} · {engineName(workload.database.engine)} {workload.database.version}
              </span>
            </SettingRow>
          ) : (
            // The type choices are wide cards, so this row spans the full width instead of the right column.
            <div className="space-y-4">
              <TypeFields
                values={values}
                errors={errors}
                set={set}
                kindHint={values.kind === "job" && workload.routes?.length ? "Jobs cannot have domains. Remove this service's domains before converting it." : undefined}
              />
            </div>
          )}
          </SettingRows>
          <FormError errors={errors} error={update.error} />
        </fieldset>
      </Section>
    </form>
  );
}

export function ResourceSource() {
  const workload = useResource();
  if (workload.database) return <ManagedConfiguration workload={workload} />;
  return <SourcePages workload={workload} />;
}

function SourcePages({ workload }: { workload: Workload }) {
  const editable = can(workload.permissions, "edit");
  const blocked = useSaveBlocked(workload.id);
  const provider = useQuery(providerSourceQuery(workload.id));
  const projects = useQuery(projectsQuery);
  // PR previews and environment-preview resources are temporary; the server refuses persistent bindings for them.
  const temporary = Boolean(workload.preview_source_id) || Boolean(findPlacement(projects.data, workload.environment_id, workload.project_id)?.environment?.preview_id);
  return (
    <>
      {editable ? <SourceForm workload={workload} /> : <SourceSummary workload={workload} />}
      {(projects.data !== undefined || temporary) && <ComposeBundleSection workload={workload} editable={editable} readable={can(workload.permissions, "read_secrets")} temporary={temporary} blocked={blocked} blockers={<SaveBlockers workload={workload} />} />}
      {editable && !workload.preview_source_id && <GitDeployKey workload={workload} blocked={blocked} blockers={<SaveBlockers workload={workload} />} />}
      {editable && workload.source && provider.data === null && (
        <Section width="form" title="Automatic deployments">
          <Webhook workloadId={workload.id} />
        </Section>
      )}
      <GitProviderSource workload={workload} editable={editable} temporary={temporary} blocked={blocked} blockers={<SaveBlockers workload={workload} />} />
      {editable && <GitHubSource workload={workload} />}
      {!temporary && <SourceTrigger workload={workload} editable={editable} blocked={blocked} blockers={<SaveBlockers workload={workload} />} />}
      {!temporary && <DeployHook workload={workload} editable={editable} />}
    </>
  );
}

function ManagedConfiguration({ workload }: { workload: Workload }) {
  return (
    <Section width="form" title="Configuration" description="Generated from the pinned built-in template when the database was created.">
      <div className="space-y-4">
        <Notice title="Managed by Cove">
          The Compose file, credentials and storage of a managed database cannot be edited, replaced by a Git source or given public routes. Rename it or change its limits on the General and Resource limits pages. Connect applications from{" "}
          <Link to="/resources/$resourceId/database" params={{ resourceId: workload.id }} className="underline underline-offset-4">
            the Database page
          </Link>
          .
        </Notice>
        <pre aria-label="Compose file" className="max-h-[32rem] overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">
          {workload.compose}
        </pre>
      </div>
    </Section>
  );
}

function SourceSummary({ workload }: { workload: Workload }) {
  return (
    <Section width="form" title="Source">
      <div className="space-y-4">
        <ViewOnly />
        <DefinitionList
          items={
            workload.source
              ? [
                  { term: "Source", value: "Git repository" },
                  { term: "Repository", value: <span className="font-mono text-xs break-all">{workload.source.repository}</span> },
                  { term: "Branch", value: <span className="font-mono text-xs">{workload.source.branch}</span> },
                  { term: "Compose file", value: <span className="font-mono text-xs">{workload.source.inline_compose ? "Managed in Cove" : workload.source.compose_path}</span> },
                  { term: "Watch paths", value: workload.source.watch_paths?.join(", ") || "All changes" },
                  ...(isSSHRepository(workload.source.repository)
                    ? [{ term: "Host public key", value: <span className="font-mono text-xs break-all">{workload.source.ssh_host_key}</span> }]
                    : [{ term: "Access token", value: workload.has_source_token ? "Saved" : "None" }]),
                ]
              : [{ term: "Source", value: "Compose file" }]
          }
        />
        {(!workload.source || workload.source.inline_compose) && workload.compose && (
          <pre aria-label="Compose file" className="max-h-[32rem] overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">
            {workload.compose}
          </pre>
        )}
      </div>
    </Section>
  );
}

function SourceForm({ workload }: { workload: Workload }) {
  const { update, saved, save } = useSectionSave(workload);
  const blocked = useSaveBlocked(workload.id);
  const [values, setValues] = useState(saved);
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const set = <K extends keyof WorkloadFormValues>(key: K, value: WorkloadFormValues[K]) => {
    update.reset();
    setValues((current) => ({ ...current, [key]: value }));
  };
  const git = values.sourceKind === "git";
  const dirty =
    values.sourceKind !== saved.sourceKind ||
    (git
      ? values.repository !== saved.repository || values.branch !== saved.branch || (!values.inlineCompose && values.composePath !== saved.composePath) || values.inlineCompose !== saved.inlineCompose || (values.inlineCompose && values.compose !== saved.compose) || values.watchPaths !== saved.watchPaths || values.token !== "" || (isSSHRepository(values.repository) && values.sshHostKey !== saved.sshHostKey)
      : values.compose !== saved.compose);
  const tokenSaved = workload.has_source_token && values.repository.trim() === workload.source?.repository;
  const tokenDiscarded = workload.has_source_token && !tokenSaved && values.token === "";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const { sourceKind, compose, repository, branch, composePath, inlineCompose, watchPaths, token, sshHostKey } = values;
    setErrors(save({ sourceKind, compose, repository, branch, composePath, inlineCompose, watchPaths, token, sshHostKey }, event.currentTarget, () => setValues((current) => ({ ...current, token: "" }))));
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title="Source"
        description={`Where the Compose definition comes from. ${nextDeploy}`}
        footer={
          <SaveBar
            dirty={dirty}
            pending={update.isPending}
            blocked={blocked}
            onDiscard={() => {
              setValues(saved);
              setErrors({});
            }}
            status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined}
          />
        }
      >
        <fieldset disabled={update.isPending} className="min-w-0 space-y-5">
          <SaveBlockers workload={workload} />
          <SourceFields
            values={values}
            errors={errors}
            set={set}
            tokenSaved={tokenSaved}
            tokenHint={
              tokenSaved
                ? "A token is saved for this repository. Leave blank to keep it, or enter a new one to replace it."
                : tokenDiscarded
                  ? "The saved token belongs to the previous repository and will not be used. Enter a token if this repository is private."
                  : "Needed for private repositories. Stored encrypted and never shown again."
            }
          />
          <FormError errors={errors} error={update.error} />
        </fieldset>
      </Section>
    </form>
  );
}

export function ResourceEnvironment() {
  const workload = useResource();
  const keys = [...workload.environment_keys].sort();
  if (workload.database) return <ManagedVariables workload={workload} keys={keys} />;
  return <EnvironmentPages workload={workload} keys={keys} />;
}

function EnvironmentPages({ workload, keys }: { workload: Workload; keys: string[] }) {
  const blocked = useSaveBlocked(workload.id);
  const projects = useQuery(projectsQuery);
  // Preview resources are temporary; the server refuses to import variables into them.
  const temporary = Boolean(workload.preview_source_id) || Boolean(findPlacement(projects.data, workload.environment_id, workload.project_id)?.environment?.preview_id);
  if (!can(workload.permissions, "edit")) {
    return (
      <div className="space-y-6">
        <Section width="form" title="Environment variables" description="Delivered to Compose at deploy time. Values are encrypted at rest and never shown." flush>
          <div className="border-b border-line p-3">
            <ViewOnly />
          </div>
          {keys.length === 0 ? (
            <p className="px-4 py-3 text-[13px] text-fg-muted">No variables are saved.</p>
          ) : (
            <ul aria-label="Saved variables" className="divide-y divide-line">
              {keys.map((key) => (
                <li key={key} className="flex items-center justify-between gap-4 px-4 py-2">
                  <span className="truncate font-mono text-xs text-fg">{key}</span>
                  <span className="text-xs text-fg-subtle">Value hidden</span>
                </li>
              ))}
            </ul>
          )}
        </Section>
        <EnvironmentFileActions workload={workload} temporary={temporary} blocked={blocked} />
        <BuiltinVariables workloadId={workload.id} />
      </div>
    );
  }
  return <div className="space-y-6"><EnvironmentForm workload={workload} savedKeys={keys} /><EnvironmentFileActions workload={workload} temporary={temporary} blocked={blocked} /><BuiltinVariables workloadId={workload.id} /><ResourceVariables key={workload.id} workloadId={workload.id} /><ResourceFileSecrets key={`files-${workload.id}`} /></div>;
}

function ManagedVariables({ workload, keys }: { workload: Workload; keys: string[] }) {
  const exported = keys.filter(isDatabaseExport);
  const generated = keys.filter((key) => !isDatabaseExport(key));
  return (
    <Section width="form" title="Environment variables" description="Generated when the database was created. Values are encrypted at rest and never shown here." flush>
      <div className="border-b border-line p-3">
        <Notice title="Managed variables">
          Variables of a managed database cannot be edited. Other resources bind the exported keys below from their own Environment variables page. Editors can read the values from{" "}
          <Link to="/resources/$resourceId/database" params={{ resourceId: workload.id }} className="underline underline-offset-4">
            the Database page
          </Link>
          .
        </Notice>
      </div>
      <h3 className="px-4 pt-3 text-[13px] font-semibold text-fg">Exported for bindings</h3>
      <ul aria-label="Exported variables" className="divide-y divide-line">
        {exported.map((key) => (
          <li key={key} className="flex items-center justify-between gap-4 px-4 py-2">
            <span className="truncate font-mono text-xs text-fg">{key}</span>
            <span className="text-xs text-fg-subtle">{exportLabels[key]}</span>
          </li>
        ))}
      </ul>
      <h3 className="border-t border-line px-4 pt-3 text-[13px] font-semibold text-fg">Generated configuration</h3>
      <ul aria-label="Generated variables" className="divide-y divide-line">
        {generated.map((key) => (
          <li key={key} className="flex items-center justify-between gap-4 px-4 py-2">
            <span className="truncate font-mono text-xs text-fg">{key}</span>
            <span className="text-xs text-fg-subtle">Value hidden</span>
          </li>
        ))}
      </ul>
    </Section>
  );
}

function EnvironmentForm({ workload, savedKeys }: { workload: Workload; savedKeys: string[] }) {
  const { update, save } = useSectionSave(workload);
  const blocked = useSaveBlocked(workload.id);
  const [rows, setRows] = useState<EnvRow[]>([]);
  const [removed, setRemoved] = useState<ReadonlySet<string>>(() => new Set());
  const [errors, setErrors] = useState<WorkloadFormErrors>({});
  const dirty = rows.length > 0 || removed.size > 0;

  function reset() {
    setRows([]);
    setRemoved(new Set());
    setErrors({});
  }

  function toggle(key: string) {
    update.reset();
    setRemoved((current) => {
      const next = new Set(current);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (rows.some((row) => removed.has(row.key))) {
      setErrors({ environment: "A variable cannot be updated and removed in the same save." });
      return;
    }
    setErrors(
      save({ environment: rows }, event.currentTarget, reset, (input) => {
        input.environment_patch = { ...Object.fromEntries([...removed].map((key) => [key, null])), ...input.environment };
        delete input.environment;
      }),
    );
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section width="form" title="Environment variables" description={`Delivered to Compose on the node at deploy time. Values are encrypted at rest and never shown after saving. ${nextDeploy}`} footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={blocked} onDiscard={reset} status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined} />}>
        <fieldset disabled={update.isPending} className="min-w-0 space-y-5">
          <SaveBlockers workload={workload} />
          <div className="space-y-2">
            <h3 className="text-[13px] font-medium text-fg">Saved variables</h3>
            {savedKeys.length === 0 ? (
              <p className="text-[13px] text-fg-muted">No variables are saved.</p>
            ) : (
              <ul aria-label="Saved variables" className="divide-y divide-line rounded-md border border-line">
                {savedKeys.map((key) => {
                  const removing = removed.has(key);
                  return (
                    <li key={key} className="flex items-center justify-between gap-4 px-3 py-1.5">
                      <span className={`truncate font-mono text-xs ${removing ? "text-fg-subtle line-through" : "text-fg"}`}>{key}</span>
                      <span className="flex shrink-0 items-center gap-3">
                        <span className="text-xs text-fg-subtle">{removing ? "Removed on save" : "Value hidden"}</span>
                        <Button variant="ghost" size="sm" aria-pressed={removing} aria-label={removing ? `Keep ${key}` : `Remove ${key}`} onClick={() => toggle(key)}>
                          {removing ? "Keep" : "Remove"}
                        </Button>
                      </span>
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
          <div className="space-y-2">
            <h3 className="text-[13px] font-medium text-fg">Add or replace variables</h3>
            <p className="text-xs text-fg-muted">Use an existing name to replace its saved value.</p>
            <EnvironmentEditor
              rows={rows}
              errors={errors.rows}
              onChange={(next) => {
                update.reset();
                setRows(next);
              }}
            />
            {errors.environment && <p className="text-xs text-bad">{errors.environment}</p>}
          </div>
          <SecretReferenceBuilder
            onAdd={(key, value) => {
              update.reset();
              setRows((current) => [...current, createRow(key, value)]);
            }}
          />
          <FormError errors={errors} error={update.error} />
        </fieldset>
      </Section>
    </form>
  );
}

export function ResourceDomains() {
  const workload = useResource();
  if (workload.database) {
    return <EmptyState title="Databases have no public domains">Applications reach a managed database through a private connection. Use temporary database access on the Networking page for administration.</EmptyState>;
  }
  if (workloadKind(workload.kind) === "job") {
    return <EmptyState title="Jobs cannot have domains">Domains route HTTPS traffic to long-running services. Change the resource type to a service first.</EmptyState>;
  }
  return (
    <>
      <DomainsPanel workload={workload} />
      <UptimeChecks workload={workload} />
    </>
  );
}

function useListenerChoices(workloadId: string, routeProtocol: number | undefined, nodeName: string, nodeId: string | undefined, nodeReadFailed: boolean): ListenerChoices {
  // Private routes need route protocol 5 and TCP and UDP routes 6; older workers cannot report their listeners.
  const enabled = (routeProtocol ?? 0) >= 5;
  const query = useQuery({ ...routeListenersQuery(workloadId), enabled });
  // A failed refetch keeps the previous data cached; treat the list as unknown until a read succeeds again.
  const list = query.isError || !enabled ? undefined : query.data;
  return { routeProtocol, list, error: query.error, retry: () => void query.refetch(), retrying: query.isFetching, nodeName, nodeId, nodeReadFailed };
}

// Managed previews cannot hold stored certificates or issuers: their hostnames are generated and the API rejects them.
function DomainsPanel({ workload }: { workload: Workload }) {
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const nodeName = node?.name ?? "its node";
  // Routes, certificate uploads and issuers need only Edit configuration, so configuration-only roles without Read
  // secrets can manage them. The legacy aggregate "edit" also means a full editor.
  const editable = can(workload.permissions, "edit_configuration") || can(workload.permissions, "edit");
  const uploads = !isManagedPreview(workload);
  const certificates = useQuery({ ...certificatesQuery(workload.id), enabled: uploads });
  const issuers = useQuery({ ...certificateIssuersQuery(workload.id), enabled: uploads });
  const zones = useQuery({ ...certificateZonesQuery(workload.id), enabled: uploads });
  const challenges = useQuery({ ...certificateChallengesQuery(workload.id), enabled: uploads });
  // DNS challenge cleanup can outlive every ACME issuer, so the section stays while any record is unresolved.
  const showChallenges = uploads && (Boolean(issuers.data?.some((issuer) => issuer.provider === "acme_dns")) || Boolean(challenges.data?.items.length) || challenges.isError);
  const busy = Boolean(useActiveOperation(workload.id));
  const choices: CertificateChoices | undefined = uploads
    ? { supported: node ? (node.capabilities?.route_protocol ?? 0) >= 3 : undefined, list: certificates.data, error: certificates.error, retry: () => void certificates.refetch(), retrying: certificates.isFetching }
    : undefined;
  // A failed refetch keeps the previous nodes cached; their capabilities stay unknown until a read succeeds again.
  const routeProtocol = nodes.isError ? undefined : routeProtocolOf(node);
  const listeners = useListenerChoices(workload.id, routeProtocol, nodeName, node?.id, nodes.isError);
  return (
    <>
      <Section
        width="form"
        title="Domains"
        description={`HTTPS hostnames and TCP and UDP listeners served by the managed Traefik proxy on ${nodeName}. For public HTTPS routes, point each hostname's DNS at that node and keep ports 80 and 443 reachable; certificates are issued automatically unless a route uses the proxy default or a stored certificate. Private routes are served only on the selected LAN or Tailscale listener, so point private DNS at that listener's address.`}
      >
        {editable ? (
          <DomainsForm workload={workload} certificates={choices} listeners={listeners} />
        ) : (
          <div className="space-y-4">
            <ViewOnly />
            <RouteList workload={workload} certificates={certificates.data} listeners={listeners.list} />
          </div>
        )}
      </Section>
      {uploads && <RouteCertificates workloadId={workload.id} certificates={certificates} routes={workload.routes ?? []} issuers={issuers.data} editable={editable} busy={busy} />}
      {uploads && <CertificateIssuers workloadId={workload.id} issuers={issuers} zones={zones} certificates={certificates.data} routes={workload.routes ?? []} editable={editable} />}
      {showChallenges && <CertificateChallenges challenges={challenges} issuers={issuers.data} />}
    </>
  );
}

function RouteList({ workload, certificates, listeners }: { workload: Workload; certificates?: RouteCertificate[]; listeners?: Listener[] }) {
  const { http, transport } = splitRoutes(workload.routes);
  if (http.length === 0 && transport.length === 0) return <p className="text-[13px] text-fg-muted">No domains.</p>;
  return (
    <div className="space-y-4">
      {http.length > 0 && <HttpRouteList routes={http} certificates={certificates} listeners={listeners} />}
      {transport.length > 0 && <TransportRouteList routes={transport} listeners={listeners} />}
    </div>
  );
}

function TransportRouteList({ routes, listeners }: { routes: Route[]; listeners?: Listener[] }) {
  return (
    <ul aria-label="TCP and UDP routes" className="divide-y divide-line rounded-md border border-line">
      {routes.map((route) => (
        <li key={`${route.protocol}-${route.policy?.listener ?? ""}`} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-3 py-2 text-[13px]">
          <span className="break-words">{transportSummary(route, listeners)}</span>
          <span className="font-mono text-xs break-all text-fg-muted">{transportTargets(route)}</span>
          {route.policy?.ip_allow_list?.length ? <span className="basis-full text-xs text-fg-muted">{route.policy.ip_allow_list.length === 1 ? "1 allowed source range" : `${route.policy.ip_allow_list.length} allowed source ranges`}</span> : null}
        </li>
      ))}
    </ul>
  );
}

function HttpRouteList({ routes, certificates, listeners }: { routes: Route[]; certificates?: RouteCertificate[]; listeners?: Listener[] }) {
  return (
    <ul aria-label="Routes" className="divide-y divide-line rounded-md border border-line">
      {routes.map((route) => {
        const extensions = extensionSummary(route.policy);
        const privacy = visibilitySummary(route.policy, listeners);
        if (privacy) extensions.unshift(privacy);
        const certificateId = route.policy?.certificate_id;
        if (certificateId) extensions.push(`stored certificate ${certificates?.find((item) => item.id === certificateId)?.name ?? shortId(certificateId)}`);
        else if (route.policy?.certificate_source === "default") extensions.push("proxy default certificate");
        return (
          <li key={`${route.domain}${route.policy?.path_prefix ?? ""}`} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-3 py-2 text-[13px]">
            {!isLinkableRoute(route) ? (
              <span className="font-mono break-all">{route.domain}</span>
            ) : (
              <a className="break-all underline underline-offset-4" href={`https://${route.domain}`} target="_blank" rel="noreferrer">
                {route.domain}
              </a>
            )}
            <span className="font-mono text-xs text-fg-muted">
              {route.service}:{route.port}
            </span>
            {extensions.length > 0 && <span className="basis-full text-xs text-fg-muted">{extensions.join(", ")}</span>}
          </li>
        );
      })}
    </ul>
  );
}

// Taking a cached Let's Encrypt certificate out of the shared proxy, or putting it back, can require restarting the proxy.
// Both a stored certificate and the proxy default take it out.
function ProxyRestartNotice({ rows, saved, nodeName = "this node" }: { rows: RouteRow[]; saved?: Route[]; nodeName?: string }) {
  const switches = certificateSwitches(rows, saved);
  if (switches.length === 0) return null;
  const one = switches.length === 1;
  return (
    <Notice tone="warn" title="Saving may briefly restart the shared proxy">
      <span className="font-mono break-all">{switches.join(", ")}</span> {one ? "switches" : "switch"} between Let&apos;s Encrypt and a stored or proxy default certificate. If the proxy holds a cached Let&apos;s Encrypt certificate for {one ? "this hostname" : "these hostnames"}, applying takes it out or puts it back and restarts the proxy, interrupting connections to every resource on {nodeName}, not just this one, for a moment.
    </Notice>
  );
}

function DomainsForm({ workload, certificates, listeners }: { workload: Workload; certificates?: CertificateChoices; listeners: ListenerChoices }) {
  const { http: saved, transport: savedTransports } = useMemo(() => splitRoutes(workload.routes), [workload.routes]);
  const client = useQueryClient();
  const operations = useQuery(workloadOperationsQuery(workload.id));
  // The resource is fetched before a deploy finishes, so also trust a release seen in the live operation list.
  const deployed = workload.deployed || Boolean(operations.data?.some((item) => item.workload_id === workload.id && (item.action === "deploy" || item.action === "rollback") && item.state === "succeeded"));
  // The validated routes of the save in progress; held outside the mutation so typed header values and forward-auth
  // URLs never become mutation variables.
  const submitted = useRef<Route[]>([]);
  const save = useUpdateRoutes(workload.id, () => submitted.current);
  const blocked = useSaveBlocked(workload.id);
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const routeProtocol = listeners.routeProtocol;
  const [rows, setRows] = useState(() => routeRows(saved));
  const [transports, setTransports] = useState(() => transportRows(savedTransports));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [applying, setApplying] = useState<string | null>(null);
  const operation = useQuery({ ...operationQuery(applying ?? ""), enabled: applying !== null });
  const applyState = applying ? operation.data?.state : undefined;
  const inProgress = save.isPending || (applying !== null && (applyState === undefined || isActive(applyState)));
  const dirty = !sameRoutes(rows, saved) || !sameTransports(transports, savedTransports);
  const transportContext = (httpCount: number): TransportContext => ({ routeProtocol, listeners: listeners.list, saved: savedTransports, httpCount });

  // Saved routes change only after the worker applies them, so reload the resource once it reports a result.
  useEffect(() => {
    if (applyState && !isActive(applyState)) void client.invalidateQueries({ queryKey: keys.workload(workload.id) });
  }, [applyState, client, workload.id]);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateRoutes(rows, saved, certificates?.list, routeProtocol, listeners.list);
    const raw = validateTransports(transports, transportContext(rows.length));
    setErrors({ ...result.errors, ...raw.errors });
    if (!result.routes || !raw.routes) {
      focusFirstInvalid(event.currentTarget);
      return;
    }
    setApplying(null);
    submitted.current = [...result.routes, ...raw.routes];
    save.mutate(undefined, {
      onSuccess: (outcome) => {
        setApplying(outcome.state === "queued" ? outcome.id : null);
        setRows(withoutRouteSecrets);
      },
      onSettled: () => {
        submitted.current = [];
      },
    });
  }

  let status = null;
  if (save.isError) {
    status = (
      <Notice tone="error" title="Could not save routes">
        {errorMessage(save.error)}
      </Notice>
    );
  } else if (applying && inProgress) {
    // A running route change with a worker message is restoring the previous routes after a failure.
    const recovery = operation.data?.result?.error;
    status = (
      <div role="status" className="flex items-center gap-2 rounded-md border border-line-strong px-3 py-2.5 text-[13px] text-fg">
        <Spinner />
        {recovery ?? <>Applying routes to the proxy. Containers are not restarted{workload.manually_stopped ? " and the stopped resource stays stopped" : ""}.</>}
      </div>
    );
  } else if (applying && applyState === "succeeded" && !deployed) {
    status = <Notice tone="success" title="Routes saved">They apply when this resource is first deployed.</Notice>;
  } else if (applying && applyState === "succeeded") {
    status = (
      <Notice tone="success" title="Routes applied">
        The proxy serves the saved routes. Containers were not restarted{workload.manually_stopped ? " and the resource is still stopped" : ""}.
      </Notice>
    );
  } else if (applying && applyState) {
    status = (
      <Notice
        tone="error"
        title="Routes were not applied"
        action={
          <Link to="/operations/$operationId" params={{ operationId: applying }} className={buttonClass("ghost")}>
            View operation log
          </Link>
        }
      >
        The previous routes are still saved and served. Fix the problem and save again to retry.
        {operation.data?.result?.error && <span className="mt-1 block font-mono text-xs break-words">{operation.data.result.error}</span>}
      </Notice>
    );
  } else if (save.data?.state === "saved" && !dirty) {
    status = <Notice tone="success" title="Routes saved">They apply when this resource is first deployed.</Notice>;
  } else if (save.data?.state === "unchanged" && !dirty) {
    status = <Notice title="Routes are already up to date" />;
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <UnsavedChangesGuard dirty={dirty} />
      {!deployed ? (
        <Notice title="Not deployed yet">Saved routes are applied by the first deploy.</Notice>
      ) : workload.manually_stopped ? (
        <Notice tone="warn" title="This resource is stopped">
          Saving updates the proxy only and does not start the resource. Requests to these hostnames fail until you start or deploy it.
        </Notice>
      ) : (
        <p className="text-xs text-fg-muted">Saving applies routes to the running proxy immediately. Application containers are not restarted.</p>
      )}
      {!applying && <SaveBlockers workload={workload} />}
      {nodes.isError && <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />}
      <fieldset disabled={inProgress} className="min-w-0">
        <RouteEditor
          routes={rows}
          errors={errors}
          policies={node ? Boolean(node.capabilities?.route_policies) : undefined}
          extensions={node ? (node.capabilities?.route_protocol ?? 0) >= 2 : undefined}
          routeProtocol={routeProtocol}
          saved={saved}
          certificates={certificates}
          listeners={listeners}
          capacity={routeLimits.routes - transports.length}
          onChange={(next) => {
            save.reset();
            setApplying(null);
            setRows(next);
            setErrors((current) => refreshCertificateErrors(current, next, saved, certificates?.list, routeProtocol, listeners.list));
          }}
        />
      </fieldset>
      <fieldset disabled={inProgress} className="min-w-0 border-t border-line pt-4">
        <TransportRouteEditor
          rows={transports}
          errors={errors}
          choices={listeners}
          capacity={routeLimits.routes - rows.length}
          onChange={(next) => {
            save.reset();
            setApplying(null);
            setTransports(next);
            setErrors((current) => refreshTransportErrors(current, transports, next, transportContext(rows.length)));
          }}
        />
      </fieldset>
      <ProxyRestartNotice rows={rows} saved={saved} nodeName={node?.name} />
      {status}
      <div className="border-t border-line pt-4">
      <SaveBar
        dirty={dirty}
        pending={inProgress}
        blocked={blocked}
        label={deployed ? "Save and apply" : "Save routes"}
        onDiscard={() => {
          setRows(routeRows(saved));
          setTransports(transportRows(savedTransports));
          setErrors({});
          save.reset();
          setApplying(null);
        }}
      />
      </div>
    </form>
  );
}

export function ResourceNetworking() {
  const workload = useResource();
  const active = useActiveOperation(workload.id);
  if (workload.database) return <DatabaseNetworking workload={workload} />;
  return <div className="space-y-6"><Networks workload={workload} /><PrivateNetwork id={workload.id} canEdit={can(workload.permissions, "edit")} />{can(workload.permissions, "edit") && <PortBindings workload={workload} busy={Boolean(active)} />}{can(workload.permissions,"edit") && <DatabaseAccess id={workload.id} />}</div>;
}

function DatabaseNetworking({ workload }: { workload: Workload }) {
  const projects = useQuery(projectsQuery);
  const placement = findPlacement(projects.data, workload.environment_id, workload.project_id);
  const graph = placement?.environment ? (
    <Link to="/projects/$projectId/environments/$environmentId" params={{ projectId: placement.project.id, environmentId: placement.environment.id }} className="underline underline-offset-4">
      {placement.environment.name} resource graph
    </Link>
  ) : placement ? (
    <Link to="/projects/$projectId" params={{ projectId: placement.project.id }} className="underline underline-offset-4">
      project resource graph
    </Link>
  ) : (
    "environment resource graph"
  );
  return (
    <div className="space-y-6">
      <Section width="form" title="Private connections" description="Applications reach this database only through private connections. No host port is published.">
        <p className="text-[13px] text-fg">
          Create a connection from the consuming resource in the {graph}. The form fills in service <span className="font-mono text-xs">{workload.database?.service}</span>, port{" "}
          <span className="font-mono text-xs">{workload.database?.port}</span> and hostname <span className="font-mono text-xs break-all">{workload.database?.host}</span> when this database is the
          target.
        </p>
      </Section>
      {can(workload.permissions, "edit") && <DatabaseAccess id={workload.id} database={workload.database ?? undefined} />}
    </div>
  );
}

export function ResourceRegistryPage() {
  const workload = useResource();
  const active = useActiveOperation(workload.id);
  return <div className="space-y-6"><ResourceRegistry id={workload.id} canEdit={can(workload.permissions, "edit")} busy={Boolean(active)} />{supportsBuildPublish(workload) && <ResourceBuild workload={workload} />}{supportsImageLifecycle(workload) && <ResourceImages workload={workload} />}<ImageScans workload={workload} /><ImageUpdatePolicy workload={workload} /><RegistryWebhook workload={workload} /></div>;
}

export function ResourceLimits() {
  const workload = useResource();
  // Each form is keyed by its own fields, so saving one does not discard an unsaved draft in the other.
  const cpuKey = JSON.stringify(Object.entries(workload.limits ?? {}).filter(([, limit]) => !isGPUOnly(limit)).map(([service, limit]) => [service, limit.cpus, limit.memory_mb]));
  // Managed database engines refuse GPU assignments.
  return <><LimitsForm key={cpuKey} workload={workload} />{!workload.database && <GPUAssignments workload={workload} />}<RightSizing workloadId={workload.id} limits={workload.limits} /></>;
}

function GPUAssignments({ workload }: { workload: Workload }) {
  const update = useUpdateWorkload(workload.id);
  const blocked = useSaveBlocked(workload.id);
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  function onSave(limits: NonNullable<WorkloadInput["limits"]>) {
    const result = validateWorkload(formValues(workload), "edit");
    if (!result.input) return;
    result.input.limits = limits;
    update.mutate(result.input);
  }
  return <ResourceGPUs workload={workload} node={node} editable={can(workload.permissions, "edit")} blockers={<SaveBlockers workload={workload} />} onSave={onSave} save={{ pending: update.isPending, blocked, saved: update.isSuccess, error: update.error, reset: update.reset }} />;
}

function LimitsForm({ workload }: { workload: Workload }) {
  const id = useId();
  const { update, save } = useSectionSave(workload);
  const blocked = useSaveBlocked(workload.id);
  const initial = Object.entries(workload.limits ?? {}).filter(([, limit]) => !isGPUOnly(limit)).map(([service, limit]) => ({ service, cpus: String(limit.cpus || ""), memory: String(limit.memory_mb || "") }));
  const [rows, setRows] = useState(initial);
  const [error, setError] = useState("");
  const editable = can(workload.permissions, "edit");
  const dirty = JSON.stringify(initial) !== JSON.stringify(rows);
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const limits: Record<string, { cpus: number; memory_mb: number }> = {};
    for (const row of rows) {
      const cpus = Number(row.cpus), memory = Number(row.memory);
      if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/.test(row.service) || Object.hasOwn(limits, row.service) || !Number.isFinite(cpus) || cpus < 0 || cpus > 1024 || (cpus > 0 && cpus < 0.01) || !Number.isSafeInteger(memory) || memory < 0 || memory > 16777216 || (memory > 0 && memory < 6)) {
        setError("Use unique Compose service names, CPU from 0.01 to 1024, and memory from 6 to 16777216 MiB. Blank values inherit Compose.");
        return;
      }
      limits[row.service] = { cpus, memory_mb: memory };
    }
    setError("");
    save({}, event.currentTarget, undefined, (input) => { input.limits = mergeCPULimits(workload.limits, limits); });
  }
  function change(index: number, key: "service" | "cpus" | "memory", value: string) {
    update.reset(); setRows((current) => current.map((row, position) => position === index ? { ...row, [key]: value } : row));
  }
  return <form onSubmit={submit}>
    <UnsavedChangesGuard dirty={dirty} />
    <Section width="form" title="Resource limits" description="Limits apply per container on the next deploy. Blank values inherit the Compose file, then resource defaults. Rollback restores the limits saved with that release." footer={editable && <SaveBar dirty={dirty} pending={update.isPending} blocked={blocked} onDiscard={() => { setRows(initial); setError(""); }} status={update.isSuccess ? `Saved. ${nextDeploy}` : undefined} />}>
      <div className="space-y-4">
        {!editable && <ViewOnly />}
        <SaveBlockers workload={workload} />
        <fieldset disabled={!editable || update.isPending} className="space-y-4">
          {rows.map((row, index) => <div key={index} className="grid gap-3 sm:grid-cols-3">
            <Field id={`${id}-${index}-service`} label="Compose service"><input id={`${id}-${index}-service`} className={inputClass} value={row.service} onChange={(event) => change(index, "service", event.target.value)} required /></Field>
            <Field id={`${id}-${index}-cpu`} label="CPU cores"><input id={`${id}-${index}-cpu`} className={inputClass} type="number" min="0.01" max="1024" step="0.01" value={row.cpus} onChange={(event) => change(index, "cpus", event.target.value)} /></Field>
            <Field id={`${id}-${index}-memory`} label="Memory (MiB)"><input id={`${id}-${index}-memory`} className={inputClass} type="number" min="6" max="16777216" step="1" value={row.memory} onChange={(event) => change(index, "memory", event.target.value)} /></Field>
            {editable && <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => setRows((current) => current.filter((_, position) => position !== index))}>Remove {row.service || "service"}</Button>}
          </div>)}
          {editable && <Button disabled={rows.length >= 64} onClick={() => setRows((current) => [...current, { service: "", cpus: "", memory: "" }])}>Add service limit</Button>}
          {error && <Notice tone="error" title="Check resource limits">{error}</Notice>}
          {update.error && <Notice tone="error" title="Could not save limits">{errorMessage(update.error)}</Notice>}
          <p className="text-xs text-fg-muted">These limits cover running containers, not builds or persistent disk usage. Removing a limit restores the setting from Compose on the next deploy.</p>
          <ResourceDefaultsNote environmentId={workload.environment_id} />
        </fieldset>
      </div>
    </Section>
  </form>;
}

export function ResourceHooks() {
 const workload=useResource();
 const {save,update}=useSectionSave(workload);
 const blocked=useSaveBlocked(workload.id);
 const [errors,setErrors]=useState<WorkloadFormErrors>({});
 function submit(event:FormEvent<HTMLFormElement>){
  event.preventDefault();
  const data=new FormData(event.currentTarget);
  const hooks:DeploymentHooks={before_steps:workload.hooks?.before_steps,assertions:workload.hooks?.assertions};
  for(const phase of ["before","after"] as const){
   const command=formText(data, `${phase}-command`).trim();
   if(command) hooks[phase]={command,service:formText(data, `${phase}-service`).trim()};
  }
  setErrors(save({},event.currentTarget,undefined,input=>{input.hooks=hooks;}));
 }
 return <Section width="form" title="Deployment hooks" description="Commands run in temporary containers from the new service image, with its configured environment, mounts and networks. Dependencies are not started. Each command has a five-minute limit and requires sh in the image.">
  {workload.kind==="job"?<Notice>Use the job's Compose command instead of deployment hooks.</Notice>:<form key={workload.id} className="space-y-4" onSubmit={submit}>
   <Notice tone="warn">A pre-deployment failure prevents application replacement. A post-deployment failure leaves the new release running and marks the operation failed. Database changes are never automatically undone; make migrations compatible with the previous release.</Notice>
   {(workload.hooks?.before_steps?.length || workload.hooks?.assertions) ? <div className="space-y-2"><p className="text-sm text-fg-muted">This resource also has deployment safeguards configured through the API. They run before the command below and are preserved when saving.</p><pre className="overflow-auto text-xs">{JSON.stringify({before_steps:workload.hooks.before_steps,assertions:workload.hooks.assertions},null,2)}</pre></div> : null}
   {(["before","after"] as const).map(phase=><div key={phase} className="space-y-3 border-b border-line pb-4">
    <Field id={`${phase}-service`} label={`${phase==="before"?"Pre":"Post"}-deployment service`}><input className={inputClass} id={`${phase}-service`} name={`${phase}-service`} defaultValue={workload.hooks?.[phase]?.service??""} disabled={!can(workload.permissions,"edit")} /></Field>
    <Field id={`${phase}-command`} label={`${phase==="before"?"Pre":"Post"}-deployment command`} hint="Leave the command empty to disable. Reference environment variables instead of embedding credentials."><textarea className={inputClass} id={`${phase}-command`} name={`${phase}-command`} rows={3} maxLength={8192} defaultValue={workload.hooks?.[phase]?.command??""} disabled={!can(workload.permissions,"edit")} /></Field>
   </div>)}
   <FormError errors={errors} error={update.error}/>
   {can(workload.permissions,"edit")&&<div className="flex justify-end"><Button type="submit" variant="primary" pending={update.isPending} disabled={blocked}>Save hooks</Button></div>}
  </form>}
 </Section>;
}
