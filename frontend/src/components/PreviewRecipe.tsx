import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import type { Project } from "../api/projects";
import { recipeQuery, useSaveRecipe, type PlannedResource, type PlannedVolume, type PreviewPlan, type PreviewRecipe, type PreviewResourcePolicy, type RecipeView, type VariableMode } from "../api/previews";
import { useOnline } from "../lib/hooks";
import { draftFromView, formatBytes, missingOverrides, recipeForSave, resourceSource, setOmitted, setVariable, shortRevision, unresolved, updatePolicy } from "../lib/previews";
import { choiceClass, inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows, SwitchField } from "./ui";

export function PreviewRecipeEditor({ project }: { project: Project }) {
  const [environmentId, setEnvironmentId] = useState("");
  const recipe = useQuery(recipeQuery(project.id, environmentId));
  const production = project.environments.filter((environment) => !environment.preview_id);

  if (recipe.isPending) {
    return (
      <Section title="Preview recipe" flush>
        <LoadingRows label="Loading the preview recipe and production environment" />
      </Section>
    );
  }
  if (recipe.isError) {
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what="the preview recipe" error={recipe.error} onRetry={() => void recipe.refetch()} retrying={recipe.isFetching} />
      </div>
    );
  }
  const selected = environmentId || recipe.data.recipe.base_environment_id;
  return (
    <RecipeForm
      // A new environment or a saved recipe starts a new draft; refetches of the same plan keep edits.
      key={`${selected}:${recipe.dataUpdatedAt}`}
      project={project}
      view={recipe.data}
      environmentId={selected}
      environments={production}
      onEnvironment={setEnvironmentId}
      refreshing={recipe.isFetching}
    />
  );
}

function RecipeForm({ project, view, environmentId, environments, onEnvironment, refreshing }: { project: Project; view: RecipeView; environmentId: string; environments: Project["environments"]; onEnvironment: (id: string) => void; refreshing: boolean }) {
  const online = useOnline();
  const save = useSaveRecipe(project.id);
  const initial = draftFromView(view, environmentId);
  const [draft, setDraft] = useState<PreviewRecipe>(initial);
  const [saved, setSaved] = useState(false);
  const dirty = JSON.stringify(draft) !== JSON.stringify(initial);
  const plan = view.plan;
  const blockers = unresolved(plan);
  const missing = missingOverrides(draft);
  const edit = (next: PreviewRecipe) => {
    setSaved(false);
    setDraft(next);
  };

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (missing.length > 0) return;
    save.mutate(recipeForSave(draft), { onSuccess: () => setSaved(true) });
  }

  return (
    <form onSubmit={submit}>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Preview recipe"
        width="form"
        description="A preview copies the selected production environment for one branch name across your repositories. Saved changes apply to previews created afterwards."
      >
        <GeneralSettings draft={draft} edit={edit} plan={plan} environmentId={environmentId} environments={environments} onEnvironment={onEnvironment} dirty={dirty} />
      </Section>
      <Section
        title="Resources"
        flush
        description="How each production resource is copied. Previews never mount, restart or write production storage, and never join production networks."
        footer={
          <div className="space-y-3">
            {save.isError && (
              <Notice tone="error" title="Could not save the recipe">
                {errorMessage(save.error)}
              </Notice>
            )}
            {missing.length > 0 && (
              <Notice tone="warn" title="Enter override values">
                {missing.join(", ")} {missing.length === 1 ? "needs" : "need"} a preview value.
              </Notice>
            )}
            <SaveBar
              dirty={dirty}
              pending={save.isPending}
              blocked={!online || missing.length > 0 || !draft.base_environment_id}
              label="Save recipe"
              onDiscard={() => setDraft(initial)}
              status={recipeStatus({ refreshing, saved, blockers, persisted: view.saved })}
            />
          </div>
        }
      >
        <ResourceList plan={plan} draft={draft} edit={edit} />
      </Section>
    </form>
  );
}

