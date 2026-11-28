import { useDeferredValue, useState } from "react";
import type { MountMetric, Node } from "../api/types";
import { useNow } from "../lib/hooks";
import { tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { FilterSelect, FilterToolbar, NoMatches } from "./FilterToolbar";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Notice, Section } from "./ui";

type MountKind = "measured" | "empty" | "pseudo" | "error";
type MountFilter = "measured" | "all" | "unmeasured";

const staleAfter = 180_000;

export function mountKind(mount: MountMetric): MountKind {
  if (mount.error) return "error";
  if (!mount.known) return "pseudo";
  if (mount.total === 0 || mount.available > mount.total) return "empty";
  return "measured";
}

const filters: { value: MountFilter; label: string }[] = [
  { value: "measured", label: "With capacity" },
  { value: "all", label: "All mounts" },
  { value: "unmeasured", label: "Without capacity" },
];

function isMountFilter(value: string): value is MountFilter {
  return filters.some((filter) => filter.value === value);
}

function MountUsage({ mount }: { mount: MountMetric }) {
  const kind = mountKind(mount);
  if (kind === "error") return <StatusLabel tone="warn" label="Unreadable" />;
  if (kind === "pseudo") return <span className="text-xs text-fg-subtle">Pseudo or unknown filesystem</span>;
  if (kind === "empty") return <span className="text-xs text-fg-subtle">No capacity reported</span>;
  const used = mount.total - mount.available;
  const percent = Math.round((used / mount.total) * 100);
  const tone = percent >= 100 ? "bg-bad" : percent >= 80 ? "bg-warn" : "bg-fg-muted";
  return (
    <div className="min-w-0">
      <p className="text-[13px] text-fg tabular-nums">
        {formatBinaryBytes(used)}
        <span className="text-fg-muted"> / {formatBinaryBytes(mount.total)}</span>
        <span className="ml-2 text-xs text-fg-subtle">{percent}%</span>
      </p>
      <div
        role="meter"
        aria-label={`${mount.path} used`}
        aria-valuemin={0}
        aria-valuemax={mount.total}
        aria-valuenow={used}
        aria-valuetext={`${formatBinaryBytes(used)} of ${formatBinaryBytes(mount.total)}, ${percent}%`}
        className="mt-1.5 h-1 max-w-48 overflow-hidden rounded-full bg-raised"
      >
        <div className={`h-full rounded-full ${tone}`} style={{ width: `${Math.min(percent, 100)}%` }} />
      </div>
    </div>
  );
}

export function NodeMounts({ node }: { node: Node }) {
  const now = useNow(10_000);
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState<MountFilter>("measured");
  const query = useDeferredValue(search.trim().toLowerCase());
  const metrics = node.capabilities?.metrics;
  const supported = node.capabilities?.mount_metrics === true;
  const description = "Filesystems mounted in the worker's namespace. In container installs this is the worker container's view, which may not include every host disk.";

  if (!supported || !metrics || !Array.isArray(metrics.mounts)) {
    return (
      <Section title="Mounted filesystems" description={description}>
        <Notice>{supported ? "No mount sample has been reported yet. Verify the node to read its mounts." : "This worker does not report mounted filesystems. Upgrade the worker and verify the node."}</Notice>
      </Section>
    );
  }

  const mounts = metrics.mounts;
  const stale = node.state !== "ready" || now - new Date(metrics.sampled_at).getTime() >= staleAfter;
  const counts = { measured: 0, unmeasured: 0 };
  for (const mount of mounts) counts[mountKind(mount) === "measured" ? "measured" : "unmeasured"]++;
  const shown = mounts.filter((mount) => {
    const measured = mountKind(mount) === "measured";
    if (filter === "measured" && !measured) return false;
    if (filter === "unmeasured" && measured) return false;
    return query === "" || mount.path.toLowerCase().includes(query) || mount.filesystem.toLowerCase().includes(query);
  });
  const active = search !== "" || filter !== "measured";
  const reset = () => {
    setSearch("");
    setFilter("measured");
  };

  return (
    <Section title="Mounted filesystems" description={description} flush>
      <div className="space-y-2 border-b border-line px-4 py-3">
        <p className="text-xs text-fg-muted">
          {stale ? "Last reported" : "Sampled"} <RelativeTime value={metrics.sampled_at} />. {counts.measured.toLocaleString("en")} with capacity, {counts.unmeasured.toLocaleString("en")} without.
        </p>
        {stale && <Notice tone="warn" title="Mount readings may be outdated">{node.state === "ready" ? "The worker has not reported a fresh sample in the last three minutes." : "The node is not ready, so these readings are from its last successful probe."}</Notice>}
        {metrics.mounts_limited && <Notice tone="warn" title="Mount list clipped">The worker reported its maximum number of mounts or path bytes. Mounts beyond the limit are not listed, measured or alerted on.</Notice>}
      </div>
      {mounts.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-fg-muted">The worker reported no mounted filesystems.</p>
      ) : (
        <>
          <FilterToolbar
            label="Filter mounts"
            search={search}
            onSearch={setSearch}
            searchPlaceholder="Search path or filesystem"
            active={active}
            onReset={reset}
            summary={`${shown.length.toLocaleString("en")} of ${mounts.length.toLocaleString("en")} mounts`}
          >
            <FilterSelect id={`node-${node.id}-mount-filter`} label="Show mounts" value={filter} onChange={(value) => isMountFilter(value) && setFilter(value)}>
              {filters.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </FilterSelect>
          </FilterToolbar>
          {shown.length === 0 ? (
            <NoMatches what="mounts" onReset={reset} />
          ) : (
            <div className="max-h-[32rem] overflow-y-auto" tabIndex={0} role="region" aria-label="Mounted filesystems list">
              <table className="w-full table-fixed text-left">
                <caption className="sr-only">Mounted filesystems</caption>
                <thead>
                  <tr className={tableHeadClass}>
                    <th scope="col" className="py-2 pr-4 pl-4 font-medium">Mount</th>
                    <th scope="col" className="hidden w-32 py-2 pr-4 font-medium md:table-cell">Filesystem</th>
                    <th scope="col" className="w-40 py-2 pr-4 font-medium sm:w-56">Used</th>
                    <th scope="col" className="hidden w-28 py-2 pr-4 text-right font-medium lg:table-cell">Available</th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((mount) => (
                    <tr key={mount.path} className={tableRowClass}>
                      <td className="py-2.5 pr-4 pl-4 align-top">
                        <span className="font-mono text-xs break-all text-fg">{mount.path}</span>
                        <span className="mt-0.5 block font-mono text-xs break-all text-fg-muted md:hidden">{mount.filesystem || "Unknown filesystem"}</span>
                        {mount.error && <span className="mt-0.5 block text-xs break-words text-warn">{mount.error}</span>}
                      </td>
                      <td className="hidden py-2.5 pr-4 align-top font-mono text-xs break-all text-fg-muted md:table-cell">{mount.filesystem || <span className="font-sans text-fg-subtle">Unknown</span>}</td>
                      <td className="py-2.5 pr-4 align-top">
                        <MountUsage mount={mount} />
                      </td>
                      <td className="hidden py-2.5 pr-4 text-right align-top text-[13px] text-fg-muted tabular-nums lg:table-cell">{mountKind(mount) === "measured" ? formatBinaryBytes(mount.available) : "Unknown"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </Section>
  );
}
