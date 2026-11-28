import { Plus, Trash2 } from "lucide-react";
import { byteLength, emptyForwardAuth, routePageLimits, type AuthMode, type ErrorPageRow, type ForwardAuthDraft, type PolicyDraft, type RouteRow } from "../lib/workload-form";
import { describedBy, inputClass } from "../lib/styles";
import { Select } from "./Select";
import { Button, Field, Notice, SwitchField } from "./ui";

const label = "mb-1 block text-xs font-medium text-fg-muted";
const bytes = new Intl.NumberFormat("en");
const textInput = { autoComplete: "off", autoCapitalize: "none", spellCheck: false } as const;

type Change = (patch: Partial<Pick<PolicyDraft, "maintenance" | "errorPages" | "forwardAuth">>) => void;

// Maintenance, custom error pages and forward-auth for one route. HTML is only ever edited as text: it is never rendered
// here. editable is false when the node's worker cannot apply these settings; saved values stay visible and can only be
// removed, which is what lets the route be saved to that worker again.
export function RouteExtensionFields({ id, route, policy, errors, editable, savedAuth, onChange }: { id: string; route: RouteRow; policy: PolicyDraft; errors: Record<string, string>; editable: boolean; savedAuth: boolean; onChange: Change }) {
  const target = `${route.domain.trim() || "this hostname"}${policy.pathPrefix.trim()}`;
  return (
    <div className="space-y-5 border-t border-line pt-4">
      {(editable || policy.forwardAuth.mode !== "off") && <ForwardAuthFields id={`${id}-auth`} route={route} auth={policy.forwardAuth} errors={errors} editable={editable} savedAuth={savedAuth} target={target} onChange={(forwardAuth) => onChange({ forwardAuth })} />}
      {(editable || policy.maintenance !== null) && <MaintenanceFields id={`${id}-maintenance`} route={route} maintenance={policy.maintenance} errors={errors} editable={editable} target={target} onChange={(maintenance) => onChange({ maintenance })} />}
      {(editable || policy.errorPages.length > 0) && <ErrorPageRows id={`${id}-pages`} route={route} rows={policy.errorPages} errors={errors} editable={editable} onChange={(errorPages) => onChange({ errorPages })} />}
    </div>
  );
}

function RemoveSaved({ label: text, onRemove }: { label: string; onRemove: () => void }) {
  return (
    <Button size="sm" variant="danger" onClick={onRemove}>
      <Trash2 aria-hidden className="size-3.5" />
      {text}
    </Button>
  );
}

const authModes: Record<AuthMode, string> = { off: "Off", url: "Public HTTPS endpoint", service: "Service in this resource" };
const authModeHints: Record<AuthMode, string | undefined> = {
  off: undefined,
  url: "An endpoint outside this resource, such as the verify URL of Authelia, Authentik or oauth2-proxy. Its certificate is verified.",
  service: "A Compose service of this resource answers the verification requests. Replicas are not created for you.",
};

type SetAuth = (patch: Partial<ForwardAuthDraft>) => void;

