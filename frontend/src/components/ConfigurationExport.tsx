import { useMutation } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { useId, useState } from "react";
import { errorMessage } from "../api/client";
import type { Environment, Project } from "../api/projects";
import { exportProjectConfiguration } from "../api/project-configuration";
import { exportFilename } from "../lib/project-configuration";
import { formatBytes } from "../lib/format";
import { Checkbox } from "./Checkbox";
import { Dialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { Button, Field, Notice, Section } from "./ui";

const offline = "Unavailable while offline.";

// The archive goes straight to the browser's download and is released; it is never kept in state, a cache or storage.
function saveFile(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.rel = "noopener";
  document.body.append(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function ConfigurationExport({ project, environments, online }: { project: Project; environments: readonly Environment[]; online: boolean }) {
  const id = useId();
  const exportable = environments.filter((environment) => environment.permissions.includes("edit"));
  const [scope, setScope] = useState("");
  const [open, setOpen] = useState(false);
  const [saved, setSaved] = useState<{ filename: string; size: number } | null>(null);
  const environment = exportable.find((item) => item.id === scope);

  return (
    <Section title="Export with secrets" description="Download a ZIP with cove.yaml, each resource's Compose files and its .env. Exports are recorded in the audit log and do not start or stop anything.">
      <div className="space-y-4">
        <Field id={`${id}-scope`} label="What to export" hint="Choose the whole project or one environment.">
          <Select id={`${id}-scope`} value={scope} onChange={(event) => setScope(event.target.value)} aria-describedby={`${id}-scope-hint`}>
            <option value="">Whole project</option>
            {exportable.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name} environment
              </option>
            ))}
          </Select>
        </Field>
        {saved && (
          <Notice tone="success" title="Export saved">
            <span className="font-mono text-xs break-all">{saved.filename}</span> ({formatBytes(saved.size)}) was handed to your browser's downloads. Store it somewhere private.
          </Notice>
        )}
        <div className="flex justify-end">
          <Button
            onClick={() => {
              setSaved(null);
              setOpen(true);
            }}
            disabled={!online}
            title={online ? undefined : offline}
          >
            <Download aria-hidden className="size-3.5" />
            Export ZIP
          </Button>
        </div>
      </div>
      <Dialog open={open} title={`Export ${environment ? environment.name : project.name}?`} onClose={() => setOpen(false)}>
        <ExportConfirmation
            projectId={project.id}
            filename={(server) => exportFilename(server, project.name, environment?.name)}
            environmentId={environment?.id ?? null}
            onCancel={() => setOpen(false)}
            onSaved={(result) => {
              setOpen(false);
              setSaved(result);
            }}
          />
      </Dialog>
    </Section>
  );
}

interface ConfirmationProps {
  projectId: string;
  environmentId: string | null;
  filename: (server: string | null) => string;
  onCancel: () => void;
  onSaved: (result: { filename: string; size: number }) => void;
}

// Dialog renders this only while open, so the confirmation and any error reset each time it closes.
function ExportConfirmation({ projectId, environmentId, filename, onCancel, onSaved }: ConfirmationProps) {
  const id = useId();
  const [acknowledged, setAcknowledged] = useState(false);
  const exported = useMutation({
    gcTime: 0,
    mutationFn: async () => {
      const { blob, filename: server } = await exportProjectConfiguration(projectId, environmentId);
      const name = filename(server);
      saveFile(blob, name);
      return { filename: name, size: blob.size };
    },
    onSuccess: onSaved,
    networkMode: "always",
  });
  return (
    <div className="space-y-4">
      <Notice tone="warn" title="The archive contains secrets">
        It holds decrypted variable values and the contents of private files such as env files. Anyone with the file can read them.
      </Notice>
      <ul className="list-disc space-y-1 pl-5">
        <li>Managed provider credentials and deploy private keys are not included.</li>
        <li>Build contexts, includes and env files from Git stay in the repository. export.json names the pinned revision to check out.</li>
        <li>Shared and external variable references are resolved. If one cannot be resolved, the export stops.</li>
      </ul>
      <label htmlFor={`${id}-ack`} className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox id={`${id}-ack`} checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} disabled={exported.isPending} />
        <span>I understand that this file contains decrypted secrets.</span>
      </label>
      {exported.isError && (
        <Notice tone="error" title="Could not export">
          {errorMessage(exported.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onCancel} disabled={exported.isPending}>
          Cancel
        </Button>
        <Button variant="primary" onClick={() => exported.mutate()} disabled={!acknowledged} pending={exported.isPending}>
          Export
        </Button>
      </div>
    </div>
  );
}
