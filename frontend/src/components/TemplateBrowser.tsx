import { Select } from "./Select";
import { useId, useState, type FormEvent } from "react";
import { ArrowLeft, ArrowRight, Database, Activity, ChartNoAxesCombined, Search } from "lucide-react";
import { templates, templateDraft, templateValues, type ResourceDraft, type ResourceTemplate } from "../lib/templates";
import { inputClass, linkClass } from "../lib/styles";
import { Button, Field, Notice, Section } from "./ui";

export function TemplateBrowser({ onUse, onCustom }: { onUse: (draft: ResourceDraft) => void; onCustom: () => void }) {
  const id = useId();
  const [search, setSearch] = useState("");
  const [category, setCategory] = useState("All");
  const [selected, setSelected] = useState<ResourceTemplate | null>(null);
  if (selected) return <TemplateSetup key={selected.id} template={selected} onBack={() => setSelected(null)} onUse={onUse} />;
  const matches = templates.filter((template) => (category === "All" || category === template.category) && `${template.name} ${template.description} ${template.category}`.toLowerCase().includes(search.trim().toLowerCase()));
  return (
    <Section title="Resource templates" description="Start with a prepared Compose configuration, or bring your own.">
      <div className="mb-5 flex flex-wrap items-end gap-3">
        <div className="min-w-0 flex-1 basis-64">
          <Field id={`${id}-search`} label="Search templates"><input id={`${id}-search`} type="search" className={inputClass} value={search} onChange={(event) => setSearch(event.target.value)} /></Field>
        </div>
        <Field id={`${id}-category`} label="Category"><Select id={`${id}-category`} className={inputClass} value={category} onChange={(event) => setCategory(event.target.value)}>{["All", "Databases", "Monitoring", "Analytics"].map((item) => <option key={item}>{item}</option>)}</Select></Field>
        <Button onClick={onCustom}>Custom Compose or Git</Button>
      </div>
      <p role="status" className="mb-2 text-xs text-fg-muted">{matches.length} templates</p>
      <ul className="divide-y divide-line border-y border-line">
        {matches.map((template) => {
          const Icon = template.category === "Databases" ? Database : template.category === "Monitoring" ? Activity : ChartNoAxesCombined;
          return <li key={template.id}>
            <button type="button" className="flex w-full items-center gap-3 rounded px-2 py-4 text-left hover:bg-panel focus-visible:outline-2 focus-visible:outline-info" onClick={() => setSelected(template)}>
              <Icon aria-hidden className="size-5 shrink-0 text-fg-muted" />
              <span className="min-w-0 flex-1"><span className="block text-sm font-medium">{template.name}</span><span className="mt-1 block text-xs text-fg-muted">{template.description}</span></span>
              <span className="hidden text-xs text-fg-muted sm:block">{template.category}</span><ArrowRight aria-hidden className="size-4 shrink-0" />
            </button>
          </li>;
        })}
      </ul>
      {matches.length === 0 && <div className="space-y-3 py-6"><p className="text-sm text-fg-muted">No templates match. Try another search or use a custom Compose file.</p><Button onClick={() => { setSearch(""); setCategory("All"); }}><Search aria-hidden className="size-3.5" />Clear filters</Button></div>}
    </Section>
  );
}

function TemplateSetup({ template, onBack, onUse }: { template: ResourceTemplate; onBack: () => void; onUse: (draft: ResourceDraft) => void }) {
  const id = useId();
  const [values, setValues] = useState(() => templateValues(template));
  const [domain, setDomain] = useState("");
  const [error, setError] = useState<string | null>(null);
  function submit(event: FormEvent) {
    event.preventDefault();
    try { onUse(templateDraft(template, values, domain)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Could not prepare template."); }
  }
  return <Section title={template.name} description={template.description}>
    <div className="max-w-2xl space-y-4">
      <Button variant="ghost" onClick={onBack}><ArrowLeft aria-hidden className="size-3.5" />Back to templates</Button>
      <p className="text-[13px] text-fg-muted">{template.notes} <a href={template.docs} target="_blank" rel="noreferrer" className={linkClass}>Installation guide</a></p>
      <form onSubmit={submit} className="space-y-4">
        {template.web && <Field id={`${id}-domain`} label="Domain" hint="Cove will create an HTTPS route. Point this hostname to the selected worker before deploying."><input id={`${id}-domain`} className={inputClass} value={domain} onChange={(event) => setDomain(event.target.value)} required autoCapitalize="none" spellCheck={false} /></Field>}
        {template.variables.map((variable) => <Field key={variable.key} id={`${id}-${variable.key}`} label={variable.label} hint={variable.secret ? "A fresh 256-bit secret is generated for this resource. You can review it in the next step." : undefined}>
          <input id={`${id}-${variable.key}`} className={inputClass} type={variable.secret ? "password" : "text"} value={values[variable.key]} onChange={(event) => setValues({ ...values, [variable.key]: event.target.value })} required autoComplete="off" spellCheck={false} />
        </Field>)}
        {error && <Notice tone="error" title="Check template settings">{error}</Notice>}
        <p className="text-xs text-fg-muted">Next, choose a node and environment and review the configuration. Nothing is deployed automatically. Persistent data uses resource-specific Docker volumes.</p>
        <Button type="submit" variant="primary">Review configuration<ArrowRight aria-hidden className="size-3.5" /></Button>
      </form>
    </div>
  </Section>;
}