function recipeStatus({ refreshing, saved, blockers, persisted }: { refreshing: boolean; saved: boolean; blockers: number; persisted: boolean }) {
  if (refreshing) return "Rechecking the production environment…";
  if (saved) return "Recipe saved.";
  if (blockers > 0) return `${blockers} ${blockers === 1 ? "item needs" : "items need"} a decision before previews can be created.`;
  return persisted ? "Ready to create previews." : "Not saved yet.";
}

function ResourceList({ plan, draft, edit }: { plan: PreviewPlan | null; draft: PreviewRecipe; edit: (recipe: PreviewRecipe) => void }) {
  if (!plan) return <p className="px-4 py-3 text-[13px] text-fg-muted">Choose the production environment to see its resources.</p>;
  if (plan.resources.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">This environment has no resources.</p>;
  return (
    <div className="divide-y divide-line">
      {plan.resources.map((resource) => (
        <ResourcePolicy key={resource.id} resource={resource} policy={draft.resources[resource.id] ?? {}} recipe={draft} onChange={edit} />
      ))}
    </div>
  );
}

interface SettingsProps {
  draft: PreviewRecipe;
  edit: (recipe: PreviewRecipe) => void;
  plan: PreviewPlan | null;
  environmentId: string;
}

// Empty or partial numeric input becomes 0, which the field's min and the server both reject.
function wholeNumber(value: string) {
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) ? parsed : 0;
}

function GeneralSettings({ draft, edit, plan, environmentId, environments, onEnvironment, dirty }: SettingsProps & { environments: Project["environments"]; onEnvironment: (id: string) => void; dirty: boolean }) {
  const id = useId();
  return (
    <SettingRows>
      <SwitchField id={`${id}-enabled`} layout="row" label="Create previews from branches" description="Matching branch pushes create or update previews. Turning this off stops new previews and branch updates; existing previews keep running until they are removed." checked={draft.enabled} onChange={(event) => edit({ ...draft, enabled: event.target.checked })} />
      <Field id={`${id}-environment`} layout="row" label="Production environment" hint="Previews copy this environment's deployed revisions, data and connections.">
        <Select
          id={`${id}-environment`}
          value={environmentId}
          required
          onChange={(event) => {
            if (dirty && !window.confirm("Switching the environment discards unsaved recipe changes.")) return;
            onEnvironment(event.target.value);
          }}
        >
          <option value="" disabled>
            Choose an environment
          </option>
          {environments.map((environment) => (
            <option key={environment.id} value={environment.id}>
              {environment.name}
            </option>
          ))}
        </Select>
      </Field>
      <Field id={`${id}-pattern`} layout="row" label="Branch pattern" hint="A branch prefix ending in *. The part after the prefix names the preview, so staging/checkout in any repository joins the checkout preview.">
        <input id={`${id}-pattern`} className={`${inputClass} font-mono`} value={draft.branch_pattern} onChange={(event) => edit({ ...draft, branch_pattern: event.target.value })} required maxLength={100} spellCheck={false} autoComplete="off" aria-describedby={`${id}-pattern-hint`} />
      </Field>
      <SettingRow label="Repositories" description="Resources from these repositories follow matching branches. Others always run the production revision pinned when the preview is created.">
        <RepositoryChoices draft={draft} edit={edit} plan={plan} environmentId={environmentId} />
      </SettingRow>
      <Field
        id={`${id}-domain`}
        layout="row"
        label="Domain template"
        optional
        hint={
          <>
            Use {"{resource}"} and {"{preview}"}, for example {"{resource}-{preview}.preview.example.com"}. Leave empty to publish no routes.
            {plan && plan.example_hostnames.length > 0 && (
              <>
                {" "}
                Example: <span className="break-all font-mono">{plan.example_hostnames[0]}</span>.{" "}
                {plan.dns === "cloudflare" ? "Cove creates the DNS records through Cloudflare." : "Point these names at the node yourself, for example with a wildcard record."}
              </>
            )}
          </>
        }
      >
        <input id={`${id}-domain`} className={`${inputClass} font-mono`} value={draft.domain_template} onChange={(event) => edit({ ...draft, domain_template: event.target.value.trim() })} maxLength={200} spellCheck={false} autoComplete="off" aria-describedby={`${id}-domain-hint`} />
      </Field>
      <Field id={`${id}-access`} layout="row" label="Access" hint={draft.access === "private" ? "Each preview gets its own username and password. Routes go live only after every resource is healthy." : "Anyone with the address can open the preview, including its copy of production data."}>
        <Select id={`${id}-access`} value={draft.access} onChange={(event) => edit({ ...draft, access: event.target.value === "public" ? "public" : "private" })} aria-describedby={`${id}-access-hint`}>
          <option value="private">Private: password required</option>
          <option value="public">Public</option>
        </Select>
      </Field>
      <Field id={`${id}-ttl`} layout="row" label="Remove after inactivity" hint="Hours without a push or preview action. Pinned previews are kept.">
        <input id={`${id}-ttl`} type="number" min={1} max={720} className={`${inputClass} tabular-nums`} value={draft.ttl_hours} onChange={(event) => edit({ ...draft, ttl_hours: wholeNumber(event.target.value) })} required aria-describedby={`${id}-ttl-hint`} />
      </Field>
      <Field id={`${id}-max`} layout="row" label="Previews at a time" hint="Further branches wait until a preview is removed. Each preview copies the production data again.">
        <input id={`${id}-max`} type="number" min={1} max={20} className={`${inputClass} tabular-nums`} value={draft.max_previews} onChange={(event) => edit({ ...draft, max_previews: wholeNumber(event.target.value) })} required aria-describedby={`${id}-max-hint`} />
      </Field>
      <SwitchField id={`${id}-approval`} layout="row" label="Require approval" description="Anyone who can push a matching branch could otherwise get a copy of production data. An editor approves each new preview." checked={draft.require_approval} onChange={(event) => edit({ ...draft, require_approval: event.target.checked })} />
    </SettingRows>
  );
}