function AuthServiceFields({ id, auth, keepsPath, serviceError, pathError, editable, set }: { id: string; auth: ForwardAuthDraft; keepsPath: boolean; serviceError?: string; pathError?: string; editable: boolean; set: SetAuth }) {
  const serviceDescribedBy = serviceError ? `${id}-service-error` : `${id}-service-hint`;
  return (
    <div className="space-y-1.5">
      <div className="grid grid-cols-[minmax(0,1fr)_6rem] gap-2 sm:grid-cols-[minmax(0,1fr)_6rem_minmax(0,1.5fr)]">
        <div>
          <label htmlFor={`${id}-service`} className={label}>
            Compose service
          </label>
          <input id={`${id}-service`} className={`${inputClass} font-mono`} value={auth.service} readOnly={!editable} onChange={(event) => set({ service: event.target.value })} placeholder="auth" {...textInput} aria-invalid={serviceError ? true : undefined} aria-describedby={serviceDescribedBy} />
        </div>
        <div>
          <label htmlFor={`${id}-port`} className={label}>
            Container port
          </label>
          <input id={`${id}-port`} className={`${inputClass} font-mono tabular-nums`} type="number" min={1} max={65535} value={Number.isNaN(auth.port) ? "" : auth.port} readOnly={!editable} onChange={(event) => set({ port: event.target.valueAsNumber })} aria-invalid={serviceError ? true : undefined} aria-describedby={serviceDescribedBy} />
        </div>
        <div className="col-span-2 sm:col-span-1">
          <label htmlFor={`${id}-path`} className={label}>
            Path <span className="font-normal text-fg-subtle">(optional)</span>
          </label>
          <input id={`${id}-path`} className={`${inputClass} font-mono`} value={auth.path} readOnly={!editable} onChange={(event) => set({ path: event.target.value })} placeholder="/api/verify" maxLength={routePageLimits.authURL} {...textInput} aria-invalid={pathError ? true : undefined} aria-describedby={pathError ? `${id}-path-error` : `${id}-service-hint`} />
        </div>
      </div>
      {serviceError && (
        <p id={`${id}-service-error`} className="text-xs text-bad">
          {serviceError}
        </p>
      )}
      {pathError && (
        <p id={`${id}-path-error`} className="text-xs text-bad">
          {pathError}
        </p>
      )}
      <p id={`${id}-service-hint`} className="text-xs text-fg-muted">
        Requests go to the service you name here, whichever replica or generation is running. The path may include a query string.
        {keepsPath && " A saved path is hidden; leaving the path blank keeps it while the service stays the same."}
      </p>
    </div>
  );
}

function AuthNameFields({ id, auth, headersError, cookiesError, editable, set }: { id: string; auth: ForwardAuthDraft; headersError?: string; cookiesError?: string; editable: boolean; set: SetAuth }) {
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-headers`} label="Identity headers" optional error={headersError} hint={`Response headers from the provider, such as Remote-User, set on the request to the service, one per line, up to ${routePageLimits.authHeaders}. Values clients send for these headers are always removed.`}>
          <textarea id={`${id}-headers`} className={`${inputClass} font-mono`} rows={3} value={auth.responseHeaders} readOnly={!editable} onChange={(event) => set({ responseHeaders: event.target.value })} placeholder={"Remote-User\nRemote-Groups"} {...textInput} aria-invalid={headersError ? true : undefined} aria-describedby={describedBy(`${id}-headers`, headersError, true)} />
        </Field>
        <Field id={`${id}-cookies`} label="Provider cookies" optional error={cookiesError} hint={`Cookie names the provider sets that are copied to the browser's response, one per line, up to ${routePageLimits.authCookies}.`}>
          <textarea id={`${id}-cookies`} className={`${inputClass} font-mono`} rows={3} value={auth.responseCookies} readOnly={!editable} onChange={(event) => set({ responseCookies: event.target.value })} placeholder="session" {...textInput} aria-invalid={cookiesError ? true : undefined} aria-describedby={describedBy(`${id}-cookies`, cookiesError, true)} />
        </Field>
      </div>
      <p className="text-xs text-fg-muted">Idle sleep cannot be used with an auth service in this resource or an auth URL on one of its hostnames, because the app would have to wake to check access. Use an independent provider, or turn idle sleep off.</p>
    </>
  );
}

