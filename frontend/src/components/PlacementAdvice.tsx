import { skipToken, useQuery } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { useId, useState, type KeyboardEvent } from "react";
import { errorMessage } from "../api/client";
import { fetchPlacementSuggestions, type PlacementReport, type PlacementRequest, type PlacementSuggestion } from "../api/resourceDefaults";
import { useOnline } from "../lib/hooks";
import { formatCpus, parseLimits, type LimitErrors } from "../lib/resource-defaults";
import { LimitInput } from "./LimitInput";
import { formatBinaryBytes } from "../lib/telemetry";
import { RelativeTime } from "./RelativeTime";
import { NodeStateLabel, StatusLabel } from "./status";
import { Button, Field, Notice } from "./ui";

function FitLabel({ fits }: { fits: boolean | null }) {
  if (fits === null) return <StatusLabel tone="neutral" label="Capacity unknown" />;
  return fits ? <StatusLabel tone="ok" label="Fits measured free capacity" /> : <StatusLabel tone="warn" label="Exceeds measured free capacity" />;
}

function freeText(node: PlacementSuggestion) {
  const cpu = node.available_cpus === null ? "CPU unknown" : `${formatCpus(node.available_cpus)} free`;
  const memory = node.available_memory_bytes === null ? "memory unknown" : `${formatBinaryBytes(node.available_memory_bytes)} memory free`;
  return `${cpu} · ${memory}`;
}

function requirementText(report: PlacementReport) {
  const { cpus, memory_mb } = report.requirements;
  const cpu = cpus > 0 ? `at least ${formatCpus(cpus)} free` : "no explicit CPU requirement";
  const memory = memory_mb > 0 ? `at least ${memory_mb} MiB memory free` : "no explicit memory requirement";
  return `Compared against ${cpu} and ${memory}. Nodes are ranked by known fit, then free headroom.`;
}

// Advice only: it never changes the selected node. People choose the node in the Node field.
export function PlacementAdvice({ projectId, environmentId, selectedNodeId }: { projectId: string; environmentId: string; selectedNodeId: string }) {
  const id = useId();
  const online = useOnline();
  const [cpus, setCpus] = useState("");
  const [memory, setMemory] = useState("");
  const [errors, setErrors] = useState<LimitErrors>({});
  // Fetched only on Inspect or Refresh; the key carries the scope, so results from another scope are never shown.
  const [submitted, setSubmitted] = useState<PlacementRequest | null>(null);
  const active = submitted?.projectId === projectId && submitted.environmentId === environmentId ? submitted : null;
  const inspect = useQuery({
    queryKey: ["placement-suggestions", active],
    queryFn: active ? ({ signal }) => fetchPlacementSuggestions(active, signal) : skipToken,
    gcTime: 0,
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

  function run() {
    const parsed = parseLimits(cpus, memory);
    setErrors(parsed.errors);
    if (parsed.errors.cpus || parsed.errors.memory) {
      document.getElementById(parsed.errors.cpus ? `${id}-cpus` : `${id}-memory`)?.focus();
      return;
    }
    const next = { projectId, environmentId, requirements: parsed.draft };
    if (active && JSON.stringify(active) === JSON.stringify(next)) void inspect.refetch();
    else setSubmitted(next);
  }

  // These inputs sit inside the resource form; Enter must inspect, not create the resource.
  function onKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key !== "Enter") return;
    event.preventDefault();
    run();
  }

  const report = inspect.data;
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <div>
        <h3 className="text-[13px] font-semibold text-fg">Placement advice</h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          Compare this team&apos;s nodes by currently measured free CPU and memory. Advice does not choose a node, move resources or reserve capacity.
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-cpus`} label="Estimated CPU cores" optional error={errors.cpus} hint="Blank ranks by free CPU only.">
          <LimitInput id={`${id}-cpus`} kind="cpus" value={cpus} error={errors.cpus} onChange={setCpus} onKeyDown={onKeyDown} />
        </Field>
        <Field id={`${id}-memory`} label="Estimated memory (MiB)" optional error={errors.memory} hint="Blank ranks by free memory only.">
          <LimitInput id={`${id}-memory`} kind="memory" value={memory} error={errors.memory} onChange={setMemory} onKeyDown={onKeyDown} />
        </Field>
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={run} pending={inspect.isFetching} disabled={!online}>
          {report && !inspect.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          {report ? "Refresh advice" : "Inspect nodes"}
        </Button>
        {!online && <p className="text-xs text-fg-muted">Reconnect to inspect nodes.</p>}
      </div>
      {inspect.isError && (
        <Notice tone="warn" title="Placement advice unavailable">
          {errorMessage(inspect.error)}
        </Notice>
      )}
      {report && (
        <div className="space-y-2" aria-live="polite">
          <p className="text-xs text-fg-muted">{requirementText(report)}</p>
          {report.nodes.length === 0 ? (
            <p className="text-[13px] text-fg-muted">No nodes are assigned to this team.</p>
          ) : (
            <ul className="divide-y divide-line border-y border-line" aria-label="Nodes by measured free capacity">
              {report.nodes.map((node) => (
                <li key={node.id} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1.5 py-2.5">
                  <div className="min-w-0">
                    <p className="text-[13px] font-medium break-words text-fg">
                      {node.name}
                      {node.id === selectedNodeId && <span className="ml-2 text-xs font-normal text-fg-subtle">Selected</span>}
                    </p>
                    <p className="text-xs text-fg-muted">
                      {freeText(node)}
                      {node.sampled_at && (
                        <>
                          {" · measured "}
                          <RelativeTime value={node.sampled_at} />
                        </>
                      )}
                    </p>
                    <p className="text-xs break-words text-fg-subtle">{node.notice}</p>
                  </div>
                  <div className="flex flex-col items-start gap-1 sm:items-end">
                    <FitLabel fits={node.fits} />
                    <NodeStateLabel state={node.state} />
                  </div>
                </li>
              ))}
            </ul>
          )}
          {report.limited && <p className="text-xs text-fg-muted">Showing the first 256 nodes assigned to this team.</p>}
        </div>
      )}
    </div>
  );
}
