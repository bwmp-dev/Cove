import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { useOnline } from "../lib/hooks";
import { CopyButton } from "./CopyButton";
import { Button, Notice, QueryError } from "./ui";

export function Webhook({ workloadId }: { workloadId: string }) {
  const client = useQueryClient();
  const online = useOnline();
  const [secret, setSecret] = useState("");
  const path = `/api/workloads/${workloadId}/webhook`;
  const config = useQuery({ queryKey: ["webhook", workloadId], queryFn: ({ signal }) => request<{ enabled: boolean; url: string; app_managed?: boolean }>("GET", path, undefined, signal) });
  const change = useMutation({ gcTime: 0, mutationFn: (enable: boolean) => request<{ secret?: string }>(enable ? "POST" : "DELETE", path), onSuccess: async (result) => { setSecret(result.secret ?? ""); await client.invalidateQueries({ queryKey: ["webhook", workloadId] }); } });
  if (config.isError) return <QueryError what="webhook configuration" error={config.error} onRetry={() => void config.refetch()} />;
  if (!config.data) return <p className="text-sm text-fg-muted">Loading webhook…</p>;
  if (config.data.app_managed) return <p className="text-sm text-fg-muted">Managed by the GitHub App connection below. No repository webhook secret is needed.</p>;
  return <div className="space-y-3 text-sm"><p>GitHub push events deploy the exact pushed commit on the configured branch. Events wait while another operation is active.</p>
    <p className="break-all font-mono text-xs">{config.data.url}</p><CopyButton text={config.data.url} label="Copy webhook URL" />
    {secret && <Notice title="Save this secret now"><p>Use application/json, this secret, and push events in GitHub’s webhook settings.</p><p className="break-all font-mono">{secret}</p><CopyButton text={secret} label="Copy webhook secret" /><Button onClick={() => { setSecret(""); change.reset(); }}>Hide secret</Button></Notice>}
    <div className="flex flex-wrap gap-2"><Button disabled={!online || change.isPending} onClick={() => change.mutate(true)}>{config.data.enabled ? "Rotate secret" : "Enable webhook"}</Button>{config.data.enabled && <Button disabled={!online || change.isPending} onClick={() => change.mutate(false)}>Disable webhook</Button>}</div>
    {config.data.enabled && <p className="text-xs text-fg-muted">Rotating the secret requires updating GitHub. Disabling discards pending deliveries; operations already queued continue.</p>}
    {change.isError && <Notice tone="error">{errorMessage(change.error)}</Notice>}
  </div>;
}
