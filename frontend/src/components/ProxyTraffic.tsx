import { useMutation, useQuery, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { nodeTrafficQuery, type ProxyTrafficResponse, type ProxyTrafficState, type TrafficSettings } from "../api/outputs";
import type { Node } from "../api/types";
import { useOnline } from "../lib/hooks";
import { maxTrustedProxies, parseTrustedProxies, sameProxies } from "../lib/outputs";
import { describedBy, inputClass } from "../lib/styles";
import { Dialog } from "./ConfirmDialog";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

type Plan = NonNullable<ProxyTrafficResponse["plan"]>;
type Control = { action: "preview"; settings: TrafficSettings } | { action: "apply"; settings: TrafficSettings; revision: string } | { action: "recover" };

const description =
  "Records each request's time, domain, status, size, duration and client address in the managed proxy's access log, so service owners see traffic on their Metrics page. Paths, query strings, authorization and other headers are left out. The proxy's local log keeps X-Forwarded-For, bounded to 12 MiB on the node, only to derive the client address; Cove stores just that address.";

function proxiesSummary(proxies: readonly string[] | null | undefined) {
  return proxies && proxies.length > 0 ? proxies.join(", ") : "None. Client addresses come from the connection itself.";
}

function ApplyDialog({ node, plan, pending, error, onApply, onClose }: { node: Node; plan: Plan | null; pending: boolean; error: unknown; onApply: () => void; onClose: () => void }) {
  const [typed, setTyped] = useState("");
  const id = useId();
  const online = useOnline();
  return (
    <Dialog open={plan !== null} title={`Restart the proxy on ${node.name}?`} onClose={onClose}>
      <form
        className="space-y-3"
        onSubmit={(event) => {
          event.preventDefault();
          if (typed === node.name && online && !pending) onApply();
        }}
      >
        <p>
          Cove starts a replacement proxy with the same image, configuration, networks and certificates, checks that it is ready and routes every domain, then removes the old one. Every team's sites on this node are briefly interrupted. If the replacement fails, the old proxy is restored.
        </p>
        {plan && (
          <DefinitionList
            compact
            items={[
              { term: "Access logging", value: plan.settings.enabled ? "On" : "Off" },
              { term: "Trusted proxies", value: <span className="break-all">{proxiesSummary(plan.settings.trusted_proxies)}</span> },
            ]}
          />
        )}
        <p>The change is not retried automatically.</p>
        <Field id={id} label={`Type ${node.name} to confirm`}>
          <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required disabled={pending} />
        </Field>
        {error !== null && <Notice tone="error" title="Proxy change failed">{errorMessage(error)} The restart may have started. Refresh the state, and use Recover if it shows an interrupted change.</Notice>}
        <div className="flex flex-wrap justify-end gap-2 pt-1">
          <Button variant="ghost" onClick={onClose} disabled={pending}>Cancel</Button>
          <Button type="submit" variant="danger" disabled={!online || typed !== node.name} pending={pending}>Restart proxy</Button>
        </div>
      </form>
    </Dialog>
  );
}

function CurrentState({ state }: { state: ProxyTrafficState }) {
  return (
    <DefinitionList
      compact
      items={[
        { term: "Saved setting", value: <StatusLabel tone={state.settings.enabled ? "ok" : "neutral"} label={state.settings.enabled ? "Access logging on" : "Access logging off"} /> },
        { term: "Running proxy", value: state.configured ? "Writes an access log" : "Does not write an access log" },
        { term: "Trusted proxies", value: <span className="break-all">{proxiesSummary(state.settings.trusted_proxies)}</span> },
      ]}
    />
  );
}

// Remounted (keyed by the saved settings) whenever the worker reports new ones, so the inputs start from them.
function TrafficForm({ node, initialState: state, onPlan, disabled }: { node: Node; initialState: ProxyTrafficState; onPlan: (plan: Plan) => void; disabled: boolean }) {
  const id = useId();
  const online = useOnline();
  const [enabled, setEnabled] = useState(() => state.settings.enabled);
  const [proxies, setProxies] = useState(() => (state.settings.trusted_proxies ?? []).join("\n"));
  const [error, setError] = useState<string>();
  const preview = useMutation({
    mutationFn: (settings: TrafficSettings) => request<ProxyTrafficResponse>("POST", `/api/nodes/${encodeURIComponent(node.id)}/traffic`, { action: "preview", settings } satisfies Control),
    onSuccess: (response) => {
      if (response.plan) onPlan(response.plan);
    },
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseTrustedProxies(proxies);
    if ("error" in parsed) {
      setError(parsed.error);
      document.getElementById(`${id}-proxies`)?.focus();
      return;
    }
    setError(undefined);
    preview.mutate({ enabled, trusted_proxies: parsed.proxies });
  }

  const parsed = parseTrustedProxies(proxies);
  const unchanged = enabled === state.settings.enabled && "proxies" in parsed && sameProxies(parsed.proxies, state.settings.trusted_proxies) && state.configured === state.settings.enabled;

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <fieldset disabled={disabled || preview.isPending} className="min-w-0 space-y-4">
        <SwitchField id={`${id}-enabled`} label="Record access logs" description="Logs rotate at 4 MiB with three files kept on the node." checked={enabled} onChange={(event) => { setEnabled(event.target.checked); preview.reset(); }} />
        <Field
          id={`${id}-proxies`}
          label="Trusted proxies"
          optional
          error={error}
          hint={`Only for a load balancer or CDN in front of this node. Cove reads the client from X-Forwarded-For only when the connection comes from one of these ranges, checking right to left. Leave empty when visitors connect directly. One CIDR per line, up to ${maxTrustedProxies}; all-address ranges such as 0.0.0.0/0 are refused.`}
        >
          <textarea
            id={`${id}-proxies`}
            className={`${inputClass} min-h-24 font-mono`}
            value={proxies}
            onChange={(event) => { setProxies(event.target.value); setError(undefined); preview.reset(); }}
            spellCheck={false}
            autoComplete="off"
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(`${id}-proxies`, error, true)}
          />
        </Field>
      </fieldset>
      {preview.isError && (
        <Notice tone="error" title="Could not preview the change">
          {errorMessage(preview.error)}
          {isStatus(preview.error, 409) && " Refresh the state, and recover any interrupted change first."}
        </Notice>
      )}
      <div className="flex flex-wrap items-center justify-end gap-2">
        {unchanged && <span className="text-xs text-fg-muted">The proxy already runs with these settings.</span>}
        <Button type="submit" variant="primary" pending={preview.isPending} disabled={!online || disabled || unchanged}>Review change</Button>
      </div>
    </form>
  );
}

