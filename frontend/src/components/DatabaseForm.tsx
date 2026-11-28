import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { databaseEnginesQuery, useCreateDatabase, type DatabaseEngine } from "../api/databases";
import { variantNames, versionOptions } from "../lib/databases";
import type { Project } from "../api/projects";
import type { DatabaseEngineId, Node } from "../api/types";
import { nodeStates } from "../lib/nodes";
import { buttonClass, choiceClass, describedBy, inputClass } from "../lib/styles";
import { ResourcePlacement } from "./ResourcePlacement";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError } from "./ui";
import { FormSection } from "./WorkloadForm";

interface DatabaseFormProps {
  nodes: Node[];
  projects: Project[];
  initialProjectId: string;
  initialEnvironmentId: string;
  cancel: ReactNode;
  onCreated: (id: string) => void;
}

function minimumText(engine: DatabaseEngine) {
  const parts: string[] = [];
  if (engine.minimum?.cpus) parts.push(`${engine.minimum.cpus} CPU`);
  if (engine.minimum?.memory_mb) parts.push(`${engine.minimum.memory_mb} MiB memory`);
  if (engine.minimum?.disk_mb) parts.push(`${engine.minimum.disk_mb} MiB disk`);
  return parts.length ? `Needs at least ${new Intl.ListFormat("en", { type: "conjunction" }).format(parts)}.` : "";
}

export function DatabaseForm({ nodes, projects, initialProjectId, initialEnvironmentId, cancel, onCreated }: DatabaseFormProps) {
  const id = useId();
  const engines = useQuery(databaseEnginesQuery);
  const create = useCreateDatabase();
  const [name, setName] = useState("");
  const [nodeId, setNodeId] = useState("");
  const [placement, setPlacement] = useState(initialProjectId ? `${initialProjectId}:${initialEnvironmentId}` : "");
  const [engineId, setEngineId] = useState<DatabaseEngineId | "">("");
  const [version, setVersion] = useState("");
  const [errors, setErrors] = useState<{ name?: string; node?: string; engine?: string }>({});
  const readyNodes = nodes.filter((node) => node.state === "ready");
  const engine = engines.data?.find((item) => item.engine === engineId);
  const selectedVersion = engine && engine.versions.includes(version) ? version : (engine?.version ?? "");

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next: typeof errors = {};
    if (!name.trim()) next.name = "Enter a name.";
    else if (name.trim().length > 120) next.name = "Use at most 120 characters.";
    if (!nodeId) next.node = "Select a node.";
    if (!engine) next.engine = "Select an engine.";
    setErrors(next);
    const [projectId, environmentId] = placement.split(":");
    if (Object.keys(next).length > 0 || !engine || !projectId) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    create.mutate(
      {
        name: name.trim(),
        node_id: nodeId,
        project_id: projectId,
        ...(environmentId ? { environment_id: environmentId } : {}),
        kind: "database",
        database: { engine: engine.engine, version: selectedVersion, template_revision: engine.template_revision },
      },
      {
        onSuccess: ({ id: created }) => onCreated(created),
        // The pinned catalogue changed since it was loaded; reload it so the person reviews the current versions.
        onError: (error) => {
          if (isStatus(error, 409)) void engines.refetch();
        },
      },
    );
  }

  if (engines.isPending) return <LoadingRows label="Loading database engines" rows={3} />;
  if (engines.isError) {
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what="the database catalogue" error={engines.error} onRetry={() => void engines.refetch()} retrying={engines.isFetching} />
      </div>
    );
  }

  return (
    <form onSubmit={submit} noValidate>
      <fieldset disabled={create.isPending} className="min-w-0">
        <FormSection title="General" description="Where the database is listed and which node runs it.">
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            <input
              id={`${id}-name`}
              className={inputClass}
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={120}
              autoComplete="off"
              aria-invalid={errors.name ? true : undefined}
              aria-describedby={describedBy(`${id}-name`, errors.name)}
            />
          </Field>
          <ResourcePlacement projects={projects} value={placement} onChange={setPlacement} />
          {readyNodes.length === 0 ? (
            <Notice
              tone="warn"
              title="No ready nodes"
              action={
                <Link to="/nodes" className={buttonClass("secondary")}>
                  Open nodes
                </Link>
              }
            >
              A database needs a node that has passed SSH verification with Docker Compose available.
            </Notice>
          ) : (
            <Field id={`${id}-node`} label="Node" error={errors.node} hint="Data is stored in named volumes on this node.">
              <Select
                id={`${id}-node`}
                className={inputClass}
                value={nodeId}
                onChange={(event) => setNodeId(event.target.value)}
                aria-invalid={errors.node ? true : undefined}
                aria-describedby={describedBy(`${id}-node`, errors.node, true)}
              >
                <option value="">Select a node</option>
                {nodes.map((node) => (
                  <option key={node.id} value={node.id} disabled={node.state !== "ready"}>
                    {node.state === "ready" ? node.name : `${node.name} (${nodeStates[node.state]?.label ?? node.state})`}
                  </option>
                ))}
              </Select>
            </Field>
          )}
        </FormSection>

        <FormSection title="Engine" description="Pinned, reviewed configurations. Credentials and storage are generated when you save, and cannot be edited afterwards.">
          <EngineFields engines={engines.data} engineId={engineId} onEngine={setEngineId} version={selectedVersion} onVersion={setVersion} error={errors.engine} />
          <p className="text-xs text-fg-muted">
            The database gets a private hostname and exports DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME and DB_URL for bindings. It starts private: applications connect through a private connection, and public TLS access is an explicit later choice on its Database page.
          </p>
        </FormSection>
      </fieldset>

      <div className="space-y-4 px-4 py-5 sm:px-6 lg:pl-[calc(14rem+3.5rem)]">
        {create.isError && (
          <div className="max-w-3xl">
            <Notice tone="error" title="Could not create the database">
              {errorMessage(create.error)}
              {isStatus(create.error, 409) ? " The engine list has been reloaded; review the selection." : ""}
            </Notice>
          </div>
        )}
        <div className="flex items-center gap-2">
          <Button type="submit" variant="primary" pending={create.isPending} disabled={readyNodes.length === 0 || !placement}>
            Create database
          </Button>
          {cancel}
        </div>
      </div>
    </form>
  );
}

