import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

interface NodeLinkSettings {
  fingerprint: string;
  link_address: string;
  link_port: number;
  default_address: string;
  capable: boolean;
  applied_at: string | null;
  last_error: string;
  connections: number;
}

export function NodeLinks({ nodeId }: { nodeId: string }) {
  const key = ["nodes", nodeId, "private-links"];
  const settings = useQuery({ queryKey: key, queryFn: ({ signal }) => request<NodeLinkSettings>("GET", `/api/nodes/${encodeURIComponent(nodeId)}/private-links`, undefined, signal) });
  return (
    <Section title="Private links" description="How other workers reach this worker's link gateway for cross-worker private connections. Traffic uses mutual TLS pinned to each worker's key.">
      {settings.isPending ? (
        <LoadingRows label="Loading private link settings" rows={2} />
      ) : settings.isError ? (
        <QueryError what="private link settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
      ) : (
        <NodeLinkForm key={`${settings.data.link_address}:${settings.data.link_port}`} nodeId={nodeId} settings={settings.data} queryKey={key} />
      )}
    </Section>
  );
}

function NodeLinkForm({ nodeId, settings, queryKey }: { nodeId: string; settings: NodeLinkSettings; queryKey: string[] }) {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [address, setAddress] = useState(settings.link_address);
  const [port, setPort] = useState(String(settings.link_port));
  const save = useMutation({
    mutationFn: () => request<NodeLinkSettings>("PUT", `/api/nodes/${encodeURIComponent(nodeId)}/private-links`, { link_address: address.trim(), link_port: Number(port) }),
    onSuccess: (data) => client.setQueryData(queryKey, data),
  });
  const changed = address.trim() !== settings.link_address || Number(port) !== settings.link_port;
  function submit(event: FormEvent) {
    event.preventDefault();
    if (online && changed && !save.isPending) save.mutate();
  }
  return (
    <form onSubmit={submit} className="space-y-4">
      {!settings.capable && <Notice tone="warn" title="Not available on this worker">Upgrade and verify this worker before it hosts private connections.</Notice>}
      {settings.last_error && <Notice tone="error" title="The last configuration attempt failed">{settings.last_error}</Notice>}
      <DefinitionList
        compact
        items={[
          { term: "Key fingerprint", value: settings.fingerprint ? <span className="font-mono text-xs break-all">{settings.fingerprint}</span> : "Created when the first connection uses this worker" },
          { term: "Connections", value: String(settings.connections) },
          { term: "Last applied", value: settings.applied_at ? <RelativeTime value={settings.applied_at} /> : "Never" },
        ]}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-address`} label="Link address" optional hint={settings.default_address ? `Defaults to the SSH address, ${settings.default_address}.` : "Required for cross-worker connections to the main node's worker."}>
          <input id={`${id}-address`} className={inputClass} value={address} maxLength={253} onChange={(event) => setAddress(event.target.value)} aria-describedby={`${id}-address-hint`} autoComplete="off" />
        </Field>
        <Field id={`${id}-port`} label="Link port" hint="TCP port other workers must reach. Changing it briefly interrupts this worker's links.">
          <input id={`${id}-port`} className={inputClass} inputMode="numeric" value={port} required onChange={(event) => setPort(event.target.value.replace(/\D/g, "").slice(0, 5))} aria-describedby={`${id}-port-hint`} />
        </Field>
      </div>
      {save.isError && <Notice tone="error" title="Could not save private link settings">{errorMessage(save.error)}</Notice>}
      <Button type="submit" pending={save.isPending} disabled={!online || !changed}>
        Save link settings
      </Button>
    </form>
  );
}
