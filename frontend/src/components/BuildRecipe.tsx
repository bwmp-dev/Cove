import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { buildRecipeQuery, useSaveBuildRecipe, type BuildRecipe, type BuildRecipeSettings } from "../api/build-recipes";
import { errorMessage, isStatus } from "../api/client";
import { workloadOperationsQuery } from "../api/queries";
import type { Operation, Workload } from "../api/types";
import { composeServices, engineLabel, engineLabels, formatCPU, formatGB, formatMemory, isRecipeEngine, railpackDisclosure, recipeEngines, recipeErrors, recipePayload, sameRecipe, savedRecipeBlocker, type RecipeErrors } from "../lib/build-recipes";
import { useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const description =
  "How builds produce the image for one Compose service. Saving changes future builds and publishes only; it never builds, deploys or changes the running release.";

const engineHints: Record<string, string> = {
  compose: "Builds use each service's existing Compose build section or Dockerfile.",
  static:
    "Publishes files that are already built in the checkout. No build command runs. Dotfiles, symbolic links and special files are excluded or refused, and directory listings are not served. The runtime runs as a non-root user on a read-only filesystem and cannot use mounts, devices or added capabilities.",
  railpack: "Railpack plans and builds the image for the selected service from its source directory. Other services keep their Compose builds.",
  buildpacks:
    "The pinned Paketo builder's normal detection order builds the image for the selected service from its source directory; a Procfile there can set its launch process. Other services keep their Compose builds.",
};

function saveBlocker(settings: UseQueryResult<BuildRecipeSettings>, operations: UseQueryResult<Operation[]>, active: boolean, online: boolean) {
  if (!online) return "You are offline.";
  if (settings.isError) return "The current settings could not be read.";
  if (settings.isPending) return "Loading the current settings.";
  if (operations.isPending) return "Checking for active operations.";
  if (operations.isError) return "Operations could not be read.";
  if (active) return "An operation is in progress.";
  return undefined;
}

// Why an engine cannot be chosen now. The saved engine stays selected even when it becomes unavailable.
function engineUnavailable(engine: string, data: BuildRecipeSettings, isJob: boolean) {
  if (engine === "compose") return undefined;
  if (engine === "static" && isJob) return "jobs cannot use static output";
  if (!data.source_ready) return "needs a Git source that uses the saved Compose configuration";
  if (engine === "buildpacks") {
    if (!data.buildpacks) return "eligibility not reported";
    return data.buildpacks.supported ? undefined : "unavailable on the build node";
  }
  if (!data.supported) return "the build worker does not support it";
  return undefined;
}

interface RecipeFormState {
  data?: BuildRecipeSettings;
  lastRead?: BuildRecipe;
  mayEdit: boolean;
  editable: boolean;
  dirty: boolean;
  activeId: string;
  blocked?: string;
  errors: RecipeErrors;
}

function recipeFormState(workload: Workload, settings: UseQueryResult<BuildRecipeSettings>, operations: UseQueryResult<Operation[]>, draft: BuildRecipe | null, online: boolean): RecipeFormState {
  // A failed refresh hides the last read instead of offering stale data for editing. The draft is kept for when it loads again.
  const data = settings.isError ? undefined : settings.data;
  const lastRead = settings.data?.recipe;
  const mayEdit = can(workload.permissions, "edit_configuration") && settings.data?.can_edit !== false;
  const activeId = data?.active_operation_id || activeOperation(operations.data ?? [], workload.id)?.id || "";
  return {
    data,
    lastRead,
    mayEdit,
    editable: mayEdit && data?.can_edit === true,
    dirty: draft !== null && lastRead !== undefined && !sameRecipe(recipePayload(draft), lastRead),
    activeId,
    blocked: saveBlocker(settings, operations, activeId !== "", online),
    errors: draft ? recipeErrors(draft, workload.kind === "job") : {},
  };
}

// What Save does next: nothing, show the field errors, confirm an engine change, or save directly.
function submitStep(draft: BuildRecipe | null, state: RecipeFormState, pending: boolean) {
  if (!draft || !state.dirty || state.blocked || pending) return "none";
  if (Object.keys(state.errors).length > 0) return "errors";
  return state.lastRead && draft.engine !== state.lastRead.engine ? "confirm" : "save";
}

export function BuildRecipeSetting({ workload }: { workload: Workload }) {
  const settings = useQuery(buildRecipeQuery(workload.id));
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const online = useOnline();
  const save = useSaveBuildRecipe(workload.id);
  const [draft, setDraft] = useState<BuildRecipe | null>(null);
  const [saved, setSaved] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const state = recipeFormState(workload, settings, operations, draft, online);
  const { data, lastRead, mayEdit, dirty, blocked } = state;

  function update(next: Partial<BuildRecipe>) {
    const base = draft ?? lastRead;
    if (!base) return;
    setDraft({ ...base, ...next });
    setSaved(false);
    save.reset();
  }

  function discard() {
    setDraft(null);
    setShowErrors(false);
    save.reset();
  }

  function commit() {
    if (!draft) return;
    save.mutate(recipePayload(draft), {
      onSuccess: () => {
        setDraft(null);
        setShowErrors(false);
        setSaved(true);
        setConfirming(false);
      },
      onError: () => setConfirming(false),
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const step = submitStep(draft, state, save.isPending);
    if (step === "errors") setShowErrors(true);
    if (step === "confirm") setConfirming(true);
    if (step === "save") commit();
  }

  return (
    <form onSubmit={submit} aria-label="Build recipe setting" noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Build recipe"
        description={description}
        width="form"
        footer={mayEdit ? <SaveBar dirty={dirty} pending={save.isPending} blocked={Boolean(blocked)} label="Save build recipe" onDiscard={discard} status={saveStatus(blocked, saved)} /> : undefined}
      >
        <div className="space-y-4">
          <div className="space-y-2 empty:hidden" aria-live="polite">
            {settings.isError && <QueryError what={settings.data ? "the current build recipe" : "the build recipe"} error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />}
            {mayEdit && <SaveNotices activeId={state.activeId} online={online} operationsFailed={operations.isError} saveError={save.error} />}
            {data && <SavedRecipeState settings={data} canEdit={state.editable} />}
          </div>
          {settings.isPending && <LoadingRows label="Loading build recipe" rows={2} />}
          {data && <RecipeBody workload={workload} settings={data} value={draft ?? data.recipe} editable={state.editable} errors={showErrors ? state.errors : {}} disabled={save.isPending} onChange={update} />}
        </div>
      </Section>
      {lastRead && draft && (
        <ConfirmDialog open={confirming} title="Change the build engine?" confirmLabel="Change build engine" pending={save.isPending} onConfirm={commit} onCancel={() => setConfirming(false)}>
          <EngineChange from={lastRead} to={draft} />
        </ConfirmDialog>
      )}
    </form>
  );
}

function RecipeBody({ workload, settings, value, editable, errors, disabled, onChange }: { workload: Workload; settings: BuildRecipeSettings; value: BuildRecipe; editable: boolean; errors: RecipeErrors; disabled: boolean; onChange: (next: Partial<BuildRecipe>) => void }) {
  return (
    <>
      {editable ? <RecipeEditor workload={workload} settings={settings} value={value} errors={errors} disabled={disabled} onChange={onChange} /> : <RecipeSummary settings={settings} />}
      {value.engine === "railpack" && <RailpackDetails settings={settings} />}
      {value.engine === "buildpacks" && <BuildpacksDetails settings={settings} />}
    </>
  );
}

function EngineChange({ from, to }: { from: BuildRecipe; to: BuildRecipe }) {
  const service = to.service || from.service;
  return (
    <div className="space-y-2">
      <p>
        From {engineLabel(from.engine)} to {engineLabel(to.engine)}.
      </p>
      {to.engine === "compose" ? (
        <p>
          Builds of {service ? <span className="font-mono break-all text-fg">{service}</span> : "the selected service"} go back to its existing Compose build section or Dockerfile.
        </p>
      ) : (
        <p>
          Builds of <span className="font-mono break-all text-fg">{to.service}</span> replace that service&apos;s existing Compose build section or Dockerfile with {engineLabel(to.engine)}. Other services keep their Compose builds.
        </p>
      )}
      {to.engine === "static" && <p>The static runtime runs as a non-root user on a read-only filesystem and cannot use mounts, devices or added capabilities.</p>}
      {to.engine === "buildpacks" && (
        <p>The pinned Paketo builder detects the app, and a Procfile in the source directory can set its launch process. Its lifecycle OCI-layout export is experimental upstream.</p>
      )}
      <p>This changes future builds only. Nothing is built, deployed or restarted now.</p>
    </div>
  );
}

function saveStatus(blocked: string | undefined, saved: boolean) {
  if (blocked) return `${blocked} Saving is paused.`;
  return saved ? "Build recipe saved. The next build and publish uses it; the running release is unchanged." : "No unsaved changes";
}

function SaveNotices({ activeId, online, operationsFailed, saveError }: { activeId: string; online: boolean; operationsFailed: boolean; saveError: Error | null }) {
  return (
    <>
      {!online && <Notice tone="warn" title="You are offline">Saving is paused until the connection returns.</Notice>}
      {activeId && (
        <Notice
          tone="warn"
          title="An operation is in progress"
          action={
            <Link to="/operations/$operationId" params={{ operationId: activeId }} className={linkClass}>
              View operation
            </Link>
          }
        >
          The build recipe cannot change until it finishes.
        </Notice>
      )}
      {operationsFailed && <Notice tone="warn" title="Operations could not be read">Saving is paused until Cove can confirm that no operation is active.</Notice>}
      {saveError && (
        <Notice tone={isStatus(saveError, 409) ? "warn" : "error"} title="The build recipe was not saved">
          <p className="break-words">{errorMessage(saveError)}</p>
          <p className="mt-1">Your changes are kept.</p>
        </Notice>
      )}
    </>
  );
}

// A saved recipe that cannot build stays saved and shown; builds are refused until it is fixed or changed.
function SavedRecipeState({ settings, canEdit }: { settings: BuildRecipeSettings; canEdit: boolean }) {
  const engine = settings.recipe.engine;
  if (!isRecipeEngine(engine)) {
    return (
      <Notice tone="warn" title="The saved build engine is not recognised">
        <p className="break-words">
          This panel does not know the engine <span className="font-mono">{engine}</span>, so builds are refused.
          {canEdit && " Choose Existing Compose / Dockerfile to clear it, or use a panel that supports it."}
        </p>
      </Notice>
    );
  }
  const blocker = savedRecipeBlocker(settings);
  if (!blocker) return null;
  return (
    <Notice tone="warn" title={`The saved ${engineLabels[engine]} recipe cannot build`}>
      <p className="break-words">{blocker}</p>
      <p className="mt-1">Builds are refused until this is resolved{canEdit ? " or you choose another engine" : ""}. The saved recipe is not changed automatically.</p>
    </Notice>
  );
}

// Select reads its options from direct option children, so these are returned as elements rather than rendered by a component.
function engineOptions(settings: BuildRecipeSettings, isJob: boolean) {
  const saved = settings.recipe.engine;
  return (
    <>
      {recipeEngines.map((engine) => {
        const reason = engineUnavailable(engine, settings, isJob);
        return (
          <option key={engine} value={engine} disabled={Boolean(reason) && engine !== saved}>
            {reason ? `${engineLabels[engine]} (${reason})` : engineLabels[engine]}
          </option>
        );
      })}
      {!isRecipeEngine(saved) && (
        <option value={saved} disabled>
          {engineLabel(saved)}
        </option>
      )}
    </>
  );
}

function RecipeEditor({ workload, settings, value, errors, disabled, onChange }: { workload: Workload; settings: BuildRecipeSettings; value: BuildRecipe; errors: RecipeErrors; disabled: boolean; onChange: (next: Partial<BuildRecipe>) => void }) {
  const isJob = workload.kind === "job";
  const services = composeServices(workload.compose);
  const serviceMissing = value.service !== "" && !services.includes(value.service);
  const engine = value.engine;
  const showsFields = engine === "static" || engine === "railpack" || engine === "buildpacks";
  return (
    <SettingRows>
      <Field id="build-recipe-engine" label="Build engine" hint={engineHints[engine] ?? "Cove does not know this engine and will not build with it."} layout="row">
        <Select id="build-recipe-engine" className={inputClass} value={engine} disabled={disabled} onChange={(event) => onChange({ engine: event.target.value })} aria-describedby="build-recipe-engine-hint">
          {engineOptions(settings, isJob)}
        </Select>
        <EngineAvailability workload={workload} settings={settings} isJob={isJob} />
      </Field>
      {showsFields && (
        <>
          <Field id="build-recipe-service" label="Service" hint="The saved Compose service whose image this recipe builds." error={errors.service} layout="row">
            <Select
              id="build-recipe-service"
              className={inputClass}
              value={value.service}
              disabled={disabled}
              onChange={(event) => onChange({ service: event.target.value })}
              aria-invalid={errors.service ? true : undefined}
              aria-describedby={describedBy("build-recipe-service", errors.service, true)}
            >
              <option value="" disabled>
                Choose a service
              </option>
              {services.map((service) => (
                <option key={service} value={service}>
                  {service}
                </option>
              ))}
              {serviceMissing && (
                <option value={value.service} disabled>
                  {value.service} (not in the saved Compose file)
                </option>
              )}
            </Select>
            {services.length === 0 && <p className="text-xs text-fg-muted">No services were found in the saved Compose file.</p>}
          </Field>
          <PathField id="build-recipe-directory" label="Source directory" hint="Relative to the repository checkout. Use . for the checkout itself." value={value.directory} error={errors.directory} disabled={disabled} onChange={(directory) => onChange({ directory })} />
          {engine === "static" && (
            <PathField id="build-recipe-output" label="Output directory" hint="The already-built public files, relative to the source directory. Use . for the source directory itself." value={value.output} error={errors.output} disabled={disabled} onChange={(output) => onChange({ output })} />
          )}
          {engine === "railpack" && (
            <>
              <CommandField id="build-recipe-build-command" label="Build command" hint="Overrides the build command Railpack plans. Leave empty to use its plan." value={value.build_command} error={errors.build_command} disabled={disabled} onChange={(build_command) => onChange({ build_command })} />
              <CommandField id="build-recipe-start-command" label="Start command" hint="Overrides the start command Railpack plans. Leave empty to use its plan." value={value.start_command} error={errors.start_command} disabled={disabled} onChange={(start_command) => onChange({ start_command })} />
            </>
          )}
        </>
      )}
    </SettingRows>
  );
}

function EngineAvailability({ workload, settings, isJob }: { workload: Workload; settings: BuildRecipeSettings; isJob: boolean }) {
  const engineBlocked = engineUnavailable("railpack", settings, isJob);
  // A saved Cloud Native Buildpacks recipe already explains its refusal above the form.
  const buildpacksBlocked = settings.source_ready && settings.recipe.engine !== "buildpacks" && Boolean(engineUnavailable("buildpacks", settings, isJob));
  return (
    <>
      {engineBlocked && (
        <p className="text-xs text-fg-muted">
          {settings.source_ready ? (
            <>Static output and Railpack need a build worker that supports them. Upgrade and verify the build node.</>
          ) : (
            <>
              Static output, Railpack and Cloud Native Buildpacks need a Git source that uses the saved Compose configuration. Change it on the{" "}
              <Link to="/resources/$resourceId/source" params={{ resourceId: workload.id }} className={linkClass}>
                Source page
              </Link>
              ; Cove does not change it for you.
            </>
          )}
        </p>
      )}
      {buildpacksBlocked && (
        <p className="text-xs break-words text-fg-muted">
          Cloud Native Buildpacks is unavailable: {settings.buildpacks ? settings.buildpacks.reason || "the build node cannot run it." : "this server did not report its eligibility, so Cove treats it as unavailable."}
        </p>
      )}
    </>
  );
}

function PathField({ id, label, hint, value, error, disabled, onChange }: { id: string; label: string; hint: string; value: string; error?: string; disabled: boolean; onChange: (value: string) => void }) {
  return (
    <Field id={id} label={label} hint={hint} error={error} layout="row">
      <input
        id={id}
        className={`${inputClass} font-mono`}
        value={value}
        placeholder="."
        disabled={disabled}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, hint)}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  );
}

function CommandField({ id, label, hint, value, error, disabled, onChange }: { id: string; label: string; hint: string; value: string; error?: string; disabled: boolean; onChange: (value: string) => void }) {
  return (
    <Field id={id} label={label} hint={hint} error={error} optional layout="row">
      <textarea
        id={id}
        className={`${inputClass} font-mono`}
        rows={2}
        value={value}
        disabled={disabled}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, hint)}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  );
}

