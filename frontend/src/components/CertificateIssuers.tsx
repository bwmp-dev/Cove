import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ExternalLink, ListPlus, RotateCw, Send } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { certificateAuthorityQuery, useCreateCertificateIssuer, useRemoveCertificateIssuer, useUpdateCertificateIssuer, type CertificateIssuerUpdate } from "../api/certificates";
import { operationQuery } from "../api/queries";
import type { CertificateAuthority, CertificateIssuer, CertificateZone, Route, RouteCertificate } from "../api/types";
import { authorityTerms, hostnameList, issuerLimits, issuerPhase, issuerProviderLabel, issuerRetryable, issuerWorking, originValidityDays, validateIssuerDraft, validityLabel, type AuthorityTerms, type IssuerDraft, type IssuerDraftErrors, type IssuerProvider } from "../lib/certificates";
import { formatAbsolute, shortId } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { choiceClass, describedBy, inputClass, linkClass } from "../lib/styles";
import { ActionMenu } from "./ActionMenu";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { CertificateValidity } from "./RouteCertificates";
import { isPublicHTTPRoute } from "../lib/route-transports";
import { Select } from "./Select";
import { OperationStateLabel, StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Spinner, SwitchField } from "./ui";
import { ChoiceGroup, type Choice } from "./ChoiceGroup";

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

// The trust notices follow the issuers people can see and the provider an editor is choosing, so each provider's
// limits are stated wherever it is in use.
export function CertificateIssuers({ workloadId, issuers, zones, certificates, routes, editable }: { workloadId: string; issuers: ListState<CertificateIssuer>; zones: ListState<CertificateZone>; certificates?: RouteCertificate[]; routes: Route[]; editable: boolean }) {
  const [removing, setRemoving] = useState<CertificateIssuer | null>(null);
  const [provider, setProvider] = useState<IssuerProvider>("cloudflare_origin");
  const list = issuers.data;
  const showOrigin = (editable && provider === "cloudflare_origin") || Boolean(list?.some((issuer) => issuer.provider === "cloudflare_origin"));
  const showACME = (editable && provider === "acme_dns") || Boolean(list?.some((issuer) => issuer.provider === "acme_dns"));
  return (
    <Section width="form" title="Certificate issuers" description="Request certificates with this team's Cloudflare zone shares, from Cloudflare Origin CA or from the panel's ACME certificate authority with DNS-01 validation. Each issued certificate is stored as a version under Certificates, and an issuer can renew it." flush>
      <div className="divide-y divide-line">
        {(showOrigin || showACME) && (
          <div className="space-y-3 p-4">
            {showOrigin && (
              <Notice title="Trusted by Cloudflare, not by browsers">
                Origin CA certificates secure the connection from Cloudflare&apos;s edge to this node. Use them only for hostnames proxied through Cloudflare with the zone&apos;s SSL/TLS encryption mode set to Full (strict), which you configure in Cloudflare. Choosing a zone here does not change Cloudflare&apos;s settings, and clients that reach the node directly see a certificate they do not trust.
              </Notice>
            )}
            {showACME && (
              <Notice title="Trusted only where the panel's certificate authority is trusted">
                ACME DNS-01 certificates come from the certificate authority the panel operator configured. Browsers trust them only if they trust that authority: a public authority is widely trusted, but a private or test authority is not. Clients reach this node directly, so point each hostname&apos;s DNS at it.
              </Notice>
            )}
          </div>
        )}
        <div>
          <IssuerList issuers={issuers} zones={zones.data} certificates={certificates} routes={routes} workloadId={workloadId} onRemove={editable ? setRemoving : undefined} />
          {issuers.error != null && list && (
            <div className="px-4 pb-3">
              <QueryError error={issuers.error} what="the latest certificate issuers" onRetry={() => void issuers.refetch()} retrying={issuers.isFetching} />
            </div>
          )}
        </div>
        {editable && <IssuerForm workloadId={workloadId} zones={zones} routes={routes} count={list?.length} provider={provider} onProvider={setProvider} />}
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
  if (list.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No certificates have been requested for this resource.</p>;
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
      {issuer.provider === "acme_dns" && <AuthorityDetails issuer={issuer} />}
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
        {issuerProviderLabel(issuer.provider)}
        <span className="text-fg-subtle"> · </span>
        Zone <span className="font-mono break-all">{zone?.domain ?? shortId(issuer.zone_id)}</span>
        <span className="text-fg-subtle"> · </span>
        {issuer.validity_days > 0 ? validityLabel(issuer.validity_days) : "Validity chosen by the authority"}
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
      {editable && retryable && (
        <p className="text-xs text-fg-muted">
          {issuer.provider === "acme_dns"
            ? "Retry resumes the saved order with the certificate authority and publishes again a DNS challenge record the provider refused, so it never requests a second certificate blindly. If updating routes failed, it applies the version already issued."
            : "Retry first checks the saved request with Cloudflare, so it never requests a second certificate blindly. If updating routes failed, it applies the version already issued."}
        </p>
      )}
    </>
  );
}

// Escaped text only: the terms come from the certificate authority and are never parsed as HTML or linked as a data URI.
function TermsText({ text }: { text: string }) {
  return (
    <div role="region" aria-label="Terms of service" tabIndex={0} className="max-h-48 overflow-auto rounded-md border border-line bg-panel px-3 py-2 text-xs break-words whitespace-pre-wrap text-fg">
      {text}
    </div>
  );
}

function TermsLink({ href, children }: { href: string; children: string }) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" className={`${linkClass} inline-flex items-center gap-1`}>
      {children}
      <ExternalLink aria-hidden className="size-3.5" />
      <span className="sr-only">(opens in a new tab)</span>
    </a>
  );
}

