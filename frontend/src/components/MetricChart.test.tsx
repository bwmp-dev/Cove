import { fireEvent, render, screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { formatPercent, seriesColour } from "../lib/telemetry";
import { MetricChart } from "./MetricChart";

const start = Date.UTC(2026, 10, 22, 12, 0);
const minute = 60_000;

it("states latest and peak readings and lists unknown samples as unknown, not zero", () => {
  render(
    <MetricChart
      title="CPU"
      series={[{ key: "cpu", label: "CPU", colour: seriesColour(0), points: [{ t: start + minute, v: 40 }, { t: start + 2 * minute, v: undefined }, { t: start + 3 * minute, v: 12.5 }] }]}
      start={start}
      end={start + 60 * minute}
      gap={3 * minute}
      format={formatPercent}
      formatTime={(time) => new Date(time).toISOString()}
      ceiling={100}
    />,
  );
  expect(screen.getByText("latest 12.5% · peak 40.0%")).toBeInTheDocument();
  fireEvent.click(screen.getByText("Samples table"));
  const table = screen.getByRole("table");
  const rows = within(table).getAllByRole("row");
  expect(rows).toHaveLength(4);
  expect(within(rows[2]!).getByText("Unknown")).toBeInTheDocument();
  expect(within(table).queryByText("0.0%")).not.toBeInTheDocument();
});

it("shows an explanation instead of an empty plot when nothing was recorded", () => {
  render(
    <MetricChart
      title="Network"
      series={[{ key: "rx", label: "Received", colour: seriesColour(0), points: [{ t: start + minute, v: undefined }] }]}
      start={start}
      end={start + 60 * minute}
      gap={3 * minute}
      format={formatPercent}
      formatTime={String}
      emptyText="This worker does not report network counters."
    />,
  );
  expect(screen.getByText("This worker does not report network counters.")).toBeInTheDocument();
  expect(screen.getByText("no readings")).toBeInTheDocument();
});