function EngineFields({ engines, engineId, onEngine, version, onVersion, error }: { engines: DatabaseEngine[]; engineId: string; onEngine: (engine: DatabaseEngineId) => void; version: string; onVersion: (version: string) => void; error?: string }) {
  const id = useId();
  const engine = engines.find((item) => item.engine === engineId);
  const options = engine ? versionOptions(engine) : [];
  const selected = options.find((option) => option.version === version) ?? options[0];
  return (
    <>
      <fieldset aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-engine-error` : undefined}>
        <legend className="mb-1.5 text-[13px] font-medium text-fg">Database engine</legend>
        <div className="grid gap-1.5 sm:grid-cols-2">
          {engines.map((item) => (
            <label key={item.engine} className={`${choiceClass} items-start`}>
              <input type="radio" name={`${id}-engine`} value={item.engine} checked={engineId === item.engine} onChange={() => onEngine(item.engine)} className="mt-0.5 accent-fg" />
              <span className="min-w-0 font-medium text-fg">
                {item.name}
                <span className="block text-xs font-normal text-fg-muted">
                  {item.version} · port {item.port} · {item.logical_backup ? "logical or volume backups" : "volume backups"}
                </span>
              </span>
            </label>
          ))}
        </div>
        {error && (
          <p id={`${id}-engine-error`} className="mt-1.5 text-xs text-bad">
            {error}
          </p>
        )}
      </fieldset>
      {engine && options.length > 1 && (
        <fieldset>
          <legend className="mb-1.5 text-[13px] font-medium text-fg">Version</legend>
          <div className="grid gap-1.5">
            {options.map((option) => (
              <label key={option.version} className={`${choiceClass} items-start`}>
                <input type="radio" name={`${id}-version`} value={option.version} checked={version === option.version} onChange={() => onVersion(option.version)} className="mt-0.5 accent-fg" />
                <span className="min-w-0">
                  <span className="font-medium text-fg">
                    {option.version}
                    {option.version === engine.version ? " (current)" : ""}
                    {variantNames[option.variant] ? ` · ${variantNames[option.variant]}` : ""}
                  </span>
                  {option.warning && <span className="mt-0.5 block text-xs text-warn">{option.warning}</span>}
                </span>
              </label>
            ))}
          </div>
        </fieldset>
      )}
      {engine && (
        <dl className="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-[8rem_minmax(0,1fr)]">
          <dt className="text-fg-muted">Image</dt>
          <dd className="font-mono break-all text-fg">{selected?.image ?? engine.image}</dd>
          <dt className="text-fg-muted">Service</dt>
          <dd className="font-mono text-fg">
            {engine.service}:{engine.port}
          </dd>
          {minimumText(engine) && (
            <>
              <dt className="text-fg-muted">Requirements</dt>
              <dd className="text-fg">{minimumText(engine)}</dd>
            </>
          )}
        </dl>
      )}
    </>
  );
}
