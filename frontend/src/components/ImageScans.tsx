import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCw, ScanLine } from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { imageScan, imageScanningQuery, imageScansKey, imageScansQuery, useSaveImageScanning, type ImageScanningInput, type ImageScanningSettings, type ImageScanResults } from "../api/image-scans";
import { useQueueOperation } from "../api/mutations";
import { nodesQuery } from "../api/queries";
import type { ImageScan, OperationState, ScanFinding, ScannedImage, Workload } from "../api/types";
import { useNow, useOnline } from "../lib/hooks";
import {
  activeScanOperation,
  coverageLabel,
  databaseMaxAge,
  enableBlocker,
  formatGiB,
  imageComplete,
  imageStateLabel,
  plural,
  reportComplete,
  resultComplete,
  resultLabel,
  scanBlocker,
  severities,
  severityCounts,
  severityLabels,
  severityOf,
  severityTones,
  totalFindings,
  unknownReasons,
  type SeverityCounts,
} from "../lib/image-scans";
import { stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { isManagedPreview } from "../lib/previews";
import { buttonClass, linkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { OperationStateLabel, StatusDot, StatusLabel } from "./status";
import { Button, DefinitionList, LoadingRows, Notice, QueryError, Section, Subsection, SwitchField } from "./ui";

const title = "Vulnerability scans";
const description =
  "Checks the images this resource retains for known vulnerabilities in operating-system and language packages. Scans only read image metadata: they never pull application images, deploy, start or wake containers, or change Compose. Results inform and can notify; they never block deploys.";
const collapsedFindings = 10;

export function ImageScans({ workload }: { workload: Workload }) {
  if (isManagedPreview(workload)) {
    return (
      <Section title={title} description={description}>
        <p className="text-[13px] text-fg-muted">Managed previews are not scanned.</p>
      </Section>
    );
  }
  return <ScanSection workload={workload} />;
}

type Queue = ReturnType<typeof useQueueOperation>;
type Save = ReturnType<typeof useSaveImageScanning>;

// The latest result changes when the active operation finishes, which the settings poll notices first.
function useRefreshWhenFinished(workloadId: string, active: string | null) {
  const client = useQueryClient();
  const previousActive = useRef(active);
  useEffect(() => {
    if (previousActive.current && !active) void client.invalidateQueries({ queryKey: imageScansKey(workloadId), exact: true });
    previousActive.current = active;
  }, [active, client, workloadId]);
}

function useNodeName(nodeId: string) {
  const nodes = useQuery(nodesQuery);
  return nodes.data?.find((item) => item.id === nodeId)?.name ?? "the resource's node";
}

function ScanSection({ workload }: { workload: Workload }) {
  const settings = useQuery(imageScanningQuery(workload.id));
  const active = activeScanOperation(settings.data);
  const results = useQuery(imageScansQuery(workload.id, Boolean(active)));
  const node = useNodeName(workload.node_id);
  const online = useOnline();
  const statusId = useId();
  // Owned here, not by the settings form or its parent rows: a successful save re-keys the form with the saved state,
  // and per-call mutation callbacks only run while the observer that called mutate is mounted.
  const queue = useQueueOperation(workload.id);
  const save = useSaveImageScanning(workload.id);
  const [outcome, setOutcome] = useState<string | null>(null);
  const canOperate = can(workload.permissions, "operate");
  const reason = scanBlocker({ online, settings: settings.data, settingsPending: settings.isPending, settingsError: settings.isError, resultsError: results.isError, node });
  useRefreshWhenFinished(workload.id, active);

  return (
    <Section
      title={title}
      description={description}
      flush
      actions={<ScanActions settings={settings} results={results} queue={queue} canOperate={canOperate} blocked={Boolean(reason) || Boolean(active)} statusId={statusId} />}
    >
      <div className="divide-y divide-line">
        <div id={statusId} className="space-y-2 p-4 empty:hidden" aria-live="polite">
          <ScanStatus canOperate={canOperate} reason={reason} active={active} />
          <QueueOutcome queue={queue} />
        </div>
        <ScanResults results={results} maxAgeHours={databaseMaxAge(settings.data)} />
        <ScanSettings workload={workload} settings={settings} save={save} node={node} online={online} outcome={outcome} onOutcome={setOutcome} />
      </div>
    </Section>
  );
}

interface ActionsProps {
  settings: UseQueryResult<ImageScanningSettings>;
  results: UseQueryResult<ImageScanResults>;
  queue: Queue;
  canOperate: boolean;
  blocked: boolean;
  statusId: string;
}

function ScanActions({ settings, results, queue, canOperate, blocked, statusId }: ActionsProps) {
  const fetching = settings.isFetching || results.isFetching;
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => void Promise.all([settings.refetch(), results.refetch()])} pending={fetching} disabled={settings.isPending && results.isPending}>
        {!fetching && <RotateCw aria-hidden className="size-3.5" />}
        Reload
      </Button>
      {canOperate && <ScanButton queue={queue} blocked={blocked} statusId={statusId} />}
    </>
  );
}

