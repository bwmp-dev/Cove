import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { Button, Field, Notice, QueryError, Section, SwitchField } from "./ui";

interface PrivateConfig { service: string; hostname: string; port: number }

function PrivateForm({ id, initial }: { id: string; initial: PrivateConfig | null }) {
  const client = useQueryClient();
  const online = useOnline();
  const [enabled, setEnabled] = useState(!!initial);
  const save = useMutation({ gcTime: 0,
    mutationFn: (value: (PrivateConfig & { auth_key: string }) | null) => request("PUT", `/api/workloads/${id}/private-network`, { private: value }),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ["private-network", id] }); },
  });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    const text = (key: string) => { const value = data.get(key); return typeof value === "string" ? value.trim() : ""; };
    save.mutate(enabled ? { service: text("service"), hostname: text("hostname"), port: Number(text("port")), auth_key: text("key") } : null, {
      onSuccess: () => { const key = form.elements.namedItem("key"); if (key instanceof HTMLInputElement) key.value = ""; },
    });
  }
  return <form onSubmit={submit} className="space-y-4">
    <SwitchField id="private-enabled" checked={enabled} onChange={event => setEnabled(event.target.checked)} label="Enable Tailscale private access" />
    {enabled && <>
      <Field id="private-service" label="Compose service"><input id="private-service" name="service" className={inputClass} defaultValue={initial?.service} required /></Field>
      <Field id="private-hostname" label="Tailnet hostname"><input id="private-hostname" name="hostname" className={inputClass} defaultValue={initial?.hostname} required /></Field>
      <Field id="private-port" label="Service TCP port"><input id="private-port" name="port" type="number" min={1} max={65535} className={inputClass} defaultValue={initial?.port ?? 3000} required /></Field>
      <Field id="private-key" label="Tailscale auth key" hint="Use a tagged, preauthorized key. Leave blank to retain the existing key when the hostname is unchanged."><input id="private-key" name="key" type="password" autoComplete="off" className={inputClass} required={!initial} /></Field>
      <Notice>Remove public domains, proxy labels and host port mappings first. The selected service joins the tailnet through its own persistent sidecar. Tailnet access policies control who can connect; Cove team permissions do not replace those policies. Shared workers remain a trust boundary.</Notice>
    </>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    {save.isSuccess && <Notice>Saved. Deploy this resource to apply the change. Disabling does not revoke its device in Tailscale.</Notice>}
    <Button type="submit" variant="primary" disabled={!online || save.isPending}>Save private access</Button>
  </form>;
}

export function PrivateNetwork({ id, canEdit }: { id: string; canEdit: boolean }) {
  const query = useQuery({ queryKey: ["private-network", id], queryFn: ({ signal }) => request<{ private: PrivateConfig | null }>("GET", `/api/workloads/${id}/private-network`, undefined, signal) });
  return <Section width="form" title="Tailscale" description="Private access and cross-worker connectivity without publishing ports to the internet.">
    {query.isError ? <QueryError what="private networking" error={query.error} onRetry={() => void query.refetch()} /> : query.data ? canEdit ? <PrivateForm id={id} initial={query.data.private} /> : <p>{query.data.private ? `${query.data.private.hostname}:${query.data.private.port}` : "Private access is not configured."}</p> : <p>Loading private access…</p>}
  </Section>;
}
