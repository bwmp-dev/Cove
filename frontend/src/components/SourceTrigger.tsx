import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, request } from "../api/client";
import { gitConnectionsQuery, gitTriggerQuery, providerSourceQuery, type GitTrigger, type TriggerKind } from "../api/git-providers";
import { githubConnectionQuery } from "../api/queries";
import type { Workload } from "../api/types";
import { describedBy, inputClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { ChoiceGroup, type Choice } from "./WorkloadForm";
import { DefinitionList, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const title = "Automatic trigger";
const description = "Which repository events deploy this resource automatically. Manual deployments always use the saved branch.";

const triggerLabels: Record<TriggerKind, string> = { branch: "Branch pushes", tags: "Tags", releases: "Releases" };

function choices(branch: string, bitbucket: boolean): Choice<TriggerKind>[] {
  const list: Choice<TriggerKind>[] = [
    { value: "branch", label: triggerLabels.branch, description: `Pushes to ${branch || "the saved branch"}, filtered by watch paths.` },
    { value: "tags", label: triggerLabels.tags, description: "New tags whose name matches the pattern." },
    { value: "releases", label: triggerLabels.releases, description: "Published releases whose tag matches the pattern." },
  ];
  return bitbucket ? list.filter((choice) => choice.value !== "releases") : list;
}

function validatePattern(pattern: string) {
  if (pattern === "") return "Enter a tag pattern, such as v*.";
  if (pattern.length > 200 || /[\r\n\0]/.test(pattern)) return "Enter a single-line pattern of at most 200 characters.";
  return null;
}

function TriggerForm({ workload, saved, github, bitbucket, blocked, blockers }: { workload: Workload; saved: GitTrigger; github: boolean; bitbucket: boolean; blocked: boolean; blockers: ReactNode }) {
  const id = useId();
  const client = useQueryClient();
  const [kind, setKind] = useState(saved.kind);
  const [pattern, setPattern] = useState(saved.pattern);
  const [error, setError] = useState<string>();
  const save = useMutation({
    mutationFn: (input: GitTrigger) => request<GitTrigger>("PUT", `/api/workloads/${encodeURIComponent(workload.id)}/git-trigger`, input),
    onSuccess: (data) => client.setQueryData(gitTriggerQuery(workload.id).queryKey, data),
    networkMode: "always",
  });
  const usesPattern = kind !== "branch";
  const dirty = kind !== saved.kind || (usesPattern && pattern !== saved.pattern);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = pattern.trim();
    const found = usesPattern ? validatePattern(trimmed) : null;
    setError(found ?? undefined);
    if (found) return;
    save.mutate({ kind, pattern: usesPattern ? trimmed : "" });
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title={title}
        description={description}
        footer={<SaveBar dirty={dirty} pending={save.isPending} blocked={blocked} onDiscard={() => { setKind(saved.kind); setPattern(saved.pattern); setError(undefined); save.reset(); }} status={save.isSuccess ? "Trigger saved" : undefined} />}
      >
        <fieldset disabled={save.isPending} className="min-w-0 space-y-5">
          {blockers}
          <ChoiceGroup legend="Deploy on" name={`${id}-kind`} value={kind} choices={choices(workload.source?.branch ?? "", bitbucket)} onChange={(next) => { save.reset(); setKind(next); }} />
          {bitbucket && <p className="-mt-3 text-xs text-fg-muted">Bitbucket Cloud has no release events. Use a tag pattern instead.</p>}
          {usesPattern && (
            <>
              <Field id={`${id}-pattern`} label="Tag pattern" error={error} hint="Glob on the tag name: * matches any characters except /, ? one character, [abc] a set. For example v* or v[0-9]*.">
                <input id={`${id}-pattern`} className={`${inputClass} font-mono sm:max-w-xs`} value={pattern} maxLength={200} placeholder="v*" autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => { save.reset(); setPattern(event.target.value); }} aria-invalid={error ? true : undefined} aria-describedby={describedBy(`${id}-pattern`, error, true)} />
              </Field>
              <p className="text-xs text-fg-muted">
                Cove deploys the commit the tag points to and ignores watch paths. The saved branch is still used for manual deployments.
                {kind === "releases" && (github ? " GitHub Apps created before release support must subscribe to the Release event." : " The connection's webhook must include release events (Release Hook on GitLab).")}
                {kind === "tags" && !github && " The connection's webhook must include tag push events."}
              </p>
            </>
          )}
          {save.isError && <Notice tone="error" title="Could not save the trigger">{errorMessage(save.error)}</Notice>}
        </fieldset>
      </Section>
    </form>
  );
}

// Only resources connected through the GitHub App or a Git provider receive repository events.
export function SourceTrigger({ workload, editable, blocked, blockers }: { workload: Workload; editable: boolean; blocked: boolean; blockers: ReactNode }) {
  const github = useQuery(githubConnectionQuery(workload.id));
  const provider = useQuery(providerSourceQuery(workload.id));
  const connections = useQuery({ ...gitConnectionsQuery, enabled: Boolean(provider.data) });
  const managed = github.data ?? provider.data;
  const trigger = useQuery({ ...gitTriggerQuery(workload.id), enabled: Boolean(managed) });
  if (!managed) return null;
  if (trigger.isPending) return <Section width="form" title={title} description={description}><LoadingRows label="Loading trigger" rows={2} /></Section>;
  if (trigger.isError) return <Section width="form" title={title} description={description}><QueryError what="the trigger" error={trigger.error} onRetry={() => void trigger.refetch()} retrying={trigger.isFetching} /></Section>;

  const automaticOff = !managed.automatic && (
    <Notice tone="warn" title="Automatic deployments are off">Repository events are ignored until automatic deployments are turned on in the connection settings on this page.</Notice>
  );
  if (!editable) {
    return (
      <Section width="form" title={title} description={description}>
        <div className="space-y-4">
          {automaticOff}
          <DefinitionList items={[{ term: "Deploy on", value: triggerLabels[trigger.data.kind] }, ...(trigger.data.kind === "branch" ? [] : [{ term: "Tag pattern", value: <span className="font-mono text-xs break-all">{trigger.data.pattern}</span> }])]} />
        </div>
      </Section>
    );
  }
  const bitbucket = connections.data?.find((item) => item.id === provider.data?.connection_id)?.kind === "bitbucket";
  return (
    <TriggerForm
      key={`${trigger.data.kind}:${trigger.data.pattern}`}
      workload={workload}
      saved={trigger.data}
      github={Boolean(github.data)}
      bitbucket={bitbucket}
      blocked={blocked}
      blockers={<>{blockers}{automaticOff}</>}
    />
  );
}
