import { Checkbox } from "./Checkbox";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { useOnline } from "../lib/hooks";
import { useSession } from "../lib/session";
import { ChevronRight } from "lucide-react";
import { choiceClass, inputClass, linkClass } from "../lib/styles";
import { Button, Field, LoadingRows, Notice, QueryError, Spinner, Subsection } from "./ui";

interface Connection { id: string; name: string; kind: "token" | "oauth" }
interface AvailableZone { id: string; name: string }
interface OAuthInfo { configured: boolean; client_id?: string; redirect_uri?: string; scopes?: string }
const connectionsKey = ["cloudflare", "connections"];
const oauthKey = ["cloudflare", "oauth"];

function formText(data: FormData, key: string) {
  const value = data.get(key);
  return typeof value === "string" ? value.trim() : "";
}

export function CloudflareConnect() {
  const session = useSession();
  const online = useOnline();
  const connections = useQuery({ queryKey: connectionsKey, queryFn: ({ signal }) => request<Connection[]>("GET", "/api/cloudflare/connections", undefined, signal) });
  const oauth = useQuery({ queryKey: oauthKey, queryFn: ({ signal }) => request<OAuthInfo>("GET", "/api/cloudflare/oauth", undefined, signal) });
  const [selecting, setSelecting] = useState<string | null>(null);
  const [tokenForm, setTokenForm] = useState<Connection | "new" | null>(null);
  const connect = useMutation({
    mutationFn: () => request<{ url: string }>("POST", "/api/cloudflare/oauth/start", {}),
    onSuccess: result => {
      const url = new URL(result.url);
      if (url.origin !== "https://dash.cloudflare.com" || url.pathname !== "/oauth2/auth" || url.username || url.password) throw new Error("Unexpected Cloudflare authorization address");
      window.location.assign(url.href);
    },
    networkMode: "always",
  });
  const result = new URLSearchParams(window.location.search).get("cloudflare");
  if (tokenForm) return <TokenConnection initial={tokenForm === "new" ? undefined : tokenForm} onDone={id => { setTokenForm(null); if (id) setSelecting(id); }} />;
  if (selecting) return <SelectCloudflareZones id={selecting} onDone={() => setSelecting(null)} />;
  return <>
    <Subsection
      title="Your connections"
      description="Private to you until you share specific domains with a team. Credentials are never shared."
      actions={<>
        <Button variant="primary" disabled={!online || !oauth.data?.configured} pending={connect.isPending} onClick={() => connect.mutate()}>Connect with Cloudflare</Button>
        <Button disabled={!online} onClick={() => setTokenForm("new")}>Use API token</Button>
      </>}
    >
      {result === "connected" && <Notice tone="success">Cloudflare authorized. Choose domains below, then configure DNS automation and team sharing.</Notice>}
      {result && result !== "connected" && <Notice tone="error">{result === "denied" ? "Cloudflare authorization was declined." : result === "expired" ? "Authorization expired or the original session ended. Connect again." : "Cloudflare authorization could not be completed. Check the OAuth client configuration and try again."}</Notice>}
      {oauth.data && !oauth.data.configured && <p className="text-xs text-fg-muted">An instance administrator must configure OAuth once. You can connect multiple domains with one API token now.</p>}
      {connect.isError && <Notice tone="error">{errorMessage(connect.error)}</Notice>}
      {oauth.isError && <QueryError what="Cloudflare authorization" error={oauth.error} onRetry={() => void oauth.refetch()} />}
      {connections.isPending && <LoadingRows label="Loading your Cloudflare connections" rows={1} />}
      {connections.isError && <QueryError what="your Cloudflare connections" error={connections.error} onRetry={() => void connections.refetch()} />}
      {connections.data?.length === 0 && <p className="text-[13px] text-fg-muted">No connections yet.</p>}
      {Boolean(connections.data?.length) && <ul aria-label="Your Cloudflare connections" className="divide-y divide-line rounded-md border border-line">
        {connections.data?.map(connection => <li key={connection.id} className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 px-3 py-2">
          <div className="min-w-0 flex-1 basis-40"><p className="text-[13px] font-medium break-words text-fg">{connection.name}</p><p className="text-xs text-fg-muted">{connection.kind === "oauth" ? "OAuth authorization" : "API token"} · Private to you</p></div>
          <div className="flex flex-wrap gap-2"><Button size="sm" onClick={() => setSelecting(connection.id)}>Choose domains</Button>{connection.kind === "token" && <Button size="sm" variant="ghost" onClick={() => setTokenForm(connection)}>Replace token</Button>}</div>
        </li>)}
      </ul>}
    </Subsection>
    {session?.is_admin && oauth.data && <details className="group p-4"><summary className="flex cursor-pointer items-center gap-1.5 rounded text-[13px] font-semibold text-fg"><ChevronRight aria-hidden className="size-4 shrink-0 text-fg-muted transition-transform group-open:rotate-90" />OAuth client setup <span className="font-normal text-fg-muted">{oauth.data.configured ? "Configured" : "Not configured"}</span></summary><OAuthClientForm info={oauth.data} /></details>}
  </>;
}

