import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link2, Link2Off, RotateCw } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { createStatusShare, revokeStatusShare, statusShareQuery, type CreatedStatusShare, type StatusShare } from "../api/status-share";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { ExpiryTime, RelativeTime } from "./RelativeTime";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const disclosure =
  "Anyone with the link sees the public title you choose and counts of each resource's latest deployment result (succeeded, failed, pending, stopped or unknown). Resource names, IDs, notes, configuration, secrets and live health are never shared.";

// Editors only: the API refuses everyone else, including for reading whether a link exists.
export function ProjectStatusShare({ projectId }: { projectId: string }) {
  const share = useQuery(statusShareQuery(projectId));
  // The one-time links live only in this component's state; they are not cached, stored or retrievable again.
  const [created, setCreated] = useState<CreatedStatusShare | null>(null);
  const [configuring, setConfiguring] = useState(false);

  return (
    <Section
      title="Public status page"
      description="Opt in to a read-only page and badge showing this project's latest deployment results, for status pages or READMEs. Off by default."
      width="form"
    >
      {share.isPending ? (
        <LoadingRows label="Loading sharing" rows={1} />
      ) : share.isError ? (
        <QueryError what="sharing" error={share.error} onRetry={() => void share.refetch()} retrying={share.isFetching} />
      ) : created ? (
        <CreatedLinks links={created} onDone={() => setCreated(null)} />
      ) : configuring || !share.data.enabled ? (
        <ShareForm
          projectId={projectId}
          current={share.data}
          onCreated={(links) => {
            setConfiguring(false);
            setCreated(links);
          }}
          onCancel={share.data.enabled ? () => setConfiguring(false) : undefined}
        />
      ) : (
        <ActiveShare projectId={projectId} share={share.data} onRotate={() => setConfiguring(true)} />
      )}
    </Section>
  );
}

