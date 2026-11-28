import type { UseQueryResult } from "@tanstack/react-query";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import type { useCreateManagedRegistry } from "../api/managed-registries";
import type { Project } from "../api/projects";
import type { Node } from "../api/types";
import { hostnameError, maxRegistryNameBytes, nameError, nodeProblem, retentionInput, validateRetention, type RetentionDraft } from "../lib/managed-registries";
import { can } from "../lib/permissions";
import { describedBy, inputClass } from "../lib/styles";
import { RetentionFields } from "./ManagedRegistryRetention";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, Field, Notice, QueryError, Subsection } from "./ui";

export interface Created {
  id: string;
  operation_id: string;
}

interface Draft extends RetentionDraft {
  name: string;
  project: string;
  node: string;
  hostname: string;
}

type Errors = Partial<Record<keyof Draft, string>>;

const emptyDraft: Draft = { name: "", project: "", node: "", hostname: "", keep: "10", age: "30" };

function validate(draft: Draft): Errors {
  const errors: Errors = { ...validateRetention(draft) };
  const name = nameError(draft.name);
  const hostname = hostnameError(draft.hostname);
  if (name) errors.name = name;
  if (hostname) errors.hostname = hostname;
  if (!draft.project) errors.project = "Choose a project.";
  if (!draft.node) errors.node = "Choose a node.";
  return errors;
}

interface Props {
  create: ReturnType<typeof useCreateManagedRegistry>;
  nodes: UseQueryResult<Node[]>;
  projects: UseQueryResult<Project[]>;
  blocked?: string;
  onCreated: (created: Created | null) => void;
}

export function ManagedRegistryCreate({ create, nodes, projects, blocked, onCreated }: Props) {
  const id = useId();
  const formRef = useRef<HTMLFormElement>(null);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [showErrors, setShowErrors] = useState(false);
  const errors = showErrors ? validate(draft) : {};
  // A failed or stale node read leaves capabilities unknown, and unknown capabilities never allow a create.
  const nodesKnown = nodes.isSuccess;
  const blocker = blocked ?? (nodesKnown ? undefined : "Node capabilities have not loaded.");
  const dirty = JSON.stringify(draft) !== JSON.stringify(emptyDraft);

  function edit(patch: Partial<Draft>) {
    setDraft((current) => ({ ...current, ...patch }));
    create.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (Object.keys(validate(draft)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    onCreated(null);
    create.mutate({ name: draft.name.trim(), node_id: draft.node, project_id: draft.project, hostname: draft.hostname.trim(), retention: retentionInput(draft) }, { onSuccess: (reply) => onCreated({ id: reply.id, operation_id: reply.operation_id }) });
  }

  return (
    <form ref={formRef} onSubmit={submit} aria-label="Create a managed registry" noValidate>
      <UnsavedChangesGuard dirty={dirty && !create.isSuccess} />
      <Subsection title="Create a managed registry" description="One live managed registry per team. Creating it queues an ordinary deploy in the project you choose.">
        <fieldset disabled={create.isPending} className="min-w-0 space-y-4">
          <legend className="sr-only">Managed registry settings</legend>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id={`${id}-name`} label="Name" error={errors.name}>
              <input id={`${id}-name`} className={inputClass} value={draft.name} maxLength={maxRegistryNameBytes} autoComplete="off" aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} onChange={(event) => edit({ name: event.target.value })} />
            </Field>
            <ProjectField id={`${id}-project`} projects={projects} value={draft.project} error={errors.project} onChange={(project) => edit({ project })} />
          </div>
          <NodeField id={`${id}-node`} nodes={nodes} value={draft.node} error={errors.node} onChange={(node) => edit({ node })} />
          <HostnameField id={`${id}-hostname`} value={draft.hostname} error={errors.hostname} onChange={(hostname) => edit({ hostname })} />
          <RetentionFields id={`${id}-retention`} draft={draft} errors={errors} onChange={edit} />
        </fieldset>
        <CreateFacts />
        {create.isError && (
          <Notice tone="error" title="The managed registry was not created">
            <p className="break-words">{errorMessage(create.error)}</p>
            <p className="mt-1">Your entries are kept.</p>
          </Notice>
        )}
        <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2 border-t border-line pt-4">
          {blocker && <p className="mr-auto text-xs text-fg-muted">{blocker} Creating is paused.</p>}
          <Button type="submit" variant="primary" pending={create.isPending} disabled={Boolean(blocker)}>
            Create managed registry
          </Button>
        </div>
      </Subsection>
    </form>
  );
}

function ProjectField({ id, projects, value, error, onChange }: { id: string; projects: UseQueryResult<Project[]>; value: string; error?: string; onChange: (value: string) => void }) {
  const editable = projects.data?.filter((project) => can(project.permissions, "edit")) ?? [];
  return (
    <div className="min-w-0 space-y-2">
      <Field id={id} label="Project" error={error} hint="The registry resource and its deploys belong to this project.">
        <Select id={id} value={value} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, error, true)} onChange={(event) => onChange(event.target.value)}>
          <option value="">{projects.isPending ? "Loading projects…" : editable.length === 0 ? "No projects available" : "Choose a project"}</option>
          {editable.map((project) => (
            <option key={project.id} value={project.id}>
              {project.name}
            </option>
          ))}
        </Select>
      </Field>
      {projects.isError && <QueryError what="projects" error={projects.error} onRetry={() => void projects.refetch()} retrying={projects.isFetching} />}
    </div>
  );
}

