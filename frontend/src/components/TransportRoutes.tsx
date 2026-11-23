import { Link } from "@tanstack/react-router";
import { Plus, Trash2 } from "lucide-react";
import { useId } from "react";
import type { Listener } from "../api/ingress-listeners";
import type { RouteVisibility } from "../api/types";
import { protocolLabels } from "../lib/ingress-listeners";
import { emptyTransport, transportProtocol, type TransportProtocol, type TransportRow } from "../lib/route-transports";
import { describedBy, inputClass } from "../lib/styles";
import { listenerBindings, privateRanges, routeLimits, visibilityNames } from "../lib/workload-form";
import { UpstreamRows } from "./RouteEditor";
import type { ListenerChoices } from "./RouteVisibility";
import { Select } from "./Select";
import { Button, Field, Notice, QueryError } from "./ui";

const label = "mb-1 block text-xs font-medium text-fg-muted";
const visibilityOptions: Record<RouteVisibility, string> = { public: "Public", lan: "LAN only", tailscale: "Tailscale only" };

function ProxyPageLink({ choices }: { choices: ListenerChoices }) {
  if (!choices.nodeId) return <>the node&apos;s Proxy page</>;
  return (
    <Link to="/nodes/$nodeId/proxy" params={{ nodeId: choices.nodeId }} className="rounded underline underline-offset-4 hover:text-fg">
      the Proxy page of {choices.nodeName}
    </Link>
  );
}

// Until the worker is confirmed to support raw routes, saved rows stay visible and removable but cannot be changed.
export function TransportRouteEditor({ rows, errors, choices, capacity, onChange }: { rows: TransportRow[]; errors: Record<string, string>; choices: ListenerChoices; capacity: number; onChange: (rows: TransportRow[]) => void }) {
  const id = useId();
  const supported = choices.routeProtocol === undefined ? undefined : choices.routeProtocol >= transportProtocol;
  const list = supported ? choices.list : undefined;
  function update(key: string, patch: Partial<TransportRow>) {
    onChange(rows.map((row) => (row.id === key ? { ...row, ...patch } : row)));
  }
  const listError = errors.transports;
  return (
    <section aria-labelledby={`${id}-title`} className="space-y-3">
      <div>
        <h3 id={`${id}-title`} className="text-[13px] font-semibold text-fg">
          TCP and UDP routes
        </h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          Forward raw connections from a dedicated managed listener on {choices.nodeName} to a Compose service. Bytes pass through unchanged, including TLS the application handles itself: there is no hostname, and Cove issues no certificate and creates no DNS record. Each listener carries one TCP or UDP route across all resources on the node.
        </p>
      </div>
      <TransportStatus rows={rows} choices={choices} supported={supported} list={list} />
      {rows.length > 0 && (
        <ul aria-label="TCP and UDP routes" className="space-y-3">
          {rows.map((row, index) => (
            <TransportRowFields
              key={row.id}
              id={`${id}-${index}`}
              index={index}
              row={row}
              rows={rows}
              errors={errors}
              locked={supported !== true}
              list={list}
              choices={choices}
              onChange={(patch) => update(row.id, patch)}
              onRemove={() => onChange(rows.filter((item) => item.id !== row.id))}
            />
          ))}
        </ul>
      )}
      {listError && <p className="text-xs text-bad">{listError}</p>}
      {rows.length > 0 && <TransportNotes choices={choices} />}
      <Button disabled={supported !== true || rows.length >= capacity} title={rows.length >= capacity ? `A resource can have at most ${routeLimits.routes} routes in total.` : undefined} onClick={() => onChange([...rows, emptyTransport()])}>
        <Plus aria-hidden className="size-3.5" />
        Add TCP or UDP route
      </Button>
    </section>
  );
}

function TransportStatus({ rows, choices, supported, list }: { rows: TransportRow[]; choices: ListenerChoices; supported?: boolean; list?: Listener[] }) {
  if (supported === undefined) {
    if (!choices.nodeReadFailed) return <p className="text-xs text-fg-muted">Checking whether the worker on {choices.nodeName} supports TCP and UDP routes.</p>;
    return (
      <p className="text-xs text-fg-muted">
        TCP and UDP routes can be added or changed once the worker capabilities of {choices.nodeName} can be read.
        {rows.length > 0 && " Saved routes are shown and can be removed."}
      </p>
    );
  }
  if (!supported) {
    return (
      <Notice tone={rows.length > 0 ? "warn" : "info"} title="TCP and UDP routes need a newer worker">
        The worker on {choices.nodeName} cannot apply them. Upgrade it, then verify the node again.
        {rows.length > 0 && " Saved routes are shown and can be removed, but saving is rejected while any remain."}
      </Notice>
    );
  }
  if (list === undefined) return choices.error != null ? <QueryError error={choices.error} what="route listeners" onRetry={choices.retry} retrying={choices.retrying} /> : null;
  if (list.some((item) => item.protocol === "tcp" || item.protocol === "udp")) return null;
  return (
    <p className="text-xs text-fg-muted">
      No TCP or UDP listener is configured on {choices.nodeName}. An instance administrator can add one on <ProxyPageLink choices={choices} />.
    </p>
  );
}

