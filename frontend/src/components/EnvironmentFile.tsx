import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Download, FileDown, FileUp } from "lucide-react";
import { useId, useState, type ChangeEvent, type FormEvent, type KeyboardEvent } from "react";
import { errorMessage, request } from "../api/client";
import { keys } from "../api/queries";
import { builtinVariablesQuery, referenceFormats, secretProviderLabels, secretProvidersQuery, secretReference, validateReference } from "../api/secret-providers";
import type { Workload } from "../api/types";
import { can } from "../lib/permissions";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { byteLength, validateEnvironmentKey } from "../lib/workload-form";
import { Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const importLimit = 1024 * 1024;

function downloadName(name: string) {
  const base = name.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  return `${base || "variables"}.env`;
}

function variableCount(dotenv: string) {
  return dotenv.split("\n").filter((line) => /^[A-Za-z_][A-Za-z0-9_]*=/.test(line)).length;
}

// The dialogs render these forms only while open, so dismissing them any way (including Escape)
// unmounts the form state and its gcTime 0 mutation. The mutations take no variables because
// variables stay in the mutation cache after it settles; mutationFn reads the payload from state.
function ImportForm({ workload, onClose, onImported }: { workload: Workload; onClose: () => void; onImported: (keys: string[]) => void }) {
  const id = useId();
  const client = useQueryClient();
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const submit = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ keys: string[] }>("POST", `/api/workloads/${encodeURIComponent(workload.id)}/variables/import`, { dotenv: text }),
    onSuccess: (result) => {
      setText("");
      onImported(result.keys);
      return client.invalidateQueries({ queryKey: keys.workload(workload.id) });
    },
    networkMode: "always",
  });

  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    if (file.size > importLimit) {
      setError("The file is larger than 1 MiB.");
      return;
    }
    try {
      setText(await file.text());
      setError("");
      submit.reset();
    } catch {
      setError("The file could not be read.");
    }
  }

  function send(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (text.trim() === "") {
      setError("Paste a .env file or choose one.");
      return;
    }
    if (byteLength(text) > importLimit) {
      setError("The file is larger than 1 MiB.");
      return;
    }
    setError("");
    submit.mutate();
  }

  return (
    <form onSubmit={send} noValidate className="space-y-4">
      <p>Variables in the file are saved to this resource. Existing names are replaced and other saved variables are kept. Nothing is deployed.</p>
      <fieldset disabled={submit.isPending} className="min-w-0 space-y-3">
        <Field id={`${id}-text`} label=".env contents" error={error} hint="NAME=value per line. Comments, export, quoted and multi-line values are accepted. Dollar signs are kept literally. If a name repeats, the last value wins.">
          <textarea
            id={`${id}-text`}
            rows={8}
            value={text}
            onChange={(event) => { setText(event.target.value); submit.reset(); }}
            className={`${inputClass} h-auto py-2 font-mono`}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            data-1p-ignore
            data-lpignore="true"
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(`${id}-text`, error, true)}
          />
        </Field>
        <div>
          <label htmlFor={`${id}-file`} className="mb-1.5 block text-[13px] font-medium text-fg">Or choose a file</label>
          <input id={`${id}-file`} type="file" accept=".env,text/plain" onChange={(event) => void choose(event)} className="block w-full max-w-full text-xs text-fg-muted file:mr-3 file:h-8 file:rounded-md file:border file:border-line-strong file:bg-raised file:px-3 file:text-[13px] file:font-medium file:text-fg" />
        </div>
      </fieldset>
      {submit.isError && <Notice tone="error" title="Could not import variables">{errorMessage(submit.error)}</Notice>}
      <div className="flex justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onClose} disabled={submit.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={submit.isPending}>Import variables</Button>
      </div>
    </form>
  );
}