function nodeLabel(node: Node, known: boolean) {
  if (!known) return `${node.name} (capabilities unknown)`;
  const problem = nodeProblem(node);
  return problem ? `${node.name} (${problem})` : node.name;
}

function NodeField({ id, nodes, value, error, onChange }: { id: string; nodes: UseQueryResult<Node[]>; value: string; error?: string; onChange: (value: string) => void }) {
  const known = nodes.isSuccess;
  const list = nodes.data ?? [];
  const anySupported = known && list.some((node) => nodeProblem(node) === null);
  const hint = known && !anySupported ? "No node of this team is ready with managed registry and secret file support. Update the worker on a node, then reload." : "A ready node of this team whose worker supports managed registries and secret files. Registry data is stored on this node.";
  return (
    <div className="min-w-0 space-y-2">
      <Field id={id} label="Node" error={error} hint={hint}>
        <Select id={id} value={value} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, error, true)} onChange={(event) => onChange(event.target.value)}>
          <option value="">{nodes.isPending ? "Loading nodes…" : "Choose a node"}</option>
          {list.map((node) => (
            <option key={node.id} value={node.id} disabled={!known || nodeProblem(node) !== null}>
              {nodeLabel(node, known)}
            </option>
          ))}
        </Select>
      </Field>
      {nodes.isError && <QueryError what="node capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />}
    </div>
  );
}

function HostnameField({ id, value, error, onChange }: { id: string; value: string; error?: string; onChange: (value: string) => void }) {
  return (
    <Field
      id={id}
      label="Hostname"
      error={error}
      hint="One exact lowercase hostname, such as registry.example.com. Cove requests a Let's Encrypt certificate for it, so public DNS must already point it at the node and ports 80 and 443 must reach the node. Cove does not configure DNS or firewalls."
    >
      <input id={id} className={`${inputClass} font-mono`} value={value} autoComplete="off" spellCheck={false} autoCapitalize="none" aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, error, true)} onChange={(event) => onChange(event.target.value)} />
    </Field>
  );
}

function CreateFacts() {
  return (
    <div className="space-y-2">
      <Notice tone="warn" title="No verified hard storage cap">
        Registry data lives in an ordinary Docker volume on the node, which has no verified filesystem quota. Retention settings and garbage collection remove eligible images; they are not storage quotas.
      </Notice>
      <ul className="list-disc space-y-1 pl-5 text-xs text-fg-muted">
        <li>The registry service is shared by the whole team and counts against the team quota as 1 CPU and 512 MiB of memory.</li>
        <li>The registry image is fixed by this Cove release and cannot be changed here.</li>
        <li>Cove generates the registry credentials and keeps them internal. Builds and deploys use them through a team registry connection that appears once authenticated HTTPS works.</li>
      </ul>
    </div>
  );
}
