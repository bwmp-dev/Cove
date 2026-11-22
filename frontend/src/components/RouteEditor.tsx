import { ChevronDown, Plus, Trash2 } from "lucide-react";
import { useId, useState } from "react";
import { cidrList, emptyPolicy, routeLimits, routesUsePolicies, type HeaderRow, type PolicyDraft, type RouteRow, type UpstreamRow } from "../lib/workload-form";
import { describedBy, inputClass } from "../lib/styles";
import { Select } from "./Select";
import { Button, Field, Notice, SwitchField } from "./ui";

const label = "mb-1 block text-xs font-medium text-fg-muted";

// policies: whether the target node's worker reports route policy support; undefined while unknown, which hides the
// advanced controls. Saved policies are always kept, whether or not they can be edited here.
export function RouteEditor({ routes, errors = {}, policies, onChange }: { routes: RouteRow[]; errors?: Record<string, string>; policies?: boolean; onChange: (routes: RouteRow[]) => void }) {
  const id = useId();
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
      {policies === false && (
        <Notice tone={routesUsePolicies(routes) ? "warn" : "info"} title="Advanced routing needs a newer worker">
          Path prefixes, headers, IP allowlists, redirects and extra upstreams are not available on this node yet. Upgrade the worker on the node, then verify the node again.
          {routesUsePolicies(routes) && " Existing advanced settings are kept, but saving them is rejected until the worker supports them."}
        </Notice>
      )}
      {routes.length > 0 && (
        <ul aria-label="Routes" className="space-y-3">
          {routes.map((route, index) => {
            const error = errors[route.id];
            const describedById = error ? `${id}-${index}-error` : undefined;
            const policyError = Object.keys(errors).some((key) => key.startsWith(`${route.id}.`));
            const open = policies === true && (expanded.has(route.id) || policyError);
            const panel = `${id}-${index}-advanced`;
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
                {route.auth && <p className="text-xs text-fg-muted">Requires a password at the proxy. The saved credential is kept.</p>}
                {policies === true && (
                  <div>
                    <button type="button" aria-expanded={open} aria-controls={panel} onClick={() => toggle(route)} className="flex max-w-full items-start gap-1.5 rounded py-1 text-left text-xs font-medium text-fg-muted hover:text-fg pointer-coarse:min-h-10">
                      <ChevronDown aria-hidden className={`mt-px size-3.5 shrink-0 transition-transform ${open ? "" : "-rotate-90"}`} />
                      <span className="min-w-0">
                        Advanced routing
                        {!open && route.policy && <span className="font-normal text-fg-subtle"> · {policySummary(route.policy)}</span>}
                      </span>
                    </button>
                    {open && route.policy && (
                      <div id={panel} className="mt-2 border-l-2 border-line pl-3 sm:pl-4">
                        <PolicyFields
                          id={`${id}-${index}`}
                          route={route}
                          policy={route.policy}
                          errors={errors}
                          onChange={(patch) => update(route.id, { policy: { ...(route.policy ?? emptyPolicy()), ...patch } })}
                        />
                      </div>
                    )}
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <Button disabled={routes.length >= 32} onClick={() => onChange([...routes, { id: crypto.randomUUID(), domain: "", service: "", port: 80 }])}>
        <Plus aria-hidden className="size-3.5" />
        Add route
      </Button>
    </div>
  );
}

function policySummary(policy: PolicyDraft) {
  const parts: string[] = [];
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
  return parts.length > 0 ? parts.join(", ") : "Not configured";
}

function PolicyFields({ id, route, policy, errors, onChange }: { id: string; route: RouteRow; policy: PolicyDraft; errors: Record<string, string>; onChange: (patch: Partial<PolicyDraft>) => void }) {
  const pathError = errors[`${route.id}.path`];
  const redirectError = errors[`${route.id}.redirect`];
  const cidrError = errors[`${route.id}.cidr`];
  const host = route.domain.trim() || "this hostname";
  return (
    <div className="space-y-5">
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
        <Field id={`${id}-certificate`} label="Certificate" hint={policy.certificateSource === "default" ? "Uses the certificate configured in the node's proxy settings, which may be self-signed. Browsers trust it only if you provisioned one." : "Requested from Let's Encrypt automatically."}>
          <Select
            id={`${id}-certificate`}
            value={policy.certificateSource || "acme"}
            onChange={(event) => onChange({ certificateSource: event.target.value === "default" ? "default" : policy.certificateSource === "acme" ? "acme" : "" })}
            aria-describedby={`${id}-certificate-hint`}
          >
            <option value="acme">Let's Encrypt</option>
            <option value="default">Proxy default certificate</option>
          </Select>
        </Field>
      </div>

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
    </div>
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
                    <label htmlFor={`${id}-value-${index}`} className={label}>
                      Value
                    </label>
                    <input id={`${id}-value-${index}`} className={`${inputClass} font-mono`} value={row.value} onChange={(event) => set({ value: event.target.value })} maxLength={4096} autoComplete="off" autoCapitalize="none" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
                  </div>
                  <Button variant="ghost" className="px-2" aria-label={`Remove ${singular} ${row.name || index + 1}`} title={`Remove ${singular}`} onClick={() => onChange(rows.filter((item) => item.id !== row.id))}>
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
      <Button size="sm" disabled={rows.length >= routeLimits.headers} onClick={() => onChange([...rows, { id: crypto.randomUUID(), name: "", value: "" }])}>
        <Plus aria-hidden className="size-3.5" />
        Add {singular}
      </Button>
    </fieldset>
  );
}

function UpstreamRows({ id, route, rows, errors, onChange }: { id: string; route: RouteRow; rows: UpstreamRow[]; errors: Record<string, string>; onChange: (rows: UpstreamRow[]) => void }) {
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
