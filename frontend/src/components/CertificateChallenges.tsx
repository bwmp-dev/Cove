import type { CertificateChallenge, CertificateChallengeList, CertificateIssuer } from "../api/types";
import { challengePhase, challengeWorking } from "../lib/certificates";
import { formatAbsolute, shortId } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { StatusLabel } from "./status";
import { LoadingRows, Notice, QueryError, Section } from "./ui";

export interface ChallengeListState {
  data: CertificateChallengeList | undefined;
  error: unknown;
  isPending: boolean;
  isFetching: boolean;
  refetch: () => unknown;
}

// "resource": rows of one resource, named by issuer. "zone": every row in a zone for its connection owner, named by
// resource, including rows whose resource was deleted.
type Scope = { kind: "resource"; issuers?: CertificateIssuer[] } | { kind: "zone" };

function owner(challenge: CertificateChallenge, scope: Scope) {
  if (scope.kind === "zone") {
    const resource = challenge.workload_id ? `Resource ${shortId(challenge.workload_id)}` : "Resource deleted";
    return challenge.issuer_id ? resource : `${resource} · issuer removed`;
  }
  if (!challenge.issuer_id) return "Issuer removed";
  const issuer = scope.issuers?.find((item) => item.id === challenge.issuer_id);
  return issuer ? `Issuer ${issuer.name}` : `Issuer ${shortId(challenge.issuer_id)}`;
}

// What the phase means for the record at the provider. A lost create response leaves its existence unknown, so the
// wording never claims a record exists or was removed before the panel confirms it.
function explanation(challenge: CertificateChallenge, scope: Scope) {
  if (challenge.phase === "cleanup" || challenge.cleanup_requested) return "Cleanup requested. Cove removes the record only after confirming it still owns it, while the panel is online and can reach Cloudflare.";
  switch (challenge.phase) {
    case "pending":
      return "Cove publishes this record while the panel is online and can reach Cloudflare.";
    case "presenting":
      return "Cove is confirming whether Cloudflare created this record. Ownership proof is kept until then, so the record may or may not exist.";
    case "present":
      return "Published. Cove asks the certificate authority to validate it once the record is visible in DNS.";
    case "blocked":
      return scope.kind === "resource" && challenge.issuer_id ? "Cloudflare refused this record, so none was created. Fix the cause below, then retry the issuer." : "Cloudflare refused this record, so none was created. It waits for the issuer to be retried or removed.";
    default:
      return "Cove does not recognise this state and does not treat the record as resolved.";
  }
}

function ChallengeRow({ challenge, scope }: { challenge: CertificateChallenge; scope: Scope }) {
  const phase = challengePhase(challenge.phase);
  return (
    <li className="space-y-1.5 px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1">
        <p className="min-w-0 flex-1 basis-56 font-mono text-[13px] break-all text-fg">{challenge.hostname}</p>
        <StatusLabel tone={phase.tone} label={phase.label} pulse={challengeWorking(challenge.phase)} />
      </div>
      <p className="text-xs break-words text-fg-muted">
        {owner(challenge, scope)}
        <span className="text-fg-subtle"> · </span>
        Since <time dateTime={challenge.created_at}>{formatAbsolute(challenge.created_at)}</time>
        {challenge.next_check_at && (
          <>
            <span className="text-fg-subtle"> · </span>
            Next check <time dateTime={challenge.next_check_at}>{formatAbsolute(challenge.next_check_at)}</time>
          </>
        )}
      </p>
      <p className="text-xs break-words text-fg-muted">{explanation(challenge, scope)}</p>
      {challenge.last_error && (
        <Notice tone={challenge.phase === "blocked" ? "error" : "warn"} title="Last error">
          <span className="break-words">{challenge.last_error}</span>
        </Notice>
      )}
    </li>
  );
}

// Read-only on purpose: the panel reconciles every record, and discarding ownership proof could leave a record behind.
export function ChallengeList({ challenges, scope }: { challenges: ChallengeListState; scope: Scope }) {
  const online = useOnline();
  const result = challenges.data;
  if (challenges.isPending) return <LoadingRows label="Loading DNS challenge records" rows={2} />;
  if (!result) {
    return (
      <div className="p-4">
        <QueryError error={challenges.error} what="DNS challenge records" onRetry={() => void challenges.refetch()} retrying={challenges.isFetching} />
      </div>
    );
  }
  return (
    <div className="divide-y divide-line">
      {(!online || challenges.error != null || result.more) && (
        <div className="space-y-3 p-4">
          {!online && <Notice tone="warn" title="You are offline">The records below may be out of date.</Notice>}
          {challenges.error != null && <QueryError error={challenges.error} what="the latest DNS challenge records" onRetry={() => void challenges.refetch()} retrying={challenges.isFetching} />}
          {result.more && (
            <Notice tone="warn" title={`Showing the oldest ${result.items.length} unresolved records`}>
              More exist. While a zone has 128 unresolved records, Cove admits no new DNS challenges there until cleanup frees capacity.
            </Notice>
          )}
        </div>
      )}
      {result.items.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">No outstanding DNS challenge records.</p>
      ) : (
        <ul aria-label="DNS challenge records" className="divide-y divide-line">
          {result.items.map((challenge) => (
            <ChallengeRow key={challenge.id} challenge={challenge} scope={scope} />
          ))}
        </ul>
      )}
    </div>
  );
}

export function CertificateChallenges({ challenges, issuers }: { challenges: ChallengeListState; issuers?: CertificateIssuer[] }) {
  return (
    <Section
      width="form"
      title="DNS challenge records"
      description="Temporary _acme-challenge TXT records for this resource's ACME DNS-01 issuers, including records Cove still has to remove after an issuer was removed. Cove publishes, checks and removes them automatically while the panel is online; nothing here needs action unless a record is refused."
      flush
    >
      <ChallengeList challenges={challenges} scope={{ kind: "resource", issuers }} />
    </Section>
  );
}