function ExportContent({ workload, onClose }: { workload: Workload; onClose: () => void }) {
  const exported = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ dotenv: string }>("POST", `/api/workloads/${encodeURIComponent(workload.id)}/variables/export`),
    networkMode: "always",
  });

  function close() {
    exported.reset();
    onClose();
  }

  function download(dotenv: string) {
    const url = URL.createObjectURL(new Blob([dotenv], { type: "text/plain" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = downloadName(workload.name);
    link.click();
    URL.revokeObjectURL(url);
  }

  const dotenv = exported.data?.dotenv;
  return (
    <>
      {dotenv === undefined ? (
        <div className="space-y-4">
          <Notice tone="warn" title="The file contains secret values in plain text">
            It includes every saved value, such as passwords and tokens, and secret provider references as written. Store it as carefully as the credentials themselves.
          </Notice>
          <p>The export is recorded in the audit log. Built-in variables and values read from secret providers are not included.</p>
          {exported.isError && <Notice tone="error" title="Could not export variables">{errorMessage(exported.error)}</Notice>}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={close} disabled={exported.isPending}>Cancel</Button>
            <Button variant="primary" pending={exported.isPending} onClick={() => exported.mutate()}>Export variables</Button>
          </div>
        </div>
      ) : (
        <div className="space-y-4">
          <p role="status">
            {variableCount(dotenv) === 1 ? "1 variable is" : `${variableCount(dotenv)} variables are`} ready. Copy or download the file, then close this dialog to clear it from the page.
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <Button onClick={() => download(dotenv)}>
              <Download aria-hidden className="size-3.5" />
              Download {downloadName(workload.name)}
            </Button>
            <CopyButton text={dotenv} label="Copy to clipboard" />
          </div>
          <div className="flex justify-end border-t border-line pt-3">
            <Button variant="primary" onClick={close}>Done</Button>
          </div>
        </div>
      )}
    </>
  );
}

const fileActionCopy = {
  both: { title: "Import and export", description: "Move saved variables in and out as a .env file." },
  import: { title: "Import", description: "Add saved variables from a .env file." },
  export: { title: "Export", description: "Download saved variables as a .env file." },
};

export function EnvironmentFileActions({ workload, temporary, blocked }: { workload: Workload; temporary: boolean; blocked: boolean }) {
  const [dialog, setDialog] = useState<"import" | "export" | null>(null);
  const [imported, setImported] = useState<string[] | null>(null);
  const saved = workload.environment_keys.length;
  // Importing writes configuration and exporting reads plaintext secrets; a custom role can hold either one alone.
  const importable = can(workload.permissions, "edit");
  const exportable = can(workload.permissions, "read_secrets");
  if (!importable && !exportable) return null;
  const { title, description } = fileActionCopy[importable ? (exportable ? "both" : "import") : "export"];
  return (
    <Section width="form" title={title} description={description}>
      <div className="space-y-3">
        {temporary && <Notice>Preview resources cannot import or export saved variables. Use the resource the preview was created from.</Notice>}
        {imported && (
          <Notice tone="success" title={`Imported ${imported.length === 1 ? "1 variable" : `${imported.length} variables`}`}>
            <span className="font-mono break-all">{imported.join(", ")}</span>. Changes apply on the next deploy.
          </Notice>
        )}
        <div className="flex flex-wrap items-center gap-2">
          {importable && (
            <Button disabled={temporary || blocked} onClick={() => { setImported(null); setDialog("import"); }}>
              <FileUp aria-hidden className="size-3.5" />
              Import .env
            </Button>
          )}
          {exportable && (
            <Button disabled={temporary || saved === 0} onClick={() => setDialog("export")}>
              <FileDown aria-hidden className="size-3.5" />
              Export .env
            </Button>
          )}
          {exportable && !temporary && saved === 0 && <span className="text-xs text-fg-muted">No variables to export.</span>}
          {!exportable && <span className="text-xs text-fg-muted">Exporting needs permission to read secrets.</span>}
          {!importable && <span className="text-xs text-fg-muted">Importing needs edit access.</span>}
        </div>
      </div>
      <Dialog open={dialog === "import"} title="Import a .env file" onClose={() => setDialog(null)}>
        <ImportForm workload={workload} onClose={() => setDialog(null)} onImported={(names) => { setImported(names); setDialog(null); }} />
      </Dialog>
      <Dialog open={dialog === "export"} title="Export saved variables" onClose={() => setDialog(null)}>
        <ExportContent workload={workload} onClose={() => setDialog(null)} />
      </Dialog>
    </Section>
  );
}

export function BuiltinVariables({ workloadId }: { workloadId: string }) {
  const builtins = useQuery(builtinVariablesQuery(workloadId));
  const summary = builtins.isPending ? "Loading" : builtins.isError ? "Could not load built-in variables" : builtins.data.available ? `${builtins.data.variables.length} variables added at deploy` : "Not supported by this resource's node yet";
  return (
    <Section width="form" title="Built-in variables" collapsed={{ summary }} description="Added automatically at deploy for Compose interpolation, container environments and Dockerfile ARG build arguments. A saved variable with the same name, or a value set on a service's environment or build args in Compose, takes precedence." flush>
      {builtins.isPending ? (
        <div className="p-4"><LoadingRows label="Loading built-in variables" rows={3} /></div>
      ) : builtins.isError ? (
        <div className="p-4"><QueryError what="built-in variables" error={builtins.error} onRetry={() => void builtins.refetch()} retrying={builtins.isFetching} /></div>
      ) : (
        <>
          {!builtins.data.available && (
            <div className="border-b border-line p-3">
              <Notice tone="warn" title="Not added yet">Upgrade the worker on this resource's node and verify the node. Until then, deployments add none of these variables.</Notice>
            </div>
          )}
          <ul aria-label="Built-in variables" className="divide-y divide-line">
            {builtins.data.variables.map((variable) => (
              <li key={variable.name} className="grid gap-x-4 gap-y-0.5 px-4 py-2 sm:grid-cols-[12rem_minmax(0,1fr)]">
                <span className="font-mono text-xs break-all text-fg">{variable.name}</span>
                <span className="text-xs text-fg-muted">{variable.description}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </Section>
  );
}

export function SecretReferenceBuilder({ onAdd }: { onAdd: (key: string, value: string) => void }) {
  const id = useId();
  const providers = useQuery(secretProvidersQuery);
  const [name, setName] = useState("");
  const [connection, setConnection] = useState("");
  const [reference, setReference] = useState("");
  const [errors, setErrors] = useState<{ name?: string; connection?: string; reference?: string }>({});
  const provider = providers.data?.find((item) => item.id === connection);
  const format = provider ? referenceFormats[provider.kind] : undefined;
  const value = provider && reference.trim() && !validateReference(reference.trim()) ? secretReference(provider.id, reference.trim()) : "";

  function add() {
    const found = {
      name: name.trim() === "" ? "Enter a variable name." : (validateEnvironmentKey(name.trim()) ?? undefined),
      connection: provider ? undefined : "Choose a provider.",
      reference: validateReference(reference.trim()) ?? undefined,
    };
    setErrors(found);
    if (found.name || found.connection || found.reference || !provider) return;
    onAdd(name.trim(), secretReference(provider.id, reference.trim()));
    setName("");
    setReference("");
  }

  // These fields sit inside the variables form; Enter adds the reference instead of saving the form.
  function keyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Enter") {
      event.preventDefault();
      add();
    }
  }

  return (
    <div className="space-y-3">
      <div>
        <h3 className="text-[13px] font-medium text-fg">Reference a secret provider</h3>
        <p className="text-xs text-fg-muted">The value is read from the provider when a deployment starts, and failures stop the deployment. Cove never shows it.</p>
      </div>
      {providers.isPending ? (
        <LoadingRows label="Loading secret providers" rows={1} />
      ) : providers.isError ? (
        <QueryError what="secret providers" error={providers.error} onRetry={() => void providers.refetch()} retrying={providers.isFetching} />
      ) : providers.data.length === 0 ? (
        <p className="text-[13px] text-fg-muted">
          No secret providers are saved for this team. Add one on <Link to="/integrations" className={linkClass}>Integrations</Link>.
        </p>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={`${id}-name`} label="Variable name" error={errors.name}>
              <input id={`${id}-name`} className={`${inputClass} font-mono`} value={name} maxLength={128} autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder="DATABASE_PASSWORD" onKeyDown={keyDown} onChange={(event) => setName(event.target.value)} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} />
            </Field>
            <Field id={`${id}-connection`} label="Provider" error={errors.connection}>
              <Select id={`${id}-connection`} value={connection} onChange={(event) => setConnection(event.target.value)} aria-invalid={errors.connection ? true : undefined}>
                <option value="">Select provider</option>
                {providers.data.map((item) => <option key={item.id} value={item.id}>{item.name} · {secretProviderLabels[item.kind]}</option>)}
              </Select>
            </Field>
          </div>
          <Field id={`${id}-reference`} label="Reference" error={errors.reference} hint={format ? `${format.format}. ${format.hint}` : "Choose a provider to see its reference format."}>
            <input id={`${id}-reference`} className={`${inputClass} font-mono`} value={reference} maxLength={2048} autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder={format?.format} onKeyDown={keyDown} onChange={(event) => setReference(event.target.value)} aria-invalid={errors.reference ? true : undefined} aria-describedby={describedBy(`${id}-reference`, errors.reference, true)} />
          </Field>
          {value && (
            <p className="text-xs text-fg-muted">
              Value: <span className="font-mono break-all text-fg">{value}</span>
            </p>
          )}
          <div className="flex flex-wrap items-center gap-2">
            <Button onClick={add}>Add to variables</Button>
            {value && <CopyButton text={value} label="Copy value" variant="ghost" />}
          </div>
        </>
      )}
    </div>
  );
}
