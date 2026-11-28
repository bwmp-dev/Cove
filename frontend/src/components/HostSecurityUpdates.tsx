import { useQuery } from "@tanstack/react-query";
import { CalendarClock, CalendarOff, RotateCw, Search } from "lucide-react";
import { useState } from "react";
import { ApiError, errorMessage } from "../api/client";
import { hostMaintenanceQuery, useSetHostSecuritySchedule, type HostPatchStatus } from "../api/hostMaintenance";
import type { Node } from "../api/types";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { nodeStates } from "../lib/nodes";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, DefinitionList, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const description = "Pending security updates from the host's cached package metadata, and Cove's optional daily security-update timer. Inspecting never refreshes package indexes or installs packages.";

const managers: Record<HostPatchStatus["manager"], string> = {
  apt: "APT",
  dnf: "DNF",
  unsupported: "No supported provider",
};

// Client errors are definite refusals; anything else may have been applied before the response was lost.
function ambiguous(error: unknown) {
  return !(error instanceof ApiError) || error.status >= 500;
}

function blocker(node: Node, online: boolean) {
  if (!node.capabilities) return "Cove has no current probe result for this worker. Verify the node first.";
  if (!node.capabilities.host_maintenance) return `${node.name} has not reported support for host maintenance. Upgrade the worker to a Linux build that supports it and verify the node.`;
  if (node.state !== "ready") return `The node is ${nodeStates[node.state]?.label.toLowerCase() ?? node.state}. Host maintenance needs a ready worker.`;
  if (!online) return "You are offline. Reconnect to inspect the host or change its schedule.";
  return undefined;
}

function Timestamp({ value }: { value: string }) {
  return (
    <span>
      <RelativeTime value={value} /> <span className="text-fg-muted">({formatAbsolute(value)})</span>
    </span>
  );
}

const unknown = (text = "Unknown") => <span className="text-warn">{text}</span>;

function HostDetails({ status }: { status: HostPatchStatus }) {
  const system = [status.os, status.version].filter(Boolean).join(" ");
  const count = status.security_updates;
  return (
    <DefinitionList
      compact
      items={[
        { term: "Operating system", value: system ? <span className="font-mono text-xs">{system}</span> : unknown() },
        { term: "Package manager", value: managers[status.manager] },
        {
          term: "Security updates",
          value:
            count === null ? (
              unknown(status.manager === "unsupported" ? "Unknown" : "Unknown. Cached metadata could not be read.")
            ) : (
              <span className={`tabular-nums ${count > 0 ? "text-warn" : ""}`}>{count === 0 ? "None in cached metadata" : `${count} pending in cached metadata`}</span>
            ),
        },
        { term: "Metadata from", value: status.metadata_at ? <Timestamp value={status.metadata_at} /> : unknown() },
        {
          term: "Reboot required",
          value: status.reboot_required === null ? unknown() : status.reboot_required ? <StatusLabel tone="warn" label="Yes" /> : "No",
        },
      ]}
    />
  );
}

function PackageList({ status }: { status: HostPatchStatus }) {
  if (status.packages.length === 0) return null;
  const total = status.security_updates ?? status.packages.length;
  return (
    <details className="text-xs">
      <summary className="w-fit cursor-pointer rounded text-fg-muted hover:text-fg">
        {status.limited ? `Packages (first ${status.packages.length} of ${total})` : `Packages (${status.packages.length})`}
      </summary>
      {/* Focusable so the bounded list can be scrolled with the keyboard in every browser. */}
      <div role="region" aria-label="Packages with pending security updates" tabIndex={0} className="mt-2 max-h-64 overflow-auto rounded-md border border-line px-3 py-2">
        <ul className="font-mono break-all text-fg">
          {status.packages.map((name) => (
            <li key={name} className="py-0.5">
              {name}
            </li>
          ))}
        </ul>
      </div>
    </details>
  );
}

function automaticReboot(status: HostPatchStatus) {
  if (status.automatic_reboot === true) return "Allowed by the host's policy";
  if (status.automatic_reboot === false) return "Not allowed by the host's policy";
  return unknown(status.manager === "dnf" ? "Unknown. DNF does not report a reboot policy." : "Unknown");
}

function lastResult(result: string) {
  if (result === "success") return <StatusLabel tone="ok" label="Succeeded" />;
  return (
    <span className="inline-flex flex-wrap items-center gap-x-2">
      <StatusLabel tone="bad" label="Failed" />
      <span className="font-mono text-xs text-fg-muted">{result}</span>
    </span>
  );
}

function ScheduleConfirmation({ enable, manager }: { enable: boolean; manager: HostPatchStatus["manager"] }) {
  if (!enable) {
    return <p>Disabling stops future activations of Cove's timer only. It does not interrupt an update that is already running, and it does not change other schedules on the host.</p>;
  }
  return (
    <div className="space-y-2">
      <p>Cove's timer runs daily at 04:00 UTC plus a random delay of up to one hour. A missed run catches up, so an update may start soon after enabling or after the host restarts.</p>
      <p>
        {manager === "dnf"
          ? "DNF applies security-only upgrades."
          : "APT uses the host's existing unattended-upgrades allowed origins and reboot policy."}{" "}
        Updates may restart services, or reboot the host when its policy allows. Cove does not change other schedules on the host.
      </p>
    </div>
  );
}

type ScheduleMutation = ReturnType<typeof useSetHostSecuritySchedule>;

