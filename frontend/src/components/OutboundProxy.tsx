import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { outboundProxyQuery, useUpdateOutboundProxy, type OutboundProxyInput, type OutboundProxySettings } from "../api/instance-settings";
import { useOnline } from "../lib/hooks";
import { proxyOriginError } from "../lib/security-settings";
import { describedBy, inputClass } from "../lib/styles";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const title = "Outbound proxy";
const description =
  "Send Cove's outbound HTTP and HTTPS requests through a proxy, including Git, registry, update, provider, webhook and audit export traffic. Saved values replace HTTP_PROXY, HTTPS_PROXY and NO_PROXY from the host environment.";

type ProxyKey = "http_proxy" | "https_proxy";
// Undefined keeps the saved URL and its credentials; the API never returns credentials, so an untouched URL is never sent back.
type ProxyEdit = { mode: "replace"; value: string } | { mode: "clear" };

const proxyFields: { key: ProxyKey; label: string; credentials: "http_credentials_set" | "https_credentials_set"; hint: string }[] = [
  { key: "http_proxy", label: "HTTP proxy", credentials: "http_credentials_set", hint: "Used for http:// destinations." },
  { key: "https_proxy", label: "HTTPS proxy", credentials: "https_credentials_set", hint: "Used for https:// destinations, through CONNECT." },
];

function ProxyField({
  field,
  saved,
  edit,
  error,
  onEdit,
}: {
  field: (typeof proxyFields)[number];
  saved: OutboundProxySettings;
  edit: ProxyEdit | undefined;
  error: string | undefined;
  onEdit: (edit: ProxyEdit | undefined) => void;
}) {
  const id = `outbound-${field.key}`;
  const origin = saved[field.key];
  const credentials = saved[field.credentials];
  if (edit?.mode === "replace") {
    const hint = `${field.hint} Enter the full URL, for example http://user:password@proxy.example.com:3128. The value is hidden because it may contain credentials and replaces the saved URL when you save.`;
    return (
      <Field id={id} layout="row" label={field.label} hint={hint} error={error}>
        <div className="flex flex-wrap items-center gap-2">
          <input
            id={id}
            className={`${inputClass} min-w-0 flex-1 basis-56 font-mono`}
            type="password"
            autoComplete="off"
            spellCheck={false}
            placeholder="http://proxy.example.com:3128"
            value={edit.value}
            onChange={(event) => onEdit({ mode: "replace", value: event.target.value })}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(id, error, hint)}
          />
          <Button variant="ghost" aria-label={`Keep the saved ${field.label}`} onClick={() => onEdit(undefined)}>
            Keep saved
          </Button>
        </div>
      </Field>
    );
  }
  const description = edit?.mode === "clear" ? `${field.hint} The saved URL${credentials ? " and its credentials" : ""} will be removed when you save.` : field.hint;
  return (
    <SettingRow label={field.label} description={description}>
      <div className="min-w-0 flex-1 basis-48 space-y-0.5 @lg:text-right">
        {edit?.mode === "clear" ? (
          <StatusLabel tone="warn" label="Cleared when saved" />
        ) : origin ? (
          <>
            <p className="font-mono text-xs break-all text-fg">{origin}</p>
            <p className="text-xs text-fg-muted">{credentials ? "Credentials saved" : "No credentials"}</p>
          </>
        ) : (
          <StatusLabel tone="neutral" label="Not set" />
        )}
      </div>
      <div className="flex gap-2">
        {edit?.mode === "clear" ? (
          <Button size="sm" variant="ghost" aria-label={`Undo clearing the ${field.label}`} onClick={() => onEdit(undefined)}>
            Undo
          </Button>
        ) : (
          <>
            <Button size="sm" aria-label={`${origin ? "Replace" : "Set"} the ${field.label}`} onClick={() => onEdit({ mode: "replace", value: "" })}>
              {origin ? "Replace" : "Set"}
            </Button>
            {origin && (
              <Button size="sm" variant="ghost" aria-label={`Clear the ${field.label}`} onClick={() => onEdit({ mode: "clear" })}>
                Clear
              </Button>
            )}
          </>
        )}
      </div>
    </SettingRow>
  );
}