function ScanButton({ queue, blocked, statusId }: { queue: Queue; blocked: boolean; statusId: string }) {
  return (
    <Button size="sm" disabled={blocked || queue.isPending} pending={queue.isPending} aria-describedby={statusId} onClick={() => queue.mutate({ action: "image_scan" })}>
      {!queue.isPending && <ScanLine aria-hidden className="size-3.5" />}
      Scan for vulnerabilities
    </Button>
  );
}

function ScanStatus({ canOperate, reason, active }: { canOperate: boolean; reason: string | null; active: string | null }) {
  return (
    <>
      {!canOperate && <p className="text-xs text-fg-muted">Queuing a scan needs operator access. Results are read-only.</p>}
      {canOperate && reason && <p className="text-xs text-fg-muted">{reason}</p>}
      {active && <ActiveOperation id={active} />}
    </>
  );
}

function OperationLink({ id, label }: { id: string; label: string }) {
  return (
    <Link to="/operations/$operationId" params={{ operationId: id }} className={linkClass}>
      {label}
    </Link>
  );
}

function ActiveOperation({ id }: { id: string }) {
  return (
    <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted">
      <span>An operation is in progress on this resource. A new scan can be queued when it finishes, and a running scan can be cancelled from its operation page.</span>
      <OperationLink id={id} label="View operation" />
    </p>
  );
}

function QueueOutcome({ queue }: { queue: Queue }) {
  if (queue.isSuccess) {
    return (
      <Notice
        tone="success"
        title="Vulnerability scan queued"
        action={
          <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost", "", "sm")}>
            View operation
          </Link>
        }
      >
        Results appear here when the operation finishes.
      </Notice>
    );
  }
  if (queue.isError) {
    return (
      <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title="Could not queue the scan">
        <p className="break-words">{errorMessage(queue.error)}</p>
      </Notice>
    );
  }
  return null;
}

function Reference({ label, value }: { label: string; value: string }) {
  return (
    <p className="grid gap-x-3 text-xs @sm:grid-cols-[5rem_minmax(0,1fr)]">
      <span className="text-fg-muted">{label}</span>
      {value ? <span className="min-w-0 font-mono break-all text-fg">{value}</span> : <span className="text-fg-muted">Not reported</span>}
    </p>
  );
}

function Time({ value }: { value: string }) {
  return Number.isFinite(Date.parse(value)) ? <RelativeTime value={value} /> : <span className="text-fg-muted">Not recorded</span>;
}

function SeverityList({ counts, label }: { counts: SeverityCounts; label: string }) {
  return (
    <ul aria-label={label} className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
      {severities
        .filter((severity) => counts[severity] > 0)
        .map((severity) => (
          <li key={severity} className="inline-flex items-center gap-1.5">
            <StatusDot tone={severityTones[severity]} />
            <span className="text-fg-muted">{severityLabels[severity]}</span>
            <span className="font-medium text-fg tabular-nums">{counts[severity]}</span>
          </li>
        ))}
    </ul>
  );
}

function UnknownReasons({ data, maxAgeHours }: { data: ImageScanResults; maxAgeHours: number }) {
  const now = useNow(60_000);
  const { terminal, reasons } = unknownReasons(data, now, maxAgeHours);
  return (
    <Notice tone="warn" title={terminal ? "Scan did not finish" : "Coverage unknown"}>
      <ul className="list-disc space-y-0.5 pl-4">
        {reasons.map((reason) => (
          <li key={reason} className="break-words">
            {reason}
          </li>
        ))}
      </ul>
      <p className="mt-1">An unknown result is not a clean result.</p>
    </Notice>
  );
}

