import { FileUp, Trash2, Upload } from "lucide-react";
import { useId, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useDeleteCertificate, useUploadCertificate } from "../api/certificates";
import type { CertificateIssuer, Route, RouteCertificate } from "../api/types";
import { certificateLimits, certificateSource, certificateStatus, certificateStatuses, validateCertificateDraft, type CertificateDraft, type CertificateDraftErrors } from "../lib/certificates";
import { formatAbsolute } from "../lib/format";
import { useNow, useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const dateFormat = new Intl.DateTimeFormat("en", { dateStyle: "medium" });

function CertificateDate({ value }: { value: string }) {
  const time = Date.parse(value);
  return (
    <time dateTime={value} title={formatAbsolute(value)} className="whitespace-nowrap">
      {Number.isFinite(time) ? dateFormat.format(time) : "an unknown date"}
    </time>
  );
}

// The validity window from the certificate itself. It says nothing about whether browsers trust the issuer.
export function CertificateValidity({ certificate }: { certificate: Pick<RouteCertificate, "not_before" | "expires_at"> }) {
  const now = useNow(60_000);
  const status = certificateStatus(certificate, now);
  const { tone, label } = certificateStatuses[status];
  return (
    <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-0.5">
      <StatusLabel tone={tone} label={label} />
      <span className="text-xs text-fg-muted">
        {status === "not_yet_valid" ? (
          <>
            Valid from <CertificateDate value={certificate.not_before} />
          </>
        ) : status === "expired" ? (
          <>
            Expired <CertificateDate value={certificate.expires_at} />
          </>
        ) : (
          <>
            Expires <CertificateDate value={certificate.expires_at} />
          </>
        )}
      </span>
    </span>
  );
}

export function CertificateNames({ certificate }: { certificate: Pick<RouteCertificate, "common_name" | "dns_names" | "ip_addresses"> }) {
  const extra = certificate.common_name && !certificate.dns_names.includes(certificate.common_name) ? [certificate.common_name] : [];
  const names = [...certificate.dns_names, ...extra, ...(certificate.ip_addresses ?? [])];
  return <span className="font-mono text-xs break-all text-fg-muted">{names.length > 0 ? names.join(", ") : "No names"}</span>;
}

interface CertificateListState {
  data: RouteCertificate[] | undefined;
  error: unknown;
  isPending: boolean;
  isFetching: boolean;
  refetch: () => unknown;
}

// Hostnames of saved routes by the certificate ID they select.
function certificateUsage(routes: Route[]) {
  const usage = new Map<string, string[]>();
  for (const route of routes) {
    const id = route.policy?.certificate_id;
    if (id) usage.set(id, [...new Set([...(usage.get(id) ?? []), route.domain])]);
  }
  return usage;
}

// Issuer names by the ID of their latest issued version.
function latestVersions(issuers: CertificateIssuer[] = []) {
  return new Map(issuers.filter((issuer) => issuer.certificate_id).map((issuer) => [issuer.certificate_id, issuer.name]));
}

// Uploading and deleting certificate versions for one resource, including versions certificate issuers created. They are
// stored for the resource only; routes use one after it is selected in the Domains form and saved. busy: an operation is
// queued or running for the resource. issuers: undefined while unknown, in which case the panel still refuses deleting
// an issuer's latest version.
export function RouteCertificates({ workloadId, certificates, routes, issuers, editable, busy }: { workloadId: string; certificates: CertificateListState; routes: Route[]; issuers?: CertificateIssuer[]; editable: boolean; busy: boolean }) {
  const [deleting, setDeleting] = useState<RouteCertificate | null>(null);
  const online = useOnline();
  const list = certificates.data;
  const usage = certificateUsage(routes);
  const latest = latestVersions(issuers);
  const blocked = busy ? "Wait for the active operation to finish" : !online ? "You are offline" : undefined;
  return (
    <Section
      width="form"
      title="Certificates"
      description="Certificate versions stored for this resource: ones you upload, such as a certificate from your own certificate authority, and ones issued by the certificate issuers below. Select a version on a route above and save to serve it."
      flush
    >
      <div className="divide-y divide-line">
        <div>
          <CertificateList certificates={certificates} usage={usage} latest={latest} blocked={blocked} onDelete={editable ? setDeleting : undefined} />
          {certificates.error != null && list && (
            <div className="px-4 pb-3">
              <QueryError error={certificates.error} what="the latest certificates" onRetry={() => void certificates.refetch()} retrying={certificates.isFetching} />
            </div>
          )}
        </div>
        {editable && <UploadForm workloadId={workloadId} full={(list?.length ?? 0) >= certificateLimits.perResource} />}
      </div>
      {editable && <DeleteCertificate workloadId={workloadId} certificate={deleting} onClose={() => setDeleting(null)} />}
    </Section>
  );
}

// onDelete is omitted for viewers. blocked: why no certificate can be deleted right now, if anything prevents it.
// latest: certificate IDs that are an issuer's latest version, by issuer name.
function CertificateList({ certificates, usage, latest, blocked, onDelete }: { certificates: CertificateListState; usage: Map<string, string[]>; latest: Map<string, string>; blocked?: string; onDelete?: (certificate: RouteCertificate) => void }) {
  const list = certificates.data;
  if (certificates.isPending) return <LoadingRows label="Loading certificates" rows={2} />;
  if (!list) {
    return (
      <div className="p-4">
        <QueryError error={certificates.error} what="certificates" onRetry={() => void certificates.refetch()} retrying={certificates.isFetching} />
      </div>
    );
  }
  if (list.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No certificates are stored for this resource.</p>;
  return (
    <ul aria-label="Stored certificates" className="divide-y divide-line">
      {list.map((certificate) => {
        const used = usage.get(certificate.id) ?? [];
        const issuer = latest.get(certificate.id);
        const reason = used.length > 0 ? "Used by saved routes" : issuer !== undefined ? "Latest version of an issuer. Remove the issuer first" : blocked;
        return <CertificateRow key={certificate.id} certificate={certificate} used={used} issuer={issuer} reason={reason} onDelete={onDelete} />;
      })}
    </ul>
  );
}

function CertificateRow({ certificate, used, issuer, reason, onDelete }: { certificate: RouteCertificate; used: string[]; issuer?: string; reason?: string; onDelete?: (certificate: RouteCertificate) => void }) {
  const reasonId = `${certificate.id}-delete-reason`;
  const source = certificateSource(certificate.source);
  return (
    <li className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-4 py-3">
      <div className="min-w-0 flex-1 basis-64 space-y-1">
        <p className="text-[13px] font-medium break-words text-fg">{certificate.name}</p>
        <p className="text-xs break-words text-fg-muted" title={source.trust}>
          {source.label}
          {issuer !== undefined && (
            <>
              <span className="text-fg-subtle"> · </span>Latest version from issuer {issuer}
            </>
          )}
        </p>
        <CertificateValidity certificate={certificate} />
        <p>
          <CertificateNames certificate={certificate} />
        </p>
        <p className="text-xs text-fg-muted">
          {used.length > 0 ? (
            <>
              Used by <span className="font-mono break-all">{used.join(", ")}</span>
            </>
          ) : (
            "Not used by saved routes"
          )}
          <span className="text-fg-subtle"> · </span>
          <span className="font-mono" title={`SHA-256 ${certificate.fingerprint}`}>
            SHA-256 {certificate.fingerprint.slice(0, 16)}…
          </span>
        </p>
      </div>
      {onDelete && (
        <div className="flex flex-col items-end gap-1">
          <Button size="sm" variant="ghost" disabled={reason !== undefined} title={reason} aria-label={`Delete certificate ${certificate.name}`} aria-describedby={reason ? reasonId : undefined} onClick={() => onDelete(certificate)}>
            <Trash2 aria-hidden className="size-3.5" />
            Delete
          </Button>
          {reason && (
            <span id={reasonId} className="text-xs text-fg-subtle">
              {reason}
            </span>
          )}
        </div>
      )}
    </li>
  );
}

const emptyDraft: CertificateDraft = { name: "", chain: "", key: "" };
const pemInput = { autoComplete: "off", autoCapitalize: "none", autoCorrect: "off", spellCheck: false } as const;

// Certificate material lives only in this component's state. It is never put in the URL, browser storage, the query
// cache or logs, and the mutation is reset as soon as its outcome is read so its variables are dropped too.
function UploadForm({ workloadId, full }: { workloadId: string; full: boolean }) {
  const id = useId();
  const upload = useUploadCertificate(workloadId);
  const online = useOnline();
  const [draft, setDraft] = useState<CertificateDraft>(emptyDraft);
  const [errors, setErrors] = useState<CertificateDraftErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [uploaded, setUploaded] = useState<string | null>(null);
  const form = useRef<HTMLFormElement>(null);

  function change(patch: Partial<CertificateDraft>) {
    setDraft((current) => ({ ...current, ...patch }));
    setUploaded(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next = validateCertificateDraft(draft);
    setErrors(next);
    setFailure(null);
    setUploaded(null);
    if (Object.keys(next).length > 0) {
      requestAnimationFrame(() => form.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    upload.mutate(
      { name: draft.name.trim(), certificate_pem: draft.chain, private_key_pem: draft.key },
      {
        onSuccess: (certificate) => {
          setDraft(emptyDraft);
          setUploaded(certificate.name);
          upload.reset();
        },
        onError: (error) => {
          setFailure(errorMessage(error));
          upload.reset();
        },
      },
    );
  }

  function clear() {
    setDraft(emptyDraft);
    setErrors({});
    setFailure(null);
    setUploaded(null);
  }

  const disabled = upload.isPending;
  return (
    <form ref={form} onSubmit={submit} noValidate aria-labelledby={`${id}-title`} className="space-y-4 p-4">
      <div>
        <h3 id={`${id}-title`} className="text-[13px] font-semibold text-fg">
          Upload a certificate
        </h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          Uploading stores the certificate for this resource only. It does not change routes or start an operation. The private key is encrypted at rest and never shown again.
        </p>
      </div>
      {full && <Notice tone="warn" title="Certificate limit reached">Each resource keeps up to {certificateLimits.perResource} certificates. Delete one that routes no longer use before uploading another.</Notice>}
      <fieldset disabled={disabled || full} className="min-w-0 space-y-4">
        <Field id={`${id}-name`} label="Name" error={errors.name} hint="Shown when choosing a certificate for a route, for example the issuer and year.">
          <input
            id={`${id}-name`}
            className={inputClass}
            value={draft.name}
            maxLength={certificateLimits.name}
            onChange={(event) => change({ name: event.target.value })}
            autoComplete="off"
            aria-invalid={errors.name ? true : undefined}
            aria-describedby={describedBy(`${id}-name`, errors.name, true)}
          />
        </Field>
        <PemField
          id={`${id}-chain`}
          label="Certificate chain"
          hint="PEM certificates in order: the server certificate first, then any intermediates. Up to 64 KiB. It must be valid now, without an IP address, and name only hostnames this resource routes."
          value={draft.chain}
          error={errors.chain}
          limit={certificateLimits.chainBytes}
          onChange={(chain) => change({ chain })}
          onFileError={(message) => setErrors((current) => ({ ...current, chain: message }))}
        />
        <PemField
          id={`${id}-key`}
          label="Private key"
          hint="The unencrypted PEM private key of the server certificate: RSA of at least 2048 bits, ECDSA of at least 256 bits, or Ed25519. Up to 16 KiB."
          value={draft.key}
          error={errors.key}
          limit={certificateLimits.keyBytes}
          onChange={(key) => change({ key })}
          onFileError={(message) => setErrors((current) => ({ ...current, key: message }))}
        />
      </fieldset>
      {failure && (
        <Notice tone="error" title="Could not upload the certificate">
          {failure}
        </Notice>
      )}
      {uploaded && (
        <Notice tone="success" title="Certificate uploaded">
          {uploaded} is stored for this resource. Select it on a route above and save to serve it.
        </Notice>
      )}
      {!online && <Notice tone="warn" title="You are offline">Certificates can be uploaded when the connection returns.</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={clear} disabled={disabled || (draft.name === "" && draft.chain === "" && draft.key === "" && !failure && Object.keys(errors).length === 0)}>
          Clear
        </Button>
        <Button type="submit" pending={upload.isPending} disabled={full || !online}>
          {!upload.isPending && <Upload aria-hidden className="size-3.5" />}
          Upload certificate
        </Button>
      </div>
    </form>
  );
}

function PemField({ id, label, hint, value, error, limit, onChange, onFileError }: { id: string; label: string; hint: string; value: string; error?: string; limit: number; onChange: (value: string) => void; onFileError: (message: string) => void }) {
  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const input = event.currentTarget;
    const file = input.files?.[0];
    // Clearing the input drops the browser's reference to the file once it has been read.
    input.value = "";
    if (!file) return;
    if (file.size > limit) {
      onFileError(`The file is larger than ${limit / 1024} KiB.`);
      return;
    }
    try {
      onChange(await file.text());
    } catch {
      onFileError("The file could not be read.");
    }
  }
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <label htmlFor={id} className="block text-[13px] font-medium text-fg">
          {label}
        </label>
        <label className="relative inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-md px-2 text-xs font-medium text-fg-muted hover:bg-raised hover:text-fg has-focus-visible:outline-2 has-focus-visible:outline-offset-2 has-focus-visible:outline-info has-disabled:cursor-not-allowed has-disabled:text-fg-disabled has-disabled:hover:bg-transparent pointer-coarse:min-h-10">
          <FileUp aria-hidden className="size-3.5" />
          Choose file
          <input type="file" aria-label={`Choose a file for the ${label.toLowerCase()}`} accept=".pem,.crt,.cer,.key,text/plain,application/x-pem-file" className="sr-only" onChange={(event) => void choose(event)} />
        </label>
      </div>
      <textarea
        id={id}
        className={`${inputClass} min-h-28 font-mono text-xs`}
        rows={5}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        {...pemInput}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
      />
      {error ? (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      ) : (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          {hint}
        </p>
      )}
    </div>
  );
}

function DeleteCertificate({ workloadId, certificate, onClose }: { workloadId: string; certificate: RouteCertificate | null; onClose: () => void }) {
  const remove = useDeleteCertificate(workloadId);
  const [failure, setFailure] = useState<string | null>(null);
  function close() {
    setFailure(null);
    remove.reset();
    onClose();
  }
  return (
    <ConfirmDialog
      open={certificate !== null}
      title={`Delete certificate ${certificate?.name ?? ""}?`}
      confirmLabel="Delete certificate"
      danger
      pending={remove.isPending}
      onCancel={close}
      onConfirm={() => {
        if (!certificate) return;
        setFailure(null);
        remove.mutate(certificate.id, {
          onSuccess: close,
          onError: (error) => setFailure(isStatus(error, 409) ? `${errorMessage(error)} The list has been refreshed.` : errorMessage(error)),
        });
      }}
    >
      <p>It can no longer be selected for routes. A certificate that saved routes use, an issuer&apos;s latest version, or any certificate while an operation is active, cannot be deleted.</p>
      <p className="mt-2">Deleting does not revoke the certificate, and it does not erase the recovery copies the worker keeps for routes that used it.</p>
      {failure && (
        <div className="mt-3">
          <Notice tone="error" title="Could not delete the certificate">
            {failure}
          </Notice>
        </div>
      )}
    </ConfirmDialog>
  );
}