function ForwardAuthFields({ id, route, auth, errors, editable, savedAuth, target, onChange }: { id: string; route: RouteRow; auth: ForwardAuthDraft; errors: Record<string, string>; editable: boolean; savedAuth: boolean; target: string; onChange: (auth: ForwardAuthDraft) => void }) {
  const urlError = errors[`${route.id}.auth.url`];
  const set: SetAuth = (patch) => onChange({ ...auth, ...patch });
  return (
    <fieldset className="min-w-0 space-y-3">
      <legend className="text-[13px] font-medium text-fg">Forward-auth</legend>
      <p className="-mt-1 text-xs text-fg-muted">
        Before each request to <span className="font-mono break-all">{target}</span>, the proxy sends a GET with the original Cookie, Authorization, Accept and User-Agent headers and X-Forwarded-Method, Proto, Host, Uri and For. A 2xx response lets the request
        through; other responses, including redirects to a sign-in page, go back to the browser. An error, a response over 32 KiB or no answer within 15 seconds returns 503.
      </p>
      <p className="text-xs text-fg-muted">Cove hides a saved verification URL and service path; leave them blank to keep the saved values. Still avoid tokens, passwords or other secrets in the URL, path or query string; use the query only for provider options.</p>
      <Field id={`${id}-mode`} label="Provider" hint={authModeHints[auth.mode]}>
        <Select id={`${id}-mode`} value={auth.mode} disabled={!editable} onChange={(event) => set({ mode: event.target.value === "url" || event.target.value === "service" ? event.target.value : "off" })} aria-describedby={auth.mode === "off" ? undefined : `${id}-mode-hint`}>
          {Object.entries(authModes).map(([value, text]) => (
            <option key={value} value={value}>
              {text}
            </option>
          ))}
        </Select>
      </Field>
      {auth.mode === "off" && savedAuth && (
        <Notice tone="warn" title="Saving removes forward-auth">
          Requests to <span className="font-mono break-all">{target}</span> will reach the service without the provider's check.
        </Notice>
      )}
      {auth.mode === "url" && (
        <Field id={`${id}-url`} label="Verification URL" error={urlError} hint={`${auth.hidden && savedAuth ? "The saved URL is hidden. Leave this blank to keep it, or enter a new URL to replace it. " : ""}Private and reserved IP addresses are refused, and hostnames are checked again when connecting. A private provider outside this resource needs its own authenticated HTTPS hostname.`}>
          <input id={`${id}-url`} className={`${inputClass} font-mono`} value={auth.url} readOnly={!editable} onChange={(event) => set({ url: event.target.value })} placeholder={auth.hidden && savedAuth ? "Saved URL hidden" : "https://auth.example.com/api/verify"} inputMode="url" maxLength={routePageLimits.authURL} {...textInput} aria-invalid={urlError ? true : undefined} aria-describedby={describedBy(`${id}-url`, urlError, true)} />
        </Field>
      )}
      {auth.mode === "service" && <AuthServiceFields id={id} auth={auth} keepsPath={auth.hidden && savedAuth} serviceError={errors[`${route.id}.auth.service`]} pathError={errors[`${route.id}.auth.path`]} editable={editable} set={set} />}
      {auth.mode !== "off" && <AuthNameFields id={id} auth={auth} headersError={errors[`${route.id}.auth.headers`]} cookiesError={errors[`${route.id}.auth.cookies`]} editable={editable} set={set} />}
      {!editable && auth.mode !== "off" && (
        <div className="space-y-2">
          <p className="text-xs text-fg-muted">Removing forward-auth lets requests reach the service without the provider's check.</p>
          <RemoveSaved label="Remove forward-auth" onRemove={() => onChange(emptyForwardAuth())} />
        </div>
      )}
    </fieldset>
  );
}

function MaintenanceFields({ id, route, maintenance, errors, editable, target, onChange }: { id: string; route: RouteRow; maintenance: PolicyDraft["maintenance"]; errors: Record<string, string>; editable: boolean; target: string; onChange: (maintenance: PolicyDraft["maintenance"]) => void }) {
  const error = errors[`${route.id}.maintenance`];
  const html = maintenance?.html ?? "";
  // Off with no custom page is the same as no setting, so it is removed rather than saved as an empty object.
  const set = (enabled: boolean, text: string) => onChange(enabled || text !== "" ? { enabled, html: text } : null);
  return (
    <fieldset className="min-w-0 space-y-3">
      <legend className="text-[13px] font-medium text-fg">Maintenance</legend>
      <SwitchField
        id={`${id}-enabled`}
        label="Maintenance mode"
        description={
          <>
            Every request to <span className="font-mono break-all">{target}</span> gets 503 with Retry-After: 60 and the page below, without reaching the service. Proxy passwords, allowed IP ranges and forward-auth still apply.
          </>
        }
        checked={maintenance?.enabled ?? false}
        disabled={!editable}
        onChange={(event) => set(event.target.checked, html)}
      />
      <Field
        id={`${id}-html`}
        label="Maintenance page"
        optional
        error={error}
        hint={`Static HTML, ${bytes.format(byteLength(html))} of ${bytes.format(routePageLimits.pageBytes)} bytes. Scripts and external files are blocked; inline styles and data: images work. Leave it empty for the default page.`}
      >
        <textarea id={`${id}-html`} className={`${inputClass} font-mono`} rows={5} value={html} readOnly={!editable} onChange={(event) => set(maintenance?.enabled ?? false, event.target.value)} placeholder="<h1>Back soon</h1>" {...textInput} aria-invalid={error ? true : undefined} aria-describedby={describedBy(`${id}-html`, error, true)} />
      </Field>
      {!editable && maintenance && <RemoveSaved label="Remove maintenance settings" onRemove={() => onChange(null)} />}
    </fieldset>
  );
}