function matchingListeners(list: Listener[] | undefined, protocol: TransportProtocol, visibility: TransportRow["visibility"]) {
  return (list ?? []).filter((item) => item.protocol === protocol && item.visibility === visibility);
}

function listenerPrompt(row: TransportRow, list: Listener[] | undefined, choices: ListenerChoices, matching: Listener[]) {
  if ((choices.routeProtocol ?? 0) < transportProtocol) return "Listeners unavailable";
  if (list === undefined) return choices.error != null ? "Listeners could not be loaded" : "Loading listeners…";
  if (!row.visibility) return "Choose a visibility first";
  return matching.length === 0 ? "No matching listener" : "Choose a listener";
}

// A saved listener is never replaced without an edit.
function keptListener(row: TransportRow, list: Listener[] | undefined, protocol: TransportProtocol, visibility: TransportRow["visibility"]) {
  return matchingListeners(list, protocol, visibility).some((item) => item.id === row.listener) ? row.listener : "";
}

function TransportRowFields({ id, index, row, rows, errors, locked, list, choices, onChange, onRemove }: { id: string; index: number; row: TransportRow; rows: TransportRow[]; errors: Record<string, string>; locked: boolean; list?: Listener[]; choices: ListenerChoices; onChange: (patch: Partial<TransportRow>) => void; onRemove: () => void }) {
  const fields = { id, row, errors, locked, onChange };
  return (
    <li className="space-y-3 border-b border-line pb-3 last:border-b-0 last:pb-0">
      <TargetFields {...fields} index={index} list={list} onRemove={onRemove} />
      <PlacementFields {...fields} rows={rows} list={list} choices={choices} />
      <AccessFields {...fields} />
    </li>
  );
}

function TargetFields({ id, index, row, errors, locked, list, onChange, onRemove }: { id: string; row: TransportRow; errors: Record<string, string>; locked: boolean; onChange: (patch: Partial<TransportRow>) => void; index: number; list?: Listener[]; onRemove: () => void }) {
  const error = errors[row.id];
  const errorId = error ? `${id}-error` : undefined;
  return (
    <div className="space-y-1.5">
      <div className="grid grid-cols-[minmax(0,1fr)_5.5rem_auto] items-end gap-2 sm:grid-cols-[7rem_minmax(0,1fr)_6rem_auto]">
        <div className="col-span-3 sm:col-span-1">
          <label htmlFor={`${id}-protocol`} className={label}>
            Protocol
          </label>
          <Select
            id={`${id}-protocol`}
            value={row.protocol}
            disabled={locked}
            onChange={(event) => {
              const next: TransportProtocol = event.target.value === "udp" ? "udp" : "tcp";
              onChange({ protocol: next, listener: keptListener(row, list, next, row.visibility) });
            }}
          >
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </Select>
        </div>
        <div>
          <label htmlFor={`${id}-service`} className={label}>
            Compose service
          </label>
          <input id={`${id}-service`} className={`${inputClass} font-mono`} value={row.service} disabled={locked} onChange={(event) => onChange({ service: event.target.value })} placeholder="db" autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
        </div>
        <div>
          <label htmlFor={`${id}-port`} className={label}>
            Container port
          </label>
          <input id={`${id}-port`} className={`${inputClass} font-mono tabular-nums`} type="number" min={1} max={65535} value={Number.isNaN(row.port) ? "" : row.port} disabled={locked} onChange={(event) => onChange({ port: event.target.valueAsNumber })} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
        </div>
        <Button variant="ghost" className="px-2" aria-label={`Remove ${protocolLabels[row.protocol]} route ${row.listener || index + 1}`} title="Remove route" onClick={onRemove}>
          <Trash2 aria-hidden className="size-4" />
        </Button>
      </div>
      {error && (
        <p id={errorId} className="text-xs text-bad">
          {error}
        </p>
      )}
    </div>
  );
}

