import { useQuery } from "@tanstack/react-query";
import { RefreshCw, X } from "lucide-react";
import { errorMessage } from "../api/client";
import { useRenewEnrollment, useVerifyNode } from "../api/mutations";
import { nodesQuery } from "../api/queries";
import type { Enrollment, NodeState } from "../api/types";
import { formatAbsolute, formatCountdown } from "../lib/format";
import { useNow } from "../lib/hooks";
import { nodeAddress } from "../lib/nodes";
import { CopyButton } from "./CopyButton";
import { NodeStateLabel } from "./status";
import { Button, Notice, Spinner } from "./ui";

interface EnrollmentPanelProps {
  enrollment: Enrollment;
  nodeId: string;
  // Node state when the command was generated; a change away from "pending" means the token was used.
  initialState: NodeState;
  onClose: () => void;
}

export function EnrollmentPanel({ enrollment: initial, nodeId, initialState, onClose }: EnrollmentPanelProps) {
  const now = useNow(1_000);
  const renew = useRenewEnrollment(nodeId);
  const verify = useVerifyNode();
  const nodes = useQuery({ ...nodesQuery, refetchInterval: 3_000 });
  const node = nodes.data?.find((item) => item.id === nodeId);

  const enrollment = renew.data ?? initial;
  const remaining = Date.parse(enrollment.expires_at) - now;
  const expired = remaining <= 0;
  const registered = initialState === "pending" && node !== undefined && node.state !== "pending";
  const showCommand = !expired && !registered;

  return (
    <section aria-labelledby="enrollment-title" className="border-b border-line px-4 py-5 sm:px-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="enrollment-title" className="text-sm font-semibold text-fg">
            Setup command{node ? ` for ${node.name}` : ""}
          </h2>
          <p className="mt-1 max-w-2xl text-[13px] text-fg-muted">
            Run this once on the worker as a user with sudo. It downloads the installer over HTTPS, installs the worker, and registers the
            host key. Treat it as a password: it is shown only here and closing this panel discards it.
          </p>
        </div>
        <Button variant="ghost" onClick={onClose}>
          <X aria-hidden className="size-3.5" />
          Close
        </Button>
      </div>

      {showCommand && (
        <div className="mt-4 space-y-3">
          <pre
            aria-label="Setup command"
            className="max-h-48 overflow-auto rounded-md border border-line-strong bg-panel p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-fg select-all"
          >
            {enrollment.setup_command}
          </pre>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
            <CopyButton text={enrollment.setup_command} label="Copy command" variant="primary" />
            <p className="text-xs text-fg-muted">
              Expires in <span className="font-mono text-fg tabular-nums">{formatCountdown(remaining)}</span>
              <span className="text-fg-subtle"> at {formatAbsolute(enrollment.expires_at)}</span>
            </p>
          </div>
        </div>
      )}

      {expired && !registered && (
        <div className="mt-4">
          <Notice
            tone="warn"
            title="This setup command has expired"
            action={
              <Button variant="secondary" pending={renew.isPending} onClick={() => renew.mutate()}>
                Generate new command
              </Button>
            }
          >
            Generate a new command for the same node. Earlier commands stop working.
          </Notice>
        </div>
      )}
      {renew.isError && (
        <div className="mt-3">
          <Notice tone="error" title="Could not generate a new command">
            {errorMessage(renew.error)}
          </Notice>
        </div>
      )}

      <div className="mt-5 border-t border-line pt-4" aria-live="polite">
        <h3 className="text-xs font-medium text-fg-subtle">Node status</h3>
        {!node ? (
          nodes.isError ? (
            <p className="mt-2 text-[13px] text-bad">Could not refresh node status: {errorMessage(nodes.error)}</p>
          ) : (
            <p className="mt-2 flex items-center gap-2 text-[13px] text-fg-muted">
              <Spinner /> Loading status
            </p>
          )
        ) : (
          <div className="mt-2 space-y-3">
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <NodeStateLabel state={node.state} />
              <span className="font-mono text-xs text-fg-muted">{nodeAddress(node)}</span>
            </div>
            {node.state === "pending" && !expired && (
              <p className="text-[13px] text-fg-muted">Waiting for the worker to register. This updates automatically.</p>
            )}
            {node.state === "enrolled" && <p className="text-[13px] text-fg-muted">Registered. Waiting for SSH verification.</p>}
            {node.state === "unreachable" && (
              <Notice
                tone="warn"
                title="The worker registered, but the main node could not connect over SSH"
                action={
                  <Button variant="secondary" pending={verify.isPending} onClick={() => verify.mutate(node.id)}>
                    {!verify.isPending && <RefreshCw aria-hidden className="size-3.5" />}
                    Verify again
                  </Button>
                }
              >
                Make sure {nodeAddress(node)} is reachable from the main node through your firewall, private network, or VPN.
              </Notice>
            )}
            {node.state === "ssh_verified" && (
              <Notice tone="warn" title="SSH works, but Docker Compose is not available">
                Install Docker with the Compose v2 plugin on the worker, then verify the node again.
              </Notice>
            )}
            {node.state === "ready" && <Notice tone="success" title="The node is ready for workloads" />}
            {verify.isError && <p className="text-[13px] text-bad">{errorMessage(verify.error)}</p>}
          </div>
        )}
      </div>
    </section>
  );
}
