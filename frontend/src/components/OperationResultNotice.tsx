import type { Operation } from "../api/types";
import { holdReason, workerResult } from "../lib/operations";
import { Notice } from "./ui";

// Recorded by the control plane when it exposed routes without confirming DNS publication. Only a string is shown.
function dnsWarning(operation: Operation): string | undefined {
  const warning = operation.result?.dns_warning;
  return typeof warning === "string" && warning ? warning : undefined;
}

function DnsWarningNotice({ warning }: { warning: string }) {
  return (
    <Notice tone="warn" title="DNS publication was not confirmed">
      <p className="break-words">{warning}</p>
      <p>DNS propagation and certificate issuance may still be pending.</p>
    </Notice>
  );
}

// Policy and quota holds also fill result.error for older clients; they are waits, not worker failures.
export function OperationResultNotice({ operation }: { operation: Operation }) {
  const result = operation.result;
  const hold = holdReason(operation);
  const warning = dnsWarning(operation);
  const queued = operation.state === "queued";
  return (
    <>
      {warning && <DnsWarningNotice warning={warning} />}
      {hold && (
        <Notice tone="warn" title={queued ? "Deployment held" : "Deployment was held"}>
          <p className="break-words">{hold}</p>
          {queued && <p>The operation stays queued and starts once this clears.</p>}
        </Notice>
      )}
      {!hold && result?.error && (
        <Notice tone="error" title={workerResult(operation) ? "Worker error" : "Operation error"}>
          <span className="font-mono text-xs break-words">{result.error}</span>
        </Notice>
      )}
    </>
  );
}
