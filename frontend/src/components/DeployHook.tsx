import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { deployHookQuery, type CreatedDeployHook, type DeployHook as Hook } from "../api/git-providers";
import type { Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { ExpiryTime } from "./RelativeTime";
import { Button, DefinitionList, LoadingRows, Notice, QueryError, Section } from "./ui";

const title = "Deploy hook";
const description = "Let CI or another system start a deployment of this resource with a bearer token. Works for Git and Compose sources.";

function HookDetails({ hook }: { hook: Hook }) {
  return (
    <DefinitionList
      items={[
        {
          term: "URL",
          value: (
            <span className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 font-mono text-xs break-all">{hook.url}</span>
              <CopyButton text={hook.url} label="Copy URL" variant="ghost" />
            </span>
          ),
        },
        { term: "Token", value: <ExpiryTime value={hook.expires_at} /> },
      ]}
    />
  );
}

function CreatedToken({ hook, onHide }: { hook: CreatedDeployHook; onHide: () => void }) {
  return (
    <Notice tone="success" title="Save this token now">
      <div className="min-w-0 space-y-3">
        <p>It is not shown again. Store it as a CI secret, such as a GitHub Actions secret, and send it only in the Authorization header.</p>
        <p className="font-mono text-xs break-all">{hook.token}</p>
        <div className="flex flex-wrap gap-2">
          <CopyButton text={hook.token} label="Copy token" />
          <Button variant="ghost" onClick={onHide}>Hide token</Button>
        </div>
      </div>
    </Notice>
  );
}

function Usage() {
  return (
    <div className="space-y-2 text-xs text-fg-muted">
      <p>
        Send <span className="font-mono">POST</span> to the URL with <span className="font-mono break-all">Authorization: Bearer &lt;token&gt;</span> and a JSON body{" "}
        <span className="font-mono break-all">{'{"delivery": "<stable id>", "revision": "<40-character commit SHA>"}'}</span>. The revision is optional and needs a Git source. Retrying the same delivery returns the same operation, and at most 10 new deliveries are accepted per minute.
      </p>
      <p>
        GitHub Actions can use the composite action in <span className="font-mono">.github/actions/deploy</span> of the Cove repository. It triggers once per workflow job and waits for the deployment to finish.
      </p>
    </div>
  );
}

export function DeployHook({ workload, editable }: { workload: Workload; editable: boolean }) {
  const online = useOnline();
  const client = useQueryClient();
  const hook = useQuery(deployHookQuery(workload.id));
  const [confirm, setConfirm] = useState<"rotate" | "revoke" | null>(null);
  const path = `/api/workloads/${encodeURIComponent(workload.id)}/deploy-hook`;
  const key = deployHookQuery(workload.id).queryKey;
  const create = useMutation({
    gcTime: 0,
    mutationFn: () => request<CreatedDeployHook>("POST", path),
    onSuccess: ({ id, url, expires_at }) => {
      setConfirm(null);
      const saved: Hook = { id, url, expires_at };
      client.setQueryData(key, saved);
    },
    networkMode: "always",
  });
  const revoke = useMutation({
    mutationFn: () => request<{ ok: boolean }>("DELETE", path),
    onSuccess: () => {
      setConfirm(null);
      create.reset();
      client.setQueryData(key, null);
    },
    networkMode: "always",
  });

  if (hook.isPending) return <Section width="form" title={title} description={description}><LoadingRows label="Loading deploy hook" rows={2} /></Section>;
  if (hook.isError) return <Section width="form" title={title} description={description}><QueryError what="the deploy hook" error={hook.error} onRetry={() => void hook.refetch()} retrying={hook.isFetching} /></Section>;
  if (!editable && !hook.data) return null;

  const disabled = !online;
  return (
    <Section width="form" title={title} description={description}>
      <div className="min-w-0 space-y-4">
        {create.data && <CreatedToken hook={create.data} onHide={() => create.reset()} />}
        {hook.data ? <HookDetails hook={hook.data} /> : <p className="text-[13px] text-fg-muted">No deploy hook exists for this resource.</p>}
        {editable && (
          <div className="space-y-2 border-t border-line pt-4">
            <div className="flex flex-wrap gap-2">
              {hook.data ? (
                <>
                  <Button disabled={disabled} onClick={() => { create.reset(); setConfirm("rotate"); }}>Rotate token</Button>
                  <Button variant="danger" disabled={disabled} onClick={() => { revoke.reset(); setConfirm("revoke"); }}>Revoke hook</Button>
                </>
              ) : (
                <Button disabled={disabled} pending={create.isPending} onClick={() => create.mutate()}>Create deploy hook</Button>
              )}
            </div>
            <p className="text-xs text-fg-muted">Tokens expire after 90 days. Deployments run with your access, which is checked again before each one starts.</p>
            {!hook.data && create.isError && <Notice tone="error" title="Could not create the deploy hook">{errorMessage(create.error)}</Notice>}
          </div>
        )}
        {hook.data && <Usage />}
      </div>
      <ConfirmDialog open={confirm === "rotate"} title="Rotate deploy hook token?" confirmLabel="Rotate token" pending={create.isPending} onCancel={() => setConfirm(null)} onConfirm={() => create.mutate()}>
        <div className="space-y-3">
          <p>A new URL and token replace the current ones. The old token stops working and its queued requests are cancelled; deployments already running continue.</p>
          {create.isError && <Notice tone="error" title="Could not rotate the token">{errorMessage(create.error)}</Notice>}
        </div>
      </ConfirmDialog>
      <ConfirmDialog open={confirm === "revoke"} title="Revoke deploy hook?" confirmLabel="Revoke hook" danger pending={revoke.isPending} onCancel={() => setConfirm(null)} onConfirm={() => revoke.mutate()}>
        <div className="space-y-3">
          <p>The token stops working and requests that have not started are cancelled. Deployments already running continue.</p>
          {revoke.isError && <Notice tone="error" title="Could not revoke the hook">{errorMessage(revoke.error)}</Notice>}
        </div>
      </ConfirmDialog>
    </Section>
  );
}