function TokenConnection({ initial, onDone }: { initial?: Connection; onDone: (id?: string) => void }) {
  const cache = useQueryClient();
  const online = useOnline();
  const save = useMutation({
    gcTime: 0, networkMode: "always",
    mutationFn: (body: { name: string; token: string }) => request<{ id: string }>(initial ? "PUT" : "POST", initial ? `/api/cloudflare/connections/${initial.id}/credentials` : "/api/cloudflare/connections", body),
    onSuccess: async result => { await cache.invalidateQueries({ queryKey: connectionsKey }); onDone(result.id); },
  });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    if (online && !save.isPending) save.mutate({ name: formText(data, "name"), token: formText(data, "token") });
  }
  return <Subsection title={initial ? "Replace Cloudflare API token" : "Connect with one API token"} description="Create a token with Zone Read and DNS Edit for the zones you want Cove to manage. One token can cover multiple domains; Cove discovers them for you.">
  <form className="space-y-4" onSubmit={submit}>
    <a className={`${linkClass} text-[13px]`} href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noreferrer">Create a token in Cloudflare</a>
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id="cf-connection-name" label="Connection name"><input id="cf-connection-name" name="name" className={inputClass} defaultValue={initial?.name ?? "My Cloudflare account"} maxLength={100} required /></Field>
      <Field id="cf-connection-token" label="API token"><input id="cf-connection-token" name="token" type="password" autoComplete="off" className={inputClass} required /></Field>
    </div>
    {initial && <Notice>The replacement must retain access to every domain already using this connection. Team sharing and DNS settings are preserved.</Notice>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex flex-wrap gap-2 border-t border-line pt-4"><Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>{save.isPending ? "Checking token…" : "Discover domains"}</Button><Button variant="ghost" disabled={save.isPending} onClick={() => onDone()}>Cancel</Button></div>
  </form>
  </Subsection>;
}

function SelectCloudflareZones({ id, onDone }: { id: string; onDone: () => void }) {
  const online = useOnline();
  const cache = useQueryClient();
  const zones = useQuery({ queryKey: ["cloudflare", "available", id], queryFn: ({ signal }) => request<AvailableZone[]>("GET", `/api/cloudflare/connections/${id}/zones`, undefined, signal) });
  const [selected, setSelected] = useState<string[]>([]);
  const save = useMutation({
    networkMode: "always",
    mutationFn: () => request("POST", `/api/cloudflare/connections/${id}/zones`, { zones: selected }),
    onSuccess: async () => { await cache.invalidateQueries({ queryKey: ["cloudflare"] }); onDone(); },
  });
  return <Subsection title="Choose domains" description="New domains have automatic DNS enabled and stay private until you share them with a team. Worker addresses are filled from their public connection addresses when unambiguous; review them under Edit. Existing domains keep their settings and team grants.">
  <form className="space-y-4" onSubmit={event => { event.preventDefault(); if (online && selected.length && !save.isPending) save.mutate(); }}>
    {zones.isPending && <p role="status" className="flex items-center gap-2 text-[13px] text-fg-muted"><Spinner />Discovering domains…</p>}
    {zones.isError && <QueryError what="available domains" error={zones.error} onRetry={() => void zones.refetch()} />}
    {zones.data?.length === 0 && <Notice>No zones are available. Check the connection’s authorized zones and Zone Read permission.</Notice>}
    <fieldset disabled={save.isPending} className="grid max-h-80 grid-cols-[repeat(auto-fill,minmax(min(100%,14rem),1fr))] gap-1.5 overflow-y-auto">
      <legend className="sr-only">Available Cloudflare domains</legend>
      {zones.data?.map(zone => <label key={zone.id} className={choiceClass}><Checkbox checked={selected.includes(zone.id)} onChange={event => setSelected(current => event.target.checked ? [...current, zone.id] : current.filter(value => value !== zone.id))} /><span className="min-w-0 break-all">{zone.name}</span></label>)}
    </fieldset>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex flex-wrap gap-2 border-t border-line pt-4"><Button type="submit" variant="primary" disabled={!online || selected.length === 0 || selected.length > 100} pending={save.isPending}>Connect selected domains{selected.length ? ` (${selected.length})` : ""}</Button><Button variant="ghost" onClick={onDone} disabled={save.isPending}>Cancel</Button></div>
  </form>
  </Subsection>;
}

function OAuthClientForm({ info }: { info: OAuthInfo }) {
  const cache = useQueryClient();
  const online = useOnline();
  const [saved, setSaved] = useState(false);
  const save = useMutation({
    gcTime: 0, networkMode: "always",
    mutationFn: (body: { client_id: string; client_secret: string }) => request("PUT", "/api/cloudflare/oauth", body),
    onSuccess: async () => { await cache.invalidateQueries({ queryKey: oauthKey }); },
  });
  return <form className="mt-4 space-y-4" onSubmit={event => {
    event.preventDefault(); const form = event.currentTarget; const data = new FormData(form);
    if (online && !save.isPending) {
      setSaved(false);
      save.mutate({ client_id: formText(data, "client_id"), client_secret: formText(data, "client_secret") }, { onSuccess: () => { form.reset(); save.reset(); setSaved(true); } });
    }
  }}>
    <p className="text-sm text-fg-muted">In Cloudflare, open Manage Account → OAuth clients. Use authorization_code and refresh_token grants, code responses, and client_secret_basic authentication. To support users outside your account, publish the client after verifying its website.</p>
    <Field id="cf-oauth-redirect" label="Redirect URL"><input id="cf-oauth-redirect" className={inputClass} readOnly value={info.redirect_uri ?? ""} /></Field>
    <p className="break-words text-xs text-fg-muted">Required scopes: {info.scopes}. Offline access is enabled by the refresh_token grant.</p>
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id="cf-oauth-client" label="Client ID"><input id="cf-oauth-client" name="client_id" className={inputClass} defaultValue={info.client_id} required /></Field>
      <Field id="cf-oauth-secret" label="Client secret"><input id="cf-oauth-secret" name="client_secret" className={inputClass} type="password" autoComplete="off" required /></Field>
    </div>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    {saved && <Notice tone="success">OAuth client saved. Test it with Connect with Cloudflare.</Notice>}
    <Button type="submit" disabled={!online} pending={save.isPending}>Save OAuth client</Button>
  </form>;
}
