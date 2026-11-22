import { useMutation } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleAlert, CircleCheck, CircleX } from "lucide-react";
import { useState, type ReactNode } from "react";
import { fetchCapacity, type CapacityCheck, type CapacityReport, type CheckState } from "../api/advice";
import { errorMessage, isStatus } from "../api/client";
import { linkClass } from "../lib/styles";
import { Dialog } from "./ConfirmDialog";
import { Button, Notice, Spinner } from "./ui";

const checkStates: Record<CheckState, { label: string; icon: typeof CircleCheck; className: string }> = {
  pass: { label: "Passed", icon: CircleCheck, className: "text-ok" },
  warning: { label: "Review", icon: CircleAlert, className: "text-warn" },
  error: { label: "Failed", icon: CircleX, className: "text-bad" },
};

export function CapacityChecks({ checks }: { checks: CapacityCheck[] }) {
  if (checks.length === 0) return <p className="text-[13px] text-fg-muted">The worker returned no capacity checks.</p>;
  return (
    <ul className="divide-y divide-line">
      {checks.map((check) => {
        const state = checkStates[check.state] ?? checkStates.warning;
        const Icon = state.icon;
        return (
          <li key={`${check.name}:${check.state}:${check.message}`} className="flex gap-2 py-2.5">
            <Icon aria-hidden className={`mt-0.5 size-3.5 shrink-0 ${state.className}`} />
            <div className="min-w-0">
              <p className="text-[13px] text-fg">
                <span className="font-medium">{check.name}</span> <span className={state.className}>{state.label}</span>
              </p>
              <p className="text-xs break-words text-fg-muted">{check.message}</p>
            </div>
          </li>
        );
      })}
    </ul>
  );
}

function allPassed(report: CapacityReport) {
  return report.checks.length > 0 && report.checks.every((check) => check.state === "pass");
}

function unavailableReason(error: unknown) {
  if (isStatus(error, 409)) return `${errorMessage(error)}. The worker may predate capacity checks, the node may not be ready, or the resource's variables or repository need attention.`;
  if (isStatus(error, 502)) return "The worker could not inspect capacity. It may be busy or unreachable.";
  return errorMessage(error);
}

const limitsNote = "Checks compare configured CPU and memory limits with what the node has available now. They are not a guarantee; services without limits can use more.";

// Every manual deploy goes through this check. Passing checks queue the deploy straight away; warnings, failures and
// an unavailable check need an explicit choice. Nothing here changes limits or blocks the deploy on its own.
export function useCapacityGate({ workloadId, actionLabel, onDeploy }: { workloadId: string; actionLabel: string; onDeploy: () => void }) {
  const [open, setOpen] = useState(false);
  const verification = useMutation({ mutationFn: () => fetchCapacity(workloadId), gcTime: 0 });

  const close = () => {
    setOpen(false);
    // Resetting detaches any check still running, so its result cannot queue a deploy after Cancel.
    verification.reset();
  };
  const proceed = () => {
    setOpen(false);
    verification.reset();
    onDeploy();
  };
  const run = () =>
    verification.mutate(undefined, {
      onSuccess: (report) => {
        if (allPassed(report)) proceed();
      },
    });
  const start = () => {
    setOpen(true);
    run();
  };

  const report = verification.data;
  const failed = report?.checks.some((item) => item.state === "error") ?? false;
  let body: ReactNode;
  let footer: ReactNode;
  if (verification.isError) {
    body = (
      <div className="space-y-3">
        <Notice tone="warn" title="Capacity check unavailable">
          {unavailableReason(verification.error)}
        </Notice>
        <p>You can retry the check or deploy without it. The worker still reports capacity warnings in the deployment output.</p>
      </div>
    );
    footer = (
      <>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button onClick={run}>Retry check</Button>
        <Button variant="primary" onClick={proceed}>
          {actionLabel} without check
        </Button>
      </>
    );
  } else if (report && !allPassed(report)) {
    body = (
      <div className="space-y-3">
        <p>{failed ? "Some capacity checks failed." : "Some capacity checks need review."} {limitsNote}</p>
        <div className="max-h-72 overflow-y-auto">
          <CapacityChecks checks={report.checks} />
        </div>
        {report.revision && <p className="font-mono text-xs break-all">Git revision: {report.revision}</p>}
        <p>
          <Link to="/resources/$resourceId/limits" params={{ resourceId: workloadId }} className={linkClass} onClick={close}>
            Review resource limits
          </Link>
        </p>
      </div>
    );
    footer = (
      <>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button variant={failed ? "danger" : "primary"} onClick={proceed}>
          {actionLabel} anyway
        </Button>
      </>
    );
  } else {
    body = (
      <div role="status" className="flex items-start gap-2">
        <Spinner className="mt-0.5 size-3.5 shrink-0" />
        <span>Checking the configured limits against the node&apos;s available CPU and memory. This can take up to 30 seconds when Git source must be fetched.</span>
      </div>
    );
    footer = (
      <Button variant="ghost" onClick={close}>
        Cancel
      </Button>
    );
  }

  return {
    start,
    checking: verification.isPending,
    dialog: (
      <Dialog open={open} title="Capacity before deploy" onClose={close} footer={footer}>
        {body}
      </Dialog>
    ),
  };
}

// Read-only check with no deploy attached, for the preflight page.
export function CapacityPanel({ workloadId, disabled }: { workloadId: string; disabled: boolean }) {
  const verification = useMutation({ mutationFn: () => fetchCapacity(workloadId), gcTime: 0 });
  return (
    <div className="space-y-4">
      <Button disabled={disabled} pending={verification.isPending} onClick={() => verification.mutate()}>
        Check capacity
      </Button>
      {verification.isPending && <p className="text-[13px] text-fg-muted">Comparing configured limits with the node&apos;s available resources. This can take up to 30 seconds.</p>}
      {verification.isError && (
        <Notice tone="warn" title="Capacity check unavailable">
          {unavailableReason(verification.error)}
        </Notice>
      )}
      {verification.data && (
        <>
          <p className="text-xs text-fg-muted">Inspection snapshot. {limitsNote}</p>
          {verification.data.revision && <p className="font-mono text-xs break-all">Git revision: {verification.data.revision}</p>}
          <CapacityChecks checks={verification.data.checks} />
        </>
      )}
    </div>
  );
}
