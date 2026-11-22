import { useId } from "react";
import type { DeployMarker } from "../api/telemetry";
import { formatAbsolute } from "../lib/format";
import { niceCeiling, segments, type ChartSeries } from "../lib/telemetry";

const width = 1000;
const height = 100;
const tableRows = 50;

interface MetricChartProps {
  title: string;
  // Units and what the value means, shown under the title.
  description?: string;
  series: ChartSeries[];
  start: number;
  end: number;
  gap: number;
  format: (value: number) => string;
  formatTime: (time: number) => string;
  // A known upper bound, such as 100% CPU or total memory. The axis still grows if a reading exceeds it.
  ceiling?: number;
  markers?: DeployMarker[];
  // Shown instead of the plot when no series has a reading.
  emptyText?: string;
}

function summary(line: ChartSeries) {
  let latest: number | undefined;
  let peak: number | undefined;
  for (const point of line.points) {
    if (point.v === undefined) continue;
    latest = point.v;
    peak = peak === undefined ? point.v : Math.max(peak, point.v);
  }
  return { latest, peak };
}

function sampleRows(series: ChartSeries[]) {
  const rows = new Map<number, (number | undefined)[]>();
  series.forEach((line, column) => {
    for (const point of line.points) {
      const row = rows.get(point.t) ?? Array.from({ length: series.length }, () => undefined);
      row[column] = point.v;
      rows.set(point.t, row);
    }
  });
  return [...rows.entries()].sort((a, b) => b[0] - a[0]);
}

// A line chart drawn with SVG presentation attributes only, so it needs no style attributes under the CSP.
// The plot is decorative for assistive technology: the legend states each line's latest and peak reading
// and the samples table lists the readings with their real timestamps.
export function MetricChart({ title, description, series, start, end, gap, format, formatTime, ceiling, markers = [], emptyText = "No readings in this range." }: MetricChartProps) {
  const id = useId();
  const span = Math.max(end - start, 1);
  const visible = series.map((line) => ({ ...line, points: line.points.filter((point) => point.t >= start && point.t <= end) }));
  const readings = visible.flatMap((line) => line.points.flatMap((point) => (point.v === undefined ? [] : [point.v])));
  const top = Math.max(ceiling ?? 0, niceCeiling(readings.length ? Math.max(...readings) : 0));
  const x = (time: number) => ((time - start) / span) * width;
  const y = (value: number) => height - (Math.max(value, 0) / top) * height;
  const rows = sampleRows(visible);
  const inRange = markers.filter((marker) => {
    const time = Date.parse(marker.timestamp);
    return time >= start && time <= end;
  });

  return (
    <figure aria-labelledby={`${id}-title`} className="min-w-0">
      <figcaption className="mb-2 min-w-0">
        <p id={`${id}-title`} className="text-[13px] font-semibold text-fg">
          {title}
        </p>
        {description && <p className="mt-0.5 text-xs text-fg-muted">{description}</p>}
        <ul className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-xs" aria-label={`${title} lines`}>
          {visible.map((line) => {
            const { latest, peak } = summary(line);
            return (
              <li key={line.key} className="flex min-w-0 items-center gap-1.5">
                <span aria-hidden className={`size-2 shrink-0 rounded-sm ${line.colour.bg}`} />
                <span className="min-w-0 truncate text-fg" title={line.label}>
                  {line.label}
                </span>
                <span className="whitespace-nowrap text-fg-muted tabular-nums">{latest === undefined ? "no readings" : `latest ${format(latest)} · peak ${format(peak ?? latest)}`}</span>
              </li>
            );
          })}
        </ul>
      </figcaption>
      <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2">
        <div aria-hidden className="flex h-32 flex-col justify-between text-right text-[11px] text-fg-subtle tabular-nums">
          <span>{format(top)}</span>
          <span>{format(top / 2)}</span>
          <span>{format(0)}</span>
        </div>
        <div className="relative h-32 border-b border-l border-line-strong">
          {readings.length === 0 ? (
            <p className="absolute inset-0 flex items-center justify-center px-3 text-center text-xs text-fg-muted">{emptyText}</p>
          ) : (
            <svg aria-hidden viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" className="absolute inset-0 size-full overflow-visible">
              <line x1={0} x2={width} y1={height / 2} y2={height / 2} className="stroke-line" strokeWidth={1} vectorEffect="non-scaling-stroke" />
              {inRange.map((marker) => (
                <line
                  key={marker.operation_id}
                  x1={x(Date.parse(marker.timestamp))}
                  x2={x(Date.parse(marker.timestamp))}
                  y1={0}
                  y2={height}
                  className={marker.state === "failed" ? "stroke-bad" : "stroke-fg-subtle"}
                  strokeWidth={1}
                  strokeDasharray="3 3"
                  vectorEffect="non-scaling-stroke"
                />
              ))}
              {visible.map((line) =>
                segments(line.points, gap).map((segment) => {
                  const lone = segment.length === 1;
                  const d = segment.map((point, index) => `${index ? "L" : "M"}${x(point.t)} ${y(point.v)}`).join("");
                  return (
                    <path
                      key={`${line.key}-${segment[0]?.t}`}
                      // A lone reading between gaps is drawn as a dot by the round cap on a near-zero-length line.
                      d={lone ? `${d}h0.01` : d}
                      className={line.colour.stroke}
                      fill="none"
                      strokeWidth={lone ? 4 : 1.5}
                      strokeLinecap="round"
                      strokeLinejoin="round"
                      vectorEffect="non-scaling-stroke"
                    />
                  );
                }),
              )}
            </svg>
          )}
        </div>
        <div aria-hidden />
        <div aria-hidden className="mt-1 flex justify-between gap-2 text-[11px] text-fg-subtle tabular-nums">
          <span>{formatTime(start)}</span>
          <span>{formatTime(end)}</span>
        </div>
      </div>
      {rows.length > 0 && (
        <details className="mt-2 text-xs">
          <summary className="w-fit cursor-pointer rounded text-fg-muted hover:text-fg">Samples table</summary>
          <div className="mt-2 max-h-64 overflow-auto rounded-md border border-line">
            <table className="w-full text-left tabular-nums">
              <caption className="sr-only">
                {title}, {rows.length > tableRows ? `most recent ${tableRows} of ${rows.length} samples` : `${rows.length} samples`}
              </caption>
              <thead className="sticky top-0 bg-surface">
                <tr className="border-b border-line text-fg-muted">
                  <th scope="col" className="px-2 py-1.5 font-medium">
                    Sampled
                  </th>
                  {visible.map((line) => (
                    <th key={line.key} scope="col" className="max-w-40 truncate px-2 py-1.5 text-right font-medium" title={line.label}>
                      {line.label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {rows.slice(0, tableRows).map(([time, values]) => (
                  <tr key={time}>
                    <td className="px-2 py-1 whitespace-nowrap text-fg-muted">
                      <time dateTime={new Date(time).toISOString()}>{formatAbsolute(new Date(time).toISOString())}</time>
                    </td>
                    {visible.map((line, column) => {
                      const value = values[column];
                      return (
                        <td key={line.key} className="px-2 py-1 text-right whitespace-nowrap text-fg">
                          {value === undefined ? <span className="text-fg-subtle">Unknown</span> : format(value)}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
            {rows.length > tableRows && <p className="border-t border-line px-2 py-1.5 text-fg-muted">Showing the most recent {tableRows} of {rows.length} samples.</p>}
          </div>
        </details>
      )}
    </figure>
  );
}
