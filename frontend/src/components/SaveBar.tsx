import { useBlocker } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./ui";

// Asks before leaving a page, or closing the tab, while it has unsaved changes.
export function UnsavedChangesGuard({ dirty }: { dirty: boolean }) {
  const blocker = useBlocker({ shouldBlockFn: () => dirty, enableBeforeUnload: dirty, withResolver: true });
  return (
    <ConfirmDialog
      open={blocker.status === "blocked"}
      title="Discard unsaved changes?"
      confirmLabel="Discard changes"
      danger
      onCancel={() => blocker.reset?.()}
      onConfirm={() => blocker.proceed?.()}
    >
      Changes on this page have not been saved.
    </ConfirmDialog>
  );
}

interface SaveBarProps {
  dirty: boolean;
  pending: boolean;
  // Blocks saving for reasons shown elsewhere, such as being offline or an active operation.
  blocked?: boolean;
  label?: string;
  onDiscard: () => void;
  // Shown when there are no unsaved changes, for example after a successful save.
  status?: ReactNode;
}

// Status on the left and actions at the end, with the primary action last. Pass it as a Section footer;
// inside a body, separate it with `border-t border-line pt-4` on a wrapper.
export function SaveBar({ dirty, pending, blocked = false, label = "Save changes", onDiscard, status }: SaveBarProps) {
  return (
    <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2">
      <p role="status" className="mr-auto text-xs text-fg-muted">
        {dirty ? "Unsaved changes" : status}
      </p>
      <Button variant="ghost" onClick={onDiscard} disabled={!dirty || pending}>
        Discard changes
      </Button>
      <Button type="submit" variant="primary" disabled={!dirty || blocked} pending={pending}>
        {label}
      </Button>
    </div>
  );
}
