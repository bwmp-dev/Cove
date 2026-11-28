import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { FileUp } from "lucide-react";
import { useId, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { deleteImport, discoverCoolify, dryRunImport, importDiscoveryQuery, importKeys, importSessionsQuery, type ImportCandidate, type ImportDiscovery, type ImportDryRun, type ImportSession } from "../api/migration-imports";
import { applyConfigurationDraft, configurationKeys, deleteConfigurationDraft, type ProjectConfiguration } from "../api/project-configuration";
import type { Project } from "../api/projects";
import { keys, nodesQuery } from "../api/queries";
import type { Node } from "../api/types";
import { countLabel, productionEnvironments } from "../lib/hierarchy";
import { importLimitBytes, initialMappings, mappingErrors, parseCoolifyEnvelope, sampleEnvelope, type MappingDraft } from "../lib/migration-imports";
import { nodeStates } from "../lib/nodes";
import { describedBy, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ChangeList } from "./ConfigurationChanges";
import { ConfirmDialog } from "./ConfirmDialog";
import { ExpiryTime, RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const offline = "Unavailable while offline.";

interface Props {
  project: Project;
  configuration: UseQueryResult<ProjectConfiguration>;
  online: boolean;
}

export function CoolifyImport({ project, configuration, online }: Props) {
  const [selected, setSelected] = useState<string | null>(null);
  return (
    <Section
      title="Import from Coolify"
      description="Turn exported Coolify Docker Compose applications and services into a reviewed draft of new resources. Cove never contacts or changes the Coolify instance, does not adopt its running containers, copies no data and deploys nothing."
      flush
    >
      <div className="divide-y divide-line">
        <ImportSource projectId={project.id} online={online} onDiscovered={setSelected} />
        <ImportSessions projectId={project.id} online={online} selected={selected} onSelect={setSelected} />
        {selected && <ImportReview key={selected} project={project} importId={selected} configuration={configuration} online={online} onClose={() => setSelected(null)} />}
      </div>
    </Section>
  );
}

function ImportSource({ projectId, online, onDiscovered }: { projectId: string; online: boolean; onDiscovered: (id: string) => void }) {
  const id = useId();
  const client = useQueryClient();
  const fileInput = useRef<HTMLInputElement>(null);
  // The envelope holds real source secrets. It lives only here: never a mutation variable, query data or storage.
  const [content, setContent] = useState("");
  const [inputError, setInputError] = useState("");
  const discover = useMutation({
    gcTime: 0,
    networkMode: "always",
    mutationFn: () => {
      const parsed = parseCoolifyEnvelope(content);
      if (!parsed.items) return Promise.reject(new Error(parsed.error));
      return discoverCoolify(projectId, parsed.items);
    },
    onSuccess: (result) => {
      setContent("");
      client.setQueryData(importKeys.detail(projectId, result.id), { id: result.id, candidates: result.candidates, warnings: result.warnings } satisfies ImportDiscovery);
      onDiscovered(result.id);
    },
    onSettled: () => client.invalidateQueries({ queryKey: importKeys.all(projectId), exact: true }),
  });

  function edit(value: string) {
    discover.reset();
    setInputError("");
    setContent(value);
  }

  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    if (file.size > importLimitBytes) {
      setInputError("The file is larger than 2 MiB.");
      return;
    }
    try {
      edit(await file.text());
    } catch {
      setInputError("The file could not be read.");
    }
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseCoolifyEnvelope(content);
    if (parsed.error) {
      setInputError(parsed.error);
      return;
    }
    discover.mutate();
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={content !== ""} />
      <Subsection
        title="1. Discover"
        description="Build a JSON envelope from Coolify API responses you are authorised to read: for each resource, GET /api/v1/applications/{uuid} or /api/v1/services/{uuid} as resource, and the matching /envs response as environment_variables."
      >
        <div className="space-y-1.5">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <label htmlFor={`${id}-content`} className="text-[13px] font-medium text-fg">
              Export envelope
            </label>
            <Button size="sm" variant="ghost" onClick={() => fileInput.current?.click()} disabled={discover.isPending}>
              <FileUp aria-hidden className="size-3.5" />
              Upload file
            </Button>
            <input ref={fileInput} type="file" accept=".json,application/json" tabIndex={-1} aria-hidden className="hidden" onChange={(event) => void choose(event)} />
          </div>
          <textarea
            id={`${id}-content`}
            rows={8}
            value={content}
            onChange={(event) => edit(event.target.value)}
            disabled={discover.isPending}
            className={`${inputClass} h-auto py-2 font-mono text-xs`}
            placeholder={'{ "items": [ … ] }'}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            data-1p-ignore
            data-lpignore="true"
            aria-invalid={inputError ? true : undefined}
            aria-describedby={inputError ? `${id}-error` : `${id}-hint`}
          />
          {inputError ? (
            <p id={`${id}-error`} className="text-xs text-bad">
              {inputError}
            </p>
          ) : (
            <p id={`${id}-hint`} className="text-xs text-fg-muted">
              Up to 2 MiB and 32 resources. Only Docker Compose applications and services are supported. Values must be real, not locked or masked; Cove seals them on discovery and never shows them again. Preview variables and unrelated fields are discarded.
            </p>
          )}
        </div>
        <details className="text-xs">
          <summary className="cursor-pointer rounded text-fg-muted hover:text-fg">Envelope format (sample with placeholder values)</summary>
          <pre className="mt-2 overflow-x-auto rounded-md border border-line bg-panel p-3 font-mono text-fg-muted">{sampleEnvelope}</pre>
        </details>
        {discover.isError && (
          <Notice tone="error" title="Could not discover the export">
            {errorMessage(discover.error)}
          </Notice>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          {content !== "" && (
            <Button variant="ghost" onClick={() => edit("")} disabled={discover.isPending}>
              Clear
            </Button>
          )}
          <Button type="submit" variant={content === "" ? "secondary" : "primary"} pending={discover.isPending} disabled={!online || content === ""} title={online ? undefined : offline}>
            Discover resources
          </Button>
        </div>
      </Subsection>
    </form>
  );
}

function ImportSessions({ projectId, online, selected, onSelect }: { projectId: string; online: boolean; selected: string | null; onSelect: (id: string | null) => void }) {
  const client = useQueryClient();
  const sessions = useQuery(importSessionsQuery(projectId));
  const [confirm, setConfirm] = useState<ImportSession | null>(null);
  const remove = useMutation({
    mutationFn: (id: string) => deleteImport(projectId, id),
    onSuccess: (_, id) => {
      setConfirm(null);
      client.removeQueries({ queryKey: importKeys.detail(projectId, id) });
      if (selected === id) onSelect(null);
    },
    onSettled: () => client.invalidateQueries({ queryKey: importKeys.all(projectId), exact: true }),
  });
  const list = sessions.data ?? [];
  return (
    <Subsection title="Discovered imports" description="Discovery results are kept sealed for 24 hours so you can come back to them. Deleting one removes its sealed export; drafts already staged from it are kept.">
      {sessions.isPending ? (
        <LoadingRows label="Loading imports" rows={1} />
      ) : sessions.isError ? (
        <QueryError what="imports" error={sessions.error} onRetry={() => void sessions.refetch()} retrying={sessions.isFetching} />
      ) : list.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No active imports. Discover an export envelope to start.</p>
      ) : (
        <ul aria-label="Discovered imports" className="divide-y divide-line rounded-md border border-line">
          {list.map((session) => (
            <li key={session.id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-3 py-2">
              <p className="min-w-0 text-xs text-fg-muted">
                <span className="text-[13px] font-medium text-fg">
                  Discovered <RelativeTime value={session.created_at} />
                </span>
                <span className="mx-1.5">·</span>
                <ExpiryTime value={session.expires_at} />
              </p>
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    remove.reset();
                    setConfirm(session);
                  }}
                  disabled={!online || remove.isPending}
                  title={online ? undefined : offline}
                >
                  Delete
                </Button>
                <Button size="sm" onClick={() => onSelect(session.id)} disabled={selected === session.id} aria-current={selected === session.id ? "true" : undefined}>
                  Review
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <ConfirmDialog open={confirm !== null} title="Delete this import?" confirmLabel="Delete import" danger pending={remove.isPending} onCancel={() => setConfirm(null)} onConfirm={() => confirm && remove.mutate(confirm.id)}>
        <div className="space-y-3">
          <p>The sealed export and its discovered values are removed from Cove. Drafts staged from it and saved configuration are kept, and the Coolify source is not affected.</p>
          {remove.isError && (
            <Notice tone="error" title="Could not delete the import">
              {errorMessage(remove.error)}
            </Notice>
          )}
        </div>
      </ConfirmDialog>
    </Subsection>
  );
}

interface ReviewProps {
  project: Project;
  importId: string;
  configuration: UseQueryResult<ProjectConfiguration>;
  online: boolean;
  onClose: () => void;
}

function ImportReview({ project, importId, configuration, online, onClose }: ReviewProps) {
  const discovery = useQuery(importDiscoveryQuery(project.id, importId));
  const nodes = useQuery(nodesQuery);
  const close = (
    <Button size="sm" variant="ghost" onClick={onClose}>
      Close
    </Button>
  );
  let body;
  // Prefer loaded data over a failed background refetch, so a saved result stays visible.
  if (discovery.data && configuration.data && nodes.data) {
    body = <MappingForm project={project} importId={importId} discovery={discovery.data} configuration={configuration.data} nodes={nodes.data} online={online} onClose={onClose} />;
  } else if (discovery.isError) {
    body = isStatus(discovery.error, 404) ? (
      <Notice tone="warn" title="This import is no longer available">
        It expired or was deleted. Discover the export envelope again.
      </Notice>
    ) : (
      <QueryError what="the import" error={discovery.error} onRetry={() => void discovery.refetch()} retrying={discovery.isFetching} />
    );
  } else if (configuration.isError) {
    body = <QueryError what="the current configuration" error={configuration.error} onRetry={() => void configuration.refetch()} retrying={configuration.isFetching} />;
  } else if (nodes.isError) {
    body = <QueryError what="nodes" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />;
  } else {
    body = <LoadingRows label="Loading import" rows={3} />;
  }
  return (
    <Subsection title="2. Map and review" description="Choose which discovered resources to import and where their new configuration goes. A dry-run on the chosen node checks each Compose file without creating, starting or stopping containers." actions={close}>
      {body}
    </Subsection>
  );
}

interface FormProps {
  project: Project;
  importId: string;
  discovery: ImportDiscovery;
  configuration: ProjectConfiguration;
  nodes: Node[];
  online: boolean;
  onClose: () => void;
}

function MappingForm({ project, importId, discovery, configuration, nodes, online, onClose }: FormProps) {
  const client = useQueryClient();
  const candidates = discovery.candidates ?? [];
  const warnings = discovery.warnings ?? [];
  const existingKeys = new Set((configuration.manifest.resources ?? []).map((resource) => resource.key));
  const [mappings, setMappings] = useState(() => initialMappings(candidates, existingKeys));
  const [attempted, setAttempted] = useState(false);
  const [review, setReview] = useState<ImportDryRun | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [applied, setApplied] = useState<number | null>(null);
  const errors = mappingErrors(mappings, existingKeys);
  const chosen = candidates.filter((candidate) => mappings[candidate.id]?.selected);
  const importable = candidates.filter((candidate) => (candidate.blockers ?? []).length === 0);
  const capableNodes = nodes.filter((node) => node.state === "ready" && node.capabilities?.imports);

  const dryRun = useMutation({
    gcTime: 0,
    mutationFn: () =>
      dryRunImport(
        project.id,
        importId,
        chosen.map((candidate) => {
          const mapping = mappings[candidate.id]!;
          return { source_id: candidate.id, key: mapping.key, name: mapping.name.trim(), environment: mapping.environment, node_id: mapping.nodeId };
        }),
      ),
    onSuccess: (result) => {
      setReview(result);
      setAcknowledged(false);
    },
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.drafts(project.id) }),
  });
  const apply = useMutation({
    gcTime: 0,
    mutationFn: () => (review ? applyConfigurationDraft(project.id, review.draft_id) : Promise.reject(new Error("No reviewed draft."))),
    onSuccess: () => {
      setApplied(review?.changes?.length ?? chosen.length);
      setReview(null);
      setAcknowledged(false);
    },
    onSettled: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: configurationKeys.all(project.id) }),
        client.invalidateQueries({ queryKey: keys.workloads }),
        client.invalidateQueries({ queryKey: keys.projects }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]),
  });

  const discard = useMutation({
    gcTime: 0,
    mutationFn: () => (review ? deleteConfigurationDraft(project.id, review.draft_id) : Promise.resolve({ ok: true })),
    onSuccess: () => {
      setReview(null);
      setAcknowledged(false);
      apply.reset();
    },
    onSettled: () => client.invalidateQueries({ queryKey: configurationKeys.drafts(project.id) }),
  });
  function update(id: string, change: Partial<MappingDraft>) {
    dryRun.reset();
    setMappings((current) => ({ ...current, [id]: { ...current[id]!, ...change } }));
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttempted(true);
    if (chosen.length === 0 || Object.keys(errors).length > 0) return;
    dryRun.mutate();
  }

  if (applied !== null) {
    return (
      <div className="space-y-3">
        <Notice tone="success" title="New configuration saved">
          <p>{countLabel(applied, "imported resource")} added to this project's configuration. Nothing was deployed, no containers were created, started or adopted, no data was copied and the Coolify source was not contacted or changed.</p>
          <p className="mt-1">Configure domains, backups and data on each new resource, pause the source manager's automation, then deploy and cut over separately when the destination is ready.</p>
        </Notice>
        <div className="flex justify-end">
          <Button onClick={onClose}>Done</Button>
        </div>
      </div>
    );
  }

  if (candidates.length === 0) return <p className="text-[13px] text-fg-muted">The export contained no applications or services.</p>;

  const stale = review !== null && review.base_version !== configuration.version;
  const locked = review !== null || dryRun.isPending;
  const environments = productionEnvironments(project);
  return (
    <div className="space-y-4">
      {warnings.length > 0 && review === null && (
        <Notice tone="warn" title="What import does not do">
          <ul className="list-disc space-y-1 pl-4">
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </Notice>
      )}
      {importable.length > 0 && capableNodes.length === 0 && (
        <Notice tone="warn" title="No node can run import dry-runs">
          Upgrade a ready worker to a version that supports config-only Compose imports and verify it, then choose it here.
        </Notice>
      )}
      <form onSubmit={submit} noValidate className="space-y-4">
        <ul aria-label="Discovered resources" className="divide-y divide-line rounded-md border border-line">
          {candidates.map((candidate) => (
            <CandidateRow
              key={candidate.id}
              candidate={candidate}
              mapping={mappings[candidate.id]!}
              errors={attempted ? errors[candidate.id] : undefined}
              environments={environments}
              nodes={nodes}
              locked={locked}
              onChange={(change) => update(candidate.id, change)}
            />
          ))}
        </ul>
        {attempted && chosen.length === 0 && <p className="text-xs text-bad">Choose at least one resource without blockers.</p>}
        {dryRun.isError && (
          <Notice tone="error" title="Dry-run failed">
            {errorMessage(dryRun.error)}
            {isStatus(dryRun.error, 409) && " Nothing was staged. Correct the source export or the destination, then run the dry-run again."}
            {isStatus(dryRun.error, 404) && " This import expired or was deleted. Discover the export envelope again."}
          </Notice>
        )}
        {review === null && (
          <div className="flex flex-wrap items-center justify-end gap-x-4 gap-y-2">
            <p className="text-xs text-fg-muted">{countLabel(chosen.length, "resource")} selected</p>
            <Button type="submit" variant="primary" pending={dryRun.isPending} disabled={!online || importable.length === 0} title={online ? undefined : offline}>
              Run dry-run
            </Button>
          </div>
        )}
      </form>
      {review && (
        <div className="space-y-3 border-t border-line pt-4">
          <div>
            <h4 className="text-[13px] font-semibold text-fg">3. Review and apply</h4>
            <p className="mt-0.5 text-xs text-fg-muted">The dry-run staged a draft; it also appears under Drafts. Applying saves only this new configuration. Variable values and Compose contents are not shown.</p>
          </div>
          <ChangeList changes={review.changes} label="Changes in the import draft" />
          {(review.warnings ?? []).length > 0 && (
            <div className="space-y-1">
              <p className="text-xs font-medium text-fg">Warnings</p>
              <ul aria-label="Import warnings" className="list-disc space-y-1 pl-5 text-xs text-fg-muted">
                {(review.warnings ?? []).map((warning) => (
                  <li key={warning}>{warning}</li>
                ))}
              </ul>
            </div>
          )}
          {stale && (
            <Notice tone="warn" title="The configuration changed after the dry-run">
              Discard this draft and run the dry-run again to review it against the current configuration.
            </Notice>
          )}
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} disabled={apply.isPending} className="mt-0.5" />
            <span>I understand this only saves new configuration in Cove. It copies no data and does not check the destination node. I will pause and verify the source manager's automation myself, and deploying or cutting over these resources is a later, separate step.</span>
          </label>
          {apply.isError && (
            <Notice tone="error" title="Could not apply the import draft">
              {errorMessage(apply.error)}
              {isStatus(apply.error, 409) && " If the configuration changed since the dry-run, discard this draft and run the dry-run again."}
            </Notice>
          )}
          {discard.isError && (
            <Notice tone="error" title="Could not discard the draft">
              {errorMessage(discard.error)}
            </Notice>
          )}
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="ghost" onClick={() => discard.mutate()} pending={discard.isPending} disabled={!online || apply.isPending} title={online ? undefined : offline}>
              Discard and edit mapping
            </Button>
            <Button variant="primary" onClick={() => apply.mutate()} pending={apply.isPending} disabled={!online || !acknowledged || stale || discard.isPending} title={!online ? offline : stale ? "Saved against an older configuration" : undefined}>
              Apply import draft
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

interface RowProps {
  candidate: ImportCandidate;
  mapping: MappingDraft;
  errors: Partial<Record<"key" | "name" | "nodeId", string>> | undefined;
  environments: { id: string; name: string }[];
  nodes: Node[];
  locked: boolean;
  onChange: (change: Partial<MappingDraft>) => void;
}

function CandidateRow({ candidate, mapping, errors, environments, nodes, locked, onChange }: RowProps) {
  const id = useId();
  const blockers = candidate.blockers ?? [];
  const blocked = blockers.length > 0;
  return (
    <li className="min-w-0 space-y-3 px-3 py-3">
      <div className="flex min-w-0 items-start gap-2.5">
        {!blocked && <Checkbox id={`${id}-include`} checked={mapping.selected} onChange={(event) => onChange({ selected: event.target.checked })} disabled={locked} aria-label={`Import ${candidate.name || candidate.id}`} className="mt-0.5" />}
        <div className="min-w-0 flex-1">
          <p className="flex flex-wrap items-baseline gap-x-2 text-[13px]">
            <span className="font-medium break-words text-fg">{candidate.name || "Unnamed resource"}</span>
            <span className="text-xs text-fg-muted">{candidate.kind === "service" ? "Service" : candidate.kind === "application" ? "Application" : candidate.kind}</span>
          </p>
          <p className="font-mono text-xs break-all text-fg-subtle">{candidate.id}</p>
        </div>
      </div>
      {blocked && (
        <div className="space-y-1 rounded-md border border-warn-line bg-warn-tint px-3 py-2">
          <p className="text-xs font-medium text-fg">Cannot be imported until the export is corrected</p>
          <ul aria-label={`Blockers for ${candidate.name || candidate.id}`} className="list-disc space-y-0.5 pl-4 text-xs text-fg-muted">
            {blockers.map((blocker) => (
              <li key={blocker}>{blocker}</li>
            ))}
          </ul>
        </div>
      )}
      {!blocked && mapping.selected && (
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <Field id={`${id}-key`} label="Resource key" error={errors?.key} hint="New, unused cove.yaml key.">
            <input id={`${id}-key`} value={mapping.key} onChange={(event) => onChange({ key: event.target.value })} disabled={locked} className={`${inputClass} font-mono`} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={errors?.key ? true : undefined} aria-describedby={describedBy(`${id}-key`, errors?.key, true)} />
          </Field>
          <Field id={`${id}-name`} label="Name" error={errors?.name} optional>
            <input id={`${id}-name`} value={mapping.name} onChange={(event) => onChange({ name: event.target.value })} disabled={locked} className={inputClass} placeholder={candidate.name} autoComplete="off" aria-invalid={errors?.name ? true : undefined} aria-describedby={errors?.name ? `${id}-name-error` : undefined} />
          </Field>
          <Field id={`${id}-environment`} label="Environment">
            <Select id={`${id}-environment`} value={mapping.environment} onChange={(event) => onChange({ environment: event.target.value })} disabled={locked}>
              <option value="">Project resources</option>
              {environments.map((environment) => (
                <option key={environment.id} value={environment.name}>
                  {environment.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field id={`${id}-node`} label="Node" error={errors?.nodeId}>
            <Select id={`${id}-node`} value={mapping.nodeId} onChange={(event) => onChange({ nodeId: event.target.value })} disabled={locked} aria-invalid={errors?.nodeId ? true : undefined} aria-describedby={errors?.nodeId ? `${id}-node-error` : undefined}>
              <option value="" disabled>
                Choose a node
              </option>
              {nodes.map((node) => (
                <option key={node.id} value={node.id} disabled={node.state !== "ready" || !node.capabilities?.imports}>
                  {node.state !== "ready" ? `${node.name} (${nodeStates[node.state]?.label ?? node.state})` : node.capabilities?.imports ? node.name : `${node.name} (needs worker upgrade)`}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      )}
    </li>
  );
}
