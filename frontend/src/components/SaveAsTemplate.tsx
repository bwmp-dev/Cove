import { FileCode2, ChevronRight } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useSaveTemplate, useTemplatePreview, type TemplateMetadata, type TemplatePreview } from "../api/templates";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { httpsLinkError, suggestTemplateId, templateIdPattern } from "../lib/templates";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button, Field, Notice, Section } from "./ui";

type MetadataField = "id" | "name" | "description" | "category" | "source" | "license" | "docs" | "cpus" | "memory" | "disk";
type Values = Record<MetadataField, string>;
type Errors = Partial<Record<MetadataField, string>>;

function validate(values: Values): { metadata?: TemplateMetadata; errors: Errors } {
  const errors: Errors = {};
  const id = values.id.trim();
  if (!templateIdPattern.test(id)) errors.id = "Use 1 to 64 lowercase letters, digits and hyphens, starting with a letter or digit.";
  const name = values.name.trim();
  if (name === "" || name.length > 120) errors.name = "Enter a name of at most 120 characters.";
  const description = values.description.trim();
  if (description === "" || description.length > 1000) errors.description = "Enter a description of at most 1000 characters.";
  const category = values.category.trim();
  if (category === "" || category.length > 80) errors.category = "Enter a category of at most 80 characters.";
  for (const [key, what] of [["source", "source"], ["license", "licence"], ["docs", "documentation"]] as const) {
    const problem = httpsLinkError(values[key], what);
    if (problem) errors[key] = problem;
  }
  const cpus = Number(values.cpus);
  if (values.cpus.trim() === "" || !Number.isFinite(cpus) || cpus <= 0 || cpus > 256) errors.cpus = "Enter more than 0 and at most 256 CPUs.";
  const memory = Number(values.memory);
  if (!/^\d+$/.test(values.memory.trim()) || memory < 16 || memory > 1048576) errors.memory = "Enter whole MiB from 16 to 1048576.";
  const disk = Number(values.disk);
  if (!/^\d+$/.test(values.disk.trim())) errors.disk = "Enter whole MiB, 0 or more.";
  if (Object.keys(errors).length > 0) return { errors };
  return {
    errors,
    metadata: {
      schema_version: 1,
      id,
      name,
      description,
      category,
      source: values.source.trim(),
      license: values.license.trim(),
      docs: values.docs.trim(),
      minimum: { cpus, memory_mb: memory, disk_mb: disk },
    },
  };
}

export function SaveAsTemplate({ workloadId, workloadName }: { workloadId: string; workloadName: string }) {
  return (
    <Section
      width="form"
      title="Save as team template"
      description="Publish this resource's Compose configuration as a template in this team's catalogue. The resource itself is not changed or deployed."
      collapsed={{ summary: "Offer this configuration to your team as a reusable template." }}
    >
      <SaveAsTemplateForm workloadId={workloadId} workloadName={workloadName} />
    </Section>
  );
}