function ScanResults({ results, maxAgeHours }: { results: UseQueryResult<ImageScanResults>; maxAgeHours: number }) {
  if (results.isPending) return <LoadingRows label="Loading vulnerability results" rows={3} />;
  if (results.isError && !results.data) {
    return (
      <div className="p-4">
        <QueryError what="vulnerability results" error={results.error} onRetry={() => void results.refetch()} retrying={results.isFetching} />
      </div>
    );
  }
  const data = results.data;
  const unverified = results.isError;
  const complete = resultComplete(data, unverified);
  return (
    <>
      <Subsection title="Latest scan">
        {unverified && <ResultsRefreshError results={results} />}
        <ResultSummary data={data} unverified={unverified} />
        <ResultConclusion data={data} complete={complete} maxAgeHours={maxAgeHours} />
      </Subsection>
      <ScannedImages scan={data.result} unverified={unverified} complete={complete} />
    </>
  );
}

function ResultsRefreshError({ results }: { results: UseQueryResult<ImageScanResults> }) {
  return (
    <Notice
      tone="error"
      title="Could not refresh vulnerability results"
      action={
        <Button variant="ghost" size="sm" onClick={() => void results.refetch()} pending={results.isFetching}>
          {!results.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      }
    >
      <p className="break-words">{errorMessage(results.error)}</p>
      <p className="mt-1">Showing the last results that loaded. They are not verified as current, and scanning is paused until they load.</p>
    </Notice>
  );
}

function reportItems(scan: ImageScan) {
  const counts = severityCounts(scan.images);
  return [
    { term: "Scanned", value: <Time value={scan.checked_at} /> },
    { term: "Findings", value: totalFindings(counts) > 0 ? <SeverityList counts={counts} label="Findings by severity" /> : <span className="text-fg-muted">None reported</span> },
    { term: "Scanner", value: <Scanner scan={scan} /> },
    { term: "Vulnerability database", value: <Database database={scan.database} /> },
  ];
}

function ResultSummary({ data, unverified }: { data: ImageScanResults; unverified: boolean }) {
  const label = resultLabel(data, unverified);
  return (
    <DefinitionList
      items={[
        { term: "Result", value: <StatusLabel tone={label.tone} label={label.label} /> },
        ...(data.operation_id ? [{ term: "Operation", value: <ScanOperation id={data.operation_id} state={data.operation_state} /> }] : []),
        ...(data.result ? reportItems(data.result) : []),
      ]}
    />
  );
}

function ResultConclusion({ data, complete, maxAgeHours }: { data: ImageScanResults; complete: boolean; maxAgeHours: number }) {
  if (!data.operation_id) return <p className="text-[13px] text-fg-muted">{data.message ?? "No vulnerability scan has completed."}</p>;
  if (!complete) return <UnknownReasons data={data} maxAgeHours={maxAgeHours} />;
  const total = totalFindings(severityCounts(data.result?.images ?? []));
  return (
    <p className="text-[13px] text-fg">
      {total > 0 ? `Scan completed. ${plural(total, "known vulnerability", "known vulnerabilities")} in scanned packages.` : "Scan completed. No known vulnerabilities in scanned packages."}
    </p>
  );
}

function ScannedImages({ scan, unverified, complete }: { scan: ImageScan | null; unverified: boolean; complete: boolean }) {
  if (!scan || scan.images.length === 0) return null;
  return (
    <ul aria-label={unverified ? "Scanned images (unverified)" : "Scanned images"} className="divide-y divide-line">
      {scan.images.map((image) => (
        <ImageResult key={`${image.service}:${image.image_identity ?? image.image_id}:${image.config_digest}`} image={image} muted={unverified || !complete} />
      ))}
    </ul>
  );
}

function ScanOperation({ id, state }: { id: string; state: string | null }) {
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {state && state in stateLabels ? <OperationStateLabel state={state as OperationState} /> : <span className="text-fg-muted">{state ? `Unknown (${state})` : "State not reported"}</span>}
      <OperationLink id={id} label="View scan" />
    </span>
  );
}

function Scanner({ scan }: { scan: ImageScan }) {
  return (
    <span className="block space-y-0.5">
      <span className="block">{scan.scanner_version ? `Version ${scan.scanner_version}` : <span className="text-fg-muted">Version not reported</span>}</span>
      {scan.scanner_image && <span className="block font-mono text-xs break-all text-fg-muted">{scan.scanner_image}</span>}
    </span>
  );
}

