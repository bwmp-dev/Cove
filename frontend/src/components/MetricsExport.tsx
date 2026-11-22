import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { errorMessage, request } from "../api/client";
import { metricsTokenQuery } from "../api/outputs";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

function scrapeConfig(origin: string) {
  const url = new URL(origin);
  return [
    "scrape_configs:",
    "  - job_name: cove",
    `    scheme: ${url.protocol.replace(":", "")}`,
    "    metrics_path: /metrics",
    "    authorization:",
    "      type: Bearer",
    "      credentials_file: /etc/prometheus/cove-token",
    "    static_configs:",
    `      - targets: ["${url.host}"]`,
  ].join("\n");
}

function IssuedToken({ token, onDone }: { token: string; onDone: () => void }) {
  const field = useRef<HTMLInputElement>(null);
  const [visible, setVisible] = useState(false);
  // The rotate button that held focus is replaced by the one-time token, so move focus to it once.
  useEffect(() => field.current?.focus(), []);
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <Notice tone="warn" title="Copy this token now">It is shown only once and replaces any previous token, which stops working immediately.</Notice>
      <Field id="metrics-token-value" label="Scrape token">
        <input id="metrics-token-value" ref={field} className={`${inputClass} font-mono`} type={visible ? "text" : "password"} readOnly value={token} autoComplete="off" spellCheck={false} />
      </Field>
      <div className="flex flex-wrap items-center gap-2">
        <CopyButton text={token} label="Copy token" />
        <Button variant="ghost" onClick={() => setVisible(!visible)} aria-pressed={visible}>{visible ? "Hide" : "Show"}</Button>
        <Button onClick={onDone}>Done</Button>
      </div>
    </div>
  );
}

function TokenRow({ configured, disabled, onRevoke, onRotate }: { configured: boolean; disabled: boolean; onRevoke: () => void; onRotate: () => void }) {
  return (
    <SettingRow label="Scrape token" description={configured ? "Only a hash is stored. Rotating issues a new token and disables the old one." : "/metrics refuses every request until a token exists."}>
      <div className="flex flex-wrap items-center justify-end gap-2">
        <StatusLabel tone={configured ? "ok" : "neutral"} label={configured ? "Configured" : "Not configured"} />
        {configured && <Button variant="ghost" disabled={disabled} onClick={onRevoke}>Revoke</Button>}
        <Button variant="primary" disabled={disabled} onClick={onRotate}>{configured ? "Rotate token" : "Create token"}</Button>
      </div>
    </SettingRow>
  );
}

function ScrapeGuide({ origin }: { origin: string }) {
  return (
    <div className="space-y-2 border-t border-line pt-4">
      <h3 className="text-[13px] font-semibold text-fg">Prometheus configuration</h3>
      <p className="text-xs text-fg-muted">Store the token in a file readable only by Prometheus, then point credentials_file at it. Keep it out of the configuration itself.</p>
      <pre className="overflow-x-auto rounded-md border border-line bg-panel p-3 font-mono text-xs text-fg">{scrapeConfig(origin)}</pre>
      <p className="text-xs text-fg-muted">
        The export includes panel process statistics, operation record counts and node and container CPU, memory, disk, network and block I/O counters reported in the last five minutes, with their sample timestamps. Missing or stale readings are omitted rather than reported as zero, and output stops after 10,000 sampled series groups, each emitting several metric samples, with cove_export_truncated set. Scrapes run one at a time, so a concurrent scrape is refused, and the output is capped at 8 MiB.
      </p>
    </div>
  );
}

export function MetricsExport() {
  const client = useQueryClient();
  const online = useOnline();
  const settings = useQuery(metricsTokenQuery);
  const [confirm, setConfirm] = useState<"rotate" | "revoke" | null>(null);
  const rotate = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ token: string }>("POST", "/api/settings/metrics-token"),
    onSuccess: () => {
      setConfirm(null);
      client.setQueryData(metricsTokenQuery.queryKey, { configured: true });
    },
  });
  const revoke = useMutation({
    mutationFn: () => request<{ configured: boolean }>("DELETE", "/api/settings/metrics-token"),
    onSuccess: (data) => {
      setConfirm(null);
      rotate.reset();
      client.setQueryData(metricsTokenQuery.queryKey, data);
    },
  });
  const origin = window.location.origin;
  const busy = rotate.isPending || revoke.isPending;

  let body;
  if (settings.isPending) {
    body = <LoadingRows label="Loading metrics export settings" rows={2} />;
  } else if (settings.isError) {
    body = <QueryError what="metrics export settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  } else {
    body = (
      <div className="space-y-4">
        <SettingRows>
          <TokenRow
            configured={settings.data.configured}
            disabled={!online || busy}
            onRevoke={() => { revoke.reset(); setConfirm("revoke"); }}
            onRotate={() => { rotate.reset(); setConfirm("rotate"); }}
          />
          <SettingRow label="Scrape URL" description="Send the token as Authorization: Bearer. Session cookies, API tokens and query-string tokens are refused.">
            <span className="font-mono text-xs break-all">{origin}/metrics</span>
          </SettingRow>
        </SettingRows>
        {rotate.data && <IssuedToken token={rotate.data.token} onDone={() => rotate.reset()} />}
        {revoke.isSuccess && <Notice tone="success" title="Token revoked">/metrics now refuses every request.</Notice>}
        {(rotate.isError || revoke.isError) && <Notice tone="error" title="Could not change the token">{errorMessage(rotate.error ?? revoke.error)}</Notice>}
        <ScrapeGuide origin={origin} />
      </div>
    );
  }

  return (
    <Section title="Prometheus metrics" description="A read-only scrape endpoint for instance-wide telemetry. It uses its own token, separate from sign-in and API tokens." width="form">
      {body}
      <ConfirmDialog
        open={confirm === "rotate"}
        title={settings.data?.configured ? "Rotate the scrape token?" : "Create a scrape token?"}
        confirmLabel={settings.data?.configured ? "Rotate token" : "Create token"}
        pending={rotate.isPending}
        onCancel={() => setConfirm(null)}
        onConfirm={() => rotate.mutate()}
      >
        {settings.data?.configured ? "The current token stops working immediately, so Prometheus fails to scrape until you give it the new one. " : ""}
        The new token is shown once on this page and is not stored in the browser.
        {rotate.isError && <span className="mt-2 block text-bad">{errorMessage(rotate.error)}</span>}
      </ConfirmDialog>
      <ConfirmDialog open={confirm === "revoke"} title="Revoke the scrape token?" confirmLabel="Revoke token" danger pending={revoke.isPending} onCancel={() => setConfirm(null)} onConfirm={() => revoke.mutate()}>
        /metrics refuses every request until a new token is created.
        {revoke.isError && <span className="mt-2 block text-bad">{errorMessage(revoke.error)}</span>}
      </ConfirmDialog>
    </Section>
  );
}