function SaveAsTemplateForm({ workloadId, workloadName }: { workloadId: string; workloadName: string }) {
  const id = useId();
  const online = useOnline();
  const exporter = useTemplatePreview(workloadId);
  const save = useSaveTemplate(workloadId);
  const [values, setValues] = useState<Values>(() => ({
    id: suggestTemplateId(workloadName),
    name: workloadName.slice(0, 120),
    description: "",
    category: "",
    source: "",
    license: "",
    docs: "",
    cpus: "",
    memory: "",
    disk: "",
  }));
  const [errors, setErrors] = useState<Errors>({});
  const [preview, setPreview] = useState<{ metadata: TemplateMetadata; result: TemplatePreview } | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [saved, setSaved] = useState<string | null>(null);
  const [stale, setStale] = useState(false);

  function change(field: MetadataField, value: string) {
    setValues((current) => ({ ...current, [field]: value }));
    // The preview describes the metadata it was made from, so any edit needs a new preview.
    setPreview(null);
    exporter.reset();
    save.reset();
    setSaved(null);
    setStale(false);
  }

  function requestPreview(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validate(values);
    setErrors(result.errors);
    setSaved(null);
    setStale(false);
    save.reset();
    if (!result.metadata) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    const metadata = result.metadata;
    exporter.mutate(metadata, { onSuccess: (result) => setPreview({ metadata, result }) });
  }

  function confirmSave() {
    if (!preview) return;
    save.mutate(
      { template: preview.metadata, revision: preview.result.configuration_revision },
      {
        onSuccess: () => {
          setConfirming(false);
          setSaved(preview.metadata.name);
          setPreview(null);
        },
        onError: (error) => {
          setConfirming(false);
          if (isStatus(error, 409)) {
            setPreview(null);
            setStale(true);
          }
        },
      },
    );
  }

  const text = (field: MetadataField, label: string, hint?: ReactNode, extra: { mono?: boolean; maxLength?: number; inputMode?: "url" | "decimal" | "numeric"; placeholder?: string } = {}) => (
    <Field id={`${id}-${field}`} label={label} hint={hint} error={errors[field]}>
      <input
        id={`${id}-${field}`}
        className={`${inputClass} ${extra.mono ? "font-mono" : ""}`}
        value={values[field]}
        onChange={(event) => change(field, event.target.value)}
        maxLength={extra.maxLength}
        inputMode={extra.inputMode}
        placeholder={extra.placeholder}
        autoComplete="off"
        autoCapitalize={extra.mono ? "none" : undefined}
        spellCheck={extra.mono ? false : undefined}
        aria-invalid={errors[field] ? true : undefined}
        aria-describedby={describedBy(`${id}-${field}`, errors[field], hint)}
      />
    </Field>
  );

  return (
    <div className="space-y-4">
      <form onSubmit={requestPreview} noValidate className="space-y-4">
        <fieldset disabled={exporter.isPending || save.isPending} className="min-w-0 space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            {text("name", "Template name", undefined, { maxLength: 120 })}
            {text("id", "Template ID", "Lowercase letters, digits and hyphens. Must be unique in this team.", { mono: true, maxLength: 64 })}
          </div>
          <Field id={`${id}-description`} label="Description" error={errors.description}>
            <textarea
              id={`${id}-description`}
              className={inputClass}
              rows={2}
              maxLength={1000}
              value={values.description}
              onChange={(event) => change("description", event.target.value)}
              aria-invalid={errors.description ? true : undefined}
              aria-describedby={describedBy(`${id}-description`, errors.description)}
            />
          </Field>
          {text("category", "Category", "Shown as a catalogue filter, for example Databases or Applications.", { maxLength: 80 })}
          <div className="space-y-4">
            {text("source", "Source link", "Where the software comes from. Give an honest upstream link; templates are not reviewed by Cove.", { mono: true, inputMode: "url", placeholder: "https://" })}
            {text("license", "Licence link", "The upstream licence that applies to the software.", { mono: true, inputMode: "url", placeholder: "https://" })}
            {text("docs", "Documentation link", undefined, { mono: true, inputMode: "url", placeholder: "https://" })}
          </div>
          <fieldset className="min-w-0">
            <legend className="text-[13px] font-medium text-fg">Starting size</legend>
            <p className="mt-0.5 text-xs text-fg-muted">The smallest worker capacity you recommend to people using this template. It is a hint, not a limit or an upstream requirement.</p>
            <div className="mt-3 grid gap-4 sm:grid-cols-3">
              {text("cpus", "CPUs", undefined, { inputMode: "decimal" })}
              {text("memory", "Memory (MiB)", undefined, { inputMode: "numeric" })}
              {text("disk", "Disk (MiB)", undefined, { inputMode: "numeric" })}
            </div>
          </fieldset>
        </fieldset>
        <Notice tone="info" title="What the template contains">
          Host ports and labels are removed. Every environment, command and configuration value becomes a required secret setting that people fill with fresh values, so nothing from this resource
          is copied. File secret mappings are kept. Domains are not copied; only the first route's service and port are kept as an internal hint. Host paths, external resources, hooks, private
          access and jobs are refused rather than dropped.
        </Notice>
        {exporter.isError && (
          <Notice tone="error" title="Could not preview the template">
            {errorMessage(exporter.error)}
          </Notice>
        )}
        {stale && (
          <Notice tone="warn" title="Preview again before saving">
            {save.error ? errorMessage(save.error) : "The resource or template changed."} Check the new preview before saving.
          </Notice>
        )}
        {save.isError && !stale && (
          <Notice tone="error" title="Could not save the template">
            {errorMessage(save.error)}
          </Notice>
        )}
        {saved && (
          <Notice tone="success" title="Template saved">
            {saved} is now in this team's catalogue on the New resource page.
          </Notice>
        )}
        <div className="flex justify-end">
          <Button type="submit" pending={exporter.isPending} disabled={!online}>
            {preview ? "Preview again" : "Preview template"}
          </Button>
        </div>
      </form>
      {preview && <PreviewDetails preview={preview.result} />}
      {preview && (
        <div className="flex flex-wrap items-center justify-end gap-2 border-t border-line pt-4">
          <p className="mr-auto text-xs text-fg-muted">Saving publishes this preview to your team. The resource stays as it is.</p>
          <Button variant="ghost" onClick={() => setPreview(null)}>
            Discard preview
          </Button>
          <Button variant="primary" onClick={() => setConfirming(true)} disabled={!online}>
            Save to team
          </Button>
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        title={`Save ${preview?.metadata.name ?? "template"} to your team?`}
        confirmLabel="Save template"
        pending={save.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={confirmSave}
      >
        Everyone in this team can create resources from it. It does not deploy anything and does not change this resource. You can delete it later from the template catalogue.
      </ConfirmDialog>
    </div>
  );
}

function PreviewDetails({ preview }: { preview: TemplatePreview }) {
  const { template } = preview;
  const warnings = (preview.warnings ?? []).filter(Boolean);
  const fileSecrets = template.file_secrets ?? [];
  return (
    <div className="space-y-4 border-t border-line pt-4">
      <h3 className="text-[13px] font-semibold text-fg">Preview</h3>
      {warnings.length > 0 && (
        <Notice tone="warn" title="Review before saving">
          <ul className="list-disc space-y-0.5 pl-4">
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </Notice>
      )}
      <div className="space-y-1.5">
        <p className="text-xs font-medium text-fg">
          {template.variables.length} required {template.variables.length === 1 ? "setting" : "settings"}
        </p>
        {template.variables.length > 0 && (
          <ul className="max-h-48 space-y-0.5 overflow-auto rounded-md border border-line px-3 py-2 text-xs">
            {template.variables.map((variable) => (
              <li key={variable.key} className="flex min-w-0 flex-wrap gap-x-2">
                <span className="font-mono text-fg">{variable.key}</span>
                <span className="min-w-0 break-words text-fg-muted">{variable.label}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      {fileSecrets.length > 0 && (
        <p className="text-xs text-fg-muted">
          File secrets kept: <span className="font-mono">{fileSecrets.map((file) => `${file.name} ← ${file.variable}`).join(", ")}</span>
        </p>
      )}
      {template.web && (
        <p className="text-xs text-fg-muted">
          Internal web port hint: <span className="font-mono">{template.web.service}:{template.web.port}</span>. No public route is created by the template.
        </p>
      )}
      <details className="group min-w-0" open>
        <summary className="flex cursor-pointer items-center gap-1.5 rounded text-xs font-medium text-fg">
          <FileCode2 aria-hidden className="size-3.5 text-fg-muted" />
          Transformed Compose file
          <ChevronRight aria-hidden className="size-3.5 text-fg-muted transition-transform group-open:rotate-90" />
        </summary>
        <pre className="mt-2 max-h-96 overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">{template.compose}</pre>
      </details>
    </div>
  );
}
