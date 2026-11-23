import type { RouteCertificate } from "../api/types";
import { certificateSource, certificateStatus } from "../lib/certificates";
import { shortId } from "../lib/format";
import { useNow } from "../lib/hooks";
import type { PolicyDraft, RouteRow } from "../lib/workload-form";
import { CertificateNames, CertificateValidity } from "./RouteCertificates";
import { Select } from "./Select";
import { Field, Notice, QueryError } from "./ui";

// The resource's stored certificate versions (uploaded or issued) as the Domains form sees them. supported: the node's
// worker serves stored certificates (route protocol 3); undefined while the node is unknown.
export interface CertificateChoices {
  supported: boolean | undefined;
  list: RouteCertificate[] | undefined;
  error: unknown;
  retry: () => void;
  retrying: boolean;
}

type Change = (patch: Partial<Pick<PolicyDraft, "certificateSource" | "certificateId">>) => void;

const sourceHints: Record<PolicyDraft["certificateSource"], string> = {
  "": "Requested from Let's Encrypt automatically.",
  acme: "Requested from Let's Encrypt automatically.",
  default: "Uses the certificate configured in the node's proxy settings, which may be self-signed, and stops Let's Encrypt renewing a certificate it cached for this hostname. Browsers trust it only if you provisioned one.",
  uploaded: "A certificate version stored for this resource under Certificates: one you uploaded or one issued by Cloudflare Origin CA or ACME DNS-01.",
};

// choices is undefined where the resource has no certificate storage yet, such as the create form. A saved stored or
// proxy default selection is still shown so it is never dropped silently. protocol4: the worker supports the proxy
// default choice (route protocol 4); undefined while unknown, which offers it only when already selected.
export function CertificateSourceField({ id, policy, choices, protocol4, onChange }: { id: string; policy: PolicyDraft; choices?: CertificateChoices; protocol4?: boolean; onChange: Change }) {
  const uploaded = policy.certificateSource === "uploaded";
  const proxyDefault = policy.certificateSource === "default";
  const hint = `${sourceHints[policy.certificateSource]}${!proxyDefault && protocol4 === false ? " The proxy default certificate needs a newer worker on this node." : ""}`;
  return (
    <Field id={id} label="Certificate" hint={hint}>
      <Select
        id={id}
        value={policy.certificateSource || "acme"}
        onChange={(event) => {
          const value = event.target.value;
          if (value === "uploaded") onChange({ certificateSource: "uploaded" });
          // Switching away from a saved uploaded certificate must name the new source: an omitted one keeps the saved selection.
          else onChange({ certificateSource: value === "default" ? "default" : policy.certificateSource === "" ? "" : "acme", certificateId: "" });
        }}
        aria-describedby={`${id}-hint`}
      >
        <option value="acme">Let's Encrypt</option>
        <option value="default" disabled={!proxyDefault && protocol4 !== true}>
          Proxy default certificate
        </option>
        {(choices || uploaded) && (
          <option value="uploaded" disabled={!uploaded && choices?.supported !== true}>
            Stored certificate
          </option>
        )}
      </Select>
    </Field>
  );
}

function optionLabel(certificate: RouteCertificate, now: number) {
  const status = certificateStatus(certificate, now);
  const suffix = status === "expired" ? " (expired)" : status === "not_yet_valid" ? " (not yet valid)" : "";
  return `${certificate.name} · ${certificateSource(certificate.source).label}${suffix}`;
}

function promptLabel(list: RouteCertificate[] | undefined, failed: boolean) {
  if (list === undefined) return failed ? "Certificates could not be loaded" : "Loading certificates…";
  return list.length === 0 ? "No stored certificates" : "Choose a certificate version";
}

function SelectedCertificate({ certificate, now }: { certificate: RouteCertificate; now: number }) {
  const status = certificateStatus(certificate, now);
  const source = certificateSource(certificate.source);
  return (
    <div className="space-y-1">
      <CertificateValidity certificate={certificate} />
      <p>
        <CertificateNames certificate={certificate} />
      </p>
      <p className="text-xs text-fg-muted">
        <span className="text-fg">{source.label}.</span> {source.trust}
      </p>
      {status === "expired" && <p className="text-xs text-warn">The proxy cannot serve an expired certificate. Choose a renewed version.</p>}
      {status === "not_yet_valid" && <p className="text-xs text-warn">The proxy cannot serve this certificate until it becomes valid.</p>}
    </div>
  );
}

function ChoicesStatus({ choices }: { choices?: CertificateChoices }) {
  if (!choices) return null;
  return (
    <>
      {choices.supported === false && (
        <Notice tone="warn" title="Stored certificates need a newer worker">
          The worker on this node cannot serve them. The saved selection is kept, but saving it to this worker is rejected. Upgrade the worker and verify the node again, or choose Let's Encrypt or the proxy default certificate to save other changes.
        </Notice>
      )}
      {choices.error != null && <QueryError error={choices.error} what="stored certificates" onRetry={choices.retry} retrying={choices.retrying} />}
    </>
  );
}

export function UploadedCertificatePicker({ id, route, policy, error, choices, onChange }: { id: string; route: RouteRow; policy: PolicyDraft; error?: string; choices?: CertificateChoices; onChange: Change }) {
  const now = useNow(60_000);
  const list = choices?.list;
  const failed = choices?.error != null;
  const selected = list?.find((item) => item.id === policy.certificateId);
  const saved = policy.certificateId !== "" && !selected ? policy.certificateId : "";
  const host = route.domain.trim() || "this hostname";
  const wildcard = host.startsWith("*.");
  return (
    <div className="space-y-3">
      <ChoicesStatus choices={choices} />
      <Field
        id={id}
        label="Certificate version"
        error={error}
        hint={`${wildcard ? `The certificate must name ${host} itself. ` : ""}Every DNS name in the certificate, including its common name, must be a hostname this resource routes, and every path on ${host} uses the same certificate. Certificates with IP addresses are not accepted. Saving checks this again.`}
      >
        <Select id={id} value={policy.certificateId} disabled={choices?.supported !== true || list === undefined} onChange={(event) => onChange({ certificateId: event.target.value })} aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : `${id}-hint`}>
          {policy.certificateId === "" && (
            <option value="" disabled>
              {promptLabel(list, failed)}
            </option>
          )}
          {saved && <option value={saved}>{`${list ? "Unknown" : "Saved"} certificate ${shortId(saved)}`}</option>}
          {list?.map((certificate) => (
            <option key={certificate.id} value={certificate.id}>
              {optionLabel(certificate, now)}
            </option>
          ))}
        </Select>
      </Field>
      {selected && <SelectedCertificate certificate={selected} now={now} />}
      {saved && list && (
        <p className="text-xs text-warn">
          Certificate <span className="font-mono">{saved}</span> is not among this resource's stored certificates. Choose another version or another certificate source.
        </p>
      )}
      <p className="text-xs text-fg-muted">
        Switching {host} between Let&apos;s Encrypt and a stored certificate or the proxy default can require a brief restart of the node&apos;s shared proxy, which interrupts connections to every resource on the node. Replacing one stored version with another does not need a restart.
      </p>
      {list?.length === 0 && <p className="text-xs text-fg-muted">Upload a certificate or request one from Cloudflare Origin CA below, then choose it here.</p>}
    </div>
  );
}