function Database({ database }: { database: ImageScan["database"] }) {
  return (
    <span className="block space-y-0.5">
      {database.digest ? <span className="block font-mono text-xs break-all">{database.digest}</span> : <span className="block text-fg-muted">Not reported</span>}
      <span className="flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-fg-muted">
        <span>
          Updated <Time value={database.updated_at} />
        </span>
        <span>
          Next update <Time value={database.next_update} />
        </span>
      </span>
    </span>
  );
}

function Coverage({ image }: { image: ScannedImage }) {
  const rows = [
    { term: "OS packages", value: coverageLabel(image.coverage.os) },
    { term: "Other language packages", value: coverageLabel(image.coverage.other_languages) },
    { term: "Java archives (JAR, WAR, EAR)", value: coverageLabel(image.coverage.java_archives, true) },
  ];
  return (
    <div className="space-y-1">
      <dl aria-label={`Coverage for ${image.service || "this image"}`} className="grid gap-x-4 gap-y-1 text-xs @sm:grid-cols-[14rem_minmax(0,1fr)]">
        {rows.map((row) => (
          <div key={row.term} className="contents">
            <dt className="text-fg-muted">{row.term}</dt>
            <dd className="mb-1 @sm:mb-0">
              <StatusLabel tone={row.value.tone} label={row.value.label} />
            </dd>
          </div>
        ))}
      </dl>
      {image.coverage.java_archives !== "not_detected" && (
        <p className="text-xs break-words text-fg-muted">{image.coverage.java_message ?? "Java archive dependency coverage is unknown for this image."}</p>
      )}
    </div>
  );
}

function ImageResult({ image, muted }: { image: ScannedImage; muted: boolean }) {
  const state = imageStateLabel(image);
  const counts = severityCounts([image]);
  const total = totalFindings(counts);
  const complete = imageComplete(image);
  return (
    <li className="min-w-0 space-y-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <h4 className="min-w-0 text-[13px] font-semibold break-words text-fg">{image.service || "Unnamed service"}</h4>
        <StatusLabel tone={muted && complete ? "neutral" : state.tone} label={state.label} />
      </div>
      <div className="space-y-0.5">
        <Reference label="Image" value={image.image_id} />
        <Reference label="Config" value={image.config_digest} />
      </div>
      {image.message && <p className="text-xs break-words text-fg-muted">{image.message}</p>}
      <Coverage image={image} />
      {image.end_of_life && (
        <Notice tone="warn" title="End-of-life operating system">
          The image&apos;s operating system release no longer receives security updates from its distribution, so findings may be incomplete.
        </Notice>
      )}
      {total > 0 ? (
        <SeverityList counts={counts} label={`Findings by severity for ${image.service || "this image"}`} />
      ) : (
        <p className="text-xs text-fg-muted">{complete ? "No known vulnerabilities in scanned packages." : "No findings reported, but coverage is unknown."}</p>
      )}
      {image.findings.length > 0 && <Findings image={image} total={total} />}
      {image.findings.length === 0 && total > 0 && <p className="text-xs text-fg-muted">No individual findings were stored for this image.</p>}
    </li>
  );
}

function Findings({ image, total }: { image: ScannedImage; total: number }) {
  const [expanded, setExpanded] = useState(false);
  const tableId = useId();
  const stored = image.findings.length;
  const shown = expanded ? image.findings : image.findings.slice(0, collapsedFindings);
  const name = image.service || "this image";
  return (
    <div className="min-w-0 space-y-2">
      {(image.truncated || stored < total) && (
        <p className="text-xs text-fg-muted">
          {stored} of {total} findings are stored, most severe first. The counts include every finding.
        </p>
      )}
      <table id={tableId} className="w-full table-fixed text-left text-xs">
        <caption className="sr-only">Findings for {name}</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="w-24 pr-3 md:w-28">
              Severity
            </th>
            <th scope="col" className="pr-3">
              Vulnerability
            </th>
            <th scope="col" className="hidden pr-3 md:table-cell">
              Package
            </th>
            <th scope="col" className="hidden pr-3 md:table-cell">
              Installed
            </th>
            <th scope="col" className="hidden md:table-cell">
              Fixed in
            </th>
          </tr>
        </thead>
        <tbody>
          {shown.map((finding) => (
            <FindingRow key={`${finding.severity}:${finding.id}:${finding.package}:${finding.installed}:${finding.fixed ?? ""}`} finding={finding} />
          ))}
        </tbody>
      </table>
      {stored > collapsedFindings && (
        <Button variant="ghost" size="sm" aria-expanded={expanded} aria-controls={tableId} onClick={() => setExpanded(!expanded)}>
          {expanded ? `Show the first ${collapsedFindings} findings` : `Show all ${stored} stored findings`}
        </Button>
      )}
    </div>
  );
}

