import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CloudUpload, PowerOff, RotateCw } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { ApiError, errorMessage } from "../api/client";
import { tunnelStatusQuery, useDisableTunnel, useRetryTunnel, useSaveTunnel, type TunnelStatus } from "../api/cloudflare-tunnel";
import type { Node } from "../api/types";
import { useOnline } from "../lib/hooks";
import {
  controlPhase,
  dnsPhase,
  draftFrom,
  draftKey,
  edgeState,
  imageVersion,
  inZone,
  maxTunnelHostnames,
  needsRetry,
  sameImage,
  tunnelRemains,
  validateTunnelDraft,
  workerPhase,
  type DraftErrors,
  type TunnelDraft,
} from "../lib/cloudflare-tunnel";
import { choiceClass, describedBy, inputClass, linkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

type StatusQuery = UseQueryResult<TunnelStatus>;
type Save = ReturnType<typeof useSaveTunnel>;
type Retry = ReturnType<typeof useRetryTunnel>;
type Disable = ReturnType<typeof useDisableTunnel>;

function noReply(error: unknown) {
  return error instanceof ApiError && (error.isNetwork || error.status === 502);
}

function failure(error: unknown) {
  return noReply(error) ? "No reply from the control plane. The request may still have been accepted; reload the status before trying again." : errorMessage(error);
}

export function CloudflareTunnel({ node }: { node: Node }) {
  const online = useOnline();
  const status = useQuery(tunnelStatusQuery(node.id));
  const save = useSaveTunnel(node.id);
  const retry = useRetryTunnel(node.id);
  const disable = useDisableTunnel(node.id);
  const supported = node.capabilities?.cloudflare_publishing === true;
  return (
    <Section
      title="Cloudflare Tunnel"
      description="Publish this node's public HTTP routes through a locally managed Cloudflare Tunnel connector, without opening inbound ports."
      width="form"
      flush
      actions={
        <Button variant="ghost" onClick={() => void status.refetch()} pending={status.isFetching} disabled={status.isPending}>
          {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Reload status
        </Button>
      }
    >
      <div className="space-y-2 p-4 pb-0" aria-live="polite">
        {!node.capabilities && (
          <Notice tone="warn" title="Worker capabilities unknown">
            {node.name} has not reported its capabilities, so Cloudflare Tunnel support cannot be confirmed. Verify the node before publishing. Retrying and disabling stay available.
          </Notice>
        )}
        {node.capabilities && !supported && (
          <Notice tone="warn" title="This worker cannot run a Cloudflare Tunnel connector">
            Upgrade and verify the worker before publishing. Retrying and disabling stay available.
          </Notice>
        )}
        {!online && <Notice tone="warn" title="You are offline">Publishing, retrying and disabling are paused until the connection returns.</Notice>}
      </div>
      <TunnelBody node={node} status={status} save={save} retry={retry} disable={disable} online={online} supported={supported} />
    </Section>
  );
}

interface BodyProps {
  node: Node;
  status: StatusQuery;
  save: Save;
  retry: Retry;
  disable: Disable;
  online: boolean;
  supported: boolean;
}

function TunnelBody({ node, status, save, retry, disable, online, supported }: BodyProps) {
  if (status.isPending) return <LoadingRows label="Loading Cloudflare Tunnel status" rows={3} />;
  const failed = status.isError ? (
    <div className="p-4">
      <QueryError what="the Cloudflare Tunnel status" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
    </div>
  ) : null;
  if (!status.data) return failed;
  const data = status.data;
  const saveBlocked = !online ? "You are offline." : !supported ? "The worker does not report Cloudflare Tunnel support." : data.cleanup_requested ? "Cleanup of the previous tunnel is still running. Wait for it to finish, or retry it above." : undefined;
  const formKey = JSON.stringify([data.configuration, data.default_image]);
  // A failed poll keeps the last status and the form below it, so an unsaved selection is not discarded.
  return (
    <div className="divide-y divide-line">
      {failed}
      <Requirements />
      {tunnelRemains(data) && <TunnelState node={node} status={data} retry={retry} disable={disable} online={online} />}
      {data.zones.length === 0 ? (
        <Subsection title="Publishing">
          <p className="text-[13px] text-fg-muted">
            No Cloudflare zones are imported. Import a zone with an API token on <Link to="/integrations" className={linkClass}>Integrations</Link>, then return here.
          </p>
        </Subsection>
      ) : (
        <TunnelForm key={formKey} status={data} save={save} blocked={saveBlocked} />
      )}
    </div>
  );
}

function Requirements() {
  return (
    <Subsection title="How publishing works">
      <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
        <li>
          The zone must be imported on <Link to="/integrations" className={linkClass}>Integrations</Link> with an API token that has Account Cloudflare Tunnel Edit, Zone DNS Edit and Zone Read. Zones connected through OAuth are DNS-only and cannot manage tunnels. No credentials are entered on this page.
        </li>
        <li>Cove runs a locally managed cloudflared connector on this node. Traffic arrives from the public Cloudflare edge, so each app's own authentication still applies, and direct source IP allowlists cannot be used on published routes.</li>
        <li>The connector's edge connection cannot go through Cove's outbound HTTP proxy, so publishing is refused while that proxy is configured, and a tunnel must be disabled and cleaned up before the proxy is enabled.</li>
        <li>Only exact public HTTP routes with a stored or trusted ACME certificate can be published, and each hostname must be shared with its resource's team in the zone's domain sharing. Wildcard, LAN, Tailscale and raw TCP or UDP routes are never published. TLS is verified on both local hops.</li>
        <li>Cove creates proxied CNAME records. An existing DNS record for a hostname is refused, never overwritten. Disabling affects only tunnel traffic; direct routes keep working.</li>
      </ul>
    </Subsection>
  );
}

interface StateProps {
  node: Node;
  status: TunnelStatus;
  retry: Retry;
  disable: Disable;
  online: boolean;
}

function TunnelState({ node, status, retry, disable, online }: StateProps) {
  const [confirming, setConfirming] = useState(false);
  const phase = controlPhase(status.phase);
  const canDisable = status.configuration !== null && !status.cleanup_requested;
  const offline = online ? undefined : "You are offline.";
  return (
    <Subsection
      title="Status"
      description={phase.active ? "Checked every few seconds while work is in progress." : "Checked every 30 seconds."}
      actions={
        <>
          {needsRetry(status) && (
            <Button onClick={() => retry.mutate(undefined)} pending={retry.isPending} disabled={!online || disable.isPending} title={offline}>
              {!retry.isPending && <RotateCw aria-hidden className="size-3.5" />}
              Retry
            </Button>
          )}
          {canDisable && (
            <Button variant="danger" onClick={() => setConfirming(true)} disabled={!online || retry.isPending} pending={disable.isPending} title={offline}>
              {!disable.isPending && <PowerOff aria-hidden className="size-3.5" />}
              Disable tunnel
            </Button>
          )}
        </>
      }
    >
      <StateNotices node={node} status={status} retry={retry} disable={disable} />
      <StateDetails status={status} />
      <DNSRecords status={status} />
      <ConfirmDialog
        open={confirming}
        title={`Disable the Cloudflare Tunnel on ${node.name}?`}
        confirmLabel="Disable and clean up"
        danger
        pending={disable.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => disable.mutate(undefined, { onSettled: () => setConfirming(false) })}
      >
        Cove stops the connector on this node, then removes the DNS records and tunnel it created. Hostnames published through the tunnel stop resolving through Cloudflare. Apps and direct routes keep running. Cleanup can stay blocked while the worker or Cloudflare is unreachable; it continues when you retry.
      </ConfirmDialog>
    </Subsection>
  );
}

function StateNotices({ node, status, retry, disable }: { node: Node; status: TunnelStatus; retry: Retry; disable: Disable }) {
  const blocked = controlPhase(status.phase).tone === "bad";
  const workerError = status.worker?.last_error;
  return (
    <div className="space-y-2" aria-live="polite">
      {status.cleanup_requested && (
        <Notice tone={blocked ? "error" : "info"} title={blocked ? "Cleanup is blocked" : "Cleanup requested"}>
          Cove stops the connector, then removes only the DNS records and tunnel it created. The node, the zone and its connection cannot be deleted until cleanup finishes.
        </Notice>
      )}
      {status.last_error && (
        <Notice tone="error" title="Cloudflare or the control plane reported an error">
          <p className="break-words">{status.last_error}</p>
          <p className="mt-1">Retry runs the saved settings again. It never creates a second tunnel.</p>
        </Notice>
      )}
      {status.worker_error && (
        <Notice tone="error" title={`Could not read the connector on ${node.name}`}>
          <p className="break-words">{status.worker_error}</p>
        </Notice>
      )}
      {workerError && (
        <Notice tone="warn" title="The connector reported an error">
          <p className="break-words">{workerError}</p>
        </Notice>
      )}
      <RequestOutcome retry={retry} disable={disable} />
    </div>
  );
}

function StateDetails({ status }: { status: TunnelStatus }) {
  const phase = controlPhase(status.phase);
  const edge = edgeState(status);
  const worker = status.worker;
  const connector = worker ? workerPhase(worker.phase) : undefined;
  const withheld = Object.entries(worker?.withheld ?? {});
  return (
    <>
      <DefinitionList
        items={[
          { term: "State", value: <StatusLabel tone={phase.tone} label={phase.label} pulse={phase.active} /> },
          {
            term: "Edge connection",
            value: (
              <span className="block">
                <StatusLabel tone={edge.tone} label={edge.label} />
                <span className="mt-0.5 block text-xs text-fg-muted">{edge.detail}</span>
              </span>
            ),
          },
          ...(worker && connector
            ? [
                { term: "Connector", value: <StatusLabel tone={connector.tone} label={connector.label} pulse={connector.active} /> },
                { term: "Running image", value: <span className="font-mono text-xs break-all">{worker.image || "Not reported"}</span> },
              ]
            : []),
          ...(worker?.tunnel_id ? [{ term: "Tunnel ID", value: <span className="font-mono text-xs break-all">{worker.tunnel_id}</span> }] : []),
          ...(status.next_check_at ? [{ term: "Next check", value: <RelativeTime value={status.next_check_at} /> }] : []),
        ]}
      />
      {withheld.length > 0 && (
        <Notice tone="warn" title={`${withheld.length} ${withheld.length === 1 ? "hostname is" : "hostnames are"} withheld by the worker`}>
          <ul className="mt-1 space-y-1">
            {withheld.map(([hostname, reason]) => (
              <li key={hostname} className="break-words">
                <span className="font-mono text-xs break-all text-fg">{hostname}</span>: {reason}
              </li>
            ))}
          </ul>
        </Notice>
      )}
    </>
  );
}

function RequestOutcome({ retry, disable }: { retry: Retry; disable: Disable }) {
  return (
    <>
      {retry.isError && <Notice tone="error" title="Retry was not accepted">{failure(retry.error)}</Notice>}
      {retry.isSuccess && <Notice title="Retry requested">The status above shows whether it succeeds.</Notice>}
      {disable.isError && <Notice tone="error" title="Disable was not accepted">{failure(disable.error)}</Notice>}
      {disable.isSuccess && <Notice title="Disable requested">Cleanup is not finished until the DNS records below are gone.</Notice>}
    </>
  );
}

function DNSRecords({ status }: { status: TunnelStatus }) {
  if (status.dns.length === 0) return null;
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full text-left text-[13px]">
        <caption className="sr-only">DNS records owned by this tunnel</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="px-3">
              DNS record
            </th>
            <th scope="col" className="px-3">
              State
            </th>
          </tr>
        </thead>
        <tbody>
          {status.dns.map((record) => {
            const info = dnsPhase(record.phase, record.cleanup_requested, record.last_error);
            return (
              <tr key={record.hostname} className={tableRowClass}>
                <td className="px-3 py-2 align-top">
                  <span className="font-mono text-xs break-all text-fg">{record.hostname}</span>
                  {record.last_error && <p className="mt-0.5 text-xs break-words text-bad">{record.last_error}</p>}
                </td>
                <td className="px-3 py-2 align-top">
                  <StatusLabel tone={info.tone} label={info.label} pulse={info.active} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

interface FormProps {
  status: TunnelStatus;
  save: Save;
  blocked?: string;
}

function TunnelForm({ status, save, blocked }: FormProps) {
  const id = useId();
  const formRef = useRef<HTMLFormElement>(null);
  const saved = draftFrom(status);
  const [draft, setDraft] = useState<TunnelDraft>(saved);
  const [showErrors, setShowErrors] = useState(false);
  const errors: DraftErrors = showErrors ? validateTunnelDraft(draft, status) : {};
  const existing = status.configuration;
  const dirty = existing ? draftKey(draft) !== draftKey(saved) : draft.hostnames.length > 0 || !sameImage(draft.image, saved.image);
  // Only a confirmed connection ends the "Requested" note; an accepted request alone never does.
  const settled = status.phase === "active" && !status.worker_error && status.worker?.running === true && status.worker.connected;
  const zone = status.zones.find((item) => item.id === draft.zoneId);

  function edit(next: TunnelDraft) {
    setDraft(next);
    save.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (Object.keys(validateTunnelDraft(draft, status)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    save.mutate({ zone_id: draft.zoneId, hostnames: [...draft.hostnames].sort(), image: draft.image.trim() });
  }

  function discard() {
    edit(saved);
    setShowErrors(false);
  }

  return (
    <form ref={formRef} onSubmit={submit} aria-label="Cloudflare Tunnel settings" noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <fieldset disabled={!!blocked} className="min-w-0">
        <legend className="sr-only">Cloudflare Tunnel settings</legend>
        <Subsection title="Publishing" description={existing ? "Update the published hostnames or the connector image. The zone stays fixed while a tunnel exists." : undefined}>
          <ZoneField status={status} draft={draft} error={errors.zone} locked={existing !== null} onChange={edit} />
          <HostnameChoices status={status} draft={draft} error={errors.hostnames} zoneDomain={zone?.domain} onChange={(hostnames) => edit({ ...draft, hostnames })} />
          <ImageField id={`${id}-image`} status={status} value={draft.image} error={errors.image} onChange={(image) => edit({ ...draft, image })} />
        </Subsection>
      </fieldset>
      <FormActions existing={existing !== null} save={save} blocked={blocked} dirty={dirty} settled={settled} onDiscard={discard} />
    </form>
  );
}

function ZoneField({ status, draft, error, locked, onChange }: { status: TunnelStatus; draft: TunnelDraft; error?: string; locked: boolean; onChange: (draft: TunnelDraft) => void }) {
  const id = useId();
  const tokenZones = status.zones.filter((zone) => zone.connection_kind === "token");
  return (
    <>
      {tokenZones.length === 0 && (
        <Notice tone="warn" title="No zone can manage tunnels">
          Every imported zone uses an OAuth DNS-only connection. Import a zone with an API token on <Link to="/integrations" className={linkClass}>Integrations</Link>.
        </Notice>
      )}
      <Field id={`${id}-zone`} label="Zone" error={error} hint={locked ? "Disable the tunnel and wait for cleanup before choosing another zone." : "Only zones imported with an API token can manage tunnels."}>
        <Select
          id={`${id}-zone`}
          value={draft.zoneId}
          disabled={locked}
          aria-labelledby={`${id}-zone-label`}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy(`${id}-zone`, error, true)}
          onChange={(event) => {
            const next = status.zones.find((item) => item.id === event.target.value);
            onChange({ ...draft, zoneId: event.target.value, hostnames: draft.hostnames.filter((hostname) => inZone(hostname, next)) });
          }}
        >
          <option value="" disabled>
            Choose a zone
          </option>
          {status.zones.map((item) => (
            <option key={item.id} value={item.id} disabled={item.connection_kind !== "token"}>
              {item.connection_kind === "token" ? item.domain : `${item.domain} (OAuth, DNS only)`}
            </option>
          ))}
        </Select>
      </Field>
    </>
  );
}

function formStatus(save: Save, dirty: boolean, existing: boolean, settled: boolean, blocked?: string) {
  if (save.isPending) return "Sending";
  if (blocked) return blocked;
  if (dirty) return "Unsaved changes";
  if (save.isSuccess && existing && !settled) return "Requested. Follow the status above; it is not published until the edge connection is confirmed.";
  return "No unsaved changes";
}

function FormActions({ existing, save, blocked, dirty, settled, onDiscard }: { existing: boolean; save: Save; blocked?: string; dirty: boolean; settled: boolean; onDiscard: () => void }) {
  return (
    <>
      {save.isError && (
        <div className="px-4 pb-4">
          <Notice tone="error" title={existing ? "The changes were not saved" : "Publishing was not started"}>
            <p className="break-words">{failure(save.error)}</p>
            <p className="mt-1">Your selections are kept.</p>
          </Notice>
        </div>
      )}
      <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2 border-t border-line px-4 py-3">
        <p role="status" className="mr-auto text-xs text-fg-muted">
          {formStatus(save, dirty, existing, settled, blocked)}
        </p>
        <Button variant="ghost" onClick={onDiscard} disabled={!dirty || save.isPending}>
          Discard changes
        </Button>
        <Button type="submit" variant="primary" disabled={!dirty || !!blocked} pending={save.isPending}>
          {!save.isPending && <CloudUpload aria-hidden className="size-3.5" />}
          {existing ? "Save changes" : "Publish through Cloudflare"}
        </Button>
      </div>
    </>
  );
}

interface HostnameProps {
  status: TunnelStatus;
  draft: TunnelDraft;
  error?: string;
  zoneDomain?: string;
  onChange: (hostnames: string[]) => void;
}

function HostnameChoices({ status, draft, error, zoneDomain, onChange }: HostnameProps) {
  const id = useId();
  const zone = status.zones.find((item) => item.id === draft.zoneId);
  const eligible = new Set(status.available_hostnames.filter((hostname) => inZone(hostname, zone)));
  // Saved hostnames that are no longer eligible stay listed so they can be removed.
  const choices = [...new Set([...eligible, ...draft.hostnames])].sort();
  const selected = new Set(draft.hostnames);
  const toggle = (hostname: string, checked: boolean) => onChange(checked ? [...draft.hostnames, hostname] : draft.hostnames.filter((item) => item !== hostname));
  return (
    <fieldset className="min-w-0 space-y-1.5" aria-describedby={error ? `${id}-error` : `${id}-hint`} aria-invalid={error ? true : undefined}>
      <legend className="text-[13px] font-medium text-fg">Hostnames</legend>
      {!zone ? (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          Choose a zone to see its eligible hostnames.
        </p>
      ) : choices.length === 0 ? (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          No public HTTP routes on this node in {zoneDomain} can be published. A route needs an exact hostname shared with its resource's team under the zone's domain sharing on <Link to="/integrations" className={linkClass}>Integrations</Link>, a stored or trusted ACME certificate, no direct source IP allowlist and not the proxy default certificate.
        </p>
      ) : (
        <>
          <p id={`${id}-hint`} className="text-xs text-fg-muted">
            {selected.size} of {choices.length} selected, at most {maxTunnelHostnames}. Each hostname must stay shared with its resource's team under the zone's domain sharing on <Link to="/integrations" className={linkClass}>Integrations</Link>. Eligibility is checked again when you save.
          </p>
          <ul className="grid max-h-72 gap-1.5 overflow-y-auto sm:grid-cols-2" aria-label="Hostnames">
            {choices.map((hostname) => (
              <li key={hostname} className="min-w-0">
                <label className={choiceClass}>
                  <Checkbox checked={selected.has(hostname)} onChange={(event) => toggle(hostname, event.target.checked)} />
                  <span className="min-w-0">
                    <span className="block font-mono text-xs break-all">{hostname}</span>
                    {!eligible.has(hostname) && <span className="block text-xs text-warn">No longer eligible; clear it to stop publishing</span>}
                  </span>
                </label>
              </li>
            ))}
          </ul>
        </>
      )}
      {error && (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      )}
    </fieldset>
  );
}

function ImageField({ id, status, value, error, onChange }: { id: string; status: TunnelStatus; value: string; error?: string; onChange: (image: string) => void }) {
  const isDefault = sameImage(value, status.default_image);
  const version = imageVersion(status.default_image);
  return (
    <Field
      id={id}
      label="Connector image"
      error={error}
      hint={
        isDefault
          ? `Cove's pinned official image${version ? ` (${version})` : ""}. New Cove releases may move this default; a saved tunnel keeps its image until you change it.`
          : "An administrator override, pinned to a release and digest. Cove releases never change it; choose the default to use the current release pin."
      }
    >
      <div className="flex min-w-0 flex-wrap gap-2">
        <input
          id={id}
          className={`${inputClass} flex-1 basis-64 font-mono text-xs`}
          value={value}
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="none"
          onChange={(event) => onChange(event.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy(id, error, true)}
        />
        {!isDefault && (
          <Button variant="ghost" onClick={() => onChange(status.default_image)}>
            Use Cove default
          </Button>
        )}
      </div>
    </Field>
  );
}