type ControlMutation = UseMutationResult<ProxyTrafficResponse, Error, Control>;

function TrafficStateView({ node, state, control, applied, planOpen, onRecover, onPlan }: { node: Node; state: ProxyTrafficState; control: ControlMutation; applied: "apply" | "recover" | null; planOpen: boolean; onRecover: () => void; onPlan: (plan: Plan) => void }) {
  const online = useOnline();
  const failed = control.isError ? control.variables.action : null;
  return (
    <div className="space-y-4">
      {state.pending && (
        <Notice tone="warn" title="A proxy change was interrupted" action={<Button onClick={onRecover} disabled={!online || control.isPending}>Recover</Button>}>
          The worker kept a record of the unfinished restart. Recover completes or rolls it back; no other change is accepted until then.
        </Notice>
      )}
      {state.notice && !state.pending && <Notice tone="warn">{state.notice}</Notice>}
      {applied && !control.isError && (
        <Notice tone="success" title={applied === "recover" ? "Recovery finished" : "Proxy restarted"}>
          {applied === "recover" ? "The interrupted change was resolved." : "The proxy is running with the new access log settings."}
        </Notice>
      )}
      {failed === "apply" && !planOpen && <Notice tone="error" title="Proxy change failed">{errorMessage(control.error)} The restart may have started. Refresh, and use Recover if the state shows an interrupted change.</Notice>}
      {failed === "recover" && <Notice tone="error" title="Recovery did not finish">{errorMessage(control.error)} The record is kept; refresh and try Recover again once active operations finish.</Notice>}
      <CurrentState state={state} />
      <div className="border-t border-line pt-4">
        <TrafficForm key={`${state.settings.enabled}-${(state.settings.trusted_proxies ?? []).join(",")}`} node={node} initialState={state} onPlan={onPlan} disabled={state.pending || control.isPending} />
      </div>
    </div>
  );
}

