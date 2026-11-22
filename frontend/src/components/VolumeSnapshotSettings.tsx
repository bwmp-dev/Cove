import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { bytesToMiBText, parseSnapshotSpace, SNAPSHOT_DEFAULT_BYTES, SNAPSHOT_SPACE_NAME, SNAPSHOT_SWITCH_NAME, type VolumeSnapshotPolicy } from "../lib/backups";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { StatusLabel } from "./status";
import { Button, Field, Notice, SwitchField } from "./ui";

interface SnapshotMount { mount: string; filesystem: string; method: string; available: boolean; reason?: string }
interface SnapshotPreview { available: boolean; reason?: string; mounts: SnapshotMount[] | null }

function PreviewResult({ preview }: { preview: SnapshotPreview }) {
  const mounts = preview.mounts ?? [];
  return <div className="space-y-3">
    {preview.available
      ? <Notice tone="success" title="Every volume can be snapshotted">Backups pause containers only while the snapshots are created.</Notice>
      : <Notice tone="warn" title="Backups will use the stopped method">{preview.reason ?? "At least one volume cannot be snapshotted, so containers stop for the whole backup."}</Notice>}
    {mounts.length === 0
      ? <p className="text-xs text-fg-muted">This resource has no persistent volumes or bind mounts to snapshot.</p>
      : <ul aria-label="Volumes checked" className="divide-y divide-line rounded-md border border-line-strong">
        {mounts.map((mount) => <li key={mount.mount} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1 px-3 py-2.5">
          <div className="min-w-0">
            <p className="font-mono text-xs break-all text-fg">{mount.mount}</p>
            <p className="text-xs text-fg-muted">{[mount.filesystem, mount.method].filter(Boolean).join(" · ") || "Unknown filesystem"}</p>
            {mount.reason && <p className="mt-0.5 text-xs break-words text-fg-muted">{mount.reason}</p>}
          </div>
          <StatusLabel tone={mount.available ? "ok" : "warn"} label={mount.available ? "Supported" : "Not supported"} />
        </li>)}
      </ul>}
    <p className="text-xs text-fg-muted">This check is advisory. Every backup inspects the volumes again.</p>
  </div>;
}

export function VolumeSnapshotSettings({ workloadId, initial, supported, nodeName, spaceError, onSpaceChange }: { workloadId: string; initial: VolumeSnapshotPolicy | null | undefined; supported: boolean | undefined; nodeName?: string; spaceError?: string; onSpaceChange: () => void }) {
  const online = useOnline();
  const [enabled, setEnabled] = useState(Boolean(initial));
  const [space, setSpace] = useState(() => bytesToMiBText(initial?.max_bytes || SNAPSHOT_DEFAULT_BYTES));
  const [checkError, setCheckError] = useState<string>();
  const blocked = supported === false && !initial;
  const preview = useMutation({
    gcTime: 0,
    mutationFn: (bytes: number) => request<SnapshotPreview>("GET", `/api/workloads/${workloadId}/backups/snapshot-preview${bytes ? `?max_bytes=${bytes}` : ""}`),
  });
  function check() {
    const parsed = parseSnapshotSpace(space, initial);
    if ("error" in parsed) {
      setCheckError(parsed.error);
      document.getElementById("backup-snapshot-space")?.focus();
      return;
    }
    setCheckError(undefined);
    preview.mutate(parsed.bytes);
  }
  const error = checkError ?? spaceError;
  return <div className="space-y-4">
    <SwitchField id="backup-volume-snapshot" name={SNAPSHOT_SWITCH_NAME} label="Use filesystem snapshots" checked={enabled} disabled={blocked} onChange={(event) => setEnabled(event.target.checked)}
      description="Containers pause briefly while Btrfs, ZFS or LVM snapshots are created, then keep running while the encrypted backup copies data. Volumes that cannot be snapshotted use the stopped method." />
    {blocked && <Notice tone="warn" title="Filesystem snapshots need an upgraded worker">Upgrade and verify {nodeName ?? "this resource's worker"} to back up volumes without stopping containers. Backups on this worker stop containers.</Notice>}
    {enabled && <>
      <Notice title="How filesystem snapshots behave">
        <ul className="list-disc space-y-1 pl-4">
          <li>Data is captured as if the processes had paused, like a database that was not shut down. Use a database backup method for databases.</li>
          <li>Unsupported filesystems, missing host tools, hosts without systemd, nested or whole-filesystem Btrfs subvolumes and thin LVM volumes use the stopped method. Cove does not install host tools.</li>
          <li>If creating or checking a snapshot fails, the backup fails and the host cleans up. It does not fall back to the stopped method after a partial failure. An LVM snapshot that runs out of space fails the backup.</li>
          <li>A backup can snapshot up to 16 volumes. A host systemd timer releases snapshots after 35 minutes even if the worker stops.</li>
          <li>Restores still stop containers and save a safety snapshot first.</li>
        </ul>
      </Notice>
      <Field id="backup-snapshot-space" label="LVM snapshot space (MiB)" error={error} hint="Copy-on-write space for traditional LVM snapshots, 64 to 65,536 MiB. It limits writes during the backup, not the backup size. Btrfs and ZFS ignore it.">
        <input id="backup-snapshot-space" name={SNAPSHOT_SPACE_NAME} inputMode="numeric" className={`${inputClass} max-w-40`} value={space} required aria-invalid={error ? true : undefined} aria-describedby={error ? "backup-snapshot-space-error" : "backup-snapshot-space-hint"}
          onChange={(event) => { setSpace(event.target.value); setCheckError(undefined); preview.reset(); onSpaceChange(); }} />
      </Field>
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <Button size="sm" disabled={!online} pending={preview.isPending} onClick={check}>Check volumes</Button>
          <p className="text-xs text-fg-muted">{online ? "Inspects this resource's volumes on the worker without changing them." : "Reconnect to check volumes."}</p>
        </div>
        {preview.isError && <Notice tone="error" title="Could not check volumes">{errorMessage(preview.error)}</Notice>}
        {preview.data && <PreviewResult preview={preview.data} />}
      </div>
    </>}
  </div>;
}
