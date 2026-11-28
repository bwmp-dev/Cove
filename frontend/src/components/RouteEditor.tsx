import { ChevronDown, Plus, Trash2 } from "lucide-react";
import { useId, useState } from "react";
import type { Route, RoutePolicy } from "../api/types";
import { cidrList, draftUsesExtensions, draftUsesTimeouts, emptyPolicy, isPrivateVisibility, isWildcardHostname, routeLimits, timeoutLimits, visibilitySummary, routesNeedProtocol4, routesUseExtensions, routesUsePolicies, type HeaderRow, type PolicyDraft, type RouteRow, type TimeoutDraft, type UpstreamRow } from "../lib/workload-form";
import { describedBy, inputClass } from "../lib/styles";
import { CertificateSourceField, UploadedCertificatePicker, type CertificateChoices } from "./RouteCertificatePicker";
import { RouteExtensionFields } from "./RouteExtensions";
import { RouteVisibilityFields, type ListenerChoices } from "./RouteVisibility";
import { Button, Field, Notice, SwitchField } from "./ui";

const label = "mb-1 block text-xs font-medium text-fg-muted";
const noRoutes: Route[] = [];
const noErrors: Record<string, string> = {};

// policies: whether the target node's worker reports route policy support; undefined while unknown, which hides the
// advanced controls. Saved policies are always kept, whether or not they can be edited here. extensions: whether it
// applies maintenance, error pages and forward-auth (route protocol 2); saved ones stay visible and removable when not.
// saved: the routes the API has, so removing forward-auth from one of their hostnames can be called out. certificates:
// the resource's stored certificates; omitted where none can exist yet, such as the create form. routeProtocol: the
// node worker's route protocol, undefined while unknown; 4 adds wildcard hostnames and the proxy default certificate.
// listeners: the node's managed listeners for private routes; omitted where they cannot be chosen yet, such as the
// create form. capacity: how many HTTP routes fit beside the resource's TCP and UDP routes.
export function RouteEditor({ routes, errors = noErrors, policies, extensions, routeProtocol, saved = noRoutes, certificates, listeners, capacity = routeLimits.routes, onChange }: { routes: RouteRow[]; errors?: Record<string, string>; policies?: boolean; extensions?: boolean; routeProtocol?: number; saved?: Route[]; certificates?: CertificateChoices; listeners?: ListenerChoices; capacity?: number; onChange: (routes: RouteRow[]) => void }) {
  const id = useId();
  const protocol4 = routeProtocol === undefined ? undefined : routeProtocol >= 4;
  const protocol6 = routeProtocol === undefined ? undefined : routeProtocol >= 6;
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
  function update(key: string, patch: Partial<RouteRow>) {
    onChange(routes.map((route) => (route.id === key ? { ...route, ...patch } : route)));
  }
  function toggle(route: RouteRow) {
    const next = new Set(expanded);
    if (next.has(route.id)) next.delete(route.id);
    else {
      next.add(route.id);
      if (!route.policy) update(route.id, { policy: emptyPolicy() });
    }
    setExpanded(next);
  }
  return (
    <div className="space-y-3">
      <WorkerNotices routes={routes} policies={policies} extensions={extensions} />
      <ProtocolNotices routes={routes} routeProtocol={routeProtocol} />
      {routes.length > 0 && (
        <ul aria-label="Routes" className="space-y-3">
          {routes.map((route, index) => {
            const error = errors[route.id];
            const describedById = error ? `${id}-${index}-error` : undefined;
            const policyError = Object.keys(errors).some((key) => key.startsWith(`${route.id}.`));
            const open = policies === true && (expanded.has(route.id) || policyError);
            const panel = `${id}-${index}-advanced`;
            const privacy = route.policy && visibilitySummary({ visibility: route.policy.visibility || undefined, listener: route.policy.listener }, listeners?.list);
            return (
              <li key={route.id} className="space-y-1.5 border-b border-line pb-3 last:border-b-0 last:pb-0">
                <div className="grid grid-cols-[minmax(0,1fr)_5.5rem_auto] items-end gap-2 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_6rem_auto]">
                  <div className="col-span-3 sm:col-span-1">
                    <label htmlFor={`${id}-domain-${index}`} className={label}>
                      Hostname
                    </label>
                    <input
                      id={`${id}-domain-${index}`}
                      className={`${inputClass} font-mono`}
                      value={route.domain}
                      onChange={(event) => update(route.id, { domain: event.target.value })}
                      placeholder="app.example.com"
                      inputMode="url"
                      autoComplete="off"
                      autoCapitalize="none"
                      spellCheck={false}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedById}
                    />
                  </div>
                  <div>
                    <label htmlFor={`${id}-service-${index}`} className={label}>
                      Compose service
                    </label>
                    <input
                      id={`${id}-service-${index}`}
                      className={`${inputClass} font-mono`}
                      value={route.service}
                      onChange={(event) => update(route.id, { service: event.target.value })}
                      placeholder="web"
                      autoComplete="off"
                      autoCapitalize="none"
                      spellCheck={false}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedById}
                    />
                  </div>
                  <div>
                    <label htmlFor={`${id}-port-${index}`} className={label}>
                      Container port
                    </label>
                    <input
                      id={`${id}-port-${index}`}
                      className={`${inputClass} font-mono tabular-nums`}
                      type="number"
                      min={1}
                      max={65535}
                      value={Number.isNaN(route.port) ? "" : route.port}
                      onChange={(event) => update(route.id, { port: event.target.valueAsNumber })}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedById}
                    />
                  </div>
                  <Button variant="ghost" className="px-2" aria-label={`Remove route ${route.domain || index + 1}`} title="Remove route" onClick={() => onChange(routes.filter((item) => item.id !== route.id))}>
                    <Trash2 aria-hidden className="size-4" />
                  </Button>
                </div>
                {error && (
                  <p id={describedById} className="text-xs text-bad">
                    {error}
                  </p>
                )}
                {isWildcardHostname(route.domain.trim()) && <WildcardHint domain={route.domain.trim()} />}
                {route.auth && <p className="text-xs text-fg-muted">Requires a password at the proxy. The saved credential is kept.</p>}
                {privacy && <p className="text-xs break-words text-fg-muted">{privacy}. Public HTTP and HTTPS requests for this hostname are refused.</p>}
                {policies === true && (
                  <div>
                    <button type="button" aria-expanded={open} aria-controls={panel} onClick={() => toggle(route)} className="flex max-w-full items-start gap-1.5 rounded py-1 text-left text-xs font-medium text-fg-muted hover:text-fg pointer-coarse:min-h-10">
                      <ChevronDown aria-hidden className={`mt-px size-3.5 shrink-0 transition-transform ${open ? "" : "-rotate-90"}`} />
                      <span className="min-w-0">
                        Advanced routing
                        {!open && route.policy && <span className="font-normal text-fg-subtle break-words"> · {policySummary(route.policy)}</span>}
                      </span>
                    </button>
                    {open && route.policy && (
                      <div id={panel} className="mt-2 border-l-2 border-line pl-3 sm:pl-4">
                        <PolicyFields
                          id={`${id}-${index}`}
                          route={route}
                          policy={route.policy}
                          errors={errors}
                          certificates={certificates}
                          protocol4={protocol4}
                          protocol6={protocol6}
                          listeners={listeners}
                          savedVisibility={saved.find((item) => item.domain === route.domain.trim().toLowerCase())?.policy?.visibility}
                          onChange={(patch) => update(route.id, { policy: { ...(route.policy ?? emptyPolicy()), ...patch } })}
                        />
                        {extensions !== undefined && (extensions || draftUsesExtensions(route.policy)) && (
                          <RouteExtensionFields
                            id={`${id}-${index}`}
                            route={route}
                            policy={route.policy}
                            errors={errors}
                            editable={extensions}
                            savedAuth={saved.some((item) => item.domain === route.domain.trim().toLowerCase() && item.policy?.forward_auth)}
                            onChange={(patch) => update(route.id, { policy: { ...(route.policy ?? emptyPolicy()), ...patch } })}
                          />
                        )}
                      </div>
                    )}
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <Button disabled={routes.length >= capacity} onClick={() => onChange([...routes, { id: crypto.randomUUID(), domain: "", service: "", port: 80 }])}>
        <Plus aria-hidden className="size-3.5" />
        Add route
      </Button>
    </div>
  );
}

// Explains settings the node's worker cannot apply; saved ones are kept for the control plane to judge.
function WorkerNotices({ routes, policies, extensions }: { routes: RouteRow[]; policies?: boolean; extensions?: boolean }) {
  return (
    <>
      {policies === false && (
        <Notice tone={routesUsePolicies(routes) ? "warn" : "info"} title="Advanced routing needs a newer worker">
          Path prefixes, headers, IP allowlists, redirects and extra upstreams are not available on this node yet. Upgrade the worker on the node, then verify the node again.
          {routesUsePolicies(routes) && " Existing advanced settings are kept, but saving them is rejected until the worker supports them."}
        </Notice>
      )}
      {policies === true && extensions === false && (
        <Notice tone={routesUseExtensions(routes) ? "warn" : "info"} title="Maintenance, error pages and forward-auth need a newer worker">
          The worker on this node cannot apply them. Upgrade it, then verify the node again.
          {routesUseExtensions(routes) && " Saved settings are shown under Advanced routing but cannot be changed, and saving is rejected while any remain. Remove them to save other route changes to this worker."}
        </Notice>
      )}
    </>
  );
}

// Route protocol 4 adds wildcard hostnames and the proxy default certificate, 5 private routes and 6 upstream timeouts.
function ProtocolNotices({ routes, routeProtocol }: { routes: RouteRow[]; routeProtocol?: number }) {
  if (routeProtocol === undefined) return null;
  return (
    <>
      {routeProtocol < 4 && routesNeedProtocol4(routes) && (
        <Notice tone="warn" title="Wildcard hostnames and the proxy default certificate need a newer worker">
          The worker on this node cannot apply them. They are kept as saved, but saving is rejected while any remain. Upgrade the worker and verify the node again, or change those routes to Let&apos;s Encrypt or a stored certificate for an exact hostname to save other changes.
        </Notice>
      )}
      {routeProtocol < 5 && routes.some((route) => isPrivateVisibility(route.policy?.visibility)) && (
        <Notice tone="warn" title="Private routes need a newer worker">
          The worker on this node cannot apply LAN or Tailscale visibility. Saved private settings are kept, but saving is rejected until the worker is upgraded and the node verified again. Switching a route to Public is also checked against the worker.
        </Notice>
      )}
      {routeProtocol < 6 && routes.some((route) => draftUsesTimeouts(route.policy)) && (
        <Notice tone="warn" title="Upstream timeouts need a newer worker">
          The worker on this node cannot apply them. Saved timeouts are kept and can be cleared, but saving them is rejected until the worker is upgraded and the node verified again.
        </Notice>
      )}
    </>
  );
}

function WildcardHint({ domain }: { domain: string }) {
  const base = domain.slice(2);
  return (
    <p className="text-xs break-words text-fg-muted">
      Matches exactly one label below <span className="font-mono break-all">{base}</span>, such as <span className="font-mono break-all">app.{base}</span>, but not <span className="font-mono break-all">{base}</span> itself or deeper names. An exact hostname on this resource always takes priority, even over a longer wildcard path, and must also select a stored certificate. Needs a stored certificate that names <span className="font-mono break-all">{domain}</span>.
    </p>
  );
}

function policySummary(policy: PolicyDraft) {
  const parts: string[] = [];
  if (policy.maintenance?.enabled) parts.push("maintenance on");
  if (policy.forwardAuth.mode !== "off") parts.push("forward-auth");
  if (policy.errorPages.length > 0) parts.push(policy.errorPages.length === 1 ? "1 error page" : `${policy.errorPages.length} error pages`);
  if (policy.pathPrefix.trim()) parts.push(`path ${policy.pathPrefix.trim()}`);
  if (policy.redirectHost.trim()) parts.push(`redirects to ${policy.redirectHost.trim()}`);
  const headers = policy.requestHeaders.length + policy.responseHeaders.length;
  if (headers > 0) parts.push(headers === 1 ? "1 header" : `${headers} headers`);
  if (policy.securityHeaders) parts.push("security headers");
  const cidrs = cidrList(policy.ipAllowList).length;
  if (cidrs > 0) parts.push(cidrs === 1 ? "1 allowed range" : `${cidrs} allowed ranges`);
  if (policy.upstreams.length > 0) parts.push(policy.upstreams.length === 1 ? "1 extra upstream" : `${policy.upstreams.length} extra upstreams`);
  if (policy.sticky) parts.push("sticky sessions");
  if (policy.certificateSource === "default") parts.push("proxy default certificate");
  if (policy.certificateSource === "uploaded") parts.push("stored certificate");
  if (draftUsesTimeouts(policy)) parts.push("upstream timeouts");
  if (isPrivateVisibility(policy.visibility)) parts.unshift(policy.visibility === "lan" ? "LAN only" : "Tailscale only");
  return parts.length > 0 ? parts.join(", ") : "Not configured";
}

function PolicyFields({ id, route, policy, errors, certificates, protocol4, protocol6, listeners, savedVisibility, onChange }: { id: string; route: RouteRow; policy: PolicyDraft; errors: Record<string, string>; certificates?: CertificateChoices; protocol4?: boolean; protocol6?: boolean; listeners?: ListenerChoices; savedVisibility?: RoutePolicy["visibility"]; onChange: (patch: Partial<PolicyDraft>) => void }) {
  const pathError = errors[`${route.id}.path`];
  const redirectError = errors[`${route.id}.redirect`];
  const cidrError = errors[`${route.id}.cidr`];
  const host = route.domain.trim() || "this hostname";
  return (
    <div className="space-y-5">
      <RouteVisibilityFields id={id} route={route} policy={policy} savedVisibility={savedVisibility} errors={errors} choices={listeners} onChange={onChange} />
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-path`} label="Path prefix" optional error={pathError} hint={`Only requests under this path on ${host} use this route. The prefix is passed to the service unchanged.`}>
          <input
            id={`${id}-path`}
            className={`${inputClass} font-mono`}
            value={policy.pathPrefix}
            onChange={(event) => onChange({ pathPrefix: event.target.value })}
            placeholder="/api"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={1024}
            aria-invalid={pathError ? true : undefined}
            aria-describedby={describedBy(`${id}-path`, pathError, true)}
          />
        </Field>
        <CertificateSourceField id={`${id}-certificate`} policy={policy} choices={certificates} protocol4={protocol4} error={policy.certificateSource === "uploaded" ? undefined : errors[`${route.id}.certificate`]} onChange={onChange} />
      </div>
      {policy.certificateSource === "uploaded" && <UploadedCertificatePicker id={`${id}-uploaded`} route={route} policy={policy} error={errors[`${route.id}.certificate`]} choices={certificates} onChange={onChange} />}

      <div className="space-y-3">
        <Field id={`${id}-redirect`} label="Redirect to hostname" optional error={redirectError} hint="Sends every request to this hostname over HTTPS, keeping the path and query. Its DNS record and route are not created for you.">
          <input
            id={`${id}-redirect`}
            className={`${inputClass} font-mono`}
            value={policy.redirectHost}
            onChange={(event) => onChange({ redirectHost: event.target.value })}
            placeholder="www.example.com"
            inputMode="url"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-invalid={redirectError ? true : undefined}
            aria-describedby={describedBy(`${id}-redirect`, redirectError, true)}
          />
        </Field>
        <SwitchField
          id={`${id}-permanent`}
          label="Permanent redirect"
          description={policy.redirectHost.trim() ? "Responds with 301 so browsers and search engines remember it. Off responds with 302." : "Enter a hostname to redirect to first."}
          checked={policy.redirectPermanent && policy.redirectHost.trim() !== ""}
          disabled={policy.redirectHost.trim() === ""}
          onChange={(event) => onChange({ redirectPermanent: event.target.checked })}
        />
      </div>

      <Field
        id={`${id}-cidr`}
        label="Allowed source IP ranges"
        optional
        error={cidrError}
        hint={`IPv4 or IPv6 CIDR ranges, one per line, up to ${routeLimits.cidrs}. Other clients get 403. The proxy checks the connecting address, so behind a CDN or another proxy list that proxy's ranges.`}
      >
        <textarea
          id={`${id}-cidr`}
          className={`${inputClass} font-mono`}
          rows={3}
          value={policy.ipAllowList}
          onChange={(event) => onChange({ ipAllowList: event.target.value })}
          placeholder={"10.0.0.0/8\nfd00::/8"}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          aria-invalid={cidrError ? true : undefined}
          aria-describedby={describedBy(`${id}-cidr`, cidrError, true)}
        />
      </Field>

      <div className="space-y-4">
        <SwitchField
          id={`${id}-security`}
          label="Security headers"
          description="Adds Strict-Transport-Security for one year, X-Content-Type-Options: nosniff, X-Frame-Options: DENY and Referrer-Policy: strict-origin-when-cross-origin."
          checked={policy.securityHeaders}
          onChange={(event) => onChange({ securityHeaders: event.target.checked })}
        />
        <HeaderRows id={`${id}-request`} title="Request headers" description="Set on requests before they reach the service. An empty value removes the header." rows={policy.requestHeaders} errorKey={`${route.id}.request`} errors={errors} onChange={(requestHeaders) => onChange({ requestHeaders })} />
        <HeaderRows id={`${id}-response`} title="Response headers" description="Set on responses sent to clients. An empty value removes the header." rows={policy.responseHeaders} errorKey={`${route.id}.response`} errors={errors} onChange={(responseHeaders) => onChange({ responseHeaders })} />
      </div>

      <div className="space-y-4">
        <UpstreamRows id={`${id}-upstreams`} route={route} rows={policy.upstreams} errors={errors} onChange={(upstreams) => onChange({ upstreams })} />
        <SwitchField
          id={`${id}-sticky`}
          label="Sticky sessions"
          description="Keeps each client on the same upstream with a secure, HTTP-only affinity cookie."
          checked={policy.sticky}
          onChange={(event) => onChange({ sticky: event.target.checked })}
        />
      </div>

      <TimeoutFields id={`${id}-timeouts`} routeId={route.id} timeouts={policy.timeouts} protocol6={protocol6} errors={errors} onChange={(timeouts) => onChange({ timeouts })} />
    </div>
  );
}

const timeoutFields = [
  { key: "dial", label: "Upstream connection timeout", hint: `Time allowed to open a connection to the service. Up to ${timeoutLimits.dial} seconds.` },
  { key: "header", label: "Response header timeout", hint: `Time allowed for the service to send response headers after receiving the request. Up to ${timeoutLimits.header} seconds.` },
  { key: "idle", label: "Idle pooled connection timeout", hint: `How long an unused keep-alive connection to the service stays open for reuse. It does not limit active WebSocket connections. Up to ${timeoutLimits.idle} seconds.` },
] as const;

// Positive overrides only: an empty or zero field keeps the proxy default, which is not unlimited. Unless the worker is
// confirmed to apply timeouts, saved values stay editable so they can be cleared, and empty fields stay disabled.
function TimeoutFields({ id, routeId, timeouts, protocol6, errors, onChange }: { id: string; routeId: string; timeouts: TimeoutDraft; protocol6?: boolean; errors: Record<string, string>; onChange: (timeouts: TimeoutDraft) => void }) {
  const capabilityError = errors[`${routeId}.timeouts`];
  const description = `${id}-description`;
  return (
    <fieldset className="min-w-0 space-y-3" aria-describedby={description}>
      <legend className="text-[13px] font-medium text-fg">Upstream timeouts</legend>
      <p id={description} className="-mt-2 text-xs text-fg-muted">
        Seconds. Leave empty or 0 to keep the proxy default. WebSocket and other HTTP upgrades work automatically; no Upgrade headers need to be set.
        {protocol6 === false && " Setting new timeouts needs a newer worker on this node."}
        {protocol6 === undefined && " New timeouts can be set once the worker capabilities of the node are known."}
      </p>
      <div className="grid gap-4 sm:grid-cols-3">
        {timeoutFields.map((field) => {
          const fieldId = `${id}-${field.key}`;
          const error = errors[`${routeId}.timeouts.${field.key}`];
          return (
            <Field key={field.key} id={fieldId} label={field.label} optional error={error} hint={field.hint}>
              <input
                id={fieldId}
                className={`${inputClass} font-mono tabular-nums`}
                type="number"
                inputMode="numeric"
                min={0}
                max={timeoutLimits[field.key]}
                step={1}
                placeholder="Default"
                value={timeouts[field.key]}
                disabled={protocol6 !== true && timeouts[field.key] === ""}
                onChange={(event) => onChange({ ...timeouts, [field.key]: event.target.value })}
                aria-invalid={error ? true : undefined}
                aria-describedby={describedBy(fieldId, error, true)}
              />
            </Field>
          );
        })}
      </div>
      {capabilityError && <p className="text-xs text-bad">{capabilityError}</p>}
    </fieldset>
  );
}

function HeaderRows({ id, title, description, rows, errorKey, errors, onChange }: { id: string; title: string; description: string; rows: HeaderRow[]; errorKey: string; errors: Record<string, string>; onChange: (rows: HeaderRow[]) => void }) {
  const singular = title.replace(/s$/, "").toLowerCase();
  const limitError = errors[errorKey];
  return (
    <fieldset className="min-w-0 space-y-2">
      <legend className="text-[13px] font-medium text-fg">{title}</legend>
      <p className="-mt-1 text-xs text-fg-muted">{description}</p>
      {rows.length > 0 && (
        <ul aria-label={title} className="space-y-2">
          {rows.map((row, index) => {
            const error = errors[`${errorKey}.${row.id}`];
            const errorId = error ? `${id}-${index}-error` : undefined;
            const hintId = row.hidden ? `${id}-${index}-hint` : undefined;
            const valueId = `${id}-value-${index}`;
            const set = (patch: Partial<HeaderRow>) => onChange(rows.map((item) => (item.id === row.id ? { ...item, ...patch } : item)));
            return (
              <li key={row.id} className="space-y-1">
                <div className="grid grid-cols-[minmax(0,1fr)_auto] items-end gap-2 sm:grid-cols-[minmax(0,2fr)_minmax(0,3fr)_auto]">
                  <div className="col-span-2 sm:col-span-1">
                    <label htmlFor={`${id}-name-${index}`} className={label}>
                      Name
                    </label>
                    <input id={`${id}-name-${index}`} className={`${inputClass} font-mono`} value={row.name} onChange={(event) => set({ name: event.target.value })} placeholder="X-App" maxLength={128} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
                  </div>
                  <div>
                    <label htmlFor={valueId} className={label}>
                      {row.hidden ? "Replacement value" : "Value"}
                    </label>
                    <input id={valueId} className={`${inputClass} font-mono`} value={row.value} onChange={(event) => set({ value: event.target.value })} placeholder={row.hidden ? "Saved value hidden" : undefined} maxLength={4096} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={[hintId, errorId].filter(Boolean).join(" ") || undefined} />
                  </div>
                  <Button variant="ghost" className="px-2" aria-label={`Remove ${singular} ${row.name || index + 1}`} title={`Remove ${singular}`} onClick={() => onChange(rows.filter((item) => item.id !== row.id))}>
                    <Trash2 aria-hidden className="size-4" />
                  </Button>
                </div>
                {row.hidden && (
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <p id={hintId} className="text-xs text-fg-muted">
                      The saved value is hidden. Leave this blank to keep it, or enter a new value to replace it.
                    </p>
                    {row.value === "" && (
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`Use an empty value for ${row.name || `${singular} ${index + 1}`}`}
                        onClick={() => {
                          set({ hidden: false });
                          requestAnimationFrame(() => document.getElementById(valueId)?.focus());
                        }}
                      >
                        Use an empty value
                      </Button>
                    )}
                  </div>
                )}
                {error && (
                  <p id={errorId} className="text-xs text-bad">
                    {error}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {limitError && <p className="text-xs text-bad">{limitError}</p>}
      <Button size="sm" disabled={rows.length >= routeLimits.headers} onClick={() => onChange([...rows, { id: crypto.randomUUID(), name: "", value: "" }])}>
        <Plus aria-hidden className="size-3.5" />
        Add {singular}
      </Button>
    </fieldset>
  );
}

// route: the HTTP or TCP/UDP route whose primary target the rows share traffic with; errors are keyed by its id.
export function UpstreamRows({ id, route, rows, errors, onChange }: { id: string; route: Pick<RouteRow, "id" | "service" | "port">; rows: UpstreamRow[]; errors: Record<string, string>; onChange: (rows: UpstreamRow[]) => void }) {
  const limitError = errors[`${route.id}.upstreams`];
  const primary = route.service.trim() ? `${route.service.trim()}:${Number.isNaN(route.port) ? "" : route.port}` : "the service above";
  return (
    <fieldset className="min-w-0 space-y-2">
      <legend className="text-[13px] font-medium text-fg">Additional upstreams</legend>
      <p className="-mt-1 text-xs text-fg-muted">
        Other services and ports in this resource that share the traffic equally with <span className="font-mono">{primary}</span>. Replicas are not created for you.
      </p>
      {rows.length > 0 && (
        <ul aria-label="Additional upstreams" className="space-y-2">
          {rows.map((row, index) => {
            const error = errors[`${route.id}.upstream.${row.id}`];
            const errorId = error ? `${id}-${index}-error` : undefined;
            const set = (patch: Partial<UpstreamRow>) => onChange(rows.map((item) => (item.id === row.id ? { ...item, ...patch } : item)));
            return (
              <li key={row.id} className="space-y-1">
                <div className="grid grid-cols-[minmax(0,1fr)_6rem_auto] items-end gap-2">
                  <div>
                    <label htmlFor={`${id}-service-${index}`} className={label}>
                      Compose service
                    </label>
                    <input id={`${id}-service-${index}`} className={`${inputClass} font-mono`} value={row.service} onChange={(event) => set({ service: event.target.value })} placeholder="web" autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
                  </div>
                  <div>
                    <label htmlFor={`${id}-port-${index}`} className={label}>
                      Container port
                    </label>
                    <input id={`${id}-port-${index}`} className={`${inputClass} font-mono tabular-nums`} type="number" min={1} max={65535} value={Number.isNaN(row.port) ? "" : row.port} onChange={(event) => set({ port: event.target.valueAsNumber })} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
                  </div>
                  <Button variant="ghost" className="px-2" aria-label={`Remove upstream ${row.service || index + 1}`} title="Remove upstream" onClick={() => onChange(rows.filter((item) => item.id !== row.id))}>
                    <Trash2 aria-hidden className="size-4" />
                  </Button>
                </div>
                {error && (
                  <p id={errorId} className="text-xs text-bad">
                    {error}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {limitError && <p className="text-xs text-bad">{limitError}</p>}
      <Button size="sm" disabled={rows.length >= routeLimits.upstreams} onClick={() => onChange([...rows, { id: crypto.randomUUID(), service: "", port: 80 }])}>
        <Plus aria-hidden className="size-3.5" />
        Add upstream
      </Button>
    </fieldset>
  );
}