function ErrorPageRows({ id, route, rows, errors, editable, onChange }: { id: string; route: RouteRow; rows: ErrorPageRow[]; errors: Record<string, string>; editable: boolean; onChange: (rows: ErrorPageRow[]) => void }) {
  const limitError = errors[`${route.id}.pages`];
  const total = rows.reduce((sum, row) => sum + byteLength(row.html), 0);
  return (
    <fieldset className="min-w-0 space-y-2">
      <legend className="text-[13px] font-medium text-fg">Error pages</legend>
      <p className="-mt-1 text-xs text-fg-muted">
        Static HTML shown instead of the service's own error response for a status, keeping the status code. Denials by the proxy password, allowed IP ranges and forward-auth are not replaced, and maintenance mode takes priority.
        {rows.length > 0 && ` ${bytes.format(total)} of ${bytes.format(routePageLimits.totalBytes)} bytes used.`}
      </p>
      {rows.length > 0 && (
        <ul aria-label="Error pages" className="space-y-3">
          {rows.map((row, index) => {
            const error = errors[`${route.id}.page.${row.id}`];
            const errorId = error ? `${id}-${index}-error` : undefined;
            const set = (patch: Partial<ErrorPageRow>) => onChange(rows.map((item) => (item.id === row.id ? { ...item, ...patch } : item)));
            const name = row.status.trim() || String(index + 1);
            return (
              <li key={row.id} className="space-y-1.5">
                <div className="flex items-end gap-2">
                  <div className="w-24">
                    <label htmlFor={`${id}-status-${index}`} className={label}>
                      Status
                    </label>
                    <input id={`${id}-status-${index}`} className={`${inputClass} font-mono tabular-nums`} value={row.status} readOnly={!editable} onChange={(event) => set({ status: event.target.value })} placeholder="502" inputMode="numeric" maxLength={3} {...textInput} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
                  </div>
                  <Button variant="ghost" className="ml-auto px-2" aria-label={`Remove error page ${name}`} title="Remove error page" onClick={() => onChange(rows.filter((item) => item.id !== row.id))}>
                    <Trash2 aria-hidden className="size-4" />
                  </Button>
                </div>
                <div>
                  <label htmlFor={`${id}-html-${index}`} className={label}>
                    HTML for status {row.status.trim() || "…"}
                  </label>
                  <textarea id={`${id}-html-${index}`} className={`${inputClass} font-mono`} rows={3} value={row.html} readOnly={!editable} onChange={(event) => set({ html: event.target.value })} placeholder="<h1>Temporarily unavailable</h1>" {...textInput} aria-invalid={error ? true : undefined} aria-describedby={errorId} />
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
      {editable ? (
        <Button size="sm" disabled={rows.length >= routePageLimits.errorPages} onClick={() => onChange([...rows, { id: crypto.randomUUID(), status: "", html: "" }])}>
          <Plus aria-hidden className="size-3.5" />
          Add error page
        </Button>
      ) : (
        rows.length > 1 && <RemoveSaved label="Remove all error pages" onRemove={() => onChange([])} />
      )}
    </fieldset>
  );
}