function FixedVersion({ value }: { value?: string }) {
  return value ? <span className="font-mono break-all text-fg">{value}</span> : <span className="text-fg-muted">No fix reported</span>;
}

function FindingRow({ finding }: { finding: ScanFinding }) {
  const severity = severityOf(finding.severity);
  return (
    <tr className={`${tableRowClass} align-top`}>
      <td className="py-2 pr-3">
        <StatusLabel tone={severityTones[severity]} label={severityLabels[severity]} />
      </td>
      <td className="py-2 pr-3">
        <span className="block font-mono break-all text-fg">{finding.id || "Not reported"}</span>
        <span className="mt-0.5 block space-y-0.5 text-fg-muted md:hidden">
          <span className="block break-all">
            <span className="font-mono text-fg">{finding.package}</span> {finding.installed && <span className="font-mono">{finding.installed}</span>}
          </span>
          <span className="block">
            Fixed in <FixedVersion value={finding.fixed} />
          </span>
        </span>
      </td>
      <td className="hidden py-2 pr-3 font-mono break-all text-fg md:table-cell">{finding.package}</td>
      <td className="hidden py-2 pr-3 font-mono break-all text-fg-muted md:table-cell">{finding.installed}</td>
      <td className="hidden py-2 md:table-cell">
        <FixedVersion value={finding.fixed} />
      </td>
    </tr>
  );
}

interface SettingsProps {
  workload: Workload;
  settings: UseQueryResult<ImageScanningSettings>;
  save: Save;
  node: string;
  online: boolean;
  outcome: string | null;
  onOutcome: (outcome: string | null) => void;
}

function ScanSettings({ workload, settings, save, node, online, outcome, onOutcome }: SettingsProps) {
  if (settings.isPending) return <LoadingRows label="Loading scanning settings" rows={2} />;
  if (settings.isError && !settings.data) {
    return (
      <div className="p-4">
        <QueryError what="the scanning settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
      </div>
    );
  }
  const data = settings.data;
  const unreadable = settings.isError;
  const blocked = !online ? "You are offline." : unreadable ? "The latest settings could not be read." : undefined;
  return (
    <Subsection title="Team setting for this node" description={`One setting for every resource of this team on ${node}, not only ${workload.name}.`}>
      <div className="space-y-2 empty:hidden" aria-live="polite">
        {unreadable && (
          <Notice
            tone="error"
            title="Could not refresh the scanning settings"
            action={
              <Button variant="ghost" size="sm" onClick={() => void settings.refetch()} pending={settings.isFetching}>
                {!settings.isFetching && <RotateCw aria-hidden className="size-3.5" />}
                Retry
              </Button>
            }
          >
            <p className="break-words">{errorMessage(settings.error)}</p>
            <p className="mt-1">{data.can_manage ? "Showing the last settings that loaded. Saving and scanning are paused until the current settings load." : "Showing the last settings that loaded. Scanning is paused until the current settings load."}</p>
          </Notice>
        )}
        {data.can_manage && !online && <Notice tone="warn" title="You are offline">Saving is paused until the connection returns.</Notice>}
        {outcome && <Notice tone="success" title={outcome} />}
      </div>
      <WorkerStatus settings={data} node={node} workload={workload} />
      {data.can_manage ? (
        <SettingsForm key={`${data.enabled}:${data.notify}`} settings={data} save={save} node={node} workload={workload} blocked={blocked} onOutcome={onOutcome} />
      ) : (
        <ReadOnlySettings settings={data} />
      )}
    </Subsection>
  );
}

