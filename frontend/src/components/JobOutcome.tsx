import type { JobResult } from "../api/types";
import { jobOutcome } from "../lib/jobs";

export function JobOutcome({ job }: { job: JobResult }) {
  const outcome = jobOutcome(job);
  return (
    <div className="space-y-0.5 text-[13px]">
      <p className={outcome.succeeded ? "text-fg" : "text-bad"}>{outcome.exit}</p>
      <p className={job.containers_stopped ? "text-fg-muted" : "text-warn"}>{outcome.cleanup}</p>
      {outcome.logs && <p className="text-warn">{outcome.logs}</p>}
    </div>
  );
}
