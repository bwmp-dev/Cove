import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { certificatesQuery } from "../api/certificates";
import { errorMessage } from "../api/client";
import type { ManagedRegistry, useSaveManagedRegistry } from "../api/managed-registries";
import { workloadQuery } from "../api/queries";
import type { RouteCertificate, Workload } from "../api/types";
import { certificateLimits, certificateSource, certificateStatus } from "../lib/certificates";
import { useNow } from "../lib/hooks";
import { UploadForm } from "./RouteCertificates";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Subsection } from "./ui";

interface Props {
  registry: ManagedRegistry;
  save: ReturnType<typeof useSaveManagedRegistry>;
  blocked?: string;
  onOutcome: (outcome: string | null) => void;
}

// The generated route keeps either Let's Encrypt ("") or a stored certificate of the registry resource.
function savedSelection(workload: Workload) {
  const policy = workload.routes?.[0]?.policy;
  return policy?.certificate_source === "uploaded" ? (policy.certificate_id ?? "") : "";
}

// Rendered for team managers only: the generated resource's own permissions are read-only, but its certificate API is
// open to team administrators.
export function ManagedRegistryCertificate({ registry, save, blocked, onOutcome }: Props) {
  const workload = useQuery(workloadQuery(registry.id));
  const certificates = useQuery(certificatesQuery(registry.id));
  return (
    <>
      <Subsection title="HTTPS certificate" description="Activation always needs trusted, authenticated HTTPS. Uploading a certificate alone does not activate the registry.">
        <p className="text-xs text-fg-muted">
          With Let&apos;s Encrypt, public DNS for <span className="font-mono break-all text-fg">{registry.hostname}</span> must point at the node and ports 80 and 443 must reach it. Otherwise upload a certificate for this hostname below, choose it here, save, and retry provisioning. Route settings of the registry are managed by Cove.
        </p>
        {/* A failed refetch keeps the last loaded data, so the form and its unsaved choice stay mounted. */}
        {workload.data && certificates.data ? (
          <CertificateForm registry={registry} saved={savedSelection(workload.data)} certificates={certificates.data} save={save} blocked={blocked} onOutcome={onOutcome} />
        ) : workload.isError || certificates.isError ? (
          <QueryError
            what="the certificate selection"
            error={workload.error ?? certificates.error}
            onRetry={() => {
              void workload.refetch();
              void certificates.refetch();
            }}
            retrying={workload.isFetching || certificates.isFetching}
          />
        ) : (
          <LoadingRows label="Loading certificate selection" rows={1} />
        )}
      </Subsection>
      {certificates.data && <UploadForm workloadId={registry.id} full={certificates.data.length >= certificateLimits.perResource} afterUpload="Choose it as the certificate above, save, and retry provisioning." />}
    </>
  );
}

function optionLabel(certificate: RouteCertificate, now: number) {
  const status = certificateStatus(certificate, now);
  const suffix = status === "expired" ? " (expired)" : status === "not_yet_valid" ? " (not yet valid)" : "";
  return `${certificate.name} · ${certificateSource(certificate.source).label}${suffix}`;
}

interface FormProps extends Props {
  saved: string;
  certificates: RouteCertificate[];
}

function CertificateForm({ registry, saved, certificates, save, blocked, onOutcome }: FormProps) {
  const id = useId();
  const now = useNow(60_000);
  const [choice, setChoice] = useState<string | null>(null);
  const selection = choice ?? saved;
  const missing = saved !== "" && !certificates.some((certificate) => certificate.id === saved);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    save.mutate({ retention: registry.retention, certificate_id: selection }, { onSuccess: () => onOutcome("Certificate selection saved. Retry provisioning to deploy it.") });
  }

  return (
    <form onSubmit={submit} aria-label="Certificate selection" className="space-y-3">
      <Field id={`${id}-certificate`} label="Certificate" hint={certificates.length === 0 ? "No certificates are stored for the registry resource yet." : "Stored certificates of the registry resource, uploaded or issued."}>
        <Select
          id={`${id}-certificate`}
          value={selection}
          disabled={save.isPending}
          aria-describedby={`${id}-certificate-hint`}
          onChange={(event) => {
            setChoice(event.target.value);
            save.reset();
            onOutcome(null);
          }}
        >
          <option value="">Let&apos;s Encrypt</option>
          {missing && <option value={saved}>Saved certificate (no longer stored)</option>}
          {certificates.map((certificate) => (
            <option key={certificate.id} value={certificate.id}>
              {optionLabel(certificate, now)}
            </option>
          ))}
        </Select>
      </Field>
      {save.isError && (
        <Notice tone="error" title="The certificate selection was not saved">
          <p className="break-words">{errorMessage(save.error)}</p>
        </Notice>
      )}
      <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2">
        {blocked && <p className="mr-auto text-xs text-fg-muted">{blocked} Saving is paused.</p>}
        <Button type="submit" pending={save.isPending} disabled={Boolean(blocked) || selection === saved}>
          Save certificate selection
        </Button>
      </div>
    </form>
  );
}