function ScheduleResult({ save }: { save: ScheduleMutation }) {
  if (save.isError) {
    return (
      <Notice tone="error" title="The schedule change was not confirmed">
        <p>{errorMessage(save.error)}</p>
        {ambiguous(save.error) && <p className="mt-1">The change may have been applied before the connection failed. The host was inspected again; check the status above before retrying.</p>}
      </Notice>
    );
  }
  if (!save.isSuccess) return null;
  const requested = save.variables;
  if (save.data.policy_enabled !== requested) {
    return (
      <Notice tone="warn" title="The host reports a different schedule state">
        Cove's timer is {save.data.policy_enabled ? "enabled" : "disabled"}. Check the notices above before retrying.
      </Notice>
    );
  }
  return (
    <Notice tone="success" title={requested ? "Schedule enabled" : "Schedule disabled"}>
      The status above was read back from the host.
    </Notice>
  );
}

function Schedule({ node, status, disabledReason, onFailure }: { node: Node; status: HostPatchStatus; disabledReason: string | undefined; onFailure: () => void }) {
  const save = useSetHostSecuritySchedule(node.id);
  const [confirming, setConfirming] = useState<boolean | null>(null);
  const enabled = status.policy_enabled;
  // A timer that is already enabled can always be disabled, even if the host no longer meets the prerequisites.
  const blocked = disabledReason !== undefined || (!enabled && !status.policy_supported);

  return (
    <Subsection
      title="Cove security-update schedule"
      description="Daily at 04:00 UTC with up to one hour of random delay. Only this worker's own timer is changed."
      actions={
        <Button variant={enabled ? "secondary" : "primary"} disabled={blocked || save.isPending} onClick={() => setConfirming(!enabled)}>
          {enabled ? <CalendarOff aria-hidden className="size-3.5" /> : <CalendarClock aria-hidden className="size-3.5" />}
          {enabled ? "Disable schedule" : "Enable schedule"}
        </Button>
      }
    >
      <DefinitionList
        compact
        items={[
          { term: "Cove timer", value: enabled ? <StatusLabel tone="ok" label="Enabled" /> : <StatusLabel tone="neutral" label="Disabled" /> },
          { term: "Automatic reboot", value: automaticReboot(status) },
          ...(status.last_result ? [{ term: "Last run", value: lastResult(status.last_result) }] : []),
        ]}
      />
      {!status.policy_supported && (
        <Notice tone="warn" title="Host prerequisites missing">
          Scheduled updates need a running systemd manager and the host's native tools: unattended-upgrades and python3-apt on APT hosts, or DNF with security advisory support. Cove does not install host prerequisites.
        </Notice>
      )}
      <ScheduleResult save={save} />
      <ConfirmDialog
        open={confirming !== null}
        title={confirming ? "Enable Cove's security-update schedule?" : "Disable Cove's security-update schedule?"}
        confirmLabel={confirming ? "Enable schedule" : "Disable schedule"}
        pending={save.isPending}
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming === null) return;
          save.mutate(confirming, { onError: onFailure, onSettled: () => setConfirming(null) });
        }}
      >
        <ScheduleConfirmation enable={confirming ?? false} manager={status.manager} />
      </ConfirmDialog>
    </Subsection>
  );
}

function Inspection({ node, status, disabledReason, onFailure }: { node: Node; status: HostPatchStatus; disabledReason: string | undefined; onFailure: () => void }) {
  return (
    <div className="divide-y divide-line">
      <Subsection>
        <p className="text-xs text-fg-muted">
          Inspected <Timestamp value={status.checked_at} />
        </p>
        <HostDetails status={status} />
        <PackageList status={status} />
        {status.notices.length > 0 && (
          <Notice tone="warn" title="Inspection notes">
            <ul className="list-disc space-y-0.5 pl-4">
              {status.notices.map((notice) => (
                <li key={notice}>{notice}</li>
              ))}
            </ul>
          </Notice>
        )}
      </Subsection>
      <Schedule node={node} status={status} disabledReason={disabledReason} onFailure={onFailure} />
    </div>
  );
}

export function HostSecurityUpdates({ node }: { node: Node }) {
  const online = useOnline();
  const status = useQuery(hostMaintenanceQuery(node.id));
  const disabledReason = blocker(node, online);
  const inspected = status.data !== undefined || status.isError;
  const refetch = () => void status.refetch();

  let body;
  if (status.isFetching && !status.data) {
    body = <LoadingRows label="Inspecting the host" rows={3} />;
  } else if (status.data) {
    body = (
      <>
        {status.isError && (
          <div className="border-b border-line p-4">
            <QueryError what="the latest host status" error={status.error} onRetry={refetch} retrying={status.isFetching} />
          </div>
        )}
        <Inspection node={node} status={status.data} disabledReason={disabledReason} onFailure={refetch} />
      </>
    );
  } else if (status.isError) {
    body = (
      <div className="p-4">
        <QueryError what="host security status" error={status.error} onRetry={refetch} retrying={status.isFetching} />
      </div>
    );
  } else if (!disabledReason) {
    body = <p className="p-4 text-[13px] text-fg-muted">Not inspected yet. Inspecting reads the host's operating system, cached security metadata and Cove's timer state without changing anything.</p>;
  }

  return (
    <Section
      title="Host security updates"
      description={description}
      flush
      actions={
        <Button onClick={refetch} pending={status.isFetching} disabled={disabledReason !== undefined}>
          {!status.isFetching && (inspected ? <RotateCw aria-hidden className="size-3.5" /> : <Search aria-hidden className="size-3.5" />)}
          {inspected ? "Inspect again" : "Inspect"}
        </Button>
      }
    >
      {disabledReason && <p className={`px-4 py-3 text-[13px] text-fg-muted ${body ? "border-b border-line" : ""}`}>{disabledReason}</p>}
      {body}
      <p className="border-t border-line px-4 py-3 text-xs text-fg-muted">
        Counts describe the advisory metadata the host already has, not a fresh scan, so they do not prove the host is current. Unknown values are never treated as zero.
      </p>
    </Section>
  );
}