function WorkerStatus({ settings, node, workload }: { settings: ImageScanningSettings; node: string; workload: Workload }) {
  return (
    <>
      {!settings.worker_supported && (
        <Notice tone="warn" title="Node operator setup needed">
          The worker on {node} does not report vulnerability scanning. The node operator turns it on by setting COVE_IMAGE_SCANNING=1 in the worker daemon&apos;s environment, restarting the daemon and verifying the node from the{" "}
          <Link to="/nodes/$nodeId" params={{ nodeId: workload.node_id }} className={linkClass}>
            node page
          </Link>
          . The team setting below does not change the node.
        </Notice>
      )}
      <DefinitionList
        items={[
          { term: "Worker", value: settings.worker_supported ? <StatusLabel tone="ok" label="Scanning reported" /> : <StatusLabel tone="warn" label="Scanning not reported" /> },
          { term: "Node", value: settings.node_ready ? <StatusLabel tone="ok" label="Ready" /> : <StatusLabel tone="warn" label="Not ready" /> },
          { term: "Scanner image", value: settings.scanner_image ? <span className="font-mono text-xs break-all">{settings.scanner_image}</span> : <span className="text-fg-muted">Not reported</span> },
          {
            term: "Java archives",
            value: (
              <span className="block space-y-0.5">
                <StatusLabel tone="warn" label={settings.java_archives === "not_scanned" ? "Not scanned" : coverageLabel(settings.java_archives, true).label} />
                <span className="block text-xs text-fg-muted">Dependencies inside JAR, WAR and EAR files are not scanned. Images that contain them report Java coverage as unknown beside their other results.</span>
              </span>
            ),
          },
        ]}
      />
    </>
  );
}

function ReadOnlySettings({ settings }: { settings: ImageScanningSettings }) {
  return (
    <div className="space-y-3">
      <DefinitionList
        items={[
          { term: "Vulnerability scans", value: settings.enabled ? "Allowed" : "Off" },
          { term: "Notifications", value: settings.notify ? (settings.enabled ? "On" : "On, but nothing is sent while scans are off") : "Off" },
        ]}
      />
      <p className="text-xs text-fg-muted">Only team administrators can change this setting.</p>
    </div>
  );
}

interface FormProps {
  settings: ImageScanningSettings;
  save: Save;
  node: string;
  workload: Workload;
  blocked?: string;
  onOutcome: (outcome: string | null) => void;
}

function SettingsForm({ settings, save, node, workload, blocked, onOutcome }: FormProps) {
  const id = useId();
  // Unsaved edits only; the form is re-keyed when the saved settings change, which discards them.
  const [draft, setDraft] = useState<ImageScanningInput | null>(null);
  const enabled = draft?.enabled ?? settings.enabled;
  const notify = draft?.notify ?? settings.notify;
  const [confirm, setConfirm] = useState(false);
  const dirty = enabled !== settings.enabled || notify !== settings.notify;
  const blocker = enableBlocker(settings, node);
  const enableError = enabled && blocker ? `${blocker}${settings.enabled ? " You can still turn scans off." : ""}` : undefined;

  function edit(next: ImageScanningInput) {
    setDraft(next);
    save.reset();
    onOutcome(null);
  }

  function send() {
    save.mutate(
      { enabled, notify },
      {
        onSuccess: () => {
          onOutcome("Scanning settings saved");
          save.reset();
        },
      },
    );
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (enableError) {
      document.getElementById(`${id}-enabled`)?.focus();
      return;
    }
    if (enabled && !settings.enabled) setConfirm(true);
    else send();
  }

  return (
    <form onSubmit={submit} aria-label="Vulnerability scan settings" noValidate className="space-y-4 border-t border-line pt-4">
      <UnsavedChangesGuard dirty={dirty} />
      <fieldset disabled={save.isPending} className="min-w-0 space-y-4">
        <legend className="sr-only">Vulnerability scan settings</legend>
        <EnableSwitch id={`${id}-enabled`} enabled={enabled} blocker={blocker} error={enableError} node={node} onChange={(value) => edit({ enabled: value, notify })} />
        <NotifySwitch id={`${id}-notify`} enabled={enabled} notify={notify} onChange={(value) => edit({ enabled, notify: value })} />
      </fieldset>
      {save.isError && (
        <Notice tone="error" title="The scanning settings were not saved">
          <p className="break-words">{errorMessage(save.error)}</p>
          <p className="mt-1">Your changes are kept.</p>
        </Notice>
      )}
      <SaveBar dirty={dirty} pending={save.isPending} blocked={Boolean(blocked)} label="Save scanning settings" onDiscard={() => edit({ enabled: settings.enabled, notify: settings.notify })} status={blocked ? `${blocked} Saving is paused.` : "No unsaved changes"} />
      <EnableDialog
        open={confirm}
        settings={settings}
        node={node}
        workload={workload}
        pending={save.isPending}
        onCancel={() => setConfirm(false)}
        onConfirm={() => {
          setConfirm(false);
          send();
        }}
      />
    </form>
  );
}