function PlacementFields({ id, row, rows, errors, locked, list, choices, onChange }: { id: string; row: TransportRow; errors: Record<string, string>; locked: boolean; onChange: (patch: Partial<TransportRow>) => void; rows: TransportRow[]; list?: Listener[]; choices: ListenerChoices }) {
  const visibilityError = errors[`${row.id}.visibility`];
  const listenerError = errors[`${row.id}.listener`];
  const protocol = protocolLabels[row.protocol];
  const matching = matchingListeners(list, row.protocol, row.visibility);
  const unavailable = row.listener !== "" && !matching.some((item) => item.id === row.listener) ? row.listener : "";
  const usedElsewhere = new Set(rows.filter((item) => item.id !== row.id).map((item) => item.listener));
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-visibility`} label="Visibility" error={visibilityError} hint={row.protocol === "tcp" ? "LAN and Tailscale accept connections only from those address ranges." : "For UDP, visibility selects the listener; source addresses are not checked."}>
          <Select
            id={`${id}-visibility`}
            value={row.visibility}
            disabled={locked}
            onChange={(event) => {
              const value = event.target.value;
              const next = value === "public" || value === "lan" || value === "tailscale" ? value : "";
              onChange({ visibility: next, listener: keptListener(row, list, row.protocol, next) });
            }}
            aria-invalid={visibilityError ? true : undefined}
            aria-describedby={describedBy(`${id}-visibility`, visibilityError, true)}
          >
            {row.visibility === "" && (
              <option value="" disabled>
                Choose a visibility
              </option>
            )}
            {(Object.keys(visibilityOptions) as RouteVisibility[]).map((value) => (
              <option key={value} value={value}>
                {visibilityOptions[value]}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-listener`} label="Listener" error={listenerError} hint={`${row.visibility ? `${visibilityNames[row.visibility]} ` : ""}${protocol} listeners of the node.`}>
          <Select id={`${id}-listener`} value={row.listener} disabled={locked || list === undefined || row.visibility === ""} onChange={(event) => onChange({ listener: event.target.value })} aria-invalid={listenerError ? true : undefined} aria-describedby={describedBy(`${id}-listener`, listenerError, true)}>
            {row.listener === "" && (
              <option value="" disabled>
                {listenerPrompt(row, list, choices, matching)}
              </option>
            )}
            {unavailable && <option value={unavailable}>{`${unavailable} (${list ? "unavailable" : "saved"})`}</option>}
            {matching.map((item) => (
              <option key={item.id} value={item.id} disabled={usedElsewhere.has(item.id)}>
                {`${item.id} · ${listenerBindings(item)}${usedElsewhere.has(item.id) ? " (used by another route here)" : ""}`}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      <ListenerStatus row={row} list={list} selected={matching.find((item) => item.id === row.listener)} choices={choices} />
    </>
  );
}

function AccessFields({ id, row, errors, locked, onChange }: { id: string; row: TransportRow; errors: Record<string, string>; locked: boolean; onChange: (patch: Partial<TransportRow>) => void }) {
  const cidrError = errors[`${row.id}.cidr`];
  return (
    <>
      {row.protocol === "tcp" ? (
        <Field id={`${id}-cidr`} label="Allowed source IP ranges" optional error={cidrError} hint={`IPv4 or IPv6 CIDR ranges, one per line, up to ${routeLimits.cidrs}. Connections from other addresses are refused. The proxy checks the connecting address.`}>
          <textarea id={`${id}-cidr`} className={`${inputClass} font-mono`} rows={2} value={row.ipAllowList} disabled={locked} onChange={(event) => onChange({ ipAllowList: event.target.value })} placeholder={"192.168.0.0/16"} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={cidrError ? true : undefined} aria-describedby={describedBy(`${id}-cidr`, cidrError, true)} />
        </Field>
      ) : (
        <p className="text-xs text-fg-muted">UDP routes have no source IP allowlist, authentication or TLS. The listener&apos;s address binding is the only restriction Cove applies; firewall rules and Tailscale ACLs are not changed.</p>
      )}
      {row.protocol === "tcp" && (row.visibility === "lan" || row.visibility === "tailscale") && <p className="text-xs text-fg-muted">Only connections from {privateRanges[row.visibility]} are accepted. Allowed source IP ranges apply on top. Firewall rules and Tailscale ACLs are not changed.</p>}
      {locked ? (
        row.upstreams.length > 0 && <p className="text-xs text-fg-muted">Also forwards to {row.upstreams.map((target) => `${target.service}:${target.port}`).join(", ")}.</p>
      ) : (
        <UpstreamRows id={`${id}-upstreams`} route={row} rows={row.upstreams} errors={errors} onChange={(upstreams) => onChange({ upstreams })} />
      )}
    </>
  );
}

function ListenerStatus({ row, list, selected, choices }: { row: TransportRow; list?: Listener[]; selected?: Listener; choices: ListenerChoices }) {
  if (!list || row.listener === "") return null;
  if (selected) {
    return (
      <p className="text-xs text-fg-muted">
        Clients connect to <span className="font-mono break-all text-fg">{listenerBindings(selected)}</span> on {choices.nodeName}.
      </p>
    );
  }
  const visibility = row.visibility ? `${visibilityNames[row.visibility]} ` : "";
  return (
    <p className="text-xs text-warn">
      Listener <span className="font-mono">{row.listener}</span> is not a {visibility}
      {protocolLabels[row.protocol]} listener on this node. Choose another listener; saving it unchanged is rejected.
    </p>
  );
}

function TransportNotes({ choices }: { choices: ListenerChoices }) {
  return (
    <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
      <li>
        Saving routes never creates listeners. An instance administrator manages them on <ProxyPageLink choices={choices} />; changing listeners recreates the shared proxy and interrupts all traffic on the node.
      </li>
      <li>While this resource has a TCP or UDP route, deploys replace containers with a guarded Compose replacement instead of a parallel, blue-green or canary rollout. Promote or abort a staged rollout before adding one.</li>
      <li>Idle sleep and CPU autoscaling cannot be enabled while the resource has TCP or UDP routes; they need HTTP-only routes.</li>
      <li>When the resource is stopped or paused, new connections are not forwarded and no maintenance page is shown. Established TCP connections and UDP sessions can continue until the service stops or the session expires.</li>
    </ul>
  );
}