function ActiveShare({ projectId, share, onRotate }: { projectId: string; share: Extract<StatusShare, { enabled: true }>; onRotate: () => void }) {
  const client = useQueryClient();
  const online = useOnline();
  const [revoking, setRevoking] = useState(false);
  const revoke = useMutation({
    mutationFn: () => revokeStatusShare(projectId),
    gcTime: 0,
    onSuccess: () => {
      setRevoking(false);
      void client.invalidateQueries({ queryKey: statusShareQuery(projectId).queryKey });
    },
  });

  return (
    <div className="space-y-4">
      <div className="space-y-1">
        <p className="text-[13px] font-medium text-fg">
          Sharing as <span className="break-words">“{share.title}”</span>
        </p>
        <p className="text-xs text-fg-muted">
          Created <RelativeTime value={share.created_at} /> · <ExpiryTime value={share.expires_at} subject="Link" />
        </p>
      </div>
      {share.ownership_required ? (
        <Notice tone="warn" title="Sharing is suspended">
          This link has no currently authorised creator: the account that created it was deactivated, no longer has edit access to the project or the required two-step verification, or is not recorded. The page and badge are not served. Rotate the link to make your account responsible for it; the current link stays off.
        </Notice>
      ) : (
        <p className="text-xs text-fg-muted">The link was shown once when it was created. Rotate it to get a new link; the current one stops working immediately.</p>
      )}
      {revoke.isError && (
        <Notice tone="error" title="Could not turn off sharing">
          {errorMessage(revoke.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="danger" onClick={() => setRevoking(true)} disabled={!online}>
          <Link2Off aria-hidden className="size-3.5" />
          Turn off sharing
        </Button>
        <Button onClick={onRotate} disabled={!online}>
          <RotateCw aria-hidden className="size-3.5" />
          Rotate link
        </Button>
      </div>
      <ConfirmDialog
        open={revoking}
        title="Turn off the public status page?"
        confirmLabel="Turn off sharing"
        danger
        pending={revoke.isPending}
        onConfirm={() => revoke.mutate()}
        onCancel={() => setRevoking(false)}
      >
        The page and badge stop working immediately for everyone who has the link. Embedded badges show as broken images.
      </ConfirmDialog>
    </div>
  );
}

function PreviousLinkNotice({ share }: { share: Extract<StatusShare, { enabled: false }> }) {
  if (share.ownership_required) {
    return (
      <Notice tone="warn" title="Sharing is suspended">
        The previous link has no currently authorised creator: the account that created it was deactivated, no longer has edit access to the project or the required two-step verification, or is not recorded. The page and badge are not served. Create a new link to make your account responsible for it.
      </Notice>
    );
  }
  if (!share.expires_at) return null;
  return (
    <Notice tone="warn" title="The previous link expired">
      Create a new link to share the status page again.
    </Notice>
  );
}

function ShareForm({ projectId, current, onCreated, onCancel }: { projectId: string; current: StatusShare; onCreated: (links: CreatedStatusShare) => void; onCancel?: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const rotating = current.enabled;
  const [title, setTitle] = useState(current.title ?? "");
  const [days, setDays] = useState("30");
  const [acknowledged, setAcknowledged] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const daysValue = Number(days);
  const daysValid = Number.isInteger(daysValue) && daysValue >= 1 && daysValue <= 365;
  const titleValid = title.trim() !== "" && new TextEncoder().encode(title.trim()).length <= 80;

  const create = useMutation({
    mutationFn: () => createStatusShare(projectId, title.trim(), daysValue),
    gcTime: 0,
    onSuccess: (links) => {
      setConfirming(false);
      create.reset();
      onCreated(links);
      void client.invalidateQueries({ queryKey: statusShareQuery(projectId).queryKey });
    },
    onError: () => setConfirming(false),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!acknowledged || !daysValid || !titleValid) return;
    if (rotating) setConfirming(true);
    else create.mutate();
  }

  return (
    <form className="space-y-4" onSubmit={submit}>
      {!current.enabled && <PreviousLinkNotice share={current} />}
      <Notice title="What is shared">{disclosure}</Notice>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-title`} label="Public title" hint="Shown on the page. Use a public name, not an internal one.">
          <input
            id={`${id}-title`}
            className={inputClass}
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            maxLength={80}
            required
            autoComplete="off"
            aria-describedby={`${id}-title-hint`}
          />
        </Field>
        <Field id={`${id}-days`} label="Expires after (days)" hint="1 to 365 days. You can turn it off at any time.">
          <input
            id={`${id}-days`}
            className={inputClass}
            type="number"
            inputMode="numeric"
            min={1}
            max={365}
            step={1}
            value={days}
            onChange={(event) => setDays(event.target.value)}
            required
            aria-invalid={!daysValid || undefined}
            aria-describedby={`${id}-days-hint`}
          />
        </Field>
      </div>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} required className="mt-px" />
        <span>I understand that anyone who has the link can see this project's deployment results until it is turned off or expires.</span>
      </label>
      {create.isError && (
        <Notice tone="error" title="Could not create the link">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        {onCancel && (
          <Button variant="ghost" onClick={onCancel} disabled={create.isPending}>
            Cancel
          </Button>
        )}
        <Button type="submit" variant="primary" pending={create.isPending && !rotating} disabled={!online || !acknowledged || !daysValid || !titleValid}>
          <Link2 aria-hidden className="size-3.5" />
          {rotating ? "Rotate link" : "Create public link"}
        </Button>
      </div>
      <ConfirmDialog
        open={confirming}
        title="Rotate the public link?"
        confirmLabel="Rotate link"
        danger
        pending={create.isPending}
        onConfirm={() => create.mutate()}
        onCancel={() => setConfirming(false)}
      >
        The current link and badge stop working immediately. Anyone using them needs the new link.
      </ConfirmDialog>
    </form>
  );
}

function CreatedLinks({ links, onDone }: { links: CreatedStatusShare; onDone: () => void }) {
  const id = useId();
  // The badge is offered as text only: loading it here would send the token in an image request from the admin page.
  const badgeMarkdown = `![Deployment status](${links.badge_url})`;
  return (
    <div className="space-y-4">
      <Notice tone="warn" title="Copy these links now">
        They are shown only once and cannot be retrieved later. Anyone who has them can see this project's deployment results until you turn sharing off or the link expires. <ExpiryTime value={links.expires_at} subject="The link" />.
      </Notice>
      <Field id={`${id}-dashboard`} label="Status page link">
        <div className="flex flex-wrap items-center gap-2">
          <input id={`${id}-dashboard`} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} value={links.dashboard_url} readOnly onFocus={(event) => event.currentTarget.select()} />
          <CopyButton text={links.dashboard_url} label="Copy status page link" />
        </div>
      </Field>
      <Field id={`${id}-badge`} label="Badge image URL">
        <div className="flex flex-wrap items-center gap-2">
          <input id={`${id}-badge`} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} value={links.badge_url} readOnly onFocus={(event) => event.currentTarget.select()} />
          <CopyButton text={links.badge_url} label="Copy badge URL" />
        </div>
      </Field>
      <Field id={`${id}-markdown`} label="Badge Markdown" hint="Paste into a README. The badge shows a generic deployment status without your title.">
        <div className="flex flex-wrap items-center gap-2">
          <input id={`${id}-markdown`} className={`${inputClass} flex-1 basis-64 font-mono text-xs`} value={badgeMarkdown} readOnly onFocus={(event) => event.currentTarget.select()} aria-describedby={`${id}-markdown-hint`} />
          <CopyButton text={badgeMarkdown} label="Copy badge Markdown" />
        </div>
      </Field>
      <div className="flex justify-end">
        <Button variant="primary" onClick={onDone}>
          I have copied the links
        </Button>
      </div>
    </div>
  );
}