interface EnableSwitchProps {
  id: string;
  enabled: boolean;
  blocker: string | null;
  error?: string;
  node: string;
  onChange: (enabled: boolean) => void;
}

function EnableSwitch({ id, enabled, blocker, error, node, onChange }: EnableSwitchProps) {
  const locked = Boolean(blocker) && !enabled;
  return (
    <SwitchField
      id={id}
      label="Allow vulnerability scans"
      checked={enabled}
      disabled={locked}
      aria-invalid={error ? true : undefined}
      onChange={(event) => onChange(event.target.checked)}
      description={
        error ? (
          <span className="text-bad">{error}</span>
        ) : locked ? (
          blocker
        ) : (
          `Off by default. Applies to every resource of this team on ${node}. Turning it off stops new scans; a scan that is already running must be cancelled from its operation.`
        )
      }
    />
  );
}

function NotifySwitch({ id, enabled, notify, onChange }: { id: string; enabled: boolean; notify: boolean; onChange: (notify: boolean) => void }) {
  return (
    <SwitchField
      id={id}
      label="Notify when a scan finishes"
      checked={notify}
      onChange={(event) => onChange(event.target.checked)}
      description={
        <>
          Each finished scan sends one Vulnerability scans event (known vulnerabilities, completed with none, or coverage unknown) to enabled notification connections that select it under Specific events. By result routing does not include them. {notify && !enabled && "Nothing is sent while scans are off. "}Choose them in{" "}
          <Link to="/notifications" className={linkClass}>
            Notifications
          </Link>
          .
        </>
      }
    />
  );
}

interface EnableDialogProps {
  open: boolean;
  settings: ImageScanningSettings;
  node: string;
  workload: Workload;
  pending: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}

function EnableDialog({ open, settings, node, workload, pending, onCancel, onConfirm }: EnableDialogProps) {
  const limit = formatGiB(settings.database_limit_bytes);
  return (
    <ConfirmDialog open={open} title={`Allow vulnerability scans on ${node}?`} confirmLabel="Allow scans" pending={pending} onCancel={onCancel} onConfirm={onConfirm}>
      <p>
        This applies to every resource of this team on {node}, not only {workload.name}. Operators of those resources can then queue scans, one at a time per node.
      </p>
      <p className="mt-2">
        A scan reads the retained images&apos; metadata and exports them to a read-only archive on the node. The node may pull the pinned scanner image and refresh the vulnerability database from the public GitHub Container Registry
        {limit ? ` (up to ${limit} per refresh)` : ""}, and each scan uses node CPU, memory and disk while it runs. No credentials are used. Scans never pull application images, deploy or start containers, and results do not block deploys.
      </p>
    </ConfirmDialog>
  );
}

// Shown on the operation page: what this scan recorded when it finished. The Registry page applies later freshness checks.
export function ScanOutcome({ scan: recorded, workloadId }: { scan: ImageScan; workloadId?: string }) {
  const scan = imageScan(recorded);
  const complete = reportComplete(scan);
  const counts = severityCounts(scan.images);
  const total = totalFindings(counts);
  const java = scan.images.filter((image) => image.coverage.java_archives === "not_scanned").length;
  return (
    <div className="space-y-1">
      <StatusLabel tone={complete ? (total > 0 ? "warn" : "ok") : "warn"} label={complete ? "Scan completed" : "Coverage unknown"} />
      <p className="text-xs text-fg-muted">
        {plural(scan.images.length, "image", "images")} scanned.{" "}
        {total > 0 ? `${plural(total, "known vulnerability", "known vulnerabilities")} in scanned packages.` : complete ? "No known vulnerabilities in scanned packages." : "No findings reported, but coverage is unknown."}
      </p>
      {total > 0 && <SeverityList counts={counts} label="Findings by severity" />}
      {java > 0 && <p className="text-xs text-fg-muted">Java archive (JAR, WAR or EAR) dependencies were not scanned in {plural(java, "image", "images")}, so their coverage is unknown.</p>}
      {scan.message && <p className="text-xs break-words text-fg-muted">{scan.message}</p>}
      {workloadId && (
        <p className="text-xs text-fg-muted">
          As recorded when the scan finished. Findings, coverage and whether the result is still current are on the{" "}
          <Link to="/resources/$resourceId/registry" params={{ resourceId: workloadId }} className={linkClass}>
            Registry page
          </Link>
          .
        </p>
      )}
    </div>
  );
}