function RepositoryChoices({ draft, edit, plan, environmentId }: SettingsProps) {
  const selected = new Set(draft.repositories);
  return (
    <>
    {plan && plan.repositories.length > 0 ? (
      <fieldset className="grid w-full gap-2">
        <legend className="sr-only">Repositories that follow matching branches</legend>
        {plan.repositories.map((repository) => (
          <label key={repository.id} className={choiceClass}>
            <Checkbox
              checked={selected.has(repository.id)}
              disabled={!repository.assigned && !selected.has(repository.id)}
              onChange={(event) => edit({ ...draft, repositories: event.target.checked ? [...draft.repositories, repository.id] : draft.repositories.filter((value) => value !== repository.id) })}
            />
            <span className="min-w-0 break-all font-mono text-xs">{repository.name}</span>
            {!repository.assigned && <span className="ml-auto shrink-0 text-xs text-warn">Installation not shared with this team</span>}
          </label>
        ))}
      </fieldset>
    ) : (
      <p className="text-xs text-fg-muted">{environmentId ? "No resources in this environment use a GitHub App repository." : "Choose an environment first."}</p>
    )}
    </>
  );
}

const variableModes: { value: VariableMode; label: string }[] = [
  { value: "copy", label: "Copy production value" },
  { value: "override", label: "Use a preview value" },
  { value: "remove", label: "Remove" },
];

type PolicyUpdate = (change: (policy: PreviewResourcePolicy) => PreviewResourcePolicy) => void;

interface PolicyProps {
  resource: PlannedResource;
  policy: PreviewResourcePolicy;
  update: PolicyUpdate;
}