function TrafficControls({ node }: { node: Node }) {
  const client = useQueryClient();
  const online = useOnline();
  const traffic = useQuery(nodeTrafficQuery(node.id));
  const [plan, setPlan] = useState<Plan | null>(null);
  const [confirmRecover, setConfirmRecover] = useState(false);
  const [applied, setApplied] = useState<"apply" | "recover" | null>(null);
  const control = useMutation({
    mutationFn: (body: Control) => request<ProxyTrafficResponse>("POST", `/api/nodes/${encodeURIComponent(node.id)}/traffic`, body),
    onSuccess: (response, body) => {
      client.setQueryData(nodeTrafficQuery(node.id).queryKey, { state: response.state, plan: null });
      setApplied(body.action === "recover" ? "recover" : "apply");
      setPlan(null);
      setConfirmRecover(false);
    },
    // A rejected change may still have left a journal; read the state again instead of guessing.
    onError: () => void client.invalidateQueries({ queryKey: nodeTrafficQuery(node.id).queryKey }),
  });

  const refresh = (
    <Button variant="ghost" onClick={() => void traffic.refetch()} pending={traffic.isFetching}>
      {!traffic.isFetching && <RotateCw aria-hidden className="size-3.5" />}
      Refresh
    </Button>
  );

  let body;
  if (traffic.isPending) {
    body = <LoadingRows label="Loading access log settings" rows={2} />;
  } else if (traffic.isError && !traffic.data) {
    body = isStatus(traffic.error, 409) ? (
      <Notice tone="warn" title="Access logging is unavailable">{errorMessage(traffic.error)}</Notice>
    ) : (
      <QueryError what="access log settings" error={traffic.error} onRetry={() => void traffic.refetch()} retrying={traffic.isFetching} />
    );
  } else if (!traffic.data.state) {
    body = <Notice tone="warn" title="The worker reported no state">Refresh to read the proxy again.</Notice>;
  } else {
    body = (
      <TrafficStateView
        node={node}
        state={traffic.data.state}
        control={control}
        applied={applied}
        planOpen={plan !== null}
        onRecover={() => { control.reset(); setConfirmRecover(true); }}
        onPlan={(next) => { control.reset(); setApplied(null); setPlan(next); }}
      />
    );
  }

  return (
    <Section title="Access logging" description={description} actions={refresh} width="form">
      {body}
      <ApplyDialog
        key={plan?.revision ?? "closed"}
        node={node}
        plan={plan}
        pending={control.isPending && control.variables.action === "apply"}
        error={control.isError && control.variables?.action === "apply" ? control.error : null}
        onApply={() => plan && control.mutate({ action: "apply", settings: plan.settings, revision: plan.revision })}
        onClose={() => setPlan(null)}
      />
      <Dialog
        open={confirmRecover}
        title={`Recover the proxy on ${node.name}?`}
        onClose={() => !control.isPending && setConfirmRecover(false)}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRecover(false)} disabled={control.isPending}>Cancel</Button>
            <Button variant="danger" pending={control.isPending} disabled={!online} onClick={() => control.mutate({ action: "recover" })}>Recover proxy</Button>
          </>
        }
      >
        Cove resumes the recorded change: it keeps the replacement proxy if it was already confirmed, otherwise it restores the previous one. Sites on this node may be briefly interrupted. The worker refuses recovery while other operations are running on it.
        {control.isError && control.variables?.action === "recover" && <span className="mt-2 block text-bad">{errorMessage(control.error)}</span>}
      </Dialog>
    </Section>
  );
}

export function ProxyTraffic({ node }: { node: Node }) {
  if (!node.capabilities?.traffic_logs) {
    return (
      <Section title="Access logging" description={description} width="form">
        <Notice title="Not supported by this worker">Upgrade the worker on {node.name} and verify it again. Access logging needs a ready Linux worker that reports traffic collection support.</Notice>
      </Section>
    );
  }
  return <TrafficControls node={node} />;
}
