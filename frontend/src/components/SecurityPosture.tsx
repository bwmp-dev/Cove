import { useQuery } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { securityPostureQuery } from "../api/instance-settings";
import { StatusLabel } from "./status";
import { Button, LoadingRows, QueryError, Section } from "./ui";

export function SecurityPosture() {
  const posture = useQuery(securityPostureQuery);
  const checks = posture.data?.checks ?? [];
  // Checks that need attention come first; the API order is kept within each group.
  const ordered = [...checks.filter((check) => check.status !== "pass"), ...checks.filter((check) => check.status === "pass")];
  const passed = checks.length - checks.filter((check) => check.status !== "pass").length;
  return (
    <Section
      title="Security checklist"
      description={
        posture.isSuccess && checks.length > 0
          ? `${passed} of ${checks.length} checks passed. Read from this instance's saved settings; checks Cove cannot verify from here are listed as needing review.`
          : "Read from this instance's saved settings; checks Cove cannot verify from here are listed as needing review."
      }
      actions={
        <Button size="sm" variant="ghost" disabled={posture.isFetching} onClick={() => void posture.refetch()}>
          <RotateCw aria-hidden className="size-3.5" />
          Recheck
        </Button>
      }
      flush
    >
      {posture.isPending ? (
        <LoadingRows label="Loading security checks" rows={4} />
      ) : posture.isError ? (
        <div className="p-3">
          <QueryError what="security checks" error={posture.error} onRetry={() => void posture.refetch()} retrying={posture.isFetching} />
        </div>
      ) : ordered.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">The control plane returned no checks.</p>
      ) : (
        <ul className="divide-y divide-line" aria-busy={posture.isFetching || undefined}>
          {ordered.map((check) => (
            <li key={check.id} className="grid gap-x-6 gap-y-1 px-4 py-3 sm:grid-cols-[8rem_minmax(0,1fr)]">
              <div className="sm:pt-px">
                <StatusLabel tone={check.status === "pass" ? "ok" : "warn"} label={check.status === "pass" ? "Passed" : "Review"} />
              </div>
              <div className="min-w-0">
                <p className="text-[13px] break-words text-fg">{check.message}</p>
                {check.status !== "pass" && check.remediation && <p className="mt-0.5 text-xs break-words text-fg-muted">{check.remediation}</p>}
              </div>
            </li>
          ))}
        </ul>
      )}
    </Section>
  );
}