function ResourcePolicy({ resource, policy, recipe, onChange }: { resource: PlannedResource; policy: PreviewResourcePolicy; recipe: PreviewRecipe; onChange: (recipe: PreviewRecipe) => void }) {
  const id = useId();
  const update: PolicyUpdate = (change) => onChange(updatePolicy(recipe, resource.id, change));
  return (
    <div className="min-w-0 space-y-4 p-4">
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="min-w-0 flex-1 basis-64">
          <h3 className="text-[13px] font-semibold break-words text-fg">{resource.name}</h3>
          <p className="mt-0.5 text-xs text-fg-muted">
            {resource.kind === "job" ? "Job" : "Service"}
            {resource.repository && (
              <>
                {" · "}
                <span className="break-all font-mono">{resource.repository}</span>
              </>
            )}
            {resource.deployed_revision && (
              <>
                {" · deployed "}
                <span className="font-mono">{shortRevision(resource.deployed_revision)}</span>
              </>
            )}
          </p>
          {resource.included && <p className="mt-0.5 text-xs text-fg-muted">{resourceSource(resource)}</p>}
        </div>
        <SwitchField id={`${id}-include`} layout="row" label="Include" checked={!policy.exclude} onChange={(event) => update((current) => ({ ...current, exclude: !event.target.checked }))} />
      </div>
      {resource.included ? (
        <>
          <Blockers resource={resource} />
          <DatabaseChoice resource={resource} policy={policy} update={update} />
          <StorageTable resource={resource} policy={policy} update={update} />
          {resource.kind === "job" ? <JobControls resource={resource} policy={policy} update={update} /> : <ServiceChoices resource={resource} policy={policy} update={update} />}
          {resource.variables.length > 0 && <Variables resource={resource} policy={policy} recipe={recipe} onChange={onChange} />}
          <OutsideDependencies resource={resource} policy={policy} recipe={recipe} onChange={onChange} />
          <FileSecretsChoice resource={resource} policy={policy} update={update} />
        </>
      ) : (
        <p className="text-xs text-fg-muted">Not copied. Preview resources that connect to it treat it as outside the preview.</p>
      )}
    </div>
  );
}

function Blockers({ resource }: { resource: PlannedResource }) {
  return (
    <>
      {resource.blockers.length > 0 && (
        <Notice tone="warn" title={`${resource.blockers.length} ${resource.blockers.length === 1 ? "decision" : "decisions"} needed`}>
          <ul className="list-disc space-y-1 pl-4">
            {resource.blockers.map((blocker) => (
              <li key={`${blocker.code}:${blocker.key ?? ""}:${blocker.message}`}>
                {blocker.message} <span className="text-fg-subtle">{blocker.resolution}</span>
              </li>
            ))}
          </ul>
        </Notice>
      )}
      {resource.notes.map((note) => (
        <p key={note} className="text-xs text-fg-muted">
          {note}
        </p>
      ))}
    </>
  );
}

function DatabaseChoice({ resource, policy, update }: PolicyProps) {
  const id = useId();
  const database = resource.database;
  if (!database) return null;
  const size = database.bytes ? `, about ${formatBytes(database.bytes)}` : "";
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id={id} label={`${database.engine === "postgresql" ? "PostgreSQL" : "MariaDB"} database ${database.name}`} hint={database.supported ? `A fresh logical snapshot when each preview is created${size}. Several databases are not snapshotted at the same instant.` : database.reason}>
        <Select id={id} value={policy.database ?? "copy"} onChange={(event) => update((current) => ({ ...current, database: event.target.value === "empty" ? "empty" : "copy" }))} aria-describedby={`${id}-hint`}>
          <option value="copy">Copy production data</option>
          <option value="empty">Start empty</option>
        </Select>
      </Field>
    </div>
  );
}

