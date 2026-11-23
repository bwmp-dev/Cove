import type { Operation } from "../api/types";
import { holdReason, workerResult } from "../lib/operations";
import { Notice } from "./ui";

// Policy and quota holds also fill result.error for older clients; they are waits, not worker failures.
export function OperationResultNotice({ operation }: { operation: Operation }) {
  const result = operation.result;
  const hold = holdReason(operation);
  if (hold) {
    const queued = operation.state === "queued";
    return (
      <Notice tone="warn" title={queued ? "Deployment held" : "Deployment was held"}>
        <p className="break-words">{hold}</p>
        {queued && <p>The operation stays queued and starts once this clears.</p>}
      </Notice>
    );
  }
  if (!result?.error) return null;
  return (
    <Notice tone="error" title={workerResult(operation) ? "Worker error" : "Operation error"}>
      <span className="font-mono text-xs break-words">{result.error}</span>
    </Notice>
  );
}