function OutboundProxyForm({ saved }: { saved: OutboundProxySettings }) {
  const update = useUpdateOutboundProxy();
  const online = useOnline();
  const [edits, setEdits] = useState<Partial<Record<ProxyKey, ProxyEdit>>>({});
  const [noProxy, setNoProxy] = useState<string>();
  const [errors, setErrors] = useState<Partial<Record<ProxyKey, string>>>({});
  const noProxyValue = noProxy ?? saved.no_proxy;
  const noProxyChanged = noProxy !== undefined && noProxy.trim() !== saved.no_proxy;
  const dirty = Object.keys(edits).length > 0 || noProxyChanged;
  const noProxyHint = "Comma-separated hosts that skip the proxy: *, IP addresses, CIDR ranges, domain suffixes and .subdomain matches, optionally with a port. Loopback and localhost always skip it.";

  function reset() {
    setEdits({});
    setNoProxy(undefined);
    setErrors({});
  }

  function editProxy(key: ProxyKey, edit: ProxyEdit | undefined) {
    update.reset();
    setErrors((current) => ({ ...current, [key]: undefined }));
    setEdits((current) => {
      const next = { ...current };
      if (edit) next[key] = edit;
      else delete next[key];
      return next;
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!dirty) return;
    const input: OutboundProxyInput = {};
    const problems: Partial<Record<ProxyKey, string>> = {};
    for (const { key } of proxyFields) {
      const edit = edits[key];
      if (!edit) continue;
      if (edit.mode === "clear") {
        input[key] = "";
        continue;
      }
      const problem = proxyOriginError(edit.value);
      if (problem) problems[key] = problem;
      else input[key] = edit.value.trim();
    }
    setErrors(problems);
    if (Object.keys(problems).length > 0) return;
    if (noProxyChanged) input.no_proxy = noProxyValue.trim();
    update.mutate(input, { onSuccess: reset });
  }

  const saveError = update.isError ? (isStatus(update.error, 400) ? "Check that each proxy is an http:// or https:// origin and that the NO_PROXY list is valid. " : "") + errorMessage(update.error) : undefined;

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title={title}
        description={description}
        width="form"
        footer={
          <SaveBar
            dirty={dirty}
            pending={update.isPending}
            blocked={!online}
            onDiscard={() => {
              update.reset();
              reset();
            }}
            status={update.isSuccess ? "Outbound proxy saved" : undefined}
          />
        }
      >
        <fieldset disabled={!online || update.isPending} className="min-w-0 space-y-4">
          <SettingRows>
            {proxyFields.map((field) => (
              <ProxyField key={field.key} field={field} saved={saved} edit={edits[field.key]} error={errors[field.key]} onEdit={(edit) => editProxy(field.key, edit)} />
            ))}
            <Field id="outbound-no-proxy" layout="row" label="Bypass the proxy for" optional hint={noProxyHint}>
              <input
                id="outbound-no-proxy"
                className={`${inputClass} font-mono`}
                type="text"
                autoComplete="off"
                spellCheck={false}
                placeholder="10.0.0.0/8, .internal.example.com"
                value={noProxyValue}
                onChange={(event) => {
                  update.reset();
                  setNoProxy(event.target.value);
                }}
                aria-describedby="outbound-no-proxy-hint"
              />
            </Field>
          </SettingRows>
          {saveError && (
            <Notice tone="error" title="Could not save the outbound proxy">
              {saveError}
            </Notice>
          )}
          <div className="space-y-1.5 text-xs text-fg-muted">
            <p>The proxy must allow CONNECT, including to port 80 for plain HTTP destinations. Webhook and audit destinations are still validated by Cove before the proxy is asked to connect.</p>
            <p>
              Docker image pulls run in dockerd, which does not use this setting. Configure dockerd on each node separately during a planned maintenance window; Docker must be restarted for it to take effect, and Cove
              does not restart it.
            </p>
          </div>
        </fieldset>
      </Section>
    </form>
  );
}

export function OutboundProxy() {
  const settings = useQuery(outboundProxyQuery);
  if (settings.isPending) {
    return (
      <Section title={title} width="form">
        <LoadingRows label="Loading outbound proxy settings" rows={3} />
      </Section>
    );
  }
  if (settings.isError) {
    return (
      <Section title={title} width="form">
        <QueryError what="outbound proxy settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
      </Section>
    );
  }
  return <OutboundProxyForm saved={settings.data} />;
}
