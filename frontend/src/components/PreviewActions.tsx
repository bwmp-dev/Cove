import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { useDeletePreview, usePinPreview, usePreviewAction, useRefreshPreview, type EnvironmentPreview } from "../api/previews";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { ActionMenu, type MenuAction } from "./ActionMenu";
import { Dialog } from "./ConfirmDialog";
import { CIApprovalButton } from "./PreviewCIApproval";
import { Button, Field, Notice } from "./ui";

const offline = "Unavailable while offline.";

// The state's next step as a button, plus the remaining actions in a menu. Used in the preview list and on
// the preview environment page, so both offer the same actions with the same confirmations.
export function PreviewActions({ preview, size = "md" }: { preview: EnvironmentPreview; size?: "sm" | "md" }) {
  const online = useOnline();
  const action = usePreviewAction();
  const pin = usePinPreview();
  const refresh = useRefreshPreview();
  const remove = useDeletePreview();
  const [dialog, setDialog] = useState<"refresh" | "delete" | null>(null);
  const editable = can(preview.permissions, "edit");
  const operable = can(preview.permissions, "operate");
  const deleting = preview.state === "deleting";
  const settled = preview.state === "ready" || preview.state === "failed";
  const created = preview.environment_id !== "";

  const reason = (allowed: boolean, why: string) => (!online ? offline : allowed ? undefined : why);
  const menu: MenuAction[] = [];
  if (operable) {
    menu.push({ label: "Redeploy", onSelect: () => action.mutate({ id: preview.id, action: "redeploy" }), disabledReason: reason(settled && created, "Wait until the preview is ready or failed.") });
  }
  if (editable) {
    menu.push(
      { label: preview.pinned ? "Unpin" : "Keep (pin)", onSelect: () => pin.mutate({ id: preview.id, pinned: !preview.pinned }), disabledReason: reason(!deleting, "The preview is being removed.") },
      { label: "Refresh data from production", onSelect: () => setDialog("refresh"), disabledReason: reason(settled && created, "Wait until the preview is ready or failed.") },
      { label: "Delete preview", danger: true, onSelect: () => setDialog("delete"), disabledReason: reason(!deleting, "The preview is already being removed.") },
    );
  }
  const failure = action.error ?? pin.error;
  return (
    <div className="flex flex-wrap items-center justify-end gap-2">
      {preview.state === "awaiting_approval" && editable && (preview.ci ? <CIApprovalButton preview={preview} size={size} disabled={!online} /> : (
        <Button size={size} variant="primary" disabled={!online} pending={action.isPending} onClick={() => action.mutate({ id: preview.id, action: "approve" })}>
          Approve
        </Button>
      ))}
      {preview.state === "failed" && operable && (
        <Button size={size} disabled={!online} pending={action.isPending} onClick={() => action.mutate({ id: preview.id, action: "retry" })}>
          Retry
        </Button>
      )}
      {menu.length > 0 && <ActionMenu label={`Actions for preview ${preview.slug}`} actions={menu} />}
      {failure && (
        <p role="alert" className="basis-full text-right text-xs text-bad">
          {errorMessage(failure)}
        </p>
      )}
      <TypedConfirmation
        open={dialog === "refresh"}
        preview={preview}
        title={`Replace ${preview.slug}'s data?`}
        confirmLabel="Replace data"
        onClose={() => setDialog(null)}
        mutation={refresh}
      >
        <p>
          The preview's databases and copied storage are replaced with a fresh copy of production. Everything written to this preview since it was created is lost. Production is only read.
        </p>
      </TypedConfirmation>
      <TypedConfirmation open={dialog === "delete"} preview={preview} title={`Delete ${preview.slug}?`} confirmLabel="Delete preview" onClose={() => setDialog(null)} mutation={remove}>
        <p>Its containers, copied data, routes and DNS records are removed. Branches and production are not changed.</p>
      </TypedConfirmation>
    </div>
  );
}

function TypedConfirmation({ open, preview, title, confirmLabel, onClose, mutation, children }: {
  open: boolean;
  preview: EnvironmentPreview;
  title: string;
  confirmLabel: string;
  onClose: () => void;
  mutation: ReturnType<typeof useDeletePreview>;
  children: ReactNode;
}) {
  const id = useId();
  const online = useOnline();
  const [typed, setTyped] = useState("");
  const close = () => {
    setTyped("");
    mutation.reset();
    onClose();
  };
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    mutation.mutate({ id: preview.id, confirm: typed }, { onSuccess: close });
  }
  return (
    <Dialog open={open} title={title} onClose={close}>
      <form onSubmit={submit} className="space-y-4 text-[13px] text-fg-muted">
        {children}
        <Field id={id} label={`Type ${preview.slug} to confirm`}>
          <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required />
        </Field>
        {mutation.isError && (
          <Notice tone="error" title="Could not complete the action">
            {errorMessage(mutation.error)}
          </Notice>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={close} disabled={mutation.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!online || typed !== preview.slug} pending={mutation.isPending}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
