import { Checkbox } from "./Checkbox";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Copy } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { keys } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { Button, Field, Notice, Section } from "./ui";

interface CloneProps {
  kind: "project" | "environment" | "resource";
  id: string;
  name: string;
  projectId?: string;
}

export function Clone(props: CloneProps) {
  return (
    <Section width="form" title={`Clone ${props.kind}`} description="Create an undeployed copy in this team, using the same nodes.">
      <CloneForm {...props} />
    </Section>
  );
}

export function CloneForm({ kind, id, name, projectId, onCancel }: CloneProps & { onCancel?: () => void }) {
  const fieldId = useId();
  const online = useOnline();
  const client = useQueryClient();
  const navigate = useNavigate();
  const [newName, setNewName] = useState(`${name.slice(0, 115)} copy`);
  const [includeSecrets, setIncludeSecrets] = useState(false);
  const endpoint = kind === "resource" ? "workloads" : `${kind}s`;
  const clone = useMutation({
    mutationFn: () => request<{ id: string }>("POST", `/api/${endpoint}/${encodeURIComponent(id)}/clone`, { name: newName.trim(), include_secrets: includeSecrets }),
    onSuccess: async ({ id: next }) => {
      await Promise.all([client.invalidateQueries({ queryKey: keys.projects }), client.invalidateQueries({ queryKey: keys.workloads })]);
      if (kind === "project") await navigate({ to: "/projects/$projectId", params: { projectId: next } });
      else if (kind === "environment" && projectId) await navigate({ to: "/projects/$projectId/environments/$environmentId", params: { projectId, environmentId: next } });
      else await navigate({ to: "/resources/$resourceId", params: { resourceId: next } });
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (online && !clone.isPending && newName.trim()) clone.mutate();
  }
  return (
    <form onSubmit={submit} className="space-y-3">
      <Field id={fieldId} label="Copy name">
        <input id={fieldId} className={inputClass} value={newName} onChange={(event) => setNewName(event.target.value)} required maxLength={120} disabled={clone.isPending} />
      </Field>
      <label className="flex items-center gap-2 text-[13px]">
        <Checkbox checked={includeSecrets} onChange={(event) => setIncludeSecrets(event.target.checked)} disabled={clone.isPending} />
        Copy environment variables and Git credentials
      </label>
      <p className="text-xs text-fg-muted">This option also copies scoped shared variables and resource variable bindings. References between copied resources point to their copies; references outside the copied group stay linked and require edit permission.</p>
      <p className="text-xs text-fg-muted">{kind === "project" ? "Includes all environments and resources. " : kind === "environment" ? "Includes all resources. " : ""}Domains, backup policies, webhooks, history and volume data are not copied. Compose content is copied as written, including any embedded secrets. Review external databases, host paths and published ports before deploying.</p>
      {clone.isError && <Notice tone="error" title="Could not clone">{errorMessage(clone.error)}</Notice>}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={!online || !newName.trim()} pending={clone.isPending}><Copy aria-hidden className="size-3.5" />Clone {kind}</Button>
        {onCancel && <Button variant="ghost" onClick={onCancel} disabled={clone.isPending}>Cancel</Button>}
      </div>
    </form>
  );
}

