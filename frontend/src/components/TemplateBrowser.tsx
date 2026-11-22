import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, ArrowRight, ChevronRight, ExternalLink, FileCode2, Trash2 } from "lucide-react";
import { useId, useMemo, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { entryKey, templatesQuery, useDeleteTeamTemplate, useTemplateDraft, type ResourceDraft, type TemplateEntry, type TemplateEvidence, type TemplateVariable } from "../api/templates";
import { formatAbsolute, formatBytes } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { useCurrentTeam } from "../lib/session";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { capabilityLabel, draftRequest, initialValues, originLabel, shortRevision, validateDraft } from "../lib/templates";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { FilterSelect, FilterToolbar, NoMatches } from "./FilterToolbar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, MetaList, Notice, QueryError, Section, Subsection } from "./ui";

interface TemplateBrowserProps {
  onUse: (draft: ResourceDraft) => void;
  onCustom: () => void;
}

interface Filters {
  search: string;
  category: string;
  origin: string;
}

const noFilters: Filters = { search: "", category: "", origin: "" };

export function TemplateBrowser({ onUse, onCustom }: TemplateBrowserProps) {
  const catalogue = useQuery(templatesQuery);
  const [filters, setFilters] = useState<Filters>(noFilters);
  const [selected, setSelected] = useState<string | null>(null);
  const entries = catalogue.data?.templates ?? [];

  if (selected) {
    const entry = entries.find((item) => entryKey(item) === selected);
    return <TemplateSetup key={selected} entry={entry} canManage={catalogue.data?.can_manage_team ?? false} onBack={() => setSelected(null)} onUse={onUse} />;
  }

  let body;
  if (catalogue.isPending) body = <LoadingRows label="Loading templates" rows={6} />;
  else if (catalogue.isError)
    body = (
      <div className="p-4">
        <QueryError what="the template catalogue" error={catalogue.error} onRetry={() => void catalogue.refetch()} retrying={catalogue.isFetching} />
      </div>
    );
  else if (entries.length === 0)
    body = (
      <div className="px-4 py-6">
        <p className="text-[13px] font-medium text-fg">No templates available</p>
        <p className="mt-0.5 text-xs text-fg-muted">The catalogue is empty for this team. Use a custom Compose file or Git repository instead.</p>
      </div>
    );
  else body = <TemplateList entries={entries} filters={filters} onFilters={setFilters} onSelect={setSelected} />;

  return (
    <Section
      title="Resource templates"
      description="Start from a reviewed Compose configuration, or bring your own. Choosing a template prepares a draft; nothing is created or deployed until you review it."
      actions={<Button onClick={onCustom}>Custom Compose or Git</Button>}
      flush
    >
      {body}
    </Section>
  );
}

function TemplateList({ entries, filters, onFilters, onSelect }: { entries: TemplateEntry[]; filters: Filters; onFilters: (filters: Filters) => void; onSelect: (key: string) => void }) {
  const categories = useMemo(() => [...new Set(entries.map((entry) => entry.template.category))].sort((a, b) => a.localeCompare(b)), [entries]);
  const origins = useMemo(() => [...new Set(entries.map((entry) => entry.origin))], [entries]);
  const query = filters.search.trim().toLowerCase();
  const sorted = useMemo(() => [...entries].sort((a, b) => a.template.name.localeCompare(b.template.name, undefined, { sensitivity: "base" })), [entries]);
  const matches = sorted.filter(
    (entry) =>
      (filters.category === "" || entry.template.category === filters.category) &&
      (filters.origin === "" || entry.origin === filters.origin) &&
      `${entry.template.name} ${entry.template.description} ${entry.template.category} ${entry.template.id}`.toLowerCase().includes(query),
  );
  const filtering = query !== "" || filters.category !== "" || filters.origin !== "";
  const reset = () => onFilters(noFilters);
  return (
    <>
      <FilterToolbar
        label="Filter templates"
        search={filters.search}
        onSearch={(search) => onFilters({ ...filters, search })}
        searchPlaceholder="Search templates"
        active={filtering}
        onReset={reset}
        summary={filtering ? `${matches.length} of ${entries.length} templates` : `${entries.length} templates`}
      >
        <FilterSelect id="template-category" label="Category" value={filters.category} onChange={(category) => onFilters({ ...filters, category })}>
          <option value="">All categories</option>
          {categories.map((item) => (
            <option key={item} value={item}>
              {item}
            </option>
          ))}
        </FilterSelect>
        {origins.length > 1 && (
          <FilterSelect id="template-origin" label="Origin" value={filters.origin} onChange={(origin) => onFilters({ ...filters, origin })}>
            <option value="">All origins</option>
            {origins.map((item) => (
              <option key={item} value={item}>
                {originLabel({ origin: item })}
              </option>
            ))}
          </FilterSelect>
        )}
      </FilterToolbar>
      {matches.length === 0 ? (
        <NoMatches what="templates" scope="No templates match the search and filters. Try another search or use a custom Compose file." onReset={reset} />
      ) : (
        <ul className="divide-y divide-line">
          {matches.map((entry) => (
            <li key={entryKey(entry)}>
              <TemplateRow entry={entry} onSelect={() => onSelect(entryKey(entry))} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function TemplateRow({ entry, onSelect }: { entry: TemplateEntry; onSelect: () => void }) {
  const { template } = entry;
  const hostAccess = (template.capabilities ?? []).length > 0;
  return (
    <button type="button" onClick={onSelect} className="flex w-full items-center gap-3 px-3 py-2.5 text-left transition-colors hover:bg-raised pointer-coarse:min-h-11">
      <span className="min-w-0 flex-1">
        <span className="block text-[13px] font-medium break-words text-fg">{template.name}</span>
        <span className="mt-0.5 block text-xs break-words text-fg-muted">{template.description}</span>
        <span className="mt-1 block text-xs text-fg-muted">
          <MetaList>
            <span key="category">{template.category}</span>
            <span key="origin">{originLabel(entry)}</span>
            {entry.validation.runtime === "started_healthy" && <span key="checked">Runtime checked</span>}
            {hostAccess && (
              <span key="host" className="text-warn">
                Host access
              </span>
            )}
          </MetaList>
        </span>
      </span>
      <ChevronRight aria-hidden className="size-4 shrink-0 text-fg-subtle" />
    </button>
  );
}

interface SetupProps {
  entry: TemplateEntry | undefined;
  canManage: boolean;
  onBack: () => void;
  onUse: (draft: ResourceDraft) => void;
}

function TemplateSetup({ entry, canManage, onBack, onUse }: SetupProps) {
  const back = (
    <Button variant="ghost" onClick={onBack}>
      <ArrowLeft aria-hidden className="size-3.5" />
      Back to templates
    </Button>
  );
  if (!entry) {
    return (
      <Section title="Template unavailable" actions={back}>
        <Notice tone="warn" title="This template is no longer in the catalogue">
          Its source was removed or no longer offers it. Choose another template.
        </Notice>
      </Section>
    );
  }
  const { template } = entry;
  return (
    <Section
      title={template.name}
      description={template.description}
      actions={
        <>
          {entry.origin === "team" && canManage && <DeleteTeamTemplate templateId={template.id} name={template.name} onDeleted={onBack} />}
          {back}
        </>
      }
      flush
    >
      <div className="grid min-w-0 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)] lg:divide-x lg:divide-line">
        <TemplateForm entry={entry} onUse={onUse} />
        <TemplateDetails entry={entry} />
      </div>
    </Section>
  );
}

function TemplateForm({ entry, onUse }: { entry: TemplateEntry; onUse: (draft: ResourceDraft) => void }) {
  const id = useId();
  const online = useOnline();
  const draft = useTemplateDraft();
  const { template } = entry;
  const capabilities = template.capabilities ?? [];
  const [values, setValues] = useState(() => initialValues(template));
  const [hostname, setHostname] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problems = validateDraft(entry, values, hostname, acknowledged);
    setErrors(problems);
    if (Object.keys(problems).length > 0) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    draft.mutate(draftRequest(entry, values, hostname, acknowledged), { onSuccess: (result) => onUse(result) });
  }

  const web = template.web;
  return (
    <form onSubmit={submit} noValidate className="min-w-0 divide-y divide-line">
      <fieldset disabled={draft.isPending} className="min-w-0 divide-y divide-line">
        {web?.route ? (
          <Subsection title="Public route" description={`Cove creates an HTTPS route to ${web.service}:${web.port}${web.url_variable ? ` and sets ${web.url_variable} from this hostname` : ""}. Point the hostname at the worker before deploying.`}>
            <Field id={`${id}-hostname`} label="Hostname" error={errors.hostname}>
              <input
                id={`${id}-hostname`}
                className={`${inputClass} font-mono`}
                value={hostname}
                onChange={(event) => setHostname(event.target.value)}
                placeholder="app.example.com"
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                inputMode="url"
                aria-invalid={errors.hostname ? true : undefined}
                aria-describedby={describedBy(`${id}-hostname`, errors.hostname)}
              />
            </Field>
          </Subsection>
        ) : (
          web && (
            <Subsection title="Internal web interface">
              <Notice tone="info" title={`No public route: ${web.service} listens on port ${web.port}`}>
                This tool is not published automatically because it does not protect itself well enough for the internet. Reach it over a private network, or add authenticated access before you
                add a route.
              </Notice>
            </Subsection>
          )
        )}
        <Subsection title="Settings" description={template.variables.length === 0 ? "This template has no settings." : "Values are sent to the server to prepare the draft. You can still edit every variable on the next step."}>
          {template.variables.length > 0 && (
            <div className="grid gap-4 sm:grid-cols-2">
              {template.variables.map((variable) => (
                <VariableField
                  key={variable.key}
                  id={`${id}-${variable.key}`}
                  variable={variable}
                  value={values[variable.key] ?? ""}
                  error={errors[variable.key]}
                  onChange={(value) => setValues((current) => ({ ...current, [variable.key]: value }))}
                />
              ))}
            </div>
          )}
        </Subsection>
        {capabilities.length > 0 && (
          <Subsection title="Host access">
            <Notice tone="warn" title="This template gets access to the worker host">
              <ul className="mt-1 list-disc space-y-0.5 pl-4">
                {capabilities.map((capability) => (
                  <li key={capability}>{capabilityLabel(capability)}</li>
                ))}
              </ul>
              <p className="mt-1.5">A compromised or misconfigured container with this access can affect everything else on the worker. Use a dedicated worker where possible.</p>
            </Notice>
            <label className="flex items-start gap-2.5 text-[13px] text-fg">
              <Checkbox
                checked={acknowledged}
                onChange={(event) => setAcknowledged(event.target.checked)}
                aria-invalid={errors.acknowledge ? true : undefined}
                aria-describedby={errors.acknowledge ? `${id}-acknowledge-error` : undefined}
              />
              <span>I reviewed this host access and want to prepare the template anyway</span>
            </label>
            {errors.acknowledge && (
              <p id={`${id}-acknowledge-error`} className="text-xs text-bad">
                {errors.acknowledge}
              </p>
            )}
          </Subsection>
        )}
      </fieldset>
      <div className="space-y-3 p-4">
        {draft.isError && <DraftError error={draft.error} />}
        <p className="text-xs text-fg-muted">Next, choose a project, node and placement and review the full configuration. Nothing is created or deployed until you save.</p>
        <div className="flex justify-end">
          <Button type="submit" variant="primary" pending={draft.isPending} disabled={!online}>
            Review configuration
            <ArrowRight aria-hidden className="size-3.5" />
          </Button>
        </div>
      </div>
    </form>
  );
}

function DraftError({ error }: { error: unknown }) {
  if (isStatus(error, 409)) {
    return (
      <Notice tone="warn" title="This template changed">
        Its source published a new revision while you were reviewing it. The details have been reloaded; check them and continue again.
      </Notice>
    );
  }
  if (isStatus(error, 404)) {
    return (
      <Notice tone="warn" title="This template was removed">
        Its source no longer offers it. Go back and choose another template.
      </Notice>
    );
  }
  return (
    <Notice tone="error" title="Could not prepare the template">
      {errorMessage(error)}
    </Notice>
  );
}

const typeHints: Partial<Record<TemplateVariable["type"], string>> = {
  integer: "A whole number.",
  hostname: "A lowercase DNS hostname.",
  url: "An HTTPS URL.",
  email: "An email address.",
};

function variableHint(variable: TemplateVariable) {
  if (variable.generated) return "Leave empty and Cove generates a fresh 256-bit secret. To use your own, enter exactly 64 lowercase hexadecimal characters.";
  if (variable.type === "secret") return "Choose a new secret for this resource.";
  return typeHints[variable.type];
}

const inputTypes: Partial<Record<TemplateVariable["type"], string>> = { secret: "password", email: "email", url: "url" };

function VariableField({ id, variable, value, error, onChange }: { id: string; variable: TemplateVariable; value: string; error?: string; onChange: (value: string) => void }) {
  const hint = variableHint(variable);
  const secret = variable.type === "secret";
  const common = { id, "aria-invalid": error ? true : undefined, "aria-describedby": describedBy(id, error, hint) };
  const control =
    variable.type === "boolean" ? (
      <Select {...common} className={inputClass} value={value} onChange={(event) => onChange(event.target.value)}>
        {!variable.required && <option value="">Not set</option>}
        <option value="true">true</option>
        <option value="false">false</option>
      </Select>
    ) : (
      <input
        {...common}
        className={`${inputClass} ${variable.type === "string" ? "" : "font-mono"}`}
        type={inputTypes[variable.type] ?? "text"}
        inputMode={variable.type === "integer" ? "numeric" : undefined}
        value={value}
        placeholder={variable.generated ? "Generated when you continue" : undefined}
        onChange={(event) => onChange(event.target.value)}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        data-1p-ignore={secret || undefined}
        data-lpignore={secret ? "true" : undefined}
      />
    );
  return (
    <Field id={id} label={variable.label} optional={!variable.required || variable.generated} hint={hint} error={error}>
      {control}
      <p className="font-mono text-[11px] text-fg-subtle">{variable.key}</p>
    </Field>
  );
}

function ExternalLinkValue({ href }: { href: string }) {
  return (
    <a href={href} target="_blank" rel="noreferrer" className={`${linkClass} inline-flex max-w-full items-center gap-1 break-all`}>
      <span className="min-w-0 break-all">{href}</span>
      <ExternalLink aria-hidden className="size-3 shrink-0" />
    </a>
  );
}

function TemplateDetails({ entry }: { entry: TemplateEntry }) {
  const { template, validation } = entry;
  const warnings = entry.warnings ?? [];
  const storage = template.storage ?? [];
  const fileSecrets = template.file_secrets ?? [];
  const images = Object.entries(template.images).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className="min-w-0 divide-y divide-line border-t border-line lg:border-t-0">
      {warnings.length > 0 && (
        <Subsection title="Warnings">
          <Notice tone="warn">
            <ul className="list-disc space-y-0.5 pl-4">
              {warnings.map((warning) => (
                <li key={warning}>{warning}</li>
              ))}
            </ul>
          </Notice>
        </Subsection>
      )}
      <Subsection title="Validation">
        <Evidence validation={validation} origin={entry.origin} />
      </Subsection>
      <Subsection title="About">
        <DefinitionList
          compact
          items={[
            { term: "Category", value: template.category },
            { term: "Origin", value: <MetaList><span key="o">{originLabel(entry)}</span><span key="r" className="font-mono text-xs" title={entry.revision}>{shortRevision(entry.revision)}</span></MetaList> },
            ...(entry.repository_name
              ? [
                  {
                    term: "Provided by",
                    value: (
                      <span className="block min-w-0 space-y-0.5">
                        <span className="block break-words">{entry.repository_name}</span>
                        {entry.repository_url && <ExternalLinkValue href={entry.repository_url} />}
                      </span>
                    ),
                  },
                ]
              : []),
            { term: "Upstream", value: <ExternalLinkValue href={template.source} /> },
            { term: "Licence", value: <ExternalLinkValue href={template.license} /> },
            { term: "Docs", value: <ExternalLinkValue href={template.docs} /> },
            {
              term: "Starting size",
              value: `${template.minimum.cpus} CPU · ${formatBytes(template.minimum.memory_mb * 1024 * 1024)} memory · ${formatBytes(template.minimum.disk_mb * 1024 * 1024)} disk`,
            },
            { term: "Storage", value: storage.length > 0 ? <span className="font-mono text-xs">{storage.join(", ")}</span> : "No named volumes" },
            ...(fileSecrets.length > 0
              ? [{ term: "File secrets", value: <span className="font-mono text-xs">{fileSecrets.map((file) => `${file.name} ← ${file.variable}`).join(", ")}</span> }]
              : []),
          ]}
        />
        {template.notes && <p className="text-xs leading-relaxed whitespace-pre-line text-fg-muted">{template.notes}</p>}
        {template.backup && (
          <p className="text-xs leading-relaxed text-fg-muted">
            <span className="font-medium text-fg">Backups: </span>
            {template.backup}
          </p>
        )}
      </Subsection>
      <Subsection title="Pinned images">
        <ul className="space-y-1 text-xs">
          {images.map(([service, image]) => (
            <li key={service} className="min-w-0 break-all">
              <span className="text-fg-muted">{service}</span> <span className="font-mono text-fg">{image}</span>
            </li>
          ))}
        </ul>
      </Subsection>
      <Subsection>
        <details className="group min-w-0">
          <summary className="flex cursor-pointer items-center gap-1.5 rounded text-[13px] font-semibold text-fg">
            <FileCode2 aria-hidden className="size-3.5 text-fg-muted" />
            Review Compose file
            <ChevronRight aria-hidden className="size-3.5 text-fg-muted transition-transform group-open:rotate-90" />
          </summary>
          <pre className="mt-3 max-h-96 overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">{template.compose}</pre>
        </details>
      </Subsection>
    </div>
  );
}

function Evidence({ validation, origin }: { validation: TemplateEvidence; origin: TemplateEntry["origin"] }) {
  const services = Object.entries(validation.services ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className="space-y-2 text-xs">
      <ul className="space-y-1">
        <li>
          <StatusLabel tone={validation.schema_validated ? "ok" : "warn"} label={validation.schema_validated ? "Compose schema validated" : "Compose schema not validated"} />
        </li>
        <li>
          <StatusLabel tone={validation.compose_config ? "ok" : "neutral"} label={validation.compose_config ? "docker compose config passed" : "docker compose config not run"} />
        </li>
        <li>
          <StatusLabel
            tone={validation.runtime === "started_healthy" ? "ok" : "neutral"}
            label={validation.runtime === "started_healthy" ? "Started healthy in Cove's local check" : "Not started in a runtime check"}
          />
        </li>
      </ul>
      {validation.message && <p className="text-fg-muted">{validation.message}</p>}
      {services.length > 0 && (
        <p className="text-fg-muted">
          {services.map(([service, status]) => (
            <span key={service} className="mr-3 inline-block">
              <span className="font-mono">{service}</span> {status}
            </span>
          ))}
        </p>
      )}
      {validation.checked_at && <p className="text-fg-muted">Checked {formatAbsolute(validation.checked_at)}</p>}
      <p className="text-fg-muted">
        {validation.runtime === "started_healthy"
          ? "Recorded on Cove's own test Docker host. It does not guarantee the template runs on your worker; run preflight after creating the resource."
          : origin === "builtin"
            ? "There is no runtime check for this revision. Run preflight on your worker before deploying."
            : "Custom sources are validated against the Compose schema only. Run preflight on your worker before deploying."}
      </p>
    </div>
  );
}

function DeleteTeamTemplate({ templateId, name, onDeleted }: { templateId: string; name: string; onDeleted: () => void }) {
  const team = useCurrentTeam();
  const remove = useDeleteTeamTemplate(team?.id ?? "");
  const [open, setOpen] = useState(false);
  const online = useOnline();
  if (!team) return null;
  return (
    <>
      <Button variant="danger" onClick={() => setOpen(true)} disabled={!online}>
        <Trash2 aria-hidden className="size-3.5" />
        Delete template
      </Button>
      <ConfirmDialog
        open={open}
        title={`Delete ${name}?`}
        confirmLabel="Delete template"
        danger
        pending={remove.isPending}
        onCancel={() => {
          remove.reset();
          setOpen(false);
        }}
        onConfirm={() =>
          remove.mutate(templateId, {
            onSuccess: () => {
              setOpen(false);
              onDeleted();
            },
          })
        }
      >
        <p>The template leaves this team's catalogue. Resources already created from it keep their configuration and are not changed.</p>
        {remove.isError && (
          <div className="mt-3">
            <Notice tone="error" title="Could not delete the template">
              {errorMessage(remove.error)}
            </Notice>
          </div>
        )}
      </ConfirmDialog>
    </>
  );
}
