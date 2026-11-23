import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ListPlus, RotateCw, Send } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useCreateCertificateIssuer, useRemoveCertificateIssuer, useUpdateCertificateIssuer, type CertificateIssuerUpdate } from "../api/certificates";
import { operationQuery } from "../api/queries";
import type { CertificateIssuer, CertificateZone, Route, RouteCertificate } from "../api/types";
import { hostnameList, issuerLimits, issuerPhase, issuerRetryable, issuerWorking, originValidityDays, validateIssuerDraft, validityLabel, type IssuerDraft, type IssuerDraftErrors } from "../lib/certificates";
import { formatAbsolute, shortId } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { ActionMenu } from "./ActionMenu";
import { ConfirmDialog } from "./ConfirmDialog";
import { CertificateValidity } from "./RouteCertificates";
import { Select } from "./Select";
import { OperationStateLabel, StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

interface ListState<T> {
  data: T[] | undefined;
  error: unknown;
  isPending: boolean;
  isFetching: boolean;
  refetch: () => unknown;
}

function Failure({ title, error }: { title: string; error: unknown }) {
  return (
    <Notice tone={isStatus(error, 409) ? "warn" : "error"} title={title}>
      <span className="break-words">{errorMessage(error)}</span>
    </Notice>
  );
}

export function CertificateIssuers({ workloadId, issuers, zones, certificates, routes, editable }: { workloadId: string; issuers: ListState<CertificateIssuer>; zones: ListState<CertificateZone>; certificates?: RouteCertificate[]; routes: Route[]; editable: boolean }) {
  const [removing, setRemoving] = useState<CertificateIssuer | null>(null);
  const list = issuers.data;
  return (
    <Section width="form" title="Cloudflare Origin CA" description="Request certificates from Cloudflare's Origin CA with this team's Cloudflare zone shares. Each issued certificate is stored as a version under Certificates, and an issuer can renew it." flush>
      <div className="divide-y divide-line">
        <div className="p-4">
          <Notice title="Trusted by Cloudflare, not by browsers">
            Origin CA certificates secure the connection from Cloudflare&apos;s edge to this node. Use them only for hostnames proxied through Cloudflare with the zone&apos;s SSL/TLS encryption mode set to Full (strict), which you configure in Cloudflare. Choosing a zone here does not change Cloudflare&apos;s settings, and clients that reach the node directly see a certificate they do not trust.
          </Notice>
        </div>
        <div>
          <IssuerList issuers={issuers} zones={zones.data} certificates={certificates} routes={routes} workloadId={workloadId} onRemove={editable ? setRemoving : undefined} />
          {issuers.error != null && list && (
            <div className="px-4 pb-3">
              <QueryError error={issuers.error} what="the latest certificate issuers" onRetry={() => void issuers.refetch()} retrying={issuers.isFetching} />
            </div>
          )}
        </div>
        {editable && <IssuerForm workloadId={workloadId} zones={zones} routes={routes} count={list?.length} />}
      </div>
      {editable && <RemoveIssuer workloadId={workloadId} issuer={removing} onClose={() => setRemoving(null)} />}
    </Section>
  );
}

function IssuerList({ issuers, zones, certificates, routes, workloadId, onRemove }: { issuers: ListState<CertificateIssuer>; zones?: CertificateZone[]; certificates?: RouteCertificate[]; routes: Route[]; workloadId: string; onRemove?: (issuer: CertificateIssuer) => void }) {
  const list = issuers.data;
  if (issuers.isPending) return <LoadingRows label="Loading certificate issuers" rows={2} />;
  if (!list) {
    return (
      <div className="p-4">
        <QueryError error={issuers.error} what="certificate issuers" onRetry={() => void issuers.refetch()} retrying={issuers.isFetching} />
      </div>
    );
  }
  if (list.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No Origin CA certificates have been requested for this resource.</p>;
  return (
    <ul aria-label="Certificate issuers" className="divide-y divide-line">
      {list.map((issuer) => (
        <IssuerRow key={issuer.id} issuer={issuer} zone={zones?.find((zone) => zone.zone_id === issuer.zone_id)} certificate={certificates?.find((item) => item.id === issuer.certificate_id)} routes={routes} workloadId={workloadId} onRemove={onRemove} />
      ))}
    </ul>
  );
}

function IssuerRow({ issuer, zone, certificate, routes, workloadId, onRemove }: { issuer: CertificateIssuer; zone?: CertificateZone; certificate?: RouteCertificate; routes: Route[]; workloadId: string; onRemove?: (issuer: CertificateIssuer) => void }) {
  const used = issuer.certificate_id ? [...new Set(routes.filter((route) => route.policy?.certificate_id === issuer.certificate_id).map((route) => route.domain))] : [];
  return (
    <li className="space-y-2 px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <IssuerSummary issuer={issuer} zone={zone} />
        {onRemove && <IssuerActions issuer={issuer} workloadId={workloadId} onRemove={onRemove} />}
      </div>
      <LatestVersion issuer={issuer} certificate={certificate} used={used} />
      {issuer.operation_id && <IssuerOperation operationId={issuer.operation_id} />}
      <IssuerNotes issuer={issuer} editable={onRemove !== undefined} />
    </li>
  );
}

function IssuerSummary({ issuer, zone }: { issuer: CertificateIssuer; zone?: CertificateZone }) {
  const phase = issuerPhase(issuer.phase);
  return (
    <div className="min-w-0 flex-1 basis-64 space-y-1">
      <p className="text-[13px] font-medium break-words text-fg">{issuer.name}</p>
      <StatusLabel tone={phase.tone} label={phase.label} pulse={issuerWorking(issuer.phase)} />
      <p className="text-xs break-words text-fg-muted">
        Zone <span className="font-mono break-all">{zone?.domain ?? shortId(issuer.zone_id)}</span>
        <span className="text-fg-subtle"> · </span>
        {validityLabel(issuer.validity_days)}
        <span className="text-fg-subtle"> · </span>
        {issuer.auto_renew ? "Renews automatically" : "Automatic renewal off"}
      </p>
      <p className="font-mono text-xs break-all text-fg-muted">{issuer.domains.join(", ")}</p>
    </div>
  );
}

function IssuerActions({ issuer, workloadId, onRemove }: { issuer: CertificateIssuer; workloadId: string; onRemove: (issuer: CertificateIssuer) => void }) {
  const update = useUpdateCertificateIssuer(workloadId);
  const online = useOnline();
  const offline = online ? undefined : "You are offline";
  const retrying = update.isPending && update.variables !== undefined && "retry" in update.variables.update;
  const failedRetry = update.variables !== undefined && "retry" in update.variables.update;

  function send(change: CertificateIssuerUpdate) {
    update.reset();
    update.mutate({ id: issuer.id, update: change });
  }

  return (
    <div className="flex basis-full flex-col items-end gap-2 sm:basis-auto">
      <div className="flex items-center gap-2">
        {issuerRetryable(issuer.phase) && (
          <Button size="sm" pending={retrying} disabled={!online || update.isPending} title={offline} onClick={() => send({ retry: true })} aria-label={`Retry issuer ${issuer.name}`}>
            {!retrying && <RotateCw aria-hidden className="size-3.5" />}
            Retry
          </Button>
        )}
        <ActionMenu
          label={`Actions for issuer ${issuer.name}`}
          actions={[
            { label: issuer.auto_renew ? "Turn off automatic renewal" : "Turn on automatic renewal", onSelect: () => send({ auto_renew: !issuer.auto_renew }), disabledReason: offline ?? (update.isPending ? "Saving the previous change" : undefined) },
            { label: "Remove issuer", danger: true, onSelect: () => onRemove(issuer), disabledReason: offline },
          ]}
        />
      </div>
      {update.isError && (
        <div className="w-full">
          <Failure title={failedRetry ? "Could not retry" : "Could not change automatic renewal"} error={update.error} />
        </div>
      )}
    </div>
  );
}

function IssuerNotes({ issuer, editable }: { issuer: CertificateIssuer; editable: boolean }) {
  const retryable = issuerRetryable(issuer.phase);
  return (
    <>
      {issuer.next_check_at && (
        <p className="text-xs text-fg-muted">
          Next check <time dateTime={issuer.next_check_at}>{formatAbsolute(issuer.next_check_at)}</time>
        </p>
      )}
      {!issuer.auto_renew && issuerWorking(issuer.phase) && <p className="text-xs text-fg-muted">Automatic renewal is off. The request or route update already in progress still completes.</p>}
      {issuer.last_error && (
        <Notice tone={issuer.phase === "failed" ? "error" : "warn"} title="Last error">
          <span className="break-words">{issuer.last_error}</span>
        </Notice>
      )}
      {editable && retryable && <p className="text-xs text-fg-muted">Retry first checks the saved request with Cloudflare, so it never requests a second certificate blindly. If updating routes failed, it applies the version already issued.</p>}
    </>
  );
}

// certificate_id names the latest issued version. It is what saved routes select, not proof that the proxy serves it.
function LatestVersion({ issuer, certificate, used }: { issuer: CertificateIssuer; certificate?: RouteCertificate; used: string[] }) {
  if (!issuer.certificate_id) return <p className="text-xs text-fg-muted">No certificate has been issued yet.</p>;
  return (
    <div className="space-y-1 border-l-2 border-line pl-3">
      <p className="text-xs text-fg-muted">
        Latest version <span className="text-fg">{certificate?.name ?? <span className="font-mono">{shortId(issuer.certificate_id)}</span>}</span>
        {!certificate && " (not in the certificate list)"}
      </p>
      {certificate && <CertificateValidity certificate={certificate} />}
      <p className="text-xs break-words text-fg-muted">
        {used.length > 0 ? (
          <>
            Selected by saved routes for <span className="font-mono break-all">{used.join(", ")}</span>
          </>
        ) : (
          "Not selected by any saved route. Select this version on the routes for these hostnames under Domains and save."
        )}
      </p>
      {issuer.phase === "applying" && <p className="text-xs text-fg-muted">Routes keep serving the previous version until the route update succeeds.</p>}
    </div>
  );
}

function IssuerOperation({ operationId }: { operationId: string }) {
  const operation = useQuery(operationQuery(operationId));
  return (
    <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-fg-muted">
      <Link to="/operations/$operationId" params={{ operationId }} className={linkClass}>
        Route update
      </Link>
      {operation.data ? <OperationStateLabel state={operation.data.state} /> : operation.isError ? <span>State unavailable</span> : null}
    </p>
  );
}

const emptyDraft = (): IssuerDraft => ({ name: "", zoneId: "", hostnames: "", validityDays: 365, autoRenew: true });

function inZone(domain: string, zone: CertificateZone | undefined) {
  if (!zone) return true;
  const base = domain.startsWith("*.") ? domain.slice(2) : domain;
  return base === zone.domain || base.endsWith(`.${zone.domain}`);
}

function IssuerForm({ workloadId, zones, routes, count }: { workloadId: string; zones: ListState<CertificateZone>; routes: Route[]; count?: number }) {
  const id = useId();
  const create = useCreateCertificateIssuer(workloadId);
  const online = useOnline();
  const [draft, setDraft] = useState<IssuerDraft>(emptyDraft);
  const [errors, setErrors] = useState<IssuerDraftErrors>({});
  const [requested, setRequested] = useState<string | null>(null);
  const form = useRef<HTMLFormElement>(null);
  const zoneList = zones.data;
  const zone = zoneList?.find((item) => item.zone_id === draft.zoneId);
  const full = (count ?? 0) >= issuerLimits.perResource;

  function change(patch: Partial<IssuerDraft>) {
    setDraft((current) => ({ ...current, ...patch }));
    setRequested(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next = validateIssuerDraft(draft, zone);
    setErrors(next);
    setRequested(null);
    create.reset();
    if (Object.keys(next).length > 0) {
      requestAnimationFrame(() => form.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    const name = draft.name.trim();
    create.mutate(
      { name, provider: "cloudflare_origin", zone_id: draft.zoneId, domains: hostnameList(draft.hostnames), validity_days: draft.validityDays, auto_renew: draft.autoRenew },
      {
        onSuccess: () => {
          setDraft(emptyDraft());
          setRequested(name);
        },
      },
    );
  }

  const unavailable = full || !zoneList?.length;
  return (
    <form ref={form} onSubmit={submit} noValidate aria-labelledby={`${id}-title`} className="space-y-4 p-4">
      <div>
        <h3 id={`${id}-title`} className="text-[13px] font-semibold text-fg">
          Request an Origin CA certificate
        </h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          The zone&apos;s Cloudflare connection must be an API token with Zone SSL and Certificates Edit permission. Cove&apos;s Cloudflare OAuth sign-in requests DNS access only and does not include this permission. The hostnames are reserved for this resource until the issuer is removed. The first certificate is stored as a version under Certificates but is not assigned to routes: select it on each route under Domains and save. Each resource keeps up to {issuerLimits.perResource} issuers and 32 certificate versions; delete old versions routes no longer use yourself.
        </p>
      </div>
      <IssuerFormAvailability full={full} zones={zones} />
      <fieldset disabled={create.isPending || unavailable} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-name`} label="Name" error={errors.name} hint="Shown in this list and on each version it creates, for example the zone and purpose.">
            <input id={`${id}-name`} className={inputClass} value={draft.name} onChange={(event) => change({ name: event.target.value })} autoComplete="off" aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name, true)} />
          </Field>
          <ZoneField id={`${id}-zone`} zones={zones} zone={zone} value={draft.zoneId} error={errors.zoneId} onChange={(zoneId) => change({ zoneId })} />
        </div>
        <HostnamesField id={`${id}-hosts`} value={draft.hostnames} error={errors.hostnames} routes={routes} zone={zone} onChange={(hostnames) => change({ hostnames })} />
        <Field id={`${id}-validity`} label="Validity" hint="How long each certificate is valid.">
          <Select id={`${id}-validity`} value={String(draft.validityDays)} onChange={(event) => change({ validityDays: Number(event.target.value) })} aria-describedby={`${id}-validity-hint`}>
            {originValidityDays.map((days) => (
              <option key={days} value={days}>
                {validityLabel(days)}
              </option>
            ))}
          </Select>
        </Field>
        <SwitchField
          id={`${id}-renew`}
          label="Renew automatically"
          checked={draft.autoRenew}
          onChange={(event) => change({ autoRenew: event.target.checked })}
          description="Renews while the panel is online, in the last third of the certificate's lifetime and no earlier than 30 days before it expires. Only routes still selecting the previous version are switched, through a route update. Routes keep serving while the panel is offline, but the certificate can expire."
        />
      </fieldset>
      {create.isError && <Failure title="Could not request the certificate" error={create.error} />}
      {requested && (
        <Notice tone="success" title="Certificate requested">
          {requested} is requesting a certificate. Once it shows Issued, select its version on the routes for these hostnames under Domains and save.
        </Notice>
      )}
      {!online && <Notice tone="warn" title="You are offline">Certificates can be requested when the connection returns.</Notice>}
      <div className="flex justify-end">
        <Button type="submit" pending={create.isPending} disabled={unavailable || !online}>
          {!create.isPending && <Send aria-hidden className="size-3.5" />}
          Request certificate
        </Button>
      </div>
    </form>
  );
}

function IssuerFormAvailability({ full, zones }: { full: boolean; zones: ListState<CertificateZone> }) {
  return (
    <>
      {full && <Notice tone="warn" title="Issuer limit reached">Each resource can have up to {issuerLimits.perResource} issuers. Remove one before requesting another.</Notice>}
      {zones.data?.length === 0 && (
        <Notice tone="warn" title="No Cloudflare zones are shared with this team">
          Ask the owner of the Cloudflare connection to share the zone, and the hostnames you need, with this resource&apos;s team.
        </Notice>
      )}
      {zones.error != null && <QueryError error={zones.error} what="shared Cloudflare zones" onRetry={() => void zones.refetch()} retrying={zones.isFetching} />}
    </>
  );
}

function zonePrompt(zones: ListState<CertificateZone>) {
  if (zones.isPending) return "Loading zones…";
  if (!zones.data) return "Zones could not be loaded";
  return zones.data.length === 0 ? "No shared zones" : "Choose a zone";
}

function ZoneField({ id, zones, zone, value, error, onChange }: { id: string; zones: ListState<CertificateZone>; zone?: CertificateZone; value: string; error?: string; onChange: (zoneId: string) => void }) {
  return (
    <Field id={id} label="Cloudflare zone" error={error} hint={zone ? `Shared hostnames: ${zone.domains.join(", ")}` : "Zones the Cloudflare connection owner shares with this team. Cove checks the share again before every request and renewal."}>
      <Select id={id} value={value} onChange={(event) => onChange(event.target.value)} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, error, true)}>
        <option value="" disabled>
          {zonePrompt(zones)}
        </option>
        {zones.data?.map((item) => (
          <option key={item.zone_id} value={item.zone_id}>
            {item.domain}
          </option>
        ))}
      </Select>
    </Field>
  );
}

function HostnamesField({ id, value, error, routes, zone, onChange }: { id: string; value: string; error?: string; routes: Route[]; zone?: CertificateZone; onChange: (hostnames: string) => void }) {
  const routeHosts = [...new Set(routes.map((route) => route.domain))].filter((domain) => inZone(domain, zone));
  const noHosts = zone ? `No saved route hostnames are in ${zone.domain}` : "This resource has no saved routes";
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <label htmlFor={id} className="block text-[13px] font-medium text-fg">
          Hostnames
        </label>
        <Button size="sm" variant="ghost" disabled={routeHosts.length === 0} title={routeHosts.length === 0 ? noHosts : undefined} onClick={() => onChange(routeHosts.join("\n"))}>
          <ListPlus aria-hidden className="size-3.5" />
          Use route hostnames
        </Button>
      </div>
      <textarea
        id={id}
        className={`${inputClass} font-mono`}
        rows={4}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={"app.example.com\n*.example.com"}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error, true)}
      />
      {error ? (
        <p id={`${id}-error`} className="text-xs break-words text-bad">
          {error}
        </p>
      ) : (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          One per line, up to {issuerLimits.hostnames}. A wildcard such as *.example.com covers one label below it, not example.com itself; add both if needed. Every name must be shared with this team.
        </p>
      )}
    </div>
  );
}

function RemoveIssuer({ workloadId, issuer, onClose }: { workloadId: string; issuer: CertificateIssuer | null; onClose: () => void }) {
  const remove = useRemoveCertificateIssuer(workloadId);
  function close() {
    remove.reset();
    onClose();
  }
  return (
    <ConfirmDialog
      open={issuer !== null}
      title={`Remove issuer ${issuer?.name ?? ""}?`}
      confirmLabel="Remove issuer"
      danger
      pending={remove.isPending}
      onCancel={close}
      onConfirm={() => {
        if (issuer) remove.mutate(issuer.id, { onSuccess: close });
      }}
    >
      <p>Cove stops renewing this certificate and releases its reserved hostnames.</p>
      <p className="mt-2">Nothing is revoked at Cloudflare. Stored certificate versions and the routes that select them stay as they are until you change them. A request Cloudflare already accepted may still complete.</p>
      {remove.isError && (
        <div className="mt-3">
          <Failure title="Could not remove the issuer" error={remove.error} />
        </div>
      )}
    </ConfirmDialog>
  );
}