function StorageTable({ resource, policy, update }: PolicyProps) {
  if (resource.volumes.length === 0) return null;
  return (
    <div className="min-w-0">
      <h4 className="mb-1 text-[11px] font-medium text-fg-subtle">Storage</h4>
      <table className="w-full table-fixed text-[13px]">
        <thead>
          <tr className={`${tableHeadClass} text-left`}>
            <th scope="col">Production storage</th>
            <th scope="col" className="hidden w-40 sm:table-cell">
              Used by
            </th>
            <th scope="col" className="w-44 sm:w-52">
              In previews
            </th>
          </tr>
        </thead>
        <tbody>
          {resource.volumes.map((volume) => (
            <tr key={volume.key} className={`${tableRowClass} align-top hover:bg-transparent`}>
              <td className="py-2 pr-3">
                <span className="block break-all font-mono text-xs text-fg">{volume.source}</span>
                <span className="block text-xs text-fg-muted">{volume.reason}</span>
                <span className="block text-xs text-fg-subtle sm:hidden">Used by {volume.services.join(", ")}</span>
              </td>
              <td className="hidden py-2 pr-3 text-xs text-fg-muted sm:table-cell">{volume.services.join(", ")}</td>
              <td className="py-2">
                <VolumeChoice volume={volume} value={policy.volumes?.[volume.key] ?? volume.default} onChange={(mode) => update((current) => ({ ...current, volumes: { ...current.volumes, [volume.key]: mode } }))} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function VolumeChoice({ volume, value, onChange }: { volume: PlannedVolume; value: string; onChange: (mode: "copy" | "empty") => void }) {
  if (volume.allowed.includes("database")) {
    return <span className="text-xs text-fg-muted">{volume.mode === "database" ? "Restored from the database snapshot" : "Starts empty"}</span>;
  }
  if (volume.allowed.length === 0) {
    return <span className="text-xs text-warn">Cannot be copied</span>;
  }
  return (
    <Select aria-label={`Storage for ${volume.source} in previews`} value={value} required onChange={(event) => onChange(event.target.value === "copy" ? "copy" : "empty")}>
      <option value="" disabled>
        Choose
      </option>
      {volume.allowed.map((option) => (
        <option key={option} value={option}>
          {option === "copy" ? "Copy files" : "Start empty"}
        </option>
      ))}
    </Select>
  );
}

function JobControls({ resource, policy, update }: PolicyProps) {
  const id = useId();
  const needsTags = resource.blockers.some((blocker) => blocker.key === "image_tags");
  return (
    <div className="space-y-3">
      <SwitchField id={id} label="Run on its production schedule" description={resource.schedule ? `Production runs it on ${resource.schedule}. Jobs never run in previews unless this is on.` : "This job has no schedule; it can be run from its resource page."} checked={!!policy.schedule} disabled={!resource.schedule} onChange={(event) => update((current) => ({ ...current, schedule: event.target.checked }))} />
      {(needsTags || policy.image_tags) && (
        <label className="flex items-start gap-2.5 text-[13px] text-fg">
          <Checkbox checked={!!policy.image_tags} onChange={(event) => update((current) => ({ ...current, image_tags: event.target.checked }))} />
          <span>Accept that image tags may resolve to newer images than production last ran</span>
        </label>
      )}
    </div>
  );
}

function ServiceChoices({ resource, policy, update }: PolicyProps) {
  if (resource.services.length === 0) return null;
  return (
    <fieldset className="min-w-0">
      <legend className="mb-1 text-[11px] font-medium text-fg-subtle">Services that run</legend>
      <div className="grid gap-2 md:grid-cols-2">
        {resource.services.map((service) => (
          <label key={service.name} className={`${choiceClass} items-start`}>
            <Checkbox checked={policy.services?.[service.name] ?? service.default} onChange={(event) => update((current) => ({ ...current, services: { ...current.services, [service.name]: event.target.checked } }))} />
            <span className="min-w-0 break-all font-mono text-xs text-fg">
              {service.name}
              <span className="block font-sans break-normal text-fg-muted">{service.reason}</span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

function OutsideDependencies({ resource, policy, recipe, onChange }: { resource: PlannedResource; policy: PreviewResourcePolicy; recipe: PreviewRecipe; onChange: (recipe: PreviewRecipe) => void }) {
  const outside = resource.dependencies.filter((dependency) => !dependency.inside);
  if (outside.length === 0) return null;
  return (
    <fieldset className="min-w-0">
      <legend className="mb-1 text-[11px] font-medium text-fg-subtle">Outside this preview</legend>
      <div className="grid gap-2">
        {outside.map((dependency) =>
          dependency.kind === "variable" ? (
            <p key={dependency.key} className="text-xs text-fg-muted">
              <span className="font-mono text-fg">{dependency.label}</span> reads a value from a resource outside the preview; decide it under Variables.
            </p>
          ) : (
            <label key={dependency.key} className={choiceClass}>
              <Checkbox checked={policy.external?.[dependency.key] === "omit"} onChange={(event) => onChange(setOmitted(recipe, resource.id, dependency.key, event.target.checked))} />
              <span className="min-w-0">
                Omit {dependencyKinds[dependency.kind]} <span className="break-all font-mono text-xs">{dependency.label}</span>
              </span>
            </label>
          ),
        )}
      </div>
    </fieldset>
  );
}

function FileSecretsChoice({ resource, policy, update }: PolicyProps) {
  const id = useId();
  if (!resource.file_secrets) return null;
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id={id} label="Secret files" hint={`${resource.file_secrets} files are mounted in production.`}>
        <Select id={id} value={policy.file_secrets ?? ""} required onChange={(event) => update((current) => ({ ...current, file_secrets: event.target.value === "copy" ? "copy" : "remove" }))} aria-describedby={`${id}-hint`}>
          <option value="" disabled>
            Choose
          </option>
          <option value="copy">Copy production files</option>
          <option value="remove">Remove</option>
        </Select>
      </Field>
    </div>
  );
}

const dependencyKinds = { connection: "private connection", network: "members of shared network", external_network: "external network", variable: "variable" } as const;

function Variables({ resource, policy, recipe, onChange }: { resource: PlannedResource; policy: PreviewResourcePolicy; recipe: PreviewRecipe; onChange: (recipe: PreviewRecipe) => void }) {
  return (
    <div className="min-w-0">
      <h4 className="mb-1 text-[11px] font-medium text-fg-subtle">Variables</h4>
      <table className="w-full table-fixed text-[13px]">
        <thead>
          <tr className={`${tableHeadClass} text-left`}>
            <th scope="col">Name</th>
            <th scope="col" className="w-44 sm:w-72">
              In previews
            </th>
          </tr>
        </thead>
        <tbody>
          {resource.variables.map((variable) => {
            const chosen = policy.variables?.[variable.key];
            const mode = chosen?.mode ?? variable.mode;
            const saved = chosen?.has_value ?? variable.has_override;
            return (
              <tr key={variable.key} className={`${tableRowClass} align-top hover:bg-transparent`}>
                <td className="py-2 pr-3">
                  <span className="block break-all font-mono text-xs text-fg">{variable.key}</span>
                  <span className={`block text-xs ${variable.classification === "sensitive" && mode === "" ? "text-warn" : "text-fg-muted"}`}>{variable.reason}</span>
                </td>
                <td className="space-y-2 py-2">
                  <Select aria-label={`${variable.key} in previews`} value={mode} required onChange={(event) => onChange(setVariable(recipe, resource.id, variable.key, event.target.value as VariableMode))}>
                    <option value="" disabled>
                      Decide
                    </option>
                    {variableModes.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                  {mode === "override" && (
                    <input
                      type="password"
                      aria-label={`Preview value for ${variable.key}`}
                      className={`${inputClass} font-mono`}
                      value={chosen?.value ?? ""}
                      placeholder={saved ? "Saved value is kept" : "Preview value"}
                      autoComplete="off"
                      spellCheck={false}
                      onChange={(event) => onChange(setVariable(recipe, resource.id, variable.key, "override", event.target.value))}
                    />
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
