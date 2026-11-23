import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { statusPageQuery, type StatusPageSettings as Settings } from "../api/outputs";
import { withBasePath } from "../lib/base-path";
import { useOnline } from "../lib/hooks";
import { buttonClass, describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Field, LoadingRows, Notice, QueryError, Section, SettingRows, SwitchField } from "./ui";

const encoder = new TextEncoder();
const description = "An optional page anyone with its link can open without signing in. It lists this project's enabled uptime checks that have a public label.";

function StatusPageForm({ projectId, saved }: { projectId: string; saved: Settings }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [enabled, setEnabled] = useState(saved.enabled);
  const [title, setTitle] = useState(saved.title);
  const [error, setError] = useState<string>();
  const [confirming, setConfirming] = useState(false);
  const save = useMutation({
    mutationFn: (input: { enabled: boolean; title: string }) => request<Settings>("PUT", `/api/projects/${encodeURIComponent(projectId)}/status-page`, input),
    onSuccess: (settings) => {
      client.setQueryData(statusPageQuery(projectId).queryKey, settings);
      setConfirming(false);
    },
  });
  const dirty = enabled !== saved.enabled || title.trim() !== saved.title;
  const publicURL = saved.slug ? `${window.location.origin}${withBasePath(`/status/${encodeURIComponent(saved.slug)}`)}` : "";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = title.trim();
    if ((enabled && trimmed === "") || encoder.encode(trimmed).length > 120) {
      setError("Enter a title of at most 120 bytes.");
      document.getElementById(`${id}-title`)?.focus();
      return;
    }
    setError(undefined);
    // Publishing is a disclosure decision, so turning the page on always asks first.
    if (enabled && !saved.enabled) {
      setConfirming(true);
      return;
    }
    save.mutate({ enabled, title: trimmed });
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Public status page"
        description={description}
        width="form"
        footer={
          <SaveBar
            dirty={dirty}
            pending={save.isPending}
            blocked={!online}
            onDiscard={() => {
              setEnabled(saved.enabled);
              setTitle(saved.title);
              setError(undefined);
              save.reset();
            }}
          />
        }
      >
        <div className="space-y-4">
          <SettingRows>
            <SwitchField id={`${id}-enabled`} layout="row" label="Publish status page" description="When off, the link returns Not found." checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
            <Field id={`${id}-title`} layout="row" label="Title" optional={!enabled} hint="Shown as the page heading." error={error}>
              <input id={`${id}-title`} className={inputClass} value={title} onChange={(event) => setTitle(event.target.value)} maxLength={120} autoComplete="off" aria-invalid={error ? true : undefined} aria-describedby={describedBy(`${id}-title`, error, true)} />
            </Field>
          </SettingRows>
          {saved.enabled && publicURL && (
            <div className="space-y-2 border-t border-line pt-4">
              <Field id={`${id}-url`} label="Public link" hint="The address stays the same when you turn the page off and on again.">
                <input id={`${id}-url`} className={`${inputClass} font-mono`} value={publicURL} readOnly aria-describedby={`${id}-url-hint`} />
              </Field>
              <div className="flex flex-wrap items-center gap-2">
                <CopyButton text={publicURL} label="Copy link" />
                <a href={publicURL} target="_blank" rel="noreferrer" className={buttonClass("ghost")}>
                  <ExternalLink aria-hidden className="size-3.5" />
                  Open page
                </a>
              </div>
            </div>
          )}
          {save.isError && <Notice tone="error" title="Could not save the status page">{errorMessage(save.error)}</Notice>}
        </div>
      </Section>
      <ConfirmDialog
        open={confirming}
        title="Publish this status page?"
        confirmLabel="Publish page"
        pending={save.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => save.mutate({ enabled: true, title: title.trim() })}
      >
        Anyone with the link can see, without signing in, the page title, each public label, whether the check is up, its latency, when it last ran and the share of successful checks over 24 hours. Domains, paths, error messages and other project details are not shown. Search engines and others may find the link if it is shared.
        {save.isError && <span className="mt-2 block text-bad">{errorMessage(save.error)}</span>}
      </ConfirmDialog>
    </form>
  );
}

export function StatusPageSettings({ projectId }: { projectId: string }) {
  const settings = useQuery(statusPageQuery(projectId));
  if (settings.isPending) {
    return (
      <Section title="Public status page" description={description} width="form" flush>
        <LoadingRows label="Loading status page settings" rows={2} />
      </Section>
    );
  }
  if (settings.isError) {
    return (
      <Section title="Public status page" description={description} width="form">
        <QueryError what="status page settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
      </Section>
    );
  }
  return <StatusPageForm key={`${settings.data.enabled}-${settings.data.title}-${settings.data.slug}`} projectId={projectId} saved={settings.data} />;
}
