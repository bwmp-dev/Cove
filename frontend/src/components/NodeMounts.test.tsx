import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { Node, NodeMetrics } from "../api/types";
import { NodeMounts } from "./NodeMounts";

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));

function nodeWith(metrics: Partial<NodeMetrics>, extra: Partial<Node> = {}): Node {
  return {
    id: "n1",
    name: "edge-1",
    state: "ready",
    mode: "local",
    address: "",
    port: 0,
    last_checked_at: null,
    capabilities: { mount_metrics: true, protocol: 1, os: "", arch: "", docker: "", compose: "", ready: true, metrics: { sampled_at: new Date().toISOString(), cpu_percent: 1, memory_total: 1, memory_available: 1, disk_total: 1, disk_available: 1, ...metrics } },
    ...extra,
  };
}

const mounts = [
  { path: "/", filesystem: "ext4", total: 1000, available: 250, known: true },
  { path: "/proc", filesystem: "proc", total: 0, available: 0, known: false },
  { path: "/mnt/nfs", filesystem: "nfs", total: 0, available: 0, known: true, error: "stale file handle" },
];

it("lists measured mounts first and shows pseudo and unreadable mounts on request", () => {
  render(<NodeMounts node={nodeWith({ mounts, mounts_limited: true })} />);
  const table = screen.getByRole("table", { name: "Mounted filesystems" });
  expect(within(table).getByRole("meter", { name: "/ used" })).toHaveAttribute("aria-valuetext", "750 B of 1000 B, 75%");
  expect(within(table).queryByText("/proc")).not.toBeInTheDocument();
  expect(screen.getByText("Mount list clipped")).toBeInTheDocument();
  expect(screen.getByText("1 of 3 mounts")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Show mounts" }));
  fireEvent.click(screen.getByRole("option", { name: "Without capacity" }));
  expect(within(table).getByText("Pseudo or unknown filesystem")).toBeInTheDocument();
  expect(within(table).getByText("Unreadable")).toBeInTheDocument();
  expect(within(table).getByText("stale file handle")).toBeInTheDocument();
  expect(within(table).queryByRole("meter")).not.toBeInTheDocument();
});

it("marks readings from a stale sample or an old worker", () => {
  const { unmount } = render(<NodeMounts node={nodeWith({ mounts, sampled_at: "2026-01-01T00:00:00Z" })} />);
  expect(screen.getByText("Mount readings may be outdated")).toBeInTheDocument();
  unmount();
  render(<NodeMounts node={{ ...nodeWith({}), capabilities: { protocol: 1, os: "", arch: "", docker: "", compose: "", ready: true } }} />);
  expect(screen.getByText(/does not report mounted filesystems/)).toBeInTheDocument();
});
