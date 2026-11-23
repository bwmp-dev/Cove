import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Globe, Lock, Network } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { databaseKeys, usePoolerSetting, usePublicAccess, type DatabaseDetails } from "../api/databases";
import { workloadOperationsQuery } from "../api/queries";
import type { Workload } from "../api/types";
import {
  engineDefaultPublicPort,
  engineName,
  normalisePem,
  poolerPort,
  poolerService,
  settingsFailed,
  settingsSettled,
  validatePublicAccess,
  type PublicAccessDraft,
  type PublicAccessErrors,
} from "../lib/databases";
import { formatAbsolute } from "../lib/format";
import { useOnline, useReviewed } from "../lib/hooks";
import type { Tone } from "../lib/nodes";
import { activeOperation } from "../lib/operations";
import { buttonClass, describedBy, inputClass, linkClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { ExpiryTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, Notice, Section, Subsection } from "./ui";

// The saved settings are the desired state; the settings operation reports whether they have been applied yet.
export function SettingsStatus({ details }: { details: DatabaseDetails }) {
  const state = details.settings_state;
  const operation = details.database.settings_operation;
  if (settingsSettled(state) || !operation) return null;
  const link = (
    <Link to="/operations/$operationId" params={{ operationId: operation }} className={buttonClass("secondary")}>
      View operation
    </Link>
  );
  if (state === "unknown") {
    return (
      <Notice tone="warn" title="Settings state unknown" action={link}>
        The operation recorded for the last settings change no longer exists, so Cove cannot tell whether the settings below were applied. Check the resource's recent deployments, or save the setting again to apply it.
      </Notice>
    );
  }
  if (settingsFailed(state)) {
    return (
      <Notice tone="error" title="The last settings change was not applied" action={link}>
        The settings below are saved but the operation that applies them ended {state}. Review the operation, then save the setting again.
        {details.database.public_access ? " If enabling public TLS access failed, disable it to restore the private configuration." : ""}
      </Notice>
    );
  }
  return (
    <Notice tone="info" title="Applying database settings" action={link}>
      The settings below are saved and the operation applying them is {state}. Clients may reconnect while it runs.
    </Notice>
  );
}

function settingTone(enabled: boolean, details: DatabaseDetails): { tone: Tone; label: string } {
  if (details.settings_state === "unknown") return { tone: "warn", label: enabled ? "Enabled, apply state unknown" : "Disabled, apply state unknown" };
  if (settingsFailed(details.settings_state)) return { tone: "bad", label: enabled ? "Enabled, not applied" : "Disabled, not applied" };
  if (!settingsSettled(details.settings_state)) return { tone: "info", label: enabled ? "Enabling" : "Applying" };
  return enabled ? { tone: "ok", label: "Enabled" } : { tone: "neutral", label: "Disabled" };
}

function useSettingsBlock(workload: Workload, details: DatabaseDetails) {
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const online = useOnline();
  const active = Boolean(operations.data && activeOperation(operations.data, workload.id));
  if (details.maintenance_locked) return "Settle the recorded maintenance first.";
  if (active) return "Wait for the active operation to finish.";
  if (!online) return "You are offline.";
  return null;
}

export function DatabaseSettings({ workload, details, editor }: { workload: Workload; details: DatabaseDetails; editor: boolean }) {
  const blocked = useSettingsBlock(workload, details);
  const reason = editor ? blocked : null;
  return (
    <Section
      title="Access settings"
      description="Private by default. Each change saves the configuration and runs a deployment operation, so approvals, freezes and deployment windows apply. Bound consumers are never redeployed automatically."
      flush
    >
      <div className="divide-y divide-line">
        {details.database.engine === "postgresql" && <PoolerSetting workload={workload} details={details} editor={editor} blocked={reason} />}
        <PublicAccessSetting workload={workload} details={details} editor={editor} blocked={reason} />
        {reason && editor && (
          <div className="px-4 py-3 text-xs text-fg-muted" role="status">
            {reason}
          </div>
        )}
      </div>
    </Section>
  );
}

function PoolerSetting({ workload, details, editor, blocked }: { workload: Workload; details: DatabaseDetails; editor: boolean; blocked: string | null }) {
  const [open, setOpen] = useState(false);
  const enabled = Boolean(details.database.pooler);
  const status = settingTone(enabled, details);
  return (
    <Subsection
      title={
        <span className="inline-flex items-center gap-2">
          <Network aria-hidden className="size-3.5 text-fg-subtle" />
          Connection pooler
        </span>
      }
      description="PgBouncer in transaction pooling mode, on the private network only. Using it is opt-in per client."
      actions={
        editor && (
          <Button variant={enabled ? "ghost" : "secondary"} disabled={blocked !== null} onClick={() => setOpen(true)}>
            {enabled ? "Disable pooler" : "Enable pooler"}
          </Button>
        )
      }
    >
      <div className="space-y-2">
        <StatusLabel tone={status.tone} label={status.label} pulse={status.tone === "info"} />
        {enabled && (
          <DefinitionList
            compact
            items={[
              { term: "Service and port", value: <span className="font-mono text-xs">{poolerService}:{poolerPort}</span> },
              { term: "Private hostname", value: <span className="font-mono text-xs break-all">{details.database.pooler_host}</span> },
            ]}
          />
        )}
        <p className="text-xs text-fg-muted">
          Transaction pooling gives a client a server connection for one transaction at a time, so session state such as SET, advisory locks and session-level prepared statements does not carry over. To use it, bind the DB_POOL_* exports on the consumer and create a private connection to the pooler endpoint, then deploy the consumer. Rotation updates both the direct and pooled credentials.
        </p>
      </div>
      {open && <PoolerDialog workload={workload} details={details} enabled={enabled} onClose={() => setOpen(false)} />}
    </Subsection>
  );
}

function QueuedSetting({ operationId, onClose, children }: { operationId: string; onClose: () => void; children: ReactNode }) {
  return (
    <div className="mt-3 space-y-4">
      <Notice tone="success" title="Settings change queued">
        {children}
      </Notice>
      <div className="flex flex-wrap items-center gap-3">
        <Link to="/operations/$operationId" params={{ operationId }} className={buttonClass("primary")} onClick={onClose}>
          View operation
        </Link>
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      </div>
    </div>
  );
}

function PoolerDialog({ workload: liveWorkload, details: liveDetails, enabled: liveEnabled, onClose }: { workload: Workload; details: DatabaseDetails; enabled: boolean; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const enabled = useReviewed(liveEnabled);
  const client = useQueryClient();
  const save = usePoolerSetting(workload.id);
  const [confirmed, setConfirmed] = useState(false);
  const online = useOnline();
  const next = !enabled;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!confirmed) return;
    save.mutate(
      { enabled: next, revision: details.revision },
      {
        onError: (error) => {
          if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workload.id) });
        },
      },
    );
  }

  return (
    <Dialog open title={next ? "Enable the connection pooler?" : "Disable the connection pooler?"} onClose={onClose}>
      {save.data ? (
        <QueuedSetting operationId={save.data.id} onClose={onClose}>
          The pooler setting is saved. It applies when the operation succeeds.
        </QueuedSetting>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit}>
          {next ? (
            <p>
              Adds PgBouncer to {workload.name} ({engineName(details.database.engine)} {details.database.version}) on <span className="font-mono">{poolerService}:{poolerPort}</span> and exports DB_POOL_* variables. Deploying the change may briefly interrupt clients. Existing clients keep connecting directly until you switch them.
            </p>
          ) : (
            <p>
              Removes PgBouncer from {workload.name}. Clients using the pooler lose their connection. Cove refuses this while any private connection or variable binding still uses the pooler; move those clients to the direct connection first.
            </p>
          )}
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />
            <span>{next ? "Deploy the pooler. Clients may reconnect." : "Remove the pooler. Clients still using it will disconnect."}</span>
          </label>
          {save.isError && (
            <Notice tone="error" title="Could not change the pooler">
              {errorMessage(save.error)}
              {isStatus(save.error, 409) ? " The current details have been reloaded; close this dialog and review them before trying again." : ""}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant={next ? "primary" : "danger"} disabled={!confirmed || !online} pending={save.isPending}>
              {next ? "Enable pooler" : "Disable pooler"}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

type PublicDialog = "enable" | "disable" | null;

function PublicAccessSetting({ workload, details, editor, blocked }: { workload: Workload; details: DatabaseDetails; editor: boolean; blocked: string | null }) {
  const [dialog, setDialog] = useState<PublicDialog>(null);
  const access = details.database.public_access;
  const status = access ? settingTone(true, details) : settingsFailed(details.settings_state) ? settingTone(false, details) : { tone: "neutral" as Tone, label: "Private only" };
  return (
    <Subsection
      title={
        <span className="inline-flex items-center gap-2">
          {access ? <Globe aria-hidden className="size-3.5 text-fg-subtle" /> : <Lock aria-hidden className="size-3.5 text-fg-subtle" />}
          Public TLS access
        </span>
      }
      description="Publishes the native listener on a worker address with mandatory TLS, using a certificate you provide."
      actions={
        editor &&
        (access ? (
          <>
            <Button disabled={blocked !== null} onClick={() => setDialog("enable")}>
              Replace certificate
            </Button>
            <Button variant="danger" disabled={blocked !== null} onClick={() => setDialog("disable")}>
              Disable public access
            </Button>
          </>
        ) : (
          <Button disabled={blocked !== null} onClick={() => setDialog("enable")}>
            Enable public access
          </Button>
        ))
      }
    >
      <div className="space-y-2">
        <StatusLabel tone={status.tone} label={status.label} pulse={status.tone === "info"} />
        {access ? (
          <>
            <DefinitionList
              compact
              items={[
                { term: "Hostname", value: <span className="font-mono text-xs break-all">{access.hostname}</span> },
                { term: "Listen address", value: <span className="font-mono text-xs break-all">{access.bind_address.includes(":") ? `[${access.bind_address}]` : access.bind_address}:{access.port}</span> },
                { term: "Certificate SHA-256", value: <span className="font-mono text-xs break-all">{access.fingerprint}</span> },
                {
                  term: "Certificate",
                  value: (
                    <span className="text-xs" title={formatAbsolute(access.expires_at)}>
                      <ExpiryTime value={access.expires_at} />
                    </span>
                  ),
                },
                ...(details.database.private_host ? [{ term: "Private hostname", value: <span className="font-mono text-xs break-all">{details.database.private_host}</span> }] : []),
              ]}
            />
            <p className="text-xs text-fg-muted">
              DB_HOST and DB_URL use the certificate hostname, and bound consumers receive the CA certificate at <span className="font-mono">/run/secrets/cove_ca_{workload.id}</span> on their next deploy. Configure each client driver to require TLS and verify the server; connection URL options differ between drivers.
            </p>
            {details.tls_ca && <CertificateAuthority pem={details.tls_ca} />}
          </>
        ) : (
          <p className="text-xs text-fg-muted">Reachable only through private connections and temporary access. Clones, previews and copies always start private.</p>
        )}
      </div>
      {dialog === "enable" && <PublicAccessDialog workload={workload} details={details} onClose={() => setDialog(null)} />}
      {dialog === "disable" && <DisablePublicDialog workload={workload} details={details} onClose={() => setDialog(null)} />}
    </Subsection>
  );
}

function CertificateAuthority({ pem }: { pem: string }) {
  return (
    <details className="rounded-md border border-line">
      <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-fg">CA certificate for clients (public)</summary>
      <div className="space-y-2 border-t border-line px-3 py-2">
        <pre role="region" tabIndex={0} aria-label="CA certificate" className="max-h-48 overflow-auto font-mono text-[11px] whitespace-pre text-fg-muted">
          {pem}
        </pre>
        <CopyButton text={pem} label="Copy CA certificate" variant="ghost" />
      </div>
    </details>
  );
}

function DisablePublicDialog({ workload: liveWorkload, details: liveDetails, onClose }: { workload: Workload; details: DatabaseDetails; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const client = useQueryClient();
  const save = usePublicAccess(workload.id, () => ({ enabled: false, revision: details.revision }));
  const online = useOnline();
  const [confirmed, setConfirmed] = useState(false);
  const host = details.database.private_host || details.database.host;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!confirmed) return;
    save.mutate(undefined, {
      onError: (error) => {
        if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workload.id) });
      },
    });
  }

  return (
    <Dialog open title="Disable public TLS access?" onClose={onClose}>
      {save.data ? (
        <QueuedSetting operationId={save.data.id} onClose={onClose}>
          Public access is turned off in the saved configuration. The listener closes when the operation succeeds.
        </QueuedSetting>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit}>
          <p>
            Closes the public listener of {workload.name}, removes its certificate and key, and returns it to its original private-only configuration. DB_HOST and DB_URL return to <span className="font-mono break-all">{host}</span> and private connections are updated. External clients lose access; deploy bound consumers again afterwards.
          </p>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />
            <span>Close public access. Clients outside the private network disconnect.</span>
          </label>
          {save.isError && (
            <Notice tone="error" title="Could not disable public access">
              {errorMessage(save.error)}
              {isStatus(save.error, 409) ? " The current details have been reloaded; close this dialog and review them before trying again." : ""}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" disabled={!confirmed || !online} pending={save.isPending}>
              Disable public access
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

const emptyDraft = (details: DatabaseDetails): PublicAccessDraft => ({
  hostname: details.database.public_access?.hostname ?? "",
  bind_address: details.database.public_access?.bind_address ?? "",
  port: String(details.database.public_access?.port ?? engineDefaultPublicPort(details.database.engine, details.database.port)),
  certificate: "",
  private_key: "",
  ca_certificate: "",
});

// Certificate material lives only in this component's state; it is cleared on success and discarded on close.
function PublicAccessDialog({ workload: liveWorkload, details: liveDetails, onClose }: { workload: Workload; details: DatabaseDetails; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [draft, setDraft] = useState<PublicAccessDraft>(() => emptyDraft(details));
  const save = usePublicAccess(workload.id, () => ({
    enabled: true,
    revision: details.revision,
    hostname: draft.hostname.trim().toLowerCase(),
    bind_address: draft.bind_address.trim(),
    port: Number(draft.port),
    certificate: draft.certificate.trim(),
    private_key: normalisePem(draft.private_key),
    ...(draft.ca_certificate.trim() ? { ca_certificate: draft.ca_certificate.trim() } : {}),
  }));
  const [errors, setErrors] = useState<PublicAccessErrors>({});
  const [acknowledged, setAcknowledged] = useState(false);
  const [queued, setQueued] = useState<string | null>(null);
  const replacing = Boolean(details.database.public_access);
  const set = (key: keyof PublicAccessDraft) => (value: string) => setDraft((current) => ({ ...current, [key]: value }));

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const normalised = { ...draft, private_key: normalisePem(draft.private_key) };
    const next = validatePublicAccess(normalised);
    setErrors(next);
    if (Object.keys(next).length > 0) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    if (!acknowledged) return;
    save.mutate(undefined, {
      onSuccess: (data) => {
        setQueued(data.id);
        setDraft(emptyDraft(details));
        save.reset();
      },
      onError: (error) => {
        if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workload.id) });
      },
    });
  }

  const textField = (key: "hostname" | "bind_address" | "port", label: string, hint: string, extra: Record<string, string> = {}) => (
    <Field id={`${id}-${key}`} label={label} hint={hint} error={errors[key]}>
      <input
        id={`${id}-${key}`}
        className={`${inputClass} font-mono`}
        value={draft[key]}
        onChange={(event) => set(key)(event.target.value)}
        autoComplete="off"
        spellCheck={false}
        aria-invalid={errors[key] ? true : undefined}
        aria-describedby={describedBy(`${id}-${key}`, errors[key], true)}
        {...extra}
      />
    </Field>
  );

  return (
    <Dialog open title={replacing ? "Replace the public TLS certificate?" : "Enable public TLS access?"} onClose={onClose}>
      {queued ? (
        <QueuedSetting operationId={queued} onClose={onClose}>
          The certificate is sealed and the listener is configured when the operation succeeds. The private key is not shown again.
        </QueuedSetting>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit} noValidate>
          <Notice tone="warn" title="This publishes the database outside the private network">
            The native {engineName(details.database.engine)} listener of {workload.name} becomes reachable on the address below. You manage DNS and firewall rules for it. TLS is mandatory and clients must verify the server with the CA certificate. DB_HOST and DB_URL switch to this hostname and existing private connections are updated to it.
          </Notice>
          {textField("hostname", "Hostname", "The DNS name in the certificate. Clients connect to it, so its DNS record must point at the worker.")}
          <div className="grid gap-4 sm:grid-cols-2">
            {textField("bind_address", "Bind address", "A worker IP address. 0.0.0.0 or :: listens on every interface.", { placeholder: "203.0.113.10" })}
            {textField("port", "Public port", "Host port on the worker, 1024 to 65535.", { inputMode: "numeric" })}
          </div>
          <Field id={`${id}-certificate`} label="Server certificate (PEM)" hint="The leaf certificate first, followed by any intermediates. Its DNS name must match the hostname and stay valid for at least 24 hours." error={errors.certificate}>
            <textarea
              id={`${id}-certificate`}
              className={`${inputClass} font-mono text-xs`}
              rows={4}
              value={draft.certificate}
              onChange={(event) => set("certificate")(event.target.value)}
              spellCheck={false}
              aria-invalid={errors.certificate ? true : undefined}
              aria-describedby={describedBy(`${id}-certificate`, errors.certificate, true)}
            />
          </Field>
          <Field id={`${id}-key`} label="Private key (PEM)" hint="Write-only. It is sealed on the control plane and never displayed or returned." error={errors.private_key}>
            <input
              id={`${id}-key`}
              type="password"
              className={`${inputClass} font-mono`}
              value={draft.private_key}
              onChange={(event) => set("private_key")(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={errors.private_key ? true : undefined}
              aria-describedby={describedBy(`${id}-key`, errors.private_key, true)}
            />
          </Field>
          <Field id={`${id}-ca`} label="CA certificates (PEM)" optional hint="Include the root. Leave empty when the server certificate is self-signed; it then acts as its own CA." error={errors.ca_certificate}>
            <textarea
              id={`${id}-ca`}
              className={`${inputClass} font-mono text-xs`}
              rows={3}
              value={draft.ca_certificate}
              onChange={(event) => set("ca_certificate")(event.target.value)}
              spellCheck={false}
              aria-invalid={errors.ca_certificate ? true : undefined}
              aria-describedby={describedBy(`${id}-ca`, errors.ca_certificate, true)}
            />
          </Field>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
            <span>I manage DNS and firewall rules for this listener, and clients will verify TLS with the CA certificate.</span>
          </label>
          {save.isError && (
            <Notice tone="error" title="Could not save public access">
              {errorMessage(save.error)}
              {isStatus(save.error, 409) ? " The current details have been reloaded; close this dialog and review them before trying again." : ""}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" disabled={!acknowledged || !online} pending={save.isPending}>
              {replacing ? "Replace certificate" : "Publish database"}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

export function Dependents({ details }: { details: DatabaseDetails }) {
  const dependents = details.dependents ?? [];
  const hidden = details.hidden_dependents ?? 0;
  const pending = dependents.filter((item) => item.needs_redeploy).length;
  return (
    <Section
      title="Bound resources"
      description="Resources that bind this database's variables. After a rotation, settings change or cut-over, deploy each one marked below so it resolves the new values. Cove does not redeploy them."
      flush
    >
      {dependents.length === 0 && hidden === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">No resource binds this database's variables.</p>
      ) : (
        <div>
          {pending > 0 && (
            <p className="px-4 pt-3 text-xs text-fg-muted" role="status">
              {pending === 1 ? "1 resource needs" : `${pending} resources need`} a deploy.
            </p>
          )}
          <ul aria-label="Bound resources" className="divide-y divide-line">
            {dependents.map((item) => (
              <li key={item.id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-2.5">
                <Link to="/resources/$resourceId" params={{ resourceId: item.id }} className={`${linkClass} min-w-0 text-[13px] break-words`}>
                  {item.name}
                </Link>
                <StatusLabel tone={item.needs_redeploy ? "warn" : "ok"} label={item.needs_redeploy ? "Deploy needed" : "Up to date"} />
              </li>
            ))}
            {hidden > 0 && (
              <li className="px-4 py-2.5 text-xs text-fg-muted">
                {hidden === 1 ? "1 more resource you cannot view also binds" : `${hidden} more resources you cannot view also bind`} this database.
              </li>
            )}
          </ul>
        </div>
      )}
    </Section>
  );
}
