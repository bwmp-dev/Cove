import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { ChevronRight } from "lucide-react";
import { useState } from "react";
import { isGitHubAuthorizeURL } from "../api/auth";
import { errorMessage, request } from "../api/client";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import type { AppInfo } from "./GitHubApp";
import { Button, Field, Notice } from "./ui";

const results: Record<string, string> = {
  connected: "GitHub account connected to your user. Choose the teams to share it with below.",
  expired: "Authorization expired or the session changed. Please connect again.",
  unavailable: "GitHub authorization could not finish. Check the app's user authorization settings and try again.",
  access_denied: "Access could not be verified. Use the personal account owner or an active organization admin, and authorize the installed app.",
};

export function GitHubTeamConnection({ configured }: { configured: boolean }) {
  const online = useOnline();
  const search = useRouterState({ select: state => state.location.searchStr });
  const result = new URLSearchParams(search).get("github");
  const [account, setAccount] = useState("");
  const connect = useMutation({
    mutationFn: async () => {
      const response = await request<{ url: string }>("POST", "/api/github/app/connect", { account: account.trim() });
      if (!isGitHubAuthorizeURL(response.url)) throw new Error("Invalid GitHub authorization URL");
      window.location.assign(response.url);
    },
    gcTime: 0,
    networkMode: "always",
  });
  return <div className="space-y-3">
    {result && results[result] && <Notice tone={result === "connected" ? "info" : "error"} title={result === "connected" ? "Connected" : "Connection incomplete"}>{results[result]}</Notice>}
    {configured ? <form className="space-y-1.5" onSubmit={event => { event.preventDefault(); connect.mutate(); }}>
      <div className="flex flex-wrap items-end gap-2">
        <div className="min-w-0 flex-1 basis-56">
          <Field id="github-connect-account" label="GitHub account or organization"><input id="github-connect-account" className={inputClass} value={account} onChange={event => setAccount(event.target.value)} required maxLength={39} pattern="[a-zA-Z0-9][a-zA-Z0-9-]*" aria-describedby="github-connect-account-hint" /></Field>
        </div>
        <Button type="submit" disabled={!online || !account.trim()} pending={connect.isPending}>Connect account on GitHub</Button>
      </div>
      <p id="github-connect-account-hint" className="text-xs text-fg-muted">You must own the personal account or be an organization admin.</p>
    </form> : <p className="text-sm text-fg-muted">An instance administrator must enable user authorization on this shared app once. They do not need access to your repositories.</p>}
    {connect.isError && <Notice tone="error" title="Could not connect GitHub">{errorMessage(connect.error)}</Notice>}
  </div>;
}

export function GitHubUserAuthorization({ app }: { app: AppInfo }) {
  const online = useOnline();
  const cache = useQueryClient();
  const [client, setClient] = useState("");
  const [secret, setSecret] = useState("");
  const save = useMutation({
    mutationFn: () => request("PUT", "/api/github/app/user-auth", { client_id: client.trim(), client_secret: secret }),
    onSuccess: async () => { setSecret(""); await cache.invalidateQueries({ queryKey: ["github-app"] }); },
    gcTime: 0, networkMode: "always",
  });
  return <details className="group p-4">
    <summary className="flex cursor-pointer items-center gap-1.5 rounded text-[13px] font-semibold text-fg"><ChevronRight aria-hidden className="size-4 shrink-0 text-fg-muted transition-transform group-open:rotate-90" />App user authorization <span className="font-normal text-fg-muted">{app.user_auth_configured ? "Configured" : "Setup required"}</span></summary>
    <form className="mt-3 space-y-3" onSubmit={event => { event.preventDefault(); save.mutate(); }}>
      <p className="text-sm text-fg-muted">In this GitHub App's settings, add the following callback URL, then save its client ID and a client secret here. This enables each team to authorize its own accounts.</p>
      <Field id="github-connect-callback" label="Callback URL"><input id="github-connect-callback" className={inputClass} readOnly value={app.callback_url ?? ""} /></Field>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field id="github-client-id" label="App client ID"><input id="github-client-id" className={inputClass} required value={client} onChange={event => setClient(event.target.value)} /></Field>
        <Field id="github-client-secret" label="App client secret"><input id="github-client-secret" className={inputClass} type="password" autoComplete="new-password" required value={secret} onChange={event => setSecret(event.target.value)} /></Field>
      </div>
      <Button type="submit" disabled={!online || !client.trim() || !secret} pending={save.isPending}>Save user authorization</Button>
      {save.isSuccess && <p className="text-sm">User authorization saved.</p>}
      {save.isError && <Notice tone="error" title="Could not save authorization">{errorMessage(save.error)}</Notice>}
    </form>
  </details>;
}

export function TeamInstallation({ installation }: { installation: { id: number; account: string } }) {
  const online = useOnline();
  const cache = useQueryClient();
  const disconnect = useMutation({
    mutationFn: () => request("DELETE", `/api/github/app/installations/${installation.id}`),
    onSuccess: async () => { await cache.invalidateQueries({ queryKey: ["github-app"] }); await cache.invalidateQueries({ queryKey: ["github-repositories"] }); },
    networkMode: "always",
  });
  return <li className="space-y-2 px-3 py-2">
    <div className="flex flex-wrap items-center justify-between gap-3"><span className="min-w-0 break-words text-[13px] font-medium">{installation.account}</span><Button size="sm" variant="ghost" disabled={!online} pending={disconnect.isPending} onClick={() => disconnect.mutate()}>Remove team access</Button></div>
    {disconnect.isError && <Notice tone="error" title="Could not disconnect">{errorMessage(disconnect.error)}</Notice>}
  </li>;
}
