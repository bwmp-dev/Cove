import type { RouteCertificate } from "../api/types";
import { certificateStatus } from "../lib/certificates";
import { shortId } from "../lib/format";
import { useNow } from "../lib/hooks";
import type { PolicyDraft, RouteRow } from "../lib/workload-form";
import { CertificateNames, CertificateValidity } from "./RouteCertificates";
import { Select } from "./Select";
import { Field, Notice, QueryError } from "./ui";

// The resource's uploaded certificates as the Domains form sees them. supported: the node's worker serves uploaded
// certificates (route protocol 3); undefined while the node is unknown.
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
  default: "Uses the certificate configured in the node's proxy settings, which may be self-signed. Browsers trust it only if you provisioned one.",
  uploaded: "A certificate uploaded to this resource under Certificates.",
};

// choices is undefined where the resource has no certificate storage yet, such as the create form. A saved uploaded
// selection is still shown so it is never dropped silently.
export function CertificateSourceField({ id, policy, choices, onChange }: { id: string; policy: PolicyDraft; choices?: CertificateChoices; onChange: Change }) {
  const uploaded = policy.certificateSource === "uploaded";
  return (
    <Field id={id} label="Certificate" hint={sourceHints[policy.certificateSource]}>
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
        <option value="default">Proxy default certificate</option>
        {(choices || uploaded) && (
          <option value="uploaded" disabled={!uploaded && choices?.supported !== true}>
            Uploaded certificate
          </option>
        )}
      </Select>
    </Field>
  );
}

function optionLabel(certificate: RouteCertificate, now: number) {
  const status = certificateStatus(certificate, now);
  const suffix = status === "expired" ? " (expired)" : status === "not_yet_valid" ? " (not yet valid)" : "";
  return `${certificate.name}${suffix}`;
}

function promptLabel(list: RouteCertificate[] | undefined, failed: boolean) {
  if (list === undefined) return failed ? "Certificates could not be loaded" : "Loading certificates…";
  return list.length === 0 ? "No certificates uploaded" : "Choose a certificate";
}

function SelectedCertificate({ certificate, now }: { certificate: RouteCertificate; now: number }) {
  const status = certificateStatus(certificate, now);
  return (
    <div className="space-y-1">
      <CertificateValidity certificate={certificate} />
      <p>
        <CertificateNames certificate={certificate} />
      </p>
      {status === "expired" && <p className="text-xs text-warn">The proxy cannot serve an expired certificate. Upload a renewed one and choose it here.</p>}
      {status === "not_yet_valid" && <p className="text-xs text-warn">The proxy cannot serve this certificate until it becomes valid.</p>}
    </div>
  );
}

function ChoicesStatus({ choices }: { choices?: CertificateChoices }) {
  if (!choices) return null;
  return (
    <>
      {choices.supported === false && (
        <Notice tone="warn" title="Uploaded certificates need a newer worker">
          The worker on this node cannot serve them. The saved selection is kept, but saving it to this worker is rejected. Upgrade the worker and verify the node again, or choose Let's Encrypt or the proxy default certificate to save other changes.
        </Notice>
      )}
      {choices.error != null && <QueryError error={choices.error} what="uploaded certificates" onRetry={choices.retry} retrying={choices.retrying} />}
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
  return (
    <div className="space-y-3">
      <ChoicesStatus choices={choices} />
      <Field id={id} label="Uploaded certificate" error={error} hint={`Every DNS name in the certificate, including its common name, must be a hostname this resource routes, and every path on ${host} uses the same certificate. Certificates with IP addresses are not accepted. Saving checks this again.`}>
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
          Certificate <span className="font-mono">{saved}</span> is not among this resource's uploaded certificates. Choose another or another certificate source.
        </p>
      )}
      <p className="text-xs text-fg-muted">
        Switching {host} between a Let's Encrypt certificate and an uploaded one can require a brief restart of the node's shared proxy, which interrupts connections to every resource on the node. Replacing one uploaded certificate with another does not need a restart.
      </p>
      {list?.length === 0 && <p className="text-xs text-fg-muted">Upload a certificate under Certificates below, then choose it here.</p>}
    </div>
  );
}
