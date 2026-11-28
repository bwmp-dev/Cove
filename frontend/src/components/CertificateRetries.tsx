import { useQuery } from "@tanstack/react-query";
import { certificateRetriesQuery } from "../api/certificates";
import type { Route } from "../api/types";
import { isPublicACMERoute } from "../lib/route-transports";
import { RelativeTime } from "./RelativeTime";
import { Notice, QueryError } from "./ui";

// Read-only on purpose: the worker paces retries to stay within the certificate authority's limits. Hostnames that are no
// longer saved as Let's Encrypt routes are ignored, and an empty list never reads as an issued certificate.
export function CertificateRetries({ workloadId, routes }: { workloadId: string; routes: Route[] | null | undefined }) {
  const hostnames = new Set((routes ?? []).filter(isPublicACMERoute).map((route) => route.domain));
  const query = useQuery({ ...certificateRetriesQuery(workloadId), enabled: hostnames.size > 0 });
  if (hostnames.size === 0) return null;
  if (query.isError) return <QueryError what="certificate retry status" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />;
  const pending = query.data?.filter((retry) => hostnames.has(retry.hostname)) ?? [];
  if (pending.length === 0) return null;
  return (
    <Notice tone="warn" title="Certificate issuance pending">
      <ul aria-label="Pending certificate retries" className="mt-1 space-y-2">
        {pending.map((retry) => (
          <li key={retry.hostname} className="space-y-0.5">
            <p className="font-mono break-all text-fg">{retry.hostname}</p>
            <p className="text-xs">
              Next automatic retry due <RelativeTime value={retry.next_attempt_at} />
            </p>
            <p className="text-xs break-words">{retry.message}</p>
          </li>
        ))}
      </ul>
      <p className="mt-2 text-xs">Retries wait while operations or maintenance are running on the node, so one can start later than shown.</p>
    </Notice>
  );
}