// Readers never see command text, only whether an override is saved.
function RecipeSummary({ settings }: { settings: BuildRecipeSettings }) {
  const recipe = settings.recipe;
  const override = (set: boolean) => (set ? "Custom command saved" : "Railpack plan");
  return (
    <SettingRows>
      <SettingRow label="Build engine" description={engineHints[recipe.engine]}>
        <span className="break-words">{engineLabel(recipe.engine)}</span>
      </SettingRow>
      {recipe.engine !== "compose" && recipe.service && (
        <SettingRow label="Service">
          <span className="font-mono break-all">{recipe.service}</span>
        </SettingRow>
      )}
      {recipe.directory && (
        <SettingRow label="Source directory">
          <span className="font-mono break-all">{recipe.directory}</span>
        </SettingRow>
      )}
      {recipe.engine === "static" && recipe.output && (
        <SettingRow label="Output directory">
          <span className="font-mono break-all">{recipe.output}</span>
        </SettingRow>
      )}
      {recipe.engine === "railpack" && (
        <>
          <SettingRow label="Build command">{override(settings.build_command_set)}</SettingRow>
          <SettingRow label="Start command">{override(settings.start_command_set)}</SettingRow>
        </>
      )}
    </SettingRows>
  );
}

function RailpackDetails({ settings }: { settings: BuildRecipeSettings }) {
  return (
    <div className="space-y-3">
      <Notice tone="info" title="Railpack uses the network while it plans and builds">
        <p>{railpackDisclosure}</p>
        <p className="mt-1">
          A build network of none and outbound build proxies are unavailable with Railpack, because the pinned Railpack frontend does not apply those policies to its build steps. Cove refuses to save
          or build a Railpack recipe while either is configured.
        </p>
        <p className="mt-1">
          The planner receives the names of the selected build secrets, never their values; build secrets are otherwise handled as in any other build.
        </p>
      </Notice>
      <div className="space-y-1.5 text-xs text-fg-muted">
        <p>
          Railpack <span className="font-mono text-fg">{settings.railpack_version || "version not reported"}</span>, with these pinned images:
        </p>
        {settings.pinned_images.length > 0 ? (
          <ul aria-label="Pinned Railpack images" className="space-y-1">
            {settings.pinned_images.map((image) => (
              <li key={image} className="font-mono break-all text-fg">
                {image}
              </li>
            ))}
          </ul>
        ) : (
          <p>No pinned images were reported.</p>
        )}
      </div>
    </div>
  );
}