// The authority and terms an ACME issuer was created with, read from the issuer itself, so showing them never
// contacts the authority.
function AuthorityDetails({ issuer }: { issuer: CertificateIssuer }) {
  const terms = authorityTerms(issuer.terms_url, issuer.terms_text);
  return (
    <div className="space-y-1 text-xs break-words text-fg-muted">
      {issuer.directory && (
        <p>
          Certificate authority <span className="font-mono break-all">{issuer.directory}</span>
        </p>
      )}
      <p className="flex flex-wrap items-center gap-x-2 gap-y-1">
        {issuer.terms_accepted_at ? (
          <span>
            Terms accepted <time dateTime={issuer.terms_accepted_at}>{formatAbsolute(issuer.terms_accepted_at)}</time>
          </span>
        ) : (
          <span>Terms acceptance time unavailable</span>
        )}
        {terms.kind === "link" && <TermsLink href={terms.href}>Accepted terms</TermsLink>}
        {terms.kind === "none" && <span>The authority published no terms.</span>}
        {terms.kind === "unreadable" && <span>The accepted terms cannot be shown.</span>}
      </p>
      {terms.kind === "text" && (
        <details>
          <summary className="cursor-pointer rounded hover:text-fg">Show accepted terms</summary>
          <div className="mt-1.5">
            <TermsText text={terms.text} />
          </div>
        </details>
      )}
    </div>
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

const providerChoices: Choice<IssuerProvider>[] = [
  { value: "cloudflare_origin", label: "Cloudflare Origin CA", description: "Trusted by Cloudflare's edge only. Needs an API token with SSL and Certificates Edit." },
  { value: "acme_dns", label: "ACME DNS-01", description: "From the panel's certificate authority, validated with a temporary DNS record in the zone." },
];

const termsChanged = (error: unknown) => isStatus(error, 409) && /terms/i.test(errorMessage(error));

interface Requested {
  name: string;
  provider: IssuerProvider;
}

function consentProblem(authority: UseQueryResult<CertificateAuthority>, terms: AuthorityTerms | undefined, accepted: boolean) {
  if (!authority.data) return authority.isError ? "The certificate authority could not be reached. Retry loading its terms." : "Wait for the certificate authority's terms to load.";
  if (terms?.kind === "unreadable") return "These terms cannot be shown, so they cannot be accepted here.";
  return accepted ? undefined : "Accept the certificate authority's terms to request a certificate.";
}

function ProviderRequirements({ provider }: { provider: IssuerProvider }) {
  const acme = provider === "acme_dns";
  return (
    <p className="mt-0.5 text-xs text-fg-muted">
      {acme ? (
        <>
          The zone&apos;s Cloudflare connection needs Zone Read and DNS Edit permission: an API token or Cove&apos;s Cloudflare OAuth sign-in. Cove adds a temporary _acme-challenge TXT record for each hostname, waits until it is visible, asks the certificate authority to validate it and then removes it. The hostnames are reserved for this resource until the issuer is removed. The first certificate is stored as a version under Certificates but is not assigned to routes: select it on each route under Domains and save. Each resource keeps up to {issuerLimits.perResource} issuers and 32 certificate versions; delete old versions routes no longer use yourself.
        </>
      ) : (
        <>
          The zone&apos;s Cloudflare connection must be an API token with Zone SSL and Certificates Edit permission. Cove&apos;s Cloudflare OAuth sign-in requests DNS access only and does not include this permission. The hostnames are reserved for this resource until the issuer is removed. The first certificate is stored as a version under Certificates but is not assigned to routes: select it on each route under Domains and save. Each resource keeps up to {issuerLimits.perResource} issuers and 32 certificate versions; delete old versions routes no longer use yourself.
        </>
      )}
    </p>
  );
}

function RequestOutcome({ provider, error, requested }: { provider: IssuerProvider; error: unknown; requested: Requested | null }) {
  return (
    <>
      {error != null &&
        (provider === "acme_dns" && termsChanged(error) ? (
          <Notice tone="warn" title="The certificate authority's terms changed">
            <span className="break-words">{errorMessage(error)}</span> Review the current terms above and accept them again to request the certificate.
          </Notice>
        ) : (
          <Failure title="Could not request the certificate" error={error} />
        ))}
      {requested && (
        <Notice tone="success" title="Certificate requested">
          {requested.provider === "acme_dns"
            ? `${requested.name} is requesting a certificate. Cove publishes the DNS challenge records and asks the certificate authority to validate them; follow progress in the list above and under DNS challenge records. Once it shows Issued, select its version on the routes for these hostnames under Domains and save.`
            : `${requested.name} is requesting a certificate. Once it shows Issued, select its version on the routes for these hostnames under Domains and save.`}
        </Notice>
      )}
    </>
  );
}

function IssuerForm({ workloadId, zones, routes, count, provider, onProvider }: { workloadId: string; zones: ListState<CertificateZone>; routes: Route[]; count?: number; provider: IssuerProvider; onProvider: (provider: IssuerProvider) => void }) {
  const id = useId();
  const create = useCreateCertificateIssuer(workloadId);
  const online = useOnline();
  const acme = provider === "acme_dns";
  // The authority may be a public one, so it is contacted only while ACME DNS-01 is chosen.
  const authority = useQuery({ ...certificateAuthorityQuery(workloadId), enabled: acme });
  const [draft, setDraft] = useState<IssuerDraft>(emptyDraft);
  // The exact terms identity the editor accepted. Acceptance holds only while it equals the current metadata, so terms
  // that change on a refresh are never accepted implicitly.
  const [acceptedTerms, setAcceptedTerms] = useState<string | null>(null);
  const [errors, setErrors] = useState<IssuerDraftErrors>({});
  const [requested, setRequested] = useState<Requested | null>(null);
  const form = useRef<HTMLFormElement>(null);
  const zoneList = zones.data;
  const zone = zoneList?.find((item) => item.zone_id === draft.zoneId);
  const full = (count ?? 0) >= issuerLimits.perResource;
  const metadata = authority.data;
  const terms = metadata ? authorityTerms(metadata.terms_url, metadata.terms_text) : undefined;
  const accepted = metadata !== undefined && acceptedTerms === metadata.terms_url;

  function change(patch: Partial<IssuerDraft>) {
    setDraft((current) => ({ ...current, ...patch }));
    setRequested(null);
  }

  function chooseProvider(next: IssuerProvider) {
    onProvider(next);
    setAcceptedTerms(null);
    setErrors({});
    setRequested(null);
    create.reset();
  }

  function accept(checked: boolean) {
    setAcceptedTerms(checked && metadata ? metadata.terms_url : null);
    setErrors((current) => ({ ...current, terms: undefined }));
    setRequested(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next = validateIssuerDraft(draft, zone);
    if (acme) {
      const problem = consentProblem(authority, terms, accepted);
      if (problem) next.terms = problem;
    }
    setErrors(next);
    setRequested(null);
    create.reset();
    if (Object.keys(next).length > 0) {
      requestAnimationFrame(() => form.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    const name = draft.name.trim();
    const common = { name, zone_id: draft.zoneId, domains: hostnameList(draft.hostnames), auto_renew: draft.autoRenew };
    create.mutate(acme && acceptedTerms !== null ? { ...common, provider: "acme_dns", validity_days: 0, accept_terms: true, terms_url: acceptedTerms } : { ...common, provider: "cloudflare_origin", validity_days: draft.validityDays }, {
      onSuccess: () => {
        setDraft(emptyDraft());
        setAcceptedTerms(null);
        setRequested({ name, provider });
      },
      onError: (error) => {
        if (!acme || !termsChanged(error)) return;
        setAcceptedTerms(null);
        void authority.refetch();
      },
    });
  }

  const unavailable = full || !zoneList?.length;
  return (
    <form ref={form} onSubmit={submit} noValidate aria-labelledby={`${id}-title`} className="space-y-4 p-4">
      <div>
        <h3 id={`${id}-title`} className="text-[13px] font-semibold text-fg">
          Request a certificate
        </h3>
        <ProviderRequirements provider={provider} />
      </div>
      <IssuerFormAvailability full={full} zones={zones} />
      <fieldset disabled={create.isPending || unavailable} className="min-w-0 space-y-4">
        <ChoiceGroup legend="Certificate authority" name={`${id}-provider`} value={provider} choices={providerChoices} onChange={chooseProvider} />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-name`} label="Name" error={errors.name} hint="Shown in this list and on each version it creates, for example the zone and purpose.">
            <input id={`${id}-name`} className={inputClass} value={draft.name} onChange={(event) => change({ name: event.target.value })} autoComplete="off" aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name, true)} />
          </Field>
          <ZoneField id={`${id}-zone`} zones={zones} zone={zone} value={draft.zoneId} error={errors.zoneId} onChange={(zoneId) => change({ zoneId })} />
        </div>
        <HostnamesField id={`${id}-hosts`} value={draft.hostnames} error={errors.hostnames} routes={routes} zone={zone} onChange={(hostnames) => change({ hostnames })} />
        {acme ? (
          <AuthorityConsent id={`${id}-terms`} authority={authority} terms={terms} accepted={accepted} error={errors.terms} onAccept={accept} />
        ) : (
          <Field id={`${id}-validity`} label="Validity" hint="How long each certificate is valid.">
            <Select id={`${id}-validity`} value={String(draft.validityDays)} onChange={(event) => change({ validityDays: Number(event.target.value) })} aria-describedby={`${id}-validity-hint`}>
              {originValidityDays.map((days) => (
                <option key={days} value={days}>
                  {validityLabel(days)}
                </option>
              ))}
            </Select>
          </Field>
        )}
        <SwitchField
          id={`${id}-renew`}
          label="Renew automatically"
          checked={draft.autoRenew}
          onChange={(event) => change({ autoRenew: event.target.checked })}
          description="Renews while the panel is online, in the last third of the certificate's lifetime and no earlier than 30 days before it expires. Certificates valid for less than 15 minutes renew up to 5 minutes before expiry, but not before half their lifetime has passed. Only routes still selecting the previous version are switched, through a route update. Routes keep serving while the panel is offline, but the certificate can expire."
        />
      </fieldset>
      <RequestOutcome provider={provider} error={create.error} requested={requested} />
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

// The operator's fixed authority and its current terms, with explicit consent that starts unchecked.
function AuthorityConsent({ id, authority, terms, accepted, error, onAccept }: { id: string; authority: UseQueryResult<CertificateAuthority>; terms?: AuthorityTerms; accepted: boolean; error?: string; onAccept: (checked: boolean) => void }) {
  const metadata = authority.data;
  const refresh = (
    <Button size="sm" variant="ghost" pending={authority.isFetching} onClick={() => void authority.refetch()}>
      {!authority.isFetching && <RotateCw aria-hidden className="size-3.5" />}
      Refresh terms
    </Button>
  );
  return (
    <fieldset className="min-w-0 space-y-3" aria-describedby={`${id}-validity`}>
      <legend className="sr-only">Certificate authority terms</legend>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0 flex-1 basis-56 space-y-0.5">
          <p className="text-[13px] font-medium text-fg">Certificate authority</p>
          {metadata ? <p className="font-mono text-xs break-all text-fg">{metadata.directory}</p> : null}
          <p className="text-xs text-fg-muted">Set by the panel operator for every resource. It cannot be changed here.</p>
        </div>
        {metadata && refresh}
      </div>
      <p id={`${id}-validity`} className="text-xs text-fg-muted">
        The certificate authority chooses how long each certificate is valid.
      </p>
      {authority.isPending && (
        <p className="flex items-center gap-2 text-xs text-fg-muted" role="status">
          <Spinner />
          Contacting the panel&apos;s certificate authority…
        </p>
      )}
      {authority.isError && <QueryError error={authority.error} what="the certificate authority's terms" onRetry={() => void authority.refetch()} retrying={authority.isFetching} />}
      {metadata && terms && <TermsBody terms={terms} />}
      {metadata && (
        <div className="space-y-1.5">
          <label className={choiceClass}>
            <Checkbox checked={accepted} disabled={terms?.kind === "unreadable"} onChange={(event) => onAccept(event.target.checked)} aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : `${id}-hint`} />
            <span className="min-w-0">I accept this certificate authority&apos;s terms of service</span>
          </label>
          {error ? (
            <p id={`${id}-error`} className="text-xs break-words text-bad">
              {error}
            </p>
          ) : (
            <p id={`${id}-hint`} className="text-xs text-fg-muted">
              Cove records your acceptance of exactly these terms with the issuer. If the authority changes them, you are asked again.
            </p>
          )}
        </div>
      )}
      {!metadata && error && (
        <p id={`${id}-error`} className="text-xs break-words text-bad">
          {error}
        </p>
      )}
    </fieldset>
  );
}

function TermsBody({ terms }: { terms: AuthorityTerms }) {
  if (terms.kind === "text") return <TermsText text={terms.text} />;
  if (terms.kind === "link") {
    return (
      <div className="space-y-0.5 text-xs text-fg-muted">
        <TermsLink href={terms.href}>Read the terms of service</TermsLink>
        <p className="font-mono break-all">{terms.href}</p>
      </div>
    );
  }
  if (terms.kind === "none") return <p className="text-xs text-fg-muted">This certificate authority publishes no terms of service. Confirm below that you want to use it.</p>;
  return (
    <Notice tone="warn" title="The terms cannot be shown">
      The certificate authority returned terms in a form Cove does not display. Ask the panel operator to check the configured authority.
    </Notice>
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
  // TCP and UDP routes have no hostname, and the API refuses issuer names that a private route serves.
  const routeHosts = [...new Set(routes.filter(isPublicHTTPRoute).map((route) => route.domain))].filter((domain) => inZone(domain, zone));
  const noHosts = zone ? `No saved public route hostnames are in ${zone.domain}` : "This resource has no saved public routes";
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
      {issuer?.provider === "acme_dns" ? (
        <>
          <p className="mt-2">Nothing is revoked at the certificate authority. Stored certificate versions and the routes that select them stay as they are until you change them.</p>
          <p className="mt-2">DNS challenge records Cove still owns are removed afterwards, while the panel is online and can reach Cloudflare. Removing the issuer does not clear them at once; follow them under DNS challenge records.</p>
        </>
      ) : (
        <p className="mt-2">Nothing is revoked at Cloudflare. Stored certificate versions and the routes that select them stay as they are until you change them. A request Cloudflare already accepted may still complete.</p>
      )}
      {remove.isError && (
        <div className="mt-3">
          <Failure title="Could not remove the issuer" error={remove.error} />
        </div>
      )}
    </ConfirmDialog>
  );
}
