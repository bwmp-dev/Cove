import type { JobOverlapPolicy, JobResult } from "../api/types";

export const defaultJobTimeoutSeconds = 1800;
export const minJobTimeoutSeconds = 5;
export const maxJobTimeoutSeconds = 86_400;

export const overlapPolicies: Record<JobOverlapPolicy, { label: string; description: string }> = {
  skip: {
    label: "Skip",
    description: "A tick that arrives while another operation is active for this job does not run.",
  },
  queue_one: {
    label: "Queue one",
    description: "One run waits and starts when the current operation finishes. Later ticks while it waits are merged into it.",
  },
  replace: {
    label: "Replace",
    description:
      "Cancels the current run if it is running, waits for its containers to stop, then starts a new run. A build, a queued operation or one waiting for approval is never cancelled; the new run waits for it instead.",
  },
};

// Only settings other than these defaults need a worker with the job_runs capability.
export function hasAdvancedExecution(overlap: JobOverlapPolicy, timeoutSeconds: number) {
  return overlap !== "skip" || timeoutSeconds !== defaultJobTimeoutSeconds;
}

export function isOverlapPolicy(value: string): value is JobOverlapPolicy {
  return value in overlapPolicies;
}

export function parseJobTimeout(text: string): { value: number; error?: undefined } | { value?: undefined; error: string } {
  const trimmed = text.trim();
  if (!trimmed) return { error: "Enter a timeout in seconds." };
  if (!/^\d+$/.test(trimmed)) return { error: "Use a whole number of seconds." };
  const value = Number(trimmed);
  if (value < minJobTimeoutSeconds || value > maxJobTimeoutSeconds) {
    return { error: `Use ${minJobTimeoutSeconds} to ${maxJobTimeoutSeconds.toLocaleString("en-US")} seconds.` };
  }
  return { value };
}

export function formatTimeout(seconds: number) {
  const units: [number, string][] = [[3600, "hour"], [60, "minute"], [1, "second"]];
  const parts: string[] = [];
  let rest = seconds;
  for (const [size, name] of units) {
    const count = Math.floor(rest / size);
    if (count > 0) parts.push(`${count} ${name}${count === 1 ? "" : "s"}`);
    rest -= count * size;
  }
  return parts.join(" ") || "0 seconds";
}

export function jobOutcome(job: JobResult): { exit: string; cleanup: string; logs?: string; succeeded: boolean } {
  const code = job.exit_code;
  const exit = job.timed_out
    ? `Execution timed out${code === undefined ? "." : ` (exit code ${code}).`}`
    : code === undefined
      ? "Exit code unknown."
      : `Exited with code ${code}.`;
  return {
    exit,
    cleanup: job.containers_stopped ? "All containers from this run are stopped." : "Cleanup could not be confirmed. Check the node for containers from this run that are still running.",
    logs: job.logs_unavailable ? "Output could not be collected from the worker." : undefined,
    succeeded: !job.timed_out && code === 0 && job.containers_stopped,
  };
}