function Value({ children }: { children: string }) {
  return <span className="text-fg tabular-nums">{children}</span>;
}

// Every number comes from the server's read; without it the panel says nothing about sizes, versions or pins.
function BuildpacksDetails({ settings }: { settings: BuildRecipeSettings }) {
  const buildpacks = settings.buildpacks;
  return (
    <div className="space-y-3">
      <Notice tone="info" title="Cloud Native Buildpacks builds with the pinned Paketo builder">
        <p>Builds use the pinned official Paketo builder&apos;s buildpacks. No repository-selected replacement builder, external or inline buildpacks or image extensions are supported.</p>
        <p className="mt-1">The selected service must be the only Compose service with a build definition. Other image-only services are unchanged.</p>
        <p className="mt-1">
          Analyze, detect, restore, build and export run on the effective build node without a Docker socket or registry, Git or host credentials. Only the build phase receives the selected
          build secrets; a buildpack can read those values and can copy them into its output, as in any other build. Detection gets no Cove secret values.
        </p>
        <p className="mt-1">The lifecycle&apos;s OCI-layout export is experimental upstream. Cove validates the output before importing and publishing it. Building never deploys.</p>
        <p className="mt-1">
          With default build networking, buildpacks download toolchains and packages from upstream through Cove&apos;s confined HTTP(S) egress path. A build network of none is enforced, so
          dependency downloads fail clearly. Configured HTTP(S) proxies are applied through the trusted relay. Private, host and cloud metadata destinations and non-HTTP(S) build egress are
          unavailable, and host or custom build networks are refused. The pins cover the lifecycle, builder and run images, not the toolchains or packages buildpacks download, so these builds are
          neither reproducible nor fully pinned.
        </p>
      </Notice>
      <div className="space-y-1.5 text-xs text-fg-muted">
        {buildpacks ? (
          <>
            <p className="font-medium text-fg">Resources on the build node</p>
            <ul aria-label="Cloud Native Buildpacks resource needs" className="list-disc space-y-0.5 pl-4">
              <li>
                Work is operation-owned disk state with a monitored <Value>{formatMemory(buildpacks.work_target_bytes)}</Value> target, not a hard storage cap. Cove cancels the build when it
                observes the target overrun or node resource pressure; brief growth between checks is possible. There is no persistent Cloud Native Buildpacks cache, and this state is separate from
                the BuildKit hard cache limits. Cleanup is journaled; a failed or cancelled build can leave cleanup pending until the worker recovers.
              </li>
              <li>
                The node needs at least <Value>{formatMemory(buildpacks.required_available_memory_bytes)}</Value> of actually available memory. At admission the phase memory limit is the
                available memory minus <Value>{formatMemory(buildpacks.memory_safety_bytes)}</Value> of safety headroom, clamped between{" "}
                <Value>{formatMemory(buildpacks.minimum_phase_memory_bytes)}</Value> and <Value>{formatMemory(buildpacks.maximum_phase_memory_bytes)}</Value> and rounded down to a whole MiB. The
                limit is recorded in the operation and build history, and a build that exceeds it can fail. There is no operator memory setting.
              </li>
              <li>
                The node needs <Value>{formatMemory(buildpacks.required_docker_disk_bytes)}</Value> of free Docker storage and <Value>{formatMemory(buildpacks.required_worker_disk_bytes)}</Value> of
                free worker-state storage. The builder image is about <Value>{formatGB(buildpacks.builder_download_bytes)}</Value> to download and about{" "}
                <Value>{formatGB(buildpacks.builder_unpacked_bytes)}</Value> once unpacked.
              </li>
              <li>
                Where a team budget is configured, a build temporarily reserves its phase memory limit plus helper overhead, up to <Value>{formatMemory(buildpacks.quota_memory_mb * 1024 ** 2)}</Value>,
                and <Value>{formatCPU(buildpacks.quota_cpu)}</Value> including helpers. The memory figure is a maximum; most builds get a smaller phase limit. Builds wait for the node&apos;s other
                build and cache operations.
              </li>
              <li>Before publishing, the worker rechecks current memory, disk, architecture, policy and the frozen build metadata. A queued build is not a promise that it runs under resource pressure.</li>
            </ul>
            <p className="pt-1">
              Lifecycle <span className="font-mono text-fg">{buildpacks.lifecycle_version}</span>, builder <span className="font-mono text-fg">{buildpacks.builder_version}</span> and platform API{" "}
              <span className="font-mono text-fg">{buildpacks.platform_api}</span>, with these pinned images:
            </p>
            {buildpacks.pinned_images.length > 0 ? (
              <ul aria-label="Pinned Cloud Native Buildpacks images" className="space-y-1">
                {buildpacks.pinned_images.map((image) => (
                  <li key={image} className="font-mono break-all text-fg">
                    {image}
                  </li>
                ))}
              </ul>
            ) : (
              <p>No pinned images were reported.</p>
            )}
          </>
        ) : (
          <p>This server did not report the resource needs, versions or pinned images for Cloud Native Buildpacks.</p>
        )}
      </div>
    </div>
  );
}
