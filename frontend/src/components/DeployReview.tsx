import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { workloadConfigurationDiffQuery, type WorkloadConfigurationDiff } from "../api/project-configuration";
import { errorMessage } from "../api/client";
import { shortRevision } from "../lib/project-configuration";
import { linkClass } from "../lib/styles";
import { FieldChanges } from "./ConfigurationChanges";
import { Button, Notice, Spinner } from "./ui";

interface Props {
  workloadId: string;
  confirmLabel: string;
  onCancel: () => void;
  onConfirm: () => void;
  // Extra inputs for the deploy request, such as a reason, shown between the review and the actions.
  children?: ReactNode;
  pending?: boolean;
  confirmDisabled?: boolean;
}

// Render inside an open Dialog: the diff is read when the review opens and dropped when it closes.
export function DeployReview({ workloadId, confirmLabel, onCancel, onConfirm, children, pending = false, confirmDisabled = false }: Props) {
  const diff = useQuery(workloadConfigurationDiffQuery(workloadId));
  return (
    <div className="space-y-4">
      <ReviewBody diff={diff} />
      {children}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onCancel} disabled={pending}>
          Cancel
        </Button>
        <Button variant="primary" onClick={onConfirm} disabled={diff.isPending || confirmDisabled} pending={pending}>
          {confirmLabel}
        </Button>
      </div>
    </div>
  );
}

// What the next deploy changes compared with the declaration the last deployment used.
function ReviewBody({ diff }: { diff: UseQueryResult<WorkloadConfigurationDiff> }) {
  if (diff.isPending) {
    return (
      <p role="status" className="flex items-center gap-2 text-[13px] text-fg-muted">
        <Spinner />
        Loading pending configuration changes
      </p>
    );
  }
  if (diff.isError) {
    return (
      <Notice tone="warn" title="Pending changes could not be loaded">
        {errorMessage(diff.error)} You can still deploy, but review the configuration first if you are unsure what changed.
      </Notice>
    );
  }
  const review = diff.data;
  if (!review.has_baseline) {
    return (
      <Notice title="Changes since the last deployment are unknown">
        There is no recorded configuration from a previous deployment to compare with. This is expected for new resources and for deployments made before Cove recorded them.
      </Notice>
    );
  }
  const fields = review.fields ?? [];
  return (
    <div className="space-y-3">
      <p className="text-xs text-fg-muted">
        Compared with{" "}
        <Link to="/operations/$operationId" params={{ operationId: review.operation_id }} className={linkClass}>
          the last deployment
        </Link>
        {review.revision && (
          <>
            {" "}at{" "}
            <span className="font-mono text-fg" title={review.revision}>
              {shortRevision(review.revision)}
            </span>
          </>
        )}
        .
      </p>
      {fields.length === 0 ? <p className="text-[13px] text-fg">No configuration changes. The deploy uses the same declaration.</p> : <FieldChanges fields={review.fields} diffs={review.diffs} />}
    </div>
  );
}
